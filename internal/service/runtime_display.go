package service

import (
	"slices"

	"github.com/kpenfound/osmia/internal/runtime"
	"github.com/kpenfound/osmia/internal/trace"
)

// displayState is state as the API reports it: the stored name of the manager
// role in a role pause, a role's profile override and an archive's actor
// becomes the manager's display name. The stored state is not changed.
func displayState(state runtime.State) runtime.State {
	state.Pauses = slices.Clone(state.Pauses)
	for i := range state.Pauses {
		state.Pauses[i].Target.Role = trace.DisplayRole(state.Pauses[i].Target.Role)
	}
	if state.Profiles != nil {
		shown := make(map[string]string, len(state.Profiles))
		for role, profile := range state.Profiles {
			shown[trace.DisplayRole(role)] = profile
		}
		state.Profiles = shown
	}
	state.Archived = slices.Clone(state.Archived)
	for i := range state.Archived {
		state.Archived[i].Actor = trace.DisplayRole(state.Archived[i].Actor)
	}
	return state
}

// displayProfiles is profiles keyed by the roles' display names.
func displayProfiles(profiles map[string]EffectiveProfile) map[string]EffectiveProfile {
	shown := make(map[string]EffectiveProfile, len(profiles))
	for role, profile := range profiles {
		shown[trace.DisplayRole(role)] = profile
	}
	return shown
}
