package db

import (
	"context"
	"testing"
	"time"
)

func TestLLMWritesBoundLockWait(t *testing.T) {
	d, _, _ := testSetup(t)
	defer d.Close()
	if err := d.EnsureLLMRecordsTable(); err != nil {
		t.Fatal(err)
	}
	if err := d.EnsureLLMUsageTable(); err != nil {
		t.Fatal(err)
	}
	tx, err := d.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if _, err = tx.Exec("LOCK TABLE llm_records,llm_usage IN ACCESS EXCLUSIVE MODE"); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name   string
		budget time.Duration
		call   func() error
	}{
		{"record", 3 * time.Second, func() error { return d.InsertLLMRecord(&LLMRecord{TaskID: "budget-regression"}) }},
		{"usage", time.Second, func() error { return d.InsertLLMUsage(&LLMUsage{TaskID: "budget-regression"}) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			started := time.Now()
			err := tc.call()
			elapsed := time.Since(started)
			if err == nil {
				t.Fatal("blocked insert succeeded")
			}
			if elapsed < tc.budget/2 || elapsed > tc.budget+2*time.Second {
				t.Fatalf("write budget %s, elapsed %s: %v", tc.budget, elapsed, err)
			}
		})
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err = d.InsertLLMUsageContext(ctx, &LLMUsage{}); err == nil {
		t.Fatal("ignored caller cancellation")
	}
	if err = tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	var count int
	if err = d.QueryRow("SELECT count(*) FROM llm_usage WHERE task_id='budget-regression'").Scan(&count); err != nil || count != 0 {
		t.Fatal("canceled usage committed", err)
	}
	if err = d.QueryRow("SELECT count(*) FROM llm_records WHERE task_id='budget-regression'").Scan(&count); err != nil || count != 0 {
		t.Fatal("canceled record committed", err)
	}
}
