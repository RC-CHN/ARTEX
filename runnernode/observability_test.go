package runnernode

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/RC-CHN/ARTEX/runner"
)

func TestHeartbeatStoreFailureRetainsDiagnosticsAndRecovers(t *testing.T) {
	n, _ := testNode(t)
	openNode(t, n)
	defer n.stopAll()
	n.admissionEnabled.Store(true)
	var logs bytes.Buffer
	n.Log = log.New(&logs, "", 0)
	var healthy atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !healthy.Load() {
			w.WriteHeader(503)
			io.WriteString(w, `{"code":"store_timeout","error":"PRIVATE SQL TOOL OUTPUT"}`)
			return
		}
		now := time.Now()
		json.NewEncoder(w).Encode(runner.PollResponse{Epoch: "epoch", ServerTime: now, Leases: []runner.Lease{{SessionID: "s", AttemptID: "a", Epoch: "epoch", Until: now.Add(time.Minute)}}})
	}))
	defer srv.Close()
	n.Config.ServerURL = srv.URL
	n.HTTP = srv.Client()
	before := n.leases["s"].Until
	n.heartbeat(t.Context())
	if !n.admissionEnabled.Load() || !n.leases["s"].Until.Equal(before) || n.sessions["s"].ctx.Err() != nil {
		t.Fatal("temporary store failure disabled diagnostics or revoked a valid lease")
	}
	healthy.Store(true)
	n.heartbeat(t.Context())
	if !n.leases["s"].Until.After(before) {
		t.Fatal("heartbeat failed to renew after recovery")
	}
	if text := logs.String(); !strings.Contains(text, "code=store_timeout http_status=503") || !strings.Contains(text, "recovered=true") || strings.Contains(text, "PRIVATE") {
		t.Fatalf("missing/private diagnostic: %s", text)
	}
}

func TestRequestDiagnosticsRejectPrivateCodesAndClassifyTransport(t *testing.T) {
	for _, test := range []struct {
		body, want string
		status     int
	}{
		{`{"code":"secret-target-password","error":"private response"}`, "http_error", 503},
		{`{"code":"store_conflict"}`, "store_conflict", 503},
		{`{"code":"internal_panic","error":"private panic value"}`, "internal_panic", 500},
		{`{"error":"legacy private response"}`, "http_error", 400},
		{`<private gateway error>`, "http_error", 502},
		{`{"epoch":`, "invalid_response", 200},
	} {
		t.Run(test.want+test.body[:1], func(t *testing.T) {
			n, _ := testNode(t)
			var logs bytes.Buffer
			n.Log = log.New(&logs, "", 0)
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(test.status); io.WriteString(w, test.body) }))
			defer srv.Close()
			n.Config.ServerURL = srv.URL
			n.HTTP = srv.Client()
			var out runner.PollResponse
			err := n.request(t.Context(), "heartbeat", runner.HeartbeatRequest{}, &out)
			code, _ := requestFailure(err)
			if code != test.want {
				t.Fatalf("got %s, want %s", code, test.want)
			}
			if text := logs.String(); strings.Contains(text, "private") || strings.Contains(text, "secret-target-password") {
				t.Fatal("response body leaked into diagnostics")
			}
		})
	}
	for _, test := range []struct {
		err  error
		code string
	}{
		{context.Canceled, "canceled"}, {context.DeadlineExceeded, "deadline_exceeded"}, {io.ErrUnexpectedEOF, "invalid_response"}, {errors.New("private-url-and-password"), "request_error"},
	} {
		if code, _ := requestFailure(test.err); code != test.code {
			t.Fatalf("got %s want %s", code, test.code)
		}
	}
}

func TestHeartbeatLegacyFallbackAndDiagnosticRateLimit(t *testing.T) {
	n, _ := testNode(t)
	n.admissionEnabled.Store(true)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(400)
		io.WriteString(w, `{"error":"unknown optional field"}`)
	}))
	defer srv.Close()
	n.Config.ServerURL = srv.URL
	n.HTTP = srv.Client()
	var logs bytes.Buffer
	n.Log = log.New(&logs, "", 0)
	n.heartbeat(t.Context())
	if n.admissionEnabled.Load() {
		t.Fatal("old-controller compatibility fallback removed")
	}
	logs.Reset()
	err := &httpStatusError{route: "heartbeat", status: 503, code: "store_timeout"}
	for range 100 {
		n.observeRequest("heartbeat", time.Now(), err, 503)
	}
	if strings.Count(logs.String(), "runner_transport") > 1 {
		t.Fatal("repeated errors flooded logs")
	}
	n.observeRequest("heartbeat", time.Now(), nil, 200)
	if !strings.Contains(logs.String(), "recovered=true") {
		t.Fatal("recovery missing")
	}
}
