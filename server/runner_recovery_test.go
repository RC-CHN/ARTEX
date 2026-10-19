package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/RC-CHN/ARTEX/runner"
)

type recoveryTestStore struct {
	runner.MemoryStore
	mu      sync.Mutex
	failure error
	entered chan struct{}
	release chan struct{}
	loads   atomic.Int32
}

func (s *recoveryTestStore) fail(err error) { s.mu.Lock(); s.failure = err; s.mu.Unlock() }
func (s *recoveryTestStore) Save(ctx context.Context, rev uint64, data json.RawMessage) (uint64, error) {
	s.mu.Lock()
	err := s.failure
	s.mu.Unlock()
	if err != nil {
		return 0, err
	}
	return s.MemoryStore.Save(ctx, rev, data)
}
func (s *recoveryTestStore) Load(ctx context.Context) (runner.Snapshot, error) {
	s.loads.Add(1)
	s.mu.Lock()
	entered, release := s.entered, s.release
	s.mu.Unlock()
	if entered != nil {
		select {
		case entered <- struct{}{}:
		default:
		}
		select {
		case <-release:
		case <-ctx.Done():
			return runner.Snapshot{}, ctx.Err()
		}
	}
	return s.MemoryStore.Load(ctx)
}

func recoverableServer(t *testing.T) (*Server, *recoveryTestStore) {
	return recoverableServerWithJournal(t, nil)
}

func recoverableServerWithJournal(t *testing.T, journal *runnerJournal) (*Server, *recoveryTestStore) {
	t.Helper()
	store := &recoveryTestStore{}
	h, err := runner.New(runner.Options{Store: store, RecordEvent: journal.Record})
	if err != nil {
		t.Fatal(err)
	}
	token, _, err := h.CreateRegistrationToken(t.Context(), time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := json.Marshal(runner.RegisterRequest{Token: token, Name: "synthetic", BootID: "boot", Protocol: runner.ProtocolVersion, Capabilities: runner.Capabilities{Slots: 1, CPUMillis: 1000, MemoryMiB: 1024}})
	w := httptest.NewRecorder()
	h.Handler().ServeHTTP(w, httptest.NewRequest("POST", "/register", bytes.NewReader(body)))
	var node runner.RegisterResponse
	if w.Code != 201 || json.Unmarshal(w.Body.Bytes(), &node) != nil {
		t.Fatal("register fixture failed")
	}
	store.fail(errors.Join(context.DeadlineExceeded, errors.New("PRIVATE synthetic database error")))
	if err := h.SetNodeEnabled(node.NodeID, false); !errors.Is(err, runner.ErrStoreUnavailable) || !h.IsClosed() {
		t.Fatal("fixture did not close broker", err)
	}
	cfg := defaultRunnerConfig()
	cfg.Enabled = true
	env := cfg.Environments["default"]
	env.Image = "example.invalid/runtime@sha256:" + strings.Repeat("a", 64)
	cfg.Environments["default"] = env
	ctx, cancel := context.WithCancelCause(t.Context())
	s := &Server{ctx: ctx, cancel: cancel, runnerCfg: cfg, runners: h, runnerJournal: journal}
	t.Cleanup(s.Close)
	return s, store
}

func TestRunnerRecoveryBackoffAndConcurrentSuccess(t *testing.T) {
	s, store := recoverableServer(t)
	old := s.runners
	base := time.Now()
	s.recoverRunner(s.ctx, base)
	if s.recoverySnapshot().Attempts != 0 || s.runnerExecutionAvailable("worker") {
		t.Fatal("cooldown admitted work or attempted early recovery")
	}
	s.recoverRunner(s.ctx, base.Add(5*time.Second))
	status := s.recoverySnapshot()
	if status.Attempts != 1 || status.Failures != 1 || status.ManualRequired || status.LastFailureCode != "store_timeout" {
		t.Fatalf("unavailable store not backed off: %+v", status)
	}
	if delay := status.NextAttemptAt.Sub(status.LastAttemptAt); delay < 10*time.Second || delay > 11*time.Second {
		t.Fatal("wrong backoff")
	}
	s.recoverRunner(s.ctx, status.NextAttemptAt.Add(-time.Nanosecond))
	if s.recoverySnapshot().Attempts != 1 {
		t.Fatal("retry ignored cooldown")
	}
	store.fail(nil)
	var wg sync.WaitGroup
	for range 32 {
		wg.Go(func() { s.recoverRunner(s.ctx, status.NextAttemptAt) })
	}
	wg.Wait()
	_, next, err := s.runnerSnapshot()
	if err != nil || next == old || next.IsClosed() || !old.IsClosed() || !s.runnerExecutionAvailable("worker") {
		t.Fatal("replacement not published safely")
	}
	if got := s.recoverySnapshot(); got.Attempts != 2 || got.Failures != 0 || got.LastRecoveredAt.IsZero() || store.loads.Load() != 3 {
		t.Fatalf("concurrent recovery created extra successors: %+v loads=%d", got, store.loads.Load())
	}
}

func TestRunnerRecoveryStopsOnOwnershipConflict(t *testing.T) {
	s, store := recoverableServer(t)
	now := time.Now()
	s.recoverRunner(s.ctx, now)
	store.fail(runner.ErrStoreConflict)
	s.recoverRunner(s.ctx, now.Add(5*time.Second))
	status := s.recoverySnapshot()
	if !status.ManualRequired || status.LastFailureCode != "store_conflict" {
		t.Fatalf("conflicting owner not respected: %+v", status)
	}
	store.fail(nil)
	s.recoverRunner(s.ctx, now.Add(time.Hour))
	if s.recoverySnapshot().Attempts != 1 || !s.runners.IsClosed() {
		t.Fatal("automatic recovery fought another owner")
	}
}

func TestRecoveryKeepsHealthResponsiveAndShutdownPreventsPublication(t *testing.T) {
	s, store := recoverableServer(t)
	old := s.runners
	store.fail(nil)
	store.entered, store.release = make(chan struct{}, 1), make(chan struct{})
	now := time.Now()
	s.recoverRunner(s.ctx, now)
	done := make(chan struct{})
	go func() { defer close(done); s.recoverRunner(s.ctx, now.Add(5*time.Second)) }()
	select {
	case <-store.entered:
	case <-time.After(time.Second):
		t.Fatal("recovery did not reach load")
	}
	returned := make(chan struct{})
	go func() {
		defer close(returned)
		w := httptest.NewRecorder()
		s.health(w, httptest.NewRequest("GET", "/api/health", nil))
		if w.Code != 503 || !strings.Contains(w.Body.String(), `"recovering":true`) {
			t.Errorf("degraded health missing: %s", w.Body.String())
		}
		w = httptest.NewRecorder()
		s.runnerBrokerDiagnostics(w, httptest.NewRequest("GET", "/api/runners/diagnostics", nil))
		if w.Code != 200 || !strings.Contains(w.Body.String(), "fence_persistence_failed") {
			t.Error("closed cause unavailable during recovery")
		}
	}()
	select {
	case <-returned:
	case <-time.After(time.Second):
		t.Fatal("health blocked behind recovery")
	}
	closed := make(chan struct{})
	go func() { s.Close(); close(closed) }()
	select {
	case <-closed:
	case <-time.After(time.Second):
		t.Fatal("shutdown blocked on recovery load")
	}
	<-done
	_, hub, _ := s.runnerSnapshot()
	if hub != old || !hub.IsClosed() {
		t.Fatal("recovery published a new broker after cancellation")
	}
}

func TestRunnerConfigSaveSerializesWithRecovery(t *testing.T) {
	m := runnerTestManager(t)
	t.Cleanup(func() { _, _ = m.pg.Exec("DELETE FROM settings WHERE key=$1", runnerConfigKey) })
	s, store := recoverableServer(t)
	s.m = m
	store.fail(nil)
	store.entered, store.release = make(chan struct{}, 1), make(chan struct{})
	now := time.Now()
	s.recoverRunner(s.ctx, now)
	done := make(chan struct{})
	go func() { defer close(done); s.recoverRunner(s.ctx, now.Add(5*time.Second)) }()
	select {
	case <-store.entered:
	case <-time.After(time.Second):
		t.Fatal("recovery did not start")
	}
	cfg, old, _ := s.runnerSnapshot()
	cfg.AcquireTimeoutSeconds = 75
	body, _ := json.Marshal(cfg)
	saved := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		w := httptest.NewRecorder()
		s.saveRunnerConfig(w, httptest.NewRequest("PUT", "/api/runners/config", bytes.NewReader(body)))
		saved <- w
	}()
	select {
	case <-saved:
		t.Fatal("configuration raced past active replacement")
	case <-time.After(30 * time.Millisecond):
	}
	close(store.release)
	<-done
	select {
	case w := <-saved:
		if w.Code != 200 {
			t.Fatalf("save failed: %s", w.Body.String())
		}
	case <-time.After(3 * time.Second):
		t.Fatal("configuration did not resume")
	}
	got, next, _ := s.runnerSnapshot()
	if got.AcquireTimeoutSeconds != 75 || next == old || next.IsClosed() || store.loads.Load() != 2 {
		t.Fatal("configuration discarded recovered broker or used stale snapshot")
	}
}

func TestExecutionHealthAndProcessProbeAreSeparate(t *testing.T) {
	s, store := recoverableServer(t)
	for _, endpoint := range []struct {
		path string
		h    http.HandlerFunc
		want int
	}{{"/api/health", s.health, 503}, {"/api/livez", s.live, 200}} {
		w := httptest.NewRecorder()
		s.requireAuth(endpoint.h).ServeHTTP(w, httptest.NewRequest("GET", endpoint.path, nil))
		if w.Code != endpoint.want {
			t.Fatalf("public %s=%d", endpoint.path, w.Code)
		}
	}
	store.fail(nil)
	now := time.Now()
	s.recoverRunner(s.ctx, now)
	s.recoverRunner(s.ctx, now.Add(5*time.Second))
	w := httptest.NewRecorder()
	s.health(w, httptest.NewRequest("GET", "/api/health", nil))
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"code":"ready"`) {
		t.Fatalf("health not recovered: %s", w.Body.String())
	}
}
