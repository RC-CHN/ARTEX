package runnernode

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/RC-CHN/ARTEX/runner"
	"github.com/RC-CHN/ARTEX/runnerfiles"
)

type KubernetesConfig struct {
	SharedMinFreeBytes  uint64            `json:"shared_min_free_bytes,omitempty"`
	SharedMinFreeInodes uint64            `json:"shared_min_free_inodes,omitempty"`
	SharedMounts        []SharedMount     `json:"shared_mounts,omitempty"`
	Binary              string            `json:"binary,omitempty"`
	Namespace           string            `json:"namespace"`
	WorkspacePVC        string            `json:"workspace_pvc"`
	WorkspaceSubPath    string            `json:"workspace_sub_path,omitempty"`
	ServiceAccount      string            `json:"service_account,omitempty"`
	ImagePullSecrets    []string          `json:"image_pull_secrets,omitempty"`
	NodeSelector        map[string]string `json:"node_selector,omitempty"`
}

var kubeName = regexp.MustCompile(`^[a-z0-9]([-a-z0-9.]*[a-z0-9])?$`)

func (c *KubernetesConfig) validate() error {
	for _, s := range append([]string{c.Namespace, c.WorkspacePVC}, c.ImagePullSecrets...) {
		if !kubeName.MatchString(s) || len(s) > 253 {
			return fmt.Errorf("invalid Kubernetes namespace, PVC or image pull secret")
		}
	}
	if c.ServiceAccount == "" {
		c.ServiceAccount = "artex-sandbox"
	}
	if !kubeName.MatchString(c.ServiceAccount) {
		return fmt.Errorf("invalid sandbox service account")
	}
	if c.WorkspaceSubPath != "" && !safeSubPath(c.WorkspaceSubPath) {
		return fmt.Errorf("invalid Kubernetes workspace subpath")
	}
	if c.Binary == "" {
		c.Binary = "kubectl"
	}
	if err := validateSharedMounts(c.SharedMounts); err != nil {
		return err
	}
	if c.SharedMinFreeBytes == 0 {
		c.SharedMinFreeBytes = 1 << 30
	}
	if c.SharedMinFreeInodes == 0 {
		c.SharedMinFreeInodes = 10000
	}
	return nil
}
func safeSubPath(p string) bool {
	return p != "" && p != "." && p != ".." && !filepath.IsAbs(p) && filepath.Clean(p) == p && !strings.HasPrefix(p, "../") && !strings.ContainsAny(p, "\\\x00\n")
}

// The manager alone has API credentials. Every execution receives only its PVC
// subdirectories and scratch volumes, with no service account token or hostPath.
type KubernetesExecutor struct {
	Config                              KubernetesConfig
	WorkspaceDir, NodeID, SandboxBinary string
	MaxPods                             int
	Log                                 io.Writer
	createOnce                          sync.Once
	createGate                          chan struct{}
	admissionMu                         sync.Mutex
	admission                           runner.AdmissionStatus
	admissionWaiting                    map[string]runner.AdmissionWait
	quotaFailures                       int
	admissionLogAt                      time.Time
	admissionEvents                     chan struct{}
	poolMu                              sync.Mutex
	policy                              runner.PoolPolicy
	policyAt                            time.Time
	warm                                map[string]*warmPod
	consumed                            map[string]bool
	claimed                             map[string]int
	poolStatus                          runner.PoolStatus
	poolWG                              sync.WaitGroup
	coldWaiting                         atomic.Int32
}
type podState struct {
	Metadata struct {
		Name        string            `json:"name"`
		Labels      map[string]string `json:"labels"`
		Annotations map[string]string `json:"annotations"`
	} `json:"metadata"`
	Status struct {
		Phase string `json:"phase"`
	} `json:"status"`
}

func (k *KubernetesExecutor) command(ctx context.Context, args ...string) *exec.Cmd {
	binary := k.Config.Binary
	if binary == "" {
		binary = "kubectl"
	}
	return exec.CommandContext(ctx, binary, append([]string{"--namespace", k.Config.Namespace}, args...)...)
}
func (k *KubernetesExecutor) output(ctx context.Context, timeout time.Duration, input []byte, args ...string) ([]byte, error) {
	// Bound the subprocess, not kubectl's global --request-timeout flag. In the
	// pinned client that flag makes the empty kubeconfig differ from its defaults
	// and bypasses ServiceAccount-based in-cluster configuration discovery.
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := k.command(ctx, args...)
	cmd.Stdin = bytes.NewReader(input)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	b, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("Kubernetes %s: %w: %s", args[0], errors.Join(err, ctx.Err()), strings.TrimSpace(stderr.String()))
	}
	return b, nil
}
func (k *KubernetesExecutor) pods(ctx context.Context) ([]podState, error) {
	b, err := k.output(ctx, 10*time.Second, nil, "get", "pods", "-l", "site.artex.runner="+safeID(k.NodeID), "-o", "json")
	if err != nil {
		return nil, err
	}
	var list struct {
		Items []podState `json:"items"`
	}
	err = json.Unmarshal(b, &list)
	return list.Items, err
}
func (k *KubernetesExecutor) subPath(root string) (string, error) {
	if !filepath.IsAbs(k.WorkspaceDir) || !within(k.WorkspaceDir, root) {
		return "", fmt.Errorf("Pod mount is outside the runner workspace")
	}
	rel, err := filepath.Rel(k.WorkspaceDir, root)
	if err != nil || !safeSubPath(rel) {
		return "", fmt.Errorf("invalid Pod session subpath")
	}
	return filepath.Join(k.Config.WorkspaceSubPath, rel), nil
}

func (k *KubernetesExecutor) Pod(id string, spec runner.RunSpec, plan runnerfiles.Plan, roots map[string]string) (map[string]any, error) {
	cfg := k.Config
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	if !imageDigest.MatchString(spec.Image) {
		return nil, fmt.Errorf("Pod image must be digest-pinned")
	}
	if spec.Resources.GPUCount != 0 {
		return nil, fmt.Errorf("use an external Docker runner for GPU execution")
	}
	uid, gid := os.Getuid(), os.Getgid()
	// Native Pods always use an unprivileged identity, including development.
	if uid == 0 {
		uid = 10001
	}
	if gid == 0 {
		gid = 10001
	}
	volumes := []any{
		map[string]any{"name": "home", "emptyDir": map[string]any{"medium": "Memory", "sizeLimit": "256Mi"}},
		map[string]any{"name": "tmp", "emptyDir": map[string]any{"medium": "Memory", "sizeLimit": "512Mi"}},
	}
	if plan.Mode != runnerfiles.SharedMode {
		volumes = append(volumes, map[string]any{"name": "cache", "persistentVolumeClaim": map[string]any{"claimName": k.Config.WorkspacePVC}})
	}
	mounts := []any{map[string]any{"name": "home", "mountPath": "/home/runner"}, map[string]any{"name": "tmp", "mountPath": "/tmp"}}
	if !inMounts("/workspace", plan) {
		volumes = append(volumes, map[string]any{"name": "scratch", "emptyDir": map[string]any{"sizeLimit": "512Mi"}})
		mounts = append(mounts, map[string]any{"name": "scratch", "mountPath": "/workspace"})
	}
	sharedVolumes := map[string]string{}
	for _, m := range plan.Mounts {
		root := roots[m.Name]
		volumeName := "cache"
		var sub string
		var err error
		if plan.Mode == runnerfiles.SharedMode {
			var mapping SharedMount
			var mappedRoot string
			mapping, mappedRoot, sub, err = k.sharedMount(m)
			if err == nil && mappedRoot != root {
				err = fmt.Errorf("shared root differs from provisioned mapping")
			}
			volumeName = sharedVolumes[mapping.PVC]
			if volumeName == "" {
				volumeName = "shared-" + safeID(mapping.PVC)
				sharedVolumes[mapping.PVC] = volumeName
				// One volume per claim, with access enforced on each mount. Multiple
				// aliases of one CSI claim with different volume-level readOnly flags
				// can stall kubelet volume setup. The cache isn't mounted in this mode.
				volumes = append(volumes, map[string]any{"name": volumeName, "persistentVolumeClaim": map[string]any{"claimName": mapping.PVC}})
			}
		} else {
			sub, err = k.subPath(root)
		}
		if err != nil {
			return nil, err
		}
		readOnly := true
		for _, w := range m.Writable {
			if w == "." {
				readOnly = false
			}
		}
		mounts = append(mounts, map[string]any{"name": volumeName, "mountPath": m.Target, "subPath": sub, "readOnly": readOnly})
		if readOnly {
			for _, w := range m.Writable {
				if !safeSubPath(w) {
					return nil, fmt.Errorf("invalid Pod writable prefix")
				}
				if plan.Mode == runnerfiles.SharedMode {
					err = sharedWritable(root, w)
				} else {
					err = prepareWritable(root, filepath.Join(root, w))
				}
				if err != nil {
					return nil, err
				}
				mounts = append(mounts, map[string]any{"name": volumeName, "mountPath": filepath.Join(m.Target, w), "subPath": filepath.Join(sub, w), "readOnly": false})
			}
		}
	}
	env := mergeEnv(spec.Env, map[string]string{"HOME": "/home/runner", "TMPDIR": "/tmp", "TMP": "/tmp", "TEMP": "/tmp"})
	vars := []any{}
	for _, pair := range envList(env) {
		name, value, _ := strings.Cut(pair, "=")
		vars = append(vars, map[string]string{"name": name, "value": value})
	}
	cpu, mem := spec.Resources.CPUMillis, spec.Resources.MemoryMiB
	if cpu <= 0 {
		cpu = 1000
	}
	if mem <= 0 {
		mem = 1024
	}
	limits := map[string]string{"cpu": fmt.Sprintf("%dm", cpu), "memory": fmt.Sprintf("%dMi", mem), "ephemeral-storage": "1Gi"}
	binary := k.SandboxBinary
	if binary == "" {
		binary = "/usr/local/bin/artex-runner"
	}
	container := map[string]any{
		"name": "sandbox", "image": spec.Image, "imagePullPolicy": "IfNotPresent",
		"command": []string{binary, "--sandbox", "--heartbeat-timeout=30s"}, "workingDir": spec.WorkingDir,
		"stdin": true, "stdinOnce": true, "tty": false, "env": vars, "volumeMounts": mounts,
		"resources":       map[string]any{"requests": limits, "limits": limits},
		"securityContext": map[string]any{"allowPrivilegeEscalation": false, "readOnlyRootFilesystem": true, "capabilities": map[string]any{"drop": []string{"ALL"}}},
	}
	ps := map[string]any{
		"restartPolicy": "Never", "automountServiceAccountToken": false, "enableServiceLinks": false,
		"serviceAccountName": cfg.ServiceAccount, "terminationGracePeriodSeconds": 5,
		"securityContext": map[string]any{"runAsNonRoot": true, "runAsUser": uid, "runAsGroup": gid, "fsGroup": gid, "fsGroupChangePolicy": "OnRootMismatch", "seccompProfile": map[string]string{"type": "RuntimeDefault"}},
		"containers":      []any{container}, "volumes": volumes,
	}
	if plan.Mode == runnerfiles.SharedMode {
		// A large shared PVC must never trigger recursive ownership changes on
		// each Pod startup. Provision its UID/GID once during migration instead.
		sc := ps["securityContext"].(map[string]any)
		delete(sc, "fsGroup")
		delete(sc, "fsGroupChangePolicy")
	}
	if len(k.Config.NodeSelector) > 0 {
		ps["nodeSelector"] = k.Config.NodeSelector
	}
	if len(k.Config.ImagePullSecrets) > 0 {
		secrets := []any{}
		for _, s := range k.Config.ImagePullSecrets {
			secrets = append(secrets, map[string]string{"name": s})
		}
		ps["imagePullSecrets"] = secrets
	}
	return map[string]any{"apiVersion": "v1", "kind": "Pod", "metadata": map[string]any{"name": containerName(k.NodeID, id), "namespace": k.Config.Namespace, "annotations": map[string]string{"site.artex.cpu-millis": strconv.FormatInt(cpu, 10), "site.artex.memory-mib": strconv.FormatInt(mem, 10)}, "labels": map[string]string{"site.artex.runner": safeID(k.NodeID), "site.artex.session": safeID(id), "app.kubernetes.io/name": "artex-sandbox"}}, "spec": ps}, nil
}

func podBudgetAvailable(pods []podState, spec runner.RunSpec) bool {
	cpu, mem := spec.Resources.CPUMillis, spec.Resources.MemoryMiB
	if cpu <= 0 {
		cpu = 1000
	}
	if mem <= 0 {
		mem = 1024
	}
	for _, p := range pods {
		c, err := strconv.ParseInt(p.Metadata.Annotations["site.artex.cpu-millis"], 10, 64)
		if err != nil || c <= 0 {
			return false
		}
		m, err := strconv.ParseInt(p.Metadata.Annotations["site.artex.memory-mib"], 10, 64)
		if err != nil || m <= 0 {
			return false
		}
		if c > 1<<50 || m > 1<<50 {
			return false
		}
		cpu += c
		mem += m
	}
	return (spec.PoolCPUMillis <= 0 || cpu <= spec.PoolCPUMillis) && (spec.PoolMemoryMiB <= 0 || mem <= spec.PoolMemoryMiB)
}

func (k *KubernetesExecutor) remove(ctx context.Context, name string) error {
	b, err := k.output(ctx, 5*time.Second, nil, "get", "pod", name, "--ignore-not-found", "-o", "json")
	if err != nil {
		return err
	}
	if len(bytes.TrimSpace(b)) == 0 {
		return nil
	}
	var pod podState
	if err := json.Unmarshal(b, &pod); err != nil {
		return err
	}
	if pod.Metadata.Labels["site.artex.runner"] != safeID(k.NodeID) {
		return fmt.Errorf("refusing to delete a Pod owned by another runner")
	}
	_, err = k.output(ctx, 21*time.Second, nil, "delete", "pod", name, "--ignore-not-found", "--wait=true", "--timeout=20s")
	return err // never force-delete: API disappearance must mean kubelet confirmed termination
}
func (k *KubernetesExecutor) CleanupOrphans(ctx context.Context) error {
	pods, err := k.pods(ctx)
	if err != nil {
		return err
	}
	for _, pod := range pods {
		if err = k.remove(ctx, pod.Metadata.Name); err != nil {
			return err
		}
	}
	return k.cleanUnusedWarmCaches()
}

func (k *KubernetesExecutor) Start(ctx context.Context, id string, spec runner.RunSpec, plan runnerfiles.Plan, roots map[string]string, gpus []string) (Sandbox, error) {
	k.coldWaiting.Add(1)
	defer k.coldWaiting.Add(-1)
	// Idle Pods yield to executions with a different image, quota or layout.
	if err := k.evictIdle(ctx); err != nil {
		return nil, err
	}
	return k.startPod(ctx, id, spec, plan, roots, gpus)
}

func (k *KubernetesExecutor) startPod(ctx context.Context, id string, spec runner.RunSpec, plan runnerfiles.Plan, roots map[string]string, gpus []string) (Sandbox, error) {
	if len(gpus) > 0 {
		return nil, fmt.Errorf("native Kubernetes executor does not accept physical GPU IDs")
	}
	pod, err := k.Pod(id, spec, plan, roots)
	if err != nil {
		return nil, err
	}
	name := containerName(k.NodeID, id)
	stop := func() error {
		stopCtx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
		defer cancel()
		return k.remove(stopCtx, name)
	}
	fail := func(err error) (Sandbox, error) {
		if cleanup := stop(); cleanup != nil {
			return &cleanupSandbox{stop: stop}, errors.Join(err, cleanup)
		}
		return nil, err
	}
	limit := k.MaxPods
	if spec.ConcurrencyLimit > 0 && spec.ConcurrencyLimit < limit {
		limit = spec.ConcurrencyLimit
	}
	if limit < 1 {
		return nil, fmt.Errorf("native Pod pool requires a positive maximum")
	}
	// Include Pending and Terminating Pods. A fenced lease is not evidence that
	// an unreachable kubelet has killed a process. Wait for actual removal.
	k.admissionStage(id, "pod_capacity", false)
	defer k.finishAdmission(id)
	for {
		if err := k.lockCreate(ctx); err != nil {
			return nil, err
		}
		if delay := k.admissionDelay(); delay > 0 {
			k.admissionStage(id, "quota", false)
			k.unlockCreate()
			if err := waitAdmission(ctx, delay); err != nil {
				return nil, err
			}
			continue
		}
		admission := spec
		currentLimit := limit
		k.poolMu.Lock()
		if !k.policyAt.IsZero() {
			currentLimit = min(currentLimit, k.policy.Slots)
			if k.policy.CPUMillis > 0 && (admission.PoolCPUMillis <= 0 || k.policy.CPUMillis < admission.PoolCPUMillis) {
				admission.PoolCPUMillis = k.policy.CPUMillis
			}
			if k.policy.MemoryMiB > 0 && (admission.PoolMemoryMiB <= 0 || k.policy.MemoryMiB < admission.PoolMemoryMiB) {
				admission.PoolMemoryMiB = k.policy.MemoryMiB
			}
		}
		k.poolMu.Unlock()
		pods, err := k.pods(ctx)
		if err != nil {
			k.unlockCreate()
			return nil, err
		}
		if len(pods) < currentLimit && podBudgetAvailable(pods, admission) {
			data, _ := json.Marshal(pod)
			k.admissionStage(id, "pod_create", true)
			_, err = k.output(ctx, 15*time.Second, data, "create", "-f", "-", "-o", "name")
			if q := parseQuotaRejection(err); q != nil && ctx.Err() == nil {
				k.recordQuotaRejection(q)
				k.admissionStage(id, "quota", false)
				k.unlockCreate()
				continue
			}
			if err == nil {
				k.recordPodCreated()
			}
			k.unlockCreate()
			if err != nil {
				return fail(err)
			}
			break
		}
		k.admissionStage(id, "pod_capacity", false)
		k.unlockCreate()
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(250 * time.Millisecond):
		}
	}
	k.admissionStage(id, "pod_ready", false)
	if _, err = k.output(ctx, 125*time.Second, nil, "wait", "--for=condition=Ready", "pod/"+name, "--timeout=120s"); err != nil {
		return fail(err)
	}
	cmd := k.command(ctx, "attach", "-i", name, "-c", "sandbox", "--pod-running-timeout=30s", "--quiet")
	sb, err := startRPC(cmd, stop, k.Log)
	if err != nil {
		return fail(err)
	}
	go func() {
		ticker := time.NewTicker(3 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-sb.done:
				return
			case <-ticker.C:
				beatCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
				_, err := sb.Request(beatCtx, SandboxRequest{Method: "heartbeat"})
				cancel()
				if err != nil {
					_ = sb.Stop()
					return
				}
			}
		}
	}()
	return sb, nil
}

type cleanupSandbox struct{ stop func() error }

func (s *cleanupSandbox) Request(context.Context, SandboxRequest) (SandboxReply, error) {
	return SandboxReply{}, fmt.Errorf("sandbox initialization failed")
}
func (s *cleanupSandbox) Stop() error { return s.stop() }
