package db

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

func TestConversationActivityMetadataRoundTrip(t *testing.T) {
	d, err := Open(testDSN(t))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	conv, err := d.CreateConversation("auto", "synthetic metadata fixture", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Exec(`DELETE FROM conversations WHERE id=$1`, conv.ID)
	legacyID, err := d.AppendConvActivity(conv.ID, Activity{Worker: "auto", Kind: "thinking", Detail: "private synthetic detail"})
	if err != nil {
		t.Fatal(err)
	}
	metadata := json.RawMessage(`{"terminal":{"reason":"incomplete_response","summary_recovery_attempted":false}}`)
	terminalID, err := d.AppendConvActivity(conv.ID, Activity{Worker: "auto", Kind: "result", IsError: true, Metadata: metadata})
	if err != nil {
		t.Fatal(err)
	}
	check := func(rows []Activity, wantIDs []int64) {
		t.Helper()
		if len(rows) != len(wantIDs) {
			t.Fatalf("rows=%d want %d", len(rows), len(wantIDs))
		}
		for i, row := range rows {
			if row.ID != wantIDs[i] || row.Detail != "" {
				t.Fatalf("wrong row or detail leaked into list: id=%d", row.ID)
			}
			var got, want map[string]any
			if err := json.Unmarshal(row.Metadata, &got); err != nil {
				t.Fatal(err)
			}
			wantJSON := metadata
			if row.ID == legacyID {
				wantJSON = json.RawMessage(`{}`)
			}
			if err := json.Unmarshal(wantJSON, &want); err != nil || !reflect.DeepEqual(got, want) {
				t.Fatalf("metadata changed: got=%v want=%v err=%v", got, want, err)
			}
			encoded, err := json.Marshal(row)
			if err != nil || strings.Contains(string(encoded), "private synthetic detail") {
				t.Fatal("list serialization exposed detail", err)
			}
		}
	}
	rows, cursor, err := d.ConvActivityList(conv.ID, 0, 10)
	if err != nil || cursor != terminalID {
		t.Fatalf("list cursor=%d err=%v", cursor, err)
	}
	check(rows, []int64{legacyID, terminalID})
	rows, more, err := d.ConvActivityPage(conv.ID, 0, 1)
	if err != nil || !more {
		t.Fatalf("latest page more=%v err=%v", more, err)
	}
	check(rows, []int64{terminalID})
	rows, more, err = d.ConvActivityPage(conv.ID, terminalID, 1)
	if err != nil || more {
		t.Fatalf("earlier page more=%v err=%v", more, err)
	}
	check(rows, []int64{legacyID})
}

func TestConversationMetadataMigrationPreservesLegacyRows(t *testing.T) {
	d, err := Open(testDSN(t))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	tx, err := d.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	// Shadow the real table so this migration test never locks or changes other
	// test data. Execute the production ALTER statement, not a test duplicate.
	if _, err := tx.Exec(`CREATE TEMP TABLE conversation_activities(id BIGINT PRIMARY KEY, summary TEXT) ON COMMIT DROP;
INSERT INTO conversation_activities VALUES (1, 'legacy synthetic summary')`); err != nil {
		t.Fatal(err)
	}
	var migration string
	for _, line := range strings.Split(schemaSQL, "\n") {
		if strings.HasPrefix(line, "ALTER TABLE conversation_activities ADD COLUMN IF NOT EXISTS metadata ") {
			migration = line
		}
	}
	if migration == "" {
		t.Fatal("metadata migration missing")
	}
	for range 2 {
		if _, err := tx.Exec(migration); err != nil {
			t.Fatal(err)
		}
	}
	var summary, metadata string
	if err := tx.QueryRow(`SELECT summary,metadata::text FROM conversation_activities WHERE id=1`).Scan(&summary, &metadata); err != nil {
		t.Fatal(err)
	}
	if summary != "legacy synthetic summary" || metadata != "{}" {
		t.Fatalf("legacy row changed: summary=%q metadata=%q", summary, metadata)
	}
}
