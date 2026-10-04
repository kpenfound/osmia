package service

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/service/beekeeper"
	"github.com/kpenfound/osmia/internal/trace"
)

// claimBeekeeperTurn records the Beekeeper's thread and one owner-request
// turn directly, the way an owner message would, and claims it, so a tool
// handler sees exactly the scope a real Beekeeper turn would carry. It
// returns that scope.
func claimBeekeeperTurn(t *testing.T, shadow *trace.Repository, turn string, at time.Time) coreadapter.Scope {
	t.Helper()
	ctx := context.Background()
	if _, err := beekeeper.EnsureThread(ctx, shadow, at); err != nil {
		t.Fatal(err)
	}
	req := trace.TurnRequest{
		Header: trace.Header{Schema: "osmia.trace.turn-request", Version: trace.Version, ID: "request_" + turn, Revision: 1,
			Project: config.ShadowProjectID, Workstream: config.BeekeeperWorkstreamID, At: at, Actor: beekeeper.OwnerActor, Cause: "owner-message"},
		AgentID: beekeeper.AgentID, ThreadID: beekeeper.ThreadID, TurnID: turn,
		Profile: coreadapter.Profile{Name: "default", Backend: "claude", Model: "test"}, Prompt: "List the factory.",
	}
	if _, err := shadow.EnqueueTurn(ctx, req); err != nil {
		t.Fatal(err)
	}
	if _, err := shadow.ClaimTurn(ctx, config.BeekeeperWorkstreamID, beekeeper.AgentID, "session_"+turn, t.TempDir(), at); err != nil {
		t.Fatal(err)
	}
	return coreadapter.Scope{Project: string(config.ShadowProjectID), Workstream: string(config.BeekeeperWorkstreamID), Thread: beekeeper.ThreadID, Turn: turn, Role: beekeeper.AgentID}
}

// The Beekeeper's list-the-factory tool lists every registered project and
// each project's workstreams with their current status, built on a fake
// model session (this suite never runs a live one, per charter#4), with two
// registered projects each in their own temporary directory, and it leaves
// out the shadow project and its one reserved workstream. The fixture
// configures no Hearsay section, so this also shows the tool working with
// Hearsay not configured.
func TestListFactoryToolListsRegisteredProjectsAndWorkstreams(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	opts, firstClone := projectFixture(t)
	opts.controls = &runtimeControls{}
	cfg, err := config.Load(opts.Config)
	must(t, err)
	if cfg.Hearsay.URL != "" || len(cfg.Hearsay.Agents) != 0 {
		t.Fatalf("test assumes Hearsay is not configured: %+v", cfg.Hearsay)
	}
	s, c := start(t, opts)

	secondClone := filepath.Join(filepath.Dir(firstClone), "second")
	demoGit(t, filepath.Dir(firstClone), "init", "--quiet", secondClone)
	first, err := c.AddProject(ctx, request(firstClone))
	must(t, err)
	second, err := c.AddProject(ctx, ProjectAddRequest{Name: "beehive", Upstream: "upstream/beehive", Fork: "owner/beehive", Clone: secondClone})
	must(t, err)

	building := config.WorkstreamID("w_11111111111111111111111111111111")
	delivered := config.WorkstreamID("w_22222222222222222222222222222222")
	repoA, err := s.repository(first.Project.ID)
	must(t, err)
	must(t, repoA.CreateWorkstream(ctx, building, s.now(), ownerActor))
	_, err = repoA.SetFeatureState(ctx, trace.Header{Schema: "osmia.trace.transition", Version: trace.Version, ID: "start", Revision: 1, Project: first.Project.ID, Workstream: building, At: s.now(), Actor: ownerActor, Cause: "start"}, BuildingState, "work started")
	must(t, err)
	repoB, err := s.repository(second.Project.ID)
	must(t, err)
	must(t, repoB.CreateWorkstream(ctx, delivered, s.now(), ownerActor))
	_, err = repoB.SetFeatureState(ctx, trace.Header{Schema: "osmia.trace.transition", Version: trace.Version, ID: "end", Revision: 1, Project: second.Project.ID, Workstream: delivered, At: s.now(), Actor: ownerActor, Cause: "end"}, DeliveredState, "shipped")
	must(t, err)

	scope := claimBeekeeperTurn(t, s.Beekeeper(), "list_turn", s.now())
	out, err := s.options.controls.listFactory(scope).Handle(ctx, json.RawMessage(`{}`))
	must(t, err)
	var got FactoryList
	must(t, json.Unmarshal(out, &got))

	if len(got.Projects) != 2 {
		t.Fatalf("projects: %+v", got)
	}
	byID := map[config.ProjectID]FactoryProjectStatus{}
	for _, p := range got.Projects {
		byID[p.Project] = p
	}
	a, ok := byID[first.Project.ID]
	if !ok || a.Name != "dagger" || len(a.Workstreams) != 1 || a.Workstreams[0].Workstream != building || a.Workstreams[0].State == nil || *a.Workstreams[0].State != BuildingState {
		t.Fatalf("first project: %+v", a)
	}
	b, ok := byID[second.Project.ID]
	if !ok || b.Name != "beehive" || len(b.Workstreams) != 1 || b.Workstreams[0].Workstream != delivered || b.Workstreams[0].State == nil || *b.Workstreams[0].State != DeliveredState {
		t.Fatalf("second project: %+v", b)
	}
	for _, p := range got.Projects {
		if p.Project == config.ShadowProjectID {
			t.Fatalf("the shadow project is listed: %+v", got)
		}
		for _, w := range p.Workstreams {
			if w.Workstream == config.BeekeeperWorkstreamID {
				t.Fatalf("the beekeeper's reserved workstream is listed: %+v", got)
			}
		}
	}

	// The agent reads no files: the tool has no write capability and
	// exposes no version control or credentials.
	if got2 := s.options.controls.listFactory(scope); got2.Effect != coreadapter.ToolRead {
		t.Fatalf("list_factory effect: %v", got2.Effect)
	}

	// A scope outside the Beekeeper's own thread is refused.
	other := scope
	other.Role = "chief_of_staff"
	if _, err := s.options.controls.listFactory(other).Handle(ctx, json.RawMessage(`{}`)); err == nil {
		t.Fatal("list_factory accepted a non-beekeeper scope")
	}
}

// With no owner project registered anywhere in the service, the
// list-the-factory tool reports an empty list and that is not an error.
func TestListFactoryToolWithNoProjectRegisteredIsEmptyNotAnError(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	opts, _ := projectFixture(t)
	opts.controls = &runtimeControls{}
	s, _ := start(t, opts)

	scope := claimBeekeeperTurn(t, s.Beekeeper(), "list_turn_empty", s.now())
	out, err := s.options.controls.listFactory(scope).Handle(ctx, json.RawMessage(`{}`))
	must(t, err)
	var got FactoryList
	must(t, json.Unmarshal(out, &got))
	if got.Projects == nil || len(got.Projects) != 0 {
		t.Fatalf("expected an empty, non-nil list of projects, got %+v", got)
	}
}
