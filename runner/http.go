package runner

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"reflect"
	"sort"
	"strings"
	"time"
)

// Handler serves protocol-relative paths. Mount with StripPrefix("/runner/v1",
// hub.Handler()); registration uses its one-time token and every other request
// uses Authorization: Bearer <node credential>.
func (h *Hub) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /register", h.handleRegister)
	mux.HandleFunc("POST /poll", h.handlePoll)
	mux.HandleFunc("POST /heartbeat", h.handleHeartbeat)
	mux.HandleFunc("POST /results", h.handleResult)
	return mux
}

func jsonResponse(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}
func protocolError(w http.ResponseWriter, err error) {
	status := http.StatusBadRequest
	switch {
	case errors.Is(err, errOperationPanic):
		status = http.StatusInternalServerError
	case errors.Is(err, ErrUnauthorized):
		status = http.StatusUnauthorized
	case errors.Is(err, ErrClosed), errors.Is(err, ErrStoreConflict):
		status = http.StatusServiceUnavailable
	case errors.Is(err, ErrStoreUnavailable):
		status = http.StatusServiceUnavailable
	case errors.Is(err, context.DeadlineExceeded):
		status = http.StatusGatewayTimeout
	case errors.Is(err, context.Canceled):
		status = http.StatusRequestTimeout
	case errors.Is(err, ErrFenced):
		status = http.StatusGone
	case errors.Is(err, ErrCallConflict):
		status = http.StatusConflict
	}
	message := err.Error()
	if errors.Is(err, ErrStoreUnavailable) {
		message = ErrStoreUnavailable.Error()
	}
	jsonResponse(w, status, map[string]string{"error": message, "code": FailureCode(err)})
}
func decodeRequest(w http.ResponseWriter, r *http.Request, target any, max int64) error {
	r.Body = http.MaxBytesReader(w, r.Body, max)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(target); err != nil {
		return err
	}
	var extra any
	if err := dec.Decode(&extra); err != io.EOF {
		return errors.New("request must contain one JSON object")
	}
	return nil
}

func validateCapabilities(c Capabilities) error {
	if c.Slots < 1 || c.Slots > 1024 || c.CPUMillis < 0 || c.MemoryMiB < 0 {
		return errors.New("invalid runner capacity")
	}
	seen := map[string]bool{}
	for _, g := range c.GPUs {
		if g.ID == "" || seen[g.ID] || g.MemoryMiB < 0 {
			return errors.New("invalid GPU capacity")
		}
		seen[g.ID] = true
	}
	return nil
}

func (h *Hub) handleRegister(w http.ResponseWriter, r *http.Request) {
	var req RegisterRequest
	if err := decodeRequest(w, r, &req, 1<<20); err != nil {
		protocolError(w, err)
		return
	}
	if req.Protocol != ProtocolVersion || strings.TrimSpace(req.Name) == "" || len(req.Name) > 128 || req.BootID == "" || len(req.BootID) > 256 {
		protocolError(w, errors.New("invalid registration or unsupported protocol"))
		return
	}
	if err := validateCapabilities(req.Capabilities); err != nil {
		protocolError(w, err)
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		protocolError(w, ErrClosed)
		return
	}
	hash := hashToken(req.Token)
	expiry, ok := h.state.Registrations[hash]
	if !ok || !h.opts.Clock().Before(expiry) {
		protocolError(w, ErrUnauthorized)
		return
	}
	delete(h.state.Registrations, hash)
	id, credential := newID(), newID()
	h.state.Nodes[id] = &nodeRecord{ID: id, Name: req.Name, BootID: req.BootID, BootIDs: map[string]bool{req.BootID: true}, CredentialHash: hashToken(credential), Enabled: true, Capabilities: req.Capabilities, LastSeen: h.opts.Clock()}
	if err := h.saveContextLocked(r.Context()); err != nil {
		protocolError(w, err)
		return
	}
	jsonResponse(w, http.StatusCreated, RegisterResponse{NodeID: id, Credential: credential, Epoch: h.state.Epoch, Protocol: ProtocolVersion, LeaseSeconds: int(h.opts.LeaseDuration / time.Second)})
}

func (h *Hub) authenticateLocked(r *http.Request) (string, error) {
	if h.closed {
		return "", ErrClosed
	}
	parts := strings.Fields(r.Header.Get("Authorization"))
	if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") {
		return "", ErrUnauthorized
	}
	hash := hashToken(parts[1])
	for _, n := range h.state.Nodes {
		if subtle.ConstantTimeCompare([]byte(hash), []byte(n.CredentialHash)) == 1 {
			return n.ID, nil
		}
	}
	return "", ErrUnauthorized
}

func (h *Hub) Authenticate(r *http.Request) (id string, err error) {
	op, err := h.lockOperation(r.Context(), "authenticate")
	if err != nil {
		return "", err
	}
	defer func() { h.mu.Unlock(); op.finish(err, 0) }()
	return h.authenticateLocked(r)
}

func (h *Hub) authorizeLocked(r *http.Request, sessionID string, write bool) (*sessionRecord, error) {
	nodeID, err := h.authenticateLocked(r)
	if err != nil {
		return nil, err
	}
	s := h.state.Sessions[sessionID]
	n := h.state.Nodes[nodeID]
	if s == nil || s.NodeID != nodeID {
		return nil, ErrUnauthorized
	}
	if !n.Enabled || s.BootID != n.BootID || s.Epoch != h.state.Epoch || !s.live() || !h.opts.Clock().Before(s.LeaseUntil) {
		return nil, ErrFenced
	}
	// Opening can read its input manifests and download blobs. File writes start
	// after Open ACK; draining retains writes only for the final synchronization.
	if write && s.State == "opening" {
		return nil, ErrFenced
	}
	return s, nil
}

func (h *Hub) AuthorizeSession(r *http.Request, sessionID string, write bool) (spec RunSpec, err error) {
	op, err := h.lockOperation(r.Context(), "file_authorize")
	if err != nil {
		return RunSpec{}, err
	}
	defer func() { h.mu.Unlock(); op.finish(err, 0) }()
	s, err := h.authorizeLocked(r, sessionID, write)
	if err != nil {
		return RunSpec{}, err
	}
	b, _ := json.Marshal(s.Spec)
	var out RunSpec
	_ = json.Unmarshal(b, &out)
	return out, nil
}

// WithSessionWrite closes the authorization-to-rename race. The callback must
// only perform its short atomic commit; read/hash/upload before entering here.
func (h *Hub) WithSessionWrite(r *http.Request, sessionID string, commit func() error) (err error) {
	op, err := h.lockOperation(r.Context(), "file_commit")
	if err != nil {
		return err
	}
	defer func() { h.mu.Unlock(); op.finish(err, 0) }()
	if _, err := h.authorizeLocked(r, sessionID, true); err != nil {
		return err
	}
	if commit == nil {
		return errors.New("missing file commit")
	}
	if guarded, ok := h.opts.Store.(StateGuard); ok {
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		defer cancel()
		err := guarded.Guard(ctx, h.revision, commit)
		if errors.Is(err, ErrStoreConflict) {
			h.closeLocked("store_conflict", err, false)
		}
		return err
	}
	return commit()
}

func (h *Hub) poll(req PollRequest, authenticated string) (PollResponse, <-chan struct{}, error) {
	return h.pollMode(req, authenticated, false)
}

func (h *Hub) pollMode(req PollRequest, authenticated string, heartbeat bool) (PollResponse, <-chan struct{}, error) {
	return h.pollContext(context.Background(), req, authenticated, heartbeat)
}

func (h *Hub) pollContext(ctx context.Context, req PollRequest, authenticated string, heartbeat bool) (out PollResponse, wake <-chan struct{}, err error) {
	name := "poll"
	if heartbeat {
		name = "heartbeat"
	}
	op, err := h.lockOperation(ctx, name)
	if err != nil {
		return PollResponse{}, nil, err
	}
	fencing := false
	defer func() {
		if recover() != nil {
			// A handler panic must not leave a partially renewed node/session or
			// report success. Do not include panic values or request data in logs.
			err = errOperationPanic
			out, wake = PollResponse{}, nil
			restoreErr := h.restoreLocked()
			if restoreErr != nil {
				h.closeLocked("rollback_failed", restoreErr, false)
			} else if fencing {
				// Preserve the failed-fence rule even if persistence panics.
				h.closeLocked("fence_panic", errOperationPanic, false)
			} else {
				h.signalLocked()
			}
			h.recordEvent(Event{Kind: "operation_panic", Operation: name, IncidentID: newID(), Code: "internal_panic", Revision: h.revision, Recovered: restoreErr == nil && !h.closed, ManualRequired: restoreErr != nil || h.closed})
		}
		h.mu.Unlock()
		op.finish(err, 0)
	}()
	if h.closed {
		return PollResponse{}, nil, ErrClosed
	}
	if req.NodeID != authenticated || req.BootID == "" || len(req.BootID) > 256 || req.Protocol != ProtocolVersion {
		return PollResponse{}, nil, ErrUnauthorized
	}
	n := h.state.Nodes[authenticated]
	if n == nil {
		return PollResponse{}, nil, ErrUnauthorized
	}
	// A delayed poll from a previous process must not fence its successor or
	// change the node's boot identity back to an already retired process.
	if n.BootID != req.BootID && n.BootIDs[req.BootID] {
		return PollResponse{}, nil, ErrFenced
	}
	if req.Capabilities != nil {
		if err := validateCapabilities(*req.Capabilities); err != nil {
			return PollResponse{}, nil, err
		}
		if n.BootID == req.BootID && !reflect.DeepEqual(n.Capabilities, *req.Capabilities) {
			return PollResponse{}, nil, errors.New("runner capability changes require a new boot ID")
		}
	}
	if err := validatePoolStatus(req.PoolStatus); err != nil {
		return PollResponse{}, nil, err
	}
	for _, env := range req.Notifications {
		if env.ID == "" || len(env.ID) > 512 || len(env.Items) > 1000 {
			return PollResponse{}, nil, errors.New("invalid notification envelope")
		}
	}
	now := h.opts.Clock()
	changed := h.expireLocked()
	bootChanged := n.BootID != req.BootID
	if bootChanged {
		fencing = true
		delete(h.poolStatus, n.ID)
		for _, s := range h.state.Sessions {
			if s.NodeID == n.ID && s.reserved(now) {
				h.fenceLocked(s, "runner boot changed; previous process outcome may be unknown", true)
			}
		}
		n.BootID = req.BootID
		n.BootIDs[req.BootID] = true
		if req.Capabilities != nil {
			b, _ := json.Marshal(req.Capabilities)
			var caps Capabilities
			_ = json.Unmarshal(b, &caps)
			n.Capabilities = caps
		}
	}
	// Renew durably before expiry, without rewriting retained command results
	// on every delivery of an unfinished Open/Call. File reads share this lock.
	renew := bootChanged || now.Sub(n.LastSeen) >= h.opts.LeaseDuration/3
	out = PollResponse{Heartbeat: true, AdmissionDiagnostics: true, Epoch: h.state.Epoch, ServerTime: now, Leases: []Lease{}, Commands: []Command{}}
	if n.Capabilities.Executor == "kubernetes" {
		if !heartbeat {
			out.Pool = h.poolPolicyLocked(n)
		}
		h.recordPoolStatusLocked(n, req.PoolStatus, now)
	}
	for _, env := range req.Notifications {
		s := h.state.Sessions[env.SessionID]
		if s == nil || s.NodeID != n.ID || s.BootID != req.BootID || s.AttemptID != env.AttemptID || s.Epoch != h.state.Epoch || !s.live() {
			continue
		}
		if !s.NotificationIDs[env.ID] {
			if len(s.NotificationIDs) >= 20000 {
				continue
			}
			if s.NotificationIDs == nil {
				s.NotificationIDs = map[string]bool{}
			}
			s.NotificationIDs[env.ID] = true
			s.Notifications = append(s.Notifications, env.Items...)
			changed = true
		}
		out.AcknowledgedNotifications = append(out.AcknowledgedNotifications, env.ID)
	}
	for _, s := range h.state.Sessions {
		if s.NodeID != n.ID || s.BootID != req.BootID || s.Epoch != h.state.Epoch {
			continue
		}
		if s.State == "closed" || s.State == "expired" {
			continue
		}
		if n.Enabled && s.live() {
			// Node liveness cannot substitute for this session's own lease.
			// Repair a lagging lease even when the node was seen recently.
			if renew || s.LeaseUntil.Sub(now) <= h.opts.LeaseDuration*2/3 {
				s.LeaseUntil = now.Add(h.opts.LeaseDuration)
				changed = true
			}
			out.Leases = append(out.Leases, Lease{SessionID: s.ID, AttemptID: s.AttemptID, Epoch: s.Epoch, Until: s.LeaseUntil})
		}
		for _, c := range s.Commands {
			if heartbeat {
				break
			}
			if c.Result != nil {
				continue
			}
			if c.Command.Kind == CommandOpen && s.State != "opening" {
				continue
			}
			if c.Command.Kind == CommandCall && s.State != "active" {
				continue
			}
			if c.Command.Kind == CommandClose && s.State != "draining" && s.State != "fenced" {
				continue
			}
			if now.Before(h.redeliver[c.Command.ID]) {
				continue
			}
			cmd := c.Command
			cmd.LeaseUntil = s.LeaseUntil
			out.Commands = append(out.Commands, cmd)
		}
	}
	if renew {
		n.LastSeen = now
		changed = true
	}
	if bootChanged {
		err = h.saveFenceContextLocked(ctx)
	} else if changed {
		err = h.saveContextLocked(ctx)
	} else {
		// Still fence a superseded controller before returning any lease. A
		// revision check is cheap and does not rewrite historical tool output.
		ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		err = h.observeCall("store_guard", 0, func() error {
			if guard, ok := h.opts.Store.(StateGuard); ok {
				return storeError(guard.Guard(ctx, h.revision, func() error { return nil }))
			}
			snapshot, loadErr := h.opts.Store.Load(ctx)
			if loadErr == nil && snapshot.Revision != h.revision {
				return ErrStoreConflict
			}
			return storeError(loadErr)
		})
		if errors.Is(err, ErrStoreConflict) {
			h.closeLocked("store_conflict", err, false)
		}
	}
	if err != nil {
		return PollResponse{}, nil, err
	}
	// A lost response is retried; a running command must not make the node
	// continuously poll and serialize the entire persistent state.
	for _, cmd := range out.Commands {
		h.redeliver[cmd.ID] = now.Add(min(time.Second, h.opts.LeaseDuration/6))
	}
	sort.Slice(out.Commands, func(i, j int) bool { return out.Commands[i].ID < out.Commands[j].ID })
	// Detach all nested maps/pointers before unlocking and encoding the response.
	b, _ := json.Marshal(out)
	var detached PollResponse
	_ = json.Unmarshal(b, &detached)
	return detached, h.wake, nil
}

func (h *Hub) handlePoll(w http.ResponseWriter, r *http.Request) {
	id, err := h.Authenticate(r)
	if err != nil {
		protocolError(w, err)
		return
	}
	var req PollRequest
	if err := decodeRequest(w, r, &req, 4<<20); err != nil {
		protocolError(w, err)
		return
	}
	out, wake, err := h.pollContext(r.Context(), req, id, false)
	if err != nil {
		protocolError(w, err)
		return
	}
	if len(out.Commands) == 0 && len(out.AcknowledgedNotifications) == 0 {
		timer := time.NewTimer(h.opts.PollWait)
		defer timer.Stop()
		select {
		case <-r.Context().Done():
			return
		case <-timer.C:
		case <-wake:
		}
		out, _, err = h.pollContext(r.Context(), req, id, false)
		if err != nil {
			protocolError(w, err)
			return
		}
	}
	jsonResponse(w, http.StatusOK, out)
}

func validateMCPTools(s *sessionRecord, defs []ToolDefinition) error {
	seen := map[string]bool{}
	for _, t := range defs {
		ok := false
		for _, m := range s.Spec.MCPs {
			if strings.HasPrefix(t.Name, "mcp__"+m.Name+"__") && len(t.Name) > len(m.Name)+7 {
				ok = true
			}
		}
		if !ok || seen[t.Name] {
			return fmt.Errorf("unexpected MCP tool %q", t.Name)
		}
		for _, builtin := range s.Spec.Tools {
			if t.Name == builtin.Name {
				return fmt.Errorf("MCP tool %q shadows a declared tool", t.Name)
			}
		}
		seen[t.Name] = true
	}
	return nil
}

func (h *Hub) acceptResult(req ResultRequest, authenticated string) error {
	return h.acceptResultContext(context.Background(), req, authenticated)
}

func (h *Hub) acceptResultContext(ctx context.Context, req ResultRequest, authenticated string) (err error) {
	op, err := h.lockOperation(ctx, "result")
	if err != nil {
		return err
	}
	defer func() { h.mu.Unlock(); op.finish(err, 0) }()
	if h.closed {
		return ErrClosed
	}
	if req.NodeID != authenticated {
		return ErrUnauthorized
	}
	s := h.state.Sessions[req.SessionID]
	n := h.state.Nodes[authenticated]
	if s == nil || n == nil || s.NodeID != authenticated {
		return ErrUnauthorized
	}
	if req.BootID != n.BootID || req.BootID != s.BootID || req.Epoch != s.Epoch || req.Epoch != h.state.Epoch || req.AttemptID != s.AttemptID {
		return ErrFenced
	}
	c := s.Commands[req.CommandID]
	if c == nil {
		return errors.New("unknown command")
	}
	if c.Result != nil {
		if !reflect.DeepEqual(*c.Result, req) {
			return ErrCallConflict
		}
		if !s.live() && c.Command.Kind != CommandClose {
			return ErrFenced
		}
		return nil
	}
	if !h.opts.Clock().Before(s.LeaseUntil) || s.State == "expired" || s.State == "closed" {
		return ErrFenced
	}
	switch c.Command.Kind {
	case CommandOpen:
		if s.State != "opening" {
			return ErrFenced
		}
		if err := validateMCPTools(s, req.MCPTools); err != nil {
			return err
		}
		if req.Error != "" || req.UnknownOutcome {
			h.fenceLocked(s, "sandbox open failed: "+req.Error, false)
		} else {
			s.State = "active"
			s.MCPTools = req.MCPTools
		}
	case CommandCall:
		if s.State != "active" {
			return ErrFenced
		}
		if req.UnknownOutcome {
			h.fenceLocked(s, "remote tool outcome unknown: "+req.Error, false)
		} else if req.Error == "session stopped" || req.Error == "unknown session or missing tool call" {
			h.fenceLocked(s, "execution_session_lost: runner rejected call before execution", false)
		}
	case CommandClose:
		if s.State != "draining" && s.State != "fenced" {
			return ErrFenced
		}
		if req.UnknownOutcome {
			h.fenceLocked(s, "remote process cleanup unconfirmed: "+req.Error, true)
		} else if !s.HoldUntilExpiry {
			s.State = "closed"
			s.FinishedAt = h.opts.Clock()
			if s.Reason == "" {
				s.addReason(req.Error)
			} else if req.Error != "" && req.Error != s.Reason {
				s.addReason("cleanup: " + req.Error)
			}
		}
	default:
		return errors.New("unsupported command kind")
	}
	c.Result = &req
	if s.State == "active" {
		s.Notifications = append(s.Notifications, req.Notifications...)
	}
	if s.State == "fenced" {
		return h.saveFenceContextLocked(ctx)
	}
	return h.saveContextLocked(ctx)
}

func (h *Hub) handleResult(w http.ResponseWriter, r *http.Request) {
	id, err := h.Authenticate(r)
	if err != nil {
		protocolError(w, err)
		return
	}
	var req ResultRequest
	if err := decodeRequest(w, r, &req, 32<<20); err != nil {
		protocolError(w, err)
		return
	}
	if err := h.acceptResultContext(r.Context(), req, id); err != nil {
		protocolError(w, err)
		return
	}
	jsonResponse(w, http.StatusOK, ResultResponse{Accepted: true})
}
