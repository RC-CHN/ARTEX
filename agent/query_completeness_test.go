package agent

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

// Exercise the same tools an agent sees, with history beyond the previous caps.
func TestAgentQueryPaginationAndOldRunningIntent(t *testing.T) {
	d := testDB(t)
	defer d.Close()
	exp, err := d.CreateExploration("query fixture", "synthetic")
	if err != nil {
		t.Fatal(err)
	}
	defer d.Exec(`DELETE FROM explorations WHERE id=$1`, exp)
	s := d.Exploration(exp)
	running, err := s.AddIntent(map[string]any{"summary": "old running fixture"}, 1, nil, "fixture")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = d.Exec(`UPDATE exploration_nodes SET state='running' WHERE id=$1`, running); err != nil {
		t.Fatal(err)
	}
	for _, kind := range []string{"intent", "finding", "fact"} {
		if _, err = d.Exec(`INSERT INTO exploration_nodes(exploration_id,kind,payload,state) SELECT $1,$2,jsonb_build_object('summary','synthetic '||i),CASE WHEN $2='intent' THEN 'done' ELSE 'confirmed' END FROM generate_series(1,1005)i`, exp, kind); err != nil {
			t.Fatal(err)
		}
	}
	tools := NewToolSet(s, "fixture")
	view := tools.graphOverviewData()
	run := view["running_intents"].([]map[string]any)
	if len(run) != 1 || run[0]["id"] != running || view["facts"] != 1005 || view["findings_total"] != 1005 {
		t.Fatalf("incomplete overview: running=%v facts=%v findings=%v", run, view["facts"], view["findings_total"])
	}
	legacy, err := tools.listFindings().Call(t.Context(), json.RawMessage(`{}`), nil)
	var old []any
	if err != nil || legacy.IsError || json.Unmarshal([]byte(legacy.Flatten()), &old) != nil || len(old) != 500 || len(legacy.Extra) != 1 {
		t.Fatal("legacy array or truncation notice lost", err)
	}
	seen := map[int64]bool{}
	before := int64(0)
	for {
		page := callReadJSON(t, tools.listFindings(), fmt.Sprintf(`{"paged":true,"limit":100,"before":%d}`, before)).(map[string]any)
		if page["total"] != float64(1005) {
			t.Fatal(page)
		}
		for _, v := range page["findings"].([]any) {
			id := int64(v.(map[string]any)["id"].(float64))
			if seen[id] {
				t.Fatal("duplicate page", id)
			}
			seen[id] = true
		}
		if page["has_more"] == false {
			break
		}
		next := int64(page["next_before"].(float64))
		if next <= 0 || (before > 0 && next >= before) {
			t.Fatal("invalid cursor", page)
		}
		before = next
	}
	if len(seen) != 1005 {
		t.Fatal("pagination lost records", len(seen))
	}
	page := callReadJSON(t, tools.listFindings(), `{"paged":true,"q":"synthetic 1005"}`).(map[string]any)
	if page["total"] != float64(1) {
		t.Fatal(page)
	}
	// Close only this test's connection pool; a failure must never become [].
	if _, err := d.Exec(`DELETE FROM explorations WHERE id=$1`, exp); err != nil {
		t.Fatal(err)
	}
	d.Close()
	failed, err := tools.listFindings().Call(t.Context(), json.RawMessage(`{}`), nil)
	if err != nil || !failed.IsError || !strings.Contains(failed.Flatten(), "query_failed") {
		t.Fatal("DB failure presented as empty result", failed, err)
	}
	view = tools.graphOverviewData()
	if view["incomplete"] != true {
		t.Fatal("overview hid database failure")
	}
}
