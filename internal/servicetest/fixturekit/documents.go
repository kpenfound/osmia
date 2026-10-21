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
