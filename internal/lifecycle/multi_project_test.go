package lifecycle_test

import (
	context "context"
	errors "errors"
	"github.com/kpenfound/osmia/internal/intake"
	os "os"
	filepath "path/filepath"
	slices "slices"
	strings "strings"
	atomic "sync/atomic"
	testing "testing"
	time "time"

	"github.com/kpenfound/osmia/internal/api"
	config "github.com/kpenfound/osmia/internal/config"
	coreadapter "github.com/kpenfound/osmia/internal/coreadapter"
	runtime "github.com/kpenfound/osmia/internal/runtime"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	"github.com/kpenfound/osmia/internal/servicetest/intakefixture"
	trace "github.com/kpenfound/osmia/internal/trace"
)

func TestSeveralProjectsShareCapacityPauseApartAndResumeFromTheirOwnTraces(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newTwoProjectFixture(t)
	store, _, err := runtime.Open(runtime.Inputs{Config: f.cfg})
	fixturekit.Must(t, err)
	fixturekit.Must(t, store.SetPause(runtime.Pause{Target: runtime.Target{Scope: "project", Project: fixturekit.Project}, Mode: "soft", Source: runtime.PauseOwner, Reason: "Owner pause"}))
	fixturekit.Must(t, store.Close())
	otherEntered, otherRelease := f.block(otherProject, "first")
	entered, _ := f.block(fixturekit.Project, "first")

	var passed atomic.Int64
	f.opts.schedulePassed = func() { passed.Add(1) }
	s, err := Start(ctx, f.opts)
	fixturekit.Must(t, err)
	stopped := false
	defer func() {
		if !stopped {
			s.Close()
		}
	}()
	if got := s.current().ProjectIDs(); !slices.Equal(got, []config.ProjectID{fixturekit.Project, otherProject}) {
		t.Fatalf("active projects %v", got)
	}
	intakefixture.Await(t, "the other project's turn", otherEntered)
	paused, other := f.repository(fixturekit.Project), f.repository(otherProject)
	if paused == nil || other == nil || paused == other {
		t.Fatalf("each project needs its own trace: %v %v", paused, other)
	}
	intakefixture.Soon(t, "the other project's turn holds the only mason slot", func() bool {
		st := s.capacityStatus(nil)
		return st.Roles[0].Used == 1 && len(st.Roles[0].Waiting) == 0
	})
	if got := f.dispatchedTurns(t, paused, fixturekit.Project); len(got) != 0 {
		t.Fatalf("paused project dispatched %v", got)
	}

	fixturekit.Must(t, s.store.ClearPause(runtime.Target{Scope: "project", Project: fixturekit.Project}, runtime.PauseOwner))
	intakefixture.Soon(t, "the first project's turn waits for the shared mason slot", func() bool {
		st := s.capacityStatus(nil)
		if st.Roles[0].Used != 1 || len(st.Roles[0].Waiting) != 1 {
			return false
		}
		w := st.Roles[0].Waiting[0]
		return w.Workstream == fixturekit.Stream && w.Turn == intakefixture.Turn(fixturekit.Project, "first") && w.Reason == "capacity"
	})
	intakefixture.AwaitPasses(t, &passed, 3)
	if got := f.dispatchedTurns(t, paused, fixturekit.Project); len(got) != 0 {
		t.Fatalf("first project dispatched %v past the shared capacity", got)
	}

	close(otherRelease)
	intakefixture.Await(t, "the first project's turn", entered)
	f.queue(t, other, otherProject, "second")
	fixturekit.Must(t, s.Close())
	stopped = true

	s, err = Start(ctx, f.opts)
	fixturekit.Must(t, err)
	stopped = false
	for _, id := range []config.ProjectID{fixturekit.Project, otherProject} {
		fixturekit.AwaitAcknowledged(t, func(t *testing.T) []trace.OperationRecord {
			ops, err := f.repository(id).Operations(f.streams[id])
			fixturekit.Must(t, err)
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
	fixturekit.Must(t, s.Close())
	stopped = true
	for _, p := range f.cfg.Projects {
		repo, err := trace.Open(f.cfg.Root, p)
		fixturekit.Must(t, err)
		th, err := repo.Thread(f.streams[p.ID], fixturekit.DemoAgent)
		fixturekit.Must(t, err)
		want := []string{intakefixture.Turn(p.ID, "first")}
		if p.ID == otherProject {
			want = append(want, intakefixture.Turn(p.ID, "second"))
		}
		if got := f.dispatchedTurns(t, repo, p.ID); !slices.Equal(got, want) {
			t.Fatalf("project %s dispatched %v, want %v", p.ID, got, want)
		}
		for _, q := range th.Turns {
			if q.CompletedAt.IsZero() || q.Request.Project != p.ID {
				t.Fatalf("project %s turn %s: %+v", p.ID, q.Request.TurnID, q)
			}
		}
		fixturekit.Must(t, repo.Close())
	}
}

func TestProjectScopedRequestsNameOneOfSeveralProjects(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newTwoProjectFixture(t)
	s, c := start(t, f.opts)
	text := "Build it"
	both := string(fixturekit.Project) + ", " + string(otherProject)
	_, err := c.HandIn(ctx, api.HandInRequest{Key: "k", Stdin: &text})
	var apiFault *api.APIError
	if !errors.As(err, &apiFault) || apiFault.Code != api.Validation || !strings.Contains(apiFault.Message, "several projects are active; name one of "+both) {
		t.Fatalf("hand-in without a project: %v", err)
	}
	unknown := config.ProjectID("p_00000000000000000000000000000000")
	for name, call := range map[string]func() error{
		"rebase": func() error { _, err := c.RebaseProject(ctx, ""); return err },
		"answer": func() error { _, err := c.Answer(ctx, 1, "yes", ""); return err },
	} {
		if err := call(); !errors.As(err, &apiFault) || apiFault.Code != api.Validation || !strings.Contains(apiFault.Message, both) {
			t.Fatalf("%s without a project: %v", name, err)
		}
	}
	if _, err := c.RebaseProject(ctx, unknown); !errors.As(err, &apiFault) || apiFault.Code != api.NotFound || !strings.Contains(apiFault.Message, "the active projects are "+both) {
		t.Fatalf("rebase of an inactive project: %v", err)
	}
	out, err := c.RebaseProject(ctx, otherProject)
	fixturekit.Must(t, err)
	if out.Project != otherProject || len(out.Covered)+len(out.Skipped) != 1 || len(out.Covered) == 1 && out.Covered[0].Workstream != otherStream || len(out.Skipped) == 1 && out.Skipped[0].Workstream != otherStream {
		t.Fatalf("rebase of the other project: %+v", out)
	}
	cfg, err := c.Configuration(ctx)
	fixturekit.Must(t, err)
	if cfg.Project != nil || len(cfg.Projects) != 2 || !(cfg.Projects[0].ID == fixturekit.Project && cfg.Projects[1].ID == otherProject || cfg.Projects[0].ID == otherProject && cfg.Projects[1].ID == fixturekit.Project) {
		t.Fatalf("configuration projects: %+v %+v", cfg.Project, cfg.Projects)
	}
	if got := cfg.Effective.ActiveProjects; len(got) != 2 || got[0] != string(fixturekit.Project) || got[1] != string(otherProject) {
		t.Fatalf("effective active projects: %v", got)
	}
	status, err := c.Statuses(ctx)
	fixturekit.Must(t, err)
	var listed []config.ProjectID
	for _, w := range status.Workstreams {
		listed = append(listed, w.Project)
	}
	slices.Sort(listed)
	if !slices.Equal(listed, []config.ProjectID{fixturekit.Project, otherProject}) {
		t.Fatalf("status lists workstreams of %v", listed)
	}
	if _, apiFault := s.workstreamStatus(string(otherStream)); apiFault != nil {
		t.Fatalf("status of the other project's workstream: %v", apiFault)
	}
	if p, ws, repo, apiFault := s.conversationTrace(string(otherStream)); apiFault != nil || p != otherProject || ws != otherStream || repo.Project() != otherProject {
		t.Fatalf("conversation of the other project's workstream: %v %v %v", p, ws, apiFault)
	}
}

func TestRemovingOneOfSeveralProjectsLeavesTheOtherRunning(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newTwoProjectFixture(t)
	s, c := start(t, f.opts)
	for _, id := range []config.ProjectID{fixturekit.Project, otherProject} {
		fixturekit.AwaitAcknowledged(t, func(t *testing.T) []trace.OperationRecord {
			ops, err := f.repository(id).Operations(f.streams[id])
			fixturekit.Must(t, err)
			return ops
		})
	}
	_, err := c.Abandon(ctx, f.streams[fixturekit.Project], "Owner finished this project's work.")
	fixturekit.Must(t, err)
	removed, err := c.RemoveProject(ctx, fixturekit.Project)
	fixturekit.Must(t, err)
	if removed.Project.ID != fixturekit.Project {
		t.Fatalf("removed %+v", removed.Project)
	}
	cfg, err := c.Configuration(ctx)
	fixturekit.Must(t, err)
	if cfg.Project == nil || cfg.Project.ID != otherProject || len(cfg.Projects) != 1 || cfg.Projects[0].ID != otherProject {
		t.Fatalf("configuration after removal: %+v %+v", cfg.Project, cfg.Projects)
	}
	if traces := s.traces(); len(traces) != 1 || traces[0].Project() != otherProject {
		t.Fatalf("traces in the capacity pool: %d", len(traces))
	}
	if got := activeProjects(t, f.opts.Config.Root); !slices.Equal(got, []string{string(otherProject)}) {
		t.Fatalf("registry %v", got)
	}
	other := f.repository(otherProject)
	f.queue(t, other, otherProject, "second")
	fixturekit.AwaitAcknowledged(t, func(t *testing.T) []trace.OperationRecord {
		ops, err := other.Operations(otherStream)
		fixturekit.Must(t, err)
		if len(ops) != 2 {
			return nil
		}
		return ops
	})
	repo, err := trace.Open(f.cfg.Root, f.cfg.For(fixturekit.Project).Project)
	fixturekit.Must(t, err)
	fixturekit.Must(t, repo.Close())
}

func appendCost(t *testing.T, repository *trace.Repository, stream config.WorkstreamID, id string, at time.Time, amount float64) {
	t.Helper()
	p := repository.Project()
	fixturekit.Must(t, repository.Append(context.Background(), trace.Cost{Header: trace.Header{Schema: "osmia.trace.cost", Version: trace.Version, ID: id, Revision: 1, Project: p, Workstream: stream, At: at, Actor: trace.Actor{Kind: "service", ID: "thread-runner"}, Cause: "budget-fixture"},
		Entry: coreadapter.LedgerEntry{Scope: coreadapter.Scope{Project: string(p), Workstream: string(stream), Thread: "thread", Turn: id, Role: intake.MasonRole}, AttemptID: id, At: at, Usage: coreadapter.Usage{CostUSD: amount, CostKnown: true, Turns: 1}}}))
}

func TestDailyBudgetCountsTheSpendOfEveryProject(t *testing.T) {
	t.Parallel()
	f := newTwoProjectFixture(t)
	top := filepath.Join(f.opts.Config.Root, "config.toml")
	data, err := os.ReadFile(top)
	fixturekit.Must(t, err)
	fixturekit.Must(t, os.WriteFile(top, append(data, []byte("[budget]\nper_day = \"1.00\"\n")...), 0600))
	for _, p := range f.cfg.Projects {
		repo, err := trace.Open(f.cfg.Root, p)
		fixturekit.Must(t, err)
		appendCost(t, repo, f.streams[p.ID], "cost", fixturekit.DemoStart, 0.6)
		fixturekit.Must(t, repo.Close())
	}
	f.opts.Location = time.UTC
	s, _ := start(t, f.opts)
	intakefixture.Soon(t, "the daily budget pauses the factory", func() bool {
		p, ok := factoryPause(t, s)
		return ok && p.Source == runtime.PauseDailyBudget
	})
	if got := budgetStatus(t, s); got.SpendUSD != "1.2" || got.LimitUSD != "1.00" {
		t.Fatalf("daily budget status %+v", got)
	}
}
