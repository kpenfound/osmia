package service

import (
	"context"

	"github.com/kpenfound/osmia/internal/servicetest/intakefixture"

	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	"testing"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"

	"github.com/kpenfound/osmia/internal/trace"
)

type heldChecks struct {
	links   []string
	entered chan []string
	release chan checkOutcome
}

func newHeldChecks() *heldChecks {
	return &heldChecks{entered: make(chan []string, 1), release: make(chan checkOutcome, 1)}
}

func (c *heldChecks) List(context.Context, string) ([]string, error) { return c.links, nil }

func (c *heldChecks) Check(ctx context.Context, _ string, links []string) (CheckResult, error) {
	return intakefixture.HeldCheck(ctx, links, c.entered, c.release)
}

func awaitCheck[T any](t *testing.T, ch <-chan T) T {
	t.Helper()
	return intakefixture.AwaitCheck(t, ch)
}

func finalCheckStarts(t *testing.T, repo *trace.Repository, stream config.WorkstreamID) []FinalCheckAttempt {
	t.Helper()
	return fixturekit.DocumentValues(t, repo, stream, func(doc trace.Document, start FinalCheckAttempt) bool {
		return start.Attempt > 0 && doc.ID == finalCheckAttemptDocument(start.Review, start.Attempt)
	})
}

func applyCheck(ctx context.Context, c coreadapter.Reconciler, op coreadapter.Operation) <-chan error {
	return intakefixture.ApplyCheck(ctx, c, op)
}
