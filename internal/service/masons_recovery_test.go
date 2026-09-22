package service

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/isolation"
	"github.com/kpenfound/osmia/internal/runtime"
	"github.com/kpenfound/osmia/internal/trace"
)

func TestCompletedMasonTurnFinishesAfterRestart(t *testing.T) {
	t.Parallel()
	f, _ := newMasonFixture(t, 1, validPlan)
	defer func() { f.stop(t) }()
	mutation(t, f.c, "PUT", "pause", PauseRequest{Target: runtime.Target{Scope: "factory"}, Mode: "soft", Source: "operator"})
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
	started, err := m.start(ctx, b, "resume")
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
		Result: coreadapter.SessionResult{Session: coreadapter.BackendSession{Backend: q.Request.Profile.Backend, ID: "completed-mason"}, SessionDirectory: directory, StartedAt: q.Claim.At, Outcome: &coreadapter.Outcome{Status: masonDone, Report: string(content)}}}
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
	mutation(t, f.c, "PUT", "pause", PauseRequest{Target: runtime.Target{Scope: "factory"}, Mode: "soft", Source: "operator"})
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
	started, err := m.start(ctx, b, "resume")
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
	changed := filepath.Join(view.Workspace().Directory, masonWrote)
	must(t, os.WriteFile(changed, []byte("package trace\n// recovered\n"), 0600))
	if _, err := os.Stat(filepath.Join(w.Path, masonWrote)); !os.IsNotExist(err) {
		t.Fatalf("edit reached workspace before recovery: %v", err)
	}
	_, err = repo.ClaimTurn(ctx, stream, masonAgent("resume"), "crashed", filepath.Join(f.s.cfg.Root.String(), "threads", string(f.project), string(stream), masonAgent("resume"), masonTurnID("resume")), f.clock.Now())
	must(t, err)
	must(t, repo.Close())
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
}
