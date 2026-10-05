package trace

import "syscall"

// statTimes returns the modification and status change times of st, in
// nanoseconds.
func statTimes(st *syscall.Stat_t) (modified, changed int64, ok bool) {
	return st.Mtim.Nano(), st.Ctim.Nano(), true
}
