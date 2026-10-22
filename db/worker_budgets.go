package db

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/RC-CHN/ARTEX/internal/operation"
)

const DefaultWorkerTurns = 150
const DefaultWorkerWrapupTurns = 10

type WorkerBudget struct {
	IntentID     int64            `json:"intent_id"`
	TaskID       int64            `json:"task_id"`
	NormalLimit  int              `json:"normal_limit"`
	WrapupLimit  int              `json:"wrapup_limit"`
	NormalUsed   int              `json:"normal_used"`
	WrapupUsed   int              `json:"wrapup_used"`
	Phase        string           `json:"phase"`
	InputTokens  int64            `json:"input_tokens"`
	OutputTokens int64            `json:"output_tokens"`
	Revision     int64            `json:"revision"`
	TaskBudget   *TaskTokenBudget `json:"task_budget,omitempty"`
}

const workerBudgetCols = `intent_id,task_id,normal_limit,wrapup_limit,normal_used,wrapup_used,phase,input_tokens,output_tokens,revision`

func scanWorkerBudget(row interface{ Scan(...any) error }) (b WorkerBudget, err error) {
	err = row.Scan(&b.IntentID, &b.TaskID, &b.NormalLimit, &b.WrapupLimit, &b.NormalUsed, &b.WrapupUsed, &b.Phase, &b.InputTokens, &b.OutputTokens, &b.Revision)
	return
}
func (d *DB) WorkerBudget(ctx context.Context, taskID, intentID int64) (WorkerBudget, error) {
	return scanWorkerBudget(d.QueryRowContext(ctx, `SELECT `+workerBudgetCols+` FROM worker_budgets WHERE task_id=$1 AND intent_id=$2`, taskID, intentID))
}
func (d *DB) ListWorkerBudgets(ctx context.Context, taskID, after int64) ([]WorkerBudget, error) {
	rows, err := d.QueryContext(ctx, `SELECT `+workerBudgetCols+` FROM worker_budgets WHERE task_id=$1 AND intent_id>$2 ORDER BY intent_id LIMIT 100`, taskID, after)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []WorkerBudget{}
	for rows.Next() {
		b, e := scanWorkerBudget(rows)
		if e != nil {
			return nil, e
		}
		out = append(out, b)
	}
	return out, rows.Err()
}
func (s *ExplorationStore) EnsureWorkerBudget(ctx context.Context, taskID, intentID int64, normal, wrapup int) (WorkerBudget, error) {
	if normal < 0 || wrapup <= 0 {
		return WorkerBudget{}, fmt.Errorf("invalid worker budget")
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO worker_budgets(intent_id,task_id,normal_limit,wrapup_limit)
 SELECT n.id,t.id,$3,$4 FROM exploration_nodes n JOIN tasks t ON t.exploration_id=n.exploration_id WHERE t.id=$1 AND n.id=$2 AND n.exploration_id=$5 AND n.kind='intent' AND t.deleted_at IS NULL AND t.archived_at IS NULL
 ON CONFLICT(intent_id) DO NOTHING`, taskID, intentID, normal, wrapup, s.expID)
	if err != nil {
		return WorkerBudget{}, err
	}
	return s.db.WorkerBudget(ctx, taskID, intentID)
}

// Reserve BEFORE a model request: a process crash consumes the reservation;
// it never silently refunds an attempt that might already have side effects.
// No transaction or lock spans model/tool execution.
func (s *ExplorationStore) ReserveWorkerTurn(ctx context.Context, taskID, intentID int64, wrapup bool) (WorkerBudget, bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return WorkerBudget{}, false, err
	}
	defer tx.Rollback()
	b, err := scanWorkerBudget(tx.QueryRowContext(ctx, `SELECT `+workerBudgetCols+` FROM worker_budgets WHERE task_id=$1 AND intent_id=$2 FOR UPDATE`, taskID, intentID))
	if err != nil {
		return b, false, err
	}
	oldPhase := b.Phase
	taskBudget, err := taskTokenBudget(ctx, tx, taskID, false)
	if err != nil {
		return b, false, err
	}
	if taskBudget.Limit > 0 {
		b.TaskBudget = &taskBudget
	}
	if taskBudget.Reached || wrapup || b.Phase != "normal" || (b.NormalLimit > 0 && b.NormalUsed >= b.NormalLimit) {
		b.Phase = "wrapup"
	}
	allowed := false
	if b.Phase == "normal" {
		b.NormalUsed++
		allowed = true
	} else if b.WrapupUsed >= b.WrapupLimit {
		b.Phase = "exhausted"
	} else if wrapup {
		b.WrapupUsed++
		allowed = true
		if b.WrapupUsed == b.WrapupLimit {
			b.Phase = "exhausted"
		}
	}
	// The final reservation marks the ledger exhausted, while allowed remains
	// true so that reserved turn can finish and write back its tool results.
	_, err = tx.ExecContext(ctx, `UPDATE worker_budgets SET normal_used=$3,wrapup_used=$4,phase=$5,updated_at=now() WHERE task_id=$1 AND intent_id=$2`, taskID, intentID, b.NormalUsed, b.WrapupUsed, b.Phase)
	if err != nil {
		return b, false, err
	}
	if oldPhase != b.Phase {
		raw, _ := json.Marshal(b)
		if _, err = tx.ExecContext(ctx, `INSERT INTO worker_budget_events(task_id,intent_id,kind,detail) VALUES($1,$2,$3,$4)`, taskID, intentID, b.Phase, string(raw)); err != nil {
			return b, false, err
		}
	}
	return b, allowed, tx.Commit()
}
func (s *ExplorationStore) RecordWorkerBudgetUsage(ctx context.Context, taskID, intentID int64, input, output int) error {
	_, err := s.db.ExecContext(ctx, `UPDATE worker_budgets SET input_tokens=input_tokens+$3,output_tokens=output_tokens+$4,updated_at=now() WHERE task_id=$1 AND intent_id=$2`, taskID, intentID, max(0, input), max(0, output))
	return err
}
func (s *ExplorationStore) WorkerBudget(ctx context.Context, taskID, intentID int64) (WorkerBudget, error) {
	return s.db.WorkerBudget(ctx, taskID, intentID)
}

type WorkerBudgetGrant struct {
	IntentID              int64  `json:"intent_id"`
	ExpectedRevision      int64  `json:"expected_revision"`
	RequestID             string `json:"request_id"`
	AdditionalTurns       int    `json:"additional_turns"`
	AdditionalWrapupTurns int    `json:"additional_wrapup_turns"`
	Reason                string `json:"reason"`
}

func (d *DB) GrantWorkerBudget(ctx context.Context, taskID int64, in WorkerBudgetGrant, actor string) (WorkerBudget, error) {
	var out WorkerBudget
	if actor != "mainagent" && actor != "human" {
		return out, &operation.Error{Code: "budget_forbidden", Message: "只有主 Agent 或人类可以追加预算。", NextAction: "Worker 使用 ask_main_agent 提交证据和所需追加轮数。"}
	}
	if err := operation.ValidateKey(in.RequestID); err != nil {
		return out, err
	}
	if in.IntentID <= 0 || in.ExpectedRevision <= 0 || in.AdditionalTurns < 1 || in.AdditionalTurns > 1000 || in.AdditionalWrapupTurns < 0 || in.AdditionalWrapupTurns > 100 || strings.TrimSpace(in.Reason) == "" || len(in.Reason) > 4000 {
		return out, &operation.Error{Code: "invalid_budget", Message: "提供 intent_id、expected_revision、reason；additional_turns 为1–1000，additional_wrapup_turns 为0–100。", NextAction: "先读取 get_worker_budget，按证据提供明确追加值；0表示不追加收尾轮数。"}
	}
	tx, err := d.BeginTx(ctx, nil)
	if err != nil {
		return out, err
	}
	defer tx.Rollback()
	var state string
	if err = tx.QueryRowContext(ctx, `SELECT n.state FROM tasks t JOIN exploration_nodes n ON n.exploration_id=t.exploration_id WHERE t.id=$1 AND n.id=$2 AND n.kind='intent' AND t.deleted_at IS NULL AND t.archived_at IS NULL FOR SHARE OF t`, taskID, in.IntentID).Scan(&state); err != nil {
		return out, err
	}
	out, err = scanWorkerBudget(tx.QueryRowContext(ctx, `SELECT `+workerBudgetCols+` FROM worker_budgets WHERE task_id=$1 AND intent_id=$2 FOR UPDATE`, taskID, in.IntentID))
	if err != nil {
		return out, err
	}
	scope := fmt.Sprintf("worker-budget:%d:%d", taskID, in.IntentID)
	if replay, e := operation.Replay(ctx, tx, scope, in.RequestID, in, &out); e != nil || replay {
		return out, e
	}
	if state == "done" || state == "stopped" || state == "deleted" {
		return out, &operation.Error{Code: "worker_terminal", Message: "该意图已完成或取消。", NextAction: "不要重复原操作；确有新工作时创建新的明确意图。"}
	}
	if out.Revision != in.ExpectedRevision {
		return out, &operation.Error{Code: "budget_conflict", Message: "预算已变化。", CurrentRevision: out.Revision, NextAction: "重新读取 get_worker_budget；基于最新用量决定追加值并用新 request_id 提交。"}
	}
	if out.NormalLimit == 0 {
		return out, &operation.Error{Code: "budget_unlimited", Message: "该意图的常规轮数未设上限。", NextAction: "无需追加；用 get_worker_budget 查看是否处于时限收尾。"}
	}
	if out.NormalLimit > 1000000-in.AdditionalTurns || out.WrapupLimit > 1000000-in.AdditionalWrapupTurns {
		return out, &operation.Error{Code: "budget_limit", Message: "累计预算上限为100万轮。", NextAction: "缩小意图范围并先完成交接。"}
	}
	out, err = scanWorkerBudget(tx.QueryRowContext(ctx, `UPDATE worker_budgets SET normal_limit=normal_limit+$3,wrapup_limit=wrapup_limit+$4,phase='normal',revision=revision+1,updated_at=now() WHERE task_id=$1 AND intent_id=$2 RETURNING `+workerBudgetCols, taskID, in.IntentID, in.AdditionalTurns, in.AdditionalWrapupTurns))
	if err != nil {
		return out, err
	}
	detail, _ := json.Marshal(map[string]any{"actor": actor, "request": in, "budget": out})
	if _, err = tx.ExecContext(ctx, `INSERT INTO worker_budget_events(task_id,intent_id,kind,detail) VALUES($1,$2,'grant',$3)`, taskID, in.IntentID, string(detail)); err != nil {
		return out, err
	}
	if err = operation.Save(ctx, tx, scope, in.RequestID, in, out); err != nil {
		return out, err
	}
	return out, tx.Commit()
}
func (s *ExplorationStore) GrantWorkerBudget(ctx context.Context, taskID int64, in WorkerBudgetGrant, actor string) (WorkerBudget, error) {
	return s.db.GrantWorkerBudget(ctx, taskID, in, actor)
}

// One-time upgrade from the previous unbounded Worker default. Later explicit
// zero/custom limits are preserved. Existing positive per-agent limits survive.
func (d *DB) seedWorkerBudgetDefaults() error {
	tx, err := d.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var inserted string
	err = tx.QueryRow(`INSERT INTO settings(key,value) VALUES('worker_budget_150_v1','true') ON CONFLICT(key) DO NOTHING RETURNING key`).Scan(&inserted)
	if err == sql.ErrNoRows {
		return nil
	}
	if err != nil {
		return err
	}
	if _, err = tx.Exec(`UPDATE agents SET max_turns=CASE WHEN max_turns=0 THEN 150 ELSE max_turns END,wrapup_max_turns=CASE WHEN wrapup_max_turns=0 THEN 10 ELSE wrapup_max_turns END WHERE key='worker'`); err != nil {
		return err
	}
	return tx.Commit()
}
