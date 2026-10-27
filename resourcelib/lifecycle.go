package resourcelib

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	"github.com/RC-CHN/ARTEX/internal/operation"
)

type Reference struct {
	TaskID    int64  `json:"task_id"`
	VersionID int64  `json:"version_id,omitempty"`
	State     string `json:"state"`
	Blocking  bool   `json:"blocking"`
}
type InUse struct {
	References []Reference `json:"references"`
}

func (e *InUse) Error() string {
	return "resource_in_use: release active task references before deleting this resource"
}

type referenceQuery interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}

func references(ctx context.Context, q referenceQuery, id int64) ([]Reference, error) {
	rows, err := q.QueryContext(ctx, `SELECT b.task_id,b.version_id,t.status,
 (t.deleted_at IS NULL AND t.archived_at IS NULL AND (NOT t.paused AND t.status NOT IN ('done','failed','timeout')
 OR EXISTS(SELECT 1 FROM exploration_nodes n WHERE n.exploration_id=t.exploration_id AND n.kind='intent' AND n.state='running')
 OR EXISTS(SELECT 1 FROM runner_diagnostics d,jsonb_array_elements(d.sessions)e WHERE e->>'task_id'=t.id::text AND e->>'state' NOT IN ('closed','expired') AND (e->>'lease_until')::timestamptz>now())))
 FROM task_owned_resources b JOIN tasks t ON t.id=b.task_id WHERE b.resource_id=$1
 ORDER BY 1,2`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Reference{}
	for rows.Next() {
		var r Reference
		if err = rows.Scan(&r.TaskID, &r.VersionID, &r.State, &r.Blocking); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}
func (s *Store) References(ctx context.Context, id int64) ([]Reference, error) {
	return references(ctx, s.DB, id)
}

type DeleteRequest struct {
	ResourceID       int64  `json:"resource_id"`
	ExpectedRevision int64  `json:"expected_revision"`
	RequestID        string `json:"request_id"`
	Reason           string `json:"reason"`
}
type DeleteResult struct {
	Resource               Resource `json:"resource"`
	Deleted                bool     `json:"deleted"`
	RemoteCleanupPerformed bool     `json:"remote_cleanup_performed"`
	HistoryRetained        bool     `json:"history_retained"`
}

func (s *Store) Delete(ctx context.Context, in DeleteRequest, actor Actor) (DeleteResult, error) {
	var out DeleteResult
	if in.ResourceID <= 0 || in.ExpectedRevision <= 0 || strings.TrimSpace(in.Reason) == "" || len(in.Reason) > 4000 {
		return out, invalid("resource_id, expected_revision and a reason (1–4000 characters) are required")
	}
	if err := operation.ValidateKey(in.RequestID); err != nil {
		return out, err
	}
	tx, r, err := s.beginResource(ctx, actor, in.ResourceID, true)
	if err != nil {
		return out, err
	}
	defer tx.Rollback()
	scope := fmt.Sprintf("resource-delete:%d:%d", actor.TaskID, in.ResourceID)
	if replay, err := operation.Replay(ctx, tx, scope, in.RequestID, in, &out); err != nil || replay {
		return out, err
	}
	if r.Revision != in.ExpectedRevision {
		return out, &Conflict{CurrentRevision: r.Revision}
	}
	if r.DeletedAt != nil {
		return out, ErrUnavailable
	}
	refs, err := references(ctx, tx, in.ResourceID)
	if err != nil {
		return out, err
	}
	blocked := []Reference{}
	for _, ref := range refs {
		if ref.Blocking && (actor.TaskID == 0 || ref.TaskID != actor.TaskID) {
			blocked = append(blocked, ref)
		}
	}
	if len(blocked) > 0 {
		return out, &InUse{References: blocked}
	}
	err = tx.QueryRowContext(ctx, `UPDATE owned_resources SET deleted_at=now(),enabled=false,revision=revision+1,updated_at=now() WHERE id=$1 RETURNING revision,updated_at,deleted_at`, in.ResourceID).Scan(&r.Revision, &r.UpdatedAt, &r.DeletedAt)
	if err != nil {
		return out, err
	}
	r.Enabled = false
	// Drafts cannot be published after deletion; release their quota and secrets.
	if _, err = tx.ExecContext(ctx, `UPDATE owned_resource_drafts SET state='discarded',secret_cipher=NULL,revision=revision+1,updated_at=now() WHERE resource_id=$1 AND state='editing'`, in.ResourceID); err != nil {
		return out, err
	}
	if err = event(ctx, tx, in.ResourceID, 0, "deleted", in.Reason, actor); err != nil {
		return out, err
	}
	out = DeleteResult{Resource: r, Deleted: true, HistoryRetained: true}
	if err = operation.Save(ctx, tx, scope, in.RequestID, in, out); err != nil {
		return out, err
	}
	return out, tx.Commit()
}
