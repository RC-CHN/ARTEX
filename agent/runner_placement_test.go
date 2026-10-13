package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"testing"

	"github.com/RC-CHN/ARTEX/db"
)

// Exercise the model-facing tool, not ExplorationStore.AddIntent directly: both
// its single-item decoder and its batch item type must carry placement through.
func TestAddIntentRunnerPlacementSingleAndBatch(t *testing.T) {
	d := testDB(t)
	t.Cleanup(func() { _ = d.Close() })
	task, err := d.CreateTask("runner placement", "schedule distinct execution environments", nil, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.DeleteTask(task.ID) })
	store := d.Exploration(task.ExplorationID)
	ts := NewToolSet(store, "planner")
	ts.SetTaskID(task.ID)
	tool := ts.addIntent()

	single := json.RawMessage(`{"node_id":"gpu-node","environment":"gpu","labels":{"network":"lab"},"resources":{"cpu_millis":2500,"memory_mib":8192,"gpu_count":1,"gpu_model":"A100","gpu_memory_mib":40960}}`)
	input, _ := json.Marshal(map[string]any{"summary": "single GPU intent", "runner": single})
	result, err := tool.Call(context.Background(), input, nil)
	if err != nil || result.IsError {
		t.Fatalf("single add_intent: %v %s", err, result.Flatten())
	}
	var singleID int64
	if _, err := fmt.Sscanf(result.Flatten(), "intent created: %d", &singleID); err != nil || singleID <= 0 {
		t.Fatalf("single result %q: %v", result.Flatten(), err)
	}
	assertPlacement := func(id int64, expected json.RawMessage) {
		t.Helper()
		node, err := store.GetNode(id)
		if err != nil || node == nil || node.Kind != db.KindIntent {
			t.Fatalf("persisted intent %d: %+v %v", id, node, err)
		}
		var payload map[string]json.RawMessage
		if err = json.Unmarshal(node.Payload, &payload); err != nil {
			t.Fatal(err)
		}
		if expected == nil {
			if _, exists := payload["runner"]; exists {
				t.Fatalf("omitted placement was invented: %s", node.Payload)
			}
			return
		}
		var want, got any
		if err = json.Unmarshal(expected, &want); err != nil {
			t.Fatal(err)
		}
		if err = json.Unmarshal(payload["runner"], &got); err != nil {
			t.Fatalf("runner payload missing: %s", node.Payload)
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("placement lost fields: got=%s want=%s", payload["runner"], expected)
		}
	}
	assertPlacement(singleID, single)

	batch := json.RawMessage(`{"environment":"cpu","labels":{"network":"isolated"},"resources":{"cpu_millis":1000,"memory_mib":2048}}`)
	input, _ = json.Marshal(map[string]any{"intents": []any{
		map[string]any{"summary": "batch CPU intent", "runner": batch},
		map[string]any{"summary": "invalid GPU request", "runner": map[string]any{"resources": map[string]int{"gpu_count": -1}}},
		map[string]any{"summary": "default environment"},
	}})
	result, err = tool.Call(context.Background(), input, nil)
	if err != nil || result.IsError {
		t.Fatalf("batch add_intent: %v %s", err, result.Flatten())
	}
	var out struct {
		IDs    []int64           `json:"ids"`
		Errors map[string]string `json:"errors"`
	}
	if err = json.Unmarshal([]byte(result.Flatten()), &out); err != nil {
		t.Fatal(err)
	}
	if len(out.IDs) != 3 || out.IDs[0] <= 0 || out.IDs[1] != 0 || out.IDs[2] <= 0 || len(out.Errors) != 1 || out.Errors["1"] == "" {
		t.Fatalf("batch validation/order: %+v", out)
	}
	assertPlacement(out.IDs[0], batch)
	assertPlacement(out.IDs[2], nil)
	intents, err := store.ListByKind(db.KindIntent, 100)
	if err != nil || len(intents) != 3 {
		t.Fatalf("invalid placement created an orphan intent: count=%d err=%v", len(intents), err)
	}
}
