package service

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/seal"
	"github.com/kpenfound/osmia/internal/shed"
	"github.com/kpenfound/osmia/internal/trace"
	"github.com/kpenfound/osmia/internal/workspace"
)

func TestDependentChainSealsAndParksAcrossRestart(t *testing.T) {
	for _, backend := range []string{config.WorkspacesGit, config.WorkspacesJujutsu} {
		t.Run(backend, func(t *testing.T) {
			if backend == config.WorkspacesJujutsu {
				requireJJ(t)
			}
			f := newDebateFixture(t, 1, 1)
			base := f.upstream(t)
			f.stop(t)
			ctx := context.Background()
			repo, err := trace.Open(f.s.cfg.Root, f.s.cfg.Project)
			must(t, err)
			defer func() { repo.Close() }()
			var streams []config.WorkstreamID
			previousCommit := base
			for i := 0; i < 3; i++ {
				stream, err := config.NewWorkstreamID()
				must(t, err)
				must(t, repo.CreateWorkstreamOn(ctx, stream, backend, f.s.now(), ownerActor))
				if i > 0 {
					_, err = repo.SetWorkstreamBase(ctx, stream, streams[i-1], 0, f.s.now())
					must(t, err)
				}
				header := trace.Header{Schema: "osmia.trace.document", Version: 1, Revision: 1, Project: repo.Project(), Workstream: stream, At: f.s.now(), Actor: ownerActor, Cause: "owner-ratification"}
				ratified, err := shed.EncodeRatification(shed.Ratify(1, shed.Pin{Spec: 1, Plan: 1}, nil))
				must(t, err)
				docs := []trace.Document{}
				for _, d := range []struct{ id, path, content string }{{"spec", "spec.md", validSpec}, {"plan", "plan.json", validPlan}, {shed.RatificationDocumentID(1), shed.RatificationPath(1), string(ratified)}} {
					h := header
					h.ID = d.id
					docs = append(docs, trace.Document{Header: h, Path: d.path, Content: d.content})
				}
				must(t, repo.RecordDocuments(ctx, docs))
				header.Schema, header.ID = "osmia.trace.transition", "in-shed"
				_, err = repo.SetFeatureState(ctx, header, InShedState, "Owner approved the draft")
				must(t, err)
				z := &sealer{s: f.s, repository: repo}
				must(t, z.Pass(ctx))
				op := actionOperation(t, repo, stream, SealAction)
				// Lose the response after branch creation, then move upstream before retry.
				f.s.boundary = func(name string) error {
					if name == "seal-branch-created" {
						return errors.New("interrupted")
					}
					return nil
				}
				if _, err = z.Apply(ctx, op); err == nil {
					t.Fatal("expected branch-creation interruption")
				}
				f.s.boundary = nil
				if i == 0 {
					advanceUpstream(t, f, map[string]string{"new-upstream": "moved during restart\n"})
				}
				must(t, repo.Close())
				repo, err = trace.Open(f.s.cfg.Root, f.s.cfg.Project)
				must(t, err)
				z.repository = repo
				result, err := z.Apply(ctx, op)
				must(t, err)
				if result.Outcome != "succeeded" {
					t.Fatal(result)
				}
				record, _, found, err := seal.Latest(repo, stream)
				must(t, err)
				if !found || record.Base.Commit != previousCommit || i > 0 && record.Base.Workstream != streams[i-1] {
					t.Fatalf("unpinned dependent base %+v", record.Base)
				}
				observed, err := z.Inspect(ctx, op)
				must(t, err)
				if observed.State != coreadapter.EffectCompleted {
					t.Fatal(observed)
				}
				g, err := featureWorkspaces(f.s.about(repo), repo).of(stream)
				must(t, err)
				w, found, err := g.Workspace(ctx, string(stream))
				must(t, err)
				if !found {
					t.Fatal("no feature workspace")
				}
				must(t, os.WriteFile(filepath.Join(w.Path, fmt.Sprintf("feature-%d", i)), []byte("feature\n"), 0600))
				previousCommit, err = g.Snapshot(ctx, w, record.Base.Commit)
				must(t, err)
				// Snapshot records the candidate; explicitly move the branch as a landing does.
				tip, _, err := g.Branch(ctx, record.Branch)
				must(t, err)
				if tip != previousCommit {
					must(t, g.Advance(ctx, w, tip, previousCommit))
				}
				streams = append(streams, stream)
			}
			// Removing the ancestor's branch parks the whole descendant chain.
			g, err := featureWorkspaces(f.s.about(repo), repo).of(streams[0])
			must(t, err)
			w, _, err := g.Workspace(ctx, string(streams[0]))
			must(t, err)
			must(t, g.Release(ctx, workspace.Worktree(w)))
			demoGit(t, filepath.Dir(f.clone), "-C", f.clone, "branch", "-D", featureBranch(streams[0]))
			must(t, f.s.baseWaitPass(ctx, repo))
			for _, stream := range streams[1:] {
				waiting, err := baseWaiting(repo, stream)
				must(t, err)
				if !waiting {
					t.Fatal("descendant did not park")
				}
			}
			// Restoring the branch releases those waits without starting a model turn.
			firstSeal, _, _, err := seal.Latest(repo, streams[0])
			must(t, err)
			demoGit(t, filepath.Dir(f.clone), "-C", f.clone, "branch", firstSeal.Branch, firstSeal.Base.Commit)
			must(t, f.s.baseWaitPass(ctx, repo))
			for _, stream := range streams[1:] {
				waiting, err := baseWaiting(repo, stream)
				must(t, err)
				if waiting {
					t.Fatal("restored dependency remained parked")
				}
			}
			h := trace.Header{Schema: "osmia.trace.transition", Version: 1, ID: "owner-abandoned", Revision: 1, Project: repo.Project(), Workstream: streams[0], At: f.s.now(), Actor: ownerActor, Cause: "owner"}
			_, err = repo.SetFeatureState(ctx, h, AbandonedState, "Owner abandoned the base")
			must(t, err)
			must(t, f.s.baseWaitPass(ctx, repo))
			for _, stream := range streams[1:] {
				waiting, err := baseWaiting(repo, stream)
				must(t, err)
				if !waiting {
					t.Fatal("abandoned ancestor did not park descendant")
				}
			}
		})
	}
}
