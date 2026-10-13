//go:build !linux && !darwin && !windows

package runnerfiles

import "os"

// Refuse synchronization when the platform cannot prove there is no hard-link
// alias. Controller release targets have an implementation above.
func singleLink(*os.File, os.FileInfo) bool { return false }
