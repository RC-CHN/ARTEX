package server

import (
	"context"
	"errors"
	"log"
	"time"

	"github.com/RC-CHN/ARTEX/runner"
)

type runnerRecoveryStatus struct {
	Attempts        uint64    `json:"attempts"`
	Failures        uint64    `json:"consecutive_failures"`
	Recovering      bool      `json:"recovering"`
	ManualRequired  bool      `json:"manual_required"`
	LastAttemptAt   time.Time `json:"last_attempt_at,omitempty"`
	NextAttemptAt   time.Time `json:"next_attempt_at,omitempty"`
	LastRecoveredAt time.Time `json:"last_recovered_at,omitempty"`
	LastFailureCode string    `json:"last_failure_code,omitempty"`
}

func (s *Server) recoverySnapshot() runnerRecoveryStatus {
	s.runnerMu.RLock()
	defer s.runnerMu.RUnlock()
	return s.runnerRecovery
}

// Called only by a lifecycle owner. Snapshot readers never wait for database I/O.
func (s *Server) publishRunnerState(cfg runnerConfig, hub *runner.Hub, err error) {
	s.runnerMu.Lock()
	s.runnerCfg, s.runners, s.runnerErr = cfg, hub, err
	s.runnerRecovery = runnerRecoveryStatus{}
	s.runnerMu.Unlock()
}

func runnerNeedsRemote(cfg runnerConfig, key string) bool {
	mode := cfg.DefaultMode
	if override := cfg.AgentModes[key]; override != "" {
		mode = override
	}
	return cfg.Enabled && mode != "local"
}

func (s *Server) runnerExecutionAvailable(key string) bool {
	if s.ctx != nil && s.ctx.Err() != nil {
		return false
	}
	cfg, hub, err := s.runnerSnapshot()
	return err == nil && (!runnerNeedsRemote(cfg, key) || (hub != nil && !hub.IsClosed()))
}

func (s *Server) runnerRecoveryLoop() {
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	for {
		select {
		case <-s.ctx.Done():
			return
		case now := <-tick.C:
			s.recoverRunner(s.ctx, now)
		}
	}
}

func runnerRecoveryBackoff(failures uint64) time.Duration {
	return min(time.Minute, 5*time.Second*time.Duration(1<<min(failures, 4)))
}

// Only one recovery/configuration/shutdown can own the broker. Do not block a
// health/protocol reader behind this operation, and do not retry CAS conflicts:
// they may indicate a second controller, not a transient storage outage.
func (s *Server) recoverRunner(ctx context.Context, now time.Time) {
	if ctx.Err() != nil || !s.runnerLifecycleMu.TryLock() {
		return
	}
	defer s.runnerLifecycleMu.Unlock()
	if ctx.Err() != nil {
		return
	}
	cfg, old, configErr := s.runnerSnapshot()
	if !cfg.Enabled || old == nil || configErr != nil || !old.BrokerState().AutoRecovery {
		return
	}
	status := s.recoverySnapshot()
	if status.ManualRequired || now.Before(status.NextAttemptAt) {
		return
	}
	// A short initial cooldown prevents an outage from creating a restart loop.
	if status.NextAttemptAt.IsZero() {
		status.NextAttemptAt = now.Add(runnerRecoveryBackoff(0))
		s.runnerMu.Lock()
		s.runnerRecovery = status
		s.runnerMu.Unlock()
		return
	}
	status.Attempts++
	status.Recovering, status.LastAttemptAt = true, now
	s.runnerMu.Lock()
	s.runnerRecovery = status
	s.runnerMu.Unlock()
	closed := old.BrokerState()
	event := runner.Event{Kind: "recovery_started", BrokerID: closed.BrokerID, IncidentID: closed.IncidentID, Attempt: status.Attempts, Code: "ok"}
	s.runnerJournal.Record(event)
	started := time.Now()
	next, err := old.Reopen(ctx)
	if err == nil && ctx.Err() != nil {
		_ = next.Close()
		err = ctx.Err()
	}
	finished := now.Add(time.Since(started))
	status.Recovering = false
	if err != nil {
		status.Failures++
		status.LastFailureCode = runner.FailureCode(err)
		status.ManualRequired = !errors.Is(err, runner.ErrStoreUnavailable) || errors.Is(err, runner.ErrStoreConflict)
		status.NextAttemptAt = finished.Add(runnerRecoveryBackoff(status.Failures))
		event.Kind = "recovery_failed"
	} else {
		next.SetWarmProfiles(s.runnerWarmProfiles(cfg))
		status.Failures, status.LastFailureCode = 0, ""
		status.LastRecoveredAt = finished
		status.NextAttemptAt = status.LastRecoveredAt.Add(runnerRecoveryBackoff(0))
		event.Kind = "recovery_succeeded"
		event.RelatedBrokerID = next.BrokerState().BrokerID
	}
	s.runnerMu.Lock()
	if err == nil {
		s.runners = next
	}
	s.runnerRecovery = status
	s.runnerMu.Unlock()
	event.Code, event.ManualRequired, event.Recovered = runner.FailureCode(err), status.ManualRequired, err == nil
	event.DurationMS = time.Since(started).Milliseconds()
	s.runnerJournal.Record(event)
	log.Printf("runner_recovery code=%s attempt=%d consecutive_failures=%d manual_required=%t recovered=%t", runner.FailureCode(err), status.Attempts, status.Failures, status.ManualRequired, err == nil)
}
