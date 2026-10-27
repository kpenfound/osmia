package service

import (
	"fmt"
	"slices"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/lifecycle"
	"github.com/kpenfound/osmia/internal/trace"
)

// projectRemovalGate requires resolution evidence for this project's members;
// another project's unfinished member never prevents removing this one.
func (s *Service) projectRemovalGate(id config.ProjectID) *APIError {
	refuse := func(group config.WorkgroupID) *APIError {
		return &APIError{Code: Validation, Message: fmt.Sprintf("project %s has an unresolved member in workgroup %s; resolve it before removing the project", id, group)}
	}
	_, active := s.runtimeOf(id)
	if active != nil {
		r := active.repository
		groups, _, err := trace.Workgroups(r)
		if err != nil {
			return &APIError{Code: Internal, Message: "cannot check project workgroups before removal"}
		}
		streams, err := r.Workstreams()
		if err != nil {
			return &APIError{Code: Internal, Message: "cannot check project members before removal"}
		}
		for _, stream := range streams {
			group, err := r.CrossRepoWorkgroupOf(stream)
			if err != nil {
				return &APIError{Code: Internal, Message: "cannot check project membership before removal"}
			}
			if group != "" {
				view, err := paneWorkgroupAt(r, stream, group)
				if err != nil || view == nil {
					return &APIError{Code: Internal, Message: fmt.Sprintf("cannot check membership in workgroup %s", group)}
				}
				if !slices.Contains(view.Members, stream) {
					continue
				}
			}
			if group == "" {
				if g, ok := trace.WorkgroupOf(groups, stream); ok {
					group = g.ID
				}
			}
			if group == "" {
				continue
			}
			done, err := resolved(r, stream)
			if err != nil {
				return &APIError{Code: Internal, Message: fmt.Sprintf("cannot check member resolution in workgroup %s", group)}
			}
			if !done {
				return refuse(group)
			}
		}
	}
	state, _ := s.effective()
	for _, a := range state.Archived {
		if a.Project == id && a.Workgroup != "" && !lifecycle.ArchiveResolved(a) {
			return refuse(a.Workgroup)
		}
	}
	return nil
}
