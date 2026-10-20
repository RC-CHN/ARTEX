package db

import (
	"bytes"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"
)

type exportBuffer struct {
	bytes.Buffer
	after func() error
}

func (b *exportBuffer) Close() error {
	if b.after != nil {
		return b.after()
	}
	return nil
}
func TestRecordExportSnapshotIsConsistentAcrossTasks(t *testing.T) {
	d, err := Open(testDSN(t))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	if err = d.EnsureLLMRecordsTable(); err != nil {
		t.Fatal(err)
	}
	if err = d.EnsureLLMUsageTable(); err != nil {
		t.Fatal(err)
	}
	first, err := d.CreateTaskWithOptions("synthetic first", "old goal", TaskCreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	second, err := d.CreateTaskWithOptions("synthetic second", "old goal", TaskCreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	rows := map[string]*exportBuffer{}
	snapshots, err := d.SnapshotTaskRecords(t.Context(), []int64{first.ID, second.ID}, func(id int64, table string) (io.WriteCloser, error) {
		b := &exportBuffer{}
		if id == first.ID && table == "tasks" {
			b.after = func() error {
				_, e := d.Exec(`UPDATE tasks SET goal='NEW_GOAL_AFTER_SNAPSHOT' WHERE id=$1`, second.ID)
				return e
			}
		}
		rows[fmt.Sprintf("%d/%s", id, table)] = b
		return b, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshots) != 2 || snapshots[0].CreatedAt != snapshots[1].CreatedAt {
		t.Fatal("different cutoffs")
	}
	if strings.Contains(rows[fmt.Sprintf("%d/tasks", second.ID)].String(), "NEW_GOAL_AFTER_SNAPSHOT") {
		t.Fatal("mixed snapshots")
	}
}
func TestRecordExportLeaseFencesCanceledAndStaleWorkers(t *testing.T) {
	d, err := Open(testDSN(t))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	task, err := d.CreateTaskWithOptions("synthetic lease", "synthetic", TaskCreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	in := RecordExportRequest{TaskIDs: []int64{task.ID}, RequestID: "lease-once"}
	job, err := d.QueueRecordExport(t.Context(), in, 0)
	if err != nil {
		t.Fatal(err)
	}
	old, err := d.ClaimRecordExport(t.Context(), "old-owner")
	if err != nil || old.ID != job.ID {
		t.Fatal(old, err)
	}
	if _, err = d.Exec(`UPDATE task_record_exports SET lease_until=now()-interval '1 second' WHERE id=$1`, job.ID); err != nil {
		t.Fatal(err)
	}
	fresh, err := d.ClaimRecordExport(t.Context(), "fresh-owner")
	if err != nil || fresh.ID != job.ID || fresh.Attempt != 2 {
		t.Fatal(fresh, err)
	}
	if err = d.HeartbeatRecordExport(t.Context(), old.ID, old.Owner); !errors.Is(err, sql.ErrNoRows) {
		t.Fatal("stale heartbeat", err)
	}
	if err = d.FinishRecordExport(t.Context(), old.ID, old.Owner, "old", time.Now(), 1, 1); !errors.Is(err, sql.ErrNoRows) {
		t.Fatal("stale completion", err)
	}
	if _, err = d.ChangeRecordExport(t.Context(), job.ID, "cancel"); err != nil {
		t.Fatal(err)
	}
	if err = d.FinishRecordExport(t.Context(), fresh.ID, fresh.Owner, "fresh", time.Now(), 1, 1); !errors.Is(err, sql.ErrNoRows) {
		t.Fatal("canceled completion", err)
	}
	if _, err = d.ChangeRecordExport(t.Context(), job.ID, "retry"); err != nil {
		t.Fatal(err)
	}
	next, err := d.ClaimRecordExport(t.Context(), "retry-owner")
	if err != nil {
		t.Fatal(err)
	}
	if err = d.FinishRecordExport(t.Context(), next.ID, next.Owner, "good", time.Now(), 1, 1); err != nil {
		t.Fatal(err)
	}
	replay, err := d.QueueRecordExport(t.Context(), in, 0)
	if err != nil || replay.ID != job.ID || replay.State != "ready" {
		t.Fatal("replay not current", replay, err)
	}
}

func TestRecordExportRetryRespectsCapacityAndIsIdempotent(t *testing.T) {
	d, err := Open(testDSN(t))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	task, err := d.CreateTaskWithOptions("synthetic capacity", "synthetic", TaskCreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	job, err := d.QueueRecordExport(t.Context(), RecordExportRequest{TaskIDs: []int64{task.ID}, RequestID: "cancel-first"}, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = d.ChangeRecordExport(t.Context(), job.ID, "cancel"); err != nil {
		t.Fatal(err)
	}
	var last RecordExport
	for i := 0; i < 10; i++ {
		last, err = d.QueueRecordExport(t.Context(), RecordExportRequest{TaskIDs: []int64{task.ID}, RequestID: fmt.Sprint("capacity-", i)}, 0)
		if err != nil {
			t.Fatal(err)
		}
	}
	if _, err = d.ChangeRecordExport(t.Context(), job.ID, "retry"); err == nil || !strings.Contains(err.Error(), "上限") {
		t.Fatal("retry bypassed capacity", err)
	}
	if _, err = d.ChangeRecordExport(t.Context(), last.ID, "cancel"); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		v, e := d.ChangeRecordExport(t.Context(), job.ID, "retry")
		if e != nil || v.State != "queued" {
			t.Fatal("retry receipt", v, e)
		}
	}
}
