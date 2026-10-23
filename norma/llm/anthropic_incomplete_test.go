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

func anthropicFrame(raw string) string { return "data: " + raw + "\n\n" }

const anthropicStart = `{"type":"message_start","message":{"usage":{"input_tokens":11,"output_tokens":1,"cache_read_input_tokens":2,"cache_creation_input_tokens":1}}}`
const anthropicThinkingFrame = `{"type":"content_block_delta","delta":{"type":"thinking_delta","thinking":"synthetic plan"}}`
const anthropicText = `{"type":"content_block_delta","delta":{"type":"text_delta","text":"complete"}}`
const anthropicDelta = `{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"input_tokens":11,"output_tokens":3,"cache_read_input_tokens":2,"cache_creation_input_tokens":1}}`
const anthropicStop = `{"type":"message_stop"}`

func TestAnthropicIncompleteProtocolBoundaries(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		want       IncompleteResponseReason
		end        StreamEnd
		finish     bool
		output     int
	}{
		{name: "text_stop", body: anthropicFrame(anthropicText) + anthropicFrame(anthropicDelta) + anthropicFrame(anthropicStop), end: StreamEndDone, finish: true, output: 3},
		{name: "stop_without_reason", body: anthropicFrame(anthropicText) + anthropicFrame(anthropicStop), end: StreamEndDone},
		{name: "explicit_stop_eof", body: anthropicFrame(anthropicText) + "data: " + anthropicDelta, end: StreamEndEOF, finish: true, output: 3},
		{name: "missing_terminal", body: anthropicFrame(anthropicText), want: IncompleteMissingTerminal, end: StreamEndEOF},
		{name: "thinking_only", body: anthropicFrame(anthropicThinkingFrame) + anthropicFrame(anthropicDelta) + anthropicFrame(anthropicStop), want: IncompleteEmptyOutput, end: StreamEndDone, finish: true, output: 3},
		{name: "empty", body: anthropicFrame(anthropicDelta) + anthropicFrame(anthropicStop), want: IncompleteEmptyOutput, end: StreamEndDone, finish: true, output: 3},
		{name: "malformed_after_usage", body: anthropicFrame(anthropicThinkingFrame) + anthropicFrame(anthropicDelta) + anthropicFrame(`{private invalid JSON`), want: IncompleteMalformedFrame, end: StreamEndProtocolError, finish: true, output: 3},
		{name: "max_tokens", body: anthropicFrame(anthropicThinkingFrame) + anthropicFrame(`{"type":"message_delta","delta":{"stop_reason":"max_tokens"},"usage":{"output_tokens":3}}`) + anthropicFrame(anthropicStop), end: StreamEndDone, finish: true, output: 3},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, anthropicFrame(anthropicStart)+tc.body) }))
			defer srv.Close()
			p, _ := NewProvider(Config{Format: FormatAnthropic, BaseURL: srv.URL, EmptyResponseRetries: -1})
			acc := NewAccumulator()
			var gotErr error
			var diag *StreamDiagnostics
			terminal := false
			for ev, err := range p.Stream(t.Context(), CompletionRequest{}) {
				if err != nil {
					gotErr = err
					break
				}
				if ev.Diagnostics != nil {
					diag = ev.Diagnostics
				}
				terminal = terminal || ev.Type == SEMessageStop
				acc.Add(ev)
			}
			if tc.want != "" {
				assertIncomplete(t, gotErr, tc.want)
				if terminal {
					t.Fatal("incomplete response emitted message_stop")
				}
			} else if gotErr != nil || !terminal {
				t.Fatalf("error=%v terminal=%t", gotErr, terminal)
			}
			if diag == nil || diag.StreamEnd != tc.end || diag.FinishReasonSeen != tc.finish {
				t.Fatalf("diagnostics=%+v", diag)
			}
			if acc.Usage.InputTokens != 14 || acc.Usage.OutputTokens != tc.output || acc.Usage.CacheReadTokens != 2 || acc.Usage.CacheWriteTokens != 1 {
				t.Fatalf("usage=%+v", acc.Usage)
			}
		})
	}
}

func TestAnthropicToolBoundariesAndInitialInput(t *testing.T) {
	for _, tc := range []struct {
		name, input, delta, tail string
		want                     IncompleteResponseReason
		raw                      string
	}{
		{name: "initial_object", input: `{"path":"RESOURCE.md"}`, tail: anthropicFrame(anthropicStop), raw: `{"path":"RESOURCE.md"}`},
		{name: "empty_initial_placeholder", input: `{ }`, delta: `{"path":"RESOURCE.md"}`, tail: anthropicFrame(anthropicStop), raw: `{"path":"RESOURCE.md"}`},
		{name: "unfinished_at_block_stop", input: `{}`, delta: `{"path":`, tail: anthropicFrame(`{"type":"content_block_stop","index":0}`), want: IncompleteInvalidToolInput},
		{name: "unfinished_at_next_tool", input: `{}`, delta: `{"path":`, tail: anthropicFrame(`{"type":"content_block_start","content_block":{"type":"tool_use","id":"next","name":"Read","input":{}}}`), want: IncompleteInvalidToolInput},
		{name: "valid_json_without_terminal", input: `{"path":"RESOURCE.md"}`, want: IncompleteMissingTerminal},
		{name: "invalid_initial_shape", input: `"not an object"`, tail: anthropicFrame(anthropicStop), want: IncompleteInvalidToolInput},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := anthropicFrame(fmt.Sprintf(`{"type":"content_block_start","content_block":{"type":"tool_use","id":"pending","name":"Write","input":%s}}`, tc.input))
			if tc.delta != "" {
				raw, _ := json.Marshal(tc.delta)
				body += anthropicFrame(fmt.Sprintf(`{"type":"content_block_delta","delta":{"type":"input_json_delta","partial_json":%s}}`, raw))
			}
			body += tc.tail
			var calls atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1); fmt.Fprint(w, body) }))
			defer srv.Close()
			p, _ := NewProvider(Config{Format: FormatAnthropic, BaseURL: srv.URL, EmptyResponseRetries: 3, EmptyResponseInterval: time.Nanosecond})
			acc := NewAccumulator()
			var gotErr error
			var types []StreamEventType
			for ev, err := range p.Stream(t.Context(), CompletionRequest{}) {
				if err != nil {
					gotErr = err
					break
				}
				types = append(types, ev.Type)
				acc.Add(ev)
			}
			if tc.want != "" {
				assertIncomplete(t, gotErr, tc.want)
				if !reflect.DeepEqual(types, []StreamEventType{SEToolUseStart, SEToolInputJSON, SEStreamDiagnostics}) {
					t.Fatalf("exposed invalid tool boundary: %v", types)
				}
			} else {
				if gotErr != nil {
					t.Fatal(gotErr)
				}
				uses := acc.Message().ToolUses()
				if len(uses) != 1 || string(uses[0].Input) != tc.raw {
					t.Fatalf("lost initial input: %+v", uses)
				}
			}
			if calls.Load() != 1 {
				t.Fatalf("replayed tool fragment: calls=%d", calls.Load())
			}
		})
	}
}

func TestAnthropicSafeRetryKeepsOnlyWinningThinkingSignature(t *testing.T) {
	for _, firstEnd := range []string{"stop", "eof", "read_error"} {
		t.Run(firstEnd, func(t *testing.T) {
			var calls atomic.Int32
			var mu sync.Mutex
			var bodies []string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				raw, _ := io.ReadAll(r.Body)
				mu.Lock()
				bodies = append(bodies, string(raw))
				mu.Unlock()
				n := calls.Add(1)
				body := anthropicFrame(anthropicStart) + anthropicFrame(fmt.Sprintf(`{"type":"content_block_delta","delta":{"type":"thinking_delta","thinking":"attempt-%d"}}`, n)) + anthropicFrame(fmt.Sprintf(`{"type":"content_block_delta","delta":{"type":"signature_delta","signature":"signature-%d"}}`, n))
				if n > 1 {
					body += anthropicFrame(anthropicText) + anthropicFrame(anthropicDelta) + anthropicFrame(anthropicStop)
				} else if firstEnd == "stop" {
					body += anthropicFrame(anthropicDelta) + anthropicFrame(anthropicStop)
				} else if firstEnd == "read_error" {
					w.Header().Set("Content-Length", fmt.Sprint(len(body)+100))
				}
				fmt.Fprint(w, body)
			}))
			defer srv.Close()
			p, _ := NewProvider(Config{Format: FormatAnthropic, BaseURL: srv.URL, ThinkingType: "enabled", EmptyResponseRetries: 1, EmptyResponseInterval: time.Nanosecond})
			acc := NewAccumulator()
			var visibleThinking []string
			req := CompletionRequest{Messages: []Message{UserText("synthetic")}, Tools: []ToolSchema{{Name: "Write"}}}
			for ev, err := range p.Stream(t.Context(), req) {
				if err != nil {
					t.Fatal(err)
				}
				if ev.Type == SEThinkingDelta {
					visibleThinking = append(visibleThinking, ev.Text)
				}
				acc.Add(ev)
			}
			msg := acc.Message()
			if calls.Load() != 2 || len(msg.Content) != 2 || msg.Content[0].Thinking != "attempt-2" || msg.Content[0].Signature != "signature-2" || msg.Text() != "complete" {
				t.Fatalf("signature contamination: calls=%d msg=%+v", calls.Load(), msg)
			}
			if !reflect.DeepEqual(visibleThinking, []string{"attempt-1", "attempt-2"}) || len(bodies) != 2 || bodies[0] != bodies[1] {
				t.Fatal("lost diagnostics or changed retry prompt/tools")
			}
			wantOutput := 3
			if firstEnd == "stop" {
				wantOutput = 6
			}
			if acc.Usage.InputTokens != 28 || acc.Usage.OutputTokens != wantOutput {
				t.Fatalf("usage=%+v", acc.Usage)
			}
			replay, err := p.(*anthropicProvider).buildBody(CompletionRequest{Messages: []Message{msg, UserText("continue")}}, true)
			if err != nil || strings.Contains(string(replay), "attempt-1") || strings.Contains(string(replay), "signature-1") || !strings.Contains(string(replay), "signature-2") {
				t.Fatalf("unsafe replay: %s error=%v", replay, err)
			}
		})
	}
}

func TestAnthropicRetryCancellationAndExhaustion(t *testing.T) {
	for _, cancelAtDiagnostic := range []bool{false, true} {
		t.Run(fmt.Sprint(cancelAtDiagnostic), func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			var calls atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				fmt.Fprint(w, anthropicFrame(anthropicStart)+anthropicFrame(anthropicThinkingFrame)+anthropicFrame(anthropicDelta)+anthropicFrame(anthropicStop))
			}))
			defer srv.Close()
			p, _ := NewProvider(Config{Format: FormatAnthropic, BaseURL: srv.URL, EmptyResponseRetries: 1, EmptyResponseInterval: time.Nanosecond})
			acc := NewAccumulator()
			var gotErr error
			for ev, err := range p.Stream(ctx, CompletionRequest{}) {
				if err != nil {
					gotErr = err
					break
				}
				acc.Add(ev)
				if cancelAtDiagnostic && ev.Diagnostics != nil {
					cancel()
				}
			}
			wantCalls := int32(2)
			if cancelAtDiagnostic {
				wantCalls = 1
				if !errors.Is(gotErr, context.Canceled) {
					t.Fatalf("lost cancellation: %v", gotErr)
				}
			} else {
				assertIncomplete(t, gotErr, IncompleteEmptyOutput)
			}
			if calls.Load() != wantCalls || acc.Usage.InputTokens != int(wantCalls)*14 || acc.Usage.OutputTokens != int(wantCalls)*3 {
				t.Fatalf("calls=%d usage=%+v", calls.Load(), acc.Usage)
			}
		})
	}
}

func TestAnthropicCompleteInvalidToolAndMalformedFrame(t *testing.T) {
	start, ok, startErr := parseAnthropicFrame(`{"type":"message_start","message":{}}`)
	if startErr != nil || !ok || start.Type != SEMessageStart || start.Usage != (Usage{}) {
		t.Fatalf("missing zero-usage attempt boundary: %+v %v", start, startErr)
	}
	for _, raw := range []string{`{"type":42}`, `{private invalid JSON`} {
		_, _, err := parseAnthropicFrame(raw)
		assertIncomplete(t, err, IncompleteMalformedFrame)
	}
	msg, stop, usage, err := parseAnthropicResponse([]byte(`{"type":"message","content":[{"type":"tool_use","id":"bad","name":"Write","input":"invalid object"}],"stop_reason":"tool_use","usage":{"input_tokens":7,"output_tokens":3}}`))
	assertIncomplete(t, err, IncompleteInvalidToolInput)
	if len(msg.ToolUses()) != 1 || stop != "tool_use" || usage.InputTokens != 7 {
		t.Fatalf("lost partial message: %+v %s %+v", msg, stop, usage)
	}
}

func TestAnthropicExplicitUpstreamFailurePreservesMachineCauseWithoutRetry(t *testing.T) {
	for _, prior := range []string{"none", "thinking", "retry"} {
		t.Run(prior, func(t *testing.T) {
			var calls atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				n := calls.Add(1)
				body := anthropicFrame(anthropicStart)
				if prior == "retry" && n == 1 {
					fmt.Fprint(w, body+anthropicFrame(anthropicThinkingFrame)+anthropicFrame(anthropicDelta)+anthropicFrame(anthropicStop))
					return
				}
				if prior == "thinking" {
					body += anthropicFrame(anthropicThinkingFrame)
				}
				fmt.Fprint(w, body+anthropicFrame(`{"type":"error","error":{"type":"overloaded_error","message":"quota exhausted; private synthetic upstream detail"}}`))
			}))
			defer srv.Close()
			p, _ := NewProvider(Config{Format: FormatAnthropic, BaseURL: srv.URL, EmptyResponseRetries: 3, EmptyResponseInterval: time.Nanosecond})
			var gotErr error
			for _, err := range p.Stream(t.Context(), CompletionRequest{}) {
				if err != nil {
					gotErr = err
					break
				}
			}
			wantCalls := int32(1)
			if prior == "retry" {
				wantCalls = 2
			}
			if calls.Load() != wantCalls {
				t.Fatalf("retried explicit upstream error: calls=%d", calls.Load())
			}
			if prior == "none" {
				var incomplete *IncompleteResponseError
				if gotErr == nil || errors.As(gotErr, &incomplete) {
					t.Fatalf("changed initial pure API error: %v", gotErr)
				}
				return
			}
			incomplete := assertIncomplete(t, gotErr, IncompleteUpstreamError)
			cause := errors.Unwrap(incomplete)
			if cause == nil || !strings.Contains(cause.Error(), "quota exhausted") || strings.Contains(incomplete.Error(), "private") {
				t.Fatalf("classification/privacy failed: %v", incomplete)
			}
			raw, _ := json.Marshal(incomplete)
			if strings.Contains(string(raw), "private") {
				t.Fatal("private cause serialized into diagnostics")
			}
		})
	}
}

func TestAnthropicConcurrentRetriesDoNotShareThinkingOrBudget(t *testing.T) {
	var mu sync.Mutex
	calls := make(map[string]int)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Messages []Message `json:"messages"`
		}
		if json.NewDecoder(r.Body).Decode(&req) != nil || len(req.Messages) == 0 {
			http.Error(w, "invalid fixture", 400)
			return
		}
		key := req.Messages[len(req.Messages)-1].Text()
		mu.Lock()
		calls[key]++
		n := calls[key]
		mu.Unlock()
		body := anthropicFrame(anthropicStart) + anthropicFrame(fmt.Sprintf(`{"type":"content_block_delta","delta":{"type":"thinking_delta","thinking":"%s-%d"}}`, key, n)) + anthropicFrame(fmt.Sprintf(`{"type":"content_block_delta","delta":{"type":"signature_delta","signature":"%s-signature-%d"}}`, key, n))
		if n > 1 {
			body += anthropicFrame(fmt.Sprintf(`{"type":"content_block_delta","delta":{"type":"text_delta","text":%q}}`, key))
		}
		fmt.Fprint(w, body+anthropicFrame(anthropicDelta)+anthropicFrame(anthropicStop))
	}))
	defer srv.Close()
	p, _ := NewProvider(Config{Format: FormatAnthropic, BaseURL: srv.URL, EmptyResponseRetries: 1, EmptyResponseInterval: time.Nanosecond})
	var wg sync.WaitGroup
	for i := range 12 {
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
			msg := acc.Message()
			if len(msg.Content) != 2 || msg.Content[0].Thinking != key+"-2" || msg.Content[0].Signature != key+"-signature-2" || msg.Text() != key || acc.Usage.InputTokens != 28 || acc.Usage.OutputTokens != 6 {
				t.Errorf("request %s mixed message=%+v usage=%+v", key, msg, acc.Usage)
			}
		}()
	}
	wg.Wait()
	mu.Lock()
	defer mu.Unlock()
	if len(calls) != 12 {
		t.Fatalf("requests=%d", len(calls))
	}
	for key, n := range calls {
		if n != 2 {
			t.Errorf("request %s attempts=%d", key, n)
		}
	}
}

func TestAnthropicMultipleMessageDeltasPreserveCumulativeUsage(t *testing.T) {
	for _, thinkingOnly := range []bool{false, true} {
		for _, tc := range []struct {
			name, delta string
			output      int
		}{
			{name: "absent_usage", delta: `{"type":"message_delta","delta":{}}`, output: 3},
			{name: "smaller_cumulative_usage", delta: `{"type":"message_delta","delta":{},"usage":{"output_tokens":2}}`, output: 3},
			{name: "larger_cumulative_usage", delta: `{"type":"message_delta","delta":{},"usage":{"output_tokens":7}}`, output: 7},
		} {
			t.Run(fmt.Sprintf("thinking_only=%t/%s", thinkingOnly, tc.name), func(t *testing.T) {
				content := anthropicText
				if thinkingOnly {
					content = anthropicThinkingFrame
				}
				body := anthropicFrame(anthropicStart) + anthropicFrame(content) + anthropicFrame(anthropicDelta) + anthropicFrame(tc.delta) + anthropicFrame(anthropicStop)
				srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, body) }))
				defer srv.Close()
				p, _ := NewProvider(Config{Format: FormatAnthropic, BaseURL: srv.URL, EmptyResponseRetries: -1})
				acc := NewAccumulator()
				var gotErr error
				deltas := 0
				for ev, err := range p.Stream(t.Context(), CompletionRequest{}) {
					if err != nil {
						gotErr = err
						break
					}
					if ev.Type == SEMessageDelta {
						deltas++
						if ev.StopReason != "end_turn" || ev.Usage.OutputTokens != tc.output {
							t.Fatalf("lost terminal metadata: %+v", ev)
						}
					}
					acc.Add(ev)
				}
				if thinkingOnly {
					assertIncomplete(t, gotErr, IncompleteEmptyOutput)
					if deltas != 0 {
						t.Fatal("incomplete response exposed terminal delta")
					}
				} else if gotErr != nil || deltas != 1 {
					t.Fatalf("error=%v deltas=%d", gotErr, deltas)
				}
				if acc.StopReason != "end_turn" || acc.Usage.InputTokens != 14 || acc.Usage.OutputTokens != tc.output || acc.Usage.CacheReadTokens != 2 || acc.Usage.CacheWriteTokens != 1 {
					t.Fatalf("stop=%q usage=%+v", acc.StopReason, acc.Usage)
				}
			})
		}
	}
}

func TestAnthropicDuplicateMessageStartCountsFirstHeaderOnly(t *testing.T) {
	for _, tc := range []struct {
		name, first, second          string
		input, cacheRead, cacheWrite int
	}{
		{name: "identical_usage", first: anthropicStart, second: anthropicStart, input: 14, cacheRead: 2, cacheWrite: 1},
		{name: "different_usage", first: anthropicStart, second: `{"type":"message_start","message":{"usage":{"input_tokens":305,"output_tokens":90,"cache_read_input_tokens":30,"cache_creation_input_tokens":7}}}`, input: 14, cacheRead: 2, cacheWrite: 1},
		{name: "first_usage_absent", first: `{"type":"message_start","message":{}}`, second: anthropicStart},
		{name: "later_usage_absent", first: anthropicStart, second: `{"type":"message_start","message":{}}`, input: 14, cacheRead: 2, cacheWrite: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := anthropicFrame(tc.first) + anthropicFrame(anthropicText) + anthropicFrame(anthropicDelta) + anthropicFrame(tc.second) + anthropicFrame(anthropicStop)
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, body) }))
			defer srv.Close()
			p, _ := NewProvider(Config{Format: FormatAnthropic, BaseURL: srv.URL, EmptyResponseRetries: -1})
			acc := NewAccumulator()
			starts := 0
			for ev, err := range p.Stream(t.Context(), CompletionRequest{}) {
				if err != nil {
					t.Fatal(err)
				}
				if ev.Type == SEMessageStart {
					starts++
				}
				acc.Add(ev)
			}
			if starts != 1 || acc.Usage.InputTokens != tc.input || acc.Usage.OutputTokens != 3 || acc.Usage.CacheReadTokens != tc.cacheRead || acc.Usage.CacheWriteTokens != tc.cacheWrite || acc.StopReason != "end_turn" || acc.Message().Text() != "complete" {
				t.Fatalf("starts=%d stop=%q usage=%+v message=%+v", starts, acc.StopReason, acc.Usage, acc.Message())
			}
		})
	}
}

func TestAnthropicDuplicateStartDoesNotFinalizePendingTool(t *testing.T) {
	body := anthropicFrame(anthropicStart) +
		anthropicFrame(`{"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"write-1","name":"Write","input":{}}}`) +
		anthropicFrame(`{"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{\"path\":"}}`) +
		anthropicFrame(anthropicStart) +
		anthropicFrame(`{"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"\"RESOURCE.md\"}"}}`) +
		anthropicFrame(`{"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":3}}`) + anthropicFrame(anthropicStop)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, body) }))
	defer srv.Close()
	p, _ := NewProvider(Config{Format: FormatAnthropic, BaseURL: srv.URL, EmptyResponseRetries: -1})
	acc := NewAccumulator()
	var events []StreamEventType
	for ev, err := range p.Stream(t.Context(), CompletionRequest{}) {
		if err != nil {
			t.Fatal(err)
		}
		events = append(events, ev.Type)
		acc.Add(ev)
	}
	want := []StreamEventType{SEMessageStart, SEToolUseStart, SEToolInputJSON, SEToolInputJSON, SEMessageDelta, SEStreamDiagnostics, SEMessageStop}
	uses := acc.Message().ToolUses()
	if !reflect.DeepEqual(events, want) || len(uses) != 1 || string(uses[0].Input) != `{"path":"RESOURCE.md"}` || acc.Usage.InputTokens != 14 || acc.Usage.OutputTokens != 3 {
		t.Fatalf("events=%v uses=%+v usage=%+v", events, uses, acc.Usage)
	}
}

func TestAnthropicDuplicateStartRetryEOFCancelBoundaries(t *testing.T) {
	for _, mode := range []string{"empty_retry", "eof_retry", "text_eof", "cancel"} {
		t.Run(mode, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			var calls atomic.Int32
			body := anthropicFrame(anthropicStart) + anthropicFrame(anthropicStart)
			if mode == "text_eof" {
				body += anthropicFrame(anthropicText)
			} else {
				body += anthropicFrame(anthropicThinkingFrame)
			}
			if mode == "empty_retry" || mode == "cancel" {
				body += anthropicFrame(anthropicDelta) + anthropicFrame(anthropicStop)
			}
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1); fmt.Fprint(w, body) }))
			defer srv.Close()
			p, _ := NewProvider(Config{Format: FormatAnthropic, BaseURL: srv.URL, EmptyResponseRetries: 1, EmptyResponseInterval: time.Nanosecond})
			acc := NewAccumulator()
			starts := 0
			var gotErr error
			for ev, err := range p.Stream(ctx, CompletionRequest{}) {
				if err != nil {
					gotErr = err
					break
				}
				if ev.Type == SEMessageStart {
					starts++
				}
				acc.Add(ev)
				if mode == "cancel" && ev.Diagnostics != nil {
					cancel()
				}
			}
			wantCalls, wantOutput := int32(2), 0
			switch mode {
			case "cancel":
				wantCalls, wantOutput = 1, 3
				if !errors.Is(gotErr, context.Canceled) {
					t.Fatalf("lost cancellation: %v", gotErr)
				}
			case "empty_retry":
				wantOutput = 6
				assertIncomplete(t, gotErr, IncompleteEmptyOutput)
			case "text_eof":
				wantCalls = 1
				assertIncomplete(t, gotErr, IncompleteMissingTerminal)
			case "eof_retry":
				assertIncomplete(t, gotErr, IncompleteMissingTerminal)
			}
			if calls.Load() != wantCalls || starts != int(wantCalls) || acc.Usage.InputTokens != 14*int(wantCalls) || acc.Usage.OutputTokens != wantOutput || acc.Usage.CacheReadTokens != 2*int(wantCalls) || acc.Usage.CacheWriteTokens != int(wantCalls) {
				t.Fatalf("calls=%d starts=%d usage=%+v", calls.Load(), starts, acc.Usage)
			}
		})
	}
}

// Reproduce only the redacted protocol shape: thinking-only end_turn with zero
// reported output tokens, followed by a complete Read call. No historical
// prompt, reasoning, resource contents or tool arguments are copied here.
func TestAnthropicZeroTokenThinkingStopRecoversToToolWithDefaultRetryBudget(t *testing.T) {
	for _, recoverWithTool := range []bool{false, true} {
		t.Run(fmt.Sprintf("recover_with_tool=%t", recoverWithTool), func(t *testing.T) {
			var calls atomic.Int32
			var mu sync.Mutex
			var requests []string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				raw, _ := io.ReadAll(r.Body)
				mu.Lock()
				requests = append(requests, string(raw))
				mu.Unlock()
				n := calls.Add(1)
				body := anthropicFrame(`{"type":"message_start","message":{"usage":{"input_tokens":34759,"output_tokens":0,"cache_read_input_tokens":0,"cache_creation_input_tokens":0}}}`)
				body += anthropicFrame(fmt.Sprintf(`{"type":"content_block_delta","delta":{"type":"thinking_delta","thinking":"synthetic attempt %d"}}`, n))
				if n > 1 && recoverWithTool {
					body += anthropicFrame(`{"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"synthetic-read","name":"Read","input":{}}}`)
					body += anthropicFrame(`{"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"{\"file_path\":"}}`)
					body += anthropicFrame(`{"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"\"synthetic-fixture.txt\"}"}}`)
					body += anthropicFrame(`{"type":"content_block_stop","index":1}`)
					// The gateway's later input/cache values differ from message_start.
					// This test records the existing input-start/output-delta policy;
					// it does not assert which gateway input values reflect billing.
					body += anthropicFrame(`{"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"input_tokens":51255,"output_tokens":197,"cache_read_input_tokens":51200,"cache_creation_input_tokens":0}}`)
				} else {
					body += anthropicFrame(`{"type":"content_block_stop","index":0}`)
					body += anthropicFrame(`{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"input_tokens":34759,"output_tokens":0,"cache_read_input_tokens":0,"cache_creation_input_tokens":0}}`)
				}
				fmt.Fprint(w, body+anthropicFrame(anthropicStop))
			}))
			defer srv.Close()
			// Leave EmptyResponseRetries unset: verify the actual default budget.
			p, _ := NewProvider(Config{Format: FormatAnthropic, BaseURL: srv.URL, MaxRetries: -1, EmptyResponseInterval: time.Nanosecond})
			req := CompletionRequest{Messages: []Message{UserText("synthetic protocol reproduction")}, Tools: []ToolSchema{{Name: "Read"}}}
			acc := NewAccumulator()
			var gotErr error
			deltas, stops := 0, 0
			for ev, err := range p.Stream(t.Context(), req) {
				if err != nil {
					gotErr = err
					break
				}
				if ev.Type == SEMessageDelta {
					deltas++
				}
				if ev.Type == SEMessageStop {
					stops++
				}
				acc.Add(ev)
			}
			wantCalls, wantOutput := int32(3), 0
			msg := acc.Message()
			if recoverWithTool {
				wantCalls, wantOutput = 2, 197
				uses := msg.ToolUses()
				if gotErr != nil || deltas != 1 || stops != 1 || len(uses) != 1 || uses[0].Name != "Read" || string(uses[0].Input) != `{"file_path":"synthetic-fixture.txt"}` || acc.StopReason != "tool_use" || thinkingOf(msg) != "synthetic attempt 2" {
					t.Fatalf("error=%v deltas=%d stops=%d stop=%q message=%+v", gotErr, deltas, stops, acc.StopReason, msg)
				}
			} else {
				assertIncomplete(t, gotErr, IncompleteEmptyOutput)
				if deltas != 0 || stops != 0 || len(msg.ToolUses()) != 0 {
					t.Fatal("thinking-only attempts exposed a successful/tool boundary")
				}
			}
			if calls.Load() != wantCalls || acc.Usage.InputTokens != 34759*int(wantCalls) || acc.Usage.OutputTokens != wantOutput || acc.Usage.CacheReadTokens != 0 || acc.Usage.CacheWriteTokens != 0 {
				t.Fatalf("calls=%d usage=%+v", calls.Load(), acc.Usage)
			}
			mu.Lock()
			defer mu.Unlock()
			for _, raw := range requests {
				if raw != requests[0] || !strings.Contains(raw, `"tools"`) {
					t.Fatal("retry changed prompt or disabled tools")
				}
			}
		})
	}
}
