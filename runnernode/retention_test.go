package runnernode

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/RC-CHN/ARTEX/runner"
)

func TestRetiredLedgerSustainedConcurrentCollection(t *testing.T) {
	n, _ := testNode(t)
	ready := make(chan struct{})
	close(ready)
	// 100,000 receipts across 10,000 sessions, with parallel collectors. The
	// retained state must depend on live/unacknowledged work, not process age.
	for batch := range 100 {
		n.mu.Lock()
		for i := range 100 {
			id := fmt.Sprintf("%d-%d", batch, i)
			n.sessions[id] = &nodeSession{id: id, attempt: "attempt", epoch: "epoch", ready: ready, stopped: true, opened: true}
			for c := range 10 {
				command := fmt.Sprintf("%s-%d", id, c)
				n.commands[command] = &commandState{result: &runner.ResultRequest{SessionID: id, CommandID: command, AttemptID: "attempt", Epoch: "epoch"}}
			}
		}
		n.mu.Unlock()
		var wg sync.WaitGroup
		for range 8 {
			wg.Go(func() { n.collectRetired(time.Now()) })
		}
		wg.Wait()
		n.mu.Lock()
		if len(n.sessions) != 0 || len(n.commands) != 0 {
			t.Errorf("batch %d leaked sessions=%d receipts=%d", batch, len(n.sessions), len(n.commands))
		}
		n.mu.Unlock()
	}
}

func TestRetentionKeepsLiveUnconfirmedAndUnacknowledgedWork(t *testing.T) {
	n, _ := testNode(t)
	ready := make(chan struct{})
	close(ready)
	for _, id := range []string{"live", "unconfirmed", "preparing", "unacknowledged", "executed", "input-only"} {
		dir := filepath.Join(t.TempDir(), id)
		if err := os.MkdirAll(dir, 0700); err != nil {
			t.Fatal(err)
		}
		n.sessions[id] = &nodeSession{id: id, attempt: "a", epoch: "e", ready: ready, stopped: true, dir: dir}
		n.commands[id] = &commandState{result: &runner.ResultRequest{SessionID: id, AttemptID: "a", Epoch: "e"}}
	}
	n.leases["live"] = runner.Lease{SessionID: "live", AttemptID: "a", Epoch: "e", Until: time.Now().Add(time.Minute)}
	n.sessions["unconfirmed"].stopped = false
	n.sessions["preparing"].ready = make(chan struct{})
	n.commands["unacknowledged"].queued = true
	n.sessions["executed"].opened = true
	executed, inputs := n.sessions["executed"].dir, n.sessions["input-only"].dir
	n.collectRetired(time.Now())
	for _, id := range []string{"live", "unconfirmed", "preparing", "unacknowledged"} {
		if n.commands[id] == nil {
			t.Errorf("discarded protected receipt %s", id)
		}
	}
	for _, id := range []string{"live", "unconfirmed", "preparing"} {
		if n.sessions[id] == nil {
			t.Errorf("released protected reservation %s", id)
		}
	}
	if _, err := os.Stat(executed); err != nil {
		t.Fatal("discarded possible outputs")
	}
	if _, err := os.Stat(inputs); !os.IsNotExist(err) {
		t.Fatal("aborted input-only cache retained")
	}
	if n.commands["executed"] != nil || n.commands["input-only"] != nil {
		t.Fatal("completed receipts retained")
	}
}

func TestLateCommandAfterCollectionCannotExecute(t *testing.T) {
	n, _ := testNode(t)
	openNode(t, n)
	s := n.sessions["s"]
	if err := n.abort(s); err != nil {
		t.Fatal(err)
	}
	delete(n.leases, "s")
	n.collectRetired(time.Now())
	if n.sessions["s"] != nil {
		t.Fatal("stopped session retained")
	}
	spec := baseSpec(t.TempDir())
	r := n.executeCommand(t.Context(), runner.Command{ID: "late-open", Kind: runner.CommandOpen, SessionID: "s", AttemptID: s.attempt, Epoch: s.epoch, Spec: &spec})
	if r.Error != "session lease expired or fenced" {
		t.Fatalf("late command was not fenced: %+v", r)
	}
}
