package sandbox

import (
	"regexp"
	"strconv"
	"strings"
)

var versionPattern = regexp.MustCompile(`[0-9]+\.[0-9]+\.[0-9]+[0-9A-Za-z.+-]*`)

var releasePattern = regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+$`)

// firstVersion returns the first version number in output, or "".
func firstVersion(output string) string {
	return versionPattern.FindString(output)
}

// versionField returns the second word of the first line whose first word is
// "version:", which is how `dagger version` reports its release.
func versionField(output string) string {
	for _, line := range strings.Split(output, "\n") {
		if f := strings.Fields(line); len(f) >= 2 && f[0] == "version:" {
			return f[1]
		}
	}
	return ""
}

// atLeast reports whether the leading major.minor.patch of have is at least
// minimum. A prerelease or build suffix is ignored; a version that does not
// start with three numbers is never enough.
func atLeast(have, minimum string) bool {
	if i := strings.IndexAny(have, "-+"); i >= 0 {
		have = have[:i]
	}
	if !releasePattern.MatchString(have) || !releasePattern.MatchString(minimum) {
		return false
	}
	h, m := strings.Split(have, "."), strings.Split(minimum, ".")
	for i := range h {
		a, errA := strconv.Atoi(h[i])
		b, errB := strconv.Atoi(m[i])
		if errA != nil || errB != nil {
			return false
		}
		if a != b {
			return a > b
		}
	}
	return true
}
