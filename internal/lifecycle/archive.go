package lifecycle

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"

	"github.com/kpenfound/osmia/internal/api"
	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/runtime"
	"github.com/kpenfound/osmia/internal/trace"
)

// archiveRequest says who archives. The chief of staff archives only while
// eligible holds, which is evaluated again on the snapshot that commits the
// archive.
type archiveRequest struct {
	actor    string
	eligible func(state string, evidence MemberEvidence) bool
}

// Archive permanently retires a terminal workstream, retaining its title.
func (s *Controller) Archive(raw string) (ArchiveResponse, *APIError) {
	return s.archive(raw, archiveRequest{})
}

// ArchiveOnMerge archives a delivered workstream as the chief of staff,
// through the owner's archive, while the outcome recorded for its current
// own pull request is merged. It writes nothing when that no longer holds,
// when the workstream is archived already, or when the archive cannot
// proceed now; the caller retries on a later pass.
func (s *Controller) ArchiveOnMerge(raw string) (ArchiveResponse, *APIError) {
	return s.archive(raw, archiveRequest{actor: runtime.ArchiveActorChief, eligible: mergedOwnPullRequest})
}

func mergedOwnPullRequest(state string, evidence MemberEvidence) bool {
	return state == DeliveredState && evidence.OwnPullRequestMerged()
}

// ArchivedBy names who archived a: the owner unless the record names the
// chief of staff.
func ArchivedBy(a runtime.Archive) string {
	if a.Actor == runtime.ArchiveActorChief {
		return api.ArchivedByChief
	}
	return api.ArchivedByOwner
}

func (s *Controller) archive(raw string, req archiveRequest) (ArchiveResponse, *APIError) {
	if a, ok := s.ArchivedWorkstream(raw); ok {
		return ArchiveResponse{Project: a.Project, Workstream: a.Workstream, Archived: true, ArchivedBy: ArchivedBy(a)}, nil
	}
	project, stream, repository, apiErr := s.ConversationTrace(raw)
	if apiErr != nil {
		return ArchiveResponse{}, apiErr
	}
	state, err := repository.Workflow(stream, trace.FeatureSubject)
	if err != nil {
		return ArchiveResponse{}, &APIError{Code: Internal, Message: fmt.Sprintf("cannot read the state of workstream %s; check the trace repository", stream)}
	}
	if req.eligible != nil {
		if session, err := isSession(repository, stream); err != nil {
			return ArchiveResponse{}, &APIError{Code: Internal, Message: err.Error()}
		} else if session {
			return ArchiveResponse{}, &APIError{Code: Conflict, Message: fmt.Sprintf("workstream %s is a session; the chief of staff does not archive sessions", stream)}
		}
		evidence, err := ReadMemberEvidence(repository, stream)
		if err != nil {
			return ArchiveResponse{}, &APIError{Code: Internal, Message: err.Error()}
		}
		if !req.eligible(state.Value, evidence) {
			return ArchiveResponse{}, &APIError{Code: Conflict, Message: fmt.Sprintf("workstream %s has no merged pull request of its own to archive on", stream)}
		}
		if err := s.Step("auto-archive-prepared"); err != nil {
			return ArchiveResponse{}, &APIError{Code: Internal, Message: err.Error()}
		}
	}
	if state.Value != DeliveredState && state.Value != AbandonedState {
		current := state.Value
		if current == "" {
			current = "handed"
		}
		return ArchiveResponse{}, &APIError{Code: Conflict, Message: fmt.Sprintf("workstream %s is %s; only a delivered or abandoned workstream can be archived, so abandon it first", stream, current)}
	}
	a, err := ArchiveTitle(repository, stream)
	if err != nil {
		return ArchiveResponse{}, &APIError{Code: Internal, Message: err.Error()}
	}
	a.Actor = req.actor
	if err := s.ArchiveLifecycle(repository, &a); err != nil {
		return ArchiveResponse{}, &APIError{Code: Internal, Message: err.Error()}
	}
	a.ArchivedAt = s.Now()
	a.DeliveryLink, err = DeliveryLinkFor(repository, stream)
	if err != nil {
		return ArchiveResponse{}, &APIError{Code: Internal, Message: err.Error()}
	}
	if a.Workgroup != "" {
		release, apiErr := s.ChangeArchiveGuard(a.Workgroup)
		if apiErr != nil {
			return ArchiveResponse{}, apiErr
		}
		defer release()
	}
	var authority *trace.Repository
	var group trace.Workgroup
	if a.Workgroup != "" {
		authority, err = trace.OpenWorkgroupAuthority(s.Current().Root, a.Workgroup)
		if err != nil {
			if authority != nil {
				authority.Close()
			}
			return ArchiveResponse{}, &APIError{Code: Internal, Message: err.Error()}
		}
		defer authority.Close()
		groups, _, readErr := trace.Workgroups(authority)
		if readErr != nil {
			return ArchiveResponse{}, &APIError{Code: Internal, Message: readErr.Error()}
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
		evidence, err := MemberEvidenceFrom(snapshot.Documents)
		if err != nil {
			return err
		}
		if req.eligible != nil {
			if !req.eligible(snapshot.Feature.Value, evidence) {
				return trace.ErrConflict
			}
			// An archive made since the selection stays as it was recorded.
			if current, _ := s.Store().Snapshot(); ArchivedIn(current, stream) {
				return nil
			}
		}
		a.ResolutionEvidence = evidence.Resolution(snapshot.Feature.Value)
		a.Resolved = a.ResolutionEvidence != "unresolved"
		evidence.PinArchiveMerge(&a, s.About(repository).Project.Upstream)
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
		// An automatic archive links the pull request that merged, which its
		// conversation entry names.
		if mergeLink != "" && req.eligible == nil {
			a.DeliveryLink = mergeLink
		}
		if authority != nil && len(group.Members) > 1 && slices.Contains(group.Members[1:], stream) {
			if err := authority.PreserveMemberQuestionSnapshot(context.Background(), group, trace.CrossRepoMember{Workstream: stream, Project: repository}, snapshot, s.Now(), serviceActor); err != nil {
				return err
			}
		}
		if err := s.Step("member-retirement-snapshot"); err != nil {
			return err
		}
		if err := s.Store().SetArchived(a); err != nil {
			return err
		}
		return s.Step("member-archive-recorded")
	})
	if errors.Is(err, trace.ErrFeatureState) || errors.Is(err, trace.ErrConflict) {
		return ArchiveResponse{}, &APIError{Code: Conflict, Message: "workstream changed while archiving; check its state and retry"}
	}
	if apiErr := ArchiveError(err); apiErr != nil {
		return ArchiveResponse{}, apiErr
	}
	if recorded, ok := s.ArchivedWorkstream(string(stream)); ok {
		a = recorded
	}
	return ArchiveResponse{Project: project, Workstream: stream, Archived: true, ArchivedBy: ArchivedBy(a)}, nil
}

// DeliveryLinkFor returns the delivery link to keep in a workstream's
// Archive record: the recorded upstream commit of an owner-recorded merge,
// or, failing that, the pull request URL of the workstream's own
// Publication. It returns "" when neither is recorded, such as for an
// abandoned workstream.
func DeliveryLinkFor(repository *trace.Repository, stream config.WorkstreamID) (string, error) {
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

// Unarchive refuses to reverse an archive, including one awaiting cleanup.
func (s *Controller) Unarchive(raw string) (ArchiveResponse, *APIError) {
	if _, ok := s.ArchivedWorkstream(raw); ok {
		return ArchiveResponse{}, &APIError{Code: Conflict, Message: "archive is permanent; the workstream cannot be restored"}
	}
	project, stream, _, api := s.ConversationTrace(raw)
	if api != nil {
		return ArchiveResponse{}, api
	}
	return ArchiveResponse{Project: project, Workstream: stream}, nil
}
func (s *Controller) ArchivedWorkstream(raw string) (runtime.Archive, bool) {
	if s.Store() == nil {
		return runtime.Archive{}, false
	}
	state, _ := s.Store().Snapshot()
	for _, a := range state.Archived {
		if string(a.Workstream) == raw && s.Current().Active(a.Project) {
			return a, true
		}
	}
	return runtime.Archive{}, false
}
func ArchiveTitle(repo *trace.Repository, stream config.WorkstreamID) (runtime.Archive, error) {
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
func ArchiveError(err error) *APIError {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, runtime.ErrValidation):
		return &APIError{Code: Validation, Message: "the runtime settings refused the change: " + err.Error()}
	case errors.Is(err, runtime.ErrConflict):
		return &APIError{Code: Conflict, Message: "runtime file changed externally; restore it or restart the service"}
	}
	return &APIError{Code: Internal, Message: "cannot write the runtime settings; check the root's runtime.json"}
}

// ArchivedIn reports whether the runtime state in force archives stream.
func ArchivedIn(state runtime.State, stream config.WorkstreamID) bool {
	return slices.ContainsFunc(state.Archived, func(a runtime.Archive) bool {
		return a.Workstream == stream
	})
}
