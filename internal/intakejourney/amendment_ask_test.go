package intakejourney

import (
	context "context"
	json "encoding/json"
	"github.com/kpenfound/osmia/internal/intake"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	"github.com/kpenfound/osmia/internal/servicetest/journey"
	filepath "path/filepath"
	slices "slices"
	strings "strings"
	testing "testing"

	agent "github.com/kpenfound/agentcore/agent"
	config "github.com/kpenfound/osmia/internal/config"
	coreadapter "github.com/kpenfound/osmia/internal/coreadapter"
	plan "github.com/kpenfound/osmia/internal/plan"
	questions "github.com/kpenfound/osmia/internal/questions"
	intakefixture "github.com/kpenfound/osmia/internal/servicetest/intakefixture"
	trace "github.com/kpenfound/osmia/internal/trace"
	mcp "github.com/modelcontextprotocol/go-sdk/mcp"
)

const parkedAmendment = "amendment 1 is parked: the architect waits for the answer to question 1"

type amendmentAskFixture struct {
	*journey.ShedFixture
	repo   *trace.Repository
	stream config.WorkstreamID
}

func newAmendmentAskFixture(t *testing.T, script func(f *journey.ShedFixture)) *amendmentAskFixture {
	t.Helper()
	f := newSketchedDebateFixture(t, 1, 1)
	f.Script("draft-1-1", map[string]string{plan.SpecPath: fixturekit.ValidSpec, plan.PlanPath: fixturekit.ValidPlan}, nil)
	script(f)
	stream := f.HandIn(t, "amendment-ask", fixturekit.HandedDesign)
	f.StopAtSketch(t, stream)
	a := &amendmentAskFixture{ShedFixture: f, stream: stream}
	a.reopen(t)
	t.Cleanup(func() { a.repo.Close() })
	seedAmendment(t, f.ArchitectFixture, a.repo, stream)
	return a
}

func (a *amendmentAskFixture) reopen(t *testing.T) {
	t.Helper()
	if a.repo != nil {
		fixturekit.Must(t, a.repo.Close())
	}
	repo, err := trace.Open(a.S.Current().Root, a.S.Current().Project)
	fixturekit.Must(t, err)
	a.repo = repo
}

func (a *amendmentAskFixture) drafter() *intake.AmendmentDrafter {
	return &intake.AmendmentDrafter{Drafter: &intake.Drafter{S: a.S.Intake().DrafterDependencies(), Repository: a.repo}}
}

func (a *amendmentAskFixture) drafts(t *testing.T) []trace.OperationRecord {
	t.Helper()
	ops, err := a.repo.Operations(a.stream)
	fixturekit.Must(t, err)
	return slices.DeleteFunc(ops, func(o trace.OperationRecord) bool { return o.Operation.Action != intake.AmendmentDraftAction })
}

func (a *amendmentAskFixture) draft(t *testing.T, transition string) trace.OperationRecord {
	t.Helper()
	ops := a.drafts(t)
	id := trace.OperationID(a.Project, a.stream, trace.EventID(transition, "run"))
	i := slices.IndexFunc(ops, func(o trace.OperationRecord) bool { return o.Operation.ID == id })
	if i < 0 {
		t.Fatalf("no draft operation of %s: %v", transition, intakefixture.Results(ops))
	}
	return ops[i]
}

func (a *amendmentAskFixture) apply(t *testing.T, op trace.OperationRecord) coreadapter.OperationResult {
	t.Helper()
	result, err := a.drafter().Apply(context.Background(), op.Operation)
	fixturekit.Must(t, err)
	return result
}

func (a *amendmentAskFixture) moves(t *testing.T, id string) []string {
	t.Helper()
	transitions, err := trace.Read[trace.Transition](a.repo, a.stream)
	fixturekit.Must(t, err)
	var out []string
	for _, tr := range transitions {
		if tr.Subject == intake.AmendmentSubject(id) {
			out = append(out, tr.ID+" "+tr.From+" -> "+tr.To)
		}
	}
	return out
}

func (a *amendmentAskFixture) transition(t *testing.T, id string) trace.Transition {
	t.Helper()
	transitions, err := trace.Read[trace.Transition](a.repo, a.stream)
	fixturekit.Must(t, err)
	for _, tr := range transitions {
		if tr.ID == id {
			return tr
		}
	}
	t.Fatalf("transition %s is missing", id)
	return trace.Transition{}
}

func (a *amendmentAskFixture) candidate(t *testing.T, id string) map[string]trace.Document {
	t.Helper()
	docs, err := trace.Read[trace.Document](a.repo, a.stream)
	fixturekit.Must(t, err)
	out := map[string]trace.Document{}
	for _, doc := range docs {
		if path, ok := strings.CutPrefix(doc.Path, "amendments/"+id+"/"); ok {
			out[path] = doc
		}
	}
	return out
}

func (a *amendmentAskFixture) thread(t *testing.T) trace.Thread {
	t.Helper()
	th, err := a.repo.Thread(a.stream, intake.ArchitectAgent)
	fixturekit.Must(t, err)
	return th
}

func (a *amendmentAskFixture) parkFirst(t *testing.T, p *intakefixture.Faults) trace.OperationRecord {
	t.Helper()
	fixturekit.Must(t, a.drafter().Pass(context.Background()))
	ops := a.drafts(t)
	if len(ops) != 1 {
		t.Fatalf("draft operations: %v", intakefixture.Results(ops))
	}
	if result := a.apply(t, ops[0]); result.Outcome != questions.Waiting || result.Evidence != parkedAmendment {
		t.Fatalf("the asking draft ended %+v", result)
	}
	p.Check(t)
	return ops[0]
}

func TestArchitectQuestionParksTheAmendmentUntilItsAnswer(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	p := &intakefixture.Faults{}
	var answering string
	f := newAmendmentAskFixture(t, func(f *journey.ShedFixture) {
		f.Script("amend-1-1", nil, intakefixture.Parks(p))
		answering = f.Answer("1", func(ctx context.Context, req agent.Request, verified *agent.Turn, tools *mcp.ClientSession) error {
			if got, err := intakefixture.ReadTool(ctx, tools, "request.json"); err != nil || !strings.Contains(got, "Revise the criterion") {
				p.Report("the answer turn's view lacks the request: %q %v", got, err)
			}
			if !strings.Contains(req.Prompt, "Answer to your question 1.") || !strings.Contains(req.Prompt, intakefixture.AskedAnswer) {
				p.Report("answer prompt:\n%s", req.Prompt)
			}
			return intakefixture.Delivers(p, plan.PlanPath, intakefixture.RestartPlan, intake.DraftValid)(ctx, req, verified, tools)
		})
	})
	first := f.parkFirst(t, p)

	park := f.transition(t, "amendment-1-waiting-1")
	if park.From != "drafting" || park.To != intake.AmendmentWaiting || park.Cause != first.Operation.ID || park.Reason != parkedAmendment || park.Actor != intake.DraftingActor {
		t.Fatalf("park: %+v", park)
	}
	if th := f.thread(t); !th.Parked() || th.Active != "" || th.Turns[len(th.Turns)-1].Request.TurnID != "amend-1-1" {
		t.Fatalf("the architect's thread holds a turn while the amendment waits: %+v", th)
	}
	if docs := f.candidate(t, "1"); len(docs) != 0 {
		t.Fatalf("a parked amendment recorded %+v", docs)
	}
	observed, err := f.drafter().Inspect(ctx, first.Operation)
	fixturekit.Must(t, err)
	if observed.State != coreadapter.EffectCompleted || observed.Result == nil || observed.Result.Outcome != questions.Waiting {
		t.Fatalf("parked draft observed as %+v", observed)
	}

	f.reopen(t)
	fixturekit.Must(t, f.drafter().Pass(ctx))
	if ops := f.drafts(t); len(ops) != 1 {
		t.Fatalf("a parked amendment was drafted again before its answer: %v", intakefixture.Results(ops))
	}
	if result := f.apply(t, first); result.Outcome != questions.Waiting {
		t.Fatalf("the parked operation reapplied as %+v", result)
	}
	if state, err := f.repo.Workflow(f.stream, intake.AmendmentSubject("1")); err != nil || state.Value != intake.AmendmentWaiting {
		t.Fatalf("amendment after the restart: %+v %v", state, err)
	}

	journey.ChiefAnswers(t, f.ShedFixture, f.repo, f.stream, "1", intakefixture.AskedAnswer, "charter#1")
	f.reopen(t)
	fixturekit.Must(t, f.drafter().Pass(ctx))
	fixturekit.Must(t, f.drafter().Pass(ctx))
	if ops := f.drafts(t); len(ops) != 2 {
		t.Fatalf("draft operations: %v", intakefixture.Results(ops))
	}
	resumption := f.draft(t, "amendment-1-draft-resume-1")
	var in intake.AmendmentDraftInput
	if err := json.Unmarshal(resumption.Operation.Input, &in); err != nil || in != (intake.AmendmentDraftInput{ID: "1", Resume: 1}) {
		t.Fatalf("resumption input %+v %v", in, err)
	}
	if resumed := f.transition(t, "amendment-1-draft-resume-1"); resumed.Cause != park.ID || resumed.Reason != "amendment 1 resumes: the architect's question is answered" {
		t.Fatalf("resumption: %+v", resumed)
	}
	f.reopen(t)
	if result := f.apply(t, resumption); result.Outcome != "succeeded" {
		t.Fatalf("the resumed draft ended %+v", result)
	}
	p.Check(t)

	want := []string{"amendment_1_filed  -> filed", "amendment-1-draft filed -> drafting", "amendment-1-waiting-1 drafting -> waiting", "amendment-1-draft-resume-1 waiting -> drafting", "amendment-1-proposed drafting -> proposed"}
	if moves := f.moves(t, "1"); !slices.Equal(moves, want) {
		t.Fatalf("amendment went %v, want %v", moves, want)
	}
	docs := f.candidate(t, "1")
	if docs[plan.SpecPath].Content != intakefixture.CheckpointSpec || docs[plan.PlanPath].Content != intakefixture.RestartPlan || docs[plan.SpecPath].Cause != resumption.Operation.ID {
		t.Fatalf("proposal lost the drafted documents: %+v", docs)
	}
	if ran := f.Ran(); ran["amend-1-1"] != 1 || ran[answering] != 1 || ran["amend-1-2"] != 0 {
		t.Fatalf("turns ran: %v", ran)
	}
	if specs := intakefixture.ReadDocs(t, f.repo, f.stream, plan.SpecDocument); len(specs) != 1 || specs[0].Content != fixturekit.ValidSpec {
		t.Fatalf("sealed spec changed: %+v", specs)
	}
}

func TestAmendmentAnswerInterruptedByAStopIsRepeated(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	p := &intakefixture.Faults{}
	var answering string
	f := newAmendmentAskFixture(t, func(f *journey.ShedFixture) {
		f.Script("amend-1-1", nil, intakefixture.Parks(p))
		answering = f.Answer("1", func(context.Context, agent.Request, *agent.Turn, *mcp.ClientSession) error {
			p.Report("the interrupted answer turn ran again")
			return nil
		})
		f.Script("amend-1-2", map[string]string{plan.SpecPath: intakefixture.CheckpointSpec}, func(_ context.Context, req agent.Request, _ *agent.Turn, _ *mcp.ClientSession) error {
			for _, want := range []string{"The answers to the questions you asked in this amendment:\n\nAnswer to your question 1.", intakefixture.AskedAnswer} {
				if !strings.Contains(req.Prompt, want) {
					p.Report("the next attempt's prompt lacks %q:\n%s", want, req.Prompt)
				}
			}
			return nil
		})
	})
	f.parkFirst(t, p)
	journey.ChiefAnswers(t, f.ShedFixture, f.repo, f.stream, "1", intakefixture.AskedAnswer, "charter#1")
	fixturekit.Must(t, f.drafter().Pass(ctx))

	_, err := f.repo.ClaimTurn(ctx, f.stream, intake.ArchitectAgent, "claim-"+answering, filepath.Join(f.S.Current().Root.String(), "interrupted"), f.Clock.Now())
	fixturekit.Must(t, err)
	f.reopen(t)

	if result := f.apply(t, f.draft(t, "amendment-1-draft-resume-1")); result.Outcome != "succeeded" {
		t.Fatalf("the resumed draft ended %+v", result)
	}
	p.Check(t)
	var statuses []string
	for _, q := range f.thread(t).Turns {
		statuses = append(statuses, q.Request.TurnID+" "+q.Status())
	}
	if want := []string{"draft-1-1 idle", "amend-1-1 waiting", answering + " interrupted", "amend-1-2 idle"}; !slices.Equal(statuses, want) {
		t.Fatalf("architect's turns %v, want %v", statuses, want)
	}
	if state, err := f.repo.Workflow(f.stream, intake.AmendmentSubject("1")); err != nil || state.Value != "proposed" {
		t.Fatalf("amendment %+v %v", state, err)
	}
}

func TestParkedAmendmentLeavesNoAnswerBlockingTheNext(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	p := &intakefixture.Faults{}
	var answering string
	f := newAmendmentAskFixture(t, func(f *journey.ShedFixture) {
		f.Script("amend-1-1", nil, intakefixture.Parks(p))
		answering = f.Answer("1", intakefixture.Delivers(p, plan.PlanPath, intakefixture.RestartPlan, intake.DraftValid))
		f.Script("amend-2-1", map[string]string{plan.PlanPath: intakefixture.RestartPlan}, nil)
		f.Script("amend-3-1", map[string]string{plan.SpecPath: intakefixture.CheckpointSpec}, nil)
	})
	f.parkFirst(t, p)

	fileAmendment(t, f.ArchitectFixture, f.repo, f.stream, "2")
	fixturekit.Must(t, f.drafter().Pass(ctx))
	if result := f.apply(t, f.draft(t, "amendment-2-draft")); result.Outcome != "succeeded" {
		t.Fatalf("amendment 2 ended %+v", result)
	}

	journey.ChiefAnswers(t, f.ShedFixture, f.repo, f.stream, "1", intakefixture.AskedAnswer, "charter#1")
	fileAmendment(t, f.ArchitectFixture, f.repo, f.stream, "3")
	fixturekit.Must(t, f.drafter().Pass(ctx))
	if ops := f.drafts(t); len(ops) != 4 {
		t.Fatalf("draft operations: %v", intakefixture.Results(ops))
	}
	for _, transition := range []string{"amendment-1-draft-resume-1", "amendment-3-draft"} {
		if result := f.apply(t, f.draft(t, transition)); result.Outcome != "succeeded" {
			t.Fatalf("%s ended %+v", transition, result)
		}
	}
	p.Check(t)
	for _, id := range []string{"1", "2", "3"} {
		if state, err := f.repo.Workflow(f.stream, intake.AmendmentSubject(id)); err != nil || state.Value != "proposed" {
			t.Fatalf("amendment %s is %+v %v", id, state, err)
		}
	}
	var turns []string
	for _, q := range f.thread(t).Turns {
		if q.CompletedAt.IsZero() {
			t.Fatalf("turn %s was left queued: %+v", q.Request.TurnID, q)
		}
		turns = append(turns, q.Request.TurnID)
	}
	if want := []string{"draft-1-1", "amend-1-1", "amend-2-1", answering, "amend-3-1"}; !slices.Equal(turns, want) {
		t.Fatalf("architect's turns %v, want %v", turns, want)
	}
}
