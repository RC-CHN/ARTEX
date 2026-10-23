package llmrec

import (
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/RC-CHN/ARTEX/norma/llm"
	"github.com/RC-CHN/ARTEX/norma/transcript"
	"github.com/google/uuid"
)

func TestAnthropicStreamDiagnosticsWithoutBodies(t *testing.T) {
	d := streamDiagnosticsDB(t)
	const private = "PRIVATE-ANTHROPIC-SYNTHETIC-CONTENT"
	// Anthropic input_tokens excludes cache hits and creations: 11+7+3=21.
	// Its start output_tokens=1 is a placeholder, not extra output usage.
	const start = "event: message_start\ndata: " + `{"type":"message_start","message":{"role":"assistant","content":[],"usage":{"input_tokens":11,"output_tokens":1,"cache_read_input_tokens":7,"cache_creation_input_tokens":3}}}` + "\n\n"
	const thinking = "event: content_block_start\ndata: " + `{"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":""}}` + "\n\n" +
		"event: content_block_delta\ndata: " + `{"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"` + private + `"}}` + "\n\n" +
		"event: content_block_stop\ndata: " + `{"type":"content_block_stop","index":0}` + "\n\n"
	const answer = "event: content_block_start\ndata: " + `{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}` + "\n\n" +
		"event: content_block_delta\ndata: " + `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"` + private + `"}}` + "\n\n" +
		"event: content_block_stop\ndata: " + `{"type":"content_block_stop","index":0}` + "\n\n"
	const finish = "event: message_delta\ndata: " + `{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"input_tokens":11,"output_tokens":5,"cache_read_input_tokens":7,"cache_creation_input_tokens":3}}` + "\n\n"
	const stop = "event: message_stop\ndata: " + `{"type":"message_stop"}` + "\n\n"
	const invalid = "event: content_block_delta\ndata: {" + private + "\n\n"
	for _, tc := range []struct {
		name, body, end, stopReason string
		failure                     llm.IncompleteResponseReason
		requests, attempts, output  int
		finish                      bool
		parseErrors                 int
		readFailure                 bool
		retryRequestFailure         bool
		requestFailure              bool
		consumerStop                bool
		consumerStopAfterUsage      bool
		consumerStopAfterRetry      bool
		recoverAfterThinking        bool
	}{
		{name: "complete", body: start + answer + finish + stop, end: "done", stopReason: "end_turn", finish: true, requests: 1, attempts: 1, output: 5},
		{name: "explicit_stop_then_eof", body: start + answer + finish, end: "eof", stopReason: "end_turn", finish: true, requests: 1, attempts: 1, output: 5},
		{name: "message_stop_without_stop_reason", body: start + answer + stop, end: "done", requests: 1, attempts: 1},
		{name: "thinking_only", body: start + thinking + finish + stop, end: "done", stopReason: "end_turn", finish: true, failure: llm.IncompleteEmptyOutput, requests: 2, attempts: 2, output: 10},
		{name: "recovered_after_thinking", body: start + thinking + finish + stop, end: "done", stopReason: "end_turn", finish: true, recoverAfterThinking: true, requests: 2, attempts: 2, output: 10},
		{name: "eof_without_terminal", body: start + thinking, end: "eof", failure: llm.IncompleteMissingTerminal, requests: 2, attempts: 2},
		{name: "invalid_frame", body: start + thinking + invalid, end: "protocol_error", failure: llm.IncompleteMalformedFrame, parseErrors: 1, requests: 1, attempts: 1},
		{name: "invalid_after_buffered_usage", body: start + answer + finish + invalid, end: "protocol_error", stopReason: "end_turn", finish: true, failure: llm.IncompleteMalformedFrame, parseErrors: 1, requests: 1, attempts: 1, output: 5},
		{name: "read_failure", body: start + thinking, end: "read_error", failure: llm.IncompleteStreamRead, readFailure: true, requests: 2, attempts: 2},
		{name: "retry_request_failure", body: start + thinking + finish + stop, end: "request_error", failure: llm.IncompleteRetryRequest, retryRequestFailure: true, requests: 2, attempts: 1, output: 5},
		{name: "initial_request_failure", body: private, end: "request_error", requestFailure: true, requests: 1},
		{name: "consumer_stop", body: start + thinking + finish + stop, end: "consumer_stop", consumerStop: true, requests: 1, attempts: 1},
		{name: "consumer_stop_after_usage", body: start + answer + finish + stop, end: "consumer_stop", stopReason: "end_turn", consumerStop: true, consumerStopAfterUsage: true, requests: 1, attempts: 1, output: 5},
		{name: "consumer_stop_in_second_attempt", body: start + thinking + finish + stop, end: "consumer_stop", consumerStop: true, consumerStopAfterRetry: true, requests: 2, attempts: 2, output: 5},
	} {
		t.Run(tc.name, func(t *testing.T) {
			profile := "anthropic-diagnostics-" + uuid.NewString()
			session := "conv-synthetic-" + uuid.NewString()
			t.Cleanup(func() { _, _ = d.Exec(`DELETE FROM llm_usage WHERE profile_name=$1`, profile) })
			var requests atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/v1/messages" {
					t.Errorf("wrong protocol endpoint: %s", r.URL.Path)
				}
				call := requests.Add(1)
				w.Header().Set("X-Private-Fixture", private)
				if tc.requestFailure || tc.retryRequestFailure && call > 1 {
					w.WriteHeader(http.StatusBadGateway)
					_, _ = fmt.Fprint(w, private)
					return
				}
				w.Header().Set("Content-Type", "text/event-stream")
				if tc.recoverAfterThinking && call > 1 {
					_, _ = fmt.Fprint(w, start+answer+finish+stop)
					return
				}
				if tc.readFailure {
					w.Header().Set("Content-Length", strconv.Itoa(len(tc.body)+100))
				}
				_, _ = fmt.Fprint(w, tc.body)
			}))
			defer srv.Close()
			provider, err := llm.NewProvider(llm.Config{Format: llm.FormatAnthropic, BaseURL: srv.URL, APIKey: private, Model: "fixture", MaxRetries: -1, EmptyResponseRetries: 1, EmptyResponseInterval: time.Nanosecond})
			if err != nil {
				t.Fatal(err)
			}
			recorder := Wrap(provider, d, "fixture", profile, "", "", func() bool { return false })
			var streamErr error
			for event, err := range recorder.Stream(transcript.WithSessionID(t.Context(), session), llm.CompletionRequest{Messages: []llm.Message{llm.UserText(private)}}) {
				if err != nil {
					streamErr = err
					break
				}
				if tc.consumerStop {
					stopAt := llm.SEThinkingDelta
					if tc.consumerStopAfterUsage {
						stopAt = llm.SEMessageDelta
					}
					if event.Type == stopAt && (!tc.consumerStopAfterRetry || requests.Load() > 1) {
						break
					}
				}
			}
			wantError := tc.failure != "" || tc.requestFailure
			if (streamErr != nil) != wantError {
				t.Fatalf("unexpected error: %v", streamErr)
			}
			if tc.failure != "" {
				var incomplete *llm.IncompleteResponseError
				if !errors.As(streamErr, &incomplete) || incomplete.Reason != tc.failure {
					t.Fatalf("error=%v want incomplete %s", streamErr, tc.failure)
				}
			}
			if int(requests.Load()) != tc.requests {
				t.Fatalf("requests=%d want %d", requests.Load(), tc.requests)
			}
			var count, records, input, output, cacheRead, cacheWrite int
			var seen sql.NullBool
			var end sql.NullString
			var parseErrors sql.NullInt64
			var storedSession, status, stopReason, row string
			if err := d.QueryRow(`SELECT session_id,input_tokens,output_tokens,cache_read,cache_write,finish_reason_seen,stream_end,parse_errors,status,stop_reason,row_to_json(llm_usage)::text
FROM llm_usage WHERE profile_name=$1`, profile).Scan(&storedSession, &input, &output, &cacheRead, &cacheWrite, &seen, &end, &parseErrors, &status, &stopReason, &row); err != nil {
				t.Fatal(err)
			}
			if err := d.QueryRow(`SELECT count(*) FROM llm_usage WHERE profile_name=$1`, profile).Scan(&count); err != nil {
				t.Fatal(err)
			}
			if err := d.QueryRow(`SELECT count(*) FROM llm_records WHERE profile_name=$1`, profile).Scan(&records); err != nil {
				t.Fatal(err)
			}
			if count != 1 || records != 0 || strings.Contains(row, private) {
				t.Fatalf("privacy/accounting failure: usage rows=%d body rows=%d leaked=%v", count, records, strings.Contains(row, private))
			}
			if storedSession != session || input != 21*tc.attempts || output != tc.output || cacheRead != 7*tc.attempts || cacheWrite != 3*tc.attempts {
				t.Fatalf("session=%q input/output/cache/create=%d/%d/%d/%d", storedSession, input, output, cacheRead, cacheWrite)
			}
			if !end.Valid || end.String != tc.end || stopReason != tc.stopReason || (status == "error") != wantError {
				t.Fatalf("end=%+v stop=%q status=%q", end, stopReason, status)
			}
			if tc.consumerStop {
				if seen.Valid || parseErrors.Valid {
					t.Fatal("consumer stop invented evidence about unread tail")
				}
			} else if !seen.Valid || seen.Bool != tc.finish || !parseErrors.Valid || parseErrors.Int64 != int64(tc.parseErrors) {
				t.Fatalf("finish=%+v parse_errors=%+v", seen, parseErrors)
			}
		})
	}
}

func TestAnthropicCompleteRetainsUsageWithoutInventingStreamEvidence(t *testing.T) {
	d := streamDiagnosticsDB(t)
	for _, mode := range []string{"complete", "thinking_only", "retry_request_failure"} {
		t.Run(mode, func(t *testing.T) {
			thinkingOnly := mode != "complete"
			const private = "PRIVATE-ANTHROPIC-ATOMIC-FIXTURE"
			profile := "anthropic-atomic-" + uuid.NewString()
			session := "conv-synthetic-" + uuid.NewString()
			t.Cleanup(func() { _, _ = d.Exec(`DELETE FROM llm_usage WHERE profile_name=$1`, profile) })
			var requests atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				call := requests.Add(1)
				if mode == "retry_request_failure" && call > 1 {
					w.WriteHeader(http.StatusBadGateway)
					_, _ = fmt.Fprint(w, private)
					return
				}
				contentType, field := "text", "text"
				if thinkingOnly {
					contentType, field = "thinking", "thinking"
				}
				w.Header().Set("Content-Type", "application/json")
				fmt.Fprintf(w, `{"type":"message","content":[{"type":%q,%q:%q}],"stop_reason":"end_turn","usage":{"input_tokens":11,"output_tokens":5,"cache_read_input_tokens":7,"cache_creation_input_tokens":3}}`, contentType, field, private)
			}))
			defer srv.Close()
			provider, err := llm.NewProvider(llm.Config{Format: llm.FormatAnthropic, BaseURL: srv.URL, Model: "fixture", MaxRetries: -1, EmptyResponseRetries: 1, EmptyResponseInterval: time.Nanosecond})
			if err != nil {
				t.Fatal(err)
			}
			recorder := Wrap(provider, d, "fixture", profile, "", "", func() bool { return false })
			_, _, _, completionErr := recorder.Complete(transcript.WithSessionID(t.Context(), session), llm.CompletionRequest{Messages: []llm.Message{llm.UserText(private)}})
			if (completionErr != nil) != thinkingOnly {
				t.Fatalf("completion error=%v thinking_only=%v", completionErr, thinkingOnly)
			}
			attempts, meteredResponses := 1, 1
			if thinkingOnly {
				attempts, meteredResponses = 2, 2
				failure := llm.IncompleteEmptyOutput
				if mode == "retry_request_failure" {
					failure, meteredResponses = llm.IncompleteRetryRequest, 1
				}
				var incomplete *llm.IncompleteResponseError
				if !errors.As(completionErr, &incomplete) || incomplete.Reason != failure {
					t.Fatalf("missing typed incomplete output error: %v", completionErr)
				}
			}
			var unknown bool
			var input, output, cacheRead, cacheWrite, records int
			var storedSession, row string
			if err := d.QueryRow(`SELECT session_id,input_tokens,output_tokens,cache_read,cache_write,
finish_reason_seen IS NULL AND stream_end IS NULL AND parse_errors IS NULL,row_to_json(llm_usage)::text
FROM llm_usage WHERE profile_name=$1`, profile).Scan(&storedSession, &input, &output, &cacheRead, &cacheWrite, &unknown, &row); err != nil {
				t.Fatal(err)
			}
			if err := d.QueryRow(`SELECT count(*) FROM llm_records WHERE profile_name=$1`, profile).Scan(&records); err != nil {
				t.Fatal(err)
			}
			if !unknown || records != 0 || strings.Contains(row, private) {
				t.Fatal("atomic completion invented stream evidence or recorded private bodies")
			}
			if storedSession != session || int(requests.Load()) != attempts || input != 21*meteredResponses || output != 5*meteredResponses || cacheRead != 7*meteredResponses || cacheWrite != 3*meteredResponses {
				t.Fatalf("attempts=%d session=%q input/output/cache/create=%d/%d/%d/%d", requests.Load(), storedSession, input, output, cacheRead, cacheWrite)
			}
		})
	}
}
