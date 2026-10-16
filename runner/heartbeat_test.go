package runner

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestHeartbeatRenewsWithoutDeliveringCommands(t *testing.T) {
	clock := &testClock{now: time.Now()}
	h := newTestHub(t, nil, clock)
	n := registerNode(t, h, defaultCapabilities())
	ch := acquireAsync(h, t.Context(), spec("heartbeat"))
	cmd := n.command(t, CommandOpen, "")
	n.ack(t, n.result(cmd))
	a := await(t, ch)
	if a.err != nil {
		t.Fatal(a.err)
	}
	clock.Advance(2 * time.Second)
	before := h.SessionStatuses()[0].LeaseUntil
	var response PollResponse
	req := HeartbeatRequest{NodeID: n.id, BootID: n.boot, Protocol: ProtocolVersion}
	if code := protocolPOST(t, h, "/heartbeat", n.credential, req, &response); code != 200 {
		t.Fatal(code)
	}
	if !response.Heartbeat || len(response.Commands) != 0 || len(response.Leases) != 1 || !response.Leases[0].Until.After(before) {
		t.Fatalf("heartbeat did not renew independently: %+v", response)
	}
}
func TestHeartbeatStoppedOwnershipBatchValidationAndIdleFailure(t *testing.T) {
	h := newTestHub(t, nil, nil)
	n := registerNode(t, h, defaultCapabilities())
	s := openSession(t, n, spec("idle"))
	other := registerNode(t, h, defaultCapabilities())
	req := HeartbeatRequest{NodeID: n.id, BootID: n.boot, Protocol: ProtocolVersion, Stopped: []StoppedSession{{SessionID: s.id, AttemptID: s.attemptID, Epoch: h.state.Epoch, Reason: "lease_expired"}}}
	forged := req
	forged.NodeID = other.id
	forged.BootID = other.boot
	if err := h.recordStopped(forged, other.id); err != nil {
		t.Fatal(err)
	}
	if h.SessionStatuses()[0].State != "active" {
		t.Fatal("foreign runner fenced session")
	}
	bad := req
	bad.Stopped = append(append([]StoppedSession{}, req.Stopped...), StoppedSession{Reason: "raw private output"})
	if err := h.recordStopped(bad, n.id); err == nil {
		t.Fatal("invalid batch accepted")
	}
	if h.SessionStatuses()[0].State != "active" {
		t.Fatal("invalid batch partially applied")
	}
	done := make(chan error, 1)
	go func() { done <- s.WaitFailure(t.Context()) }()
	if err := h.recordStopped(req, n.id); err != nil {
		t.Fatal(err)
	}
	if err := await(t, done); err == nil || !strings.Contains(err.Error(), "lease_expired") {
		t.Fatalf("idle owner not notified: %v", err)
	}
	revision := h.revision
	if err := h.recordStopped(req, n.id); err != nil || revision != h.revision {
		t.Fatalf("duplicate stop not idempotent: %v", err)
	}
	cmd := n.command(t, CommandClose, s.ID())
	n.ack(t, n.result(cmd))
	if err := s.WaitFailure(t.Context()); err == nil {
		t.Fatal("fast cleanup erased failure before observer read it")
	}
}
func TestStoppedCallIsFatalKnownNotExecuted(t *testing.T) {
	for _, unknown := range []bool{false, true} {
		t.Run(map[bool]string{false: "not_started", true: "unknown_outcome"}[unknown], func(t *testing.T) {
			h := newTestHub(t, nil, nil)
			n := registerNode(t, h, defaultCapabilities())
			s := openSession(t, n, spec("call"))
			done := make(chan error, 1)
			go func() {
				_, e := s.Call(context.Background(), "Bash", json.RawMessage(`{"command":"synthetic"}`), nil)
				done <- e
			}()
			cmd := n.command(t, CommandCall, s.ID())
			result := n.result(cmd)
			result.Error = "session stopped"
			result.UnknownOutcome = unknown
			n.ack(t, result)
			e := await(t, done)
			var fatal interface{ FatalExecution() bool }
			if !errors.As(e, &fatal) || !fatal.FatalExecution() {
				t.Fatalf("session loss was recoverable tool error: %v", e)
			}
			if errors.Is(e, ErrUnknownOutcome) != unknown {
				t.Fatalf("execution outcome misclassified: %v", e)
			}
			if h.SessionStatuses()[0].State != "fenced" {
				t.Fatal("stopped session remained active")
			}
		})
	}
}
func TestDiagnosticProjectionExcludesContent(t *testing.T) {
	h := newTestHub(t, nil, nil)
	n := registerNode(t, h, defaultCapabilities())
	sp := spec("task:1:intent:2:run:3")
	sp.WorkingDir = "/private/task-name"
	sp.Tools[0].Exec = json.RawMessage(`{"secret":"private-command"}`)
	s := openSession(t, n, sp)
	if err := h.abort(s.ID(), "private-output"); err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(h.SessionStatuses())
	for _, secret := range []string{"private/task-name", "private-command", "private-output"} {
		if strings.Contains(string(raw), secret) {
			t.Fatal("diagnostics leaked content")
		}
	}
}
