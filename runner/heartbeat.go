package runner

import (
	"errors"
	"net/http"
)

func (h *Hub) handleHeartbeat(w http.ResponseWriter, r *http.Request) {
	id, err := h.Authenticate(r)
	if err != nil {
		protocolError(w, err)
		return
	}
	var req HeartbeatRequest
	if err = decodeRequest(w, r, &req, 1<<20); err != nil {
		protocolError(w, err)
		return
	}
	if req.NodeID != id || req.Protocol != ProtocolVersion || len(req.Stopped) > 1024 {
		protocolError(w, ErrUnauthorized)
		return
	}
	if err = h.recordStopped(req, id); err != nil {
		protocolError(w, err)
		return
	}
	out, _, err := h.pollMode(PollRequest{NodeID: req.NodeID, BootID: req.BootID, Protocol: req.Protocol}, id, true)
	if err != nil {
		protocolError(w, err)
		return
	}
	jsonResponse(w, http.StatusOK, out)
}

func (h *Hub) recordStopped(req HeartbeatRequest, id string) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return ErrClosed
	}
	n := h.state.Nodes[id]
	if n == nil || req.BootID != n.BootID {
		return ErrUnauthorized
	}
	// Validate the entire batch before changing anything.
	for _, stop := range req.Stopped {
		switch stop.Reason {
		case "lease_expired", "lease_revoked", "session_aborted":
		default:
			return errors.New("invalid session stop reason")
		}
	}
	changed := false
	for _, stop := range req.Stopped {
		s := h.state.Sessions[stop.SessionID]
		if s == nil || s.NodeID != id || s.BootID != req.BootID || s.AttemptID != stop.AttemptID || s.Epoch != stop.Epoch || s.Epoch != h.state.Epoch {
			continue
		}
		if s.State != "opening" && s.State != "active" {
			continue
		}
		h.fenceLocked(s, "execution_session_lost: "+stop.Reason, false)
		changed = true
	}
	if changed {
		return h.saveFenceLocked()
	}
	return nil
}
