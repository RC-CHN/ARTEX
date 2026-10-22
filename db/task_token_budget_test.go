package db

import (
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
)

func TestTaskTokenBudgetCASAndCumulativeWrapup(t *testing.T) {
	d, err := Open(testDSN(t))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	if err = d.EnsureLLMUsageTable(); err != nil {
		t.Fatal(err)
	}
	task, err := d.CreateTaskWithOptions("synthetic task budget", "synthetic", TaskCreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	b, err := d.TaskTokenBudget(t.Context(), task.ID, true)
	if err != nil || b.Limit != 0 || b.Revision != 1 || b.Reached {
		t.Fatal(b, err)
	}
	var wins atomic.Int32
	var wg sync.WaitGroup
	for range 12 {
		wg.Go(func() {
			if _, e := d.SetTaskTokenBudget(t.Context(), task.ID, 1000, 1); e == nil {
				wins.Add(1)
			}
		})
	}
	wg.Wait()
	if wins.Load() != 1 {
		t.Fatal("CAS winners", wins.Load())
	}
	for _, role := range []string{"main", "worker", "planner", "compactor"} {
		if err = d.InsertLLMUsage(&LLMUsage{TaskID: fmt.Sprint(task.ID), Worker: role, InputTokens: 200, OutputTokens: 50, CacheRead: 100, Status: "ok"}); err != nil {
			t.Fatal(err)
		}
	}
	b, err = d.TaskTokenBudget(t.Context(), task.ID, true)
	if err != nil || b.Used != 1000 || !b.Reached || b.Revision != 2 {
		t.Fatal(b, err)
	}
	s := d.Exploration(task.ExplorationID)
	id, err := s.AddIntent(map[string]any{"summary": "synthetic"}, 1, nil, "test")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.EnsureWorkerBudget(t.Context(), task.ID, id, 150, 10); err != nil {
		t.Fatal(err)
	}
	// A one-connection pool catches accidental reads outside the reservation tx.
	d.SetMaxOpenConns(1)
	w, ok, err := s.ReserveWorkerTurn(t.Context(), task.ID, id, false)
	if err != nil || ok || w.Phase != "wrapup" || w.NormalUsed != 0 || w.TaskBudget == nil || !w.TaskBudget.Reached {
		t.Fatal(w, ok, err)
	}
	for range 10 {
		if _, ok, err = s.ReserveWorkerTurn(t.Context(), task.ID, id, true); err != nil || !ok {
			t.Fatal(ok, err)
		}
	}
	if _, ok, err = s.ReserveWorkerTurn(t.Context(), task.ID, id, true); err != nil || ok {
		t.Fatal(ok, err)
	}
	b, err = d.SetTaskTokenBudget(t.Context(), task.ID, 0, 2)
	if err != nil || b.Used != 1000 || b.Reached {
		t.Fatal(b, err)
	}
	if _, ok, err = s.ReserveWorkerTurn(t.Context(), task.ID, id, false); err != nil || ok {
		t.Fatal("reset bypass", ok, err)
	}
	if _, err = d.SetTaskTokenBudget(t.Context(), task.ID, -1, 3); err == nil {
		t.Fatal("negative allowed")
	}
	if _, err = d.Exec(`UPDATE tasks SET archived_at=now() WHERE id=$1`, task.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = d.SetTaskTokenBudget(t.Context(), task.ID, 123, 3); err == nil {
		t.Fatal("archived mutation")
	}
}
