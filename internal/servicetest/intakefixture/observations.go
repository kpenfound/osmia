package intakefixture

import (
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	"testing"
	"time"
)

// AwaitView waits until the read response satisfies done.
func AwaitView[T any](t *testing.T, read func() (T, error), done func(T) bool, failure string) T {
	t.Helper()
	var view T
	wake, release := fixturekit.Ticks(20 * time.Millisecond)
	defer release()
	fixturekit.Until(t, "the view to settle", wake, func() bool {
		var err error
		view, err = read()
		fixturekit.Must(t, err)
		return done(view)
	})
	return view
}
