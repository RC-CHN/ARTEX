package llm

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
)

const gatewayUsageFixture = `{"input_tokens":3893,"output_tokens":23,"cache_read_input_tokens":3840,"cache_creation_input_tokens":0,"billing_usage":{"semantic":"openai","openai_usage":{"prompt_tokens":3893,"completion_tokens":23,"prompt_tokens_details":{"cached_tokens":3840}}}}`

func TestAnthropicCumulativeMetering(t *testing.T) {
	for _, tc := range []struct {
		name, start string
		deltas      []string
		want        Usage
	}{
		{"native_output_only", `{"input_tokens":11,"cache_read_input_tokens":7,"cache_creation_input_tokens":3,"output_tokens":1}`, []string{`{"output_tokens":5}`}, Usage{21, 5, 7, 3}},
		{"late_cache", `{"input_tokens":100,"cache_read_input_tokens":0}`, []string{`{"input_tokens":10,"cache_read_input_tokens":80,"cache_creation_input_tokens":10,"output_tokens":5}`}, Usage{100, 5, 80, 10}},
		{"duplicate_and_partial", `{"input_tokens":100}`, []string{`{"input_tokens":10,"cache_read_input_tokens":80,"output_tokens":3}`, `{"input_tokens":10,"cache_read_input_tokens":80,"output_tokens":3}`, `{"output_tokens":5}`, `{}`}, Usage{90, 5, 80, 0}},
		{"explicit_zero_correction", `{"input_tokens":10,"cache_read_input_tokens":80,"cache_creation_input_tokens":10}`, []string{`{"input_tokens":7,"cache_read_input_tokens":0,"cache_creation_input_tokens":0,"output_tokens":5}`}, Usage{7, 5, 0, 0}},
		{"missing_start_usage", `{}`, []string{`{"input_tokens":10,"cache_read_input_tokens":80,"output_tokens":5}`}, Usage{90, 5, 80, 0}},
		{"gateway_declared_openai", `{"input_tokens":4481,"cache_read_input_tokens":0}`, []string{gatewayUsageFixture, `{"output_tokens":24}`, `{}`}, Usage{3893, 24, 3840, 0}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			for _, malformedTail := range []bool{false, true} {
				body := anthropicFrame(`{"type":"message_start","message":{"usage":`+tc.start+`}}`) + anthropicFrame(anthropicText)
				for _, u := range tc.deltas {
					body += anthropicFrame(`{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":` + u + `}`)
				}
				if malformedTail {
					body += anthropicFrame(`{invalid synthetic frame`)
				} else {
					body += anthropicFrame(anthropicStop)
				}
				srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { fmt.Fprint(w, body) }))
				defer srv.Close()
				p, _ := NewProvider(Config{Format: FormatAnthropic, BaseURL: srv.URL, EmptyResponseRetries: -1})
				acc := NewAccumulator()
				var gotErr error
				terminal := false
				for ev, err := range p.Stream(t.Context(), CompletionRequest{}) {
					if err != nil {
						gotErr = err
						break
					}
					terminal = terminal || ev.Type == SEMessageStop
					acc.Add(ev)
				}
				if malformedTail {
					assertIncomplete(t, gotErr, IncompleteMalformedFrame)
				} else if gotErr != nil {
					t.Fatal(gotErr)
				}
				if terminal == malformedTail || acc.Usage != tc.want {
					t.Fatalf("malformed=%t terminal=%t usage=%+v want=%+v", malformedTail, terminal, acc.Usage, tc.want)
				}
			}
		})
	}
}

func TestAnthropicMeteringSemantics(t *testing.T) {
	for _, tc := range []struct {
		name, raw string
		want      Usage
	}{
		{"native", `{"input_tokens":53,"output_tokens":23,"cache_read_input_tokens":3840}`, Usage{3893, 23, 3840, 0}},
		{"gateway", gatewayUsageFixture, Usage{3893, 23, 3840, 0}},
		{"unknown_semantic", `{"input_tokens":53,"cache_read_input_tokens":3840,"billing_usage":{"semantic":"future","openai_usage":{"prompt_tokens":8000}}}`, Usage{3893, 0, 3840, 0}},
		{"estimated_billing", `{"input_tokens":53,"cache_read_input_tokens":3840,"billing_usage":{"estimated":true,"semantic":"openai","openai_usage":{"prompt_tokens":8000}}}`, Usage{3893, 0, 3840, 0}},
		{"missing_billing_input", `{"input_tokens":53,"cache_read_input_tokens":3840,"billing_usage":{"semantic":"openai","openai_usage":{}}}`, Usage{3893, 0, 3840, 0}},
		{"cache_write_subset", `{"input_tokens":100,"cache_read_input_tokens":80,"cache_creation_input_tokens":10,"billing_usage":{"semantic":"openai","openai_usage":{"prompt_tokens":100,"prompt_tokens_details":{"cached_tokens":80,"cache_write_tokens":10}}}}`, Usage{100, 0, 80, 10}},
		{"cache_miss_is_not_write", `{"input_tokens":100,"billing_usage":{"semantic":"openai","openai_usage":{"prompt_tokens":100,"prompt_cache_hit_tokens":80,"prompt_cache_miss_tokens":20}}}`, Usage{100, 0, 80, 0}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var u anthropicUsage
			if err := json.Unmarshal([]byte(tc.raw), &u); err != nil {
				t.Fatal(err)
			}
			if got := u.norm(); got != tc.want {
				t.Fatalf("usage=%+v want=%+v", got, tc.want)
			}
			// The real non-streaming parser must use the same normalization.
			_, _, got, err := parseAnthropicResponse([]byte(`{"content":[{"type":"text","text":"OK"}],"stop_reason":"end_turn","usage":` + tc.raw + `}`))
			if err != nil || got != tc.want {
				t.Fatalf("complete usage=%+v err=%v", got, err)
			}
		})
	}
}
