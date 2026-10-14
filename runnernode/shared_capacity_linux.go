//go:build linux

package runnernode

import (
	"fmt"
	"golang.org/x/sys/unix"
)

func sharedCapacity(path string, minBytes, minInodes uint64) error {
	var st unix.Statfs_t
	if err := unix.Statfs(path, &st); err != nil {
		return fmt.Errorf("shared storage capacity unavailable: %w", err)
	}
	// Divide the requested minimum instead of multiplying filesystem counters.
	blocks := uint64(0)
	if st.Bsize > 0 {
		blocks = minBytes / uint64(st.Bsize)
		if minBytes%uint64(st.Bsize) != 0 {
			blocks++
		}
	}
	if st.Bsize <= 0 || st.Bavail < blocks || (st.Files > 0 && st.Ffree < minInodes) {
		return fmt.Errorf("shared storage free-space reserve reached (minimum %d bytes / %d inodes); execution stopped, durable files retained", minBytes, minInodes)
	}
	return nil
}
