package server

import (
	"encoding/json"
	"github.com/RC-CHN/ARTEX/db"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestTaskTokenBudgetHTTPAndClaimGate(t *testing.T) {
	s, _, _ := libraryFixture(t)
	task, err := s.m.CreateTask("synthetic token budget", "synthetic", nil, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	call := func(body string, want int) db.TaskTokenBudget {
		t.Helper()
		method := "POST"
		if body == "" {
			method = "GET"
		}
		r := httptest.NewRequest(method, "/api/tasks/"+task.ID+"/token-budget", strings.NewReader(body))
		r.SetPathValue("id", task.ID)
		w := httptest.NewRecorder()
		s.taskTokenBudget(w, r)
		if w.Code != want {
			t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
		}
		var b db.TaskTokenBudget
		if want == 200 {
			if err = json.Unmarshal(w.Body.Bytes(), &b); err != nil {
				t.Fatal(err)
			}
		}
		return b
	}
	if b := call("", 200); b.Limit != 0 {
		t.Fatal(b)
	}
	call(`{"expected_revision":1}`, 400)
	call(`{"limit":1,"expected_revision":1}`, 200)
	call(`{"limit":0,"expected_revision":1}`, 409)
	if err = s.m.pg.InsertLLMUsage(&db.LLMUsage{TaskID: task.ID, InputTokens: 1, Status: "ok"}); err != nil {
		t.Fatal(err)
	}
	intent, err := task.Store.AddIntent(map[string]any{"summary": "synthetic"}, 1, nil, "test")
	if err != nil {
		t.Fatal(err)
	}
	e := &Engine{m: s.m}
	if !e.taskBudgetBlocks(t.Context(), task) || e.claimNext(t.Context(), task, "fixture") != nil {
		t.Fatal("threshold did not block new work")
	}
	n, err := task.Store.GetNode(intent)
	if err != nil || n.State != "open" {
		t.Fatal("claim changed state", n, err)
	}
	b := call(`{"limit":0,"expected_revision":2}`, 200)
	if b.Used != 1 || b.Reached {
		t.Fatal(b)
	}
	if e.taskBudgetBlocks(t.Context(), task) {
		t.Fatal("unlimited still blocked")
	}
	if n = e.claimNext(t.Context(), task, "fixture"); n == nil || n.ID != intent {
		t.Fatal("eligible work not claimed", n)
	}
}
