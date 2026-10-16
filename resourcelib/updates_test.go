package resourcelib

import (
	"errors"
	"sync"
	"testing"

	"github.com/RC-CHN/ARTEX/db"
)

func TestExplicitVersionSwitchCASAndDurableUpdates(t *testing.T) {
	s, a := fixture(t)
	r := resource(t, s, a)
	v1 := publish(t, s, draft(t, s, r, a), a)
	v2 := publish(t, s, draft(t, s, r, a), a)
	v3 := publish(t, s, draft(t, s, r, a), a)
	if _, err := s.Switch(t.Context(), a.TaskID, r.ID, v2.ID, v1.ID); !errors.Is(err, ErrConflict) {
		t.Fatalf("switch must not create access: %v", err)
	}
	if _, err := s.Pin(t.Context(), a.TaskID, r.ID, v1.ID, false); err != nil {
		t.Fatal(err)
	}
	states, err := s.UpdateStates(t.Context(), a.TaskID)
	if err != nil || len(states) != 1 || states[0].PinnedID != v1.ID || states[0].LatestID != v3.ID {
		t.Fatalf("durable update projection: %v %v", states, err)
	}
	var wg sync.WaitGroup
	wins := make(chan int64, 2)
	for _, target := range []int64{v2.ID, v3.ID} {
		wg.Go(func() {
			v, err := s.Switch(t.Context(), a.TaskID, r.ID, target, v1.ID)
			if err == nil {
				wins <- v.ID
			} else if !errors.Is(err, ErrConflict) {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	close(wins)
	if len(wins) != 1 {
		t.Fatalf("concurrent switch winners=%d", len(wins))
	}
	winner := <-wins
	current, err := s.Pin(t.Context(), a.TaskID, r.ID, 0, false)
	if err != nil || current.ID != winner {
		t.Fatal("binding inconsistent")
	}
	if _, err = s.Switch(t.Context(), a.TaskID, r.ID, v2.ID, v1.ID); !errors.Is(err, ErrConflict) {
		t.Fatal("stale switch overwrote current version")
	}
	if _, err = s.ReadDocument(t.Context(), v1.Manifest); err != nil {
		t.Fatal("switch removed old version")
	}
}

func TestArchiveRestoresExactResourceVersion(t *testing.T) {
	s, a := fixture(t)
	r := resource(t, s, a)
	v := publish(t, s, draft(t, s, r, a), a)
	if _, err := s.Pin(t.Context(), a.TaskID, r.ID, v.ID, false); err != nil {
		t.Fatal(err)
	}
	pg := &db.DB{DB: s.DB}
	if err := pg.EnsureLLMRecordsTable(); err != nil {
		t.Fatal(err)
	}
	if err := pg.EnsureLLMUsageTable(); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB.Exec(`UPDATE tasks SET paused=true WHERE id=$1`, a.TaskID); err != nil {
		t.Fatal(err)
	}
	archive, err := pg.QueueTaskArchive(a.TaskID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.DB.Exec(`UPDATE task_archives SET state='archiving' WHERE id=$1`, archive.ID); err != nil {
		t.Fatal(err)
	}
	snapshot, err := pg.SnapshotTaskArchive(a.TaskID)
	if err != nil {
		t.Fatal(err)
	}
	if err = pg.CompleteTaskArchive(archive.ID, snapshot, "synthetic.tar", "fixture", 1, 1); err != nil {
		t.Fatal(err)
	}
	if states, err := s.UpdateStates(t.Context(), a.TaskID); err != nil || len(states) != 0 {
		t.Fatal("archived task retained active binding")
	}
	if _, err = s.DB.Exec(`UPDATE task_archives SET state='restoring' WHERE id=$1`, archive.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = pg.RestoreTaskArchive(archive.ID, snapshot, 0); err != nil {
		t.Fatal(err)
	}
	var restored int64
	if err = s.DB.QueryRow(`SELECT version_id FROM task_owned_resources WHERE task_id=$1 AND resource_id=$2`, a.TaskID, r.ID).Scan(&restored); err != nil || restored != v.ID {
		t.Fatalf("restore=%d %v", restored, err)
	}
}
