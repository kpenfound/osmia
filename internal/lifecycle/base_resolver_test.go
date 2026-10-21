package lifecycle

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/kpenfound/osmia/internal/pulls"
	"github.com/kpenfound/osmia/internal/seal"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	"github.com/kpenfound/osmia/internal/shed"
	"github.com/kpenfound/osmia/internal/trace"
)

const quiet = fixturekit.Quiet
const project = fixturekit.Project

func TestBaseResolverUsesDependencyUntilUpstreamIntegration(t *testing.T) {
	t.Parallel()
	for _, fork := range []bool{true, false} {
		name := "without-fork"
		if fork {
			name = "with-fork"
		}
		t.Run(name, func(t *testing.T) { testBaseResolver(t, fork) })
	}
}
func testBaseResolver(t *testing.T, fork bool) {
	_, cfg := fixturekit.Conversation(t, "base-")
	if !fork {
		cfg.Project.Fork = ""
	}
	repo, err := trace.Open(cfg.Root, cfg.Project)
	must(t, err)
	defer repo.Close()
	ctx := context.Background()
	_, err = repo.SetWorkstreamBase(ctx, quiet, stream, 0, demoStart)
	must(t, err)
	g := &fixturekit.BaseProvider{Upstream: "upstream-sha", Tip: "base-sha"}
	hosting := &fixturekit.FakePulls{}
	s := New(Dependencies{Options: Execution{PullRequests: hosting}})
	_, err = s.ResolveBase(ctx, cfg, repo, quiet, g)
	if !errors.Is(err, ErrBaseUnavailable) {
		t.Fatalf("unsealed dependency: %v", err)
	}
	parent := seal.Seal{Version: 1, Seal: 1, Round: 1, Revision: shed.Pin{Spec: 1, Plan: 1}, SpecHash: seal.SpecHash("spec"), Base: seal.Base{Remote: "upstream", Branch: "main", Commit: "upstream-sha"}, Branch: featureBranch(stream), Workspace: "/owned/feature"}
	data, err := seal.Encode(parent)
	must(t, err)
	h := trace.Header{Schema: "osmia.trace.document", Version: 1, ID: seal.DocumentID, Revision: 1, Project: project, Workstream: stream, At: demoStart, Actor: foremanActor, Cause: "seal"}
	must(t, repo.RecordDocuments(ctx, []trace.Document{{Header: h, Path: seal.Path, Content: string(data)}}))
	selected, err := s.ResolveBase(ctx, cfg, repo, quiet, g)
	must(t, err)
	if selected.Workstream != stream || selected.Commit != g.Tip || selected.Branch != featureBranch(stream) {
		t.Fatalf("dependent base: %+v", selected)
	}
	g.Tip = "moved-base"
	selected, err = s.ResolveBase(ctx, cfg, repo, quiet, g)
	must(t, err)
	if selected.Commit != g.Tip {
		t.Fatalf("base movement ignored: %+v", selected)
	}
	g.IsAncestor = true
	selected, err = s.ResolveBase(ctx, cfg, repo, quiet, g)
	must(t, err)
	if selected.Workstream != stream || selected.Commit != g.Tip {
		t.Fatalf("undelivered parent mistaken for integrated: %+v", selected)
	}
	g.IsAncestor = false
	// A squash publication supplies the actual fork base commit, and the merged
	// hosting record detects its integration even without an ancestor relation.
	publication := DeliveryPublication{Status: publicationOpened, Base: "main", Upstream: cfg.Project.Upstream, Fork: cfg.Project.PushRepository(), Branch: featureBranch(stream), Commit: "squashed-base", PullRequest: 5}
	data, err = json.Marshal(publication)
	must(t, err)
	h.ID = publicationDocument
	must(t, repo.RecordDocuments(ctx, []trace.Document{{Header: h, Path: publicationPath, Content: string(data)}}))
	h.Schema, h.ID = "osmia.trace.transition", "delivered"
	_, err = repo.SetFeatureState(ctx, h, DeliveredState, "Published")
	must(t, err)
	g.IsAncestor = true
	selected, err = s.ResolveBase(ctx, cfg, repo, quiet, g)
	must(t, err)
	if selected.Workstream != "" || selected.Commit != g.Upstream {
		t.Fatalf("integrated published base: %+v", selected)
	}
	g.IsAncestor = false
	selected, err = s.ResolveBase(ctx, cfg, repo, quiet, g)
	must(t, err)
	if selected.Commit != "squashed-base" || selected.Workstream != stream {
		t.Fatalf("squash base %+v", selected)
	}
	hosting.Prs = []pulls.PullRequest{{Number: 5, Merged: true, Head: featureBranch(stream), Base: "main", HeadRepository: cfg.Project.PushRepository(), HeadCommit: "squashed-base"}}
	selected, err = s.ResolveBase(ctx, cfg, repo, quiet, g)
	must(t, err)
	if selected.Workstream != "" || selected.Commit != g.Upstream {
		t.Fatalf("merged squash base %+v", selected)
	}
	g.Tip = ""
	selected, err = s.ResolveBase(ctx, cfg, repo, quiet, g)
	must(t, err)
	if selected.Workstream != "" {
		t.Fatal("merged dependency required a deleted local branch")
	}
	hosting.Prs[0].Base = "osmia/another-parent"
	_, err = s.ResolveBase(ctx, cfg, repo, quiet, g)
	if !errors.Is(err, ErrBaseUnavailable) {
		t.Fatalf("merge into another feature was mistaken for upstream integration: %v", err)
	}
	hosting.Prs[0].Base = "main"
	hosting.Prs[0].Merged = false
	_, err = s.ResolveBase(ctx, cfg, repo, quiet, g)
	if !errors.Is(err, ErrBaseUnavailable) {
		t.Fatalf("unmerged missing branch: %v", err)
	}
}
