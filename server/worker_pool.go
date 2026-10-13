package server

import (
	"context"
	"sync"
)

// workerPool changes admission immediately while allowing already-running work
// to finish. Its own lifetime joins dynamic goroutines before task teardown.
type workerPool struct {
	mu          sync.Mutex
	ctx         context.Context
	limit, busy int
	closed      bool
	workers     map[int]bool
	wg          sync.WaitGroup
	run         func(context.Context, int, *workerPool)
}

func newWorkerPool(ctx context.Context, limit int, run func(context.Context, int, *workerPool)) *workerPool {
	p := &workerPool{ctx: ctx, workers: map[int]bool{}, run: run}
	p.Resize(limit)
	return p
}
func (p *workerPool) Resize(limit int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed || p.ctx.Err() != nil {
		return
	}
	if limit < 1 {
		limit = 1
	}
	p.limit = limit
	for i := 0; i < limit; i++ {
		if !p.workers[i] {
			p.spawnLocked(i)
		}
	}
}
func (p *workerPool) spawnLocked(i int) {
	p.workers[i] = true
	p.wg.Add(1)
	go func() {
		defer p.wg.Done()
		defer func() {
			p.mu.Lock()
			defer p.mu.Unlock()
			delete(p.workers, i)
			// A simultaneous shrink/grow may revive this index while the old
			// worker is retiring. Replace it only after the old loop exits.
			if !p.closed && p.ctx.Err() == nil && i < p.limit {
				p.spawnLocked(i)
			}
		}()
		p.run(p.ctx, i, p)
	}()
}
func (p *workerPool) allowed(i int) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return !p.closed && i < p.limit && p.ctx.Err() == nil
}
func (p *workerPool) begin(i int) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed || i >= p.limit || p.busy >= p.limit || p.ctx.Err() != nil {
		return false
	}
	p.busy++
	return true
}
func (p *workerPool) end() { p.mu.Lock(); p.busy--; p.mu.Unlock() }
func (p *workerPool) Close() {
	p.mu.Lock()
	p.closed = true
	p.mu.Unlock()
	p.wg.Wait()
}

// ReconcileWorkers reads the persisted value while serializing updates with task
// startup/deletion. It never cancels the task's execution context.
func (e *Engine) ReconcileWorkers() {
	e.deleteMu.RLock()
	defer e.deleteMu.RUnlock()
	if e.closed || e.m == nil {
		return
	}
	e.runtimeMu.Lock()
	defer e.runtimeMu.Unlock()
	limit := e.m.Workers()
	for id, rt := range e.runtimes {
		if rt.pool != nil && !e.IsDeleting(id) {
			rt.pool.Resize(limit)
		}
	}
}
