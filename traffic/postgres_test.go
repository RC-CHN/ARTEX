package traffic

import (
	"bytes"
	"context"
	"database/sql"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// Integration tests and benchmarks use explicitly opted-in, isolated schemas.
// Never fall back to the application's production ARTEX_PG_DSN.
func postgresTestSchema(tb testing.TB) (dsn, schema string) {
	tb.Helper()
	dsn = os.Getenv("ARTEX_TRAFFIC_TEST_DSN")
	if dsn == "" {
		tb.Skip("set ARTEX_TRAFFIC_TEST_DSN to a disposable PostgreSQL database")
	}
	schema = "traffic_test_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	d, err := sql.Open("pgx", dsn)
	if err != nil {
		tb.Fatal(err)
	}
	if err := d.Ping(); err != nil {
		d.Close()
		tb.Fatal(err)
	}
	tb.Cleanup(func() {
		if _, err := d.Exec(`DROP SCHEMA IF EXISTS ` + pgx.Identifier{schema}.Sanitize() + ` CASCADE`); err != nil {
			tb.Error(err)
		}
		d.Close()
	})
	return dsn, schema
}

func openTestPostgres(tb testing.TB, dir string) *Traffic {
	tb.Helper()
	dsn, schema := postgresTestSchema(tb)
	tr, err := openPostgres(dir, "127.0.0.1:0", dsn, schema)
	if err != nil {
		tb.Fatal(err)
	}
	tb.Cleanup(func() { tr.Close() })
	return tr
}

func TestPostgresCaptureSearchEvidenceAndArchive(t *testing.T) {
	tr := openTestPostgres(t, t.TempDir())
	if tr.Backend() != "postgres" {
		t.Fatal("unexpected backend")
	}
	body := []byte(strings.Repeat("searchable payload 中文正文 ", 18000) + "deep-marker 100%_literal\\path")
	flow := newFlow("api.example.com:8443", "POST", "/v1/login", []byte(`{"password":"P@ssw0rd"}`), body)
	// Raw headers can contain non-UTF-8 octets; bytea must preserve them exactly.
	flow.Request.Header.Set("X-Legacy", string([]byte{0xff, 0xfe}))
	if err := tr.recordFlow(flow); err != nil {
		t.Fatal(err)
	}
	id := onlyExchangeID(t, tr)
	if err := tr.recordFlow(newFlow("other.example.com", "GET", "/health", nil, []byte{0, 255, 254})); err != nil {
		t.Fatal(err)
	}
	for _, term := range []string{"ssw0r", "中文正文", "deep-marker", "100%_literal\\path", "PASSWORD"} {
		hits, err := tr.query("https://api.example.com:8443", "", term, 0, 10)
		if err != nil || len(hits) != 1 || hits[0].ID != id {
			t.Fatalf("search %q: hits=%v err=%v", term, hits, err)
		}
	}
	for _, term := range []string{"100X_literal\\path", "nonexistent", "\" OR 1=1 --"} {
		hits, err := tr.query("api.example.com", "", term, 0, 10)
		if err != nil || len(hits) != 0 {
			t.Fatalf("literal search %q matched: %v %v", term, hits, err)
		}
	}
	if hits, err := tr.query("api.example.com:8080", "", "", 0, 10); err != nil || len(hits) != 0 {
		t.Fatalf("port isolation: %v %v", hits, err)
	}
	page, total, err := tr.Page(PageQuery{Host: "API.EXAMPLE", Body: "deep-marker", Status: "2xx", RespMin: -1, RespMax: -1, Sort: "resp_len"}, 0, 10)
	if err != nil || total != 1 || len(page) != 1 {
		t.Fatalf("page: %d %v %v", total, page, err)
	}
	req, _, err := tr.Get(id)
	if err != nil || !strings.Contains(req, string([]byte{0xff, 0xfe})) {
		t.Fatalf("raw header lost: %v", err)
	}
	if err := tr.ReadEvidence(context.Background(), []string{id}, func(e EvidenceExchange) error {
		got, err := io.ReadAll(e.Response)
		if err != nil {
			return err
		}
		if !bytes.Equal(got, body) {
			return fmt.Errorf("full evidence body changed")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	archive := filepath.Join(t.TempDir(), "snapshot")
	if n, err := tr.ExportHosts([]string{"api.example.com"}, archive); err != nil || n != 1 {
		t.Fatalf("export: %d %v", n, err)
	}
	stage, err := tr.StageDeleteHostsExact([]string{"api.example.com"})
	if err != nil {
		t.Fatal(err)
	}
	if err := stage.Rollback(); err != nil {
		t.Fatal(err)
	}
	if _, _, err := tr.Get(id); err != nil {
		t.Fatalf("rollback lost capture: %v", err)
	}
	if n, err := tr.DeleteHostsExact([]string{"api.example.com"}); err != nil || n != 1 {
		t.Fatalf("delete: %d %v", n, err)
	}
	for _, want := range []int64{1, 0} {
		if n, err := tr.ImportArchive(archive); err != nil || n != want {
			t.Fatalf("restore: %d %v want %d", n, err, want)
		}
	}
	if hits, err := tr.query("api.example.com", "", "deep-marker", 0, 10); err != nil || len(hits) != 1 {
		t.Fatalf("restored blob not fully searchable: %v %v", hits, err)
	}
	if n, _, err := tr.DeleteAll(); err != nil || n != 2 {
		t.Fatalf("purge: %d %v", n, err)
	}
	for _, table := range []string{"exchanges", "exchange_bodies", "blob_refs", "ex_fts"} {
		var n int
		if err := tr.db.QueryRow(`SELECT count(*) FROM ` + table).Scan(&n); err != nil || n != 0 {
			t.Fatalf("purge table %s: %d %v", table, n, err)
		}
	}
	if _, err := os.Stat(filepath.Join(tr.dir, "_index", "index.sqlite")); !os.IsNotExist(err) {
		t.Fatalf("PostgreSQL created SQLite file: %v", err)
	}
}

func TestPostgresConcurrentCaptureAndAtomicFailure(t *testing.T) {
	tr := openTestPostgres(t, t.TempDir())
	const count = 100
	var wg sync.WaitGroup
	for i := 0; i < count; i++ {
		wg.Go(func() {
			if err := tr.recordFlow(newFlow("parallel.example", "GET", "/data", nil, []byte("concurrent payload"))); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	for _, table := range []string{"exchanges", "exchange_bodies", "ex_fts"} {
		var n int
		if err := tr.db.QueryRow(`SELECT count(*) FROM ` + table).Scan(&n); err != nil || n != count {
			t.Fatalf("%s count=%d err=%v", table, n, err)
		}
	}
	if _, err := tr.db.Exec(`ALTER TABLE ex_fts ADD CONSTRAINT reject_marker CHECK(content NOT LIKE '%reject-this-marker%')`); err != nil {
		t.Fatal(err)
	}
	if err := tr.recordFlow(newFlow("failure.example", "GET", "/data", nil, []byte("reject-this-marker"))); err == nil {
		t.Fatal("expected injected index failure")
	}
	if n, err := tr.Count(); err != nil || n != count {
		t.Fatalf("partial capture committed: %d %v", n, err)
	}
}

func TestPostgresMigrationPreservesIDsBodiesBlobsAndLegacyPaths(t *testing.T) {
	dir := t.TempDir()
	old, err := Open(dir, "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	if err := old.recordFlow(newFlow("old.example", "POST", "/capture", []byte("password=old"), []byte(strings.Repeat("long body ", 30000)+"tail-migration-marker"))); err != nil {
		t.Fatal(err)
	}
	id := onlyExchangeID(t, old)
	beforeReq, beforeResp, err := old.Get(id)
	if err != nil {
		t.Fatal(err)
	}
	legacyPath := "legacy.example/GET/old"
	if err := os.MkdirAll(filepath.Join(dir, legacyPath), 0o700); err != nil {
		t.Fatal(err)
	}
	for name, data := range map[string]string{"request.http": "GET /old HTTP/1.1\n\nlegacy request", "response.http": "HTTP/1.1 200 OK\n\nlegacy-response-marker"} {
		if err := os.WriteFile(filepath.Join(dir, legacyPath, name), []byte(data), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := old.db.Exec(`INSERT INTO exchanges(id,ts,host,method,url_template,url,status,content_type,req_len,resp_len,path) VALUES('legacy-id',1,'legacy.example','GET','/old','http://legacy.example/old',200,'text/plain',14,22,?)`, legacyPath); err != nil {
		t.Fatal(err)
	}
	if err := old.Close(); err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(dir, "_index", "index.sqlite")
	beforeFile, err := os.ReadFile(source)
	if err != nil {
		t.Fatal(err)
	}
	dsn, schema := postgresTestSchema(t)
	tr, err := openPostgres(dir, "127.0.0.1:0", dsn, schema)
	if err != nil {
		t.Fatal(err)
	}
	req, resp, err := tr.Get(id)
	if err != nil || req != beforeReq || resp != beforeResp {
		t.Fatalf("migrated payload mismatch: %v", err)
	}
	if hits, err := tr.query("old.example", "", "tail-migration-marker", 0, 10); err != nil || len(hits) != 1 || hits[0].ID != id {
		t.Fatalf("blob FTS migration: %v %v", hits, err)
	}
	if hits, err := tr.query("legacy.example", "", "legacy-response-marker", 0, 10); err != nil || len(hits) != 1 {
		t.Fatalf("legacy FTS migration: %v %v", hits, err)
	}
	if _, resp, err := tr.Get("legacy-id"); err != nil || !strings.Contains(resp, "legacy-response-marker") {
		t.Fatalf("legacy get: %v", err)
	}
	afterFile, err := os.ReadFile(source)
	if err != nil || !bytes.Equal(beforeFile, afterFile) {
		t.Fatal("migration modified source SQLite")
	}
	if _, err := tr.DeleteHostsExact([]string{"old.example"}); err != nil {
		t.Fatal(err)
	}
	tr.Close()
	// The source backup must never resurrect captures deleted after migration.
	tr, err = openPostgres(dir, "127.0.0.1:0", dsn, schema)
	if err != nil {
		t.Fatal(err)
	}
	defer tr.Close()
	if n, err := tr.Count(); err != nil || n != 1 {
		t.Fatalf("restart repeated migration: %d %v", n, err)
	}
}

func TestPostgresMigrationFailureRollsBackAndRetries(t *testing.T) {
	dir := t.TempDir()
	old, err := Open(dir, "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	if err := old.recordFlow(newFlow("retry.example", "GET", "/blob", nil, []byte(strings.Repeat("large-body", 40000)))); err != nil {
		t.Fatal(err)
	}
	var hash string
	if err := old.db.QueryRow(`SELECT resp_blob FROM exchange_bodies`).Scan(&hash); err != nil {
		t.Fatal(err)
	}
	old.Close()
	blob, _ := old.blobPath(hash)
	backup := blob + ".backup"
	if err := os.Rename(blob, backup); err != nil {
		t.Fatal(err)
	}
	dsn, schema := postgresTestSchema(t)
	if tr, err := openPostgres(dir, "127.0.0.1:0", dsn, schema); err == nil {
		tr.Close()
		t.Fatal("missing blob migration succeeded")
	}
	d, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	var n int
	if err := d.QueryRow(`SELECT count(*) FROM ` + pgx.Identifier{schema, "exchanges"}.Sanitize()).Scan(&n); err != nil || n != 0 {
		t.Fatalf("partial migration persisted: %d %v", n, err)
	}
	if err := os.Rename(backup, blob); err != nil {
		t.Fatal(err)
	}
	tr, err := openPostgres(dir, "127.0.0.1:0", dsn, schema)
	if err != nil {
		t.Fatal(err)
	}
	defer tr.Close()
	if n, err := tr.Count(); err != nil || n != 1 {
		t.Fatalf("migration retry: %d %v", n, err)
	}
}

func TestPostgresMigrationRecoversCommittedWAL(t *testing.T) {
	old, err := Open(t.TempDir(), "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer old.Close()
	if _, err := old.db.Exec(`PRAGMA wal_autocheckpoint=0`); err != nil {
		t.Fatal(err)
	}
	if _, err := old.db.Exec(`PRAGMA wal_checkpoint(TRUNCATE)`); err != nil {
		t.Fatal(err)
	}
	if err := old.recordFlow(newFlow("wal.example", "POST", "/pending", []byte("committed request"), []byte("committed-wal-response"))); err != nil {
		t.Fatal(err)
	}
	id := onlyExchangeID(t, old)
	// Copy while idle but before Close checkpoints the WAL, simulating files
	// left by a stopped process after an unclean exit.
	dir := t.TempDir()
	indexDir := filepath.Join(dir, "_index")
	if err := os.MkdirAll(indexDir, 0o700); err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(indexDir, "index.sqlite")
	if _, err := snapshotSQLite(filepath.Join(old.dir, "_index", "index.sqlite"), source); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(source + "-wal")
	if err != nil || len(before) <= 32 {
		t.Fatalf("fixture has no pending WAL: %v", err)
	}
	tr := openTestPostgres(t, dir)
	if _, response, err := tr.Get(id); err != nil || !strings.Contains(response, "committed-wal-response") {
		t.Fatalf("committed WAL capture lost: %v", err)
	}
	after, err := os.ReadFile(source + "-wal")
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("migration changed source WAL")
	}
}

func TestPostgresMigrationRejectsNonemptyDestination(t *testing.T) {
	dir := t.TempDir()
	dsn, schema := postgresTestSchema(t)
	tr, err := openPostgres(dir, "127.0.0.1:0", dsn, schema)
	if err != nil {
		t.Fatal(err)
	}
	if err := tr.recordFlow(newFlow("current.example", "GET", "/current", nil, []byte("current body"))); err != nil {
		t.Fatal(err)
	}
	id := onlyExchangeID(t, tr)
	tr.Close()
	old, err := Open(dir, "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	if err := old.recordFlow(newFlow("legacy.example", "GET", "/legacy", nil, []byte("legacy body"))); err != nil {
		t.Fatal(err)
	}
	old.Close()
	if tr, err := openPostgres(dir, "127.0.0.1:0", dsn, schema); err == nil {
		tr.Close()
		t.Fatal("merged unrelated indexes without a migration marker")
	}
	d, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	var got string
	if err := d.QueryRow(`SELECT id FROM ` + pgx.Identifier{schema, "exchanges"}.Sanitize()).Scan(&got); err != nil || got != id {
		t.Fatalf("existing PostgreSQL capture changed: %q %v", got, err)
	}
}
