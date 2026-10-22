package db

import (
	"time"

	"github.com/RC-CHN/ARTEX/norma/runtimeobs"
)

// StructureNodes supplies only the fields used for graph reachability. Large
// evidence/payload text stays in PostgreSQL until a consumer actually needs it.
func (s *ExplorationStore) StructureNodes() (out []*Node, err error) {
	start := time.Now()
	defer func() { runtimeobs.Record(runtimeobs.GraphStructure, start, err != nil) }()
	rows, err := s.db.Query(`SELECT id,kind,state FROM exploration_nodes WHERE exploration_id=$1 ORDER BY id`, s.expID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var nodes []*Node
	for rows.Next() {
		n := new(Node)
		if err := rows.Scan(&n.ID, &n.Kind, &n.State); err != nil {
			return nil, err
		}
		nodes = append(nodes, n)
	}
	return nodes, rows.Err()
}
