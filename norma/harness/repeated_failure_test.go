package harness

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/RC-CHN/ARTEX/norma/llm"
	"github.com/RC-CHN/ARTEX/norma/permission"
	"github.com/RC-CHN/ARTEX/norma/tool"
)

func TestRepeatedFailureGuidanceDoesNotAbortOrRepeatEffects(t *testing.T) {
	for _, success := range []bool{false, true} {
		t.Run(fmt.Sprint(success), func(t *testing.T) {
			calls := 0
			fail := tool.Build(tool.Spec{Name: "probe", Schema: map[string]any{"type": "object"}, Run: func(context.Context, json.RawMessage, *tool.ToolContext) (tool.Result, error) {
				calls++
				if success {
					return tool.Text("still running"), nil
				}
				return tool.Fail("missing_input", "synthetic input unavailable", "Read the current state."), nil
			}})
			m := &scriptedModel{}
			for i := range 7 {
				input := `{"x":1,"y":2}`
				if i%2 == 1 {
					input = `{ "y": 2, "x": 1 }`
				}
				m.turns = append(m.turns, toolTurn(fmt.Sprint(i), "probe", input))
			}
			m.turns = append(m.turns, textTurn("done"))
			term := drain(t, QueryInput{Tools: tool.NewRegistry(fail, tool.Build(tool.Spec{Name: "ask_main_agent"})), PermissionMode: permission.ModeBypass}, QueryDeps{CallModel: m.call})
			if term.Reason != ReasonCompleted || calls != 7 {
				t.Fatalf("unexpected interruption/replay: %v %d", term.Reason, calls)
			}
			n := 0
			escalate := false
			for _, msg := range term.Messages {
				if strings.Contains(msg.Text(), "[repeated-tool-failure]") {
					n++
					escalate = escalate || strings.Contains(msg.Text(), "one ask_main_agent")
				}
			}
			want := 2
			if success {
				want = 0
			}
			if n != want || (!success && !escalate) {
				t.Fatalf("notices=%d escalate=%v", n, escalate)
			}
		})
	}
}

func TestRepeatedFailureResetsOnChangedArgumentsOrSuccess(t *testing.T) {
	l := &loop{ctx: t.Context(), in: QueryInput{Tools: tool.NewRegistry()}}
	use := llm.ContentBlock{ID: "one", Name: "probe", Input: json.RawMessage(`{"id":1}`)}
	fail := llm.ToolResultText("one", "missing", true)
	l.noteRepeatedFailures([]llm.ContentBlock{use}, []llm.ContentBlock{fail})
	l.noteRepeatedFailures([]llm.ContentBlock{use}, []llm.ContentBlock{fail})
	use.Input = json.RawMessage(`{"id":2}`)
	l.noteRepeatedFailures([]llm.ContentBlock{use}, []llm.ContentBlock{fail})
	l.noteRepeatedFailures([]llm.ContentBlock{use}, []llm.ContentBlock{llm.ToolResultText("one", "ok", false)})
	l.noteRepeatedFailures([]llm.ContentBlock{use}, []llm.ContentBlock{fail})
	if len(l.messages) != 0 {
		t.Fatal("changed attempt counted as repeated failure")
	}
}
