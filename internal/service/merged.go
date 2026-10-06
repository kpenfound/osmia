package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/pulls"
	"github.com/kpenfound/osmia/internal/seal"
	"github.com/kpenfound/osmia/internal/trace"
)

const (
	// mergePath is the workstream document that records the owner's own
	// merge of the feature branch into upstream, with its trace record ID.
	mergePath     = "final/merge.json"
	mergeDocument = "final-merge"
	// mergeCause is the cause of the transitions the owner's merge records.
	mergeCause = "owner-merge"
)

// DeliveryMerge records a feature branch the owner merged into upstream
// outside the factory: the branch, its tip, and the upstream commit found
// holding that tip's changes.
type DeliveryMerge struct {
	Branch   string    `json:"branch"`
	Commit   string    `json:"commit"`
	Upstream seal.Base `json:"upstream"`
}

// recordMerge records the assembled workstream named by raw delivered by
// the owner's own merge of its feature branch into upstream.
func (s *Service) recordMerge(ctx context.Context, raw string) (MergeResponse, *APIError) {
	_, stream, repository, api := s.conversationTrace(raw)
	if api != nil {
		return MergeResponse{}, api
	}
	return s.mergedOutside(ctx, repository, stream)
}

// mergedOutside records the assembled workstream delivered by the owner's
// own merge of its feature branch into upstream. Upstream is fetched first
// and must already hold the changes of the branch tip: their merge is clean
// and changes nothing, so fast-forward, merge, squash and rebase merges
// count. Nothing is pushed and no pull request is opened. One
// commit records final/merge.json, the move to delivered with a notice for
// the chief of staff, and the refusal of a publication that waits for a
// GitHub token. While this service could still carry out a requested
// publication, the merge is refused until the publication has an outcome.
func (s *Service) mergedOutside(ctx context.Context, repository *trace.Repository, stream config.WorkstreamID) (MergeResponse, *APIError) {
	project := repository.Project()
	failed := &APIError{Internal, fmt.Sprintf("cannot record workstream %s merged; check the trace repository", stream)}
	feature, err := repository.Workflow(stream, trace.FeatureSubject)
	if err != nil {
		return MergeResponse{}, failed
	}
	if feature.Value != AssembledState {
		return MergeResponse{}, &APIError{Conflict, fmt.Sprintf("workstream %s is %s; only an assembled workstream is recorded merged", stream, featureState(feature.Value))}
	}
	publication, err := repository.Workflow(stream, publicationSubject)
	if err != nil {
		return MergeResponse{}, failed
	}
	waiting := 0
	if k, ok := strings.CutPrefix(publication.Value, "requested-"); ok {
		if pulls.Credentialed(s.options.PullRequests) {
			return MergeResponse{}, &APIError{Conflict, fmt.Sprintf("the publication of owner approval %s of workstream %s may still open a pull request; record the merge once osmia delivery shows its outcome", k, stream)}
		}
		if waiting, err = strconv.Atoi(k); err != nil {
			return MergeResponse{}, failed
		}
	}
	cfg := s.about(repository)
	g, err := featureWorkspaces(cfg, repository).of(stream)
	if err != nil {
		return MergeResponse{}, &APIError{Internal, fmt.Sprintf("cannot open the workspaces of workstream %s; check the clone", stream)}
	}
	branch := featureBranch(stream)
	tip, exists, err := g.Branch(ctx, branch)
	if err != nil {
		return MergeResponse{}, &APIError{Internal, fmt.Sprintf("cannot read the branches of workstream %s; check the clone", stream)}
	}
	if !exists {
		return MergeResponse{}, &APIError{Conflict, fmt.Sprintf("the clone has no feature branch %s to compare with upstream", branch)}
	}
	unreadable := &APIError{Internal, fmt.Sprintf("cannot fetch %s from %s; check the clone's remotes", cfg.Project.BaseBranch, cfg.Project.Upstream)}
	remote, err := g.Remote(ctx, cfg.Project.Upstream)
	if err != nil {
		return MergeResponse{}, unreadable
	}
	commit, err := g.Fetch(ctx, remote, cfg.Project.BaseBranch)
	if err != nil {
		return MergeResponse{}, unreadable
	}
	upstream := seal.Base{Remote: remote, Branch: cfg.Project.BaseBranch, Commit: commit}
	onto := fmt.Sprintf("%s/%s at %s", remote, upstream.Branch, commit)
	integrated, err := g.Integrated(ctx, tip, commit)
	if err != nil {
		return MergeResponse{}, &APIError{Internal, fmt.Sprintf("cannot compare %s with %s; check the clone", tip, onto)}
	}
	if !integrated {
		return MergeResponse{}, &APIError{Conflict, fmt.Sprintf("%s does not hold the changes of %s at %s; merge the branch into %s first", onto, branch, tip, upstream.Branch)}
	}
	record := DeliveryMerge{Branch: branch, Commit: tip, Upstream: upstream}
	data, err := json.Marshal(record)
	if err != nil {
		return MergeResponse{}, failed
	}
	reason := fmt.Sprintf("the owner merged %s at %s into upstream outside the factory; %s holds its changes", branch, tip, onto)
	h := trace.Header{Schema: "osmia.trace.document", Version: trace.Version, ID: mergeDocument, Revision: 1, Project: project, Workstream: stream, At: s.now(), Actor: ownerActor, Cause: mergeCause}
	doc := trace.Document{Header: h, Path: mergePath, Content: string(data)}
	h.Schema, h.ID = "osmia.trace.transition", DeliveredState
	txs := []trace.Transaction{{ExpectedVersion: feature.Version,
		Transition: trace.Transition{Header: h, Subject: trace.FeatureSubject, From: feature.Value, To: DeliveredState, Reason: reason},
		Events:     []trace.Event{trace.Notice(DeliveredState, "state", fmt.Sprintf("Workstream delivered: the owner merged %s into %s/%s outside the factory, at %s. No pull request was opened.", branch, remote, upstream.Branch, commit))}}}
	if waiting > 0 {
		transition, _ := publishIDs(waiting)
		h.ID = transition + "-refused"
		txs = append(txs, trace.Transaction{ExpectedVersion: publication.Version,
			Transition: trace.Transition{Header: h, Subject: publicationSubject, From: publication.Value, To: fmt.Sprintf("refused-%d", waiting), Reason: fmt.Sprintf("owner approval %d was not published: the owner merged the branch into upstream outside the factory", waiting)}})
	}
	if _, err := repository.RecordDocumentsWith(ctx, []trace.Document{doc}, txs...); errors.Is(err, trace.ErrConflict) {
		return MergeResponse{}, &APIError{Conflict, fmt.Sprintf("workstream %s changed while recording its merge; check osmia status and retry", stream)}
	} else if err != nil {
		return MergeResponse{}, failed
	}
	return MergeResponse{Project: project, Workstream: stream, State: DeliveredState, Merge: record, Refused: waiting}, nil
}

// deliveryMerge returns the owner's recorded merge of the workstream's
// feature branch into upstream, and whether one is recorded.
func deliveryMerge(repository *trace.Repository, stream config.WorkstreamID) (DeliveryMerge, bool, error) {
	docs, err := trace.Read[trace.Document](repository, stream)
	if err != nil {
		return DeliveryMerge{}, false, err
	}
	for _, d := range slices.Backward(docs) {
		if d.ID != mergeDocument {
			continue
		}
		var merge DeliveryMerge
		if err := json.Unmarshal([]byte(d.Content), &merge); err != nil {
			return DeliveryMerge{}, false, fmt.Errorf("%s revision %d: %w", mergePath, d.Revision, err)
		}
		return merge, true, nil
	}
	return DeliveryMerge{}, false, nil
}
