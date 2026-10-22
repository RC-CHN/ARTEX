package server

import (
	"archive/zip"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/RC-CHN/ARTEX/agent"
	"github.com/RC-CHN/ARTEX/db"
	"github.com/RC-CHN/ARTEX/norma/llm"
	"github.com/RC-CHN/ARTEX/norma/transcript"
	"github.com/RC-CHN/ARTEX/recordexport"
)

func TestRecordExportToolsPackageAndBoundaries(t *testing.T) {
	s, r, v := libraryFixture(t)
	if err := s.m.pg.EnsureLLMRecordsTable(); err != nil {
		t.Fatal(err)
	}
	if err := s.m.pg.EnsureLLMUsageTable(); err != nil {
		t.Fatal(err)
	}
	task, err := s.m.CreateTaskWithOptions("synthetic export", "synthetic", db.TaskCreateOptions{Resources: []db.TaskResourceSelection{{ResourceID: r.ID, VersionID: v.ID}}})
	if err != nil {
		t.Fatal(err)
	}
	id, _ := strconv.ParseInt(task.ID, 10, 64)
	t.Cleanup(func() {
		if _, err := s.m.pg.Exec(`DELETE FROM task_record_exports WHERE task_ids @> jsonb_build_array($1::bigint)`, id); err != nil {
			t.Error(err)
		}
	})
	other, err := s.m.CreateTask("OTHER_TASK_SENTINEL", "private synthetic unrelated", nil, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(s.m.dir, "tasks", task.ID)
	if err = os.MkdirAll(root, 0755); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(root, "evidence.txt"), []byte("synthetic recorded result\npassword=synthetic-secret\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(root, "binary.bin"), []byte{0, 1, 255, 0}, 0600); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "outside.txt")
	if err = os.WriteFile(outside, []byte("OUTSIDE_SENTINEL"), 0600); err != nil {
		t.Fatal(err)
	}
	if err = os.Symlink(outside, filepath.Join(root, "escape")); err != nil {
		t.Fatal(err)
	}
	future := filepath.Join(root, "after-cutoff.txt")
	if err = os.WriteFile(future, []byte("future file"), 0600); err != nil {
		t.Fatal(err)
	}
	if err = os.Chtimes(future, time.Now().Add(time.Hour), time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	tx := transcript.NewStore(filepath.Join(s.m.dir, "transcripts"))
	session := fmt.Sprintf("exp%d-main", task.Store.ID())
	now := time.Now().UTC().Add(-time.Second)
	msg := llm.UserText("synthetic dialogue with password=synthetic-secret")
	if err = tx.Append(tx.MainPath(session), transcript.Record{UUID: "synthetic-record", SessionID: session, Timestamp: now.Format(time.RFC3339Nano), Type: "message", Message: &msg}); err != nil {
		t.Fatal(err)
	}
	msg = llm.UserText("AFTER_CUTOFF_SENTINEL")
	if err = tx.Append(tx.MainPath(session), transcript.Record{UUID: "future-record", SessionID: session, Timestamp: now.Add(time.Hour).Format(time.RFC3339Nano), Type: "message", Message: &msg}); err != nil {
		t.Fatal(err)
	}
	if err = s.m.pg.InsertLLMRecord(&db.LLMRecord{TaskID: task.ID, Model: "synthetic", SessionID: session, Status: "ok", RawRequest: `{"password":"synthetic-secret"}`, RawResponse: "synthetic result"}); err != nil {
		t.Fatal(err)
	}
	tool := s.recordExportTools()[0]
	ctx := agent.WithRunInfo(t.Context(), agent.RunInfo{TaskID: id})
	var calls []map[string]any
	call := func(input any, wantError bool) map[string]any {
		t.Helper()
		raw, _ := json.Marshal(input)
		result, e := tool.Call(ctx, raw, nil)
		if e != nil || result.IsError != wantError {
			t.Fatalf("export tool %v %s", e, result.Flatten())
		}
		var body map[string]any
		if e = json.Unmarshal([]byte(result.Flatten()), &body); e != nil {
			t.Fatal(e)
		}
		calls = append(calls, map[string]any{"input": json.RawMessage(raw), "output": body})
		return body
	}
	created := call(map[string]any{"action": "create", "request_id": "synthetic-export"}, false)
	missing := call(map[string]any{"action": "status", "id": 9223372036854775807}, true)
	if missing["code"] != "export_not_found" {
		t.Fatal(missing)
	}
	replay := call(map[string]any{"action": "create", "request_id": "synthetic-export"}, false)
	exportID := int64(created["export"].(map[string]any)["id"].(float64))
	if replay["export"].(map[string]any)["id"] != float64(exportID) {
		t.Fatal("duplicate export")
	}
	call(map[string]any{"action": "create", "request_id": "synthetic-export", "redacted": true}, true)
	job, err := s.m.pg.ClaimRecordExport(t.Context(), "synthetic-owner")
	if err != nil || job.ID != exportID {
		t.Fatal(job, err)
	}
	if err = s.buildRecordExport(t.Context(), job); err != nil {
		t.Fatal(err)
	}
	ready := call(map[string]any{"action": "status", "id": job.ID}, false)
	if ready["export"].(map[string]any)["state"] != "ready" {
		t.Fatal(ready)
	}
	rawzip, err := os.ReadFile(s.recordExportPath(job))
	if err != nil {
		t.Fatal(err)
	}
	hash := sha256.Sum256(rawzip)
	if ready["export"].(map[string]any)["sha256"] != hex.EncodeToString(hash[:]) {
		t.Fatal("package digest mismatch")
	}
	verify := func(job db.RecordExport, redacted bool) {
		t.Helper()
		reader, err := zip.OpenReader(s.recordExportPath(job))
		if err != nil {
			t.Fatal(err)
		}
		defer reader.Close()
		seen := map[string][]byte{}
		for _, entry := range reader.File {
			f, e := entry.Open()
			if e != nil {
				t.Fatal(e)
			}
			data, e := io.ReadAll(f)
			f.Close()
			if e != nil {
				t.Fatal(e)
			}
			seen[entry.Name] = data
			if redacted && bytes.Contains(data, []byte("synthetic-secret")) {
				t.Fatalf("credential remained in filtered export: %s", entry.Name)
			}
			if bytes.Contains(data, []byte("OUTSIDE_SENTINEL")) || bytes.Contains(data, []byte("OTHER_TASK_SENTINEL")) || bytes.Contains(data, []byte("AFTER_CUTOFF_SENTINEL")) {
				t.Fatalf("scope leak in %s", entry.Name)
			}
		}
		var manifest recordexport.Manifest
		if err = json.Unmarshal(seen["manifest.json"], &manifest); err != nil {
			t.Fatal(err)
		}
		for _, entry := range manifest.Files {
			data, ok := seen[entry.Path]
			if !ok {
				t.Fatal("missing", entry.Path)
			}
			sum := sha256.Sum256(data)
			if entry.SHA256 != hex.EncodeToString(sum[:]) || entry.Size != int64(len(data)) {
				t.Fatal("manifest mismatch", entry.Path)
			}
		}
		if len(manifest.Omissions) < 2 {
			t.Fatal("missing omission list")
		}
		prefix := fmt.Sprintf("tasks/%d/", id)
		var handoff map[string]any
		if err = json.Unmarshal(seen[prefix+"handoff.json"], &handoff); err != nil || handoff["scope"] != "exported_database_snapshot" || handoff["external_cleanup_verified"] != false {
			t.Fatal("invalid handoff", handoff, err)
		}
		if len(seen[prefix+"transcripts/"+session+".html"]) == 0 || len(seen[prefix+"database/llm_records.jsonl"]) == 0 {
			t.Fatal("missing transcripts/model records")
		}
		if redacted {
			if _, ok := seen[prefix+"workspace/binary.bin"]; ok {
				t.Fatal("redacted binary included")
			}
			if bytes.Contains(seen[prefix+"transcripts/"+session+".jsonl"], []byte("synthetic-secret")) {
				t.Fatal("redaction missing")
			}
		}
	}
	verify(job, false)
	mux := http.NewServeMux()
	s.registerLibraryRoutes(mux)
	request := httptest.NewRequest("GET", fmt.Sprintf("/api/task-record-exports/%d/download", job.ID), nil)
	request.Header.Set("Range", "bytes=0-9")
	response := httptest.NewRecorder()
	mux.ServeHTTP(response, request)
	if response.Code != 206 || response.Body.Len() != 10 {
		t.Fatalf("range download %d %d", response.Code, response.Body.Len())
	}
	redacted, err := s.m.pg.QueueRecordExport(t.Context(), db.RecordExportRequest{TaskIDs: []int64{id}, Redacted: true, RequestID: fmt.Sprint("redacted-", id)}, 0)
	if err != nil {
		t.Fatal(err)
	}
	redacted, err = s.m.pg.ClaimRecordExport(t.Context(), "redacted-owner")
	if err != nil {
		t.Fatal(err)
	}
	if err = s.buildRecordExport(t.Context(), redacted); err != nil {
		t.Fatal(err)
	}
	verify(redacted, true)
	canceled := call(map[string]any{"action": "create", "request_id": "cancel-me"}, false)
	cid := canceled["export"].(map[string]any)["id"]
	call(map[string]any{"action": "cancel", "id": cid}, false)
	call(map[string]any{"action": "retry", "id": cid}, false)
	call(map[string]any{"action": "list"}, false)
	otherID, _ := strconv.ParseInt(other.ID, 10, 64)
	t.Cleanup(func() {
		if _, err := s.m.pg.Exec(`DELETE FROM task_record_exports WHERE task_ids @> jsonb_build_array($1::bigint)`, otherID); err != nil {
			t.Error(err)
		}
	})
	foreign, err := s.m.pg.QueueRecordExport(t.Context(), db.RecordExportRequest{TaskIDs: []int64{otherID}, RequestID: fmt.Sprint("foreign-export-", otherID)}, 0)
	if err != nil {
		t.Fatal(err)
	}
	call(map[string]any{"action": "status", "id": foreign.ID}, true)
	if data, err := os.ReadFile(filepath.Join(root, "evidence.txt")); err != nil || !strings.Contains(string(data), "synthetic-secret") {
		t.Fatal("original changed")
	}
	if dir := os.Getenv("ARTEX_DOGFOOD_OUTPUT"); dir != "" {
		if err = os.MkdirAll(dir, 0700); err != nil {
			t.Fatal(err)
		}
		data, _ := json.MarshalIndent(calls, "", "  ")
		if err = os.WriteFile(filepath.Join(dir, "record-export-tools.json"), data, 0600); err != nil {
			t.Fatal(err)
		}
		if err = os.WriteFile(filepath.Join(dir, "synthetic-records.zip"), rawzip, 0600); err != nil {
			t.Fatal(err)
		}
	}
}

func TestRecordExportCustomScratch(t *testing.T) {
	scratch := t.TempDir()
	t.Setenv("ARTEX_RECORD_EXPORT_SCRATCH", scratch)
	TestRecordExportToolsPackageAndBoundaries(t)
	entries, err := os.ReadDir(filepath.Join(scratch, "artex-record-export"))
	if err != nil || len(entries) != 0 {
		t.Fatalf("scratch copies not reclaimed: entries=%d err=%v", len(entries), err)
	}
}
