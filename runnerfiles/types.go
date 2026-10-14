// Package runnerfiles transfers versioned files between the controller and
// standalone runners. Shared plans instead require explicitly provisioned direct
// storage mappings and are forbidden on the snapshot HTTP endpoints.
package runnerfiles

import (
	"errors"
	"fmt"
	"io/fs"
	"path"
	"strings"
)

const Missing = "missing"

const SharedMode = "shared"
const SharedCapability = "artex.io/shared-files"

// Snapshot preparation is deliberately bounded. Large/long-lived workspaces
// use shared storage or explicitly narrowed inputs instead of unbounded copies.
const MaxSnapshotFiles = 10000
const MaxSnapshotBytes int64 = 1 << 30
const MaxSnapshotEntries = 20000

var (
	ErrConflict = errors.New("file changed since the downloaded version")
	// SourceBusy means no conflicting publication happened: a source changed
	// while being snapshotted. Retry file synchronization, never the tool command.
	ErrSourceBusy = fmt.Errorf("source file is changing; retry synchronization: %w", ErrConflict)
	ErrLocked     = errors.New("file is leased by another execution")
	ErrPath       = errors.New("invalid or unauthorized file path")
	ErrDigest     = errors.New("file digest does not match")
	ErrLimit      = errors.New("snapshot transfer limit exceeded; narrow inputs or use shared storage")
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
	// Shared requires explicitly provisioned Kubernetes mappings. Never fall back
	// to snapshots: mixing publication protocols can overwrite live shared data.
	Mode   string  `json:"mode,omitempty"`
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
