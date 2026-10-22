package db

import (
	"context"
	"encoding/json"
)

// InsertToolUsageBatch has an explicit lifetime; metering must never hold an
// execution or shutdown hostage to a database connection/lock wait.
func (d *DB) InsertToolUsageBatch(ctx context.Context, rows []ToolUsage) error {
	if len(rows) == 0 {
		return nil
	}
	b, err := json.Marshal(rows)
	if err != nil {
		return err
	}
	_, err = d.ExecContext(ctx, `INSERT INTO tool_usage(tool_key,agent_key,task_id,exploration_id,intent_id,session_id)
SELECT tool_key,NULLIF(agent_key,''),NULLIF(task_id,0),NULLIF(exploration_id,0),NULLIF(intent_id,0),NULLIF(session_id,'')
FROM jsonb_to_recordset($1::jsonb) AS x(tool_key text,agent_key text,task_id bigint,exploration_id bigint,intent_id bigint,session_id text)`, string(b))
	return err
}

// ToolUsage is one catalog tool invocation. It stores attribution dimensions only:
// tool arguments and results are deliberately excluded from the ledger.
type ToolUsage struct {
	ToolKey       string `json:"tool_key"`
	AgentKey      string `json:"agent_key"`
	TaskID        int64  `json:"task_id"`
	ExplorationID int64  `json:"exploration_id"`
	IntentID      int64  `json:"intent_id"`
	SessionID     string `json:"session_id"`
}

// InsertToolUsage appends one ledger row. Runtime callers treat metering as
// best-effort so a statistics failure never interrupts the tool itself.
func (d *DB) InsertToolUsage(u *ToolUsage) error {
	_, err := d.Exec(`
INSERT INTO tool_usage(tool_key, agent_key, task_id, exploration_id, intent_id, session_id)
VALUES ($1, NULLIF($2,''), $3, $4, $5, NULLIF($6,''))`,
		u.ToolKey, u.AgentKey, nullIfZero(u.TaskID), nullIfZero(u.ExplorationID),
		nullIfZero(u.IntentID), u.SessionID)
	return err
}

// ToolUsageCounts returns invocation totals keyed by catalog tool key. Entries
// without calls are absent; API callers merge the result into the tools catalog.
func (d *DB) ToolUsageCounts() (map[string]int, error) {
	rows, err := d.Query(`
SELECT tool_key, COUNT(*)
FROM tool_usage
GROUP BY tool_key`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]int{}
	for rows.Next() {
		var key string
		var calls int
		if err := rows.Scan(&key, &calls); err != nil {
			return nil, err
		}
		out[key] = calls
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	archived, err := d.archivedTaskAggregates()
	if err != nil {
		return nil, err
	}
	for _, aggregate := range archived {
		for key, calls := range aggregate.Tools {
			out[key] += calls
		}
	}
	return out, nil
}
