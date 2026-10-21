package executionjourney

import (
	"testing"

	"github.com/kpenfound/osmia/internal/servicetest/intakefixture"

	"github.com/kpenfound/osmia/internal/config"

	"github.com/kpenfound/osmia/internal/runtime"
)

func archiveRecord(t *testing.T, store *runtime.Store, stream config.WorkstreamID) (runtime.Archive, bool) {
	t.Helper()
	return intakefixture.ArchiveRecord(t, store, stream)
}
