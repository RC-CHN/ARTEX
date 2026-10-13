package runnerfiles

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
)

type Client struct {
	BaseURL    string // controller origin, without /runner/v1/files
	Credential string
	HTTP       *http.Client
}

func (c *Client) request(ctx context.Context, method, action, session, mount, p string, body io.Reader) (*http.Request, error) {
	u, e := url.Parse(strings.TrimRight(c.BaseURL, "/") + "/runner/v1/files/" + action)
	if e != nil {
		return nil, e
	}
	q := u.Query()
	q.Set("session", session)
	q.Set("mount", mount)
	if p != "" {
		q.Set("path", p)
	}
	u.RawQuery = q.Encode()
	r, e := http.NewRequestWithContext(ctx, method, u.String(), body)
	if e == nil {
		r.Header.Set("Authorization", "Bearer "+c.Credential)
	}
	return r, e
}
func (c *Client) do(r *http.Request) (*http.Response, error) {
	cl := c.HTTP
	if cl == nil {
		cl = http.DefaultClient
	}
	res, e := cl.Do(r)
	if e != nil {
		return nil, e
	}
	if res.StatusCode >= 200 && res.StatusCode < 300 {
		return res, nil
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(res.Body, 4096))
	var cause error
	switch res.StatusCode {
	case 409:
		cause = ErrConflict
	case 423:
		cause = ErrLocked
	case 403:
		cause = ErrPath
	case 422:
		cause = ErrDigest
	case 413:
		cause = ErrLimit
	default:
		cause = fmt.Errorf("HTTP %d", res.StatusCode)
	}
	return nil, fmt.Errorf("%w: %s", cause, strings.TrimSpace(string(b)))
}
func (c *Client) Manifest(ctx context.Context, session, mount string) ([]Entry, error) {
	r, e := c.request(ctx, "GET", "manifest", session, mount, "", nil)
	if e != nil {
		return nil, e
	}
	res, e := c.do(r)
	if e != nil {
		return nil, e
	}
	defer res.Body.Close()
	var entries []Entry
	e = json.NewDecoder(io.LimitReader(res.Body, 32<<20)).Decode(&entries)
	if e != nil {
		return nil, e
	}
	seen := map[string]bool{}
	for _, f := range entries {
		if validateEntry(f) != nil || seen[f.Path] {
			return nil, ErrPath
		}
		seen[f.Path] = true
	}
	return entries, nil
}
func validateEntry(entry Entry) error {
	if !validPath(entry.Path) || entry.Size < 0 || entry.Size >= 1<<63-1 || len(entry.Digest) != 64 {
		return ErrPath
	}
	if _, err := hex.DecodeString(entry.Digest); err != nil {
		return ErrDigest
	}
	return nil
}

func localVersion(ctx context.Context, root *os.Root, name string) (string, error) {
	entry, err := digestFileContext(ctx, root, name, 1<<63-2)
	if errors.Is(err, fs.ErrNotExist) {
		return Missing, nil
	}
	return entry.Digest, err
}

func (c *Client) Download(ctx context.Context, session, mount string, entry Entry, destination string) error {
	return c.download(ctx, session, mount, entry, destination, "")
}

// expectedLocal is the version checked by Syncer before the download. Recheck it
// after receiving the full blob so a write during transfer is a visible conflict.
func (c *Client) download(ctx context.Context, session, mount string, entry Entry, destination, expectedLocal string) error {
	if err := validateEntry(entry); err != nil {
		return err
	}
	if err := os.MkdirAll(destination, 0700); err != nil {
		return err
	}
	m := Mount{Source: destination}
	root, name, err := openParent(m, entry.Path, true)
	if err != nil {
		return err
	}
	defer root.Close()
	if expectedLocal == "" {
		expectedLocal, err = localVersion(ctx, root, name)
		if err != nil {
			return err
		}
	}
	tmp := ".artex-transfer-" + randomID()
	f, err := root.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	defer func() { f.Close(); root.Remove(tmp) }()
	req, err := c.request(ctx, "GET", "blob", session, mount, entry.Path, nil)
	if err != nil {
		return err
	}
	q := req.URL.Query()
	q.Set("digest", entry.Digest)
	req.URL.RawQuery = q.Encode()
	res, err := c.do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(f, h), io.LimitReader(contextReader{ctx, res.Body}, entry.Size+1))
	if err == nil && (n != entry.Size || hex.EncodeToString(h.Sum(nil)) != entry.Digest) {
		err = ErrDigest
	}
	if err == nil {
		err = f.Chmod(os.FileMode(entry.Mode) & 0777 &^ 0022)
	}
	if err == nil {
		err = f.Sync()
	}
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	if err = ctx.Err(); err != nil {
		return err
	}
	if err = sameParent(m, entry.Path, root); err != nil {
		return err
	}
	current, err := localVersion(ctx, root, name)
	if err != nil {
		return err
	}
	if current == entry.Digest {
		return ctx.Err()
	}
	if current != expectedLocal {
		return fmt.Errorf("%w: local %s changed during download", ErrConflict, entry.Path)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return root.Rename(tmp, name)
}

func (c *Client) Upload(ctx context.Context, session, mount, rootPath string, entry Entry, expected string) error {
	if err := validateEntry(entry); err != nil {
		return err
	}
	root, name, err := openParent(Mount{Source: rootPath}, entry.Path, false)
	if err != nil {
		return err
	}
	defer root.Close()
	// Never send a live file: background commands can overwrite it between the
	// manifest hash and HTTP streaming. Verify a private immutable snapshot first.
	snapshot, err := os.CreateTemp("", "artex-upload-*")
	if err != nil {
		return err
	}
	defer func() { snapshot.Close(); os.Remove(snapshot.Name()) }()
	copied, err := snapshotFile(ctx, root, name, snapshot, entry.Size)
	if err != nil {
		return err
	}
	if copied.Digest != entry.Digest || copied.Size != entry.Size {
		return ErrConflict
	}
	if _, err = snapshot.Seek(0, io.SeekStart); err != nil {
		return err
	}
	req, err := c.request(ctx, "PUT", "blob", session, mount, entry.Path, snapshot)
	if err != nil {
		return err
	}
	req.Header.Set("If-Match", expected)
	req.Header.Set("X-Content-SHA256", entry.Digest)
	req.Header.Set("X-File-Mode", strconv.FormatUint(uint64(entry.Mode), 8))
	res, err := c.do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	var published Entry
	if err = json.NewDecoder(io.LimitReader(res.Body, 4096)).Decode(&published); err != nil {
		return err
	}
	if published.Path != entry.Path || published.Digest != entry.Digest || published.Size != entry.Size {
		return ErrDigest
	}
	return ctx.Err()
}

// Syncer owns one attempt's private host cache. Calls are serialized. Files are
// only published after their complete bytes have been verified; a conflict is an
// error, never last-writer-wins. Remote deletions do not erase local work.
type Syncer struct {
	mu       sync.Mutex
	Client   *Client
	Session  string
	Plan     Plan
	Roots    map[string]string // mount name -> runner host cache, NOT sandbox target
	versions map[string]map[string]string
}

func (s *Syncer) Pull(ctx context.Context) error { return s.pull(ctx, false) }

// PullReadOnly refreshes peer artifacts without replacing this execution's
// writable files. Use after the initial Pull, while commands may be running.
func (s *Syncer) PullReadOnly(ctx context.Context) error { return s.pull(ctx, true) }

func (s *Syncer) pull(ctx context.Context, readOnly bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.versions == nil {
		s.versions = map[string]map[string]string{}
	}
	for _, m := range s.Plan.Mounts {
		root := s.Roots[m.Name]
		if root == "" {
			return ErrPath
		}
		if e := os.MkdirAll(root, 0700); e != nil {
			return e
		}
		entries, e := s.Client.Manifest(ctx, s.Session, m.Name)
		if e != nil {
			return e
		}
		if s.versions[m.Name] == nil {
			s.versions[m.Name] = map[string]string{}
		}
		for _, entry := range entries {
			if readOnly && writable(m, entry.Path) {
				continue
			}
			old := s.versions[m.Name][entry.Path]
			if old == entry.Digest {
				continue
			}
			rt, name, e := openParent(Mount{Source: root}, entry.Path, true)
			if e != nil {
				return e
			}
			local, le := digestFile(rt, name)
			rt.Close()
			if le == nil && old != "" && local.Digest != old && local.Digest != entry.Digest {
				return fmt.Errorf("%w: %s", ErrConflict, entry.Path)
			}
			if le == nil && old == "" && local.Digest != entry.Digest {
				return fmt.Errorf("%w: untracked local file %s", ErrConflict, entry.Path)
			}
			if le != nil && !os.IsNotExist(le) {
				return le
			}
			if old != "" && os.IsNotExist(le) {
				return fmt.Errorf("%w: local %s was removed", ErrConflict, entry.Path)
			}
			if local.Digest != entry.Digest {
				expectedLocal := local.Digest
				if os.IsNotExist(le) {
					expectedLocal = Missing
				}
				if e = s.Client.download(ctx, s.Session, m.Name, entry, root, expectedLocal); e != nil {
					return e
				}
			}
			s.versions[m.Name][entry.Path] = entry.Digest
		}
	}
	return nil
}
func (s *Syncer) Push(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.versions == nil {
		s.versions = map[string]map[string]string{}
	}
	for _, m := range s.Plan.Mounts {
		if len(m.Writable) == 0 {
			continue
		}
		root := s.Roots[m.Name]
		if root == "" {
			return ErrPath
		}
		entries, e := NewStore().Manifest(ctx, Mount{Source: root})
		if e != nil {
			return e
		}
		if s.versions[m.Name] == nil {
			s.versions[m.Name] = map[string]string{}
		}
		for _, entry := range entries {
			if !writable(m, entry.Path) {
				continue
			}
			old := s.versions[m.Name][entry.Path]
			if old == entry.Digest {
				continue
			}
			if old == "" {
				old = Missing
			}
			if e = s.Client.Upload(ctx, s.Session, m.Name, root, entry, old); e != nil {
				return fmt.Errorf("publish %s: %w", entry.Path, e)
			}
			s.versions[m.Name][entry.Path] = entry.Digest
		}
	}
	return nil
}
