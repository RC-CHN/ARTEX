package server

import (
	"context"
	"fmt"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestWorkerSettingsResizeActivePool(t *testing.T) {
	m := runnerTestManager(t)
	ctx, cancel := context.WithCancel(context.Background())
	p := newWorkerPool(ctx, 1, func(ctx context.Context, _ int, _ *workerPool) { <-ctx.Done() })
	defer func() { cancel(); p.Close() }()
	e := &Engine{m: m, runtimes: map[string]*taskRuntime{"active": {ctx: ctx, pool: p}}}
	s := &Server{m: m, engine: e}
	for _, want := range []int{4, 2} {
		w := httptest.NewRecorder()
		s.putSettings(w, httptest.NewRequest("PUT", "/api/settings", strings.NewReader(fmt.Sprintf(`{"workers":%d}`, want))))
		if w.Code != 200 || m.Workers() != want {
			t.Fatalf("settings save: status=%d workers=%d", w.Code, m.Workers())
		}
		p.mu.Lock()
		got := p.limit
		p.mu.Unlock()
		if got != want || ctx.Err() != nil {
			t.Fatalf("active pool limit=%d, want=%d, ctx=%v", got, want, ctx.Err())
		}
	}
	w := httptest.NewRecorder()
	s.putSettings(w, httptest.NewRequest("PUT", "/api/settings", strings.NewReader(`{"workers":0}`)))
	if w.Code != 400 || m.Workers() != 2 {
		t.Fatal("invalid limit changed persisted worker count")
	}
}

func TestWorkerPoolLiveResizeDrainsWithoutInterrupting(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	started := make(chan int, 32)
	finish := make(chan struct{})
	p := newWorkerPool(ctx, 1, func(ctx context.Context, i int, p *workerPool) {
		for p.allowed(i) {
			if !p.begin(i) {
				if sleepCtx(ctx, time.Millisecond) {
					return
				}
				continue
			}
			started <- i
			select {
			case <-finish:
			case <-ctx.Done():
			}
			p.end()
		}
	})
	defer func() { cancel(); p.Close() }()
	next := func() {
		t.Helper()
		select {
		case <-started:
		case <-time.After(time.Second):
			t.Fatal("worker did not start")
		}
	}
	next()
	p.Resize(3)
	next()
	next()
	p.Resize(1)
	// Active work remains alive. No new step may start even if a low-index worker
	// finishes before the high-index workers that are draining.
	finish <- struct{}{}
	select {
	case <-started:
		t.Fatal("new work started above lowered limit")
	case <-time.After(20 * time.Millisecond):
	}
	finish <- struct{}{}
	select {
	case <-started:
		t.Fatal("new work started while capacity still full")
	case <-time.After(20 * time.Millisecond):
	}
	finish <- struct{}{}
	next()
}

func TestWorkerPoolResizeRacesWithShutdown(t *testing.T) {
	for range 20 {
		ctx, cancel := context.WithCancel(context.Background())
		p := newWorkerPool(ctx, 3, func(ctx context.Context, i int, p *workerPool) {
			for p.allowed(i) {
				if p.begin(i) {
					p.end()
				}
				if sleepCtx(ctx, time.Millisecond) {
					return
				}
			}
		})
		var wg sync.WaitGroup
		for range 4 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for i := 0; i < 50; i++ {
					p.Resize(i%5 + 1)
				}
			}()
		}
		cancel()
		p.Close()
		wg.Wait()
		p.mu.Lock()
		left, busy := len(p.workers), p.busy
		p.mu.Unlock()
		if left != 0 || busy != 0 {
			t.Fatalf("pool leaked workers=%d busy=%d", left, busy)
		}
	}
}
