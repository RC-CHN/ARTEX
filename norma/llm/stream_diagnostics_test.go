package llm

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestOpenAIStreamDiagnosticsEndEvidence(t *testing.T) {
	thinking := `data: {"choices":[{"delta":{"reasoning_content":"private synthetic thinking"}}]}` + "\n\n"
	finish := `data: {"choices":[{"delta":{},"finish_reason":"stop"}]}` + "\n\n"
	done := "data: [DONE]\n\n"
	for _, tc := range []struct {
		name, body     string
		wantEnd        StreamEnd
		wantFinish     bool
		parseErrors    int
		readFailure    bool
		requestError   bool
		wantIncomplete IncompleteResponseReason
	}{
		{name: "done_without_finish", body: thinking + done, wantEnd: StreamEndDone, wantIncomplete: IncompleteEmptyOutput},
		{name: "eof_without_finish", body: thinking, wantEnd: StreamEndEOF, wantIncomplete: IncompleteMissingTerminal},
		{name: "finish_and_done", body: thinking + finish + done, wantEnd: StreamEndDone, wantFinish: true, wantIncomplete: IncompleteEmptyOutput},
		{name: "finish_then_eof", body: thinking + finish, wantEnd: StreamEndEOF, wantFinish: true, wantIncomplete: IncompleteEmptyOutput},
		{name: "malformed_frames", body: thinking + "data: {private-invalid-json\n\ndata: {\"choices\":42}\n\n" + finish + done, wantEnd: StreamEndProtocolError, parseErrors: 1, wantIncomplete: IncompleteMalformedFrame},
		{name: "valid_unknown_frame", body: thinking + "data: {\"unknown\":\"private synthetic payload\"}\n\n" + done, wantEnd: StreamEndDone, wantIncomplete: IncompleteEmptyOutput},
		{name: "read_error", body: thinking, wantEnd: StreamEndReadError, readFailure: true, wantIncomplete: IncompleteStreamRead},
		{name: "http_failure", body: `{"error":"private upstream failure"}`, wantEnd: StreamEndRequestError, requestError: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var requests atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests.Add(1)
				w.Header().Set("X-Fixture-Secret", "private synthetic header")
				if tc.readFailure {
					w.Header().Set("Content-Length", strconv.Itoa(len(tc.body)+100))
				}
				if tc.requestError {
					w.WriteHeader(http.StatusBadGateway)
				}
				_, _ = w.Write([]byte(tc.body))
			}))
			defer srv.Close()
			provider, err := NewProvider(Config{Format: FormatOpenAI, BaseURL: srv.URL, Model: "fixture", APIKey: "private fixture key", MaxRetries: -1, EmptyResponseRetries: -1})
			if err != nil {
				t.Fatal(err)
			}
			acc := NewAccumulator()
			var diagnostics []*StreamDiagnostics
			var streamErr error
			for event, err := range provider.Stream(t.Context(), CompletionRequest{Messages: []Message{UserText("private synthetic request")}}) {
				if err != nil {
					if len(diagnostics) != 1 {
						t.Fatal("error surfaced before its structural diagnostics")
					}
					streamErr = err
					break
				}
				if event.Type == SEStreamDiagnostics {
					if event.Diagnostics == nil {
						t.Fatal("diagnostic event has no typed payload")
					}
					diagnostics = append(diagnostics, event.Diagnostics)
					raw, _ := json.Marshal(event)
					if strings.Contains(string(raw), "private") || event.Text != "" || event.ToolID != "" || event.ToolName != "" {
						t.Fatalf("diagnostics included response/request/header data: %s", raw)
					}
				}
				acc.Add(event)
			}
			if (streamErr != nil) != (tc.wantIncomplete != "" || tc.requestError) {
				t.Fatalf("error behavior changed: %v", streamErr)
			}
			if tc.wantIncomplete != "" {
				assertIncomplete(t, streamErr, tc.wantIncomplete)
			}
			if requests.Load() != 1 || len(diagnostics) != 1 {
				t.Fatalf("requests=%d diagnostics=%d", requests.Load(), len(diagnostics))
			}
			got := *diagnostics[0]
			want := StreamDiagnostics{FinishReasonSeen: tc.wantFinish, StreamEnd: tc.wantEnd, ParseErrors: tc.parseErrors}
			if got != want {
				t.Fatalf("diagnostics=%+v want %+v", got, want)
			}
			if !tc.requestError {
				message := acc.Message()
				if len(message.Content) != 1 || message.Content[0].Thinking != "private synthetic thinking" {
					t.Fatalf("diagnostics changed accumulated content: %+v", message)
				}
			}
		})
	}
}

func TestOpenAIStreamDiagnosticsDoNotPreventEmptyRetry(t *testing.T) {
	var requests atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if requests.Add(1) == 1 {
			_, _ = fmt.Fprint(w, emptyStop)
			return
		}
		_, _ = fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"fixture\"}}]}\n\ndata: [DONE]\n\n")
	}))
	defer srv.Close()
	provider, err := NewProvider(Config{Format: FormatOpenAI, BaseURL: srv.URL, Model: "fixture", EmptyResponseRetries: 1, EmptyResponseInterval: time.Nanosecond})
	if err != nil {
		t.Fatal(err)
	}
	var diagnostics []StreamDiagnostics
	for event, err := range provider.Stream(t.Context(), CompletionRequest{}) {
		if err != nil {
			t.Fatal(err)
		}
		if event.Diagnostics != nil {
			diagnostics = append(diagnostics, *event.Diagnostics)
		}
	}
	if requests.Load() != 2 || len(diagnostics) != 2 || !diagnostics[0].FinishReasonSeen || diagnostics[0].ParseErrors != 0 || diagnostics[1].FinishReasonSeen || diagnostics[1].ParseErrors != 0 {
		t.Fatalf("requests=%d diagnostics=%+v", requests.Load(), diagnostics)
	}
}

func TestOpenAIStreamDiagnosticsRespectConsumerStop(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"fixture\"}}]}\n\ndata: [DONE]\n\n")
	}))
	defer srv.Close()
	provider, err := NewProvider(Config{Format: FormatOpenAI, BaseURL: srv.URL, Model: "fixture"})
	if err != nil {
		t.Fatal(err)
	}
	seen := 0
	for event, err := range provider.Stream(context.Background(), CompletionRequest{}) {
		if err != nil {
			t.Fatal(err)
		}
		if event.Type != SETextDelta {
			t.Fatalf("unexpected event before content: %s", event.Type)
		}
		seen++
		break // Any attempt to yield diagnostics after this must panic in Go's iterator runtime.
	}
	if seen != 1 {
		t.Fatal("missing content event")
	}
}
