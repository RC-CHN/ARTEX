package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/RC-CHN/ARTEX/db"
)

func TestResourceUnknownResultNeverClaimsNoExecution(t *testing.T) {
	result, err := resourceToolError(errors.New("database commit connection closed"))
	if err != nil || !result.IsError || result.Failure != nil {
		t.Fatal(result, err)
	}
	var body map[string]any
	if err = json.Unmarshal([]byte(result.Flatten()), &body); err != nil {
		t.Fatal(err)
	}
	if _, exists := body["executed"]; exists || body["execution_status"] != "unknown" || body["next_action"] == nil {
		t.Fatal(body)
	}
}

func TestTaskHandoffHTTPShapeAndReportScope(t *testing.T) {
	s, _, _ := libraryFixture(t)
	first, err := s.m.CreateTask("Synthetic report one", "Synthetic", nil, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	second, err := s.m.CreateTask("Synthetic report two", "Synthetic", nil, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	for i, task := range []string{first.ID, second.ID, second.ID} {
		id, _ := strconv.ParseInt(task, 10, 64)
		if _, err = s.m.Assets().UpsertRootDomain(db.UpsertRootDomainReq{Domain: fmt.Sprintf("lifecycle-%d-%s.example", i, task), TaskID: id}); err != nil {
			t.Fatal(err)
		}
	}
	req := httptest.NewRequest("GET", "/api/tasks/"+first.ID+"/handoff", nil)
	req.SetPathValue("id", first.ID)
	response := httptest.NewRecorder()
	s.taskHandoff(response, req)
	var body struct {
		Checks []db.HandoffCheck `json:"checks"`
	}
	if err = json.Unmarshal(response.Body.Bytes(), &body); err != nil || body.Checks == nil {
		t.Fatal("handoff contract", response.Body.String(), err)
	}
	response = httptest.NewRecorder()
	s.getReport(response, httptest.NewRequest("GET", "/api/report?task="+first.ID, nil))
	if response.Code != 200 || !strings.Contains(response.Body.String(), "root_domain 1") || strings.Contains(response.Body.String(), "root_domain 3") {
		t.Fatal(response.Body.String())
	}
}

func TestRecordExportOrphanCleanupKeepsLiveOwner(t *testing.T) {
	s, _, _ := libraryFixture(t)
	s.ctx = t.Context()
	task, err := s.m.CreateTask("Synthetic GC", "Synthetic", nil, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	id, _ := strconv.ParseInt(task.ID, 10, 64)
	t.Cleanup(func() {
		if _, err := s.m.pg.Exec(`DELETE FROM task_record_exports WHERE task_ids @> jsonb_build_array($1::bigint)`, id); err != nil {
			t.Error(err)
		}
	})
	if _, err = s.m.pg.QueueRecordExport(t.Context(), db.RecordExportRequest{TaskIDs: []int64{id}, RequestID: fmt.Sprint("gc-", id)}, 0); err != nil {
		t.Fatal(err)
	}
	job, err := s.m.pg.ClaimRecordExport(t.Context(), "LiveOwner")
	if err != nil {
		t.Fatal(err)
	}
	if err = os.MkdirAll(s.recordExportRoot(), 0700); err != nil {
		t.Fatal(err)
	}
	live := fmt.Sprintf(".%d-LiveOwner-stage", job.ID)
	old := fmt.Sprintf(".%d-OldOwner-stage", job.ID)
	for _, name := range []string{live, old, "unrelated.txt"} {
		path := filepath.Join(s.recordExportRoot(), name)
		if err = os.WriteFile(path, []byte("synthetic"), 0600); err != nil {
			t.Fatal(err)
		}
		then := time.Now().Add(-time.Hour)
		if err = os.Chtimes(path, then, then); err != nil {
			t.Fatal(err)
		}
	}
	s.gcOrphanRecordCopies()
	if _, err = os.Stat(filepath.Join(s.recordExportRoot(), old)); !os.IsNotExist(err) {
		t.Fatal("orphan retained", err)
	}
	for _, name := range []string{live, "unrelated.txt"} {
		if _, err = os.Stat(filepath.Join(s.recordExportRoot(), name)); err != nil {
			t.Fatal("protected file removed", name, err)
		}
	}
	if _, err = s.m.pg.ChangeRecordExport(t.Context(), job.ID, "cancel"); err != nil {
		t.Fatal(err)
	}
	if err = s.buildRecordExport(t.Context(), job); err == nil {
		t.Fatal("canceled owner built package")
	}
	if _, err = os.Stat(s.recordExportPath(job)); !os.IsNotExist(err) {
		t.Fatal("canceled package retained", err)
	}
}
