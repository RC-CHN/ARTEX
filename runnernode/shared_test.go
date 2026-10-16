package runnernode

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/RC-CHN/ARTEX/runner"
	"github.com/RC-CHN/ARTEX/runnerfiles"
)

func sharedFixture(t *testing.T) (*KubernetesExecutor, runner.RunSpec, runnerfiles.Plan) {
	t.Helper()
	k := kubeFixture(t)
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "tasks", "42", "i1"), 0770); err != nil {
		t.Fatal(err)
	}
	k.Config.SharedMounts = []SharedMount{{Source: "/app/data/tasks", Path: filepath.Join(root, "tasks"), PVC: "shared-data", SubPath: "tasks"}}
	plan := runnerfiles.Plan{Mode: runnerfiles.SharedMode, Mounts: []runnerfiles.Mount{
		{Name: "workspace", Source: "/app/data/tasks/42", Target: "/app/data/tasks/42", Writable: []string{"i1", "shared"}},
		{Name: "published", Source: "/app/data/tasks/42", Target: "/app/data/tasks/.runner-published/42"},
	}}
	spec := baseSpec("/app/data/tasks/42/i1")
	spec.Environment = "shared"
	spec.Resources = runner.Resources{CPUMillis: 500, MemoryMiB: 512, PIDs: 256}
	spec.Files, _ = json.Marshal(plan)
	return k, spec, plan
}

func TestSharedMountsPreserveScopeWithoutScanningHistory(t *testing.T) {
	k, spec, plan := sharedFixture(t)
	task := filepath.Join(k.Config.SharedMounts[0].Path, "42")
	// A snapshot walker would reject this symlink and try to read the sparse 8 GiB
	// file. Direct mount preparation must never enumerate or hash either one.
	if err := os.Symlink("/unavailable/history", filepath.Join(task, "historical-link")); err != nil {
		t.Fatal(err)
	}
	f, err := os.Create(filepath.Join(task, "large-history"))
	if err != nil {
		t.Fatal(err)
	}
	if err = f.Truncate(8 << 30); err != nil {
		t.Fatal(err)
	}
	f.Close()
	roots, err := k.sharedRoots(plan)
	if err != nil {
		t.Fatal(err)
	}
	pod, err := k.Pod("shared", spec, plan, roots)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(pod)
	for _, need := range []string{`"claimName":"shared-data"`, `"subPath":"tasks/42/i1"`, `"subPath":"tasks/42/shared"`, `"readOnly":true`, `"automountServiceAccountToken":false`, `"readOnlyRootFilesystem":true`} {
		if !strings.Contains(string(raw), need) {
			t.Errorf("missing %s", need)
		}
	}
	if strings.Contains(string(raw), `"fsGroup"`) || strings.Contains(string(raw), task) {
		t.Fatalf("recursive ownership or manager path leaked: %s", raw)
	}
	ps := pod["spec"].(map[string]any)
	claims := 0
	for _, volume := range ps["volumes"].([]any) {
		if claim, ok := volume.(map[string]any)["persistentVolumeClaim"]; ok {
			claims++
			if claim.(map[string]any)["claimName"] != "shared-data" {
				t.Fatal("unused private PVC attached")
			}
		}
	}
	if claims != 1 {
		t.Fatalf("one shared PVC declared %d times", claims)
	}
	mounts := ps["containers"].([]any)[0].(map[string]any)["volumeMounts"].([]any)
	writable := map[string]bool{}
	for _, v := range mounts {
		m := v.(map[string]any)
		if strings.HasPrefix(m["name"].(string), "shared-") && m["readOnly"] == false {
			writable[m["mountPath"].(string)] = true
		}
	}
	if len(writable) != 2 || !writable["/app/data/tasks/42/i1"] || !writable["/app/data/tasks/42/shared"] {
		t.Fatalf("overbroad writes: %v", writable)
	}
}

func TestSharedResourceMountIsImmutableVersionScoped(t *testing.T) {
	k, spec, plan := sharedFixture(t)
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "17"), 0755); err != nil {
		t.Fatal(err)
	}
	k.Config.SharedMounts = append(k.Config.SharedMounts, SharedMount{Source: "/app/data/.resource-library/versions", Path: root, PVC: "resources", ReadOnly: true})
	plan.Mounts = append(plan.Mounts, runnerfiles.Mount{Name: "resource-17", Source: "/app/data/.resource-library/versions/17", Target: "/resources/v17"})
	roots, err := k.sharedRoots(plan)
	if err != nil {
		t.Fatal(err)
	}
	pod, err := k.Pod("resource", spec, plan, roots)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(pod)
	if !strings.Contains(string(raw), `"subPath":"17"`) || !strings.Contains(string(raw), `"claimName":"resources"`) {
		t.Fatal("resource not scoped to the selected version")
	}
	plan.Mounts[len(plan.Mounts)-1].Writable = []string{"."}
	if _, err = k.sharedRoots(plan); err == nil {
		t.Fatal("global resource write mount accepted")
	}
}

func TestSharedRejectsUnmappedEscapedReadonlyAndReplacedRoots(t *testing.T) {
	for _, kind := range []string{"unmapped", "traversal", "prefix", "source-link", "writable-link", "readonly", "wrong-root", "missing"} {
		t.Run(kind, func(t *testing.T) {
			k, spec, plan := sharedFixture(t)
			task := filepath.Join(k.Config.SharedMounts[0].Path, "42")
			switch kind {
			case "unmapped":
				plan.Mounts[0].Source = "/etc"
			case "traversal":
				plan.Mounts[0].Source = "/app/data/tasks/../credentials"
			case "prefix":
				plan.Mounts[0].Source = "/app/data/tasks-other/42"
			case "source-link":
				os.Symlink(task, filepath.Join(k.Config.SharedMounts[0].Path, "link"))
				plan.Mounts[0].Source = "/app/data/tasks/link"
			case "writable-link":
				os.Symlink(t.TempDir(), filepath.Join(task, "shared"))
			case "readonly":
				k.Config.SharedMounts[0].ReadOnly = true
			case "missing":
				plan.Mounts[0].Source = "/app/data/tasks/absent"
			}
			roots, err := k.sharedRoots(plan)
			if err == nil {
				if kind == "wrong-root" {
					roots["workspace"] = t.TempDir()
				}
				_, err = k.Pod("reject", spec, plan, roots)
			}
			if err == nil {
				t.Fatal("unsafe mapping accepted")
			}
		})
	}
}

type failFileTransport struct{ requests atomic.Int32 }

func (f *failFileTransport) RoundTrip(*http.Request) (*http.Response, error) {
	f.requests.Add(1)
	return nil, fmt.Errorf("unexpected file transfer")
}

func TestSharedOpenCallCloseAndPrewarmNeverTransferOrDeleteData(t *testing.T) {
	for _, warm := range []bool{false, true} {
		t.Run(fmt.Sprint(warm), func(t *testing.T) {
			k, spec, plan := sharedFixture(t)
			c := config(t)
			c.Executor = "kubernetes"
			c.WorkspaceDir = k.WorkspaceDir
			c.Kubernetes = k.Config
			n, err := New(c)
			if err != nil {
				t.Fatal(err)
			}
			n.Executor = k
			tr := &failFileTransport{}
			n.HTTP = &http.Client{Transport: tr}
			n.epoch = "epoch"
			n.leases["s"] = runner.Lease{SessionID: "s", AttemptID: "a", Epoch: "epoch", Until: time.Now().Add(time.Minute)}
			// Fake kubectl uses this process's filesystem, so use a real local cwd while
			// the manifest still checks the logical source and mount configuration.
			spec.WorkingDir = filepath.Join(k.Config.SharedMounts[0].Path, "42", "i1")
			plan.Mounts[0].Target = filepath.Dir(spec.WorkingDir)
			spec.Files, _ = json.Marshal(plan)
			data := filepath.Join(spec.WorkingDir, "kept")
			if err = os.WriteFile(data, []byte("durable"), 0600); err != nil {
				t.Fatal(err)
			}
			if warm {
				k.UpdatePool(runner.PoolPolicy{Slots: 2, CPUMillis: 4000, MemoryMiB: 4096, Profiles: []runner.WarmProfile{runner.WarmProfileFromSpec(spec)}})
				readyWarm(t, k)
			}
			defer k.evictIdle(context.Background())
			command := runner.Command{ID: "open", Kind: runner.CommandOpen, SessionID: "s", AttemptID: "a", Epoch: "epoch", Spec: &spec}
			if r := n.executeCommand(t.Context(), command); r.Error != "" {
				t.Fatal(r.Error)
			}
			command.ID = "call"
			command.Kind = runner.CommandCall
			command.Call = &runner.ToolCall{ID: "read", Name: "Read", Input: json.RawMessage(`{"file_path":"kept"}`)}
			if r := n.executeCommand(t.Context(), command); r.Error != "" || r.Result.IsError {
				t.Fatalf("read failed: %+v", r)
			}
			command.ID = "close"
			command.Kind = runner.CommandClose
			if r := n.executeCommand(t.Context(), command); r.Error != "" {
				t.Fatal(r.Error)
			}
			if tr.requests.Load() != 0 {
				t.Fatalf("file requests: %d", tr.requests.Load())
			}
			if b, err := os.ReadFile(data); err != nil || string(b) != "durable" {
				t.Fatal("shared results deleted", err)
			}
			if _, err := os.Stat(filepath.Join(n.Config.WorkspaceDir, safeID("s"))); !os.IsNotExist(err) {
				t.Fatal("private cache retained", err)
			}
		})
	}
}

func TestSharedMappingValidationAndUnsupportedExecutor(t *testing.T) {
	k, spec, plan := sharedFixture(t)
	c := config(t)
	if _, _, err := c.Prepare(spec); err == nil {
		t.Fatal("Docker accepted shared mode")
	}
	c.Executor = "kubernetes"
	c.WorkspaceDir = k.WorkspaceDir
	c.Kubernetes = k.Config
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	if c.Capabilities.Labels[runnerfiles.SharedCapability] != "v1" {
		t.Fatal("capability absent")
	}
	c.Kubernetes.SharedMounts = append(c.Kubernetes.SharedMounts, c.Kubernetes.SharedMounts[0])
	if err := c.Validate(); err == nil {
		t.Fatal("overlap accepted")
	}
	plan.Mode = "unknown"
	raw, _ := json.Marshal(plan)
	if _, err := decodePlan(raw); err == nil {
		t.Fatal("unknown mode accepted")
	}
	plan.Mode = runnerfiles.SharedMode
	plan.Mounts[0].Writable = []string{"../outside"}
	raw, _ = json.Marshal(plan)
	if _, err := decodePlan(raw); err == nil {
		t.Fatal("writable traversal accepted")
	}
}

func TestSharedStorageReserveRejectsBeforePodStart(t *testing.T) {
	k, _, plan := sharedFixture(t)
	k.Config.SharedMinFreeBytes = ^uint64(0)
	if _, err := k.sharedRoots(plan); err == nil || !strings.Contains(err.Error(), "reserve") {
		t.Fatalf("byte floor not enforced: %v", err)
	}
	k.Config.SharedMinFreeBytes = 1
	k.Config.SharedMinFreeInodes = ^uint64(0)
	if _, err := k.sharedRoots(plan); err == nil || !strings.Contains(err.Error(), "reserve") {
		t.Fatalf("inode floor not enforced: %v", err)
	}
}

func TestRetiringUnusedSharedWarmPodKeepsHistory(t *testing.T) {
	k, spec, plan := sharedFixture(t)
	roots, err := k.sharedRoots(plan)
	if err != nil {
		t.Fatal(err)
	}
	kept := filepath.Join(roots["workspace"], "durable")
	if err = os.WriteFile(kept, []byte("kept"), 0600); err != nil {
		t.Fatal(err)
	}
	k.UpdatePool(runner.PoolPolicy{Slots: 2, CPUMillis: 4000, MemoryMiB: 4096, Profiles: []runner.WarmProfile{runner.WarmProfileFromSpec(spec)}})
	warm := readyWarm(t, k)
	if err = k.evictIdle(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, err = os.Stat(warm.dir); !os.IsNotExist(err) {
		t.Fatal("unused cache retained", err)
	}
	if b, err := os.ReadFile(kept); err != nil || string(b) != "kept" {
		t.Fatal("shared history removed", err)
	}
}
