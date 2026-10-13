//go:build !linux

package runnernode

import (
	"fmt"
	"os"
)

func lockNode(string) (*os.File, error) {
	return nil, fmt.Errorf("the standalone runner currently requires Linux")
}
