package runnernode

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/RC-CHN/ARTEX/runner"
	"github.com/RC-CHN/ARTEX/runnerfiles"
)

func admissionEventually(t *testing.T, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !condition() {
		if time.Now().After(deadline) {
			t.Fatal("admission condition did not settle")
		}
		time.Sleep(10 * time.Millisecond)
	}
}
func admissionMarker(t *testing.T, name string) string {
	t.Helper()
	p := filepath.Join(os.Getenv("ARTEX_KUBE_TEST_STATE"), name)
	if err := os.WriteFile(p, nil, 0600); err != nil {
		t.Fatal(err)
	}
	return p
}
func TestQuotaRejectWaitsForRecoveryWithoutLeakingPods(t *testing.T) {
	k := kubeFixture(t)
	marker := admissionMarker(t, "quota-block")
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	type result struct {
		sandbox Sandbox
		err     error
	}
	done := make(chan result, 1)
	go func() {
		sb, err := k.Start(ctx, "quota-recovery", baseSpec("/workspace"), runnerfiles.Plan{}, nil, nil)
		done <- result{sb, err}
	}()
	admissionEventually(t, func() bool {
		s := k.admissionStatus()
		return s.QuotaRejections >= 2 && len(s.Waiting) == 1 && s.Waiting[0].Stage == "quota"
	})
	status := k.PoolStatus().Admission
	if status.LastQuotaRejection.Used["requests.cpu"] != "16100m" || status.LastQuotaRejection.Hard["count/pods"] != "5" {
		t.Fatalf("quota diagnostics: %+v", status)
	}
	status.LastQuotaRejection.Used["requests.cpu"] = "changed"
	if k.admissionStatus().LastQuotaRejection.Used["requests.cpu"] != "16100m" {
		t.Fatal("snapshot aliases executor state")
	}
	if pods, err := k.pods(ctx); err != nil || len(pods) != 0 {
		t.Fatalf("quota rejection created Pod: %v %v", pods, err)
	}
	select {
	case r := <-done:
		t.Fatalf("quota rejection returned early: %v", r.err)
	default:
	}
	if err := os.Remove(marker); err != nil {
		t.Fatal(err)
	}
	select {
	case r := <-done:
		if r.err != nil {
			t.Fatal(r.err)
		}
		defer r.sandbox.Stop()
	case <-ctx.Done():
		t.Fatal("quota did not recover")
	}
	status = k.admissionStatus()
	if len(status.Waiting) != 0 || status.LastCreateAt.Before(status.LastQuotaRejection.At) || !status.RetryAt.IsZero() {
		t.Fatalf("recovery diagnostics stale: %+v", status)
	}
}
func TestQuotaConcurrentWaitersBackOffAndCancel(t *testing.T) {
	k := kubeFixture(t)
	admissionMarker(t, "quota-block")
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 8)
	for i := range 8 {
		go func() {
			_, err := k.Start(ctx, fmt.Sprintf("waiting-%d", i), baseSpec("/workspace"), runnerfiles.Plan{}, nil, nil)
			done <- err
		}()
	}
	admissionEventually(t, func() bool { return len(k.admissionStatus().Waiting) == 8 && k.admissionStatus().QuotaRejections >= 2 })
	if got := k.admissionStatus().QuotaRejections; got > 4 {
		t.Fatalf("concurrent waiters bypassed shared backoff: %d", got)
	}
	start := time.Now()
	cancel()
	for range 8 {
		select {
		case err := <-done:
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("cancellation: %v", err)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("cancellation stuck behind create gate")
		}
	}
	if time.Since(start) > time.Second || len(k.admissionStatus().Waiting) != 0 {
		t.Fatal("capacity cancellation did not settle promptly")
	}
	if pods, err := k.pods(t.Context()); err != nil || len(pods) != 0 {
		t.Fatalf("cancelled waiters leaked Pods: %v %v", pods, err)
	}
}
func TestAdmissionGateWaitIsCancellable(t *testing.T) {
	k := kubeFixture(t)
	admissionMarker(t, "hold-create")
	firstCtx, stop := context.WithCancel(t.Context())
	defer stop()
	first := make(chan error, 1)
	go func() {
		_, err := k.Start(firstCtx, "first", baseSpec("/workspace"), runnerfiles.Plan{}, nil, nil)
		first <- err
	}()
	admissionEventually(t, func() bool {
		_, err := os.Stat(filepath.Join(os.Getenv("ARTEX_KUBE_TEST_STATE"), "create-attempts"))
		return err == nil
	})
	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := k.Start(ctx, "second", baseSpec("/workspace"), runnerfiles.Plan{}, nil, nil)
	if !errors.Is(err, context.DeadlineExceeded) || time.Since(start) > time.Second {
		t.Fatalf("queued cancellation waited for unrelated create: %v", err)
	}
	stop()
	select {
	case <-first:
	case <-time.After(3 * time.Second):
		t.Fatal("creating command did not cancel")
	}
}
func TestAdmissionPermanentErrorsAreNotCapacityRetries(t *testing.T) {
	for _, marker := range []string{"rbac-denied", "fail-wait", "fail-create"} {
		t.Run(marker, func(t *testing.T) {
			k := kubeFixture(t)
			admissionMarker(t, marker)
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			if _, err := k.Start(ctx, "permanent", baseSpec("/workspace"), runnerfiles.Plan{}, nil, nil); err == nil {
				t.Fatal("permanent failure hidden")
			}
			b, _ := os.ReadFile(filepath.Join(os.Getenv("ARTEX_KUBE_TEST_STATE"), "create-attempts"))
			if strings.Count(string(b), "\n") != 1 || k.admissionStatus().QuotaRejections != 0 {
				t.Fatal("non-quota error retried")
			}
		})
	}
}
func TestQuotaDiagnosticParserExcludesUnstructuredText(t *testing.T) {
	err := fmt.Errorf(`Kubernetes create: exit status 1: Error from server (Forbidden): pods "fixture" is forbidden: exceeded quota: pool, requested: limits.memory=2Gi,private=secret, used: limits.memory=17Gi, limited: limits.memory=17Gi`)
	q := parseQuotaRejection(err)
	if q == nil || q.Requested["limits.memory"] != "2Gi" || len(q.Requested) != 1 {
		t.Fatalf("numeric quota parser: %+v", q)
	}
	for _, message := range []string{"exceeded quota: secret", `Error from server (Forbidden): secrets "private" is forbidden: exceeded quota: pool, requested: pods=1, used: pods=1, limited: pods=1`} {
		if parseQuotaRejection(errors.New(message)) != nil {
			t.Fatal("unrelated denial treated as Pod quota")
		}
	}
}

func TestQuotaHeartbeatReachesControllerBeforeAcquireDeadline(t *testing.T) {
	k := kubeFixture(t)
	// The fake kubectl runs the sandbox on the test host, so give it an
	// explicitly mapped temporary directory instead of requiring /workspace.
	workspace := t.TempDir()
	k.Config.SharedMounts = []SharedMount{{Source: workspace, Path: workspace, PVC: "synthetic-workspace"}}
	marker := admissionMarker(t, "quota-block")
	h, err := runner.New(runner.Options{Store: &runner.MemoryStore{}, LeaseDuration: 10 * time.Second, PollWait: 4 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	srv := httptest.NewServer(http.StripPrefix("/runner/v1", h.Handler()))
	defer srv.Close()
	n, _ := testNode(t)
	n.Executor = k
	n.Config.ServerURL = srv.URL
	n.Config.AllowInsecureHTTP = true
	n.HTTP = srv.Client()
	n.Config.Executor = "kubernetes"
	n.Config.Capabilities.Executor = "kubernetes"
	n.Config.WorkspaceDir = k.WorkspaceDir
	n.Config.Kubernetes = k.Config
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	token, _, err := h.CreateRegistrationToken(ctx, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- n.Run(ctx, token) }()
	defer func() {
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("manager did not settle")
		}
	}()
	admissionEventually(t, func() bool { return len(h.ListNodes()) == 1 && n.admissionEnabled.Load() })
	sp := baseSpec(workspace)
	sp.Files, err = json.Marshal(runnerfiles.Plan{Mode: runnerfiles.SharedMode, Mounts: []runnerfiles.Mount{{Name: "workspace", Source: workspace, Target: workspace}}})
	if err != nil {
		t.Fatal(err)
	}
	sp.ThreadID = "synthetic-quota"
	sp.TaskID = "synthetic"
	acquireCtx, stop := context.WithTimeout(ctx, 1500*time.Millisecond)
	defer stop()
	if _, err = h.Acquire(acquireCtx, sp); !errors.Is(err, runner.ErrCapacityUnavailable) {
		t.Fatalf("capacity metadata lost across real HTTP heartbeat: %v", err)
	}
	admissionEventually(t, func() bool { return h.ListNodes()[0].ActiveSessions == 0 })
	statuses := h.SessionStatuses()
	if len(statuses) != 1 || statuses[0].ReasonCode != "runner_capacity_unavailable" {
		t.Fatalf("failure diagnostics missing: %+v", statuses)
	}
	if err = os.Remove(marker); err != nil {
		t.Fatal(err)
	}
	session, err := h.Acquire(ctx, sp)
	if err != nil {
		t.Fatal(err)
	}
	if err = session.Close(); err != nil {
		t.Fatal(err)
	}
	admissionEventually(t, func() bool { return h.ListNodes()[0].ActiveSessions == 0 })
	if pods, err := k.pods(ctx); err != nil || len(pods) != 0 {
		t.Fatalf("recovery leaked Pod: %v %v", pods, err)
	}
}

func TestColdStartEvictsQuotaBlockedPrewarmWithoutDeadlock(t *testing.T) {
	k, _, _, _ := warmFixture(t)
	admissionMarker(t, "quota-block")
	if err := k.reconcilePool(t.Context()); err != nil {
		t.Fatal(err)
	}
	admissionEventually(t, func() bool { s := k.PoolStatus(); return s.Preparing == 1 && s.Admission.QuotaRejections > 0 })
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := k.Start(ctx, "cold-demand", baseSpec("/workspace"), runnerfiles.Plan{}, nil, nil)
		done <- err
	}()
	admissionEventually(t, func() bool {
		s := k.admissionStatus()
		return len(s.Waiting) == 1 && s.Waiting[0].SessionID == "cold-demand"
	})
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("cold start deadlocked on prewarm retirement")
	}
	k.poolMu.Lock()
	remaining := len(k.warm)
	k.poolMu.Unlock()
	if remaining != 0 {
		t.Fatal("quota-blocked prewarm was not retired")
	}
}
