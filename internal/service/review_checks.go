package service

import (
	"context"
	"errors"
	"os"
	"path/filepath"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/isolation"
	"github.com/kpenfound/osmia/internal/trace"
)

// unitReviewerIdentity binds reads and checks to the queued turn's candidate,
// including answer turns that continue an earlier review.
func unitReviewerIdentity(r *trace.Repository, scope coreadapter.Scope) (UnitReviewIdentity, error) {
	stream := config.WorkstreamID(scope.Workstream)
	thread, err := r.Thread(stream, scope.Thread)
	if err != nil {
		return UnitReviewIdentity{}, err
	}
	for _, turn := range thread.Turns {
		if turn.Request.TurnID == scope.Turn {
			reader := &reviewers{masons: &masons{repository: r}}
			identity, err := reader.turnIdentity(stream, scope.Unit, turn)
			if err != nil {
				return identity, err
			}
			if identity.Subject != scope.Workstream+"/"+scope.Unit || identity.Candidate.Revision == "" {
				return identity, errors.New("review candidate does not match the turn")
			}
			return identity, nil
		}
	}
	return UnitReviewIdentity{}, errors.New("review turn not found")
}

func unitReviewerSelection(ctx context.Context, cfg *config.Config, r *trace.Repository, scope coreadapter.Scope, execution coreadapter.ExecutionSettings) (isolation.Selection, error) {
	identity, err := unitReviewerIdentity(r, scope)
	if err != nil {
		return isolation.Selection{}, err
	}
	provider, err := newUnitWorkspaces(cfg, r).of(config.WorkstreamID(scope.Workstream))
	if err != nil {
		return isolation.Selection{}, err
	}
	// A thread runs one turn at a time, so the exports of its earlier turns
	// are done with.
	exports := filepath.Join(cfg.Root.String(), reviewInputsDirectory, scope.Project, scope.Workstream, scope.Thread)
	dir := filepath.Join(exports, scope.Turn)
	if err := os.RemoveAll(exports); err != nil {
		return isolation.Selection{}, err
	}
	if err := provider.Export(ctx, identity.Candidate.Revision, dir); err != nil {
		return isolation.Selection{}, err
	}
	paths, err := viewPaths(dir)
	return isolation.Selection{Workspace: coreadapter.WorkspaceRequest{SourceDirectory: dir, Directory: dir, BaseRevision: identity.Candidate.Revision}, Paths: paths, Execution: execution}, err
}
