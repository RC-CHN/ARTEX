package resourcelib

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"time"
)

// Sweep removes old, unreferenced blobs left by discarded/replaced drafts or an
// uncertain commit. The publication lock prevents deleting a just-published blob.
// Maintenance is independent of execution startup and never scans task folders.
func (s *Store) Sweep(ctx context.Context, olderThan time.Time) (int, error) {
	tx, e := s.DB.BeginTx(ctx, nil)
	if e != nil {
		return 0, e
	}
	defer tx.Rollback()
	var locked bool
	if e = tx.QueryRowContext(ctx, `SELECT pg_try_advisory_xact_lock(7337741010)`).Scan(&locked); e != nil || !locked {
		return 0, e
	}
	keep := map[string]bool{}
	versions := map[string]bool{}
	rows, e := tx.QueryContext(ctx, `SELECT id,manifest,true FROM owned_resource_versions UNION ALL SELECT id,manifest,false FROM owned_resource_drafts WHERE state='editing'`)
	if e != nil {
		return 0, e
	}
	for rows.Next() {
		var id int64
		var raw []byte
		var version bool
		if e = rows.Scan(&id, &raw, &version); e != nil {
			rows.Close()
			return 0, e
		}
		var m Manifest
		if e = json.Unmarshal(raw, &m); e != nil {
			rows.Close()
			return 0, e
		}
		for _, f := range m.Files {
			keep[filepath.Base(blobName(f))] = true
		}
		if version {
			versions[filepath.Base(s.VersionPath(Version{ID: id}))] = true
		}
	}
	e = rows.Err()
	rows.Close()
	if e != nil {
		return 0, e
	}
	root, e := os.OpenRoot(s.Root)
	if errors.Is(e, os.ErrNotExist) {
		return 0, nil
	}
	if e != nil {
		return 0, e
	}
	defer root.Close()
	removed := 0
	for _, dir := range []string{"blobs", "versions", "staging"} {
		folder, err := root.Open(dir)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return removed, err
		}
		entries, err := folder.ReadDir(-1)
		folder.Close()
		if err != nil {
			return removed, err
		}
		for _, entry := range entries {
			if e = ctx.Err(); e != nil {
				return removed, e
			}
			if (dir == "blobs" && keep[entry.Name()]) || (dir == "versions" && versions[entry.Name()]) {
				continue
			}
			info, err := entry.Info()
			if err != nil {
				return removed, err
			}
			if !info.ModTime().Before(olderThan) {
				continue
			}
			p := filepath.Join(dir, entry.Name())
			if dir == "versions" {
				err = root.RemoveAll(p)
			} else {
				err = root.Remove(p)
			}
			if err != nil && !errors.Is(err, os.ErrNotExist) {
				return removed, err
			}
			removed++
		}
	}
	return removed, tx.Commit()
}
