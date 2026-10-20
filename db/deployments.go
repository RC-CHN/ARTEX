package db

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/RC-CHN/ARTEX/internal/operation"
)

type Deployment struct {
	ID           int64           `json:"id"`
	TaskID       int64           `json:"task_id"`
	ResourceID   *int64          `json:"resource_id,omitempty"`
	Name         string          `json:"name"`
	Kind         string          `json:"kind"`
	Location     string          `json:"location"`
	Identifier   string          `json:"identifier"`
	Cleanup      string          `json:"cleanup"`
	State        string          `json:"state"`
	Verification string          `json:"verification"`
	Note         string          `json:"note"`
	Revision     int64           `json:"revision"`
	Actor        json.RawMessage `json:"actor"`
	CreatedAt    time.Time       `json:"created_at"`
	UpdatedAt    time.Time       `json:"updated_at"`
}
type DeploymentChange struct {
	ID               int64   `json:"id"`
	ExpectedRevision int64   `json:"expected_revision"`
	RequestID        string  `json:"request_id"`
	ResourceID       *int64  `json:"resource_id,omitempty"`
	Name             *string `json:"name,omitempty"`
	Kind             *string `json:"kind,omitempty"`
	Location         *string `json:"location,omitempty"`
	Identifier       *string `json:"identifier,omitempty"`
	Cleanup          *string `json:"cleanup,omitempty"`
	State            *string `json:"state,omitempty"`
	Verification     *string `json:"verification,omitempty"`
	Note             *string `json:"note,omitempty"`
}

const deploymentCols = `id,task_id,resource_id,name,kind,location,identifier,cleanup,state,verification,note,revision,actor,created_at,updated_at`

func scanDeployment(row interface{ Scan(...any) error }) (Deployment, error) {
	var r Deployment
	err := row.Scan(&r.ID, &r.TaskID, &r.ResourceID, &r.Name, &r.Kind, &r.Location, &r.Identifier, &r.Cleanup, &r.State, &r.Verification, &r.Note, &r.Revision, &r.Actor, &r.CreatedAt, &r.UpdatedAt)
	return r, err
}
func (d *DB) Deployment(ctx context.Context, taskID, id int64) (Deployment, error) {
	v, err := scanDeployment(d.QueryRowContext(ctx, `SELECT `+deploymentCols+` FROM task_deployments WHERE task_id=$1 AND id=$2`, taskID, id))
	if err == sql.ErrNoRows {
		err = &operation.Error{Code: "deployment_not_found", Message: "当前任务没有这条部署登记。", NextAction: "先用 manage_deployments(action=list) 核对部署 id，不要使用资源 id。"}
	}
	return v, err
}
func (d *DB) ListDeployments(ctx context.Context, taskID, before int64, state string) ([]Deployment, error) {
	rows, err := d.QueryContext(ctx, `SELECT `+deploymentCols+` FROM task_deployments WHERE task_id=$1 AND ($2::bigint=0 OR id<$2) AND ($3='' OR state=$3) ORDER BY id DESC LIMIT 100`, taskID, before, state)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Deployment{}
	for rows.Next() {
		v, e := scanDeployment(rows)
		if e != nil {
			return nil, e
		}
		out = append(out, v)
	}
	return out, rows.Err()
}
func deploymentInput(message string) error {
	return &operation.Error{Code: "invalid_deployment", Message: message, NextAction: "读取 manage_deployments(get/list)，只修正所提示字段后提交；不执行任何远端命令。"}
}
func (d *DB) ChangeDeployment(ctx context.Context, taskID int64, in DeploymentChange, actor any) (Deployment, error) {
	var out Deployment
	if taskID <= 0 || in.ID < 0 || (in.ID > 0 && in.ExpectedRevision <= 0) {
		return out, deploymentInput("需要有效 task_id；更新必须提供 id 和 expected_revision。")
	}
	if err := operation.ValidateKey(in.RequestID); err != nil {
		return out, err
	}
	tx, err := d.BeginTx(ctx, nil)
	if err != nil {
		return out, err
	}
	defer tx.Rollback()
	// Match resource publication/deletion lock order, then serialize this task's
	// ledger and receipts. No external command executes in this transaction.
	if _, err = tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(7337741010)`); err != nil {
		return out, err
	}
	var found int64
	if err = tx.QueryRowContext(ctx, `SELECT id FROM tasks WHERE id=$1 AND (deleted_at IS NULL OR archived_at IS NOT NULL) FOR UPDATE`, taskID).Scan(&found); err != nil {
		if err == sql.ErrNoRows {
			err = &operation.Error{Code: "task_not_found", Message: "任务不存在或已删除，不能修改部署登记。", NextAction: "核对任务 ID；不要重建同名任务掩盖丢失的清理记录。"}
		}
		return out, err
	}
	scope := fmt.Sprintf("task-deployment:%d", taskID)
	if replay, e := operation.Replay(ctx, tx, scope, in.RequestID, in, &out); e != nil || replay {
		return out, e
	}
	if in.ID > 0 {
		out, err = scanDeployment(tx.QueryRowContext(ctx, `SELECT `+deploymentCols+` FROM task_deployments WHERE id=$1 AND task_id=$2 FOR UPDATE`, in.ID, taskID))
		if err != nil {
			if err == sql.ErrNoRows {
				err = &operation.Error{Code: "deployment_not_found", Message: "当前任务没有这条部署登记。", NextAction: "先用 manage_deployments(action=list) 核对部署 id。"}
			}
			return out, err
		}
		if out.Revision != in.ExpectedRevision {
			return out, &operation.Error{Code: "deployment_conflict", Message: "部署记录已变更。", CurrentRevision: out.Revision, NextAction: "重新读取当前记录，合并变更；使用新 request_id 和当前 expected_revision 提交。"}
		}
	} else {
		out = Deployment{TaskID: taskID, State: "active"}
	}
	linkChanged := in.ResourceID != nil && (out.ResourceID == nil || *in.ResourceID != *out.ResourceID)
	if in.ResourceID != nil {
		if *in.ResourceID == 0 {
			out.ResourceID = nil
		} else {
			out.ResourceID = in.ResourceID
		}
	}
	for _, field := range []struct {
		src *string
		dst *string
	}{{in.Name, &out.Name}, {in.Kind, &out.Kind}, {in.Location, &out.Location}, {in.Identifier, &out.Identifier}, {in.Cleanup, &out.Cleanup}, {in.State, &out.State}, {in.Verification, &out.Verification}, {in.Note, &out.Note}} {
		if field.src != nil {
			*field.dst = strings.TrimSpace(*field.src)
		}
	}
	for _, f := range []struct {
		name, value string
		max         int
	}{{"name", out.Name, 200}, {"kind", out.Kind, 80}, {"location", out.Location, 2000}, {"identifier", out.Identifier, 2000}, {"cleanup", out.Cleanup, 16000}} {
		if f.value == "" || len(f.value) > f.max {
			return out, deploymentInput(fmt.Sprintf("%s 必填且最长 %d 字节。", f.name, f.max))
		}
	}
	if len(out.Verification) > 16000 || len(out.Note) > 16000 {
		return out, deploymentInput("verification/note 最长16000字节。")
	}
	switch out.State {
	case "active", "removing", "unknown", "failed":
	case "removed", "retained":
		if out.Verification == "" {
			return out, deploymentInput("removed 需要实际撤除验证；retained 需要明确的保留依据，填写 verification。")
		}
	default:
		return out, deploymentInput("state 必须为 active/removing/removed/retained/unknown/failed。")
	}
	if in.State != nil && (out.State == "removed" || out.State == "retained") && in.Verification == nil {
		return out, deploymentInput("转入 removed/retained 时必须提供本次 verification，不能沿用旧结果。")
	}
	if out.ResourceID != nil && (in.ID == 0 || linkChanged) {
		var enabled, allowed bool
		err = tx.QueryRowContext(ctx, `SELECT enabled,access_mode='global' OR creator_task_id=$2 OR EXISTS(SELECT 1 FROM task_owned_resources WHERE task_id=$2 AND resource_id=$1) FROM owned_resources WHERE id=$1 FOR SHARE`, *out.ResourceID, taskID).Scan(&enabled, &allowed)
		if err != nil {
			return out, err
		}
		if !enabled || !allowed {
			return out, deploymentInput("关联资源不可用或当前任务未获授权。")
		}
	}
	out.Actor, err = json.Marshal(actor)
	if err != nil {
		return out, err
	}
	args := []any{taskID, out.ResourceID, out.Name, out.Kind, out.Location, out.Identifier, out.Cleanup, out.State, out.Verification, out.Note, string(out.Actor)}
	if in.ID == 0 {
		err = tx.QueryRowContext(ctx, `INSERT INTO task_deployments(task_id,resource_id,name,kind,location,identifier,cleanup,state,verification,note,actor) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11) RETURNING id,revision,created_at,updated_at`, args...).Scan(&out.ID, &out.Revision, &out.CreatedAt, &out.UpdatedAt)
	} else {
		args = append(args, in.ID)
		err = tx.QueryRowContext(ctx, `UPDATE task_deployments SET resource_id=$2,name=$3,kind=$4,location=$5,identifier=$6,cleanup=$7,state=$8,verification=$9,note=$10,actor=$11,revision=revision+1,updated_at=now() WHERE task_id=$1 AND id=$12 RETURNING revision,updated_at`, args...).Scan(&out.Revision, &out.UpdatedAt)
	}
	if err != nil {
		return out, err
	}
	raw, err := json.Marshal(out)
	if err != nil {
		return out, err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO task_deployment_events(task_id,deployment_id,revision,state,actor,snapshot) VALUES($1,$2,$3,$4,$5,$6)`, taskID, out.ID, out.Revision, out.State, string(out.Actor), string(raw)); err != nil {
		return out, err
	}
	if err = operation.Save(ctx, tx, scope, in.RequestID, in, out); err != nil {
		return out, err
	}
	return out, tx.Commit()
}

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
		{"cleanup_pending", `SELECT count(*) FROM task_deployments WHERE task_id=$1 AND state NOT IN ('removed','retained')`, "用 manage_deployments(list) 检查部署；执行获准撤除后记录实际验证，未知结果先核实。", []any{taskID}},
		{"deployments_retained", `SELECT count(*) FROM task_deployments WHERE task_id=$1 AND state='retained'`, "在交接中说明保留依据及后续负责人。", []any{taskID}},
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
