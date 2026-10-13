package runnerfiles

import (
	"bytes"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"testing"
)

func hardLink(t *testing.T, source, destination string) {
	t.Helper()
	if err := os.Link(source, destination); err != nil {
		t.Fatalf("create hard-link fixture: %v", err)
	}
}

func TestStoreRejectsHardLinks(t *testing.T) {
	s, m := fixture(t)
	outside := filepath.Join(t.TempDir(), "private")
	if err := os.WriteFile(outside, []byte("private bytes"), 0600); err != nil {
		t.Fatal(err)
	}
	hardLink(t, outside, filepath.Join(m.Source, "result"))
	if _, err := s.Manifest(t.Context(), m); !errors.Is(err, ErrPath) {
		t.Fatalf("hard link exposed in manifest: %v", err)
	}
	var received bytes.Buffer
	if _, err := s.Read(m, "result", "", &received); !errors.Is(err, ErrPath) || received.Len() != 0 {
		t.Fatalf("hard link exposed by read: error=%v bytes=%d", err, received.Len())
	}
	for _, body := range []string{"replacement", "private bytes"} {
		if _, err := put(t, s, m, "result", digest("private bytes"), body); !errors.Is(err, ErrPath) {
			t.Fatalf("hard-link destination accepted, including idempotent upload: %v", err)
		}
	}
	assertContents(t, m.Source, "result", "private bytes")
	assertNoTemps(t, m.Source)
}

func TestClientRejectsHardLinkedUploadAndDownloadTarget(t *testing.T) {
	local := t.TempDir()
	writeFixture(t, local, "result", "original")
	hardLink(t, filepath.Join(local, "result"), filepath.Join(local, "alias"))
	calls := 0
	client := &Client{BaseURL: "http://controller.invalid", HTTP: &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		calls++
		return response("unexpected network operation"), nil
	})}}
	if err := client.Upload(t.Context(), "session", "workspace", local, entry("result", "original"), Missing); !errors.Is(err, ErrPath) {
		t.Fatalf("hard-linked upload source accepted: %v", err)
	}
	if err := client.Download(t.Context(), "session", "workspace", entry("result", "new"), local); !errors.Is(err, ErrPath) {
		t.Fatalf("hard-linked download target accepted: %v", err)
	}
	if calls != 0 {
		t.Fatalf("hard-linked file reached network: calls=%d", calls)
	}
	assertContents(t, local, "result", "original")
}

func TestSnapshotRejectsHardLinkCreatedDuringHash(t *testing.T) {
	_, m := fixture(t)
	writeFixture(t, m.Source, "result", "original")
	root, name, err := openParent(m, "result", false)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	writer := writerFunc(func(p []byte) (int, error) {
		hardLink(t, filepath.Join(m.Source, "result"), filepath.Join(m.Source, "alias"))
		return len(p), nil
	})
	if _, err := snapshotFile(t.Context(), root, name, writer, 100); !errors.Is(err, ErrConflict) {
		t.Fatalf("new hard link did not invalidate snapshot: %v", err)
	}
}
