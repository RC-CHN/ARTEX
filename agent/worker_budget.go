package agent

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/RC-CHN/ARTEX/db"
	"github.com/RC-CHN/ARTEX/internal/operation"
	"github.com/RC-CHN/ARTEX/norma/harness"
	"github.com/RC-CHN/ARTEX/norma/llm"
	actool "github.com/RC-CHN/ARTEX/norma/tool"
)

type workerTurnBudget struct {
	store            *db.ExplorationStore
	taskID, intentID int64
	announced        bool
	taskWarned       bool
}

func (b *workerTurnBudget) BeforeTurn(ctx context.Context, settling bool) (harness.TurnDecision, error) {
	bounded, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	state, allowed, err := b.store.ReserveWorkerTurn(bounded, b.taskID, b.intentID, settling)
	if err != nil {
		return harness.TurnDecision{}, err
	}
	d := harness.TurnDecision{Allowed: allowed, Settle: !allowed && state.Phase == "wrapup"}
	if !allowed {
		if d.Settle {
			d.Notice = fmt.Sprintf("【Worker 预算已到】本意图累计常规轮数 %d/%d；进入收尾，还可用 %d 轮。保存已有结果、部署清单、证据路径及未完成事项，然后纯文本汇报。若仍需探索，使用 ask_main_agent 明确请求追加预算；不能自行续期。", state.NormalUsed, state.NormalLimit, max(0, state.WrapupLimit-state.WrapupUsed))
			if state.TaskBudget != nil && state.TaskBudget.Reached {
				d.Notice = fmt.Sprintf("【任务累计软预算已到】已记录输入＋输出 token %d/%d，包含本任务各 Agent 与压缩调用。现在进入现有收尾阶段，保存结果并汇报；单独追加 Worker 轮数不能解除任务软预算。需要继续时向主 Agent 说明，由人类调整任务预算。", state.TaskBudget.Used, state.TaskBudget.Limit)
			}
		} else {
			d.Notice = "【Worker 收尾预算耗尽】已保存的进度保留，暂停继续执行。需主 Agent 或人类明确追加预算后再恢复。"
		}
		return d, nil
	}
	if settling {
		left := state.WrapupLimit - state.WrapupUsed
		d.Notice = fmt.Sprintf("【收尾预算】当前为第 %d/%d 轮，本轮后剩余 %d 轮。只保存和核对已有结果，不发起新探测。", state.WrapupUsed, state.WrapupLimit, left)
		if left == 0 {
			d.Notice += "这是最后一轮，请现在直接用纯文本给出结果、证据路径、未完成事项与撤除情况，不再发起工具调用。"
		}
	} else if !b.announced || state.NormalLimit > 0 && (state.NormalLimit-state.NormalUsed == 10 || state.NormalLimit-state.NormalUsed == 1) {
		limit := fmt.Sprint(state.NormalLimit)
		if state.NormalLimit == 0 {
			limit = "不限"
		}
		d.Notice = fmt.Sprintf("【Worker 预算】本意图累计常规执行轮次 %d/%s；收尾额度 %d，已用 %d。暂停、重试、压缩和恢复不重置；底层网络重试和压缩调用不另计轮次。用 get_worker_budget 查看；需要追加时通过 ask_main_agent 说明证据、剩余工作和所需轮数。", state.NormalUsed, limit, state.WrapupLimit, state.WrapupUsed)
	}
	b.announced = true
	if state.TaskBudget != nil && !b.taskWarned && state.TaskBudget.Used >= state.TaskBudget.Limit*8/10 {
		b.taskWarned = true
		d.Notice += fmt.Sprintf("\n【任务累计预算提醒】已记录输入＋输出 token %d/%d；达到后将停止自动领取新工作，并在轮次边界收尾。", state.TaskBudget.Used, state.TaskBudget.Limit)
	}
	return d, nil
}
func (b *workerTurnBudget) AfterTurn(ctx context.Context, u llm.Usage) error {
	if u.InputTokens == 0 && u.OutputTokens == 0 {
		return nil
	}
	bounded, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	return b.store.RecordWorkerBudgetUsage(bounded, b.taskID, b.intentID, u.InputTokens, u.OutputTokens)
}
func budgetToolError(err error) (actool.Result, error) {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return actool.Result{}, err
	}
	var fault *operation.Error
	if errors.As(err, &fault) {
		out := map[string]any{"code": fault.Code, "message": fault.Message, "next_action": fault.NextAction, "executed": false}
		if fault.CurrentRevision > 0 {
			out["current_revision"] = fault.CurrentRevision
		}
		raw, _ := json.Marshal(out)
		return actool.Errorf(string(raw)), nil
	}
	if errors.Is(err, sql.ErrNoRows) {
		return actool.Fail("budget_not_found", "当前任务尚无该意图预算记录。", "核对意图 ID；首次执行时创建预算，尚未开始的 Worker 无累计用量。"), nil
	}
	if errors.Is(err, operation.ErrKeyReuse) || errors.Is(err, operation.ErrKeyRequired) {
		return actool.Fail("invalid_request_id", err.Error(), "相同 request_id 仅重试完全相同参数；新的追加操作使用新标识。"), nil
	}
	return actool.Errorf(`{"code":"budget_result_unknown","execution_status":"unknown","message":"预算操作结果尚未确认。","next_action":"读取 get_worker_budget 核对；追加操作只使用原 request_id 和相同参数重试。"}`), nil
}
func (t *ToolSet) getWorkerBudget() actool.CoreTool {
	return t.readExpTool("get_worker_budget", "查询本任务 Worker 意图的累计预算、已预留轮数和记录用量。默认150轮常规执行+10轮收尾，暂停/重试/恢复不重置。Worker 省略 intent_id 查询自己；尚未开始返回 budget_not_found。每次 Worker 模型迭代（含输出恢复迭代）预留一轮，崩溃不返还；底层网络重试和压缩调用不另计。", obj(map[string]any{"intent_id": idp("意图编号；Worker 可省略")}), func(ctx context.Context, raw json.RawMessage) (actool.Result, error) {
		var in struct {
			IntentID int64 `json:"intent_id"`
		}
		if json.Unmarshal(raw, &in) != nil {
			return actool.Fail("invalid_input", "预算参数不是有效 JSON。", "提供 intent_id，或 Worker 用空对象查询自己。"), nil
		}
		info := RunInfoFrom(ctx)
		if in.IntentID == 0 {
			in.IntentID = info.IntentID
		}
		if info.TaskID <= 0 || in.IntentID <= 0 {
			return actool.Fail("invalid_input", "需要任务和意图上下文。", "提供本任务的正整数 intent_id。"), nil
		}
		b, err := t.ts.WorkerBudget(ctx, info.TaskID, in.IntentID)
		if err != nil {
			return budgetToolError(err)
		}
		taskBudget, err := t.ts.TaskTokenBudget(ctx, info.TaskID, true)
		if err != nil {
			return budgetToolError(err)
		}
		b.TaskBudget = &taskBudget
		return jsonResult(b)
	})
}
func (t *ToolSet) grantWorkerBudget() actool.CoreTool {
	return t.writeExpTool("grant_worker_budget", "主 Agent 明确追加本任务 Worker 的累计预算。先 get_worker_budget 获取 revision，再提交理由、追加常规轮数及收尾轮数；默认不追加收尾，常用 additional_wrapup_turns=10。需要 request_id 幂等重试和 expected_revision 防并发覆盖。不会自动恢复任务或解除时限，追加后用 steer_work 协调继续。Worker 无权自行调用。", obj(map[string]any{"intent_id": idp("意图编号"), "expected_revision": idp("刚读到的 revision"), "request_id": str("稳定操作标识"), "additional_turns": idp("追加常规轮数1–1000"), "additional_wrapup_turns": idp("追加收尾轮数0–100，省略为0"), "reason": str("基于证据的追加理由")}, "intent_id", "expected_revision", "request_id", "additional_turns", "reason"), func(ctx context.Context, raw json.RawMessage) (actool.Result, error) {
		info := RunInfoFrom(ctx)
		if info.AgentKey != "mainagent" || info.IntentID != 0 || info.TaskID <= 0 {
			return actool.Fail("budget_forbidden", "只有任务主 Agent 可追加预算。", "Worker 使用 ask_main_agent 提交证据及所需轮数。"), nil
		}
		var in db.WorkerBudgetGrant
		if json.Unmarshal(raw, &in) != nil {
			return actool.Fail("invalid_input", "预算参数不是有效 JSON。", "提供 intent_id、expected_revision、request_id、additional_turns、reason。"), nil
		}
		b, err := t.ts.GrantWorkerBudget(ctx, info.TaskID, in, "mainagent")
		if err != nil {
			return budgetToolError(err)
		}
		return jsonResult(map[string]any{"budget": b, "automatic_resume": false, "next_action": "预算已追加。运行中可通过 steer_work 协调；已经停止时从 Worker 会话重跑继续，保留原意图和历史。不要新建相同意图绕过预算。"})
	})
}
