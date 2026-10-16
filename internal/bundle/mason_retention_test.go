package bundle_test

import (
	"context"
	"errors"
	"testing"

	"github.com/kpenfound/osmia/internal/bundle"
	"github.com/kpenfound/osmia/internal/plan"
	"github.com/kpenfound/osmia/internal/trace"
)

func TestMasonActiveBundleRefusesResolvedMemberAndRetainsHistory(t *testing.T) {
	for _, tc := range []struct {
		name, state, publication, outcome string
		owner, retired                    bool
	}{
		{name: "cancelled", state: "abandoned", retired: true},
		{name: "owner merged", state: "delivered", owner: true, retired: true},
		{name: "current merge", state: "delivered", publication: `{"status":"opened","pull_request":42}`, outcome: `{"pull_request":42,"outcome":"merged"}`, retired: true},
		{name: "ready", state: "delivered", publication: `{"status":"opened","pull_request":42}`},
		{name: "closed", state: "delivered", publication: `{"status":"opened","pull_request":42}`, outcome: `{"pull_request":42,"outcome":"closed"}`},
		{name: "stale merge", state: "delivered", publication: `{"status":"opened","pull_request":43}`, outcome: `{"pull_request":42,"outcome":"merged"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := sealed(t)
			ctx := context.Background()
			h := trace.Header{Schema: "osmia.trace.transition", Version: trace.Version, ID: "terminal", Revision: 1, Project: project, Workstream: first, At: timestamp, Actor: owner, Cause: "retention"}
			if _, err := f.repo.SetFeatureState(ctx, h, tc.state, "Member outcome"); err != nil {
				t.Fatal(err)
			}
			for _, d := range []trace.Document{document("final-publication", "final/publication.json", tc.publication), document("own-pull-request-outcome", "own-pull-request-outcome.json", tc.outcome)} {
				if d.Content != "" {
					if err := f.repo.RecordDocuments(ctx, []trace.Document{d}); err != nil {
						t.Fatal(err)
					}
				}
			}
			if tc.owner {
				if err := f.repo.RecordDocuments(ctx, []trace.Document{document("final-merge", "final/merge.json", `{}`)}); err != nil {
					t.Fatal(err)
				}
			}
			m, err := f.mason("resume")
			if tc.retired {
				if !errors.Is(err, bundle.ErrResolvedWorkstream) || m.Spec.Content != "" || m.Plan.Content != "" {
					t.Fatalf("resolved bundle=%+v err=%v", m, err)
				}
			} else if err != nil || m.Spec.Content != masonSpec || m.Plan.Content != masonPlan {
				t.Fatalf("unresolved bundle=%+v err=%v", m, err)
			}
			spec, err := trace.Get[trace.Document](f.repo, first, plan.SpecDocument, 1)
			if err != nil || spec.Content != masonSpec {
				t.Fatalf("historical spec=%+v %v", spec, err)
			}
			p, err := trace.Get[trace.Document](f.repo, first, plan.PlanDocument, 1)
			if err != nil || p.Content != masonPlan {
				t.Fatalf("historical plan=%+v %v", p, err)
			}
		})
	}
}
