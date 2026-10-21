package service

import (
	"testing"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/runtime"
	"github.com/kpenfound/osmia/internal/servicetest/intakefixture"
)

// archiveRecord returns stream's archive record from store's current
// snapshot, and whether it has one at all.
func archiveRecord(t *testing.T, store *runtime.Store, stream config.WorkstreamID) (runtime.Archive, bool) {
	t.Helper()
	return intakefixture.ArchiveRecord(t, store, stream)
}
