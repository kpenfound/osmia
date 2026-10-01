package jev

import (
	"testing"
	"time"
)

func TestOutagesCoolDownAndASuccessClearsThem(t *testing.T) {
	now := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	var c coolDown
	unavailable := &failure{reason: ReasonUnavailable}
	for range outageThreshold - 1 {
		c.observe(now, unavailable)
	}
	if cooling, _ := c.cooling(now); cooling {
		t.Fatal("cooling before the outage threshold")
	}
	c.observe(now, &failure{reason: ReasonRejected})
	c.observe(now, unavailable)
	if cooling, until := c.cooling(now); !cooling || !until.Equal(now.Add(firstCoolDown)) {
		t.Fatalf("cooling = %v until %v", cooling, until)
	}
	later := now.Add(firstCoolDown)
	for range outageThreshold {
		c.observe(later, unavailable)
	}
	if _, until := c.cooling(later); !until.Equal(later.Add(2 * firstCoolDown)) {
		t.Fatalf("second episode until %v; want doubled backoff", until)
	}
	end := later.Add(2 * firstCoolDown)
	c.observe(end, &failure{reason: ReasonRateLimited, retryAfter: time.Hour})
	if _, until := c.cooling(end); !until.Equal(end.Add(maxCoolDown)) {
		t.Fatalf("rate limit until %v; want capped retry-after", until)
	}
	c.observe(end.Add(maxCoolDown), nil)
	if c.lastFailure() != nil || c.episodes != 0 {
		t.Fatal("success did not clear the outage")
	}
}
