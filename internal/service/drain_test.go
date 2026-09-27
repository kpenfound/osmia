package service

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/kpenfound/osmia/internal/bundle"
	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/scheduler"
	"github.com/kpenfound/osmia/internal/trace"
)

// awaitDrained waits until no removed project is still draining.
func awaitDrained(t *testing.T, s *Service) {
	t.Helper()
	soon(t, "the removed projects to drain", func() bool { return len(s.drainingProjects()) == 0 })
}

// list rewrites active_projects in the fixture's top-level configuration.
func (f *twoProjectFixture) list(t *testing.T, ids ...config.ProjectID) {
	t.Helper()
	path := filepath.Join(f.opts.Config.Root, "config.toml")
	text, err := os.ReadFile(path)
	must(t, err)
	quoted := make([]string, len(ids))
	for i, id := range ids {
		quoted[i] = `"` + string(id) + `"`
	}
	edited := regexp.MustCompile(`(?m)^active_projects = \[.*\]$`).ReplaceAllString(string(text), "active_projects = ["+strings.Join(quoted, ", ")+"]")
	must(t, os.WriteFile(path, []byte(edited), 0600))
}

// withMasons sets the fixture's shared mason capacity.
func (f *twoProjectFixture) withMasons(t *testing.T, n string) {
	t.Helper()
	path := filepath.Join(f.opts.Config.Root, "config.toml")
	text, err := os.ReadFile(path)
	must(t, err)
	must(t, os.WriteFile(path, []byte(strings.Replace(string(text), "masons = 1\n", "masons = "+n+"\n", 1)), 0600))
}

// operations waits until the project's workstream has want operations, every
// one acknowledged.
func (f *twoProjectFixture) operations(t *testing.T, repo *trace.Repository, id config.ProjectID, want int) {
	t.Helper()
	awaitAcknowledged(t, func(t *testing.T) []trace.OperationRecord {
		ops, err := repo.Operations(f.streams[id])
		must(t, err)
		if len(ops) != want {
			return nil
		}
		return ops
	})
}

func drainingIDs(t *testing.T, c *Client) []config.ProjectID {
	t.Helper()
	st, err := c.Statuses(context.Background())
	must(t, err)
	var out []config.ProjectID
	for _, p := range st.Draining {
		out = append(out, p.ID)
	}
	return out
}

func TestRemovedProjectFinishesItsTurnInFlightThenStops(t *testing.T) {
	ctx := context.Background()
	f := newTwoProjectFixture(t)
	f.withMasons(t, "3")
	entered, release := f.block(project, "first")
	s, c := start(t, f.opts)
	await(t, "the removed project's turn", entered)
	f.operations(t, f.repository(otherProject), otherProject, 1)
	removing := f.repository(project)
	// The thread is busy, so its next turn waits in the queue.
	f.queue(t, removing, project, "second")

	removed, err := c.RemoveProject(ctx, project)
	must(t, err)
	if removed.Project.ID != project || len(removed.Unfinished) != 1 || removed.Unfinished[0] != (UnfinishedWorkstream{Workstream: stream}) || !strings.Contains(removed.NextStep, "The project drains") {
		t.Fatalf("remove response: %+v", removed)
	}
	cfg, err := c.Configuration(ctx)
	must(t, err)
	if len(cfg.Projects) != 1 || cfg.Projects[0].ID != otherProject {
		t.Fatalf("configuration after removal: %+v", cfg.Projects)
	}
	// A turn queued on an idle thread has a free slot but is not dispatched.
	owner := trace.Actor{Kind: "owner", ID: "local"}
	must(t, removing.CreateThread(ctx, trace.Agent{Header: trace.Header{Schema: "osmia.trace.agent", Version: 1, Revision: 1, ID: "agent_idle", Project: project, Workstream: stream, At: f.clock.Now(), Actor: owner, Cause: "workstream_created"}, Role: demoRole, ThreadID: "thread_idle"}))
	_, err = removing.EnqueueTurn(ctx, trace.TurnRequest{Header: trace.Header{Schema: "osmia.trace.turn-request", Version: 1, Revision: 1, ID: "request_idle", Project: project, Workstream: stream, At: f.clock.Now(), Actor: owner, Cause: "message_idle", Depth: 1},
		AgentID: "agent_idle", ThreadID: "thread_idle", TurnID: "idle", Profile: coreadapter.Profile{Name: "default", Backend: "fake", Model: "test"}, Prompt: "Owner message: idle"})
	must(t, err)
	// Several passes run while the turn is in flight: the project drains,
	// its turn keeps its slot, and nothing new is dispatched.
	time.Sleep(2500 * time.Millisecond)
	if got := drainingIDs(t, c); !slices.Equal(got, []config.ProjectID{project}) {
		t.Fatalf("draining %v", got)
	}
	if got := f.dispatchedTurns(t, removing, project); !slices.Equal(got, []string{turn(project, "first")}) {
		t.Fatalf("draining project dispatched %v", got)
	}
	if traces := s.traces(); len(traces) != 2 || traces[1] != removing {
		t.Fatalf("capacity pool: %v", traces)
	}
	st, d := s.capacityStatus(nil)
	if d != nil || st.Roles[0].Used != 1 {
		t.Fatalf("capacity while draining: %+v %+v", st, d)
	}
	// The draining project's work in flight still reads its configuration
	// and its turn bundles, and its dispatch gate declines every candidate.
	if about := s.about(removing); about.Project.ID != project || about.Project.Upstream != "upstream/repo" {
		t.Fatalf("configuration about the draining project: %+v", about.Project)
	}
	if _, err := s.Context().Assemble(ctx, project, bundle.Scope{Workstream: stream}); err != nil {
		t.Fatalf("bundle of the draining project: %v", err)
	}
	candidate := scheduler.Candidate{Project: project, Workstream: stream, Thread: trace.Thread{Identity: trace.Agent{Role: demoRole}}}
	if ok, err := s.admit(f.cfg.For(project), removing)(ctx, candidate); ok || err != nil {
		t.Fatalf("draining project admitted a turn: %v %v", ok, err)
	}

	close(release)
	awaitDrained(t, s)
	if got := drainingIDs(t, c); len(got) != 0 {
		t.Fatalf("still draining %v", got)
	}
	if traces := s.traces(); len(traces) != 1 || traces[0].Project() != otherProject {
		t.Fatalf("capacity pool after the drain: %v", traces)
	}
	if about := s.about(removing); about.HasProject() {
		t.Fatalf("configuration about the drained project: %+v", about.Project)
	}
	if _, err := s.Context().Assemble(ctx, project, bundle.Scope{Workstream: stream}); !errors.Is(err, errNoActiveProject) {
		t.Fatalf("bundle of the drained project: %v", err)
	}
	// The other project keeps running.
	other := f.repository(otherProject)
	f.queue(t, other, otherProject, "second")
	f.operations(t, other, otherProject, 2)

	// The drained trace is closed and kept: its turn finished and the queued
	// one never ran.
	repo, err := trace.Open(f.cfg.Root, f.cfg.For(project).Project)
	must(t, err)
	if got := f.dispatchedTurns(t, repo, project); !slices.Equal(got, []string{turn(project, "first")}) {
		t.Fatalf("drained project dispatched %v", got)
	}
	th, err := repo.Thread(stream, demoAgent)
	must(t, err)
	for _, q := range th.Turns {
		finished := !q.CompletedAt.IsZero()
		if first := q.Request.TurnID == turn(project, "first"); finished != first || q.Claim != nil && !first {
			t.Fatalf("turn %s: %+v", q.Request.TurnID, q)
		}
	}
	must(t, repo.Close())

	// A restart does not bring the removed project back.
	must(t, s.Close())
	s, c = start(t, f.opts)
	if got := s.current().ProjectIDs(); !slices.Equal(got, []config.ProjectID{otherProject}) {
		t.Fatalf("active projects after restart %v", got)
	}
	if traces := s.traces(); len(traces) != 1 || traces[0].Project() != otherProject {
		t.Fatalf("running after restart: %v", traces)
	}
	if got := drainingIDs(t, c); len(got) != 0 {
		t.Fatalf("draining after restart %v", got)
	}
}

func TestReloadStartsListedProjectsAndDrainsUnlistedOnes(t *testing.T) {
	ctx := context.Background()
	f := newTwoProjectFixture(t)
	// The draining project's turn keeps its slot, so the started project
	// needs another.
	f.withMasons(t, "2")
	f.list(t, project)
	// The unlisted project's trace holds a turn a stopped service claimed.
	owner := trace.Actor{Kind: "owner", ID: "local"}
	stale, err := trace.Open(f.cfg.Root, f.cfg.For(otherProject).Project)
	must(t, err)
	must(t, stale.CreateThread(ctx, trace.Agent{Header: trace.Header{Schema: "osmia.trace.agent", Version: 1, Revision: 1, ID: "agent_review", Project: otherProject, Workstream: otherStream, At: f.clock.Now(), Actor: owner, Cause: "workstream_created"}, Role: reviewerRole, ThreadID: "thread_review"}))
	_, err = stale.EnqueueTurn(ctx, trace.TurnRequest{Header: trace.Header{Schema: "osmia.trace.turn-request", Version: 1, Revision: 1, ID: "request_stale", Project: otherProject, Workstream: otherStream, At: f.clock.Now(), Actor: owner, Cause: "message_stale", Depth: 1},
		AgentID: "agent_review", ThreadID: "thread_review", TurnID: "stale", Profile: coreadapter.Profile{Name: "default", Backend: "fake", Model: "test"}, Prompt: "Review"})
	must(t, err)
	_, err = stale.ClaimTurn(ctx, otherStream, "agent_review", "stale-claim", t.TempDir(), f.clock.Now())
	must(t, err)
	must(t, stale.Close())
	s, c := start(t, f.opts)
	first := f.repository(project)
	f.operations(t, first, project, 1)
	if f.repository(otherProject) != nil {
		t.Fatal("an unlisted project opened")
	}
	entered, release := f.block(project, "second")
	f.queue(t, first, project, "second")
	await(t, "the second turn", entered)

	// The reload starts the listed project and drains the other.
	f.list(t, otherProject)
	reloaded, err := c.Reload(ctx)
	must(t, err)
	if len(reloaded.RestartRequired) != 0 {
		t.Fatalf("reload: %+v", reloaded)
	}
	cfg, err := c.Configuration(ctx)
	must(t, err)
	if len(cfg.Projects) != 1 || cfg.Projects[0].ID != otherProject || hasCode(cfg.Diagnostics, "configuration", RestartRequired) || hasCode(cfg.Diagnostics, "configuration", ReloadRequired) {
		t.Fatalf("configuration after the reload: %+v", cfg)
	}
	other := f.repository(otherProject)
	if other == nil {
		t.Fatal("the listed project did not open")
	}
	f.operations(t, other, otherProject, 1)
	// Its interrupted session is recovered before its loop runs.
	th, err := other.Thread(otherStream, "agent_review")
	must(t, err)
	if len(th.Turns) != 1 || th.Turns[0].CompletedAt.IsZero() || th.Turns[0].Response == nil || th.Turns[0].Response.Result.ErrorSubtype != "interrupted" {
		t.Fatalf("stale claimed turn: %+v", th.Turns)
	}
	if got := drainingIDs(t, c); !slices.Equal(got, []config.ProjectID{project}) {
		t.Fatalf("draining %v", got)
	}

	// Listing the project again while it drains is refused.
	f.list(t, otherProject, project)
	reloadFails(t, c, filepath.Join(f.opts.Config.Root, "config.toml"), "active_projects")
	now, err := c.Configuration(ctx)
	must(t, err)
	if now.LastError == nil || !strings.Contains(now.LastError.Message, "project "+string(project)+" is still draining") || len(now.Projects) != 1 {
		t.Fatalf("after the refused reload: %+v", now)
	}

	close(release)
	awaitDrained(t, s)
	// Once drained, listing it again reopens its trace and it dispatches.
	_, err = c.Reload(ctx)
	must(t, err)
	if got := s.current().ProjectIDs(); !slices.Equal(got, []config.ProjectID{otherProject, project}) {
		t.Fatalf("active projects %v", got)
	}
	again := f.repository(project)
	if again == nil || again == first {
		t.Fatal("the relisted project did not reopen its trace")
	}
	f.operations(t, again, project, 2)
	f.queue(t, again, project, "third")
	f.operations(t, again, project, 3)
	if got := f.dispatchedTurns(t, again, project); !slices.Equal(got, []string{turn(project, "first"), turn(project, "second"), turn(project, "third")}) {
		t.Fatalf("relisted project dispatched %v", got)
	}
}

// The workstreams a removed project leaves unfinished are those neither
// delivered nor abandoned; the librarian's is never listed.
func TestUnfinishedWorkstreamsSkipDeliveredAndAbandonedOnes(t *testing.T) {
	ctx := context.Background()
	opts := fixture(t)
	cfg, err := config.Load(opts.Config)
	must(t, err)
	owner := trace.Actor{Kind: "owner", ID: "local"}
	at := time.Date(2026, 9, 26, 12, 0, 0, 0, time.UTC)
	must(t, os.MkdirAll(cfg.Project.Clone, 0700))
	repo, err := trace.Create(ctx, cfg.Root, cfg.Project, at, owner)
	must(t, err)
	defer repo.Close()
	streams := map[string]config.WorkstreamID{
		"":             "w_00000000000000000000000000000001",
		"building":     "w_00000000000000000000000000000002",
		DeliveredState: "w_00000000000000000000000000000003",
		AbandonedState: "w_00000000000000000000000000000004",
	}
	for _, state := range []string{"", "building", DeliveredState, AbandonedState} {
		ws := streams[state]
		must(t, repo.CreateWorkstream(ctx, ws, at, owner))
		if state != "" {
			_, err := repo.SetFeatureStateUnless(ctx, trace.Header{Schema: "osmia.trace.transition", Version: trace.Version, ID: "to-" + state, Revision: 1, Project: project, Workstream: ws, At: at, Actor: owner, Cause: "fixture"}, state, "fixture")
			must(t, err)
		}
	}
	must(t, repo.CreateWorkstream(ctx, librarianWorkstream(project), at, owner))
	got, err := unfinishedWorkstreams(repo)
	must(t, err)
	want := []UnfinishedWorkstream{{Workstream: streams[""]}, {Workstream: streams["building"], State: "building"}}
	if !slices.Equal(got, want) {
		t.Fatalf("unfinished %+v, want %+v", got, want)
	}
}
