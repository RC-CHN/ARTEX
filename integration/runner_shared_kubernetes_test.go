package integration

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/RC-CHN/ARTEX/runner"
	"github.com/RC-CHN/ARTEX/runnerfiles"
	"github.com/RC-CHN/ARTEX/runnernode"
)

// Opt-in, dedicated namespace/PVC only. This test never connects to an ARTEX
// deployment or reads its task data; the controller and all inputs are synthetic.
func TestSharedKubernetesNoHistorySync(t *testing.T) {
	ns := os.Getenv("ARTEX_SHARED_TEST_NAMESPACE")
	if ns == "" {
		t.Skip("set ARTEX_SHARED_TEST_NAMESPACE, ARTEX_SHARED_TEST_ROOT/PVC/IMAGE/BINARY for isolated K3s validation")
	}
	root, pvc, image, binary := os.Getenv("ARTEX_SHARED_TEST_ROOT"), os.Getenv("ARTEX_SHARED_TEST_PVC"), os.Getenv("ARTEX_SHARED_TEST_IMAGE"), os.Getenv("ARTEX_SHARED_TEST_BINARY")
	if root == "" || pvc == "" || image == "" || binary == "" {
		t.Fatal("incomplete dedicated fixture configuration")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 8*time.Minute)
	defer cancel()
	prefix := fmt.Sprintf("shared-%d", time.Now().UnixNano())
	fixture := filepath.Join(root, prefix)
	task := filepath.Join(fixture, "tasks", "synthetic")
	for _, dir := range []string{"i1", "i2", "shared", "history"} {
		if err := os.MkdirAll(filepath.Join(task, dir), 0770); err != nil {
			t.Fatal(err)
		}
	}
	writeFile(t, filepath.Join(fixture, "tasks", "other", "private"), "other synthetic task")
	binDir := filepath.Join(fixture, "bin")
	os.MkdirAll(binDir, 0770)
	copyExecutable(t, binary, filepath.Join(binDir, "artex-runner"))
	payload := strings.Repeat("synthetic-history\n", 288) // about 112 MiB, 24,000 files
	seeded := time.Now()
	for i := range 24000 {
		if err := os.WriteFile(filepath.Join(task, "history", fmt.Sprintf("%05d", i)), []byte(payload), 0644); err != nil {
			t.Fatal(err)
		}
	}
	t.Logf("seeded 24000 synthetic files, %d bytes in %s", 24000*len(payload), time.Since(seeded))
	hub, err := runner.New(runner.Options{Store: &runner.MemoryStore{}, LeaseDuration: 30 * time.Second, PollWait: 200 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { hub.Close() })
	var transfers atomic.Int64
	mux := http.NewServeMux()
	mux.HandleFunc("/runner/v1/files/", func(w http.ResponseWriter, r *http.Request) {
		transfers.Add(1)
		http.Error(w, "unexpected snapshot request", 500)
	})
	mux.Handle("/runner/v1/", http.StripPrefix("/runner/v1", hub.Handler()))
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	cfg := runnernode.Config{ServerURL: srv.URL, Name: "shared-fixture", DataDir: t.TempDir(), WorkspaceDir: filepath.Join(fixture, "private"), Executor: "kubernetes", AllowInsecureHTTP: true, SandboxBinary: "/validation-bin/artex-runner", AllowedImages: []string{image}, Capabilities: runner.Capabilities{Slots: 2, CPUMillis: 2000, MemoryMiB: 2048, Environments: []string{"fixture"}}, Kubernetes: runnernode.KubernetesConfig{Namespace: ns, WorkspacePVC: pvc, WorkspaceSubPath: prefix + "/private", ImagePullSecrets: []string{"registry-pull"}, SharedMounts: []runnernode.SharedMount{
		{Source: "/app/data/tasks", Path: filepath.Join(fixture, "tasks"), PVC: pvc, SubPath: prefix + "/tasks"},
		{Source: "/fixture/bin", Path: binDir, PVC: pvc, SubPath: prefix + "/bin", ReadOnly: true},
	}}}
	hosts := []string{os.Getenv("ARTEX_SHARED_TEST_NODE_A"), os.Getenv("ARTEX_SHARED_TEST_NODE_B")}
	managerCount := 1
	if hosts[0] != "" && hosts[1] != "" {
		if hosts[0] == hosts[1] {
			t.Fatal("cross-node fixture needs distinct hosts")
		}
		managerCount = 2
	}
	for i := range managerCount {
		copyCfg := cfg
		copyCfg.Name = fmt.Sprintf("shared-fixture-%d", i)
		copyCfg.DataDir = t.TempDir()
		copyCfg.WorkspaceDir = filepath.Join(fixture, fmt.Sprintf("private-%d", i))
		copyCfg.Kubernetes.WorkspaceSubPath = prefix + fmt.Sprintf("/private-%d", i)
		if managerCount == 2 {
			copyCfg.Capabilities.Slots = 1
			copyCfg.Kubernetes.NodeSelector = map[string]string{"kubernetes.io/hostname": hosts[i]}
		}
		token, _, err := hub.CreateRegistrationToken(ctx, time.Minute)
		if err != nil {
			t.Fatal(err)
		}
		node, err := runnernode.New(copyCfg)
		if err != nil {
			t.Fatal(err)
		}
		nodeCtx, stopNode := context.WithCancel(ctx)
		done := make(chan error, 1)
		go func() { done <- node.Run(nodeCtx, token) }()
		t.Cleanup(func() {
			stopNode()
			select {
			case err := <-done:
				if err != nil && nodeCtx.Err() == nil {
					t.Error(err)
				}
			case <-time.After(35 * time.Second):
				t.Error("manager failed to stop")
			}
		})
	}
	eventually(t, ctx, "synthetic managers online", func() bool {
		nodes := hub.ListNodes()
		if len(nodes) != managerCount {
			return false
		}
		for _, node := range nodes {
			if !node.Online {
				return false
			}
		}
		return true
	})
	spec := func(own, thread string) runner.RunSpec {
		plan := runnerfiles.Plan{Mode: runnerfiles.SharedMode, Mounts: []runnerfiles.Mount{
			{Name: "workspace", Source: "/app/data/tasks/synthetic", Target: "/app/data/tasks/synthetic", Writable: []string{own, "shared"}},
			{Name: "published", Source: "/app/data/tasks/synthetic", Target: "/published"},
			{Name: "binary", Source: "/fixture/bin", Target: "/validation-bin"},
		}}
		raw, _ := json.Marshal(plan)
		return runner.RunSpec{ThreadID: thread, TaskID: "synthetic", Environment: "fixture", Image: image, WorkingDir: "/app/data/tasks/synthetic/" + own, Files: raw, Labels: map[string]string{runnerfiles.SharedCapability: "v1"}, Resources: runner.Resources{CPUMillis: 500, MemoryMiB: 512, PIDs: 256}, Tools: []runner.ToolDefinition{{Name: "Bash", Kind: "builtin"}, {Name: "Read", Kind: "builtin"}, {Name: "TaskList", Kind: "builtin"}}}
	}
	acquire := func(s runner.RunSpec) *runner.Session {
		t.Helper()
		start := time.Now()
		v, err := hub.Acquire(ctx, s)
		if err != nil {
			t.Fatal(err)
		}
		t.Logf("%s ready in %s, file HTTP requests=%d", s.ThreadID, time.Since(start), transfers.Load())
		t.Cleanup(func() { v.Close() })
		return v
	}
	a := acquire(spec("i1", "cold-a"))
	b := acquire(spec("i2", "cold-b"))
	if managerCount == 2 && a.NodeID() == b.NodeID() {
		t.Fatal("cross-node sessions used one manager")
	}
	t.Logf("execution managers=%d, configured hosts=%v", managerCount, hosts)
	da := newDockerSDK(t, ctx, a, "/app/data/tasks/synthetic/i1")
	db := newDockerSDK(t, ctx, b, "/app/data/tasks/synthetic/i2")
	call := func(d *dockerSDK, command string) string {
		return mustDockerCall(t, ctx, d, "Bash", map[string]any{"command": command}).Flatten()
	}
	call(da, `test ! -e /app/data/tasks/other && test ! -e /var/run/secrets/kubernetes.io/serviceaccount/token && test ! -e /var/run/docker.sock && printf A > "$HOME/private" && printf A > /tmp/private && printf durable > durable && printf shared > ../shared/.pending && mv ../shared/.pending ../shared/published`)
	call(db, `test ! -e "$HOME/private" && test ! -e /tmp/private && test "$(cat ../i1/durable)" = durable && test "$(cat /published/shared/published)" = shared && if touch ../i1/forbidden 2>/dev/null; then exit 1; fi; if touch /published/forbidden 2>/dev/null; then exit 1; fi; printf B > "$HOME/private"`)
	call(da, `test "$(cat "$HOME/private")" = A && test "$(cat /sys/fs/cgroup/memory.max)" = 536870912 && printf x >> once`)
	// Background writes remain live across periodic checks, without a scan.
	call(da, `python3 -c 'from pathlib import Path; import time; p=Path("background"); [ (p.write_text(str(i)), time.sleep(.1)) for i in range(65) ]; p.write_text("complete")' &`)
	deadline := time.Now().Add(15 * time.Second)
	for {
		v, _ := os.ReadFile(filepath.Join(task, "i1", "background"))
		if string(v) == "complete" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("background execution stopped")
		}
		time.Sleep(100 * time.Millisecond)
	}
	if err = a.Close(); err != nil {
		t.Fatal(err)
	}
	if err = b.Close(); err != nil {
		t.Fatal(err)
	}
	if value, err := os.ReadFile(filepath.Join(task, "i1", "once")); err != nil || string(value) != "x" {
		t.Fatal("durable output changed", err)
	}
	warmSpec := spec("i1", "warm-c")
	warmSpec.NodeID = a.NodeID()
	hub.SetWarmProfiles([]runner.WarmProfile{{Environment: "fixture", Idle: 1}, runner.WarmProfileFromSpec(warmSpec)})
	eventually(t, ctx, "shared prewarm ready", func() bool {
		nodes := hub.ListNodes()
		for _, node := range nodes {
			if node.ID == a.NodeID() && node.PoolStatus != nil && node.PoolStatus.Idle > 0 {
				return true
			}
		}
		return false
	})
	c := acquire(warmSpec)
	dc := newDockerSDK(t, ctx, c, warmSpec.WorkingDir)
	call(dc, `test ! -e "$HOME/private" && test ! -e /tmp/private && test "$(cat durable)" = durable && test "$(cat once)" = x`)
	if err = c.Close(); err != nil {
		t.Fatal(err)
	}
	hub.SetWarmProfiles(nil)
	cancelCtx, cancelAttempt := context.WithCancel(ctx)
	d, err := hub.Acquire(cancelCtx, spec("i1", "cancel-d"))
	if err != nil {
		t.Fatal(err)
	}
	dd := newDockerSDK(t, ctx, d, "/app/data/tasks/synthetic/i1")
	mustDockerCall(t, ctx, dd, "Bash", map[string]any{"command": "while true; do printf x >> cancelled-writer; sleep 0.05; done", "run_in_background": true})
	eventually(t, ctx, "cancellable writer running", func() bool {
		st, err := os.Stat(filepath.Join(task, "i1", "cancelled-writer"))
		return err == nil && st.Size() > 0
	})
	cancelAttempt()
	eventually(t, ctx, "cancelled shared execution cleaned", func() bool {
		for _, node := range hub.ListNodes() {
			if node.ActiveSessions > 0 {
				return false
			}
		}
		return true
	})
	before, err := os.ReadFile(filepath.Join(task, "i1", "cancelled-writer"))
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(250 * time.Millisecond)
	after, err := os.ReadFile(filepath.Join(task, "i1", "cancelled-writer"))
	if err != nil || string(before) != string(after) {
		t.Fatal("writer survived cancellation")
	}
	if transfers.Load() != 0 {
		t.Fatalf("shared lifecycle made %d file requests", transfers.Load())
	}
	t.Log("PASS: cold + warm, private HOME/tmp, task/write boundaries, live publication, background work, durable reopen, cancellation, resource limit, zero snapshot requests")
}
