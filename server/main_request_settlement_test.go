package server

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/RC-CHN/ARTEX/agent"
	"github.com/RC-CHN/ARTEX/db"
	"github.com/RC-CHN/ARTEX/norma/llm"
)

func TestMainRequestStreamingSettlement(t *testing.T) {
	for _, tc := range []struct {
		action, state string
		cause         *agent.AbortCause
	}{
		{"worker_done", "cancelled", agent.AbortMainRequestWorkerFinished},
		{"worker_stopped", "cancelled", agent.AbortMainRequestCancelled},
		{"task_done", "cancelled", agent.AbortMainRequestCancelled},
		{"human_answer", "answered", agent.AbortMainRequestHumanAnswered},
		{"human_delivered", "delivered", agent.AbortMainRequestHumanAnswered},
		{"delivered", "delivered", agent.AbortMainRequestDelivered},
	} {
		t.Run(tc.action, func(t *testing.T) {
			s, task, p, intentID := newMainRequestFixture(t)
			observed := make(chan error, 1)
			p.stream = func(ctx context.Context, yield func(llm.StreamEvent, error) bool) {
				for _, ev := range []llm.StreamEvent{
					{Type: llm.SEToolUseStart, ToolID: "partial-reply", ToolName: "reply_to_worker"},
					{Type: llm.SEToolInputJSON, Text: `{"request_id":1,"answer":"unfinished`},
				} {
					if !yield(ev, nil) {
						return
					}
				}
				close(p.entered)
				<-ctx.Done()
				observed <- context.Cause(ctx)
				yield(llm.StreamEvent{}, ctx.Err())
			}
			s.taskAgents[task.ID].main.SetNonStreaming(func() bool { return false })
			done := make(chan struct{})
			if !s.startMainRequest(task, func() { close(done) }) {
				t.Fatal("not admitted")
			}
			select {
			case <-p.entered:
			case <-time.After(4 * time.Second):
				t.Fatal("stream never started")
			}
			var err error
			switch tc.action {
			case "worker_done":
				err = task.Store.SetIntentState(intentID, "done")
			case "worker_stopped":
				err = task.Store.SetIntentState(intentID, "stopped")
			case "task_done":
				_, err = s.m.pg.Exec("UPDATE tasks SET status='done' WHERE id=$1", task.ID)
			default:
				source := "human"
				if tc.action == "delivered" {
					source = "mainagent"
				}
				err = task.Store.AnswerMainRequest(t.Context(), p.requestID, "synthetic answer", source)
				if err == nil && strings.Contains(tc.action, "delivered") {
					err = task.Store.AckMainReply(p.requestID)
				}
			}
			if err != nil {
				t.Fatal(err)
			}
			select {
			case <-done:
			case <-time.After(5 * time.Second):
				t.Fatal("obsolete turn did not settle")
			}
			if got := <-observed; !errors.Is(got, tc.cause) {
				t.Fatalf("cause=%v, want %v", got, tc.cause)
			}
			r, err := task.Store.MainRequest(p.requestID)
			if err != nil || r.State != tc.state || r.Attempts != 1 {
				t.Fatalf("settlement=%+v err=%v", r, err)
			}
			if tc.state == "cancelled" && r.Answer != "" {
				t.Fatal("cancelled partial answer was persisted")
			}
			if strings.HasPrefix(tc.action, "human") && r.AnswerSource != "human" {
				t.Fatal("human answer overwritten")
			}
			rows, _, err := task.Store.ActivityPage(db.ActivitySessionFilter{Worker: "mainagent"}, 0, 100)
			if err != nil {
				t.Fatal(err)
			}
			results := 0
			for _, a := range rows {
				if a.Kind == "tool_use" || a.Kind == "tool_result" {
					t.Fatalf("partial reply was recorded as an executed tool: %+v", a)
				}
				if a.Kind != "result" {
					continue
				}
				results++
				a.Detail, err = task.Store.ActivityDetail(a.ID)
				if err != nil {
					t.Fatal(err)
				}
				if a.IsError || !strings.Contains(a.Summary, tc.cause.Short) || !strings.Contains(a.Detail, tc.cause.Code) || !strings.Contains(a.Detail, "尚未提交执行") {
					t.Fatalf("misleading lifecycle result: %+v", a)
				}
			}
			if results != 1 {
				t.Fatalf("result count=%d", results)
			}
			if s.engine.inflightCount(task.ID) != 0 || s.engine.ActiveLLMCalls(task.ID) != 0 {
				t.Fatal("execution counters leaked")
			}
		})
	}
}

func TestMainRequestAutoAnswerWaitsForDelivery(t *testing.T) {
	for _, state := range []string{"pending", "processing", "answered"} {
		if cause := mainRequestStopCause(&db.MainRequest{State: state, AnswerSource: "mainagent"}); cause != nil {
			t.Fatalf("automatic reply stopped before delivery: state=%s cause=%v", state, cause)
		}
	}
}
