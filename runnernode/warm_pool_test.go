package runnernode

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/RC-CHN/ARTEX/runner"
	"github.com/RC-CHN/ARTEX/runnerfiles"
)

func warmFixture(t *testing.T) (*KubernetesExecutor, runner.RunSpec, runnerfiles.Plan, runner.PoolPolicy) {
	t.Helper()
	k := kubeFixture(t)
	spec := baseSpec("/app/task/i1")
	spec.Environment = "fixture"
	spec.Env = map[string]string{"FIXTURE": "literal $HOME"}
	spec.Resources = runner.Resources{CPUMillis: 500, MemoryMiB: 512, PIDs: 256}
	plan := runnerfiles.Plan{Mounts: []runnerfiles.Mount{{Name: "task", Source: t.TempDir(), Target: "/app/task", Writable: []string{"i1", "shared"}}, {Name: "assets", Source: t.TempDir(), Target: "/app/assets"}}}
	spec.Files, _ = json.Marshal(plan)
	policy := runner.PoolPolicy{Slots: 2, CPUMillis: 1000, MemoryMiB: 1024, Profiles: []runner.WarmProfile{runner.WarmProfileFromSpec(spec)}}
	k.UpdatePool(policy)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = os.Remove(filepath.Join(os.Getenv("ARTEX_KUBE_TEST_STATE"), "fail-delete"))
		if err := k.evictIdle(ctx); err != nil {
			t.Error(err)
		}
		k.poolWG.Wait()
	})
	return k, spec, plan, policy
}

func readyWarm(t *testing.T, k *KubernetesExecutor) *warmPod {
	t.Helper()
	if err := k.reconcilePool(t.Context()); err != nil {
		t.Fatal(err)
	}
	k.poolMu.Lock()
	var p *warmPod
	for _, w := range k.warm {
		p = w
	}
	k.poolMu.Unlock()
	if p == nil {
		t.Fatal("no prewarm Pod created")
	}
	select {
	case <-p.ready:
		if p.err != nil {
			t.Fatal(p.err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("prewarm did not become ready")
	}
	return p
}

func TestWarmAndColdPodBehaviorIdentical(t *testing.T) {
	k, spec, plan, _ := warmFixture(t)
	p := readyWarm(t, k)
	warmRoots, err := p.roots(plan)
	if err != nil {
		t.Fatal(err)
	}
	coldRoots := map[string]string{}
	for _, m := range plan.Mounts {
		coldRoots[m.Name] = filepath.Join(k.WorkspaceDir, safeID("cold"), "mounts", safeID(m.Name))
		if err := secureDir(coldRoots[m.Name]); err != nil {
			t.Fatal(err)
		}
	}
	cold, err := k.Pod("cold", spec, plan, coldRoots)
	if err != nil {
		t.Fatal(err)
	}
	warm, err := k.Pod(p.id, spec, plan, warmRoots)
	if err != nil {
		t.Fatal(err)
	}
	// Only the private identity and its backing PVC subdirectory may differ.
	delete(cold, "metadata")
	delete(warm, "metadata")
	a, _ := json.Marshal(cold)
	b, _ := json.Marshal(warm)
	b = []byte(strings.ReplaceAll(string(b), safeID(p.id), safeID("cold")))
	if string(a) != string(b) {
		t.Fatalf("prewarming changed Pod behavior:\ncold=%s\nwarm=%s", a, b)
	}
}

func TestWarmClaimConcurrentExactlyOnceAndNoReuse(t *testing.T) {
	k, spec, plan, policy := warmFixture(t)
	p := readyWarm(t, k)
	var wg sync.WaitGroup
	winners := make(chan *warmPod, 64)
	for i := range 64 {
		wg.Go(func() {
			got, err := k.ClaimWarm(t.Context(), fmt.Sprint(i), spec, plan)
			if err != nil {
				t.Error(err)
			}
			if got != nil {
				winners <- got
			}
		})
	}
	wg.Wait()
	close(winners)
	count := 0
	for got := range winners {
		count++
		if got != p {
			t.Fatal("wrong Pod claimed")
		}
	}
	if count != 1 {
		t.Fatalf("claimed %d times", count)
	}
	// A transient empty frontier must not recreate a running execution's Pod.
	k.UpdatePool(runner.PoolPolicy{Slots: 2})
	if err := k.reconcilePool(t.Context()); err != nil {
		t.Fatal(err)
	}
	k.UpdatePool(policy)
	if err := k.reconcilePool(t.Context()); err != nil {
		t.Fatal(err)
	}
	if status := k.PoolStatus(); status.Idle != 0 || status.Preparing != 0 {
		t.Fatalf("dirty execution reused: %+v", status)
	}
	if err := p.sandbox.Stop(); err != nil {
		t.Fatal(err)
	}
	if err := p.sandbox.Stop(); err != nil {
		t.Fatal(err)
	}
	k.UpdatePool(runner.PoolPolicy{Slots: 2})
	if err := k.reconcilePool(t.Context()); err != nil {
		t.Fatal(err)
	}
	if len(k.claimed) != 0 || len(k.consumed) != 0 {
		t.Fatal("claimed profile bookkeeping leaked")
	}
}

func TestWarmExactMatchAndPolicyFence(t *testing.T) {
	k, spec, plan, policy := warmFixture(t)
	_ = readyWarm(t, k)
	for _, change := range []func(*runner.RunSpec){
		func(s *runner.RunSpec) { s.Image = strings.ReplaceAll(s.Image, "0123", "4321") },
		func(s *runner.RunSpec) { s.WorkingDir += "-other" },
		func(s *runner.RunSpec) { s.Resources.MemoryMiB++ },
		func(s *runner.RunSpec) { s.Env = map[string]string{"FIXTURE": "other"} },
		func(s *runner.RunSpec) { s.Environment = "other" },
	} {
		changed := spec
		change(&changed)
		if p, err := k.ClaimWarm(t.Context(), "different", changed, plan); err != nil || p != nil {
			t.Fatalf("mismatched profile accepted: %v", err)
		}
	}
	changedPlan := plan
	changedPlan.Mounts = append([]runnerfiles.Mount(nil), plan.Mounts...)
	changedPlan.Mounts[0].Writable = []string{"."}
	if p, _ := k.ClaimWarm(t.Context(), "changed-access", spec, changedPlan); p != nil {
		t.Fatal("changed writable policy accepted")
	}
	k.UpdatePool(runner.PoolPolicy{Slots: 2})
	if p, _ := k.ClaimWarm(t.Context(), "disabled", spec, plan); p != nil {
		t.Fatal("disabled prewarm claimed")
	}
	k.UpdatePool(policy)
	k.poolMu.Lock()
	k.policyAt = time.Now().Add(-time.Minute)
	k.poolMu.Unlock()
	if p, _ := k.ClaimWarm(t.Context(), "disconnected", spec, plan); p != nil {
		t.Fatal("stale controller policy claimed")
	}
	if err := k.reconcilePool(t.Context()); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(k.WorkspaceDir)
	if err != nil || len(entries) != 0 {
		t.Fatalf("idle directories leaked: %v %v", entries, err)
	}
}

func TestWarmFailedDeletionKeepsCapacity(t *testing.T) {
	k, _, _, _ := warmFixture(t)
	_ = readyWarm(t, k)
	file := filepath.Join(os.Getenv("ARTEX_KUBE_TEST_STATE"), "fail-delete")
	if err := os.WriteFile(file, nil, 0600); err != nil {
		t.Fatal(err)
	}
	k.UpdatePool(runner.PoolPolicy{Slots: 1, CPUMillis: 500, MemoryMiB: 512})
	if err := k.reconcilePool(t.Context()); err == nil {
		t.Fatal("unconfirmed deletion accepted")
	}
	pods, err := k.pods(t.Context())
	if err != nil || len(pods) != 1 || len(k.warm) != 1 {
		t.Fatal("failed cleanup released capacity")
	}
	if err := os.Remove(file); err != nil {
		t.Fatal(err)
	}
	if err := k.reconcilePool(t.Context()); err != nil {
		t.Fatal(err)
	}
	if len(k.warm) != 0 {
		t.Fatal("confirmed cleanup retained Pod")
	}
}

func TestWarmSustainedCreateCancelAndQuotaChanges(t *testing.T) {
	k, spec, plan, policy := warmFixture(t)
	for i := range 30 {
		p := readyWarm(t, k)
		if i%2 == 0 {
			got, err := k.ClaimWarm(t.Context(), fmt.Sprint(i), spec, plan)
			if err != nil || got != p {
				t.Fatalf("cycle %d claim: %v", i, err)
			}
			if err := got.sandbox.Stop(); err != nil {
				t.Fatal(err)
			}
			if err := os.RemoveAll(got.dir); err != nil {
				t.Fatal(err)
			}
		}
		var wg sync.WaitGroup
		for range 8 {
			wg.Go(func() { _ = k.PoolStatus(); k.UpdatePool(runner.PoolPolicy{Slots: 1, CPUMillis: 500, MemoryMiB: 512}) })
		}
		wg.Wait()
		if err := k.reconcilePool(t.Context()); err != nil {
			t.Fatal(err)
		}
		pods, err := k.pods(t.Context())
		entries, de := os.ReadDir(k.WorkspaceDir)
		if err != nil || de != nil || len(pods) != 0 || len(entries) != 0 || len(k.warm) != 0 || len(k.consumed) != 0 || len(k.claimed) != 0 {
			t.Fatalf("cycle %d accumulated state: pods=%d dirs=%d warm=%d consumed=%d claimed=%d errors=%v/%v", i, len(pods), len(entries), len(k.warm), len(k.consumed), len(k.claimed), err, de)
		}
		k.UpdatePool(policy)
	}
}

func TestOlderControllerDoesNotDisableKubernetesAdmission(t *testing.T) {
	k := kubeFixture(t)
	n := &Node{Executor: k, leases: map[string]runner.Lease{}}
	n.applyPoll(t.Context(), runner.PollResponse{}, time.Now())
	if !k.policyAt.IsZero() || !reflect.DeepEqual(k.policy, runner.PoolPolicy{}) {
		t.Fatal("missing optional policy became a zero Pod limit")
	}
}

func TestOptionalPoolProtocolNegotiationAndControllerRollback(t *testing.T) {
	for _, rollback := range []bool{false, true} {
		t.Run(fmt.Sprintf("rollback=%t", rollback), func(t *testing.T) {
			k := kubeFixture(t)
			n, _ := testNode(t)
			n.Executor = k
			ctx, cancel := context.WithTimeout(t.Context(), 8*time.Second)
			defer cancel()
			var polls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if strings.HasSuffix(r.URL.Path, "/register") {
					_ = json.NewEncoder(w).Encode(runner.RegisterResponse{NodeID: "node", Credential: "fixture", Protocol: runner.ProtocolVersion})
					return
				}
				var req runner.PollRequest
				if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
					t.Error(err)
					return
				}
				i := polls.Add(1)
				if rollback && i == 2 {
					if req.PoolStatus == nil || req.PoolStatus.Admission == nil {
						t.Error("new controller never received negotiated status")
					}
					http.Error(w, "unknown field pool_status", http.StatusBadRequest)
					return
				}
				if req.PoolStatus != nil {
					t.Error("sent unsupported optional field to old controller")
				}
				response := runner.PollResponse{Epoch: "e", ServerTime: time.Now()}
				if rollback && i == 1 {
					response.Pool = &runner.PoolPolicy{Slots: 2}
					response.AdmissionDiagnostics = true
				}
				_ = json.NewEncoder(w).Encode(response)
				if i == 4 {
					cancel()
				}
			}))
			defer server.Close()
			n.Config.ServerURL, n.HTTP = server.URL, server.Client()
			_ = n.Run(ctx, "registration")
			if polls.Load() != 4 {
				t.Fatalf("controller compatibility stalled after %d polls", polls.Load())
			}
			if !k.policyAt.IsZero() {
				t.Fatal("controller rollback retained stale administrative limits")
			}
		})
	}
}

func TestWarmRestartCleansOnlyUnusedCaches(t *testing.T) {
	k, _, _, _ := warmFixture(t)
	p := readyWarm(t, k)
	if err := p.sandbox.Stop(); err != nil {
		t.Fatal(err)
	}
	// Another directory has potential outputs: even a stale marker must not
	// cause them to disappear after a manager/filesystem crash.
	retained := filepath.Join(k.WorkspaceDir, safeID("recovery"))
	if err := secureDir(retained); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{idleMarker, "result"} {
		if err := os.WriteFile(filepath.Join(retained, name), []byte("retain"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if err := k.CleanupOrphans(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(p.dir); !os.IsNotExist(err) {
		t.Fatal("unused prewarm cache survived restart")
	}
	if _, err := os.Stat(filepath.Join(retained, "result")); err != nil {
		t.Fatal("recovery output lost")
	}
	if err := os.RemoveAll(retained); err != nil {
		t.Fatal(err)
	}
}

func TestWarmActivationProtectsCacheFromRestartCleanup(t *testing.T) {
	k, spec, plan, _ := warmFixture(t)
	_ = readyWarm(t, k)
	p, err := k.ClaimWarm(t.Context(), "actual", spec, plan)
	if err != nil || p == nil {
		t.Fatal("claim failed")
	}
	if err := p.activate(); err != nil {
		t.Fatal(err)
	}
	if err := p.sandbox.Stop(); err != nil {
		t.Fatal(err)
	}
	if err := k.CleanupOrphans(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(p.dir); err != nil {
		t.Fatal("claimed cache treated as unused")
	}
	if err := os.RemoveAll(p.dir); err != nil {
		t.Fatal(err)
	}
}
