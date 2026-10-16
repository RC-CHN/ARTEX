package db

import (
	"database/sql"
	"errors"
	"fmt"
	"sort"
)

var ErrTaskResourceInvalid = errors.New("selected resource unavailable, unpublished, duplicated or over task quota")

type TaskResourceSelection struct {
	ResourceID int64 `json:"resource_id"`
	VersionID  int64 `json:"version_id"`
}

// Initial selection is an owner grant, committed together with task creation.
// Resolve latest once under the resource lock, never store a floating version.
func insertTaskResources(tx *sql.Tx, taskID int64, selections []TaskResourceSelection) error {
	if len(selections) > 32 {
		return ErrTaskResourceInvalid
	}
	ordered := append([]TaskResourceSelection(nil), selections...)
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].ResourceID < ordered[j].ResourceID })
	var total int64
	var previous int64
	for _, item := range ordered {
		if item.ResourceID <= 0 || item.ResourceID == previous || item.VersionID < 0 {
			return ErrTaskResourceInvalid
		}
		previous = item.ResourceID
		var enabled bool
		if e := tx.QueryRow(`SELECT enabled FROM owned_resources WHERE id=$1 FOR SHARE`, item.ResourceID).Scan(&enabled); e != nil {
			if errors.Is(e, sql.ErrNoRows) {
				return ErrTaskResourceInvalid
			}
			return e
		}
		if !enabled {
			return ErrTaskResourceInvalid
		}
		var version, size int64
		if e := tx.QueryRow(`SELECT id,size FROM owned_resource_versions WHERE resource_id=$1 AND ($2::bigint=0 OR id=$2) ORDER BY id DESC LIMIT 1`, item.ResourceID, item.VersionID).Scan(&version, &size); e != nil {
			if errors.Is(e, sql.ErrNoRows) {
				return ErrTaskResourceInvalid
			}
			return e
		}
		total += size
		if total > 1<<30 {
			return ErrTaskResourceInvalid
		}
		if _, e := tx.Exec(`INSERT INTO task_owned_resources(task_id,resource_id,version_id) VALUES($1,$2,$3)`, taskID, item.ResourceID, version); e != nil {
			return e
		}
		if _, e := tx.Exec(`INSERT INTO owned_resource_events(resource_id,version_id,kind,actor,detail) VALUES($1,$2,'task_bound','{}',$3)`, item.ResourceID, version, fmt.Sprintf("task:%d", taskID)); e != nil {
			return e
		}
	}
	return nil
}
