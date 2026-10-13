package agent

import (
	"fmt"
	"testing"
	"time"

	"github.com/Autumn-27/artex/db"
)

func TestGraphOverviewCompanyScopeWithoutAssetsStaysLive(t *testing.T) {
	d := testDB(t)
	t.Cleanup(func() { _ = d.Close() })
	companies := d.Companies()
	companyID, _, err := companies.UpsertCompany(fmt.Sprintf("overview-empty-%d", time.Now().UnixNano()), "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = companies.DeleteCompany(companyID) })
	enabled := false
	task, err := d.CreateTaskWithOptions("empty company", "discover configured targets", db.TaskCreateOptions{
		CompanyIDs: []int64{companyID}, CoverageEnabled: &enabled,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.DeleteTask(task.ID) })
	tools := NewToolSet(d.Exploration(task.ExplorationID), "planner")
	tools.SetTaskID(task.ID)
	tools.SetAssetStore(d.Assets(), companies)
	tools.SetCoverageEnabled(false)

	// Company rules are live context, not a snapshot of materialized assets or
	// of the rules that existed when the task was created.
	for _, keyword := range []string{"Original Company Name", "Updated Company Name", ""} {
		var inputs []db.ScopeInput
		if keyword != "" {
			inputs = []db.ScopeInput{{Kind: "keyword", Value: keyword}}
		}
		added, invalid, validationErrors, err := companies.UpdateScopeInputsChecked(companyID, inputs, "scope update")
		if err != nil || invalid != 0 || len(validationErrors) != 0 || added != len(inputs) {
			t.Fatalf("replace company scope: added=%d invalid=%d validation=%v err=%v", added, invalid, validationErrors, err)
		}
		coverage, ok := tools.graphOverviewData()["coverage"].(map[string]any)
		if !ok || coverage["host_count"] != 0 {
			t.Fatalf("empty company asset context: %#v", coverage)
		}
		if _, exists := coverage["denominator"]; exists {
			t.Fatalf("disabled coverage exposed metrics: %#v", coverage)
		}
		scope, ok := coverage["scope"].([]map[string]any)
		if !ok || len(scope) != 1 || scope[0]["company_id"] != companyID {
			t.Fatalf("company scope missing without assets: %#v", coverage["scope"])
		}
		rules, ok := scope[0]["company_scope"].([]map[string]any)
		keywords, keywordsOK := scope[0]["company_keywords"].([]string)
		if !ok || !keywordsOK || len(rules) != len(inputs) || len(keywords) != len(inputs) {
			t.Fatalf("company scope is stale: %#v", scope[0])
		}
		if keyword != "" && (rules[0]["value"] != keyword || keywords[0] != keyword) {
			t.Fatalf("company keyword spelling lost: %#v", scope[0])
		}
	}
}

func TestGraphOverviewPreservesDirectTaskScope(t *testing.T) {
	d := testDB(t)
	t.Cleanup(func() { _ = d.Close() })
	task, err := d.CreateTask("direct scope", "preserve task boundary", nil, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.DeleteTask(task.ID) })
	assets := d.Assets()
	inputs := []struct{ kind, input, want string }{
		{"root_domain", "scope.example", "scope.example"},
		{"subdomain", "api.scope.example", "api.scope.example"},
		{"ip", "2001:db8::42", "2001:db8::42/128"},
		{"cidr", "2001:db8:1::/64", "2001:db8:1::/64"},
		{"icp", "京 ICP 备 123 号", "京icp备123号"},
		{"keyword", "Example Company", "example company"},
	}
	for _, input := range inputs {
		if _, err := assets.AddAgentScope(task.ID, input.kind, input.input, "explicit boundary", "manual"); err != nil {
			t.Fatal(err)
		}
	}
	tools := NewToolSet(d.Exploration(task.ExplorationID), "planner")
	tools.SetTaskID(task.ID)
	tools.SetAssetStore(assets, nil)
	for _, enabled := range []bool{true, false} {
		tools.SetCoverageEnabled(enabled)
		coverage, ok := tools.graphOverviewData()["coverage"].(map[string]any)
		if !ok {
			t.Fatalf("scope context missing (coverage=%t)", enabled)
		}
		scope, ok := coverage["scope"].([]map[string]any)
		if !ok || len(scope) != len(inputs) {
			t.Fatalf("scope missing (coverage=%t): %#v", enabled, coverage["scope"])
		}
		for i, input := range inputs {
			row := scope[i]
			if row["kind"] != input.kind || row["value"] != input.want || row["source"] != "manual" || row["reason"] != "explicit boundary" {
				t.Errorf("scope %d (coverage=%t): %#v", i, enabled, row)
			}
		}
	}
}
