package runnerfiles

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func response(body string) *http.Response {
	return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}
}
func entry(p, body string) Entry {
	return Entry{Path: p, Digest: digest(body), Size: int64(len(body)), Mode: 0644}
}
func fileServer(t *testing.T, s *Store, m Mount, guard CommitGuard) *Client {
	t.Helper()
	h := Handler(s, func(r *http.Request, session string, _ bool) (Plan, error) {
		if r.Header.Get("Authorization") != "Bearer fixture-credential" || session != "allowed" {
			return Plan{}, ErrPath
		}
		return Plan{Mounts: []Mount{m}}, nil
	}, guard)
	mux := http.NewServeMux()
	mux.Handle("/runner/v1/files/", http.StripPrefix("/runner/v1/files", h))
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return &Client{BaseURL: srv.URL, Credential: "fixture-credential", HTTP: srv.Client()}
}

func TestHTTPFileAuthorizationAndCommitFencing(t *testing.T) {
	s, m := fixture(t)
	writeFixture(t, m.Source, "result", "old")
	var revoked atomic.Bool
	client := fileServer(t, s, m, func(_ *http.Request, session string, f func() error) error {
		if session != "allowed" || revoked.Load() {
			return ErrPath
		}
		return f()
	})
	if _, err := client.Manifest(t.Context(), "other-execution", m.Name); !errors.Is(err, ErrPath) {
		t.Fatalf("foreign session allowed: %v", err)
	}
	if _, err := client.Manifest(t.Context(), "allowed", "unknown-mount"); !errors.Is(err, ErrPath) {
		t.Fatalf("foreign mount allowed: %v", err)
	}
	unauthorized := *client
	unauthorized.Credential = "incorrect"
	if _, err := unauthorized.Manifest(t.Context(), "allowed", m.Name); !errors.Is(err, ErrPath) {
		t.Fatalf("bad credential allowed: %v", err)
	}
	local := t.TempDir()
	writeFixture(t, local, "result", "new")
	revoked.Store(true)
	if err := client.Upload(t.Context(), "allowed", m.Name, local, entry("result", "new"), digest("old")); !errors.Is(err, ErrPath) {
		t.Fatalf("revoked execution committed: %v", err)
	}
	assertContents(t, m.Source, "result", "old")
	revoked.Store(false)
	if err := client.Upload(t.Context(), "allowed", m.Name, local, entry("result", "new"), digest("old")); err != nil {
		t.Fatal(err)
	}
	// A lost successful response can be retried with the original precondition.
	if err := client.Upload(t.Context(), "allowed", m.Name, local, entry("result", "new"), digest("old")); err != nil {
		t.Fatalf("idempotent retry: %v", err)
	}
	assertContents(t, m.Source, "result", "new")
}

func TestDownloadRejectsCorruptInterruptedOrCanceledData(t *testing.T) {
	for _, kind := range []string{"digest", "short-body", "interrupted", "canceled"} {
		t.Run(kind, func(t *testing.T) {
			local := t.TempDir()
			writeFixture(t, local, "result", "old")
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			client := &Client{BaseURL: "http://controller.invalid", HTTP: &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
				switch kind {
				case "digest":
					return response("bad!"), nil
				case "short-body":
					return response("n"), nil
				case "interrupted":
					return &http.Response{StatusCode: 200, Body: io.NopCloser(readerFunc(func(p []byte) (int, error) { return copy(p, "ne"), io.ErrUnexpectedEOF }))}, nil
				default:
					return &http.Response{StatusCode: 200, Body: io.NopCloser(readerFunc(func(p []byte) (int, error) { cancel(); return copy(p, "new!"), io.EOF }))}, nil
				}
			})}}
			if err := client.Download(ctx, "allowed", "data", entry("result", "new!"), local); err == nil {
				t.Fatal("invalid download published")
			}
			assertContents(t, local, "result", "old")
			assertNoTemps(t, local)
		})
	}
}

func TestDownloadRejectsLocalWriteAndParentSwapDuringTransfer(t *testing.T) {
	for _, swap := range []bool{false, true} {
		t.Run(map[bool]string{false: "local-write", true: "parent-symlink"}[swap], func(t *testing.T) {
			local := t.TempDir()
			writeFixture(t, local, "own/result", "old")
			writeFixture(t, local, "peer/result", "peer")
			client := &Client{BaseURL: "http://controller.invalid", HTTP: &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
				if swap {
					if err := os.Rename(filepath.Join(local, "own"), filepath.Join(local, "previous")); err != nil {
						t.Fatal(err)
					}
					if err := os.Symlink("peer", filepath.Join(local, "own")); err != nil {
						t.Fatal(err)
					}
				} else {
					writeFixture(t, local, "own/result", "local-update")
				}
				return response("new"), nil
			})}}
			err := client.Download(t.Context(), "allowed", "data", entry("own/result", "new"), local)
			if !errors.Is(err, ErrConflict) && !errors.Is(err, ErrPath) {
				t.Fatalf("concurrent replacement accepted: %v", err)
			}
			if swap {
				assertContents(t, local, "peer/result", "peer")
				assertContents(t, local, "previous/result", "old")
			} else {
				assertContents(t, local, "own/result", "local-update")
			}
			assertNoTemps(t, local)
		})
	}
}

func TestUploadVerifiesSnapshotBeforeNetworkAndPinsBytes(t *testing.T) {
	for _, changedBefore := range []bool{false, true} {
		t.Run(map[bool]string{false: "change-after-snapshot", true: "changed-since-manifest"}[changedBefore], func(t *testing.T) {
			local := t.TempDir()
			writeFixture(t, local, "result", "version1")
			expected := entry("result", "version1")
			if changedBefore {
				writeFixture(t, local, "result", "version2")
			}
			calls := 0
			client := &Client{BaseURL: "http://controller.invalid", HTTP: &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
				calls++
				writeFixture(t, local, "result", "version2")
				body, err := io.ReadAll(req.Body)
				if err != nil {
					t.Fatal(err)
				}
				if string(body) != "version1" || req.Header.Get("X-Content-SHA256") != digest(string(body)) {
					t.Errorf("sent changed/corrupt blob: %q", body)
				}
				raw, _ := json.Marshal(expected)
				return response(string(raw)), nil
			})}}
			err := client.Upload(t.Context(), "allowed", "data", local, expected, Missing)
			if changedBefore {
				if !errors.Is(err, ErrConflict) || calls != 0 {
					t.Fatalf("unstable file transmitted: calls=%d err=%v", calls, err)
				}
			} else if err != nil || calls != 1 {
				t.Fatalf("stable snapshot failed: calls=%d err=%v", calls, err)
			}
		})
	}
}

func TestClientRejectsMaliciousManifestAndUncertainUploadResponse(t *testing.T) {
	cases := []Entry{entry("../escape", "x"), entry(".artex-transfer-dir/file", "x"), {Path: "ok", Size: 1, Digest: strings.Repeat("z", 64)}}
	for _, item := range cases {
		raw, _ := json.Marshal([]Entry{item})
		client := &Client{BaseURL: "http://controller.invalid", HTTP: &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) { return response(string(raw)), nil })}}
		if _, err := client.Manifest(t.Context(), "allowed", "data"); err == nil {
			t.Fatalf("invalid manifest accepted: %+v", item)
		}
	}
	local := t.TempDir()
	writeFixture(t, local, "result", "new")
	client := &Client{BaseURL: "http://controller.invalid", HTTP: &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) { return response(`{"path":"result"`), nil })}}
	if err := client.Upload(t.Context(), "allowed", "data", local, entry("result", "new"), Missing); err == nil {
		t.Fatal("truncated publication response treated as success")
	}
}

func TestSyncerReadOnlyRefreshPreservesOwnWritesAndPublishesArtifacts(t *testing.T) {
	store, m := fixture(t)
	m.Writable = []string{"own"}
	writeFixture(t, m.Source, "own/result", "original")
	writeFixture(t, m.Source, "peer/result", "peer-v1")
	client := fileServer(t, store, m, nil)
	local := t.TempDir()
	syncer := &Syncer{Client: client, Session: "allowed", Plan: Plan{Mounts: []Mount{m}}, Roots: map[string]string{m.Name: local}}
	if err := syncer.Pull(t.Context()); err != nil {
		t.Fatal(err)
	}
	writeFixture(t, local, "own/result", "local-v2")
	writeFixture(t, m.Source, "peer/result", "peer-v2")
	writeFixture(t, m.Source, "peer/new", "peer artifact")
	if err := syncer.PullReadOnly(t.Context()); err != nil {
		t.Fatal(err)
	}
	assertContents(t, local, "own/result", "local-v2")
	assertContents(t, local, "peer/result", "peer-v2")
	assertContents(t, local, "peer/new", "peer artifact")
	if err := syncer.Push(t.Context()); err != nil {
		t.Fatal(err)
	}
	assertContents(t, m.Source, "own/result", "local-v2")
	// Deletions are deliberately not propagated to another execution's cache.
	if err := os.Remove(filepath.Join(m.Source, "peer/new")); err != nil {
		t.Fatal(err)
	}
	if err := syncer.PullReadOnly(t.Context()); err != nil {
		t.Fatal(err)
	}
	assertContents(t, local, "peer/new", "peer artifact")
}

func TestSyncersRejectLostUpdatesAndUntrackedLocalCollisions(t *testing.T) {
	store, m := fixture(t)
	writeFixture(t, m.Source, "result", "base")
	client := fileServer(t, store, m, nil)
	makeSyncer := func() *Syncer {
		return &Syncer{Client: client, Session: "allowed", Plan: Plan{Mounts: []Mount{m}}, Roots: map[string]string{m.Name: t.TempDir()}}
	}
	a, b := makeSyncer(), makeSyncer()
	for _, s := range []*Syncer{a, b} {
		if err := s.Pull(t.Context()); err != nil {
			t.Fatal(err)
		}
	}
	writeFixture(t, a.Roots[m.Name], "result", "writer-a")
	writeFixture(t, b.Roots[m.Name], "result", "writer-b")
	results := make(chan error, 2)
	start := make(chan struct{})
	for _, s := range []*Syncer{a, b} {
		go func(s *Syncer) { <-start; results <- s.Push(t.Context()) }(s)
	}
	close(start)
	success, conflict := 0, 0
	for range 2 {
		err := <-results
		if err == nil {
			success++
		} else if errors.Is(err, ErrConflict) {
			conflict++
		} else {
			t.Fatal(err)
		}
	}
	if success != 1 || conflict != 1 {
		t.Fatalf("lost update: success=%d conflict=%d", success, conflict)
	}
	c := makeSyncer()
	writeFixture(t, c.Roots[m.Name], "result", "untracked-local")
	if err := c.Pull(t.Context()); !errors.Is(err, ErrConflict) {
		t.Fatalf("untracked local file overwritten: %v", err)
	}
	assertContents(t, c.Roots[m.Name], "result", "untracked-local")
}
