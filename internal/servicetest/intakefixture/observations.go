package intakefixture

import (
	"testing"
	"time"
)

// AwaitView waits until the read response satisfies done.
func AwaitView[T any](t *testing.T, read func() (T, error), done func(T) bool, failure string) T {
	t.Helper()
	deadline := time.Now().Add(DemoTimeout)
	for {
		view, err := read()
		Must(t, err)
		if done(view) {
			return view
		}
		if time.Now().After(deadline) {
			t.Fatalf(failure, view)
		}
		time.Sleep(20 * time.Millisecond)
	}
}
