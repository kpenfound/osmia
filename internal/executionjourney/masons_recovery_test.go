package executionjourney

import (
	context "context"
	json "encoding/json"
	errors "errors"
	os "os"
	filepath "path/filepath"
	slices "slices"
	strings "strings"
	testing "testing"

	"github.com/kpenfound/osmia/internal/api"
	"github.com/kpenfound/osmia/internal/execution"
	"github.com/kpenfound/osmia/internal/intake"
	"github.com/kpenfound/osmia/internal/service"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	"github.com/kpenfound/osmia/internal/servicetest/intakefixture"
	"github.com/kpenfound/osmia/internal/servicetest/journey"

	config "github.com/kpenfound/osmia/internal/config"
	coreadapter "github.com/kpenfound/osmia/internal/coreadapter"
	isolation "github.com/kpenfound/osmia/internal/isolation"
	runtime "github.com/kpenfound/osmia/internal/runtime"
	trace "github.com/kpenfound/osmia/internal/trace"
)

func TestCompletedMasonTurnFinishesAfterRestart(t *testing.T) {
	t.Parallel()
	f, _ := journey.NewMasonFixture(t, 1, fixturekit.ValidPlan)
	defer func() { f.Stop(t) }()
	stream := seedBuildingPaused(f, t, runtime.Target{Scope: "factory"}, fixturekit.ValidPlan, "finish-recovery")[0]
	f.Stop(t)
	ctx := context.Background()
	repo, err := trace.Open(f.S.Cfg.Root, f.S.Cfg.Project)
	fixturekit.Must(t, err)
	m := &journey.Masons{S: f.S, Cfg: f.S.Cfg, Repository: repo}
	b, found, err := m.Read(stream)
	fixturekit.Must(t, err)
	if !found {
		t.Fatal("building workstream missing")
	}
	started, _, err := m.Start(ctx, b, "resume")
	fixturekit.Must(t, err)
	if !started {
		t.Fatal("mason did not start")
	}
	unit := journey.NewUnitWorkspaces(f.S.Cfg, repo)
	w, _, found, err := unit.Find(ctx, stream, "resume")
	fixturekit.Must(t, err)
	if !found {
		t.Fatal("unit workspace missing")
	}
	fixturekit.Must(t, os.MkdirAll(filepath.Dir(filepath.Join(w.Path, fixturekit.MasonWrote)), 0700))
	fixturekit.Must(t, os.WriteFile(filepath.Join(w.Path, fixturekit.MasonWrote), []byte("package trace\n"), 0600))
	directory := filepath.Join(f.S.Cfg.Root.String(), "threads", string(f.Project), string(stream), intake.MasonAgent("resume"), execution.MasonTurnID("resume"))
	q, err := repo.ClaimTurn(ctx, stream, intake.MasonAgent("resume"), "completed", directory, f.Clock.Now())
	fixturekit.Must(t, err)
	content, err := json.Marshal(service.MasonReport{Outcome: "Built"})
	fixturekit.Must(t, err)
	h := q.Request.Header
	h.Schema, h.ID, h.At = "osmia.trace.turn-response", trace.EventID(q.Request.ID, "response"), f.Clock.Now()
	response := trace.TurnResponse{Header: h, AgentID: intake.MasonAgent("resume"), ThreadID: intake.MasonAgent("resume"), TurnID: q.Request.TurnID, RequestID: q.Request.ID, RequestRevision: q.Request.Revision,
		Result: coreadapter.SessionResult{Session: coreadapter.BackendSession{Backend: q.Request.Profile.Backend, ID: "completed-mason"}, SessionDirectory: directory, StartedAt: q.Claim.At, Outcome: &coreadapter.Outcome{Status: execution.MasonDone, Report: string(content), Card: &intakefixture.ExampleCard}}}
	fixturekit.Must(t, repo.CaptureTurn(ctx, q.Claim.Token, response))
	fixturekit.Must(t, repo.CompleteTurn(ctx, stream, intake.MasonAgent("resume"), q.Request.TurnID, q.Claim.Token, f.Clock.Now()))
	fixturekit.Must(t, repo.Close())
	repo, err = trace.Open(f.S.Cfg.Root, f.S.Cfg.Project)
	fixturekit.Must(t, err)
	defer repo.Close()
	m.Repository = repo
	fixturekit.Must(t, m.Pass(ctx))
	fixturekit.Must(t, m.Pass(ctx))
	state, err := repo.Workflow(stream, trace.UnitSubject("resume"))
	fixturekit.Must(t, err)
	if state.Value != execution.UnitChecking {
		t.Fatalf("unit recovered as %s", state.Value)
	}
	docs, err := trace.Read[trace.Document](repo, stream)
	fixturekit.Must(t, err)
	reports := 0
	for _, doc := range docs {
		if doc.ID == execution.ReportDocument("resume") {
			reports++
		}
	}
	if reports != 1 {
		t.Fatalf("report revisions after recovery: %d", reports)
	}
}

func TestInterruptedMasonViewIsRecoveredBeforeOneContinuation(t *testing.T) {
	t.Parallel()
	f, _ := journey.NewMasonFixture(t, 1, fixturekit.ValidPlan)
	defer func() { f.Stop(t) }()
	stream := seedBuildingPaused(f, t, runtime.Target{Scope: "factory"}, fixturekit.ValidPlan, "recover")[0]
	f.Stop(t)
	ctx := context.Background()
	repo, err := trace.Open(f.S.Cfg.Root, f.S.Cfg.Project)
	fixturekit.Must(t, err)
	m := &journey.Masons{S: f.S, Cfg: f.S.Cfg, Repository: repo}
	b, found, err := m.Read(stream)
	fixturekit.Must(t, err)
	if !found {
		t.Fatal("building workstream missing")
	}
	started, _, err := m.Start(ctx, b, "resume")
	fixturekit.Must(t, err)
	if !started {
		t.Fatal("mason did not start")
	}
	units := journey.NewUnitWorkspaces(f.S.Cfg, repo)
	w, _, found, err := units.Find(ctx, stream, "resume")
	fixturekit.Must(t, err)
	if !found {
		t.Fatal("unit workspace missing")
	}
	paths, err := units.Driver().Paths(w)
	fixturekit.Must(t, err)
	viewRoot := filepath.Join(f.S.Cfg.Root.String(), "views", string(f.Project), string(stream), intake.MasonAgent("resume"), execution.MasonTurnID("resume"))
	fixturekit.Must(t, os.MkdirAll(viewRoot, 0700))
	view, err := (isolation.Views{Directory: viewRoot}).Create(ctx, coreadapter.Workspace{Directory: w.Path, Access: coreadapter.ReadWrite}, paths)
	fixturekit.Must(t, err)
	fixturekit.Must(t, os.WriteFile(filepath.Join(viewRoot, "ready"), []byte(filepath.Base(view.Workspace().Directory)), 0600))
	changed := filepath.Join(view.Workspace().Directory, fixturekit.MasonWrote)
	fixturekit.Must(t, os.WriteFile(changed, []byte("package trace\n// recovered\n"), 0600))
	if _, err := os.Stat(filepath.Join(w.Path, fixturekit.MasonWrote)); !os.IsNotExist(err) {
		t.Fatalf("edit reached workspace before recovery: %v", err)
	}
	_, err = repo.ClaimTurn(ctx, stream, intake.MasonAgent("resume"), "crashed", filepath.Join(f.S.Cfg.Root.String(), "threads", string(f.Project), string(stream), intake.MasonAgent("resume"), execution.MasonTurnID("resume")), f.Clock.Now())
	fixturekit.Must(t, err)
	fixturekit.Must(t, repo.Close())
	store, _, err := runtime.Open(runtime.Inputs{Config: f.S.Cfg, Workstreams: map[config.ProjectID][]config.WorkstreamID{f.S.Cfg.Project.ID: []config.WorkstreamID{stream}}})
	fixturekit.Must(t, err)
	fixturekit.Must(t, store.SetProfile(intake.MasonRole, "other"))
	f.S.Store = store
	journey.InspectForTest(f.S).SetRuntimeStore(store)
	fixturekit.Must(t, journey.InspectForTest(f.S).SetStoppedRuntime(store))
	defer store.Close()
	repo, err = trace.Open(f.S.Cfg.Root, f.S.Cfg.Project)
	fixturekit.Must(t, err)
	defer repo.Close()
	m.Repository = repo
	units.Repository = repo
	fixturekit.Must(t, m.Pass(ctx))
	fixturekit.Must(t, m.Pass(ctx))
	th, err := repo.Thread(stream, intake.MasonAgent("resume"))
	fixturekit.Must(t, err)
	if len(th.Turns) != 2 || th.Turns[0].Status() != "interrupted" || th.Turns[1].Request.TurnID != intake.MasonAgent("resume")+"-recover-1" || !strings.Contains(th.Turns[1].Request.Prompt, "The service stopped during your last turn") {
		t.Fatalf("recovered mason thread: %+v", th)
	}
	if th.Turns[0].Request.Profile.Name != "default" || th.Turns[1].Request.Profile.Name != "other" || th.Turns[1].Request.Profile.Backend != "codex" {
		t.Fatalf("recovery profiles: %+v", th.Turns)
	}
	data, err := os.ReadFile(filepath.Join(w.Path, fixturekit.MasonWrote))
	fixturekit.Must(t, err)
	if string(data) != "package trace\n// recovered\n" {
		t.Fatalf("recovered workspace file %q", data)
	}
	if _, err := os.Stat(view.Workspace().Directory); !os.IsNotExist(err) {
		t.Fatalf("surviving view was not consumed: %v", err)
	}
	if _, _, found, err := units.Find(ctx, stream, "resume"); err != nil || !found {
		t.Fatalf("unit workspace changed after recovery: found %t: %v", found, err)
	}

	next := th.Turns[1].Request.TurnID
	partialRoot := filepath.Join(f.S.Cfg.Root.String(), "views", string(f.Project), string(stream), intake.MasonAgent("resume"), next)
	fixturekit.Must(t, os.MkdirAll(partialRoot, 0700))
	_, err = os.MkdirTemp(partialRoot, "turn-")
	fixturekit.Must(t, err)
	_, err = repo.ClaimTurn(ctx, stream, intake.MasonAgent("resume"), "crashed-again", filepath.Join(f.S.Cfg.Root.String(), "threads", string(f.Project), string(stream), intake.MasonAgent("resume"), next), f.Clock.Now())
	fixturekit.Must(t, err)
	fixturekit.Must(t, repo.Close())
	repo, err = trace.Open(f.S.Cfg.Root, f.S.Cfg.Project)
	fixturekit.Must(t, err)
	defer repo.Close()
	m.Repository = repo
	fixturekit.Must(t, m.Pass(ctx))
	data, err = os.ReadFile(filepath.Join(w.Path, fixturekit.MasonWrote))
	fixturekit.Must(t, err)
	if string(data) != "package trace\n// recovered\n" {
		t.Fatalf("partial view replaced the workspace: %q", data)
	}
}

func TestRestartCountsImplementingUnitsBeforeStarting(t *testing.T) {
	t.Parallel()
	f, masons := journey.NewParallelMasonFixture(t, 2, 3, intakefixture.DisjointPlan)

	clarification := intake.MasonAgent("upload") + "-clarify-1"
	masons.Play[clarification] = intakefixture.FailMasonTurn
	f.Engine.Mu.Lock()
	f.Engine.Turns[clarification] = masons.Turn
	f.Engine.Mu.Unlock()
	defer func() { f.Stop(t) }()
	factory := runtime.Target{Scope: "factory"}
	stream := seedBuildingPaused(f, t, factory, intakefixture.DisjointPlan, "start-recovery")[0]
	f.Stop(t)
	repo, err := trace.Open(f.S.Cfg.Root, f.S.Cfg.Project)
	fixturekit.Must(t, err)
	m := journey.NewMasonController(f.S, repo)
	b, found, err := m.Read(stream)
	fixturekit.Must(t, err)
	if !found {
		t.Fatal("building workstream missing")
	}
	started, _, err := m.Start(context.Background(), b, "resume")
	fixturekit.Must(t, errors.Join(err, repo.Close()))
	if !started {
		t.Fatal("resume did not start")
	}

	want := []string{trace.UnitSubject("resume"), trace.UnitSubject("upload")}
	for restart := range 2 {
		f.Start(t)
		if restart == 0 {
			intakefixture.Mutation(t, f.C, "DELETE", "pause", factory)
		}
		f.AwaitMasonRan(t, stream, "resume")
		f.AwaitMasonRan(t, stream, "upload")
		awaitDeferral(f, t, stream, "audit", execution.DeferCapacity)
		masons.Check(t)
		if got := journey.Starts(t, f, stream); !slices.Equal(got, want) {
			t.Fatalf("after restart %d, started %v, want %v", restart+1, got, want)
		}
		var ran []string
		for _, req := range implementationRuns(masons, stream) {
			ran = append(ran, req.Name)
		}
		if slices.Sort(ran); !slices.Equal(ran, []string{execution.MasonTurnID("resume"), execution.MasonTurnID("upload")}) {
			t.Fatalf("after restart %d, mason turns ran %v", restart+1, ran)
		}
		f.CheckUnits(t, stream, []api.UnitStatus{{Unit: "resume", State: intake.UnitImplementing}, {Unit: "upload", State: intake.UnitImplementing}, deferred(f, t, stream, "audit", slotless(2))})
		f.Stop(t)
	}
	f.Start(t)
}
