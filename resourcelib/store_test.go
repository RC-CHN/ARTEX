package resourcelib

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/RC-CHN/ARTEX/db"
	"github.com/RC-CHN/ARTEX/internal/testpg"
)

func TestMain(m *testing.M) { os.Exit(testpg.Run(m, "resourcelib")) }
func fixture(t *testing.T) (*Store, Actor) {
	t.Helper()
	dsn, _, err := db.DSN()
	if err != nil {
		t.Skip(err)
	}
	pg, err := db.Open(dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { pg.Close() })
	var exp, task int64
	if err = pg.QueryRow(`INSERT INTO explorations(goal) VALUES('synthetic resource tests') RETURNING id`).Scan(&exp); err != nil {
		t.Fatal(err)
	}
	if err = pg.QueryRow(`INSERT INTO tasks(description,goal,exploration_id) VALUES('synthetic','synthetic',$1) RETURNING id`, exp).Scan(&task); err != nil {
		t.Fatal(err)
	}
	return &Store{DB: pg.DB, Root: t.TempDir(), Key: []byte(strings.Repeat("k", 32))}, Actor{TaskID: task, ThreadID: "synthetic", RunnerID: "fixture"}
}
func resource(t *testing.T, s *Store, a Actor) Resource {
	t.Helper()
	r := Resource{Name: "Synthetic", Kind: "file", Description: "Fixture only", AccessMode: "global", Enabled: true}
	if e := s.Create(t.Context(), &r, a); e != nil {
		t.Fatal(e)
	}
	return r
}
func draft(t *testing.T, s *Store, r Resource, a Actor) Draft {
	t.Helper()
	d, e := s.NewDraft(t.Context(), r.ID, 0, a)
	if e != nil {
		t.Fatal(e)
	}
	return d
}
func publish(t *testing.T, s *Store, d Draft, a Actor) Version {
	t.Helper()
	v, e := s.Publish(t.Context(), d.ID, d.Revision, "fixture change", "synthetic test", a)
	if e != nil {
		t.Fatal(e)
	}
	return v
}
func upload(t *testing.T, s *Store, d Draft, p, content string, a Actor) Draft {
	t.Helper()
	v, e := s.Upload(t.Context(), d.ID, d.Revision, p, true, strings.NewReader(content), a)
	if e != nil {
		t.Fatal(e)
	}
	return v
}
func TestResourceTextFilesEndpointsAndPinnedVersions(t *testing.T) {
	s, a := fixture(t)
	r := resource(t, s, a)
	d := draft(t, s, r, a)
	// Text-only publication is a first-class resource.
	v1 := publish(t, s, d, a)
	if len(v1.Files) != 1 || v1.Files[0].Path != DocumentName {
		t.Fatal("missing versioned entry document")
	}
	if _, e := s.Pin(t.Context(), a.TaskID, r.ID, v1.ID, false); e != nil {
		t.Fatal(e)
	}
	d = draft(t, s, r, a)
	d = upload(t, s, d, "bin/linux-amd64/check", "synthetic binary", a)
	d = upload(t, s, d, "references/guide.md", "guide", a)
	secret := "fixture-secret-not-in-results"
	var e error
	d, e = s.Configure(t.Context(), d.ID, d.Revision, []Endpoint{{Name: "one", URL: "ssh://example.invalid:22"}, {Name: "two", URL: "tcp://example.invalid:4000"}}, "", "", map[string]string{"one": secret}, a)
	if e != nil {
		t.Fatal(e)
	}
	v2 := publish(t, s, d, a)
	raw, _ := json.Marshal(v2)
	if strings.Contains(string(raw), secret) || len(v2.Files) != 3 || len(v2.Endpoints) != 2 || !v2.Endpoints[0].HasCredential {
		t.Fatal("mixed resource manifest or credential disclosure")
	}
	pin, e := s.Pin(t.Context(), a.TaskID, r.ID, 0, false)
	if e != nil || pin.ID != v1.ID {
		t.Fatal("existing task silently upgraded")
	}
	if _, e = s.Pin(t.Context(), a.TaskID, r.ID, v2.ID, false); !errors.Is(e, ErrConflict) {
		t.Fatal("agent silently changed fixed version")
	}
	if _, e = s.Credentials(t.Context(), a.TaskID, v2.ID); e == nil {
		t.Fatal("delivered unpinned credentials")
	}
	if _, e = s.Pin(t.Context(), a.TaskID, r.ID, v2.ID, true); e != nil {
		t.Fatal(e)
	}
	secrets, e := s.Credentials(t.Context(), a.TaskID, v2.ID)
	if e != nil || secrets["one"] != secret {
		t.Fatal("credential delivery failed")
	}
	var encrypted []byte
	if e = s.DB.QueryRow(`SELECT secret_cipher FROM owned_resource_versions WHERE id=$1`, v2.ID).Scan(&encrypted); e != nil || strings.Contains(string(encrypted), secret) {
		t.Fatal("credential stored in plaintext")
	}
	originalKey := s.Key
	s.Key = []byte(strings.Repeat("x", 32))
	if _, e = s.Credentials(t.Context(), a.TaskID, v2.ID); e == nil {
		t.Fatal("wrong key accepted")
	}
	s.Key = originalKey
	root := t.TempDir()
	p, e := s.Materialize(t.Context(), v2, root)
	if e != nil {
		t.Fatal(e)
	}
	file := filepath.Join(p, "bin/linux-amd64/check")
	if e = os.WriteFile(file, []byte("task edits"), 0644); e != nil {
		t.Fatal(e)
	}
	source, e := os.ReadFile(filepath.Join(s.VersionPath(v2), "bin/linux-amd64/check"))
	if e != nil || string(source) != "synthetic binary" {
		t.Fatal("task modification mutated global version")
	}
	d = draft(t, s, r, a)
	d = upload(t, s, d, DocumentName, "---\nname: Updated\ndescription: New summary\ntags: [new]\n---\n\n# Documentation v3\n", a)
	v3 := publish(t, s, d, a)
	got, e := s.Get(t.Context(), r.ID)
	if e != nil || got.Name != "Updated" {
		t.Fatal("document index not published atomically")
	}
	old, e := s.ReadDocument(t.Context(), v1.Manifest)
	if e != nil || strings.Contains(old, "Updated") {
		t.Fatal("old document changed")
	}
	current, _ := s.ReadDocument(t.Context(), v3.Manifest)
	if !strings.Contains(current, "Documentation v3") {
		t.Fatal("document missing")
	}
	if e = s.Observe(t.Context(), r.ID, v3.ID, "unreachable", "only runner A", a); e != nil {
		t.Fatal(e)
	}
	got, _ = s.Get(t.Context(), r.ID)
	if !got.Enabled {
		t.Fatal("one runner observation disabled global resource")
	}
	events, e := s.Events(t.Context(), r.ID, 0)
	if e != nil || len(events) == 0 || events[0].Actor.RunnerID != "fixture" {
		t.Fatal("missing provenance")
	}
}
func TestConcurrentDraftEditsAndPublicationCAS(t *testing.T) {
	s, a := fixture(t)
	r := resource(t, s, a)
	d := draft(t, s, r, a)
	var wg sync.WaitGroup
	wins := make(chan Draft, 12)
	for i := range 12 {
		wg.Go(func() {
			out, e := s.Upload(t.Context(), d.ID, d.Revision, fmt.Sprintf("file-%d", i), false, strings.NewReader("unique"), a)
			if e == nil {
				wins <- out
			} else if !errors.Is(e, ErrConflict) {
				t.Error(e)
			}
		})
	}
	wg.Wait()
	close(wins)
	if len(wins) != 1 {
		t.Fatalf("CAS winners=%d", len(wins))
	}
	d = <-wins
	publish(t, s, d, a)
	drafts := []Draft{draft(t, s, r, a), draft(t, s, r, a)}
	published := make(chan Version, 2)
	conflicts := make(chan error, 2)
	for _, d := range drafts {
		wg.Go(func() {
			v, e := s.Publish(t.Context(), d.ID, d.Revision, "parallel", "test", a)
			if e == nil {
				published <- v
			} else {
				conflicts <- e
			}
		})
	}
	wg.Wait()
	if len(published) != 1 || len(conflicts) != 1 {
		t.Fatalf("publication winners=%d conflicts=%d", len(published), len(conflicts))
	}
	if e := <-conflicts; !errors.Is(e, ErrConflict) {
		t.Fatal(e)
	}
	v := <-published
	for _, d := range drafts {
		got, _ := s.Draft(t.Context(), d.ID)
		if got.State == "published" {
			again, e := s.Publish(t.Context(), d.ID, d.Revision, "parallel", "test", a)
			if e != nil || again.ID != v.ID {
				t.Fatal("duplicate publication did not return same version")
			}
		} else if got.State != "editing" {
			t.Fatal("conflicting draft lost")
		}
	}
}
func TestResourceAuthorizationRevocationAndLockCancellation(t *testing.T) {
	s, a := fixture(t)
	r := resource(t, s, Actor{})
	r.AccessMode = "restricted"
	if e := s.Update(t.Context(), &r, Actor{}); e != nil {
		t.Fatal(e)
	}
	d := draft(t, s, r, Actor{})
	v := publish(t, s, d, Actor{})
	if _, e := s.NewDraft(t.Context(), r.ID, 0, a); !errors.Is(e, ErrUnavailable) {
		t.Fatal("ungranted agent edited restricted resource")
	}
	if _, e := s.Pin(t.Context(), a.TaskID, r.ID, 0, false); !errors.Is(e, ErrUnavailable) {
		t.Fatal("ungranted restricted pin")
	}
	if _, e := s.Pin(t.Context(), a.TaskID, r.ID, v.ID, true); e != nil {
		t.Fatal(e)
	}
	d = draft(t, s, r, a)
	if _, e := s.DB.Exec(`DELETE FROM task_owned_resources WHERE task_id=$1 AND resource_id=$2`, a.TaskID, r.ID); e != nil {
		t.Fatal(e)
	}
	if _, e := s.Upload(t.Context(), d.ID, d.Revision, "test", false, strings.NewReader("data"), a); !errors.Is(e, ErrUnavailable) {
		t.Fatal("revoked agent modified draft")
	}
	tx, e := s.DB.Begin()
	if e != nil {
		t.Fatal(e)
	}
	defer tx.Rollback()
	if _, e = tx.Exec(`SELECT pg_advisory_xact_lock(7337741010)`); e != nil {
		t.Fatal(e)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 40*time.Millisecond)
	defer cancel()
	if _, e = s.NewDraft(ctx, r.ID, 0, Actor{}); e == nil {
		t.Fatal("cancelled lock wait succeeded")
	}
	tx.Rollback()
	r, _ = s.Get(t.Context(), r.ID)
	r.Enabled = false
	if e = s.Update(t.Context(), &r, Actor{}); e != nil {
		t.Fatal(e)
	}
	if _, e = s.Pin(t.Context(), a.TaskID, r.ID, v.ID, true); !errors.Is(e, ErrUnavailable) {
		t.Fatal("disabled resource delivered")
	}
}

type brokenReader struct{}

func (brokenReader) Read([]byte) (int, error) { return 0, io.ErrUnexpectedEOF }
func TestInterruptedUploadsConflictsAndQuotaLeaveDraftIntact(t *testing.T) {
	s, a := fixture(t)
	r := resource(t, s, a)
	d := draft(t, s, r, a)
	if _, e := s.Upload(t.Context(), d.ID, d.Revision, "broken", false, brokenReader{}, a); e == nil {
		t.Fatal("broken upload accepted")
	}
	got, _ := s.Draft(t.Context(), d.ID)
	if got.Revision != d.Revision || len(got.Files) != 1 {
		t.Fatal("interrupted upload partially committed")
	}
	entries, _ := os.ReadDir(filepath.Join(s.Root, "staging"))
	if len(entries) != 0 {
		t.Fatal("staging files leaked")
	}
	if _, e := s.Upload(t.Context(), d.ID, d.Revision, "../escape", false, strings.NewReader("x"), a); e == nil {
		t.Fatal("traversal accepted")
	}
	d = upload(t, s, d, "foo", "x", a)
	if _, e := s.Upload(t.Context(), d.ID, d.Revision, "foo/bar", false, strings.NewReader("y"), a); e == nil {
		t.Fatal("file/directory collision accepted")
	}
	if _, e := s.Upload(t.Context(), d.ID, d.Revision, DocumentName, false, strings.NewReader("no frontmatter"), a); e == nil {
		t.Fatal("invalid entry accepted")
	}
	if _, e := s.RemoveFile(t.Context(), d.ID, d.Revision, DocumentName, a); e == nil {
		t.Fatal("required entry deleted")
	}
	for range 15 {
		draft(t, s, r, a)
	}
	if _, e := s.NewDraft(t.Context(), r.ID, 0, a); !errors.Is(e, ErrLimit) {
		t.Fatal("active draft quota not enforced")
	}
	if e := s.Discard(t.Context(), d.ID, d.Revision, a); e != nil {
		t.Fatal(e)
	}
	draft(t, s, r, a)
}
func TestResourceFilesystemConfinement(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	if e := os.WriteFile(filepath.Join(outside, "secret"), []byte("never read"), 0600); e != nil {
		t.Fatal(e)
	}
	if e := os.Symlink(outside, filepath.Join(root, "link")); e != nil {
		t.Fatal(e)
	}
	for _, p := range []string{"link/secret", "../secret", "/etc/passwd", "a/../secret"} {
		if f, e := OpenTaskFile(root, p); e == nil {
			f.Close()
			t.Fatalf("accepted %q", p)
		}
	}
	if e := os.Symlink(outside, filepath.Join(root, "shared")); e != nil {
		t.Fatal(e)
	}
	v := Version{ID: 1}
	if _, e := WriteCredential(t.Context(), root, v, "x", []byte("secret")); e == nil {
		t.Fatal("credential escaped task")
	}
	entries, _ := os.ReadDir(outside)
	if len(entries) != 1 {
		t.Fatal("wrote outside task")
	}
}
func TestManifestLimitsAndEndpointValidation(t *testing.T) {
	for _, u := range []string{"ssh://user:secret@host", "https://host/?token=secret", "file:///etc/passwd", "https://host/#secret"} {
		if _, e := validateManifest(Manifest{Endpoints: []Endpoint{{Name: "x", URL: u}}}); e == nil {
			t.Fatalf("accepted endpoint %q", u)
		}
	}
	files := make([]File, MaxFiles+1)
	if _, e := validateManifest(Manifest{Files: files}); !errors.Is(e, ErrLimit) {
		t.Fatal("file count quota")
	}
	if _, e := ParseDocument([]byte(strings.Repeat("x", MaxDocumentBytes+1))); e == nil {
		t.Fatal("entry size limit")
	}
}

func TestResourceMaintenanceKeepsPublishedAndActiveDraftFiles(t *testing.T) {
	s, a := fixture(t)
	r := resource(t, s, a)
	d := draft(t, s, r, a)
	d = upload(t, s, d, "payload", "old", a)
	v := publish(t, s, d, a)
	d = draft(t, s, r, a)
	d = upload(t, s, d, "payload", "replacement", a)
	d = upload(t, s, d, "unused", "discard me", a)
	unused := d.Files[len(d.Files)-1] // Find by path; sorted order is not part of storage semantics.
	for _, f := range d.Files {
		if f.Path == "unused" {
			unused = f
		}
	}
	d, e := s.RemoveFile(t.Context(), d.ID, d.Revision, "unused", a)
	if e != nil {
		t.Fatal(e)
	}
	old := time.Now().Add(-48 * time.Hour)
	if e = os.Chtimes(s.BlobPath(unused), old, old); e != nil {
		t.Fatal(e)
	}
	n, e := s.Sweep(t.Context(), time.Now().Add(-24*time.Hour))
	if e != nil || n < 1 {
		t.Fatalf("cleanup: %d %v", n, e)
	}
	if _, e = os.Stat(s.BlobPath(unused)); !os.IsNotExist(e) {
		t.Fatal("unreferenced file retained")
	}
	for _, f := range append(v.Files, d.Files...) {
		if _, e = os.Stat(s.BlobPath(f)); e != nil {
			t.Fatalf("referenced file removed: %v", e)
		}
	}
}
