package runner

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"testing"
	"time"
)

func TestReopenFencesPredecessorAndRetainsReservations(t *testing.T) {
	clock := &testClock{now: time.Now()}
	store := &diagnosticFaultStore{}
	h := newTestHub(t, store, clock)
	caps := defaultCapabilities()
	caps.Slots = 1
	n := registerNode(t, h, caps)
	session := openSession(t, n, spec("old"))
	lease := h.SessionStatuses()[0].LeaseUntil
	store.fail(context.DeadlineExceeded, nil)
	if err := h.abort(session.ID(), "synthetic cancellation"); !errors.Is(err, ErrStoreUnavailable) {
		t.Fatalf("expected failed durable fence: %v", err)
	}
	state := h.BrokerState()
	if !state.Closed || !state.AutoRecovery || state.Reason != "fence_persistence_failed" || state.FailureCode != "store_timeout" {
		t.Fatalf("missing close cause: %+v", state)
	}
	_ = h.Close()
	if h.BrokerState() != state {
		t.Fatal("joining sweep replaced the original failure")
	}
	store.fail(nil, nil)
	next, err := h.Reopen(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = next.Close() })
	if !h.IsClosed() || next.IsClosed() || next == h {
		t.Fatal("old broker revived")
	}
	status := next.SessionStatuses()[0]
	if status.State != "fenced" || !status.LeaseUntil.Equal(lease) || next.ListNodes()[0].ActiveSessions != 1 {
		t.Fatal("recovery dropped or renewed old lease")
	}
	if _, err := next.AuthorizeSession(requestFor(n), session.ID(), true); !errors.Is(err, ErrFenced) {
		t.Fatalf("old file writer admitted: %v", err)
	}
	if _, err := session.Call(t.Context(), "Bash", nil, nil); !errors.Is(err, ErrClosed) || !errors.Is(err, ErrUnknownOutcome) {
		t.Fatalf("old handle transferred to successor: %v", err)
	}
	n.h = next
	var heartbeat PollResponse
	if code := protocolPOST(t, next, "/heartbeat", n.credential, HeartbeatRequest{NodeID: n.id, BootID: n.boot, Protocol: ProtocolVersion}, &heartbeat); code != http.StatusOK || len(heartbeat.Leases) != 0 {
		t.Fatal("recovery renewed predecessor lease")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Millisecond)
	defer cancel()
	if _, err := next.Acquire(ctx, spec("new")); !errors.Is(err, ErrCapacityUnavailable) {
		t.Fatalf("old reservation bypassed: %v", err)
	}
	clock.Advance(6 * time.Second)
	n.poll(t)
	if next.ListNodes()[0].ActiveSessions != 0 {
		t.Fatal("expired reservation was not released")
	}
	openSession(t, n, spec("new"))
}

func TestConflictingOwnerCannotAutoReopen(t *testing.T) {
	store := &MemoryStore{}
	h := newTestHub(t, store, nil)
	n := registerNode(t, h, defaultCapabilities())
	owner := newTestHub(t, store, nil)
	if _, _, err := h.poll(PollRequest{NodeID: n.id, BootID: n.boot, Protocol: ProtocolVersion}, n.id); !errors.Is(err, ErrStoreConflict) {
		t.Fatalf("old owner did not detect takeover: %v", err)
	}
	state := h.BrokerState()
	if !state.Closed || state.AutoRecovery || state.Reason != "store_conflict" {
		t.Fatalf("conflict treated as recoverable: %+v", state)
	}
	if _, err := h.Reopen(t.Context()); !errors.Is(err, ErrClosed) {
		t.Fatal("conflicting owner automatically took over again")
	}
	if _, _, err := owner.CreateRegistrationToken(t.Context(), time.Minute); err != nil {
		t.Fatal("recovery disturbed current owner", err)
	}
}

func TestConcurrentReopenCreatesOnlyOneSuccessor(t *testing.T) {
	store := &diagnosticFaultStore{}
	h := newTestHub(t, store, nil)
	n := registerNode(t, h, defaultCapabilities())
	store.fail(context.DeadlineExceeded, nil)
	_ = h.SetNodeEnabled(n.id, false)
	store.fail(nil, nil)
	results := make(chan *Hub, 16)
	var wg sync.WaitGroup
	for range 16 {
		wg.Go(func() {
			next, err := h.Reopen(t.Context())
			if err != nil && !errors.Is(err, ErrClosed) {
				t.Errorf("reopen: %v", err)
			}
			results <- next
		})
	}
	wg.Wait()
	close(results)
	count := 0
	for next := range results {
		if next != nil {
			count++
			_ = next.Close()
		}
	}
	if count != 1 {
		t.Fatalf("created %d successors", count)
	}
}

func TestReopenDoesNotDisplaceControllerThatTookOverDuringOutage(t *testing.T) {
	store := &diagnosticFaultStore{}
	h := newTestHub(t, store, nil)
	n := registerNode(t, h, defaultCapabilities())
	store.fail(context.DeadlineExceeded, nil)
	_ = h.SetNodeEnabled(n.id, false)
	store.fail(nil, nil)
	owner := newTestHub(t, store, nil)
	before, _ := store.Load(t.Context())
	if _, err := h.Reopen(t.Context()); !errors.Is(err, ErrStoreConflict) {
		t.Fatalf("recovery displaced a different epoch: %v", err)
	}
	after, _ := store.Load(t.Context())
	if after.Revision != before.Revision || string(after.Data) != string(before.Data) || owner.IsClosed() {
		t.Fatal("refused recovery mutated the new owner's state")
	}
}

func TestBrokerCloseStateNeverWaitsForExecutionLock(t *testing.T) {
	h := newTestHub(t, nil, nil)
	h.mu.Lock()
	done := make(chan struct{})
	go func() { _ = h.IsClosed(); _ = h.BrokerState(); close(done) }()
	select {
	case <-done:
		h.mu.Unlock()
	case <-time.After(time.Second):
		h.mu.Unlock()
		t.Fatal("health waited behind execution lock")
	}
}
