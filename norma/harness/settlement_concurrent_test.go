package harness

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/RC-CHN/ARTEX/norma/llm"
	"github.com/RC-CHN/ARTEX/norma/permission"
	"github.com/RC-CHN/ARTEX/norma/tool"
)

// A timed-out streaming executor can return before its tool goroutines exit.
// Tool admission and the loop's transition must therefore be safe concurrently,
// even when the old executor has not joined. No sleeps or timeout luck required.
func TestSettlementConcurrentToolAdmission(t *testing.T) {
	var calls atomic.Int32
	probe := tool.Build(tool.Spec{
		Name: "probe", Schema: map[string]any{"type": "object"},
		Run: func(context.Context, json.RawMessage, *tool.ToolContext) (tool.Result, error) {
			calls.Add(1)
			return tool.Text("executed"), nil
		},
	})
	l := &loop{ctx: t.Context(), in: QueryInput{
		Tools: tool.NewRegistry(probe), PermissionMode: permission.ModeBypass,
		Settlement: &Settlement{Prompt: "save findings", DisabledTools: []string{"probe"}},
	}}
	use := llm.ContentBlock{ID: "probe", Name: "probe", Input: json.RawMessage(`{}`)}
	if result, _ := l.execOne(t.Context(), use, nil); result.IsError || calls.Load() != 1 {
		t.Fatal("normal execution was not admitted", result)
	}

	start := make(chan struct{})
	var wg sync.WaitGroup
	for range 16 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			for range 64 {
				result, _ := l.execOne(t.Context(), use, nil)
				if result.IsError {
					encoded, _ := json.Marshal(result)
					if !strings.Contains(string(encoded), "settlement_tool_disabled") {
						t.Errorf("unexpected admission result: %s", encoded)
					}
				}
			}
		}()
	}
	close(start)
	schemas := []llm.ToolSchema{{Name: "probe"}}
	if !l.beginSettlement(ReasonTimeout, &schemas) {
		t.Error("settlement did not begin")
	}
	wg.Wait()

	before := calls.Load()
	result, _ := l.execOne(t.Context(), use, nil)
	if !result.IsError || calls.Load() != before || len(schemas) != 0 {
		t.Fatal("settlement admitted a disabled tool", result)
	}
	if l.settleReason != ReasonTimeout || len(l.messages) != 1 {
		t.Fatal("settlement lost its reason or checkpoint prompt")
	}
}
