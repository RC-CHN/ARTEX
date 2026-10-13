package runnernode

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/RC-CHN/ARTEX/runner"
)

type countingSandbox struct {
	Sandbox
	stops atomic.Int32
	gate  <-chan struct{}
	fail  atomic.Bool
}

func (s *countingSandbox) Stop() error {
	s.stops.Add(1)
	if s.gate != nil {
		<-s.gate
	}
	if s.fail.Load() {
		return errors.New("runtime temporarily unavailable")
	}
	return nil
}

func TestConcurrentWatchdogAndCloseCleanUpOnlyOnce(t *testing.T) {
	n, _ := testNode(t)
	openNode(t, n)
	s := n.sessions["s"]
	gate := make(chan struct{})
	box := &countingSandbox{Sandbox: s.sandbox, gate: gate}
	s.sandbox = box
	for range 100 {
		n.scheduleAbort(s)
	}
	var wg sync.WaitGroup
	for range 20 {
		wg.Go(func() {
			if err := n.abort(s); err != nil {
				t.Error(err)
			}
		})
	}
	time.Sleep(30 * time.Millisecond)
	if box.stops.Load() != 1 {
		t.Errorf("parallel cleanup requests=%d", box.stops.Load())
	}
	close(gate)
	wg.Wait()
	for range 100 {
		n.scheduleAbort(s)
	}
	if box.stops.Load() != 1 {
		t.Fatalf("confirmed cleanup repeated %d times", box.stops.Load())
	}
}

func TestFailedCleanupBacksOffAndCanRecover(t *testing.T) {
	n, _ := testNode(t)
	openNode(t, n)
	s := n.sessions["s"]
	box := &countingSandbox{Sandbox: s.sandbox}
	box.fail.Store(true)
	s.sandbox = box
	n.scheduleAbort(s)
	deadline := time.Now().Add(time.Second)
	for {
		s.mu.Lock()
		done := !s.cleanupRunning && !s.cleanupAfter.IsZero()
		s.mu.Unlock()
		if done {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("cleanup never completed")
		}
		time.Sleep(time.Millisecond)
	}
	for range 100 {
		n.scheduleAbort(s)
	}
	if box.stops.Load() != 1 {
		t.Fatal("failed cleanup retried without backoff")
	}
	s.mu.Lock()
	stopped := s.stopped
	s.mu.Unlock()
	if stopped {
		t.Fatal("unconfirmed cleanup released reservation")
	}
	box.fail.Store(false)
	r := n.executeCommand(t.Context(), runner.Command{ID: "close", Kind: runner.CommandClose, SessionID: "s", AttemptID: "a", Epoch: "epoch", Abort: true})
	if r.UnknownOutcome || r.Error != "" || box.stops.Load() != 2 {
		t.Fatalf("cleanup did not recover: %+v, attempts=%d", r, box.stops.Load())
	}
	if n.sessions["s"] != nil {
		t.Fatal("confirmed cleanup kept reservation")
	}
}

func TestShutdownRetainsPreparingSessionUntilFinalCleanup(t *testing.T) {
	n, _ := testNode(t)
	ctx, cancel := context.WithCancel(t.Context())
	s := &nodeSession{id: "preparing", ctx: ctx, cancel: cancel, ready: make(chan struct{})}
	n.sessions[s.id] = s
	n.stopAll()
	if n.sessions[s.id] != s {
		t.Fatal("shutdown lost a still-preparing session")
	}
	box := &countingSandbox{}
	s.mu.Lock()
	s.sandbox = box // runtime create returned late, after the first cleanup pass
	s.mu.Unlock()
	close(s.ready)
	n.stopAll()
	if box.stops.Load() != 1 || len(n.sessions) != 0 {
		t.Fatal("late-created sandbox survived shutdown")
	}
}

func TestRPCStopRetriesRuntimeFailure(t *testing.T) {
	read, write := io.Pipe()
	defer read.Close()
	calls := 0
	s := &rpcSandbox{stdin: write, cmd: exec.Command("unused"), stop: func() error {
		calls++
		if calls == 1 {
			return errors.New("daemon unavailable")
		}
		return nil
	}}
	if err := s.Stop(); err == nil {
		t.Fatal("failed cleanup reported success")
	}
	if err := s.Stop(); err != nil {
		t.Fatalf("runtime recovery was never retried: %v", err)
	}
	if err := s.Stop(); err != nil || calls != 2 {
		t.Fatalf("confirmed cleanup repeated: calls=%d, err=%v", calls, err)
	}
}

func TestRejectedRunnerIdentityStopsInsteadOfPollingForever(t *testing.T) {
	for _, status := range []int{401, 403, 410} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			var calls atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { calls.Add(1); w.WriteHeader(status) }))
			defer srv.Close()
			n, e := testNode(t)
			n.Config.ServerURL = srv.URL
			n.identity = identity{NodeID: "fixture", Credential: "fixture-secret", ServerURL: srv.URL}
			if err := saveIdentity(filepath.Join(n.Config.DataDir, "credentials.json"), n.identity); err != nil {
				t.Fatal(err)
			}
			openNode(t, n)
			ctx, cancel := context.WithTimeout(t.Context(), time.Second)
			defer cancel()
			err := n.Run(ctx, "")
			if err == nil || !strings.Contains(err.Error(), "identity rejected") || calls.Load() != 1 || e.box.stops.Load() != 1 {
				t.Fatalf("identity rejection not terminal: err=%v requests=%d stops=%d", err, calls.Load(), e.box.stops.Load())
			}
		})
	}
}

func TestResultRequiresExplicitAcknowledgment(t *testing.T) {
	for _, unavailable := range []bool{false, true} {
		t.Run(fmt.Sprint(unavailable), func(t *testing.T) {
			var calls atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				calls.Add(1)
				if unavailable {
					w.WriteHeader(503)
					return
				}
				_, _ = io.WriteString(w, `{"accepted":false}`)
			}))
			defer srv.Close()
			n, _ := testNode(t)
			n.Config.ServerURL = srv.URL
			n.commands["pending"] = &commandState{queued: true, result: &runner.ResultRequest{CommandID: "pending"}}
			ctx, cancel := context.WithTimeout(t.Context(), 700*time.Millisecond)
			defer cancel()
			n.sendResults(ctx)
			if !n.commands["pending"].queued {
				t.Fatal("unacknowledged result discarded")
			}
			if count := calls.Load(); count < 2 || count > 3 {
				t.Fatalf("retry backoff requests=%d", count)
			}
		})
	}
}
