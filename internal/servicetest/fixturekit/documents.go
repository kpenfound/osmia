package fixturekit

import (
	"encoding/json"
	"testing"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/trace"
)

// DocumentValues returns decoded document revisions accepted by include.
func DocumentValues[T any](t *testing.T, repo *trace.Repository, stream config.WorkstreamID, include func(trace.Document, T) bool) []T {
	t.Helper()
	docs, err := trace.Read[trace.Document](repo, stream)
	Must(t, err)
	var out []T
	for _, doc := range docs {
		var value T
		if json.Unmarshal([]byte(doc.Content), &value) == nil && include(doc, value) {
			out = append(out, value)
		}
	}
	return out
}

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
