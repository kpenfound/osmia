package service

import (
	"strconv"
	"strings"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
)

// committeePerspectives is the review focus each committee perspective adds
// to a member's shed system prompt.
var committeePerspectives = map[string]string{
	"correctness": "Your primary perspective is correctness and charter compliance. Look for criteria that contradict each other, the charter or the handed design, behavior the spec leaves unobservable, and acceptance a reviewer could not verify from a unit's work. Explain the concrete failure behind each objection. You may raise any valid objection; this focus does not limit your authority.",
	"integration": "Your primary perspective is integration with the existing project. Test the plan against the architecture and decisions the knowledge base holds and the code in repo/: the subsystems each unit touches, the interfaces it changes and the order units depend on each other. For each gap, name the decision or code the plan relies on and the behavior that would break. You may raise any valid objection; this focus does not limit your authority.",
	"scope":       "Your primary perspective is scope and size. Challenge requirements the handed design does not ask for, expansion beyond it, and work that exceeds the assigned profile's practical capacity. Broad coherent units are valid; splitting must justify its coordination cost. Require a concrete consequence for each objection you raise, and accept sufficient work without demanding every possible refinement. You may raise any valid objection; this focus does not limit your authority.",
}

// committeeMember returns the member number of a committee agent.
func committeeMember(agent string) (int, bool) {
	rest, ok := strings.CutPrefix(agent, committeeAgentPrefix)
	if !ok {
		return 0, false
	}
	n, err := strconv.Atoi(rest)
	return n, err == nil && n > 0
}

// memberPerspective returns the review focus of a committee member's shed
// turns: committee.perspectives taken in turn by member number.
func memberPerspective(cfg *config.Config, agent string) string {
	n, ok := committeeMember(agent)
	perspectives := cfg.Committee.Perspectives
	if !ok || len(perspectives) == 0 {
		return ""
	}
	return committeePerspectives[perspectives[(n-1)%len(perspectives)]]
}

// agentProfile returns the profile of an agent's new turn. A committee member
// runs on committee.profiles taken in turn by member number, or on the first
// profile of that profile's fallback chain whose provider is not limited. The
// owner's committee override, an empty committee.profiles, a chain with every
// provider limited and every other agent take the role's effective profile.
func (s *Service) agentProfile(cfg *config.Config, role, agent string) (coreadapter.Profile, error) {
	n, member := committeeMember(agent)
	assigned := cfg.Committee.Profiles
	state, _ := s.effective()
	stored, _ := s.store.Snapshot()
	override := stored.Profiles[role]
	if role == committeeRole && member && len(assigned) > 0 && (override == "" || state.Profiles[role] != override) {
		for name := assigned[(n-1)%len(assigned)]; name != ""; name = cfg.Profiles[name].Fallback {
			limited := false
			for _, limit := range state.ProviderLimits {
				limited = limited || limit.Backend == cfg.Profiles[name].Agent
			}
			if !limited {
				return cfg.NamedProfile(name)
			}
		}
	}
	profile, _, err := s.roleExecution(cfg, role)
	return profile, err
}
