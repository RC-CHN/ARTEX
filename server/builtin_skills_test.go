package server

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/RC-CHN/ARTEX/agent"
	"github.com/RC-CHN/ARTEX/db"
	actool "github.com/RC-CHN/ARTEX/norma/tool"
	"github.com/RC-CHN/ARTEX/skills"
)

func TestBuiltinSecuritySkillAssemblyAndVisibility(t *testing.T) {
	dsn, _, err := db.DSN()
	if err != nil {
		t.Skip(err)
	}
	pg, err := db.Open(dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer pg.Close()
	dir := t.TempDir()
	if err := skills.SeedDefaults(dir); err != nil {
		t.Fatal(err)
	}
	previous := agent.ToolAugment
	defer func() { agent.ToolAugment = previous }()
	wireAgentAugment(pg, dir, nil)
	for _, key := range []string{"mainagent", "planner", "worker", "auto"} {
		tools, _, cleanup := agent.ToolAugment(context.Background(), key)
		defer cleanup()
		var native actool.CoreTool
		for _, tool := range tools {
			if tool.Name() == "Skill" {
				native = tool
			}
		}
		if native == nil {
			t.Fatalf("missing native Skill for %s", key)
		}
		for _, name := range []string{"nuclei", "vulnx", "python-environment", "resource-creator", "tack"} {
			raw, _ := json.Marshal(map[string]string{"name": name})
			result, err := native.Call(context.Background(), raw, nil)
			if err != nil || result.IsError || len(result.Extra) != 1 {
				t.Fatalf("%s skill %s unavailable: %+v %v", key, name, result, err)
			}
		}
	}
	a, _ := pg.GetAgentByKey("worker")
	defer pg.ToggleSkillVisibility(a.ID, "nuclei", true)
	if err := pg.ToggleSkillVisibility(a.ID, "nuclei", false); err != nil {
		t.Fatal(err)
	}
	tools, _, cleanup := agent.ToolAugment(context.Background(), "worker")
	defer cleanup()
	for _, tool := range tools {
		if tool.Name() != "Skill" {
			continue
		}
		result, err := tool.Call(context.Background(), json.RawMessage(`{"name":"nuclei"}`), nil)
		if err != nil || !result.IsError || !strings.Contains(result.Flatten(), "unknown skill") {
			t.Fatalf("disabled skill remained callable: %+v %v", result, err)
		}
	}
}
