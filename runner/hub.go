package runner

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

type Hub struct {
	id              string // immutable identity, also used as the controller epoch
	mu              contextMutex
	observer        brokerObserver
	opts            Options
	state           persistedState
	revision        uint64
	lastSaved       json.RawMessage
	durableLeases   map[string]time.Time // last committed, never the pending renewal
	durableNodeSeen map[string]time.Time
	metadataOverlay bool // committed lease metadata newer than lastSaved
	wake            chan struct{}
	stop            chan struct{}
	done            chan struct{}
	stopOnce        sync.Once
	closed          bool
	reopenMu        sync.Mutex
	reopened        bool
	redeliver       map[string]time.Time // transport hints only; never authorize execution
	warmProfiles    []WarmProfile
	poolStatus      map[string]PoolStatus
}

func New(opts Options) (*Hub, error) {
	return NewContext(context.Background(), opts)
}

func NewContext(parent context.Context, opts Options) (*Hub, error) {
	return newHub(parent, opts, "")
}

func newHub(parent context.Context, opts Options, predecessorEpoch string) (*Hub, error) {
	if opts.Store == nil {
		return nil, errors.New("runner StateStore is required")
	}
	if opts.Clock == nil {
		opts.Clock = time.Now
	}
	if opts.LeaseDuration == 0 {
		opts.LeaseDuration = 30 * time.Second
	}
	if opts.LeaseDuration < time.Second || opts.LeaseDuration > 2*time.Minute {
		return nil, errors.New("runner lease must be between one second and two minutes")
	}
	if opts.RegistrationTTL == 0 {
		opts.RegistrationTTL = 10 * time.Minute
	}
	if opts.RegistrationTTL <= 0 || opts.RegistrationTTL > time.Hour {
		return nil, errors.New("registration TTL must be within one hour")
	}
	if opts.PollWait == 0 {
		opts.PollWait = 10 * time.Second
	}
	if opts.PollWait < 0 {
		return nil, errors.New("negative poll wait")
	}
	if opts.PollWait > opts.LeaseDuration/3 {
		opts.PollWait = opts.LeaseDuration / 3
	}
	ctx, cancel := context.WithTimeout(parent, 5*time.Second)
	defer cancel()
	snap, err := opts.Store.Load(ctx)
	if err != nil {
		return nil, storeError(err)
	}
	h := &Hub{opts: opts, revision: snap.Revision, lastSaved: append(json.RawMessage(nil), snap.Data...), wake: make(chan struct{}), stop: make(chan struct{}), done: make(chan struct{}), redeliver: map[string]time.Time{}}
	if h.state, err = decodeState(snap.Data); err != nil {
		return nil, fmt.Errorf("decode runner state: %w", err)
	}
	// Recovery may observe a newer revision from an ambiguously committed write,
	// but it must never fence a different controller that took over meanwhile.
	if predecessorEpoch != "" && h.state.Epoch != predecessorEpoch {
		return nil, ErrStoreConflict
	}
	if h.state.Version != 0 && h.state.Version != ProtocolVersion {
		return nil, errors.New("unsupported persisted runner state version")
	}
	h.state.Version = ProtocolVersion
	h.state.Epoch = newID()
	h.id = h.state.Epoch
	h.observer.status.State.BrokerID = h.id
	// A controller restart does not prove that an old remote process has died.
	// Fence its writes immediately, but retain all reservations until its old lease
	// expires. Fresh polls cannot extend a predecessor's lease.
	for _, s := range h.state.Sessions {
		if s.State != "closed" && s.State != "expired" {
			s.State, s.HoldUntilExpiry = "fenced", true
			s.addReason("controller restarted; previous execution outcome may be unknown")
		}
	}
	h.expireLocked()
	if err := h.saveContextLocked(parent); err != nil {
		return nil, err
	}
	if err := parent.Err(); err != nil {
		return nil, err
	}
	h.recordEvent(Event{Kind: "broker_started", Revision: h.revision})
	go h.sweepLoop()
	return h, nil
}

func newID() string {
	var b [24]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic("crypto/rand unavailable: " + err.Error())
	}
	return hex.EncodeToString(b[:])
}
func hashToken(s string) string { v := sha256.Sum256([]byte(s)); return hex.EncodeToString(v[:]) }

func (h *Hub) signalLocked() { close(h.wake); h.wake = make(chan struct{}) }

// Every visible mutation is durably saved before commands or leases are exposed.
// Roll back on failure; a CAS conflict permanently fences this Hub instance.
func (h *Hub) saveLocked() error { return h.saveContextLocked(context.Background()) }

func (h *Hub) saveContextLocked(parent context.Context) (err error) {
	op := h.observe("checkpoint", false)
	var size int
	defer op.finishDeferred(&err, &size)
	ctx, cancel := context.WithTimeout(parent, h.checkpointTimeoutLocked())
	defer cancel()
	raw, err := h.marshalState()
	size = len(raw)
	if err != nil {
		return err
	}
	var rev uint64
	err = h.observeCall("checkpoint_store", size, func() error {
		if store, ok := h.opts.Store.(DiagnosticStore); ok {
			rev, err = store.SaveDiagnostic(ctx, h.revision, raw, h.sessionStatusesLocked())
		} else {
			rev, err = h.opts.Store.Save(ctx, h.revision, raw)
		}
		return storeError(err)
	})
	if err != nil {
		if len(h.lastSaved) > 0 {
			if restoreErr := h.restoreLocked(); restoreErr != nil {
				h.closeLocked("rollback_failed", restoreErr, false)
			}
		}
		if errors.Is(err, ErrStoreConflict) {
			h.closeLocked("store_conflict", err, false)
		}
		h.signalLocked()
		return err
	}
	h.revision, h.lastSaved = rev, raw
	h.metadataOverlay = false
	h.rememberLeaseMetadataLocked()
	h.signalLocked()
	return nil
}

func (h *Hub) rememberLeaseMetadataLocked() {
	h.durableLeases = make(map[string]time.Time)
	h.durableNodeSeen = make(map[string]time.Time)
	for id, n := range h.state.Nodes {
		h.durableNodeSeen[id] = n.LastSeen
	}
	for id, s := range h.state.Sessions {
		if s.live() {
			h.durableLeases[id] = s.LeaseUntil
		}
	}
}

func (h *Hub) checkpointTimeoutLocked() time.Duration {
	budget := 5 * time.Second
	if store, ok := h.opts.Store.(ProgressiveStore); ok {
		budget = min(2*time.Minute, max(budget, store.CheckpointTimeout()))
	}
	now := h.opts.Clock()
	for id, until := range h.durableLeases {
		if s := h.state.Sessions[id]; s != nil && s.live() {
			// Leave time for another heartbeat and delivery. Even a pending
			// renewal cannot grant more time until its transaction commits.
			budget = min(budget, max(100*time.Millisecond, until.Sub(now)-h.opts.LeaseDuration/6))
		}
	}
	return budget
}

// Caller holds h.mu. Publish only a fully decoded, writable checkpoint.
func (h *Hub) restoreLocked() error {
	return h.observeCall("checkpoint_rollback", len(h.lastSaved), func() error {
		if len(h.lastSaved) == 0 {
			return errors.New("missing saved runner state")
		}
		state, err := decodeState(h.lastSaved)
		if err == nil {
			if h.metadataOverlay {
				for id, seen := range h.durableNodeSeen {
					if n := state.Nodes[id]; n != nil {
						n.LastSeen = seen
					}
				}
				for id, until := range h.durableLeases {
					if s := state.Sessions[id]; s != nil {
						s.LeaseUntil = until
					}
				}
			}
			h.state = state
		}
		return err
	})
}

// A failed durable cancellation must never resurrect the in-memory attempt.
// Refuse all further leases and file access until the controller is reopened;
// reopening fences the persisted predecessor and waits out its last lease.
func (h *Hub) saveFenceLocked() error {
	return h.saveFenceContextLocked(context.Background())
}

func (h *Hub) saveFenceContextLocked(ctx context.Context) error {
	err := h.saveContextLocked(ctx)
	if err != nil {
		h.closeLocked("fence_persistence_failed", err, errors.Is(err, ErrStoreUnavailable))
	}
	return err
}

func (h *Hub) expireLocked() bool {
	now := h.opts.Clock()
	changed := false
	for hash, expiry := range h.state.Registrations {
		if !now.Before(expiry) {
			delete(h.state.Registrations, hash)
			changed = true
		}
	}
	for id, s := range h.state.Sessions {
		if s.State != "closed" && s.State != "expired" && !now.Before(s.LeaseUntil) {
			h.observer.mu.Lock()
			h.observer.status.LeaseExpirations++
			h.observer.status.LastLeaseExpirationAt = now
			h.observer.mu.Unlock()
			if h.opts.Log != nil {
				h.opts.Log.Printf("runner_broker op=lease_expired session=%q node=%q overdue_ms=%d", s.ID, s.NodeID, now.Sub(s.LeaseUntil).Milliseconds())
			}
			h.recordEvent(Event{Kind: "lease_expired", Code: "lease_expired", SessionID: s.ID, NodeID: s.NodeID, DurationMS: now.Sub(s.LeaseUntil).Milliseconds()})
			s.State, s.FinishedAt = "expired", now
			s.addReason("runner lease expired; remote outcome may be unknown")
			changed = true
		}
		if !s.FinishedAt.IsZero() && now.Sub(s.FinishedAt) > 24*time.Hour {
			for id := range s.Commands {
				delete(h.redeliver, id)
			}
			delete(h.state.Sessions, id)
			changed = true
		}
	}
	return changed
}

func (h *Hub) sweepLoop() {
	defer close(h.done)
	tick := time.NewTicker(250 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case <-h.stop:
			return
		case <-tick.C:
			h.mu.Lock()
			if !h.closed && h.expireLocked() {
				_ = h.saveLocked()
			}
			h.mu.Unlock()
		}
	}
}

func (h *Hub) CreateRegistrationToken(ctx context.Context, ttl time.Duration) (string, time.Time, error) {
	if err := ctx.Err(); err != nil {
		return "", time.Time{}, err
	}
	if ttl == 0 {
		ttl = h.opts.RegistrationTTL
	}
	if ttl <= 0 || ttl > time.Hour {
		return "", time.Time{}, errors.New("registration TTL must be within one hour")
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return "", time.Time{}, ErrClosed
	}
	h.expireLocked()
	token := newID()
	expiry := h.opts.Clock().Add(ttl)
	h.state.Registrations[hashToken(token)] = expiry
	if err := h.saveContextLocked(ctx); err != nil {
		return "", time.Time{}, err
	}
	return token, expiry, nil
}

func (h *Hub) ListNodes() []NodeView {
	h.mu.Lock()
	defer h.mu.Unlock()
	now := h.opts.Clock()
	out := make([]NodeView, 0, len(h.state.Nodes))
	for _, n := range h.state.Nodes {
		v := NodeView{ID: n.ID, Name: n.Name, BootID: n.BootID, Enabled: n.Enabled, Online: !h.closed && n.Enabled && now.Sub(n.LastSeen) < h.opts.LeaseDuration, LastSeen: n.LastSeen, Capabilities: n.Capabilities}
		if status, ok := h.poolStatus[n.ID]; ok {
			v.PoolStatus = &status
		}
		v.EnvironmentPrompt = n.EnvironmentPrompt
		v.ConcurrencyLimit, v.EffectiveSlots = n.ConcurrencyLimit, n.effectiveSlots()
		v.CPULimitMillis, v.MemoryLimitMiB = n.CPULimitMillis, n.MemoryLimitMiB
		v.EffectiveCPUMillis, v.EffectiveMemoryMiB = n.effectiveCPU(), n.effectiveMemory()
		for _, s := range h.state.Sessions {
			if s.NodeID == n.ID && s.reserved(now) {
				v.ActiveSessions++
				v.ReservedCPUMillis += s.Spec.Resources.CPUMillis
				v.ReservedMemoryMiB += s.Spec.Resources.MemoryMiB
				v.ReservedGPUs = append(v.ReservedGPUs, s.GPUIds...)
			}
		}
		// Do not expose mutable maps owned by the broker.
		raw, _ := json.Marshal(v)
		var copy NodeView
		_ = json.Unmarshal(raw, &copy)
		out = append(out, copy)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

func (h *Hub) SetNodeEnabled(id string, enabled bool) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return ErrClosed
	}
	n := h.state.Nodes[id]
	if n == nil {
		return errors.New("runner node not found")
	}
	n.Enabled = enabled
	if !enabled {
		for _, s := range h.state.Sessions {
			if s.NodeID == id && s.live() {
				h.fenceLocked(s, "runner disabled", false)
			}
		}
		return h.saveFenceLocked()
	}
	return h.saveLocked()
}

// Environment notes are set by the administrator, never by registration or
// heartbeat payloads. Existing sessions retain their original prompt snapshot.
func ValidateEnvironmentPrompt(prompt string) error {
	if !utf8.ValidString(prompt) || strings.ContainsRune(prompt, 0) || utf8.RuneCountInString(prompt) > 16000 {
		return errors.New("环境提示词最多 16000 个字符，且不能包含无效字符")
	}
	return nil
}

func (h *Hub) SetNodeEnvironmentPrompt(id, prompt string) error {
	if err := ValidateEnvironmentPrompt(prompt); err != nil {
		return err
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return ErrClosed
	}
	n := h.state.Nodes[id]
	if n == nil {
		return errors.New("runner node not found")
	}
	n.EnvironmentPrompt = prompt
	return h.saveLocked()
}

func (n *nodeRecord) effectiveSlots() int {
	if n.ConcurrencyLimit > 0 && n.ConcurrencyLimit < n.Capabilities.Slots {
		return n.ConcurrencyLimit
	}
	return n.Capabilities.Slots
}

func (n *nodeRecord) effectiveCPU() int64 {
	if n.CPULimitMillis > 0 && n.CPULimitMillis < n.Capabilities.CPUMillis {
		return n.CPULimitMillis
	}
	return n.Capabilities.CPUMillis
}
func (n *nodeRecord) effectiveMemory() int64 {
	if n.MemoryLimitMiB > 0 && n.MemoryLimitMiB < n.Capabilities.MemoryMiB {
		return n.MemoryLimitMiB
	}
	return n.Capabilities.MemoryMiB
}

// SetNodeLimits atomically updates admission budgets without resizing or killing
// running containers. Zero inherits the corresponding advertised hard capacity.
func (h *Hub) SetNodeLimits(id string, slots int, cpu, memory int64) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return ErrClosed
	}
	n := h.state.Nodes[id]
	if n == nil {
		return errors.New("runner node not found")
	}
	if slots < 0 || slots > n.Capabilities.Slots || cpu < 0 || cpu > n.Capabilities.CPUMillis || memory < 0 || memory > n.Capabilities.MemoryMiB {
		return errors.New("limits must be nonnegative and within the node's advertised hard capacity")
	}
	n.ConcurrencyLimit, n.CPULimitMillis, n.MemoryLimitMiB = slots, cpu, memory
	return h.saveLocked()
}

// A smaller limit drains naturally; it never interrupts running attempts.
// Zero restores the node's advertised hard ceiling.
func (h *Hub) SetNodeConcurrencyLimit(id string, limit int) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return ErrClosed
	}
	n := h.state.Nodes[id]
	if n == nil {
		return errors.New("runner node not found")
	}
	if limit < 0 || limit > n.Capabilities.Slots {
		return fmt.Errorf("concurrency limit must be between 0 and %d", n.Capabilities.Slots)
	}
	n.ConcurrencyLimit = limit
	return h.saveLocked()
}

func (h *Hub) chooseNodeLocked(spec RunSpec) (*nodeRecord, []string) {
	now := h.opts.Clock()
	ids := make([]string, 0, len(h.state.Nodes))
	for id := range h.state.Nodes {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		if spec.NodeID != "" && spec.NodeID != id {
			continue
		}
		n := h.state.Nodes[id]
		if !n.Enabled || now.Sub(n.LastSeen) >= h.opts.LeaseDuration {
			continue
		}
		match := true
		for k, v := range spec.Labels {
			if n.Capabilities.Labels[k] != v {
				match = false
				break
			}
		}
		if !match {
			continue
		}
		if spec.Environment != "" {
			found := false
			for _, e := range n.Capabilities.Environments {
				if e == spec.Environment {
					found = true
				}
			}
			if !found {
				continue
			}
		}
		usedSlots := 0
		var cpu, mem int64
		usedGPU := map[string]bool{}
		for _, s := range h.state.Sessions {
			if s.NodeID == id && s.reserved(now) {
				usedSlots++
				cpu += s.Spec.Resources.CPUMillis
				mem += s.Spec.Resources.MemoryMiB
				for _, g := range s.GPUIds {
					usedGPU[g] = true
				}
			}
		}
		if usedSlots >= n.effectiveSlots() || spec.Resources.CPUMillis > n.effectiveCPU()-cpu || spec.Resources.MemoryMiB > n.effectiveMemory()-mem {
			continue
		}
		var gpuIDs []string
		for _, g := range n.Capabilities.GPUs {
			if len(gpuIDs) >= spec.Resources.GPUCount {
				break
			}
			if !usedGPU[g.ID] && (spec.Resources.GPUModel == "" || strings.EqualFold(g.Model, spec.Resources.GPUModel)) && g.MemoryMiB >= spec.Resources.GPUMemoryMiB {
				gpuIDs = append(gpuIDs, g.ID)
			}
		}
		if len(gpuIDs) < spec.Resources.GPUCount {
			continue
		}
		return n, gpuIDs
	}
	return nil, nil
}

func (h *Hub) fenceLocked(s *sessionRecord, reason string, hold bool) {
	if s.State == "closed" || s.State == "expired" {
		return
	}
	s.State = "fenced"
	s.addReason(reason)
	s.HoldUntilExpiry = s.HoldUntilExpiry || hold
	for _, c := range s.Commands {
		if c.Command.Kind == CommandClose {
			c.Command.Abort = true
			c.Command.Reason = reason
			return
		}
	}
	id := newID()
	s.Commands[id] = &commandRecord{Command: Command{ID: id, Kind: CommandClose, SessionID: s.ID, AttemptID: s.AttemptID, Epoch: s.Epoch, LeaseUntil: s.LeaseUntil, Reason: reason, Abort: true}}
}

func (h *Hub) abort(id, reason string) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return ErrClosed
	}
	s := h.state.Sessions[id]
	if s == nil || !s.live() {
		return nil
	}
	h.fenceLocked(s, reason, false)
	return h.saveFenceLocked()
}

func (h *Hub) Close() error {
	h.stopOnce.Do(func() { close(h.stop) })
	h.mu.Lock()
	var err error
	if !h.closed {
		for _, s := range h.state.Sessions {
			if s.live() {
				h.fenceLocked(s, "controller shutdown", true)
			}
		}
		err = h.saveLocked()
		h.closeLocked("shutdown", err, false)
	}
	h.mu.Unlock()
	<-h.done
	return err
}

func (h *Hub) IsClosed() bool {
	return h.BrokerState().Closed
}

// Reopen only recovers failed fence persistence. A CAS conflict can mean another
// controller owns the state; never automatically compete with that owner. The
// replacement fences every predecessor session and retains its original lease
// reservations. Existing Session handles remain attached to the closed hub.
func (h *Hub) Reopen(ctx context.Context) (*Hub, error) {
	h.reopenMu.Lock()
	defer h.reopenMu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if h.reopened || !h.BrokerState().AutoRecovery {
		return nil, ErrClosed
	}
	_ = h.Close()
	h.mu.Lock()
	epoch := h.state.Epoch
	h.mu.Unlock()
	next, err := newHub(ctx, h.opts, epoch)
	if err == nil {
		h.reopened = true
	}
	return next, err
}

// DisableIfIdle commits the feature switch while excluding new admissions.
// Fenced attempts still hold reservations until their processes are confirmed
// stopped or their lease expires; switching these to local execution is unsafe.
// commit should only persist the central configuration, without calling Hub APIs.
func (h *Hub) DisableIfIdle(commit func() error) error {
	h.mu.Lock()
	for _, s := range h.state.Sessions {
		if s.reserved(h.opts.Clock()) {
			h.mu.Unlock()
			return ErrSessionsActive
		}
	}
	if commit == nil {
		h.mu.Unlock()
		return errors.New("missing disable commit")
	}
	if err := commit(); err != nil {
		h.mu.Unlock()
		return err
	}
	h.closeLocked("disabled", nil, false)
	h.stopOnce.Do(func() { close(h.stop) })
	h.mu.Unlock()
	<-h.done
	return nil
}
