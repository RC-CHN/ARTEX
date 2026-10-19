package runner

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

type diagnosticFaultStore struct {
	MemoryStore
	faultMu      sync.RWMutex
	failure      error
	guardFailure error
}

func (s *diagnosticFaultStore) Save(ctx context.Context, rev uint64, raw json.RawMessage) (uint64, error) {
	s.faultMu.RLock()
	defer s.faultMu.RUnlock()
	if s.failure != nil {
		return 0, s.failure
	}
	return s.MemoryStore.Save(ctx, rev, raw)
}
func (s *diagnosticFaultStore) Guard(ctx context.Context, rev uint64, f func() error) error {
	s.faultMu.RLock()
	defer s.faultMu.RUnlock()
	if s.guardFailure != nil {
		return s.guardFailure
	}
	return s.MemoryStore.Guard(ctx, rev, f)
}

func (s *diagnosticFaultStore) fail(write, guard error) {
	s.faultMu.Lock()
	defer s.faultMu.Unlock()
	s.failure, s.guardFailure = write, guard
}

type diagnosticLogBuffer struct {
	mu sync.Mutex
	bytes.Buffer
}

func (b *diagnosticLogBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.Buffer.Write(p)
}
func (b *diagnosticLogBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.Buffer.String()
}

func TestPersistenceFailureIsRetryableAndDoesNotPublishLease(t *testing.T) {
	clock := &testClock{now: time.Now()}
	store := &diagnosticFaultStore{}
	h := newTestHub(t, store, clock)
	n := registerNode(t, h, defaultCapabilities())
	s := openSession(t, n, spec("synthetic"))
	before := h.SessionStatuses()[0].LeaseUntil
	clock.Advance(2 * time.Second)
	store.fail(errors.Join(context.DeadlineExceeded, errors.New("PRIVATE SQL OR OUTPUT")), nil)
	var logs diagnosticLogBuffer
	h.opts.Log = log.New(&logs, "", 0)
	req := HeartbeatRequest{NodeID: n.id, BootID: n.boot, Protocol: ProtocolVersion}
	if status := protocolPOST(t, h, "/heartbeat", n.credential, req, nil); status != http.StatusServiceUnavailable {
		t.Fatalf("store timeout returned %d", status)
	}
	if got := h.SessionStatuses()[0]; got.State != "active" || !got.LeaseUntil.Equal(before) {
		t.Fatal("failed renewal published uncommitted lease")
	}
	d := h.Diagnostics()
	if d.Operations["checkpoint"].LastFailureCode != "store_timeout" || d.Operations["checkpoint_rollback"].Calls == 0 {
		t.Fatalf("missing rollback diagnosis: %+v", d)
	}
	store.fail(nil, nil)
	var response PollResponse
	if status := protocolPOST(t, h, "/heartbeat", n.credential, req, &response); status != 200 || len(response.Leases) != 1 || !response.Leases[0].Until.After(before) {
		t.Fatal("renewal did not recover")
	}
	if h.Diagnostics().Operations["checkpoint"].ConsecutiveFailures != 0 || !strings.Contains(logs.String(), "recovered=true") || strings.Contains(logs.String(), "PRIVATE") {
		t.Fatal("recovery diagnostics missing or leaked data")
	}
	store.fail(nil, errors.New("PRIVATE database connection error"))
	if status := protocolPOST(t, h, "/heartbeat", n.credential, req, nil); status != 503 {
		t.Fatalf("guard failure returned %d", status)
	}
	store.fail(nil, nil)
	// A real lease expiry must still fence this attempt after successful recovery.
	clock.Advance(6 * time.Second)
	if _, _, err := h.poll(PollRequest{NodeID: n.id, BootID: n.boot, Protocol: ProtocolVersion}, n.id); err != nil {
		t.Fatal(err)
	}
	if h.SessionStatuses()[0].State != "expired" {
		t.Fatal("expired attempt was revived")
	}
	if err := s.WaitFailure(t.Context()); err == nil {
		t.Fatal("lost attempt was not fenced")
	}
}

func TestCancelledBrokerRequestsLeaveQueueWithoutRenewing(t *testing.T) {
	clock := &testClock{now: time.Now()}
	h := newTestHub(t, nil, clock)
	n := registerNode(t, h, defaultCapabilities())
	openSession(t, n, spec("synthetic"))
	clock.Advance(2 * time.Second)
	h.mu.Lock()
	locked := true
	defer func() {
		if locked {
			h.mu.Unlock()
		}
	}()
	revision := h.revision
	before := h.state.Nodes[n.id].LastSeen
	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Go(func() {
			ctx, cancel := context.WithTimeout(t.Context(), 40*time.Millisecond)
			defer cancel()
			_, _, err := h.pollContext(ctx, PollRequest{NodeID: n.id, BootID: n.boot, Protocol: ProtocolVersion}, n.id, true)
			if !errors.Is(err, context.DeadlineExceeded) {
				t.Errorf("waiting request: %v", err)
			}
		})
	}
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	// These calls must finish while the execution mutex is still held.
	await(t, done)
	d := h.Diagnostics()
	if d.Operations["heartbeat"].Failures != 32 || d.Operations["heartbeat"].InFlight != 0 || d.Operations["heartbeat"].Waiting != 0 {
		t.Fatalf("queue not drained: %+v", d)
	}
	if h.revision != revision || !h.state.Nodes[n.id].LastSeen.Equal(before) {
		t.Fatal("canceled request mutated lease")
	}
	h.mu.Unlock()
	locked = false
	if _, _, err := h.poll(PollRequest{NodeID: n.id, BootID: n.boot, Protocol: ProtocolVersion}, n.id); err != nil {
		t.Fatal(err)
	}
	if h.revision != revision+1 {
		t.Fatal("obsolete requests performed extra saves")
	}
}

func TestProtocolCancellationAndErrorPrivacy(t *testing.T) {
	h := newTestHub(t, nil, nil)
	n := registerNode(t, h, defaultCapabilities())
	h.mu.Lock()
	for _, route := range []string{"/heartbeat", "/poll", "/results"} {
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		r := httptest.NewRequest("POST", route, strings.NewReader(`{}`)).WithContext(ctx)
		r.Header.Set("Authorization", "Bearer "+n.credential)
		w := httptest.NewRecorder()
		done := make(chan struct{})
		go func() { h.Handler().ServeHTTP(w, r); close(done) }()
		select {
		case <-done:
		case <-time.After(time.Second):
			h.mu.Unlock()
			t.Fatal("canceled HTTP request stuck on authentication")
		}
		if w.Code != 408 {
			h.mu.Unlock()
			t.Fatalf("canceled request returned %d", w.Code)
		}
	}
	h.mu.Unlock()
	w := httptest.NewRecorder()
	protocolError(w, storeError(errors.Join(context.DeadlineExceeded, errors.New("private-command-secret"))))
	if w.Code != 503 || strings.Contains(w.Body.String(), "private-command-secret") || !strings.Contains(w.Body.String(), `"code":"store_timeout"`) {
		t.Fatalf("unsafe protocol error: %s", w.Body.String())
	}
}

func TestDiagnosticReasonsPreserveFirstFailure(t *testing.T) {
	h := newTestHub(t, nil, nil)
	n := registerNode(t, h, defaultCapabilities())
	s := openSession(t, n, spec("synthetic"))
	h.mu.Lock()
	h.state.Sessions[s.ID()].Reason = "execution_session_lost: lease_revoked; private-data; runner lease expired"
	h.mu.Unlock()
	d := h.SessionStatuses()[0]
	if d.ReasonCode != "execution_session_lost" || len(d.ReasonCodes) != 3 {
		t.Fatalf("original cause hidden: %+v", d)
	}
	raw, _ := json.Marshal(d)
	if strings.Contains(string(raw), "private-data") {
		t.Fatal("diagnostic projection leaked raw reason")
	}
}

func TestBrokerGateCancellationRacesDoNotLeakOwnership(t *testing.T) {
	var gate contextMutex
	for range 20 {
		gate.Lock()
		ctx, cancel := context.WithCancel(t.Context())
		var wg sync.WaitGroup
		for range 32 {
			wg.Go(func() {
				if err := gate.LockContext(ctx); err == nil {
					gate.Unlock()
				} else if !errors.Is(err, context.Canceled) {
					t.Errorf("unexpected gate error: %v", err)
				}
			})
		}
		wg.Go(cancel)
		gate.Unlock()
		wg.Wait()
		check, stop := context.WithTimeout(t.Context(), time.Second)
		if err := gate.LockContext(check); err != nil {
			stop()
			t.Fatal("gate ownership leaked", err)
		}
		gate.Unlock()
		stop()
	}
}
