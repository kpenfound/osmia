package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/runtime"
	"github.com/kpenfound/osmia/internal/trace"
)

// archive permanently retires a terminal workstream, retaining its title.
func (s *Service) archive(raw string) (ArchiveResponse, *APIError) {
	if a, ok := s.archivedWorkstream(raw); ok {
		return ArchiveResponse{Project: a.Project, Workstream: a.Workstream, Archived: true}, nil
	}
	project, stream, repository, api := s.conversationTrace(raw)
	if api != nil {
		return ArchiveResponse{}, api
	}
	state, err := repository.Workflow(stream, trace.FeatureSubject)
	if err != nil {
		return ArchiveResponse{}, &APIError{Internal, fmt.Sprintf("cannot read the state of workstream %s; check the trace repository", stream)}
	}
	if state.Value != DeliveredState && state.Value != AbandonedState {
		current := state.Value
		if current == "" {
			current = "handed"
		}
		return ArchiveResponse{}, &APIError{Conflict, fmt.Sprintf("workstream %s is %s; only a delivered or abandoned workstream can be archived, so abandon it first", stream, current)}
	}
	a, err := archiveTitle(repository, stream)
	if err != nil {
		return ArchiveResponse{}, &APIError{Internal, err.Error()}
	}
	if err := s.archiveLifecycle(repository, &a); err != nil {
		return ArchiveResponse{}, &APIError{Internal, err.Error()}
	}
	a.ArchivedAt = s.now()
	a.DeliveryLink, err = deliveryLinkFor(repository, stream)
	if err != nil {
		return ArchiveResponse{}, &APIError{Internal, err.Error()}
	}
	if a.Workgroup != "" {
		release, api := s.changeArchiveGuard(a.Workgroup)
		if api != nil {
			return ArchiveResponse{}, api
		}
		defer release()
	}
	var authority *trace.Repository
	var group trace.Workgroup
	if a.Workgroup != "" {
		authority, err = trace.OpenWorkgroupAuthority(s.current().Root, a.Workgroup)
		if err != nil {
			if authority != nil {
				authority.Close()
			}
			return ArchiveResponse{}, &APIError{Internal, err.Error()}
		}
		defer authority.Close()
		groups, _, readErr := trace.Workgroups(authority)
		if readErr != nil {
			return ArchiveResponse{}, &APIError{Internal, readErr.Error()}
		}
		for _, g := range groups {
			if g.ID == a.Workgroup {
				group = g
			}
		}
	}
	err = repository.WithMemberQuestionSnapshot(stream, func(snapshot trace.MemberQuestionSnapshot) error {
		if snapshot.Workgroup != a.Workgroup {
			return trace.ErrConflict
		}
		if snapshot.Feature.Value != DeliveredState && snapshot.Feature.Value != AbandonedState {
			return trace.ErrFeatureState
		}
		if snapshot.Feature.Value != a.State {
			return trace.ErrConflict
		}
		evidence, err := memberEvidenceFrom(snapshot.Documents)
		if err != nil {
			return err
		}
		a.ResolutionEvidence = evidence.resolution(snapshot.Feature.Value)
		a.Resolved = a.ResolutionEvidence != "unresolved"
		evidence.pinArchiveMerge(&a, s.about(repository).Project.Upstream)
		var mergeLink, publicationLink string
		for _, doc := range snapshot.Documents {
			switch doc.ID {
			case publicationDocument:
				var p DeliveryPublication
				if err := json.Unmarshal([]byte(doc.Content), &p); err != nil {
					return err
				}
				if p.Status == publicationOpened && p.PullRequest > 0 {
					publicationLink = p.URL
				}
			case mergeDocument:
				var merge DeliveryMerge
				if err := json.Unmarshal([]byte(doc.Content), &merge); err != nil {
					return err
				}
				mergeLink = merge.Upstream.Commit
			}
		}
		a.DeliveryLink = publicationLink
		if mergeLink != "" {
			a.DeliveryLink = mergeLink
		}
		if authority != nil && len(group.Members) > 1 && slices.Contains(group.Members[1:], stream) {
			if err := authority.PreserveMemberQuestionSnapshot(context.Background(), group, trace.CrossRepoMember{Workstream: stream, Project: repository}, snapshot, s.now(), serviceActor); err != nil {
				return err
			}
		}
		if err := s.step("member-retirement-snapshot"); err != nil {
			return err
		}
		if err := s.store.SetArchived(a); err != nil {
			return err
		}
		return s.step("member-archive-recorded")
	})
	if errors.Is(err, trace.ErrFeatureState) || errors.Is(err, trace.ErrConflict) {
		return ArchiveResponse{}, &APIError{Conflict, "workstream changed while archiving; check its state and retry"}
	}
	if api := archiveError(err); api != nil {
		return ArchiveResponse{}, api
	}
	return ArchiveResponse{Project: project, Workstream: stream, Archived: true}, nil
}

// deliveryLinkFor returns the delivery link to keep in a workstream's
// archive record: the recorded upstream commit of an owner-recorded merge,
// or, failing that, the pull request URL of the workstream's own
// publication. It returns "" when neither is recorded, such as for an
// abandoned workstream.
func deliveryLinkFor(repository *trace.Repository, stream config.WorkstreamID) (string, error) {
	if merge, found, err := deliveryMerge(repository, stream); err != nil {
		return "", err
	} else if found {
		return merge.Upstream.Commit, nil
	}
	if pub, found, err := currentOwnPublication(repository, stream); err != nil {
		return "", err
	} else if found {
		return pub.URL, nil
	}
	return "", nil
}

// unarchive refuses to reverse an archive, including one awaiting cleanup.
func (s *Service) unarchive(raw string) (ArchiveResponse, *APIError) {
	if _, ok := s.archivedWorkstream(raw); ok {
		return ArchiveResponse{}, &APIError{Conflict, "archive is permanent; the workstream cannot be restored"}
	}
	project, stream, _, api := s.conversationTrace(raw)
	if api != nil {
		return ArchiveResponse{}, api
	}
	return ArchiveResponse{Project: project, Workstream: stream}, nil
}

func (s *Service) archivedWorkstream(raw string) (runtime.Archive, bool) {
	if s.store == nil {
		return runtime.Archive{}, false
	}
	state, _ := s.store.Snapshot()
	for _, a := range state.Archived {
		if string(a.Workstream) == raw && s.current().Active(a.Project) {
			return a, true
		}
	}
	return runtime.Archive{}, false
}

func archiveTitle(repo *trace.Repository, stream config.WorkstreamID) (runtime.Archive, error) {
	list, err := repo.Statuses()
	if err != nil {
		return runtime.Archive{}, err
	}
	for _, w := range list {
		if w.Workstream != stream {
			continue
		}
		title := string(stream)
		if w.Status != nil && w.Status.Goal != "" {
			title = w.Status.Goal
		}
		return runtime.Archive{Project: repo.Project(), Workstream: stream, Title: title, State: w.State}, nil
	}
	return runtime.Archive{}, fmt.Errorf("cannot preserve title of workstream %s", stream)
}

func archiveError(err error) *APIError {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, runtime.ErrValidation):
		return &APIError{Validation, "the runtime settings refused the change: " + err.Error()}
	case errors.Is(err, runtime.ErrConflict):
		return &APIError{Conflict, "runtime file changed externally; restore it or restart the service"}
	}
	return &APIError{Internal, "cannot write the runtime settings; check the root's runtime.json"}
}

// archivedIn reports whether the runtime state in force archives stream.
func archivedIn(state runtime.State, stream config.WorkstreamID) bool {
	return slices.ContainsFunc(state.Archived, func(a runtime.Archive) bool { return a.Workstream == stream })
}
