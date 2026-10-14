//go:build !linux

package runnernode

import "fmt"

func sharedCapacity(string, uint64, uint64) error {
	return fmt.Errorf("shared Kubernetes execution requires Linux")
}
