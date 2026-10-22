package harness

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/RC-CHN/ARTEX/norma/llm"
	"github.com/RC-CHN/ARTEX/norma/permission"
	"github.com/RC-CHN/ARTEX/norma/tool"
	"iter"
	"strings"
	"sync/atomic"
	"testing"
)

func TestFinalTextRecoveryBoundedAndNeverReplaysTools(t *testing.T) {
	for _, syncMode := range []bool{false, true} {
		for _, ending := range []string{"text", "thinking", "tool", "error", "max_tokens"} {
			t.Run(fmt.Sprintf("sync=%v/%s", syncMode, ending), func(t *testing.T) {
				var executed atomic.Int32
				probe := tool.Build(tool.Spec{Name: "probe", Schema: map[string]any{"type": "object"}, Run: func(context.Context, json.RawMessage, *tool.ToolContext) (tool.Result, error) {
					executed.Add(1)
					return tool.Text("saved fixture #1"), nil
				}})
				calls := 0
				model := func(_ context.Context, r llm.CompletionRequest) (llm.Message, string, llm.Usage, error) {
					calls++
					if calls > 3 {
						t.Fatal("unbounded recovery")
					}
					u := llm.Usage{InputTokens: 100, OutputTokens: 10}
					call := llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{{Type: llm.BlockToolUse, ID: fmt.Sprint(calls), Name: "probe", Input: json.RawMessage(`{}`)}}}
					thinking := llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{{Type: llm.BlockThinking, Thinking: "synthetic"}}}
					if calls == 1 {
						return call, "tool_use", u, nil
					}
					if calls == 2 {
						return thinking, "end_turn", u, nil
					}
					if len(r.Tools) != 0 {
						t.Fatal("recovery advertises tools")
					}
					raw, _ := json.Marshal(r.Messages)
					if !strings.Contains(string(raw), "saved fixture #1") {
						t.Fatal("prior tool receipt missing")
					}
					switch ending {
					case "text":
						return llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{llm.TextBlock("saved #1; scope incomplete")}}, "end_turn", u, nil
					case "tool":
						return call, "tool_use", u, nil
					case "error":
						return llm.Message{}, "", u, fmt.Errorf("synthetic provider failure")
					case "max_tokens":
						return thinking, "max_tokens", u, nil
					}
					return thinking, "end_turn", u, nil
				}
				stream := func(ctx context.Context, r llm.CompletionRequest) iter.Seq2[llm.StreamEvent, error] {
					return func(y func(llm.StreamEvent, error) bool) {
						m, s, u, e := model(ctx, r)
						if e != nil {
							y(llm.StreamEvent{}, e)
							return
						}
						for _, b := range m.Content {
							switch b.Type {
							case llm.BlockText:
								if !y(llm.StreamEvent{Type: llm.SETextDelta, Text: b.Text}, nil) {
									return
								}
							case llm.BlockThinking:
								if !y(llm.StreamEvent{Type: llm.SEThinkingDelta, Text: b.Thinking}, nil) {
									return
								}
							case llm.BlockToolUse:
								if !y(llm.StreamEvent{Type: llm.SEToolUseStart, ToolID: b.ID, ToolName: b.Name}, nil) {
									return
								}
								if !y(llm.StreamEvent{Type: llm.SEToolInputJSON, Text: string(b.Input)}, nil) {
									return
								}
							}
						}
						if !y(llm.StreamEvent{Type: llm.SEMessageDelta, StopReason: s, Usage: u}, nil) {
							return
						}
						y(llm.StreamEvent{Type: llm.SEMessageStop}, nil)
					}
				}
				term := drain(t, QueryInput{Tools: tool.NewRegistry(probe), PermissionMode: permission.ModeBypass, RequireFinalText: true, NonStreaming: syncMode}, QueryDeps{CallModel: stream, CallModelSync: model})
				if calls != 3 || executed.Load() != 1 || !term.SummaryRecoveryAttempted || term.SummaryMissing != (ending != "text") {
					t.Fatalf("calls=%d effects=%d terminal=%+v", calls, executed.Load(), term)
				}
				if ending == "error" && (term.Err != nil || term.Reason != ReasonCompleted || !term.SummaryRecoveryFailed) {
					t.Fatal("summary failure can retry prior work", term)
				}
				if ending != "error" && term.Usage.InputTokens != 300 {
					t.Fatalf("usage=%+v", term.Usage)
				}
				if ending == "tool" {
					raw, _ := json.Marshal(term.Messages)
					if !strings.Contains(string(raw), "summary_only") {
						t.Fatal("missing tool rejection")
					}
				}
			})
		}
	}
}

type finalTextBudget struct{ remaining, accounted int }

func (b *finalTextBudget) BeforeTurn(context.Context, bool) (TurnDecision, error) {
	if b.remaining == 0 {
		return TurnDecision{}, nil
	}
	b.remaining--
	return TurnDecision{Allowed: true}, nil
}
func (b *finalTextBudget) AfterTurn(context.Context, llm.Usage) error { b.accounted++; return nil }

func TestFinalTextRecoveryRespectsBudgetsAndOptIn(t *testing.T) {
	for _, tc := range []struct {
		name    string
		enabled bool
		max     int
		budget  *finalTextBudget
		want    int
	}{{"disabled", false, 0, nil, 1}, {"local_limit", true, 1, nil, 1}, {"durable_limit", true, 0, &finalTextBudget{remaining: 1}, 1}, {"recover_once", true, 0, nil, 2}} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			in := QueryInput{RequireFinalText: tc.enabled, MaxTurns: tc.max}
			if tc.budget != nil {
				in.TurnBudget = tc.budget
			}
			term := drain(t, in, QueryDeps{CallModel: func(context.Context, llm.CompletionRequest) iter.Seq2[llm.StreamEvent, error] {
				calls++
				return func(y func(llm.StreamEvent, error) bool) {
					y(llm.StreamEvent{Type: llm.SEThinkingDelta, Text: "synthetic"}, nil)
					y(llm.StreamEvent{Type: llm.SEMessageStop}, nil)
				}
			}})
			if calls != tc.want || term.SummaryRecoveryAttempted != (tc.want == 2) {
				t.Fatalf("calls=%d terminal=%+v", calls, term)
			}
			if tc.budget != nil && tc.budget.accounted != 1 {
				t.Fatal("budget accounting")
			}
		})
	}
}

func TestFinalTextRecoveryNoExtraOnCancellationOrNormalText(t *testing.T) {
	for _, cancelled := range []bool{false, true} {
		ctx, cancel := context.WithCancel(t.Context())
		calls := 0
		var term *Terminal
		for ev, err := range Query(ctx, QueryInput{RequireFinalText: true}, QueryDeps{CallModel: func(context.Context, llm.CompletionRequest) iter.Seq2[llm.StreamEvent, error] {
			calls++
			return func(y func(llm.StreamEvent, error) bool) {
				if cancelled {
					cancel()
					y(llm.StreamEvent{}, ctx.Err())
					return
				}
				for _, ev := range textTurn("done") {
					if !y(ev, nil) {
						return
					}
				}
			}
		}}) {
			if err != nil {
				t.Fatal(err)
			}
			if ev.Terminal != nil {
				term = ev.Terminal
			}
		}
		cancel()
		if calls != 1 || term == nil || term.SummaryRecoveryAttempted {
			t.Fatalf("calls=%d term=%+v", calls, term)
		}
	}
}
