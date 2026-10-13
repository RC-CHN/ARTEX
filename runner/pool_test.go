package runner

import (
	"encoding/json"
	"testing"
)

func TestWarmPolicyHonorsPlacementAndAdministrativeLimits(t *testing.T) {
	h := newTestHub(t, nil, nil)
	caps := defaultCapabilities()
	caps.Executor = "kubernetes"
	n := registerNode(t, h, caps)
	base := WarmProfile{Environment: "default", Idle: 2}
	eligible := WarmProfile{Environment: "default", Idle: 1, WorkingDir: "/workspace/task", NodeID: n.id, Labels: map[string]string{"os": "linux"}}
	wrongNode := eligible
	wrongNode.NodeID = "different-node"
	wrongLabel := eligible
	wrongLabel.Labels = map[string]string{"os": "other"}
	h.SetWarmProfiles([]WarmProfile{base, wrongNode, wrongLabel, eligible, eligible})
	if err := h.SetNodeLimits(n.id, 2, 2000, 2048); err != nil {
		t.Fatal(err)
	}
	p := n.poll(t).Pool
	if p == nil || p.Slots != 2 || p.CPUMillis != 2000 || p.MemoryMiB != 2048 || len(p.Profiles) != 2 || p.Profiles[1].NodeID != n.id {
		t.Fatalf("incorrect filtered pool: %+v", p)
	}
	// Caller-owned maps must not mutate hub policy after publication.
	eligible.Labels["os"] = "changed"
	if len(n.poll(t).Pool.Profiles) != 2 {
		t.Fatal("warm policy shared caller maps")
	}
	h.SetWarmProfiles(nil)
	if len(n.poll(t).Pool.Profiles) != 0 {
		t.Fatal("prewarming remained enabled")
	}
}

func TestWarmProfileContainsNoMCPOrModelSessionState(t *testing.T) {
	s := spec("thread-secret")
	s.MCPs = []MCPConfig{{Name: "private", Transport: "http", Headers: map[string]string{"Authorization": "mcp-secret"}}}
	b, err := json.Marshal(WarmProfileFromSpec(s))
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]any
	if err := json.Unmarshal(b, &fields); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"mcps", "thread_id", "tools", "model", "prompt"} {
		if _, ok := fields[key]; ok {
			t.Fatalf("prewarm disclosed session-only %s", key)
		}
	}
}
