package service

import (
	"fmt"
	"slices"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/lifecycle"
	"github.com/kpenfound/osmia/internal/runtime"
	"github.com/kpenfound/osmia/internal/trace"
)

// paneWorkgroup reads a live member's recorded workgroup identity and graph.
func paneWorkgroup(repo *trace.Repository, stream config.WorkstreamID) (*WorkgroupView, error) {
	id, err := repo.CrossRepoWorkgroupOf(stream)
	if err != nil {
		return nil, err
	}
	return paneWorkgroupAt(repo, stream, id)
}

// paneWorkgroupAt reads only project-level records. An archived member's
// trace may have been cleanup-deleted; its archive pins the workgroup ID,
// or migration supplies its one-member mapping in the project record.
func paneWorkgroupAt(repo *trace.Repository, stream config.WorkstreamID, id config.WorkgroupID) (*WorkgroupView, error) {
	if id != "" {
		spec, group, _, err := repo.CrossRepoProjectSpec(id)
		if err != nil {
			return nil, err
		}
		if group.ID != "" {
			if len(group.Members) < 2 {
				return nil, fmt.Errorf("workgroup %s has no target members", id)
			}
			// The first member of a cross-repo record is its synthetic trace host.
			return &WorkgroupView{ID: id, Members: group.Members[1:], Conversation: group.Conversation, Edges: append([]trace.Edge{}, spec.Graph...)}, nil
		}
	}
	groups, _, err := trace.Workgroups(repo)
	if err != nil {
		return nil, err
	}
	if group, ok := trace.WorkgroupOf(groups, stream); ok {
		spec, _, err := trace.ProjectSpecFor(repo, group.ID)
		if err != nil {
			return nil, err
		}
		return &WorkgroupView{ID: group.ID, Members: group.Members, Conversation: group.Conversation, Edges: append([]trace.Edge{}, spec.Graph...)}, nil
	}
	if id != "" {
		return nil, fmt.Errorf("workgroup %s is unavailable", id)
	}
	return nil, nil
}

func (s *Service) presentWorkstream(repo *trace.Repository, w *WorkstreamStatus) error {
	if w.Kind == "session" || w.Kind == "design" {
		return nil
	}
	group, err := paneWorkgroup(repo, w.Workstream)
	if err != nil {
		return err
	}
	w.Workgroup = group
	if err := s.markDroppedEdges(repo, w.Workstream, group); err != nil {
		return err
	}
	evidence, err := readMemberEvidence(repo, w.Workstream)
	if err != nil {
		return err
	}
	merged := w.State != nil && evidence.Resolution(*w.State) == "merged"
	open := evidence.Published && !(evidence.Outcome.PullRequest == evidence.Publication.PullRequest && evidence.Outcome.Outcome != "")
	_, paused := s.pausing(w.Project, w.Workstream)
	w.DisplayState = paneState(w.State, w.Wait, paused, open, merged)
	return nil
}

// markDroppedEdges names the edges of the workgroup that their dependent
// dropped. Each waiver lives in the dependent's own trace, which is stream's
// repository for stream's own edges and another project's for the rest.
func (s *Service) markDroppedEdges(repo *trace.Repository, stream config.WorkstreamID, group *WorkgroupView) error {
	if group == nil {
		return nil
	}
	_, projects := s.runtimes()
	for _, e := range group.Edges {
		home := repo
		if e.To != stream {
			home = nil
			for _, p := range projects {
				if p.repository == nil {
					continue
				}
				if streams, err := p.repository.Workstreams(); err == nil && slices.Contains(streams, e.To) {
					home = p.repository
					break
				}
			}
		}
		if home == nil {
			continue
		}
		dropped, err := lifecycle.EdgeWaived(home, e.To, e.From, e.Type)
		if err != nil {
			return err
		}
		if dropped {
			group.Dropped = append(group.Dropped, e)
		}
	}
	return nil
}

func presentArchivedWorkstream(repo *trace.Repository, w *WorkstreamStatus, a runtime.Archive) error {
	var err error
	w.Workgroup, err = paneWorkgroupAt(repo, w.Workstream, a.Workgroup)
	done := archiveResolved(a)
	if !done {
		w.Wait = fmt.Sprintf("archived member %s has no pinned merge or cancel evidence; workgroup remains unresolved", w.Workstream)
	}
	w.DisplayState = paneState(w.State, w.Wait, false, false, done && a.State == DeliveredState)
	return err
}

func paneState(state *string, wait string, paused, open, merged bool) string {
	if state != nil && *state == AbandonedState {
		return "cancelled"
	}
	if merged {
		return "delivered (merged)"
	}
	if wait != "" {
		return "blocked"
	}
	if paused {
		return "paused"
	}
	if open {
		return "PR open"
	}
	if state != nil {
		switch *state {
		case RatifiedState:
			return "ready"
		case BuildingState, AssembledState, DeliveredState:
			return "building"
		}
	}
	return "planning"
}
