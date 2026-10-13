// Package testpg provides PostgreSQL databases owned by one test process.
// Integration tests never use the configured application's database directly.
package testpg

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// Run creates a fresh database for a package and drops it after m.Run returns.
// ARTEX_TEST_PG_DSN (or the explicit legacy ARTEX_PG_DSN) identifies a test
// PostgreSQL server whose user has CREATEDB permission. Its database is only
// used as an administrative connection; no application tables are touched.
// With neither variable set, integration tests skip instead of reading config.json.
func Run(m *testing.M, suite string) (code int) {
	dsn := strings.TrimSpace(os.Getenv("ARTEX_TEST_PG_DSN"))
	if dsn == "" {
		dsn = strings.TrimSpace(os.Getenv("ARTEX_PG_DSN"))
	}
	configDir, err := os.MkdirTemp("", "artex-test-config-")
	if err != nil {
		fmt.Fprintln(os.Stderr, "test PostgreSQL config:", err)
		return 1
	}
	defer os.RemoveAll(configDir)
	_ = os.Setenv("ARTEX_CONFIG", filepath.Join(configDir, "absent.json"))
	_ = os.Unsetenv("ARTEX_PG_DSN")
	if dsn == "" {
		fmt.Fprintln(os.Stderr, "PostgreSQL integration tests skipped: set ARTEX_TEST_PG_DSN to an isolated test server")
		return m.Run()
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	admin, err := pgx.Connect(ctx, dsn)
	if err != nil {
		fmt.Fprintln(os.Stderr, "connect test PostgreSQL:", err)
		return 1
	}
	defer admin.Close(context.Background())
	name := "artex_test_" + suite + "_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	ident := pgx.Identifier{name}.Sanitize()
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+ident); err != nil {
		fmt.Fprintln(os.Stderr, "create isolated test database (CREATEDB required):", err)
		return 1
	}
	defer func() {
		cleanup, stop := context.WithTimeout(context.Background(), 30*time.Second)
		defer stop()
		if _, err := admin.Exec(cleanup, "DROP DATABASE "+ident+" WITH (FORCE)"); err != nil {
			fmt.Fprintln(os.Stderr, "drop isolated test database:", err)
			code = 1
		}
	}()
	isolated := dsn
	if strings.HasPrefix(isolated, "postgres://") || strings.HasPrefix(isolated, "postgresql://") {
		u, err := url.Parse(isolated)
		if err != nil {
			fmt.Fprintln(os.Stderr, "parse test PostgreSQL DSN:", err)
			return 1
		}
		u.Path = "/" + name
		u.RawPath = ""
		query := u.Query()
		query.Del("dbname")
		u.RawQuery = query.Encode()
		isolated = u.String()
	} else {
		isolated += " dbname=" + name
	}
	if err := os.Setenv("ARTEX_PG_DSN", isolated); err != nil {
		fmt.Fprintln(os.Stderr, "set test PostgreSQL DSN:", err)
		return 1
	}
	return m.Run()
}
