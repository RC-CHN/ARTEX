package db

import (
	"context"
	"database/sql"
	"strconv"

	"github.com/RC-CHN/ARTEX/internal/operation"
)

// Soft admission threshold over existing metering, not a billing guarantee.
// In-flight requests and wrap-up may exceed it; zero preserves unlimited work.
type TaskTokenBudget struct {
	Limit          int64 `json:"limit"`
	Used           int64 `json:"used"`
	Revision       int64 `json:"revision"`
	Reached        bool  `json:"reached"`
	UsageAvailable bool  `json:"usage_available"`
}

type tokenBudgetReader interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

func (d *DB) TaskTokenBudget(ctx context.Context, taskID int64, includeUsage bool) (TaskTokenBudget, error) {
	return taskTokenBudget(ctx, d.DB, taskID, includeUsage)
}
func taskTokenBudget(ctx context.Context, q tokenBudgetReader, taskID int64, includeUsage bool) (b TaskTokenBudget, err error) {
	err = q.QueryRowContext(ctx, `SELECT token_budget_limit,token_budget_revision FROM tasks WHERE id=$1 AND deleted_at IS NULL AND archived_at IS NULL`, taskID).Scan(&b.Limit, &b.Revision)
	if err != nil {
		return
	}
	if b.Limit == 0 && !includeUsage {
		return
	}
	var available bool
	if err = q.QueryRowContext(ctx, `SELECT to_regclass('llm_usage') IS NOT NULL`).Scan(&available); err != nil {
		return
	}
	if !available {
		if b.Limit > 0 {
			err = &operation.Error{Code: "budget_usage_unavailable", Message: "累计用量暂不可查询。", NextAction: "恢复用量数据库后再开始自动工作；不要推断预算尚未使用。"}
		}
		return
	}
	err = q.QueryRowContext(ctx, `SELECT COALESCE(sum(input_tokens::bigint+output_tokens::bigint),0) FROM llm_usage WHERE task_id=$1`, strconv.FormatInt(taskID, 10)).Scan(&b.Used)
	b.UsageAvailable = err == nil
	b.Reached = b.Limit > 0 && b.Used >= b.Limit
	return
}

func (s *ExplorationStore) TaskTokenBudget(ctx context.Context, taskID int64, includeUsage bool) (TaskTokenBudget, error) {
	return s.db.TaskTokenBudget(ctx, taskID, includeUsage)
}

func (d *DB) SetTaskTokenBudget(ctx context.Context, taskID, limit, revision int64) (TaskTokenBudget, error) {
	if limit < 0 || limit > 1_000_000_000_000 || revision <= 0 {
		return TaskTokenBudget{}, &operation.Error{Code: "invalid_budget", Message: "预算必须为0至1万亿的整数，并提供当前revision。", NextAction: "0表示不限额；重新读取预算后提交。"}
	}
	tx, err := d.BeginTx(ctx, nil)
	if err != nil {
		return TaskTokenBudget{}, err
	}
	defer tx.Rollback()
	var next int64
	err = tx.QueryRowContext(ctx, `UPDATE tasks SET token_budget_limit=$2,token_budget_revision=token_budget_revision+1 WHERE id=$1 AND token_budget_revision=$3 AND deleted_at IS NULL AND archived_at IS NULL RETURNING token_budget_revision`, taskID, limit, revision).Scan(&next)
	if err == sql.ErrNoRows {
		return TaskTokenBudget{}, &operation.Error{Code: "budget_conflict", Message: "任务预算已变更或任务不可用。", NextAction: "重新读取预算，核对后再设置；不自动恢复已停止的Worker。"}
	}
	if err != nil {
		return TaskTokenBudget{}, err
	}
	b, err := taskTokenBudget(ctx, tx, taskID, true)
	if err != nil {
		return b, err
	}
	err = tx.Commit()
	return b, err
}
