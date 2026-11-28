package intakejourney

import (
	context "context"
	json "encoding/json"
	errors "errors"
	os "os"
	filepath "path/filepath"
	reflect "reflect"
	testing "testing"
	time "time"

	"github.com/kpenfound/osmia/internal/api"
	"github.com/kpenfound/osmia/internal/intake"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	"github.com/kpenfound/osmia/internal/servicetest/journey"

	config "github.com/kpenfound/osmia/internal/config"
	coreadapter "github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/projectstore"
	questions "github.com/kpenfound/osmia/internal/questions"
	intakefixture "github.com/kpenfound/osmia/internal/servicetest/intakefixture"
	trace "github.com/kpenfound/osmia/internal/trace"
)

type charterFixture struct {
	opts  journey.Options
	cfg   *config.Config
	clock *fixturekit.FixedClock
	ticks chan time.Time
	s     *journey.Service
	c     *journey.Client
}

func newCharterFixture(t *testing.T) *charterFixture {
	t.Helper()
	ctx := context.Background()
	home, err := os.MkdirTemp("", "cr-")
	fixturekit.Must(t, err)
	t.Cleanup(func() { os.RemoveAll(home) })
	f := &charterFixture{opts: journey.FixtureAt(t, home), clock: &fixturekit.FixedClock{At: fixturekit.DemoStart}, ticks: make(chan time.Time)}
	f.opts.Reconciliation.Now, f.opts.Reconciliation.Ticks = f.clock.Now, f.ticks
	f.cfg, err = projectstore.LoadConfig(f.opts.Config)
	fixturekit.Must(t, err)
	fixturekit.DemoGit(t, home, "init", "-q", f.cfg.Project.Clone)
	repo, err := trace.Create(ctx, f.cfg.Root, f.cfg.Project, f.clock.Now(), intake.OwnerActor)
	fixturekit.Must(t, err)
	current, err := repo.Charter(ctx)
	fixturekit.Must(t, err)
	_, err = repo.EditCharter(ctx, current.Revision, intakefixture.CharterBefore, f.clock.Now())
	fixturekit.Must(t, err)
	for _, ws := range []config.WorkstreamID{fixturekit.Stream, intakefixture.Quiet} {
		fixturekit.Must(t, repo.CreateWorkstream(ctx, ws, f.clock.Now(), intake.OwnerActor))
	}
	h := trace.Header{Schema: "osmia.trace.agent", Version: trace.Version, ID: "agent_mason", Revision: 1, Project: fixturekit.Project, Workstream: fixturekit.Stream, At: f.clock.Now(), Actor: intake.ServiceActor, Cause: "fixture"}
	fixturekit.Must(t, repo.CreateThread(ctx, trace.Agent{Header: h, Role: intake.MasonRole, ThreadID: "thread_mason"}))
	_, err = repo.EnsureManager(ctx, fixturekit.Stream, f.clock.Now(), intake.ServiceActor)
	fixturekit.Must(t, err)
	mason := f.claim(t, repo, "agent_mason", "thread_mason", intake.MasonRole, "build", intake.ServiceActor)
	_, err = repo.Ask(ctx, "agent_mason", mason, "May an upload restart from zero?", f.clock.Now())
	fixturekit.Must(t, err)
	chief := f.claim(t, repo, trace.Manager, trace.Manager, trace.Manager, "events_1", intake.ServiceActor)
	_, err = repo.EscalateQuestions(ctx, trace.Manager, chief, trace.EscalationRequest{Questions: []string{"1"}, Rephrasing: "May an upload restart from zero?",
		Blocked: "The upload unit.", Options: []string{"Resume", "Restart"}, Recommendation: "Resume."}, f.clock.Now())
	fixturekit.Must(t, err)
	_, err = repo.Rule(ctx, 1, intakefixture.CharterRuling, intake.OwnerActor, f.clock.Now())
	fixturekit.Must(t, err)
	tools, err := questions.Tools(repo, trace.Manager, chief, f.clock.Now)
	fixturekit.Must(t, err)
	for _, tool := range tools {
		if tool.Name != questions.ProposeCharterTool {
			continue
		}
		out, err := tool.Handle(ctx, json.RawMessage(`{"question":"1","rule":"`+intakefixture.CharterRule+`"}`))
		fixturekit.Must(t, err)
		if string(out) != `{"recorded":true,"question":"1","number":2,"next":"The owner ratifies or declines the proposal; make it the attention of your status until they do."}` {
			t.Fatalf("propose_charter: %s", out)
		}
	}
	fixturekit.Must(t, repo.Close())

	repo, err = trace.Open(f.cfg.Root, f.cfg.Project)
	fixturekit.Must(t, err)
	fixturekit.Must(t, repo.AbandonTurn(ctx, fixturekit.Stream, "agent_mason", "build", f.clock.Now()))
	fixturekit.Must(t, repo.AbandonTurn(ctx, fixturekit.Stream, trace.Manager, "events_1", f.clock.Now()))
	fixturekit.Must(t, repo.Close())
	return f
}

func (f *charterFixture) claim(t *testing.T, repo *trace.Repository, agent, thread, role, turn string, actor trace.Actor) coreadapter.Scope {
	t.Helper()
	ctx := context.Background()
	h := trace.Header{Schema: "osmia.trace.turn-request", Version: trace.Version, ID: "request_" + turn, Revision: 1, Project: fixturekit.Project, Workstream: fixturekit.Stream, At: f.clock.Now(), Actor: actor, Cause: "fixture", Depth: 1}
	_, err := repo.EnqueueTurn(ctx, trace.TurnRequest{Header: h, AgentID: agent, ThreadID: thread, TurnID: turn,
		Profile: coreadapter.Profile{Name: "default", Backend: "claude", Model: "test"}, SystemPrompt: "You are the " + role + ".", Prompt: "Work: " + turn})
	fixturekit.Must(t, err)
	_, err = repo.ClaimTurn(ctx, fixturekit.Stream, agent, "token_"+turn, filepath.Join(t.TempDir(), turn), f.clock.Now())
	fixturekit.Must(t, err)
	return coreadapter.Scope{Project: string(fixturekit.Project), Workstream: string(fixturekit.Stream), Thread: thread, Turn: turn, Role: role}
}

func (f *charterFixture) charter(t *testing.T) string {
	t.Helper()
	content, err := fixturekit.ProjectDocument(t, f.cfg.Root, fixturekit.Project, "charter.md")
	fixturekit.Must(t, err)
	return string(content)
}

func (f *charterFixture) start(t *testing.T) {
	t.Helper()
	s, err := journey.Start(context.Background(), f.opts)
	fixturekit.Must(t, err)
	f.s, f.c = s, journey.NewClient(s.Socket())
	f.settle(t)
}

func (f *charterFixture) settle(t *testing.T) {
	t.Helper()
	for range 3 {
		select {
		case f.ticks <- f.clock.Now():
		case <-time.After(fixturekit.DemoTimeout):
			t.Fatal("service did not finish its pass")
		}
	}
}

func (f *charterFixture) stop(t *testing.T) {
	t.Helper()
	f.c.Close()
	fixturekit.Must(t, f.s.Close())
}

func (f *charterFixture) open(t *testing.T) *trace.Repository {
	t.Helper()
	repo, err := trace.Open(f.cfg.Root, f.cfg.Project)
	fixturekit.Must(t, err)
	return repo
}

func TestRatifiedCharterRuleReachesEveryWorkstreamOnce(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newCharterFixture(t)
	f.start(t)

	want := api.CharterProposalView{Workstream: fixturekit.Stream, Question: "1", State: trace.CharterProposed, Rule: intakefixture.CharterRule, Number: 2, Ruling: intakefixture.CharterRulingPath, RulingRevision: 1, OwnerResponse: intakefixture.CharterRuling, ProposedAt: fixturekit.DemoStart}
	list, err := f.c.CharterProposals(ctx)
	fixturekit.Must(t, err)
	if !reflect.DeepEqual(list, api.CharterProposalsResponse{Proposals: []api.CharterProposalView{want}}) {
		t.Fatalf("proposals %+v", list)
	}
	if st, err := f.c.Status(ctx, fixturekit.Stream); err != nil || !reflect.DeepEqual(st.Gates, []trace.OwnerGate{{Kind: "charter", Reference: "1"}}) {
		t.Fatalf("status gates %+v: %v", st.Gates, err)
	}
	if b := intakefixture.Assemble(t, f.s.Sole().Repository, intakefixture.Quiet); len(b.CharterNotices) != 0 {
		t.Fatalf("a proposed rule is a notice: %+v", b.CharterNotices)
	}
	_, err = f.c.DecideCharter(ctx, fixturekit.Stream, "1", api.CharterDecisionRequest{Decision: "accept"})
	intakefixture.ApiError(t, err, api.Validation, "a charter decision is ratify or decline")
	_, err = f.c.DecideCharter(ctx, fixturekit.Stream, "9", api.CharterDecisionRequest{Decision: trace.CharterRatify})
	intakefixture.ApiError(t, err, api.NotFound, "workstream "+string(fixturekit.Stream)+" has no charter proposal for question 9")
	_, err = f.c.CharterProposal(ctx, intakefixture.Quiet, "1")
	intakefixture.ApiError(t, err, api.NotFound, "workstream "+string(intakefixture.Quiet)+" has no charter proposal for question 1")

	ratified, err := f.c.DecideCharter(ctx, fixturekit.Stream, "1", api.CharterDecisionRequest{Decision: trace.CharterRatify, Note: "It is how we work."})
	fixturekit.Must(t, err)
	if ratified.State != trace.CharterRatified || ratified.Decision != trace.CharterRatify || ratified.Note != "It is how we work." {
		t.Fatalf("ratified %+v", ratified)
	}
	f.settle(t)
	chartered, err := f.c.CharterProposal(ctx, fixturekit.Stream, "1")
	fixturekit.Must(t, err)
	if chartered.State != trace.CharterChartered || chartered.Number != 2 || chartered.Charter == 0 {
		t.Fatalf("chartered %+v", chartered)
	}
	if content := f.charter(t); content != intakefixture.WithRule(intakefixture.CharterBefore, 2) {
		t.Fatalf("charter:\n%s", content)
	}
	intakefixture.CheckChartered(t, f.s.Sole().Repository, intakefixture.WithRule(intakefixture.CharterBefore, 2), 2)
	if st, err := f.c.Status(ctx, fixturekit.Stream); err != nil || len(st.Gates) != 0 {
		t.Fatalf("status gates after the decision %+v: %v", st.Gates, err)
	}
	if list, err := f.c.CharterProposals(ctx); err != nil || len(list.Proposals) != 0 {
		t.Fatalf("proposals after the decision %+v: %v", list, err)
	}
	again, err := f.c.DecideCharter(ctx, fixturekit.Stream, "1", api.CharterDecisionRequest{Decision: trace.CharterRatify})
	fixturekit.Must(t, err)
	if again.State != trace.CharterChartered || again.Detail != "the owner's decision to ratify the charter proposal of question 1 is already recorded" {
		t.Fatalf("ratified again %+v", again)
	}
	_, err = f.c.DecideCharter(ctx, fixturekit.Stream, "1", api.CharterDecisionRequest{Decision: trace.CharterDecline})
	intakefixture.ApiError(t, err, api.Conflict, "the charter proposal of question 1 is already decided: ratify")
	revisions := len(intakefixture.CharterRevisions(t, f.s.Sole().Repository))
	f.stop(t)

	f.start(t)
	f.stop(t)
	repo := f.open(t)
	defer repo.Close()
	if got := len(intakefixture.CharterRevisions(t, repo)); got != revisions {
		t.Fatalf("%d charter revisions after a restart, was %d", got, revisions)
	}
	intakefixture.CheckChartered(t, repo, intakefixture.WithRule(intakefixture.CharterBefore, 2), 2)
}

func TestDeclinedCharterProposalChangesNothing(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newCharterFixture(t)
	f.start(t)
	f.stop(t)
	repo := f.open(t)
	defer func() { _ = repo.Close() }()
	before := intakefixture.CharterRevisions(t, repo)
	_, err := repo.EnqueueTurn(ctx, trace.TurnRequest{Header: trace.Header{Schema: "osmia.trace.turn-request", Version: trace.Version, ID: "request_message_1", Revision: 1, Project: fixturekit.Project, Workstream: fixturekit.Stream, At: fixturekit.DemoStart, Actor: intake.OwnerActor, Cause: "fixture"},
		AgentID: trace.Manager, ThreadID: trace.Manager, TurnID: "message_1", Profile: coreadapter.Profile{Name: "default", Backend: "claude", Model: "test"}, SystemPrompt: "chief", Prompt: "Decline the charter rule."})
	fixturekit.Must(t, err)
	_, err = repo.ClaimTurn(ctx, fixturekit.Stream, trace.Manager, "token-message", filepath.Join(t.TempDir(), "message"), fixturekit.DemoStart)
	fixturekit.Must(t, err)
	controls := &journey.RuntimeControls{}
	controls.Service.Store(f.s)
	scope := coreadapter.Scope{Project: string(fixturekit.Project), Workstream: string(fixturekit.Stream), Role: trace.Manager, Thread: trace.Manager, Turn: "message_1"}
	out, err := controls.DecideCharter(repo, scope).Handle(ctx, json.RawMessage(`{"question":"1","decision":"decline","note":"It is this feature's call."}`))
	fixturekit.Must(t, err)
	if want := `{"recorded":true,"question":"1","state":"declined","detail":"the owner declined the charter proposal of question 1; the charter is unchanged: It is this feature's call."}`; string(out) != want {
		t.Fatalf("decide_charter: %s", out)
	}
	proposals, err := repo.CharterProposals()
	fixturekit.Must(t, err)
	if len(proposals) != 1 || proposals[0].Latest.Actor != intake.OwnerActor || proposals[0].Latest.Cause != "request_message_1" || proposals[0].Proposal.Decision != trace.CharterDecline {
		t.Fatalf("declined proposal %+v", proposals)
	}
	fixturekit.Must(t, repo.Close())

	f.start(t)
	_, err = f.c.DecideCharter(ctx, fixturekit.Stream, "1", api.CharterDecisionRequest{Decision: trace.CharterRatify})
	intakefixture.ApiError(t, err, api.Conflict, "the charter proposal of question 1 is already decided: decline")
	if st, err := f.c.Status(ctx, fixturekit.Stream); err != nil || len(st.Gates) != 0 {
		t.Fatalf("status gates %+v: %v", st.Gates, err)
	}
	f.stop(t)
	repo = f.open(t)
	if got := intakefixture.CharterRevisions(t, repo); !reflect.DeepEqual(got, before) {
		t.Fatalf("charter revisions %+v, were %+v", got, before)
	}
	if content := f.charter(t); content != intakefixture.CharterBefore {
		t.Fatalf("charter:\n%s", content)
	}
	for _, ws := range []config.WorkstreamID{fixturekit.Stream, intakefixture.Quiet} {
		if b := intakefixture.Assemble(t, repo, ws); len(b.CharterNotices) != 0 || len(b.CharterEdits) != 1 || len(b.Notices) != 0 {
			t.Fatalf("bundle of %s:\n%s", ws, b.Render())
		}
	}
	if events := intakefixture.CharterEvents(t, repo, fixturekit.Stream); !reflect.DeepEqual(events, []string{"the owner declined the charter proposal of question 1; the charter is unchanged: It is this feature's call."}) {
		t.Fatalf("chief of staff notices %q", events)
	}
}

func TestCharterWriteKeepsAConcurrentOwnerEdit(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newCharterFixture(t)
	f.start(t)
	f.stop(t)
	repo := f.open(t)
	defer repo.Close()
	if _, apiErr := f.s.RecordCharterDecision(ctx, fixturekit.Project, fixturekit.Stream, repo, "1", api.CharterDecisionRequest{Decision: trace.CharterRatify}, intake.OwnerActor, "owner-charter"); apiErr != nil {
		t.Fatal(apiErr)
	}
	edited := intakefixture.CharterBefore + "2. Owner's own rule.\n"
	edits := 0
	f.s.Boundary = func(name string) error {
		if name == "charter-write" && edits == 0 {
			edits++
			current, err := repo.Charter(ctx)
			if err != nil {
				return err
			}
			_, err = repo.EditCharter(ctx, current.Revision, edited, f.clock.Now())
			return err
		}
		return nil
	}
	fixturekit.Must(t, intake.Charterer{S: f.s.Intake().ChartererDependencies(), Repository: repo}.Pass(ctx))
	if edits != 1 {
		t.Fatalf("%d edits", edits)
	}
	revisions := intakefixture.CharterRevisions(t, repo)
	owner := revisions[len(revisions)-2]
	if owner.Content != edited || owner.Actor != intake.OwnerActor || owner.Cause != "owner-edit" {
		t.Fatalf("the owner's edit is not recorded before the rule: %+v", owner)
	}
	if latest := revisions[len(revisions)-1]; latest.Content != intakefixture.WithRule(edited, 3) {
		t.Fatalf("charter:\n%s", latest.Content)
	}
	intakefixture.CheckChartered(t, repo, intakefixture.WithRule(edited, 3), 3)
}

func TestCharterRuleIsWrittenOnceAcrossRestarts(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newCharterFixture(t)
	f.start(t)
	f.stop(t)
	repo := f.open(t)
	ratified, apiErr := f.s.RecordCharterDecision(ctx, fixturekit.Project, fixturekit.Stream, repo, "1", api.CharterDecisionRequest{Decision: trace.CharterRatify}, intake.OwnerActor, "owner-charter")
	if apiErr != nil || ratified.State != trace.CharterRatified {
		t.Fatalf("ratified %+v: %v", ratified, apiErr)
	}
	rules := intake.Charterer{S: f.s.Intake().ChartererDependencies(), Repository: repo}
	crash := errors.New("crash")
	for _, step := range []string{"charter-write", "charter-written"} {
		f.s.Boundary = func(name string) error {
			if name == step {
				return crash
			}
			return nil
		}
		if err := rules.Pass(ctx); !errors.Is(err, crash) {
			t.Fatalf("pass stopping at %s: %v", step, err)
		}
		want := 0
		if step == "charter-written" {
			want = 1
		}
		if got := intakefixture.Appended(t, repo); len(got) != want {
			t.Fatalf("stopped at %s: %d charter revisions append the rule", step, len(got))
		}
		if p, err := repo.CharterProposals(); err != nil || p[0].State.Value != trace.CharterRatified {
			t.Fatalf("stopped at %s: %+v %v", step, p, err)
		}
	}
	f.s.Boundary = nil
	fixturekit.Must(t, repo.Close())

	f.start(t)
	f.stop(t)
	repo = f.open(t)
	defer repo.Close()
	intakefixture.CheckChartered(t, repo, intakefixture.WithRule(intakefixture.CharterBefore, 2), 2)
}
