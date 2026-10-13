package runner

import (
	"encoding/json"
	"reflect"
)

func WarmProfileFromSpec(s RunSpec) WarmProfile {
	return WarmProfile{Environment: s.Environment, Idle: 1, Image: s.Image, Resources: s.Resources, Env: s.Env, Labels: s.Labels, WorkingDir: s.WorkingDir, Files: s.Files, NodeID: s.NodeID}
}

// Profiles are reconstructed from durable application configuration on startup.
// MCP/tool initialization belongs to the assigned session, not an idle Pod.
func (h *Hub) SetWarmProfiles(profiles []WarmProfile) {
	raw, _ := json.Marshal(profiles)
	var detached []WarmProfile
	_ = json.Unmarshal(raw, &detached)
	h.mu.Lock()
	defer h.mu.Unlock()
	if !reflect.DeepEqual(h.warmProfiles, detached) {
		h.warmProfiles = detached
		h.signalLocked()
	}
}

func (h *Hub) poolPolicyLocked(n *nodeRecord) *PoolPolicy {
	out := &PoolPolicy{Slots: n.effectiveSlots(), CPUMillis: n.effectiveCPU(), MemoryMiB: n.effectiveMemory(), Profiles: []WarmProfile{}}
	if !n.Enabled {
		return out
	}
	supported := func(p WarmProfile) bool {
		if p.NodeID != "" && p.NodeID != n.ID {
			return false
		}
		match := false
		for _, e := range n.Capabilities.Environments {
			match = match || e == p.Environment
		}
		for k, v := range p.Labels {
			if n.Capabilities.Labels[k] != v {
				match = false
			}
		}
		return match
	}
	budget := map[string]int{}
	for _, p := range h.warmProfiles {
		if p.WorkingDir == "" && supported(p) {
			budget[p.Environment] = p.Idle
			out.Profiles = append(out.Profiles, p)
		}
	}
	candidates := []WarmProfile{}
	for _, s := range h.state.Sessions {
		if s.NodeID == n.ID && s.State == "opening" {
			candidates = append(candidates, WarmProfileFromSpec(s.Spec))
		}
	}
	for _, p := range h.warmProfiles {
		if p.WorkingDir != "" {
			candidates = append(candidates, p)
		}
	}
	seen := map[string]bool{}
	for _, p := range candidates {
		if !supported(p) || budget[p.Environment] <= 0 {
			continue
		}
		raw, _ := json.Marshal(p)
		key := string(raw)
		if seen[key] {
			continue
		}
		seen[key] = true
		budget[p.Environment]--
		out.Profiles = append(out.Profiles, p)
	}
	return out
}
