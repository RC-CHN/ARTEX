package server

import (
	"encoding/json"
	"testing"

	"github.com/RC-CHN/ARTEX/db"
)

func TestRetiredDeploymentCatalogUpgrade(t *testing.T) {
	s, _, _ := libraryFixture(t)
	pg := s.m.pg
	keys := []string{"manage_deployments"}
	for key := range legacyResourceDescriptions {
		keys = append(keys, key)
	}
	rows, err := pg.Query(`SELECT to_jsonb(tools) FROM tools WHERE key=ANY($1)`, keys)
	if err != nil {
		t.Fatal(err)
	}
	var originals []json.RawMessage
	for rows.Next() {
		var raw json.RawMessage
		if err := rows.Scan(&raw); err != nil {
			rows.Close()
			t.Fatal(err)
		}
		originals = append(originals, raw)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if _, err := pg.Exec(`DELETE FROM tools WHERE key=ANY($1)`, keys); err != nil {
			t.Error(err)
			return
		}
		for _, raw := range originals {
			if _, err := pg.Exec(`INSERT INTO tools SELECT * FROM jsonb_populate_record(NULL::tools,$1::jsonb)`, raw); err != nil {
				t.Error(err)
			}
		}
	})
	if _, err := pg.Exec(`DELETE FROM tools WHERE key=ANY($1)`, keys); err != nil {
		t.Fatal(err)
	}
	if err := pg.SeedTool("manage_deployments", "retired", nil, nil); err != nil {
		t.Fatal(err)
	}
	for key, old := range legacyResourceDescriptions {
		if err := pg.UpsertToolForce(key, old, json.RawMessage(`{"description":"operator schema"}`), json.RawMessage(`["auto"]`)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := pg.Exec(`UPDATE tools SET enabled=false WHERE key='delete_resource'`); err != nil {
		t.Fatal(err)
	}
	if _, err := pg.Exec(`UPDATE tools SET description=description || ' operator edit' WHERE key='manage_resource'`); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := s.retireDeploymentCatalog(); err != nil {
			t.Fatal(err)
		}
	}
	if old, err := pg.GetTool("manage_deployments"); err != nil || old != nil {
		t.Fatal("retired system tool remains in catalog", err)
	}
	for _, tool := range s.resourceTools() {
		old, ok := legacyResourceDescriptions[tool.Name()]
		if !ok {
			continue
		}
		got, err := pg.GetTool(tool.Name())
		want := tool.Description()
		if tool.Name() == "manage_resource" {
			want = old + " operator edit"
		}
		if err != nil || got == nil || got.Description != want {
			t.Fatalf("wrong description for %s: %+v %v", tool.Name(), got, err)
		}
		var schema map[string]any
		if err := json.Unmarshal(got.Schema, &schema); err != nil {
			t.Fatal(err)
		}
		if schema["description"] != "operator schema" || len(got.Agents) != 1 || got.Agents[0] != "auto" || (tool.Name() == "delete_resource" && got.Enabled) {
			t.Fatal("catalog cleanup overwrote operator configuration", got)
		}
	}
	// A user-created tool with the same name is not the retired system feature.
	if err := pg.CreateCustomTool(&db.Tool{Key: "manage_deployments", Description: "operator tool", Kind: "command", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	if err := s.retireDeploymentCatalog(); err != nil {
		t.Fatal(err)
	}
	if got, err := pg.GetTool("manage_deployments"); err != nil || got == nil || got.System {
		t.Fatal("removed an operator-owned custom tool", err)
	}
}
