package harness

import (
	"context"
	"encoding/json"
	"iter"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/RC-CHN/ARTEX/norma/llm"
	"github.com/RC-CHN/ARTEX/norma/permission"
	"github.com/RC-CHN/ARTEX/norma/tool"
)

// On hitting MaxTurns with a Settlement configured, the loop injects the wrap-up
// prompt, hides DisabledTools during wrap-up, and finishes with the ORIGINAL
// budget reason (not ReasonCompleted).
func TestSettlementOnBudgetHit(t *testing.T) {
	var toolsSeen [][]string
	turns := [][]llm.StreamEvent{
		toolTurn("a", "Bash", `{"command":"echo 1"}`), // turn 1 → hits MaxTurns=1
		textTurn("已写回事实并总结"),                          // settlement turn → natural stop
	}
	call := func(_ context.Context, req llm.CompletionRequest) iter.Seq2[llm.StreamEvent, error] {
		names := make([]string, 0, len(req.Tools))
		for _, ts := range req.Tools {
			names = append(names, ts.Name)
		}
		i := len(toolsSeen)
		toolsSeen = append(toolsSeen, names)
		evs := turns[i]
		return func(yield func(llm.StreamEvent, error) bool) {
			for _, ev := range evs {
				if !yield(ev, nil) {
					return
				}
			}
		}
	}
	in := QueryInput{
		Messages:       []llm.Message{llm.UserText("go")},
		Tools:          tool.NewRegistry(tool.NewBash()),
		PermissionMode: permission.ModeBypass,
		WorkingDir:     t.TempDir(),
		MaxTurns:       1,
		Settlement:     &Settlement{Prompt: "SETTLE_NOW 请写回事实", DisabledTools: []string{"Bash"}},
	}
	term := drain(t, in, QueryDeps{CallModel: call})

	if term.Reason != ReasonMaxTurns {
		t.Fatalf("reason=%v, want ReasonMaxTurns（收尾后仍报原预算 reason）", term.Reason)
	}
	found := false
	for _, m := range term.Messages {
		for _, c := range m.Content {
			if strings.Contains(c.Text, "SETTLE_NOW") {
				found = true
			}
		}
	}
	if !found {
		t.Fatalf("收尾提示未被注入到对话")
	}
	if len(toolsSeen) < 2 {
		t.Fatalf("应至少 2 次模型调用(主+收尾)，得 %d", len(toolsSeen))
	}
	if !contains(toolsSeen[0], "Bash") {
		t.Fatalf("主阶段应能看到 Bash: %v", toolsSeen[0])
	}
	if contains(toolsSeen[1], "Bash") {
		t.Fatalf("收尾阶段应屏蔽 Bash: %v", toolsSeen[1])
	}
}

// With no Settlement, hitting the budget finishes immediately (unchanged behavior).
func TestNoSettlementFinishesImmediately(t *testing.T) {
	m := &scriptedModel{turns: [][]llm.StreamEvent{
		toolTurn("a", "Bash", `{"command":"echo 1"}`),
		toolTurn("b", "Bash", `{"command":"echo 2"}`),
	}}
	in := QueryInput{
		Tools:          tool.NewRegistry(tool.NewBash()),
		PermissionMode: permission.ModeBypass,
		WorkingDir:     t.TempDir(),
		MaxTurns:       1,
	}
	term := drain(t, in, QueryDeps{CallModel: m.call})
	if term.Reason != ReasonMaxTurns {
		t.Fatalf("reason=%v", term.Reason)
	}
	if m.calls != 1 {
		t.Fatalf("无收尾应只调用 1 次模型，得 %d", m.calls)
	}
}

func contains(xs []string, s string) bool {
	for _, x := range xs {
		if x == s {
			return true
		}
	}
	return false
}

// A model can still emit an advertised-in-an-earlier-turn tool. Enforce the
// phase at execution too, including deferred and nested wrappers.
func TestSettlementRejectsDirectAndDeferredCommands(t *testing.T) {
	ctx := t.Context()
	dir := t.TempDir()
	registry := tool.NewRegistry(tool.NewBash())
	registry.Add(tool.NewExecuteExtraTool(registry, nil))
	l := &loop{ctx: ctx, in: QueryInput{Tools: registry, WorkingDir: dir, PermissionMode: permission.ModeBypass, Settlement: &Settlement{DisabledTools: []string{"Bash"}}}}
	l.settling.Store(true)
	command := json.RawMessage(`{"command":"touch unexpected-side-effect"}`)
	names := []string{"Bash", tool.ExecuteExtraToolName, tool.ExecuteExtraToolName}
	inputs := []json.RawMessage{command}
	for i := 0; i < 2; i++ {
		wrapped, _ := json.Marshal(map[string]any{"tool_name": names[i], "params": inputs[i]})
		inputs = append(inputs, wrapped)
	}
	for i, name := range names {
		result, _ := l.execOne(ctx, llm.ContentBlock{ID: "blocked", Name: name, Input: inputs[i]}, nil)
		encoded, _ := json.Marshal(result)
		if !result.IsError || !strings.Contains(string(encoded), "settlement_tool_disabled") {
			t.Fatalf("wrapper %d did not return actionable rejection: %s", i, encoded)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "unexpected-side-effect")); !os.IsNotExist(err) {
		t.Fatal("disabled tool executed", err)
	}
	l.settling.Store(false)
	result, _ := l.execOne(ctx, llm.ContentBlock{ID: "allowed", Name: "Bash", Input: command}, nil)
	if result.IsError {
		t.Fatal("normal phase rejected command", result)
	}
	if _, err := os.Stat(filepath.Join(dir, "unexpected-side-effect")); err != nil {
		t.Fatal(err)
	}
}
