package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/seal"
	"github.com/kpenfound/osmia/internal/trace"
	"github.com/kpenfound/osmia/internal/workspace"
)

// baseWaitSubject is the workflow subject that parks a workstream whose base
// is unavailable: waiting while it is parked, available otherwise.
const baseWaitSubject = "base-wait"

// baseUpstream moves the workstream named by raw onto upstream, answering
// its parked base as the owner's request decides.
func (s *Service) baseUpstream(ctx context.Context, raw string, req BaseUpstreamRequest) (BaseUpstreamResponse, *APIError) {
	_, stream, repository, api := s.conversationTrace(raw)
	if api != nil {
		return BaseUpstreamResponse{}, api
	}
	cfg := s.about(repository)
	g, err := featureWorkspaces(cfg, repository).of(stream)
	if err != nil {
		return BaseUpstreamResponse{}, &APIError{Internal, fmt.Sprintf("cannot open the workspaces of workstream %s; check the clone", stream)}
	}
	return s.moveOntoUpstream(ctx, cfg, repository, g, stream, req.Base)
}

// moveOntoUpstream is the owner's decision on a workstream parked because
// its base workstream base was abandoned: the workstream continues from
// upstream instead. It is refused unless upstream already holds the changes
// of base that the workstream holds, so moving drops none of them. It records
// a base observation pinned to upstream and releases the park in one
// transaction; the next drift rebase replays the workstream's own commits
// onto upstream and moves its seal there.
func (s *Service) moveOntoUpstream(ctx context.Context, cfg *config.Config, repository *trace.Repository, g workspace.Provider, stream, base config.WorkstreamID) (BaseUpstreamResponse, *APIError) {
	failed := &APIError{Internal, fmt.Sprintf("cannot move workstream %s onto upstream; check the trace repository", stream)}
	wait, err := repository.Workflow(stream, baseWaitSubject)
	if err != nil {
		return BaseUpstreamResponse{}, failed
	}
	feature, err := repository.Workflow(stream, trace.FeatureSubject)
	if err != nil {
		return BaseUpstreamResponse{}, failed
	}
	if wait.Value != "waiting" || feature.Value == AbandonedState || feature.Value == DeliveredState {
		return BaseUpstreamResponse{}, &APIError{Conflict, fmt.Sprintf("workstream %s is not parked on its base", stream)}
	}
	dependency, err := repository.WorkstreamBase(stream)
	if err != nil {
		return BaseUpstreamResponse{}, failed
	}
	if dependency.Base != base {
		return BaseUpstreamResponse{}, &APIError{Conflict, fmt.Sprintf("workstream %s builds on %s, not %s; read the inbox again", stream, dependency.Base, base)}
	}
	parent, err := repository.Workflow(base, trace.FeatureSubject)
	if err != nil {
		return BaseUpstreamResponse{}, failed
	}
	if parent.Value != AbandonedState {
		return BaseUpstreamResponse{}, &APIError{Conflict, fmt.Sprintf("base workstream %s is not abandoned; only a workstream whose own base was abandoned moves onto upstream", base)}
	}
	held, api := heldBaseCommit(ctx, repository, g, stream, base)
	if api != nil {
		return BaseUpstreamResponse{}, api
	}
	unreadable := &APIError{Internal, fmt.Sprintf("cannot fetch %s from %s; check the clone's remotes", cfg.Project.BaseBranch, cfg.Project.Upstream)}
	remote, err := g.Remote(ctx, cfg.Project.Upstream)
	if err != nil {
		return BaseUpstreamResponse{}, unreadable
	}
	commit, err := g.Fetch(ctx, remote, cfg.Project.BaseBranch)
	if err != nil {
		return BaseUpstreamResponse{}, unreadable
	}
	upstream := seal.Base{Remote: remote, Branch: cfg.Project.BaseBranch, Commit: commit}
	onto := fmt.Sprintf("%s/%s at %s", remote, upstream.Branch, commit)
	reason := fmt.Sprintf("the owner moved the workstream onto %s: its base workstream %s was abandoned", onto, base)
	if held != "" {
		integrated, err := g.Integrated(ctx, held, commit)
		if err != nil {
			return BaseUpstreamResponse{}, &APIError{Internal, fmt.Sprintf("cannot compare %s with %s; check the clone", held, onto)}
		}
		if !integrated {
			return BaseUpstreamResponse{}, &APIError{Conflict, fmt.Sprintf("%s does not hold the changes of base workstream %s at %s, which workstream %s builds on; land them upstream first, or abandon workstream %s", onto, base, held, stream, stream)}
		}
		reason += fmt.Sprintf(", and the changes of it at %s that the workstream builds on are in upstream", held)
	}
	data, err := json.Marshal(baseObservation{Base: upstream})
	if err != nil {
		return BaseUpstreamResponse{}, failed
	}
	_, revision, err := baseObservationAt(repository, stream)
	if err != nil {
		return BaseUpstreamResponse{}, failed
	}
	id := fmt.Sprintf("%s-%d", baseWaitSubject, wait.Version+1)
	h := trace.Header{Schema: "osmia.trace.document", Version: trace.Version, ID: baseObservationDocument, Revision: revision + 1, Project: repository.Project(), Workstream: stream, At: s.now(), Actor: ownerActor, Cause: id}
	doc := trace.Document{Header: h, Path: "base-observation.json", Content: string(data)}
	h.Schema, h.ID, h.Revision, h.Cause = "osmia.trace.transition", id, 1, "owner-base-upstream"
	tx := trace.Transaction{ExpectedVersion: wait.Version, Transition: trace.Transition{Header: h, Subject: baseWaitSubject, From: wait.Value, To: "available", Reason: reason},
		Events: []trace.Event{trace.Notice(id, "base", reason)}}
	if _, err := repository.RecordDocumentsWith(ctx, []trace.Document{doc}, tx); errors.Is(err, trace.ErrConflict) {
		return BaseUpstreamResponse{}, &APIError{Conflict, fmt.Sprintf("workstream %s changed while moving it onto upstream; read the inbox again", stream)}
	} else if err != nil {
		return BaseUpstreamResponse{}, failed
	}
	s.driftAsked.Store(true)
	return BaseUpstreamResponse{Project: repository.Project(), Workstream: stream, Base: base, Integrated: held, Upstream: upstream}, nil
}

// heldBaseCommit returns the commit of base that the workstream's history
// holds: the base its feature branch was last rebased onto, or, before the
// workstream is sealed, base's branch tip. It is "" when the workstream
// holds nothing of base: neither is sealed yet.
func heldBaseCommit(ctx context.Context, repository *trace.Repository, g workspace.Provider, stream, base config.WorkstreamID) (string, *APIError) {
	failed := &APIError{Internal, fmt.Sprintf("cannot read the branches of workstream %s; check the clone", stream)}
	_, _, sealed, err := seal.Latest(repository, stream)
	if err != nil {
		return "", failed
	}
	if sealed {
		tip, exists, err := g.Branch(ctx, featureBranch(stream))
		if err != nil {
			return "", failed
		}
		if !exists {
			return "", &APIError{Conflict, fmt.Sprintf("the clone has no feature branch %s to move onto upstream", featureBranch(stream))}
		}
		current, err := branchBase(ctx, repository, stream, g, tip)
		if err != nil {
			return "", failed
		}
		return current.Commit, nil
	}
	parent, _, found, err := seal.Latest(repository, base)
	if err != nil {
		return "", failed
	}
	if !found {
		return "", nil
	}
	tip, exists, err := g.Branch(ctx, parent.Branch)
	if err != nil {
		return "", failed
	}
	if !exists {
		return "", &APIError{Conflict, fmt.Sprintf("the clone has no branch %s of base workstream %s to compare with upstream; restore it, or abandon workstream %s", parent.Branch, base, stream)}
	}
	return tip, nil
}
