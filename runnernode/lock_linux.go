//go:build linux

package runnernode

import (
	"fmt"
	"os"
	"syscall"
)

func lockNode(path string) (*os.File, error) {
	fd, e := syscall.Open(path, syscall.O_RDWR|syscall.O_CREAT|syscall.O_NOFOLLOW, 0600)
	if e != nil {
		return nil, e
	}
	f := os.NewFile(uintptr(fd), path)
	if e = syscall.Flock(fd, syscall.LOCK_EX|syscall.LOCK_NB); e != nil {
		f.Close()
		return nil, fmt.Errorf("another runner process already owns this data directory")
	}
	if e = f.Chmod(0600); e != nil {
		f.Close()
		return nil, e
	}
	return f, nil
}
