package readview

import (
	"slices"
)

const maxConsistentReadAttempts = 20

// Consistent runs fn repeatedly until none of sources changes across one
// call, so every separate trace or runtime read fn performs together
// reflects one generation of each: a commit or a runtime mutation that lands
// between two of fn's reads is retried from scratch, rather than assembled
// into a response that mixes state from before it with state from after it.
// Pass a repository's Generation method for a read scoped to one trace, and
// also the service's runtime store's Generation when fn reads runtime state
// too, such as a workstream's archived flag alongside its trace-read
// status. fn must not write to anything sources covers.
func Consistent[T any](fn func() (T, error), sources ...func() uint64) (T, error) {
	for attempt := 0; attempt < maxConsistentReadAttempts-1; attempt++ {
		before := generationsOf(sources)
		v, err := fn()
		if slices.Equal(generationsOf(sources), before) {
			return v, err
		}
	}
	return fn()
}

func generationsOf(sources []func() uint64) []uint64 {
	out := make([]uint64, len(sources))
	for i, g := range sources {
		out[i] = g()
	}
	return out
}
