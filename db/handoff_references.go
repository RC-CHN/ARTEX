package db

import (
	"context"
	"encoding/json"
)

// HandoffReferences points to recorded evidence, without treating a receipt as
// proof of external success. The bounded ID lists are navigation aids only.
type HandoffReference struct {
	Kind       string  `json:"kind"`
	Count      int64   `json:"count"`
	IDs        []int64 `json:"recent_ids"`
	Truncated  bool    `json:"truncated"`
	NextAction string  `json:"next_action"`
}

func (d *DB) TaskHandoffReferences(ctx context.Context, taskID int64) ([]HandoffReference, error) {
	rows, err := d.QueryContext(ctx, `WITH scoped AS (SELECT exploration_id FROM tasks WHERE id=$1 AND deleted_at IS NULL AND archived_at IS NULL) SELECT k.kind,(SELECT count(*) FROM exploration_nodes n,scoped s WHERE n.exploration_id=s.exploration_id AND n.kind=k.kind),to_json(ARRAY(SELECT n.id FROM exploration_nodes n,scoped s WHERE n.exploration_id=s.exploration_id AND n.kind=k.kind ORDER BY n.id DESC LIMIT 20)) FROM (VALUES ('fact'),('finding'),('intent')) k(kind) ORDER BY k.kind`, taskID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []HandoffReference{}
	for rows.Next() {
		var r HandoffReference
		var raw []byte
		if err = rows.Scan(&r.Kind, &r.Count, &raw); err != nil {
			return nil, err
		}
		if err = json.Unmarshal(raw, &r.IDs); err != nil {
			return nil, err
		}
		r.Truncated = r.Count > int64(len(r.IDs))
		r.NextAction = "Use node_detail(id) to inspect recorded source and state; use paged list_findings for further findings. IDs do not prove external cleanup."
		out = append(out, r)
	}
	return out, rows.Err()
}
