package server

import (
	"context"
	"encoding/json"
	"fmt"
	"unicode/utf8"

	"github.com/RC-CHN/ARTEX/resourcelib"
)

func (s *Server) taskResourceSnapshot(ctx context.Context, task int64) (string, error) {
	if task <= 0 || s.m.pg == nil {
		return "", nil
	}
	rows, err := s.m.pg.QueryContext(ctx, `SELECT b.resource_id,v.id,v.version,v.manifest->'metadata' FROM task_owned_resources b JOIN owned_resource_versions v ON v.id=b.version_id WHERE b.task_id=$1 ORDER BY b.resource_id LIMIT 32`, task)
	if err != nil {
		return "", fmt.Errorf("load task resource snapshot: %w", err)
	}
	defer rows.Close()
	type item struct {
		ID          int64    `json:"resource_id"`
		VersionID   int64    `json:"version_id"`
		Version     int      `json:"version"`
		Name        string   `json:"name"`
		Description string   `json:"description"`
		Tags        []string `json:"tags"`
	}
	items := []item{}
	for rows.Next() {
		var v item
		var data []byte
		if err = rows.Scan(&v.ID, &v.VersionID, &v.Version, &data); err != nil {
			return "", err
		}
		var meta resourcelib.DocumentMeta
		if err = json.Unmarshal(data, &meta); err != nil {
			return "", err
		}
		v.Name = meta.Name
		v.Description = meta.Description
		if utf8.RuneCountInString(v.Description) > 300 {
			v.Description = string([]rune(v.Description)[:300]) + "…"
		}
		v.Tags = meta.Tags
		items = append(items, v)
	}
	if err = rows.Err(); err != nil {
		return "", err
	}
	if len(items) == 0 {
		return "", nil
	}
	data, err := json.Marshal(items)
	return string(data), err
}
