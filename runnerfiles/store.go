package runnerfiles

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"hash/fnv"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

type fileLease struct {
	Owner, Token string
	Until        time.Time
}

// Store serializes publication within one controller. Atomic rename means readers
// see complete files; compare-and-swap prevents lost updates between runners.
// A single controller owns the workspace (matching ARTEX's existing architecture).
type Store struct {
	locks         [127]sync.Mutex
	mu            sync.Mutex
	leases        map[string]fileLease
	MaxBytes      int64
	MaxFiles      int
	MaxTotalBytes int64
	MaxEntries    int
	Now           func() time.Time
}

func NewStore() *Store {
	return &Store{leases: map[string]fileLease{}, MaxBytes: MaxSnapshotBytes, MaxFiles: MaxSnapshotFiles, MaxTotalBytes: MaxSnapshotBytes, MaxEntries: MaxSnapshotEntries, Now: time.Now}
}
func (s *Store) key(m Mount, p string) string { return filepath.Clean(m.Source) + "/" + p }
func (s *Store) lock(key string) func() {
	h := fnv.New32a()
	_, _ = h.Write([]byte(key))
	mu := &s.locks[h.Sum32()%uint32(len(s.locks))]
	mu.Lock()
	return mu.Unlock
}
func (s *Store) checkLease(key, owner, token string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	l, ok := s.leases[key]
	if !ok {
		if token != "" {
			return ErrLocked
		}
		return nil
	}
	if !s.Now().Before(l.Until) {
		delete(s.leases, key)
		if token != "" {
			return ErrLocked
		}
		return nil
	}
	if l.Owner != owner || l.Token != token {
		return ErrLocked
	}
	return nil
}
func randomID() string {
	var b [24]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b[:])
}
func (s *Store) Lease(m Mount, p, owner string, ttl time.Duration) (string, error) {
	if !writable(m, p) || owner == "" || ttl <= 0 || ttl > 5*time.Minute {
		return "", ErrPath
	}
	k := s.key(m, p)
	unlock := s.lock(k)
	defer unlock()
	s.mu.Lock()
	defer s.mu.Unlock()
	if l, ok := s.leases[k]; ok && s.Now().Before(l.Until) {
		return "", ErrLocked
	}
	token := randomID()
	s.leases[k] = fileLease{owner, token, s.Now().Add(ttl)}
	return token, nil
}
func (s *Store) Unlock(m Mount, p, owner, token string) error {
	if !writable(m, p) {
		return ErrPath
	}
	k := s.key(m, p)
	unlock := s.lock(k)
	defer unlock()
	s.mu.Lock()
	defer s.mu.Unlock()
	l, ok := s.leases[k]
	if !ok {
		return nil
	}
	if l.Owner != owner || l.Token != token {
		return ErrLocked
	}
	delete(s.leases, k)
	return nil
}

func digestFile(r *os.Root, p string) (Entry, error) {
	return digestFileContext(context.Background(), r, p, 1<<63-2)
}

func digestFileContext(ctx context.Context, r *os.Root, p string, max int64) (Entry, error) {
	return snapshotFile(ctx, r, p, io.Discard, max)
}

// snapshotFile refuses files changed while copying, including atomic replacement.
// The caller never publishes or transmits the snapshot until this check succeeds.
func snapshotFile(ctx context.Context, r *os.Root, p string, dst io.Writer, max int64) (Entry, error) {
	f, fi, err := openRegular(r, p)
	if err != nil {
		return Entry{}, err
	}
	defer f.Close()
	if fi.Size() > max || max < 0 || max == 1<<63-1 {
		return Entry{}, ErrLimit
	}
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(dst, h), io.LimitReader(contextReader{ctx, f}, max+1))
	if err != nil {
		return Entry{}, err
	}
	if !unchanged(r, p, f, fi, n) {
		return Entry{}, fmt.Errorf("%w: %s", ErrSourceBusy, p)
	}
	if err := ctx.Err(); err != nil {
		return Entry{}, err
	}
	return Entry{p, hex.EncodeToString(h.Sum(nil)), n, uint32(fi.Mode().Perm())}, nil
}
func (s *Store) Manifest(ctx context.Context, m Mount) ([]Entry, error) {
	return s.manifest(ctx, m, false)
}

func (s *Store) manifest(ctx context.Context, m Mount, checkpoint bool) ([]Entry, error) {
	r, err := os.OpenRoot(m.Source)
	if err != nil {
		return nil, err
	}
	defer r.Close()
	out := []Entry{}
	var total int64
	visited := 0
	var walk func(*os.Root, string) error
	walk = func(dir *os.Root, prefix string) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if strings.Count(prefix, "/") >= 64 {
			return ErrLimit
		}
		listing, err := dir.Open(".")
		if err != nil {
			return err
		}
		defer listing.Close()
		for {
			entries, err := listing.ReadDir(128)
			if err != nil && err != io.EOF {
				return err
			}
			if len(entries) == 0 {
				break
			}
			for _, d := range entries {
				if err := ctx.Err(); err != nil {
					return err
				}
				visited++
				if visited > s.MaxEntries {
					return ErrLimit
				}
				if strings.HasPrefix(d.Name(), ".artex-transfer-") {
					continue
				}
				p := path.Join(prefix, d.Name())
				if !validPath(p) {
					return ErrPath
				}
				if d.IsDir() {
					child, err := openDirectory(dir, d.Name(), false)
					if err != nil {
						return err
					}
					err = walk(child, p)
					child.Close()
					if err != nil {
						return err
					}
					continue
				}
				if !d.Type().IsRegular() {
					return fmt.Errorf("%w: %s (only regular files can be synchronized)", ErrPath, p)
				}
				if len(out) >= s.MaxFiles {
					return ErrLimit
				}
				unlock := s.lock(s.key(m, p))
				entry, err := digestFileContext(ctx, dir, d.Name(), min(s.MaxBytes, s.MaxTotalBytes-total))
				unlock()
				if errors.Is(err, fs.ErrNotExist) {
					continue
				}
				if checkpoint && errors.Is(err, ErrSourceBusy) {
					continue // a later checkpoint or the mandatory final push captures it
				}
				if err != nil {
					return fmt.Errorf("snapshot %s: %w", p, err)
				}
				entry.Path = p
				total += entry.Size
				out = append(out, entry)
			}
		}
		return nil
	}
	err = walk(r, "")
	return out, err
}

// Read pins the version before sending any bytes. The immutable snapshot also
// prevents in-place local writes from corrupting an otherwise valid download.
func (s *Store) Read(m Mount, p, expected string, w io.Writer) (Entry, error) {
	return s.ReadContext(context.Background(), m, p, expected, w)
}

func (s *Store) ReadContext(ctx context.Context, m Mount, p, expected string, w io.Writer) (Entry, error) {
	if !validPath(p) {
		return Entry{}, ErrPath
	}
	unlock := s.lock(s.key(m, p))
	defer unlock()
	r, name, err := openParent(m, p, false)
	if err != nil {
		return Entry{}, err
	}
	defer r.Close()
	snapshot, err := os.CreateTemp("", "artex-download-*")
	if err != nil {
		return Entry{}, err
	}
	defer func() { snapshot.Close(); os.Remove(snapshot.Name()) }()
	entry, err := snapshotFile(ctx, r, name, snapshot, s.MaxBytes)
	if err != nil {
		return Entry{}, err
	}
	entry.Path = p
	if expected != "" && entry.Digest != expected {
		return Entry{}, fmt.Errorf("%w: %s changed after manifest", ErrSourceBusy, p)
	}
	if _, err = snapshot.Seek(0, io.SeekStart); err != nil {
		return Entry{}, err
	}
	_, err = io.Copy(w, contextReader{ctx, snapshot})
	return entry, err
}

type contextReader struct {
	ctx context.Context
	r   io.Reader
}

func (r contextReader) Read(b []byte) (int, error) {
	if e := r.ctx.Err(); e != nil {
		return 0, e
	}
	return r.r.Read(b)
}

// Put validates an entire upload before acquiring the publication lock. A failed
// upload, canceled context, stale lease, or digest mismatch never replaces data.
// authorize is rechecked under the lock immediately before commit to fence a
// runner whose execution was revoked while it was uploading.
func (s *Store) Put(ctx context.Context, m Mount, p, expected, digest string, mode uint32, owner, lease string, body io.Reader, commitGuard func(func() error) error) (Entry, error) {
	if !writable(m, p) || expected == "" || len(digest) != 64 || s.MaxBytes < 0 || s.MaxBytes == 1<<63-1 {
		return Entry{}, ErrPath
	}
	if _, e := hex.DecodeString(digest); e != nil {
		return Entry{}, ErrDigest
	}
	r, name, e := openParent(m, p, true)
	if e != nil {
		return Entry{}, e
	}
	defer r.Close()
	tmp := ".artex-transfer-" + randomID()
	f, e := r.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if e != nil {
		return Entry{}, e
	}
	defer r.Remove(tmp)
	h := sha256.New()
	n, e := io.Copy(io.MultiWriter(f, h), io.LimitReader(contextReader{ctx, body}, s.MaxBytes+1))
	if e == nil && n > s.MaxBytes {
		e = ErrLimit
	}
	if e == nil && hex.EncodeToString(h.Sum(nil)) != digest {
		e = ErrDigest
	}
	if e == nil {
		e = f.Sync()
	}
	if e == nil {
		e = f.Chmod(os.FileMode(mode) & 0777 &^ 0022)
	}
	ce := f.Close()
	if e == nil {
		e = ce
	}
	if e != nil {
		return Entry{}, e
	}
	unlock := s.lock(s.key(m, p))
	defer unlock()
	if e = ctx.Err(); e != nil {
		return Entry{}, e
	}
	if e = s.checkLease(s.key(m, p), owner, lease); e != nil {
		return Entry{}, e
	}
	if e = sameParent(m, p, r); e != nil {
		return Entry{}, e
	}
	current, e := digestFileContext(ctx, r, name, s.MaxBytes)
	current.Path = p
	if errors.Is(e, fs.ErrNotExist) {
		current.Digest = Missing
		e = nil
	}
	if e != nil {
		return Entry{}, e
	}
	if current.Digest == digest {
		if commitGuard != nil {
			if e = commitGuard(func() error { return ctx.Err() }); e != nil {
				return Entry{}, e
			}
		}
		return current, ctx.Err()
	} // idempotent retry after a lost response
	if current.Digest != expected {
		return Entry{}, ErrConflict
	}
	commit := func() error {
		if err := ctx.Err(); err != nil {
			return err
		}
		return r.Rename(tmp, name)
	}
	if commitGuard != nil {
		e = commitGuard(commit)
	} else {
		e = commit()
	}
	if e != nil {
		return Entry{}, e
	}
	if dir, e := r.Open("."); e == nil {
		_ = dir.Sync()
		_ = dir.Close()
	}
	return Entry{p, digest, n, uint32(os.FileMode(mode) & 0777 &^ 0022)}, nil
}
