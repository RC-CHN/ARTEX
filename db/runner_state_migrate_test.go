package db

import (
	"encoding/json"
	"testing"
)

func TestRunnerStateMigrationPreservesLegacyCheckpointAndBinaryStrings(t *testing.T) {
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
	if _, err := tx.Exec(`DELETE FROM runner_state;
		ALTER TABLE runner_state ALTER COLUMN payload TYPE JSONB USING payload::jsonb;
		INSERT INTO runner_state VALUES (1,7,'{"version":1,"epoch":"legacy","sessions":{}}')`); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if _, err := tx.Exec(schemaSQL); err != nil {
			t.Fatalf("upgrade existing checkpoint: %v", err)
		}
	}
	var kind, raw string
	var revision int
	if err := tx.QueryRow(`SELECT revision, payload, pg_typeof(payload)::text FROM runner_state WHERE id=1`).Scan(&revision, &raw, &kind); err != nil {
		t.Fatal(err)
	}
	var legacy struct{ Epoch string }
	if err := json.Unmarshal([]byte(raw), &legacy); err != nil || kind != "json" || revision != 7 || legacy.Epoch != "legacy" {
		t.Fatalf("migration lost state: type=%s revision=%d epoch=%s error=%v", kind, revision, legacy.Epoch, err)
	}
	const binary = `{"output":"before\u0000after","literal":"\\u0000","input":{"n":9007199254740993}}`
	if _, err := tx.Exec(`UPDATE runner_state SET payload=$1, revision=8 WHERE id=1`, binary); err != nil {
		t.Fatalf("NUL output rejected after migration: %v", err)
	}
	// A subsequent startup must neither reinterpret the escaped bytes nor
	// attempt to convert the checkpoint back to JSONB.
	if _, err := tx.Exec(schemaSQL); err != nil {
		t.Fatalf("restart with binary checkpoint: %v", err)
	}
	if err := tx.QueryRow(`SELECT payload FROM runner_state WHERE id=1`).Scan(&raw); err != nil || raw != binary {
		t.Fatalf("binary checkpoint changed on restart: exact=%t error=%v", raw == binary, err)
	}
}
