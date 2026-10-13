package integration

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"iter"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/RC-CHN/ARTEX/norma/agentcore"
	"github.com/RC-CHN/ARTEX/norma/harness"
	"github.com/RC-CHN/ARTEX/norma/llm"
	"github.com/RC-CHN/ARTEX/norma/permission"
	"github.com/RC-CHN/ARTEX/norma/tool"
	"github.com/RC-CHN/ARTEX/runner"
	"github.com/RC-CHN/ARTEX/runnerfiles"
	"github.com/RC-CHN/ARTEX/runnernode"
)

// Opt-in: build cmd/artex-runner with CGO_ENABLED=0, then point these variables
// at that binary and a locally available digest-pinned image containing Bash and
// Python 3. No registry publication or production infrastructure is involved.
func TestRunnerDockerExecutionAndIsolation(t *testing.T) {
	image, binary := os.Getenv("ARTEX_RUNNER_DOCKER_IMAGE"), os.Getenv("ARTEX_RUNNER_BINARY")
	if image == "" || binary == "" {
		t.Skip("set ARTEX_RUNNER_DOCKER_IMAGE and ARTEX_RUNNER_BINARY for real Docker integration")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	if output, err := exec.CommandContext(ctx, "docker", "image", "inspect", image).CombinedOutput(); err != nil {
		t.Fatalf("integration image unavailable: %v %s", err, output)
	}
	t.Setenv("ARTEX_CONTROLLER_TEST_SENTINEL", "must-stay-on-controller")
	state := &runner.MemoryStore{}
	hub, err := runner.New(runner.Options{Store: state, LeaseDuration: 20 * time.Second, PollWait: 200 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = hub.Close() })
	files := runnerfiles.NewStore()
	fileHandler := runnerfiles.Handler(files, func(req *http.Request, id string, write bool) (runnerfiles.Plan, error) {
		spec, err := hub.AuthorizeSession(req, id, write)
		if err != nil {
			return runnerfiles.Plan{}, err
		}
		var plan runnerfiles.Plan
		err = json.Unmarshal(spec.Files, &plan)
		return plan, err
	}, hub.WithSessionWrite)
	mux := http.NewServeMux()
	mux.Handle("/runner/v1/files/", http.StripPrefix("/runner/v1/files", fileHandler))
	mux.Handle("/runner/v1/", http.StripPrefix("/runner/v1", hub.Handler()))
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	inputDir := t.TempDir()
	copyExecutable(t, binary, filepath.Join(inputDir, "artex-runner"))
	writeFile(t, filepath.Join(inputDir, "mcp.py"), pythonMCP)
	taskDir := t.TempDir()
	const publishedDir = "/tmp/artex-published-task"
	writeFile(t, filepath.Join(taskDir, "i1", "seed"), "one")
	writeFile(t, filepath.Join(taskDir, "i2", "seed"), "two")
	hostOnly := filepath.Join(t.TempDir(), "host-only")
	writeFile(t, hostOnly, "controller-private-file")
	otherTask := filepath.Join(t.TempDir(), "other-task", "secret")
	writeFile(t, otherTask, "other-task-private")
	token, _, err := hub.CreateRegistrationToken(ctx, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	node, err := runnernode.New(runnernode.Config{ServerURL: srv.URL, Name: "docker-integration", DataDir: t.TempDir(),
		AllowInsecureHTTP: true, AllowedImages: []string{image}, SandboxBinary: "/app/runner-bin/artex-runner",
		Capabilities: runner.Capabilities{Slots: 2, Environments: []string{"fixture"}, CPUMillis: 2000, MemoryMiB: 1024}})
	if err != nil {
		t.Fatal(err)
	}
	node.Log = log.New(io.Discard, "", 0)
	// Fault injection only targets artifact transport. The actual tool command
	// must keep exactly one side effect while its committed upload loses an ACK.
	var lostArtifactACK atomic.Int32
	node.HTTP.Transport = runnerFaultTransport(func(req *http.Request) (*http.Response, error) {
		res, err := http.DefaultTransport.RoundTrip(req)
		if req.Method == http.MethodPut && req.URL.Query().Get("path") == "i1/retry-counter" && err == nil && res.StatusCode == http.StatusOK {
			if lostArtifactACK.Add(1) == 1 {
				_, _ = io.Copy(io.Discard, res.Body)
				res.Body.Close()
				return nil, io.ErrUnexpectedEOF
			}
		}
		return res, err
	})
	nodeCtx, stopNode := context.WithCancel(context.Background())
	nodeDone := make(chan error, 1)
	go func() { nodeDone <- node.Run(nodeCtx, token) }()
	t.Cleanup(func() {
		stopNode()
		select {
		case <-nodeDone:
		case <-time.After(20 * time.Second):
			t.Error("node shutdown did not finish")
		}
	})
	eventually(t, ctx, "runner registration", func() bool { return len(hub.ListNodes()) == 1 && hub.ListNodes()[0].Online })
	names := []string{"Bash", "Read", "Write", "Edit", "MultiEdit", "LS", "Glob", "Grep", "TaskOutput", "TaskStop", "TaskList", "Monitor"}
	definitions := make([]runner.ToolDefinition, 0, len(names))
	for _, name := range names {
		definitions = append(definitions, runner.ToolDefinition{Name: name, Kind: "builtin"})
	}
	sessions := make([]*runner.Session, 2)
	var wg sync.WaitGroup
	acquireErrors := make(chan error, 2)
	for i := range 2 {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			own := fmt.Sprintf("i%d", i+1)
			plan := runnerfiles.Plan{Mounts: []runnerfiles.Mount{
				{Name: "task", Source: taskDir, Target: "/app/data/task", Writable: []string{own, "shared"}},
				{Name: "published", Source: taskDir, Target: publishedDir},
				{Name: "inputs", Source: inputDir, Target: "/app/runner-bin"},
			}}
			raw, _ := json.Marshal(plan)
			spec := runner.RunSpec{ThreadID: "docker-" + own, TaskID: "same-task", AgentKey: "worker", Environment: "fixture", Image: image, WorkingDir: "/app/data/task/" + own,
				Resources: runner.Resources{CPUMillis: 500, MemoryMiB: 256, PIDs: 64}, Tools: definitions, Files: raw,
				MCPs: []runner.MCPConfig{{Name: "fixture", Transport: "stdio", Command: "python3", Args: []string{"/app/runner-bin/mcp.py"}, Env: map[string]string{"FIXTURE_VALUE": own}, WorkingDir: "/app/data/task/" + own}}}
			s, err := hub.Acquire(ctx, spec)
			sessions[i] = s
			acquireErrors <- err
		}(i)
	}
	wg.Wait()
	close(acquireErrors)
	for err := range acquireErrors {
		if err != nil {
			t.Fatal(err)
		}
	}
	a := newDockerSDK(t, ctx, sessions[0], "/app/data/task/i1")
	b := newDockerSDK(t, ctx, sessions[1], "/app/data/task/i2")
	t.Cleanup(func() { _ = a.session.Close(); _ = b.session.Close() })
	mustDockerCall(t, ctx, a, "Bash", map[string]any{"command": "printf x >> retry-counter"})
	if content, err := os.ReadFile(filepath.Join(taskDir, "i1", "retry-counter")); err != nil || string(content) != "x" || lostArtifactACK.Load() != 2 {
		t.Fatalf("artifact retry changed/replayed execution: content=%q uploads=%d err=%v", content, lostArtifactACK.Load(), err)
	}
	// Reproduce live output files spanning several checkpoints. A changing
	// source is deferred, not a reason to cancel the entire execution environment.
	writer := `import pathlib,time
p=pathlib.Path('changing.bin')
end=time.monotonic()+7
i=0
while time.monotonic()<end:
 p.write_bytes(bytes([i%256])*(256*1024))
 i+=1
 time.sleep(0.002)
p.write_bytes(b'FINAL-STABLE-OUTPUT')
pathlib.Path('writer-complete').write_text('done')`
	writerStart := mustDockerCall(t, ctx, a, "Bash", map[string]any{"command": "python3 -c " + shellQuote(writer), "run_in_background": true})
	writerTask := regexp.MustCompile(`task_[0-9]+`).FindString(writerStart.Flatten())
	if writerTask == "" {
		t.Fatalf("writer did not start: %s", writerStart.Flatten())
	}
	lastWriterOutput := ""
	eventually(t, ctx, "live-file writer survives checkpoints", func() bool {
		result, err := a.call(ctx, "Bash", map[string]any{"command": "if test -f writer-complete; then cat changing.bin; else printf pending; fi"})
		if err != nil {
			t.Fatalf("live-file checkpoint cancelled the environment: %v", err)
		}
		output := mustDockerCall(t, ctx, a, "TaskOutput", map[string]any{"task_id": writerTask}).Flatten()
		if output != lastWriterOutput {
			t.Logf("live writer: %s; observer: %s", output, result.Flatten())
			lastWriterOutput = output
		}
		if strings.Contains(output, "| failed") {
			memory := mustDockerCall(t, ctx, a, "Bash", map[string]any{"command": "cat /sys/fs/cgroup/memory.events"})
			t.Fatalf("writer failed: %s; cgroup: %s", output, memory.Flatten())
		}
		return strings.Contains(result.Flatten(), "FINAL-STABLE-OUTPUT")
	})
	eventually(t, ctx, "stable final file is published", func() bool {
		content, _ := os.ReadFile(filepath.Join(taskDir, "i1", "changing.bin"))
		return string(content) == "FINAL-STABLE-OUTPUT"
	})
	containers := []string{containerFor(t, ctx, sessions[0].ID()), containerFor(t, ctx, sessions[1].ID())}
	if containers[0] == containers[1] {
		t.Fatal("two intent sessions share one container")
	}
	// Both sessions hold real long-running child commands concurrently. The SDK's
	// auto-added TaskList/TaskOutput must resolve their remote manager, not a local one.
	taskIDs := make([]string, 2)
	for i, driver := range []*dockerSDK{a, b} {
		result := mustDockerCall(t, ctx, driver, "Bash", map[string]any{"command": `printf 'BACKGROUND_READY\n'; python3 -c 'import time; time.sleep(300)'`, "run_in_background": true})
		taskIDs[i] = regexp.MustCompile(`task_[0-9]+`).FindString(result.Flatten())
		if taskIDs[i] == "" {
			t.Fatalf("no remote task id: %s", result.Flatten())
		}
	}
	for i, driver := range []*dockerSDK{a, b} {
		result := mustDockerCall(t, ctx, driver, "TaskList", map[string]any{})
		if !strings.Contains(result.Flatten(), "running") {
			t.Fatalf("background command not live: %s", result.Flatten())
		}
		eventually(t, ctx, "remote background output", func() bool {
			result, err := driver.call(ctx, "TaskOutput", map[string]any{"task_id": taskIDs[i]})
			return err == nil && strings.Contains(result.Flatten(), "BACKGROUND_READY")
		})
	}
	isolation := "test ! -e " + shellQuote(hostOnly) + " && test ! -e " + shellQuote(otherTask) + " && test ! -e /var/run/docker.sock && test -z \"${ARTEX_CONTROLLER_TEST_SENTINEL-}\""
	mustDockerCall(t, ctx, a, "Bash", map[string]any{"command": isolation + ` && printf A > "$HOME/same-name" && printf A > /tmp/same-name && printf 'artifact-from-A' > artifact.txt`})
	mustDockerCall(t, ctx, b, "Bash", map[string]any{"command": isolation + ` && test ! -e "$HOME/same-name" && test ! -e /tmp/same-name && printf B > "$HOME/same-name" && printf B > /tmp/same-name`})
	for i, driver := range []*dockerSDK{a, b} {
		want := []string{"AA", "BB"}[i]
		result := mustDockerCall(t, ctx, driver, "Bash", map[string]any{"command": `cat "$HOME/same-name" /tmp/same-name`})
		if strings.TrimSpace(result.Flatten()) != want {
			t.Fatalf("HOME/tmp not isolated: %q", result.Flatten())
		}
	}
	local, err := os.ReadFile(filepath.Join(taskDir, "i1", "artifact.txt"))
	if err != nil || string(local) != "artifact-from-A" {
		t.Fatalf("artifact not published: %q %v", local, err)
	}
	peer := mustDockerCall(t, ctx, b, "Read", map[string]any{"file_path": "/app/data/task/i1/artifact.txt"})
	if !strings.Contains(peer.Flatten(), "artifact-from-A") {
		t.Fatalf("peer artifact not refreshed: %s", peer.Flatten())
	}
	denied, err := b.call(ctx, "Write", map[string]any{"file_path": "/app/data/task/i1/artifact.txt", "content": "overwrite peer"})
	if err != nil || !denied.IsError {
		t.Fatalf("peer readonly mount not enforced: %+v %v", denied, err)
	}
	// Writable snapshots must never be refreshed over background writes. A second,
	// read-only mount of the same controller directory provides the latest published
	// peer files, even in shared/ where both workers have write authority.
	mustDockerCall(t, ctx, a, "Write", map[string]any{"file_path": "/app/data/task/shared/from-A", "content": "shared-A"})
	mustDockerCall(t, ctx, b, "Write", map[string]any{"file_path": "/app/data/task/shared/from-B", "content": "shared-B"})
	for i, driver := range []*dockerSDK{a, b} {
		peerName := []string{"B", "A"}[i]
		result := mustDockerCall(t, ctx, driver, "Bash", map[string]any{"command": "test ! -e /app/data/task/shared/from-" + peerName + " && cat " + publishedDir + "/shared/from-" + peerName})
		if strings.TrimSpace(result.Flatten()) != "shared-"+peerName {
			t.Fatalf("published peer file missing or writable snapshot replaced: %q", result.Flatten())
		}
	}
	denied, err = a.call(ctx, "Write", map[string]any{"file_path": publishedDir + "/shared/from-B", "content": "overwrite published"})
	if err != nil || !denied.IsError {
		t.Fatalf("published mount is writable: %+v %v", denied, err)
	}
	for i, driver := range []*dockerSDK{a, b} {
		result := mustDockerCall(t, ctx, driver, "mcp__fixture__inspect", map[string]any{})
		text := result.Flatten()
		if !strings.Contains(text, fmt.Sprintf("/app/data/task/i%d", i+1)) || !strings.Contains(text, fmt.Sprintf(`"value": "i%d"`, i+1)) || strings.Contains(text, "must-stay-on-controller") {
			t.Fatalf("MCP cwd/environment not isolated: %s", text)
		}
	}
	if err := a.session.Close(); err != nil {
		snapshot, _ := state.Load(ctx)
		var stored struct {
			Sessions map[string]struct{ State, Reason string }
		}
		_ = json.Unmarshal(snapshot.Data, &stored)
		t.Logf("closed session diagnostic: %+v", stored.Sessions[sessions[0].ID()])
		t.Fatalf("normal SDK close: %v", err)
	}
	eventually(t, ctx, "closed sandbox and its background command removed", func() bool { return containerRemoved(ctx, containers[0]) })
	// A planner retains the legacy whole-workspace write scope, so its ordinary
	// snapshot cannot be refreshed safely. It sees later worker output via the
	// published mirror. Native absolute paths below /home and /tmp remain usable.
	const nativeDir = "/home/artex-native/data/task"
	plannerPlan, _ := json.Marshal(runnerfiles.Plan{Mounts: []runnerfiles.Mount{
		{Name: "task", Source: taskDir, Target: nativeDir, Writable: []string{"."}},
		{Name: "published", Source: taskDir, Target: publishedDir},
		{Name: "inputs", Source: inputDir, Target: "/app/runner-bin"},
	}})
	plannerRuntime, err := hub.Acquire(ctx, runner.RunSpec{ThreadID: "docker-planner", TaskID: "same-task", AgentKey: "planner", Environment: "fixture", Image: image, WorkingDir: nativeDir,
		Resources: runner.Resources{CPUMillis: 500, MemoryMiB: 256, PIDs: 64}, Tools: definitions, Files: plannerPlan})
	if err != nil {
		t.Fatal(err)
	}
	planner := newDockerSDK(t, ctx, plannerRuntime, nativeDir)
	t.Cleanup(func() { _ = planner.session.Close() })
	plannerContainer := containerFor(t, ctx, plannerRuntime.ID())
	mustDockerCall(t, ctx, b, "Write", map[string]any{"file_path": "/app/data/task/i2/after-planner-started", "content": "latest-for-planner"})
	plannerView := mustDockerCall(t, ctx, planner, "Bash", map[string]any{"command": "test \"$PWD\" = " + nativeDir + " && test ! -e i2/after-planner-started && cat " + publishedDir + "/i2/after-planner-started"})
	if strings.TrimSpace(plannerView.Flatten()) != "latest-for-planner" {
		t.Fatalf("planner did not observe newly published output: %s", plannerView.Flatten())
	}
	if err := planner.session.Close(); err != nil {
		t.Fatalf("planner close: %v", err)
	}
	eventually(t, ctx, "planner sandbox removed", func() bool { return containerRemoved(ctx, plannerContainer) })
	// A canceled in-flight command fences the attempt and tears down its container,
	// including its already-running background command, without an automatic replay.
	callCtx, stopCall := context.WithCancel(ctx)
	defer stopCall()
	callDone := make(chan error, 1)
	go func() {
		_, err := b.call(callCtx, "Bash", map[string]any{"command": `printf started > cancel-started; python3 -c 'import time; time.sleep(300)'`})
		callDone <- err
	}()
	eventually(t, ctx, "foreground command actually started", func() bool {
		return exec.CommandContext(ctx, "docker", "exec", containers[1], "test", "-f", "/app/data/task/i2/cancel-started").Run() == nil
	})
	stopCall()
	select {
	case err := <-callDone:
		if !errors.Is(err, context.Canceled) && !errors.Is(err, runner.ErrUnknownOutcome) {
			t.Fatalf("canceled run lost cause: %v", err)
		}
	case <-ctx.Done():
		t.Fatal("canceled SDK call did not return")
	}
	eventually(t, ctx, "canceled sandbox and all children removed", func() bool { return containerRemoved(ctx, containers[1]) })
	t.Log("real Docker sandboxes: two parallel workers, SDK/background tools, shared published mirrors, planner whole-workspace snapshot, native /home and /tmp paths, isolation, stdio MCP, close and cancellation passed")
}

type runnerFaultTransport func(*http.Request) (*http.Response, error)

func (f runnerFaultTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

type dockerSDK struct {
	session *agentcore.Session
	next    *llm.ContentBlock
	seq     int
}

func newDockerSDK(t *testing.T, ctx context.Context, runtime *runner.Session, cwd string) *dockerSDK {
	t.Helper()
	driver := &dockerSDK{}
	defs, err := runtime.MCPTools(ctx)
	if err != nil {
		t.Fatal(err)
	}
	tools := tool.DefaultTools()
	for _, def := range defs {
		tools = append(tools, tool.Build(tool.Spec{Name: def.Name, Description: def.Description, Schema: def.InputSchema, Permissions: func(context.Context, json.RawMessage, permission.Context) permission.Decision {
			return permission.Allowed()
		}, Run: func(context.Context, json.RawMessage, *tool.ToolContext) (tool.Result, error) {
			return tool.Result{}, errors.New("MCP unexpectedly executed on controller")
		}}))
	}
	driver.session = agentcore.NewSession(agentcore.Options{Runtime: runtime, Tools: tools, WorkingDir: cwd, PermissionMode: permission.ModeBypass, MaxTurns: 3,
		Deps: harness.QueryDeps{CallModel: func(context.Context, llm.CompletionRequest) iter.Seq2[llm.StreamEvent, error] {
			return func(yield func(llm.StreamEvent, error) bool) {
				events := []llm.StreamEvent{{Type: llm.SETextDelta, Text: "done"}, {Type: llm.SEMessageDelta, StopReason: "end_turn"}, {Type: llm.SEMessageStop}}
				if driver.next != nil {
					call := driver.next
					driver.next = nil
					events = []llm.StreamEvent{{Type: llm.SEToolUseStart, ToolID: call.ID, ToolName: call.Name}, {Type: llm.SEToolInputJSON, Text: string(call.Input)}, {Type: llm.SEMessageDelta, StopReason: "tool_use"}, {Type: llm.SEMessageStop}}
				}
				for _, event := range events {
					if !yield(event, nil) {
						return
					}
				}
			}
		}}})
	return driver
}
func (d *dockerSDK) call(ctx context.Context, name string, input any) (tool.Result, error) {
	raw, _ := json.Marshal(input)
	d.seq++
	d.next = &llm.ContentBlock{ID: fmt.Sprintf("fixture-%d", d.seq), Name: name, Input: raw}
	var result tool.Result
	var callErr error
	for event, err := range d.session.Prompt(ctx, "run one controlled fixture tool") {
		if err != nil {
			callErr = err
		}
		if event.ToolResult != nil {
			result = tool.Result{Content: event.ToolResult.Content, IsError: event.ToolResult.IsError}
		}
		if event.Terminal != nil && event.Terminal.Err != nil {
			callErr = event.Terminal.Err
		}
	}
	return result, callErr
}
func mustDockerCall(t *testing.T, ctx context.Context, d *dockerSDK, name string, input any) tool.Result {
	t.Helper()
	r, err := d.call(ctx, name, input)
	if err != nil || r.IsError {
		t.Fatalf("%s failed: %v %s", name, err, r.Flatten())
	}
	return r
}
func eventually(t *testing.T, ctx context.Context, what string, check func() bool) {
	t.Helper()
	timer := time.NewTimer(30 * time.Second)
	defer timer.Stop()
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()
	for {
		if check() {
			return
		}
		select {
		case <-ticker.C:
		case <-timer.C:
			t.Fatalf("timed out: %s", what)
		case <-ctx.Done():
			t.Fatalf("%s: %v", what, ctx.Err())
		}
	}
}
func containerFor(t *testing.T, ctx context.Context, id string) string {
	t.Helper()
	sum := sha256.Sum256([]byte(id))
	out, err := exec.CommandContext(ctx, "docker", "ps", "-q", "--filter", fmt.Sprintf("label=site.artex.session=%x", sum[:12])).Output()
	ids := strings.Fields(string(out))
	if err != nil || len(ids) != 1 {
		t.Fatalf("container lookup: %v %q", err, out)
	}
	return ids[0]
}
func containerRemoved(ctx context.Context, id string) bool {
	output, err := exec.CommandContext(ctx, "docker", "ps", "-aq", "--filter", "id="+id).Output()
	return err == nil && strings.TrimSpace(string(output)) == ""
}
func shellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }
func writeFile(t *testing.T, p, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(body), 0644); err != nil {
		t.Fatal(err)
	}
}
func copyExecutable(t *testing.T, src, dst string) {
	t.Helper()
	in, err := os.Open(src)
	if err != nil {
		t.Fatal(err)
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0755)
	if err != nil {
		t.Fatal(err)
	}
	_, err = io.Copy(out, in)
	closeErr := out.Close()
	if err != nil || closeErr != nil {
		t.Fatalf("copy runner binary: %v %v", err, closeErr)
	}
}

const pythonMCP = `import json, os, sys
for line in sys.stdin:
    request=json.loads(line)
    if "id" not in request:
        continue
    method=request.get("method")
    if method == "initialize":
        result={"protocolVersion":"2024-11-05","capabilities":{"tools":{}},"serverInfo":{"name":"fixture","version":"1"}}
    elif method == "tools/list":
        result={"tools":[{"name":"inspect","description":"Inspect controlled fixture environment","inputSchema":{"type":"object"}}]}
    elif method == "tools/call":
        text=json.dumps({"cwd":os.getcwd(),"home":os.getenv("HOME"),"value":os.getenv("FIXTURE_VALUE"),"controller_secret":os.getenv("ARTEX_CONTROLLER_TEST_SENTINEL")})
        result={"content":[{"type":"text","text":text}]}
    else:
        result={}
    print(json.dumps({"jsonrpc":"2.0","id":request["id"],"result":result}), flush=True)
`
