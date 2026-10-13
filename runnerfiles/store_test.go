package runnerfiles

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func digest(s string) string { sum := sha256.Sum256([]byte(s)); return hex.EncodeToString(sum[:]) }
func fixture(t *testing.T) (*Store, Mount) {
	t.Helper()
	return NewStore(), Mount{Name: "data", Source: t.TempDir(), Target: "/data", Writable: []string{"."}}
}
func writeFixture(t *testing.T, root, p, contents string) {
	t.Helper()
	f := filepath.Join(root, filepath.FromSlash(p))
	if err := os.MkdirAll(filepath.Dir(f), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(f, []byte(contents), 0644); err != nil {
		t.Fatal(err)
	}
}
func assertContents(t *testing.T, root, p, want string) {
	t.Helper()
	got, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(p)))
	if err != nil || string(got) != want {
		t.Fatalf("%s=%q err=%v want=%q", p, got, err, want)
	}
}
func put(t *testing.T, s *Store, m Mount, p, old, body string) (Entry, error) {
	t.Helper()
	return s.Put(t.Context(), m, p, old, digest(body), 0666, "run", "", strings.NewReader(body), nil)
}

type readerFunc func([]byte) (int, error)

func (r readerFunc) Read(p []byte) (int, error) { return r(p) }

type writerFunc func([]byte) (int, error)

func (w writerFunc) Write(p []byte) (int, error) { return w(p) }
func assertNoTemps(t *testing.T, root string) {
	t.Helper()
	err := filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if strings.HasPrefix(d.Name(), ".artex-transfer-") {
			t.Errorf("leaked upload %s", p)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestStoreConcurrentCASAndIdempotentRetry(t *testing.T) {
	for _, same := range []bool{false, true} {
		t.Run(map[bool]string{false: "competing", true: "duplicate"}[same], func(t *testing.T) {
			s, m := fixture(t)
			writeFixture(t, m.Source, "result", "old")
			ready := make(chan struct{}, 2)
			release := make(chan struct{})
			outcomes := make(chan error, 2)
			for i := range 2 {
				go func(i int) {
					body := []string{"new-a", "new-b"}[i]
					if same {
						body = "same"
					}
					once := sync.Once{}
					reader := readerFunc(func(p []byte) (int, error) {
						once.Do(func() { ready <- struct{}{}; <-release })
						return copy(p, body), io.EOF
					})
					_, err := s.Put(t.Context(), m, "result", digest("old"), digest(body), 0644, "run", "", reader, nil)
					outcomes <- err
				}(i)
			}
			<-ready
			<-ready
			close(release)
			succeeded, conflicted := 0, 0
			for range 2 {
				err := <-outcomes
				switch {
				case err == nil:
					succeeded++
				case errors.Is(err, ErrConflict):
					conflicted++
				default:
					t.Fatalf("unexpected CAS result %v", err)
				}
			}
			if same && succeeded != 2 || !same && (succeeded != 1 || conflicted != 1) {
				t.Fatalf("success=%d conflict=%d", succeeded, conflicted)
			}
			entries, err := s.Manifest(t.Context(), m)
			if err != nil || len(entries) != 1 {
				t.Fatalf("manifest %v %v", entries, err)
			}
			body, _ := os.ReadFile(filepath.Join(m.Source, "result"))
			if entries[0].Digest != digest(string(body)) {
				t.Fatal("published torn bytes")
			}
			if entries[0].Mode != 0644 {
				t.Fatalf("mode=%o", entries[0].Mode)
			}
			assertNoTemps(t, m.Source)
		})
	}
}

func TestStoreFailedUploadNeverReplacesFile(t *testing.T) {
	for _, kind := range []string{"digest", "limit", "canceled", "stream-error", "revoked"} {
		t.Run(kind, func(t *testing.T) {
			s, m := fixture(t)
			writeFixture(t, m.Source, "result", "old")
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			body := "replacement"
			hash := digest(body)
			var reader io.Reader = strings.NewReader(body)
			var guard func(func() error) error
			want := ErrDigest
			switch kind {
			case "digest":
				hash = digest("different")
			case "limit":
				s.MaxBytes = 3
				want = ErrLimit
			case "canceled":
				want = context.Canceled
				reader = readerFunc(func(p []byte) (int, error) { cancel(); return copy(p, body), io.EOF })
			case "stream-error":
				want = io.ErrUnexpectedEOF
				reader = readerFunc(func(p []byte) (int, error) { return copy(p, "partial"), io.ErrUnexpectedEOF })
			case "revoked":
				want = ErrPath
				guard = func(func() error) error { return ErrPath }
			}
			_, err := s.Put(ctx, m, "result", digest("old"), hash, 0644, "run", "", reader, guard)
			if !errors.Is(err, want) {
				t.Fatalf("error=%v want=%v", err, want)
			}
			assertContents(t, m.Source, "result", "old")
			assertNoTemps(t, m.Source)
		})
	}
}

func TestStoreLeaseFencesExpiredTokensAndUnauthorizedRetries(t *testing.T) {
	s, m := fixture(t)
	now := time.Unix(100, 0)
	s.Now = func() time.Time { return now }
	writeFixture(t, m.Source, "result", "old")
	token, err := s.Lease(m, "result", "owner", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	_, err = put(t, s, m, "result", digest("old"), "new")
	if !errors.Is(err, ErrLocked) {
		t.Fatalf("unleased writer: %v", err)
	}
	upload := func(token string) error {
		_, err := s.Put(t.Context(), m, "result", digest("old"), digest("new"), 0644, "owner", token, strings.NewReader("new"), nil)
		return err
	}
	now = now.Add(time.Minute)
	if err = upload(token); !errors.Is(err, ErrLocked) {
		t.Fatalf("expired lease published: %v", err)
	}
	newer, err := s.Lease(m, "result", "new-owner", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Unlock(m, "result", "owner", token); !errors.Is(err, ErrLocked) {
		t.Fatalf("old owner unlocked new lease: %v", err)
	}
	if err = s.Unlock(m, "result", "new-owner", newer); err != nil {
		t.Fatal(err)
	}
	if _, err = put(t, s, m, "result", digest("old"), "new"); err != nil {
		t.Fatal(err)
	}
	called := false
	_, err = s.Put(t.Context(), m, "result", digest("old"), digest("new"), 0644, "run", "", strings.NewReader("new"), func(func() error) error { called = true; return ErrPath })
	if !called || !errors.Is(err, ErrPath) {
		t.Fatalf("idempotent retry bypassed fencing: called=%v err=%v", called, err)
	}
}

func TestStoreRejectsPathsAndSymlinkReplacementDuringUpload(t *testing.T) {
	s, m := fixture(t)
	for _, p := range []string{"", ".", "../escape", "/absolute", "a/../b", `a\b`, ".artex-transfer-hidden/result", "a/.artex-transfer-hidden"} {
		if _, err := put(t, s, m, p, Missing, "value"); !errors.Is(err, ErrPath) {
			t.Errorf("accepted %q: %v", p, err)
		}
	}
	m.Writable = []string{"own"}
	writeFixture(t, m.Source, "own/result", "old")
	writeFixture(t, m.Source, "peer/result", "peer")
	reader := readerFunc(func(p []byte) (int, error) {
		if err := os.Rename(filepath.Join(m.Source, "own"), filepath.Join(m.Source, "previous")); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink("peer", filepath.Join(m.Source, "own")); err != nil {
			t.Fatal(err)
		}
		return copy(p, "new"), io.EOF
	})
	_, err := s.Put(t.Context(), m, "own/result", digest("old"), digest("new"), 0644, "run", "", reader, nil)
	if !errors.Is(err, ErrPath) && !errors.Is(err, ErrConflict) {
		t.Fatalf("directory swap accepted: %v", err)
	}
	assertContents(t, m.Source, "peer/result", "peer")
	assertContents(t, m.Source, "previous/result", "old")
	if _, err = s.Read(m, "own/result", "", io.Discard); !errors.Is(err, ErrPath) {
		t.Fatalf("symlink read allowed: %v", err)
	}
	if _, err = s.Manifest(t.Context(), m); !errors.Is(err, ErrPath) {
		t.Fatalf("symlink manifest allowed: %v", err)
	}
	assertNoTemps(t, m.Source)
}

func TestSnapshotRejectsWritesAndAtomicReplacementDuringHash(t *testing.T) {
	for _, replace := range []bool{false, true} {
		t.Run(map[bool]string{false: "in-place", true: "replacement"}[replace], func(t *testing.T) {
			_, m := fixture(t)
			writeFixture(t, m.Source, "result", "old")
			root, name, err := openParent(m, "result", false)
			if err != nil {
				t.Fatal(err)
			}
			defer root.Close()
			writer := writerFunc(func(p []byte) (int, error) {
				if replace {
					writeFixture(t, m.Source, "new", "old")
					if err := os.Rename(filepath.Join(m.Source, "new"), filepath.Join(m.Source, "result")); err != nil {
						t.Fatal(err)
					}
				} else {
					writeFixture(t, m.Source, "result", "changed length")
				}
				return len(p), nil
			})
			if _, err := snapshotFile(t.Context(), root, name, writer, 100); !errors.Is(err, ErrConflict) {
				t.Fatalf("unstable snapshot accepted: %v", err)
			}
		})
	}
}

func TestStoreReadServesImmutableVersionDuringConcurrentPublication(t *testing.T) {
	s, m := fixture(t)
	writeFixture(t, m.Source, "result", "old")
	reading := make(chan struct{})
	release := make(chan struct{})
	readDone := make(chan error, 1)
	var received bytes.Buffer
	go func() {
		_, err := s.Read(m, "result", digest("old"), writerFunc(func(p []byte) (int, error) { close(reading); <-release; return received.Write(p) }))
		readDone <- err
	}()
	<-reading
	writeDone := make(chan error, 1)
	go func() { _, err := put(t, s, m, "result", digest("old"), "new"); writeDone <- err }()
	close(release)
	if err := <-readDone; err != nil {
		t.Fatal(err)
	}
	if err := <-writeDone; err != nil {
		t.Fatal(err)
	}
	if received.String() != "old" {
		t.Fatalf("download mixed versions: %q", received.String())
	}
	assertContents(t, m.Source, "result", "new")
	if _, err := s.Read(m, "result", digest("old"), io.Discard); !errors.Is(err, ErrConflict) {
		t.Fatalf("stale digest accepted: %v", err)
	}
}
