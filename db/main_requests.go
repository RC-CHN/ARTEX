package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// MainRequest is a durable consultation, scoped to a single local intent. A
// terminal intent/task cancels it in the same database transaction (see schema).
type MainRequest struct {
	ID           int64     `json:"id"`
	IntentID     int64     `json:"intent_id"`
	CallID       string    `json:"call_id"`
	Question     string    `json:"question"`
	Context      string    `json:"context"`
	State        string    `json:"state"`
	Answer       string    `json:"answer"`
	AnswerSource string    `json:"answer_source"`
	MainSeg      int       `json:"main_seg"`
	Attempts     int       `json:"attempts"`
	LastError    string    `json:"last_error"`
	CreatedAt    time.Time `json:"created_at"`
}

const mainRequestColumns = `id,intent_id,call_id,question,context,state,answer,answer_source,main_seg,attempts,last_error,created_at`

func scanMainRequest(row interface{ Scan(...any) error }) (*MainRequest, error) {
	r := new(MainRequest)
	err := row.Scan(&r.ID, &r.IntentID, &r.CallID, &r.Question, &r.Context, &r.State, &r.Answer, &r.AnswerSource, &r.MainSeg, &r.Attempts, &r.LastError, &r.CreatedAt)
	return r, err
}

// Lock order is task -> intent -> request everywhere that changes an intent.
// A task row may be absent for standalone exploration stores used by tools/tests.
func (s *ExplorationStore) lockConsultationTask(ctx context.Context, tx *sql.Tx) error {
	var status string
	var deleted bool
	err := tx.QueryRowContext(ctx, `SELECT status,deleted_at IS NOT NULL FROM tasks WHERE exploration_id=$1 FOR UPDATE`, s.expID).Scan(&status, &deleted)
	if errors.Is(err, sql.ErrNoRows) {
		// Standalone explorations still need a shared admission lock so concurrent
		// intents cannot each observe spare capacity and overflow the inbox.
		var id int64
		return tx.QueryRowContext(ctx, `SELECT id FROM explorations WHERE id=$1 FOR NO KEY UPDATE`, s.expID).Scan(&id)
	}
	if err != nil {
		return err
	}
	if deleted || status == "done" || status == "failed" || status == "timeout" {
		return ErrIntentStateConflict
	}
	return nil
}

func (s *ExplorationStore) CreateMainRequest(ctx context.Context, intentID int64, callID, question, evidence string) (*MainRequest, error) {
	callID, question, evidence = strings.TrimSpace(callID), strings.TrimSpace(question), strings.TrimSpace(evidence)
	if callID == "" || len(callID) > 256 || question == "" || len(question) > 8000 || len(evidence) > 24000 {
		return nil, fmt.Errorf("请示需要调用编号、1–8000 字节问题和不超过 24000 字节的上下文")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	if err = s.lockConsultationTask(ctx, tx); err != nil {
		return nil, err
	}
	var state string
	err = tx.QueryRowContext(ctx, `SELECT state FROM exploration_nodes WHERE id=$1 AND exploration_id=$2 AND kind='intent' FOR UPDATE`, intentID, s.expID).Scan(&state)
	if err != nil {
		return nil, err
	}
	r, err := scanMainRequest(tx.QueryRowContext(ctx, `SELECT `+mainRequestColumns+` FROM main_requests WHERE intent_id=$1 AND call_id=$2`, intentID, callID))
	if err == nil {
		if r.Question != utf8Clean(question) || r.Context != utf8Clean(evidence) {
			return nil, fmt.Errorf("调用编号已用于另一个问题")
		}
		return r, tx.Commit()
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	if state != "running" {
		return nil, ErrIntentStateConflict
	}
	var count int
	if err = tx.QueryRowContext(ctx, `SELECT count(*) FROM main_requests WHERE exploration_id=$1 AND state IN ('pending','processing','answered')`, s.expID).Scan(&count); err != nil {
		return nil, err
	}
	if count >= 32 {
		return nil, fmt.Errorf("本任务已有 32 条未完成请示，请先处理已有请求")
	}
	r, err = scanMainRequest(tx.QueryRowContext(ctx, `INSERT INTO main_requests(exploration_id,intent_id,call_id,question,context,main_seg)
VALUES($1,$2,$3,$4,$5,COALESCE((SELECT max(seq) FROM main_sessions WHERE exploration_id=$1),0)) RETURNING `+mainRequestColumns, s.expID, intentID, callID, utf8Clean(question), utf8Clean(evidence)))
	if err != nil {
		return nil, err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO activity(exploration_id,worker,kind,summary,detail,main_seg,metadata)
VALUES($1,'mainagent','consultation',$2,$3,$4,jsonb_build_object('request_id',$5::bigint,'intent_id',$6::bigint,'source','worker'))`, s.expID, fmt.Sprintf("Worker #%d 请示 #%d：%s", intentID, r.ID, question), question+"\n\n"+evidence, r.MainSeg, r.ID, intentID)
	if err != nil {
		return nil, err
	}
	return r, tx.Commit()
}

func (s *ExplorationStore) MainRequests(intentID int64, activeOnly bool) ([]*MainRequest, error) {
	return s.MainRequestsContext(context.Background(), intentID, activeOnly)
}

func (s *ExplorationStore) MainRequestsContext(ctx context.Context, intentID int64, activeOnly bool) ([]*MainRequest, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+mainRequestColumns+` FROM main_requests WHERE exploration_id=$1 AND ($2::bigint=0 OR intent_id=$2)
AND (NOT $3::boolean OR state IN ('pending','processing','answered')) ORDER BY CASE WHEN state IN ('pending','processing','answered') THEN 0 ELSE 1 END,id DESC LIMIT 100`, s.expID, intentID, activeOnly)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*MainRequest{}
	for rows.Next() {
		r, err := scanMainRequest(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func (s *ExplorationStore) ClaimMainRequest(ctx context.Context) (*MainRequest, error) {
	r, err := scanMainRequest(s.db.QueryRowContext(ctx, `UPDATE main_requests SET state='processing',attempts=attempts+1,updated_at=now()
WHERE id=(SELECT id FROM main_requests WHERE exploration_id=$1 AND ((state='pending' AND available_at<=now()) OR (state='processing' AND updated_at<now()-interval '11 minutes')) ORDER BY id LIMIT 1 FOR UPDATE SKIP LOCKED) RETURNING `+mainRequestColumns, s.expID))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return r, err
}

// AnswerMainRequest is idempotent for an identical response, never revives a
// paused/finished/cancelled intent, and serializes with cancellation.
func (s *ExplorationStore) AnswerMainRequest(ctx context.Context, id int64, answer, source string) error {
	return s.AnswerMainRequestAttempt(ctx, id, answer, source, 0)
}

// expectedAttempt fences a late automatic answer from a superseded run. Zero
// is reserved for an explicit human/main-chat answer to the current request.
func (s *ExplorationStore) AnswerMainRequestAttempt(ctx context.Context, id int64, answer, source string, expectedAttempt int) error {
	answer = strings.TrimSpace(answer)
	if answer == "" || len(answer) > 32000 {
		return fmt.Errorf("回复需要 1–32000 字节")
	}
	if source != "mainagent" && source != "human" && source != "system" {
		return fmt.Errorf("invalid answer source")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err = s.lockConsultationTask(ctx, tx); err != nil {
		return err
	}
	var intentID int64
	if err = tx.QueryRowContext(ctx, `SELECT intent_id FROM main_requests WHERE id=$1 AND exploration_id=$2`, id, s.expID).Scan(&intentID); err != nil {
		return err
	}
	var state string
	if err = tx.QueryRowContext(ctx, `SELECT state FROM exploration_nodes WHERE id=$1 FOR UPDATE`, intentID).Scan(&state); err != nil {
		return err
	}
	r, err := scanMainRequest(tx.QueryRowContext(ctx, `SELECT `+mainRequestColumns+` FROM main_requests WHERE id=$1 FOR UPDATE`, id))
	if err != nil {
		return err
	}
	if expectedAttempt > 0 && (r.Attempts != expectedAttempt || r.State == "pending") {
		return ErrIntentStateConflict
	}
	if (r.State == "answered" || r.State == "delivered") && r.Answer == answer && r.AnswerSource == source {
		return tx.Commit()
	}
	if (r.State != "pending" && r.State != "processing") || (state != "running" && state != "open" && state != "paused") {
		return ErrIntentStateConflict
	}
	if _, err = tx.ExecContext(ctx, `UPDATE main_requests SET state='answered',answer=$2,answer_source=$3,updated_at=now() WHERE id=$1`, id, utf8Clean(answer), source); err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO activity(exploration_id,worker,kind,summary,detail,main_seg,metadata)
VALUES($1,'mainagent','consultation_reply',$2,$3,$4,jsonb_build_object('request_id',$5::bigint,'intent_id',$6::bigint,'source',$7::text))`, s.expID, fmt.Sprintf("请示 #%d → Worker #%d：%s", id, intentID, answer), answer, r.MainSeg, id, intentID, source)
	if err != nil {
		return err
	}
	return tx.Commit()
}

func (s *ExplorationStore) AckMainReply(id int64) error {
	return s.AckMainReplyContext(context.Background(), id)
}

func (s *ExplorationStore) AckMainReplyContext(ctx context.Context, id int64) error {
	_, err := s.db.ExecContext(ctx, `UPDATE main_requests SET state='delivered',updated_at=now() WHERE exploration_id=$1 AND id=$2 AND state='answered'`, s.expID, id)
	return err
}

func (s *ExplorationStore) MainRequest(id int64) (*MainRequest, error) {
	return s.MainRequestContext(context.Background(), id)
}

func (s *ExplorationStore) MainRequestContext(ctx context.Context, id int64) (*MainRequest, error) {
	return scanMainRequest(s.db.QueryRowContext(ctx, `SELECT `+mainRequestColumns+` FROM main_requests WHERE exploration_id=$1 AND id=$2`, s.expID, id))
}

// MainRequestActivities returns the transactional notices for live broadcasting.
// Re-broadcasts keep their original IDs, so clients can safely deduplicate them.
func (s *ExplorationStore) MainRequestActivities(id int64) ([]Activity, error) {
	rows, err := s.db.Query(`SELECT id,kind,summary,detail,metadata,main_seg,created_at FROM activity WHERE exploration_id=$1 AND kind IN ('consultation','consultation_reply') AND metadata->>'request_id'=$2 ORDER BY id`, s.expID, fmt.Sprint(id))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Activity
	for rows.Next() {
		a := Activity{Worker: "mainagent"}
		if err := rows.Scan(&a.ID, &a.Kind, &a.Summary, &a.Detail, &a.Metadata, &a.MainSeg, &a.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

func (s *ExplorationStore) RetryMainRequest(ctx context.Context, id int64, attempt int, detail string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE main_requests SET state='pending',last_error=$3,available_at=now()+interval '15 seconds',updated_at=now() WHERE exploration_id=$1 AND id=$2 AND state='processing' AND attempts=$4`, s.expID, id, utf8Clean(detail), attempt)
	return err
}

// RecoverMainRequests is startup-only, before any new automatic main turn starts.
func (d *DB) RecoverMainRequests() error {
	_, err := d.Exec(`UPDATE main_requests SET state='pending',updated_at=now() WHERE state='processing'`)
	return err
}

func (d *DB) MainRequestTaskIDs(ctx context.Context, afterID int64) ([]int64, error) {
	rows, err := d.QueryContext(ctx, `SELECT DISTINCT t.id FROM tasks t JOIN main_requests r ON r.exploration_id=t.exploration_id WHERE ((r.state='pending' AND r.available_at<=now()) OR (r.state='processing' AND r.updated_at<now()-interval '11 minutes')) AND t.status NOT IN ('done','failed','timeout') AND NOT t.paused AND NOT t.queued AND t.deleted_at IS NULL AND t.archived_at IS NULL AND t.id>$1 ORDER BY t.id LIMIT 64`, afterID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []int64
	for rows.Next() {
		var id int64
		if err = rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}
