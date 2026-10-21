package service

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	reviewtools "github.com/kpenfound/osmia/internal/review"
	"github.com/kpenfound/osmia/internal/trace"
)

// reviewDiffTool serves the exact diff a unit review is pinned to: the
// candidate against its base, verified against the recorded digest.
func reviewDiffTool(cfg *config.Config, r *trace.Repository, scope coreadapter.Scope, identity UnitReviewIdentity) coreadapter.Tool {
	return reviewtools.DiffTool(reviewtools.PinnedDiff{About: "the unit's candidate against the feature branch commit it was built on", From: identity.Candidate.BaseRevision, To: identity.Candidate.Revision,
		Read: func(ctx context.Context) (string, error) {
			g, err := newUnitWorkspaces(cfg, r).Of(config.WorkstreamID(scope.Workstream))
			if err != nil {
				return "", err
			}
			diff, err := g.Diff(ctx, identity.Candidate.BaseRevision, identity.Candidate.Revision)
			if err != nil {
				return "", fmt.Errorf("candidate diff: %w", err)
			}
			sum := sha256.Sum256([]byte(diff))
			if hex.EncodeToString(sum[:]) != identity.DiffSHA256 {
				return "", errors.New("the candidate diff differs from the one this review is pinned to")
			}
			return diff, nil
		}})
}
