//go:build !darwin && !linux

package trace

import "syscall"

// statTimes reports no times where they are not known, so every check reads
// the file again.
func statTimes(st *syscall.Stat_t) (modified, changed int64, ok bool) {
	return 0, 0, false
}
