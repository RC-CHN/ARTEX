package integration

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime/pprof"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/RC-CHN/ARTEX/runner"
	"github.com/RC-CHN/ARTEX/runnerfiles"
	"github.com/RC-CHN/ARTEX/runnernode"
)

// Docker mode runs the manager itself in a read-only container. Kubernetes mode
// runs this test inside a dedicated namespace with a shared workspace PVC.
func TestManagedRunnerEnvironments(t *testing.T) {
	manager, namespace := os.Getenv("ARTEX_RUNNER_MANAGER_IMAGE"), os.Getenv("ARTEX_RUNNER_KUBE_NAMESPACE")
	if manager == "" && namespace == "" {
		t.Skip("set a manager image or Kubernetes test namespace")
	}
	image, binary := os.Getenv("ARTEX_RUNNER_DOCKER_IMAGE"), os.Getenv("ARTEX_RUNNER_BINARY")
	if image == "" || binary == "" {
		t.Fatal("set digest-pinned execution image and runner binary")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	hub, err := runner.New(runner.Options{Store: &runner.MemoryStore{}, LeaseDuration: 20 * time.Second, PollWait: 200 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = hub.Close() })
	files := runnerfiles.NewStore()
	mux := http.NewServeMux()
	mux.Handle("/runner/v1/files/", http.StripPrefix("/runner/v1/files", runnerfiles.Handler(files, func(req *http.Request, id string, write bool) (runnerfiles.Plan, error) {
		spec, err := hub.AuthorizeSession(req, id, write)
		if err != nil {
			return runnerfiles.Plan{}, err
		}
		var p runnerfiles.Plan
		err = json.Unmarshal(spec.Files, &p)
		return p, err
	}, hub.WithSessionWrite)))
	mux.Handle("/runner/v1/", http.StripPrefix("/runner/v1", hub.Handler()))
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	token, _, err := hub.CreateRegistrationToken(ctx, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	c := runnernode.Config{ServerURL: srv.URL, Name: "managed-fixture", DataDir: t.TempDir(), AllowInsecureHTTP: true, SandboxBinary: "/app/runner-bin/artex-runner", AllowedImages: []string{image}, Capabilities: runner.Capabilities{Slots: 2, CPUMillis: 2000, MemoryMiB: 2048, Environments: []string{"fixture"}}}
	if namespace != "" {
		// Native prewarming starts the image's runtime before task inputs exist.
		c.SandboxBinary = "/usr/local/bin/artex-runner"
		prefix := fmt.Sprintf("case-%d", time.Now().UnixNano())
		c.Executor = "kubernetes"
		c.WorkspaceDir = filepath.Join(os.Getenv("ARTEX_RUNNER_KUBE_WORKSPACE"), prefix)
		c.Kubernetes = runnernode.KubernetesConfig{Namespace: namespace, WorkspacePVC: os.Getenv("ARTEX_RUNNER_KUBE_PVC"), WorkspaceSubPath: prefix}
		if secret := os.Getenv("ARTEX_RUNNER_KUBE_PULL_SECRET"); secret != "" {
			c.Kubernetes.ImagePullSecrets = []string{secret}
		}
		node, err := runnernode.New(c)
		if err != nil {
			t.Fatal(err)
		}
		node.Log = log.New(os.Stderr, "native manager: ", log.LstdFlags)
		nodeCtx, stopNode := context.WithCancel(ctx)
		done := make(chan error, 1)
		go func() {
			err := node.Run(nodeCtx, token)
			if nodeCtx.Err() == nil {
				node.Log.Printf("manager stopped unexpectedly: %v", err)
			}
			done <- err
		}()
		t.Cleanup(func() {
			stopNode()
			select {
			case <-done:
			case <-time.After(35 * time.Second):
				t.Error("native manager failed to stop")
			}
		})
	} else {
		daemonDir := c.DataDir
		c.DataDir = "/runner-data"
		c.DockerHostDataDir = daemonDir
		conf := t.TempDir()
		raw, _ := json.Marshal(c)
		writeFile(t, filepath.Join(conf, "runner.json"), string(raw))
		writeFile(t, filepath.Join(conf, "token"), token)
		group, err := exec.Command("stat", "-c", "%g", "/var/run/docker.sock").Output()
		if err != nil {
			t.Fatal(err)
		}
		name := fmt.Sprintf("artex-manager-test-%d", time.Now().UnixNano())
		args := []string{"run", "--rm", "--name", name, "--network=host", "--read-only", "--cap-drop=ALL", "--security-opt=no-new-privileges", "--user", fmt.Sprintf("%d:%d", os.Getuid(), os.Getgid()), "--group-add", strings.TrimSpace(string(group)), "--tmpfs", "/tmp:rw,mode=1777", "--mount", "type=bind,source=" + daemonDir + ",target=/runner-data", "--mount", "type=bind,source=" + conf + ",target=/config,readonly", "--mount", "type=bind,source=/var/run/docker.sock,target=/var/run/docker.sock", "--entrypoint", "/usr/local/bin/artex-runner", manager, "-config", "/config/runner.json", "-token-file", "/config/token"}
		cmd := exec.CommandContext(ctx, "docker", args...)
		logs, err := os.CreateTemp("", "artex-managed-test-*.log")
		if err != nil {
			t.Fatal(err)
		}
		cmd.Stdout, cmd.Stderr = logs, logs
		if err = cmd.Start(); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			_ = exec.Command("docker", "stop", "--time", "30", name).Run()
			_ = cmd.Wait()
			_ = logs.Close()
			if t.Failed() {
				b, _ := os.ReadFile(logs.Name())
				t.Log(string(b))
			}
			_ = os.Remove(logs.Name())
		})
	}
	eventually(t, ctx, "managed node online", func() bool { return len(hub.ListNodes()) == 1 && hub.ListNodes()[0].Online })
	inputs, task := t.TempDir(), t.TempDir()
	if namespace == "" {
		copyExecutable(t, binary, filepath.Join(inputs, "artex-runner"))
	}
	writeFile(t, filepath.Join(inputs, "mcp.py"), pythonMCP)
	for i := 1; i <= 3; i++ {
		writeFile(t, filepath.Join(task, fmt.Sprintf("i%d", i), "seed"), "input")
	}
	podNames := func() string {
		t.Helper()
		out, err := exec.CommandContext(ctx, "kubectl", "-n", namespace, "get", "pods", "-l", "app.kubernetes.io/name=artex-sandbox", "-o", "jsonpath={range .items[*]}{.metadata.uid}{\"\\n\"}{end}").CombinedOutput()
		if err != nil {
			t.Fatalf("list Pod identities: %s %v", out, err)
		}
		return string(out)
	}
	acquire := func(i int) *runner.Session {
		t.Helper()
		own := fmt.Sprintf("i%d", i)
		plan := runnerfiles.Plan{Mounts: []runnerfiles.Mount{{Name: "task", Source: task, Target: "/app/task", Writable: []string{own, "shared"}}, {Name: "inputs", Source: inputs, Target: "/app/runner-bin"}}}
		raw, _ := json.Marshal(plan)
		spec := runner.RunSpec{ThreadID: own, Environment: "fixture", Image: image, WorkingDir: "/app/task/" + own, Resources: runner.Resources{CPUMillis: 500, MemoryMiB: 512, PIDs: 256}, Files: raw, Tools: []runner.ToolDefinition{{Name: "Bash", Kind: "builtin"}, {Name: "Read", Kind: "builtin"}, {Name: "Write", Kind: "builtin"}}, MCPs: []runner.MCPConfig{{Name: "fixture", Transport: "stdio", Command: "python3", Args: []string{"/app/runner-bin/mcp.py"}}}}
		var before string
		if namespace != "" && i%2 == 0 {
			hub.SetWarmProfiles([]runner.WarmProfile{{Environment: "fixture", Idle: 1}, runner.WarmProfileFromSpec(spec)})
			eventually(t, ctx, "clean matching prewarm Pod ready", func() bool {
				n := hub.ListNodes()[0]
				return n.PoolStatus != nil && n.PoolStatus.Idle == 1
			})
			before = podNames()
		} else {
			hub.SetWarmProfiles(nil)
		}
		start := time.Now()
		s, err := hub.Acquire(ctx, spec)
		if err != nil {
			t.Fatal(err)
		}
		if before != "" && before != podNames() {
			t.Fatal("warm acquisition replaced the prepared Pod")
		}
		t.Logf("acquire %s warm=%t elapsed=%s", own, before != "", time.Since(start))
		return s
	}
	a, b := acquire(1), acquire(2)
	call := func(s *runner.Session, command string) string {
		t.Helper()
		raw, _ := json.Marshal(map[string]any{"command": command})
		r, err := s.Call(ctx, "Bash", raw, nil)
		if err != nil || r.IsError {
			t.Fatalf("sandbox command: %v %s", err, r.Flatten())
		}
		return r.Flatten()
	}
	if os.Getenv("ARTEX_RUNNER_REPLY_STRESS") == "1" {
		var wg sync.WaitGroup
		for _, session := range []*runner.Session{a, b} {
			wg.Go(func() {
				for i := 0; i < 60; i++ {
					body := fmt.Sprintf("reply-%d-", i) + strings.Repeat("x", 8192+i*37)
					input, _ := json.Marshal(map[string]any{"command": fmt.Sprintf("printf '%%s\\000' '%s'", body)})
					body += "\x00"
					callCtx, stop := context.WithTimeout(ctx, 8*time.Second)
					result, err := session.Call(callCtx, "Bash", input, nil)
					stop()
					if err != nil || result.IsError || result.Flatten() != body {
						_ = pprof.Lookup("goroutine").WriteTo(os.Stderr, 2)
						t.Errorf("reply %d: err=%v error=%t got=%d want=%d", i, err, result.IsError, len(result.Flatten()), len(body))
						return
					}
				}
			})
		}
		wg.Wait()
		if t.Failed() {
			return
		}
		t.Log("120 binary RPC results with concurrent heartbeats and file checkpoints preserved exactly")
	}
	check := `set -eu; test ! -e /var/run/docker.sock; test ! -e /var/run/secrets/kubernetes.io/serviceaccount/token; test ! -e /runner-data/credentials.json; test ! -e "$HOME/dirty"; test ! -e /tmp/dirty; if touch /etc/artex-dirty 2>/dev/null; then exit 1; fi`
	call(a, check+`; printf first > "$HOME/dirty"; printf first > /tmp/dirty; printf published > result`)
	call(b, check+`; test ! -e /app/task/i2/result`)
	call(a, `set -eu; test "$(pwd -P)" = /app/task/i1; test "$PWD" = /app/task/i1; if touch /app/task/i2/forbidden 2>/dev/null; then exit 1; fi`)
	call(b, `set -eu; test "$(pwd -P)" = /app/task/i2; test "$PWD" = /app/task/i2; if touch /app/task/i1/forbidden 2>/dev/null; then exit 1; fi; test "$(cat /app/task/i1/result)" = published`)
	if got, err := os.ReadFile(filepath.Join(task, "i1", "result")); err != nil || string(got) != "published" {
		t.Fatalf("publication %q %v", got, err)
	}
	defs, err := b.MCPTools(ctx)
	if err != nil || len(defs) != 1 {
		t.Fatalf("MCP discovery %v %v", defs, err)
	}
	resourceCheck := `set -eu; if test -r /sys/fs/cgroup/cpu.max; then cat /sys/fs/cgroup/cpu.max /sys/fs/cgroup/memory.max; else cat /sys/fs/cgroup/cpu/cpu.cfs_quota_us /sys/fs/cgroup/memory/memory.limit_in_bytes; fi`
	beforeResources := call(a, resourceCheck)
	if resources := call(b, resourceCheck); resources != beforeResources {
		t.Fatalf("cold/warm cgroups differ: %s / %s", beforeResources, resources)
	}
	if err = hub.SetNodeLimits(hub.ListNodes()[0].ID, 1, 500, 512); err != nil {
		t.Fatal(err)
	}
	if after := call(a, resourceCheck); after != beforeResources {
		t.Fatalf("lowered pool quota changed active resources: before=%s after=%s", beforeResources, after)
	}
	waiting, cancelWait := context.WithTimeout(ctx, 150*time.Millisecond)
	_, err = hub.Acquire(waiting, runner.RunSpec{ThreadID: "queued", Environment: "fixture", WorkingDir: "/workspace"})
	cancelWait()
	if err == nil {
		t.Fatal("admitted above administrative limit")
	}
	if err = a.Close(); err != nil {
		t.Fatal(err)
	}
	if err = b.Close(); err != nil {
		t.Fatal(err)
	}
	cSession := acquire(3)
	call(cSession, check+`; test ! -e /app/task/i3/result`)
	if err = cSession.Close(); err != nil {
		t.Fatal(err)
	}
	if namespace != "" {
		// Alternate fresh warm/cold sessions repeatedly, keeping all historical
		// results in the broker while exercising one-slot scheduling and cleanup.
		for i := 4; i <= 9; i++ {
			s := acquire(i)
			call(s, check+`; printf clean > result; printf dirty > "$HOME/dirty"; printf dirty > /tmp/dirty`)
			if err := s.Close(); err != nil {
				t.Fatal(err)
			}
		}
		hub.SetWarmProfiles(nil)
	}
	eventually(t, ctx, "all execution reservations released", func() bool { return hub.ListNodes()[0].ActiveSessions == 0 })
	if namespace != "" {
		out, err := exec.CommandContext(ctx, "kubectl", "-n", namespace, "get", "pods", "-l", "app.kubernetes.io/name=artex-sandbox", "-o", "name").CombinedOutput()
		if err != nil || strings.TrimSpace(string(out)) != "" {
			t.Fatalf("native execution Pods leaked: %s %v", out, err)
		}
	}
	t.Log("manager isolation, fresh sequential environments, live concurrency drain, read-only system, absent credentials, MCP discovery, artifacts and cleanup passed")
}
