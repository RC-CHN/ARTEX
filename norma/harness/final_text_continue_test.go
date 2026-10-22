package harness

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"iter"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/RC-CHN/ARTEX/norma/llm"
	"github.com/RC-CHN/ARTEX/norma/permission"
	"github.com/RC-CHN/ARTEX/norma/tool"
)

// Exercise both execution paths with the same scripted response, including an
// empty stop reason: a missing provider stop marker is not proof of a network
// interruption and must not by itself remove the main agent's tools.
func finalContinueDeps(model func(context.Context, llm.CompletionRequest) (llm.Message, string, llm.Usage, error)) QueryDeps {
	return QueryDeps{CallModelSync: model, CallModel: func(ctx context.Context, req llm.CompletionRequest) iter.Seq2[llm.StreamEvent, error] {
		return func(yield func(llm.StreamEvent, error) bool) {
			msg, stop, usage, err := model(ctx, req)
			if err != nil {
				yield(llm.StreamEvent{}, err)
				return
			}
			for _, block := range msg.Content {
				var events []llm.StreamEvent
				switch block.Type {
				case llm.BlockText:
					events = []llm.StreamEvent{{Type: llm.SETextDelta, Text: block.Text}}
				case llm.BlockThinking:
					events = []llm.StreamEvent{{Type: llm.SEThinkingDelta, Text: block.Thinking}}
				case llm.BlockToolUse:
					events = []llm.StreamEvent{{Type: llm.SEToolUseStart, ToolID: block.ID, ToolName: block.Name}, {Type: llm.SEToolInputJSON, Text: string(block.Input)}}
				}
				for _, event := range events {
					if !yield(event, nil) {
						return
					}
				}
			}
			if yield(llm.StreamEvent{Type: llm.SEMessageDelta, StopReason: stop, Usage: usage}, nil) {
				yield(llm.StreamEvent{Type: llm.SEMessageStop}, nil)
			}
		}
	}}
}

func finalContinueThinking() llm.Message {
	return llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{{Type: llm.BlockThinking, Thinking: "synthetic thinking"}}}
}

func finalContinueCall(id, name, input string) llm.Message {
	return llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{{Type: llm.BlockToolUse, ID: id, Name: name, Input: json.RawMessage(input)}}}
}

func finalContinueText(text string) llm.Message {
	return llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{llm.TextBlock(text)}}
}

func finalContinueProbe(name string, effects *atomic.Int32) tool.CoreTool {
	return tool.Build(tool.Spec{Name: name, Schema: map[string]any{"type": "object"}, Run: func(context.Context, json.RawMessage, *tool.ToolContext) (tool.Result, error) {
		return tool.Text(fmt.Sprintf("%s receipt %d", name, effects.Add(1))), nil
	}})
}

type finalContinueCompactor struct{ seen []llm.Message }

func (c *finalContinueCompactor) Pre(_ context.Context, msgs []llm.Message, _ int) []llm.Message {
	c.seen = append(c.seen, msgs...)
	return msgs
}
func (c *finalContinueCompactor) View(_ context.Context, msgs []llm.Message) []llm.Message {
	c.seen = append(c.seen, msgs...)
	return msgs
}
func (c *finalContinueCompactor) Reactive(_ context.Context, msgs []llm.Message) ([]llm.Message, bool) {
	c.seen = append(c.seen, msgs...)
	return msgs, false
}
func (*finalContinueCompactor) IsOverflow(error) bool { return false }

func TestFinalTextContinueKeepsToolsAndEphemeralPrompt(t *testing.T) {
	for _, syncMode := range []bool{false, true} {
		t.Run(fmt.Sprintf("sync=%v", syncMode), func(t *testing.T) {
			var ordinaryEffects, deferredEffects atomic.Int32
			reg := tool.NewRegistry(finalContinueProbe("ordinary", &ordinaryEffects), finalContinueProbe("deferred", &deferredEffects))
			reg.Add(tool.NewSearchExtraTools(reg, []string{"deferred"}))
			reg.Add(tool.NewExecuteExtraTool(reg, tool.NewUnlockSet("deferred")))
			recorder, compactor := &capRecorder{}, &finalContinueCompactor{}
			calls, prompt := 0, ""
			usage := llm.Usage{InputTokens: 100, OutputTokens: 10}
			deps := finalContinueDeps(func(_ context.Context, req llm.CompletionRequest) (llm.Message, string, llm.Usage, error) {
				calls++
				if calls > 5 {
					t.Fatal("continuation did not terminate")
				}
				names := map[string]bool{}
				for _, schema := range req.Tools {
					names[schema.Name] = true
				}
				if !names["ordinary"] || !names[tool.SearchExtraToolsName] || !names[tool.ExecuteExtraToolName] || names["deferred"] {
					t.Fatalf("call %d changed ordinary/deferred tool availability: %v", calls, names)
				}
				if calls > 3 {
					for _, msg := range req.Messages {
						if msg.Text() == prompt {
							t.Fatal("recovery prompt leaked into a later model request")
						}
					}
				}
				switch calls {
				case 1:
					return finalContinueCall("before", "ordinary", `{}`), "tool_use", usage, nil
				case 2:
					return finalContinueThinking(), "", usage, nil
				case 3:
					last := req.Messages[len(req.Messages)-1]
					prompt = last.Text()
					if last.Role != llm.RoleUser || strings.TrimSpace(prompt) == "" || prompt == "start fixture" {
						t.Fatal("empty reply did not receive an explicit recovery prompt")
					}
					raw, _ := json.Marshal(req.Messages)
					if !strings.Contains(string(raw), "ordinary receipt 1") {
						t.Fatal("recovery lost the completed tool's receipt")
					}
					return finalContinueCall("resumed", tool.ExecuteExtraToolName, `{"tool_name":"deferred","params":{}}`), "tool_use", usage, nil
				case 4:
					return finalContinueCall("continued", "ordinary", `{}`), "tool_use", usage, nil
				default:
					return finalContinueText("fixture finished"), "end_turn", usage, nil
				}
			})
			term := drain(t, QueryInput{Messages: []llm.Message{llm.UserText("start fixture")}, Tools: reg, DeferredTools: []string{"deferred"}, PermissionMode: permission.ModeBypass, RequireFinalText: true, ContinueAfterEmptyReply: true, NonStreaming: syncMode, Recorder: recorder, Compactor: compactor}, deps)
			if calls != 5 || ordinaryEffects.Load() != 2 || deferredEffects.Load() != 1 || term.Reason != ReasonCompleted || term.Text != "fixture finished" || !term.SummaryRecoveryAttempted || term.SummaryMissing || term.Err != nil || term.Usage.InputTokens != 500 {
				t.Fatalf("calls=%d effects=%d/%d terminal=%+v", calls, ordinaryEffects.Load(), deferredEffects.Load(), term)
			}
			for name, messages := range map[string][]llm.Message{"terminal": term.Messages, "recorder": recorder.msgs, "compactor": compactor.seen} {
				for _, msg := range messages {
					if msg.Text() == prompt {
						t.Fatalf("request-only recovery prompt leaked into %s", name)
					}
				}
			}
			callsByID, receiptsByID := map[string]int{}, map[string]int{}
			for _, msg := range term.Messages {
				for _, block := range msg.Content {
					if block.Type == llm.BlockToolUse {
						callsByID[block.ID]++
					}
					if block.Type == llm.BlockToolResult {
						receiptsByID[block.ToolUseID]++
						if block.IsError {
							t.Fatalf("resumed tool was rejected: %+v", block)
						}
					}
				}
			}
			for _, id := range []string{"before", "resumed", "continued"} {
				if callsByID[id] != 1 || receiptsByID[id] != 1 {
					t.Fatalf("effect %s was replayed or lost: calls=%v receipts=%v", id, callsByID, receiptsByID)
				}
			}
		})
	}
}

func TestFinalTextContinueOnlyOneReminder(t *testing.T) {
	for _, syncMode := range []bool{false, true} {
		for _, resumeWithTool := range []bool{false, true} {
			t.Run(fmt.Sprintf("sync=%v/tool=%v", syncMode, resumeWithTool), func(t *testing.T) {
				var effects atomic.Int32
				calls := 0
				term := drain(t, QueryInput{Tools: tool.NewRegistry(finalContinueProbe("probe", &effects)), PermissionMode: permission.ModeBypass, RequireFinalText: true, ContinueAfterEmptyReply: true, NonStreaming: syncMode}, finalContinueDeps(func(context.Context, llm.CompletionRequest) (llm.Message, string, llm.Usage, error) {
					calls++
					if calls > 3 {
						t.Fatal("second empty reply received another recovery")
					}
					if calls == 2 && resumeWithTool {
						return finalContinueCall("once", "probe", `{}`), "tool_use", llm.Usage{}, nil
					}
					return finalContinueThinking(), "", llm.Usage{}, nil
				}))
				wantCalls, wantEffects := 2, int32(0)
				if resumeWithTool {
					wantCalls, wantEffects = 3, 1
				}
				if calls != wantCalls || effects.Load() != wantEffects || !term.SummaryRecoveryAttempted || !term.SummaryMissing || term.Reason != ReasonCompleted {
					t.Fatalf("calls=%d effects=%d terminal=%+v", calls, effects.Load(), term)
				}
			})
		}
	}
}

func TestFinalTextContinueBudgetsAndOptIn(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   QueryInput
		want int
	}{
		{"not_required", QueryInput{ContinueAfterEmptyReply: true}, 1},
		{"local_limit", QueryInput{RequireFinalText: true, ContinueAfterEmptyReply: true, MaxTurns: 1}, 1},
		{"elapsed_limit", QueryInput{RequireFinalText: true, ContinueAfterEmptyReply: true, MaxDuration: time.Nanosecond}, 1},
		{"durable_limit", QueryInput{RequireFinalText: true, ContinueAfterEmptyReply: true, TurnBudget: &finalTextBudget{remaining: 1}}, 1},
		{"durable_recovery", QueryInput{RequireFinalText: true, ContinueAfterEmptyReply: true, TurnBudget: &finalTextBudget{remaining: 2}}, 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			term := drain(t, tc.in, finalContinueDeps(func(context.Context, llm.CompletionRequest) (llm.Message, string, llm.Usage, error) {
				calls++
				if calls > 2 {
					t.Fatal("budget failed to bound recovery")
				}
				return finalContinueThinking(), "", llm.Usage{OutputTokens: 1}, nil
			}))
			if calls != tc.want || term.SummaryRecoveryAttempted != (tc.want == 2) {
				t.Fatalf("calls=%d terminal=%+v", calls, term)
			}
			if budget, ok := tc.in.TurnBudget.(*finalTextBudget); ok && budget.accounted != calls {
				t.Fatalf("budget accounted %d of %d actual attempts", budget.accounted, calls)
			}
		})
	}
}

func TestFinalTextContinueSettlementStillDisablesTools(t *testing.T) {
	for _, syncMode := range []bool{false, true} {
		t.Run(fmt.Sprintf("sync=%v", syncMode), func(t *testing.T) {
			var effects atomic.Int32
			calls := 0
			term := drain(t, QueryInput{Tools: tool.NewRegistry(finalContinueProbe("probe", &effects)), PermissionMode: permission.ModeBypass, RequireFinalText: true, ContinueAfterEmptyReply: true, NonStreaming: syncMode, MaxTurns: 1, Settlement: &Settlement{Prompt: "fixture budget exhausted", MaxTurns: 3}}, finalContinueDeps(func(_ context.Context, req llm.CompletionRequest) (llm.Message, string, llm.Usage, error) {
				calls++
				if calls > 3 {
					t.Fatal("settlement recovery escaped its bound")
				}
				if calls == 2 {
					return finalContinueThinking(), "", llm.Usage{}, nil
				}
				if calls == 3 && len(req.Tools) != 0 {
					t.Fatal("budget settlement recovery advertised tools")
				}
				return finalContinueCall(fmt.Sprint(calls), "probe", `{}`), "tool_use", llm.Usage{}, nil
			}))
			raw, _ := json.Marshal(term.Messages)
			if calls != 3 || effects.Load() != 1 || term.Reason != ReasonMaxTurns || !term.SummaryRecoveryAttempted || !term.SummaryMissing || !strings.Contains(string(raw), "summary_only") {
				t.Fatalf("calls=%d effects=%d terminal=%+v", calls, effects.Load(), term)
			}
		})
	}
}

func TestFinalTextContinuePropagatesModelFailure(t *testing.T) {
	for _, syncMode := range []bool{false, true} {
		for _, failAt := range []int{2, 3} {
			t.Run(fmt.Sprintf("sync=%v/fail_at=%d", syncMode, failAt), func(t *testing.T) {
				var effects atomic.Int32
				calls := 0
				failure := errors.New("synthetic resumed model failure")
				term := drain(t, QueryInput{Tools: tool.NewRegistry(finalContinueProbe("probe", &effects)), PermissionMode: permission.ModeBypass, RequireFinalText: true, ContinueAfterEmptyReply: true, NonStreaming: syncMode}, finalContinueDeps(func(context.Context, llm.CompletionRequest) (llm.Message, string, llm.Usage, error) {
					calls++
					if calls > failAt {
						t.Fatal("model failure retried earlier tool effects")
					}
					if calls == failAt {
						return llm.Message{}, "", llm.Usage{}, failure
					}
					if calls == 2 {
						return finalContinueCall("saved", "probe", `{}`), "tool_use", llm.Usage{}, nil
					}
					return finalContinueThinking(), "", llm.Usage{}, nil
				}))
				if calls != failAt || effects.Load() != int32(failAt-2) || term.Reason != ReasonModelError || !errors.Is(term.Err, failure) || term.SummaryRecoveryFailed != (failAt == 2) {
					t.Fatalf("calls=%d effects=%d terminal=%+v", calls, effects.Load(), term)
				}
			})
		}
	}
}

type finalContinueSettlementBudget struct{ reservations, accounted int }

func (b *finalContinueSettlementBudget) BeforeTurn(_ context.Context, settling bool) (TurnDecision, error) {
	b.reservations++
	if b.reservations == 1 || settling && b.accounted < 4 {
		return TurnDecision{Allowed: true}, nil
	}
	return TurnDecision{Settle: !settling}, nil
}

func (b *finalContinueSettlementBudget) AfterTurn(context.Context, llm.Usage) error {
	b.accounted++
	return nil
}

func TestFinalTextContinueBudgetExpiresBeforeRecovery(t *testing.T) {
	for _, syncMode := range []bool{false, true} {
		t.Run(fmt.Sprintf("sync=%v", syncMode), func(t *testing.T) {
			var effects atomic.Int32
			calls := 0
			budget := &finalContinueSettlementBudget{}
			term := drain(t, QueryInput{Tools: tool.NewRegistry(finalContinueProbe("probe", &effects)), PermissionMode: permission.ModeBypass, RequireFinalText: true, ContinueAfterEmptyReply: true, NonStreaming: syncMode, TurnBudget: budget, Settlement: &Settlement{Prompt: "fixture budget exhausted", MaxTurns: 3}}, finalContinueDeps(func(_ context.Context, req llm.CompletionRequest) (llm.Message, string, llm.Usage, error) {
				calls++
				if calls == 1 || calls == 3 {
					return finalContinueThinking(), "", llm.Usage{}, nil
				}
				if calls > 4 {
					t.Fatal("durable budget allowed additional work")
				}
				if calls == 2 {
					if len(req.Tools) == 0 {
						t.Error("entering settlement disabled an allowed checkpoint tool")
					}
					for _, msg := range req.Messages {
						if msg.Role == llm.RoleUser && msg.Text() != "fixture budget exhausted" {
							t.Error("pending continuation prompt survived entry into settlement")
						}
					}
					return finalContinueCall("checkpoint", "probe", `{}`), "tool_use", llm.Usage{}, nil
				}
				if len(req.Tools) != 0 {
					t.Error("empty settlement reply entered ordinary continuation instead of text-only recovery")
				}
				return finalContinueCall("must-not-run", "probe", `{}`), "tool_use", llm.Usage{}, nil
			}))
			if calls != 4 || effects.Load() != 1 || budget.accounted != 4 || term.Reason != ReasonMaxTurns || !term.SummaryRecoveryAttempted {
				t.Fatalf("calls=%d effects=%d budget=%+v terminal=%+v", calls, effects.Load(), budget, term)
			}
		})
	}
}

func TestFinalTextContinueDoesNotExtendLocalBudget(t *testing.T) {
	var effects atomic.Int32
	calls := 0
	term := drain(t, QueryInput{Tools: tool.NewRegistry(finalContinueProbe("probe", &effects)), PermissionMode: permission.ModeBypass, RequireFinalText: true, ContinueAfterEmptyReply: true, MaxTurns: 2}, finalContinueDeps(func(context.Context, llm.CompletionRequest) (llm.Message, string, llm.Usage, error) {
		calls++
		if calls == 1 {
			return finalContinueThinking(), "", llm.Usage{}, nil
		}
		if calls > 2 {
			t.Fatal("recovery extended the configured turn limit")
		}
		return finalContinueCall("allowed-once", "probe", `{}`), "tool_use", llm.Usage{}, nil
	}))
	if calls != 2 || effects.Load() != 1 || term.Reason != ReasonMaxTurns || !term.SummaryRecoveryAttempted {
		t.Fatalf("calls=%d effects=%d terminal=%+v", calls, effects.Load(), term)
	}
}

func TestFinalTextContinueCancellationStopsRecovery(t *testing.T) {
	for _, syncMode := range []bool{false, true} {
		for _, cancelAt := range []int{1, 2} {
			t.Run(fmt.Sprintf("sync=%v/cancel_at=%d", syncMode, cancelAt), func(t *testing.T) {
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				calls := 0
				var term *Terminal
				deps := finalContinueDeps(func(context.Context, llm.CompletionRequest) (llm.Message, string, llm.Usage, error) {
					calls++
					if calls > cancelAt {
						t.Fatal("model called after cancellation")
					}
					if calls == cancelAt {
						cancel()
						return llm.Message{}, "", llm.Usage{}, ctx.Err()
					}
					return finalContinueThinking(), "", llm.Usage{}, nil
				})
				for ev, err := range Query(ctx, QueryInput{RequireFinalText: true, ContinueAfterEmptyReply: true, NonStreaming: syncMode}, deps) {
					if err != nil {
						t.Fatal(err)
					}
					if ev.Terminal != nil {
						term = ev.Terminal
					}
				}
				if calls != cancelAt || term == nil || term.Reason != ReasonAbortedStreaming || !errors.Is(term.Err, context.Canceled) || term.SummaryRecoveryAttempted != (cancelAt == 2) {
					t.Fatalf("calls=%d terminal=%+v", calls, term)
				}
			})
		}
	}
}

func TestFinalTextContinueConcurrentModesAreIsolated(t *testing.T) {
	for run := 0; run < 16; run++ {
		t.Run(fmt.Sprint(run), func(t *testing.T) {
			t.Parallel()
			continueWork, syncMode := run%2 == 0, run%4 < 2
			var effects atomic.Int32
			calls := 0
			term := drain(t, QueryInput{Tools: tool.NewRegistry(finalContinueProbe("probe", &effects)), PermissionMode: permission.ModeBypass, RequireFinalText: true, ContinueAfterEmptyReply: continueWork, NonStreaming: syncMode}, finalContinueDeps(func(_ context.Context, req llm.CompletionRequest) (llm.Message, string, llm.Usage, error) {
				calls++
				if calls > 3 {
					t.Fatal("unbounded recovery")
				}
				if calls == 1 {
					return finalContinueThinking(), "", llm.Usage{}, nil
				}
				if calls == 2 {
					if (len(req.Tools) > 0) != continueWork {
						t.Fatal("another query's recovery policy changed this request")
					}
					return finalContinueCall("probe-once", "probe", `{}`), "tool_use", llm.Usage{}, nil
				}
				return finalContinueText("done"), "end_turn", llm.Usage{}, nil
			}))
			wantCalls, wantEffects := 2, int32(0)
			if continueWork {
				wantCalls, wantEffects = 3, 1
			}
			if calls != wantCalls || effects.Load() != wantEffects || !term.SummaryRecoveryAttempted || term.SummaryMissing == continueWork {
				t.Fatalf("calls=%d effects=%d terminal=%+v", calls, effects.Load(), term)
			}
		})
	}
}
