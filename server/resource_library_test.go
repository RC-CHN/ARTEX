package server

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/RC-CHN/ARTEX/agent"
	"github.com/RC-CHN/ARTEX/db"
	"github.com/RC-CHN/ARTEX/resourcelib"
	"github.com/RC-CHN/ARTEX/runner"
)

func libraryFixture(t *testing.T) (*Server, resourcelib.Resource, resourcelib.Version) {
	t.Helper()
	m := runnerTestManager(t)
	s := &Server{m: m, jwtKey: []byte(strings.Repeat("k", 32))}
	lib := s.resourceLibrary()
	r := resourcelib.Resource{Name: "resource fixture", Kind: "file", Description: "local synthetic resource", AccessMode: "global", Enabled: true}
	if e := lib.Create(t.Context(), &r, resourcelib.Actor{}); e != nil {
		t.Fatal(e)
	}
	d, e := lib.NewDraft(t.Context(), r.ID, 0, resourcelib.Actor{})
	if e != nil {
		t.Fatal(e)
	}
	v, e := lib.Publish(t.Context(), d.ID, d.Revision, "initial", "text-only test", resourcelib.Actor{})
	if e != nil {
		t.Fatal(e)
	}
	return s, r, v
}
func TestResourceTaskSelectionAtomicSnapshotAndReadonlyMount(t *testing.T) {
	s, r, v := libraryFixture(t)
	task, e := s.m.CreateTaskWithOptions("synthetic selection", "test", db.TaskCreateOptions{Resources: []db.TaskResourceSelection{{ResourceID: r.ID, VersionID: v.ID}}})
	if e != nil {
		t.Fatal(e)
	}
	taskID, _ := strconv.ParseInt(task.ID, 10, 64)
	before, e := s.taskResourceSnapshot(t.Context(), taskID)
	if e != nil || !strings.Contains(before, fmt.Sprintf(`"version_id":%d`, v.ID)) {
		t.Fatalf("startup snapshot missing: %q %v", before, e)
	}
	d, e := s.resourceLibrary().NewDraft(t.Context(), r.ID, 0, resourcelib.Actor{})
	if e != nil {
		t.Fatal(e)
	}
	d, e = s.resourceLibrary().Upload(t.Context(), d.ID, d.Revision, "RESOURCE.md", false, strings.NewReader("---\nname: later\ndescription: later version\n---\n\nnew content\n"), resourcelib.Actor{})
	if e != nil {
		t.Fatal(e)
	}
	if _, e = s.resourceLibrary().Publish(t.Context(), d.ID, d.Revision, "new", "test", resourcelib.Actor{}); e != nil {
		t.Fatal(e)
	}
	after, e := s.taskResourceSnapshot(t.Context(), taskID)
	if e != nil || before != after {
		t.Fatal("global publication changed running task prompt snapshot")
	}
	plan, e := s.runnerFilePlan(agent.RunInfo{TaskID: taskID}, filepath.Join(s.m.dir, "tasks", task.ID), nil)
	if e != nil {
		t.Fatal(e)
	}
	found := false
	for _, mount := range plan.Mounts {
		if mount.Name == fmt.Sprintf("resource-%d", v.ID) {
			found = true
			if len(mount.Writable) != 0 || mount.Source != s.resourceLibrary().VersionPath(v) {
				t.Fatal("resource mount is writable or not version scoped")
			}
		}
	}
	if !found {
		t.Fatal("selected version not mounted")
	}
	var count int
	s.m.pg.QueryRow(`SELECT count(*) FROM tasks`).Scan(&count)
	if _, e = s.m.CreateTaskWithOptions("should rollback", "fixture", db.TaskCreateOptions{Resources: []db.TaskResourceSelection{{ResourceID: r.ID}, {ResourceID: 9223372036854775800}}}); !errors.Is(e, db.ErrTaskResourceInvalid) {
		t.Fatalf("invalid selection accepted: %v", e)
	}
	var remaining int
	s.m.pg.QueryRow(`SELECT count(*) FROM tasks`).Scan(&remaining)
	if remaining != count {
		t.Fatal("failed resource binding left a task behind")
	}
}
func TestResourceAgentDraftImportAndSecretRedaction(t *testing.T) {
	s, r, _ := libraryFixture(t)
	task, e := s.m.CreateTask("synthetic agent", "test", nil, 0, 0)
	if e != nil {
		t.Fatal(e)
	}
	id, _ := strconv.ParseInt(task.ID, 10, 64)
	ctx := agent.WithRunInfo(t.Context(), agent.RunInfo{TaskID: id, IntentID: 123})
	root := filepath.Join(s.m.dir, "tasks", task.ID)
	if e = os.MkdirAll(root, 0755); e != nil {
		t.Fatal(e)
	}
	data := []byte("synthetic import")
	if e = os.WriteFile(filepath.Join(root, "input.txt"), data, 0644); e != nil {
		t.Fatal(e)
	}
	hash := sha256.Sum256(data)
	call := func(name string, a any) map[string]any {
		t.Helper()
		raw, _ := json.Marshal(a)
		for _, tool := range s.resourceTools() {
			if tool.Name() != name {
				continue
			}
			result, e := tool.Call(ctx, raw, nil)
			if e != nil || result.IsError {
				t.Fatalf("%s: %v %s", name, e, result.Flatten())
			}
			var out map[string]any
			if e = json.Unmarshal([]byte(result.Flatten()), &out); e != nil {
				t.Fatal(e)
			}
			return out
		}
		t.Fatalf("missing tool %s", name)
		return nil
	}
	d := call("resource_draft", map[string]any{"action": "create", "resource_id": r.ID})
	d = call("resource_draft", map[string]any{"action": "import_file", "draft_id": d["id"], "revision": d["revision"], "source_path": "input.txt", "path": "files/input.txt", "sha256": hex.EncodeToString(hash[:])})
	// A caller-computed digest prevents importing a stale controller copy.
	bad, _ := json.Marshal(map[string]any{"action": "import_file", "draft_id": d["id"], "revision": d["revision"], "source_path": "input.txt", "path": "files/input.txt", "sha256": strings.Repeat("0", 64)})
	for _, tool := range s.resourceTools() {
		if tool.Name() == "resource_draft" {
			out, e := tool.Call(ctx, bad, nil)
			if e != nil || !out.IsError || !strings.Contains(out.Flatten(), "SHA-256") {
				t.Fatal("stale source did not give corrective feedback")
			}
		}
	}
	secret := "synthetic-secret"
	os.WriteFile(filepath.Join(root, "credential"), []byte(secret), 0600)
	h := sha256.Sum256([]byte(secret))
	d = call("resource_draft", map[string]any{"action": "configure", "draft_id": d["id"], "revision": d["revision"], "endpoints": []map[string]string{{"name": "service", "url": "ssh://example.invalid"}}, "credential_files": map[string]any{"service": map[string]string{"path": "credential", "sha256": hex.EncodeToString(h[:])}}})
	raw, _ := json.Marshal(d)
	if strings.Contains(string(raw), secret) {
		t.Fatal("configuration tool leaked credential")
	}
	v := call("publish_resource", map[string]any{"draft_id": d["id"], "revision": d["revision"], "note": "publish", "validation": "synthetic"})
	used := call("use_resource", map[string]any{"resource_id": r.ID, "version_id": v["id"]})
	raw, _ = json.Marshal(used)
	if strings.Contains(string(raw), secret) || used["document"] == nil {
		t.Fatal("credential leaked or entry document missing")
	}
	paths := used["credential_files"].(map[string]any)
	path := paths["service"].(string)
	fi, e := os.Stat(path)
	if e != nil || fi.Mode().Perm() != 0600 {
		t.Fatal("credential file permissions")
	}
}
func TestResourceHTTPDocumentConflictAndWorkspaceProtection(t *testing.T) {
	s, r, _ := libraryFixture(t)
	mux := http.NewServeMux()
	s.registerLibraryRoutes(mux)
	lib := s.resourceLibrary()
	d, e := lib.NewDraft(t.Context(), r.ID, 0, resourcelib.Actor{})
	if e != nil {
		t.Fatal(e)
	}
	request := func(method, path, body string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, httptest.NewRequest(method, path, strings.NewReader(body)))
		return w
	}
	w := request("GET", fmt.Sprintf("/api/resource-drafts/%d/document", d.ID), "")
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	body := fmt.Sprintf(`{"revision":%d,"content":"---\nname: edit\ndescription: edited\n---\n\nnew text\n"}`, d.Revision)
	path := fmt.Sprintf("/api/resource-drafts/%d/document", d.ID)
	if w = request("PUT", path, body); w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	if w = request("PUT", path, body); w.Code != 409 || !strings.Contains(w.Body.String(), "current_revision") {
		t.Fatal("stale edit did not return conflict metadata")
	}
	if _, ok := s.wsResolve(".resource-library/versions"); ok {
		t.Fatal("library reachable through workspace mutation API")
	}
	os.Symlink(lib.Root, filepath.Join(s.m.dir, "alias"))
	if _, ok := s.wsResolve("alias/new-file"); ok {
		t.Fatal("new file through symlink bypassed protection")
	}
}
func TestRunnerDiagnosticProjectionAtomicAndContentFree(t *testing.T) {
	m := runnerTestManager(t)
	m.pg.Exec(`DELETE FROM runner_state`)
	m.pg.Exec(`DELETE FROM runner_diagnostics`)
	defer m.pg.Exec(`DELETE FROM runner_state`)
	defer m.pg.Exec(`DELETE FROM runner_diagnostics`)
	store := runnerPGStore{m.pg.DB}
	raw := json.RawMessage(`{"private_tool_output":"secret\u0000"}`)
	statuses := []runner.SessionStatus{{ID: "s", State: "active", TaskID: "1", ThreadID: "task:1:intent:2:run:3"}}
	rev, e := store.SaveDiagnostic(t.Context(), 0, raw, statuses)
	if e != nil {
		t.Fatal(e)
	}
	var projected []byte
	var diagnosticRev uint64
	if e = m.pg.QueryRow(`SELECT revision,sessions FROM runner_diagnostics WHERE id=1`).Scan(&diagnosticRev, &projected); e != nil || diagnosticRev != rev || strings.Contains(string(projected), "secret") {
		t.Fatal("invalid diagnostic projection")
	}
	if _, e = store.SaveDiagnostic(t.Context(), 0, json.RawMessage(`{}`), []runner.SessionStatus{{ID: "bad"}}); !errors.Is(e, runner.ErrStoreConflict) {
		t.Fatal("diagnostic transaction bypassed checkpoint CAS")
	}
	m.pg.QueryRow(`SELECT revision FROM runner_diagnostics WHERE id=1`).Scan(&diagnosticRev)
	if diagnosticRev != rev {
		t.Fatal("CAS failure partially wrote diagnostics")
	}
	snap, e := store.Load(context.Background())
	if e != nil || string(snap.Data) != string(raw) {
		t.Fatal("opaque binary checkpoint was changed")
	}
}
