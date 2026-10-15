package server

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"iter"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/RC-CHN/ARTEX/agent"
	"github.com/RC-CHN/ARTEX/db"
	"github.com/RC-CHN/ARTEX/norma/llm"
	"github.com/RC-CHN/ARTEX/norma/transcript"
)

type mainRequestTestProvider struct {
	entered   chan struct{}
	release   chan struct{}
	requestID int64
	calls     atomic.Int32
	noReply   bool
}

func TestMainRequestLateSSEKeepsNoticeAndMonotonicCursor(t *testing.T) {
	s, task, p, _ := newMainRequestFixture(t)
	newer := s.engine.emitActivity(task, db.Activity{Worker: "worker", Kind: "text", Summary: "worker already continued"})
	hs := httptest.NewServer(http.HandlerFunc(s.streamActivity))
	defer hs.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", hs.URL+"?task="+task.ID+"&since="+strconv.FormatInt(newer.ID, 10), nil)
	resp, err := hs.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	s.publishMainRequest(task, p.requestID)
	scan := bufio.NewScanner(resp.Body)
	eventID := int64(0)
	seen := false
	for scan.Scan() {
		line := scan.Text()
		if strings.HasPrefix(line, "id: ") {
			eventID, _ = strconv.ParseInt(strings.TrimPrefix(line, "id: "), 10, 64)
		}
		if strings.HasPrefix(line, "data: ") {
			var activity ActivityDTO
			if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &activity); err != nil {
				t.Fatal(err)
			}
			if activity.Kind != "consultation" {
				t.Fatalf("wrong event: %+v", activity)
			}
			if eventID != newer.ID {
				t.Fatalf("cursor regressed: got %d want %d", eventID, newer.ID)
			}
			seen = true
			break
		}
	}
	if !seen {
		t.Fatalf("late consultation dropped: %v", scan.Err())
	}
}

func TestMainRequestCancellationAndHumanAnswerDuringMainTurn(t *testing.T) {
	for _, action := range []string{"pause", "stop", "shutdown", "intent_cancel", "human_answer"} {
		t.Run(action, func(t *testing.T) {
			s, task, p, id := newMainRequestFixture(t)
			done := make(chan struct{})
			if !s.startMainRequest(task, func() { close(done) }) {
				t.Fatal("not admitted")
			}
			select {
			case <-p.entered:
			case <-time.After(3 * time.Second):
				t.Fatal("did not start")
			}
			want := "pending"
			switch action {
			case "pause":
				if _, err := s.applyTaskControl(task, "pause"); err != nil {
					t.Fatal(err)
				}
			case "stop":
				s.cancelTaskChat(task.ID, agent.AbortChatStoppedByUser)
				want = "answered"
			case "shutdown":
				s.cancel(agent.AbortShutdown)
			case "intent_cancel":
				if err := task.Store.SetIntentState(id, "stopped"); err != nil {
					t.Fatal(err)
				}
				want = "cancelled"
			case "human_answer":
				req := httptest.NewRequest("POST", "/", bytes.NewBufferString(`{"answer":"human decision"}`))
				req.SetPathValue("id", task.ID)
				req.SetPathValue("request", strconv.FormatInt(p.requestID, 10))
				resp := httptest.NewRecorder()
				s.answerMainRequest(resp, req)
				if resp.Code != 200 {
					t.Fatalf("human answer: %d %s", resp.Code, resp.Body.String())
				}
				want = "answered"
			}
			select {
			case <-done:
			case <-time.After(4 * time.Second):
				t.Fatal("turn leaked after cancellation")
			}
			r, err := task.Store.MainRequest(p.requestID)
			if err != nil || r.State != want {
				t.Fatalf("settlement: %+v %v", r, err)
			}
			if action == "stop" && r.AnswerSource != "system" {
				t.Fatal("manual stop masqueraded as agent answer")
			}
			if action == "human_answer" && (r.AnswerSource != "human" || r.Answer != "human decision") {
				t.Fatal("main overwrote human reply")
			}
			if action == "pause" && s.startMainRequest(task, func() {}) {
				t.Fatal("paused task dispatched")
			}
			if s.engine.inflightCount(task.ID) != 0 || s.engine.ActiveLLMCalls(task.ID) != 0 {
				t.Fatal("admission/LLM count leaked")
			}
		})
	}
}

func TestMainRequestConcurrentAdmissionAndBoundedRetries(t *testing.T) {
	s, task, p, _ := newMainRequestFixture(t)
	var winners atomic.Int32
	var wg sync.WaitGroup
	done := make(chan struct{}, 20)
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if s.startMainRequest(task, func() { done <- struct{}{} }) {
				winners.Add(1)
			}
		}()
	}
	wg.Wait()
	if winners.Load() != 1 {
		t.Fatalf("multiple main admissions: %d", winners.Load())
	}
	s.cancelTaskChat(task.ID, agent.AbortChatStoppedByUser)
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("main did not stop")
	}
	// A different request on the same running worker uses the same serial lane.
	if err := task.Store.AckMainReply(p.requestID); err != nil {
		t.Fatal(err)
	}
	old, _ := task.Store.MainRequest(p.requestID)
	r, err := task.Store.CreateMainRequest(t.Context(), old.IntentID, "retry-call", "unanswered question", "")
	if err != nil {
		t.Fatal(err)
	}
	p.requestID = r.ID
	p.calls.Store(0)
	p.entered = make(chan struct{})
	p.noReply = true
	close(p.release)
	if _, err = s.m.pg.Exec(`UPDATE main_requests SET attempts=2 WHERE id=$1`, r.ID); err != nil {
		t.Fatal(err)
	}
	if !s.startMainRequest(task, func() { done <- struct{}{} }) {
		t.Fatal("third attempt not started")
	}
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("third attempt did not finish")
	}
	got, _ := task.Store.MainRequest(r.ID)
	if got.State != "answered" || got.AnswerSource != "system" || got.Attempts != 3 {
		t.Fatalf("unbounded retry: %+v", got)
	}
}

func TestMainRequestRecoversExhaustedAttemptWithoutModel(t *testing.T) {
	s, task, p, _ := newMainRequestFixture(t)
	if _, err := s.m.pg.Exec(`UPDATE main_requests SET state='processing',attempts=3,updated_at=now()-interval '12 minutes' WHERE id=$1`, p.requestID); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	if !s.startMainRequest(task, func() { close(done) }) {
		t.Fatal("recovery not admitted")
	}
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("exhausted recovery leaked")
	}
	r, err := task.Store.MainRequest(p.requestID)
	if err != nil || r.State != "answered" || r.AnswerSource != "system" || p.calls.Load() != 0 {
		t.Fatalf("unbounded model retry: %+v, calls=%d err=%v", r, p.calls.Load(), err)
	}
}

func (p *mainRequestTestProvider) Stream(context.Context, llm.CompletionRequest) iter.Seq2[llm.StreamEvent, error] {
	return func(y func(llm.StreamEvent, error) bool) { y(llm.StreamEvent{}, context.Canceled) }
}
func (p *mainRequestTestProvider) Complete(ctx context.Context, _ llm.CompletionRequest) (llm.Message, string, llm.Usage, error) {
	if p.calls.Add(1) == 1 {
		close(p.entered)
		select {
		case <-p.release:
		case <-ctx.Done():
			return llm.Message{}, "", llm.Usage{}, ctx.Err()
		}
		if p.noReply {
			return llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{llm.TextBlock("no answer")}}, "end_turn", llm.Usage{}, nil
		}
		input, _ := json.Marshal(map[string]any{"request_id": p.requestID, "answer": "confirmed synthetic answer"})
		return llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{{Type: llm.BlockToolUse, ID: "reply", Name: "reply_to_worker", Input: input}}}, "tool_use", llm.Usage{}, nil
	}
	return llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{llm.TextBlock("replied")}}, "end_turn", llm.Usage{}, nil
}

func newMainRequestFixture(t *testing.T) (*Server, *Task, *mainRequestTestProvider, int64) {
	t.Helper()
	requireTestPostgres(t)
	m, err := NewManager(t.TempDir(), "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { m.Close() })
	task, err := m.CreateTask("main request fixture", "synthetic", nil, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { m.DeleteTask(task.ID, DeleteTaskOptions{}) })
	id, err := task.Store.AddIntent(map[string]any{"summary": "keep working"}, 1, nil, "planner")
	if err != nil {
		t.Fatal(err)
	}
	_, _ = task.Store.ClaimIntent(id, "worker")
	r, err := task.Store.CreateMainRequest(t.Context(), id, "call", "question", "")
	if err != nil {
		t.Fatal(err)
	}
	p := &mainRequestTestProvider{entered: make(chan struct{}), release: make(chan struct{}), requestID: r.ID}
	ctx, cancel := context.WithCancelCause(context.Background())
	t.Cleanup(func() { cancel(context.Canceled) })
	s := &Server{m: m, ctx: ctx, cancel: cancel, engine: NewEngine(m), llmOn: true, llmProv: p, chatBusy: map[string]bool{}, chatCancel: map[string]context.CancelCauseFunc{}}
	ma := agent.NewMainAgent(p, "fixture", m.dir, transcript.NewStore(filepath.Join(m.dir, "transcripts")), 100000, 5)
	ma.SetNonStreaming(func() bool { return true })
	s.taskAgents = map[string]*taskAgentBundle{task.ID: {main: ma}}
	// Other server tests install process-wide augmentation; keep this fixture local.
	prepare, augment, resolve := agent.PrepareExecution, agent.ToolAugment, agent.ToolResolve
	agent.PrepareExecution, agent.ToolAugment, agent.ToolResolve = nil, nil, nil
	t.Cleanup(func() { agent.PrepareExecution, agent.ToolAugment, agent.ToolResolve = prepare, augment, resolve })
	t.Cleanup(s.Close)
	return s, task, p, id
}

func TestMainRequestDispatchSerializesHumanChatAndPreservesWorker(t *testing.T) {
	s, task, p, id := newMainRequestFixture(t)
	r, _ := task.Store.MainRequest(p.requestID)
	s.chatBusy[task.ID] = true
	if s.startMainRequest(task, func() {}) {
		t.Fatal("automatic turn entered occupied human chat")
	}
	still, _ := task.Store.MainRequest(r.ID)
	if still.State != "pending" {
		t.Fatal("busy chat consumed queued request")
	}
	delete(s.chatBusy, task.ID)
	done := make(chan struct{})
	if !s.startMainRequest(task, func() { close(done) }) {
		t.Fatal("request was not admitted")
	}
	select {
	case <-p.entered:
	case <-time.After(3 * time.Second):
		t.Fatal("automatic main did not start")
	}
	if s.startMainRequest(task, func() {}) {
		t.Fatal("concurrent main turn admitted")
	}
	req := httptest.NewRequest("POST", "/api/exploration/chat?task="+task.ID, bytes.NewBufferString(`{"message":"human turn"}`))
	resp := httptest.NewRecorder()
	s.chat(resp, req)
	if resp.Code != 409 {
		t.Fatalf("human overlapped automatic transcript: %d %s", resp.Code, resp.Body.String())
	}
	n, _ := task.Store.GetNode(id)
	if n.State != "running" {
		t.Fatal("main turn blocked worker")
	}
	close(p.release)
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("main turn did not finish")
	}
	got, _ := task.Store.MainRequest(r.ID)
	if got.State != "answered" {
		t.Fatalf("not replied: %+v", got)
	}
	s.chatMu.Lock()
	busy := s.chatBusy[task.ID]
	s.chatMu.Unlock()
	if busy {
		t.Fatal("main admission not released")
	}
	if s.engine.inflightCount(task.ID) != 0 || s.engine.ActiveLLMCalls(task.ID) != 0 {
		t.Fatal("automatic run leaked activity/admission")
	}
	// Direct human response cannot overwrite an already committed main answer.
	req = httptest.NewRequest("POST", "/", bytes.NewBufferString(`{"answer":"overwrite"}`))
	req.SetPathValue("id", task.ID)
	req.SetPathValue("request", strconv.FormatInt(r.ID, 10))
	resp = httptest.NewRecorder()
	s.answerMainRequest(resp, req)
	if resp.Code != 409 {
		t.Fatalf("overwrite accepted: %d", resp.Code)
	}
}
