package fixturekit

import (
	"sync"
	"time"
)

type FixedClock struct {
	Mu sync.Mutex
	At time.Time
}

func (c *FixedClock) Now() time.Time { c.Mu.Lock(); defer c.Mu.Unlock(); return c.At }

func (c *FixedClock) Advance(d time.Duration) { c.Mu.Lock(); defer c.Mu.Unlock(); c.At = c.At.Add(d) }
