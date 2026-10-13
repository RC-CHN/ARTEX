// Package runnerfiles transfers versioned files between the controller and
// standalone runners. It deliberately does not emulate a shared POSIX filesystem.
package runnerfiles

import (
	"errors"
	"fmt"
	"io/fs"
	"path"
	"strings"
)

const Missing = "missing"

var (
	ErrConflict = errors.New("file changed since the downloaded version")
	// SourceBusy means no conflicting publication happened: a source changed
	// while being snapshotted. Retry file synchronization, never the tool command.
	ErrSourceBusy = fmt.Errorf("source file is changing; retry synchronization: %w", ErrConflict)
	ErrLocked     = errors.New("file is leased by another execution")
	ErrPath       = errors.New("invalid or unauthorized file path")
	ErrDigest     = errors.New("file digest does not match")
	ErrLimit      = errors.New("file exceeds configured transfer limit")
)

// Source belongs to the controller; a runner must never interpret it as a host
// mount. Target is the absolute path presented inside its execution sandbox.
type Mount struct {
	Name     string   `json:"name"`
	Source   string   `json:"source"`
	Target   string   `json:"target"`
	Writable []string `json:"writable,omitempty"`
}
type Plan struct {
	Mounts []Mount `json:"mounts"`
}
type Entry struct {
	Path   string `json:"path"`
	Digest string `json:"digest"`
	Size   int64  `json:"size"`
	Mode   uint32 `json:"mode"`
}

func validPath(p string) bool {
	if p == "." || !fs.ValidPath(p) || strings.ContainsAny(p, "\\\x00") || path.Clean(p) != p {
		return false
	}
	for _, segment := range strings.Split(p, "/") {
		if strings.HasPrefix(segment, ".artex-transfer-") {
			return false
		}
	}
	return true
}
func writable(m Mount, p string) bool {
	if !validPath(p) {
		return false
	}
	for _, prefix := range m.Writable {
		if prefix == "." || (validPath(prefix) && (p == prefix || strings.HasPrefix(p, prefix+"/"))) {
			return true
		}
	}
	return false
}
