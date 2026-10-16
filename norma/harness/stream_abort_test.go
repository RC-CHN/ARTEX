package harness

import (
	"context"
	"encoding/json"
	"errors"
	"iter"
	"sync/atomic"
	"testing"
	"time"

	"github.com/RC-CHN/ARTEX/norma/llm"
	"github.com/RC-CHN/ARTEX/norma/permission"
	"github.com/RC-CHN/ARTEX/norma/tool"
)

func TestInterruptedToolInputNeverDispatches(t *testing.T) {
	for _, input := range []string{"", `{"request_id":1,"answer":"partial`, `{"request_id":1,"answer":"complete JSON without block end"}`} {
		for _, mode := range []string{"cancel", "silent_cancel", "late_event", "provider_error"} {
			t.Run(mode+"/"+input, func(t *testing.T) {
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				var calls atomic.Int32
				probe := tool.Build(tool.Spec{Name: "reply_to_worker", Schema: map[string]any{"type": "object"},
					Run: func(context.Context, json.RawMessage, *tool.ToolContext) (tool.Result, error) {
						calls.Add(1)
						return tool.Text("unexpected execution"), nil
					}})
				model := func(context.Context, llm.CompletionRequest) iter.Seq2[llm.StreamEvent, error] {
					return func(yield func(llm.StreamEvent, error) bool) {
						for _, ev := range []llm.StreamEvent{
							{Type: llm.SEMessageStart, Usage: llm.Usage{InputTokens: 17}},
							{Type: llm.SEToolUseStart, ToolID: "pending", ToolName: "reply_to_worker"},
							{Type: llm.SEToolInputJSON, Text: input},
						} {
							if !yield(ev, nil) {
								return
							}
						}
						if mode == "provider_error" {
							yield(llm.StreamEvent{}, errors.New("synthetic stream failure"))
							return
						}
						cancel()
						if mode == "cancel" {
							yield(llm.StreamEvent{}, ctx.Err())
						}
						if mode == "late_event" {
							yield(llm.StreamEvent{Type: llm.SEMessageStop}, nil)
						}
					}
				}
				var term *Terminal
				for ev, err := range Query(ctx, QueryInput{Tools: tool.NewRegistry(probe), PermissionMode: permission.ModeBypass}, QueryDeps{CallModel: model}) {
					if err != nil {
						t.Fatal(err)
					}
					if ev.ToolUse != nil || ev.ToolResult != nil {
						t.Fatal("unfinished block emitted a tool execution event")
					}
					if ev.Terminal != nil {
						term = ev.Terminal
					}
				}
				want := ReasonAbortedStreaming
				if mode == "provider_error" {
					want = ReasonModelError
				}
				if term == nil || term.Reason != want || term.Err == nil || term.Usage.InputTokens != 17 {
					t.Fatalf("terminal: %+v", term)
				}
				if calls.Load() != 0 || term.InterruptedToolInput == nil || term.InterruptedToolInput.Name != "reply_to_worker" || string(term.InterruptedToolInput.Input) != input {
					t.Fatalf("lost unsubmitted input or ran tool: calls=%d terminal=%+v", calls.Load(), term)
				}
				for _, m := range term.Messages {
					if len(m.ToolUses()) > 0 {
						t.Fatal("unsubmitted tool leaked into replayable conversation")
					}
				}
			})
		}
	}
}

func TestStreamFailureSettlesPreviouslyStartedTools(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	started, stopped := make(chan struct{}), make(chan struct{})
	probe := tool.Build(tool.Spec{Name: "probe", Schema: map[string]any{"type": "object"},
		Run: func(ctx context.Context, _ json.RawMessage, _ *tool.ToolContext) (tool.Result, error) {
			close(started)
			<-ctx.Done()
			close(stopped)
			return tool.Result{}, ctx.Err()
		}})
	model := func(ctx context.Context, _ llm.CompletionRequest) iter.Seq2[llm.StreamEvent, error] {
		return func(yield func(llm.StreamEvent, error) bool) {
			for _, ev := range []llm.StreamEvent{
				{Type: llm.SEToolUseStart, ToolID: "started", ToolName: "probe"},
				{Type: llm.SEToolInputJSON, Text: `{}`},
				{Type: llm.SEToolUseStart, ToolID: "pending", ToolName: "probe"},
				{Type: llm.SEToolInputJSON, Text: `{"partial`},
			} {
				if !yield(ev, nil) {
					return
				}
			}
			select {
			case <-started:
			case <-ctx.Done():
			}
			yield(llm.StreamEvent{}, errors.New("synthetic stream failure"))
		}
	}
	uses, results := 0, 0
	var term *Terminal
	for ev, err := range Query(ctx, QueryInput{Tools: tool.NewRegistry(probe), PermissionMode: permission.ModeBypass}, QueryDeps{CallModel: model}) {
		if err != nil {
			t.Fatal(err)
		}
		if ev.ToolUse != nil {
			uses++
			if ev.ToolUse.ID != "started" {
				t.Fatal("dispatched partial call")
			}
		}
		if ev.ToolResult != nil {
			results++
			if ev.ToolResult.ToolUseID != "started" || !ev.ToolResult.IsError {
				t.Fatal("missing interrupted receipt")
			}
		}
		if ev.Terminal != nil {
			term = ev.Terminal
		}
	}
	if term == nil || term.Reason != ReasonModelError || uses != 1 || results != 1 || len(term.Messages) != 2 || len(term.Messages[0].ToolUses()) != 1 || term.Messages[1].Content[0].ToolUseID != "started" {
		t.Fatalf("lost executed call/result: uses=%d results=%d terminal=%+v", uses, results, term)
	}
	select {
	case <-stopped:
	case <-ctx.Done():
		t.Fatal("tool context leaked after stream failure")
	}
}

func TestSuccessfulEmptyArgumentToolStillRuns(t *testing.T) {
	var calls atomic.Int32
	probe := tool.Build(tool.Spec{Name: "probe", Schema: map[string]any{"type": "object"}, Run: func(_ context.Context, in json.RawMessage, _ *tool.ToolContext) (tool.Result, error) {
		calls.Add(1)
		if string(in) != "{}" {
			t.Errorf("input=%s", in)
		}
		return tool.Text("ok"), nil
	}})
	m := &scriptedModel{turns: [][]llm.StreamEvent{toolTurn("valid", "probe", ""), textTurn("done")}}
	term := drain(t, QueryInput{Tools: tool.NewRegistry(probe), PermissionMode: permission.ModeBypass}, QueryDeps{CallModel: m.call})
	if calls.Load() != 1 || term.Reason != ReasonCompleted || term.InterruptedToolInput != nil {
		t.Fatalf("valid tool broken: %+v", term)
	}
}
