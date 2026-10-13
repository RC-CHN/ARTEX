package server

import "sync"

// asyncWrites stays open while cancelled agents finish reporting their final
// state. Close it only after their execution and background workers have joined.
type asyncWrites struct {
	mu      sync.Mutex
	writeMu sync.Mutex
	wg      sync.WaitGroup
	closed  bool
}

func (w *asyncWrites) start(write func()) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return false
	}
	w.wg.Add(1)
	go func() {
		defer w.wg.Done()
		// Serialize I/O without making callers wait for a previous database
		// write. Callbacks resolve their current state after taking this lock.
		w.writeMu.Lock()
		defer w.writeMu.Unlock()
		write()
	}()
	return true
}

func (w *asyncWrites) close() {
	w.mu.Lock()
	w.closed = true
	w.mu.Unlock()
	w.wg.Wait()
}
