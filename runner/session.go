package runner

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"time"

	"github.com/RC-CHN/ARTEX/norma/tool"
	"github.com/RC-CHN/ARTEX/toolenv"
)

type Session struct {
	hub                   *Hub
	id, attemptID, nodeID string
	environmentPrompt     string
}

func (s *Session) ID() string                { return s.id }
func (s *Session) AttemptID() string         { return s.attemptID }
func (s *Session) NodeID() string            { return s.nodeID }
func (s *Session) EnvironmentPrompt() string { return s.environmentPrompt }

// OutcomeError deliberately remains distinguishable from a normal tool error.
// The caller must reconcile this attempt, not automatically replay the command.
type OutcomeError struct{ Cause error }

func (e *OutcomeError) Error() string {
	if e.Cause == nil {
		return ErrUnknownOutcome.Error()
	}
	return ErrUnknownOutcome.Error() + ": " + e.Cause.Error()
}
func (e *OutcomeError) Unwrap() error        { return e.Cause }
func (e *OutcomeError) Is(target error) bool { return target == ErrUnknownOutcome }
func (e *OutcomeError) FatalExecution() bool { return true }

func validateSpec(spec RunSpec) error {
	if strings.TrimSpace(spec.ThreadID) == "" || len(spec.ThreadID) > 256 {
		return errors.New("thread ID is required (maximum 256 bytes)")
	}
	if spec.WorkingDir == "" {
		return errors.New("sandbox working directory is required")
	}
	r := spec.Resources
	if r.CPUMillis < 0 || r.MemoryMiB < 0 || r.GPUCount < 0 || r.GPUMemoryMiB < 0 || r.PIDs < 0 {
		return errors.New("negative resource request")
	}
	seen := map[string]bool{}
	for _, t := range spec.Tools {
		if t.Name == "" || seen[t.Name] {
			return errors.New("empty or duplicate tool name")
		}
		seen[t.Name] = true
	}
	seen = map[string]bool{}
	for _, m := range spec.MCPs {
		if m.Name == "" || strings.Contains(m.Name, "__") || seen[m.Name] {
			return errors.New("invalid or duplicate MCP name")
		}
		seen[m.Name] = true
	}
	return nil
}

// Acquire reserves a complete remote execution session. Waiting for capacity
// does not claim an intent on the node or rotate individual tools between nodes.
// Cancellation of ctx fences the session even after Acquire has returned.
func (h *Hub) Acquire(ctx context.Context, spec RunSpec) (*Session, error) {
	if err := validateSpec(spec); err != nil {
		return nil, err
	}
	// Match the executor's minimum default limits before reserving capacity;
	// an omitted request must not count as a resource-free sandbox.
	if spec.Resources.CPUMillis == 0 {
		spec.Resources.CPUMillis = 1000
	}
	if spec.Resources.MemoryMiB == 0 {
		spec.Resources.MemoryMiB = 1024
	}
	if spec.Resources.PIDs == 0 {
		spec.Resources.PIDs = 256
	}
	raw, err := json.Marshal(spec)
	if err != nil {
		return nil, err
	}
	var detachedSpec RunSpec
	if err = json.Unmarshal(raw, &detachedSpec); err != nil {
		return nil, err
	}
	spec = detachedSpec
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		h.mu.Lock()
		if h.closed {
			h.mu.Unlock()
			return nil, ErrClosed
		}
		if h.expireLocked() {
			if err := h.saveLocked(); err != nil {
				h.mu.Unlock()
				return nil, err
			}
		}
		for _, s := range h.state.Sessions {
			if s.reserved(h.opts.Clock()) && incompatibleFileModes(spec.Files, s.Spec.Files) {
				h.mu.Unlock()
				return nil, errors.New("overlapping workspace has an active snapshot/shared execution; wait for cleanup before switching file mode")
			}
			if s.Spec.ThreadID == spec.ThreadID && s.reserved(h.opts.Clock()) {
				h.mu.Unlock()
				return nil, ErrThreadBusy
			}
		}
		n, gpus := h.chooseNodeLocked(spec)
		if n == nil {
			wake := h.wake
			h.mu.Unlock()
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-wake:
				continue
			}
		}
		id, attempt, commandID := newID(), newID(), newID()
		spec.ConcurrencyLimit = n.effectiveSlots()
		spec.PoolCPUMillis, spec.PoolMemoryMiB = n.effectiveCPU(), n.effectiveMemory()
		r := &sessionRecord{ID: id, AttemptID: attempt, NodeID: n.ID, BootID: n.BootID, Epoch: h.state.Epoch, State: "opening", Spec: spec, GPUIds: gpus, LeaseUntil: h.opts.Clock().Add(h.opts.LeaseDuration), Commands: map[string]*commandRecord{}, CallIDs: map[string]string{}, NotificationIDs: map[string]bool{}}
		r.Commands[commandID] = &commandRecord{Command: Command{ID: commandID, Kind: CommandOpen, SessionID: id, AttemptID: attempt, Epoch: r.Epoch, LeaseUntil: r.LeaseUntil, Spec: &r.Spec, GPUIds: gpus}}
		h.state.Sessions[id] = r
		if err := h.saveLocked(); err != nil {
			h.mu.Unlock()
			return nil, err
		}
		s := &Session{hub: h, id: id, attemptID: attempt, nodeID: n.ID, environmentPrompt: n.EnvironmentPrompt}
		h.mu.Unlock()
		if err := s.waitOpen(ctx); err != nil {
			return nil, err
		}
		go s.watchContext(ctx)
		return s, nil
	}
}

func (s *Session) waitOpen(ctx context.Context) error {
	h := s.hub
	for {
		h.mu.Lock()
		r := h.state.Sessions[s.id]
		if h.closed {
			h.mu.Unlock()
			return ErrClosed
		}
		if r == nil || !r.live() {
			var reason string
			if r != nil {
				reason = r.Reason
			}
			h.mu.Unlock()
			return fmt.Errorf("remote sandbox did not open: %s: %w", reason, ErrFenced)
		}
		if r.State == "active" {
			h.mu.Unlock()
			return nil
		}
		wake := h.wake
		h.mu.Unlock()
		select {
		case <-ctx.Done():
			_ = h.abort(s.id, "acquire cancelled")
			return ctx.Err()
		case <-wake:
		}
	}
}

func (s *Session) watchContext(ctx context.Context) {
	h := s.hub
	for {
		h.mu.Lock()
		r := h.state.Sessions[s.id]
		if h.closed || r == nil || !r.live() {
			h.mu.Unlock()
			return
		}
		wake := h.wake
		h.mu.Unlock()
		select {
		case <-ctx.Done():
			_ = h.abort(s.id, "agent execution cancelled; unsynchronized output may be lost")
			return
		case <-wake:
		}
	}
}

func allowedTool(r *sessionRecord, name string) bool {
	for _, t := range r.Spec.Tools {
		if t.Name == name {
			return true
		}
	}
	for _, t := range r.MCPTools {
		if t.Name == name {
			return true
		}
	}
	return false
}

func (s *Session) Handles(name string) bool {
	s.hub.mu.Lock()
	defer s.hub.mu.Unlock()
	r := s.hub.state.Sessions[s.id]
	return r != nil && allowedTool(r, name)
}

func canonicalInput(in json.RawMessage) (json.RawMessage, error) {
	var v any
	if len(in) == 0 {
		in = json.RawMessage(`{}`)
	}
	if !json.Valid(in) {
		return nil, errors.New("tool input must be one JSON value")
	}
	dec := json.NewDecoder(strings.NewReader(string(in)))
	dec.UseNumber()
	if err := dec.Decode(&v); err != nil {
		return nil, err
	}
	return json.Marshal(v)
}

func (s *Session) Call(ctx context.Context, name string, input json.RawMessage, tc *tool.ToolContext) (tool.Result, error) {
	if err := ctx.Err(); err != nil {
		return tool.Result{}, err
	}
	in, err := canonicalInput(input)
	if err != nil {
		return tool.Result{}, err
	}
	call := ToolCall{ID: newID(), Name: name, Input: in}
	if tc != nil {
		if tc.ToolUseID != "" {
			call.ID = tc.ToolUseID
		}
		call.WorkingDir = tc.WorkingDir
		call.OutputDir = tc.OutputDir
		call.MaxOutputChars = tc.MaxOutputChars
		call.Env = append([]string(nil), tc.Env...)
	}
	if len(call.ID) > 512 {
		return tool.Result{}, errors.New("tool call ID is too long")
	}
	if deadline, ok := ctx.Deadline(); ok {
		call.Deadline = deadline
	}
	h := s.hub
	h.mu.Lock()
	r := h.state.Sessions[s.id]
	if h.closed {
		h.mu.Unlock()
		return tool.Result{}, &OutcomeError{Cause: ErrClosed}
	}
	if r == nil || r.State != "active" || !h.opts.Clock().Before(r.LeaseUntil) {
		h.mu.Unlock()
		return tool.Result{}, &OutcomeError{Cause: ErrFenced}
	}
	if !allowedTool(r, name) {
		h.mu.Unlock()
		return tool.Result{}, fmt.Errorf("tool %q is not permitted in this remote session", name)
	}
	commandID := r.CallIDs[call.ID]
	if commandID != "" {
		old := *r.Commands[commandID].Command.Call
		old.Deadline, call.Deadline = time.Time{}, time.Time{}
		if !reflect.DeepEqual(old, call) {
			h.mu.Unlock()
			return tool.Result{}, ErrCallConflict
		}
	} else {
		if len(r.CallIDs) >= 20000 {
			h.mu.Unlock()
			return tool.Result{}, errors.New("remote session tool-call limit reached")
		}
		commandID = newID()
		r.CallIDs[call.ID] = commandID
		r.Commands[commandID] = &commandRecord{Command: Command{ID: commandID, Kind: CommandCall, SessionID: r.ID, AttemptID: r.AttemptID, Epoch: r.Epoch, LeaseUntil: r.LeaseUntil, Call: &call}}
		if err := h.saveLocked(); err != nil {
			h.mu.Unlock()
			return tool.Result{}, err
		}
	}
	h.mu.Unlock()
	for {
		h.mu.Lock()
		r = h.state.Sessions[s.id]
		if r == nil {
			h.mu.Unlock()
			return tool.Result{}, &OutcomeError{Cause: ErrFenced}
		}
		// An acknowledged result is authoritative even if receiving it also
		// fenced the attempt (for example an artifact conflict after execution).
		// Preserve that concrete reason instead of replacing it with ErrFenced.
		if result := r.Commands[commandID].Result; result != nil {
			b, _ := json.Marshal(result)
			var out ResultRequest
			_ = json.Unmarshal(b, &out)
			h.mu.Unlock()
			if out.UnknownOutcome {
				return tool.Result{}, &OutcomeError{Cause: errors.New(out.Error)}
			}
			if out.Error != "" {
				return tool.Result{}, errors.New(out.Error)
			}
			return out.Result, nil
		}
		if h.closed {
			h.mu.Unlock()
			return tool.Result{}, &OutcomeError{Cause: ErrClosed}
		}
		if r.State != "active" || !h.opts.Clock().Before(r.LeaseUntil) {
			h.mu.Unlock()
			return tool.Result{}, &OutcomeError{Cause: ErrFenced}
		}
		wake := h.wake
		h.mu.Unlock()
		select {
		case <-ctx.Done():
			_ = h.abort(s.id, "tool call cancelled; execution outcome and unsynchronized output may be unknown")
			return tool.Result{}, &OutcomeError{Cause: ctx.Err()}
		case <-wake:
		}
	}
}

func (s *Session) DrainNotifications() []tool.Notification {
	h := s.hub
	h.mu.Lock()
	defer h.mu.Unlock()
	r := h.state.Sessions[s.id]
	if h.closed || r == nil || r.State != "active" || len(r.Notifications) == 0 {
		return nil
	}
	out := append([]tool.Notification(nil), r.Notifications...)
	r.Notifications = nil
	if err := h.saveLocked(); err != nil {
		return nil
	}
	return out
}

func (s *Session) MCPTools(ctx context.Context) ([]ToolDefinition, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	h := s.hub
	h.mu.Lock()
	defer h.mu.Unlock()
	r := h.state.Sessions[s.id]
	if h.closed || r == nil || r.State != "active" {
		return nil, ErrFenced
	}
	b, _ := json.Marshal(r.MCPTools)
	var out []ToolDefinition
	_ = json.Unmarshal(b, &out)
	return out, nil
}

func (s *Session) EnvironmentTools() toolenv.Inventory {
	s.hub.mu.Lock()
	defer s.hub.mu.Unlock()
	r := s.hub.state.Sessions[s.id]
	if r == nil {
		return toolenv.Inventory{}
	}
	for _, command := range r.Commands {
		if command.Command.Kind == CommandOpen && command.Result != nil {
			b, _ := json.Marshal(command.Result.EnvironmentTools)
			var out toolenv.Inventory
			_ = json.Unmarshal(b, &out)
			return out
		}
	}
	return toolenv.Inventory{}
}

// Close drains final filesystem updates before acknowledging closure. Cancellation
// uses abort instead and immediately fences writes. Both retain reservations until
// the node confirms process cleanup or the old lease expires.
func (s *Session) Close() error {
	h := s.hub
	h.mu.Lock()
	r := h.state.Sessions[s.id]
	if h.closed {
		h.mu.Unlock()
		return ErrClosed
	}
	if r == nil || r.State == "closed" {
		var err error
		if r != nil && r.Reason != "" {
			err = errors.New(r.Reason)
		}
		h.mu.Unlock()
		return err
	}
	if !r.live() {
		err := fmt.Errorf("%w: %s", ErrFenced, r.Reason)
		h.mu.Unlock()
		return &OutcomeError{Cause: err}
	}
	if r.State != "draining" {
		r.State = "draining"
		id := newID()
		r.Commands[id] = &commandRecord{Command: Command{ID: id, Kind: CommandClose, SessionID: r.ID, AttemptID: r.AttemptID, Epoch: r.Epoch, LeaseUntil: r.LeaseUntil, Reason: "session completed"}}
		if err := h.saveLocked(); err != nil {
			h.mu.Unlock()
			return err
		}
	}
	h.mu.Unlock()
	for {
		h.mu.Lock()
		r = h.state.Sessions[s.id]
		if r != nil && r.State == "closed" {
			reason := r.Reason
			h.mu.Unlock()
			if reason != "" {
				return errors.New(reason)
			}
			return nil
		}
		if h.closed || r == nil || !r.live() || !h.opts.Clock().Before(r.LeaseUntil) {
			err := ErrFenced
			if r != nil && r.Reason != "" {
				err = fmt.Errorf("%w: %s", ErrFenced, r.Reason)
			}
			h.mu.Unlock()
			return &OutcomeError{Cause: err}
		}
		wake := h.wake
		h.mu.Unlock()
		// Final artifacts may take longer than one heartbeat interval. Continue
		// while the node renews this drain lease; the owning Acquire context,
		// lease watchdog, and Hub.Close still interrupt it immediately.
		<-wake
	}
}
