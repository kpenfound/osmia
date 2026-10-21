package lifecycle

import (
	"context"
	"slices"

	"github.com/kpenfound/osmia/internal/config"
)

// pollOrder resumes after the last admitted stream so a full pool cannot
// repeatedly admit only the beginning of a large scan.
func pollOrder[T any](ctx context.Context, s *Controller, kind string, items []T, stream func(T) config.WorkstreamID) []T {
	if s.PollCursor == nil {
		return items
	}
	last := s.PollCursor(ctx, kind)
	i := slices.IndexFunc(items, func(item T) bool { return stream(item) == last })
	if i < 0 || i == len(items)-1 {
		return items
	}
	return append(slices.Clone(items[i+1:]), items[:i+1]...)
}
