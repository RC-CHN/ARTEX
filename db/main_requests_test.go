package db

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

func mainRequestFixture(t *testing.T) (*DB, *ExplorationStore, int64) {
	t.Helper()
	d, err := Open(testDSN(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	exp, err := d.CreateExploration("consultation synthetic fixture", "unit test")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = d.Exec(`DELETE FROM explorations WHERE id=$1`, exp) })
	s := d.Exploration(exp)
	id, err := s.AddIntent(map[string]any{"summary": "independent work"}, 1, nil, "planner")
	if err != nil {
		t.Fatal(err)
	}
	if ok, err := s.ClaimIntent(id, "worker"); err != nil || !ok {
		t.Fatalf("claim: %v", err)
	}
	return d, s, id
}

func TestMainRequestAsyncIdempotencyAndScope(t *testing.T) {
	_, s, id := mainRequestFixture(t)
	ctx := context.Background()
	r, err := s.CreateMainRequest(ctx, id, "call-1", "which evidence?", "fact #1")
	if err != nil {
		t.Fatal(err)
	}
	n, _ := s.GetNode(id)
	if n.State != "running" {
		t.Fatalf("request blocked worker: %s", n.State)
	}
	again, err := s.CreateMainRequest(ctx, id, "call-1", "which evidence?", "fact #1")
	if err != nil || again.ID != r.ID {
		t.Fatalf("duplicate: %+v %v", again, err)
	}
	if _, err = s.CreateMainRequest(ctx, id, "call-2", "duplicate outstanding question", ""); err == nil {
		t.Fatal("accepted second pending request")
	}
	claimed, err := s.ClaimMainRequest(t.Context())
	if err != nil || claimed.ID != r.ID {
		t.Fatalf("claim: %+v %v", claimed, err)
	}
	if another, err := s.ClaimMainRequest(t.Context()); err != nil || another != nil {
		t.Fatalf("double claim: %+v %v", another, err)
	}
	if err = s.AnswerMainRequest(ctx, r.ID, "use confirmed fact", "mainagent"); err != nil {
		t.Fatal(err)
	}
	if err = s.AnswerMainRequest(ctx, r.ID, "use confirmed fact", "mainagent"); err != nil {
		t.Fatal("reply not idempotent", err)
	}
	if err = s.AnswerMainRequest(ctx, r.ID, "overwrite", "human"); !errors.Is(err, ErrIntentStateConflict) {
		t.Fatalf("reply overwritten: %v", err)
	}
	n, _ = s.GetNode(id)
	if n.State != "running" {
		t.Fatal("reply changed worker state")
	}
	if err = s.AckMainReply(r.ID); err != nil {
		t.Fatal(err)
	}
	got, _ := s.MainRequest(r.ID)
	if got.State != "delivered" {
		t.Fatalf("receipt: %+v", got)
	}
	rows, err := s.MainRequestActivities(r.ID)
	if err != nil || len(rows) != 2 {
		t.Fatalf("transactional audit: %d %v", len(rows), err)
	}
	for _, a := range rows {
		if a.Kind == "user" || a.NodeID != nil || a.Worker != "mainagent" || !strings.Contains(string(a.Metadata), "request_id") {
			t.Fatalf("wrong audit origin: %+v", a)
		}
	}
	otherID, err := s.db.CreateExploration("other", "fixture")
	if err != nil {
		t.Fatal(err)
	}
	defer s.db.Exec(`DELETE FROM explorations WHERE id=$1`, otherID)
	if err = s.db.Exploration(otherID).AnswerMainRequest(ctx, r.ID, "cross task", "mainagent"); err == nil {
		t.Fatal("cross task reply accepted")
	}
}

func TestMainRequestCancellationRaceAndRetry(t *testing.T) {
	for _, state := range []string{"done", "blocked", "stopped", "deleted", "exhausted"} {
		t.Run(state, func(t *testing.T) {
			_, s, id := mainRequestFixture(t)
			r, err := s.CreateMainRequest(t.Context(), id, "call", "question", "")
			if err != nil {
				t.Fatal(err)
			}
			start := make(chan struct{})
			var wg sync.WaitGroup
			wg.Add(2)
			var answerErr, stateErr error
			go func() {
				defer wg.Done()
				<-start
				answerErr = s.AnswerMainRequest(context.Background(), r.ID, "answer", "mainagent")
			}()
			go func() { defer wg.Done(); <-start; stateErr = s.SetIntentState(id, state) }()
			close(start)
			wg.Wait()
			if stateErr != nil {
				t.Fatal(stateErr)
			}
			if answerErr != nil && !errors.Is(answerErr, ErrIntentStateConflict) {
				t.Fatal(answerErr)
			}
			got, _ := s.MainRequest(r.ID)
			n, _ := s.GetNode(id)
			if got.State != "cancelled" || n.State != state {
				t.Fatalf("late reply revived intent: %+v %+v", got, n)
			}
			if state == "blocked" {
				if ok, err := s.ReopenIntent(id); !ok || err != nil {
					t.Fatal(err)
				}
				if err := s.AnswerMainRequest(t.Context(), r.ID, "stale", "mainagent"); !errors.Is(err, ErrIntentStateConflict) {
					t.Fatalf("old reply after rerun: %v", err)
				}
			}
		})
	}
}

func TestMainRequestRecoveryPauseAndLimits(t *testing.T) {
	d, s, id := mainRequestFixture(t)
	for _, q := range []string{"", strings.Repeat("a", 8001)} {
		if _, err := s.CreateMainRequest(t.Context(), id, "bad", q, ""); err == nil {
			t.Fatal("invalid input accepted")
		}
	}
	r, err := s.CreateMainRequest(t.Context(), id, "call", "question", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.ClaimMainRequest(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err = d.RecoverMainRequests(); err != nil {
		t.Fatal(err)
	}
	r, err = s.ClaimMainRequest(t.Context())
	if err != nil || r.Attempts != 2 {
		t.Fatalf("recovery: %+v %v", r, err)
	}
	if err = s.SetIntentState(id, "paused"); err != nil {
		t.Fatal(err)
	}
	if err = s.AnswerMainRequest(t.Context(), r.ID, "stored while paused", "mainagent"); err != nil {
		t.Fatal(err)
	}
	n, _ := s.GetNode(id)
	if n.State != "paused" {
		t.Fatal("reply unpaused worker")
	}
	// 32 outstanding requests bound per-task queue growth, including undelivered replies.
	for i := 1; i < 32; i++ {
		iid, err := s.AddIntent(map[string]any{"summary": "queue"}, 1, nil, "planner")
		if err != nil {
			t.Fatal(err)
		}
		_, _ = s.ClaimIntent(iid, "worker")
		if _, err = s.CreateMainRequest(t.Context(), iid, fmt.Sprint(i), "question", ""); err != nil {
			t.Fatal(err)
		}
	}
	_, _ = s.CompareAndSetIntentState(id, "paused", "running")
	if _, err = s.CreateMainRequest(t.Context(), id, "overflow", "question", ""); err == nil {
		t.Fatal("queue limit ignored")
	}
}

func TestMainRequestTaskTerminalAndQueueAdmission(t *testing.T) {
	d, err := Open(testDSN(t))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	task, err := d.CreateTask("consultation lifecycle", "fixture", nil, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		_, _ = d.Exec(`DELETE FROM tasks WHERE id=$1`, task.ID)
		_, _ = d.Exec(`DELETE FROM explorations WHERE id=$1`, task.ExplorationID)
	}()
	s := d.Exploration(task.ExplorationID)
	id, _ := s.AddIntent(map[string]any{"summary": "running fixture"}, 1, nil, "planner")
	_, _ = s.ClaimIntent(id, "worker")
	r, err := s.CreateMainRequest(t.Context(), id, "call", "question", "")
	if err != nil {
		t.Fatal(err)
	}
	containsTask := func() bool {
		ids, err := d.MainRequestTaskIDs(t.Context(), 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, id := range ids {
			if id == task.ID {
				return true
			}
		}
		return false
	}
	if !containsTask() {
		t.Fatal("pending task missing")
	}
	if err = d.SetPaused(task.ID, true); err != nil {
		t.Fatal(err)
	}
	if containsTask() {
		t.Fatal("paused task dispatched")
	}
	if err = d.SetPaused(task.ID, false); err != nil {
		t.Fatal(err)
	}
	if _, err = d.Exec(`UPDATE tasks SET status='done' WHERE id=$1`, task.ID); err != nil {
		t.Fatal(err)
	}
	got, _ := s.MainRequest(r.ID)
	if got.State != "cancelled" || containsTask() {
		t.Fatalf("terminal request: %+v", got)
	}
	if err = s.AnswerMainRequest(t.Context(), r.ID, "late", "mainagent"); !errors.Is(err, ErrIntentStateConflict) {
		t.Fatalf("late reply: %v", err)
	}
}

func TestMainRequestConcurrentRepliesAndStaleAttempt(t *testing.T) {
	d, s, id := mainRequestFixture(t)
	for round := 0; round < 40; round++ {
		r, err := s.CreateMainRequest(t.Context(), id, fmt.Sprint(round), "question", "")
		if err != nil {
			t.Fatal(err)
		}
		first, err := s.ClaimMainRequest(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		if _, err = d.Exec(`UPDATE main_requests SET updated_at=now()-interval '12 minutes' WHERE id=$1`, r.ID); err != nil {
			t.Fatal(err)
		}
		second, err := s.ClaimMainRequest(t.Context())
		if err != nil || second.Attempts != first.Attempts+1 {
			t.Fatalf("orphan processing not recovered: %+v %v", second, err)
		}
		if err = s.AnswerMainRequestAttempt(t.Context(), r.ID, "old run", "mainagent", first.Attempts); !errors.Is(err, ErrIntentStateConflict) {
			t.Fatalf("stale answer won: %v", err)
		}
		if err = s.RetryMainRequest(t.Context(), r.ID, first.Attempts, "old retry"); err != nil {
			t.Fatal(err)
		}
		current, _ := s.MainRequest(r.ID)
		if current.State != "processing" {
			t.Fatal("old retry reset new attempt")
		}
		start := make(chan struct{})
		results := make(chan error, 2)
		go func() {
			<-start
			results <- s.AnswerMainRequestAttempt(context.Background(), r.ID, "main answer", "mainagent", second.Attempts)
		}()
		go func() { <-start; results <- s.AnswerMainRequest(context.Background(), r.ID, "human answer", "human") }()
		close(start)
		wins := 0
		for i := 0; i < 2; i++ {
			err := <-results
			if err == nil {
				wins++
			} else if !errors.Is(err, ErrIntentStateConflict) {
				t.Fatal(err)
			}
		}
		if wins != 1 {
			t.Fatalf("reply winners=%d", wins)
		}
		rows, err := s.MainRequestActivities(r.ID)
		if err != nil || len(rows) != 2 {
			t.Fatalf("duplicate reply audit: %d %v", len(rows), err)
		}
		if err = s.AckMainReply(r.ID); err != nil {
			t.Fatal(err)
		}
	}
}

func TestMainRequestLockWaitHonorsCancellation(t *testing.T) {
	d, s, id := mainRequestFixture(t)
	r, err := s.CreateMainRequest(t.Context(), id, "call", "question", "")
	if err != nil {
		t.Fatal(err)
	}
	tx, err := d.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if _, err = tx.Exec(`SELECT id FROM exploration_nodes WHERE id=$1 FOR UPDATE`, id); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if err = s.AnswerMainRequest(ctx, r.ID, "cancelled caller", "human"); err == nil {
		t.Fatal("cancelled lock waiter committed")
	}
	got, _ := s.MainRequest(r.ID)
	if got.State != "pending" {
		t.Fatal("cancelled answer mutated request")
	}
}

func TestMainRequestArchiveRoundTrip(t *testing.T) {
	d, err := Open(testDSN(t))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	if err := d.EnsureLLMRecordsTable(); err != nil {
		t.Fatal(err)
	}
	if err := d.EnsureLLMUsageTable(); err != nil {
		t.Fatal(err)
	}
	task, err := d.CreateTask("request archive", "synthetic", nil, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer d.DeleteTask(task.ID)
	s := d.Exploration(task.ExplorationID)
	id, _ := s.AddIntent(map[string]any{"summary": "fixture"}, 1, nil, "planner")
	_, _ = s.ClaimIntent(id, "worker")
	r, err := s.CreateMainRequest(t.Context(), id, "call", "preserve question", "preserve evidence")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.ClaimMainRequest(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err = d.SetPaused(task.ID, true); err != nil {
		t.Fatal(err)
	}
	a, err := d.QueueTaskArchive(task.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Exec(`DELETE FROM task_archives WHERE id=$1`, a.ID)
	if _, err = d.ClaimTaskArchiveJob(t.Context()); err != nil {
		t.Fatal(err)
	}
	snapshot, err := d.SnapshotTaskArchive(task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.DataCounts["main_requests"] != 1 {
		t.Fatal("archive omitted requests")
	}
	if err = d.CompleteTaskArchive(a.ID, snapshot, "/tmp/synthetic-main-request.tar.zst", "fixture", 1, 1); err != nil {
		t.Fatal(err)
	}
	if _, err = d.QueueTaskArchiveRestore(a.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = d.ClaimTaskArchiveJob(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, err = d.RestoreTaskArchive(a.ID, snapshot, 0); err != nil {
		t.Fatal(err)
	}
	got, err := s.MainRequest(r.ID)
	if err != nil || got.State != "pending" || got.Context != "preserve evidence" {
		t.Fatalf("restore: %+v %v", got, err)
	}
}

func TestMainRequestConcurrentQueueLimit(t *testing.T) {
	d, err := Open(testDSN(t))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	task, err := d.CreateTask("concurrent inbox", "synthetic", nil, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer d.DeleteTask(task.ID)
	s := d.Exploration(task.ExplorationID)
	ids := make([]int64, 40)
	for i := range ids {
		ids[i], err = s.AddIntent(map[string]any{"summary": "parallel question"}, 1, nil, "planner")
		if err != nil {
			t.Fatal(err)
		}
		_, _ = s.ClaimIntent(ids[i], "worker")
	}
	start := make(chan struct{})
	results := make(chan error, len(ids))
	for _, id := range ids {
		go func() {
			<-start
			_, err := s.CreateMainRequest(context.Background(), id, "call", "question", "")
			results <- err
		}()
	}
	close(start)
	wins := 0
	for range ids {
		err := <-results
		if err == nil {
			wins++
		} else if !strings.Contains(err.Error(), "32") {
			t.Fatal(err)
		}
	}
	if wins != 32 {
		t.Fatalf("queue overflow/underflow: %d", wins)
	}
	rows, err := s.MainRequests(0, true)
	if err != nil || len(rows) != 32 {
		t.Fatalf("queue count: %d %v", len(rows), err)
	}
}

func TestMainRequestOutstandingSurvivesLongHistory(t *testing.T) {
	d, s, id := mainRequestFixture(t)
	r, err := s.CreateMainRequest(t.Context(), id, "old-pending", "still needs attention", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = d.Exec(`INSERT INTO main_requests(exploration_id,intent_id,call_id,question,state) SELECT $1,$2,'history-'||n,'synthetic historical question','delivered' FROM generate_series(1,150) n`, s.ID(), id); err != nil {
		t.Fatal(err)
	}
	rows, err := s.MainRequests(0, false)
	if err != nil || len(rows) != 100 || rows[0].ID != r.ID {
		t.Fatalf("outstanding request hidden by history: %d rows, %v", len(rows), err)
	}
}
