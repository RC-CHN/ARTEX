package server

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	"github.com/RC-CHN/ARTEX/runner"
)

const checkpointIdleTimeout = 5 * time.Second
const checkpointBatchBytes = 1024 * 1024
const checkpointBatchRows = 64

type runnerPGStore struct{ db *sql.DB }

// The broker further limits this by leases it has already delivered. Individual
// database operations must also make progress within checkpointIdleTimeout.
func (p runnerPGStore) CheckpointTimeout() time.Duration { return 2 * time.Minute }

func (p runnerPGStore) Guard(ctx context.Context, revision uint64, commit func() error) error {
	tx, err := p.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var current uint64
	if err = tx.QueryRowContext(ctx, "SELECT revision FROM runner_state WHERE id=1 FOR UPDATE").Scan(&current); err != nil {
		return err
	}
	if current != revision {
		return runner.ErrStoreConflict
	}
	if err = commit(); err != nil {
		return err
	}
	return tx.Commit()
}

func (p runnerPGStore) Load(ctx context.Context) (runner.Snapshot, error) {
	tx, err := p.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true})
	if err != nil {
		return runner.Snapshot{}, err
	}
	defer tx.Rollback()
	snap, err := loadRunnerCheckpoint(ctx, tx)
	if err == nil {
		err = tx.Commit()
	}
	return snap, err
}

func loadRunnerCheckpoint(ctx context.Context, tx *sql.Tx) (runner.Snapshot, error) {
	var s runner.Snapshot
	err := tx.QueryRowContext(ctx, "SELECT revision,payload FROM runner_state WHERE id=1").Scan(&s.Revision, &s.Data)
	if errors.Is(err, sql.ErrNoRows) {
		return runner.Snapshot{}, nil
	}
	if err != nil {
		return s, err
	}
	var format struct {
		Format string `json:"checkpoint_format"`
		Parts  int    `json:"parts"`
	}
	if err = json.Unmarshal(s.Data, &format); err != nil {
		return s, err
	}
	if format.Format == "" {
		return s, nil
	}
	if format.Format != "parts-v1" {
		return s, errors.New("unsupported runner checkpoint format")
	}
	rows, err := tx.QueryContext(ctx, "SELECT part_key,payload,digest FROM runner_state_parts WHERE checkpoint_id=1")
	if err != nil {
		return s, err
	}
	defer rows.Close()
	var parts []checkpointPart
	for rows.Next() {
		var p checkpointPart
		var digest []byte
		if err = rows.Scan(&p.key, &p.data, &digest); err != nil {
			return s, err
		}
		if len(digest) != 32 {
			return s, errors.New("invalid runner checkpoint digest")
		}
		copy(p.hash[:], digest)
		parts = append(parts, p)
	}
	if err = rows.Err(); err != nil {
		return s, err
	}
	if len(parts) != format.Parts || len(parts) == 0 {
		return s, errors.New("incomplete runner checkpoint")
	}
	s.Data, err = joinCheckpoint(parts)
	return s, err
}

func (p runnerPGStore) Save(ctx context.Context, revision uint64, data json.RawMessage) (uint64, error) {
	return p.SaveDiagnostic(ctx, revision, data, nil)
}
func (p runnerPGStore) SaveDiagnostic(ctx context.Context, revision uint64, data json.RawMessage, statuses []runner.SessionStatus) (uint64, error) {
	return p.saveDiagnostic(ctx, revision, data, statuses, checkpointIdleTimeout)
}

// Only successfully completed database steps refresh the inactivity budget.
// Lock waits, connection waits and a stuck COMMIT cannot keep it alive. The
// enclosing broker/caller deadline remains an absolute upper bound.
type checkpointProgress struct {
	mu      sync.Mutex
	last    time.Time
	idle    time.Duration
	timer   *time.Timer
	cancel  context.CancelCauseFunc
	stopped bool
}

func newCheckpointProgress(parent context.Context, idle time.Duration) (context.Context, *checkpointProgress) {
	ctx, cancel := context.WithCancelCause(parent)
	p := &checkpointProgress{last: time.Now(), idle: idle, cancel: cancel}
	p.timer = time.AfterFunc(idle, p.expire)
	return ctx, p
}
func (p *checkpointProgress) expire() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.stopped {
		return
	}
	remaining := p.idle - time.Since(p.last)
	if remaining <= 0 {
		p.stopped = true
		p.cancel(context.DeadlineExceeded)
	} else {
		p.timer.Reset(remaining)
	}
}
func (p *checkpointProgress) pulse() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.stopped {
		p.last = time.Now()
	}
}
func (p *checkpointProgress) stop() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.stopped = true
	p.timer.Stop()
	p.cancel(context.Canceled)
}

func (p runnerPGStore) saveDiagnostic(parent context.Context, revision uint64, data json.RawMessage, statuses []runner.SessionStatus, idle time.Duration) (next uint64, err error) {
	// Also bounded when used outside Hub (migration, tests and maintenance).
	parent, done := context.WithTimeout(parent, p.CheckpointTimeout())
	defer done()
	ctx := parent
	var progress *checkpointProgress
	defer func() {
		if progress != nil {
			progress.stop()
		}
	}()
	start, phase := time.Now(), "diff"
	written, deleted, writeBytes, batches, total := 0, 0, 0, 0, 0
	lastLog := start
	defer func() {
		if err != nil && ctx.Err() != nil {
			err = context.Cause(ctx)
		}
		if err != nil || time.Since(start) >= time.Second {
			classified := err
			if err != nil {
				classified = errors.Join(runner.ErrStoreUnavailable, err)
			}
			log.Printf("runner_store phase=%s code=%s sqlstate=%s duration_ms=%d bytes=%d revision=%d parts_total=%d parts_written=%d parts_deleted=%d bytes_written=%d batches=%d", phase, runner.FailureCode(classified), runner.SQLState(err), time.Since(start).Milliseconds(), len(data), revision, total, written, deleted, writeBytes, batches)
		}
	}()
	advance := func() {
		progress.pulse()
		if time.Since(lastLog) >= time.Second {
			log.Printf("runner_store progress=advanced phase=%s revision=%d parts_written=%d parts_deleted=%d bytes_written=%d batches=%d elapsed_ms=%d", phase, revision, written, deleted, writeBytes, batches, time.Since(start).Milliseconds())
			lastLog = time.Now()
		}
	}
	parts, err := splitCheckpoint(data)
	if err != nil {
		return 0, err
	}
	total = len(parts)
	marker := fmt.Sprintf(`{"version":-1,"checkpoint_format":"parts-v1","parts":%d}`, total)
	ctx, progress = newCheckpointProgress(parent, idle)
	phase = "begin"
	tx, err := p.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	advance()
	phase = "checkpoint_lock"
	if revision == 0 {
		err = tx.QueryRowContext(ctx, "INSERT INTO runner_state(id,revision,payload) VALUES(1,1,$1) ON CONFLICT DO NOTHING RETURNING revision", marker).Scan(&next)
	} else {
		err = tx.QueryRowContext(ctx, "UPDATE runner_state SET revision=revision+1,payload=$1 WHERE id=1 AND revision=$2 RETURNING revision", marker, revision).Scan(&next)
	}
	if errors.Is(err, sql.ErrNoRows) {
		err = runner.ErrStoreConflict
	}
	if err != nil {
		return 0, err
	}
	advance()
	phase = "manifest"
	rows, err := tx.QueryContext(ctx, "SELECT part_key,digest FROM runner_state_parts WHERE checkpoint_id=1")
	if err != nil {
		return 0, err
	}
	existing := map[string][]byte{}
	for rows.Next() {
		var key string
		var digest []byte
		if err = rows.Scan(&key, &digest); err != nil {
			rows.Close()
			return 0, err
		}
		existing[key] = digest
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return 0, err
	}
	advance()
	var changed []checkpointPart
	for _, part := range parts {
		if !bytes.Equal(existing[part.key], part.hash[:]) {
			changed = append(changed, part)
		}
		delete(existing, part.key)
	}
	phase = "write_parts"
	for len(changed) > 0 {
		count, size := 0, 0
		for count < len(changed) && count < checkpointBatchRows && (count == 0 || size+len(changed[count].data) <= checkpointBatchBytes) {
			size += len(changed[count].data)
			count++
		}
		var q strings.Builder
		q.WriteString("INSERT INTO runner_state_parts(checkpoint_id,part_key,payload,digest) VALUES ")
		args := make([]any, 0, count*3)
		for i, part := range changed[:count] {
			if i > 0 {
				q.WriteByte(',')
			}
			fmt.Fprintf(&q, "(1,$%d,$%d,$%d)", i*3+1, i*3+2, i*3+3)
			args = append(args, part.key, []byte(part.data), part.hash[:])
		}
		q.WriteString(" ON CONFLICT(checkpoint_id,part_key) DO UPDATE SET payload=EXCLUDED.payload,digest=EXCLUDED.digest")
		if _, err = tx.ExecContext(ctx, q.String(), args...); err != nil {
			return 0, err
		}
		changed = changed[count:]
		written += count
		writeBytes += size
		batches++
		advance()
	}
	phase = "delete_parts"
	var stale []string
	for key := range existing {
		stale = append(stale, key)
	}
	for len(stale) > 0 {
		count := min(len(stale), checkpointBatchRows)
		var q strings.Builder
		q.WriteString("DELETE FROM runner_state_parts WHERE checkpoint_id=1 AND part_key IN (")
		args := make([]any, count)
		for i, key := range stale[:count] {
			if i > 0 {
				q.WriteByte(',')
			}
			fmt.Fprintf(&q, "$%d", i+1)
			args[i] = key
		}
		q.WriteByte(')')
		if _, err = tx.ExecContext(ctx, q.String(), args...); err != nil {
			return 0, err
		}
		deleted += count
		stale = stale[count:]
		batches++
		advance()
	}
	if statuses != nil {
		phase = "diagnostics"
		projection, e := json.Marshal(statuses)
		if e != nil {
			return 0, e
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO runner_diagnostics(id,revision,sessions) VALUES(1,$1,$2) ON CONFLICT(id) DO UPDATE SET revision=EXCLUDED.revision,sessions=EXCLUDED.sessions,updated_at=now()`, next, string(projection))
		if err != nil {
			return 0, err
		}
		advance()
	}
	phase = "commit"
	err = tx.Commit()
	return next, err
}
