package runnernode

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/RC-CHN/ARTEX/norma/tool"
	"github.com/RC-CHN/ARTEX/runner"
	"github.com/RC-CHN/ARTEX/runnerfiles"
)

const testImage = "example.invalid/runtime@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

func TestMain(m *testing.M) {
	if os.Getenv("ARTEX_RUNNER_TEST_HELPER") == "1" {
		if ServeSandbox(context.Background(), os.Stdin, os.Stdout) != nil {
			os.Exit(2)
		}
		os.Exit(0)
	}
	os.Exit(m.Run())
}
func baseSpec(dir string) runner.RunSpec {
	return runner.RunSpec{Image: testImage, WorkingDir: dir, Tools: []runner.ToolDefinition{{Name: "Bash", Kind: "builtin"}, {Name: "Read", Kind: "builtin"}, {Name: "TaskList", Kind: "builtin"}}}
}
func local(t *testing.T) *LocalSession {
	t.Helper()
	s, e := OpenLocalSession(context.Background(), t.Name(), baseSpec(t.TempDir()))
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { s.Close() })
	return s
}
func TestDuplicateCallHasOneSideEffect(t *testing.T) {
	s := local(t)
	call := runner.ToolCall{ID: "stable-call", Name: "Bash", Input: json.RawMessage(`{"command":"printf x >> counter"}`)}
	var wg sync.WaitGroup
	for range 16 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r := s.Call(context.Background(), call)
			if r.Error != "" || r.Result.IsError {
				t.Errorf("call failed: %s %s", r.Error, r.Result.Flatten())
			}
		}()
	}
	wg.Wait()
	b, e := os.ReadFile(filepath.Join(s.spec.WorkingDir, "counter"))
	if e != nil || string(b) != "x" {
		t.Fatalf("duplicate execution: %q, %v", b, e)
	}
	call.Input = json.RawMessage(`{"command":"printf y >> counter"}`)
	if r := s.Call(context.Background(), call); r.Error != runner.ErrCallConflict.Error() {
		t.Fatalf("ID collision accepted: %+v", r)
	}
}
func TestSessionCloseStopsBackgroundProcess(t *testing.T) {
	s := local(t)
	r := s.Call(context.Background(), runner.ToolCall{ID: "bg", Name: "Bash", Input: json.RawMessage(`{"command":"while true; do printf x >> activity; sleep 0.02; done","run_in_background":true}`)})
	if r.Error != "" || r.Result.IsError {
		t.Fatalf("start failed: %+v", r)
	}
	file := filepath.Join(s.spec.WorkingDir, "activity")
	deadline := time.Now().Add(2 * time.Second)
	for {
		b, _ := os.ReadFile(file)
		if len(b) > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("background task never started")
		}
		time.Sleep(10 * time.Millisecond)
	}
	s.Close()
	before, _ := os.ReadFile(file)
	time.Sleep(100 * time.Millisecond)
	after, _ := os.ReadFile(file)
	if string(before) != string(after) {
		t.Fatal("background process survived session close")
	}
	if r := s.Call(context.Background(), runner.ToolCall{ID: "later", Name: "Read", Input: json.RawMessage(`{"file_path":"activity"}`)}); r.Error == "" {
		t.Fatal("closed session accepted a new call")
	}
}
func TestSandboxRPCEnvironmentAndSessionSeparation(t *testing.T) {
	t.Setenv("CONTROLLER_SECRET", "do-not-copy")
	p := &ProcessExecutor{Binary: os.Args[0], Env: []string{"ARTEX_RUNNER_TEST_HELPER=1"}}
	var boxes []Sandbox
	for range 2 {
		spec := baseSpec(t.TempDir())
		sb, e := p.Start(context.Background(), randomID(), spec, runnerfiles.Plan{}, nil, nil)
		if e != nil {
			t.Fatal(e)
		}
		boxes = append(boxes, sb)
		open, e := sb.Request(context.Background(), SandboxRequest{Method: "open", SessionID: randomID(), Spec: &spec})
		if e != nil || open.Error != "" {
			t.Fatalf("open: %+v %v", open, e)
		}
		r, e := sb.Request(context.Background(), SandboxRequest{Method: "call", Call: &runner.ToolCall{ID: "env", Name: "Bash", Input: json.RawMessage(`{"command":"printf '%s|%s|%s' \"$HOME\" \"$TMPDIR\" \"${CONTROLLER_SECRET-unset}\""}`)}})
		if e != nil || r.Error != "" || !strings.Contains(r.Result.Flatten(), spec.WorkingDir) || !strings.Contains(r.Result.Flatten(), "unset") {
			t.Fatalf("unsafe process env isolation: %+v %v", r, e)
		}
		_, _ = sb.Request(context.Background(), SandboxRequest{Method: "close"})
		_ = sb.Stop()
	}
}
func TestCustomCommandQuotesParameters(t *testing.T) {
	dir := t.TempDir()
	spec := baseSpec(dir)
	spec.Tools = []runner.ToolDefinition{{Name: "echo_custom", Kind: "command", InputSchema: map[string]any{"type": "object", "properties": map[string]any{"value": map[string]any{"type": "string"}}}, Exec: json.RawMessage(`{"command":"printf '%s' {value}"}`)}}
	s, e := OpenLocalSession(context.Background(), t.Name(), spec)
	if e != nil {
		t.Fatal(e)
	}
	defer s.Close()
	r := s.Call(context.Background(), runner.ToolCall{ID: "custom", Name: "echo_custom", Input: json.RawMessage(`{"value":"$(touch escaped); 'quoted'"}`)})
	if r.Error != "" || !strings.Contains(r.Result.Flatten(), "$(touch escaped)") {
		t.Fatalf("custom output: %+v", r)
	}
	if _, e := os.Stat(filepath.Join(dir, "escaped")); !os.IsNotExist(e) {
		t.Fatal("parameter became shell code")
	}
}
func TestAllowlistAndUnknownToolFailClosed(t *testing.T) {
	s := local(t)
	r := s.Call(context.Background(), runner.ToolCall{ID: "deny", Name: "Write", Input: json.RawMessage(`{}`)})
	if r.Error == "" {
		t.Fatal("non-allowlisted tool ran")
	}
	spec := baseSpec(t.TempDir())
	spec.Tools = []runner.ToolDefinition{{Name: "controller_only_tool"}}
	if other, e := OpenLocalSession(context.Background(), t.Name()+"2", spec); e == nil {
		other.Close()
		t.Fatal("unknown tool silently accepted")
	}
}

func config(t *testing.T) Config {
	t.Helper()
	return Config{ServerURL: "https://controller.invalid", Name: "test", DataDir: t.TempDir(), Capabilities: runner.Capabilities{Slots: 4, CPUMillis: 4000, MemoryMiB: 4096}}
}
func TestConfigPinningOverridesAndControllerLocalPaths(t *testing.T) {
	c := config(t)
	spec := baseSpec("/workspace")
	spec.Image = "runtime:latest"
	if _, _, e := c.Prepare(spec); e == nil {
		t.Fatal("mutable image accepted")
	}
	spec.Image = testImage
	spec.MCPs = []runner.MCPConfig{{Name: "browser", Transport: "http", URL: "http://127.0.0.1:8080/mcp"}}
	if _, _, e := c.Prepare(spec); e == nil {
		t.Fatal("controller loopback silently reused")
	}
	endpoint := "https://mcp.invalid/mcp"
	c.MCPOverrides = map[string]MCPOverride{"browser": {URL: &endpoint}}
	prepared, _, e := c.Prepare(spec)
	if e != nil || prepared.MCPs[0].URL != endpoint {
		t.Fatalf("override: %v", e)
	}
	spec.MCPs = nil
	spec.Env = map[string]string{"HTTP_PROXY": "http://localhost:8080"}
	if _, _, e = c.Prepare(spec); e == nil {
		t.Fatal("loopback proxy accepted")
	}
	c.Env = map[string]string{"HTTP_PROXY": "http://proxy.invalid:8080"}
	if _, _, e = c.Prepare(spec); e != nil {
		t.Fatal(e)
	}
}
func TestDockerIsolationArgumentsAndWritableOverlays(t *testing.T) {
	root := t.TempDir()
	spec := baseSpec("/home/test/project/tasks/one")
	plan := runnerfiles.Plan{Mounts: []runnerfiles.Mount{{Name: "data", Source: "/private-controller-source-not-on-node", Target: "/home/test/project", Writable: []string{"tasks/one"}}}}
	raw, _ := json.Marshal(plan)
	spec.Files = raw
	c := config(t)
	prepared, p, e := c.Prepare(spec)
	if e != nil {
		t.Fatal(e)
	}
	args, e := (&DockerExecutor{NodeID: "node"}).DockerArgs("session", prepared, p, map[string]string{"data": root}, []string{"GPU-1"})
	if e != nil {
		t.Fatal(e)
	}
	joined := strings.Join(args, " ")
	for _, want := range []string{"--read-only", "--cap-drop=ALL", "--security-opt=no-new-privileges", "--pids-limit 256", "--cpus 1.000", "--memory 1024m", "--tmpfs /tmp:", "--tmpfs /home/runner:", "--gpus", "target=/home/test/project,bind-propagation=rprivate,readonly", "target=/home/test/project/tasks/one,bind-propagation=rprivate"} {
		if !strings.Contains(joined, want) {
			t.Errorf("missing isolation option %q", want)
		}
	}
	for _, bad := range []string{plan.Mounts[0].Source, "docker.sock", "--privileged", "--network=host", "--pid=host"} {
		if strings.Contains(joined, bad) {
			t.Errorf("unsafe option %q", bad)
		}
	}
	if _, e = os.Stat(filepath.Join(root, "tasks", "one")); e != nil {
		t.Fatal(e)
	}
}
func TestForbiddenMountAndSymlinkWritableTarget(t *testing.T) {
	for _, target := range []string{"/", "/etc", "/usr/local/bin/artex-runner", "/tmp", "/home/runner", "/opt/artex-tools"} {
		raw, _ := json.Marshal(runnerfiles.Plan{Mounts: []runnerfiles.Mount{{Name: "x", Target: target}}})
		if _, e := decodePlan(raw); e == nil {
			t.Errorf("protected target accepted %s", target)
		}
	}
	root := t.TempDir()
	outside := t.TempDir()
	if e := os.Symlink(outside, filepath.Join(root, "link")); e != nil {
		t.Fatal(e)
	}
	if e := prepareWritable(root, filepath.Join(root, "link", "child")); e == nil {
		t.Fatal("writable host symlink accepted")
	}
}
func TestIdentityFilePermissionsAndTLSDefault(t *testing.T) {
	c := config(t)
	c.ServerURL = "http://controller.invalid"
	if e := c.Validate(); e == nil {
		t.Fatal("HTTP accepted without explicit unsafe flag")
	}
	c.AllowInsecureHTTP = true
	if e := c.Validate(); e != nil {
		t.Fatal(e)
	}
	file := filepath.Join(t.TempDir(), "credentials.json")
	want := identity{NodeID: "node", Credential: "secret-test", ServerURL: c.ServerURL}
	if e := saveIdentity(file, want); e != nil {
		t.Fatal(e)
	}
	st, _ := os.Stat(file)
	if st.Mode().Perm() != 0600 {
		t.Fatalf("credential mode: %o", st.Mode().Perm())
	}
	if _, e := loadIdentity(file); e != nil {
		t.Fatal(e)
	}
	_ = os.Chmod(file, 0644)
	if _, e := loadIdentity(file); e == nil {
		t.Fatal("world-readable credential file accepted")
	}
}

type fakeSandbox struct {
	calls   atomic.Int32
	stops   atomic.Int32
	gate    chan struct{}
	once    sync.Once
	stopErr error
}

func (s *fakeSandbox) Request(ctx context.Context, r SandboxRequest) (SandboxReply, error) {
	if r.Method == "call" {
		s.calls.Add(1)
		return SandboxReply{Result: tool.Text("executed")}, nil
	}
	return SandboxReply{}, nil
}
func (s *fakeSandbox) Stop() error {
	s.once.Do(func() {
		if s.gate != nil {
			<-s.gate
		}
		s.stops.Add(1)
	})
	return s.stopErr
}

type fakeExecutor struct {
	starts atomic.Int32
	box    *fakeSandbox
}

func (e *fakeExecutor) Start(context.Context, string, runner.RunSpec, runnerfiles.Plan, map[string]string, []string) (Sandbox, error) {
	e.starts.Add(1)
	return e.box, nil
}
func testNode(t *testing.T) (*Node, *fakeExecutor) {
	t.Helper()
	n, e := New(config(t))
	if e != nil {
		t.Fatal(e)
	}
	executor := &fakeExecutor{box: &fakeSandbox{}}
	n.Executor = executor
	n.Log = log.New(io.Discard, "", 0)
	return n, executor
}
func openNode(t *testing.T, n *Node) {
	t.Helper()
	now := time.Now()
	n.epoch = "epoch"
	n.leases["s"] = runner.Lease{SessionID: "s", AttemptID: "a", Epoch: "epoch", Until: now.Add(time.Minute)}
	spec := baseSpec("/workspace")
	r := n.executeCommand(context.Background(), runner.Command{ID: "open", Kind: runner.CommandOpen, SessionID: "s", AttemptID: "a", Epoch: "epoch", Spec: &spec})
	if r.Error != "" {
		t.Fatal(r.Error)
	}
}
func TestLeaseWatchdogStopsWithoutPolling(t *testing.T) {
	n, e := testNode(t)
	openNode(t, n)
	n.mu.Lock()
	l := n.leases["s"]
	l.Until = time.Now().Add(20 * time.Millisecond)
	n.leases["s"] = l
	n.mu.Unlock()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go n.watchdog(ctx)
	deadline := time.Now().Add(time.Second)
	for e.box.stops.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if e.box.stops.Load() != 1 {
		t.Fatal("lease expiry did not stop sandbox")
	}
}
func TestFencedCloseWaitsForRealStop(t *testing.T) {
	n, e := testNode(t)
	openNode(t, n)
	e.box.gate = make(chan struct{})
	n.applyPoll(context.Background(), runner.PollResponse{Epoch: "epoch", ServerTime: time.Now()}, time.Now())
	done := make(chan runner.ResultRequest, 1)
	go func() {
		done <- n.executeCommand(context.Background(), runner.Command{ID: "close", Kind: runner.CommandClose, SessionID: "s", AttemptID: "a", Epoch: "epoch", Abort: true})
	}()
	select {
	case <-done:
		t.Fatal("close ACK released a still-running sandbox")
	case <-time.After(40 * time.Millisecond):
	}
	close(e.box.gate)
	select {
	case r := <-done:
		if r.UnknownOutcome || r.Error != "" {
			t.Fatalf("close: %+v", r)
		}
	case <-time.After(time.Second):
		t.Fatal("close did not finish after stop")
	}
}
func TestStopFailureRemainsUnknown(t *testing.T) {
	n, e := testNode(t)
	openNode(t, n)
	e.box.stopErr = errors.New("Docker unavailable")
	r := n.executeCommand(context.Background(), runner.Command{ID: "close", Kind: runner.CommandClose, SessionID: "s", AttemptID: "a", Epoch: "epoch", Abort: true})
	if !r.UnknownOutcome {
		t.Fatal("failed Stop reported known termination")
	}
	if n.sessions["s"] == nil {
		t.Fatal("failed stop released local reservation")
	}
}
func TestLeaseDeadlineSubtractsNetworkRoundTrip(t *testing.T) {
	n, _ := testNode(t)
	sent := time.Now().Add(-3 * time.Second)
	server := time.Now()
	n.applyPoll(context.Background(), runner.PollResponse{Epoch: "epoch", ServerTime: server, Leases: []runner.Lease{{SessionID: "s", AttemptID: "a", Epoch: "epoch", Until: server.Add(30 * time.Second)}}}, sent)
	if !n.leases["s"].Until.Equal(sent.Add(30 * time.Second)) {
		t.Fatal("lease deadline ignored response latency")
	}
}
func TestNodeCommandRedeliveryDoesNotExecuteAgain(t *testing.T) {
	n, e := testNode(t)
	openNode(t, n)
	cmd := runner.Command{ID: "call-command", Kind: runner.CommandCall, SessionID: "s", AttemptID: "a", Epoch: "epoch", Call: &runner.ToolCall{ID: "tool-id", Name: "Bash", Input: json.RawMessage(`{"command":"true"}`)}}
	for range 20 {
		n.acceptCommand(context.Background(), cmd)
	}
	deadline := time.Now().Add(time.Second)
	for e.box.calls.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	for range 20 {
		n.acceptCommand(context.Background(), cmd)
	}
	time.Sleep(30 * time.Millisecond)
	if e.box.calls.Load() != 1 {
		t.Fatalf("redelivered command executed %d times", e.box.calls.Load())
	}
	n.stopAll()
}
func TestExclusiveGPUAndCPUAccounting(t *testing.T) {
	n, _ := testNode(t)
	n.Config.Capabilities.GPUs = []runner.GPU{{ID: "gpu0"}}
	a := &nodeSession{}
	b := &nodeSession{}
	n.sessions = map[string]*nodeSession{"a": a, "b": b}
	spec := baseSpec("/workspace")
	spec.Resources.GPUCount = 1
	if e := n.reserveResources(a, &spec, []string{"gpu0"}); e != nil {
		t.Fatal(e)
	}
	if e := n.reserveResources(b, &spec, []string{"gpu0"}); e == nil {
		t.Fatal("GPU assigned twice")
	}
	a.stopped = true
	if e := n.reserveResources(b, &spec, []string{"gpu0"}); e != nil {
		t.Fatal(e)
	}
}
func TestEnrollmentUsesPersistedCredentialAcrossBoots(t *testing.T) {
	var registrations atomic.Int32
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/runner/v1/register" {
			t.Errorf("unexpected route %s", r.URL.Path)
			http.NotFound(w, r)
			return
		}
		registrations.Add(1)
		var req runner.RegisterRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		if req.Token != "one-time" || req.BootID == "" || req.Protocol != runner.ProtocolVersion {
			t.Error("invalid registration")
		}
		_ = json.NewEncoder(w).Encode(runner.RegisterResponse{NodeID: "node", Credential: "persistent", Protocol: runner.ProtocolVersion})
	}))
	defer server.Close()
	c := config(t)
	c.ServerURL = server.URL
	n, e := New(c)
	if e != nil {
		t.Fatal(e)
	}
	n.HTTP = server.Client()
	if e = n.enroll(context.Background(), "one-time"); e != nil {
		t.Fatal(e)
	}
	next, e := New(c)
	if e != nil {
		t.Fatal(e)
	}
	next.HTTP = server.Client()
	if e = next.enroll(context.Background(), ""); e != nil {
		t.Fatal(e)
	}
	if registrations.Load() != 1 || next.identity.Credential != "persistent" || n.bootID == next.bootID {
		t.Fatal("credential recovery or boot identity failed")
	}
}

func TestNodeDirectoryHasSingleOwner(t *testing.T) {
	path := filepath.Join(t.TempDir(), "node.lock")
	first, e := lockNode(path)
	if e != nil {
		t.Fatal(e)
	}
	if second, e := lockNode(path); e == nil {
		second.Close()
		first.Close()
		t.Fatal("two node processes acquired one identity")
	}
	first.Close()
	third, e := lockNode(path)
	if e != nil {
		t.Fatal(e)
	}
	third.Close()
}
func TestLostResultAcknowledgmentRetriesSameOutcome(t *testing.T) {
	var requests atomic.Int32
	acknowledged := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var result runner.ResultRequest
		if e := json.NewDecoder(r.Body).Decode(&result); e != nil {
			t.Error(e)
		}
		if result.CommandID != "already-executed" || result.Result.Flatten() != "one-side-effect" {
			t.Error("retried result changed identity or content")
		}
		if requests.Add(1) == 1 {
			conn, _, e := w.(http.Hijacker).Hijack()
			if e != nil {
				t.Error(e)
				return
			}
			conn.Close()
			return
		}
		_ = json.NewEncoder(w).Encode(runner.ResultResponse{Accepted: true})
		select {
		case <-acknowledged:
		default:
			close(acknowledged)
		}
	}))
	defer server.Close()
	n, _ := testNode(t)
	n.Config.ServerURL = server.URL
	n.commands["already-executed"] = &commandState{queued: true, result: &runner.ResultRequest{CommandID: "already-executed", Result: tool.Text("one-side-effect")}}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go n.sendResults(ctx)
	select {
	case <-acknowledged:
	case <-time.After(2 * time.Second):
		t.Fatal("lost ACK was not retried")
	}
	if requests.Load() != 2 {
		t.Fatalf("expected two identical result deliveries, got %d", requests.Load())
	}
}
func TestFilesClientHasNoPollTimeout(t *testing.T) {
	n, _ := testNode(t)
	openNode(t, n)
	if n.HTTP.Timeout != 25*time.Second {
		t.Fatal("protocol timeout changed")
	}
	if n.sessions["s"].syncer.Client.HTTP.Timeout != 0 {
		t.Fatal("file transfer inherited polling timeout")
	}
	n.stopAll()
}
func TestCallEnvironmentUsesNodeOverrides(t *testing.T) {
	n, _ := testNode(t)
	call := runner.ToolCall{Env: []string{"HTTP_PROXY=http://127.0.0.1:8080", "HOME=/controller/private"}}
	if _, e := n.prepareCall(call); e == nil {
		t.Fatal("controller-local proxy accepted")
	}
	n.Config.Env = map[string]string{"HTTP_PROXY": "http://proxy.invalid:8080"}
	prepared, e := n.prepareCall(call)
	if e != nil {
		t.Fatal(e)
	}
	joined := strings.Join(prepared.Env, " ")
	if strings.Contains(joined, "HOME=") || strings.Contains(joined, "127.0.0.1") || !strings.Contains(joined, "proxy.invalid") {
		t.Fatal("node environment override failed")
	}
}

func TestAbortKeepsUnpublishedSnapshot(t *testing.T) {
	n, _ := testNode(t)
	openNode(t, n)
	dir := n.sessions["s"].dir
	path := filepath.Join(dir, "unpublished")
	if e := os.WriteFile(path, []byte("side effect"), 0600); e != nil {
		t.Fatal(e)
	}
	n.sessions["s"].mu.Lock()
	n.sessions["s"].syncErr = runnerfiles.ErrConflict
	n.sessions["s"].mu.Unlock()
	r := n.executeCommand(context.Background(), runner.Command{ID: "close", Kind: runner.CommandClose, SessionID: "s", AttemptID: "a", Epoch: "epoch", Abort: true})
	if r.UnknownOutcome {
		t.Fatal("fake sandbox did not stop")
	}
	if b, e := os.ReadFile(path); e != nil || string(b) != "side effect" {
		t.Fatal("fenced cleanup deleted unpublished files")
	}
}
