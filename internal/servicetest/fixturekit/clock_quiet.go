package fixturekit

import "time"

// QuietFor sets the clock to d after DemoStart.
func (c *FixedClock) QuietFor(d time.Duration) {
	c.Mu.Lock()
	defer c.Mu.Unlock()
	c.At = DemoStart.Add(d)
}
