package service

import (
	"context"
	"github.com/kpenfound/osmia/internal/delivery"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	"github.com/kpenfound/osmia/internal/servicetest/intakefixture"
	"os"
	"path/filepath"
	"testing"

	"github.com/kpenfound/osmia/internal/plan"
	"github.com/kpenfound/osmia/internal/projectstore"
	"github.com/kpenfound/osmia/internal/trace"
)

var chiefViewOutcomes = []string{"cancelled", "owner merged", "current merge", "ready", "closed", "stale merge"}

// stageChiefViewAfter stages a chief of staff's documents before and after a
// member ends as mode names and returns the staged workspace and the trace.
func stageChiefViewAfter(t *testing.T, mode string) (workspace string, repo *trace.Repository) {
	t.Helper()
	ctx := context.Background()
	opts := fixtureAt(t, t.TempDir())
	cfg, err := projectstore.LoadConfig(opts.Config)
	intakefixture.Must(t, err)
	repo, err = trace.Create(ctx, cfg.Root, cfg.Project, fixturekit.DemoStart, ownerActor)
	intakefixture.Must(t, err)
	t.Cleanup(func() { repo.Close() })
	intakefixture.Must(t, repo.CreateWorkstream(ctx, intakefixture.Stream, fixturekit.DemoStart, ownerActor))
	for _, doc := range []struct{ id, path, content string }{{plan.SpecDocument, plan.SpecPath, "Private member spec"}, {plan.PlanDocument, plan.PlanPath, `{"version":2,"units":[]}`}, {"final-report", "final/report.json", "{}"}} {
		intakefixture.Must(t, repo.RecordDocuments(ctx, []trace.Document{{Header: trace.Header{Schema: "osmia.trace.document", Version: trace.Version, ID: doc.id, Revision: 1, Project: repo.Project(), Workstream: intakefixture.Stream, At: fixturekit.DemoStart, Actor: ownerActor, Cause: "fixture"}, Path: doc.path, Content: doc.content}}))
	}
	workspace = filepath.Join(t.TempDir(), "view")
	_, err = stageChiefDocuments(repo, intakefixture.Stream, workspace)
	intakefixture.Must(t, err)
	state := DeliveredState
	if mode == "cancelled" {
		state = AbandonedState
	}
	_, err = repo.SetFeatureState(ctx, trace.Header{Schema: "osmia.trace.transition", Version: trace.Version, ID: "terminal", Revision: 1, Project: repo.Project(), Workstream: intakefixture.Stream, At: fixturekit.DemoStart, Actor: ownerActor, Cause: "fixture"}, state, "Member outcome")
	intakefixture.Must(t, err)
	if mode == "owner merged" {
		recordResolutionMerge(t, repo, intakefixture.Stream)
	}
	if mode != "cancelled" && mode != "owner merged" {
		pr := 42
		if mode == "stale merge" {
			pr = 43
		}
		resolutionDocument(t, repo, intakefixture.Stream, publicationDocument, publicationPath, DeliveryPublication{Status: publicationOpened, PullRequest: pr})
		if mode != "ready" {
			outcome := ownPullRequestOutcomeMerged
			if mode == "closed" {
				outcome = ownPullRequestOutcomeClosed
			}
			resolutionDocument(t, repo, intakefixture.Stream, ownPullRequestOutcomeDocument, delivery.OwnPullRequestOutcomePath, ownPullRequestOutcome{PullRequest: 42, Outcome: outcome})
		}
	}
	_, err = stageChiefDocuments(repo, intakefixture.Stream, workspace)
	intakefixture.Must(t, err)
	return workspace, repo
}

func eachChiefViewOutcome(t *testing.T, check func(t *testing.T, mode string)) {
	for _, mode := range chiefViewOutcomes {
		t.Run(mode, func(t *testing.T) { check(t, mode) })
	}
}

func TestChiefActiveViewRetiresItsSpecAndPlanOnceTheMemberIsResolved(t *testing.T) {
	eachChiefViewOutcome(t, func(t *testing.T, mode string) {
		workspace, _ := stageChiefViewAfter(t, mode)
		retired := mode == "cancelled" || mode == "owner merged" || mode == "current merge"
		for _, path := range []string{plan.SpecPath, plan.PlanPath} {
			_, err := os.Stat(filepath.Join(workspace, path))
			if retired && !os.IsNotExist(err) || !retired && err != nil {
				t.Fatalf("staged %s: %v retired=%v", path, err, retired)
			}
		}
	})
}

func TestChiefActiveViewKeepsTheFinalReviewWhateverTheOutcome(t *testing.T) {
	eachChiefViewOutcome(t, func(t *testing.T, mode string) {
		workspace, _ := stageChiefViewAfter(t, mode)
		data, err := os.ReadFile(filepath.Join(workspace, "final/report.json"))
		intakefixture.Must(t, err)
		if string(data) != "{}" {
			t.Fatal("final review changed")
		}
	})
}

func TestChiefActiveViewLeavesTheHistoryOfItsDocumentsInTheTrace(t *testing.T) {
	eachChiefViewOutcome(t, func(t *testing.T, mode string) {
		_, repo := stageChiefViewAfter(t, mode)
		for _, id := range []string{plan.SpecDocument, plan.PlanDocument} {
			d, err := trace.Get[trace.Document](repo, intakefixture.Stream, id, 1)
			intakefixture.Must(t, err)
			if d.Content == "" {
				t.Fatal("history lost")
			}
		}
	})
}
