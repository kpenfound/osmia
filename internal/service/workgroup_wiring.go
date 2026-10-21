package service

import (
	"context"
	"fmt"

	"github.com/kpenfound/osmia/internal/trace"
)

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
