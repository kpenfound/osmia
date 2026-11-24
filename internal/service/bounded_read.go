package service

import (
	"time"
)

const coreReadBound = time.Second

// StaleHeader marks a response assembled from an earlier snapshot because
// the fresh read did not finish within coreReadBound, or because the
// request itself ended first. Its value is always "true" when present.
const StaleHeader = "X-Osmia-Stale"
