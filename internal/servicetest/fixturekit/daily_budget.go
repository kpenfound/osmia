package fixturekit

import (
	"time"
)

// Jump sets the clock; its next reading is one second after at.
func Jump(c *DemoClock, at time.Time) {
	c.Mu.Lock()
	defer c.Mu.Unlock()
	c.At = at
}
