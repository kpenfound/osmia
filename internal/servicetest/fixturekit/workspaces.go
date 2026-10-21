package fixturekit

import (
	"context"
	"sync"
	"time"
)

// FakeJJ stands in for the check of the jj on PATH: it reports version, and
// err when set. It leaves the real jj binary's actual version output and
// startup behaviour unverified.
type FakeJJ struct {
	Mu      sync.Mutex
	Version string
	Err     error
	Bounded bool
}

func (j *FakeJJ) Set(version string, err error) {
	j.Mu.Lock()
	defer j.Mu.Unlock()
	j.Version, j.Err = version, err
}

func (j *FakeJJ) Check(ctx context.Context) (string, error) {
	j.Mu.Lock()
	defer j.Mu.Unlock()
	deadline, ok := ctx.Deadline()
	j.Bounded = ok && time.Until(deadline) <= JJCheckTimeout
	return j.Version, j.Err
}

// lastBounded reports whether the latest check had a deadline no later than
// jjCheckTimeout from when it ran.
func (j *FakeJJ) LastBounded() bool {
	j.Mu.Lock()
	defer j.Mu.Unlock()
	return j.Bounded
}
