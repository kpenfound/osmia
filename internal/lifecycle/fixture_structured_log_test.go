package lifecycle_test

import (
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
)

// syncBuffer is a bytes.Buffer safe for the concurrent writes a running
// service's background goroutines, such as the tailnet supervisor, make
// through its logger.
type syncBuffer = fixturekit.SyncBuffer

var decodeStructuredLines = fixturekit.DecodeStructuredLines
