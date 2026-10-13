package runner

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/RC-CHN/ARTEX/runnerfiles"
)

type diagnosticSlowStore struct {
	MemoryStore
	delay atomic.Int64
	saves atomic.Int64
}

func (s *diagnosticSlowStore) Save(ctx context.Context, revision uint64, data json.RawMessage) (uint64, error) {
	if d := time.Duration(s.delay.Load()); d > 0 {
		timer := time.NewTimer(d)
		defer timer.Stop()
		select {
		case <-ctx.Done():
			return 0, ctx.Err()
		case <-timer.C:
		}
	}
	s.saves.Add(1)
	return s.MemoryStore.Save(ctx, revision, data)
}

// Isolated reproduction: generated files, fake nodes, no tools or real tasks.
func TestOpeningPollDoesNotStarveFileTransfers(t *testing.T) {
	for _, mode := range []string{"no_polls", "pending_open_hot_polls", "pending_open_paced_polls"} {
		t.Run(mode, func(t *testing.T) {
			store := &diagnosticSlowStore{}
			// Test contention, not a CI machine's HTTP/disk throughput. Keep lease
			// time fixed so a slow race run cannot turn an unchanged poll into a
			// legitimate renewal write; renewal itself is tested separately below.
			clock := &testClock{now: time.Now()}
			h, err := New(Options{Store: store, Clock: clock.Now, LeaseDuration: time.Minute, PollWait: time.Second})
			if err != nil {
				t.Fatal(err)
			}
			defer h.Close()
			n := registerNode(t, h, defaultCapabilities())
			source := t.TempDir()
			for i := 0; i < 40; i++ {
				if err := os.WriteFile(filepath.Join(source, fmt.Sprintf("file-%03d", i)), []byte(strings.Repeat("fixture ", 512)), 0600); err != nil {
					t.Fatal(err)
				}
			}
			plan := runnerfiles.Plan{Mounts: []runnerfiles.Mount{{Name: "workspace", Source: source, Target: "/workspace"}, {Name: "published", Source: source, Target: "/published"}}}
			raw, _ := json.Marshal(plan)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			acq := acquireAsync(h, ctx, RunSpec{ThreadID: "diagnostic", WorkingDir: "/workspace", Environment: "default", Files: raw})
			open := n.command(t, CommandOpen, "")
			// Mimic the production amount of retained history with artificial strings.
			h.mu.Lock()
			h.state.Sessions["synthetic-history"] = &sessionRecord{ID: "synthetic-history", State: "closed", FinishedAt: time.Now(), Spec: RunSpec{Env: map[string]string{"padding": strings.Repeat("x", 5<<20)}}}
			err = h.saveLocked()
			h.mu.Unlock()
			if err != nil {
				t.Fatal(err)
			}
			var authN, authNS, polls, repeats atomic.Int64
			handler := runnerfiles.Handler(runnerfiles.NewStore(), func(r *http.Request, id string, write bool) (runnerfiles.Plan, error) {
				start := time.Now()
				_, e := h.AuthorizeSession(r, id, write)
				authNS.Add(int64(time.Since(start)))
				authN.Add(1)
				return plan, e
			}, h.WithSessionWrite)
			mux := http.NewServeMux()
			mux.Handle("/runner/v1/files/", http.StripPrefix("/runner/v1/files", handler))
			srv := httptest.NewServer(mux)
			defer srv.Close()
			store.delay.Store(int64(25 * time.Millisecond))
			beforeSaves := store.saves.Load()
			var wg sync.WaitGroup
			pollCtx, stopPoll := context.WithCancel(ctx)
			if mode != "no_polls" {
				wg.Go(func() {
					for pollCtx.Err() == nil {
						p, _, e := h.poll(PollRequest{NodeID: n.id, BootID: n.boot, Protocol: ProtocolVersion}, n.id)
						if e != nil {
							return
						}
						polls.Add(1)
						for _, c := range p.Commands {
							if c.ID == open.ID {
								repeats.Add(1)
							}
						}
						if mode == "pending_open_paced_polls" {
							select {
							case <-pollCtx.Done():
								return
							case <-time.After(200 * time.Millisecond):
							}
						}
					}
				})
			}
			syncer := &runnerfiles.Syncer{Client: &runnerfiles.Client{BaseURL: srv.URL, Credential: n.credential, HTTP: srv.Client()}, Session: open.SessionID, Plan: plan, Roots: map[string]string{"workspace": t.TempDir(), "published": t.TempDir()}}
			pullCtx, stopPull := context.WithTimeout(ctx, 15*time.Second)
			start := time.Now()
			pullErr := syncer.Pull(pullCtx)
			elapsed := time.Since(start)
			stopPull()
			stopPoll()
			wg.Wait()
			store.delay.Store(0)

			t.Logf("mode=%s elapsed=%s pull_error=%v authorizations=%d authorization_wait=%s polls=%d same_open_deliveries=%d", mode, elapsed, pullErr, authN.Load(), time.Duration(authNS.Load()), polls.Load(), repeats.Load())
			if pullErr != nil {
				t.Fatalf("file synchronization starved by polling: %v", pullErr)
			}
			if written := store.saves.Load() - beforeSaves; written != 0 {
				t.Fatalf("unchanged polls rewrote retained history %d times", written)
			}
			cancel()
			select {
			case <-acq:
			case <-time.After(time.Second):
				t.Fatal("acquire cleanup timeout")
			}
		})
	}
}

func TestPollingRetainedHistoryRenewsDurablyAndCollects(t *testing.T) {
	clock := &testClock{now: time.Now()}
	store := &diagnosticSlowStore{}
	h := newTestHub(t, store, clock)
	n := registerNode(t, h, defaultCapabilities())
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	acq := acquireAsync(h, ctx, spec("live"))
	open := n.command(t, CommandOpen, "")
	h.mu.Lock()
	for i := range 10000 {
		id := fmt.Sprintf("history-%d", i)
		h.state.Sessions[id] = &sessionRecord{ID: id, NodeID: n.id, BootID: n.boot, Epoch: h.state.Epoch, State: "closed", FinishedAt: clock.Now(), Spec: RunSpec{Env: map[string]string{"fixture": strings.Repeat("x", 512)}}}
		h.redeliver[id] = clock.Now()
		h.state.Sessions[id].Commands = map[string]*commandRecord{id: {Command: Command{ID: id}}}
	}
	if err := h.saveLocked(); err != nil {
		t.Fatal(err)
	}
	h.mu.Unlock()
	before := store.saves.Load()
	start := time.Now()
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			for range 125 {
				p, _, err := h.poll(PollRequest{NodeID: n.id, BootID: n.boot, Protocol: ProtocolVersion}, n.id)
				if err != nil {
					t.Error(err)
					return
				}
				if len(p.Commands) != 0 {
					t.Error("unfinished command was continuously redelivered")
					return
				}
			}
		})
	}
	wg.Wait()
	if store.saves.Load() != before {
		t.Fatal("polling accumulated history caused a state write storm")
	}
	t.Logf("10,000 retained sessions, 1,000 concurrent polls: %s; state rewrites=0", time.Since(start))
	clock.Advance(2 * time.Second)
	p := n.poll(t)
	if store.saves.Load() != before+1 || len(p.Commands) != 1 || p.Commands[0].ID != open.ID {
		t.Fatal("lease renewal or lost-response retry failed")
	}
	snapshot, err := store.Load(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	var durable persistedState
	if err := json.Unmarshal(snapshot.Data, &durable); err != nil {
		t.Fatal(err)
	}
	if !durable.Sessions[open.SessionID].LeaseUntil.Equal(p.Leases[0].Until) {
		t.Fatal("advertised lease was not durable")
	}
	clock.Advance(25 * time.Hour)
	_ = n.poll(t)
	h.mu.Lock()
	if len(h.state.Sessions) != 1 || len(h.redeliver) != 1 {
		t.Errorf("retained history not collected: sessions=%d delivery hints=%d", len(h.state.Sessions), len(h.redeliver))
	}
	h.mu.Unlock()
	cancel()
	select {
	case <-acq:
	case <-time.After(time.Second):
		t.Fatal("acquire did not finish")
	}
}
