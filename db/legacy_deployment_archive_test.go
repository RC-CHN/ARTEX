package db

import (
	"testing"
)

func TestLegacyDeploymentArchiveCompatibility(t *testing.T) {
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
	// Old rows are archival data, never live handoff checks or editable ledger.
	var entryID int64
	if err = d.QueryRow(`INSERT INTO task_deployments(task_id,name,kind,location,identifier,cleanup,state,actor) VALUES($1,'fixture','file','local','fixture.txt','verify absence','active','{}') RETURNING id`, task.ID).Scan(&entryID); err != nil {
		t.Fatal(err)
	}
	if _, err = d.Exec(`INSERT INTO task_deployment_events(task_id,deployment_id,revision,state,actor,snapshot) SELECT task_id,id,revision,state,actor,to_jsonb(task_deployments) FROM task_deployments WHERE id=$1`, entryID); err != nil {
		t.Fatal(err)
	}
	checks, err := d.TaskHandoff(t.Context(), task.ID)
	if err != nil || len(checks) != 0 {
		t.Fatalf("legacy row blocks handoff: %+v %v", checks, err)
	}
	// Worker checks remain active after removal of deployment checks.
	intent, err := d.Exploration(task.ExplorationID).AddIntent(map[string]any{"summary": "synthetic"}, 5, nil, "planner")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = d.Exec(`UPDATE exploration_nodes SET state='blocked' WHERE id=$1`, intent); err != nil {
		t.Fatal(err)
	}
	checks, err = d.TaskHandoff(t.Context(), task.ID)
	if err != nil || len(checks) != 1 || checks[0].Code != "workers_incomplete" || checks[0].Count != 1 {
		t.Fatalf("worker check missing: %+v %v", checks, err)
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
	if snapshot.DataCounts["task_deployments"] != 1 || snapshot.DataCounts["task_deployment_events"] != 1 {
		t.Fatal(snapshot.DataCounts)
	}
	if err = d.CompleteTaskArchive(archive.ID, snapshot, "/tmp/synthetic-archive", "hash", 1, 1); err != nil {
		t.Fatal(err)
	}
	var state string
	if err = d.QueryRow(`SELECT state FROM task_deployments WHERE id=$1`, entryID).Scan(&state); err != nil || state != "active" {
		t.Fatalf("legacy row lost after archive: %q %v", state, err)
	}
}
