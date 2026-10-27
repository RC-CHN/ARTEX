package llmrec

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/RC-CHN/ARTEX/norma/llm"
	"github.com/google/uuid"
)

func TestLateAnthropicUsagePersistsWithoutBodies(t *testing.T) {
	d := streamDiagnosticsDB(t)
	const start = "data: " + `{"type":"message_start","message":{"usage":{"input_tokens":4481,"cache_read_input_tokens":0,"output_tokens":0}}}` + "\n\n"
	const finish = "data: " + `{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"input_tokens":3893,"cache_read_input_tokens":3840,"output_tokens":23,"billing_usage":{"semantic":"openai","openai_usage":{"prompt_tokens":3893,"completion_tokens":23,"prompt_tokens_details":{"cached_tokens":3840}}}}}` + "\n\n"
	const stop = "data: " + `{"type":"message_stop"}` + "\n\n"
	for _, mode := range []string{"complete", "retry", "malformed_tail", "consumer_stop"} {
		t.Run(mode, func(t *testing.T) {
			profile := "late-usage-" + uuid.NewString()
			t.Cleanup(func() { _, _ = d.Exec(`DELETE FROM llm_usage WHERE profile_name=$1`, profile) })
			var calls atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				n := calls.Add(1)
				kind, field := "text_delta", "text"
				if mode == "retry" && n == 1 {
					kind, field = "thinking_delta", "thinking"
				}
				body := start + fmt.Sprintf("data: {\"type\":\"content_block_delta\",\"delta\":{\"type\":%q,%q:\"private synthetic output\"}}\n\n", kind, field) + finish
				if mode == "malformed_tail" {
					body += "data: {invalid synthetic frame\n\n"
				} else {
					body += stop
				}
				fmt.Fprint(w, body)
			}))
			defer srv.Close()
			provider, err := llm.NewProvider(llm.Config{Format: llm.FormatAnthropic, BaseURL: srv.URL, EmptyResponseRetries: 1, EmptyResponseInterval: time.Nanosecond})
			if err != nil {
				t.Fatal(err)
			}
			recorder := Wrap(provider, d, "fixture", profile, "", "", func() bool { return false })
			acc := llm.NewAccumulator()
			var streamErr error
			for ev, err := range recorder.Stream(t.Context(), llm.CompletionRequest{}) {
				if err != nil {
					streamErr = err
					break
				}
				acc.Add(ev)
				if mode == "consumer_stop" && ev.Type == llm.SEMessageDelta {
					break
				}
			}
			if (streamErr != nil) != (mode == "malformed_tail") {
				t.Fatalf("error=%v", streamErr)
			}
			attempts := 1
			if mode == "retry" {
				attempts = 2
			}
			want := llm.Usage{InputTokens: 3893 * attempts, OutputTokens: 23 * attempts, CacheReadTokens: 3840 * attempts}
			var stored llm.Usage
			var rows, bodies int
			if err := d.QueryRow(`SELECT count(*),sum(input_tokens),sum(output_tokens),sum(cache_read),sum(cache_write) FROM llm_usage WHERE profile_name=$1`, profile).Scan(&rows, &stored.InputTokens, &stored.OutputTokens, &stored.CacheReadTokens, &stored.CacheWriteTokens); err != nil {
				t.Fatal(err)
			}
			if err := d.QueryRow(`SELECT count(*) FROM llm_records WHERE profile_name=$1`, profile).Scan(&bodies); err != nil {
				t.Fatal(err)
			}
			if int(calls.Load()) != attempts || rows != 1 || bodies != 0 || acc.Usage != want || stored != want {
				t.Fatalf("calls=%d rows=%d bodies=%d accumulator=%+v stored=%+v want=%+v", calls.Load(), rows, bodies, acc.Usage, stored, want)
			}
		})
	}
}
