package fixturekit

import (
	"strings"
	"testing"
)

// Git runs git in dir and returns its output without the trailing newline.
func Git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	return strings.TrimSpace(DemoGit(t, dir, args...))
}
