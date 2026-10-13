package runner

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestCleanupAcknowledgmentPreservesOriginalFailure(t *testing.T) {
	h := newTestHub(t, nil, nil)
	n := registerNode(t, h, defaultCapabilities())
	s := openSession(t, n, spec("failed-checkpoint"))
	call := callAsync(s, context.Background(), "write", "{}")
	cmd := n.command(t, CommandCall, s.ID())
	r := n.result(cmd)
	r.UnknownOutcome = true
	r.Error = "publish result.bin: remote version conflict"
	n.ack(t, r)
	if got := await(t, call); got.err == nil {
		t.Fatal("unknown outcome ignored")
	}
	if err := s.Close(); err == nil || !strings.Contains(err.Error(), r.Error) {
		t.Fatalf("fencing hid root cause: %v", err)
	}
	closeCmd := n.command(t, CommandClose, s.ID())
	n.ack(t, n.result(closeCmd))
	h.mu.Lock()
	state, reason := h.state.Sessions[s.ID()].State, h.state.Sessions[s.ID()].Reason
	h.mu.Unlock()
	if state != "closed" || !strings.Contains(reason, r.Error) {
		t.Fatalf("cleanup erased failure: state=%s reason=%q", state, reason)
	}
	if err := s.Close(); err == nil || !strings.Contains(err.Error(), r.Error) {
		t.Fatalf("repeated Close lost failure: %v", err)
	}
}

func TestFinalPublicationErrorSurvivesRepeatedClose(t *testing.T) {
	h := newTestHub(t, nil, nil)
	n := registerNode(t, h, defaultCapabilities())
	s := openSession(t, n, spec("final-publication"))
	done := make(chan error, 1)
	go func() { done <- s.Close() }()
	cmd := n.command(t, CommandClose, s.ID())
	r := n.result(cmd)
	r.Error = "final file publication: connection unavailable"
	n.ack(t, r)
	first, second := await(t, done), s.Close()
	if first == nil || second == nil || first.Error() != r.Error || second.Error() != r.Error {
		t.Fatalf("close results disagree: %v, %v", first, second)
	}
}

func TestFailedCleanupRestartAndExpiryPreserveRootCause(t *testing.T) {
	store := &MemoryStore{}
	clock := &testClock{now: time.Now()}
	h := newTestHub(t, store, clock)
	n := registerNode(t, h, defaultCapabilities())
	s := openSession(t, n, spec("cleanup-unconfirmed"))
	call := callAsync(s, context.Background(), "write", "{}")
	cmd := n.command(t, CommandCall, s.ID())
	r := n.result(cmd)
	r.UnknownOutcome, r.Error = true, "publish result.bin: disk full"
	n.ack(t, r)
	_ = await(t, call)
	closeCmd := n.command(t, CommandClose, s.ID())
	closeResult := n.result(closeCmd)
	closeResult.UnknownOutcome, closeResult.Error = true, "runtime unavailable"
	n.ack(t, closeResult)
	if err := s.Close(); err == nil || !strings.Contains(err.Error(), r.Error) || !strings.Contains(err.Error(), closeResult.Error) {
		t.Fatalf("cleanup lost original error: %v", err)
	}
	_ = h.Close()
	next := newTestHub(t, store, clock)
	clock.Advance(10 * time.Second)
	next.mu.Lock()
	next.expireLocked()
	record := next.state.Sessions[s.ID()]
	state, reason := record.State, record.Reason
	next.mu.Unlock()
	if state != "expired" || !strings.Contains(reason, r.Error) || !strings.Contains(reason, closeResult.Error) || !strings.Contains(reason, "controller restarted") {
		t.Fatalf("restart/expiry erased cause: state=%s reason=%q", state, reason)
	}
}
