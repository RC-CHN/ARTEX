package resourcelib

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"os"
	"sort"
	"strings"
)

const draftCols = `id,resource_id,base_version_id,revision,state,COALESCE(published_version_id,0),size,manifest,actor,updated_at`

func scanDraft(row scanner) (Draft, error) {
	var d Draft
	var m, a []byte
	err := row.Scan(&d.ID, &d.ResourceID, &d.BaseVersionID, &d.Revision, &d.State, &d.PublishedVersionID, &d.Size, &m, &a, &d.UpdatedAt)
	if err == nil {
		err = json.Unmarshal(m, &d.Manifest)
	}
	if err == nil {
		err = json.Unmarshal(a, &d.Actor)
	}
	return d, err
}
func (s *Store) Draft(ctx context.Context, id int64) (Draft, error) {
	return scanDraft(s.DB.QueryRowContext(ctx, `SELECT `+draftCols+` FROM owned_resource_drafts WHERE id=$1`, id))
}
func (s *Store) Drafts(ctx context.Context, id int64) ([]Draft, error) {
	rows, err := s.DB.QueryContext(ctx, `SELECT `+draftCols+` FROM owned_resource_drafts WHERE resource_id=$1 AND state='editing' ORDER BY id DESC LIMIT 16`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Draft{}
	for rows.Next() {
		d, e := scanDraft(rows)
		if e != nil {
			return nil, e
		}
		out = append(out, d)
	}
	return out, rows.Err()
}
func quota(ctx context.Context, tx *sql.Tx, extra int64, draftID int64, fileCount int) error {
	var used int64
	err := tx.QueryRowContext(ctx, `SELECT (SELECT COALESCE(sum(size),0) FROM owned_resource_versions)+(SELECT COALESCE(sum(size),0) FROM owned_resource_drafts WHERE state='editing')`).Scan(&used)
	if err != nil {
		return err
	}
	var files int64
	if err = tx.QueryRowContext(ctx, `SELECT (SELECT COALESCE(sum(jsonb_array_length(manifest->'files')),0) FROM owned_resource_versions)+(SELECT COALESCE(sum(jsonb_array_length(manifest->'files')),0) FROM owned_resource_drafts WHERE state='editing' AND id<>$1)`, draftID).Scan(&files); err != nil {
		return err
	}
	if files+int64(fileCount) > 100000 {
		return ErrLimit
	}
	if used+extra > MaxLibraryBytes {
		return ErrLimit
	}
	return nil
}
func (s *Store) NewDraft(ctx context.Context, id, base int64, a Actor) (Draft, error) {
	tx, r, err := s.begin(ctx, a, id)
	if err != nil {
		return Draft{}, err
	}
	defer tx.Rollback()
	if !r.Enabled {
		return Draft{}, ErrUnavailable
	}
	var count, total int
	if err = tx.QueryRowContext(ctx, `SELECT count(*) FILTER(WHERE resource_id=$1),count(*) FROM owned_resource_drafts WHERE state='editing'`, id).Scan(&count, &total); err != nil {
		return Draft{}, err
	}
	if count >= 16 || total >= 256 {
		return Draft{}, ErrLimit
	}
	v, err := scanVersion(tx.QueryRowContext(ctx, `SELECT `+versionCols+` FROM owned_resource_versions WHERE resource_id=$1 ORDER BY id DESC LIMIT 1`, id))
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return Draft{}, err
	}
	if base != 0 && base != v.ID {
		return Draft{}, &Conflict{CurrentVersionID: v.ID}
	}
	m := Manifest{Files: []File{}, Endpoints: []Endpoint{}}
	var secret []byte
	if v.ID != 0 {
		m = v.Manifest
		if err = tx.QueryRowContext(ctx, `SELECT secret_cipher FROM owned_resource_versions WHERE id=$1`, v.ID).Scan(&secret); err != nil {
			return Draft{}, err
		}
	}
	if v.ID == 0 {
		staged, file, e := s.stage(ctx, bytes.NewReader(initialDocument(r)))
		if e != nil {
			return Draft{}, e
		}
		defer os.Remove(staged)
		file.Path = DocumentName
		if e = s.commitBlob(staged, file); e != nil {
			return Draft{}, e
		}
		m.Files = []File{file}
		m.Metadata, _ = ParseDocument(initialDocument(r))
		v.Size = file.Size
	}
	if err = quota(ctx, tx, v.Size, 0, len(m.Files)); err != nil {
		return Draft{}, err
	}
	d, err := scanDraft(tx.QueryRowContext(ctx, `INSERT INTO owned_resource_drafts(resource_id,base_version_id,size,manifest,secret_cipher,actor) VALUES($1,$2,$3,$4,$5,$6) RETURNING `+draftCols, id, v.ID, v.Size, marshal(m), secret, marshal(a)))
	if err != nil {
		return d, err
	}
	if err = event(ctx, tx, id, v.ID, "draft_created", "", a); err != nil {
		return d, err
	}
	return d, tx.Commit()
}
func (s *Store) draftTx(ctx context.Context, id, revision int64, a Actor) (*sql.Tx, Draft, error) {
	d, err := s.Draft(ctx, id)
	if err != nil {
		return nil, d, err
	}
	tx, r, err := s.begin(ctx, a, d.ResourceID)
	if err != nil {
		return nil, d, err
	}
	d, err = scanDraft(tx.QueryRowContext(ctx, `SELECT `+draftCols+` FROM owned_resource_drafts WHERE id=$1 FOR UPDATE`, id))
	if err == nil && !r.Enabled {
		err = ErrUnavailable
	}
	if err == nil && (d.Revision != revision || d.State != "editing") {
		err = &Conflict{CurrentRevision: d.Revision, CurrentVersionID: d.PublishedVersionID}
	}
	if err != nil {
		tx.Rollback()
		return nil, d, err
	}
	return tx, d, nil
}
func saveDraft(ctx context.Context, tx *sql.Tx, d *Draft, a Actor) error {
	size, err := validateManifest(d.Manifest)
	if err != nil {
		return err
	}
	if err = quota(ctx, tx, size-d.Size, d.ID, len(d.Files)); err != nil {
		return err
	}
	d.Size = size
	sort.Slice(d.Files, func(i, j int) bool { return d.Files[i].Path < d.Files[j].Path })
	d.Actor = a
	return tx.QueryRowContext(ctx, `UPDATE owned_resource_drafts SET manifest=$2,size=$3,actor=$4,revision=revision+1,updated_at=now() WHERE id=$1 RETURNING revision,updated_at`, d.ID, marshal(d.Manifest), size, marshal(a)).Scan(&d.Revision, &d.UpdatedAt)
}
func (s *Store) Upload(ctx context.Context, id, revision int64, p string, executable bool, reader io.Reader, a Actor) (Draft, error) {
	if err := ValidatePath(p); err != nil {
		return Draft{}, err
	}
	// Check access/revision before reading a potentially large input, then recheck
	// under lock after streaming. Concurrent edits cannot silently replace a file.
	pre, _, err := s.draftTx(ctx, id, revision, a)
	if err != nil {
		return Draft{}, err
	}
	pre.Rollback()
	staged, f, err := s.stage(ctx, reader)
	if err != nil {
		return Draft{}, err
	}
	defer os.Remove(staged)
	if p == DocumentName {
		if f.Size > MaxDocumentBytes {
			return Draft{}, ErrLimit
		}
		content, e := os.ReadFile(staged)
		if e != nil {
			return Draft{}, e
		}
		if _, e = ParseDocument(content); e != nil {
			return Draft{}, e
		}
		executable = false
	}
	f.Path = p
	f.Executable = executable
	tx, d, err := s.draftTx(ctx, id, revision, a)
	if err != nil {
		return d, err
	}
	defer tx.Rollback()
	if p == DocumentName {
		content, e := os.ReadFile(staged)
		if e != nil {
			return d, e
		}
		d.Metadata, e = ParseDocument(content)
		if e != nil {
			return d, e
		}
	}
	replaced := false
	for i := range d.Files {
		if d.Files[i].Path == p {
			d.Files[i] = f
			replaced = true
			break
		}
	}
	if !replaced {
		d.Files = append(d.Files, f)
	}
	if err = saveDraft(ctx, tx, &d, a); err != nil {
		return d, err
	}
	if err = s.commitBlob(staged, f); err != nil {
		return d, err
	}
	if err = event(ctx, tx, d.ResourceID, 0, "draft_file_updated", p, a); err != nil {
		return d, err
	}
	return d, tx.Commit()
}
func (s *Store) RemoveFile(ctx context.Context, id, revision int64, p string, a Actor) (Draft, error) {
	if p == DocumentName {
		return Draft{}, invalid("RESOURCE.md is required; edit it instead of deleting it")
	}
	tx, d, err := s.draftTx(ctx, id, revision, a)
	if err != nil {
		return d, err
	}
	defer tx.Rollback()
	files := []File{}
	found := false
	for _, f := range d.Files {
		if f.Path != p {
			files = append(files, f)
		} else {
			found = true
		}
	}
	if !found {
		return d, sql.ErrNoRows
	}
	d.Files = files
	if err = saveDraft(ctx, tx, &d, a); err != nil {
		return d, err
	}
	if err = event(ctx, tx, d.ResourceID, 0, "draft_file_removed", p, a); err != nil {
		return d, err
	}
	return d, tx.Commit()
}

// Configure replaces endpoints/platform metadata, preserving credentials for
// unchanged endpoint names. An explicit empty secret clears that credential.
func (s *Store) Configure(ctx context.Context, id, revision int64, endpoints []Endpoint, osName, arch string, secrets map[string]string, a Actor) (Draft, error) {
	tx, d, err := s.draftTx(ctx, id, revision, a)
	if err != nil {
		return d, err
	}
	defer tx.Rollback()
	var raw []byte
	if err = tx.QueryRowContext(ctx, `SELECT secret_cipher FROM owned_resource_drafts WHERE id=$1`, id).Scan(&raw); err != nil {
		return d, err
	}
	old, err := s.unseal(raw, d.ResourceID)
	if err != nil {
		return d, err
	}
	next := map[string]string{}
	names := map[string]bool{}
	for i := range endpoints {
		e := &endpoints[i]
		names[e.Name] = true
		value := old[e.Name]
		if v, ok := secrets[e.Name]; ok {
			value = v
		}
		if len(value) > 65536 {
			return d, ErrLimit
		}
		if value != "" {
			next[e.Name] = value
		}
		e.HasCredential = value != ""
	}
	for name := range secrets {
		if !names[name] {
			return d, invalid("credential refers to a missing endpoint")
		}
	}
	d.Endpoints = endpoints
	if d.Endpoints == nil {
		d.Endpoints = []Endpoint{}
	}
	d.OS = osName
	d.Arch = arch
	if err = saveDraft(ctx, tx, &d, a); err != nil {
		return d, err
	}
	raw, err = s.seal(next, d.ResourceID)
	if err != nil {
		return d, err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE owned_resource_drafts SET secret_cipher=$2 WHERE id=$1`, id, raw); err != nil {
		return d, err
	}
	if err = event(ctx, tx, d.ResourceID, 0, "draft_configuration_updated", "", a); err != nil {
		return d, err
	}
	return d, tx.Commit()
}
func (s *Store) Discard(ctx context.Context, id, revision int64, a Actor) error {
	tx, d, err := s.draftTx(ctx, id, revision, a)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, `UPDATE owned_resource_drafts SET state='discarded',secret_cipher=NULL,manifest='{"files":[],"endpoints":[]}',size=0,revision=revision+1,updated_at=now() WHERE id=$1`, id); err != nil {
		return err
	}
	if err = event(ctx, tx, d.ResourceID, 0, "draft_discarded", "", a); err != nil {
		return err
	}
	return tx.Commit()
}
func (s *Store) Publish(ctx context.Context, id, revision int64, note, validation string, a Actor) (Version, error) {
	if strings.TrimSpace(note) == "" || strings.TrimSpace(validation) == "" || len(note) > 4000 || len(validation) > 8000 {
		return Version{}, invalid("change note and validation required (max 4000/8000 bytes)")
	}
	d, err := s.Draft(ctx, id)
	if err != nil {
		return Version{}, err
	}
	tx, r, err := s.begin(ctx, a, d.ResourceID)
	if err != nil {
		return Version{}, err
	}
	defer tx.Rollback()
	if !r.Enabled {
		return Version{}, ErrUnavailable
	}
	d, err = scanDraft(tx.QueryRowContext(ctx, `SELECT `+draftCols+` FROM owned_resource_drafts WHERE id=$1 FOR UPDATE`, id))
	if err != nil {
		return Version{}, err
	}
	if d.State == "published" && d.Revision == revision+1 {
		v, e := scanVersion(tx.QueryRowContext(ctx, `SELECT `+versionCols+` FROM owned_resource_versions WHERE id=$1`, d.PublishedVersionID))
		return v, e
	}
	var versionCount int
	if err = tx.QueryRowContext(ctx, `SELECT count(*) FROM owned_resource_versions`).Scan(&versionCount); err != nil {
		return Version{}, err
	}
	if versionCount >= 10000 {
		return Version{}, ErrLimit
	}
	var latest int64
	var number int
	if err = tx.QueryRowContext(ctx, `SELECT COALESCE(max(id),0),COALESCE(max(version),0)+1 FROM owned_resource_versions WHERE resource_id=$1`, d.ResourceID).Scan(&latest, &number); err != nil {
		return Version{}, err
	}
	if d.State != "editing" || d.Revision != revision || d.BaseVersionID != latest {
		return Version{}, &Conflict{CurrentRevision: d.Revision, CurrentVersionID: latest}
	}
	if len(d.Files) == 0 && len(d.Endpoints) == 0 {
		return Version{}, invalid("draft must contain a file or endpoint")
	}
	if _, err = validateManifest(d.Manifest); err != nil {
		return Version{}, err
	}
	doc, err := s.ReadDocument(ctx, d.Manifest)
	if err != nil {
		return Version{}, err
	}
	meta, err := ParseDocument([]byte(doc))
	if err != nil {
		return Version{}, err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE owned_resources SET name=$2,description=$3,tags=$4,revision=revision+1,updated_at=now() WHERE id=$1`, d.ResourceID, meta.Name, meta.Description, marshal(meta.Tags)); err != nil {
		return Version{}, err
	}
	v, err := scanVersion(tx.QueryRowContext(ctx, `INSERT INTO owned_resource_versions(resource_id,version,size,manifest,secret_cipher,note,validation,actor) SELECT resource_id,$2,size,manifest,secret_cipher,$3,$4,$5 FROM owned_resource_drafts WHERE id=$1 RETURNING `+versionCols, id, number, note, validation, marshal(a)))
	if err != nil {
		return v, err
	}
	// The directory is complete before the version becomes visible in the DB.
	if err = s.publishTree(ctx, v); err != nil {
		return v, err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE owned_resource_drafts SET state='published',published_version_id=$2,secret_cipher=NULL,revision=revision+1,updated_at=now() WHERE id=$1`, id, v.ID); err != nil {
		return v, err
	}
	if err = event(ctx, tx, d.ResourceID, v.ID, "published", note, a); err != nil {
		return v, err
	}
	return v, tx.Commit()
}
