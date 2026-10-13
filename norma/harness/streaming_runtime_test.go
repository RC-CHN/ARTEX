package harness

import (
	"context"
	"encoding/json"
	"sync/atomic"
	"testing"

	"github.com/RC-CHN/ARTEX/norma/llm"
	"github.com/RC-CHN/ARTEX/norma/permission"
	"github.com/RC-CHN/ARTEX/norma/tool"
)

func TestStreamingExecutorKeepsItsTurnCancellationDuringSettlement(t *testing.T) {
	budgetCtx, endBudget := context.WithCancel(t.Context())
	defer endBudget()
	var calls atomic.Int32
	probe := tool.Build(tool.Spec{Name: "probe", Schema: map[string]any{"type": "object"},
		Run: func(ctx context.Context, _ json.RawMessage, _ *tool.ToolContext) (tool.Result, error) {
			calls.Add(1)
			return tool.Text("settled"), ctx.Err()
		}})
	l := &loop{ctx: t.Context(), toolCtx: budgetCtx, sem: make(chan struct{}, 1),
		in:    QueryInput{Tools: tool.NewRegistry(probe), PermissionMode: permission.ModeBypass},
		yield: func(Event, error) bool { return true },
	}
	oldTurn := newStreamExec(l)
	endBudget()
	// The loop begins settlement before the prior tool goroutine's trailing queue
	// check runs. That old queue must retain its canceled budget context.
	l.toolCtx = l.ctx
	oldTurn.add(llm.ContentBlock{ID: "old", Name: "probe", Input: json.RawMessage(`{}`)})
	if stopped, aborted := oldTurn.finish(); stopped || !aborted || calls.Load() != 0 {
		t.Fatalf("expired turn resumed on settlement context: stopped=%t aborted=%t calls=%d", stopped, aborted, calls.Load())
	}
	settlement := newStreamExec(l)
	settlement.add(llm.ContentBlock{ID: "settle", Name: "probe", Input: json.RawMessage(`{}`)})
	if stopped, aborted := settlement.finish(); stopped || aborted || calls.Load() != 1 {
		t.Fatalf("live settlement did not execute: stopped=%t aborted=%t calls=%d", stopped, aborted, calls.Load())
	}
}
