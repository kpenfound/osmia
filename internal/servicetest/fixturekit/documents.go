package fixturekit

import (
	"testing"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/trace"
)

// ReopenTrace closes the offline repository and reopens it from the fixture's configuration.
func ReopenTrace(t *testing.T, cfg *config.Config, repository *trace.Repository) *trace.Repository {
	t.Helper()
	Must(t, repository.Close())
	return OpenTrace(t, cfg.Root, cfg.Project)
}

// OpenTrace opens an offline trace and closes it at test cleanup.
func OpenTrace(t *testing.T, root config.Root, project config.Project) *trace.Repository {
	t.Helper()
	reopened, err := trace.Open(root, project)
	Must(t, err)
	t.Cleanup(func() { reopened.Close() })
	return reopened
}
