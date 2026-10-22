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

func TestFinalTextContinueProgressRequiresSuccessfulReceipt(t *testing.T) {
	for _, syncMode := range []bool{false, true} {
		for _, outcome := range []string{"success", "failure", "unknown_tool", "mixed"} {
			t.Run(fmt.Sprintf("sync=%v/%s", syncMode, outcome), func(t *testing.T) {
				t.Parallel()
				var effects atomic.Int32
				failed := tool.Build(tool.Spec{Name: "failed", Run: func(context.Context, json.RawMessage, *tool.ToolContext) (tool.Result, error) {
					return tool.Errorf("synthetic failure"), nil
				}})
				reg := tool.NewRegistry(finalContinueProbe("success", &effects), failed)
				calls := 0
				progress := outcome == "success" || outcome == "mixed"
				term := drain(t, QueryInput{Tools: reg, PermissionMode: permission.ModeBypass, RequireFinalText: true, ContinueAfterEmptyReply: true, NonStreaming: syncMode}, finalContinueDeps(func(_ context.Context, req llm.CompletionRequest) (llm.Message, string, llm.Usage, error) {
					calls++
					if calls > 4 {
						t.Fatal("unbounded recovery")
					}
					switch calls {
					case 1, 3:
						return finalContinueThinking(), "", llm.Usage{}, nil
					case 2:
						name := outcome
						if outcome == "failure" || outcome == "mixed" {
							name = "failed"
						}
						msg := finalContinueCall("first", name, `{}`)
						if outcome == "mixed" {
							msg.Content = append(msg.Content, finalContinueCall("second", "success", `{}`).Content...)
						}
						return msg, "tool_use", llm.Usage{}, nil
					default:
						if !progress || len(req.Tools) == 0 {
							t.Fatal("failed tool renewed recovery or recovery disabled tools")
						}
						return finalContinueText("verified"), "end_turn", llm.Usage{}, nil
					}
				}))
				wantCalls, wantAttempts, wantEffects := 3, 1, int32(0)
				if progress {
					wantCalls, wantAttempts, wantEffects = 4, 2, 1
					if term.Reason != ReasonCompleted || term.Text != "verified" || term.Err != nil {
						t.Fatalf("successful progress did not resume: %+v", term)
					}
				} else if term.Reason != ReasonEmptyResponse || !errors.Is(term.Err, ErrEmptyResponse) {
					t.Fatalf("failed tool must not replenish recovery: %+v", term)
				}
				if calls != wantCalls || term.SummaryRecoveryAttempts != wantAttempts || effects.Load() != wantEffects || term.SummaryRecoveryMode != "continue" {
					t.Fatalf("calls=%d effects=%d terminal=%+v", calls, effects.Load(), term)
				}
			})
		}
	}
}

func TestFinalTextContinueCancelAfterProgressDoesNotRecover(t *testing.T) {
	for _, syncMode := range []bool{false, true} {
		t.Run(fmt.Sprint(syncMode), func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			var effects atomic.Int32
			calls := 0
			deps := finalContinueDeps(func(context.Context, llm.CompletionRequest) (llm.Message, string, llm.Usage, error) {
				calls++
				switch calls {
				case 1:
					return finalContinueThinking(), "", llm.Usage{}, nil
				case 2:
					return finalContinueCall("once", "probe", `{}`), "tool_use", llm.Usage{}, nil
				case 3:
					cancel()
					return llm.Message{}, "", llm.Usage{}, ctx.Err()
				default:
					t.Fatal("canceled query attempted another recovery")
					return llm.Message{}, "", llm.Usage{}, ctx.Err()
				}
			})
			var term *Terminal
			for ev := range Query(ctx, QueryInput{Tools: tool.NewRegistry(finalContinueProbe("probe", &effects)), PermissionMode: permission.ModeBypass, RequireFinalText: true, ContinueAfterEmptyReply: true, NonStreaming: syncMode}, deps) {
				if ev.Terminal != nil {
					term = ev.Terminal
				}
			}
			if term == nil || term.Reason != ReasonAbortedStreaming || !errors.Is(term.Err, context.Canceled) || term.SummaryRecoveryAttempts != 1 || calls != 3 || effects.Load() != 1 {
				t.Fatalf("calls=%d effects=%d terminal=%+v", calls, effects.Load(), term)
			}
		})
	}
}

func TestStreamDiagnosticsDoNotFinalizeToolInput(t *testing.T) {
	for _, failStream := range []bool{false, true} {
		t.Run(fmt.Sprint(failStream), func(t *testing.T) {
			var effects atomic.Int32
			failure := errors.New("synthetic stream failure")
			calls := 0
			deps := QueryDeps{CallModel: func(context.Context, llm.CompletionRequest) iter.Seq2[llm.StreamEvent, error] {
				return func(yield func(llm.StreamEvent, error) bool) {
					calls++
					if calls > 2 {
						t.Fatal("diagnostics caused additional model requests")
					}
					if calls == 2 {
						yield(llm.StreamEvent{Type: llm.SETextDelta, Text: "done"}, nil)
						return
					}
					for _, ev := range []llm.StreamEvent{
						{Type: llm.SEToolUseStart, ToolID: "once", ToolName: "probe"},
						{Type: llm.SEToolInputJSON, Text: `{"fixture":`},
						{Type: llm.SEStreamDiagnostics, Diagnostics: &llm.StreamDiagnostics{}},
					} {
						if !yield(ev, nil) {
							return
						}
					}
					if failStream {
						yield(llm.StreamEvent{}, failure)
						return
					}
					if yield(llm.StreamEvent{Type: llm.SEToolInputJSON, Text: `true}`}, nil) {
						yield(llm.StreamEvent{Type: llm.SEMessageStop}, nil)
					}
				}
			}}
			probe := tool.Build(tool.Spec{Name: "probe", Run: func(_ context.Context, input json.RawMessage, _ *tool.ToolContext) (tool.Result, error) {
				if string(input) != `{"fixture":true}` {
					t.Errorf("diagnostics truncated the input: %s", input)
				}
				effects.Add(1)
				return tool.Text("receipt"), nil
			}})
			term := drain(t, QueryInput{Tools: tool.NewRegistry(probe), PermissionMode: permission.ModeBypass, RequireFinalText: true, ContinueAfterEmptyReply: true}, deps)
			if failStream {
				if effects.Load() != 0 || term.Reason != ReasonModelError || !errors.Is(term.Err, failure) || term.InterruptedToolInput == nil {
					t.Fatalf("diagnostics executed an incomplete tool: %+v effects=%d", term, effects.Load())
				}
			} else if effects.Load() != 1 || term.Reason != ReasonCompleted || term.Err != nil || calls != 2 {
				t.Fatalf("diagnostics disrupted a tool: %+v effects=%d calls=%d", term, effects.Load(), calls)
			}
		})
	}
}

func TestFinalTextContinueCancellationAtStopBoundary(t *testing.T) {
	for _, syncMode := range []bool{false, true} {
		t.Run(fmt.Sprint(syncMode), func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			calls := 0
			hooks := &fakeHooks{stop: func(n int) (bool, []string, string) {
				if n == 2 {
					cancel()
				}
				return false, nil, ""
			}}
			deps := finalContinueDeps(func(context.Context, llm.CompletionRequest) (llm.Message, string, llm.Usage, error) {
				calls++
				if calls > 2 {
					t.Fatal("cancellation at stop boundary started another request")
				}
				return finalContinueThinking(), "", llm.Usage{}, nil
			})
			var term *Terminal
			for ev := range Query(ctx, QueryInput{Hooks: hooks, RequireFinalText: true, ContinueAfterEmptyReply: true, NonStreaming: syncMode}, deps) {
				if ev.Terminal != nil {
					term = ev.Terminal
				}
			}
			if term == nil || term.Reason != ReasonAbortedStreaming || !errors.Is(term.Err, context.Canceled) || calls != 2 || term.SummaryRecoveryAttempts != 1 {
				t.Fatalf("calls=%d terminal=%+v", calls, term)
			}
		})
	}
}
