package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/RC-CHN/ARTEX/runner"
)

type blockedDiagnosticStore struct {
	runner.MemoryStore
	block   atomic.Bool
	entered chan struct{}
	release chan struct{}
}

func (s *blockedDiagnosticStore) Save(ctx context.Context, rev uint64, data json.RawMessage) (uint64, error) {
	if s.block.Load() {
		close(s.entered)
		select {
		case <-s.release:
		case <-ctx.Done():
			return 0, ctx.Err()
		}
	}
	return s.MemoryStore.Save(ctx, rev, data)
}

func TestRunnerDiagnosticsRespondDuringBlockedCheckpoint(t *testing.T) {
	store := &blockedDiagnosticStore{entered: make(chan struct{}), release: make(chan struct{})}
	h, err := runner.New(runner.Options{Store: store})
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	s := &Server{runners: h}
	store.block.Store(true)
	done := make(chan error, 1)
	go func() { _, _, e := h.CreateRegistrationToken(t.Context(), time.Minute); done <- e }()
	defer func() { store.block.Store(false); close(store.release); <-done }()
	select {
	case <-store.entered:
	case <-time.After(time.Second):
		t.Fatal("checkpoint did not block")
	}
	w := httptest.NewRecorder()
	returned := make(chan struct{})
	go func() {
		s.runnerBrokerDiagnostics(w, httptest.NewRequest("GET", "/api/runners/diagnostics", nil))
		close(returned)
	}()
	select {
	case <-returned:
	case <-time.After(time.Second):
		t.Fatal("diagnostics waited for broker execution lock")
	}
	var d runner.BrokerDiagnostics
	if err := json.Unmarshal(w.Body.Bytes(), &d); err != nil || w.Code != 200 || d.Operations["checkpoint_store"].InFlight != 1 {
		t.Fatalf("missing in-flight checkpoint: %d %s", w.Code, w.Body.String())
	}
}

func TestRunnerPGCheckpointTimeoutDoesNotPartiallyCommit(t *testing.T) {
	m := runnerTestManager(t)
	if _, err := m.pg.Exec("DELETE FROM runner_state"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = m.pg.Exec("DELETE FROM runner_state")
		_, _ = m.pg.Exec("DELETE FROM runner_diagnostics")
	})
	store := runnerPGStore{m.pg.DB}
	original := json.RawMessage(`{"synthetic":"original"}`)
	if _, err := store.SaveDiagnostic(t.Context(), 0, original, []runner.SessionStatus{{ID: "original"}}); err != nil {
		t.Fatal(err)
	}
	tx, err := m.pg.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if _, err = tx.Exec("SELECT revision FROM runner_state WHERE id=1 FOR UPDATE"); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()
	_, err = store.SaveDiagnostic(ctx, 1, json.RawMessage(`{"synthetic":"must-not-commit"}`), []runner.SessionStatus{{ID: "must-not-commit"}})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected bounded lock timeout, got %v", err)
	}
	if err = tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	snap, err := store.Load(t.Context())
	if err != nil || snap.Revision != 1 || string(snap.Data) != string(original) {
		t.Fatal("timeout partially changed checkpoint")
	}
	var revision uint64
	var statuses []byte
	if err = m.pg.QueryRow("SELECT revision,sessions FROM runner_diagnostics WHERE id=1").Scan(&revision, &statuses); err != nil {
		t.Fatal(err)
	}
	var projected []runner.SessionStatus
	if err = json.Unmarshal(statuses, &projected); err != nil || revision != 1 || len(projected) != 1 || projected[0].ID != "original" {
		t.Fatal("timeout partially changed diagnostic projection")
	}
	if _, err = store.SaveDiagnostic(t.Context(), 1, original, []runner.SessionStatus{{ID: "recovered"}}); err != nil {
		t.Fatal("checkpoint failed to recover", err)
	}
}
