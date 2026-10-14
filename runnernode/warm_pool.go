package runnernode

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"sync"
	"time"

	"github.com/RC-CHN/ARTEX/runner"
	"github.com/RC-CHN/ARTEX/runnerfiles"
)

type warmPod struct {
	id, dir, key string
	profile      runner.WarmProfile
	sandbox      Sandbox
	cancel       context.CancelFunc
	ready        chan struct{}
	err          error
	retiring     bool // protected by poolMu
}

const idleMarker = ".artex-prewarm-idle"

// Remove and persist the idle marker before inputs or MCPs can write anything.
// A restart may discard only caches still explicitly marked as unused.
func (p *warmPod) activate() error {
	if err := os.Remove(filepath.Join(p.dir, idleMarker)); err != nil {
		return err
	}
	dir, err := os.Open(p.dir)
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}

func (k *KubernetesExecutor) cleanUnusedWarmCaches() error {
	entries, err := os.ReadDir(k.WorkspaceDir)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if !entry.IsDir() || len(entry.Name()) != 24 {
			continue
		}
		dir := filepath.Join(k.WorkspaceDir, entry.Name())
		marker, err := os.Lstat(filepath.Join(dir, idleMarker))
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return err
		}
		if !marker.Mode().IsRegular() {
			continue
		}
		// Be conservative if a crash/filesystem anomaly retained a stale marker:
		// input/output files always win over automatic idle-cache reclamation.
		empty := true
		err = filepath.WalkDir(dir, func(path string, item os.DirEntry, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			if !item.IsDir() && path != filepath.Join(dir, idleMarker) {
				empty = false
			}
			return nil
		})
		if err != nil {
			return err
		}
		if empty {
			if err := os.RemoveAll(dir); err != nil {
				return err
			}
		}
	}
	return nil
}

type warmSandbox struct {
	Sandbox
	cancel context.CancelFunc
	stopMu sync.Mutex
}

type claimedSandbox struct {
	Sandbox
	once    sync.Once
	release func()
}

func (s *claimedSandbox) Stop() error {
	err := s.Sandbox.Stop()
	if err == nil {
		s.once.Do(s.release)
	}
	return err
}

// poolMu must be held. A temporary disappearance from the controller's queued
// frontier must not allow a second Pod for an execution that is still running.
func (k *KubernetesExecutor) claimLocked(key string, sandbox Sandbox) Sandbox {
	if k.consumed == nil {
		k.consumed = map[string]bool{}
	}
	if k.claimed == nil {
		k.claimed = map[string]int{}
	}
	k.consumed[key] = true
	k.claimed[key]++
	return &claimedSandbox{Sandbox: sandbox, release: func() {
		k.poolMu.Lock()
		defer k.poolMu.Unlock()
		k.claimed[key]--
		if k.claimed[key] == 0 {
			delete(k.claimed, key)
		}
	}}
}

func (s *warmSandbox) Stop() error {
	s.stopMu.Lock()
	defer s.stopMu.Unlock()
	s.cancel()
	return s.Sandbox.Stop()
}

func profileKey(p runner.WarmProfile) string {
	p.Idle = 0
	raw, _ := json.Marshal(p)
	return safeID(string(raw))
}

func (k *KubernetesExecutor) UpdatePool(policy runner.PoolPolicy) {
	raw, _ := json.Marshal(policy)
	var copy runner.PoolPolicy
	_ = json.Unmarshal(raw, &copy)
	k.poolMu.Lock()
	defer k.poolMu.Unlock()
	k.policy = copy
	k.policyAt = time.Now()
}
func (k *KubernetesExecutor) PoolStatus() runner.PoolStatus {
	k.poolMu.Lock()
	defer k.poolMu.Unlock()
	out := k.poolStatus
	for _, p := range k.warm {
		if p.retiring {
			continue
		}
		select {
		case <-p.ready:
			if p.err == nil {
				out.Idle++
			}
		default:
			out.Preparing++
		}
	}
	return out
}
func (k *KubernetesExecutor) policySnapshot() runner.PoolPolicy {
	k.poolMu.Lock()
	defer k.poolMu.Unlock()
	p := k.policy
	// Lost controller contact cannot keep growing an idle pool.
	if time.Since(k.policyAt) > 20*time.Second {
		p.Profiles = nil
	}
	return p
}
func warmLayout(p runner.WarmProfile) (runnerfiles.Plan, error) { return decodePlan(p.Files) }
func warmMatches(p runner.WarmProfile, spec runner.RunSpec, plan runnerfiles.Plan) bool {
	if p.Environment != spec.Environment || p.Image != spec.Image || p.Resources != spec.Resources || p.WorkingDir != spec.WorkingDir || !maps.Equal(p.Env, spec.Env) {
		return false
	}
	candidate, err := decodePlan(p.Files)
	return err == nil && reflect.DeepEqual(candidate, plan)
}

// Claim removes a clean Pod from the idle set exactly once. Task files are
// supplied only after ownership is assigned. A used Pod never returns to idle.
func (k *KubernetesExecutor) ClaimWarm(ctx context.Context, id string, spec runner.RunSpec, plan runnerfiles.Plan) (*warmPod, error) {
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	k.poolMu.Lock()
	defer k.poolMu.Unlock()
	if time.Since(k.policyAt) > 20*time.Second {
		return nil, nil
	}
	for name, p := range k.warm {
		if p.retiring || !warmMatches(p.profile, spec, plan) {
			continue
		}
		select {
		case <-p.ready:
		default:
			continue
		}
		if p.err != nil {
			continue
		}
		if live, ok := p.sandbox.(*warmSandbox); ok {
			if rpc, ok := live.Sandbox.(*rpcSandbox); ok {
				select {
				case <-rpc.done:
					p.retiring = true
					continue
				default:
				}
			}
		}
		// Only claim profiles still present in the most recent policy.
		valid := false
		for _, q := range k.policy.Profiles {
			valid = valid || q.Idle > 0 && profileKey(q) == p.key
		}
		if !valid {
			continue
		}
		delete(k.warm, name)
		p.sandbox = k.claimLocked(p.key, p.sandbox)
		return p, nil
	}
	return nil, nil
}

func (p *warmPod) roots(plan runnerfiles.Plan) (map[string]string, error) {
	roots := map[string]string{}
	for _, m := range plan.Mounts {
		roots[m.Name] = filepath.Join(p.dir, "mounts", safeID(m.Name))
		if err := secureDir(roots[m.Name]); err != nil {
			return nil, err
		}
	}
	return roots, nil
}

func (k *KubernetesExecutor) createWarm(ctx context.Context, p *warmPod, policy runner.PoolPolicy) {
	defer k.poolWG.Done()
	defer close(p.ready)
	plan, err := warmLayout(p.profile)
	if err == nil {
		err = secureDir(p.dir)
	}
	if err == nil {
		err = os.WriteFile(filepath.Join(p.dir, idleMarker), []byte("unused prewarm cache\n"), 0600)
	}
	roots := map[string]string{}
	if err == nil && plan.Mode == runnerfiles.SharedMode {
		roots, err = k.sharedRoots(plan)
	}
	if err == nil && plan.Mode != runnerfiles.SharedMode {
		for _, m := range plan.Mounts {
			roots[m.Name] = filepath.Join(p.dir, "mounts", safeID(m.Name))
			if err = secureDir(roots[m.Name]); err != nil {
				break
			}
		}
	}
	if err == nil {
		spec := runner.RunSpec{Environment: p.profile.Environment, Image: p.profile.Image, Resources: p.profile.Resources, Env: p.profile.Env, WorkingDir: p.profile.WorkingDir, ConcurrencyLimit: policy.Slots, PoolCPUMillis: policy.CPUMillis, PoolMemoryMiB: policy.MemoryMiB}
		p.sandbox, err = k.startPod(ctx, p.id, spec, plan, roots, nil)
	}
	if p.sandbox != nil {
		p.sandbox = &warmSandbox{Sandbox: p.sandbox, cancel: p.cancel}
	}
	p.err = err
}
func (k *KubernetesExecutor) retireWarm(ctx context.Context, p *warmPod) error {
	p.cancel()
	select {
	case <-p.ready:
	case <-ctx.Done():
		return ctx.Err()
	}
	var err error
	if p.sandbox != nil {
		err = p.sandbox.Stop()
	} else {
		err = k.remove(ctx, containerName(k.NodeID, p.id))
	}
	if err != nil {
		return err
	}
	if err := os.RemoveAll(p.dir); err != nil {
		return err
	}
	k.poolMu.Lock()
	if k.warm[p.id] == p {
		delete(k.warm, p.id)
	}
	k.poolMu.Unlock()
	return nil
}
func (k *KubernetesExecutor) evictIdle(ctx context.Context) error {
	k.poolMu.Lock()
	list := []*warmPod{}
	for _, p := range k.warm {
		p.retiring = true
		list = append(list, p)
	}
	k.poolMu.Unlock()
	for _, p := range list {
		if err := k.retireWarm(ctx, p); err != nil {
			return err
		}
	}
	return nil
}

func (k *KubernetesExecutor) RunPool(ctx context.Context) {
	defer func() {
		clean, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_ = k.evictIdle(clean)
		k.poolWG.Wait()
	}()
	tick := time.NewTicker(2 * time.Second)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
		if err := k.reconcilePool(ctx); err != nil {
			k.poolMu.Lock()
			message := err.Error()
			if len(message) > 2048 {
				message = message[:2048]
			}
			k.poolStatus.Error = message
			k.poolMu.Unlock()
		}
	}
}
func (k *KubernetesExecutor) reconcilePool(ctx context.Context) error {
	policy := k.policySnapshot()
	if k.coldWaiting.Load() > 0 {
		policy.Profiles = nil
	}
	desired := map[string]runner.WarmProfile{}
	for _, p := range policy.Profiles {
		if p.WorkingDir == "" {
			continue
		}
		var err error

		if p.Idle < 0 || p.Idle > 32 || p.Resources.CPUMillis <= 0 || p.Resources.MemoryMiB <= 0 || p.Resources.GPUCount != 0 {
			return fmt.Errorf("invalid prewarm profile")
		}
		if _, err = warmLayout(p); err != nil {
			return err
		}
		desired[profileKey(p)] = p
	}
	pods, err := k.pods(ctx)
	if err != nil {
		return err
	}
	status := runner.PoolStatus{Pods: len(pods)}
	livePods := map[string]bool{}
	for _, pod := range pods {
		livePods[pod.Metadata.Name] = pod.Status.Phase != "Failed" && pod.Status.Phase != "Succeeded"
		var cpu, mem int64
		_, _ = fmt.Sscan(pod.Metadata.Annotations["site.artex.cpu-millis"], &cpu)
		_, _ = fmt.Sscan(pod.Metadata.Annotations["site.artex.memory-mib"], &mem)
		status.CPUMillis += cpu
		status.MemoryMiB += mem
	}
	over := len(pods) > min(k.MaxPods, policy.Slots) || (policy.CPUMillis > 0 && status.CPUMillis > policy.CPUMillis) || (policy.MemoryMiB > 0 && status.MemoryMiB > policy.MemoryMiB)
	k.poolMu.Lock()
	k.poolStatus = status
	for key := range k.consumed {
		if _, ok := desired[key]; !ok && k.claimed[key] == 0 {
			delete(k.consumed, key)
		}
	}
	retiring := []*warmPod{}
	counts := map[string]int{}
	preparing := false
	for _, p := range k.warm {
		q, ok := desired[p.key]
		failed := false
		select {
		case <-p.ready:
			failed = p.err != nil || !livePods[containerName(k.NodeID, p.id)]
		default:
			preparing = true
		}
		if p.retiring || !ok || counts[p.key] >= q.Idle || over || failed {
			p.retiring = true
			retiring = append(retiring, p)
		} else {
			counts[p.key]++
		}
	}
	k.poolMu.Unlock()
	for _, p := range retiring {
		if err := k.retireWarm(ctx, p); err != nil {
			return err
		}
	}
	if len(retiring) > 0 || preparing || policy.Slots < 1 {
		return nil
	}
	keys := []string{}
	for key := range desired {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		p := desired[key]
		k.poolMu.Lock()
		consumed := k.consumed[key]
		k.poolMu.Unlock()
		if consumed {
			continue
		}
		if counts[key] >= p.Idle || len(pods) >= min(k.MaxPods, policy.Slots) {
			continue
		}
		spec := runner.RunSpec{Resources: p.Resources, PoolCPUMillis: policy.CPUMillis, PoolMemoryMiB: policy.MemoryMiB}
		if !podBudgetAvailable(pods, spec) {
			continue
		}
		id := "warm-" + randomID()
		warmCtx, cancel := context.WithCancel(ctx)
		w := &warmPod{id: id, dir: filepath.Join(k.WorkspaceDir, safeID(id)), key: key, profile: p, cancel: cancel, ready: make(chan struct{})}
		k.poolMu.Lock()
		current := false
		for _, latest := range k.policy.Profiles {
			current = current || latest.Idle > 0 && profileKey(latest) == key
		}
		if !current || k.coldWaiting.Load() > 0 || k.consumed[key] {
			k.poolMu.Unlock()
			cancel()
			continue
		}
		if k.warm == nil {
			k.warm = map[string]*warmPod{}
		}
		k.warm[id] = w
		k.poolMu.Unlock()
		k.poolWG.Add(1)
		go k.createWarm(warmCtx, w, policy)
		break
	}
	return nil
}

func prepareWarmProfile(c Config, p runner.WarmProfile) (runner.WarmProfile, error) {
	if p.WorkingDir == "" {
		return p, nil
	}
	workingDir := p.WorkingDir
	if workingDir == "" {
		workingDir = "/workspace"
	}
	spec, _, err := c.Prepare(runner.RunSpec{Image: p.Image, Environment: p.Environment, Env: p.Env, WorkingDir: workingDir, Files: p.Files, Resources: p.Resources})
	if err != nil {
		return p, err
	}
	p.Image, p.Env = spec.Image, spec.Env
	return p, nil
}
