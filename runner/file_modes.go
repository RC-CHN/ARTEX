package runner

import (
	"encoding/json"
	"path/filepath"
	"strings"

	"github.com/RC-CHN/ARTEX/runnerfiles"
)

// Snapshot CAS only protects snapshot writers. Do not admit a live shared
// writer over overlapping snapshot inputs/outputs (including fenced attempts).
func incompatibleFileModes(a, b json.RawMessage) bool {
	var left, right runnerfiles.Plan
	if len(a) == 0 || len(b) == 0 {
		return false
	}
	if json.Unmarshal(a, &left) != nil || json.Unmarshal(b, &right) != nil {
		return true
	}
	if (left.Mode == runnerfiles.SharedMode) == (right.Mode == runnerfiles.SharedMode) {
		return false
	}
	contains := func(parent, child string) bool {
		parent, child = filepath.Clean(parent), filepath.Clean(child)
		return parent == child || strings.HasPrefix(child, parent+string(filepath.Separator))
	}
	for _, x := range left.Mounts {
		for _, y := range right.Mounts {
			if x.Source != "" && y.Source != "" && (len(x.Writable) > 0 || len(y.Writable) > 0) && (contains(x.Source, y.Source) || contains(y.Source, x.Source)) {
				return true
			}
		}
	}
	return false
}
