package server

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/RC-CHN/ARTEX/runner"
)

// SaveMetadata shares the checkpoint revision/fencing transaction. No manifest
// scan, history payload or fragment-count change is needed for a lease renewal.
func (p runnerPGStore) SaveMetadata(parent context.Context, revision uint64, parts map[string]json.RawMessage, statuses []runner.SessionStatus) (next uint64, err error) {
	parent, stop := context.WithTimeout(parent, p.CheckpointTimeout())
	defer stop()
	ctx, progress := newCheckpointProgress(parent, checkpointIdleTimeout)
	defer progress.stop()
	tx, err := p.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	progress.pulse()
	err = tx.QueryRowContext(ctx, "UPDATE runner_state SET revision=revision+1 WHERE id=1 AND revision=$1 RETURNING revision", revision).Scan(&next)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, runner.ErrStoreConflict
	}
	if err != nil {
		return 0, err
	}
	progress.pulse()
	keys := make([]string, 0, len(parts))
	for key, raw := range parts {
		// Only one metadata chunk; callers cannot introduce roots or receipts.
		if !strings.HasSuffix(key, "/0") || len(raw) > checkpointBatchBytes || !json.Valid(raw) {
			return 0, errors.New("invalid metadata fragment")
		}
		var path []string
		if json.Unmarshal([]byte(strings.TrimSuffix(key, "/0")), &path) != nil || len(path) != 2 || (path[0] != "nodes" && path[0] != "sessions") {
			return 0, errors.New("invalid metadata path")
		}
		keys = append(keys, key)
	}
	for len(keys) > 0 {
		count, size := 0, 0
		for count < len(keys) && count < checkpointBatchRows && (count == 0 || size+len(parts[keys[count]]) <= checkpointBatchBytes) {
			size += len(parts[keys[count]])
			count++
		}
		var q strings.Builder
		q.WriteString("UPDATE runner_state_parts AS p SET payload=v.payload,digest=v.digest FROM (VALUES ")
		args := make([]any, 0, count*3)
		for i, key := range keys[:count] {
			if i > 0 {
				q.WriteByte(',')
			}
			fmt.Fprintf(&q, "($%d::text,$%d::bytea,$%d::bytea)", 3*i+1, 3*i+2, 3*i+3)
			hash := sha256.Sum256(parts[key])
			args = append(args, key, []byte(parts[key]), hash[:])
		}
		q.WriteString(") AS v(part_key,payload,digest) WHERE p.checkpoint_id=1 AND p.part_key=v.part_key AND NOT EXISTS (SELECT 1 FROM runner_state_parts AS extra WHERE extra.checkpoint_id=1 AND extra.part_key=left(v.part_key,length(v.part_key)-1)||'1')")
		r, e := tx.ExecContext(ctx, q.String(), args...)
		if e != nil {
			return 0, e
		}
		n, e := r.RowsAffected()
		if e != nil {
			return 0, e
		}
		if n != int64(count) {
			return 0, errors.New("missing metadata fragment")
		}
		progress.pulse()
		keys = keys[count:]
	}
	if statuses != nil {
		raw, e := json.Marshal(statuses)
		if e != nil {
			return 0, e
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO runner_diagnostics(id,revision,sessions) VALUES(1,$1,$2) ON CONFLICT(id) DO UPDATE SET revision=EXCLUDED.revision,sessions=EXCLUDED.sessions,updated_at=now()`, next, string(raw))
		if err != nil {
			return 0, err
		}
		progress.pulse()
	}
	err = tx.Commit()
	return next, err
}
