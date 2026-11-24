package lifecycle_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/kpenfound/osmia/internal/servicetest/journey"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/intake"
	"github.com/kpenfound/osmia/internal/lifecycle"
	"github.com/kpenfound/osmia/internal/seal"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	"github.com/kpenfound/osmia/internal/servicetest/intakefixture"
	"github.com/kpenfound/osmia/internal/shed"
	"github.com/kpenfound/osmia/internal/trace"
	"github.com/kpenfound/osmia/internal/workspace"
)

func TestDependentChainSealsAndParksAcrossRestart(t *testing.T) {
	t.Parallel()
	for _, backend := range []string{config.WorkspacesGit, config.WorkspacesJujutsu} {
		t.Run(backend, func(t *testing.T) {
			if backend == config.WorkspacesJujutsu {
				intakefixture.RequireJJ(t)
			}
			f := journey.NewDebateFixture(t, 1, 1)
			base := f.Upstream(t)
			f.Stop(t)
			ctx := context.Background()
			repo, err := trace.Open(f.S.Cfg.Root, f.S.Cfg.Project)
			fixturekit.Must(t, err)
			defer func() { repo.Close() }()
			var streams []config.WorkstreamID
			previousCommit := base
			for i := 0; i < 3; i++ {
				stream, err := config.NewWorkstreamID()
				fixturekit.Must(t, err)
				fixturekit.Must(t, repo.CreateWorkstreamOn(ctx, stream, backend, f.S.Now(), intake.OwnerActor))
				if i > 0 {
					_, err = repo.SetWorkstreamBase(ctx, stream, streams[i-1], 0, f.S.Now())
					fixturekit.Must(t, err)
				}
				header := trace.Header{Schema: "osmia.trace.document", Version: 1, Revision: 1, Project: repo.Project(), Workstream: stream, At: f.S.Now(), Actor: intake.OwnerActor, Cause: "owner-ratification"}
				ratified, err := shed.EncodeRatification(shed.Ratify(1, shed.Pin{Spec: 1, Plan: 1}, nil))
				fixturekit.Must(t, err)
				docs := []trace.Document{}
				for _, d := range []struct{ id, path, content string }{{"spec", "spec.md", fixturekit.ValidSpec}, {"plan", "plan.json", fixturekit.ValidPlan}, {shed.RatificationDocumentID(1), shed.RatificationPath(1), string(ratified)}} {
					h := header
					h.ID = d.id
					docs = append(docs, trace.Document{Header: h, Path: d.path, Content: d.content})
				}
				fixturekit.Must(t, repo.RecordDocuments(ctx, docs))
				header.Schema, header.ID = "osmia.trace.transition", "in-shed"
				_, err = repo.SetFeatureState(ctx, header, intake.InShedState, "Owner approved the draft")
				fixturekit.Must(t, err)
				z := &intake.Sealer{S: f.S.Intake().SealerDependencies(), Repository: repo}
				fixturekit.Must(t, z.Pass(ctx))
				op := fixturekit.ActionOperation(t, repo, stream, intake.SealAction)
				f.S.Boundary = func(name string) error {
					if name == "seal-branch-created" {
						return errors.New("interrupted")
					}
					return nil
				}
				if _, err = z.Apply(ctx, op); err == nil {
					t.Fatal("expected branch-creation interruption")
				}
				f.S.ClearBoundary()
				if i == 0 {
					journey.AdvanceUpstream(t, f, map[string]string{"new-upstream": "moved during restart\n"})
				}
				fixturekit.Must(t, repo.Close())
				repo, err = trace.Open(f.S.Cfg.Root, f.S.Cfg.Project)
				fixturekit.Must(t, err)
				z.Repository = repo
				result, err := z.Apply(ctx, op)
				fixturekit.Must(t, err)
				if result.Outcome != "succeeded" {
					t.Fatal(result)
				}
				record, _, found, err := seal.Latest(repo, stream)
				fixturekit.Must(t, err)
				if !found || record.Base.Commit != previousCommit || i > 0 && record.Base.Workstream != streams[i-1] {
					t.Fatalf("unpinned dependent base %+v", record.Base)
				}
				observed, err := z.Inspect(ctx, op)
				fixturekit.Must(t, err)
				if observed.State != coreadapter.EffectCompleted {
					t.Fatal(observed)
				}
				g, err := intake.FeatureWorkspaces(f.S.About(repo), repo).Of(stream)
				fixturekit.Must(t, err)
				w, found, err := g.Workspace(ctx, string(stream))
				fixturekit.Must(t, err)
				if !found {
					t.Fatal("no feature workspace")
				}
				fixturekit.Must(t, os.WriteFile(filepath.Join(w.Path, fmt.Sprintf("feature-%d", i)), []byte("feature\n"), 0600))
				previousCommit, err = g.Snapshot(ctx, w, record.Base.Commit)
				fixturekit.Must(t, err)
				tip, _, err := g.Branch(ctx, record.Branch)
				fixturekit.Must(t, err)
				if tip != previousCommit {
					fixturekit.Must(t, g.Advance(ctx, w, tip, previousCommit))
				}
				streams = append(streams, stream)
			}
			g, err := intake.FeatureWorkspaces(f.S.About(repo), repo).Of(streams[0])
			fixturekit.Must(t, err)
			w, _, err := g.Workspace(ctx, string(streams[0]))
			fixturekit.Must(t, err)
			fixturekit.Must(t, g.Release(ctx, workspace.Worktree(w)))
			fixturekit.DemoGit(t, filepath.Dir(f.Clone), "-C", f.Clone, "branch", "-D", intake.FeatureBranch(streams[0]))
			fixturekit.Must(t, f.S.Lifecycle().BaseWaitPass(ctx, repo))
			for _, stream := range streams[1:] {
				waiting, err := lifecycle.BaseWaiting(repo, stream)
				fixturekit.Must(t, err)
				if !waiting {
					t.Fatal("descendant did not park")
				}
			}
			firstSeal, _, _, err := seal.Latest(repo, streams[0])
			fixturekit.Must(t, err)
			fixturekit.DemoGit(t, filepath.Dir(f.Clone), "-C", f.Clone, "branch", firstSeal.Branch, firstSeal.Base.Commit)
			fixturekit.Must(t, f.S.Lifecycle().BaseWaitPass(ctx, repo))
			for _, stream := range streams[1:] {
				waiting, err := lifecycle.BaseWaiting(repo, stream)
				fixturekit.Must(t, err)
				if waiting {
					t.Fatal("restored dependency remained parked")
				}
			}
			h := trace.Header{Schema: "osmia.trace.transition", Version: 1, ID: "owner-abandoned", Revision: 1, Project: repo.Project(), Workstream: streams[0], At: f.S.Now(), Actor: intake.OwnerActor, Cause: "owner"}
			_, err = repo.SetFeatureState(ctx, h, intake.AbandonedState, "Owner abandoned the base")
			fixturekit.Must(t, err)
			fixturekit.Must(t, f.S.Lifecycle().BaseWaitPass(ctx, repo))
			for _, stream := range streams[1:] {
				waiting, err := lifecycle.BaseWaiting(repo, stream)
				fixturekit.Must(t, err)
				if !waiting {
					t.Fatal("abandoned ancestor did not park descendant")
				}
			}
		})
	}
}
