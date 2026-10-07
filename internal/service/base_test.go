package service

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/pulls"
	"github.com/kpenfound/osmia/internal/seal"
	"github.com/kpenfound/osmia/internal/shed"
	"github.com/kpenfound/osmia/internal/trace"
	"github.com/kpenfound/osmia/internal/workspace"
)

type baseProvider struct {
	workspace.Provider
	upstream, tip string
	integrated    bool
	// remotes and fetches count calls, to show resolveBase makes neither
	// once a dependency's integration into upstream is recorded (spec#8).
	remotes, fetches int
}

func (g *baseProvider) Remote(context.Context, string) (string, error) {
	g.remotes++
	return "upstream", nil
}
func (g *baseProvider) Fetch(context.Context, string, string) (string, error) {
	g.fetches++
	return g.upstream, nil
}
func (g *baseProvider) Branch(context.Context, string) (string, bool, error) {
	return g.tip, g.tip != "", nil
}
func (g *baseProvider) Ancestor(context.Context, string, string) (bool, error) {
	return g.integrated, nil
}

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
	_, cfg := conversationFixture(t, "base-")
	if !fork {
		cfg.Project.Fork = ""
	}
	repo, err := trace.Open(cfg.Root, cfg.Project)
	must(t, err)
	defer repo.Close()
	ctx := context.Background()
	_, err = repo.SetWorkstreamBase(ctx, quiet, stream, 0, demoStart)
	must(t, err)
	g := &baseProvider{upstream: "upstream-sha", tip: "base-sha"}
	hosting := &fakePulls{}
	s := &Service{options: Options{PullRequests: hosting}}
	_, err = s.resolveBase(ctx, cfg, repo, quiet, g)
	if !errors.Is(err, errBaseUnavailable) {
		t.Fatalf("unsealed dependency: %v", err)
	}
	parent := seal.Seal{Version: 1, Seal: 1, Round: 1, Revision: shed.Pin{Spec: 1, Plan: 1}, SpecHash: seal.SpecHash("spec"), Base: seal.Base{Remote: "upstream", Branch: "main", Commit: "upstream-sha"}, Branch: featureBranch(stream), Workspace: "/owned/feature"}
	data, err := seal.Encode(parent)
	must(t, err)
	h := trace.Header{Schema: "osmia.trace.document", Version: 1, ID: seal.DocumentID, Revision: 1, Project: project, Workstream: stream, At: demoStart, Actor: foremanActor, Cause: "seal"}
	must(t, repo.RecordDocuments(ctx, []trace.Document{{Header: h, Path: seal.Path, Content: string(data)}}))
	selected, err := s.resolveBase(ctx, cfg, repo, quiet, g)
	must(t, err)
	if selected.Workstream != stream || selected.Commit != g.tip || selected.Branch != featureBranch(stream) {
		t.Fatalf("dependent base: %+v", selected)
	}
	g.tip = "moved-base"
	selected, err = s.resolveBase(ctx, cfg, repo, quiet, g)
	must(t, err)
	if selected.Commit != g.tip {
		t.Fatalf("base movement ignored: %+v", selected)
	}
	g.integrated = true
	selected, err = s.resolveBase(ctx, cfg, repo, quiet, g)
	must(t, err)
	if selected.Workstream != stream || selected.Commit != g.tip {
		t.Fatalf("undelivered parent mistaken for integrated: %+v", selected)
	}
	g.integrated = false
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
	g.integrated = true
	selected, err = s.resolveBase(ctx, cfg, repo, quiet, g)
	must(t, err)
	if selected.Workstream != "" || selected.Commit != g.upstream {
		t.Fatalf("integrated published base: %+v", selected)
	}
	g.integrated = false
	selected, err = s.resolveBase(ctx, cfg, repo, quiet, g)
	must(t, err)
	if selected.Commit != "squashed-base" || selected.Workstream != stream {
		t.Fatalf("squash base %+v", selected)
	}
	hosting.prs = []pulls.PullRequest{{Number: 5, Merged: true, Head: featureBranch(stream), Base: "main", HeadRepository: cfg.Project.PushRepository(), HeadCommit: "squashed-base"}}
	selected, err = s.resolveBase(ctx, cfg, repo, quiet, g)
	must(t, err)
	if selected.Workstream != "" || selected.Commit != g.upstream {
		t.Fatalf("merged squash base %+v", selected)
	}
	g.tip = ""
	selected, err = s.resolveBase(ctx, cfg, repo, quiet, g)
	must(t, err)
	if selected.Workstream != "" {
		t.Fatal("merged dependency required a deleted local branch")
	}
	hosting.prs[0].Base = "osmia/another-parent"
	_, err = s.resolveBase(ctx, cfg, repo, quiet, g)
	if !errors.Is(err, errBaseUnavailable) {
		t.Fatalf("merge into another feature was mistaken for upstream integration: %v", err)
	}
	hosting.prs[0].Base = "main"
	hosting.prs[0].Merged = false
	_, err = s.resolveBase(ctx, cfg, repo, quiet, g)
	if !errors.Is(err, errBaseUnavailable) {
		t.Fatalf("unmerged missing branch: %v", err)
	}
}

func TestHandInBaseIsPinnedToItsIdempotencyKey(t *testing.T) {
	t.Parallel()
	opts, _ := conversationFixture(t, "base-hand-")
	s, c := start(t, opts)
	text := "Build on the earlier feature"
	req := HandInRequest{Project: project, Key: "dependent", Stdin: &text, Base: stream}
	first, err := c.HandIn(context.Background(), req)
	must(t, err)
	second, err := c.HandIn(context.Background(), req)
	must(t, err)
	if first.Workstream != second.Workstream {
		t.Fatal("retry created another workstream")
	}
	req.Base = quiet
	_, err = c.HandIn(context.Background(), req)
	assertCode(t, err, Conflict)
	repo, err := s.repository(project)
	must(t, err)
	base, err := repo.WorkstreamBase(first.Workstream)
	must(t, err)
	if base.Base != stream || base.Revision != 1 {
		t.Fatalf("base %+v", base)
	}
	_, err = repo.SetWorkstreamBase(context.Background(), first.Workstream, quiet, base.Revision, s.now())
	must(t, err)
	req.Base = stream
	_, err = c.HandIn(context.Background(), req)
	must(t, err)
	req.Base = quiet
	_, err = c.HandIn(context.Background(), req)
	assertCode(t, err, Conflict)
	req.Key = "missing"
	req.Base = config.WorkstreamID("w_ffffffffffffffffffffffffffffffff")
	_, err = c.HandIn(context.Background(), req)
	assertCode(t, err, Validation)
}
