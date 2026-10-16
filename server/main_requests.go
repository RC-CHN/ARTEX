package server

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"time"

	"github.com/RC-CHN/ARTEX/agent"
	"github.com/RC-CHN/ARTEX/db"
)

// mainRequestLoop dispatches a durable inbox. Each task shares the human chat's
// admission mutex/transcript; the global bound prevents a burst of requests from
// opening an unbounded number of main-agent executions.
func (s *Server) mainRequestLoop() {
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	slots := make(chan struct{}, 4)
	var afterID int64
	for {
		if s.ctx.Err() != nil {
			return
		}
		ids, err := s.m.pg.MainRequestTaskIDs(s.ctx, afterID)
		if err != nil {
			log.Printf("[main requests] queue: %v", err)
		}
		if len(ids) == 0 {
			afterID = 0
		}
		for _, id := range ids {
			afterID = id
			select {
			case slots <- struct{}{}:
			default:
				continue
			}
			t, ok := s.m.Task(strconv.FormatInt(id, 10))
			if !ok || !s.startMainRequest(t, func() { <-slots }) {
				<-slots
			}
		}
		select {
		case <-s.ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (s *Server) startMainRequest(t *Task, done func()) bool {
	life := t.lifecycleSnapshot()
	if life.Paused || life.Queued || isTerminalStatus(life.Status) || s.engine.IsDeleting(t.ID) || !s.taskRuntimeAvailable(t, "mainagent") {
		return false
	}
	s.chatMu.Lock()
	if s.chatBusy[t.ID] || s.engine.IsDeleting(t.ID) || s.engine.IsPaused(t.ID) {
		s.chatMu.Unlock()
		return false
	}
	ctx, cancel := context.WithCancelCause(s.ctx)
	s.chatBusy[t.ID] = true
	s.chatCancel[t.ID] = cancel
	s.chatMu.Unlock()
	if !s.engine.beginTaskOperation(t.ID) {
		s.finishTaskChat(t.ID, cancel)
		return false
	}
	if !s.background(func() {
		defer done()
		defer s.engine.decInflight(t.ID)
		defer s.finishTaskChat(t.ID, cancel)
		r, err := t.Store.ClaimMainRequest(ctx)
		if err != nil {
			log.Printf("[main requests] claim: %v", err)
			return
		}
		if r == nil {
			return
		}
		s.runMainRequest(ctx, t, r)
	}) {
		s.engine.decInflight(t.ID)
		s.finishTaskChat(t.ID, cancel)
		return false
	}
	return true
}

func (s *Server) publishMainRequest(t *Task, id int64) {
	if rows, err := t.Store.MainRequestActivities(id); err == nil {
		for _, a := range rows {
			s.engine.bc.Publish(t.ID, a)
		}
	}
}

func (s *Server) runMainRequest(parent context.Context, t *Task, r *db.MainRequest) {
	if r.Attempts > 3 {
		// A restart or failed settlement can leave the third attempt processing.
		// Recover its receipt without spending another model turn.
		s.failMainRequest(t, r, "处理轮次已用尽，恢复失败回执")
		return
	}
	deadlineCtx, stopDeadline := context.WithTimeoutCause(parent, 10*time.Minute, agent.AbortMainRequestTimeout)
	defer stopDeadline()
	ctx, cancel := context.WithCancelCause(deadlineCtx)
	defer cancel(agent.AbortChatTurnFinished)
	s.publishMainRequest(t, r.ID)
	emit := func(a db.Activity) { a.MainSeg = &r.MainSeg; s.engine.emitActivity(t, a) }
	emit(db.Activity{Worker: "mainagent", Kind: "round", Summary: fmt.Sprintf("自动处理 Worker #%d 的请示 #%d（第 %d 次）", r.IntentID, r.ID, r.Attempts)})
	// Stop obsolete turns promptly. A model may still return after cancellation;
	// the database state boundary remains authoritative for its reply tool.
	watchDone := make(chan struct{})
	go func() {
		defer close(watchDone)
		tick := time.NewTicker(2 * time.Second)
		defer tick.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-tick.C:
				current, err := t.Store.MainRequestContext(ctx, r.ID)
				if err == nil {
					if cause := mainRequestStopCause(current); cause != nil {
						cancel(cause)
						return
					}
				}
			}
		}
	}()
	ma := s.agentsForTask(t).main
	id, _ := strconv.ParseInt(t.ID, 10, 64)
	s.engine.BeginLLMCall(t.ID)
	_, err := ma.Consult(ctx, id, s.m.Assets(), t.Store, t.Goal, r, emit, t.Notify)
	s.engine.EndLLMCall(t.ID)
	cancel(agent.AbortChatTurnFinished)
	<-watchDone
	s.publishMainRequest(t, r.ID)
	cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cleanupCancel()
	current, readErr := t.Store.MainRequestContext(cleanupCtx, r.ID)
	if readErr != nil || current.State != "processing" {
		return
	}
	detail := "主 Agent 本轮未调用 reply_to_worker"
	if err != nil {
		detail = err.Error()
	}
	// Shutdown/pause preserves the queued request. Explicit stop suppresses
	// automatic retries by delivering a neutral failure receipt to the worker.
	if parent.Err() != nil && context.Cause(parent) != agent.AbortChatStoppedByUser {
		_ = t.Store.RetryMainRequest(cleanupCtx, r.ID, r.Attempts, "运行中断，稍后重试")
		return
	}
	if r.Attempts >= 3 || context.Cause(parent) == agent.AbortChatStoppedByUser {
		s.failMainRequest(t, r, detail)
		return
	}
	if err := t.Store.RetryMainRequest(cleanupCtx, r.ID, r.Attempts, detail); err != nil {
		log.Printf("[main requests] retry: %v", err)
	}
}

func mainRequestStopCause(r *db.MainRequest) error {
	// Human answers retain their origin after delivery; do not label them as an
	// automatic answer merely because the worker acknowledged before the poll.
	if (r.State == "answered" || r.State == "delivered") && r.AnswerSource == "human" {
		return agent.AbortMainRequestHumanAnswered
	}
	switch r.State {
	case "cancelled":
		if r.LastError == "intent done" {
			return agent.AbortMainRequestWorkerFinished
		}
		return agent.AbortMainRequestCancelled
	case "delivered":
		return agent.AbortMainRequestDelivered
	}
	return nil
}

func (s *Server) failMainRequest(t *Task, r *db.MainRequest, detail string) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	answer := "主 Agent 未能完成这条请示。没有新的决定或授权；请依据现有目标和约束继续可独立的工作，必要时向人类说明阻碍。"
	if err := t.Store.AnswerMainRequestAttempt(ctx, r.ID, answer, "system", r.Attempts); err != nil {
		log.Printf("[main requests] failure receipt: %v", err)
		return
	}
	s.publishMainRequest(t, r.ID)
	s.engine.emitActivity(t, db.Activity{Worker: "mainagent", MainSeg: &r.MainSeg, Kind: "text", IsError: true, Summary: fmt.Sprintf("请示 #%d 自动处理未完成：%s", r.ID, detail)})
}

func (s *Server) mainRequests(w http.ResponseWriter, r *http.Request) {
	t := s.m.ResolveTask(r.PathValue("id"))
	if t == nil {
		writeErr(w, 404, "task not found")
		return
	}
	items, err := t.Store.MainRequestsContext(r.Context(), 0, false)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, items)
}

func (s *Server) answerMainRequest(w http.ResponseWriter, r *http.Request) {
	t := s.m.ResolveTask(r.PathValue("id"))
	if t == nil {
		writeErr(w, 404, "task not found")
		return
	}
	id, err := strconv.ParseInt(r.PathValue("request"), 10, 64)
	if err != nil {
		writeErr(w, 400, "invalid request id")
		return
	}
	var req struct {
		Answer string `json:"answer"`
	}
	if err = json.NewDecoder(http.MaxBytesReader(w, r.Body, 40000)).Decode(&req); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	if !s.engine.beginTaskOperation(t.ID) {
		writeErr(w, 409, "task is being deleted")
		return
	}
	defer s.engine.decInflight(t.ID)
	if err = t.Store.AnswerMainRequest(r.Context(), id, req.Answer, "human"); err != nil {
		writeErr(w, 409, err.Error())
		return
	}
	s.publishMainRequest(t, id)
	writeJSON(w, 200, map[string]any{"request_id": id, "status": "answered"})
}
