package server

import (
	"os"
	"testing"

	"github.com/Autumn-27/artex/internal/testpg"
)

func TestMain(m *testing.M) {
	os.Exit(testpg.Run(m, "server"))
}
