package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"iter"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/RC-CHN/ARTEX/db"
	"github.com/RC-CHN/ARTEX/norma/harness"
	"github.com/RC-CHN/ARTEX/norma/llm"
	actool "github.com/RC-CHN/ARTEX/norma/tool"
	"github.com/RC-CHN/ARTEX/norma/transcript"
)

type consultationProvider struct {
	complete func(context.Context, llm.CompletionRequest) (llm.Message, string, llm.Usage, error)
}

func (p consultationProvider) Complete(ctx context.Context, r llm.CompletionRequest) (llm.Message, string, llm.Usage, error) {
	return p.complete(ctx, r)
}
func (p consultationProvider) Stream(ctx context.Context, r llm.CompletionRequest) iter.Seq2[llm.StreamEvent, error] {
	return func(y func(llm.StreamEvent, error) bool) {
		m, stop, u, err := p.complete(ctx, r)
		if err != nil {
			y(llm.StreamEvent{}, err)
			return
		}
		for _, b := range m.Content {
			if b.Type == llm.BlockToolUse {
				if !y(llm.StreamEvent{Type: llm.SEToolUseStart, ToolID: b.ID, ToolName: b.Name}, nil) {
					return
				}
				if !y(llm.StreamEvent{Type: llm.SEToolInputJSON, Text: string(b.Input)}, nil) {
					return
				}
			} else if !y(llm.StreamEvent{Type: llm.SETextDelta, Text: b.Text}, nil) {
				return
			}
		}
		if !y(llm.StreamEvent{Type: llm.SEMessageDelta, StopReason: stop, Usage: u}, nil) {
			return
		}
		y(llm.StreamEvent{Type: llm.SEMessageStop}, nil)
	}
}
func consultationCall(name, id string, args any) (llm.Message, string, llm.Usage, error) {
	b, _ := json.Marshal(args)
	return llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{{Type: llm.BlockToolUse, Name: name, ID: id, Input: b}}}, "tool_use", llm.Usage{}, nil
}
func consultationText(text string) (llm.Message, string, llm.Usage, error) {
	return llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{llm.TextBlock(text)}}, "end_turn", llm.Usage{}, nil
}

func TestMainRequestWorkerContinuesAndReceivesSteering(t *testing.T) {
	for _, stream := range []bool{false, true} {
		for _, atStop := range []bool{false, true} {
			t.Run(fmt.Sprintf("stream=%v/stop=%v", stream, atStop), func(t *testing.T) {
				d := testDB(t)
				defer d.Close()
				exp, err := d.CreateExploration("async fixture", "synthetic test")
				if err != nil {
					t.Fatal(err)
				}
				defer d.Exec(`DELETE FROM explorations WHERE id=$1`, exp)
				s := d.Exploration(exp)
				id, _ := s.AddIntent(map[string]any{"summary": "continue independent work"}, 1, nil, "planner")
				_, _ = s.ClaimIntent(id, "worker")
				intent, _ := s.GetNode(id)
				dir := t.TempDir()
				tx := transcript.NewStore(filepath.Join(dir, "transcripts"))
				var workDone atomic.Int32
				var blockedAction atomic.Int32
				var request *db.MainRequest
				mainCalls := 0
				mp := consultationProvider{complete: func(ctx context.Context, req llm.CompletionRequest) (llm.Message, string, llm.Usage, error) {
					mainCalls++
					if !strings.Contains(strings.Join(req.System, "\n"), "不是人类消息") {
						return llm.Message{}, "", llm.Usage{}, fmt.Errorf("automatic origin missing")
					}
					for _, tool := range req.Tools {
						if tool.Name == "set_goals" || tool.Name == "set_constraints" {
							return llm.Message{}, "", llm.Usage{}, fmt.Errorf("automatic turn can expand authorization")
						}
					}
					switch mainCalls {
					case 1:
						return consultationCall("graph_overview", "main-read", map[string]any{})
					case 2:
						return consultationCall("add_hint", "main-hint", map[string]any{"hints": []map[string]any{{"text": "confirmed coordination"}}})
					case 3:
						return consultationCall("reply_to_worker", "main-reply", map[string]any{"request_id": request.ID, "answer": "use confirmed evidence; stay in current scope"})
					default:
						return consultationText("已查证并回复 Worker。")
					}
				}}
				ma := NewMainAgent(mp, "fixture", dir, tx, 100000, 8)
				ma.SetNonStreaming(func() bool { return !stream })
				workerCalls := 0
				wp := consultationProvider{complete: func(ctx context.Context, req llm.CompletionRequest) (llm.Message, string, llm.Usage, error) {
					workerCalls++
					switch workerCalls {
					case 1:
						return consultationCall("ask_main_agent", "ask-1", map[string]any{"question": "which evidence?", "context": "synthetic observation"})
					case 2:
						rs, err := s.MainRequests(id, true)
						if err != nil || len(rs) != 1 || rs[0].State != "pending" {
							return llm.Message{}, "", llm.Usage{}, fmt.Errorf("request not queued: %v", err)
						}
						request = rs[0]
						return consultationCall("independent_work", "continue-1", map[string]any{})
					case 3:
						if workDone.Load() != 1 {
							return llm.Message{}, "", llm.Usage{}, fmt.Errorf("worker waited for main agent")
						}
						request, err = s.ClaimMainRequest(ctx)
						if err != nil {
							return llm.Message{}, "", llm.Usage{}, err
						}
						if _, err := ma.Consult(ctx, 0, nil, s, "synthetic test", request, nil, nil); err != nil {
							return llm.Message{}, "", llm.Usage{}, err
						}
						if atStop {
							return consultationText("finished independent work")
						}
						return consultationCall("blocked_action", "next-1", map[string]any{})
					default:
						if !hasMainReply(req.Messages, request.ID) {
							return llm.Message{}, "", llm.Usage{}, fmt.Errorf("steering missing")
						}
						return consultationText("received and applied reply")
					}
				}}
				extra := func(name string, counter *atomic.Int32) actool.CoreTool {
					return writeTool(name, "synthetic tool", obj(map[string]any{}), func(context.Context, json.RawMessage) (actool.Result, error) {
						counter.Add(1)
						return actool.Text("done"), nil
					})
				}
				w := NewWorker(wp, "fixture", dir, tx, 100000, 8, extra("independent_work", &workDone), extra("blocked_action", &blockedAction))
				w.SetNonStreaming(func() bool { return !stream })
				reason, _, err := w.Execute(t.Context(), "worker", 0, nil, s, intent, nil, nil, nil, nil)
				if err != nil || reason != harness.ReasonCompleted {
					t.Fatalf("execution: %s %v", reason, err)
				}
				if blockedAction.Load() != 0 || workerCalls != 4 {
					t.Fatalf("steering did not intercept next action: %d, calls %d", blockedAction.Load(), workerCalls)
				}
				got, _ := s.MainRequest(request.ID)
				if got.State != "delivered" {
					t.Fatalf("no durable delivery receipt: %+v", got)
				}
				hints, _ := s.ListByKind(db.KindHint, 100)
				if len(hints) != 1 || hints[0].Origin != "mainagent" {
					t.Fatalf("automatic coordination mislabeled: %+v", hints)
				}
				h := &mainReplyHooks{ts: s, intentID: id, store: tx, sessionID: WorkerSessionID(exp, id)}
				h.acknowledge()
				if _, ok := h.drainContext(t.Context()); ok {
					t.Fatal("delivered reply replayed")
				}
			})
		}
	}
}

func TestMainReplyRecoveryAndConcurrentDrain(t *testing.T) {
	d := testDB(t)
	defer d.Close()
	exp, _ := d.CreateExploration("recovery", "fixture")
	defer d.Exec(`DELETE FROM explorations WHERE id=$1`, exp)
	s := d.Exploration(exp)
	id, _ := s.AddIntent(map[string]any{"summary": "fixture"}, 1, nil, "planner")
	_, _ = s.ClaimIntent(id, "worker")
	r, err := s.CreateMainRequest(t.Context(), id, "request", "question", "")
	if err != nil {
		t.Fatal(err)
	}
	if err = s.AnswerMainRequest(t.Context(), r.ID, "answer", "mainagent"); err != nil {
		t.Fatal(err)
	}
	tx := transcript.NewStore(t.TempDir())
	sid := WorkerSessionID(exp, id)
	h := &mainReplyHooks{ts: s, intentID: id, store: tx, sessionID: sid}
	count := make(chan bool, 16)
	for i := 0; i < 16; i++ {
		go func() { _, ok := h.drainContext(t.Context()); count <- ok }()
	}
	n := 0
	for i := 0; i < 16; i++ {
		if <-count {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("duplicate steering: %d", n)
	}
	h.acknowledge()
	got, _ := s.MainRequest(r.ID)
	if got.State != "answered" {
		t.Fatal("acknowledged without transcript")
	}
	quoted := llm.UserText("Quoted diagnostic output contains " + mainReplyMarker(r.ID) + " but is not a delivery receipt")
	if err := tx.Append(tx.MainPath(sid), transcript.Record{Type: "message", Message: &quoted}); err != nil {
		t.Fatal(err)
	}
	h.acknowledge()
	got, _ = s.MainRequest(r.ID)
	if got.State != "answered" {
		t.Fatal("quoted marker falsely confirmed delivery")
	}
	msg := llm.Message{Role: llm.RoleUser, Content: []llm.ContentBlock{llm.ToolResultText("tool", fmt.Sprintf("Blocked by hook: %s\n【主 Agent 回复请示 #%d】\nanswer", mainReplyMarker(r.ID), r.ID), true)}}
	if err := tx.Append(tx.MainPath(sid), transcript.Record{Type: "message", Message: &msg}); err != nil {
		t.Fatal(err)
	}
	if _, ok := h.drainContext(t.Context()); ok {
		t.Fatal("live delivery replayed")
	}
	got, _ = s.MainRequest(r.ID)
	if got.State != "delivered" {
		t.Fatal("delivery not confirmed at next tool boundary")
	}
	// Simulate a crash after the transcript write but before the database ack.
	if _, err := d.Exec(`UPDATE main_requests SET state='answered' WHERE id=$1`, r.ID); err != nil {
		t.Fatal(err)
	}
	restarted := &mainReplyHooks{ts: s, intentID: id, store: tx, sessionID: sid}
	restarted.acknowledge()
	if _, ok := restarted.drainContext(t.Context()); ok {
		t.Fatal("crash between transcript write and ack caused replay")
	}
}
