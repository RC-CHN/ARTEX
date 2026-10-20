package resourcelib

import (
	"errors"
	"fmt"
	"sync"
	"testing"

	"github.com/RC-CHN/ARTEX/internal/operation"
)

func TestResourceDeleteReceiptsReferencesAndHistory(t *testing.T) {
	s, a := fixture(t)
	r := resource(t, s, a)
	v := publish(t, s, draft(t, s, r, a), a)
	if _, err := s.Pin(t.Context(), a.TaskID, r.ID, v.ID, false); err != nil {
		t.Fatal(err)
	}
	var other, exp int64
	if err := s.DB.QueryRow(`INSERT INTO explorations(goal) VALUES('synthetic other') RETURNING id`).Scan(&exp); err != nil {
		t.Fatal(err)
	}
	if err := s.DB.QueryRow(`INSERT INTO tasks(description,goal,exploration_id) VALUES('synthetic','synthetic',$1) RETURNING id`, exp).Scan(&other); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Pin(t.Context(), other, r.ID, v.ID, true); err != nil {
		t.Fatal(err)
	}
	r, err := s.Get(t.Context(), r.ID)
	if err != nil {
		t.Fatal(err)
	}
	in := DeleteRequest{ResourceID: r.ID, ExpectedRevision: r.Revision, RequestID: "retire-once", Reason: "synthetic cleanup verified"}
	var blocked *InUse
	if _, err = s.Delete(t.Context(), in, a); !errors.As(err, &blocked) || len(blocked.References) != 1 || blocked.References[0].TaskID != other {
		t.Fatalf("missing actionable blocker: %v", err)
	}
	if _, err = s.DB.Exec(`UPDATE tasks SET status='done' WHERE id=$1`, other); err != nil {
		t.Fatal(err)
	}
	out, err := s.Delete(t.Context(), in, a)
	if err != nil {
		t.Fatal(err)
	}
	if !out.Deleted || !out.HistoryRetained || out.RemoteCleanupPerformed || out.Resource.Enabled {
		t.Fatalf("misleading receipt: %+v", out)
	}
	again, err := s.Delete(t.Context(), in, a)
	if err != nil || again.Resource.Revision != out.Resource.Revision {
		t.Fatalf("idempotency: %+v %v", again, err)
	}
	in.Reason = "different request"
	if _, err = s.Delete(t.Context(), in, a); !errors.Is(err, operation.ErrKeyReuse) {
		t.Fatalf("request key reused: %v", err)
	}
	if _, err = s.Pin(t.Context(), a.TaskID, r.ID, v.ID, false); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("deleted resource still claimable: %v", err)
	}
	if _, err = s.NewDraft(t.Context(), r.ID, v.ID, a); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("deleted resource editable: %v", err)
	}
	if _, err = s.Version(t.Context(), v.ID); err != nil {
		t.Fatal("history disappeared", err)
	}
	items, err := s.List(t.Context(), "", "", 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range items {
		if item.ID == r.ID {
			t.Fatal("tombstone appears in search")
		}
	}
}

func TestResourceCreateRequestConcurrentRetry(t *testing.T) {
	s, a := fixture(t)
	var wg sync.WaitGroup
	ids := make(chan int64, 12)
	errs := make(chan error, 12)
	for range 12 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r := Resource{Name: "idempotent synthetic", Kind: "file", AccessMode: "global", Enabled: true}
			err := s.CreateWithRequest(t.Context(), &r, a, "create-once")
			ids <- r.ID
			errs <- err
		}()
	}
	wg.Wait()
	close(ids)
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	var id int64
	for v := range ids {
		if id == 0 {
			id = v
		}
		if v != id {
			t.Fatal("duplicate resource", id, v)
		}
	}
	changed := Resource{Name: "different", Kind: "file", AccessMode: "global", Enabled: true}
	if err := s.CreateWithRequest(t.Context(), &changed, a, "create-once"); !errors.Is(err, operation.ErrKeyReuse) {
		t.Fatal(err)
	}
	var n int
	if err := s.DB.QueryRow(`SELECT count(*) FROM owned_resource_events WHERE resource_id=$1 AND kind='created'`, id).Scan(&n); err != nil || n != 1 {
		t.Fatal(fmt.Sprint("duplicate events ", n, " ", err))
	}
}
