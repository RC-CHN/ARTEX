package integration

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/RC-CHN/ARTEX/runner"
	"github.com/RC-CHN/ARTEX/runnerfiles"
	"github.com/RC-CHN/ARTEX/runnernode"
)

// Opt-in: a uniquely named, explicitly labelled synthetic namespace only.
// No ARTEX API/database, task volumes, or production namespace is accessed.
func TestKubernetesQuotaRecoveryAndCancellation(t *testing.T) {
	ns, image := os.Getenv("ARTEX_CAPACITY_TEST_NAMESPACE"), os.Getenv("ARTEX_CAPACITY_TEST_IMAGE")
	if ns == "" {
		t.Skip("set ARTEX_CAPACITY_TEST_NAMESPACE/IMAGE for an isolated real-quota regression")
	}
	if !strings.HasPrefix(ns, "artex-capacity-test-") || !strings.Contains(image, "@sha256:") {
		t.Fatal("dedicated namespace and pinned image required")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 8*time.Minute)
	defer cancel()
	kube := func(args ...string) []byte {
		t.Helper()
		cmd := exec.CommandContext(ctx, "kubectl", append([]string{"-n", ns}, args...)...)
		b, err := cmd.Output()
		if err != nil {
			t.Fatalf("synthetic namespace operation %s failed: %v", args[0], err)
		}
		return b
	}
	var namespace struct {
		Metadata struct {
			Labels map[string]string `json:"labels"`
		} `json:"metadata"`
	}
	if err := json.Unmarshal(kube("get", "namespace", ns, "-o", "json"), &namespace); err != nil || namespace.Metadata.Labels["artex.test/capacity"] != "synthetic" {
		t.Fatal("refusing non-fixture namespace")
	}
	kube("create", "quota", "capacity-fixture", "--hard=count/pods=0,limits.cpu=0,requests.cpu=0")
	defer func() {
		cleanup, stop := context.WithTimeout(context.Background(), 30*time.Second)
		defer stop()
		_ = exec.CommandContext(cleanup, "kubectl", "-n", ns, "delete", "quota", "capacity-fixture", "--ignore-not-found").Run()
	}()
	k := &runnernode.KubernetesExecutor{Config: runnernode.KubernetesConfig{Namespace: ns, WorkspacePVC: "unused-no-task-volumes", ServiceAccount: "default", ImagePullSecrets: []string{"registry-pull"}}, WorkspaceDir: t.TempDir(), NodeID: "synthetic-capacity-" + fmt.Sprint(time.Now().UnixNano()), MaxPods: 4, Log: io.Discard}
	defer func() {
		cleanup, stop := context.WithTimeout(context.Background(), 45*time.Second)
		defer stop()
		if err := k.CleanupOrphans(cleanup); err != nil {
			t.Error("synthetic Pod cleanup", err)
		}
	}()
	sp := runner.RunSpec{Image: image, WorkingDir: "/workspace", Resources: runner.Resources{CPUMillis: 100, MemoryMiB: 128, PIDs: 64}, PoolCPUMillis: 400, PoolMemoryMiB: 512}
	// Empty shared layout produces only emptyDir volumes, never task/PVC mounts.
	plan := runnerfiles.Plan{Mode: runnerfiles.SharedMode}
	type outcome struct {
		sb  runnernode.Sandbox
		err error
	}
	start := func(id string, c context.Context) <-chan outcome {
		out := make(chan outcome, 1)
		go func() { sb, err := k.Start(c, id, sp, plan, nil, nil); out <- outcome{sb, err} }()
		return out
	}
	await := func(out <-chan outcome) runnernode.Sandbox {
		t.Helper()
		select {
		case r := <-out:
			if r.err != nil {
				t.Fatal(r.err)
			}
			t.Cleanup(func() { _ = r.sb.Stop() })
			return r.sb
		case <-ctx.Done():
			t.Fatal("synthetic quota recovery timed out")
			return nil
		}
	}
	first := start("initial", ctx)
	eventually(t, ctx, "real ResourceQuota rejection", func() bool {
		select {
		case r := <-first:
			t.Fatalf("initial synthetic start returned before quota recovery: %v", r.err)
		default:
		}
		return k.PoolStatus().Admission.QuotaRejections >= 2
	})
	q := k.PoolStatus().Admission.LastQuotaRejection
	if q.Hard["count/pods"] != "0" || q.Requested["count/pods"] != "1" {
		t.Fatalf("unexpected numeric quota metadata: %+v", q)
	}
	t.Logf("real quota rejection observed: rejections=%d", k.PoolStatus().Admission.QuotaRejections)
	kube("patch", "quota", "capacity-fixture", "--type=merge", "-p", `{"spec":{"hard":{"count/pods":"1","limits.cpu":"100m","requests.cpu":"100m"}}}`)
	a := await(first)
	if _, err := a.Request(ctx, runnernode.SandboxRequest{Method: "heartbeat"}); err != nil {
		t.Fatal(err)
	}
	t.Log("quota increase recovered; first synthetic sandbox ready")
	before := k.PoolStatus().Admission.QuotaRejections
	cancelCtx, stop := context.WithCancel(ctx)
	second := start("cancelled", cancelCtx)
	third := start("replacement", ctx)
	eventually(t, ctx, "second quota saturation", func() bool {
		return k.PoolStatus().Admission.QuotaRejections > before && len(k.PoolStatus().Admission.Waiting) == 2
	})
	stoppedAt := time.Now()
	stop()
	select {
	case r := <-second:
		if !errors.Is(r.err, context.Canceled) {
			t.Fatalf("cancelled admission: %v", r.err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("quota wait did not cancel")
	}
	t.Logf("blocked request cancelled in %s", time.Since(stoppedAt))
	if err := a.Stop(); err != nil {
		t.Fatal(err)
	}
	releasedAt := time.Now()
	b := await(third)
	t.Logf("Pod deletion and quota reclamation recovered in %s", time.Since(releasedAt))
	if err := b.Stop(); err != nil {
		t.Fatal(err)
	}
	var pods struct {
		Items []json.RawMessage `json:"items"`
	}
	if err := json.Unmarshal(kube("get", "pods", "-o", "json"), &pods); err != nil || len(pods.Items) != 0 {
		t.Fatal("synthetic Pods remain after close")
	}
	if len(k.PoolStatus().Admission.Waiting) != 0 {
		t.Fatal("admission waiters leaked")
	}
	t.Logf("PASS: empty fixture namespace, quota_rejections=%d", k.PoolStatus().Admission.QuotaRejections)
}
