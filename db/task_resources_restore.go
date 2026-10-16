package db

import (
	"database/sql"
	"encoding/json"
	"fmt"
)

// Restore references, not global files or credentials. Old archive formats have
// no selection; missing global versions produce a warning instead of silently
// choosing a different version.
func restoreTaskResources(tx *sql.Tx, taskID int64, raw json.RawMessage) ([]string, error) {
	rows, err := decodeArchiveRows(raw)
	if err != nil {
		return nil, err
	}
	if len(rows) > 32 {
		return nil, ErrTaskResourceInvalid
	}
	warnings := []string{}
	for _, row := range rows {
		resource, _ := jsonInt64(row["resource_id"])
		version, _ := jsonInt64(row["version_id"])
		result, err := tx.Exec(`INSERT INTO task_owned_resources(task_id,resource_id,version_id)
 SELECT $1,$2,$3 WHERE EXISTS(SELECT 1 FROM owned_resource_versions WHERE id=$3 AND resource_id=$2)
 ON CONFLICT(task_id,resource_id) DO NOTHING`, taskID, resource, version)
		if err != nil {
			return nil, err
		}
		n, err := result.RowsAffected()
		if err != nil {
			return nil, err
		}
		if n == 0 {
			warnings = append(warnings, fmt.Sprintf("资源 %d 的固定版本 %d 不可恢复，未自动选择其他版本", resource, version))
		}
	}
	var total int64
	if err := tx.QueryRow(`SELECT COALESCE(sum(v.size),0) FROM task_owned_resources b JOIN owned_resource_versions v ON v.id=b.version_id WHERE b.task_id=$1`, taskID).Scan(&total); err != nil {
		return nil, err
	}
	if total > 1<<30 {
		return nil, ErrTaskResourceInvalid
	}
	return warnings, nil
}
