package llmrec

import (
	"database/sql"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/RC-CHN/ARTEX/db"
	"github.com/RC-CHN/ARTEX/norma/llm"
	"github.com/google/uuid"
)

func streamDiagnosticsDB(t *testing.T) *db.DB {
	t.Helper()
	dsn := os.Getenv("ARTEX_PG_DSN")
	if dsn == "" {
		t.Skip("requires isolated ARTEX_PG_DSN")
	}
	d, err := db.Open(dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Close() })
	if err := d.EnsureLLMUsageTable(); err != nil {
		t.Fatal(err)
	}
	if err := d.EnsureLLMRecordsTable(); err != nil {
		t.Fatal(err)
	}
	return d
}

func TestStreamDiagnosticsLedgerWithoutBodies(t *testing.T) {
	d := streamDiagnosticsDB(t)
	const private = "PRIVATE-SYNTHETIC-CONTENT"
	thinking := `data: {"choices":[{"delta":{"reasoning_content":"` + private + `"}}]}` + "\n\n"
	answer := `data: {"choices":[{"delta":{"content":"` + private + `"}}]}` + "\n\n"
	finish := `data: {"choices":[{"delta":{},"finish_reason":"stop"}]}` + "\n\n"
	done := "data: [DONE]\n\n"
	for _, tc := range []struct {
		name, body     string
		end            string
		finish         bool
		parseErrors    int64
		readFailure    bool
		requestFailure bool
		emptyRetry     bool
		consumerStop   bool
		wantError      bool
		requests       int32
	}{
		{name: "done_without_finish", body: thinking + done, end: "done", wantError: true, requests: 2},
		{name: "eof_without_finish", body: thinking, end: "eof", wantError: true, requests: 2},
		{name: "explicit_finish", body: thinking + answer + finish + done, end: "done", finish: true},
		{name: "explicit_finish_without_output", body: thinking + finish + done, end: "done", finish: true, wantError: true, requests: 2},
		{name: "malformed_frame", body: thinking + "data: {" + private + "\n\n" + done, end: "protocol_error", parseErrors: 1, wantError: true},
		{name: "read_error", body: thinking, end: "read_error", readFailure: true, wantError: true, requests: 2},
		{name: "request_error", body: private, end: "request_error", requestFailure: true, wantError: true},
		{name: "empty_retry_final_evidence", body: thinking + answer + done, end: "done", emptyRetry: true, requests: 2},
		{name: "consumer_stop", body: thinking + finish + done, end: "consumer_stop", consumerStop: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			profile := "stream-diagnostics-" + uuid.NewString()
			t.Cleanup(func() { _, _ = d.Exec(`DELETE FROM llm_usage WHERE profile_name=$1`, profile) })
			var requests atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				call := requests.Add(1)
				w.Header().Set("X-Private-Fixture", private)
				if tc.emptyRetry && call == 1 {
					_, _ = fmt.Fprint(w, finish+done)
					return
				}
				if tc.readFailure {
					w.Header().Set("Content-Length", strconv.Itoa(len(tc.body)+100))
				}
				if tc.requestFailure {
					w.WriteHeader(http.StatusBadGateway)
				}
				_, _ = fmt.Fprint(w, tc.body)
			}))
			defer srv.Close()
			provider, err := llm.NewProvider(llm.Config{Format: llm.FormatOpenAI, BaseURL: srv.URL, APIKey: private, Model: "fixture", MaxRetries: -1, EmptyResponseRetries: 1, EmptyResponseInterval: time.Nanosecond})
			if err != nil {
				t.Fatal(err)
			}
			recorder := Wrap(provider, d, "fixture", profile, "", "", func() bool { return false })
			var streamErr error
			for event, err := range recorder.Stream(t.Context(), llm.CompletionRequest{Messages: []llm.Message{llm.UserText(private)}}) {
				if err != nil {
					streamErr = err
					break
				}
				if tc.consumerStop && event.Type == llm.SEThinkingDelta {
					break
				}
			}
			if (streamErr != nil) != tc.wantError {
				t.Fatalf("unexpected completion error: %v", streamErr)
			}
			var count, records int
			if err := d.QueryRow(`SELECT count(*) FROM llm_usage WHERE profile_name=$1`, profile).Scan(&count); err != nil {
				t.Fatal(err)
			}
			if err := d.QueryRow(`SELECT count(*) FROM llm_records WHERE profile_name=$1`, profile).Scan(&records); err != nil {
				t.Fatal(err)
			}
			if count != 1 || records != 0 {
				t.Fatalf("ledger rows=%d body records=%d", count, records)
			}
			var seen sql.NullBool
			var end sql.NullString
			var parseErrors sql.NullInt64
			var status, jsonRow string
			if err := d.QueryRow(`SELECT finish_reason_seen,stream_end,parse_errors,status,row_to_json(llm_usage)::text FROM llm_usage WHERE profile_name=$1`, profile).Scan(&seen, &end, &parseErrors, &status, &jsonRow); err != nil {
				t.Fatal(err)
			}
			if strings.Contains(jsonRow, private) {
				t.Fatal("private fixture request/response/error/header entered the usage ledger")
			}
			if !end.Valid || end.String != tc.end || (status == "error") != (streamErr != nil) {
				t.Fatalf("end=%+v status=%s", end, status)
			}
			if tc.consumerStop {
				if seen.Valid || parseErrors.Valid {
					t.Fatal("consumer stop invented evidence for the unread tail")
				}
			} else if !seen.Valid || seen.Bool != tc.finish || !parseErrors.Valid || parseErrors.Int64 != tc.parseErrors {
				t.Fatalf("seen=%+v parse_errors=%+v", seen, parseErrors)
			}
			wantRequests := max(tc.requests, 1)
			if requests.Load() != wantRequests {
				t.Fatalf("retry policy changed: requests=%d want %d", requests.Load(), wantRequests)
			}
		})
	}
}

func TestStreamDiagnosticsUninstrumentedCallsRemainUnknown(t *testing.T) {
	d := streamDiagnosticsDB(t)
	for _, streaming := range []bool{false, true} {
		profile := "unknown-diagnostics-" + uuid.NewString()
		t.Cleanup(func() { _, _ = d.Exec(`DELETE FROM llm_usage WHERE profile_name=$1`, profile) })
		recorder := Wrap(meteredStreamProvider{}, d, "fixture", profile, "", "", func() bool { return false })
		if streaming {
			for _, err := range recorder.Stream(t.Context(), llm.CompletionRequest{}) {
				if err != nil {
					t.Fatal(err)
				}
			}
		} else if _, _, _, err := recorder.Complete(t.Context(), llm.CompletionRequest{}); err != nil {
			t.Fatal(err)
		}
		var unknown bool
		if err := d.QueryRow(`SELECT finish_reason_seen IS NULL AND stream_end IS NULL AND parse_errors IS NULL FROM llm_usage WHERE profile_name=$1`, profile).Scan(&unknown); err != nil {
			t.Fatal(err)
		}
		if !unknown {
			t.Fatalf("streaming=%v invented adapter diagnostics", streaming)
		}
	}
}

func TestOpenAIUsageSurvivesIncompleteStreamWithoutDoubleCounting(t *testing.T) {
	d := streamDiagnosticsDB(t)
	const usage = `data: {"choices":[],"usage":{"prompt_tokens":101,"completion_tokens":7,"prompt_tokens_details":{"cached_tokens":80}}}` + "\n\n"
	const terminal = `data: {"choices":[{"delta":{},"finish_reason":"stop"}]}` + "\n\ndata: [DONE]\n\n"
	for _, incomplete := range []bool{false, true} {
		t.Run(fmt.Sprintf("incomplete=%v", incomplete), func(t *testing.T) {
			profile := "http-usage-" + uuid.NewString()
			t.Cleanup(func() { _, _ = d.Exec(`DELETE FROM llm_usage WHERE profile_name=$1`, profile) })
			field := "content"
			if incomplete {
				field = "reasoning_content"
			}
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				fmt.Fprintf(w, "data: {\"choices\":[{\"delta\":{\"%s\":\"private fixture\"}}]}\n\n%s%s", field, usage, terminal)
			}))
			defer srv.Close()
			provider, err := llm.NewProvider(llm.Config{Format: llm.FormatOpenAI, BaseURL: srv.URL, Model: "fixture", MaxRetries: -1, EmptyResponseRetries: -1})
			if err != nil {
				t.Fatal(err)
			}
			recorder := Wrap(provider, d, "fixture", profile, "", "", func() bool { return false })
			var completionErr error
			for _, err := range recorder.Stream(t.Context(), llm.CompletionRequest{}) {
				if err != nil {
					completionErr = err
				}
			}
			if (completionErr != nil) != incomplete {
				t.Fatalf("completion error=%v incomplete=%v", completionErr, incomplete)
			}
			var input, output, cached int
			var stop, status string
			if err := d.QueryRow(`SELECT input_tokens,output_tokens,cache_read,stop_reason,status FROM llm_usage WHERE profile_name=$1`, profile).Scan(&input, &output, &cached, &stop, &status); err != nil {
				t.Fatal(err)
			}
			if input != 101 || output != 7 || cached != 80 || stop != "end_turn" || (status == "error") != incomplete {
				t.Fatalf("input/output/cache=%d/%d/%d stop=%q status=%q", input, output, cached, stop, status)
			}
		})
	}
}
