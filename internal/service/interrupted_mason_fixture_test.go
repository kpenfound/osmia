package service

import (
	"sync/atomic"
	"testing"

	"github.com/kpenfound/osmia/internal/servicetest/intakefixture"
)

func awaitFurtherPasses(t *testing.T, passed *atomic.Int64, n int64) {
	t.Helper()
	intakefixture.AwaitFurtherPasses(t, passed, n)
}
