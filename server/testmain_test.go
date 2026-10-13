package server

import (
	"os"
	"testing"

	"github.com/RC-CHN/ARTEX/internal/testpg"
)

func TestMain(m *testing.M) {
	os.Exit(testpg.Run(m, "server"))
}

func requireTestPostgres(t *testing.T) {
	t.Helper()
	// TestMain supplies this only after creating an isolated test database.
	if os.Getenv("ARTEX_PG_DSN") == "" {
		t.Skip("PostgreSQL integration test: set ARTEX_TEST_PG_DSN to an isolated test server")
	}
}
