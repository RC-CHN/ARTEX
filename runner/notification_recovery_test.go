package runner

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/RC-CHN/ARTEX/norma/tool"
)

func notificationFor(s *Session, id string) NotificationEnvelope {
	return NotificationEnvelope{ID: id, SessionID: s.id, AttemptID: s.attemptID,
		Items: []tool.Notification{{TaskID: "synthetic", Summary: "synthetic completion"}}}
}

// Reproduce #3199/#3167: the first notification after a failed checkpoint used
// to panic on a nil map and poison renewal of every session on the same node.
func TestNotificationsAndLeasesSurviveCheckpointRollback(t *testing.T) {
	clock := &testClock{now: time.Now()}
	store := &diagnosticFaultStore{}
	h := newTestHub(t, store, clock)
	n := registerNode(t, h, defaultCapabilities())
	first := openSession(t, n, spec("synthetic-one"))
	second := openSession(t, n, spec("synthetic-two"))
	initial := h.SessionStatuses()[0].LeaseUntil
	req := PollRequest{NodeID: n.id, BootID: n.boot, Protocol: ProtocolVersion,
		Notifications: []NotificationEnvelope{notificationFor(first, "notice")}}

	clock.Advance(2 * time.Second)
	store.fail(context.DeadlineExceeded, nil)
	response, _, err := h.poll(req, n.id)
	if !errors.Is(err, ErrStoreUnavailable) || len(response.AcknowledgedNotifications) != 0 {
		t.Fatalf("uncommitted notification acknowledged: %+v %v", response, err)
	}
	store.fail(nil, nil)
	for _, s := range h.SessionStatuses() {
		if s.State != "active" || !s.LeaseUntil.Equal(initial) {
			t.Fatal("failed save published an uncommitted lease")
		}
	}
	// Run beyond the original lease, retrying the same envelope. Both sessions
	// must remain live and exactly one notification must survive another rollback.
	for i := range 4 {
		if i > 0 {
			clock.Advance(2 * time.Second)
		}
		response, _, err = h.poll(req, n.id)
		if err != nil || len(response.AcknowledgedNotifications) != 1 || len(response.Leases) != 2 {
			t.Fatalf("notification retry did not recover: %+v %v", response, err)
		}
		for _, l := range response.Leases {
			if !l.Until.After(clock.Now()) || !l.Until.After(initial) {
				t.Fatal("session lease was not renewed")
			}
		}
		if i == 1 {
			clock.Advance(2 * time.Second)
			store.fail(context.DeadlineExceeded, nil)
			if _, _, err := h.poll(req, n.id); !errors.Is(err, ErrStoreUnavailable) {
				t.Fatal("expected second checkpoint rollback", err)
			}
			store.fail(nil, nil)
		}
	}
	// The same round-trip also omits an empty CallIDs map. Exercise a real
	// first call after rollback instead of checking notification handling alone.
	callDone := make(chan error, 1)
	go func() {
		_, err := second.Call(t.Context(), "Bash", json.RawMessage(`{}`), nil)
		callDone <- err
	}()
	command := n.command(t, CommandCall, second.id)
	n.ack(t, n.result(command))
	if err := await(t, callDone); err != nil {
		t.Fatal("first call after rollback failed", err)
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, id := range []string{first.id, second.id} {
		s := h.state.Sessions[id]
		if s.State != "active" || s.Commands == nil || s.CallIDs == nil || s.NotificationIDs == nil {
			t.Fatal("rollback did not restore writable session state")
		}
	}
	if len(h.state.Sessions[first.id].Notifications) != 1 || len(h.state.Sessions[first.id].NotificationIDs) != 1 {
		t.Fatal("rollback lost notification deduplication")
	}
}

func TestHeartbeatRepairsLaggingLiveLeaseOnly(t *testing.T) {
	clock := &testClock{now: time.Now()}
	h := newTestHub(t, nil, clock)
	n := registerNode(t, h, defaultCapabilities())
	live := openSession(t, n, spec("live"))
	fenced := openSession(t, n, spec("fenced"))
	before := h.SessionStatuses()[0].LeaseUntil
	if err := h.abort(fenced.id, "synthetic stop"); err != nil {
		t.Fatal(err)
	}
	clock.Advance(2 * time.Second)
	h.mu.Lock()
	// Simulate a recent node observation whose per-session renewal was missed.
	h.state.Nodes[n.id].LastSeen = clock.Now()
	h.mu.Unlock()
	var response PollResponse
	if code := protocolPOST(t, h, "/heartbeat", n.credential, HeartbeatRequest{NodeID: n.id, BootID: n.boot, Protocol: ProtocolVersion}, &response); code != 200 {
		t.Fatal(code)
	}
	if len(response.Leases) != 1 || response.Leases[0].SessionID != live.id || !response.Leases[0].Until.After(before) {
		t.Fatalf("recent node timestamp hid lagging lease: %+v", response)
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if s := h.state.Sessions[fenced.id]; s.State != "fenced" || !s.LeaseUntil.Equal(before) {
		t.Fatal("heartbeat renewed a fenced attempt")
	}
}

func TestConcurrentNotificationRetriesAfterRollback(t *testing.T) {
	clock := &testClock{now: time.Now()}
	store := &diagnosticFaultStore{}
	h := newTestHub(t, store, clock)
	n := registerNode(t, h, defaultCapabilities())
	s := openSession(t, n, spec("concurrent"))
	clock.Advance(2 * time.Second)
	store.fail(context.DeadlineExceeded, nil)
	if _, _, err := h.poll(PollRequest{NodeID: n.id, BootID: n.boot, Protocol: ProtocolVersion}, n.id); !errors.Is(err, ErrStoreUnavailable) {
		t.Fatal(err)
	}
	store.fail(nil, nil)
	var wg sync.WaitGroup
	for i := range 32 {
		wg.Go(func() {
			req := PollRequest{NodeID: n.id, BootID: n.boot, Protocol: ProtocolVersion}
			if i%2 == 0 {
				req.Notifications = []NotificationEnvelope{notificationFor(s, fmt.Sprintf("notice-%d", i%8))}
			}
			for range 3 {
				if _, _, err := h.pollContext(t.Context(), req, n.id, i%2 != 0); err != nil {
					t.Errorf("concurrent renewal failed: %v", err)
				}
			}
		})
	}
	wg.Wait()
	h.mu.Lock()
	defer h.mu.Unlock()
	if got := h.state.Sessions[s.id]; got.State != "active" || len(got.Notifications) != 4 || len(got.NotificationIDs) != 4 {
		t.Fatal("concurrent retry lost or duplicated notifications")
	}
}

type panicCheckpointStore struct {
	MemoryStore
	mu          sync.Mutex
	panicSave   bool
	panicGuard  bool
	commitFirst bool
}

func (s *panicCheckpointStore) Save(ctx context.Context, rev uint64, data json.RawMessage) (uint64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.panicSave {
		s.panicSave = false
		if s.commitFirst {
			if _, err := s.MemoryStore.Save(ctx, rev, data); err != nil {
				return 0, err
			}
		}
		panic("PRIVATE panic containing a synthetic secret")
	}
	return s.MemoryStore.Save(ctx, rev, data)
}

func (s *panicCheckpointStore) Guard(ctx context.Context, rev uint64, commit func() error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.panicGuard {
		s.panicGuard = false
		panic("PRIVATE guard panic")
	}
	return s.MemoryStore.Guard(ctx, rev, commit)
}

func TestPollPanicRollsBackAndRecordsSanitizedIncident(t *testing.T) {
	for _, mode := range []string{"save", "guard", "committed_save", "boot_change", "bad_checkpoint"} {
		t.Run(mode, func(t *testing.T) {
			clock := &testClock{now: time.Now()}
			store := &panicCheckpointStore{}
			h := newTestHub(t, store, clock)
			n := registerNode(t, h, defaultCapabilities())
			s := openSession(t, n, spec("synthetic"))
			before := h.SessionStatuses()[0].LeaseUntil
			h.mu.Lock()
			seen, revision := h.state.Nodes[n.id].LastSeen, h.revision
			h.mu.Unlock()
			var events []Event
			var eventMu sync.Mutex
			h.opts.RecordEvent = func(event Event) { eventMu.Lock(); defer eventMu.Unlock(); events = append(events, event) }
			store.mu.Lock()
			if mode == "guard" {
				store.panicGuard = true
			} else {
				store.panicSave, store.commitFirst = true, mode == "committed_save"
				clock.Advance(2 * time.Second)
			}
			store.mu.Unlock()
			req := PollRequest{NodeID: n.id, BootID: n.boot, Protocol: ProtocolVersion}
			if mode != "guard" {
				req.Notifications = []NotificationEnvelope{notificationFor(s, "uncommitted")}
			}
			if mode == "boot_change" {
				req.BootID = "new-synthetic-boot"
			}
			if mode == "bad_checkpoint" {
				h.mu.Lock()
				h.lastSaved = json.RawMessage(`{"invalid":`)
				h.mu.Unlock()
			}
			var failure struct{ Code, Error string }
			// protocolPOST only decodes success; verify the public error separately.
			out, _, err := h.poll(req, n.id)
			if !errors.Is(err, errOperationPanic) || len(out.Leases) != 0 || len(out.AcknowledgedNotifications) != 0 {
				t.Fatalf("panic published a partial response: %+v %v", out, err)
			}
			failure.Code, failure.Error = FailureCode(err), err.Error()
			if failure.Code != "internal_panic" || !KnownFailureCode(failure.Code) || strings.Contains(failure.Error, "PRIVATE") {
				t.Fatal("panic response leaked content or lost classification")
			}
			manual := mode == "boot_change" || mode == "bad_checkpoint"
			if h.IsClosed() != manual || manual && h.BrokerState().AutoRecovery {
				t.Fatal("unsafe continuation after failed recovery/fence")
			}
			if mode != "bad_checkpoint" {
				h.mu.Lock()
				unchanged := h.state.Nodes[n.id].LastSeen.Equal(seen) && h.state.Sessions[s.id].LeaseUntil.Equal(before) && len(h.state.Sessions[s.id].Notifications) == 0 && h.revision == revision
				h.mu.Unlock()
				if !unchanged {
					t.Fatal("panic retained partial node, lease or notification mutation")
				}
			}
			d := h.Diagnostics()
			if d.Operations["poll"].LastFailureCode != "internal_panic" {
				t.Fatal("panic was reported as a successful poll")
			}
			for name, status := range d.Operations {
				if status.InFlight != 0 || status.Waiting != 0 {
					t.Fatalf("panic leaked observation: %s %+v", name, status)
				}
			}
			eventMu.Lock()
			raw, _ := json.Marshal(events)
			incident := false
			for _, e := range events {
				if e.Kind == "operation_panic" && e.Operation == "poll" && e.Code == "internal_panic" && e.IncidentID != "" && e.Recovered == !manual && e.ManualRequired == manual {
					incident = true
				}
			}
			eventMu.Unlock()
			if !incident || strings.Contains(string(raw), "PRIVATE") {
				t.Fatal("missing or unsafe panic journal event")
			}
			if manual {
				return
			}
			_, _, err = h.poll(req, n.id)
			if mode == "committed_save" {
				if !errors.Is(err, ErrStoreConflict) || !h.IsClosed() {
					t.Fatal("ambiguous commit bypassed revision fence", err)
				}
			} else if err != nil || h.IsClosed() {
				t.Fatal("poll could not safely retry after rollback", err)
			}
		})
	}
}

func TestHeartbeatPanicReturnsSafeHTTPError(t *testing.T) {
	store := &panicCheckpointStore{}
	h := newTestHub(t, store, nil)
	n := registerNode(t, h, defaultCapabilities())
	store.mu.Lock()
	store.panicGuard = true
	store.mu.Unlock()
	if code := protocolPOST(t, h, "/heartbeat", n.credential, HeartbeatRequest{NodeID: n.id, BootID: n.boot, Protocol: ProtocolVersion}, nil); code != http.StatusInternalServerError {
		t.Fatalf("panic should return HTTP 500, got %d", code)
	}
}
