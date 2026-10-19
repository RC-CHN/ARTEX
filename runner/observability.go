package runner

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"sync"
	"time"
)

// A cancellable gate prevents disconnected HTTP requests from accumulating
// behind a slow checkpoint and subsequently performing obsolete renewals.
// Ordinary in-process callers retain the usual Lock/Unlock interface.
type contextMutex struct {
	once sync.Once
	gate chan struct{}
}

func (m *contextMutex) init() { m.once.Do(func() { m.gate = make(chan struct{}, 1) }) }
func (m *contextMutex) Lock() { _ = m.LockContext(context.Background()) }
func (m *contextMutex) Unlock() {
	select {
	case <-m.gate:
	default:
		panic("unlock of unlocked runner mutex")
	}
}
func (m *contextMutex) LockContext(ctx context.Context) error {
	m.init()
	if err := ctx.Err(); err != nil {
		return err
	}
	select {
	case m.gate <- struct{}{}:
		if err := ctx.Err(); err != nil {
			m.Unlock()
			return err
		}
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// FailureCode never incorporates error text, URLs, SQL, or tool data.
func FailureCode(err error) string {
	switch {
	case err == nil:
		return "ok"
	case errors.Is(err, ErrStoreConflict):
		return "store_conflict"
	case errors.Is(err, ErrStoreUnavailable):
		if errors.Is(err, context.DeadlineExceeded) {
			return "store_timeout"
		}
		return "store_unavailable"
	case errors.Is(err, context.DeadlineExceeded):
		return "deadline_exceeded"
	case errors.Is(err, context.Canceled):
		return "canceled"
	case errors.Is(err, ErrClosed):
		return "broker_closed"
	case errors.Is(err, ErrUnauthorized):
		return "unauthorized"
	case errors.Is(err, ErrFenced):
		return "fenced"
	case errors.Is(err, ErrCallConflict):
		return "call_conflict"
	}
	var tooLarge *http.MaxBytesError
	if errors.As(err, &tooLarge) {
		return "request_too_large"
	}
	return "invalid_request"
}

func KnownFailureCode(code string) bool {
	switch code {
	case "store_conflict", "store_timeout", "store_unavailable", "deadline_exceeded", "canceled", "broker_closed", "unauthorized", "fenced", "call_conflict", "request_too_large", "invalid_request":
		return true
	}
	return false
}

func SQLState(err error) string {
	var state interface{ SQLState() string }
	if errors.As(err, &state) {
		code := state.SQLState()
		if len(code) != 5 {
			return ""
		}
		for _, c := range code {
			if !(c >= '0' && c <= '9' || c >= 'A' && c <= 'Z') {
				return ""
			}
		}
		return code
	}
	return ""
}

type OperationStatus struct {
	Calls               uint64    `json:"calls"`
	Failures            uint64    `json:"failures"`
	ConsecutiveFailures uint64    `json:"consecutive_failures"`
	InFlight            int       `json:"in_flight"`
	Waiting             int       `json:"waiting"`
	LastDurationMS      int64     `json:"last_duration_ms"`
	MaxDurationMS       int64     `json:"max_duration_ms"`
	LastLockWaitMS      int64     `json:"last_lock_wait_ms"`
	MaxLockWaitMS       int64     `json:"max_lock_wait_ms"`
	LastBytes           int       `json:"last_bytes,omitempty"`
	LastSuccessAt       time.Time `json:"last_success_at,omitempty"`
	LastFailureAt       time.Time `json:"last_failure_at,omitempty"`
	LastFailureCode     string    `json:"last_failure_code,omitempty"`
	LastSQLState        string    `json:"last_sqlstate,omitempty"`
}

type BrokerDiagnostics struct {
	ObservedAt            time.Time                  `json:"observed_at"`
	Operations            map[string]OperationStatus `json:"operations"`
	LeaseExpirations      uint64                     `json:"lease_expirations"`
	LastLeaseExpirationAt time.Time                  `json:"last_lease_expiration_at,omitempty"`
}

type brokerObserver struct {
	mu      sync.Mutex // Deliberately independent of Hub.mu and the database.
	status  BrokerDiagnostics
	lastLog map[string]time.Time
}

func (h *Hub) Diagnostics() BrokerDiagnostics {
	h.observer.mu.Lock()
	defer h.observer.mu.Unlock()
	out := h.observer.status
	out.ObservedAt = time.Now()
	out.Operations = make(map[string]OperationStatus, len(h.observer.status.Operations))
	for k, v := range h.observer.status.Operations {
		out.Operations[k] = v
	}
	return out
}

type observation struct {
	h       *Hub
	op      string
	start   time.Time
	wait    time.Duration
	waiting bool
}

func (h *Hub) observe(op string, waiting bool) *observation {
	o := &observation{h: h, op: op, start: time.Now(), waiting: waiting}
	h.observer.mu.Lock()
	defer h.observer.mu.Unlock()
	if h.observer.status.Operations == nil {
		h.observer.status.Operations = map[string]OperationStatus{}
		h.observer.lastLog = map[string]time.Time{}
	}
	s := h.observer.status.Operations[op]
	s.Calls++
	s.InFlight++
	if waiting {
		s.Waiting++
	}
	h.observer.status.Operations[op] = s
	return o
}

func (o *observation) acquired() {
	o.wait = time.Since(o.start)
	o.h.observer.mu.Lock()
	s := o.h.observer.status.Operations[o.op]
	s.Waiting--
	o.h.observer.status.Operations[o.op] = s
	o.h.observer.mu.Unlock()
	o.waiting = false
}

func (o *observation) finish(err error, bytes int) {
	now := time.Now()
	elapsed := now.Sub(o.start)
	h := o.h
	h.observer.mu.Lock()
	s := h.observer.status.Operations[o.op]
	s.InFlight--
	if o.waiting {
		s.Waiting--
		o.wait = elapsed
	}
	s.LastDurationMS, s.LastLockWaitMS, s.LastBytes = elapsed.Milliseconds(), o.wait.Milliseconds(), bytes
	s.MaxDurationMS = max(s.MaxDurationMS, s.LastDurationMS)
	s.MaxLockWaitMS = max(s.MaxLockWaitMS, s.LastLockWaitMS)
	recovered := err == nil && s.ConsecutiveFailures > 0
	changed := err != nil && s.LastFailureCode != FailureCode(err)
	if err != nil {
		s.Failures++
		s.ConsecutiveFailures++
		s.LastFailureAt, s.LastFailureCode, s.LastSQLState = now, FailureCode(err), SQLState(err)
	} else {
		s.LastSuccessAt = now
		s.ConsecutiveFailures = 0
	}
	logEvent := recovered || changed || (err != nil && s.ConsecutiveFailures == 1) || ((err != nil || elapsed >= time.Second) && now.Sub(h.observer.lastLog[o.op]) >= 30*time.Second)
	if logEvent {
		h.observer.lastLog[o.op] = now
	}
	h.observer.status.Operations[o.op] = s
	h.observer.mu.Unlock()
	if logEvent && h.opts.Log != nil {
		h.opts.Log.Printf("runner_broker op=%s code=%s sqlstate=%s duration_ms=%d lock_wait_ms=%d bytes=%d consecutive_failures=%d recovered=%t", o.op, FailureCode(err), SQLState(err), s.LastDurationMS, s.LastLockWaitMS, bytes, s.ConsecutiveFailures, recovered)
	}
}

func (h *Hub) lockOperation(ctx context.Context, op string) (*observation, error) {
	o := h.observe(op, true)
	if err := h.mu.LockContext(ctx); err != nil {
		o.finish(err, 0)
		return nil, err
	}
	o.acquired()
	return o, nil
}

func storeError(err error) error {
	if err == nil || errors.Is(err, ErrStoreConflict) {
		return err
	}
	return errors.Join(ErrStoreUnavailable, err)
}

// Keep JSON encoding failures separate from database failures in diagnostics.
func (h *Hub) marshalState() (raw json.RawMessage, err error) {
	o := h.observe("checkpoint_marshal", false)
	defer func() { o.finish(err, len(raw)) }()
	return json.Marshal(h.state)
}
