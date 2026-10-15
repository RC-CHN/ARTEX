package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/RC-CHN/ARTEX/db"
)

func TestPlannerCancelPendingIntentTool(t *testing.T) {
	d := testDB(t)
	defer d.Close()
	exp, err := d.CreateExploration("planner cancellation", "synthetic fixture")
	if err != nil {
		t.Fatal(err)
	}
	defer d.Exec(`DELETE FROM explorations WHERE id=$1`, exp)
	s := d.Exploration(exp)
	id, err := s.AddIntent(map[string]any{"summary": "obsolete direction"}, 1, nil, "planner")
	if err != nil {
		t.Fatal(err)
	}
	ts := NewToolSet(s, "planner")
	tool := ts.cancelPendingIntentTool()
	for _, input := range []string{`{`, `{}`, fmt.Sprintf(`{"intent_id":%d,"reason":" "}`, id)} {
		result, err := tool.Call(context.Background(), json.RawMessage(input), nil)
		if err != nil || !result.IsError {
			t.Fatalf("invalid input accepted: %+v, %v", result, err)
		}
	}
	input := json.RawMessage(fmt.Sprintf(`{"intent_id":%d,"reason":"另一条工作已经验证"}`, id))
	result, err := tool.Call(context.Background(), input, nil)
	if err != nil || result.IsError || !strings.Contains(result.Flatten(), "stopped") {
		t.Fatalf("cancel result: %+v, %v", result, err)
	}
	result, err = tool.Call(context.Background(), input, nil)
	if err != nil || !result.IsError || !strings.Contains(result.Flatten(), "状态冲突") {
		t.Fatalf("repeat should explain race: %+v, %v", result, err)
	}
	node, err := s.GetNode(id)
	if err != nil {
		t.Fatal(err)
	}
	compact := compactIntents([]*db.Node{node}, nil, nil)
	if compact[0]["cancellation"] == nil {
		t.Fatal("planner summary lost cancellation reason")
	}
	found := false
	for _, seed := range BuiltinToolSeeds() {
		if seed.Key == tool.Name() {
			found = true
			if !slices.Equal(seed.Agents, []string{"planner"}) {
				t.Fatalf("unexpected default binding: %v", seed.Agents)
			}
		}
	}
	if !found {
		t.Fatal("tool missing from seeded catalog")
	}
}
