package evidence

import (
	"os"
	"testing"

	"github.com/RC-CHN/ARTEX/internal/testpg"
)

func TestMain(m *testing.M) {
	os.Exit(testpg.Run(m, "evidence"))
}
