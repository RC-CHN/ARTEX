package db

import (
	"errors"
	"sync"
	"testing"

	"github.com/RC-CHN/ARTEX/internal/operation"
)

func TestDeploymentConcurrentCASAndArchive(t *testing.T) {
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
	task, err := d.CreateTaskWithOptions("synthetic cleanup", "synthetic", TaskCreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	str := func(v string) *string { return &v }
	entry, err := d.ChangeDeployment(t.Context(), task.ID, DeploymentChange{RequestID: "new", Name: str("fixture"), Kind: str("file"), Location: str("local"), Identifier: str("/tmp/synthetic-fixture"), Cleanup: str("remove exact fixture and verify absence")}, map[string]any{"task_id": task.ID})
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	results := make(chan error, 2)
	for _, key := range []string{"left", "right"} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, e := d.ChangeDeployment(t.Context(), task.ID, DeploymentChange{ID: entry.ID, ExpectedRevision: entry.Revision, RequestID: key, Note: str(key)}, nil)
			results <- e
		}()
	}
	wg.Wait()
	close(results)
	wins, conflicts := 0, 0
	for e := range results {
		var conflict *operation.Error
		if e == nil {
			wins++
		} else if errors.As(e, &conflict) && conflict.Code == "deployment_conflict" {
			conflicts++
		} else {
			t.Fatal(e)
		}
	}
	if wins != 1 || conflicts != 1 {
		t.Fatalf("CAS wins=%d conflicts=%d", wins, conflicts)
	}
	if _, err = d.Exec(`UPDATE tasks SET paused=true WHERE id=$1`, task.ID); err != nil {
		t.Fatal(err)
	}
	archive, err := d.QueueTaskArchive(task.ID)
	if err != nil {
		t.Fatal(err)
	}
	claimed, err := d.ClaimTaskArchiveJob(t.Context())
	if err != nil || claimed == nil || claimed.ID != archive.ID {
		t.Fatalf("claim: %v %v", claimed, err)
	}
	snapshot, err := d.SnapshotTaskArchive(task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.DataCounts["task_deployments"] != 1 || snapshot.DataCounts["task_deployment_events"] != 2 {
		t.Fatal(snapshot.DataCounts)
	}
	if err = d.CompleteTaskArchive(archive.ID, snapshot, "/tmp/synthetic-archive", "hash", 1, 1); err != nil {
		t.Fatal(err)
	}
	kept, err := d.Deployment(t.Context(), task.ID, entry.ID)
	if err != nil || kept.State != "active" {
		t.Fatalf("cleanup lost after archive: %+v %v", kept, err)
	}
	_, err = d.ChangeDeployment(t.Context(), task.ID, DeploymentChange{ID: kept.ID, ExpectedRevision: kept.Revision, RequestID: "cleanup-after-archive", State: str("removed"), Verification: str("Synthetic fixture absence verified after cold archive")}, nil)
	if err != nil {
		t.Fatal("cannot record cleanup for archived task", err)
	}
}
