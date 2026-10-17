package runner

import (
	"sort"
	"strings"
	"time"
)

type SessionStatus struct {
	ID                    string         `json:"id"`
	TaskID                string         `json:"task_id"`
	ThreadID              string         `json:"thread_id"`
	NodeID                string         `json:"node_id"`
	State                 string         `json:"state"`
	ReasonCode            string         `json:"reason_code,omitempty"`
	LeaseUntil            time.Time      `json:"lease_until"`
	FinishedAt            time.Time      `json:"finished_at,omitempty"`
	CommandCount          int            `json:"command_count"`
	CreatedAt             time.Time      `json:"created_at,omitempty"`
	Preparation           *AdmissionWait `json:"preparation,omitempty"`
	PreparationReportedAt time.Time      `json:"preparation_reported_at,omitempty"`
}

func (h *Hub) SessionStatuses() []SessionStatus {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.sessionStatusesLocked()
}
func (h *Hub) sessionStatusesLocked() []SessionStatus {
	out := make([]SessionStatus, 0, len(h.state.Sessions))
	for _, s := range h.state.Sessions {
		code := ""
		for _, term := range []string{"runner_capacity_unavailable", "lease_expired", "lease_revoked", "session_aborted", "runner lease expired", "controller restarted", "runner boot changed", "agent execution cancelled", "remote tool outcome unknown", "sandbox open failed", "execution_session_lost"} {
			if strings.Contains(s.Reason, term) {
				code = term
				break
			}
		}
		var preparation *AdmissionWait
		if s.Preparation != nil {
			copy := *s.Preparation
			preparation = &copy
		}
		out = append(out, SessionStatus{ID: s.ID, TaskID: s.Spec.TaskID, ThreadID: s.Spec.ThreadID, NodeID: s.NodeID, State: s.State, ReasonCode: code, LeaseUntil: s.LeaseUntil, FinishedAt: s.FinishedAt, CommandCount: len(s.Commands), CreatedAt: s.CreatedAt, Preparation: preparation, PreparationReportedAt: s.PreparationReportedAt})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}
