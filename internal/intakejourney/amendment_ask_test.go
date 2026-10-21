package intakejourney

import (
	context "context"
	json "encoding/json"

	intakefixture "github.com/kpenfound/osmia/internal/servicetest/intakefixture"

	filepath "path/filepath"
	slices "slices"
	strings "strings"
	testing "testing"

	agent "github.com/kpenfound/agentcore/agent"
	config "github.com/kpenfound/osmia/internal/config"
	coreadapter "github.com/kpenfound/osmia/internal/coreadapter"
	plan "github.com/kpenfound/osmia/internal/plan"
	questions "github.com/kpenfound/osmia/internal/questions"
	trace "github.com/kpenfound/osmia/internal/trace"
	mcp "github.com/modelcontextprotocol/go-sdk/mcp"
)

var checkpointSpec = intakefixture.CheckpointSpec
var restartPlan = intakefixture.RestartPlan

const parkedAmendment = "amendment 1 is parked: the architect waits for the answer to question 1"

// amendmentAskFixture is a stopped debate fixture whose sealed workstream is
// building with amendment 1 filed, and the open trace its controller tests
// drive the amendment drafter over.
type amendmentAskFixture struct {
	*shedFixture
	repo   *trace.Repository
	stream config.WorkstreamID
}

func newAmendmentAskFixture(t *testing.T, script func(f *shedFixture)) *amendmentAskFixture {
	t.Helper()
	f := newSketchedDebateFixture(t, 1, 1)
	f.script("draft-1-1", map[string]string{plan.SpecPath: validSpec, plan.PlanPath: validPlan}, nil)
	script(f)
	stream := f.handIn(t, "amendment-ask", handedDesign)
	f.stopAtSketch(t, stream)
	a := &amendmentAskFixture{shedFixture: f, stream: stream}
	a.reopen(t)
	t.Cleanup(func() { a.repo.Close() })
	seedAmendment(t, f.architectFixture, a.repo, stream)
	return a
}

// reopen closes the trace and opens it again as a restarted service would.
func (a *amendmentAskFixture) reopen(t *testing.T) {
	t.Helper()
	if a.repo != nil {
		must(t, a.repo.Close())
	}
	repo, err := trace.Open(a.s.current().Root, a.s.current().Project)
	must(t, err)
	a.repo = repo
}

func (a *amendmentAskFixture) drafter() *amendmentDrafter {
	return &amendmentDrafter{Drafter: &drafter{S: a.s.intake().DrafterDependencies(), Repository: a.repo}}
}

// drafts returns the workstream's amendment draft operations in the order
// they were requested.
func (a *amendmentAskFixture) drafts(t *testing.T) []trace.OperationRecord {
	t.Helper()
	ops, err := a.repo.Operations(a.stream)
	must(t, err)
	return slices.DeleteFunc(ops, func(o trace.OperationRecord) bool { return o.Operation.Action != AmendmentDraftAction })
}

// draft returns the draft operation the given transition requested.
func (a *amendmentAskFixture) draft(t *testing.T, transition string) trace.OperationRecord {
	t.Helper()
	ops := a.drafts(t)
	id := trace.OperationID(a.project, a.stream, trace.EventID(transition, "run"))
	i := slices.IndexFunc(ops, func(o trace.OperationRecord) bool { return o.Operation.ID == id })
	if i < 0 {
		t.Fatalf("no draft operation of %s: %v", transition, results(ops))
	}
	return ops[i]
}

// apply runs the draft operation and returns its result.
func (a *amendmentAskFixture) apply(t *testing.T, op trace.OperationRecord) coreadapter.OperationResult {
	t.Helper()
	result, err := a.drafter().Apply(context.Background(), op.Operation)
	must(t, err)
	return result
}

// moves lists amendment id's transitions.
func (a *amendmentAskFixture) moves(t *testing.T, id string) []string {
	t.Helper()
	transitions, err := trace.Read[trace.Transition](a.repo, a.stream)
	must(t, err)
	var out []string
	for _, tr := range transitions {
		if tr.Subject == amendmentSubject(id) {
			out = append(out, tr.ID+" "+tr.From+" -> "+tr.To)
		}
	}
	return out
}

func (a *amendmentAskFixture) transition(t *testing.T, id string) trace.Transition {
	t.Helper()
	transitions, err := trace.Read[trace.Transition](a.repo, a.stream)
	must(t, err)
	for _, tr := range transitions {
		if tr.ID == id {
			return tr
		}
	}
	t.Fatalf("transition %s is missing", id)
	return trace.Transition{}
}

// candidate returns amendment id's proposed documents by path.
func (a *amendmentAskFixture) candidate(t *testing.T, id string) map[string]trace.Document {
	t.Helper()
	docs, err := trace.Read[trace.Document](a.repo, a.stream)
	must(t, err)
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
	th, err := a.repo.Thread(a.stream, architectAgent)
	must(t, err)
	return th
}

var parks = intakefixture.Parks

// parkFirst requests amendment 1's draft and applies it until the
// architect's question parks it.
func (a *amendmentAskFixture) parkFirst(t *testing.T, p *faults) trace.OperationRecord {
	t.Helper()
	must(t, a.drafter().Pass(context.Background()))
	ops := a.drafts(t)
	if len(ops) != 1 {
		t.Fatalf("draft operations: %v", results(ops))
	}
	if result := a.apply(t, ops[0]); result.Outcome != questions.Waiting || result.Evidence != parkedAmendment {
		t.Fatalf("the asking draft ended %+v", result)
	}
	p.Check(t)
	return ops[0]
}

// An architect that asks while drafting an amendment parks it: the operation
// ends waiting, the thread holds no turn, and the amendment stays waiting
// across a restart without drafting again. Once the answer is queued, also
// across a restart, the amendment resumes through the answer turn, which
// sees the request and adds to what the asking turn delivered, and the
// proposal holds both turns' documents without spending another attempt.
func TestArchitectQuestionParksTheAmendmentUntilItsAnswer(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	p := &faults{}
	var answering string
	f := newAmendmentAskFixture(t, func(f *shedFixture) {
		f.script("amend-1-1", nil, parks(p))
		answering = f.answer("1", func(ctx context.Context, req agent.Request, verified *agent.Turn, tools *mcp.ClientSession) error {
			if got, err := readTool(ctx, tools, "request.json"); err != nil || !strings.Contains(got, "Revise the criterion") {
				p.Report("the answer turn's view lacks the request: %q %v", got, err)
			}
			if !strings.Contains(req.Prompt, "Answer to your question 1.") || !strings.Contains(req.Prompt, askedAnswer) {
				p.Report("answer prompt:\n%s", req.Prompt)
			}
			return delivers(p, plan.PlanPath, restartPlan, draftValid)(ctx, req, verified, tools)
		})
	})
	first := f.parkFirst(t, p)

	park := f.transition(t, "amendment-1-waiting-1")
	if park.From != "drafting" || park.To != amendmentWaiting || park.Cause != first.Operation.ID || park.Reason != parkedAmendment || park.Actor != draftingActor {
		t.Fatalf("park: %+v", park)
	}
	if th := f.thread(t); !th.Parked() || th.Active != "" || th.Turns[len(th.Turns)-1].Request.TurnID != "amend-1-1" {
		t.Fatalf("the architect's thread holds a turn while the amendment waits: %+v", th)
	}
	if docs := f.candidate(t, "1"); len(docs) != 0 {
		t.Fatalf("a parked amendment recorded %+v", docs)
	}
	observed, err := f.drafter().Inspect(ctx, first.Operation)
	must(t, err)
	if observed.State != coreadapter.EffectCompleted || observed.Result == nil || observed.Result.Outcome != questions.Waiting {
		t.Fatalf("parked draft observed as %+v", observed)
	}

	f.reopen(t)
	must(t, f.drafter().Pass(ctx))
	if ops := f.drafts(t); len(ops) != 1 {
		t.Fatalf("a parked amendment was drafted again before its answer: %v", results(ops))
	}
	if result := f.apply(t, first); result.Outcome != questions.Waiting {
		t.Fatalf("the parked operation reapplied as %+v", result)
	}
	if state, err := f.repo.Workflow(f.stream, amendmentSubject("1")); err != nil || state.Value != amendmentWaiting {
		t.Fatalf("amendment after the restart: %+v %v", state, err)
	}

	chiefAnswers(t, f.shedFixture, f.repo, f.stream, "1", askedAnswer, "charter#1")
	f.reopen(t)
	must(t, f.drafter().Pass(ctx))
	must(t, f.drafter().Pass(ctx))
	if ops := f.drafts(t); len(ops) != 2 {
		t.Fatalf("draft operations: %v", results(ops))
	}
	resumption := f.draft(t, "amendment-1-draft-resume-1")
	var in amendmentDraftInput
	if err := json.Unmarshal(resumption.Operation.Input, &in); err != nil || in != (amendmentDraftInput{ID: "1", Resume: 1}) {
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
	if docs[plan.SpecPath].Content != checkpointSpec || docs[plan.PlanPath].Content != restartPlan || docs[plan.SpecPath].Cause != resumption.Operation.ID {
		t.Fatalf("proposal lost the drafted documents: %+v", docs)
	}
	if ran := f.ran(); ran["amend-1-1"] != 1 || ran[answering] != 1 || ran["amend-1-2"] != 0 {
		t.Fatalf("turns ran: %v", ran)
	}
	if specs := readDocs(t, f.repo, f.stream, plan.SpecDocument); len(specs) != 1 || specs[0].Content != validSpec {
		t.Fatalf("sealed spec changed: %+v", specs)
	}
}

// An answer turn a service stop interrupts is followed by the amendment's
// next attempt, whose prompt repeats the answer. The wait spent no attempt:
// the next attempt is the second.
func TestAmendmentAnswerInterruptedByAStopIsRepeated(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	p := &faults{}
	var answering string
	f := newAmendmentAskFixture(t, func(f *shedFixture) {
		f.script("amend-1-1", nil, parks(p))
		answering = f.answer("1", func(context.Context, agent.Request, *agent.Turn, *mcp.ClientSession) error {
			p.Report("the interrupted answer turn ran again")
			return nil
		})
		f.script("amend-1-2", map[string]string{plan.SpecPath: checkpointSpec}, func(_ context.Context, req agent.Request, _ *agent.Turn, _ *mcp.ClientSession) error {
			for _, want := range []string{"The answers to the questions you asked in this amendment:\n\nAnswer to your question 1.", askedAnswer} {
				if !strings.Contains(req.Prompt, want) {
					p.Report("the next attempt's prompt lacks %q:\n%s", want, req.Prompt)
				}
			}
			return nil
		})
	})
	f.parkFirst(t, p)
	chiefAnswers(t, f.shedFixture, f.repo, f.stream, "1", askedAnswer, "charter#1")
	must(t, f.drafter().Pass(ctx))

	_, err := f.repo.ClaimTurn(ctx, f.stream, architectAgent, "claim-"+answering, filepath.Join(f.s.current().Root.String(), "interrupted"), f.clock.Now())
	must(t, err)
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
	if state, err := f.repo.Workflow(f.stream, amendmentSubject("1")); err != nil || state.Value != "proposed" {
		t.Fatalf("amendment %+v %v", state, err)
	}
}

// A parked amendment does not hold the architect's thread: another amendment
// is drafted while it waits, and once its answer is queued behind that
// draft's turns the parked amendment resumes through it, so no answer is
// left queued in front of the next amendment's turn.
func TestParkedAmendmentLeavesNoAnswerBlockingTheNext(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	p := &faults{}
	var answering string
	f := newAmendmentAskFixture(t, func(f *shedFixture) {
		f.script("amend-1-1", nil, parks(p))
		answering = f.answer("1", delivers(p, plan.PlanPath, restartPlan, draftValid))
		f.script("amend-2-1", map[string]string{plan.PlanPath: restartPlan}, nil)
		f.script("amend-3-1", map[string]string{plan.SpecPath: checkpointSpec}, nil)
	})
	f.parkFirst(t, p)

	fileAmendment(t, f.architectFixture, f.repo, f.stream, "2")
	must(t, f.drafter().Pass(ctx))
	if result := f.apply(t, f.draft(t, "amendment-2-draft")); result.Outcome != "succeeded" {
		t.Fatalf("amendment 2 ended %+v", result)
	}

	chiefAnswers(t, f.shedFixture, f.repo, f.stream, "1", askedAnswer, "charter#1")
	fileAmendment(t, f.architectFixture, f.repo, f.stream, "3")
	must(t, f.drafter().Pass(ctx))
	if ops := f.drafts(t); len(ops) != 4 {
		t.Fatalf("draft operations: %v", results(ops))
	}
	for _, transition := range []string{"amendment-1-draft-resume-1", "amendment-3-draft"} {
		if result := f.apply(t, f.draft(t, transition)); result.Outcome != "succeeded" {
			t.Fatalf("%s ended %+v", transition, result)
		}
	}
	p.Check(t)
	for _, id := range []string{"1", "2", "3"} {
		if state, err := f.repo.Workflow(f.stream, amendmentSubject(id)); err != nil || state.Value != "proposed" {
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
