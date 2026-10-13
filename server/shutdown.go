package server

import "github.com/Autumn-27/artex/agent"

// background admits work before shutdown begins. The mutex makes Wait safe
// even when a finishing task attempts to schedule follow-up work.
func (s *Server) background(run func()) bool {
	s.backgroundMu.Lock()
	defer s.backgroundMu.Unlock()
	if s.closing {
		return false
	}
	s.backgroundWG.Add(1)
	go func() {
		defer s.backgroundWG.Done()
		run()
	}()
	return true
}

// Close cancels and joins server-owned work before the manager closes its
// database or callers remove the data directory. It is safe to call repeatedly.
// Stop accepting HTTP requests before calling Close.
func (s *Server) Close() {
	s.closeOnce.Do(func() {
		s.backgroundMu.Lock()
		s.closing = true
		if s.cancel != nil {
			s.cancel(agent.AbortShutdown)
		}
		s.backgroundMu.Unlock()
		if s.engine != nil {
			s.engine.Close()
		}
		s.backgroundWG.Wait()
		s.archiveWG.Wait()
		if s.logDone != nil {
			<-s.logDone
		}
		// Executions can enqueue final breaker updates while they are draining.
		// Seal persistence after those producers finish, then keep PG alive until
		// every accepted update has completed.
		s.persistence.close()
	})
}

// Close prevents new engine admissions and waits for both task loops and
// already admitted operations, including standalone worker chat turns.
func (e *Engine) Close() {
	e.closeOnce.Do(func() {
		e.deleteMu.Lock()
		e.closed = true
		// Name execution cancellation before cancelling their ordinary parent
		// contexts. Context cancellation keeps the first cause it observes.
		e.execMu.Lock()
		for _, cancel := range e.execCancel {
			cancel(agent.AbortShutdown)
		}
		e.execMu.Unlock()
		e.runtimeMu.Lock()
		runtimes := make([]*taskRuntime, 0, len(e.runtimes))
		for _, rt := range e.runtimes {
			rt.cancel()
			runtimes = append(runtimes, rt)
		}
		e.runtimeMu.Unlock()
		e.deleteMu.Unlock()
		for _, rt := range runtimes {
			rt.wg.Wait()
		}
		e.operationsWG.Wait()
	})
}

func (m *Manager) onClose(close func()) {
	m.closeMu.Lock()
	if m.closing {
		m.closeMu.Unlock()
		close()
		return
	}
	m.closeHooks = append(m.closeHooks, close)
	m.closeMu.Unlock()
}
