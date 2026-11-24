package servicetest

import (
	"context"
	"testing"

	"github.com/kpenfound/osmia/internal/api"
	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/service"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	"github.com/kpenfound/osmia/internal/trace"
)

// EditDraft records spec and plan as the owner's edit of the workstream's draft
// through the API. An empty argument keeps that document as it is.
func EditDraft(t *testing.T, c *service.Client, stream config.WorkstreamID, spec, plan string) map[string]trace.Document {
	t.Helper()
	ctx := context.Background()
	docs, err := c.Documents(ctx, stream)
	fixturekit.Must(t, err)
	if spec == "" {
		spec = docs["spec"].Content
	}
	if plan == "" {
		plan = docs["plan"].Content
	}
	edited, err := c.EditDraft(ctx, stream, api.DraftEdit{SpecRevision: docs["spec"].Revision, PlanRevision: docs["plan"].Revision, Spec: spec, Plan: plan})
	fixturekit.Must(t, err)
	return edited
}
