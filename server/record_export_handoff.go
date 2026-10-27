package server

import (
	"encoding/json"
	"path/filepath"

	"github.com/RC-CHN/ARTEX/recordexport"
)

type exportReference struct {
	Source    string  `json:"source"`
	Kind      string  `json:"kind"`
	State     string  `json:"state,omitempty"`
	Count     int64   `json:"count"`
	RecentIDs []int64 `json:"recent_ids"`
	Truncated bool    `json:"truncated"`
}

// Derive navigation solely from exported database rows, never a later live read.
func exportHandoff(b *recordexport.Builder, prefix string) error {
	refs := []*exportReference{}
	groups := map[string]*exportReference{}
	for _, table := range []string{"exploration_nodes", "main_requests", "findings", "traffic_evidence_snapshots"} {
		source := prefix + "/database/" + table + ".jsonl"
		if err := readExportRows(filepath.Join(b.Dir, source), func(raw json.RawMessage) error {
			var r struct {
				ID          int64
				Kind, State string
			}
			if err := json.Unmarshal(raw, &r); err != nil {
				return err
			}
			// Group only graph kinds and lifecycle states, never user labels.
			if table != "exploration_nodes" {
				r.Kind = ""
			}
			key := table + "/" + r.Kind + "/" + r.State
			g := groups[key]
			if g == nil {
				g = &exportReference{Source: source, Kind: r.Kind, State: r.State, RecentIDs: []int64{}}
				groups[key] = g
				refs = append(refs, g)
			}
			g.Count++
			g.RecentIDs = append(g.RecentIDs, r.ID)
			if len(g.RecentIDs) > 20 {
				copy(g.RecentIDs, g.RecentIDs[1:])
				g.RecentIDs = g.RecentIDs[:20]
				g.Truncated = true
			}
			return nil
		}); err != nil {
			return err
		}
	}
	return b.JSON(prefix+"/handoff.json", map[string]any{"scope": "exported_database_snapshot", "database_cutoff": b.Manifest.Cutoff, "references": refs, "external_cleanup_verified": false, "next_action": "Use source JSONL + record IDs to verify evidence, outcome and unresolved work. Recent IDs are bounded navigation; full records remain in source. Check manifest omissions before concluding completeness."})
}
