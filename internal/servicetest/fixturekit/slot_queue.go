package fixturekit

import "context"

// QueueSlot starts an asynchronous slot acquisition for a capacity assertion.
func QueueSlot(ctx context.Context, acquire func(context.Context) (func(), error)) <-chan func() {
	acquired := make(chan func(), 1)
	go func() { release, _ := acquire(ctx); acquired <- release }()
	return acquired
}
