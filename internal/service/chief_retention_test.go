package service

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/plan"
	"github.com/kpenfound/osmia/internal/trace"
)

func TestChiefActiveViewRetiresOwnDocumentsAndPreservesHistory(t *testing.T) {
	for _, mode := range []string{"cancelled", "owner merged", "current merge", "ready", "closed", "stale merge"} {
		t.Run(mode, func(t *testing.T) {
			ctx := context.Background()
			opts := fixtureAt(t, t.TempDir())
			cfg, err := config.Load(opts.Config)
			must(t, err)
			repo, err := trace.Create(ctx, cfg.Root, cfg.Project, demoStart, ownerActor)
			must(t, err)
			defer repo.Close()
			must(t, repo.CreateWorkstream(ctx, stream, demoStart, ownerActor))
			for _, doc := range []struct{ id, path, content string }{{plan.SpecDocument, plan.SpecPath, "Private member spec"}, {plan.PlanDocument, plan.PlanPath, `{"version":2,"units":[]}`}, {"final-report", "final/report.json", "{}"}} {
				must(t, repo.RecordDocuments(ctx, []trace.Document{{Header: trace.Header{Schema: "osmia.trace.document", Version: trace.Version, ID: doc.id, Revision: 1, Project: repo.Project(), Workstream: stream, At: demoStart, Actor: ownerActor, Cause: "fixture"}, Path: doc.path, Content: doc.content}}))
			}
			workspace := filepath.Join(t.TempDir(), "view")
			_, err = stageChiefDocuments(repo, stream, workspace)
			must(t, err)
			state := DeliveredState
			if mode == "cancelled" {
				state = AbandonedState
			}
			_, err = repo.SetFeatureState(ctx, trace.Header{Schema: "osmia.trace.transition", Version: trace.Version, ID: "terminal", Revision: 1, Project: repo.Project(), Workstream: stream, At: demoStart, Actor: ownerActor, Cause: "fixture"}, state, "Member outcome")
			must(t, err)
			if mode == "owner merged" {
				recordResolutionMerge(t, repo, stream)
			}
			if mode != "cancelled" && mode != "owner merged" {
				pr := 42
				if mode == "stale merge" {
					pr = 43
				}
				resolutionDocument(t, repo, stream, publicationDocument, publicationPath, DeliveryPublication{Status: publicationOpened, PullRequest: pr})
				if mode != "ready" {
					outcome := ownPullRequestOutcomeMerged
					if mode == "closed" {
						outcome = ownPullRequestOutcomeClosed
					}
					resolutionDocument(t, repo, stream, ownPullRequestOutcomeDocument, ownPullRequestOutcomePath, ownPullRequestOutcome{PullRequest: 42, Outcome: outcome})
				}
			}
			_, err = stageChiefDocuments(repo, stream, workspace)
			must(t, err)
			retired := mode == "cancelled" || mode == "owner merged" || mode == "current merge"
			for _, path := range []string{plan.SpecPath, plan.PlanPath} {
				_, err := os.Stat(filepath.Join(workspace, path))
				if retired && !os.IsNotExist(err) || !retired && err != nil {
					t.Fatalf("staged %s: %v retired=%v", path, err, retired)
				}
			}
			data, err := os.ReadFile(filepath.Join(workspace, "final/report.json"))
			must(t, err)
			if string(data) != "{}" {
				t.Fatal("final review changed")
			}
			for _, id := range []string{plan.SpecDocument, plan.PlanDocument} {
				d, err := trace.Get[trace.Document](repo, stream, id, 1)
				must(t, err)
				if d.Content == "" {
					t.Fatal("history lost")
				}
			}
		})
	}
}
