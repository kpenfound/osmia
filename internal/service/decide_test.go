package service

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/intake"
	"github.com/kpenfound/osmia/internal/jev"
	"github.com/kpenfound/osmia/internal/jev/jevfixture"
	"github.com/kpenfound/osmia/internal/questions"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	"github.com/kpenfound/osmia/internal/servicetest/intakefixture"
	"github.com/kpenfound/osmia/internal/trace"
)

// The decide tool is held by an ordinary manager turn and by no other
// role's turn, a design session's included.
func TestDecideIsAManagerToolOnly(t *testing.T) {
	t.Parallel()
	if !slices.Contains(managerGrant.Tools, decideTool) || !slices.Contains(managerOrdinaryNarrow.Tools, decideTool) {
		t.Fatalf("an ordinary manager turn does not hold decide: %v", managerOrdinaryNarrow.Tools)
	}
	for name, tools := range map[string][]string{"design session": designNarrow.Tools, "mason": masonGrant.Tools, "unit mason": unitMasonGrant.Tools, "reviewer": reviewerGrant.Tools} {
		if slices.Contains(tools, decideTool) {
			t.Errorf("a %s turn holds decide", name)
		}
	}
}

type decideKit struct {
	f       *gateFixture
	p       *gateJev
	manager coreadapter.Scope
	decide  coreadapter.Tool
	tools   map[string]coreadapter.Tool
}

func newDecideKit(t *testing.T, p *gateJev, role string) *decideKit {
	t.Helper()
	f := newGateFixture(t, p)
	fixturekit.RecordCharter(t, f.repo, "1. Every change has a test.\n")
	manager := claimThread(t, f.repo, f.now(), "chief", trace.Manager, "chief1", "")
	asker := claimThread(t, f.repo, f.now(), "asker", role, "build_asker", "unit1")
	_, err := f.repo.Ask(context.Background(), "asker", asker, "Should uploads resume after a restart?", f.now())
	fixturekit.Must(t, err)
	k := &decideKit{f: f, p: p, manager: manager}
	k.turn(t, "chief", manager)
	return k
}

func (k *decideKit) turn(t *testing.T, agent string, scope coreadapter.Scope) {
	t.Helper()
	controls := &runtimeControls{}
	controls.service.Store(k.f.s)
	k.decide = controls.decide(k.f.repo, scope)
	list, err := questions.Tools(k.f.repo, agent, scope, k.f.now)
	fixturekit.Must(t, err)
	k.tools = map[string]coreadapter.Tool{}
	for _, tool := range list {
		k.tools[tool.Name] = tool
	}
}

func (k *decideKit) call(t *testing.T, tool coreadapter.Tool, input string) map[string]any {
	t.Helper()
	out := map[string]any{}
	fixturekit.Must(t, json.Unmarshal([]byte(intakefixture.Handle(t, tool, input)), &out))
	return out
}

func (k *decideKit) judgments(t *testing.T) int {
	t.Helper()
	records, _ := judgments(t, k.f.repo, fixturekit.Stream)
	return len(records)
}

const (
	decideQuestion = `{"kind":"question-routing","subject":"1","evidence":"The spec leaves restart behaviour to the plan."}`
	answerUploads  = `{"question":"1","text":"Resume from the last chunk.","citations":["charter#1"]}`
	escalateUpload = `{"questions":["1"],"rephrasing":"Should uploads resume?","blocked":"The upload unit.","options":["Resume","Restart"],"recommendation":"Resume."}`
)

func TestAConfidentRoutingPickBindsTheManagersOutcomeToolsInLaterTurns(t *testing.T) {
	t.Parallel()
	p := &gateJev{confidence: 0.95, pick: func(_ string, options []string) string { return "answer" }}
	k := newDecideKit(t, p, masonRole)
	if got := k.call(t, k.tools["answer"], answerUploads); got["recorded"] != false || !strings.Contains(got["reason"].(string), "decide") {
		t.Fatalf("answer before decide: %v", got)
	}
	got := k.call(t, k.decide, decideQuestion)
	if got["decision"] != "binding" || got["pick"] != "answer" {
		t.Fatalf("decide: %v", got)
	}
	if want := []string{"answer", "route_amendment", "escalate"}; !slices.Equal(optionNames(p), want) {
		t.Fatalf("options %v", optionNames(p))
	}
	if got := k.call(t, k.tools["escalate"], escalateUpload); got["recorded"] != false || !strings.Contains(got["reason"].(string), "Jev decided answer") {
		t.Fatalf("escalate against the pick: %v", got)
	}
	later := claimThread(t, k.f.repo, k.f.now(), "chief2", trace.Manager, "chief2", "")
	k.turn(t, "chief2", later)
	if again := k.call(t, k.decide, decideQuestion); again["decision"] != "binding" || again["pick"] != "answer" {
		t.Fatalf("decide again: %v", again)
	}
	if got := k.call(t, k.tools["escalate"], escalateUpload); got["recorded"] != false {
		t.Fatalf("escalate in a later turn: %v", got)
	}
	if got := k.call(t, k.tools["answer"], answerUploads); got["recorded"] != true {
		t.Fatalf("answer: %v", got)
	}
	if len(p.Requests()) != 1 {
		t.Fatalf("%d requests", len(p.Requests()))
	}
	states, err := k.f.repo.Questions(fixturekit.Stream)
	fixturekit.Must(t, err)
	judgment, _, _ := decisionJudgment(t, k.f.repo, fixturekit.Stream, jev.KindQuestionRouting, jev.QuestionKey("1"))
	if r := states[0].Ruling; states[0].State != trace.QuestionAnswered || r == nil || r.Actor != trace.JevActor || r.DecidedBy != trace.DecidedByJev || r.Judgment != judgment || r.ReturnedAnswer != "Resume from the last chunk." {
		t.Fatalf("question %+v", states[0])
	}
}

// A pick below the threshold, and an unconfigured Jev, leave the decision to
// the manager: decide says so and any valid outcome is accepted.
func TestADecisionThatIsNotConfidentLeavesTheManagerFree(t *testing.T) {
	t.Parallel()
	for name, p := range map[string]*gateJev{"below the threshold": {confidence: 0.6}, "unconfigured": nil} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			k := newDecideKit(t, p, masonRole)
			got := k.call(t, k.decide, decideQuestion)
			if got["decision"] != "manager" || got["pick"] != nil {
				t.Fatalf("decide: %v", got)
			}
			if got := k.call(t, k.tools["escalate"], escalateUpload); got["recorded"] != true {
				t.Fatalf("escalate: %v", got)
			}
		})
	}
}

// A question the architect asked is the owner's route, not Jev's: decide sends
// no request and records no judgment, and the manager's outcome tool needs no
// decide result for it.
func TestAQuestionJevDoesNotRouteIsRefusedWithoutARequest(t *testing.T) {
	t.Parallel()
	p := &gateJev{confidence: 0.95}
	k := newDecideKit(t, p, "architect")
	got := k.call(t, k.decide, decideQuestion)
	if got["recorded"] != false || !strings.Contains(got["reason"].(string), "architect") {
		t.Fatalf("decide: %v", got)
	}
	if len(p.Requests()) != 0 || k.judgments(t) != 0 {
		t.Fatalf("%d requests, %d judgments", len(p.Requests()), k.judgments(t))
	}
	if got := k.call(t, k.tools["escalate"], escalateUpload); got["recorded"] != true {
		t.Fatalf("escalate: %v", got)
	}
}

func TestDecideRefusalsAreOrdinaryResults(t *testing.T) {
	t.Parallel()
	p := &gateJev{confidence: 0.95}
	k := newDecideKit(t, p, masonRole)
	for _, input := range []string{
		`{"kind":"question-routing","subject":"1","evidence":" "}`,
		`{"kind":"question-routing","subject":"9","evidence":"x"}`,
		`{"kind":"question-routing","subject":"1","evidence":"x","options":[{"name":"a","description":"b"}]}`,
		`{"kind":"contested-ruling","subject":"resume","evidence":"x"}`,
		`{"kind":"objection","subject":"nobody","evidence":"x"}`,
	} {
		if got := k.call(t, k.decide, input); got["recorded"] != false || got["reason"] == "" {
			t.Fatalf("%s: %v", input, got)
		}
	}
	if len(p.Requests()) != 0 || k.judgments(t) != 0 {
		t.Fatalf("%d requests, %d judgments", len(p.Requests()), k.judgments(t))
	}
}

func TestAContestRulingBindsAndAnUnexecutablePickGoesToTheOwner(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	p := &gateJev{confidence: 0.95, pick: func(_ string, options []string) string { return "revise" }}
	c := newContestFixture(t, "decide-contest")
	c.f.s.jev = gateJudge(c.f.clock.Now, p)
	contest := c.contest(t)
	decide := c.controls.decide(c.repo, c.scope)
	note := `{"unit":"resume","decision":"review","note":"Review it again."}`
	if out := intakefixture.Handle(t, c.tool, note); !strings.Contains(out, `"recorded":false`) || !strings.Contains(out, "call decide") {
		t.Fatalf("resolve before decide: %s", out)
	}
	if out := intakefixture.Handle(t, decide, `{"kind":"contested-ruling","subject":"resume","evidence":"The reviewer ended without a verdict."}`); !strings.Contains(out, `"decision":"binding"`) || !strings.Contains(out, `"pick":"revise"`) {
		t.Fatalf("decide: %s", out)
	}
	if out := intakefixture.Handle(t, c.tool, note); !strings.Contains(out, `"recorded":false`) || !strings.Contains(out, "Jev decided revise") {
		t.Fatalf("resolve against the pick: %s", out)
	}
	out := intakefixture.Handle(t, c.tool, `{"unit":"resume","decision":"revise","note":"Fix it."}`)
	if !strings.Contains(out, `"recorded":true`) || !strings.Contains(out, "owner's inbox") {
		t.Fatalf("resolve with the pick: %s", out)
	}
	if got := intakefixture.UnitWorkflow(t, c.repo, c.stream, "resume").Value; got != UnitContested {
		t.Fatalf("the unit is %q", got)
	}
	gate, carried, err := c.f.s.contestedGate(c.repo, c.stream, "resume")
	if err != nil || gate != nil || carried == nil || carried.Pick != "revise" {
		t.Fatalf("gate %+v carried %+v: %v", gate, carried, err)
	}
	card, open, err := c.f.s.intake().ContestedCard(c.repo, c.stream, "resume")
	if err != nil || !open || card.Jev == nil || !card.Jev.Binding || card.Jev.Pick != "revise" {
		t.Fatalf("card %+v open %v: %v", card, open, err)
	}
	if gates, err := c.f.s.intake().ReachedGates(ctx, &intake.Project{ID: c.repo.Project(), Repository: c.repo}); err != nil || len(gates) != 0 {
		t.Fatalf("gates %+v: %v", gates, err)
	}
	if len(p.Requests()) != 1 {
		t.Fatalf("%d requests", len(p.Requests()))
	}
	if _, err := jev.Recorded(c.repo, c.stream, jev.KindEscalatedContest, jev.ContestKey(contest.ID)); err != nil {
		t.Fatal(err)
	}
	if res, _ := jev.Recorded(c.repo, c.stream, jev.KindEscalatedContest, jev.ContestKey(contest.ID)); res.Status != jev.None {
		t.Fatalf("the owner gate judged the contest: %+v", res)
	}
}

func TestAContestRulingByJevIsRecordedAsJevsWithTheManagersNote(t *testing.T) {
	t.Parallel()
	p := &gateJev{confidence: 0.95, pick: func(_ string, options []string) string { return "review" }}
	c := newContestFixture(t, "decide-contest-review")
	c.f.s.jev = gateJudge(c.f.clock.Now, p)
	contest := c.contest(t)
	decide := c.controls.decide(c.repo, c.scope)
	if out := intakefixture.Handle(t, decide, `{"kind":"contested-ruling","subject":"resume","evidence":"The reviewer ended without a verdict."}`); !strings.Contains(out, `"pick":"review"`) {
		t.Fatalf("decide: %s", out)
	}
	if out := intakefixture.Handle(t, c.tool, `{"unit":"resume","decision":"escalate","note":"Owner."}`); !strings.Contains(out, `"recorded":false`) || !strings.Contains(out, "Jev decided review") {
		t.Fatalf("escalate against the pick: %s", out)
	}
	later := &gateJev{confidence: 0.99, pick: func(_ string, options []string) string { return "escalate" }}
	c.f.s.jev = gateJudge(c.f.clock.Now, later)
	if out := intakefixture.Handle(t, decide, `{"kind":"contested-ruling","subject":"resume","evidence":"Changed evidence."}`); !strings.Contains(out, `"pick":"review"`) {
		t.Fatalf("decide after the restart: %s", out)
	}
	if out := intakefixture.Handle(t, c.tool, `{"unit":"resume","decision":"escalate","note":"Owner."}`); !strings.Contains(out, "Jev decided review") {
		t.Fatalf("escalate after the restart: %s", out)
	}
	if len(later.Requests()) != 0 {
		t.Fatalf("%d requests after the restart", len(later.Requests()))
	}
	if out := intakefixture.Handle(t, c.tool, `{"unit":"resume","decision":"review","note":"Check the verdict file."}`); !strings.Contains(out, `"recorded":true`) {
		t.Fatalf("review: %s", out)
	}
	if got := intakefixture.UnitWorkflow(t, c.repo, c.stream, "resume").Value; got != UnitReviewing {
		t.Fatalf("the unit is %q", got)
	}
	judgment, _, _ := decisionJudgment(t, c.repo, c.stream, jev.KindContestedRuling, jev.ContestKey(contest.ID))
	transitions, err := trace.Read[trace.Transition](c.repo, c.stream)
	fixturekit.Must(t, err)
	found := false
	for _, tr := range transitions {
		if tr.Subject == trace.UnitSubject("resume") && tr.From == UnitContested && tr.To == UnitReviewing && tr.Actor == trace.JevActor && strings.Contains(tr.Reason, "Check the verdict file.") {
			found = true
		}
	}
	if !found || judgment == "" {
		t.Fatalf("no ruling by Jev (judgment %q) in %+v", judgment, transitions)
	}
	if len(p.Requests()) != 1 {
		t.Fatalf("%d requests", len(p.Requests()))
	}
}

// A contest ruling that is not confident leaves the manager free: with a
// pick below the threshold, or Jev unconfigured, decide says the ruling is the
// manager's and resolve_contested takes any ruling the contest takes.
func TestAContestRulingThatIsNotConfidentLeavesTheManagerFree(t *testing.T) {
	t.Parallel()
	for name, p := range map[string]*gateJev{"below the threshold": {confidence: 0.5, pick: func(_ string, options []string) string { return "escalate" }}, "unconfigured": nil} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			c := newContestFixture(t, "decide-free-"+strings.ReplaceAll(name, " ", "-"))
			c.f.s.jev = gateJudge(c.f.clock.Now, p)
			c.contest(t)
			decide := c.controls.decide(c.repo, c.scope)
			if out := intakefixture.Handle(t, decide, `{"kind":"contested-ruling","subject":"resume","evidence":"The reviewer ended without a verdict."}`); !strings.Contains(out, `"decision":"manager"`) || strings.Contains(out, `"pick"`) {
				t.Fatalf("decide: %s", out)
			}
			if out := intakefixture.Handle(t, c.tool, `{"unit":"resume","decision":"review","note":"Check the verdict file."}`); !strings.Contains(out, `"recorded":true`) {
				t.Fatalf("review: %s", out)
			}
			if got := intakefixture.UnitWorkflow(t, c.repo, c.stream, "resume").Value; got != UnitReviewing {
				t.Fatalf("the unit is %q", got)
			}
		})
	}
}

// A decide result of another contest, even a confident one, does not let
// resolve_contested record a ruling for this contest, and records nothing.
func TestAnotherContestsDecideResultDoesNotPermitARuling(t *testing.T) {
	t.Parallel()
	c := newContestFixture(t, "decide-other-contest")
	p := &gateJev{confidence: 0.95, pick: func(_ string, options []string) string { return "review" }}
	judge := gateJudge(c.f.clock.Now, p)
	contest := c.contest(t)
	other := jev.ContestKey(contest.ID + "-other")
	if res := jevfixture.Decide(judge, c.repo, c.repo.Project(), c.stream, jev.KindContestedRuling, other, "review", "revise", "escalate"); res.Status != jev.Binding {
		t.Fatalf("result %+v", res)
	}
	out := intakefixture.Handle(t, c.tool, `{"unit":"resume","decision":"review","note":"Review it again."}`)
	if !strings.Contains(out, `"recorded":false`) || !strings.Contains(out, "call decide") {
		t.Fatalf("resolve on another contest's result: %s", out)
	}
	if got := intakefixture.UnitWorkflow(t, c.repo, c.stream, "resume").Value; got != UnitContested {
		t.Fatalf("the unit is %q", got)
	}
}
