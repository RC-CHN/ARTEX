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
	ReasonCodes           []string       `json:"reason_codes,omitempty"`
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
		codes := []string{}
		for _, term := range []string{"runner_capacity_unavailable", "lease_expired", "lease_revoked", "session_aborted", "runner lease expired", "controller restarted", "runner boot changed", "agent execution cancelled", "remote tool outcome unknown", "sandbox open failed", "execution_session_lost"} {
			if strings.Contains(s.Reason, term) {
				codes = append(codes, term)
			}
		}
		// Preserve the originating failure before later cleanup/expiry symptoms.
		sort.Slice(codes, func(i, j int) bool { return strings.Index(s.Reason, codes[i]) < strings.Index(s.Reason, codes[j]) })
		if len(codes) > 0 {
			code = codes[0]
		}
		var preparation *AdmissionWait
		if s.Preparation != nil {
			copy := *s.Preparation
			preparation = &copy
		}
		out = append(out, SessionStatus{ID: s.ID, TaskID: s.Spec.TaskID, ThreadID: s.Spec.ThreadID, NodeID: s.NodeID, State: s.State, ReasonCode: code, ReasonCodes: codes, LeaseUntil: s.LeaseUntil, FinishedAt: s.FinishedAt, CommandCount: len(s.Commands), CreatedAt: s.CreatedAt, Preparation: preparation, PreparationReportedAt: s.PreparationReportedAt})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}
