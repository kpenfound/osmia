package fixturekit

import (
	"slices"

	"github.com/kpenfound/osmia/internal/api"
	"github.com/kpenfound/osmia/internal/runtime"
)

// pauseOf returns the effective pause of target, if any.
func PauseOf(rt api.RuntimeResponse, target runtime.Target) (runtime.Pause, bool) {
	i := slices.IndexFunc(rt.Effective.Pauses, func(p runtime.Pause) bool { return p.Target == target })
	if i < 0 {
		return runtime.Pause{}, false
	}
	return rt.Effective.Pauses[i], true
}
