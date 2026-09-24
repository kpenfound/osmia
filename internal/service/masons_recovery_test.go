package service

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/isolation"
	"github.com/kpenfound/osmia/internal/runtime"
	"github.com/kpenfound/osmia/internal/trace"
)

func TestCompletedMasonTurnFinishesAfterRestart(t *testing.T) {
	t.Parallel()
	f, _ := newMasonFixture(t, 1, validPlan)
	defer func() { f.stop(t) }()
	mutation(t, f.c, "PUT", "pause", PauseRequest{Target: runtime.Target{Scope: "factory"}, Mode: "soft", Source: "owner"})
	stream, _ := f.builtAs(t, "finish-recovery")
	f.stop(t)
	ctx := context.Background()
	repo, err := trace.Open(f.s.cfg.Root, f.s.cfg.Project)
	must(t, err)
	m := &masons{s: f.s, cfg: f.s.cfg, repository: repo}
	b, found, err := m.read(stream)
	must(t, err)
	if !found {
		t.Fatal("building workstream missing")
	}
	started, _, err := m.start(ctx, b, "resume")
	must(t, err)
	if !started {
		t.Fatal("mason did not start")
	}
	unit := newUnitWorkspaces(f.s.cfg)
	w, _, found, err := unit.find(ctx, stream, "resume")
	must(t, err)
	if !found {
		t.Fatal("unit workspace missing")
	}
	must(t, os.MkdirAll(filepath.Dir(filepath.Join(w.Path, masonWrote)), 0700))
	must(t, os.WriteFile(filepath.Join(w.Path, masonWrote), []byte("package trace\n"), 0600))
	directory := filepath.Join(f.s.cfg.Root.String(), "threads", string(f.project), string(stream), masonAgent("resume"), masonTurnID("resume"))
	q, err := repo.ClaimTurn(ctx, stream, masonAgent("resume"), "completed", directory, f.clock.Now())
	must(t, err)
	content, err := json.Marshal(MasonReport{Outcome: "Built", Criteria: []CriterionReport{resumeReport}})
	must(t, err)
	h := q.Request.Header
	h.Schema, h.ID, h.At = "osmia.trace.turn-response", trace.EventID(q.Request.ID, "response"), f.clock.Now()
	response := trace.TurnResponse{Header: h, AgentID: masonAgent("resume"), ThreadID: masonAgent("resume"), TurnID: q.Request.TurnID, RequestID: q.Request.ID, RequestRevision: q.Request.Revision,
		Result: coreadapter.SessionResult{Session: coreadapter.BackendSession{Backend: q.Request.Profile.Backend, ID: "completed-mason"}, SessionDirectory: directory, StartedAt: q.Claim.At, Outcome: &coreadapter.Outcome{Status: masonDone, Report: string(content), Card: &exampleCard}}}
	must(t, repo.CaptureTurn(ctx, q.Claim.Token, response))
	must(t, repo.CompleteTurn(ctx, stream, masonAgent("resume"), q.Request.TurnID, q.Claim.Token, f.clock.Now()))
	must(t, repo.Close())
	repo, err = trace.Open(f.s.cfg.Root, f.s.cfg.Project)
	must(t, err)
	defer repo.Close()
	m.repository = repo
	must(t, m.Pass(ctx))
	must(t, m.Pass(ctx))
	state, err := repo.Workflow(stream, trace.UnitSubject("resume"))
	must(t, err)
	if state.Value != UnitReviewing {
		t.Fatalf("unit recovered as %s", state.Value)
	}
	docs, err := trace.Read[trace.Document](repo, stream)
	must(t, err)
	reports := 0
	for _, doc := range docs {
		if doc.ID == reportDocument("resume") {
			reports++
		}
	}
	if reports != 1 {
		t.Fatalf("report revisions after recovery: %d", reports)
	}
}

func TestInterruptedMasonViewIsRecoveredBeforeOneContinuation(t *testing.T) {
	t.Parallel()
	f, _ := newMasonFixture(t, 1, validPlan)
	defer func() { f.stop(t) }()
	mutation(t, f.c, "PUT", "pause", PauseRequest{Target: runtime.Target{Scope: "factory"}, Mode: "soft", Source: "owner"})
	stream, _ := f.builtAs(t, "recover")
	f.stop(t)
	ctx := context.Background()
	repo, err := trace.Open(f.s.cfg.Root, f.s.cfg.Project)
	must(t, err)
	m := &masons{s: f.s, cfg: f.s.cfg, repository: repo}
	b, found, err := m.read(stream)
	must(t, err)
	if !found {
		t.Fatal("building workstream missing")
	}
	started, _, err := m.start(ctx, b, "resume")
	must(t, err)
	if !started {
		t.Fatal("mason did not start")
	}
	units := newUnitWorkspaces(f.s.cfg)
	w, _, found, err := units.find(ctx, stream, "resume")
	must(t, err)
	if !found {
		t.Fatal("unit workspace missing")
	}
	paths, err := units.paths(w)
	must(t, err)
	viewRoot := filepath.Join(f.s.cfg.Root.String(), "views", string(f.project), string(stream), masonAgent("resume"), masonTurnID("resume"))
	must(t, os.MkdirAll(viewRoot, 0700))
	view, err := (isolation.Views{Directory: viewRoot}).Create(ctx, coreadapter.Workspace{Directory: w.Path, Access: coreadapter.ReadWrite}, paths)
	must(t, err)
	must(t, os.WriteFile(filepath.Join(viewRoot, "ready"), []byte(filepath.Base(view.Workspace().Directory)), 0600))
	changed := filepath.Join(view.Workspace().Directory, masonWrote)
	must(t, os.WriteFile(changed, []byte("package trace\n// recovered\n"), 0600))
	if _, err := os.Stat(filepath.Join(w.Path, masonWrote)); !os.IsNotExist(err) {
		t.Fatalf("edit reached workspace before recovery: %v", err)
	}
	_, err = repo.ClaimTurn(ctx, stream, masonAgent("resume"), "crashed", filepath.Join(f.s.cfg.Root.String(), "threads", string(f.project), string(stream), masonAgent("resume"), masonTurnID("resume")), f.clock.Now())
	must(t, err)
	must(t, repo.Close())
	store, _, err := runtime.Open(runtime.Inputs{Config: f.s.cfg, Workstreams: []config.WorkstreamID{stream}})
	must(t, err)
	must(t, store.SetProfile(masonRole, "other"))
	f.s.store = store
	defer store.Close()
	repo, err = trace.Open(f.s.cfg.Root, f.s.cfg.Project)
	must(t, err)
	defer repo.Close()
	m.repository = repo
	must(t, m.Pass(ctx))
	must(t, m.Pass(ctx))
	th, err := repo.Thread(stream, masonAgent("resume"))
	must(t, err)
	if len(th.Turns) != 2 || th.Turns[0].Status() != "interrupted" || th.Turns[1].Request.TurnID != masonAgent("resume")+"-recover-1" || !strings.Contains(th.Turns[1].Request.Prompt, "The service stopped during your last turn") {
		t.Fatalf("recovered mason thread: %+v", th)
	}
	if th.Turns[0].Request.Profile.Name != "default" || th.Turns[1].Request.Profile.Name != "other" || th.Turns[1].Request.Profile.Backend != "codex" {
		t.Fatalf("recovery profiles: %+v", th.Turns)
	}
	data, err := os.ReadFile(filepath.Join(w.Path, masonWrote))
	must(t, err)
	if string(data) != "package trace\n// recovered\n" {
		t.Fatalf("recovered workspace file %q", data)
	}
	if _, err := os.Stat(view.Workspace().Directory); !os.IsNotExist(err) {
		t.Fatalf("surviving view was not consumed: %v", err)
	}
	if _, _, found, err := units.find(ctx, stream, "resume"); err != nil || !found {
		t.Fatalf("unit workspace changed after recovery: found %t: %v", found, err)
	}
	// A stop while copying the next view leaves no ready marker. Its partial
	// contents must not replace the complete workspace on another restart.
	next := th.Turns[1].Request.TurnID
	partialRoot := filepath.Join(f.s.cfg.Root.String(), "views", string(f.project), string(stream), masonAgent("resume"), next)
	must(t, os.MkdirAll(partialRoot, 0700))
	_, err = os.MkdirTemp(partialRoot, "turn-")
	must(t, err)
	_, err = repo.ClaimTurn(ctx, stream, masonAgent("resume"), "crashed-again", filepath.Join(f.s.cfg.Root.String(), "threads", string(f.project), string(stream), masonAgent("resume"), next), f.clock.Now())
	must(t, err)
	must(t, repo.Close())
	repo, err = trace.Open(f.s.cfg.Root, f.s.cfg.Project)
	must(t, err)
	defer repo.Close()
	m.repository = repo
	must(t, m.Pass(ctx))
	data, err = os.ReadFile(filepath.Join(w.Path, masonWrote))
	must(t, err)
	if string(data) != "package trace\n// recovered\n" {
		t.Fatalf("partial view replaced the workspace: %q", data)
	}
}

// A unit started just before a stop, its mason's first turn queued and not
// yet run, is recovered from the trace before anything starts: it takes a
// mason slot, the one slot left goes to one more unit, and its turn runs
// once. Later restarts start and run nothing more.
func TestRestartCountsImplementingUnitsBeforeStarting(t *testing.T) {
	t.Parallel()
	f, masons := newParallelMasonFixture(t, 2, 3, disjointPlan)
	defer func() { f.stop(t) }()
	factory := runtime.Target{Scope: "factory"}
	mutation(t, f.c, "PUT", "pause", PauseRequest{Target: factory, Mode: "soft", Source: "owner"})
	stream, _ := f.builtAs(t, "start-recovery")
	f.stop(t)
	repo, err := trace.Open(f.s.cfg.Root, f.s.cfg.Project)
	must(t, err)
	m := newMasonController(f.s, repo)
	b, found, err := m.read(stream)
	must(t, err)
	if !found {
		t.Fatal("building workstream missing")
	}
	started, _, err := m.start(context.Background(), b, "resume")
	must(t, errors.Join(err, repo.Close()))
	if !started {
		t.Fatal("resume did not start")
	}

	want := []string{trace.UnitSubject("resume"), trace.UnitSubject("upload")}
	for restart := range 2 {
		f.start(t)
		if restart == 0 {
			mutation(t, f.c, "DELETE", "pause", factory)
		}
		f.awaitMasonRan(t, stream, "resume")
		f.awaitMasonRan(t, stream, "upload")
		settle()
		masons.check(t)
		if got := starts(t, f, stream); !slices.Equal(got, want) {
			t.Fatalf("after restart %d, started %v, want %v", restart+1, got, want)
		}
		var ran []string
		for _, req := range masons.requests(stream) {
			ran = append(ran, req.Name)
		}
		if slices.Sort(ran); !slices.Equal(ran, []string{masonTurnID("resume"), masonTurnID("upload")}) {
			t.Fatalf("after restart %d, mason turns ran %v", restart+1, ran)
		}
		f.checkUnits(t, stream, []UnitStatus{{Unit: "resume", State: UnitImplementing}, {Unit: "upload", State: UnitImplementing}, f.deferred(t, stream, "audit", slotless(2))})
		f.stop(t)
	}
	f.start(t)
}
