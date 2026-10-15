package db

import (
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
)

func TestCancelPendingIntentPreservesHistoryAndScope(t *testing.T) {
	d, err := Open(testDSN(t))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	exp, err := d.CreateExploration("pending cancellation", "synthetic fixture")
	if err != nil {
		t.Fatal(err)
	}
	defer d.Exec(`DELETE FROM explorations WHERE id=$1`, exp)
	s := d.Exploration(exp)
	add := func() int64 {
		t.Helper()
		id, err := s.AddIntent(map[string]any{"summary": "obsolete direction"}, 1, nil, "planner")
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	id := add()
	fact, err := s.AddNode(KindFact, map[string]any{"text": "existing evidence"}, 0, "confirmed", "worker", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Link(id, RelYields, fact); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AppendActivity(Activity{NodeID: &id, Worker: "worker", Kind: "result", Summary: "earlier progress"}); err != nil {
		t.Fatal(err)
	}
	for _, reason := range []string{"", " \n ", strings.Repeat("x", 4097)} {
		if err := s.CancelPendingIntent(id, reason, "planner"); err == nil {
			t.Fatal("invalid reason accepted")
		}
	}
	if err := s.CancelPendingIntent(id, "  another intent answered this question  ", "planner"); err != nil {
		t.Fatal(err)
	}
	node, err := s.GetNode(id)
	if err != nil || node == nil || node.State != "stopped" {
		t.Fatalf("cancelled node: %+v, %v", node, err)
	}
	var payload map[string]any
	if err := json.Unmarshal(node.Payload, &payload); err != nil {
		t.Fatal(err)
	}
	cancellation := payload["cancellation"].(map[string]any)
	if payload["summary"] != "obsolete direction" || cancellation["reason"] != "another intent answered this question" || cancellation["actor"] != "planner" || cancellation["at"] == nil {
		t.Fatalf("payload lost context: %s", node.Payload)
	}
	var completed bool
	var version, edges, decisions, prior int
	if err := d.QueryRow(`SELECT completed_at IS NOT NULL, content_version FROM exploration_nodes WHERE id=$1`, id).Scan(&completed, &version); err != nil || !completed || version != 1 {
		t.Fatalf("completion/version: %v/%d, %v", completed, version, err)
	}
	if err := d.QueryRow(`SELECT count(*) FROM exploration_edges WHERE src_id=$1 AND dst_id=$2`, id, fact).Scan(&edges); err != nil || edges != 1 {
		t.Fatalf("lineage lost: %d, %v", edges, err)
	}
	if err := s.CancelPendingIntent(id, "overwrite", "planner"); !errors.Is(err, ErrIntentStateConflict) {
		t.Fatalf("repeat must conflict: %v", err)
	}
	if err := d.QueryRow(`SELECT count(*) FILTER (WHERE tool='cancel_pending_intent'), count(*) FILTER (WHERE kind='result') FROM activity WHERE node_id=$1`, id).Scan(&decisions, &prior); err != nil || decisions != 1 || prior != 1 {
		t.Fatalf("audit/history: %d/%d, %v", decisions, prior, err)
	}
	if claimed, err := s.ClaimIntent(id, "worker"); err != nil || claimed {
		t.Fatalf("cancelled intent claimed: %v, %v", claimed, err)
	}
	if frontier, err := s.Frontier(10); err != nil || len(frontier) != 0 {
		t.Fatalf("cancelled intent still queued: %+v, %v", frontier, err)
	}
	// A user can explicitly requeue it. Cancellation remains in its audit, and a
	// newly obsolete queued retry can be closed without deleting earlier work.
	if reopened, err := s.ReopenIntent(id); err != nil || !reopened {
		t.Fatalf("explicit requeue: %v, %v", reopened, err)
	}
	node, err = s.GetNode(id)
	if err != nil || strings.Contains(string(node.Payload), `"cancellation"`) {
		t.Fatalf("requeued intent retained a stale cancellation reason: %+v, %v", node, err)
	}
	if err := d.QueryRow(`SELECT content_version FROM exploration_nodes WHERE id=$1`, id).Scan(&version); err != nil || version != 2 {
		t.Fatalf("requeue did not invalidate cached summaries: version=%d, %v", version, err)
	}
	if err := s.CancelPendingIntent(id, "retry no longer needed", "planner"); err != nil {
		t.Fatal(err)
	}
	for _, state := range []string{"running", "done", "blocked", "exhausted", "paused", "stopped", "deleted"} {
		t.Run(state, func(t *testing.T) {
			id := add()
			if err := s.SetIntentState(id, state); err != nil {
				t.Fatal(err)
			}
			if err := s.CancelPendingIntent(id, "obsolete", "planner"); !errors.Is(err, ErrIntentStateConflict) {
				t.Fatalf("non-pending state accepted: %v", err)
			}
			node, err := s.GetNode(id)
			if err != nil || node.State != state {
				t.Fatalf("state changed: %+v, %v", node, err)
			}
		})
	}
	goal, err := s.AddGoal(map[string]any{"text": "still open"}, "human")
	if err != nil {
		t.Fatal(err)
	}
	for _, wrongID := range []int64{goal, fact, 9223372036854775807} {
		if err := s.CancelPendingIntent(wrongID, "obsolete", "planner"); !errors.Is(err, ErrIntentStateConflict) {
			t.Fatalf("wrong kind/missing node accepted: %v", err)
		}
	}
	foreign := d.Exploration(exp + 1000000000)
	queued := add()
	if err := foreign.CancelPendingIntent(queued, "foreign", "planner"); !errors.Is(err, ErrIntentStateConflict) {
		t.Fatalf("cross-exploration mutation: %v", err)
	}
}

func TestCancelPendingIntentRacesClaimWithSingleWinner(t *testing.T) {
	d, err := Open(testDSN(t))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	exp, err := d.CreateExploration("claim/cancel race", "synthetic fixture")
	if err != nil {
		t.Fatal(err)
	}
	defer d.Exec(`DELETE FROM explorations WHERE id=$1`, exp)
	s := d.Exploration(exp)
	for range 32 {
		id, err := s.AddIntent(map[string]any{"summary": "racing"}, 1, nil, "planner")
		if err != nil {
			t.Fatal(err)
		}
		start := make(chan struct{})
		var wg sync.WaitGroup
		var claimed bool
		var claimErr, cancelErr error
		wg.Add(2)
		go func() { defer wg.Done(); <-start; claimed, claimErr = s.ClaimIntent(id, "worker") }()
		go func() { defer wg.Done(); <-start; cancelErr = s.CancelPendingIntent(id, "obsolete", "planner") }()
		close(start)
		wg.Wait()
		if claimErr != nil || (cancelErr != nil && !errors.Is(cancelErr, ErrIntentStateConflict)) || claimed == (cancelErr == nil) {
			t.Fatalf("expected one winner: claim=%v/%v, cancel=%v", claimed, claimErr, cancelErr)
		}
		node, err := s.GetNode(id)
		if err != nil || (claimed && node.State != "running") || (!claimed && node.State != "stopped") {
			t.Fatalf("wrong final state: %+v, %v", node, err)
		}
		var audits int
		if err := d.QueryRow(`SELECT count(*) FROM activity WHERE node_id=$1 AND tool='cancel_pending_intent'`, id).Scan(&audits); err != nil || (claimed && audits != 0) || (!claimed && audits != 1) {
			t.Fatalf("decision not atomic: %d, %v", audits, err)
		}
	}
}
