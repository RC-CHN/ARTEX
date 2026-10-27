package db

import "context"

// Handoff checks contain IDs and actionable gaps, never guessed success.
type HandoffCheck struct {
	Code       string `json:"code"`
	Count      int64  `json:"count"`
	NextAction string `json:"next_action"`
}

func (d *DB) TaskHandoff(ctx context.Context, taskID int64) ([]HandoffCheck, error) {
	var exp int64
	if err := d.QueryRowContext(ctx, `SELECT exploration_id FROM tasks WHERE id=$1 AND deleted_at IS NULL`, taskID).Scan(&exp); err != nil {
		return nil, err
	}
	checks := []struct {
		code, q, next string
		args          []any
	}{
		{"workers_running", `SELECT count(*) FROM exploration_nodes WHERE exploration_id=$1 AND kind='intent' AND state='running'`, "等待 Worker 完成或明确协调停止；当前导出只能代表截止点快照。", []any{exp}},
		{"workers_incomplete", `SELECT count(*) FROM exploration_nodes WHERE exploration_id=$1 AND kind='intent' AND state IN ('blocked','exhausted','paused')`, "检查尚未完成的意图，汇报未覆盖范围和失败原因。", []any{exp}},
		{"main_requests_pending", `SELECT count(*) FROM main_requests WHERE exploration_id=$1 AND state IN ('pending','processing','answered')`, "核对主 Agent 问答是否仍需处理或投递。", []any{exp}},
	}
	out := []HandoffCheck{}
	for _, c := range checks {
		var n int64
		if err := d.QueryRowContext(ctx, c.q, c.args...).Scan(&n); err != nil {
			return nil, err
		}
		if n > 0 {
			out = append(out, HandoffCheck{Code: c.code, Count: n, NextAction: c.next})
		}
	}
	return out, nil
}
