package jev

import (
	"sync"
	"time"

	"github.com/kpenfound/busybees/core/ops"
)

// The cool-down policy. A rate limit, or outageThreshold consecutive
// transient failures, stops requests for a while; each further episode
// without a success between them doubles the wait up to maxCoolDown.
const (
	outageThreshold = 3
	firstCoolDown   = 30 * time.Second
	maxCoolDown     = 15 * time.Minute
)

// coolDown keeps an outage from becoming a retry storm: while it is cooling,
// judgments fall back without a request. Its zero value is ready.
type coolDown struct {
	pause    ops.CapacityPause
	mu       sync.Mutex
	failures int
	episodes int
	last     *failure
}

// cooling reports whether requests are held, and until when.
func (c *coolDown) cooling(now time.Time) (bool, time.Time) {
	paused, _ := c.pause.Check(now)
	return paused, c.pause.Until()
}

// observe records one request's outcome. A nil failure is a success.
func (c *coolDown) observe(now time.Time, f *failure) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if f == nil {
		c.failures, c.episodes, c.last = 0, 0, nil
		return
	}
	switch {
	case f.reason == ReasonRateLimited:
	case f.reason.transient():
		c.failures++
		if c.failures < outageThreshold {
			c.last = f
			return
		}
	default:
		// The provider answered; a refused or unusable request says
		// nothing about its availability.
		c.last = f
		return
	}
	backoff := firstCoolDown << min(c.episodes, 5)
	if backoff > maxCoolDown {
		backoff = maxCoolDown
	}
	resets := time.Time{}
	if f.retryAfter > 0 {
		resets = now.Add(f.retryAfter)
	}
	c.pause.Extend(now, ops.PauseUntil(now, resets, backoff, maxCoolDown))
	c.failures, c.last = 0, f
	c.episodes++
}

// lastFailure is the most recent failure since the last success.
func (c *coolDown) lastFailure() *failure {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.last
}
