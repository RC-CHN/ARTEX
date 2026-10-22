// Package resourcelib manages operator-owned reusable resources, independently
// of discovered assets. Publication is explicit; stored files are never executed.
package resourcelib

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"path"
	"strings"
	"time"

	"github.com/RC-CHN/ARTEX/internal/operation"
)

const (
	MaxFileBytes     int64 = 256 << 20
	MaxLibraryBytes  int64 = 10 << 30
	MaxTaskBytes     int64 = 1 << 30
	MaxTaskResources       = 32
	MaxFiles               = 2048
	MaxEndpoints           = 32
)

var (
	ErrConflict    = errors.New("resource changed; reload the current revision and create a draft from the latest version before retrying")
	ErrUnavailable = errors.New("resource unavailable or task access not granted")
	ErrLimit       = errors.New("resource quota exceeded")
)

type Conflict struct {
	CurrentRevision  int64 `json:"current_revision"`
	CurrentVersionID int64 `json:"current_version_id"`
}

func (e *Conflict) Error() string {
	return fmt.Sprintf("%s (current_revision=%d, current_version_id=%d)", ErrConflict, e.CurrentRevision, e.CurrentVersionID)
}
func (e *Conflict) Unwrap() error { return ErrConflict }

type InputError struct{ Message string }

func (e *InputError) Error() string { return e.Message }
func invalid(message string) error  { return &InputError{Message: message} }

type Store struct {
	DB   *sql.DB
	Root string
	Key  []byte
}

// Actor is populated by the server, never trusted from tool/HTTP input.
type Actor struct {
	TaskID   int64  `json:"task_id"`
	ThreadID string `json:"thread_id"`
	RunnerID string `json:"runner_id"`
}
type Resource struct {
	ID            int64      `json:"id"`
	Name          string     `json:"name"`
	Kind          string     `json:"kind"`
	Description   string     `json:"description"`
	Tags          []string   `json:"tags"`
	AccessMode    string     `json:"access_mode"`
	Enabled       bool       `json:"enabled"`
	Revision      int64      `json:"revision"`
	CreatorTaskID int64      `json:"creator_task_id"`
	UpdatedAt     time.Time  `json:"updated_at"`
	DeletedAt     *time.Time `json:"deleted_at,omitempty"`
}
type File struct {
	Path       string `json:"path"`
	Digest     string `json:"sha256"`
	Size       int64  `json:"size"`
	Executable bool   `json:"executable"`
}
type Endpoint struct {
	Name          string `json:"name"`
	URL           string `json:"url"`
	HasCredential bool   `json:"has_credential"`
}
type Manifest struct {
	Metadata  DocumentMeta `json:"metadata"`
	Files     []File       `json:"files"`
	Endpoints []Endpoint   `json:"endpoints"`
	OS        string       `json:"os"`
	Arch      string       `json:"arch"`
}
type Version struct {
	ID         int64 `json:"id"`
	ResourceID int64 `json:"resource_id"`
	Version    int   `json:"version"`
	Size       int64 `json:"size"`
	Manifest
	Note       string    `json:"note"`
	Validation string    `json:"validation"`
	Actor      Actor     `json:"actor"`
	CreatedAt  time.Time `json:"created_at"`
}
type Draft struct {
	ID                 int64  `json:"id"`
	ResourceID         int64  `json:"resource_id"`
	BaseVersionID      int64  `json:"base_version_id"`
	Revision           int64  `json:"revision"`
	State              string `json:"state"`
	PublishedVersionID int64  `json:"published_version_id"`
	Size               int64  `json:"size"`
	Manifest
	Actor     Actor     `json:"actor"`
	UpdatedAt time.Time `json:"updated_at"`
}
type Event struct {
	ID        int64     `json:"id"`
	VersionID int64     `json:"version_id"`
	Kind      string    `json:"kind"`
	Actor     Actor     `json:"actor"`
	Detail    string    `json:"detail"`
	CreatedAt time.Time `json:"created_at"`
}

func marshal(v any) string { b, _ := json.Marshal(v); return string(b) }
func ValidateResource(r *Resource) error {
	r.Name = strings.TrimSpace(r.Name)
	if r.Name == "" || len(r.Name) > 120 || len(r.Description) > 8000 || len(r.Tags) > 20 {
		return invalid("name, description or tags exceed limits")
	}
	switch r.Kind {
	case "tool", "file", "dataset", "proxy", "host", "beacon":
	default:
		return invalid("invalid resource kind")
	}
	if r.AccessMode != "global" && r.AccessMode != "restricted" {
		return invalid("invalid access mode")
	}
	if r.Tags == nil {
		r.Tags = []string{}
	}
	for _, t := range r.Tags {
		if t == "" || len(t) > 64 {
			return invalid("invalid tag")
		}
	}
	return nil
}
func ValidatePath(p string) error {
	if p == "" || len(p) > 512 || path.Clean(p) != p || strings.HasPrefix(p, "/") || strings.HasPrefix(p, "../") || p == ".." || p == "." || strings.ContainsAny(p, "\\\x00\r\n") {
		return invalid("file path must be a normalized relative path")
	}
	for _, c := range strings.Split(p, "/") {
		if len(c) > 160 || strings.HasPrefix(c, ".") {
			return invalid("hidden or overlong file path component")
		}
	}
	return nil
}
func validateManifest(m Manifest) (int64, error) {
	if len(m.Files) > MaxFiles || len(m.Endpoints) > MaxEndpoints || len(m.OS) > 40 || len(m.Arch) > 40 {
		return 0, ErrLimit
	}
	seen := map[string]bool{}
	var total int64
	for _, f := range m.Files {
		if err := ValidatePath(f.Path); err != nil {
			return 0, err
		}
		if seen[f.Path] || f.Size < 0 || f.Size > MaxFileBytes || len(f.Digest) != 64 {
			return 0, invalid("invalid or duplicate file")
		}
		seen[f.Path] = true
		total += f.Size
	}
	for p := range seen {
		for d := path.Dir(p); d != "."; d = path.Dir(d) {
			if seen[d] {
				return 0, invalid("file/directory path conflict")
			}
		}
	}
	if total > MaxTaskBytes {
		return 0, ErrLimit
	}
	names := map[string]bool{}
	for _, e := range m.Endpoints {
		if e.Name == "" || len(e.Name) > 64 || strings.ContainsAny(e.Name, "/\\\x00\r\n") || names[e.Name] {
			return 0, invalid("endpoint names must be unique")
		}
		names[e.Name] = true
		u, err := url.Parse(e.URL)
		if err != nil || len(e.URL) > 2048 || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
			return 0, invalid("endpoint must include host, without embedded credentials, query or fragment")
		}
		switch u.Scheme {
		case "http", "https", "socks5", "socks5h", "ssh", "tcp", "udp":
		default:
			return 0, invalid("unsupported endpoint scheme")
		}
	}
	return total, nil
}

const resourceCols = `id,name,kind,description,tags,access_mode,enabled,revision,creator_task_id,updated_at,deleted_at`

type scanner interface{ Scan(...any) error }

func scanResource(row scanner) (Resource, error) {
	var r Resource
	var tags []byte
	err := row.Scan(&r.ID, &r.Name, &r.Kind, &r.Description, &tags, &r.AccessMode, &r.Enabled, &r.Revision, &r.CreatorTaskID, &r.UpdatedAt, &r.DeletedAt)
	if err == nil {
		err = json.Unmarshal(tags, &r.Tags)
	}
	return r, err
}
func (s *Store) Get(ctx context.Context, id int64) (Resource, error) {
	return scanResource(s.DB.QueryRowContext(ctx, `SELECT `+resourceCols+` FROM owned_resources WHERE id=$1`, id))
}
func (s *Store) List(ctx context.Context, q, kind string, before int64) ([]Resource, error) {
	if len(q) > 200 {
		return nil, invalid("search too long")
	}
	rows, err := s.DB.QueryContext(ctx, `SELECT `+resourceCols+` FROM owned_resources WHERE deleted_at IS NULL AND ($1='' OR name ILIKE '%'||$1||'%' OR description ILIKE '%'||$1||'%') AND ($2='' OR kind=$2) AND ($3=0 OR id<$3) ORDER BY id DESC LIMIT 100`, q, kind, before)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Resource{}
	for rows.Next() {
		r, e := scanResource(rows)
		if e != nil {
			return nil, e
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// Every mutation uses the same lock order: shared GC -> quota -> task -> resource -> draft.
// Bytes are received before acquiring locks, so slow uploads don't block users.
const resourceGCLock int64 = 7337741011

func (s *Store) begin(ctx context.Context, a Actor, id int64) (*sql.Tx, Resource, error) {
	return s.beginResource(ctx, a, id, false)
}
func (s *Store) beginResource(ctx context.Context, a Actor, id int64, allowDeleted bool) (*sql.Tx, Resource, error) {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return nil, Resource{}, err
	}
	if _, err = tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock_shared($1)`, resourceGCLock); err != nil {
		tx.Rollback()
		return nil, Resource{}, err
	}
	r, err := lockResource(ctx, tx, a, id, allowDeleted)
	if err != nil {
		tx.Rollback()
		return nil, Resource{}, err
	}
	return tx, r, nil
}

// A publisher can prepare its immutable directory under the shared GC lock,
// then enter this short quota/access/CAS critical section in the same transaction.
func lockResource(ctx context.Context, tx *sql.Tx, a Actor, id int64, allowDeleted bool) (Resource, error) {
	var err error
	fail := func(e error) (Resource, error) { return Resource{}, e }
	if _, err = tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(7337741010)`); err != nil {
		return fail(err)
	}
	if a.TaskID > 0 {
		var task int64
		if err = tx.QueryRowContext(ctx, `SELECT id FROM tasks WHERE id=$1 AND deleted_at IS NULL AND archived_at IS NULL FOR SHARE`, a.TaskID).Scan(&task); err != nil {
			return fail(ErrUnavailable)
		}
	}
	if id == 0 {
		return Resource{}, nil
	}
	r, err := scanResource(tx.QueryRowContext(ctx, `SELECT `+resourceCols+` FROM owned_resources WHERE id=$1 FOR UPDATE`, id))
	if err != nil {
		return fail(err)
	}
	if r.DeletedAt != nil && !allowDeleted {
		return fail(ErrUnavailable)
	}
	if a.TaskID > 0 {
		if !r.Enabled && !(allowDeleted && r.DeletedAt != nil) {
			return fail(ErrUnavailable)
		}
		if r.AccessMode == "restricted" && r.CreatorTaskID != a.TaskID {
			var granted bool
			if err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM task_owned_resources WHERE task_id=$1 AND resource_id=$2)`, a.TaskID, id).Scan(&granted); err != nil {
				return fail(err)
			}
			if !granted {
				return fail(ErrUnavailable)
			}
		}
	}
	return r, nil
}
func event(ctx context.Context, tx *sql.Tx, id, version int64, kind, detail string, a Actor) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO owned_resource_events(resource_id,version_id,kind,detail,actor) VALUES($1,$2,$3,$4,$5)`, id, version, kind, detail, marshal(a))
	return err
}
func (s *Store) Create(ctx context.Context, r *Resource, a Actor) error {
	return s.CreateWithRequest(ctx, r, a, "")
}
func (s *Store) CreateWithRequest(ctx context.Context, r *Resource, a Actor, requestID string) error {
	if err := ValidateResource(r); err != nil {
		return err
	}
	tx, _, err := s.begin(ctx, a, 0)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	input := *r
	scope := fmt.Sprintf("resource-create:%d", a.TaskID)
	if requestID != "" {
		if replay, e := operation.Replay(ctx, tx, scope, requestID, input, r); e != nil || replay {
			return e
		}
	}
	r.CreatorTaskID = a.TaskID
	err = tx.QueryRowContext(ctx, `INSERT INTO owned_resources(name,kind,description,tags,access_mode,enabled,creator_task_id) VALUES($1,$2,$3,$4,$5,$6,$7) RETURNING id,revision,updated_at`, r.Name, r.Kind, r.Description, marshal(r.Tags), r.AccessMode, r.Enabled, a.TaskID).Scan(&r.ID, &r.Revision, &r.UpdatedAt)
	if err != nil {
		return err
	}
	if err = event(ctx, tx, r.ID, 0, "created", "", a); err != nil {
		return err
	}
	if requestID != "" {
		if err = operation.Save(ctx, tx, scope, requestID, input, r); err != nil {
			return err
		}
	}
	return tx.Commit()
}
func (s *Store) Update(ctx context.Context, r *Resource, a Actor) error {
	return s.UpdateWithRequest(ctx, r, a, "")
}
func (s *Store) UpdateWithRequest(ctx context.Context, r *Resource, a Actor, requestID string) error {
	if err := ValidateResource(r); err != nil {
		return err
	}
	tx, current, err := s.begin(ctx, a, r.ID)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	input := *r
	scope := fmt.Sprintf("resource-update:%d:%d", a.TaskID, r.ID)
	if requestID != "" {
		if replay, e := operation.Replay(ctx, tx, scope, requestID, input, r); e != nil || replay {
			return e
		}
	}
	if current.Revision != r.Revision {
		return &Conflict{CurrentRevision: current.Revision}
	}
	// Task grants permit maintenance, not changing access policy or disabling a shared service.
	if a.TaskID > 0 && (r.AccessMode != current.AccessMode || r.Enabled != current.Enabled) {
		return invalid("agents can update metadata; access policy is managed in the resource library UI")
	}
	var published bool
	if err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM owned_resource_versions WHERE resource_id=$1)`, r.ID).Scan(&published); err != nil {
		return err
	}
	if published && (r.Name != current.Name || r.Description != current.Description || marshal(r.Tags) != marshal(current.Tags)) {
		return invalid("published name, description and tags are indexed from RESOURCE.md; edit a draft and publish it")
	}
	if r.Kind != current.Kind {
		return invalid("resource kind cannot change")
	}
	r.CreatorTaskID = current.CreatorTaskID
	err = tx.QueryRowContext(ctx, `UPDATE owned_resources SET name=$2,description=$3,tags=$4,access_mode=$5,enabled=$6,revision=revision+1,updated_at=now() WHERE id=$1 RETURNING revision,updated_at`, r.ID, r.Name, r.Description, marshal(r.Tags), r.AccessMode, r.Enabled).Scan(&r.Revision, &r.UpdatedAt)
	if err != nil {
		return err
	}
	if err = event(ctx, tx, r.ID, 0, "metadata_updated", "", a); err != nil {
		return err
	}
	if requestID != "" {
		if err = operation.Save(ctx, tx, scope, requestID, input, r); err != nil {
			return err
		}
	}
	return tx.Commit()
}

const versionCols = `id,resource_id,version,size,manifest,note,validation,actor,created_at`

func scanVersion(row scanner) (Version, error) {
	var v Version
	var m, a []byte
	err := row.Scan(&v.ID, &v.ResourceID, &v.Version, &v.Size, &m, &v.Note, &v.Validation, &a, &v.CreatedAt)
	if err == nil {
		err = json.Unmarshal(m, &v.Manifest)
	}
	if err == nil {
		err = json.Unmarshal(a, &v.Actor)
	}
	return v, err
}
func (s *Store) Version(ctx context.Context, id int64) (Version, error) {
	return scanVersion(s.DB.QueryRowContext(ctx, `SELECT `+versionCols+` FROM owned_resource_versions WHERE id=$1`, id))
}
func (s *Store) Versions(ctx context.Context, id int64) ([]Version, error) {
	rows, err := s.DB.QueryContext(ctx, `SELECT `+versionCols+` FROM owned_resource_versions WHERE resource_id=$1 ORDER BY id DESC LIMIT 100`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Version{}
	for rows.Next() {
		v, e := scanVersion(rows)
		if e != nil {
			return nil, e
		}
		out = append(out, v)
	}
	return out, rows.Err()
}
func (s *Store) Events(ctx context.Context, id, before int64) ([]Event, error) {
	rows, err := s.DB.QueryContext(ctx, `SELECT id,version_id,kind,actor,detail,created_at FROM owned_resource_events WHERE resource_id=$1 AND ($2=0 OR id<$2) ORDER BY id DESC LIMIT 100`, id, before)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Event{}
	for rows.Next() {
		var e Event
		var a []byte
		if err = rows.Scan(&e.ID, &e.VersionID, &e.Kind, &a, &e.Detail, &e.CreatedAt); err != nil {
			return nil, err
		}
		if err = json.Unmarshal(a, &e.Actor); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}
func (s *Store) Observe(ctx context.Context, id, version int64, status, detail string, a Actor) error {
	if status != "available" && status != "unreachable" && status != "untested" {
		return invalid("status must be available/unreachable/untested")
	}
	if len(detail) > 2000 {
		return ErrLimit
	}
	tx, _, err := s.begin(ctx, a, id)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var ok bool
	if err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM owned_resource_versions WHERE id=$1 AND resource_id=$2)`, version, id).Scan(&ok); err != nil {
		return err
	}
	if !ok {
		return sql.ErrNoRows
	}
	if err = event(ctx, tx, id, version, "observation:"+status, detail, a); err != nil {
		return err
	}
	return tx.Commit()
}
func (s *Store) aead() (cipher.AEAD, error) {
	if len(s.Key) < 32 {
		return nil, invalid("resource encryption key unavailable")
	}
	key := sha256.Sum256(append([]byte("artex-owned-resources-v1\x00"), s.Key...))
	b, e := aes.NewCipher(key[:])
	if e != nil {
		return nil, e
	}
	return cipher.NewGCM(b)
}
func (s *Store) seal(secrets map[string]string, id int64) ([]byte, error) {
	if len(secrets) == 0 {
		return nil, nil
	}
	a, e := s.aead()
	if e != nil {
		return nil, e
	}
	nonce := make([]byte, a.NonceSize())
	if _, e = rand.Read(nonce); e != nil {
		return nil, e
	}
	return a.Seal(nonce, nonce, []byte(marshal(secrets)), []byte(fmt.Sprint(id))), nil
}
func (s *Store) unseal(raw []byte, id int64) (map[string]string, error) {
	out := map[string]string{}
	if len(raw) == 0 {
		return out, nil
	}
	a, e := s.aead()
	if e != nil {
		return nil, e
	}
	if len(raw) < a.NonceSize() {
		return nil, invalid("invalid credential ciphertext")
	}
	b, e := a.Open(nil, raw[:a.NonceSize()], raw[a.NonceSize():], []byte(fmt.Sprint(id)))
	if e != nil {
		return nil, e
	}
	defer clear(b)
	e = json.Unmarshal(b, &out)
	return out, e
}
func (s *Store) Credentials(ctx context.Context, taskID, versionID int64) (map[string]string, error) {
	var raw []byte
	var id int64
	err := s.DB.QueryRowContext(ctx, `SELECT v.secret_cipher,v.resource_id FROM task_owned_resources b JOIN owned_resources r ON r.id=b.resource_id JOIN owned_resource_versions v ON v.id=b.version_id JOIN tasks t ON t.id=b.task_id WHERE b.task_id=$1 AND v.id=$2 AND r.enabled AND t.deleted_at IS NULL AND t.archived_at IS NULL`, taskID, versionID).Scan(&raw, &id)
	if err != nil {
		return nil, err
	}
	return s.unseal(raw, id)
}

// Pin binds one immutable version per resource per task. Admin grants can change
// a pin explicitly; agent use is idempotent and never silently upgrades it.
func (s *Store) Pin(ctx context.Context, taskID, id, versionID int64, grant bool) (Version, error) {
	return s.pin(ctx, taskID, id, versionID, grant, nil)
}

func (s *Store) Grant(ctx context.Context, taskID, id, versionID, expected int64) (Version, error) {
	if expected < 0 {
		return Version{}, invalid("expected_version_id cannot be negative")
	}
	return s.pin(ctx, taskID, id, versionID, true, &expected)
}

// Switch changes an existing task pin only if the caller still observes the
// expected version. It grants no new access and preserves all old file paths.
func (s *Store) Switch(ctx context.Context, taskID, id, versionID, expected int64) (Version, error) {
	if versionID <= 0 || expected <= 0 {
		return Version{}, invalid("version_id and expected_version_id must be positive")
	}
	return s.pin(ctx, taskID, id, versionID, false, &expected)
}

func (s *Store) pin(ctx context.Context, taskID, id, versionID int64, grant bool, expected *int64) (Version, error) {
	var zero Version
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return zero, err
	}
	defer tx.Rollback()
	var task int64
	if err = tx.QueryRowContext(ctx, `SELECT id FROM tasks WHERE id=$1 AND deleted_at IS NULL AND archived_at IS NULL FOR UPDATE`, taskID).Scan(&task); err != nil {
		return zero, ErrUnavailable
	}
	var mode string
	var enabled bool
	var creator int64
	if err = tx.QueryRowContext(ctx, `SELECT access_mode,enabled,creator_task_id FROM owned_resources WHERE id=$1 FOR SHARE`, id).Scan(&mode, &enabled, &creator); err != nil {
		return zero, err
	}
	if !enabled {
		return zero, ErrUnavailable
	}
	var existing int64
	err = tx.QueryRowContext(ctx, `SELECT version_id FROM task_owned_resources WHERE task_id=$1 AND resource_id=$2`, taskID, id).Scan(&existing)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return zero, err
	}
	if expected != nil && existing != *expected {
		return zero, &Conflict{CurrentVersionID: existing}
	}
	if existing > 0 && !grant && expected == nil {
		if versionID > 0 && existing != versionID {
			return zero, ErrConflict
		}
		versionID = existing
	}
	if existing == 0 && mode == "restricted" && creator != taskID && !grant {
		return zero, ErrUnavailable
	}
	var v Version
	if versionID == 0 {
		v, err = scanVersion(tx.QueryRowContext(ctx, `SELECT `+versionCols+` FROM owned_resource_versions WHERE resource_id=$1 ORDER BY id DESC LIMIT 1`, id))
	} else {
		v, err = scanVersion(tx.QueryRowContext(ctx, `SELECT `+versionCols+` FROM owned_resource_versions WHERE id=$1 AND resource_id=$2`, versionID, id))
	}
	if err != nil {
		return zero, err
	}
	var count int
	var bytes int64
	if err = tx.QueryRowContext(ctx, `SELECT count(*),COALESCE(sum(v.size),0) FROM task_owned_resources b JOIN owned_resource_versions v ON v.id=b.version_id WHERE b.task_id=$1 AND b.resource_id<>$2`, taskID, id).Scan(&count, &bytes); err != nil {
		return zero, err
	}
	if count >= MaxTaskResources || bytes+v.Size > MaxTaskBytes {
		return zero, ErrLimit
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO task_owned_resources(task_id,resource_id,version_id) VALUES($1,$2,$3) ON CONFLICT(task_id,resource_id) DO UPDATE SET version_id=EXCLUDED.version_id`, taskID, id, v.ID)
	if err != nil {
		return zero, err
	}
	if existing != v.ID {
		if err = event(ctx, tx, id, v.ID, "task_pin", fmt.Sprintf("previous_version_id=%d", existing), Actor{TaskID: taskID}); err != nil {
			return zero, err
		}
	}
	return v, tx.Commit()
}
func (s *Store) TaskVersions(ctx context.Context, taskID int64) ([]Version, error) {
	rows, err := s.DB.QueryContext(ctx, `SELECT v.id,v.resource_id,v.version,v.size,v.manifest,v.note,v.validation,v.actor,v.created_at FROM task_owned_resources b JOIN owned_resource_versions v ON v.id=b.version_id JOIN owned_resources r ON r.id=b.resource_id WHERE b.task_id=$1 AND r.enabled ORDER BY b.resource_id LIMIT 32`, taskID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Version{}
	for rows.Next() {
		v, e := scanVersion(rows)
		if e != nil {
			return nil, e
		}
		out = append(out, v)
	}
	return out, rows.Err()
}
