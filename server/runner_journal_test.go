package server

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/RC-CHN/ARTEX/runner"
)

type journalPanicStore struct {
	runner.MemoryStore
	panicNext atomic.Bool
}

func (s *journalPanicStore) Guard(ctx context.Context, revision uint64, commit func() error) error {
	if s.panicNext.Swap(false) {
		panic("PRIVATE synthetic runtime panic")
	}
	return s.MemoryStore.Guard(ctx, revision, commit)
}

func TestRunnerJournalPersistsSanitizedPollPanic(t *testing.T) {
	dir := t.TempDir()
	j := newRunnerJournal(dir)
	defer j.Close()
	store := &journalPanicStore{}
	h, err := runner.New(runner.Options{Store: store, RecordEvent: j.Record})
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	token, _, err := h.CreateRegistrationToken(t.Context(), time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := json.Marshal(runner.RegisterRequest{Token: token, Name: "synthetic", BootID: "boot", Protocol: runner.ProtocolVersion, Capabilities: runner.Capabilities{Slots: 1}})
	w := httptest.NewRecorder()
	h.Handler().ServeHTTP(w, httptest.NewRequest("POST", "/register", bytes.NewReader(body)))
	var node runner.RegisterResponse
	if w.Code != 201 || json.Unmarshal(w.Body.Bytes(), &node) != nil {
		t.Fatal("registration failed")
	}
	store.panicNext.Store(true)
	body, _ = json.Marshal(runner.PollRequest{NodeID: node.NodeID, BootID: "boot", Protocol: runner.ProtocolVersion})
	req := httptest.NewRequest("POST", "/poll", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+node.Credential)
	w = httptest.NewRecorder()
	h.Handler().ServeHTTP(w, req)
	if w.Code != 500 || !strings.Contains(w.Body.String(), `"code":"internal_panic"`) || strings.Contains(w.Body.String(), "PRIVATE") {
		t.Fatal("panic did not produce a safe structured response")
	}
	waitJournal(t, j, func(s runnerJournalStatus) bool { return s.Pending == 0 })
	_, events := j.snapshot(500, "")
	incident := ""
	for _, event := range events {
		if event.Kind == "operation_panic" && event.Operation == "poll" && event.Recovered {
			incident = event.IncidentID
		}
	}
	data, err := os.ReadFile(filepath.Join(dir, "diagnostics", "runner-incidents.jsonl"))
	if err != nil || incident == "" || !strings.Contains(string(data), incident) || strings.Contains(string(data), "PRIVATE") {
		t.Fatal("panic was not safely persisted", err)
	}
}

func waitJournal(t *testing.T, j *runnerJournal, ready func(runnerJournalStatus) bool) runnerJournalStatus {
	t.Helper()
	deadline := time.Now().Add(4 * time.Second)
	for time.Now().Before(deadline) {
		state, _ := j.snapshot(0, "")
		if ready(state) {
			return state
		}
		time.Sleep(time.Millisecond)
	}
	state, _ := j.snapshot(0, "")
	t.Fatalf("journal did not settle: %+v", state)
	return state
}

func TestRunnerJournalRotationRestartAndPrivatePermissions(t *testing.T) {
	path := filepath.Join(t.TempDir(), "diagnostics", "runner-incidents.jsonl")
	j := startRunnerJournal(path, 1024, 2, 64)
	for range 24 {
		j.Record(runner.Event{Kind: "operation", Code: "store_timeout"})
	}
	j.Close()
	state, before := j.snapshot(1, "")
	if state.Pending != 0 || state.Dropped != 0 || state.LastPersisted != before[0].ID {
		t.Fatalf("journal not flushed: %+v", state)
	}
	files, err := filepath.Glob(path + "*")
	if err != nil || len(files) != 3 {
		t.Fatalf("rotation count: %v %d", err, len(files))
	}
	for _, name := range files {
		info, err := os.Stat(name)
		if err != nil || info.Size() > 1024 || info.Mode().Perm() != 0600 {
			t.Fatalf("unbounded or unprotected file: %v", err)
		}
	}
	reopened := startRunnerJournal(path, 1024, 2, 64)
	defer reopened.Close()
	waitJournal(t, reopened, func(s runnerJournalStatus) bool { return s.HistoryLoaded })
	_, records := reopened.snapshot(500, "")
	if len(records) == 0 || records[0].ID != before[0].ID {
		t.Fatal("restart lost retained records")
	}
	// Diagnostic history remains accessible even if no broker can initialize.
	s := &Server{runnerJournal: reopened}
	w := httptest.NewRecorder()
	s.runnerIncidents(w, httptest.NewRequest("GET", "/api/runners/incidents?limit=1", nil))
	if w.Code != 200 || !strings.Contains(w.Body.String(), before[0].ID) {
		t.Fatal("incident API depended on a healthy broker")
	}
	w = httptest.NewRecorder()
	s.requireAuth(http.HandlerFunc(s.runnerIncidents)).ServeHTTP(w, httptest.NewRequest("GET", "/api/runners/incidents", nil))
	if w.Code != 401 {
		t.Fatal("incident history is not authenticated")
	}
}

func TestRunnerJournalRecoversFromStorageFailureAndReportsOverflow(t *testing.T) {
	parent := filepath.Join(t.TempDir(), "blocked")
	if err := os.WriteFile(parent, []byte("synthetic blocker"), 0600); err != nil {
		t.Fatal(err)
	}
	j := startRunnerJournal(filepath.Join(parent, "events.jsonl"), 8<<20, 2, 4)
	defer j.Close()
	for range 50 {
		j.Record(runner.Event{Kind: "operation", Code: "store_timeout"})
	}
	state := waitJournal(t, j, func(s runnerJournalStatus) bool { return s.WriteFailures > 0 })
	if state.Dropped == 0 || state.Pending > 5 || state.LastErrorCode == "" {
		t.Fatalf("loss was hidden: %+v", state)
	}
	if err := os.Remove(parent); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(parent, 0700); err != nil {
		t.Fatal(err)
	}
	waitJournal(t, j, func(s runnerJournalStatus) bool {
		return s.Pending == 0 && !s.LastWriteAt.IsZero() && s.LastErrorCode == ""
	})
	j.Record(runner.Event{Kind: "recovery_succeeded", Code: "ok"})
	j.Close()
	data, err := os.ReadFile(filepath.Join(parent, "events.jsonl"))
	if err != nil || !strings.Contains(string(data), "recovery_succeeded") {
		t.Fatal("disk recovery did not resume writes", err)
	}
}

func TestRunnerJournalPartialTailDoesNotSwallowNextRecord(t *testing.T) {
	dir := t.TempDir()
	j := newRunnerJournal(dir)
	j.Record(runner.Event{Kind: "before_crash", Code: "ok"})
	j.Close()
	path := filepath.Join(dir, "diagnostics", "runner-incidents.jsonl")
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = f.WriteString(`{"incomplete":`)
	_ = f.Close()
	reopened := newRunnerJournal(dir)
	reopened.Record(runner.Event{Kind: "after_crash", Code: "ok"})
	reopened.Close()
	state, records := reopened.snapshot(500, "")
	if state.InvalidLines != 1 || len(records) != 2 {
		t.Fatalf("partial tail handled incorrectly: %+v count=%d", state, len(records))
	}
	f, err = os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	scan := bufio.NewScanner(f)
	valid := 0
	for scan.Scan() {
		if json.Valid(scan.Bytes()) {
			valid++
		}
	}
	if valid != 2 {
		t.Fatal("partial tail swallowed a new event")
	}
}

func TestRunnerJournalConcurrentCloseAndRecord(t *testing.T) {
	j := newRunnerJournal(t.TempDir())
	var wg sync.WaitGroup
	for range 16 {
		wg.Go(func() {
			for range 20 {
				j.Record(runner.Event{Kind: "operation", Code: "ok"})
			}
		})
	}
	wg.Go(j.Close)
	wg.Wait()
	j.Close()
	state, records := j.snapshot(500, "")
	if len(records) > runnerJournalRecent || state.Pending != 0 {
		t.Fatal("close raced with queue ownership")
	}
}

func TestRunnerJournalCorrelatesRecoveryWithoutPrivateErrorText(t *testing.T) {
	dir := t.TempDir()
	j := newRunnerJournal(dir)
	s, store := recoverableServerWithJournal(t, j)
	incident := s.runners.BrokerState().IncidentID
	now := time.Now()
	s.recoverRunner(s.ctx, now)
	s.recoverRunner(s.ctx, now.Add(5*time.Second)) // failed persistence retry
	store.fail(nil)
	s.recoverRunner(s.ctx, s.recoverySnapshot().NextAttemptAt)
	s.Close()
	_, events := j.snapshot(500, incident)
	kinds := map[string]bool{}
	for _, event := range events {
		kinds[event.Kind] = true
	}
	for _, kind := range []string{"broker_closed", "recovery_started", "recovery_failed", "recovery_succeeded"} {
		if !kinds[kind] {
			t.Fatalf("incident missing %s", kind)
		}
	}
	data, err := os.ReadFile(filepath.Join(dir, "diagnostics", "runner-incidents.jsonl"))
	if err != nil || strings.Contains(string(data), "PRIVATE") || !strings.Contains(string(data), "store_timeout") {
		t.Fatal("journal leaked error text or lost error classification", err)
	}
}
