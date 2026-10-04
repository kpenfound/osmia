package service

import "github.com/kpenfound/osmia/internal/trace"

// apiErrAsError lets consistentRead's fn, whose signature returns the
// standard error interface, carry an *APIError through a retry so the
// caller can recover it with errors.As.
type apiErrAsError struct{ api *APIError }

func (e apiErrAsError) Error() string { return e.api.Message }

// maxConsistentReadAttempts bounds consistentRead's retries. Each attempt
// only repeats in-memory assembly over the trace's own generation-cached
// scan, so it is cheap; the bound exists to give up and return the last
// attempt rather than retry forever under a sustained burst of commits.
const maxConsistentReadAttempts = 20

// consistentRead runs fn repeatedly until repository's generation does not
// change across one call, so every separate Repository read fn performs
// together reflects the trace at one generation: a commit that lands between
// two of fn's reads is retried from scratch, rather than assembled into a
// response that mixes state from before the commit with state from after
// it. fn must not write to repository.
func consistentRead[T any](repository *trace.Repository, fn func() (T, error)) (T, error) {
	for attempt := 0; attempt < maxConsistentReadAttempts-1; attempt++ {
		before := repository.Generation()
		v, err := fn()
		if repository.Generation() == before {
			return v, err
		}
	}
	return fn()
}
