package runner

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/RC-CHN/ARTEX/norma/tool"
)

type testClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *testClock) Now() time.Time          { c.mu.Lock(); defer c.mu.Unlock(); return c.now }
func (c *testClock) Advance(d time.Duration) { c.mu.Lock(); c.now = c.now.Add(d); c.mu.Unlock() }

func newTestHub(t *testing.T, store StateStore, clock *testClock) *Hub {
	t.Helper()
	if store == nil {
		store = &MemoryStore{}
	}
	opts := Options{Store: store, LeaseDuration: 5 * time.Second, PollWait: time.Millisecond}
	if clock != nil {
		opts.Clock = clock.Now
	}
	h, err := New(opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = h.Close() })
	return h
}

type testNode struct {
	h                    *Hub
	id, credential, boot string
}

func protocolPOST(t *testing.T, h *Hub, route, credential string, body any, out any) int {
	t.Helper()
	b, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest(http.MethodPost, route, bytes.NewReader(b))
	if credential != "" {
		r.Header.Set("Authorization", "Bearer "+credential)
	}
	w := httptest.NewRecorder()
	h.Handler().ServeHTTP(w, r)
	if out != nil && w.Code < 300 {
		if err := json.Unmarshal(w.Body.Bytes(), out); err != nil {
			t.Fatal(err)
		}
	}
	return w.Code
}
func registerNode(t *testing.T, h *Hub, caps Capabilities) *testNode {
	t.Helper()
	token, _, err := h.CreateRegistrationToken(context.Background(), time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	boot := newID()
	var out RegisterResponse
	if code := protocolPOST(t, h, "/register", "", RegisterRequest{Token: token, Name: "test-node", BootID: boot, Capabilities: caps, Protocol: ProtocolVersion}, &out); code != 201 {
		t.Fatalf("register status %d", code)
	}
	return &testNode{h: h, id: out.NodeID, credential: out.Credential, boot: boot}
}
func defaultCapabilities() Capabilities {
	return Capabilities{Slots: 4, CPUMillis: 8000, MemoryMiB: 16384, Environments: []string{"default"}, Labels: map[string]string{"os": "linux"}}
}
func spec(thread string) RunSpec {
	return RunSpec{ThreadID: thread, WorkingDir: "/workspace/task", Environment: "default", Tools: []ToolDefinition{{Name: "Bash", Kind: "builtin"}}}
}
func (n *testNode) poll(t *testing.T) PollResponse {
	t.Helper()
	out, _, err := n.h.poll(PollRequest{NodeID: n.id, BootID: n.boot, Protocol: ProtocolVersion}, n.id)
	if err != nil {
		t.Fatal(err)
	}
	return out
}
func (n *testNode) command(t *testing.T, kind, session string) Command {
	t.Helper()
	deadline := time.NewTimer(3 * time.Second)
	defer deadline.Stop()
	for {
		out := n.poll(t)
		for _, cmd := range out.Commands {
			if cmd.Kind == kind && (session == "" || cmd.SessionID == session) {
				return cmd
			}
		}
		select {
		case <-deadline.C:
			t.Fatalf("waiting for %s command for %s", kind, session)
		case <-time.After(time.Millisecond):
		}
	}
}
func (n *testNode) result(cmd Command) ResultRequest {
	return ResultRequest{NodeID: n.id, BootID: n.boot, CommandID: cmd.ID, SessionID: cmd.SessionID, AttemptID: cmd.AttemptID, Epoch: cmd.Epoch}
}
func (n *testNode) ack(t *testing.T, req ResultRequest) {
	t.Helper()
	if err := n.h.acceptResult(req, n.id); err != nil {
		t.Fatal(err)
	}
}

type acquireResult struct {
	s   *Session
	err error
}

func acquireAsync(h *Hub, ctx context.Context, r RunSpec) <-chan acquireResult {
	ch := make(chan acquireResult, 1)
	go func() { s, err := h.Acquire(ctx, r); ch <- acquireResult{s, err} }()
	return ch
}
func await[T any](t *testing.T, ch <-chan T) T {
	t.Helper()
	select {
	case v := <-ch:
		return v
	case <-time.After(3 * time.Second):
		t.Fatal("operation did not complete")
		var zero T
		return zero
	}
}
func openSession(t *testing.T, n *testNode, r RunSpec) *Session {
	t.Helper()
	ch := acquireAsync(n.h, context.Background(), r)
	cmd := n.command(t, CommandOpen, "")
	n.ack(t, n.result(cmd))
	v := await(t, ch)
	if v.err != nil {
		t.Fatal(v.err)
	}
	return v.s
}
func closeSession(t *testing.T, n *testNode, s *Session) {
	t.Helper()
	ch := make(chan error, 1)
	go func() { ch <- s.Close() }()
	cmd := n.command(t, CommandClose, s.ID())
	n.ack(t, n.result(cmd))
	if err := await(t, ch); err != nil {
		t.Fatal(err)
	}
}
func requestFor(n *testNode) *http.Request {
	r := httptest.NewRequest(http.MethodPut, "/files", nil)
	r.Header.Set("Authorization", "Bearer "+n.credential)
	return r
}

func TestRegistrationIsSingleUseHashedAndScoped(t *testing.T) {
	clock := &testClock{now: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	store := &MemoryStore{}
	h := newTestHub(t, store, clock)
	token, _, err := h.CreateRegistrationToken(context.Background(), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	req := RegisterRequest{Token: token, Name: "gpu-node", BootID: "first-boot", Capabilities: defaultCapabilities(), Protocol: ProtocolVersion}
	var reg RegisterResponse
	if got := protocolPOST(t, h, "/register", "", req, &reg); got != 201 {
		t.Fatal(got)
	}
	if got := protocolPOST(t, h, "/register", "", req, nil); got != 401 {
		t.Fatalf("token reused: %d", got)
	}
	snap, _ := store.Load(context.Background())
	if bytes.Contains(snap.Data, []byte(token)) || bytes.Contains(snap.Data, []byte(reg.Credential)) {
		t.Fatal("plaintext token or credential persisted")
	}
	r := httptest.NewRequest("GET", "/", nil)
	r.Header.Set("Authorization", "Bearer "+reg.Credential)
	if id, err := h.Authenticate(r); err != nil || id != reg.NodeID {
		t.Fatalf("authenticate %q %v", id, err)
	}
	if got := protocolPOST(t, h, "/poll", reg.Credential, PollRequest{NodeID: "other", BootID: req.BootID, Protocol: ProtocolVersion}, nil); got != 401 {
		t.Fatalf("identity impersonation: %d", got)
	}
	token, _, _ = h.CreateRegistrationToken(context.Background(), time.Second)
	clock.Advance(2 * time.Second)
	req.Token = token
	if got := protocolPOST(t, h, "/register", "", req, nil); got != 401 {
		t.Fatalf("expired token accepted: %d", got)
	}
	if got := protocolPOST(t, h, "/poll", "wrong", PollRequest{}, nil); got != 401 {
		t.Fatal(got)
	}
}

func TestCapacityMatchingHoldsExclusiveGPUAndThread(t *testing.T) {
	h := newTestHub(t, nil, nil)
	caps := defaultCapabilities()
	caps.Slots = 2
	caps.CPUMillis = 2000
	caps.MemoryMiB = 2048
	caps.GPUs = []GPU{{ID: "gpu0", Model: "A100", MemoryMiB: 40960}}
	n := registerNode(t, h, caps)
	r := spec("first")
	r.NodeID = n.id
	r.Labels = map[string]string{"os": "linux"}
	r.Resources = Resources{CPUMillis: 1000, MemoryMiB: 1024, GPUCount: 1, GPUModel: "a100", GPUMemoryMiB: 40000}
	s := openSession(t, n, r)
	if _, err := h.Acquire(context.Background(), r); !errors.Is(err, ErrThreadBusy) {
		t.Fatalf("duplicate thread: %v", err)
	}
	for name, change := range map[string]func(*RunSpec){
		"gpu competition":   func(r *RunSpec) {},
		"cpu capacity":      func(r *RunSpec) { r.Resources = Resources{CPUMillis: 1001} },
		"memory capacity":   func(r *RunSpec) { r.Resources = Resources{MemoryMiB: 1025} },
		"wrong label":       func(r *RunSpec) { r.Resources = Resources{}; r.Labels = map[string]string{"os": "windows"} },
		"wrong environment": func(r *RunSpec) { r.Resources = Resources{}; r.Environment = "missing" },
		"wrong node":        func(r *RunSpec) { r.Resources = Resources{}; r.NodeID = "missing" },
	} {
		t.Run(name, func(t *testing.T) {
			q := r
			q.ThreadID = name
			change(&q)
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
			defer cancel()
			if _, err := h.Acquire(ctx, q); !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("unexpected assignment: %v", err)
			}
		})
	}
	second := spec("second")
	second.Resources = Resources{CPUMillis: 1000, MemoryMiB: 1024}
	s2 := openSession(t, n, second)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, err := h.Acquire(ctx, spec("no slot")); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("slot overcommit: %v", err)
	}
	v := h.ListNodes()[0]
	if v.ActiveSessions != 2 || len(v.ReservedGPUs) != 1 || v.ReservedGPUs[0] != "gpu0" {
		t.Fatalf("bad capacity view: %+v", v)
	}
	closeSession(t, n, s)
	r.ThreadID = "reuse-gpu"
	s3 := openSession(t, n, r)
	closeSession(t, n, s2)
	closeSession(t, n, s3)
}

type callResult struct {
	result tool.Result
	err    error
}

func callAsync(s *Session, ctx context.Context, id, input string) <-chan callResult {
	ch := make(chan callResult, 1)
	go func() {
		r, e := s.Call(ctx, "Bash", json.RawMessage(input), &tool.ToolContext{ToolUseID: id})
		ch <- callResult{r, e}
	}()
	return ch
}

func TestToolCallsDeduplicateAndRejectChangedReplay(t *testing.T) {
	h := newTestHub(t, nil, nil)
	n := registerNode(t, h, defaultCapabilities())
	s := openSession(t, n, spec("dedup"))
	a := callAsync(s, context.Background(), "call-1", `{"id":9007199254740993,"command":"pwd"}`)
	cmd := n.command(t, CommandCall, s.ID())
	b := callAsync(s, context.Background(), "call-1", `{ "command":"pwd", "id":9007199254740993 }`)
	if !strings.Contains(string(cmd.Call.Input), "9007199254740993") {
		t.Fatal("JSON integer rounded")
	}
	_, err := s.Call(context.Background(), "Bash", json.RawMessage(`{"command":"rm"}`), &tool.ToolContext{ToolUseID: "call-1"})
	if !errors.Is(err, ErrCallConflict) {
		t.Fatalf("changed duplicate: %v", err)
	}
	result := n.result(cmd)
	result.Result = tool.Text("/workspace/task")
	n.ack(t, result)
	for _, ch := range []<-chan callResult{a, b} {
		v := await(t, ch)
		if v.err != nil || v.result.Flatten() != "/workspace/task" {
			t.Fatalf("call result %+v", v)
		}
	}
	n.ack(t, result)
	result.Result = tool.Text("altered")
	if err := h.acceptResult(result, n.id); !errors.Is(err, ErrCallConflict) {
		t.Fatalf("result replacement allowed: %v", err)
	}
	if _, err := s.Call(context.Background(), "undeclared", nil, nil); err == nil {
		t.Fatal("undeclared tool accepted")
	}
	if _, err := s.Call(context.Background(), "Bash", json.RawMessage(`{} {}`), nil); err == nil {
		t.Fatal("trailing JSON accepted")
	}
	h.mu.Lock()
	count := len(h.state.Sessions[s.ID()].CallIDs)
	h.mu.Unlock()
	if count != 1 {
		t.Fatalf("dedup created %d calls", count)
	}
}

func TestCancellationFencesLateResultsAndFilesUntilCleanup(t *testing.T) {
	h := newTestHub(t, nil, nil)
	caps := defaultCapabilities()
	caps.Slots = 1
	n := registerNode(t, h, caps)
	s := openSession(t, n, spec("cancelled"))
	ctx, cancel := context.WithCancel(context.Background())
	ch := callAsync(s, ctx, "call", "{}")
	cmd := n.command(t, CommandCall, s.ID())
	cancel()
	v := await(t, ch)
	var fatal interface{ FatalExecution() bool }
	if !errors.Is(v.err, ErrUnknownOutcome) || !errors.Is(v.err, context.Canceled) || !errors.As(v.err, &fatal) || !fatal.FatalExecution() {
		t.Fatalf("not fatal unknown cancellation: %v", v.err)
	}
	if err := h.acceptResult(n.result(cmd), n.id); !errors.Is(err, ErrFenced) {
		t.Fatalf("late result accepted: %v", err)
	}
	called := false
	if err := h.WithSessionWrite(requestFor(n), s.ID(), func() error { called = true; return nil }); !errors.Is(err, ErrFenced) || called {
		t.Fatalf("late file write: %v %v", err, called)
	}
	if h.ListNodes()[0].ActiveSessions != 1 {
		t.Fatal("capacity released before process cleanup")
	}
	closeCmd := n.command(t, CommandClose, s.ID())
	if !closeCmd.Abort {
		t.Fatal("cancelled attempt requested graceful sync")
	}
	if len(n.poll(t).Leases) != 0 {
		t.Fatal("fenced attempt lease renewed")
	}
	n.ack(t, n.result(closeCmd))
	if h.ListNodes()[0].ActiveSessions != 0 {
		t.Fatal("capacity not released after confirmed stop")
	}
}

func TestNormalCloseAllowsFinalSyncAndSerializesFence(t *testing.T) {
	h := newTestHub(t, nil, nil)
	n := registerNode(t, h, defaultCapabilities())
	s := openSession(t, n, spec("drain"))
	ch := make(chan error, 1)
	go func() { ch <- s.Close() }()
	cmd := n.command(t, CommandClose, s.ID())
	if cmd.Abort || len(n.poll(t).Leases) != 1 {
		t.Fatal("normal close lost its drain lease")
	}
	entered, release := make(chan struct{}), make(chan struct{})
	commit := make(chan error, 1)
	go func() {
		commit <- h.WithSessionWrite(requestFor(n), s.ID(), func() error { close(entered); <-release; return nil })
	}()
	<-entered
	acked := make(chan error, 1)
	go func() { acked <- h.acceptResult(n.result(cmd), n.id) }()
	select {
	case err := <-acked:
		t.Fatalf("Close ACK bypassed in-flight atomic commit: %v", err)
	case <-time.After(20 * time.Millisecond):
	}
	close(release)
	if err := await(t, commit); err != nil {
		t.Fatal(err)
	}
	if err := await(t, acked); err != nil {
		t.Fatal(err)
	}
	if err := await(t, ch); err != nil {
		t.Fatal(err)
	}
	if err := h.WithSessionWrite(requestFor(n), s.ID(), func() error { t.Fatal("post-close commit invoked"); return nil }); !errors.Is(err, ErrFenced) {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal("Close not idempotent", err)
	}
}

func TestRestartAndBootReplayRetainOldLeaseReservations(t *testing.T) {
	clock := &testClock{now: time.Now()}
	store := &MemoryStore{}
	h := newTestHub(t, store, clock)
	n := registerNode(t, h, defaultCapabilities())
	s := openSession(t, n, spec("stable-thread"))
	ch := callAsync(s, context.Background(), "unknown", "{}")
	oldcmd := n.command(t, CommandCall, s.ID())
	h2 := newTestHub(t, store, clock)
	n.h = h2
	if _, err := h2.Acquire(context.Background(), spec("stable-thread")); !errors.Is(err, ErrThreadBusy) {
		t.Fatalf("restart immediately replayed thread: %v", err)
	}
	if err := h2.acceptResult(n.result(oldcmd), n.id); !errors.Is(err, ErrFenced) {
		t.Fatalf("old epoch accepted: %v", err)
	}
	if _, _, err := h.poll(PollRequest{NodeID: n.id, BootID: n.boot, Protocol: ProtocolVersion}, n.id); !errors.Is(err, ErrStoreConflict) {
		t.Fatalf("old controller retained ownership: %v", err)
	}
	if v := await(t, ch); !errors.Is(v.err, ErrUnknownOutcome) {
		t.Fatalf("lost call not unknown: %v", v.err)
	}
	if len(n.poll(t).Leases) != 0 {
		t.Fatal("restart renewed old lease")
	}
	clock.Advance(6 * time.Second)
	n.poll(t)
	fresh := openSession(t, n, spec("stable-thread"))
	if fresh.AttemptID() == s.AttemptID() {
		t.Fatal("reused old attempt")
	}
	oldboot := n.boot
	n.boot = "replacement-boot"
	out := n.poll(t)
	if len(out.Leases) != 0 {
		t.Fatal("new boot inherited old session")
	}
	if _, _, err := h2.poll(PollRequest{NodeID: n.id, BootID: oldboot, Protocol: ProtocolVersion}, n.id); !errors.Is(err, ErrFenced) {
		t.Fatalf("retired boot replayed: %v", err)
	}
	if _, err := h2.Acquire(context.Background(), spec("stable-thread")); !errors.Is(err, ErrThreadBusy) {
		t.Fatalf("boot change released reservation early: %v", err)
	}
	clock.Advance(6 * time.Second)
	n.poll(t)
	openSession(t, n, spec("stable-thread"))
}

func TestLeaseExpiryAndUnknownStopNeverReleaseEarly(t *testing.T) {
	clock := &testClock{now: time.Now()}
	h := newTestHub(t, nil, clock)
	n := registerNode(t, h, defaultCapabilities())
	s := openSession(t, n, spec("expiry"))
	ch := callAsync(s, context.Background(), "pending", "{}")
	cmd := n.command(t, CommandCall, s.ID())
	clock.Advance(6 * time.Second)
	if err := h.acceptResult(n.result(cmd), n.id); !errors.Is(err, ErrFenced) {
		t.Fatalf("expired result accepted: %v", err)
	}
	if _, err := h.AuthorizeSession(requestFor(n), s.ID(), true); !errors.Is(err, ErrFenced) {
		t.Fatalf("expired file allowed: %v", err)
	}
	n.poll(t)
	if v := await(t, ch); !errors.Is(v.err, ErrUnknownOutcome) {
		t.Fatal(v.err)
	}
	s = openSession(t, n, spec("stop-unknown"))
	if err := h.abort(s.ID(), "cancel"); err != nil {
		t.Fatal(err)
	}
	cmd = n.command(t, CommandClose, s.ID())
	result := n.result(cmd)
	result.UnknownOutcome = true
	result.Error = "container stop timed out"
	n.ack(t, result)
	if h.ListNodes()[0].ActiveSessions != 1 {
		t.Fatal("unconfirmed cleanup released capacity")
	}
	clock.Advance(6 * time.Second)
	n.poll(t)
	if h.ListNodes()[0].ActiveSessions != 0 {
		t.Fatal("old lease did not expire")
	}
}

func TestMCPMetadataAndNotificationReplay(t *testing.T) {
	h := newTestHub(t, nil, nil)
	n := registerNode(t, h, defaultCapabilities())
	r := spec("mcp")
	r.MCPs = []MCPConfig{{Name: "browser", Transport: "stdio", Command: "browser"}}
	ch := acquireAsync(h, context.Background(), r)
	cmd := n.command(t, CommandOpen, "")
	res := n.result(cmd)
	res.MCPTools = []ToolDefinition{{Name: "mcp__other__read"}}
	if err := h.acceptResult(res, n.id); err == nil {
		t.Fatal("undeclared MCP accepted")
	}
	res.MCPTools = []ToolDefinition{{Name: "mcp__browser__read", InputSchema: map[string]any{"type": "object"}}}
	n.ack(t, res)
	opened := await(t, ch)
	if opened.err != nil {
		t.Fatal(opened.err)
	}
	s := opened.s
	if !s.Handles("mcp__browser__read") || s.Handles("mcp__other__read") {
		t.Fatal("incorrect remote MCP allowlist")
	}
	defs, err := s.MCPTools(context.Background())
	if err != nil || len(defs) != 1 {
		t.Fatalf("metadata %v %v", defs, err)
	}
	note := NotificationEnvelope{ID: "batch1", SessionID: s.ID(), AttemptID: s.AttemptID(), Items: []tool.Notification{{Status: "completed", TaskID: "background"}}}
	poll := PollRequest{NodeID: n.id, BootID: n.boot, Protocol: ProtocolVersion, Notifications: []NotificationEnvelope{note}}
	for i := 0; i < 2; i++ {
		out, _, err := h.poll(poll, n.id)
		if err != nil || len(out.AcknowledgedNotifications) != 1 {
			t.Fatalf("notification ack %+v %v", out, err)
		}
	}
	if notes := s.DrainNotifications(); len(notes) != 1 {
		t.Fatalf("replayed notifications: %v", notes)
	}
	if notes := s.DrainNotifications(); len(notes) != 0 {
		t.Fatalf("drain not consuming: %v", notes)
	}
}

type failureStore struct {
	MemoryStore
	mu   sync.Mutex
	fail bool
}

func (s *failureStore) Save(ctx context.Context, rev uint64, data json.RawMessage) (uint64, error) {
	s.mu.Lock()
	fail := s.fail
	s.fail = false
	s.mu.Unlock()
	if fail {
		return 0, errors.New("injected durable write failure")
	}
	return s.MemoryStore.Save(ctx, rev, data)
}
func (s *failureStore) failNext() { s.mu.Lock(); s.fail = true; s.mu.Unlock() }
func TestPersistenceFailureRollsBackTokenConsumptionAndCommands(t *testing.T) {
	store := &failureStore{}
	h := newTestHub(t, store, nil)
	token, _, _ := h.CreateRegistrationToken(context.Background(), time.Minute)
	req := RegisterRequest{Token: token, Name: "node", BootID: "boot", Capabilities: defaultCapabilities(), Protocol: ProtocolVersion}
	store.failNext()
	if code := protocolPOST(t, h, "/register", "", req, nil); code < 400 {
		t.Fatal("unpersisted registration succeeded")
	}
	if len(h.ListNodes()) != 0 {
		t.Fatal("failed registration leaked node")
	}
	var reg RegisterResponse
	if code := protocolPOST(t, h, "/register", "", req, &reg); code != 201 {
		t.Fatal("registration token consumed by failed persistence")
	}
	n := &testNode{h: h, id: reg.NodeID, credential: reg.Credential, boot: "boot"}
	s := openSession(t, n, spec("storefail"))
	store.failNext()
	if _, err := s.Call(context.Background(), "Bash", nil, &tool.ToolContext{ToolUseID: "never-dispatched"}); err == nil {
		t.Fatal("call accepted without durable command")
	}
	if cmds := n.poll(t).Commands; len(cmds) != 0 {
		t.Fatalf("unpersisted command leaked: %+v", cmds)
	}
}

func TestStateGuardSerializesControllerTakeoverWithFileCommit(t *testing.T) {
	store := &MemoryStore{}
	h := newTestHub(t, store, nil)
	n := registerNode(t, h, defaultCapabilities())
	s := openSession(t, n, spec("artifact-owner"))
	entered, release := make(chan struct{}), make(chan struct{})
	committed := make(chan error, 1)
	go func() {
		committed <- h.WithSessionWrite(requestFor(n), s.ID(), func() error {
			close(entered)
			<-release
			return nil
		})
	}()
	<-entered
	type hubResult struct {
		hub *Hub
		err error
	}
	taken := make(chan hubResult, 1)
	go func() { next, err := New(Options{Store: store}); taken <- hubResult{next, err} }()
	select {
	case got := <-taken:
		if got.hub != nil {
			_ = got.hub.Close()
		}
		t.Fatalf("takeover passed uncommitted file write: %v", got.err)
	case <-time.After(20 * time.Millisecond):
	}
	close(release)
	if err := await(t, committed); err != nil {
		t.Fatal(err)
	}
	next := await(t, taken)
	if next.err != nil {
		t.Fatal(next.err)
	}
	t.Cleanup(func() { _ = next.hub.Close() })
	called := false
	err := h.WithSessionWrite(requestFor(n), s.ID(), func() error { called = true; return nil })
	if !errors.Is(err, ErrStoreConflict) || called {
		t.Fatalf("superseded controller committed a file: called=%v err=%v", called, err)
	}
	if _, err := h.Authenticate(requestFor(n)); !errors.Is(err, ErrClosed) {
		t.Fatalf("superseded hub did not close after guard conflict: %v", err)
	}
}

func TestFailedCancellationPersistenceCannotRenewOrWrite(t *testing.T) {
	store := &failureStore{}
	h := newTestHub(t, store, nil)
	n := registerNode(t, h, defaultCapabilities())
	s := openSession(t, n, spec("durable-cancel"))
	ctx, cancel := context.WithCancel(context.Background())
	call := callAsync(s, ctx, "call", "{}")
	cmd := n.command(t, CommandCall, s.ID())
	store.failNext()
	cancel()
	if out := await(t, call); !errors.Is(out.err, ErrUnknownOutcome) {
		t.Fatal(out.err)
	}
	if _, _, err := h.poll(PollRequest{NodeID: n.id, BootID: n.boot, Protocol: ProtocolVersion}, n.id); !errors.Is(err, ErrClosed) {
		t.Fatalf("failed cancellation renewed lease: %v", err)
	}
	if err := h.acceptResult(n.result(cmd), n.id); !errors.Is(err, ErrClosed) {
		t.Fatalf("failed cancellation accepted late result: %v", err)
	}
	if err := h.WithSessionWrite(requestFor(n), s.ID(), func() error { t.Fatal("failed cancellation accepted file commit"); return nil }); !errors.Is(err, ErrClosed) {
		t.Fatal(err)
	}
	// The durable snapshot may still be active, but a replacement controller must
	// quarantine it until the last issued lease expires rather than replay it.
	next := newTestHub(t, store, nil)
	if _, err := next.Acquire(context.Background(), spec("durable-cancel")); !errors.Is(err, ErrThreadBusy) {
		t.Fatalf("unpersisted cancellation lost reservation: %v", err)
	}
}

func TestDisableIfIdleRejectsReservedAttemptAndFailedCommit(t *testing.T) {
	h := newTestHub(t, nil, nil)
	n := registerNode(t, h, defaultCapabilities())
	s := openSession(t, n, spec("disable"))
	if err := h.abort(s.ID(), "cancel"); err != nil {
		t.Fatal(err)
	}
	called := false
	if err := h.DisableIfIdle(func() error { called = true; return nil }); !errors.Is(err, ErrSessionsActive) || called {
		t.Fatalf("disabled before cleanup: %v", err)
	}
	cmd := n.command(t, CommandClose, s.ID())
	n.ack(t, n.result(cmd))
	injected := errors.New("configuration persistence failure")
	if err := h.DisableIfIdle(func() error { return injected }); !errors.Is(err, injected) || h.IsClosed() {
		t.Fatalf("failed commit changed admission: %v", err)
	}
	if err := h.DisableIfIdle(func() error { return nil }); err != nil || !h.IsClosed() {
		t.Fatalf("disable: %v", err)
	}
	if _, err := h.Acquire(context.Background(), spec("after-off")); !errors.Is(err, ErrClosed) {
		t.Fatalf("disabled broker admitted session: %v", err)
	}
}

func TestDisableIfIdleSerializesRacingAcquire(t *testing.T) {
	h := newTestHub(t, nil, nil)
	registerNode(t, h, defaultCapabilities())
	entered, release := make(chan struct{}), make(chan struct{})
	disabled := make(chan error, 1)
	go func() { disabled <- h.DisableIfIdle(func() error { close(entered); <-release; return nil }) }()
	<-entered
	acquired := acquireAsync(h, context.Background(), spec("racing-off"))
	close(release)
	if err := await(t, disabled); err != nil {
		t.Fatal(err)
	}
	if out := await(t, acquired); !errors.Is(out.err, ErrClosed) {
		t.Fatalf("Acquire passed a committing disable: %v", out.err)
	}
	if len(h.ListNodes()) != 1 || h.ListNodes()[0].ActiveSessions != 0 {
		t.Fatal("disabled broker retained a new reservation")
	}
}

func TestDrainingCanRenewBeyondOneLeaseAndStillCancel(t *testing.T) {
	h, err := New(Options{Store: &MemoryStore{}, LeaseDuration: time.Second, PollWait: time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = h.Close() })
	n := registerNode(t, h, defaultCapabilities())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	opening := acquireAsync(h, ctx, spec("large-final-artifact"))
	open := n.command(t, CommandOpen, "")
	n.ack(t, n.result(open))
	opened := await(t, opening)
	if opened.err != nil {
		t.Fatal(opened.err)
	}
	s := opened.s
	closed := make(chan error, 1)
	go func() { closed <- s.Close() }()
	cmd := n.command(t, CommandClose, s.ID())
	deadline := time.NewTimer(1300 * time.Millisecond)
	defer deadline.Stop()
	tick := time.NewTicker(50 * time.Millisecond)
	defer tick.Stop()
renew:
	for {
		select {
		case err := <-closed:
			t.Fatalf("final artifact synchronization was capped by one heartbeat lease: %v", err)
		case <-tick.C:
			out := n.poll(t)
			if len(out.Leases) != 1 {
				t.Fatal("draining lease was not renewed")
			}
		case <-deadline.C:
			break renew
		}
	}
	if _, err := h.AuthorizeSession(requestFor(n), s.ID(), true); err != nil {
		t.Fatalf("long drain lost file write permission: %v", err)
	}
	cancel()
	if err := await(t, closed); !errors.Is(err, ErrUnknownOutcome) {
		t.Fatalf("draining cancellation was ignored: %v", err)
	}
	if err := h.WithSessionWrite(requestFor(n), s.ID(), func() error { t.Fatal("cancelled drain wrote file"); return nil }); !errors.Is(err, ErrFenced) {
		t.Fatal(err)
	}
	// Even after cancellation, the same close command can release capacity only
	// when the node has actually stopped all processes.
	n.ack(t, n.result(cmd))
}

func TestCapabilityUpdatesRequireFreshBootAndPreserveReservation(t *testing.T) {
	clock := &testClock{now: time.Now()}
	h := newTestHub(t, nil, clock)
	n := registerNode(t, h, defaultCapabilities())
	s := openSession(t, n, spec("old-configuration"))
	next := defaultCapabilities()
	next.Slots = 1
	next.Labels = map[string]string{"os": "linux", "network": "gpu-lab"}
	next.Environments = []string{"gpu"}
	next.GPUs = []GPU{{ID: "gpu0", Model: "A100", MemoryMiB: 40960}}
	req := PollRequest{NodeID: n.id, BootID: n.boot, Protocol: ProtocolVersion, Capabilities: &next}
	if _, _, err := h.poll(req, n.id); err == nil {
		t.Fatal("same-boot resource mutation accepted")
	}
	if _, err := h.AuthorizeSession(requestFor(n), s.ID(), true); err != nil {
		t.Fatalf("invalid update changed session: %v", err)
	}
	oldBoot := n.boot
	n.boot = "fresh-capabilities"
	req.BootID = n.boot
	out, _, err := h.poll(req, n.id)
	if err != nil || len(out.Leases) != 0 {
		t.Fatalf("new boot: %+v %v", out, err)
	}
	view := h.ListNodes()[0]
	if view.Capabilities.Slots != 1 || view.Capabilities.Labels["network"] != "gpu-lab" || len(view.Capabilities.GPUs) != 1 || view.ActiveSessions != 1 {
		t.Fatalf("capability update lost reservation: %+v", view)
	}
	req.BootID = oldBoot
	if _, _, err := h.poll(req, n.id); !errors.Is(err, ErrFenced) {
		t.Fatalf("old boot reverted capabilities: %v", err)
	}
	clock.Advance(6 * time.Second)
	n.poll(t)
	r := spec("new-configuration")
	r.Environment = "gpu"
	r.Resources.GPUCount = 1
	r.Labels = next.Labels
	openSession(t, n, r)
}

func TestUnknownOutcomePreservesAcknowledgedFailureReason(t *testing.T) {
	h := newTestHub(t, nil, nil)
	n := registerNode(t, h, defaultCapabilities())
	s := openSession(t, n, spec("artifact-conflict"))
	call := callAsync(s, context.Background(), "executed", "{}")
	cmd := n.command(t, CommandCall, s.ID())
	result := n.result(cmd)
	result.Error = "publish shared/result.bin: file changed since the downloaded version"
	result.UnknownOutcome = true
	n.ack(t, result)
	got := await(t, call)
	if !errors.Is(got.err, ErrUnknownOutcome) || !strings.Contains(got.err.Error(), result.Error) {
		t.Fatalf("concrete remote failure lost after fencing: %v", got.err)
	}
}
