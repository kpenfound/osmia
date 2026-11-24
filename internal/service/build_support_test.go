package service

import (
	"testing"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/servicetest/intakefixture"
	"github.com/kpenfound/osmia/internal/trace"
)

func approvedReview(t *testing.T, repository *trace.Repository, stream config.WorkstreamID, unit string) (trace.Document, UnitReviewResult) {
	t.Helper()
	f := &foreman{masons: &masons{repository: repository}}
	return intakefixture.ApprovedReview(t, stream, unit, f.approval)
}
