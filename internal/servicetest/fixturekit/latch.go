package fixturekit

import "sync"

// Latch is released once; Done is closed from the first Release on. The
// zero value is ready to use.
type Latch struct {
	created, released sync.Once
	done              chan struct{}
}

// Done returns the channel Release closes.
func (l *Latch) Done() <-chan struct{} {
	l.created.Do(func() { l.done = make(chan struct{}) })
	return l.done
}

// Release closes Done. Further calls do nothing.
func (l *Latch) Release() {
	l.Done()
	l.released.Do(func() { close(l.done) })
}
