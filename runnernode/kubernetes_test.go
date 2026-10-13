package runnernode

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/RC-CHN/ARTEX/runner"
	"github.com/RC-CHN/ARTEX/runnerfiles"
)

func TestDockerContainerHostPathTranslation(t *testing.T) {
	data := t.TempDir()
	root := filepath.Join(data, "sessions", "s", "mounts", "task")
	if err := os.MkdirAll(root, 0700); err != nil {
		t.Fatal(err)
	}
	d := &DockerExecutor{DataDir: data, HostDataDir: "/daemon/private-runner", Network: "runner-network"}
	plan := runnerfiles.Plan{Mounts: []runnerfiles.Mount{{Name: "task", Target: "/app/task", Writable: []string{"i1"}}}}
	args, err := d.DockerArgs("s", baseSpec("/app/task/i1"), plan, map[string]string{"task": root}, nil)
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(args, " ")
	if strings.Contains(joined, data) || !strings.Contains(joined, "source=/daemon/private-runner/sessions/s/mounts/task/i1,target=/app/task/i1") {
		t.Fatalf("bad daemon mapping: %s", joined)
	}
	if _, err = d.hostPath(filepath.Join(data, "credentials.json")); err == nil {
		t.Fatal("identity directory exposed")
	}
}

func kubeFixture(t *testing.T) *KubernetesExecutor {
	t.Helper()
	dir := t.TempDir()
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("ARTEX_KUBE_TEST_BINARY", binary)
	t.Setenv("ARTEX_KUBE_TEST_STATE", dir)
	wrapper := filepath.Join(dir, "kubectl")
	if err := os.WriteFile(wrapper, []byte("#!/bin/sh\nexport GORACE=atexit_sleep_ms=0\nexec \"$ARTEX_KUBE_TEST_BINARY\" -test.run=TestKubeCLIHelper -- \"$@\"\n"), 0700); err != nil {
		t.Fatal(err)
	}
	c := KubernetesConfig{Binary: wrapper, Namespace: "runner-test", WorkspacePVC: "workspace", WorkspaceSubPath: "sessions"}
	if err := c.validate(); err != nil {
		t.Fatal(err)
	}
	return &KubernetesExecutor{Config: c, WorkspaceDir: t.TempDir(), NodeID: "test-owner", MaxPods: 2, Log: io.Discard}
}

// Exercise real command cancellation/streaming without a required cluster.
func TestKubeCLIHelper(t *testing.T) {
	dir := os.Getenv("ARTEX_KUBE_TEST_STATE")
	if dir == "" {
		return
	}
	args := os.Args
	for len(args) > 0 && args[0] != "--" {
		args = args[1:]
	}
	args = args[3:]
	exit := func(err error) {
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		os.Exit(0)
	}
	exists := func(name string) bool { _, err := os.Stat(filepath.Join(dir, name)); return err == nil }
	switch args[0] {
	case "get":
		if args[1] == "pods" {
			entries, _ := filepath.Glob(filepath.Join(dir, "*.pod.json"))
			list := []json.RawMessage{}
			for _, p := range entries {
				b, _ := os.ReadFile(p)
				list = append(list, b)
			}
			_ = json.NewEncoder(os.Stdout).Encode(map[string]any{"items": list})
		} else {
			b, err := os.ReadFile(filepath.Join(dir, args[2]+".pod.json"))
			if err == nil {
				_, _ = os.Stdout.Write(b)
			} else if !os.IsNotExist(err) {
				exit(err)
			}
		}
	case "create":
		b, _ := io.ReadAll(os.Stdin)
		var p podState
		if err := json.Unmarshal(b, &p); err != nil {
			exit(err)
		}
		f, err := os.OpenFile(filepath.Join(dir, p.Metadata.Name+".pod.json"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
		if err != nil {
			exit(err)
		}
		_, _ = f.Write(b)
		_ = f.Close()
		if exists("fail-create") {
			exit(fmt.Errorf("create reply lost"))
		}
	case "delete":
		if exists("fail-delete") {
			exit(fmt.Errorf("kubelet unavailable"))
		}
		err := os.Remove(filepath.Join(dir, args[2]+".pod.json"))
		if err != nil && !os.IsNotExist(err) {
			exit(err)
		}
	case "wait":
		if exists("fail-wait") {
			exit(fmt.Errorf("image pull failed"))
		}
	case "attach":
		exit(ServeSandbox(context.Background(), os.Stdin, os.Stdout))
	default:
		exit(fmt.Errorf("unexpected fake kubectl %v", args))
	}
	os.Exit(0)
}

func TestNativePodIsolationAndMountConfinement(t *testing.T) {
	k := kubeFixture(t)
	root := filepath.Join(k.WorkspaceDir, "s", "mounts", "task")
	_ = os.MkdirAll(root, 0700)
	plan := runnerfiles.Plan{Mounts: []runnerfiles.Mount{{Name: "task", Target: "/app/task", Writable: []string{"i1"}}}}
	p, err := k.Pod("s", baseSpec("/app/task/i1"), plan, map[string]string{"task": root})
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(p)
	for _, required := range []string{`"automountServiceAccountToken":false`, `"readOnlyRootFilesystem":true`, `"allowPrivilegeEscalation":false`, `"runAsNonRoot":true`, `"restartPolicy":"Never"`, `"subPath":"sessions/s/mounts/task/i1"`, `--heartbeat-timeout=30s`} {
		if !bytes.Contains(raw, []byte(required)) {
			t.Errorf("missing %s", required)
		}
	}
	for _, forbidden := range []string{"hostPath", "docker.sock", "credentials.json", k.WorkspaceDir, "hostNetwork", "privileged"} {
		if bytes.Contains(raw, []byte(forbidden)) {
			t.Errorf("exposed %s", forbidden)
		}
	}
	if _, err = k.subPath(filepath.Join(k.WorkspaceDir, "..", "identity")); err == nil {
		t.Fatal("outside workspace mounted")
	}
	plan.Mounts[0].Writable = []string{"../escape"}
	if _, err = k.Pod("s", baseSpec("/app/task"), plan, map[string]string{"task": root}); err == nil {
		t.Fatal("traversal accepted")
	}
}

func TestNativePodCeilingAndCleanup(t *testing.T) {
	k := kubeFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	spec := baseSpec("/workspace")
	spec.ConcurrencyLimit = 1
	a, err := k.Start(ctx, "a", spec, runnerfiles.Plan{}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Stop()
	if _, err = a.Request(ctx, SandboxRequest{Method: "heartbeat"}); err != nil {
		t.Fatal(err)
	}
	short, stop := context.WithTimeout(ctx, 100*time.Millisecond)
	defer stop()
	if _, err = k.Start(short, "b", spec, runnerfiles.Plan{}, nil, nil); err == nil {
		t.Fatal("physical Pod ceiling exceeded")
	}
	if err = a.Stop(); err != nil {
		t.Fatal(err)
	}
	b, err := k.Start(ctx, "b", spec, runnerfiles.Plan{}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err = b.Stop(); err != nil {
		t.Fatal(err)
	}
	pods, err := k.pods(ctx)
	if err != nil || len(pods) != 0 {
		t.Fatalf("leaked Pods: %v %v", pods, err)
	}
}

func TestNativeConcurrentStartsRespectPhysicalBudgets(t *testing.T) {
	for _, budget := range []string{"slots", "cpu", "memory"} {
		t.Run(budget, func(t *testing.T) {
			k := kubeFixture(t)
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			spec := baseSpec("/workspace")
			switch budget {
			case "slots":
				k.MaxPods = 1
			case "cpu":
				spec.PoolCPUMillis = 1000 // default per-execution CPU
			case "memory":
				spec.PoolMemoryMiB = 1024 // default per-execution memory
			}
			var wg sync.WaitGroup
			opened := make(chan Sandbox, 8)
			for i := range 8 {
				wg.Add(1)
				go func() {
					defer wg.Done()
					sb, err := k.Start(ctx, fmt.Sprintf("concurrent-%d", i), spec, runnerfiles.Plan{}, nil, nil)
					if err == nil {
						opened <- sb
					}
				}()
			}
			wg.Wait()
			close(opened)
			var count int
			for sb := range opened {
				count++
				if err := sb.Stop(); err != nil {
					t.Error(err)
				}
			}
			if count != 1 {
				t.Fatalf("%s budget admitted %d simultaneous Pods; want 1", budget, count)
			}
		})
	}
}

func TestNativeFailedCreateAndUnconfirmedDeletion(t *testing.T) {
	k := kubeFixture(t)
	dir := os.Getenv("ARTEX_KUBE_TEST_STATE")
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	_ = os.WriteFile(filepath.Join(dir, "fail-create"), nil, 0600)
	if sb, err := k.Start(ctx, "lost", baseSpec("/workspace"), runnerfiles.Plan{}, nil, nil); err == nil || sb != nil {
		t.Fatalf("lost response cleanup: %v %v", sb, err)
	}
	pods, _ := k.pods(ctx)
	if len(pods) != 0 {
		t.Fatal("lost create response left Pod")
	}
	_ = os.WriteFile(filepath.Join(dir, "fail-delete"), nil, 0600)
	sb, err := k.Start(ctx, "unknown", baseSpec("/workspace"), runnerfiles.Plan{}, nil, nil)
	if err == nil || sb == nil || sb.Stop() == nil {
		t.Fatal("unconfirmed termination reported as clean")
	}
	_ = os.Remove(filepath.Join(dir, "fail-delete"))
	if err = k.CleanupOrphans(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestSandboxHeartbeatTimeoutAndRenewal(t *testing.T) {
	reader, writer := io.Pipe()
	out := &bytes.Buffer{}
	done := make(chan error, 1)
	go func() { done <- ServeSandboxWithHeartbeat(context.Background(), reader, out, 150*time.Millisecond) }()
	for i := 0; i < 4; i++ {
		time.Sleep(50 * time.Millisecond)
		_, _ = fmt.Fprintln(writer, `{"method":"heartbeat"}`)
		select {
		case <-done:
			t.Fatal("live manager expired")
		default:
		}
	}
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("orphaned sandbox stayed alive")
	}
	_ = writer.Close()
}

func TestSandboxHeartbeatDuringSlowMCPOpen(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	mcp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		once.Do(func() { close(started) })
		select {
		case <-r.Context().Done():
		case <-release:
		}
	}))
	defer mcp.Close()
	defer close(release)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	in, input := io.Pipe()
	output, out := io.Pipe()
	defer input.Close()
	defer output.Close()
	done := make(chan error, 1)
	go func() { done <- ServeSandboxWithHeartbeat(ctx, in, out, time.Second); _ = out.Close() }()
	replies := make(chan SandboxReply, 8)
	go func() {
		defer close(replies)
		d := json.NewDecoder(output)
		for {
			var reply SandboxReply
			if d.Decode(&reply) != nil {
				return
			}
			replies <- reply
		}
	}()
	spec := baseSpec(t.TempDir())
	spec.MCPs = []runner.MCPConfig{{Name: "slow", Transport: "http", URL: mcp.URL}}
	enc := json.NewEncoder(input)
	if err := enc.Encode(SandboxRequest{ID: "opening", Method: "open", SessionID: "slow", Spec: &spec}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("MCP did not start")
	}
	if err := enc.Encode(SandboxRequest{ID: "beat", Method: "heartbeat"}); err != nil {
		t.Fatal(err)
	}
	select {
	case reply := <-replies:
		if reply.ID != "beat" || reply.Error != "" {
			t.Fatalf("heartbeat blocked behind open: %+v", reply)
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("slow MCP blocked heartbeat reply")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("cancel did not terminate pending MCP open")
	}
}
