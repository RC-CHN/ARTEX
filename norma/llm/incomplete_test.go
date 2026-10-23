package llm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func assertIncomplete(t *testing.T, err error, reason IncompleteResponseReason) *IncompleteResponseError {
	t.Helper()
	var incomplete *IncompleteResponseError
	if !errors.As(err, &incomplete) || incomplete.Reason != reason {
		t.Fatalf("error=%v, want incomplete %s", err, reason)
	}
	return incomplete
}

func collectIncomplete(t *testing.T, p Provider, reason IncompleteResponseReason) Message {
	t.Helper()
	acc := NewAccumulator()
	var streamErr error
	for ev, err := range p.Stream(t.Context(), CompletionRequest{Messages: []Message{UserText("synthetic request")}}) {
		if err != nil {
			streamErr = err
			break
		}
		if ev.Type == SEMessageStop || ev.Type == SEMessageDelta {
			t.Fatal("incomplete response exposed a successful terminal boundary")
		}
		acc.Add(ev)
	}
	assertIncomplete(t, streamErr, reason)
	return acc.Message()
}

func fixtureProvider(t *testing.T, body string) Provider {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(srv.Close)
	p, err := NewProvider(Config{Format: FormatOpenAI, BaseURL: srv.URL, HTTPClient: srv.Client(), MaxRetries: -1, EmptyResponseRetries: -1})
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestOpenAITerminalCompatibilityAndUnterminatedSSELine(t *testing.T) {
	content := "data: {\"choices\":[{\"delta\":{\"content\":\"complete\"}}]}\n\n"
	finish := `data: {"choices":[{"delta":{},"finish_reason":"stop"}]}`
	for _, suffix := range []string{"data: [DONE]", finish, finish + "\n\ndata: [DONE]"} {
		t.Run(suffix, func(t *testing.T) {
			msg := collect(t, fixtureProvider(t, content+suffix), CompletionRequest{})
			if msg.Text() != "complete" {
				t.Fatalf("lost final unterminated SSE line: %+v", msg)
			}
		})
	}
	collectIncomplete(t, fixtureProvider(t, content), IncompleteMissingTerminal)
}

func TestOpenAIThinkingRetryPreservesRequestToolsAndUsage(t *testing.T) {
	var requests atomic.Int32
	var mu sync.Mutex
	var bodies []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		mu.Lock()
		bodies = append(bodies, string(raw))
		mu.Unlock()
		if requests.Add(1) == 1 {
			fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"reasoning_content\":\"synthetic plan\"},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":11,\"completion_tokens\":3}}\n\ndata: [DONE]\n\n")
			return
		}
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"write-1\",\"function\":{\"name\":\"Write\",\"arguments\":\"{\\\"path\\\":\\\"RESOURCE.md\\\"}\"}}]}}],\"usage\":{\"prompt_tokens\":13,\"completion_tokens\":5}}\n\ndata: [DONE]\n\n")
	}))
	defer srv.Close()
	p, _ := NewProvider(Config{Format: FormatOpenAI, BaseURL: srv.URL, EmptyResponseRetries: 1, EmptyResponseInterval: time.Nanosecond})
	req := CompletionRequest{Messages: []Message{UserText("synthetic task")}, Tools: []ToolSchema{{Name: "Write"}}}
	acc := NewAccumulator()
	var diagnosticStops []string
	for ev, err := range p.Stream(t.Context(), req) {
		if err != nil {
			t.Fatal(err)
		}
		acc.Add(ev)
		if ev.Type == SEStreamDiagnostics {
			diagnosticStops = append(diagnosticStops, ev.StopReason)
		}
	}
	msg := acc.Message()
	if requests.Load() != 2 || len(msg.ToolUses()) != 1 || msg.ToolUses()[0].Name != "Write" || acc.Usage.InputTokens != 24 || acc.Usage.OutputTokens != 8 {
		t.Fatalf("requests=%d message=%+v usage=%+v", requests.Load(), msg, acc.Usage)
	}
	if len(bodies) != 2 || bodies[0] != bodies[1] || !strings.Contains(bodies[1], `"tools"`) {
		t.Fatal("retry altered prompt or tool availability")
	}
	if !reflect.DeepEqual(diagnosticStops, []string{"end_turn", ""}) || acc.StopReason != "" {
		t.Fatalf("stale previous-attempt finish reason: diagnostics=%v final=%q", diagnosticStops, acc.StopReason)
	}
}

func TestOpenAIIncompleteToolDoesNotExposeDispatchBoundary(t *testing.T) {
	tool := "data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"pending\",\"function\":{\"name\":\"Write\",\"arguments\":\"{\\\"path\\\":\"}}]}}]}\n\n"
	for _, tail := range []string{
		"data: [DONE]\n\n",
		"data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"tool_calls\"}]}\n\n",
		"data: {\"choices\":[{\"delta\":{\"content\":\"must not dispatch tool\"}}]}\n\ndata: [DONE]\n\n",
		"data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":1,\"id\":\"next\",\"function\":{\"name\":\"Read\",\"arguments\":\"{}\"}}]}}]}\n\n",
	} {
		t.Run(tail, func(t *testing.T) {
			var events []StreamEventType
			var streamErr error
			for ev, err := range fixtureProvider(t, tool+tail).Stream(t.Context(), CompletionRequest{}) {
				if err != nil {
					streamErr = err
					break
				}
				events = append(events, ev.Type)
			}
			assertIncomplete(t, streamErr, IncompleteInvalidToolInput)
			if !reflect.DeepEqual(events, []StreamEventType{SEToolUseStart, SEToolInputJSON, SEStreamDiagnostics}) {
				t.Fatalf("unexpected dispatch boundary: %v", events)
			}
		})
	}
}

func TestOpenAIIncompleteNeverRetriesCommittedOutput(t *testing.T) {
	for _, delta := range []string{`{"content":" "}`, `{"content":"partial"}`, `{"tool_calls":[{"index":0,"id":"pending","function":{"name":"Write","arguments":"{}"}}]}`} {
		t.Run(delta, func(t *testing.T) {
			var requests atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests.Add(1)
				fmt.Fprintf(w, "data: {\"choices\":[{\"delta\":%s}]}\n\n", delta)
			}))
			defer srv.Close()
			p, _ := NewProvider(Config{Format: FormatOpenAI, BaseURL: srv.URL, EmptyResponseRetries: 3, EmptyResponseInterval: time.Nanosecond})
			collectIncomplete(t, p, IncompleteMissingTerminal)
			if requests.Load() != 1 {
				t.Fatalf("replayed committed content: requests=%d", requests.Load())
			}
		})
	}
}

func TestOpenAIExplicitUpstreamErrorStopsBeforeDoneWithoutReplay(t *testing.T) {
	frame := func(v string) string { return "data: " + v + "\n\n" }
	thinking := frame(`{"choices":[{"delta":{"reasoning_content":"synthetic thinking"}}]}`)
	text := frame(`{"choices":[{"delta":{"content":"synthetic partial answer"},"finish_reason":"stop"}]}`)
	partialTool := frame(`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"pending","function":{"name":"Write","arguments":"{\"path\":"}}]}}]}`)
	validTool := frame(`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"complete","function":{"name":"Write","arguments":"{}"}}]}}]}`)
	upstreamError := frame(`{"error":{"message":"PRIVATE_SYNTHETIC_DETAIL","type":"request_error","code":"insufficient_quota"},"usage":{"prompt_tokens":7,"completion_tokens":3}}`)
	for _, tc := range []struct {
		name, prefix  string
		retry, cancel bool
		dispatched    int
	}{
		{name: "initial_pure_error"},
		{name: "thinking_only", prefix: thinking},
		{name: "error_after_safe_retry", retry: true},
		{name: "text_and_finish_before_error", prefix: text},
		{name: "partial_tool", prefix: partialTool},
		{name: "valid_tool_still_pending", prefix: validTool},
		{name: "already_dispatched_tool", prefix: validTool + text, dispatched: 1},
		{name: "cancellation_wins", prefix: thinking, cancel: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			var calls atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if calls.Add(1) == 1 && tc.retry {
					fmt.Fprint(w, thinking+frame(`{"choices":[{"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":11,"completion_tokens":2}}`)+frame("[DONE]"))
					return
				}
				fmt.Fprint(w, tc.prefix+upstreamError+frame("[DONE]"))
			}))
			defer srv.Close()
			p, _ := NewProvider(Config{Format: FormatOpenAI, BaseURL: srv.URL, MaxRetries: -1, EmptyResponseRetries: 3, EmptyResponseInterval: time.Nanosecond})
			acc := NewAccumulator()
			var gotErr error
			pendingTool, dispatched := false, 0
			for ev, err := range p.Stream(ctx, CompletionRequest{}) {
				if err != nil {
					gotErr = err
					break
				}
				if ev.Type == SEMessageDelta || ev.Type == SEMessageStop {
					t.Fatal("explicit upstream error exposed successful terminal boundary")
				}
				// Match the harness boundary contract: metadata and argument
				// fragments must never finish a pending tool on failure.
				if pendingTool && ev.Type != SEToolInputJSON && ev.Type != SEStreamDiagnostics {
					dispatched++
					pendingTool = false
				}
				if ev.Type == SEToolUseStart {
					pendingTool = true
				}
				acc.Add(ev)
				if tc.cancel && ev.Type == SEStreamDiagnostics {
					cancel()
				}
			}
			wantCalls, wantInput, wantOutput := int32(1), 7, 3
			if tc.retry {
				wantCalls, wantInput, wantOutput = 2, 18, 5
			}
			if calls.Load() != wantCalls || acc.Usage.InputTokens != wantInput || acc.Usage.OutputTokens != wantOutput || dispatched != tc.dispatched {
				t.Fatalf("calls=%d usage=%+v dispatched=%d", calls.Load(), acc.Usage, dispatched)
			}
			if tc.cancel {
				if !errors.Is(gotErr, context.Canceled) {
					t.Fatalf("cancellation lost precedence: %v", gotErr)
				}
				return
			}
			if tc.name == "initial_pure_error" {
				var incomplete *IncompleteResponseError
				if gotErr == nil || errors.As(gotErr, &incomplete) || !strings.Contains(gotErr.Error(), "insufficient_quota") {
					t.Fatalf("initial API error lost ordinary classification: %v", gotErr)
				}
				return
			}
			incomplete := assertIncomplete(t, gotErr, IncompleteUpstreamError)
			if incomplete.Diagnostics.StreamEnd != StreamEndProtocolError || incomplete.Diagnostics.ParseErrors != 0 {
				t.Fatalf("wrong error-frame diagnostics: %+v", incomplete.Diagnostics)
			}
			cause := errors.Unwrap(incomplete)
			if cause == nil || !strings.Contains(cause.Error(), "insufficient_quota") || strings.Contains(incomplete.Error(), "PRIVATE_SYNTHETIC_DETAIL") {
				t.Fatalf("classification/privacy failed: %v", incomplete)
			}
			raw, _ := json.Marshal(incomplete)
			if strings.Contains(string(raw), "PRIVATE_SYNTHETIC_DETAIL") {
				t.Fatal("private upstream error serialized into diagnostics")
			}
		})
	}
}

func TestOpenAIEmptyRetryCancellationPreservesUsage(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	var requests atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"reasoning_content\":\"synthetic plan\"},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":17,\"completion_tokens\":2}}\n\ndata: [DONE]\n\n")
	}))
	defer srv.Close()
	p, _ := NewProvider(Config{Format: FormatOpenAI, BaseURL: srv.URL, EmptyResponseInterval: time.Hour})
	acc := NewAccumulator()
	var streamErr error
	for ev, err := range p.Stream(ctx, CompletionRequest{}) {
		if err != nil {
			streamErr = err
			break
		}
		acc.Add(ev)
		if ev.Type == SEStreamDiagnostics {
			cancel()
		}
	}
	if !errors.Is(streamErr, context.Canceled) || requests.Load() != 1 || acc.Usage.InputTokens != 17 || acc.Usage.OutputTokens != 2 {
		t.Fatalf("error=%v requests=%d usage=%+v", streamErr, requests.Load(), acc.Usage)
	}
}

func TestOpenAICompleteThinkingRetryAndIncompleteUsage(t *testing.T) {
	for _, recover := range []bool{false, true} {
		t.Run(fmt.Sprint(recover), func(t *testing.T) {
			var requests atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if requests.Add(1) > 1 && recover {
					fmt.Fprint(w, `{"choices":[{"message":{"tool_calls":[{"id":"write-1","function":{"name":"Write","arguments":"{}"}}]},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":7,"completion_tokens":4}}`)
					return
				}
				fmt.Fprint(w, `{"choices":[{"message":{"reasoning_content":"synthetic plan"},"finish_reason":"stop"}],"usage":{"prompt_tokens":7,"completion_tokens":4}}`)
			}))
			defer srv.Close()
			p, _ := NewProvider(Config{Format: FormatOpenAI, BaseURL: srv.URL, EmptyResponseRetries: 1, EmptyResponseInterval: time.Nanosecond})
			msg, stop, usage, err := p.Complete(t.Context(), CompletionRequest{})
			if requests.Load() != 2 || usage.InputTokens != 14 || usage.OutputTokens != 8 {
				t.Fatalf("requests=%d usage=%+v", requests.Load(), usage)
			}
			if recover {
				if err != nil || len(msg.ToolUses()) != 1 || stop != "tool_use" {
					t.Fatalf("error=%v message=%+v stop=%q", err, msg, stop)
				}
			} else {
				assertIncomplete(t, err, IncompleteEmptyOutput)
				if thinkingOf(msg) == "" || stop != "end_turn" {
					t.Fatal("partial response and protocol evidence discarded")
				}
			}
		})
	}
}

func TestOpenAICompleteInvalidToolIsNeverRetried(t *testing.T) {
	var requests atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		fmt.Fprint(w, `{"choices":[{"message":{"tool_calls":[{"id":"pending","function":{"name":"Write","arguments":"{\"path\":"}}]},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":3,"completion_tokens":2}}`)
	}))
	defer srv.Close()
	p, _ := NewProvider(Config{Format: FormatOpenAI, BaseURL: srv.URL, EmptyResponseRetries: 3, EmptyResponseInterval: time.Nanosecond})
	msg, stop, usage, err := p.Complete(t.Context(), CompletionRequest{})
	assertIncomplete(t, err, IncompleteInvalidToolInput)
	if requests.Load() != 1 || len(msg.ToolUses()) != 1 || string(msg.ToolUses()[0].Input) != `{"path":` || stop != "tool_use" || usage.InputTokens != 3 {
		t.Fatalf("error=%v requests=%d message=%+v stop=%q usage=%+v", err, requests.Load(), msg, stop, usage)
	}
}

func TestIncompleteErrorNeverIncludesUnknownDetails(t *testing.T) {
	err := &IncompleteResponseError{Reason: "private synthetic reason", StopReason: "private synthetic stop"}
	if strings.Contains(err.Error(), "private") {
		t.Fatalf("error includes uncontrolled provider strings: %v", err)
	}
}

func TestOpenAIConcurrentIncompleteRecoveryIsPerRequest(t *testing.T) {
	var mu sync.Mutex
	calls := make(map[string]int)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Messages []struct {
				Content string `json:"content"`
			} `json:"messages"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil || len(body.Messages) == 0 {
			http.Error(w, "bad synthetic request", http.StatusBadRequest)
			return
		}
		key := body.Messages[len(body.Messages)-1].Content
		mu.Lock()
		calls[key]++
		attempt := calls[key]
		mu.Unlock()
		if attempt == 1 {
			fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"reasoning_content\":\"synthetic\"},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":3,\"completion_tokens\":2}}\n\ndata: [DONE]\n\n")
			return
		}
		fmt.Fprintf(w, "data: {\"choices\":[{\"delta\":{\"content\":%q},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":3,\"completion_tokens\":2}}\n\ndata: [DONE]\n\n", key)
	}))
	defer srv.Close()
	p, _ := NewProvider(Config{Format: FormatOpenAI, BaseURL: srv.URL, EmptyResponseRetries: 1, EmptyResponseInterval: time.Nanosecond})
	var wg sync.WaitGroup
	for i := range 16 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			key := fmt.Sprintf("synthetic-%d", i)
			acc := NewAccumulator()
			for ev, err := range p.Stream(t.Context(), CompletionRequest{Messages: []Message{UserText(key)}}) {
				if err != nil {
					t.Errorf("request %s: %v", key, err)
					return
				}
				acc.Add(ev)
			}
			if got := acc.Message().Text(); got != key || acc.Usage.InputTokens != 6 || acc.Usage.OutputTokens != 4 {
				t.Errorf("request %s text=%q usage=%+v", key, got, acc.Usage)
			}
		}()
	}
	wg.Wait()
	mu.Lock()
	defer mu.Unlock()
	if len(calls) != 16 {
		t.Fatalf("request keys=%d", len(calls))
	}
	for key, n := range calls {
		if n != 2 {
			t.Errorf("request %s attempts=%d", key, n)
		}
	}
}

type incompleteRoundTripper func(*http.Request) (*http.Response, error)

func (f incompleteRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

type incompleteCancelBody struct {
	io.Reader
	cancel context.CancelFunc
}

func (b incompleteCancelBody) Close() error { b.cancel(); return nil }

func TestOpenAICompleteCancellationWinsAndPreservesAttemptUsage(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	var requests atomic.Int32
	client := &http.Client{Transport: incompleteRoundTripper(func(*http.Request) (*http.Response, error) {
		requests.Add(1)
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: incompleteCancelBody{
			Reader: strings.NewReader(`{"choices":[{"message":{"reasoning_content":"synthetic"},"finish_reason":"stop"}],"usage":{"prompt_tokens":19,"completion_tokens":5}}`),
			cancel: cancel,
		}}, nil
	})}
	p, _ := NewProvider(Config{Format: FormatOpenAI, BaseURL: "http://synthetic.invalid", HTTPClient: client, EmptyResponseInterval: time.Hour})
	msg, stop, usage, err := p.Complete(ctx, CompletionRequest{})
	if !errors.Is(err, context.Canceled) || requests.Load() != 1 || usage.InputTokens != 19 || usage.OutputTokens != 5 || stop != "end_turn" || thinkingOf(msg) != "synthetic" {
		t.Fatalf("error=%v requests=%d usage=%+v stop=%q message=%+v", err, requests.Load(), usage, stop, msg)
	}
}

func TestOpenAIThinkingInterruptionRetriesWithinSingleBudget(t *testing.T) {
	thinking := "data: {\"choices\":[{\"delta\":{\"reasoning_content\":\"synthetic plan\"}}],\"usage\":{\"prompt_tokens\":2,\"completion_tokens\":1}}\n\n"
	for _, readFailure := range []bool{false, true} {
		for _, recovery := range []string{"text", "tool", "exhausted"} {
			t.Run(fmt.Sprintf("read_error=%t/%s", readFailure, recovery), func(t *testing.T) {
				var requests atomic.Int32
				srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					n := requests.Add(1)
					body := thinking
					interrupted := n == 1 || recovery == "exhausted"
					if !interrupted {
						delta := `{"content":"recovered"}`
						if recovery == "tool" {
							delta = `{"tool_calls":[{"index":0,"id":"write-1","function":{"name":"Write","arguments":"{}"}}]}`
						}
						body = fmt.Sprintf("data: {\"choices\":[{\"delta\":%s}],\"usage\":{\"prompt_tokens\":2,\"completion_tokens\":1}}\n\ndata: [DONE]\n\n", delta)
					}
					if interrupted && readFailure {
						w.Header().Set("Content-Length", fmt.Sprint(len(body)+100))
					}
					fmt.Fprint(w, body)
				}))
				defer srv.Close()
				p, _ := NewProvider(Config{Format: FormatOpenAI, BaseURL: srv.URL, EmptyResponseRetries: 2, EmptyResponseInterval: time.Nanosecond})
				acc := NewAccumulator()
				var streamErr error
				terminal := false
				for ev, err := range p.Stream(t.Context(), CompletionRequest{}) {
					if err != nil {
						streamErr = err
						break
					}
					terminal = terminal || ev.Type == SEMessageStop
					acc.Add(ev)
				}
				wantCalls := int32(2)
				if recovery == "exhausted" {
					wantCalls = 3
					wantReason := IncompleteMissingTerminal
					if readFailure {
						wantReason = IncompleteStreamRead
					}
					assertIncomplete(t, streamErr, wantReason)
					if terminal {
						t.Fatal("exhausted interruption emitted successful stop")
					}
				} else if streamErr != nil || !terminal {
					t.Fatalf("recovery error=%v terminal=%t", streamErr, terminal)
				}
				if requests.Load() != wantCalls || acc.Usage.InputTokens != int(wantCalls)*2 || acc.Usage.OutputTokens != int(wantCalls) {
					t.Fatalf("requests=%d usage=%+v", requests.Load(), acc.Usage)
				}
				msg := acc.Message()
				if recovery == "tool" && len(msg.ToolUses()) != 1 || recovery == "text" && msg.Text() != "recovered" {
					t.Fatalf("recovery message=%+v", msg)
				}
			})
		}
	}
}

func TestOpenAIIncompleteThenRequestFailureRetainsBoundaryAndUsage(t *testing.T) {
	for _, streaming := range []bool{false, true} {
		for _, first := range []string{"empty", "thinking", "eof"} {
			if !streaming && first == "eof" {
				continue
			}
			for _, cancellation := range []bool{false, true} {
				t.Run(fmt.Sprintf("stream=%t/first=%s/cancel=%t", streaming, first, cancellation), func(t *testing.T) {
					ctx, cancel := context.WithCancel(t.Context())
					defer cancel()
					var requests atomic.Int32
					srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						if requests.Add(1) > 1 {
							if cancellation {
								cancel()
							}
							http.Error(w, "private synthetic upstream error", http.StatusBadGateway)
							return
						}
						delta := `{}`
						if first != "empty" {
							delta = `{"reasoning_content":"synthetic plan"}`
						}
						if !streaming {
							fmt.Fprintf(w, `{"choices":[{"message":%s,"finish_reason":"stop"}],"usage":{"prompt_tokens":11,"completion_tokens":3}}`, delta)
						} else if first == "eof" {
							fmt.Fprintf(w, "data: {\"choices\":[{\"delta\":%s}],\"usage\":{\"prompt_tokens\":11,\"completion_tokens\":3}}\n\n", delta)
						} else {
							fmt.Fprintf(w, "data: {\"choices\":[{\"delta\":%s,\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":11,\"completion_tokens\":3}}\n\ndata: [DONE]\n\n", delta)
						}
					}))
					defer srv.Close()
					p, _ := NewProvider(Config{Format: FormatOpenAI, BaseURL: srv.URL, MaxRetries: -1, EmptyResponseRetries: 3, EmptyResponseInterval: time.Nanosecond})
					var msg Message
					var usage Usage
					var stop string
					var callErr error
					if streaming {
						acc := NewAccumulator()
						var lastDiagnostics *StreamDiagnostics
						for ev, err := range p.Stream(ctx, CompletionRequest{}) {
							if err != nil {
								callErr = err
								break
							}
							if ev.Type == SEMessageStop || ev.Type == SEMessageDelta {
								t.Fatal("failed recovery exposed a successful completion")
							}
							if ev.Diagnostics != nil {
								lastDiagnostics = ev.Diagnostics
							}
							acc.Add(ev)
						}
						msg, usage, stop = acc.Message(), acc.Usage, acc.StopReason
						if lastDiagnostics == nil || lastDiagnostics.StreamEnd != StreamEndRequestError || lastDiagnostics.FinishReasonSeen {
							t.Fatalf("diagnostics=%+v", lastDiagnostics)
						}
					} else {
						msg, stop, usage, callErr = p.Complete(ctx, CompletionRequest{})
					}
					if requests.Load() != 2 || usage.InputTokens != 11 || usage.OutputTokens != 3 || stop != "" {
						t.Fatalf("requests=%d usage=%+v stop=%q", requests.Load(), usage, stop)
					}
					if first != "empty" && !streaming && thinkingOf(msg) != "synthetic plan" {
						t.Fatalf("discarded partial response: %+v", msg)
					}
					if streaming && thinkingOf(msg) != "" {
						t.Fatal("failed retry reasoning entered replayable message")
					}
					if cancellation {
						if !errors.Is(callErr, context.Canceled) {
							t.Fatalf("cancellation lost: %v", callErr)
						}
					} else {
						incomplete := assertIncomplete(t, callErr, IncompleteRetryRequest)
						if incomplete.Diagnostics.StreamEnd != StreamEndRequestError || strings.Contains(callErr.Error(), "private") {
							t.Fatalf("unsafe diagnostics/error: %+v", incomplete)
						}
					}
				})
			}
		}
	}
}

func TestOpenAIRetryRequestInternalTimeoutRemainsIncomplete(t *testing.T) {
	for _, err := range []error{context.DeadlineExceeded, context.Canceled, errors.New("private synthetic upstream error")} {
		got := incompleteRetryRequestError(t.Context(), err, 1)
		assertIncomplete(t, got, IncompleteRetryRequest)
		if errors.Unwrap(got) != err {
			t.Fatal("lost machine-readable request failure cause")
		}
		if first := incompleteRetryRequestError(t.Context(), err, 0); first != err {
			t.Fatal("changed first-attempt error behavior")
		}
	}
}

func TestOpenAICompleteIncompleteThenErrorResponse(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		want       IncompleteResponseReason
	}{
		{name: "http_200_error", body: `{"error":{"message":"private synthetic error","type":"upstream_error"}}`, want: IncompleteRetryRequest},
		{name: "malformed_body", body: `{private synthetic invalid JSON`, want: IncompleteMalformedFrame},
		{name: "invalid_tool", body: `{"choices":[{"message":{"tool_calls":[{"id":"pending","function":{"name":"Write","arguments":"{"}}]},"finish_reason":"tool_calls"}]}`, want: IncompleteInvalidToolInput},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var requests atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if requests.Add(1) > 1 {
					fmt.Fprint(w, tc.body)
					return
				}
				fmt.Fprint(w, `{"choices":[{"message":{"reasoning_content":"synthetic plan"},"finish_reason":"stop"}],"usage":{"prompt_tokens":11,"completion_tokens":3}}`)
			}))
			defer srv.Close()
			p, _ := NewProvider(Config{Format: FormatOpenAI, BaseURL: srv.URL, MaxRetries: -1, EmptyResponseRetries: 3, EmptyResponseInterval: time.Nanosecond})
			msg, _, usage, err := p.Complete(t.Context(), CompletionRequest{})
			assertIncomplete(t, err, tc.want)
			if requests.Load() != 2 || usage.InputTokens != 11 || usage.OutputTokens != 3 || strings.Contains(err.Error(), "private") {
				t.Fatalf("requests=%d usage=%+v error=%v", requests.Load(), usage, err)
			}
			if tc.want == IncompleteRetryRequest && thinkingOf(msg) != "synthetic plan" {
				t.Fatalf("lost previous partial response: %+v", msg)
			}
		})
	}
}
