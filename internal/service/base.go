package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/seal"
	"github.com/kpenfound/osmia/internal/trace"
	"github.com/kpenfound/osmia/internal/workspace"
)

var errBaseUnavailable = errors.New("workstream base is unavailable")

// resolveBase selects one immutable revision for sealing and rebasing. A
// dependency remains on the owner's local feature branch until integrated.
func (s *Service) resolveBase(ctx context.Context, cfg *config.Config, repo *trace.Repository, stream config.WorkstreamID, g workspace.Provider) (seal.Base, error) {
	dependency, err := repo.WorkstreamBase(stream)
	if err != nil {
		return seal.Base{}, err
	}
	remote, err := g.Remote(ctx, cfg.Project.Upstream)
	if err != nil {
		return seal.Base{}, err
	}
	upstream, err := g.Fetch(ctx, remote, cfg.Project.BaseBranch)
	if err != nil {
		return seal.Base{}, err
	}
	resolved := seal.Base{Remote: remote, Branch: cfg.Project.BaseBranch, Commit: upstream}
	if dependency.Base == "" {
		return resolved, nil
	}
	observed, revision, err := baseObservationAt(repo, stream)
	if err != nil {
		return seal.Base{}, err
	}
	if revision > 0 && observed.Base.Commit != "" && observed.Base.Workstream == "" {
		return resolved, nil
	}
	state, err := repo.Workflow(dependency.Base, trace.FeatureSubject)
	if err != nil {
		return seal.Base{}, err
	}
	if state.Value == AbandonedState {
		return seal.Base{}, fmt.Errorf("%w: base %s was abandoned", errBaseUnavailable, dependency.Base)
	}
	parent, _, found, err := seal.Latest(repo, dependency.Base)
	if err != nil {
		return seal.Base{}, err
	}
	if !found {
		return seal.Base{}, fmt.Errorf("%w: base %s has no sealed branch", errBaseUnavailable, dependency.Base)
	}
	tip, exists, err := g.Branch(ctx, parent.Branch)
	if err != nil {
		return seal.Base{}, err
	}
	if state.Value == DeliveredState {
		tip, err = deliveredBaseTip(repo, dependency.Base, tip)
		if err != nil {
			return seal.Base{}, err
		}
	}
	// A newly sealed branch starts on upstream too. Only a delivered parent's
	// published revision can establish integration, not its initial base commit.
	if exists && state.Value == DeliveredState {
		integrated, err := g.Ancestor(ctx, tip, upstream)
		if err != nil {
			return seal.Base{}, err
		}
		if integrated {
			return resolved, nil
		}
	}
	if state.Value == DeliveredState && s.options.PullRequests != nil {
		// The hosting record also detects squash and rebase merges, whose upstream
		// commit need not be an ancestor of the local feature tip.
		records, err := publications(repo, dependency.Base)
		if err != nil {
			return seal.Base{}, err
		}
		for _, publication := range records {
			if publication.Status != publicationOpened || publication.Upstream != cfg.Project.Upstream {
				continue
			}
			pulls, err := s.options.PullRequests.Find(ctx, publication.Upstream, publication.Fork, publication.Branch)
			if err != nil {
				return seal.Base{}, err
			}
			for _, pr := range pulls {
				if pr.Number == publication.PullRequest && pr.Merged && pr.HeadCommit == publication.Commit {
					return resolved, nil
				}
			}
		}
	}
	if !exists {
		return seal.Base{}, fmt.Errorf("%w: base branch %s is missing", errBaseUnavailable, parent.Branch)
	}
	return seal.Base{Remote: ".", Branch: parent.Branch, Commit: tip, Workstream: dependency.Base}, nil
}

// dependentBaseChanged reads local branch state without fetching on the scheduler path.
func dependentBaseChanged(ctx context.Context, cfg *config.Config, repo *trace.Repository, stream config.WorkstreamID) (bool, error) {
	dependency, err := repo.WorkstreamBase(stream)
	if err != nil || dependency.Base == "" {
		return false, err
	}
	_, _, sealed, err := seal.Latest(repo, stream)
	if err != nil || !sealed {
		return false, err
	}
	g, err := featureWorkspaces(cfg, repo).of(stream)
	if err != nil {
		return false, err
	}
	tip, exists, err := g.Branch(ctx, featureBranch(stream))
	if err != nil || !exists {
		return false, err
	}
	current, err := branchBase(ctx, repo, stream, g, tip)
	if err != nil {
		return false, err
	}
	observed, revision, err := baseObservationAt(repo, stream)
	if err != nil {
		return false, err
	}
	if revision > 0 && !observed.Unavailable && observed.Base != current {
		return true, nil
	}
	if current.Workstream == "" {
		return false, nil
	}
	tip, exists, err = g.Branch(ctx, featureBranch(dependency.Base))
	if err != nil || !exists {
		return false, err
	}
	state, err := repo.Workflow(dependency.Base, trace.FeatureSubject)
	if err != nil {
		return false, err
	}
	if state.Value == DeliveredState {
		tip, err = deliveredBaseTip(repo, dependency.Base, tip)
	}
	return tip != current.Commit, err
}

func baseWaiting(repo *trace.Repository, stream config.WorkstreamID) (bool, error) {
	state, err := repo.Workflow(stream, "base-wait")
	return state.Value == "waiting", err
}

func (s *Service) baseWaitPass(ctx context.Context, repo *trace.Repository) error {
	streams, err := repo.Workstreams()
	if err != nil {
		return err
	}
	for _, stream := range streams {
		dependency, err := repo.WorkstreamBase(stream)
		if err != nil {
			return err
		}
		if dependency.Base == "" {
			continue
		}
		feature, err := repo.Workflow(stream, trace.FeatureSubject)
		if err != nil {
			return err
		}
		if feature.Value == AbandonedState || feature.Value == DeliveredState {
			continue
		}
		own, _, sealed, err := seal.Latest(repo, stream)
		if err != nil {
			return err
		}
		parent := dependency.Base
		observed, revision, err := baseObservationAt(repo, stream)
		if err != nil {
			return err
		}
		if sealed && own.Base.Workstream == "" || revision > 0 && observed.Base.Commit != "" && observed.Base.Workstream == "" {
			parent = ""
		}
		reason := ""
		// An unavailable ancestor parks the whole dependent chain.
		for parent != "" {
			state, err := repo.Workflow(parent, trace.FeatureSubject)
			if err != nil {
				return err
			}
			if state.Value == AbandonedState {
				reason = fmt.Sprintf("base workstream %s was abandoned; the owner must choose how to continue", parent)
				break
			}
			sealed, _, found, err := seal.Latest(repo, parent)
			if err != nil {
				return err
			}
			if !found {
				reason = fmt.Sprintf("base workstream %s has no sealed branch yet", parent)
				break
			}
			g, err := featureWorkspaces(s.about(repo), repo).of(parent)
			if err != nil {
				return err
			}
			_, exists, err := g.Branch(ctx, sealed.Branch)
			if err != nil {
				return err
			}
			if !exists {
				reason = fmt.Sprintf("base branch %s is unavailable; the owner must restore it or abandon this workstream", sealed.Branch)
				break
			}
			if sealed.Base.Workstream == "" {
				break
			}
			next, err := repo.WorkstreamBase(parent)
			if err != nil {
				return err
			}
			parent = next.Base
		}
		state, err := repo.Workflow(stream, "base-wait")
		if err != nil {
			return err
		}
		next := "available"
		if reason != "" {
			next = "waiting"
		}
		if state.Value == next || state.Value == "" && next == "available" {
			continue
		}
		id := fmt.Sprintf("base-wait-%d", state.Version+1)
		if reason == "" {
			reason = "the workstream base is available again"
		}
		h := trace.Header{Schema: "osmia.trace.transition", Version: trace.Version, ID: id, Revision: 1, Project: repo.Project(), Workstream: stream, At: s.now(), Actor: foremanActor, Cause: "base-availability"}
		_, err = repo.Transact(ctx, trace.Transaction{ExpectedVersion: state.Version, Transition: trace.Transition{Header: h, Subject: "base-wait", From: state.Value, To: next, Reason: reason}, Events: []trace.Event{trace.Notice(id, "base", reason)}})
		if err != nil {
			return err
		}
	}
	return nil
}

func deliveredBaseTip(repo *trace.Repository, stream config.WorkstreamID, fallback string) (string, error) {
	records, err := publications(repo, stream)
	if err != nil {
		return "", err
	}
	for _, p := range records {
		if p.Status == publicationOpened {
			fallback = p.Commit
		}
	}
	return fallback, nil
}

// branchBase reads the newest recorded base whose rebase actually reached
// this branch. A final rebase may precede a later drift or final review.
func branchBase(ctx context.Context, repo *trace.Repository, stream config.WorkstreamID, g workspace.Provider, tip string) (seal.Base, error) {
	docs, err := trace.Read[trace.Document](repo, stream)
	if err != nil {
		return seal.Base{}, err
	}
	for _, d := range slices.Backward(docs) {
		if d.ID == finalRebaseDocument {
			var rebased FinalRebase
			if err := json.Unmarshal([]byte(d.Content), &rebased); err != nil {
				return seal.Base{}, err
			}
			included, err := g.Ancestor(ctx, rebased.Commit, tip)
			if err != nil {
				return seal.Base{}, err
			}
			if included {
				return rebased.Upstream, nil
			}
		}
		if d.ID == seal.DocumentID {
			sealed, err := seal.Parse([]byte(d.Content))
			return sealed.Base, err
		}
	}
	return seal.Base{}, errors.New("workstream has no recorded branch base")
}
