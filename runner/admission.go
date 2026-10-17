package runner

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"time"
)

var diagnosticQuotaName = regexp.MustCompile(`^[a-z0-9.-]{1,253}$`)
var diagnosticQuantity = regexp.MustCompile(`^[0-9]+(?:\.[0-9]+)?(?:[eE][+-]?[0-9]+|[numkKMGTEP]i?)?$`)

func validatePoolStatus(p *PoolStatus) error {
	if p == nil {
		return nil
	}
	invalid := errors.New("invalid pool admission status")
	if p.Idle < 0 || p.Preparing < 0 || p.Pods < 0 || p.CPUMillis < 0 || p.MemoryMiB < 0 || len(p.Error) > 2048 {
		return invalid
	}
	a := p.Admission
	if a == nil {
		return nil
	}
	if a.ObservedAt.IsZero() || len(a.Waiting) > 1024 {
		return invalid
	}
	seen := map[string]bool{}
	for _, w := range a.Waiting {
		if w.SessionID == "" || len(w.SessionID) > 256 || seen[w.SessionID] || w.Attempts < 0 || w.Since.IsZero() {
			return invalid
		}
		switch w.Stage {
		case "pod_capacity", "quota", "pod_create", "pod_ready":
		default:
			return invalid
		}
		seen[w.SessionID] = true
	}
	if q := a.LastQuotaRejection; q != nil {
		if !diagnosticQuotaName.MatchString(q.Name) || q.At.IsZero() {
			return invalid
		}
		for _, quantities := range []map[string]string{q.Requested, q.Used, q.Hard} {
			if len(quantities) > 8 {
				return invalid
			}
			for resource, value := range quantities {
				switch resource {
				case "count/pods", "pods", "limits.cpu", "limits.memory", "limits.ephemeral-storage", "requests.cpu", "requests.memory", "requests.ephemeral-storage":
				default:
					return invalid
				}
				if len(value) > 40 || !diagnosticQuantity.MatchString(value) {
					return invalid
				}
			}
		}
	}
	return nil
}

// Called under Hub.mu. Old long-poll snapshots cannot overwrite newer heartbeat
// observations. Diagnostics never renew leases or release reservations.
func (h *Hub) recordPoolStatusLocked(n *nodeRecord, status *PoolStatus, now time.Time) {
	if status == nil {
		return
	}
	if h.poolStatus == nil {
		h.poolStatus = map[string]PoolStatus{}
	}
	if old := h.poolStatus[n.ID].Admission; old != nil && status.Admission != nil && !status.Admission.ObservedAt.After(old.ObservedAt) {
		return
	}
	b, _ := json.Marshal(status)
	var copy PoolStatus
	_ = json.Unmarshal(b, &copy)
	if copy.Admission == nil {
		// A poll sent before capability negotiation may finish after a new
		// heartbeat. Keep the timestamped observation, without refreshing it.
		copy.Admission = h.poolStatus[n.ID].Admission
		h.poolStatus[n.ID] = copy
		return
	}
	h.poolStatus[n.ID] = copy
	waiting := map[string]AdmissionWait{}
	for _, w := range copy.Admission.Waiting {
		waiting[w.SessionID] = w
	}
	for _, s := range h.state.Sessions {
		if s.NodeID != n.ID || s.BootID != n.BootID || s.Epoch != h.state.Epoch || s.State != "opening" {
			continue
		}
		if w, ok := waiting[s.ID]; ok {
			s.Preparation = &w
			s.PreparationReportedAt = now
		} else if s.Preparation != nil {
			// Start has returned; file/MCP initialization or its result is pending.
			s.Preparation = nil
			s.PreparationReportedAt = now
		}
	}
}

func capacityDeadline(ctx context.Context) error {
	if errors.Is(context.Cause(ctx), context.DeadlineExceeded) {
		return fmt.Errorf("%w: %w", ErrCapacityUnavailable, context.DeadlineExceeded)
	}
	return ctx.Err()
}

// Preserve the exact preparation failure before fencing and asynchronous cleanup.
func (s *Session) cancelOpen(ctx context.Context) error {
	h := s.hub
	h.mu.Lock()
	if h.closed {
		h.mu.Unlock()
		return ErrClosed
	}
	r := h.state.Sessions[s.id]
	err := ctx.Err()
	if r != nil && r.State == "opening" && r.Preparation != nil && h.opts.Clock().Sub(r.PreparationReportedAt) < h.opts.LeaseDuration {
		if r.Preparation.Stage == "quota" || r.Preparation.Stage == "pod_capacity" {
			err = capacityDeadline(ctx)
		}
	}
	if r != nil && r.live() {
		reason := "acquire cancelled"
		if errors.Is(err, ErrCapacityUnavailable) {
			reason = ErrCapacityUnavailable.Error()
		}
		h.fenceLocked(r, reason, false)
		if saveErr := h.saveFenceLocked(); saveErr != nil {
			err = saveErr
		}
	}
	h.mu.Unlock()
	return err
}
