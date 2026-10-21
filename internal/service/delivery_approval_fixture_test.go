package service

import (
	"context"
	"testing"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/servicetest/intakefixture"
	"github.com/kpenfound/osmia/internal/trace"
)

func deliveryFixture(t *testing.T) (*shedFixture, config.WorkstreamID, *trace.Repository, FinalReport) {
	t.Helper()
	return deliveryFixtureWith(t, "")
}

// deliveryFixtureWith is deliveryFixture with extra configuration appended.
func deliveryFixtureWith(t *testing.T, extra string) (*shedFixture, config.WorkstreamID, *trace.Repository, FinalReport) {
	t.Helper()
	f, stream, repository, a := newFinalFixtureWith(t, "delivery", extra)
	report := intakefixture.SeedDelivery(t, repository, stream, func(unit string, files map[string]string) string {
		return mergeDirectly(t, f, repository, stream, unit, files)
	}, a.Pass, func(ctx context.Context) (FinalReport, error) {
		in, sealed, err := a.governing(ctx, stream)
		return FinalReport{Commit: in.Commit, Seal: in.Seal, SpecHash: sealed.SpecHash, Spec: in.Spec, Plan: in.Plan, Charter: in.Charter, Upstream: &sealed.Base}, err
	}, finalReviewActor, finalReportDocument, finalReportPath, finalReviewed)
	f.s.setSole(&activeProject{repository: repository})
	return f, stream, repository, report
}
