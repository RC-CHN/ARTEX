package harness

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"iter"
	"sync/atomic"
	"testing"

	"github.com/RC-CHN/ARTEX/norma/llm"
	"github.com/RC-CHN/ARTEX/norma/permission"
	"github.com/RC-CHN/ARTEX/norma/tool"
)

func TestIncompleteResponseRetainsEvidenceWithoutDispatch(t *testing.T) {
	for _, nonstream := range []bool{false, true} {
		for _, canceled := range []bool{false, true} {
			t.Run(fmt.Sprintf("nonstream=%v/canceled=%v", nonstream, canceled), func(t *testing.T) {
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				var effects atomic.Int32
				probe := tool.Build(tool.Spec{Name: "probe", Schema: map[string]any{"type": "object"}, Run: func(context.Context, json.RawMessage, *tool.ToolContext) (tool.Result, error) {
					effects.Add(1)
					return tool.Text("must not execute"), nil
				}})
				cause := &llm.IncompleteResponseError{Reason: llm.IncompleteInvalidToolInput}
				failure := fmt.Errorf("wrapped provider failure: %w", cause)
				usage := llm.Usage{InputTokens: 23, OutputTokens: 7}
				msg := llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{
					llm.TextBlock("synthetic partial answer"),
					{Type: llm.BlockToolUse, ID: "pending", Name: "probe", Input: json.RawMessage(`{"partial":`)},
				}}
				calls := 0
				deps := QueryDeps{
					CallModelSync: func(context.Context, llm.CompletionRequest) (llm.Message, string, llm.Usage, error) {
						calls++
						if canceled {
							cancel()
						}
						return msg, "tool_use", usage, failure
					},
					CallModel: func(context.Context, llm.CompletionRequest) iter.Seq2[llm.StreamEvent, error] {
						return func(yield func(llm.StreamEvent, error) bool) {
							calls++
							for _, ev := range []llm.StreamEvent{
								{Type: llm.SETextDelta, Text: "synthetic partial answer"},
								{Type: llm.SEToolUseStart, ToolID: "pending", ToolName: "probe"},
								{Type: llm.SEToolInputJSON, Text: `{"partial":`},
								{Type: llm.SEStreamDiagnostics, Usage: usage, StopReason: "tool_use", Diagnostics: &llm.StreamDiagnostics{StreamEnd: llm.StreamEndProtocolError}},
							} {
								if !yield(ev, nil) {
									return
								}
							}
							if canceled {
								cancel()
							}
							yield(llm.StreamEvent{}, failure)
						}
					},
				}
				var term *Terminal
				for ev, err := range Query(ctx, QueryInput{Tools: tool.NewRegistry(probe), PermissionMode: permission.ModeBypass, NonStreaming: nonstream, RequireFinalText: true}, deps) {
					if err != nil || ev.ToolUse != nil || ev.ToolResult != nil {
						t.Fatalf("invalid tool reached execution: event=%+v err=%v", ev, err)
					}
					if ev.Terminal != nil {
						term = ev.Terminal
					}
				}
				wantReason := ReasonIncompleteResponse
				if canceled {
					wantReason = ReasonAbortedStreaming
				}
				if term == nil || term.Reason != wantReason || term.Usage != usage || calls != 1 || effects.Load() != 0 || term.SummaryRecoveryAttempted {
					t.Fatalf("terminal=%+v calls=%d effects=%d", term, calls, effects.Load())
				}
				if !canceled && (!errors.Is(term.Err, cause) || term.Text != "synthetic partial answer") {
					t.Fatalf("lost partial response: %+v", term)
				}
				if term.InterruptedToolInput == nil || string(term.InterruptedToolInput.Input) != `{"partial":` {
					t.Fatalf("lost unsubmitted input: %+v", term)
				}
				for _, m := range term.Messages {
					if len(m.ToolUses()) != 0 {
						t.Fatal("unsubmitted tool became replayable history")
					}
				}
			})
		}
	}
}

func TestCancellationRetainsDiagnosticUsageWithoutProcessingContent(t *testing.T) {
	for _, cancelBeforeDiagnostics := range []bool{false, true} {
		t.Run(fmt.Sprintf("cancel_before_diagnostics=%v", cancelBeforeDiagnostics), func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			var effects atomic.Int32
			probe := tool.Build(tool.Spec{Name: "probe", Schema: map[string]any{"type": "object"}, Run: func(context.Context, json.RawMessage, *tool.ToolContext) (tool.Result, error) {
				effects.Add(1)
				return tool.Text("must not execute"), nil
			}})
			usage := llm.Usage{InputTokens: 23, OutputTokens: 7, CacheReadTokens: 11}
			deps := QueryDeps{CallModel: func(context.Context, llm.CompletionRequest) iter.Seq2[llm.StreamEvent, error] {
				return func(yield func(llm.StreamEvent, error) bool) {
					for _, ev := range []llm.StreamEvent{
						{Type: llm.SETextDelta, Text: "before cancellation"},
						{Type: llm.SEToolUseStart, ToolID: "pending", ToolName: "probe"},
						{Type: llm.SEToolInputJSON, Text: `{"partial":`},
					} {
						if !yield(ev, nil) {
							return
						}
					}
					if cancelBeforeDiagnostics {
						cancel()
					}
					if !yield(llm.StreamEvent{Type: llm.SEStreamDiagnostics, Usage: usage, Diagnostics: &llm.StreamDiagnostics{StreamEnd: llm.StreamEndReadError}}, nil) {
						return
					}
					cancel()
					// A late content event must neither reach the user nor finalize
					// the pending tool after accounting has crossed cancellation.
					if !yield(llm.StreamEvent{Type: llm.SETextDelta, Text: "after cancellation"}, nil) {
						return
					}
					yield(llm.StreamEvent{}, ctx.Err())
				}
			}}
			var term *Terminal
			var text []string
			for ev, err := range Query(ctx, QueryInput{Tools: tool.NewRegistry(probe), PermissionMode: permission.ModeBypass, RequireFinalText: true}, deps) {
				if err != nil || ev.ToolUse != nil || ev.ToolResult != nil {
					t.Fatalf("canceled content reached execution: event=%+v err=%v", ev, err)
				}
				if ev.Kind == KindText {
					text = append(text, ev.Text)
				}
				if ev.Terminal != nil {
					term = ev.Terminal
				}
			}
			if term == nil || term.Reason != ReasonAbortedStreaming || term.Usage != usage || effects.Load() != 0 || term.SummaryRecoveryAttempted {
				t.Fatalf("terminal=%+v effects=%d", term, effects.Load())
			}
			if len(text) != 1 || text[0] != "before cancellation" {
				t.Fatalf("late text escaped cancellation: %v", text)
			}
			if term.InterruptedToolInput == nil || string(term.InterruptedToolInput.Input) != `{"partial":` {
				t.Fatalf("lost pending input: %+v", term.InterruptedToolInput)
			}
		})
	}
}
