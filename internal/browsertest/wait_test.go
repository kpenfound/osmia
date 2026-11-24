package browsertest

import (
	"testing"
	"time"
)

func awaitCondition(t *testing.T, what string, done func() bool) {
	t.Helper()
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()
	timeout := time.After(browserTimeout)
	for !done() {
		select {
		case <-ticker.C:
		case <-timeout:
			t.Fatalf("%s did not happen within %s", what, browserTimeout)
		}
	}
}
