package server

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// RestoreLegacyRunnerCheckpoint is an offline downgrade operation. Stop both
// controller and manager first. Only the checkpoint representation changes;
// task progress is never restored from a stale backup. No content is returned.
func RestoreLegacyRunnerCheckpoint(ctx context.Context, dsn string) error {
	database, err := sql.Open("pgx", dsn)
	if err != nil {
		return err
	}
	defer database.Close()
	return restoreLegacyRunnerCheckpoint(ctx, database)
}

func restoreLegacyRunnerCheckpoint(ctx context.Context, database *sql.DB) error {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	tx, err := database.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err = restoreLegacyRunnerCheckpointTx(ctx, tx); err != nil {
		return err
	}
	return tx.Commit()
}

func restoreLegacyRunnerCheckpointTx(ctx context.Context, tx *sql.Tx) (err error) {
	if _, err = tx.ExecContext(ctx, `SET LOCAL lock_timeout='5s'; LOCK TABLE tasks,exploration_nodes,main_requests IN SHARE ROW EXCLUSIVE MODE; LOCK TABLE runner_state IN EXCLUSIVE MODE`); err != nil {
		return err
	}
	var busy bool
	if err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM tasks WHERE NOT paused AND status NOT IN ('done','failed','timeout') AND deleted_at IS NULL AND archived_at IS NULL)
 OR EXISTS(SELECT 1 FROM exploration_nodes WHERE kind='intent' AND state='running')
 OR EXISTS(SELECT 1 FROM main_requests WHERE state='processing')
 OR EXISTS(SELECT 1 FROM runner_diagnostics,jsonb_array_elements(sessions)s WHERE s->>'state' NOT IN ('closed','expired') AND (s->>'lease_until')::timestamptz>now())`).Scan(&busy); err != nil {
		return err
	}
	if busy {
		return errors.New("runner checkpoint downgrade requires an idle cluster")
	}
	snap, err := loadRunnerCheckpoint(ctx, tx)
	if err != nil {
		return err
	}
	if snap.Revision == 0 {
		return nil
	}
	if _, err = tx.ExecContext(ctx, "UPDATE runner_state SET revision=revision+1,payload=$1 WHERE id=1", string(snap.Data)); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, "UPDATE runner_diagnostics SET revision=$1,updated_at=now() WHERE id=1", snap.Revision+1); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, "DELETE FROM runner_state_parts WHERE checkpoint_id=1"); err != nil {
		return err
	}
	return nil
}
