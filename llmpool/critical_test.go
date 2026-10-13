package llmpool

import (
	"context"
	"errors"
	"fmt"
	"iter"
	"reflect"
	"sync/atomic"
	"testing"
	"time"

	"github.com/RC-CHN/ARTEX/norma/llm"
)

type callbackProvider struct {
	stream   func(context.Context, llm.CompletionRequest) iter.Seq2[llm.StreamEvent, error]
	complete func(context.Context, llm.CompletionRequest) (llm.Message, string, llm.Usage, error)
}

func (p callbackProvider) Stream(ctx context.Context, req llm.CompletionRequest) iter.Seq2[llm.StreamEvent, error] {
	return p.stream(ctx, req)
}

func (p callbackProvider) Complete(ctx context.Context, req llm.CompletionRequest) (llm.Message, string, llm.Usage, error) {
	return p.complete(ctx, req)
}

func TestCompleteFailoverPreservesAtomicReplyAndUsage(t *testing.T) {
	wantMessage := llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{llm.TextBlock("fallback answer")}}
	wantUsage := llm.Usage{InputTokens: 19, OutputTokens: 7, CacheReadTokens: 3, CacheWriteTokens: 2}
	var primaryCalls, fallbackCalls int
	primary := callbackProvider{complete: func(context.Context, llm.CompletionRequest) (llm.Message, string, llm.Usage, error) {
		primaryCalls++
		return llm.Message{}, "", llm.Usage{}, fmt.Errorf("provider: status 402: exhausted")
	}}
	fallback := callbackProvider{complete: func(context.Context, llm.CompletionRequest) (llm.Message, string, llm.Usage, error) {
		fallbackCalls++
		return wantMessage, "end_turn", wantUsage, nil
	}}
	health := NewRegistry(nil, nil)
	pool := New([]*Member{member(1, "primary", 10, primary), member(2, "fallback", 5, fallback)}, health)
	for range 2 {
		message, stop, usage, err := pool.Complete(t.Context(), llm.CompletionRequest{})
		if err != nil || !reflect.DeepEqual(message, wantMessage) || stop != "end_turn" || usage != wantUsage {
			t.Fatalf("atomic fallback reply changed: message=%+v stop=%q usage=%+v err=%v", message, stop, usage, err)
		}
	}
	if primaryCalls != 1 || fallbackCalls != 2 || !health.IsOpen(1) {
		t.Fatalf("failed primary was not skipped: primary=%d fallback=%d state=%+v", primaryCalls, fallbackCalls, health.Get(1))
	}
}

func TestInFlightCancellationDoesNotStartFallback(t *testing.T) {
	for _, mode := range []string{"stream", "complete"} {
		t.Run(mode, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			started := make(chan struct{})
			transportErr := errors.New("provider: status 503: transport stopped")
			failAfterCancel := func(ctx context.Context) error {
				close(started)
				<-ctx.Done()
				// Transports may report their own error rather than context.Canceled.
				return transportErr
			}
			primary := callbackProvider{
				stream: func(ctx context.Context, _ llm.CompletionRequest) iter.Seq2[llm.StreamEvent, error] {
					return func(yield func(llm.StreamEvent, error) bool) { yield(llm.StreamEvent{}, failAfterCancel(ctx)) }
				},
				complete: func(ctx context.Context, _ llm.CompletionRequest) (llm.Message, string, llm.Usage, error) {
					return llm.Message{}, "", llm.Usage{}, failAfterCancel(ctx)
				},
			}
			var fallbackCalls atomic.Int32
			fallback := callbackProvider{
				stream: func(context.Context, llm.CompletionRequest) iter.Seq2[llm.StreamEvent, error] {
					fallbackCalls.Add(1)
					return func(func(llm.StreamEvent, error) bool) {}
				},
				complete: func(context.Context, llm.CompletionRequest) (llm.Message, string, llm.Usage, error) {
					fallbackCalls.Add(1)
					return llm.Message{}, "", llm.Usage{}, nil
				},
			}
			health := NewRegistry(nil, nil)
			pool := New([]*Member{member(1, "primary", 10, primary), member(2, "fallback", 5, fallback)}, health)
			done := make(chan error, 1)
			go func() {
				if mode == "stream" {
					_, err := drain(pool.Stream(ctx, llm.CompletionRequest{}))
					done <- err
				} else {
					_, _, _, err := pool.Complete(ctx, llm.CompletionRequest{})
					done <- err
				}
			}()
			select {
			case <-started:
			case <-time.After(time.Second):
				t.Fatal("primary request did not start")
			}
			cancel()
			select {
			case err := <-done:
				if !errors.Is(err, transportErr) {
					t.Fatalf("transport cancellation error changed: %v", err)
				}
			case <-time.After(time.Second):
				t.Fatal("cancelled request did not finish")
			}
			if fallbackCalls.Load() != 0 || health.Get(1) != (State{}) {
				t.Fatalf("cancelled request consumed a fallback or tripped health: calls=%d state=%+v", fallbackCalls.Load(), health.Get(1))
			}
		})
	}
}

func TestStreamConsumerStopDoesNotRetryOrResetHealth(t *testing.T) {
	closed := false
	primary := callbackProvider{stream: func(context.Context, llm.CompletionRequest) iter.Seq2[llm.StreamEvent, error] {
		return func(yield func(llm.StreamEvent, error) bool) {
			defer func() { closed = true }()
			if !yield(llm.StreamEvent{Type: llm.SETextDelta, Text: "partial"}, nil) {
				return
			}
			yield(llm.StreamEvent{}, errors.New("provider: status 503: after partial output"))
		}
	}}
	fallback := okProv("fallback", "duplicated answer")
	health := NewRegistry(nil, nil)
	health.Trip(1, "previous transient failure", false)
	before := health.Get(1)
	pool := New([]*Member{member(1, "primary", 10, primary), member(2, "fallback", 5, fallback)}, health)
	for event, err := range pool.Stream(t.Context(), llm.CompletionRequest{}) {
		if err != nil || event.Text != "partial" {
			t.Fatalf("first event=%+v err=%v", event, err)
		}
		break
	}
	if !closed || fallback.calls != 0 || health.Get(1) != before {
		t.Fatalf("consumer stop did not preserve state: closed=%v fallback=%d before=%+v after=%+v", closed, fallback.calls, before, health.Get(1))
	}
}
