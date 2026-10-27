package lifecycle_test

import (
	context "context"
	errors "errors"
	config "github.com/kpenfound/osmia/internal/config"
	coreadapter "github.com/kpenfound/osmia/internal/coreadapter"
	runtime "github.com/kpenfound/osmia/internal/runtime"
	trace "github.com/kpenfound/osmia/internal/trace"
	os "os"
	filepath "path/filepath"
	slices "slices"
	strings "strings"
	atomic "sync/atomic"
	testing "testing"
	time "time"
)

func TestSeveralProjectsShareCapacityPauseApartAndResumeFromTheirOwnTraces(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newTwoProjectFixture(t)
	// The first project starts paused, so the other project's turn takes the
	// only mason slot.
	store, _, err := runtime.Open(runtime.Inputs{Config: f.cfg})
	must(t, err)
	must(t, store.SetPause(runtime.Pause{Target: runtime.Target{Scope: "project", Project: project}, Mode: "soft", Source: runtime.PauseOwner, Reason: "Owner pause"}))
	must(t, store.Close())
	otherEntered, otherRelease := f.block(otherProject, "first")
	entered, _ := f.block(project, "first")

	var passed atomic.Int64
	f.opts.schedulePassed = func() { passed.Add(1) }
	s, err := Start(ctx, f.opts)
	must(t, err)
	stopped := false
	defer func() {
		if !stopped {
			s.Close()
		}
	}()
	if got := s.current().ProjectIDs(); !slices.Equal(got, []config.ProjectID{project, otherProject}) {
		t.Fatalf("active projects %v", got)
	}
	await(t, "the other project's turn", otherEntered)
	paused, other := f.repository(project), f.repository(otherProject)
	if paused == nil || other == nil || paused == other {
		t.Fatalf("each project needs its own trace: %v %v", paused, other)
	}
	// The pause holds only its own project: its turn is neither dispatched nor
	// waiting for a slot.
	soon(t, "the other project's turn holds the only mason slot", func() bool {
		st := s.capacityStatus(nil)
		return st.Roles[0].Used == 1 && len(st.Roles[0].Waiting) == 0
	})
	if got := f.dispatchedTurns(t, paused, project); len(got) != 0 {
		t.Fatalf("paused project dispatched %v", got)
	}

	// Resumed, the first project's turn waits for the slot the other project
	// holds.
	must(t, s.store.ClearPause(runtime.Target{Scope: "project", Project: project}, runtime.PauseOwner))
	soon(t, "the first project's turn waits for the shared mason slot", func() bool {
		st := s.capacityStatus(nil)
		if st.Roles[0].Used != 1 || len(st.Roles[0].Waiting) != 1 {
			return false
		}
		w := st.Roles[0].Waiting[0]
		return w.Workstream == stream && w.Turn == turn(project, "first") && w.Reason == "capacity"
	})
	// Several passes of both loops run while the slot stays taken.
	awaitPasses(t, &passed, 3)
	if got := f.dispatchedTurns(t, paused, project); len(got) != 0 {
		t.Fatalf("first project dispatched %v past the shared capacity", got)
	}

	// Once the other project's turn completes, the slot goes to the first.
	close(otherRelease)
	await(t, "the first project's turn", entered)
	f.queue(t, other, otherProject, "second")
	must(t, s.Close())
	stopped = true

	// The first project's turn was in flight at the stop; after the restart
	// each project resumes from its own trace.
	s, err = Start(ctx, f.opts)
	must(t, err)
	stopped = false
	for _, id := range []config.ProjectID{project, otherProject} {
		awaitAcknowledged(t, func(t *testing.T) []trace.OperationRecord {
			ops, err := f.repository(id).Operations(f.streams[id])
			must(t, err)
			want := 1
			if id == otherProject {
				want = 2
			}
			if len(ops) != want {
				return nil
			}
			return ops
		})
	}
	must(t, s.Close())
	stopped = true
	for _, p := range f.cfg.Projects {
		repo, err := trace.Open(f.cfg.Root, p)
		must(t, err)
		th, err := repo.Thread(f.streams[p.ID], demoAgent)
		must(t, err)
		want := []string{turn(p.ID, "first")}
		if p.ID == otherProject {
			want = append(want, turn(p.ID, "second"))
		}
		if got := f.dispatchedTurns(t, repo, p.ID); !slices.Equal(got, want) {
			t.Fatalf("project %s dispatched %v, want %v", p.ID, got, want)
		}
		for _, q := range th.Turns {
			if q.CompletedAt.IsZero() || q.Request.Project != p.ID {
				t.Fatalf("project %s turn %s: %+v", p.ID, q.Request.TurnID, q)
			}
		}
		must(t, repo.Close())
	}
}

func TestProjectScopedRequestsNameOneOfSeveralProjects(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newTwoProjectFixture(t)
	s, c := start(t, f.opts)
	text := "Build it"
	both := string(project) + ", " + string(otherProject)
	_, err := c.HandIn(ctx, HandInRequest{Key: "k", Stdin: &text})
	var api *APIError
	if !errors.As(err, &api) || api.Code != Validation || !strings.Contains(api.Message, "several projects are active; name one of "+both) {
		t.Fatalf("hand-in without a project: %v", err)
	}
	unknown := config.ProjectID("p_00000000000000000000000000000000")
	for name, call := range map[string]func() error{
		"extract": func() error { _, err := c.ExtractProject(ctx, ""); return err },
		"rebase":  func() error { _, err := c.RebaseProject(ctx, ""); return err },
		"answer":  func() error { _, err := c.Answer(ctx, 1, "yes", ""); return err },
	} {
		if err := call(); !errors.As(err, &api) || api.Code != Validation || !strings.Contains(api.Message, both) {
			t.Fatalf("%s without a project: %v", name, err)
		}
	}
	if _, err := c.RebaseProject(ctx, unknown); !errors.As(err, &api) || api.Code != NotFound || !strings.Contains(api.Message, "the active projects are "+both) {
		t.Fatalf("rebase of an inactive project: %v", err)
	}
	// A named project is acted on alone.
	out, err := c.RebaseProject(ctx, otherProject)
	must(t, err)
	if out.Project != otherProject || len(out.Covered)+len(out.Skipped) != 1 || len(out.Covered) == 1 && out.Covered[0].Workstream != otherStream || len(out.Skipped) == 1 && out.Skipped[0].Workstream != otherStream {
		t.Fatalf("rebase of the other project: %+v", out)
	}
	cfg, err := c.Configuration(ctx)
	must(t, err)
	if cfg.Project != nil || len(cfg.Projects) != 2 || cfg.Projects[0].ID != project || cfg.Projects[1].ID != otherProject {
		t.Fatalf("configuration projects: %+v %+v", cfg.Project, cfg.Projects)
	}
	status, err := c.Statuses(ctx)
	must(t, err)
	var listed []config.ProjectID
	for _, w := range status.Workstreams {
		listed = append(listed, w.Project)
	}
	slices.Sort(listed)
	if !slices.Equal(listed, []config.ProjectID{project, otherProject}) {
		t.Fatalf("status lists workstreams of %v", listed)
	}
	// A workstream is found in whichever project holds it.
	if _, api := s.workstreamStatus(string(otherStream)); api != nil {
		t.Fatalf("status of the other project's workstream: %v", api)
	}
	if p, ws, repo, api := s.conversationTrace(string(otherStream)); api != nil || p != otherProject || ws != otherStream || repo.Project() != otherProject {
		t.Fatalf("conversation of the other project's workstream: %v %v %v", p, ws, api)
	}
}

func TestRemovingOneOfSeveralProjectsLeavesTheOtherRunning(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newTwoProjectFixture(t)
	s, c := start(t, f.opts)
	for _, id := range []config.ProjectID{project, otherProject} {
		awaitAcknowledged(t, func(t *testing.T) []trace.OperationRecord {
			ops, err := f.repository(id).Operations(f.streams[id])
			must(t, err)
			return ops
		})
	}
	_, err := c.Abandon(ctx, f.streams[project], "Owner finished this project's work.")
	must(t, err)
	removed, err := c.RemoveProject(ctx, project)
	must(t, err)
	if removed.Project.ID != project {
		t.Fatalf("removed %+v", removed.Project)
	}
	cfg, err := c.Configuration(ctx)
	must(t, err)
	if cfg.Project == nil || cfg.Project.ID != otherProject || len(cfg.Projects) != 1 || cfg.Projects[0].ID != otherProject {
		t.Fatalf("configuration after removal: %+v %+v", cfg.Project, cfg.Projects)
	}
	if traces := s.traces(); len(traces) != 1 || traces[0].Project() != otherProject {
		t.Fatalf("traces in the capacity pool: %d", len(traces))
	}
	disk, err := config.Load(f.opts.Config)
	must(t, err)
	if !slices.Equal(disk.ProjectIDs(), []config.ProjectID{otherProject}) {
		t.Fatalf("active projects on disk %v", disk.ProjectIDs())
	}
	// The remaining project still dispatches.
	other := f.repository(otherProject)
	f.queue(t, other, otherProject, "second")
	awaitAcknowledged(t, func(t *testing.T) []trace.OperationRecord {
		ops, err := other.Operations(otherStream)
		must(t, err)
		if len(ops) != 2 {
			return nil
		}
		return ops
	})
	// The removed project's trace is closed: it can be opened again.
	repo, err := trace.Open(f.cfg.Root, f.cfg.For(project).Project)
	must(t, err)
	must(t, repo.Close())
}

// appendCost records a known cost of amount on stream of repository's
// project at at.
func appendCost(t *testing.T, repository *trace.Repository, stream config.WorkstreamID, id string, at time.Time, amount float64) {
	t.Helper()
	p := repository.Project()
	must(t, repository.Append(context.Background(), trace.Cost{Header: trace.Header{Schema: "osmia.trace.cost", Version: trace.Version, ID: id, Revision: 1, Project: p, Workstream: stream, At: at, Actor: trace.Actor{Kind: "service", ID: "thread-runner"}, Cause: "budget-fixture"},
		Entry: coreadapter.LedgerEntry{Scope: coreadapter.Scope{Project: string(p), Workstream: string(stream), Thread: "thread", Turn: id, Role: masonRole}, AttemptID: id, At: at, Usage: coreadapter.Usage{CostUSD: amount, CostKnown: true, Turns: 1}}}))
}

func TestDailyBudgetCountsTheSpendOfEveryProject(t *testing.T) {
	t.Parallel()
	f := newTwoProjectFixture(t)
	top := filepath.Join(f.opts.Config.Root, "config.toml")
	data, err := os.ReadFile(top)
	must(t, err)
	must(t, os.WriteFile(top, append(data, []byte("[budget]\nper_day = \"1.00\"\n")...), 0600))
	// Each project's spend alone stays under the limit; together they reach it.
	for _, p := range f.cfg.Projects {
		repo, err := trace.Open(f.cfg.Root, p)
		must(t, err)
		appendCost(t, repo, f.streams[p.ID], "cost", demoStart, 0.6)
		must(t, repo.Close())
	}
	f.opts.Location = time.UTC
	s, _ := start(t, f.opts)
	soon(t, "the daily budget pauses the factory", func() bool {
		p, ok := factoryPause(t, s)
		return ok && p.Source == runtime.PauseDailyBudget
	})
	if got := budgetStatus(t, s); got.SpendUSD != "1.2" || got.LimitUSD != "1.00" {
		t.Fatalf("daily budget status %+v", got)
	}
}
