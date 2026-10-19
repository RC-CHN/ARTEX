package runner

import "time"

// Event is an allowlist of operational metadata. Never add raw errors, request
// bodies, tool data, task descriptions, URLs, or credentials to this structure.
type Event struct {
	At              time.Time          `json:"at"`
	Kind            string             `json:"kind"`
	BrokerID        string             `json:"broker_id,omitempty"`
	RelatedBrokerID string             `json:"related_broker_id,omitempty"`
	IncidentID      string             `json:"incident_id,omitempty"`
	Operation       string             `json:"operation,omitempty"`
	Code            string             `json:"code,omitempty"`
	Reason          string             `json:"reason,omitempty"`
	SQLState        string             `json:"sqlstate,omitempty"`
	Revision        uint64             `json:"revision,omitempty"`
	SessionID       string             `json:"session_id,omitempty"`
	NodeID          string             `json:"node_id,omitempty"`
	DurationMS      int64              `json:"duration_ms,omitempty"`
	LockWaitMS      int64              `json:"lock_wait_ms,omitempty"`
	Bytes           int                `json:"bytes,omitempty"`
	Count           uint64             `json:"count,omitempty"`
	Attempt         uint64             `json:"attempt,omitempty"`
	AutoRecovery    bool               `json:"auto_recovery,omitempty"`
	ManualRequired  bool               `json:"manual_required,omitempty"`
	Recovered       bool               `json:"recovered,omitempty"`
	Diagnostics     *BrokerDiagnostics `json:"diagnostics,omitempty"`
}

func (h *Hub) recordEvent(event Event) {
	if h.opts.RecordEvent == nil {
		return
	}
	if event.At.IsZero() {
		event.At = time.Now()
	}
	event.BrokerID = h.id
	h.opts.RecordEvent(event)
}
