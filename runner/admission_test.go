package runner

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func admissionHeartbeat(t *testing.T, n *testNode, when time.Time, waits ...AdmissionWait) {
	t.Helper()
	var out PollResponse
	code := protocolPOST(t, n.h, "/heartbeat", n.credential, HeartbeatRequest{NodeID: n.id, BootID: n.boot, Protocol: ProtocolVersion, PoolStatus: &PoolStatus{Admission: &AdmissionStatus{ObservedAt: when, Waiting: waits}}}, &out)
	if code != 200 || !out.AdmissionDiagnostics {
		t.Fatalf("admission heartbeat: %d %+v", code, out)
	}
}
func TestAdmissionTimeoutKeepsReservationUntilCleanupAndAllowsRetry(t *testing.T) {
	h := newTestHub(t, nil, nil)
	caps := defaultCapabilities()
	caps.Executor = "kubernetes"
	n := registerNode(t, h, caps)
	ctx, cancel := context.WithCancelCause(t.Context())
	defer cancel(context.Canceled)
	sp := spec("capacity-thread")
	ch := acquireAsync(h, ctx, sp)
	cmd := n.command(t, CommandOpen, "")
	now := time.Now()
	admissionHeartbeat(t, n, now, AdmissionWait{SessionID: cmd.SessionID, Stage: "quota", Since: now, Attempts: 2})
	status := h.SessionStatuses()[0]
	if status.CreatedAt.IsZero() || status.Preparation.Stage != "quota" {
		t.Fatalf("missing diagnostics: %+v", status)
	}
	status.Preparation.Stage = "private-untrusted-value"
	if h.SessionStatuses()[0].Preparation.Stage != "quota" {
		t.Fatal("diagnostic snapshot aliases state")
	}
	cancel(context.DeadlineExceeded)
	if r := await(t, ch); !errors.Is(r.err, ErrCapacityUnavailable) || !errors.Is(r.err, context.DeadlineExceeded) {
		t.Fatalf("wrong capacity timeout: %v", r.err)
	}
	if h.ListNodes()[0].ActiveSessions != 1 || h.SessionStatuses()[0].ReasonCode != "runner_capacity_unavailable" {
		t.Fatal("capacity timeout released reservation or lost reason")
	}
	retryCtx, stop := context.WithCancel(t.Context())
	defer stop()
	retry := acquireAsync(h, retryCtx, sp)
	select {
	case r := <-retry:
		t.Fatalf("retry did not await cleanup: %v", r.err)
	case <-time.After(30 * time.Millisecond):
	}
	closeCmd := n.command(t, CommandClose, cmd.SessionID)
	n.ack(t, n.result(closeCmd))
	open := n.command(t, CommandOpen, "")
	if open.SessionID == cmd.SessionID {
		t.Fatal("retry reused fenced sandbox")
	}
	n.ack(t, n.result(open))
	r := await(t, retry)
	if r.err != nil {
		t.Fatal(r.err)
	}
	closeSession(t, n, r.s)
}
func TestAdmissionStaleForeignAndInvalidReportsCannotChangeLifecycle(t *testing.T) {
	clock := &testClock{now: time.Now()}
	h := newTestHub(t, nil, clock)
	caps := defaultCapabilities()
	caps.Executor = "kubernetes"
	n := registerNode(t, h, caps)
	other := registerNode(t, h, caps)
	ctx, cancel := context.WithCancelCause(t.Context())
	defer cancel(context.Canceled)
	sp := spec("isolation")
	sp.NodeID = n.id
	ch := acquireAsync(h, ctx, sp)
	cmd := n.command(t, CommandOpen, "")
	now := clock.Now()
	w := AdmissionWait{SessionID: cmd.SessionID, Stage: "quota", Since: now}
	admissionHeartbeat(t, other, now, w)
	if h.SessionStatuses()[0].Preparation != nil {
		t.Fatal("foreign node changed session")
	}
	admissionHeartbeat(t, n, now.Add(time.Second), w)
	w.Stage = "pod_ready"
	admissionHeartbeat(t, n, now.Add(2*time.Second), w)
	w.Stage = "quota"
	admissionHeartbeat(t, n, now, w)
	if h.SessionStatuses()[0].Preparation.Stage != "pod_ready" {
		t.Fatal("delayed poll overwrote new stage")
	}
	if _, _, err := h.poll(PollRequest{NodeID: n.id, BootID: n.boot, Protocol: ProtocolVersion, PoolStatus: &PoolStatus{}}, n.id); err != nil {
		t.Fatal(err)
	}
	if h.ListNodes()[0].PoolStatus.Admission == nil {
		t.Fatal("pre-negotiation poll erased new heartbeat diagnostics")
	}
	w.Stage = "private prompt"
	req := HeartbeatRequest{NodeID: n.id, BootID: n.boot, Protocol: ProtocolVersion, Stopped: []StoppedSession{{SessionID: cmd.SessionID, AttemptID: cmd.AttemptID, Epoch: cmd.Epoch, Reason: "session_aborted"}}, PoolStatus: &PoolStatus{Admission: &AdmissionStatus{ObservedAt: now.Add(3 * time.Second), Waiting: []AdmissionWait{w}}}}
	if code := protocolPOST(t, h, "/heartbeat", n.credential, req, nil); code != 400 {
		t.Fatalf("invalid metadata accepted: %d", code)
	}
	if h.SessionStatuses()[0].State != "opening" {
		t.Fatal("invalid heartbeat partially applied")
	}
	cancel(context.DeadlineExceeded)
	if r := await(t, ch); errors.Is(r.err, ErrCapacityUnavailable) {
		t.Fatal("image readiness timeout mistaken for capacity")
	}
}
func TestAdmissionCancellationAndStaleQuotaAreNotRetrySignals(t *testing.T) {
	for _, stale := range []bool{false, true} {
		t.Run(map[bool]string{false: "cancelled", true: "stale"}[stale], func(t *testing.T) {
			clock := &testClock{now: time.Now()}
			h := newTestHub(t, nil, clock)
			caps := defaultCapabilities()
			caps.Executor = "kubernetes"
			n := registerNode(t, h, caps)
			ctx, cancel := context.WithCancelCause(t.Context())
			defer cancel(context.Canceled)
			ch := acquireAsync(h, ctx, spec("cancel"))
			cmd := n.command(t, CommandOpen, "")
			admissionHeartbeat(t, n, clock.Now(), AdmissionWait{SessionID: cmd.SessionID, Stage: "quota", Since: clock.Now()})
			if stale {
				clock.Advance(h.opts.LeaseDuration)
				cancel(context.DeadlineExceeded)
			} else {
				cancel(context.Canceled)
			}
			if r := await(t, ch); errors.Is(r.err, ErrCapacityUnavailable) {
				t.Fatalf("unsafe retry signal: %v", r.err)
			}
		})
	}
}
func TestQuotaStatusRejectsPrivateFields(t *testing.T) {
	p := &PoolStatus{Admission: &AdmissionStatus{ObservedAt: time.Now(), LastQuotaRejection: &QuotaRejection{At: time.Now(), Name: "pool", Used: map[string]string{"requests.cpu": "private value"}}}}
	if validatePoolStatus(p) == nil {
		t.Fatal("non-numeric metadata accepted")
	}
	p.Admission.LastQuotaRejection.Used = map[string]string{"private field": "1"}
	if validatePoolStatus(p) == nil {
		t.Fatal("unbounded resource labels accepted")
	}
	p.Admission.LastQuotaRejection.Used = map[string]string{"requests.cpu": "16100m"}
	if err := validatePoolStatus(p); err != nil {
		t.Fatal(err)
	}
	p.Admission.Waiting = []AdmissionWait{{SessionID: strings.Repeat("x", 257), Since: time.Now(), Stage: "quota"}}
	if validatePoolStatus(p) == nil {
		t.Fatal("unbounded session ID accepted")
	}
}
