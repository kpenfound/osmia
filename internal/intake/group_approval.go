package intake

import (
	"context"
	"errors"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/trace"
)

func GroupApprovalResponse(project config.ProjectID, stream config.WorkstreamID, decision trace.GroupApproval) RatifyResponse {
	out := RatifyResponse{Project: project, Workstream: stream, Sealing: "pending", Detail: "the owner approved the workgroup; member seal preparations are pending"}
	for _, member := range decision.Members {
		out.Members = append(out.Members, member.Workstream)
		if member.Workstream == stream {
			out.Round, out.Spec, out.Plan = member.Round, member.Spec, member.Plan
		}
	}
	if decision.Complete {
		out.Sealing = "succeeded"
		out.Detail = "every pinned member seal is complete; the workgroup is ratified"
	}
	return out
}

// GroupApprovalAsked reports whether the owner is being asked for the group
// approval of the cross-repo workgroup stream belongs to: the workgroup needs
// one, none is recorded, and the inbox lists the ratification card of every
// member, which is when the approval can be given. A workgroup still joining
// its members, or with an archived member, is not asked.
func (s *Controller) GroupApprovalAsked(ctx context.Context, repository *trace.Repository, stream config.WorkstreamID) (bool, error) {
	id, err := repository.CrossRepoWorkgroupOf(stream)
	if err != nil || id == "" {
		return false, err
	}
	status, err := repository.GroupApprovalStatusOf(id)
	if err != nil || !status.Required || status.Found {
		return false, err
	}
	g, _, err := repository.CrossRepoWorkgroup(id)
	if err != nil {
		return false, err
	}
	members, err := s.CrossRepoMemberRepos(ctx, g)
	if errors.Is(err, ErrWorkgroupJoining) {
		return false, nil
	}
	if err != nil || len(members) == 0 {
		return false, err
	}
	for _, m := range members {
		if s.ArchivedWorkstream != nil {
			if _, archived := s.ArchivedWorkstream(string(m.Workstream)); archived {
				return false, nil
			}
		}
		state, err := m.Project.Workflow(m.Workstream, trace.FeatureSubject)
		if err != nil {
			return false, err
		}
		if _, open, err := s.ratificationCard(m.Project, m.Workstream, state.Value); err != nil || !open {
			return false, err
		}
	}
	return true, nil
}
