package resourcelib

import "context"

// UpdateState contains only identifiers, never documents, files or credentials.
type UpdateState struct {
	ResourceID int64 `json:"resource_id"`
	PinnedID   int64 `json:"pinned_version_id"`
	LatestID   int64 `json:"latest_version_id"`
	Enabled    bool  `json:"enabled"`
}

// UpdateStates queries the bounded set of task pins, not resource files/history.
// The version index is the durable source of truth, so restarts or lost network
// notifications cannot permanently lose a publication.
func (s *Store) UpdateStates(ctx context.Context, taskID int64) ([]UpdateState, error) {
	rows, err := s.DB.QueryContext(ctx, `SELECT b.resource_id,b.version_id,
 COALESCE((SELECT v.id FROM owned_resource_versions v WHERE v.resource_id=b.resource_id ORDER BY v.version DESC LIMIT 1),b.version_id),r.enabled
 FROM task_owned_resources b JOIN owned_resources r ON r.id=b.resource_id
 JOIN tasks t ON t.id=b.task_id WHERE b.task_id=$1 AND t.deleted_at IS NULL AND t.archived_at IS NULL
 ORDER BY b.resource_id LIMIT 32`, taskID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []UpdateState{}
	for rows.Next() {
		var state UpdateState
		if err = rows.Scan(&state.ResourceID, &state.PinnedID, &state.LatestID, &state.Enabled); err != nil {
			return nil, err
		}
		out = append(out, state)
	}
	return out, rows.Err()
}
