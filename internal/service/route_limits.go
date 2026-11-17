package service

import (
	"context"
	"fmt"
	"slices"

	"github.com/kpenfound/osmia/internal/router"
	"github.com/kpenfound/osmia/internal/runtime"
	"github.com/kpenfound/osmia/internal/scheduler"
	"github.com/kpenfound/osmia/internal/trace"
)

// routePaused is the owner's pause on the targets of an exit that start agent
// work. A pause lifts, so the router holds such an exit without recording it
// and offers it again once the pause is gone. It returns why work is paused, or
// "".
func (s *Service) routePaused(_ context.Context, x router.Exit) (string, error) {
	if s.store == nil {
		return "", nil
	}
	state, _ := s.store.Snapshot()
	owner := slices.DeleteFunc(slices.Clone(state.Pauses), func(p runtime.Pause) bool { return p.Source != runtime.PauseOwner })
	if p, paused := scheduler.Pausing(owner, x.Project, x.Workstream); paused {
		return fmt.Sprintf("work is paused (%s): %s", p.Source, p.Reason), nil
	}
	return "", nil
}

// routeStops is the hard limit on the targets of an exit that start agent work:
// a pause in force on the workstream that is not the owner's (the daily
// budget's, a provider's usage limit or the loop guard's), or the loop guard's
// limit of sessions without progress already reached before its pass pauses the
// workstream. A target that fails it is refused, however it was reached. Jev's
// requests are sessions of the workstream, so the moves it routes count toward
// the limit. It returns why work cannot start, or "".
func (s *Service) routeStops(ctx context.Context, x router.Exit) (string, error) {
	if s.store == nil {
		return "", nil
	}
	state, _ := s.store.Snapshot()
	limits := slices.DeleteFunc(slices.Clone(state.Pauses), func(p runtime.Pause) bool { return p.Source == runtime.PauseOwner })
	if p, paused := scheduler.Pausing(limits, x.Project, x.Workstream); paused {
		return fmt.Sprintf("work is paused (%s): %s", p.Source, p.Reason), nil
	}
	limit := s.about(x.Repo).Loop.MaxSessions
	if limit == 0 {
		return "", nil
	}
	since, what, err := lastProgress(x.Repo, x.Workstream)
	if err != nil {
		return "", err
	}
	costs, err := trace.Read[trace.Cost](x.Repo, x.Workstream)
	if err != nil {
		return "", err
	}
	if n := sessionsSince(costs, x.Workstream, since); n >= limit {
		return fmt.Sprintf("the loop guard: %d agent sessions ran since the workstream's last progress, %s, reaching loop.max_sessions (%d)", n, what, limit), nil
	}
	return "", nil
}
