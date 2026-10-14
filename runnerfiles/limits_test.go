package runnerfiles

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSnapshotScanLimitsBoundFilesBytesAndEmptyDirectories(t *testing.T) {
	for _, kind := range []string{"files", "bytes", "directories", "temporary", "depth"} {
		t.Run(kind, func(t *testing.T) {
			s, m := fixture(t)
			switch kind {
			case "files":
				s.MaxFiles = 2
				for i := range 3 {
					writeFixture(t, m.Source, fmt.Sprint(i), "x")
				}
			case "bytes":
				s.MaxTotalBytes = 3
				writeFixture(t, m.Source, "a", "xx")
				writeFixture(t, m.Source, "b", "xx")
			case "directories":
				s.MaxEntries = 2
				for i := range 3 {
					os.Mkdir(filepath.Join(m.Source, fmt.Sprint(i)), 0700)
				}
			case "temporary":
				s.MaxEntries = 2
				for i := range 3 {
					writeFixture(t, m.Source, fmt.Sprintf(".artex-transfer-%d", i), "")
				}
			case "depth":
				os.MkdirAll(filepath.Join(m.Source, strings.Repeat("d/", 66)), 0700)
			}
			if _, err := s.Manifest(t.Context(), m); !errors.Is(err, ErrLimit) {
				t.Fatalf("limit not enforced: %v", err)
			}
		})
	}
}

func TestSnapshotBudgetRejectsAggregateBeforeAnyDownload(t *testing.T) {
	for _, kind := range []string{"bytes", "files"} {
		t.Run(kind, func(t *testing.T) {
			requests := 0
			client := &Client{BaseURL: "http://fixture", HTTP: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
				if !strings.HasSuffix(r.URL.Path, "/manifest") {
					requests++
					return nil, fmt.Errorf("unexpected download")
				}
				entries := []Entry{entry("large", "")}
				entries[0].Size = MaxSnapshotBytes/2 + 1
				if kind == "files" {
					entries = nil
					for i := range MaxSnapshotFiles/2 + 1 {
						entries = append(entries, entry(fmt.Sprint(i), ""))
					}
				}
				raw, _ := json.Marshal(entries)
				return response(string(raw)), nil
			})}}
			syncer := &Syncer{Client: client, Plan: Plan{Mounts: []Mount{{Name: "a"}, {Name: "b"}}}, Roots: map[string]string{"a": t.TempDir(), "b": t.TempDir()}}
			if err := syncer.Pull(t.Context()); !errors.Is(err, ErrLimit) {
				t.Fatalf("expected bounded preparation: %v", err)
			}
			if requests != 0 {
				t.Fatal("download began before complete preflight")
			}
		})
	}
}

func TestSharedPlanRejectsAllSnapshotEndpoints(t *testing.T) {
	s := NewStore()
	root := t.TempDir()
	writeFixture(t, root, "keep", "original")
	h := Handler(s, func(*http.Request, string, bool) (Plan, error) {
		return Plan{Mode: SharedMode, Mounts: []Mount{{Name: "task", Source: root, Writable: []string{"."}}}}, nil
	}, nil)
	for _, request := range []struct{ method, path string }{{"GET", "manifest"}, {"GET", "blob"}, {"PUT", "blob"}, {"POST", "lock"}, {"DELETE", "lock"}} {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest(request.method, "/"+request.path+"?session=s&mount=task&path=keep", strings.NewReader("replacement")))
		if w.Code != http.StatusForbidden {
			t.Errorf("%s %s allowed: %d", request.method, request.path, w.Code)
		}
	}
	assertContents(t, root, "keep", "original")
}
