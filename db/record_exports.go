package db

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"sort"
	"time"

	"github.com/RC-CHN/ARTEX/internal/operation"
)

const MaxRecordExportBytes int64 = 20 << 30
const MaxRetainedRecordExportBytes int64 = 100 << 30

type RecordExport struct {
	ID        int64      `json:"id"`
	TaskIDs   []int64    `json:"task_ids"`
	Redacted  bool       `json:"redacted"`
	State     string     `json:"state"`
	Phase     string     `json:"phase"`
	Error     string     `json:"error"`
	Owner     string     `json:"-"`
	Attempt   int        `json:"attempt"`
	Files     int64      `json:"files"`
	Bytes     int64      `json:"bytes"`
	SHA256    string     `json:"sha256,omitempty"`
	CutoffAt  *time.Time `json:"cutoff_at,omitempty"`
	CreatedAt time.Time  `json:"created_at"`
	UpdatedAt time.Time  `json:"updated_at"`
	ExpiresAt time.Time  `json:"expires_at"`
}

const recordExportCols = `id,task_ids,redacted,state,phase,error,owner,attempt,files,bytes,sha256,cutoff_at,created_at,updated_at,expires_at`

func scanRecordExport(row interface{ Scan(...any) error }) (v RecordExport, err error) {
	var ids []byte
	err = row.Scan(&v.ID, &ids, &v.Redacted, &v.State, &v.Phase, &v.Error, &v.Owner, &v.Attempt, &v.Files, &v.Bytes, &v.SHA256, &v.CutoffAt, &v.CreatedAt, &v.UpdatedAt, &v.ExpiresAt)
	if err == nil {
		err = json.Unmarshal(ids, &v.TaskIDs)
	}
	return
}
func (d *DB) RecordExport(ctx context.Context, id int64) (RecordExport, error) {
	v, err := scanRecordExport(d.QueryRowContext(ctx, `SELECT `+recordExportCols+` FROM task_record_exports WHERE id=$1`, id))
	if err == sql.ErrNoRows {
		err = exportInput("export_not_found", "没有找到该导出任务。", "使用导出 create 回执的 id，或先 list 查询；不要使用原任务 ID。")
	}
	return v, err
}
func (d *DB) RecordExports(ctx context.Context, taskID, before int64) ([]RecordExport, error) {
	rows, err := d.QueryContext(ctx, `SELECT `+recordExportCols+` FROM task_record_exports WHERE ($1::bigint=0 OR task_ids @> jsonb_build_array($1::bigint)) AND ($2::bigint=0 OR id<$2) AND state<>'deleted' ORDER BY id DESC LIMIT 50`, taskID, before)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []RecordExport{}
	for rows.Next() {
		v, e := scanRecordExport(rows)
		if e != nil {
			return nil, e
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

type RecordExportRequest struct {
	TaskIDs   []int64 `json:"task_ids"`
	Redacted  bool    `json:"redacted"`
	RequestID string  `json:"request_id"`
}

func exportInput(code, message, next string) error {
	return &operation.Error{Code: code, Message: message, NextAction: next}
}
func (d *DB) QueueRecordExport(ctx context.Context, in RecordExportRequest, actorTaskID int64) (RecordExport, error) {
	var out RecordExport
	if err := operation.ValidateKey(in.RequestID); err != nil {
		return out, err
	}
	in.TaskIDs = append([]int64(nil), in.TaskIDs...)
	sort.Slice(in.TaskIDs, func(i, j int) bool { return in.TaskIDs[i] < in.TaskIDs[j] })
	if len(in.TaskIDs) == 0 || len(in.TaskIDs) > 20 {
		return out, exportInput("invalid_export_scope", "一次选择1–20个任务。", "明确列出任务 ID，不会自动加入关联任务。")
	}
	for i, id := range in.TaskIDs {
		if id <= 0 || i > 0 && id == in.TaskIDs[i-1] || actorTaskID > 0 && (len(in.TaskIDs) != 1 || id != actorTaskID) {
			return out, exportInput("invalid_export_scope", "任务 ID 必须为不重复的正整数；Agent 只能导出当前任务。", "多任务导出由人类在界面选择精确范围。")
		}
	}
	tx, err := d.BeginTx(ctx, nil)
	if err != nil {
		return out, err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(7337741012)`); err != nil {
		return out, err
	}
	scope := fmt.Sprintf("record-export:%d", actorTaskID)
	if replay, e := operation.Replay(ctx, tx, scope, in.RequestID, in, &out); e != nil {
		return out, e
	} else if replay {
		return scanRecordExport(tx.QueryRowContext(ctx, `SELECT `+recordExportCols+` FROM task_record_exports WHERE id=$1`, out.ID))
	}
	var pending int
	var retained int64
	if err = tx.QueryRowContext(ctx, `SELECT count(*) FILTER(WHERE state IN ('queued','running')),COALESCE(sum(bytes) FILTER(WHERE state='ready'),0) FROM task_record_exports`).Scan(&pending, &retained); err != nil {
		return out, err
	}
	if pending >= 10 || retained >= MaxRetainedRecordExportBytes {
		return out, exportInput("export_capacity", "导出队列或保留包达到上限。", "最多10个排队/生成任务、100 GiB 已完成包；等待完成或仅删除不再使用的导出副本。")
	}
	for _, id := range in.TaskIDs {
		var found int64
		if err = tx.QueryRowContext(ctx, `SELECT id FROM tasks WHERE id=$1 AND deleted_at IS NULL AND archived_at IS NULL FOR SHARE`, id).Scan(&found); err != nil {
			return out, exportInput("export_task_unavailable", fmt.Sprintf("任务 #%d 不存在、已删除或已冷归档。", id), "核对选择；冷归档任务须先恢复，再生成记录导出。")
		}
	}
	raw, _ := json.Marshal(in.TaskIDs)
	out, err = scanRecordExport(tx.QueryRowContext(ctx, `INSERT INTO task_record_exports(task_ids,redacted) VALUES($1,$2) RETURNING `+recordExportCols, string(raw), in.Redacted))
	if err != nil {
		return out, err
	}
	if err = operation.Save(ctx, tx, scope, in.RequestID, in, out); err != nil {
		return out, err
	}
	return out, tx.Commit()
}

// A unique attempt owner fences stale workers after restart/lease expiry.
func (d *DB) ClaimRecordExport(ctx context.Context, owner string) (RecordExport, error) {
	return scanRecordExport(d.QueryRowContext(ctx, `WITH candidate AS(SELECT id FROM task_record_exports WHERE state='queued' OR (state='running' AND lease_until<now()) ORDER BY id FOR UPDATE SKIP LOCKED LIMIT 1) UPDATE task_record_exports j SET state='running',phase='snapshot',owner=$1,lease_until=now()+interval '60 seconds',attempt=attempt+1,files=0,bytes=0,error='',sha256='',cutoff_at=NULL,updated_at=now() FROM candidate c WHERE j.id=c.id RETURNING `+prefixRecordExportCols("j"), owner))
}
func prefixRecordExportCols(alias string) string {
	cols := []string{"id", "task_ids", "redacted", "state", "phase", "error", "owner", "attempt", "files", "bytes", "sha256", "cutoff_at", "created_at", "updated_at", "expires_at"}
	out := ""
	for i, c := range cols {
		if i > 0 {
			out += ","
		}
		out += alias + "." + c
	}
	return out
}
func (d *DB) HeartbeatRecordExport(ctx context.Context, id int64, owner string) error {
	r, err := d.ExecContext(ctx, `UPDATE task_record_exports SET lease_until=now()+interval '60 seconds' WHERE id=$1 AND owner=$2 AND state='running' AND lease_until>=now()`, id, owner)
	if err != nil {
		return err
	}
	n, err := r.RowsAffected()
	if err == nil && n != 1 {
		err = sql.ErrNoRows
	}
	return err
}
func (d *DB) ProgressRecordExport(ctx context.Context, id int64, owner, phase string, files, bytes int64) error {
	r, err := d.ExecContext(ctx, `UPDATE task_record_exports SET phase=$3,files=$4,bytes=$5,updated_at=now() WHERE id=$1 AND owner=$2 AND state='running' AND lease_until>=now()`, id, owner, phase, files, bytes)
	if err != nil {
		return err
	}
	n, err := r.RowsAffected()
	if err == nil && n != 1 {
		err = sql.ErrNoRows
	}
	return err
}
func (d *DB) FinishRecordExport(ctx context.Context, id int64, owner, checksum string, cutoff time.Time, files, bytes int64) error {
	tx, err := d.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(7337741012)`); err != nil {
		return err
	}
	var retained int64
	if err = tx.QueryRowContext(ctx, `SELECT COALESCE(sum(bytes),0) FROM task_record_exports WHERE state='ready'`).Scan(&retained); err != nil {
		return err
	}
	if retained+bytes > MaxRetainedRecordExportBytes {
		return exportInput("export_capacity", "已完成导出包达到100 GiB上限。", "删除不再使用的导出副本后重试；原始记录不受影响。")
	}
	r, err := tx.ExecContext(ctx, `UPDATE task_record_exports SET state='ready',phase='ready',sha256=$3,cutoff_at=$4,files=$5,bytes=$6,expires_at=now()+interval '7 days',updated_at=now() WHERE id=$1 AND owner=$2 AND state='running' AND lease_until>=now()`, id, owner, checksum, cutoff, files, bytes)
	if err != nil {
		return err
	}
	n, err := r.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return sql.ErrNoRows
	}
	return tx.Commit()
}
func (d *DB) FailRecordExport(ctx context.Context, id int64, owner, message string) error {
	_, err := d.ExecContext(ctx, `UPDATE task_record_exports SET state='failed',phase='failed',error=$3,updated_at=now() WHERE id=$1 AND owner=$2 AND state='running'`, id, owner, message)
	return err
}
func (d *DB) ChangeRecordExport(ctx context.Context, id int64, action string) (RecordExport, error) {
	if action == "retry" {
		return d.retryRecordExport(ctx, id)
	}
	var q string
	switch action {
	case "cancel":
		q = `UPDATE task_record_exports SET state='canceled',phase='canceled',updated_at=now() WHERE id=$1 AND state IN ('queued','running','canceled') RETURNING `
	case "delete":
		q = `UPDATE task_record_exports SET state='deleting',phase='deleting',updated_at=now() WHERE id=$1 AND state IN ('ready','failed','canceled','deleting') RETURNING `
	default:
		return RecordExport{}, exportInput("invalid_export_action", "未知导出操作。", "使用 cancel/retry/delete。")
	}
	v, err := scanRecordExport(d.QueryRowContext(ctx, q+recordExportCols, id))
	if err == sql.ErrNoRows {
		err = exportInput("export_state_conflict", "导出状态已变化，当前操作不适用。", "重新查询状态；生成中先取消，完成包可下载或删除副本。")
	}
	return v, err
}

func (d *DB) retryRecordExport(ctx context.Context, id int64) (RecordExport, error) {
	tx, err := d.BeginTx(ctx, nil)
	if err != nil {
		return RecordExport{}, err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(7337741012)`); err != nil {
		return RecordExport{}, err
	}
	current, err := scanRecordExport(tx.QueryRowContext(ctx, `SELECT `+recordExportCols+` FROM task_record_exports WHERE id=$1 FOR UPDATE`, id))
	if err != nil {
		return current, err
	}
	if current.State == "queued" || current.State == "running" {
		return current, nil
	}
	if current.State != "failed" && current.State != "canceled" {
		return current, exportInput("export_state_conflict", "当前导出状态不支持重试。", "先查询状态；已完成包可下载，要新快照请创建新导出。")
	}
	var pending int
	var retained int64
	if err = tx.QueryRowContext(ctx, `SELECT count(*) FILTER(WHERE state IN ('queued','running')),COALESCE(sum(bytes) FILTER(WHERE state='ready'),0) FROM task_record_exports`).Scan(&pending, &retained); err != nil {
		return current, err
	}
	if pending >= 10 || retained >= MaxRetainedRecordExportBytes {
		return current, exportInput("export_capacity", "导出队列或保留包达到上限。", "最多10个排队/生成任务、100 GiB 已完成包；等待完成或仅删除不再使用的导出副本。")
	}
	current, err = scanRecordExport(tx.QueryRowContext(ctx, `UPDATE task_record_exports SET state='queued',phase='queued',error='',owner='',lease_until=NULL,updated_at=now() WHERE id=$1 RETURNING `+recordExportCols, id))
	if err != nil {
		return current, err
	}
	return current, tx.Commit()
}
