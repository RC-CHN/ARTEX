package server

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/RC-CHN/ARTEX/agent"
	actool "github.com/RC-CHN/ARTEX/norma/tool"
)

// These are literal Agent-facing calls, following only schemas and returned
// receipts. Optional output is a synthetic transcript for manual UX review.
func TestLifecycleToolDogfood(t *testing.T) {
	s, _, _ := libraryFixture(t)
	task, err := s.m.CreateTask("synthetic handoff", "synthetic", nil, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	tid, _ := strconv.ParseInt(task.ID, 10, 64)
	ctx := agent.WithRunInfo(t.Context(), agent.RunInfo{TaskID: tid, IntentID: 7})
	tools := map[string]actool.CoreTool{}
	for _, tool := range s.resourceTools() {
		tools[tool.Name()] = tool
	}
	var transcript []map[string]any
	call := func(name string, input any, wantError bool) map[string]any {
		t.Helper()
		raw, _ := json.Marshal(input)
		tool, ok := tools[name]
		if !ok {
			t.Fatal("missing tool", name)
		}
		result, err := tool.Call(ctx, raw, nil)
		if err != nil || result.IsError != wantError {
			t.Fatalf("%s: %v %s", name, err, result.Flatten())
		}
		var out map[string]any
		if err = json.Unmarshal([]byte(result.Flatten()), &out); err != nil {
			t.Fatalf("%s returned non-JSON: %s", name, result.Flatten())
		}
		transcript = append(transcript, map[string]any{"tool": name, "input": json.RawMessage(raw), "output": out, "is_error": result.IsError})
		if wantError && (out["code"] == nil || out["next_action"] == nil) {
			t.Fatalf("non-actionable error: %v", out)
		}
		return out
	}
	r := call("manage_resource", map[string]any{"action": "create", "request_id": "register-synthetic", "resource": map[string]any{"name": "synthetic local fixture", "kind": "tool"}}, false)
	id := r["id"]
	call("manage_resource", map[string]any{"action": "create", "request_id": "register-synthetic", "resource": map[string]any{"name": "synthetic local fixture", "kind": "tool"}}, false)
	call("manage_resource", map[string]any{"action": "create", "request_id": "register-synthetic", "resource": map[string]any{"name": "changed input", "kind": "tool"}}, true)
	// The only lifecycle ledger is the resource document. Exercise the actual
	// Agent interfaces for writing, publishing and reading its cleanup record.
	if _, exists := tools["manage_deployments"]; exists {
		t.Fatal("retired deployment tool remains exposed")
	}
	root := filepath.Join(s.m.dir, "tasks", task.ID)
	if err = os.MkdirAll(root, 0755); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(root, "fixture.txt")
	if err = os.WriteFile(file, []byte("synthetic"), 0600); err != nil {
		t.Fatal(err)
	}
	if err = os.Remove(file); err != nil {
		t.Fatal(err)
	}
	if _, err = os.Stat(file); !os.IsNotExist(err) {
		t.Fatal("synthetic file remains")
	}
	document := "---\nname: synthetic local fixture\ndescription: Fixture with cleanup history\n---\n\n## Instance\nLocation: isolated test directory; identifier: fixture.txt.\nCleanup: remove the exact file, then verify absence.\nVerification: removed fixture.txt; stat confirmed absence.\n"
	if err = os.WriteFile(filepath.Join(root, "RESOURCE.md"), []byte(document), 0600); err != nil {
		t.Fatal(err)
	}
	draft := call("resource_draft", map[string]any{"action": "create", "resource_id": id}, false)
	draft = call("resource_draft", map[string]any{"action": "import_file", "draft_id": draft["id"], "revision": draft["revision"], "source_path": "RESOURCE.md", "path": "RESOURCE.md", "sha256": fmt.Sprintf("%x", sha256.Sum256([]byte(document)))}, false)
	version := call("publish_resource", map[string]any{"draft_id": draft["id"], "revision": draft["revision"], "note": "Record verified cleanup", "validation": "Created and removed the local synthetic fixture; stat confirmed absence."}, false)
	used := call("use_resource", map[string]any{"resource_id": id, "version_id": version["id"]}, false)
	if !strings.Contains(used["document"].(string), "stat confirmed absence") {
		t.Fatal("resource lost cleanup record", used)
	}
	refs := call("resource_references", map[string]any{"resource_id": id}, false)
	if len(refs["references"].([]any)) != 1 {
		t.Fatal("task reference missing", refs)
	}
	check := call("check_task_handoff", map[string]any{}, false)
	if check["references"] == nil || check["task_budget"] == nil {
		t.Fatal("handoff lacks navigation/budget", check)
	}
	if len(check["checks"].([]any)) != 0 {
		t.Fatal("unexpected handoff gap", check)
	}
	deletion := map[string]any{"resource_id": id, "expected_revision": r["revision"], "request_id": "delete-stale", "reason": "synthetic cleanup complete"}
	stale := call("delete_resource", deletion, true)
	if stale["code"] != "resource_conflict" {
		t.Fatal("stale delete lacks actionable conflict", stale)
	}
	current := call("resource_versions", map[string]any{"resource_id": id}, false)
	deletion["expected_revision"] = current["resource"].(map[string]any)["revision"]
	deletion["request_id"] = "delete-synthetic"
	deleted := call("delete_resource", deletion, false)
	if deleted["remote_cleanup_performed"] != false || deleted["history_retained"] != true {
		t.Fatal("misleading deletion receipt", deleted)
	}
	call("delete_resource", deletion, false)
	call("check_task_handoff", map[string]any{}, false)
	batch := call("manage_resources_batch", map[string]any{"items": []any{map[string]any{"action": "create", "request_id": "batch-new", "resource": map[string]any{"name": "batch fixture", "kind": "file"}}, map[string]any{"action": "create", "resource": map[string]any{"name": "invalid item", "kind": "file"}}}}, false)
	if batch["succeeded"] != float64(1) || batch["failed"] != float64(1) {
		t.Fatal(batch)
	}
	if dir := os.Getenv("ARTEX_DOGFOOD_OUTPUT"); dir != "" {
		if err = os.MkdirAll(dir, 0700); err != nil {
			t.Fatal(err)
		}
		f, err := os.Create(filepath.Join(dir, "resource-lifecycle.jsonl"))
		if err != nil {
			t.Fatal(err)
		}
		defer f.Close()
		for _, entry := range transcript {
			if err = json.NewEncoder(f).Encode(entry); err != nil {
				t.Fatal(err)
			}
		}
	}
	t.Log(fmt.Sprintf("manually reviewable synthetic tool workflow: %d calls", len(transcript)))
}

func TestRetiredDeploymentHTTPRoutes(t *testing.T) {
	mux := http.NewServeMux()
	(&Server{}).registerLibraryRoutes(mux)
	for _, method := range []string{http.MethodGet, http.MethodPost} {
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, httptest.NewRequest(method, "/api/tasks/1/deployments", strings.NewReader("{}")))
		if w.Code != http.StatusNotFound {
			t.Fatalf("retired %s route still reachable: %d", method, w.Code)
		}
	}
}
