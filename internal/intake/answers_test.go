package intake

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/runtime"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	"github.com/kpenfound/osmia/internal/trace"
)

func TestAnswersAreNotDeliveredToAbandonedWorkstreams(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	home, err := os.MkdirTemp("", "qb-")
	fixturekit.Must(t, err)
	t.Cleanup(func() { os.RemoveAll(home) })
	cfg, err := config.Load(fixturekit.ConfigAt(t, home))
	fixturekit.Must(t, err)
	clock := &fixturekit.FixedClock{At: fixturekit.DemoStart}
	owner := trace.Actor{Kind: "owner", ID: "local"}
	repo, err := trace.Create(ctx, cfg.Root, cfg.Project, clock.Now(), owner)
	fixturekit.Must(t, err)
	defer repo.Close()
	fixturekit.RecordCharter(t, repo, "# Charter\n\n1. Keep state in files.\n")
	for _, ws := range []config.WorkstreamID{fixturekit.Stream, questionQuiet} {
		fixturekit.Must(t, repo.CreateWorkstream(ctx, ws, clock.Now(), owner))
		identity := trace.Agent{Header: trace.Header{Schema: "osmia.trace.agent", Version: 1, Revision: 1, ID: fixturekit.DemoAgent, Project: fixturekit.Project, Workstream: ws, At: clock.Now(), Actor: owner, Cause: "workstream_created"}, Role: fixturekit.DemoRole, ThreadID: fixturekit.DemoThread}
		fixturekit.Must(t, repo.CreateThread(ctx, identity))
		_, err := repo.Ask(ctx, fixturekit.DemoAgent, fixturekit.ClaimTurn(t, repo, home, ws, fixturekit.DemoAgent, fixturekit.DemoThread, "build", clock.Now()), "Where does state live?", clock.Now())
		fixturekit.Must(t, err)
		_, err = repo.AnswerQuestion(ctx, trace.ChiefOfStaff, fixturekit.ClaimTurn(t, repo, home, ws, trace.ChiefOfStaff, trace.ChiefOfStaff, "events", clock.Now()), "1", "In files.", []string{"charter#1"}, clock.Now())
		fixturekit.Must(t, err)
	}
	h := trace.Header{Schema: "osmia.trace.transition", Version: trace.Version, ID: AbandonTransition, Revision: 1, Project: fixturekit.Project, Workstream: fixturekit.Stream, At: clock.Now(), Actor: owner, Cause: AbandonTransition}
	_, err = repo.SetFeatureState(ctx, h, AbandonedState, "gone")
	fixturekit.Must(t, err)

	store, _, err := runtime.Open(runtime.Inputs{Config: cfg, Workstreams: map[config.ProjectID][]config.WorkstreamID{cfg.Project.ID: []config.WorkstreamID{fixturekit.Stream, questionQuiet}}})
	fixturekit.Must(t, err)
	defer store.Close()
	s := New(Dependencies{
		Now:       clock.Now,
		Store:     func() *runtime.Store { return store },
		Effective: func() (runtime.State, []runtime.Diagnostic) { return store.EffectiveAt(clock.Now()) },
		RoleExecution: func(c *config.Config, role string) (coreadapter.Profile, coreadapter.ExecutionSettings, error) {
			profile, err := c.NamedProfile(c.Roles[role].Profile)
			return profile, coreadapter.ExecutionSettings{}, err
		},
	})
	fixturekit.Must(t, s.Answers(cfg, repo).Pass(ctx))
	abandonedThread, err := repo.Thread(fixturekit.Stream, fixturekit.DemoAgent)
	fixturekit.Must(t, err)
	live, err := repo.Thread(questionQuiet, fixturekit.DemoAgent)
	fixturekit.Must(t, err)
	if len(abandonedThread.Turns) != 1 || len(live.Turns) != 2 {
		t.Fatalf("abandoned workstream has %d turns, live one %d", len(abandonedThread.Turns), len(live.Turns))
	}

	if req := live.Turns[1].Request; req.TurnID != "answer_1" || req.Profile.Name != "default" || req.Profile.Backend != "claude" || req.Profile.Model != "test" || !req.At.Equal(clock.Now()) {
		t.Fatalf("answer turn: %+v", req)
	}
}

func TestAnswersAreNotDeliveredToMergedUnits(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	home, err := os.MkdirTemp("", "qm-")
	fixturekit.Must(t, err)
	t.Cleanup(func() { os.RemoveAll(home) })
	cfg, err := config.Load(fixturekit.ConfigAt(t, home))
	fixturekit.Must(t, err)
	clock := &fixturekit.FixedClock{At: fixturekit.DemoStart}
	owner := trace.Actor{Kind: "owner", ID: "local"}
	repo, err := trace.Create(ctx, cfg.Root, cfg.Project, clock.Now(), owner)
	fixturekit.Must(t, err)
	defer repo.Close()
	fixturekit.RecordCharter(t, repo, "# Charter\n\n1. Keep state in files.\n")
	for _, ws := range []config.WorkstreamID{fixturekit.Stream, questionQuiet} {
		fixturekit.Must(t, repo.CreateWorkstream(ctx, ws, clock.Now(), owner))
		identity := trace.Agent{Header: trace.Header{Schema: "osmia.trace.agent", Version: 1, Revision: 1, ID: fixturekit.DemoAgent, Project: fixturekit.Project, Workstream: ws, At: clock.Now(), Actor: owner, Cause: "workstream_created"}, Role: fixturekit.DemoRole, ThreadID: fixturekit.DemoThread}
		fixturekit.Must(t, repo.CreateThread(ctx, identity))
		h := trace.Header{Schema: "osmia.trace.turn-request", Version: 1, Revision: 1, ID: "request_build", Project: fixturekit.Project, Workstream: ws, Unit: "resume", At: clock.Now(), Actor: owner, Cause: "dispatch"}
		_, err := repo.EnqueueTurn(ctx, trace.TurnRequest{Header: h, AgentID: fixturekit.DemoAgent, ThreadID: fixturekit.DemoThread, TurnID: "build", Profile: coreadapter.Profile{Name: "other", Backend: "codex", Model: "other"}, Prompt: "Work"})
		fixturekit.Must(t, err)
		_, err = repo.ClaimTurn(ctx, ws, fixturekit.DemoAgent, "token_build", filepath.Join(home, string(ws), "build"), clock.Now())
		fixturekit.Must(t, err)
		scope := coreadapter.Scope{Project: string(fixturekit.Project), Workstream: string(ws), Unit: "resume", Thread: fixturekit.DemoThread, Turn: "build", Role: fixturekit.DemoRole}
		_, err = repo.Ask(ctx, fixturekit.DemoAgent, scope, "Where does state live?", clock.Now())
		fixturekit.Must(t, err)
		_, err = repo.AnswerQuestion(ctx, trace.ChiefOfStaff, fixturekit.ClaimTurn(t, repo, home, ws, trace.ChiefOfStaff, trace.ChiefOfStaff, "events", clock.Now()), "1", "In files.", []string{"charter#1"}, clock.Now())
		fixturekit.Must(t, err)
	}
	recordQuestionUnitMerged(t, repo, fixturekit.Stream, "resume")

	store, _, err := runtime.Open(runtime.Inputs{Config: cfg, Workstreams: map[config.ProjectID][]config.WorkstreamID{cfg.Project.ID: []config.WorkstreamID{fixturekit.Stream, questionQuiet}}})
	fixturekit.Must(t, err)
	defer store.Close()
	s := New(Dependencies{
		Now:       clock.Now,
		Store:     func() *runtime.Store { return store },
		Effective: func() (runtime.State, []runtime.Diagnostic) { return store.EffectiveAt(clock.Now()) },
		RoleExecution: func(c *config.Config, role string) (coreadapter.Profile, coreadapter.ExecutionSettings, error) {
			profile, err := c.NamedProfile(c.Roles[role].Profile)
			return profile, coreadapter.ExecutionSettings{}, err
		},
	})
	fixturekit.Must(t, s.Answers(cfg, repo).Pass(ctx))
	merged, err := repo.Thread(fixturekit.Stream, fixturekit.DemoAgent)
	fixturekit.Must(t, err)
	inFlight, err := repo.Thread(questionQuiet, fixturekit.DemoAgent)
	fixturekit.Must(t, err)
	if len(merged.Turns) != 1 {
		t.Fatalf("the merged unit's asker has turns %+v, want only the asking turn", merged.Turns)
	}
	if len(inFlight.Turns) != 2 || inFlight.Turns[1].Request.TurnID != "answer_1" {
		t.Fatalf("the in-flight unit's asker has turns %+v, want the asking turn and answer_1", inFlight.Turns)
	}
}

func recordQuestionUnitMerged(t *testing.T, repository *trace.Repository, stream config.WorkstreamID, unit string) {
	t.Helper()
	state, err := repository.Workflow(stream, trace.UnitSubject(unit))
	fixturekit.Must(t, err)
	h := trace.Header{Schema: "osmia.trace.transition", Version: trace.Version, ID: trace.UnitSubject(unit) + "-" + UnitMerged, Revision: 1, Project: repository.Project(), Workstream: stream, Unit: unit, At: time.Now().UTC(), Actor: trace.Actor{Kind: "service", ID: "foreman"}, Cause: "landing-" + unit}
	_, err = repository.Transact(context.Background(), trace.Transaction{ExpectedVersion: state.Version,
		Transition: trace.Transition{Header: h, Subject: trace.UnitSubject(unit), From: state.Value, To: UnitMerged, Reason: fmt.Sprintf("unit %s landed", unit)}})
	fixturekit.Must(t, err)
}

const questionQuiet = config.WorkstreamID("w_fedcba9876543210fedcba9876543210")
