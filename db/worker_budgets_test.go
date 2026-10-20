package db

import (
	"errors"
	"fmt"
	"sync"
	"testing"

	"github.com/RC-CHN/ARTEX/internal/operation"
)

func TestWorkerBudgetDefaultMigrationRunsOnlyOnce(t *testing.T) {
	d, err := Open(testDSN(t))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	var originalNormal, originalWrap int
	if err = d.QueryRow(`SELECT max_turns,wrapup_max_turns FROM agents WHERE key='worker'`).Scan(&originalNormal, &originalWrap); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if _, err := d.Exec(`UPDATE agents SET max_turns=$1,wrapup_max_turns=$2 WHERE key='worker'`, originalNormal, originalWrap); err != nil {
			t.Error(err)
		}
	}()
	for _, tc := range []struct{ normal, wrap, wantNormal, wantWrap int }{{0, 0, 150, 10}, {37, 4, 37, 4}} {
		if _, err = d.Exec(`DELETE FROM settings WHERE key='worker_budget_150_v1'`); err != nil {
			t.Fatal(err)
		}
		if _, err = d.Exec(`UPDATE agents SET max_turns=$1,wrapup_max_turns=$2 WHERE key='worker'`, tc.normal, tc.wrap); err != nil {
			t.Fatal(err)
		}
		if err = d.seedWorkerBudgetDefaults(); err != nil {
			t.Fatal(err)
		}
		var normal, wrap int
		if err = d.QueryRow(`SELECT max_turns,wrapup_max_turns FROM agents WHERE key='worker'`).Scan(&normal, &wrap); err != nil {
			t.Fatal(err)
		}
		if normal != tc.wantNormal || wrap != tc.wantWrap {
			t.Fatalf("migration %d+%d", normal, wrap)
		}
	}
	if _, err = d.Exec(`UPDATE agents SET max_turns=0 WHERE key='worker'`); err != nil {
		t.Fatal(err)
	}
	if err = d.seedWorkerBudgetDefaults(); err != nil {
		t.Fatal(err)
	}
	var normal int
	if err = d.QueryRow(`SELECT max_turns FROM agents WHERE key='worker'`).Scan(&normal); err != nil || normal != 0 {
		t.Fatal("explicit unlimited changed", normal, err)
	}
}

func TestWorkerBudgetConcurrentReservationAndGrant(t *testing.T) {
	d, err := Open(testDSN(t))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	task, err := d.CreateTaskWithOptions("synthetic budget", "synthetic", TaskCreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	s := d.Exploration(task.ExplorationID)
	id, err := s.AddIntent(map[string]any{"summary": "synthetic"}, 1, nil, "test")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.EnsureWorkerBudget(t.Context(), task.ID, id, 150, 10); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	allowed := make(chan bool, 180)
	errs := make(chan error, 180)
	for range 180 {
		wg.Go(func() { _, a, e := s.ReserveWorkerTurn(t.Context(), task.ID, id, false); allowed <- a; errs <- e })
	}
	wg.Wait()
	close(allowed)
	close(errs)
	n := 0
	for a := range allowed {
		if a {
			n++
		}
	}
	for e := range errs {
		if e != nil {
			t.Fatal(e)
		}
	}
	if n != 150 {
		t.Fatal("overspent normal", n)
	}
	b, err := s.EnsureWorkerBudget(t.Context(), task.ID, id, 999, 99)
	if err != nil || b.NormalUsed != 150 || b.NormalLimit != 150 || b.WrapupLimit != 10 {
		t.Fatalf("resume reset: %+v %v", b, err)
	}
	for i := range 13 {
		_, a, e := s.ReserveWorkerTurn(t.Context(), task.ID, id, true)
		if e != nil || a != (i < 10) {
			t.Fatalf("wrapup %d allowed=%v err=%v", i, a, e)
		}
	}
	b, err = d.WorkerBudget(t.Context(), task.ID, id)
	if err != nil || b.Phase != "exhausted" || b.WrapupUsed != 10 {
		t.Fatal(b, err)
	}
	in := WorkerBudgetGrant{IntentID: id, ExpectedRevision: b.Revision, RequestID: "grant-once", AdditionalTurns: 20, AdditionalWrapupTurns: 10, Reason: "synthetic additional evidence"}
	if _, err = d.GrantWorkerBudget(t.Context(), task.ID, in, "worker"); err == nil {
		t.Fatal("worker granted itself budget")
	}
	once, err := d.GrantWorkerBudget(t.Context(), task.ID, in, "mainagent")
	if err != nil {
		t.Fatal(err)
	}
	again, err := d.GrantWorkerBudget(t.Context(), task.ID, in, "mainagent")
	if err != nil || again != once {
		t.Fatalf("duplicate grant %+v %v", again, err)
	}
	if once.NormalLimit != 170 || once.WrapupLimit != 20 || once.NormalUsed != 150 || once.WrapupUsed != 10 {
		t.Fatal(once)
	}
	in.RequestID = "stale"
	var conflict *operation.Error
	if _, err = d.GrantWorkerBudget(t.Context(), task.ID, in, "mainagent"); !errors.As(err, &conflict) || conflict.Code != "budget_conflict" {
		t.Fatalf("stale grant %v", err)
	}
	in.RequestID = "grant-once"
	in.Reason = "changed"
	if _, err = d.GrantWorkerBudget(t.Context(), task.ID, in, "mainagent"); !errors.Is(err, operation.ErrKeyReuse) {
		t.Fatal(err)
	}
	for i := range 21 {
		_, a, e := s.ReserveWorkerTurn(t.Context(), task.ID, id, false)
		if e != nil || a != (i < 20) {
			t.Fatal(fmt.Sprint(i, a, e))
		}
	}
}
