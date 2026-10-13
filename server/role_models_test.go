package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/RC-CHN/ARTEX/db"
	"github.com/RC-CHN/ARTEX/norma/llm"
)

func TestRoleModelsRouteRealRequestsIndependentlyAndCanUnbind(t *testing.T) {
	m := runnerTestManager(t)
	received := make(chan string, 4)
	endpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var input struct {
			Model string `json:"model"`
		}
		if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
			t.Error(err)
		}
		received <- input.Model
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"test","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`))
	}))
	defer endpoint.Close()
	ids := map[string]int64{}
	for _, model := range []string{"strong", "cheap", "task-fallback"} {
		id, err := m.pg.SaveProfile(&db.LLMProfile{Name: fmt.Sprintf("roles-%s-%d", model, time.Now().UnixNano()), Format: "openai", Model: model, BaseURL: endpoint.URL + "/v1", APIKey: "test-only"})
		if err != nil {
			t.Fatal(err)
		}
		ids[model] = id
		t.Cleanup(func() { _ = m.pg.DeleteProfile(id) })
	}
	task, err := m.CreateTaskWithOptions("role routing", "local model routing test", db.TaskCreateOptions{LLMProfileIDs: []int64{ids["task-fallback"]}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = m.DeleteTask(task.ID, DeleteTaskOptions{}) })
	s := &Server{m: m, provByProfile: map[int64]*provEntry{}}
	for _, role := range []string{"mainagent", "planner", "worker"} {
		agent, err := m.pg.GetAgentByKey(role)
		if err != nil || agent == nil {
			t.Fatal("missing agent", role, err)
		}
		old := agent.LLMProfileID
		t.Cleanup(func() { _ = m.pg.SetAgentLLMProfile(role, old) })
		model := "strong"
		if role == "worker" {
			model = "cheap"
		}
		id := ids[model]
		request := httptest.NewRequest("PUT", "/agents/"+role+"/config", strings.NewReader(fmt.Sprintf(`{"llm_profile_id":%d}`, id)))
		request.SetPathValue("key", role)
		response := httptest.NewRecorder()
		s.pgSaveAgentConfig(response, request)
		if response.Code != 200 {
			t.Fatalf("save %s: %d %s", role, response.Code, response.Body.String())
		}
		runtime := &taskLLMRuntime{s: s, taskID: task.ID, agentKey: role}
		selection, err := runtime.current()
		if err != nil {
			t.Fatal(err)
		}
		if selection.profileID != 0 {
			t.Fatal("binding incorrectly joined the task failover chain")
		}
		_, _, _, err = selection.provider.Complete(t.Context(), llm.CompletionRequest{Messages: []llm.Message{{Role: llm.RoleUser, Content: []llm.ContentBlock{{Type: llm.BlockText, Text: "hello"}}}}})
		if err != nil {
			t.Fatal(err)
		}
		if got := <-received; got != model {
			t.Fatalf("%s used %q, want %q", role, got, model)
		}
	}
	if err := m.pg.SetAgentLLMProfile("worker", nil); err != nil {
		t.Fatal(err)
	}
	resolved, err := s.resolveTaskRoleLLM(task, "worker")
	if err != nil || resolved.Model != "task-fallback" {
		t.Fatal("unbind did not restore task routing", resolved, err)
	}
	resolved, err = s.resolveTaskRoleLLM(task, "planner")
	if err != nil || resolved.Model != "strong" {
		t.Fatal("worker unbind changed planner", resolved, err)
	}
}
