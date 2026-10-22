package db

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"testing"
	"time"
)

func TestUsageStreamDiagnosticsAllowlist(t *testing.T) {
	if usageStreamEnd(nil) != nil || usageParseErrors(nil) != nil {
		t.Fatal("unknown observations must remain NULL")
	}
	for _, end := range []string{"done", "eof", "read_error", "request_error", "consumer_stop"} {
		if got := usageStreamEnd(&end); got != end {
			t.Fatalf("stream end %q changed to %v", end, got)
		}
	}
	private := "private provider body or error"
	if usageStreamEnd(&private) != "other" {
		t.Fatal("untrusted stream end was not sanitized")
	}
	negative, tooLarge := -1, 2147483647
	if usageParseErrors(&negative) != 0 || usageParseErrors(&tooLarge) != 2147483647 {
		t.Fatal("parse counters were not bounded")
	}
}

func TestLLMUsageStreamDiagnosticsNullableArchiveRestore(t *testing.T) {
	d, err := Open(testDSN(t))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	if err := d.EnsureLLMUsageTable(); err != nil {
		t.Fatal(err)
	}
	tx, err := d.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	for _, hasDiagnostics := range []bool{false, true} {
		var id int64
		if err := tx.QueryRow(`SELECT nextval(pg_get_serial_sequence('llm_usage','id'))`).Scan(&id); err != nil {
			t.Fatal(err)
		}
		row := map[string]any{"id": id, "ts": time.Now().UTC(), "model": "synthetic", "profile_name": fmt.Sprintf("archive-diagnostics-%d", id), "input_tokens": 1, "output_tokens": 1, "cache_read": 0, "cache_write": 0, "status": "ok"}
		if hasDiagnostics {
			row["finish_reason_seen"], row["stream_end"], row["parse_errors"] = false, "done", 2
		}
		raw, err := json.Marshal([]map[string]any{row})
		if err != nil {
			t.Fatal(err)
		}
		// This is the production archive restoration path. Old JSON rows omit
		// new columns; absence must not become false/zero or violate constraints.
		if err := insertArchiveRows(tx, "llm_usage", raw); err != nil {
			t.Fatal(err)
		}
		var finish sql.NullBool
		var end sql.NullString
		var parseErrors sql.NullInt64
		if err := tx.QueryRow(`SELECT finish_reason_seen,stream_end,parse_errors FROM llm_usage WHERE id=$1`, id).Scan(&finish, &end, &parseErrors); err != nil {
			t.Fatal(err)
		}
		if hasDiagnostics {
			if !finish.Valid || finish.Bool || !end.Valid || end.String != "done" || !parseErrors.Valid || parseErrors.Int64 != 2 {
				t.Fatalf("new archive diagnostics changed: %+v %+v %+v", finish, end, parseErrors)
			}
		} else if finish.Valid || end.Valid || parseErrors.Valid {
			t.Fatalf("old archive acquired invented observations: %+v %+v %+v", finish, end, parseErrors)
		}
	}
}

func TestLLMUsageStreamDiagnosticsSanitizedAtWrite(t *testing.T) {
	d, err := Open(testDSN(t))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	if err := d.EnsureLLMUsageTable(); err != nil {
		t.Fatal(err)
	}
	profile := fmt.Sprintf("metadata-sanitized-%d", time.Now().UnixNano())
	defer d.Exec(`DELETE FROM llm_usage WHERE profile_name=$1`, profile)
	finish, end, parseErrors := true, "private fixture provider error", -42
	if err := d.InsertLLMUsage(&LLMUsage{ProfileName: profile, FinishReasonSeen: &finish, StreamEnd: &end, ParseErrors: &parseErrors}); err != nil {
		t.Fatal(err)
	}
	var storedEnd string
	var storedErrors int
	var storedFinish bool
	if err := d.QueryRow(`SELECT finish_reason_seen,stream_end,parse_errors FROM llm_usage WHERE profile_name=$1`, profile).Scan(&storedFinish, &storedEnd, &storedErrors); err != nil {
		t.Fatal(err)
	}
	if !storedFinish || storedEnd != "other" || storedErrors != 0 {
		t.Fatalf("metadata was not sanitized: finish=%v end=%q errors=%d", storedFinish, storedEnd, storedErrors)
	}
}
