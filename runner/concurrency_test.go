package runner

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestConcurrencyLimitQueuesDrainsAndPersists(t *testing.T) {
	store := &MemoryStore{}
	h := newTestHub(t, store, nil)
	n := registerNode(t, h, defaultCapabilities())
	a := openSession(t, n, spec("a"))
	b := openSession(t, n, spec("b"))
	if err := h.SetNodeConcurrencyLimit(n.id, 1); err != nil {
		t.Fatal(err)
	}
	view := h.ListNodes()[0]
	if view.ActiveSessions != 2 || view.EffectiveSlots != 1 {
		t.Fatalf("running work was lost: %+v", view)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	waiting := acquireAsync(h, ctx, spec("queued"))
	closeSession(t, n, a)
	select {
	case r := <-waiting:
		t.Fatalf("admitted above reduced limit: %+v", r)
	case <-time.After(25 * time.Millisecond):
	}
	closeSession(t, n, b)
	cmd := n.command(t, CommandOpen, "")
	if cmd.Spec.ConcurrencyLimit != 1 {
		t.Fatal("hard Pod admission did not receive controller limit")
	}
	n.ack(t, n.result(cmd))
	c := await(t, waiting)
	if c.err != nil {
		t.Fatal(c.err)
	}
	closeSession(t, n, c.s)
	for _, bad := range []int{-1, 5} {
		if h.SetNodeConcurrencyLimit(n.id, bad) == nil {
			t.Fatal("invalid limit accepted")
		}
	}
	_ = h.Close()
	next := newTestHub(t, store, nil)
	if got := next.ListNodes()[0]; got.ConcurrencyLimit != 1 || got.EffectiveSlots != 1 {
		t.Fatalf("lost persisted limit: %+v", got)
	}
	if err := next.SetNodeConcurrencyLimit(n.id, 0); err != nil {
		t.Fatal(err)
	}
	if next.ListNodes()[0].EffectiveSlots != 4 {
		t.Fatal("reset did not restore advertised ceiling")
	}
}

func TestConcurrencyCeilingCountsOpeningAndFencedAttempts(t *testing.T) {
	h := newTestHub(t, nil, nil)
	n := registerNode(t, h, defaultCapabilities())
	if err := h.SetNodeConcurrencyLimit(n.id, 1); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	pending := acquireAsync(h, ctx, spec("opening"))
	cmd := n.command(t, CommandOpen, "")
	cancel()
	<-pending
	ctx2, cancel2 := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel2()
	if _, err := h.Acquire(ctx2, spec("other")); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("fenced attempt released capacity: %v", err)
	}
	closeCmd := n.command(t, CommandClose, cmd.SessionID)
	n.ack(t, n.result(closeCmd))
	if h.ListNodes()[0].ActiveSessions != 0 {
		t.Fatal("confirmed cleanup did not release capacity")
	}
}

func TestResourceBudgetsApplyLiveAndPersist(t *testing.T) {
	store := &MemoryStore{}
	h := newTestHub(t, store, nil)
	n := registerNode(t, h, defaultCapabilities())
	a := openSession(t, n, spec("active"))
	if err := h.SetNodeLimits(n.id, 4, 1000, 1024); err != nil {
		t.Fatal(err)
	}
	v := h.ListNodes()[0]
	if v.ReservedCPUMillis != 1000 || v.ReservedMemoryMiB != 1024 || v.EffectiveCPUMillis != 1000 {
		t.Fatalf("wrong resource accounting: %+v", v)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	if _, err := h.Acquire(ctx, spec("over-budget")); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("budget bypass: %v", err)
	}
	if err := h.SetNodeLimits(n.id, 4, 2000, 2048); err != nil {
		t.Fatal(err)
	}
	b := openSession(t, n, spec("raised"))
	if err := h.SetNodeLimits(n.id, 4, 500, 512); err != nil {
		t.Fatal(err)
	}
	if h.ListNodes()[0].ActiveSessions != 2 {
		t.Fatal("reducing resources interrupted work")
	}
	closeSession(t, n, a)
	closeSession(t, n, b)
	if h.SetNodeLimits(n.id, 4, 9000, 1024) == nil {
		t.Fatal("exceeded advertised capacity")
	}
	_ = h.Close()
	next := newTestHub(t, store, nil)
	if got := next.ListNodes()[0]; got.EffectiveCPUMillis != 500 || got.EffectiveMemoryMiB != 512 {
		t.Fatalf("resource limits not persistent: %+v", got)
	}
}
