package server

import (
	"context"
	"errors"
	"fmt"
	"iter"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/RC-CHN/ARTEX/agent"
	"github.com/RC-CHN/ARTEX/db"
	"github.com/RC-CHN/ARTEX/norma/harness"
	"github.com/RC-CHN/ARTEX/norma/llm"
)

func TestIncompleteResponseDoesNotReplayWholeWorkerIntent(t *testing.T) {
	err := fmt.Errorf("provider ended incompletely: %w", &llm.IncompleteResponseError{Reason: "missing_terminal"})
	if retryableWorkerModelError(harness.ReasonIncompleteResponse, err) {
		t.Fatal("an incomplete response must not replay prior tool effects from the whole intent")
	}
	if !retryableWorkerModelError(harness.ReasonModelError, errors.New("temporary network error")) {
		t.Fatal("ordinary model failures lost their existing retry behavior")
	}
}

func TestAnthropicIncompleteQuotaAdvancesNextRunWithoutReplaying(t *testing.T) {
	for _, nonstream := range []bool{false, true} {
		for _, persistenceFailure := range []bool{false, true} {
			t.Run(fmt.Sprintf("nonstream=%v/persistenceFailure=%v", nonstream, persistenceFailure), func(t *testing.T) {
				calls := 0
				srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					calls++
					if nonstream {
						w.Header().Set("Content-Type", "application/json")
						if calls == 1 {
							fmt.Fprint(w, `{"type":"message","content":[{"type":"thinking","thinking":"synthetic"}],"stop_reason":"end_turn","usage":{"input_tokens":11,"output_tokens":3}}`)
						} else {
							w.WriteHeader(http.StatusPaymentRequired)
							fmt.Fprint(w, `{"error":{"message":"insufficient_quota SYNTHETIC_PRIVATE_DIAGNOSTIC"}}`)
						}
						return
					}
					w.Header().Set("Content-Type", "text/event-stream")
					fmt.Fprint(w, "data: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":11}}}\n\n")
					fmt.Fprint(w, "data: {\"type\":\"content_block_delta\",\"delta\":{\"type\":\"thinking_delta\",\"thinking\":\"synthetic\"}}\n\n")
					fmt.Fprint(w, "data: {\"type\":\"error\",\"error\":{\"message\":\"insufficient_quota SYNTHETIC_PRIVATE_DIAGNOSTIC\"}}\n\n")
				}))
				defer srv.Close()
				p, err := llm.NewProvider(llm.Config{Format: llm.FormatAnthropic, BaseURL: srv.URL, Model: "synthetic", MaxRetries: -1, EmptyResponseRetries: 1, EmptyResponseInterval: time.Nanosecond})
				if err != nil {
					t.Fatal(err)
				}
				selections, marks, transitions := 0, 0, 0
				next := int64(12)
				hooks := taskLLMStreamHooks{
					current: func() (taskLLMSelection, error) {
						selections++
						if selections > 1 {
							return taskLLMSelection{}, errors.New("must not replay on next profile")
						}
						return taskLLMSelection{profileID: 11, provider: p}, nil
					},
					exhaust: func(_ taskLLMSelection, cause error) (db.TaskLLMTransition, error) {
						marks++
						if !isQuotaExhaustedError(cause) || strings.Contains(cause.Error(), "SYNTHETIC_PRIVATE_DIAGNOSTIC") {
							t.Fatal("quota classification lost or private upstream error exposed")
						}
						if persistenceFailure {
							return db.TaskLLMTransition{}, errors.New("synthetic persistence failure")
						}
						return db.TaskLLMTransition{Advanced: true, NextProfileID: &next}, nil
					},
					transition: func(taskLLMSelection, db.TaskLLMTransition, error) { transitions++ },
				}
				var usage llm.Usage
				if nonstream {
					_, _, usage, err = completeTaskLLM(t.Context(), "synthetic", llm.CompletionRequest{}, hooks)
				} else {
					acc := llm.NewAccumulator()
					for ev, streamErr := range streamTaskLLM(t.Context(), "synthetic", llm.CompletionRequest{}, hooks) {
						if streamErr != nil {
							err = streamErr
							break
						}
						acc.Add(ev)
					}
					usage = acc.Usage
				}
				wantCalls, wantTransitions := 1, 1
				if nonstream {
					wantCalls = 2
				}
				if persistenceFailure {
					wantTransitions = 0
				}
				if !isIncompleteModelResponse(err) || calls != wantCalls || selections != 1 || marks != 1 || transitions != wantTransitions || usage.InputTokens != 11 {
					t.Fatalf("err=%v calls=%d selections=%d marks=%d transitions=%d usage=%+v", err, calls, selections, marks, transitions, usage)
				}
			})
		}
	}
}

type incompleteTaskProvider struct {
	calls int
	err   error
}

func (p *incompleteTaskProvider) Stream(context.Context, llm.CompletionRequest) iter.Seq2[llm.StreamEvent, error] {
	return func(yield func(llm.StreamEvent, error) bool) {
		p.calls++
		if !yield(llm.StreamEvent{Type: llm.SEMessageStart, Usage: llm.Usage{InputTokens: 11, OutputTokens: 3}}, nil) {
			return
		}
		if !yield(llm.StreamEvent{Type: llm.SEStreamDiagnostics, Diagnostics: &llm.StreamDiagnostics{StreamEnd: llm.StreamEndProtocolError, ParseErrors: 1}}, nil) {
			return
		}
		yield(llm.StreamEvent{}, p.err)
	}
}

func (p *incompleteTaskProvider) Complete(context.Context, llm.CompletionRequest) (llm.Message, string, llm.Usage, error) {
	p.calls++
	return llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{{Type: llm.BlockText, Text: "synthetic partial answer"}}}, "length", llm.Usage{InputTokens: 11, OutputTokens: 3}, p.err
}

func TestTaskIncompleteResponseDoesNotMultiplyAdapterRetries(t *testing.T) {
	for _, nonstream := range []bool{false, true} {
		t.Run(fmt.Sprintf("nonstream=%v", nonstream), func(t *testing.T) {
			cause := &llm.IncompleteResponseError{Reason: "malformed_frame"}
			p := &incompleteTaskProvider{err: fmt.Errorf("synthetic provider: %w", cause)}
			selections, transitions := 0, 0
			hooks := taskLLMStreamHooks{
				current: func() (taskLLMSelection, error) {
					selections++
					return taskLLMSelection{profileID: 11, provider: p, retry: agent.RetryConfig{StreamAttempts: 4, StreamInterval: time.Nanosecond}}, nil
				},
				exhaust: func(taskLLMSelection, error) (db.TaskLLMTransition, error) {
					transitions++
					return db.TaskLLMTransition{}, errors.New("incomplete response must not switch task profile")
				},
			}
			ctx, cancel := context.WithTimeout(t.Context(), time.Second)
			defer cancel()
			if nonstream {
				message, stop, usage, err := completeTaskLLM(ctx, "synthetic", llm.CompletionRequest{}, hooks)
				if !errors.Is(err, cause) || message.Text() != "synthetic partial answer" || stop != "length" || usage.InputTokens != 11 || usage.OutputTokens != 3 {
					t.Fatalf("incomplete Complete lost evidence: text=%q stop=%q usage=%+v err=%v", message.Text(), stop, usage, err)
				}
			} else {
				events, err := collectTaskLLMStream(streamTaskLLM(ctx, "synthetic", llm.CompletionRequest{}, hooks))
				if !errors.Is(err, cause) || len(events) != 2 || events[0].Usage.InputTokens != 11 || events[1].Diagnostics == nil {
					t.Fatalf("incomplete stream lost diagnostics/usage: events=%+v err=%v", events, err)
				}
			}
			if p.calls != 1 || selections != 1 || transitions != 0 {
				t.Fatalf("adapter recovery repeated outside provider: calls=%d selections=%d transitions=%d", p.calls, selections, transitions)
			}
		})
	}
}
