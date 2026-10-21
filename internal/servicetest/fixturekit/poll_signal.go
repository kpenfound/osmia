package fixturekit

import (
	"testing"
	"time"
)

func AwaitPollSignal(t *testing.T, ch <-chan struct{}) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(DemoTimeout):
		t.Fatal("network observer did not reach its gate")
	}
}
