package service

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/kpenfound/osmia/internal/delivery"
	"github.com/kpenfound/osmia/internal/intake"
	"github.com/kpenfound/osmia/internal/plan"
	"github.com/kpenfound/osmia/internal/projectstore"
	"github.com/kpenfound/osmia/internal/seal"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	"github.com/kpenfound/osmia/internal/trace"
)

var managerViewOutcomes = []string{"cancelled", "owner merged", "current merge", "ready", "closed", "stale merge"}

func stageManagerViewAfter(t *testing.T, mode string) (workspace string, repo *trace.Repository) {
	t.Helper()
	ctx := context.Background()
	opts := fixtureAt(t, t.TempDir())
	cfg, err := projectstore.LoadConfig(opts.Config)
	fixturekit.Must(t, err)
	repo, err = trace.Create(ctx, cfg.Root, cfg.Project, fixturekit.DemoStart, ownerActor)
	fixturekit.Must(t, err)
	t.Cleanup(func() { repo.Close() })
	fixturekit.Must(t, repo.CreateWorkstream(ctx, fixturekit.Stream, fixturekit.DemoStart, ownerActor))
	for _, doc := range []struct{ id, path, content string }{{plan.SpecDocument, plan.SpecPath, "Private member spec"}, {plan.PlanDocument, plan.PlanPath, `{"version":2,"units":[]}`}, {"final-report", "final/report.json", "{}"}} {
		fixturekit.Must(t, repo.RecordDocuments(ctx, []trace.Document{{Header: trace.Header{Schema: "osmia.trace.document", Version: trace.Version, ID: doc.id, Revision: 1, Project: repo.Project(), Workstream: fixturekit.Stream, At: fixturekit.DemoStart, Actor: ownerActor, Cause: "fixture"}, Path: doc.path, Content: doc.content}}))
	}
	workspace = filepath.Join(t.TempDir(), "view")
	_, err = StageManagerDocuments(repo, fixturekit.Stream, workspace)
	fixturekit.Must(t, err)
	state := DeliveredState
	if mode == "cancelled" {
		state = AbandonedState
	}
	_, err = repo.SetFeatureState(ctx, trace.Header{Schema: "osmia.trace.transition", Version: trace.Version, ID: "terminal", Revision: 1, Project: repo.Project(), Workstream: fixturekit.Stream, At: fixturekit.DemoStart, Actor: ownerActor, Cause: "fixture"}, state, "Member outcome")
	fixturekit.Must(t, err)
	if mode == "owner merged" {
		fixturekit.ResolutionDocument(t, repo, fixturekit.Stream, delivery.MergeDocument, delivery.MergePath, DeliveryMerge{Branch: intake.FeatureBranch(fixturekit.Stream), Commit: "merged-feature", Upstream: seal.Base{Remote: "upstream", Branch: "main", Commit: "merged-upstream"}}, fixturekit.DemoStart, intake.OwnerActor)
	}
	if mode != "cancelled" && mode != "owner merged" {
		pr := 42
		if mode == "stale merge" {
			pr = 43
		}
		fixturekit.OwnerResolutionDocument(t, repo, fixturekit.Stream, publicationDocument, publicationPath, DeliveryPublication{Status: publicationOpened, PullRequest: pr})
		if mode != "ready" {
			outcome := delivery.OwnPullRequestOutcomeMerged
			if mode == "closed" {
				outcome = delivery.OwnPullRequestOutcomeClosed
			}
			fixturekit.OwnerResolutionDocument(t, repo, fixturekit.Stream, delivery.OwnPullRequestOutcomeDocument, delivery.OwnPullRequestOutcomePath, delivery.OwnPullRequestOutcome{PullRequest: 42, Outcome: outcome})
		}
	}
	_, err = StageManagerDocuments(repo, fixturekit.Stream, workspace)
	fixturekit.Must(t, err)
	return workspace, repo
}

func eachManagerViewOutcome(t *testing.T, check func(t *testing.T, mode string)) {
	for _, mode := range managerViewOutcomes {
		t.Run(mode, func(t *testing.T) { check(t, mode) })
	}
}

func TestManagerActiveViewRetiresItsSpecAndPlanOnceTheMemberIsResolved(t *testing.T) {
	eachManagerViewOutcome(t, func(t *testing.T, mode string) {
		workspace, _ := stageManagerViewAfter(t, mode)
		retired := mode == "cancelled" || mode == "owner merged" || mode == "current merge"
		for _, path := range []string{plan.SpecPath, plan.PlanPath} {
			_, err := os.Stat(filepath.Join(workspace, path))
			if retired && !os.IsNotExist(err) || !retired && err != nil {
				t.Fatalf("staged %s: %v retired=%v", path, err, retired)
			}
		}
	})
}

func TestManagerActiveViewKeepsTheFinalReviewWhateverTheOutcome(t *testing.T) {
	eachManagerViewOutcome(t, func(t *testing.T, mode string) {
		workspace, _ := stageManagerViewAfter(t, mode)
		data, err := os.ReadFile(filepath.Join(workspace, "final/report.json"))
		fixturekit.Must(t, err)
		if string(data) != "{}" {
			t.Fatal("final review changed")
		}
	})
}

func TestManagerActiveViewLeavesTheHistoryOfItsDocumentsInTheTrace(t *testing.T) {
	eachManagerViewOutcome(t, func(t *testing.T, mode string) {
		_, repo := stageManagerViewAfter(t, mode)
		for _, id := range []string{plan.SpecDocument, plan.PlanDocument} {
			d, err := trace.Get[trace.Document](repo, fixturekit.Stream, id, 1)
			fixturekit.Must(t, err)
			if d.Content == "" {
				t.Fatal("history lost")
			}
		}
	})
}
