package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/RC-CHN/ARTEX/db"
	"github.com/RC-CHN/ARTEX/norma/harness"
	"github.com/RC-CHN/ARTEX/norma/llm"
	"github.com/RC-CHN/ARTEX/norma/permission"
	actool "github.com/RC-CHN/ARTEX/norma/tool"
	"github.com/RC-CHN/ARTEX/norma/transcript"
)

func (t *ToolSet) askMainAgent() actool.CoreTool {
	return actool.Build(actool.Spec{
		Name: "ask_main_agent", Description: "异步向主 Agent 请示判断、信息或协作。立即返回已入队回执，你继续可独立进行的工作；回复经 steering 注入，不需要轮询。说明问题、已有证据和需要的决定。不是向人类申请授权；请勿重复提交同一问题。意图结束/取消后未送达请求会作废。",
		Schema: obj(map[string]any{"question": str("需要主 Agent 回答的具体问题"), "context": str("证据、已尝试方法、节点编号及建议选项；不要粘贴整份日志")}, "question"),
		Permissions: func(context.Context, json.RawMessage, permission.Context) permission.Decision {
			return permission.Allowed()
		},
		Run: func(ctx context.Context, in json.RawMessage, tc *actool.ToolContext) (actool.Result, error) {
			if t.ts == nil || t.ownerNode == 0 || tc == nil || tc.ToolUseID == "" {
				return actool.Errorf("仅执行中的 Worker 可提交请示"), nil
			}
			var args struct {
				Question string `json:"question"`
				Context  string `json:"context"`
			}
			if err := json.Unmarshal(in, &args); err != nil {
				return actool.Errorf(err.Error()), nil
			}
			r, err := t.ts.CreateMainRequest(ctx, t.ownerNode, tc.ToolUseID, args.Question, args.Context)
			if err != nil {
				return actool.Errorf(err.Error()), nil
			}
			if t.publishMainNotice != nil {
				if notices, err := t.ts.MainRequestActivities(r.ID); err == nil {
					for _, notice := range notices {
						t.publishMainNotice(notice)
					}
				}
			}
			return jsonResult(map[string]any{"request_id": r.ID, "status": r.State, "receipt": "已进入主 Agent 处理队列。继续可独立执行的工作，回复会通过 steering 送达；无需等待或轮询。"})
		},
	})
}

func (t *ToolSet) listMainRequests() actool.CoreTool {
	return t.readExpTool("list_main_requests", "查看本任务的 Worker 请示和回复状态（最多最近 100 条）；可按 intent_id 筛选。", obj(map[string]any{"intent_id": idp("可选，意图编号")}), func(_ context.Context, in json.RawMessage) (actool.Result, error) {
		var args struct {
			IntentID int64 `json:"intent_id"`
		}
		if err := json.Unmarshal(in, &args); err != nil {
			return actool.Errorf(err.Error()), nil
		}
		rs, err := t.ts.MainRequests(args.IntentID, false)
		if err != nil {
			return actool.Errorf(err.Error()), nil
		}
		return jsonResult(rs)
	})
}

func (t *ToolSet) replyToWorker() actool.CoreTool {
	return t.writeExpTool("reply_to_worker", "回复本任务的 Worker 请示。先查证、协调，再给出可执行建议；回复持久化后经 steering 送达。不得把自动回复说成人类授权。已取消或结束的意图不再接受回复。", obj(map[string]any{"request_id": idp("请示编号"), "answer": str("答复、依据、建议下一步；若需要人类决定，明确说明尚未获得授权")}, "request_id", "answer"), func(ctx context.Context, in json.RawMessage) (actool.Result, error) {
		var args struct {
			ID     int64  `json:"request_id"`
			Answer string `json:"answer"`
		}
		if err := json.Unmarshal(in, &args); err != nil {
			return actool.Errorf(err.Error()), nil
		}
		attempt := 0
		if t.mainRequest != nil {
			if args.ID != t.mainRequest.ID {
				return actool.Errorf("本轮只能答复触发它的请示；其他 Worker 可通过 steer_work 协调"), nil
			}
			attempt = t.mainRequest.Attempts
		}
		if err := t.ts.AnswerMainRequestAttempt(ctx, args.ID, args.Answer, "mainagent", attempt); err != nil {
			return actool.Errorf(err.Error()), nil
		}
		return jsonResult(map[string]any{"request_id": args.ID, "status": "answered", "delivery": "steering"})
	})
}

func mainReplyMarker(id int64) string { return fmt.Sprintf("<!-- ARTEX_MAIN_REPLY:%d -->", id) }

func hasMainReply(messages []llm.Message, id int64) bool {
	marker := fmt.Sprintf("%s\n【主 Agent 回复请示 #%d】\n", mainReplyMarker(id), id)
	var contains func([]llm.ContentBlock) bool
	contains = func(blocks []llm.ContentBlock) bool {
		for _, b := range blocks {
			if strings.HasPrefix(b.Text, marker) || strings.HasPrefix(b.Text, "Blocked by hook: "+marker) || contains(b.Content) {
				return true
			}
		}
		return false
	}
	for _, m := range messages {
		if m.Role == llm.RoleUser && contains(m.Content) {
			return true
		}
	}
	return false
}

// mainReplyHooks extends the existing steering boundary without blocking Worker
// execution on a main turn. One receipt is injected at most once per run; a cold
// restart checks the durable transcript before acknowledging it. Acknowledgment
// never precedes persistence, so a crash cannot silently eat a reply.
type mainReplyHooks struct {
	inner     harness.HookRunner
	ts        *db.ExplorationStore
	intentID  int64
	store     *transcript.Store
	sessionID string
	mu        sync.Mutex
	seen      map[int64]bool
}

func (h *mainReplyHooks) acknowledge() {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	h.mu.Lock()
	defer h.mu.Unlock()
	rs, err := h.ts.MainRequestsContext(ctx, h.intentID, true)
	if err == nil {
		h.confirmReplies(ctx, rs, true)
	}
}

// confirmReplies is called under mu. During a live execution only previously
// injected receipts need checking. With no answers we never scan a transcript.
// On restart, check all outstanding answers before allowing any injection.
func (h *mainReplyHooks) confirmReplies(ctx context.Context, rs []*db.MainRequest, includeUnseen bool) {
	if h.store == nil {
		return
	}
	var candidates []int64
	for _, r := range rs {
		if r.State == "answered" && (includeUnseen || h.seen[r.ID]) {
			candidates = append(candidates, r.ID)
		}
	}
	if len(candidates) == 0 {
		return
	}
	recs, err := h.store.Load(h.store.MainPath(h.sessionID))
	if err != nil {
		return
	}
	messages, _ := transcript.Messages(recs)
	if h.seen == nil {
		h.seen = map[int64]bool{}
	}
	for _, id := range candidates {
		if hasMainReply(messages, id) {
			// Even if the database is temporarily unavailable, this run must not
			// inject a receipt already present in its resumed transcript again.
			h.seen[id] = true
			_ = h.ts.AckMainReplyContext(ctx, id)
		}
	}
}

func (h *mainReplyHooks) drainContext(ctx context.Context) (string, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.seen == nil {
		h.seen = map[int64]bool{}
	}
	rs, err := h.ts.MainRequestsContext(ctx, h.intentID, true)
	if err != nil {
		return "", false
	}
	h.confirmReplies(ctx, rs, false)
	// Oldest first, bounded by the task's outstanding-request limit.
	for i := len(rs) - 1; i >= 0; i-- {
		r := rs[i]
		if r.State != "answered" || h.seen[r.ID] {
			continue
		}
		h.seen[r.ID] = true
		return fmt.Sprintf("%s\n【主 Agent 回复请示 #%d】\n问题：%s\n回复来源：%s\n%s\n这是对本意图的协作回复，不代表新增人类授权；结合当前进度调整下一步，不重复已完成的操作。", mainReplyMarker(r.ID), r.ID, r.Question, r.AnswerSource, r.Answer), true
	}
	return "", false
}

func (h *mainReplyHooks) PreToolUse(ctx context.Context, name string, input []byte) (bool, string, []byte) {
	if msg, ok := h.drainContext(ctx); ok {
		return true, msg + "\n本次工具调用未执行，请根据回复重新决定。", nil
	}
	if h.inner != nil {
		return h.inner.PreToolUse(ctx, name, input)
	}
	return false, "", nil
}
func (h *mainReplyHooks) PostToolUse(ctx context.Context, name string, input, result []byte, isErr bool) {
	if h.inner != nil {
		h.inner.PostToolUse(ctx, name, input, result, isErr)
	}
}
func (h *mainReplyHooks) Stop(ctx context.Context, messages []llm.Message) (bool, []string, string) {
	if h.inner != nil {
		stop, blocking, msg := h.inner.Stop(ctx, messages)
		if stop || len(blocking) > 0 {
			return stop, blocking, msg
		}
	}
	if msg, ok := h.drainContext(ctx); ok {
		return false, []string{msg}, ""
	}
	return false, nil, ""
}

const mainConsultationPrompt = `

本轮由 Worker 的异步请示自动触发，不是人类消息，也不是新增授权。
请在现有任务目标和约束内，用工具查证事实、查看其他 Worker 的进展、必要时协调它们。
Worker 提问后仍继续执行，所以回复前核对最新进度。用 reply_to_worker(request_id, answer) 正式答复；仅在对话里写答案不会送达。
不要等待 Worker 停下来，不要替代 Planner 持续生成计划。不得修改目标、放宽约束或扩大授权范围。
确需人类决策时，在主对话明确指出，并回复 Worker：尚未获得决定、哪些工作可以继续。不得替人类批准。
所有调查、协调和答复都会向人类展示。请求已取消/意图已结束时停止处理，不要复活它。`
