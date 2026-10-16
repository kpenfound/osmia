package service

import (
	"context"
	"errors"
	"fmt"
	"os"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/thread"
	"github.com/kpenfound/osmia/internal/trace"
)

var errWorkgroupJoining = errors.New("the workgroup is still joining its members")

// crossRepoMemberRepos resolves fixed historical membership to open repo
// handles. Approved identities locate retired members without their history;
// live members retain locator and trace validation. Unapproved groups remain
// fenced by the production join protocol. Shutdown reports cancellation.
func (s *Service) crossRepoMemberRepos(ctx context.Context, g trace.Workgroup) ([]trace.CrossRepoMember, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if len(g.Members) < 2 {
		return nil, errWorkgroupJoining
	}
	if members, ready, err := s.workgroupMembers(ctx, g); err != nil || ready {
		return members, err
	}
	if g.Conversation != nil {
		_, _, source, api := s.conversationTrace(string(g.Conversation.Workstream))
		if api != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			return nil, fmt.Errorf("workgroup conversation origin %s: %s", g.Conversation.Workstream, api.Message)
		}
		state, err := source.Workflow(g.Conversation.Workstream, designStartSubject)
		if err != nil {
			return nil, err
		}
		if state.Value != "active" && state.Value != "complete" {
			return nil, errWorkgroupJoining
		}
	}
	real := g.Members[1:]
	members := make([]trace.CrossRepoMember, 0, len(real))
	for _, m := range real {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		_, _, repo, api := s.conversationTrace(string(m))
		if api != nil {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			return nil, fmt.Errorf("workstream %s: %s", m, api.Message)
		}
		members = append(members, trace.CrossRepoMember{Workstream: m, Project: repo})
		id, err := repo.CrossRepoWorkgroupOf(m)
		if err != nil {
			return nil, err
		}
		if id != g.ID {
			return nil, errWorkgroupJoining
		}
		groups, _, err := trace.Workgroups(repo)
		if err != nil {
			return nil, err
		}
		if local, exists := trace.WorkgroupOf(groups, m); exists {
			if len(local.Members) != 1 {
				return nil, fmt.Errorf("workstream %s has conflicting local and shared membership", m)
			}
			// Member and project traces commit independently. A published
			// locator keeps routing valid, but admission awaits local removal.
			return nil, errWorkgroupJoining
		}
	}
	return members, nil
}

// nextReadyTurn returns the thread's oldest unfinished turn, and whether it
// is ready to dispatch: unclaimed, with every earlier turn of the thread
// already complete.
func nextReadyTurn(t trace.Thread) (trace.QueuedTurn, bool) {
	for _, q := range t.Turns {
		if q.CompletedAt.IsZero() {
			return q, q.Claim == nil
		}
	}
	return trace.QueuedTurn{}, false
}

// workgroupChiefDispatchPass runs the next queued turn, if one is ready, of
// each cross-repo workgroup's shared chief-of-staff thread, for every such
// workgroup one of repository's own workstreams belongs to. It reuses the
// exact production toolset and staging an ordinary chief-of-staff turn
// gets (Options.Threads), pointed at the workgroup's own trace instead of
// repository, opened only for this call. Only the workgroup's
// alphabetically-first live member drives it, so every
// member's own pass finding the same workgroup does not race to dispatch
// the same turn twice.
func (s *Service) workgroupChiefDispatchPass(ctx context.Context, repository *trace.Repository) error {
	if s.options.Threads == nil {
		return nil
	}
	streams, err := repository.Workstreams()
	if err != nil {
		return err
	}
	seen := map[config.WorkgroupID]bool{}
	for _, stream := range streams {
		if err := ctx.Err(); err != nil {
			return err
		}
		id, err := repository.CrossRepoWorkgroupOf(stream)
		if err != nil {
			return fmt.Errorf("workstream %s cross-repo workgroup: %w", stream, err)
		}
		if id == "" || seen[id] {
			continue
		}
		_, g, _, err := repository.CrossRepoProjectSpec(id)
		if errors.Is(err, trace.ErrLocked) {
			// Another member's own pass, or this workgroup's leader, holds
			// the workgroup trace open right now; try again next pass.
			continue
		}
		if err != nil {
			return fmt.Errorf("workgroup %s: %w", id, err)
		}
		if len(g.Members) < 2 {
			continue
		}
		leader, err := s.liveWorkgroupLeader(g)
		if err != nil {
			return err
		}
		if leader != stream {
			continue
		}
		// The retained design source can name this group in the leader's
		// repo too. Only the real leader consumes the group's dispatch slot.
		seen[id] = true
		if err := s.dispatchWorkgroupChiefTurn(ctx, g); err != nil {
			if errors.Is(err, trace.ErrLocked) {
				continue
			}
			return fmt.Errorf("workgroup %s chief turn: %w", id, err)
		}
	}
	return nil
}

// dispatchWorkgroupChiefTurn runs the next ready turn of g's shared
// chief-of-staff thread, if one is queued and unclaimed, through the same
// Options.Threads reconciler an ordinary chief-of-staff turn runs through,
// pointed at g's own trace, opened and closed for this call alone.
func (s *Service) dispatchWorkgroupChiefTurn(ctx context.Context, g trace.Workgroup) error {
	members, err := s.crossRepoMemberRepos(ctx, g)
	if errors.Is(err, errWorkgroupJoining) {
		return nil
	}
	if err != nil {
		return err
	}
	wt, err := trace.OpenWorkgroupTrace(s.current().Root, g.ID, members)
	if wt != nil && err != nil {
		wt.Close()
	}
	if err != nil {
		return err
	}
	defer wt.Close()
	host := g.Members[0]
	t, err := wt.Thread(host, trace.ChiefOfStaff)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	q, ready := nextReadyTurn(t)
	if !ready {
		return nil
	}
	reconciler, err := s.options.Threads(wt, s.current())
	if err != nil {
		return err
	}
	op, err := thread.TurnOperation(wt.Project(), "workgroup-chief-"+q.Request.TurnID, thread.TurnInput{Workstream: host, Agent: trace.ChiefOfStaff, Turn: q.Request.TurnID})
	if err != nil {
		return err
	}
	ctx, release := s.stoppable(ctx, wt.Project(), host)
	defer release()
	_, err = reconciler.Apply(ctx, op)
	return err
}
