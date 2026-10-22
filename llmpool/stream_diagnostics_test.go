package llmpool

import (
	"context"
	"errors"
	"fmt"
	"iter"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sync/atomic"
	"testing"

	"github.com/RC-CHN/ARTEX/norma/llm"
)

func TestOpenAIDiagnosticsPreservePreResponseFailover(t *testing.T) {
	for _, status := range []int{401, 402, 429} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			var primaryCalls, fallbackCalls atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/primary/chat/completions":
					primaryCalls.Add(1)
					w.WriteHeader(status)
					_, _ = fmt.Fprint(w, `{"error":"synthetic unavailable profile"}`)
				case "/fallback/chat/completions":
					fallbackCalls.Add(1)
					w.Header().Set("Content-Type", "text/event-stream")
					_, _ = fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"backup answer\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
				default:
					http.NotFound(w, r)
				}
			}))
			defer srv.Close()
			provider := func(path string) llm.Provider {
				t.Helper()
				p, err := llm.NewProvider(llm.Config{Format: llm.FormatOpenAI, BaseURL: srv.URL + path, Model: "synthetic", HTTPClient: srv.Client(), MaxRetries: -1, EmptyResponseRetries: -1})
				if err != nil {
					t.Fatal(err)
				}
				return p
			}
			health := NewRegistry(nil, nil)
			pool := New([]*Member{member(1, "primary", 10, provider("/primary")), member(2, "fallback", 5, provider("/fallback"))}, health)
			acc := llm.NewAccumulator()
			var diagnostics []llm.StreamDiagnostics
			for event, err := range pool.Stream(t.Context(), llm.CompletionRequest{}) {
				if err != nil {
					t.Fatalf("diagnostics prevented failover: %v", err)
				}
				if event.Type == llm.SEStreamDiagnostics {
					if event.Diagnostics == nil {
						t.Fatal("missing diagnostic payload")
					}
					diagnostics = append(diagnostics, *event.Diagnostics)
				}
				acc.Add(event)
			}
			message := acc.Message()
			if primaryCalls.Load() != 1 || fallbackCalls.Load() != 1 || message.Text() != "backup answer" || len(message.ToolUses()) != 0 || acc.StopReason != "end_turn" {
				t.Fatalf("unexpected failover: primary=%d fallback=%d message=%+v stop=%q", primaryCalls.Load(), fallbackCalls.Load(), message, acc.StopReason)
			}
			want := []llm.StreamDiagnostics{{StreamEnd: llm.StreamEndRequestError}, {StreamEnd: llm.StreamEndDone, FinishReasonSeen: true}}
			if !reflect.DeepEqual(diagnostics, want) {
				t.Fatalf("attempt diagnostics changed: %+v", diagnostics)
			}
			wantFails, wantTrips := 0, 1
			if status == 429 {
				wantFails, wantTrips = 1, 0
			}
			if health.Get(1).Fails != wantFails || health.Get(1).Trips != wantTrips || health.IsOpen(1) != (status != 429) || health.Get(2) != (State{}) {
				t.Fatalf("failover health changed: primary=%+v fallback=%+v", health.Get(1), health.Get(2))
			}
		})
	}
}

func TestDiagnosticsDoNotResetCommittedStreamBoundary(t *testing.T) {
	for _, kind := range []llm.StreamEventType{llm.SETextDelta, llm.SEThinkingDelta, llm.SEToolUseStart, llm.SEToolInputJSON, llm.SEMessageStart, llm.SEMessageDelta, llm.SEMessageStop, llm.SEThinkingSignature} {
		t.Run(string(kind), func(t *testing.T) {
			diagnostic := llm.StreamEvent{Type: llm.SEStreamDiagnostics, Diagnostics: &llm.StreamDiagnostics{StreamEnd: llm.StreamEndReadError}}
			committed := llm.StreamEvent{Type: kind, Text: "synthetic", ToolID: "call", ToolName: "Write"}
			wantErr := errors.New("provider: status 503: synthetic mid-stream failure")
			primary := &fakeProv{events: [][]llm.StreamEvent{{diagnostic, committed, diagnostic}}, errs: []error{wantErr}}
			fallback := okProv("fallback", "must not replay")
			pool := New([]*Member{member(1, "primary", 10, primary), member(2, "fallback", 5, fallback)}, NewRegistry(nil, nil))
			var seen []llm.StreamEvent
			var streamErr error
			for event, err := range pool.Stream(t.Context(), llm.CompletionRequest{}) {
				if err != nil {
					streamErr = err
					break
				}
				seen = append(seen, event)
			}
			if !errors.Is(streamErr, wantErr) || fallback.calls != 0 || !reflect.DeepEqual(seen, []llm.StreamEvent{diagnostic, committed, diagnostic}) {
				t.Fatalf("committed %s replayed: fallback=%d events=%+v err=%v", kind, fallback.calls, seen, streamErr)
			}
		})
	}
}

func TestDiagnosticConsumerStopDoesNotRetryOrChangeHealth(t *testing.T) {
	closed := false
	primary := callbackProvider{stream: func(context.Context, llm.CompletionRequest) iter.Seq2[llm.StreamEvent, error] {
		return func(yield func(llm.StreamEvent, error) bool) {
			defer func() { closed = true }()
			if !yield(llm.StreamEvent{Type: llm.SEStreamDiagnostics, Diagnostics: &llm.StreamDiagnostics{StreamEnd: llm.StreamEndRequestError}}, nil) {
				return
			}
			yield(llm.StreamEvent{}, errors.New("provider: status 402: synthetic unavailable profile"))
		}
	}}
	fallback := okProv("fallback", "must not start")
	health := NewRegistry(nil, nil)
	health.Trip(1, "previous transient failure", false)
	before := health.Get(1)
	pool := New([]*Member{member(1, "primary", 10, primary), member(2, "fallback", 5, fallback)}, health)
	for event, err := range pool.Stream(t.Context(), llm.CompletionRequest{}) {
		if err != nil || event.Type != llm.SEStreamDiagnostics {
			t.Fatalf("first event=%+v err=%v", event, err)
		}
		break
	}
	if !closed || fallback.calls != 0 || health.Get(1) != before {
		t.Fatalf("diagnostic consumer stop changed state: closed=%v fallback=%d health=%+v", closed, fallback.calls, health.Get(1))
	}
}

func TestCancellationAfterDiagnosticDoesNotStartFallback(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	wantErr := errors.New("provider: status 503: synthetic cancelled transport")
	primary := callbackProvider{stream: func(context.Context, llm.CompletionRequest) iter.Seq2[llm.StreamEvent, error] {
		return func(yield func(llm.StreamEvent, error) bool) {
			if !yield(llm.StreamEvent{Type: llm.SEStreamDiagnostics}, nil) {
				return
			}
			cancel()
			yield(llm.StreamEvent{}, wantErr)
		}
	}}
	fallback := okProv("fallback", "must not start")
	health := NewRegistry(nil, nil)
	pool := New([]*Member{member(1, "primary", 10, primary), member(2, "fallback", 5, fallback)}, health)
	_, err := drain(pool.Stream(ctx, llm.CompletionRequest{}))
	if !errors.Is(err, wantErr) || fallback.calls != 0 || health.Get(1) != (State{}) {
		t.Fatalf("cancelled diagnostic stream retried: fallback=%d health=%+v err=%v", fallback.calls, health.Get(1), err)
	}
}
