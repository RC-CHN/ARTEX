package server

import (
	"context"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"time"

	"github.com/RC-CHN/ARTEX/agent"
	"github.com/RC-CHN/ARTEX/runner"
)

// Kubernetes Pod mounts and process environment are immutable. Prewarm concrete
// queued layouts only; never replace real mounts with filesystem aliases.
func (s *Server) runnerWarmProfiles(cfg runnerConfig) []runner.WarmProfile {
	profiles := []runner.WarmProfile{}
	if !cfg.Enabled {
		return profiles
	}
	for name, env := range cfg.Environments {
		if env.WarmIdle > 0 && env.Resources.GPUCount == 0 {
			profiles = append(profiles, runner.WarmProfile{Environment: name, Idle: env.WarmIdle})
		}
	}
	sort.Slice(profiles, func(i, j int) bool { return profiles[i].Environment < profiles[j].Environment })
	return profiles
}

func (s *Server) refreshRunnerPool(cfg runnerConfig, h *runner.Hub) {
	if !s.runnerPoolMu.TryLock() {
		return
	}
	if time.Since(s.runnerPoolAt) < time.Second {
		s.runnerPoolMu.Unlock()
		return
	}
	s.runnerPoolAt = time.Now()
	// Frontier/skill lookups must never delay heartbeats or file authorization.
	// Keep at most one refresh in flight even when database reads are slow.
	if !s.background(func() {
		defer s.runnerPoolMu.Unlock()
		parent := s.ctx
		if parent == nil {
			parent = context.Background()
		}
		refreshCtx, cancel := context.WithTimeout(parent, 2*time.Second)
		defer cancel()
		s.buildRunnerPool(refreshCtx, cfg, h)
	}) {
		s.runnerPoolMu.Unlock()
	}
}

func (s *Server) buildRunnerPool(ctx context.Context, cfg runnerConfig, h *runner.Hub) {
	profiles := s.runnerWarmProfiles(cfg)
	mode := cfg.DefaultMode
	if cfg.AgentModes["worker"] != "" {
		mode = cfg.AgentModes["worker"]
	}
	if mode == "remote" && len(profiles) > 0 {
		counts := map[string]int{}
		for _, task := range s.m.List() {
			if ctx.Err() != nil {
				break
			}
			if task.lifecycleSnapshot().Status != "running" {
				continue
			}
			frontier, err := task.Store.Frontier(32)
			if err != nil {
				continue
			}
			for _, intent := range frontier {
				if len(profiles) >= 64 || ctx.Err() != nil {
					break
				}
				full := true
				for name, env := range cfg.Environments {
					full = full && (env.WarmIdle == 0 || env.Resources.GPUCount > 0 || counts[name] >= env.WarmIdle)
				}
				if full {
					break
				}
				id, _ := strconv.ParseInt(task.ID, 10, 64)
				dir := filepath.Join(s.m.dir, "tasks", task.ID, "i"+strconv.FormatInt(intent.ID, 10))
				spec, _, err := s.runnerSpec(ctx, cfg, "worker", agent.RunInfo{TaskID: id, IntentID: intent.ID}, dir)
				if err != nil {
					continue
				}
				target := cfg.Environments[spec.Environment].WarmIdle
				if target <= 0 || counts[spec.Environment] >= target || spec.Resources.GPUCount > 0 {
					continue
				}
				counts[spec.Environment]++
				profiles = append(profiles, runner.WarmProfileFromSpec(spec))
			}
		}
	}
	if ctx.Err() != nil {
		return
	}
	// A slow refresh cannot resurrect a disabled/previous configuration.
	s.runnerMu.RLock()
	defer s.runnerMu.RUnlock()
	if s.runners == h && reflect.DeepEqual(cfg, s.runnerCfg) {
		h.SetWarmProfiles(profiles)
	}
}
