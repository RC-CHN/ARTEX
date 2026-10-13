package server

import (
	"sync"
	"testing"
	"time"

	"github.com/Autumn-27/artex/db"
)

type gatedHealthStore struct {
	mu          sync.Mutex
	row         *db.LLMHealth
	writes      int
	firstSave   chan struct{}
	releaseSave chan struct{}
	nextWrite   chan struct{}
}

func (s *gatedHealthStore) LoadLLMHealth() ([]db.LLMHealth, error) { return nil, nil }

func (s *gatedHealthStore) SaveLLMHealth(row db.LLMHealth) error {
	s.mu.Lock()
	s.writes++
	first := s.writes == 1
	s.mu.Unlock()
	if first {
		close(s.firstSave)
		<-s.releaseSave
	} else {
		s.nextWrite <- struct{}{}
	}
	s.mu.Lock()
	s.row = &row
	s.mu.Unlock()
	return nil
}

func (s *gatedHealthStore) ClearLLMHealth(int64) error {
	s.nextWrite <- struct{}{}
	s.mu.Lock()
	s.row = nil
	s.mu.Unlock()
	return nil
}

func TestLLMHealthPersistenceKeepsLatestState(t *testing.T) {
	for _, tripAgain := range []bool{false, true} {
		name := "trip_then_pass"
		if tripAgain {
			name = "trip_pass_then_new_trip"
		}
		t.Run(name, func(t *testing.T) {
			store := &gatedHealthStore{firstSave: make(chan struct{}), releaseSave: make(chan struct{}), nextWrite: make(chan struct{}, 4)}
			var writes asyncWrites
			var release sync.Once
			defer func() {
				release.Do(func() { close(store.releaseSave) })
				writes.close()
			}()
			reg := newLLMHealthRegistryWithStore(store, &writes)
			reg.Trip(1, "old outage", true)
			select {
			case <-store.firstSave:
			case <-time.After(time.Second):
				t.Fatal("first save did not start")
			}
			// The first Save already holds its old snapshot. Recovery and a new
			// failure must remain responsive while that database call is blocked.
			changed := make(chan struct{})
			go func() {
				reg.Pass(1)
				if tripAgain {
					reg.Trip(1, "new outage", true)
				}
				close(changed)
			}()
			select {
			case <-changed:
			case <-time.After(time.Second):
				t.Fatal("registry callback blocked on database persistence")
			}
			select {
			case <-store.nextWrite:
				t.Fatal("a newer write overtook the blocked first save")
			case <-time.After(20 * time.Millisecond):
			}
			release.Do(func() { close(store.releaseSave) })
			writes.close()
			store.mu.Lock()
			defer store.mu.Unlock()
			if !tripAgain && store.row != nil {
				t.Fatalf("recovery was overwritten by stale failure: %+v", store.row)
			}
			if tripAgain && (store.row == nil || store.row.LastError != "new outage" || store.row.Trips != 1) {
				t.Fatalf("persisted state did not match the latest failure: %+v", store.row)
			}
		})
	}
}
