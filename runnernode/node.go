package runnernode

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/RC-CHN/ARTEX/norma/tool"
	"github.com/RC-CHN/ARTEX/runner"
	"github.com/RC-CHN/ARTEX/runnerfiles"
)

type identity struct {
	NodeID     string `json:"node_id"`
	Credential string `json:"credential"`
	ServerURL  string `json:"server_url"`
}
type commandState struct {
	hash   [32]byte
	result *runner.ResultRequest
	queued bool
}
type nodeSession struct {
	id, attempt, epoch string
	ctx                context.Context
	cancel             context.CancelFunc
	sandbox            Sandbox
	syncer             *runnerfiles.Syncer
	storageCheck       func() error // shared-volume admission/periodic free-space guard
	mu                 sync.Mutex
	stopMu             sync.Mutex // serialize and confirm process cleanup
	ended              bool
	stopReason         string
	stopped            bool
	cleanupRunning     bool
	cleanupAfter       time.Time
	syncErr            error
	checkpointRunning  bool
	opened             bool // Open may initialize MCPs and create recovery-worthy files.
	ready              chan struct{}
	dir                string
	resources          runner.Resources // protected by Node.mu
	gpus               []string
}

// Node holds only transport/lifecycle state. Tool code runs in Executor's isolated
// subprocess. HTTP is injectable for TLS-pinned deployments and protocol tests.
type Node struct {
	Config           Config
	HTTP             *http.Client
	Executor         Executor
	Log              *log.Logger
	identity         identity
	bootID           string
	mu               sync.Mutex
	epoch            string
	serverTime       time.Time
	pollSent         time.Time
	sessions         map[string]*nodeSession
	leases           map[string]runner.Lease
	commands         map[string]*commandState
	notifications    map[string]runner.NotificationEnvelope
	commandsWG       sync.WaitGroup
	heartbeatEnabled atomic.Bool
	admissionEnabled atomic.Bool
	responseMu       sync.Mutex
}
type httpStatusError struct {
	route  string
	status int
}

func (e *httpStatusError) Error() string {
	return fmt.Sprintf("runner %s returned HTTP %d", e.route, e.status)
}

func New(c Config) (*Node, error) {
	if err := c.Validate(); err != nil {
		return nil, err
	}
	return &Node{Config: c, HTTP: &http.Client{Timeout: 25 * time.Second, CheckRedirect: func(req *http.Request, via []*http.Request) error {
		if len(via) > 0 && (req.URL.Scheme != via[0].URL.Scheme || req.URL.Host != via[0].URL.Host) {
			return fmt.Errorf("controller redirect changed origin")
		}
		if len(via) >= 5 {
			return fmt.Errorf("too many controller redirects")
		}
		return nil
	}}, Log: log.New(os.Stderr, "artex-runner: ", log.LstdFlags), bootID: randomID(), sessions: map[string]*nodeSession{}, leases: map[string]runner.Lease{}, commands: map[string]*commandState{}, notifications: map[string]runner.NotificationEnvelope{}}, nil
}
func randomID() string {
	var b [16]byte
	if _, e := rand.Read(b[:]); e != nil {
		panic(e)
	}
	return hex.EncodeToString(b[:])
}

func secureDir(dir string) error {
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	st, err := os.Lstat(dir)
	if err != nil {
		return err
	}
	if !st.IsDir() || st.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("runner data directory must be a real directory")
	}
	return os.Chmod(dir, 0700)
}
func loadIdentity(file string) (identity, error) {
	var id identity
	st, e := os.Lstat(file)
	if e != nil {
		return id, e
	}
	if !st.Mode().IsRegular() || st.Mode().Perm() != 0600 {
		return id, fmt.Errorf("runner credential file must be a regular file with mode 0600")
	}
	b, e := os.ReadFile(file)
	if e != nil {
		return id, e
	}
	e = json.Unmarshal(b, &id)
	if e == nil && (id.NodeID == "" || id.Credential == "") {
		e = fmt.Errorf("invalid runner credential file")
	}
	return id, e
}
func saveIdentity(file string, id identity) error {
	f, e := os.CreateTemp(filepath.Dir(file), ".credentials-*")
	if e != nil {
		return e
	}
	defer os.Remove(f.Name())
	if e = f.Chmod(0600); e != nil {
		f.Close()
		return e
	}
	if e = json.NewEncoder(f).Encode(id); e != nil {
		f.Close()
		return e
	}
	if e = f.Sync(); e != nil {
		f.Close()
		return e
	}
	if e = f.Close(); e != nil {
		return e
	}
	return os.Rename(f.Name(), file)
}
func (n *Node) request(ctx context.Context, route string, input, output any) error {
	body, err := json.Marshal(input)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, "POST", strings.TrimRight(n.Config.ServerURL, "/")+"/runner/v1/"+route, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if n.identity.Credential != "" {
		req.Header.Set("Authorization", "Bearer "+n.identity.Credential)
	}
	resp, err := n.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
		return &httpStatusError{route: route, status: resp.StatusCode}
	}
	return json.NewDecoder(io.LimitReader(resp.Body, 32<<20)).Decode(output)
}
func (n *Node) enroll(ctx context.Context, token string) error {
	if err := secureDir(n.Config.DataDir); err != nil {
		return err
	}
	file := filepath.Join(n.Config.DataDir, "credentials.json")
	id, err := loadIdentity(file)
	if err == nil {
		if id.ServerURL != n.Config.ServerURL {
			return fmt.Errorf("saved credential belongs to another controller")
		}
		n.identity = id
		return nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if token == "" {
		return fmt.Errorf("enrollment token required for first registration")
	}
	var response runner.RegisterResponse
	if err = n.request(ctx, "register", runner.RegisterRequest{Token: token, Name: n.Config.Name, BootID: n.bootID, Capabilities: n.Config.Capabilities, Protocol: runner.ProtocolVersion}, &response); err != nil {
		return err
	}
	if response.Protocol != runner.ProtocolVersion || response.NodeID == "" || response.Credential == "" {
		return fmt.Errorf("invalid controller registration response")
	}
	n.identity = identity{NodeID: response.NodeID, Credential: response.Credential, ServerURL: n.Config.ServerURL}
	return saveIdentity(file, n.identity)
}

// Run polls independently of calls/image preparation, so long tools cannot starve
// heartbeat renewal. Lease expiry still stops containers when HTTPS is down.
func (n *Node) Run(ctx context.Context, enrollmentToken string) error {
	if err := secureDir(n.Config.DataDir); err != nil {
		return err
	}
	if err := secureDir(n.Config.WorkspaceDir); err != nil {
		return err
	}
	lock, err := lockNode(filepath.Join(n.Config.DataDir, "node.lock"))
	if err != nil {
		return err
	}
	defer lock.Close()
	if err := n.enroll(ctx, enrollmentToken); err != nil {
		return err
	}
	if n.Executor == nil {
		if n.Config.UnsafeProcess {
			n.Executor = &ProcessExecutor{Binary: n.Config.SandboxBinary, Log: n.Log.Writer()}
			n.Log.Print("UNSAFE development process executor: no OS isolation")
		} else {
			var d interface {
				Executor
				CleanupOrphans(context.Context) error
			}
			if n.Config.Executor == "kubernetes" {
				d = &KubernetesExecutor{Config: n.Config.Kubernetes, WorkspaceDir: n.Config.WorkspaceDir, NodeID: n.identity.NodeID, MaxPods: n.Config.Capabilities.Slots, SandboxBinary: n.Config.SandboxBinary, Log: n.Log.Writer()}
			} else {
				d = &DockerExecutor{Binary: n.Config.DockerBinary, SandboxBinary: n.Config.SandboxBinary, NodeID: n.identity.NodeID, DataDir: n.Config.DataDir, HostDataDir: n.Config.DockerHostDataDir, Network: n.Config.DockerNetwork, Log: n.Log.Writer()}
			}
			cleanCtx, done := context.WithTimeout(ctx, 30*time.Second)
			err := d.CleanupOrphans(cleanCtx)
			done()
			if err != nil {
				return fmt.Errorf("clean old runner containers before new boot: %w", err)
			}
			n.Executor = d
		}
	}
	ctx, cancel := context.WithCancel(ctx)
	defer func() { cancel(); n.stopAll(); n.commandsWG.Wait(); n.stopAll() }()
	if k, ok := n.Executor.(*KubernetesExecutor); ok {
		poolCtx, stopPool := context.WithCancel(ctx)
		poolDone := make(chan struct{})
		go func() { defer close(poolDone); k.RunPool(poolCtx) }()
		defer func() { stopPool(); <-poolDone }()
	}
	go n.watchdog(ctx)
	go n.heartbeats(ctx)
	go n.sendResults(ctx)
	go n.checkpoints(ctx)
	failures := 0
	poolSupported := false // Negotiate optional fields before sending to strict older controllers.
	for ctx.Err() == nil {
		n.mu.Lock()
		notes := make([]runner.NotificationEnvelope, 0, len(n.notifications))
		for _, note := range n.notifications {
			notes = append(notes, note)
		}
		n.mu.Unlock()
		var poll runner.PollResponse
		var poolStatus *runner.PoolStatus
		if k, ok := n.Executor.(*KubernetesExecutor); ok && poolSupported {
			value := k.PoolStatus()
			if !n.admissionEnabled.Load() {
				value.Admission = nil
			}
			poolStatus = &value
		}
		sent := time.Now()
		err := n.request(ctx, "poll", runner.PollRequest{NodeID: n.identity.NodeID, BootID: n.bootID, Protocol: runner.ProtocolVersion, Notifications: notes, Capabilities: &n.Config.Capabilities, PoolStatus: poolStatus}, &poll)
		if err != nil {
			var status *httpStatusError
			if errors.As(err, &status) && status.status == http.StatusBadRequest {
				poolSupported = false // Controller may have been rolled back.
				n.admissionEnabled.Store(false)
			}
			if errors.As(err, &status) && (status.status == http.StatusUnauthorized || status.status == http.StatusForbidden || status.status == http.StatusGone) {
				return fmt.Errorf("runner identity rejected; stopping local executions: %w", err)
			}
			failures++
			if failures == 1 || failures%10 == 0 {
				n.Log.Printf("controller poll unavailable (%d consecutive failures); lease watchdog remains active", failures)
			}
			delay := time.Duration(min(failures, 10)) * time.Second
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(delay):
			}
			continue
		}
		failures = 0
		n.heartbeatEnabled.Store(poll.Heartbeat)
		n.admissionEnabled.Store(poll.AdmissionDiagnostics)
		poolSupported = poll.Pool != nil
		n.applyPoll(ctx, poll, sent)
		// Also bound repeated outstanding commands from older controllers.
		if remaining := 100*time.Millisecond - time.Since(sent); remaining > 0 {
			select {
			case <-ctx.Done():
			case <-time.After(remaining):
			}
		}
	}
	return ctx.Err()
}
func (n *Node) applyPoll(ctx context.Context, poll runner.PollResponse, sent time.Time) {
	n.responseMu.Lock()
	defer n.responseMu.Unlock()
	if !n.applyLeases(poll, sent) {
		return
	}

	if k, ok := n.Executor.(*KubernetesExecutor); ok && poll.Pool == nil {
		k.poolMu.Lock()
		k.policy, k.policyAt = runner.PoolPolicy{}, time.Time{}
		k.poolMu.Unlock()
	}
	if k, ok := n.Executor.(*KubernetesExecutor); ok && poll.Pool != nil {
		policy := runner.PoolPolicy{}
		if poll.Pool != nil {
			policy = *poll.Pool
			policy.Profiles = nil
			for _, p := range poll.Pool.Profiles {
				prepared, err := prepareWarmProfile(n.Config, p)
				if err != nil {
					n.Log.Printf("prewarm profile %s: %v", p.Environment, err)
					continue
				}
				policy.Profiles = append(policy.Profiles, prepared)
			}
		}
		k.UpdatePool(policy)
	}
	for _, cmd := range poll.Commands {
		n.acceptCommand(ctx, cmd)
	}
}

// Caller serializes response handling. A late command poll cannot revoke a
// session admitted by a newer heartbeat, nor shorten a previously safe deadline.
func (n *Node) applyLeases(poll runner.PollResponse, sent time.Time) bool {
	n.mu.Lock()
	defer n.mu.Unlock()
	if (!n.serverTime.IsZero() && poll.Epoch == n.epoch && poll.ServerTime.Before(n.serverTime)) || (poll.Epoch != n.epoch && sent.Before(n.pollSent)) {
		return false
	}
	valid := map[string]runner.Lease{}
	for _, l := range poll.Leases {
		ttl := l.Until.Sub(poll.ServerTime)
		if l.Epoch != poll.Epoch || ttl <= 0 || ttl > 2*time.Minute {
			continue
		}
		l.Until = sent.Add(ttl)
		if previous, ok := n.leases[l.SessionID]; ok && previous.AttemptID == l.AttemptID && previous.Epoch == l.Epoch && previous.Until.After(l.Until) {
			l.Until = previous.Until
		}
		valid[l.SessionID] = l
	}
	n.epoch = poll.Epoch
	n.serverTime = poll.ServerTime
	n.pollSent = sent
	n.leases = valid
	for id, s := range n.sessions {
		l, ok := valid[id]
		if !ok || s.epoch != poll.Epoch || l.AttemptID != s.attempt {
			n.scheduleAbortReason(s, "lease_revoked")
		}
	}
	for _, id := range poll.AcknowledgedNotifications {
		delete(n.notifications, id)
	}
	for id, note := range n.notifications {
		if l, ok := valid[note.SessionID]; !ok || l.AttemptID != note.AttemptID {
			delete(n.notifications, id)
		}
	}
	return true
}

func commandHash(cmd runner.Command) [32]byte {
	cmd.LeaseUntil = time.Time{}
	b, _ := json.Marshal(cmd)
	return sha256.Sum256(b)
}
func (n *Node) acceptCommand(ctx context.Context, cmd runner.Command) {
	if cmd.ID == "" {
		return
	}
	hash := commandHash(cmd)
	n.mu.Lock()
	if old := n.commands[cmd.ID]; old != nil {
		if old.hash == hash && old.result != nil {
			old.queued = true
		}
		n.mu.Unlock()
		return
	}
	if len(n.commands) >= 1000000 {
		n.mu.Unlock()
		n.Log.Print("command ledger full; refusing further commands until restart")
		return
	}
	n.commands[cmd.ID] = &commandState{hash: hash}
	n.commandsWG.Add(1)
	n.mu.Unlock()
	go func() {
		defer n.commandsWG.Done()
		r := n.executeCommand(ctx, cmd)
		n.mu.Lock()
		record := n.commands[cmd.ID]
		record.result = &r
		record.queued = true
		n.mu.Unlock()
	}()
}
func (n *Node) executeCommand(ctx context.Context, cmd runner.Command) runner.ResultRequest {
	result := runner.ResultRequest{NodeID: n.identity.NodeID, BootID: n.bootID, CommandID: cmd.ID, SessionID: cmd.SessionID, AttemptID: cmd.AttemptID, Epoch: cmd.Epoch}
	if ctx.Err() != nil && cmd.Kind != runner.CommandClose {
		result.Error = ctx.Err().Error()
		return result
	}
	n.mu.Lock()
	lease, leased := n.leases[cmd.SessionID]
	s := n.sessions[cmd.SessionID]
	epoch := n.epoch
	n.mu.Unlock()
	if cmd.Kind != runner.CommandClose && (!leased || lease.AttemptID != cmd.AttemptID || epoch != cmd.Epoch || !time.Now().Before(lease.Until)) {
		result.Error = "session lease expired or fenced"
		return result
	}
	switch cmd.Kind {
	case runner.CommandOpen:
		if cmd.Spec == nil {
			result.Error = "missing session spec"
			return result
		}
		if s != nil {
			result.Error = "session already exists"
			return result
		}
		sessionCtx, cancel := context.WithCancel(ctx)
		s = &nodeSession{id: cmd.SessionID, attempt: cmd.AttemptID, epoch: cmd.Epoch, ctx: sessionCtx, cancel: cancel, ready: make(chan struct{}), dir: filepath.Join(n.Config.WorkspaceDir, safeID(cmd.SessionID))}
		n.mu.Lock()
		active := 0
		for _, existing := range n.sessions {
			existing.mu.Lock()
			if !existing.stopped {
				active++
			}
			existing.mu.Unlock()
		}
		if active >= n.Config.Capabilities.Slots {
			n.mu.Unlock()
			cancel()
			result.Error = "node session capacity exhausted"
			return result
		}
		if old := n.sessions[cmd.SessionID]; old != nil {
			n.mu.Unlock()
			cancel()
			result.Error = "session already exists"
			return result
		}
		n.sessions[cmd.SessionID] = s
		n.mu.Unlock()
		defer close(s.ready)
		spec, plan, err := n.Config.Prepare(*cmd.Spec)
		if err == nil {
			err = n.reserveResources(s, &spec, cmd.GPUIds)
		}
		var warm *warmPod
		if k, ok := n.Executor.(*KubernetesExecutor); ok && err == nil {
			warm, err = k.ClaimWarm(sessionCtx, s.id, spec, plan)
			if warm != nil {
				s.dir = warm.dir
				s.mu.Lock()
				s.sandbox = warm.sandbox
				s.mu.Unlock()
			}
		}
		if err == nil {
			err = secureDir(s.dir)
		}
		roots := map[string]string{}
		if err == nil && warm != nil {
			err = warm.activate()
			if err == nil {
				if plan.Mode == runnerfiles.SharedMode {
					roots, err = n.Executor.(*KubernetesExecutor).sharedRoots(plan)
				} else {
					roots, err = warm.roots(plan)
				}
			}
		}
		if err == nil && warm == nil && plan.Mode == runnerfiles.SharedMode {
			roots, err = n.Executor.(*KubernetesExecutor).sharedRoots(plan)
		}
		if err == nil && plan.Mode == runnerfiles.SharedMode {
			k := n.Executor.(*KubernetesExecutor)
			s.storageCheck = func() error { return k.checkSharedCapacity(roots) }
		}
		if err == nil && warm == nil && plan.Mode != runnerfiles.SharedMode {
			for _, mount := range plan.Mounts {
				roots[mount.Name] = filepath.Join(s.dir, "mounts", safeID(mount.Name))
				if e := os.MkdirAll(roots[mount.Name], 0700); e != nil {
					err = e
					break
				}
			}
		}
		var preparing Sandbox
		if k, ok := n.Executor.(*KubernetesExecutor); ok && err == nil && warm == nil && k.prewarmingEnabled(spec.Environment) {
			preparing = k.PrepareExecution(sessionCtx, s.id, spec, plan, roots)
			s.mu.Lock()
			s.sandbox = preparing
			s.mu.Unlock()
		}
		if err == nil {
			fileHTTP := *n.HTTP
			fileHTTP.Timeout = 0 // Large files are bounded by the session lease, not a 25s poll timeout.
			if fileHTTP.Transport == nil {
				tr := http.DefaultTransport.(*http.Transport).Clone()
				tr.ResponseHeaderTimeout = 30 * time.Second
				fileHTTP.Transport = tr
			}
			transferPlan := plan
			if plan.Mode == runnerfiles.SharedMode {
				transferPlan = runnerfiles.Plan{}
			}
			s.syncer = &runnerfiles.Syncer{Client: &runnerfiles.Client{BaseURL: n.Config.ServerURL, Credential: n.identity.Credential, HTTP: &fileHTTP}, Session: s.id, Plan: transferPlan, Roots: roots}
			err = s.syncer.Pull(sessionCtx)
		}
		if err == nil {
			var sandbox Sandbox
			if warm != nil {
				sandbox = warm.sandbox
			} else if preparing != nil {
				sandbox = preparing
			} else {
				sandbox, err = n.Executor.Start(sessionCtx, s.id, spec, plan, roots, cmd.GPUIds)
			}
			if sandbox != nil {
				s.mu.Lock()
				s.sandbox = sandbox
				s.mu.Unlock()
			}
			if err == nil {
				var reply SandboxReply
				s.mu.Lock()
				s.opened = true
				s.mu.Unlock()
				reply, err = sandbox.Request(sessionCtx, SandboxRequest{Method: "open", SessionID: s.id, Spec: &spec})
				if err == nil && reply.Error != "" {
					err = errors.New(reply.Error)
				}
				result.MCPTools = reply.MCPTools
				result.EnvironmentTools = reply.EnvironmentTools
			}
		}
		if err != nil {
			result.Error = err.Error()
			if stopErr := n.abort(s); stopErr != nil {
				result.UnknownOutcome = true
			}
		}
	case runner.CommandCall:
		if s == nil || s.attempt != cmd.AttemptID || s.epoch != cmd.Epoch || cmd.Call == nil {
			result.Error = "unknown session or missing tool call"
			return result
		}
		select {
		case <-s.ready:
		case <-s.ctx.Done():
			result.Error = "session stopped"
			return result
		}
		s.mu.Lock()
		sandbox := s.sandbox
		ended := s.ended
		syncErr := s.syncErr
		s.mu.Unlock()
		if syncErr != nil {
			result.Error = "session file checkpoint failed: " + syncErr.Error()
			result.UnknownOutcome = true
			return result
		}
		if ended || sandbox == nil {
			result.Error = "session stopped"
			return result
		}
		preparedCall, prepareErr := n.prepareCall(*cmd.Call)
		if prepareErr != nil {
			result.Error = prepareErr.Error()
			return result
		}
		if !preparedCall.Deadline.IsZero() {
			n.mu.Lock()
			serverTime, sent := n.serverTime, n.pollSent
			n.mu.Unlock()
			if !serverTime.IsZero() {
				preparedCall.Deadline = sent.Add(preparedCall.Deadline.Sub(serverTime))
			}
		}
		// Read-only refresh is safe because the container cannot write these paths.
		if err := pullReadOnly(s.ctx, s.syncer); err != nil {
			result.Error = "refresh shared inputs: " + err.Error()
			return result
		}
		reply, err := sandbox.Request(s.ctx, SandboxRequest{Method: "call", Call: &preparedCall})
		result.Result = reply.Result
		result.Notifications = reply.Notifications
		result.Error = reply.Error
		if reply.Error != "" && reply.Executed {
			result.UnknownOutcome = true
		}
		if err != nil {
			result.Error = err.Error()
			result.UnknownOutcome = true
			s.mu.Lock()
			if s.syncErr != nil {
				result.Error = "session file checkpoint failed: " + s.syncErr.Error()
			}
			s.mu.Unlock()
			return result
		}
		if err = s.syncer.Checkpoint(s.ctx); err != nil && !errors.Is(err, runnerfiles.ErrSourceBusy) {
			result.Error = "publish session files: " + err.Error()
			result.UnknownOutcome = true
			s.mu.Lock()
			s.syncErr = err
			s.mu.Unlock()
		}
	case runner.CommandClose:
		if s == nil {
			return result
		}
		if s.attempt != cmd.AttemptID || s.epoch != cmd.Epoch {
			result.Error = "close does not own this session"
			return result
		}
		if cmd.Abort {
			// An Open might still be preparing an image. Cancellation alone is
			// not proof of process exit; wait before releasing the reservation.
			s.cancel()
			select {
			case <-s.ready:
			case <-time.After(10 * time.Second):
				result.Error = "session preparation has not stopped"
				result.UnknownOutcome = true
				return result
			}
			if err := n.abort(s); err != nil {
				result.Error = err.Error()
				result.UnknownOutcome = true
			}
		} else {
			select {
			case <-s.ready:
			case <-s.ctx.Done():
			}
			s.mu.Lock()
			sandbox := s.sandbox
			s.ended = true
			s.mu.Unlock()
			if sandbox != nil {
				closeCtx, done := context.WithTimeout(ctx, 10*time.Second)
				_, e := sandbox.Request(closeCtx, SandboxRequest{Method: "close"})
				done()
				if e != nil {
					result.Error = e.Error()
				}
			}
			if e := n.stopSession(s); e != nil {
				result.Error = e.Error()
				result.UnknownOutcome = true
			}
			if !result.UnknownOutcome && s.syncer != nil {
				if e := s.syncer.Push(s.ctx); e != nil {
					result.Error = "final file publication: " + e.Error()
				}
			}
			s.cancel()
		}
		n.mu.Lock()
		if !result.UnknownOutcome && n.sessions[s.id] == s {
			delete(n.sessions, s.id)
		}
		n.mu.Unlock()
		s.mu.Lock()
		preserve := s.storageCheck == nil && (s.syncErr != nil || (cmd.Abort && s.opened))
		s.mu.Unlock()
		if !preserve && !result.UnknownOutcome && (result.Error == "" || s.storageCheck != nil) {
			_ = os.RemoveAll(s.dir)
		} // retain conflicted files for recovery
	default:
		result.Error = "unknown runner command"
	}
	return result
}
func pullReadOnly(ctx context.Context, s *runnerfiles.Syncer) error {
	return s.PullReadOnly(ctx)
}

func (n *Node) prepareCall(call runner.ToolCall) (runner.ToolCall, error) {
	env := map[string]string{}
	for _, pair := range call.Env {
		k, v, ok := strings.Cut(pair, "=")
		if !ok {
			return call, fmt.Errorf("invalid tool environment")
		}
		env[k] = v
	}
	if err := validEnv(env); err != nil {
		return call, err
	}
	for k, v := range env {
		if override, ok := n.Config.Env[k]; ok {
			env[k] = override
			continue
		}
		if controllerLocalURL(v) {
			return call, fmt.Errorf("tool environment %s references controller loopback; configure a node override", k)
		}
	}
	for _, k := range []string{"HOME", "TMPDIR", "TMP", "TEMP"} {
		delete(env, k)
	}
	call.Env = envList(env)
	return call, nil
}

func (n *Node) reserveResources(s *nodeSession, spec *runner.RunSpec, gpus []string) error {
	r := &spec.Resources
	if r.CPUMillis < 0 || r.MemoryMiB < 0 || r.PIDs < 0 || r.GPUCount < 0 {
		return fmt.Errorf("resource limits cannot be negative")
	}
	if r.CPUMillis == 0 {
		r.CPUMillis = 1000
	}
	if r.MemoryMiB == 0 {
		r.MemoryMiB = 1024
	}
	if r.PIDs == 0 {
		r.PIDs = 256
	}
	if len(gpus) != r.GPUCount {
		return fmt.Errorf("assigned GPU count differs from resource request")
	}
	available := map[string]bool{}
	for _, g := range n.Config.Capabilities.GPUs {
		available[g.ID] = true
	}
	used := map[string]bool{}
	cpu, memory := r.CPUMillis, r.MemoryMiB
	n.mu.Lock()
	defer n.mu.Unlock()
	for _, other := range n.sessions {
		if other == s {
			continue
		}
		other.mu.Lock()
		stopped := other.stopped
		other.mu.Unlock()
		if stopped {
			continue
		}
		cpu += other.resources.CPUMillis
		memory += other.resources.MemoryMiB
		for _, g := range other.gpus {
			used[g] = true
		}
	}
	for _, g := range gpus {
		if !available[g] || used[g] {
			return fmt.Errorf("GPU device is unavailable or already assigned")
		}
		used[g] = true
	}
	cpuLimit, memoryLimit := n.Config.Capabilities.CPUMillis, n.Config.Capabilities.MemoryMiB
	if spec.PoolCPUMillis > 0 && spec.PoolCPUMillis < cpuLimit {
		cpuLimit = spec.PoolCPUMillis
	}
	if spec.PoolMemoryMiB > 0 && spec.PoolMemoryMiB < memoryLimit {
		memoryLimit = spec.PoolMemoryMiB
	}
	if cpuLimit > 0 && cpu > cpuLimit {
		return fmt.Errorf("node CPU capacity exhausted")
	}
	if memoryLimit > 0 && memory > memoryLimit {
		return fmt.Errorf("node memory capacity exhausted")
	}
	s.resources = *r
	s.gpus = append([]string{}, gpus...)
	return nil
}
func (n *Node) abort(s *nodeSession) error {
	s.cancel()
	s.mu.Lock()
	s.ended = true
	s.mu.Unlock()
	return n.stopSession(s)
}

func (n *Node) stopSession(s *nodeSession) error {
	s.stopMu.Lock()
	defer s.stopMu.Unlock()
	s.mu.Lock()
	sandbox, stopped := s.sandbox, s.stopped
	s.mu.Unlock()
	if stopped {
		return nil
	}
	if sandbox != nil {
		err := sandbox.Stop()
		if err == nil {
			s.mu.Lock()
			s.stopped = true
			s.mu.Unlock()
		}
		return err
	}
	select {
	case <-s.ready:
		s.mu.Lock()
		s.stopped = true
		s.mu.Unlock()
	default:
	}
	return nil
}

// A slow/unavailable container runtime must not create another blocked Stop
// goroutine on every poll/watchdog tick. Explicit close still waits for proof.
func (n *Node) scheduleAbort(s *nodeSession) { n.scheduleAbortReason(s, "session_aborted") }
func (n *Node) scheduleAbortReason(s *nodeSession, reason string) {
	s.cancel()
	s.mu.Lock()
	s.ended = true
	if s.stopReason == "" {
		s.stopReason = reason
	}
	if s.stopped || s.cleanupRunning || time.Now().Before(s.cleanupAfter) {
		s.mu.Unlock()
		return
	}
	s.cleanupRunning = true
	s.mu.Unlock()
	go func() {
		err := n.stopSession(s)
		s.mu.Lock()
		s.cleanupRunning = false
		if err != nil {
			s.cleanupAfter = time.Now().Add(5 * time.Second)
		}
		s.mu.Unlock()
		if err != nil {
			n.Log.Printf("session %s cleanup unconfirmed; retaining reservation and retrying: %v", s.id, err)
		}
	}()
}
func (n *Node) stopAll() {
	n.mu.Lock()
	sessions := []*nodeSession{}
	for _, s := range n.sessions {
		sessions = append(sessions, s)
	}
	n.mu.Unlock()
	for _, s := range sessions {
		_ = n.abort(s)
	}
	// Keep preparing sessions reachable: Start may return a sandbox after this
	// pass. Run waits for commands, then repeats cleanup before releasing its lock.
	n.mu.Lock()
	defer n.mu.Unlock()
	for id, s := range n.sessions {
		s.mu.Lock()
		stopped := s.stopped
		s.mu.Unlock()
		if stopped {
			delete(n.sessions, id)
		}
	}
}
func (n *Node) watchdog(ctx context.Context) {
	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-ticker.C:
			n.collectRetired(now)
			n.mu.Lock()
			for id, s := range n.sessions {
				l, ok := n.leases[id]
				if !ok || !now.Before(l.Until) || l.AttemptID != s.attempt || l.Epoch != s.epoch {
					n.scheduleAbortReason(s, "lease_expired")
				}
			}
			n.mu.Unlock()
		}
	}
}
func (n *Node) sendResults(ctx context.Context) {
	timer := time.NewTimer(100 * time.Millisecond)
	defer timer.Stop()
	failures := 0
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		}
		n.mu.Lock()
		pending := []runner.ResultRequest{}
		for _, record := range n.commands {
			if record.queued && record.result != nil {
				pending = append(pending, *record.result)
			}
		}
		n.mu.Unlock()
		failed := false
		var firstErr error
		var failedCommand string
		for _, result := range pending {
			var response runner.ResultResponse
			err := n.request(ctx, "results", result, &response)
			if err == nil && !response.Accepted {
				err = errors.New("controller did not acknowledge result")
			}
			if err != nil {
				var status *httpStatusError
				if !errors.As(err, &status) || (status.status != http.StatusGone && status.status != http.StatusConflict) {
					failed = true
					if firstErr == nil {
						firstErr, failedCommand = err, result.CommandID
					}
					// Keep this receipt for retry without delaying acknowledgments
					// of other completed commands behind a rejected result.
					continue
				}
			}
			n.mu.Lock()
			if record := n.commands[result.CommandID]; record != nil {
				record.queued = false
			}
			n.mu.Unlock()
		}
		delay := 100 * time.Millisecond
		if failed {
			failures++
			if failures == 1 || failures%10 == 0 {
				n.Log.Printf("result delivery unavailable (%d consecutive attempts), command %s: %v; retaining completed result for retry without re-execution", failures, failedCommand, firstErr)
			}
			delay = min(time.Duration(1<<min(failures-1, 6))*250*time.Millisecond, 10*time.Second)
		} else {
			if failures > 0 {
				n.Log.Print("result delivery recovered")
			}
			failures = 0
		}
		timer.Reset(delay)
	}
}
func (n *Node) checkpoints(ctx context.Context) {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		n.mu.Lock()
		sessions := []*nodeSession{}
		for _, s := range n.sessions {
			sessions = append(sessions, s)
		}
		n.mu.Unlock()
		for _, s := range sessions {
			select {
			case <-s.ready:
			default:
				continue
			}
			s.mu.Lock()
			sb, ended := s.sandbox, s.ended || s.checkpointRunning
			if sb != nil && !ended {
				s.checkpointRunning = true
			}
			s.mu.Unlock()
			if sb == nil || ended {
				continue
			}
			go func(s *nodeSession, sb Sandbox) {
				defer func() { s.mu.Lock(); s.checkpointRunning = false; s.mu.Unlock() }()
				noteCtx, cancel := context.WithTimeout(s.ctx, 3*time.Second)
				reply, e := sb.Request(noteCtx, SandboxRequest{Method: "notifications"})
				cancel()
				var err error
				if s.storageCheck != nil {
					err = s.storageCheck()
				}
				if err == nil {
					err = s.syncer.Checkpoint(s.ctx)
				}
				if errors.Is(err, runnerfiles.ErrSourceBusy) {
					// A command is still writing; the next checkpoint/final Push
					// retries synchronization without replaying any commands.
					err = nil
				}
				if err != nil {
					s.mu.Lock()
					ending := s.ended
					s.mu.Unlock()
					if ending || s.ctx.Err() != nil {
						return // expected drain/cancellation, not a new file failure
					}
					n.Log.Printf("session %s background file checkpoint failed: %v; preserving local snapshot for recovery", s.id, err)
					s.mu.Lock()
					first := s.syncErr == nil
					s.syncErr = err
					s.mu.Unlock()
					if first {
						note := runner.NotificationEnvelope{ID: randomID(), SessionID: s.id, AttemptID: s.attempt, Items: []tool.Notification{{TaskID: "file-sync", Kind: "file_sync", Status: tool.TaskFailed, Summary: "File checkpoint failed; execution stopped and local results were retained. Do not replay commands: " + err.Error()}}}
						n.mu.Lock()
						n.notifications[note.ID] = note
						n.mu.Unlock()
					}
					_ = n.abort(s)
				}
				if e == nil && len(reply.Notifications) > 0 {
					note := runner.NotificationEnvelope{ID: randomID(), SessionID: s.id, AttemptID: s.attempt, Items: reply.Notifications}
					n.mu.Lock()
					n.notifications[note.ID] = note
					n.mu.Unlock()
				}
			}(s, sb)
		}
	}
}
