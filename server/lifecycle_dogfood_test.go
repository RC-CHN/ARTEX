package server

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
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
	entry := map[string]any{"request_id": "register-file", "name": "synthetic fixture file", "kind": "file", "location": "local synthetic directory", "identifier": "fixture.txt", "cleanup": "Remove this synthetic file after checking its exact path, then verify absence.", "resource_id": id}
	d := call("manage_deployments", map[string]any{"action": "save", "item": entry}, false)
	call("manage_deployments", map[string]any{"action": "list"}, false)
	call("manage_deployments", map[string]any{"action": "get", "id": 9223372036854775807}, true)
	call("manage_deployments", map[string]any{"action": "get", "id": d["id"]}, false)
	call("resource_references", map[string]any{"resource_id": id}, false)
	check := call("check_task_handoff", map[string]any{}, false)
	if len(check["checks"].([]any)) == 0 {
		t.Fatal("cleanup gap missing")
	}
	deletion := map[string]any{"resource_id": id, "expected_revision": r["revision"], "request_id": "delete-synthetic", "reason": "synthetic cleanup complete"}
	blocked := call("delete_resource", deletion, true)
	if blocked["code"] != "resource_in_use" {
		t.Fatal(blocked)
	}
	update := map[string]any{"id": d["id"], "expected_revision": d["revision"], "request_id": "cleanup-result", "state": "removed"}
	call("manage_deployments", map[string]any{"action": "save", "item": update}, true)
	// Actually exercise a harmless local cleanup before recording success.
	file := filepath.Join(t.TempDir(), "fixture.txt")
	if err = os.WriteFile(file, []byte("synthetic"), 0600); err != nil {
		t.Fatal(err)
	}
	if err = os.Remove(file); err != nil {
		t.Fatal(err)
	}
	if _, err = os.Stat(file); !os.IsNotExist(err) {
		t.Fatal("synthetic file remains")
	}
	update["verification"] = "Created and removed the exact synthetic fixture.txt; stat confirmed absence."
	d2 := call("manage_deployments", map[string]any{"action": "save", "item": update}, false)
	call("manage_deployments", map[string]any{"action": "save", "item": update}, false)
	stale := map[string]any{"id": d["id"], "expected_revision": d["revision"], "request_id": "stale-change", "note": "late writer"}
	conflict := call("manage_deployments", map[string]any{"action": "save", "item": stale}, true)
	if conflict["current_revision"] != d2["revision"] {
		t.Fatal(conflict)
	}
	deleted := call("delete_resource", deletion, false)
	if deleted["remote_cleanup_performed"] != false {
		t.Fatal("deletion claimed remote cleanup")
	}
	call("delete_resource", deletion, false)
	call("manage_deployments", map[string]any{"action": "save", "item": map[string]any{"id": d["id"], "expected_revision": d2["revision"], "request_id": "annotate-after-delete", "note": "Historical resource deleted; verified removal remains recorded."}}, false)
	call("check_task_handoff", map[string]any{}, false)
	batch := call("manage_resources_batch", map[string]any{"items": []any{map[string]any{"action": "create", "request_id": "batch-new", "resource": map[string]any{"name": "batch fixture", "kind": "file"}}, map[string]any{"action": "create", "resource": map[string]any{"name": "invalid item", "kind": "file"}}}}, false)
	if batch["succeeded"] != float64(1) || batch["failed"] != float64(1) {
		t.Fatal(batch)
	}
	batch = call("manage_deployments", map[string]any{"action": "save", "items": []any{map[string]any{"id": d["id"], "expected_revision": d2["revision"].(float64) + 1, "request_id": "batch-annotation", "note": "Synthetic batch verified"}, map[string]any{"request_id": "invalid-deployment"}}}, false)
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
