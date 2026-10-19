package runner

import (
	"context"
	"encoding/json"
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

type progressiveMemoryStore struct{ MemoryStore }

func (*progressiveMemoryStore) CheckpointTimeout() time.Duration { return 2 * time.Minute }

func TestCheckpointBudgetUsesCommittedLease(t *testing.T) {
	now := time.Now()
	h := &Hub{opts: Options{Store: &progressiveMemoryStore{}, Clock: func() time.Time { return now }, LeaseDuration: 30 * time.Second}, state: persistedState{Sessions: map[string]*sessionRecord{}}, durableLeases: map[string]time.Time{}}
	if got := h.checkpointTimeoutLocked(); got != 2*time.Minute {
		t.Fatal("idle migration budget", got)
	}
	h.state.Sessions["s"] = &sessionRecord{State: "active", LeaseUntil: now.Add(30 * time.Second)}
	h.durableLeases["s"] = now.Add(12 * time.Second)
	if got := h.checkpointTimeoutLocked(); got != 7*time.Second {
		t.Fatal("pending renewal incorrectly extended write budget", got)
	}
	h.state.Sessions["another"] = &sessionRecord{State: "active"}
	h.durableLeases["another"] = now.Add(7 * time.Second)
	if got := h.checkpointTimeoutLocked(); got != 2*time.Second {
		t.Fatal("earliest lease not protected", got)
	}
	h.state.Sessions["another"].State = "fenced"
	if got := h.checkpointTimeoutLocked(); got != 7*time.Second {
		t.Fatal("fenced lease held up unrelated writes", got)
	}
	h.durableLeases["s"] = now.Add(time.Second)
	if got := h.checkpointTimeoutLocked(); got != 100*time.Millisecond {
		t.Fatal("nearly expired lease allowed a long write", got)
	}
}

type cancelCheckpointStore struct {
	progressiveMemoryStore
	block   atomic.Bool
	entered chan struct{}
}

func (s *cancelCheckpointStore) Save(ctx context.Context, revision uint64, raw json.RawMessage) (uint64, error) {
	if s.block.Load() {
		close(s.entered)
		<-ctx.Done()
		return 0, ctx.Err()
	}
	return s.MemoryStore.Save(ctx, revision, raw)
}

func TestPollCancellationRollsBackInProgressCheckpoint(t *testing.T) {
	clock := &testClock{now: time.Now()}
	store := &cancelCheckpointStore{entered: make(chan struct{})}
	h := newTestHub(t, store, clock)
	n := registerNode(t, h, defaultCapabilities())
	openSession(t, n, spec("cancel-checkpoint"))
	before := h.SessionStatuses()[0].LeaseUntil
	clock.Advance(2 * time.Second)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	store.block.Store(true)
	done := make(chan error, 1)
	go func() {
		_, _, err := h.pollContext(ctx, PollRequest{NodeID: n.id, BootID: n.boot, Protocol: ProtocolVersion}, n.id, true)
		done <- err
	}()
	await(t, store.entered)
	cancel()
	if err := await(t, done); !errors.Is(err, context.Canceled) {
		t.Fatal("in-progress save ignored caller cancellation", err)
	}
	store.block.Store(false)
	if !h.SessionStatuses()[0].LeaseUntil.Equal(before) {
		t.Fatal("canceled save published pending renewal")
	}
	if _, _, err := h.poll(PollRequest{NodeID: n.id, BootID: n.boot, Protocol: ProtocolVersion}, n.id); err != nil {
		t.Fatal("checkpoint did not recover after cancellation", err)
	}
}
