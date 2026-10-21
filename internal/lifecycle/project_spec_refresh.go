package lifecycle

import (
	"context"
	"errors"
	"fmt"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/trace"
)

func (s *Controller) ProjectSpecRefreshPass(ctx context.Context, repository *trace.Repository) error {
	streams, err := repository.Workstreams()
	if err != nil {
		return err
	}
	for _, stream := range streams {
		if err := ctx.Err(); err != nil {
			return err
		}
		id, err := repository.CrossRepoWorkgroupOf(stream)
		if err != nil {
			return fmt.Errorf("workstream %s cross-repo workgroup: %w", stream, err)
		}
		if id == "" {
			continue
		}
		_, g, found, err := repository.CrossRepoProjectSpec(id)
		if errors.Is(err, trace.ErrLocked) {
			continue
		}
		if err != nil {
			return fmt.Errorf("workgroup %s: %w", id, err)
		}
		if !found || len(g.Members) < 2 || g.Conversation != nil && len(g.Members) == 2 || LeastWorkstream(g.Members[1:]) != stream {
			continue
		}
		lock := s.GroupApprovalLock(id)
		if !lock.TryLock() {
			continue
		}
		err = s.RefreshProjectSpec(ctx, repository, id)
		lock.Unlock()
		if canceled := ctx.Err(); canceled != nil {
			return canceled
		}
		if errors.Is(err, trace.ErrLocked) || errors.Is(err, errWorkgroupJoining) || errors.Is(err, trace.ErrMembershipChanged) || errors.Is(err, trace.ErrMemberRetired) {
			continue
		}
		if err != nil {
			return fmt.Errorf("workgroup %s project spec refresh: %w", id, err)
		}
	}
	return nil
}

// RefreshProjectSpec requires the group approval lock throughout the read and
// append, so ratification either sees this revision or suppresses the refresh.
func (s *Controller) RefreshProjectSpec(ctx context.Context, repository *trace.Repository, id config.WorkgroupID) error {
	_, g, _, err := repository.CrossRepoProjectSpec(id)
	if err != nil {
		return err
	}
	live, err := s.LiveWorkgroupMembers(g)
	if err != nil || len(live) != len(g.Members)-1 {
		return err
	}
	wt, g, err := s.ApprovalTrace(ctx, repository, id)
	if err != nil {
		return err
	}
	defer wt.Close()
	if _, _, approved, err := trace.ReadGroupApproval(wt, g.Members[0]); err != nil || approved {
		return err
	}
	spec, found, err := trace.ProjectSpecFor(wt, id)
	if err != nil || !found {
		return err
	}
	changed := false
	for _, member := range g.Members[1:] {
		if err := ctx.Err(); err != nil {
			return err
		}
		_, _, repo, api := s.ConversationTrace(string(member))
		if api != nil {
			if err := ctx.Err(); err != nil {
				return err
			}
			return fmt.Errorf("workstream %s: %s", member, api.Message)
		}
		content, graph, err := MemberSpecAndPlan(repo, member)
		if err != nil {
			return err
		}
		for i, slice := range spec.Slices {
			if slice.Workstream != member {
				continue
			}
			if slice.Spec == content && slice.Plan == graph {
				break
			}
			if MemberSliceRefreshAllowed(slice, content, graph) {
				spec.Slices[i].Spec, spec.Slices[i].Plan = content, graph
				changed = true
			}
			break
		}
	}
	if !changed {
		return nil
	}
	if err := s.Step("project-spec-refresh-read"); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := trace.RecordProjectSpec(ctx, wt, g, spec, s.Now(), trace.Actor{Kind: "service", ID: "project-spec-refresh"}, "member-slice-refresh"); err != nil {
		return err
	}
	return s.Step("project-spec-refresh-recorded")
}
