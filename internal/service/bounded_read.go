package service

import (
	"time"
)

// coreReadBound is how long a core read endpoint waits for a fresh result
// before it falls back to the last one it produced, so a request that
// cannot refresh within it answers from that snapshot, marked stale,
// instead of blocking past the bound.
const coreReadBound = time.Second

// StaleHeader marks a response assembled from an earlier snapshot because
// the fresh read did not finish within coreReadBound, or because the
// request itself ended first. Its value is always "true" when present.
const StaleHeader = "X-Osmia-Stale"
