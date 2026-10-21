package intakejourney

import (
	context "context"
	errors "errors"
	fmt "fmt"

	agent "github.com/kpenfound/agentcore/agent"
	config "github.com/kpenfound/osmia/internal/config"
	coreadapter "github.com/kpenfound/osmia/internal/coreadapter"
	plan "github.com/kpenfound/osmia/internal/plan"
	intakefixture "github.com/kpenfound/osmia/internal/servicetest/intakefixture"

	os "os"
	filepath "path/filepath"
	slices "slices"
	strings "strings"
	sync "sync"
	testing "testing"
	time "time"

	scheduler "github.com/kpenfound/osmia/internal/scheduler"
	shed "github.com/kpenfound/osmia/internal/shed"
	trace "github.com/kpenfound/osmia/internal/trace"
	mcp "github.com/modelcontextprotocol/go-sdk/mcp"
)

const shedCharter = intakefixture.ShedCharter

// shedFixture is an architect fixture whose service also runs the committee,
// in a container, with the given number of members.
type shedFixture struct {
	*architectFixture
	members int
}

// newShedFixture debates for one round, so a test of a round finds the shed
// at rest once the round's debate concluded.
func newShedFixture(t *testing.T, members int) *shedFixture {
	t.Helper()
	return newDebateFixture(t, members, 1)
}

// newDebateFixture debates for at most the given number of rounds. The
// architect's replies are silent unless a test scripts them.
func newDebateFixture(t *testing.T, members, rounds int) *shedFixture {
	t.Helper()
	return newDebateFixtureWith(t, members, rounds, "", nil)
}

// newSketchedDebateFixture leaves the committee out of the initial service so
// setup can stop at sketched before planting an amendment. stopAtSketch
// restores the runner for manual passes and subsequent service starts.
func newSketchedDebateFixture(t *testing.T, members, rounds int) *shedFixture {
	t.Helper()
	var committee *Committee
	f := newDebateFixtureWith(t, members, rounds, "", func(o *Options) {
		committee, o.Committee = o.Committee, nil
	})
	f.opts.Committee = committee
	return f
}

func (f *shedFixture) stopAtSketch(t *testing.T, stream config.WorkstreamID) {
	t.Helper()
	f.await(t, stream, sketched)
	f.stop(t)
	f.s.options.Committee = f.opts.Committee
}

// newDebateFixtureWith is newDebateFixture with extra configuration appended
// and prepare, when given, adjusting the options before the service starts.
func newDebateFixtureWith(t *testing.T, members, rounds int, extra string, prepare func(*Options)) *shedFixture {
	t.Helper()
	opts, clone, engine, sessions, clock := newArchitectOptions(t)
	intakefixture.ConfigureDebate(t, opts.Config.Root, members, rounds, extra)
	opts.Committee = &Committee{Engine: engine, Hosts: opts.Architect.Hosts}
	if prepare != nil {
		prepare(&opts)
	}
	f := &architectFixture{opts: opts, clone: clone, engine: engine, sessions: sessions, clock: clock}
	f.start(t)
	f.project, f.trace = intakefixture.AddArchitectProject(t, f.c, clone, shedCharter, func(t *testing.T, p config.ProjectID, text string) { writeCharter(t, f.c, p, text) })
	f.scenario().ScriptDebate(rounds)
	return &shedFixture{architectFixture: f, members: members}
}

// member installs the fake behaviour of one member's turn. Errors it returns
// fail the turn, so checks collect theirs for the test to read.
func (f *shedFixture) member(round, i, attempt int, run func(ctx context.Context, req agent.Request, verified *agent.Turn, tools *mcp.ClientSession) error) string {
	return f.scenario().Member(round, i, attempt, run)
}

// awaitShed waits until the workstream's shed has reached the wanted state,
// whatever it moved to since. It fails at once when the shed reaches a state
// in never, after which the wanted one cannot come: by default, for a heard
// round, its failure, and for a conclusion, the next round.
func (f *shedFixture) awaitShed(t *testing.T, stream config.WorkstreamID, want string, never ...string) {
	t.Helper()
	f.scenario().AwaitShed(t, stream, want, never...)
}

func (f *shedFixture) roundOperations(t *testing.T, stream config.WorkstreamID) []trace.OperationRecord {
	t.Helper()
	return f.scenario().RoundOperations(t, stream)
}

// acknowledgedRoundOperations returns the round operations once every one of
// them is acknowledged, for assertions reached through an awaited state.
func (f *shedFixture) acknowledgedRoundOperations(t *testing.T, stream config.WorkstreamID) []trace.OperationRecord {
	t.Helper()
	return f.scenario().AcknowledgedRoundOperations(t, stream)
}

var shedTool = intakefixture.ShedTool

var newBarrier = intakefixture.NewBarrier

// seen is what one member read in its turn.
type seen struct{ spec, plan, prompt string }

var checkCommitteeBoundary = intakefixture.CheckCommitteeBoundary

func TestCommitteeRoundRunsEveryMemberInParallelOnOnePinnedRevision(t *testing.T) {
	t.Parallel()
	const members = 3
	f := newShedFixture(t, members)
	defer f.stop(t)
	ctx := context.Background()
	// The architect, like every role but the committee, cannot contribute.
	var architectProblems []error
	f.script("draft-1-1", map[string]string{plan.SpecPath: validSpec, plan.PlanPath: validPlan},
		func(ctx context.Context, _ agent.Request, _ *agent.Turn, tools *mcp.ClientSession) error {
			for _, name := range []string{shed.ObjectTool, shed.ConcedeTool} {
				if _, err := callTool(ctx, tools, name, map[string]any{"kind": "fit", "part": "spec", "argument": "x", "citations": []string{"charter#1"}, "objection": "x", "reason": "y"}); err == nil {
					architectProblems = append(architectProblems, fmt.Errorf("the architect called %s", name))
				}
			}
			return nil
		})
	all := newBarrier(members)
	var mu sync.Mutex
	reads := map[int]seen{}
	var problems []error
	report := func(err error) {
		mu.Lock()
		defer mu.Unlock()
		if err != nil {
			problems = append(problems, err)
		}
	}
	read := func(ctx context.Context, i int, req agent.Request, tools *mcp.ClientSession) error {
		if err := all.Wait(ctx); err != nil {
			return err
		}
		spec, err := readTool(ctx, tools, "spec.md")
		if err != nil {
			return err
		}
		graph, err := readTool(ctx, tools, "plan.json")
		mu.Lock()
		reads[i] = seen{spec, graph, req.Prompt}
		mu.Unlock()
		return err
	}
	var turns []string
	turns = append(turns, f.member(1, 1, 1, func(ctx context.Context, req agent.Request, verified *agent.Turn, tools *mcp.ClientSession) error {
		if err := read(ctx, 1, req, tools); err != nil {
			return err
		}
		report(checkCommitteeBoundary(ctx, req, verified, tools, f.clone))

		if recorded, reason, err := shedTool(ctx, tools, shed.ObjectTool, map[string]any{"kind": "charter", "part": "spec#2", "argument": "No test shows it.", "citations": []string{"spec#2"}}); err != nil || recorded || !strings.Contains(reason, "must cite the charter rule") {
			report(fmt.Errorf("charter objection without a charter citation: %v %q %v", recorded, reason, err))
		}
		if recorded, id, err := shedTool(ctx, tools, shed.ObjectTool, map[string]any{"kind": "charter", "part": "spec#2", "argument": "No test shows it.", "citations": []string{"charter#2", "plan#dedupe"}}); err != nil || !recorded || id != shed.ObjectionID(1, committeeAgent(1), 1) {
			report(fmt.Errorf("charter objection: %v %q %v", recorded, id, err))
		}
		recorded, id, err := shedTool(ctx, tools, shed.ObjectTool, map[string]any{"kind": "size", "part": "plan#resume", "argument": "Too wide.", "citations": []string{"kb/entities.json#internal.trace"}})
		if err != nil || !recorded {
			report(fmt.Errorf("size objection: %v %q %v", recorded, id, err))
		}
		if recorded, reason, err := shedTool(ctx, tools, shed.ConcedeTool, map[string]any{"objection": id, "reason": "One criterion is not wide."}); err != nil || !recorded {
			report(fmt.Errorf("concede: %q %v", reason, err))
		}
		return nil
	}))
	turns = append(turns, f.member(1, 2, 1, func(ctx context.Context, req agent.Request, _ *agent.Turn, tools *mcp.ClientSession) error {
		if err := read(ctx, 2, req, tools); err != nil {
			return err
		}
		if recorded, reason, err := shedTool(ctx, tools, shed.ConcedeTool, map[string]any{"objection": shed.ObjectionID(1, committeeAgent(1), 1), "reason": "x"}); err != nil || recorded {
			report(fmt.Errorf("conceded for another member: %q %v", reason, err))
		}
		if recorded, id, err := shedTool(ctx, tools, shed.ObjectTool, map[string]any{"kind": "fit", "part": "plan", "argument": "The design resumes; the plan restarts.", "citations": []string{"spec#1"}}); err != nil || !recorded || id != shed.ObjectionID(1, committeeAgent(2), 1) {
			report(fmt.Errorf("fit objection: %v %q %v", recorded, id, err))
		}
		return nil
	}))
	turns = append(turns, f.member(1, 3, 1, func(ctx context.Context, req agent.Request, _ *agent.Turn, tools *mcp.ClientSession) error {
		return read(ctx, 3, req, tools)
	}))
	stream := f.handIn(t, "design", handedDesign)
	f.awaitShed(t, stream, "concluded-1")
	if err := errors.Join(append(problems, architectProblems...)...); err != nil {
		t.Fatal(err)
	}

	runs := f.runs()
	for _, turn := range turns {
		if n := len(slices.DeleteFunc(slices.Clone(runs), func(r string) bool { return r != turn })); n != 1 {
			t.Fatalf("turn %s ran %d times: %v", turn, n, runs)
		}
	}
	if len(reads) != members {
		t.Fatalf("members that read the revision: %d", len(reads))
	}
	for i, got := range reads {
		if got != (seen{validSpec, validPlan, reads[1].prompt}) {
			t.Fatalf("member %d read another revision or prompt: %+v", i, got)
		}
	}
	// sketched -> in-shed is one recorded transition with its actor and reason.
	var moves []trace.Transition
	for _, tr := range f.transitions(t, stream) {
		if tr.Subject == trace.FeatureSubject {
			moves = append(moves, tr)
		}
	}
	if len(moves) != 3 || moves[2].ID != InShedState || moves[2].From != SketchedState || moves[2].To != InShedState || moves[2].Actor != shedActor || moves[2].Cause != SketchedState ||
		moves[2].Reason != "spec.md revision 1 and plan.json revision 1 enter the shed with a committee of 3" {
		t.Fatalf("feature transitions: %+v", moves)
	}
	if status, err := f.c.Status(ctx, stream); err != nil || status.State == nil || *status.State != InShedState {
		t.Fatalf("status: %+v %v", status, err)
	}

	threads, err := f.repository().Threads(stream)
	must(t, err)
	var committee []string
	for _, th := range threads {
		if th.Identity.Role == committeeRole {
			committee = append(committee, th.Identity.ID)
			if len(th.Turns) != 1 || th.Turns[0].Status() != "idle" || th.Identity.Actor != shedActor {
				t.Fatalf("committee thread: %+v", th)
			}
		}
	}
	if want := []string{committeeAgent(1), committeeAgent(2), committeeAgent(3)}; !slices.Equal(committee, want) {
		t.Fatalf("committee %v, want %v", committee, want)
	}

	th, err := f.repository().Thread(stream, committeeAgent(1))
	must(t, err)
	if held, err := f.s.dispatchHolds(f.s.current(), f.repository())(scheduler.Candidate{Workstream: stream, Thread: th}); err != nil || !held {
		t.Fatalf("the scheduler does not hold committee turns: %v %v", held, err)
	}

	ops := f.acknowledgedRoundOperations(t, stream)
	if len(ops) != 1 || !ops[0].Acknowledged || ops[0].Result == nil || ops[0].Result.Outcome != "succeeded" ||
		ops[0].Result.Evidence != "round 1 against spec.md revision 1 and plan.json revision 1: 3 members heard, 3 objections, 1 concessions, 0 failed turns; 2 objections stand" {
		t.Fatalf("operations: %+v", ops)
	}
	records, err := shed.Records(f.repository(), stream)
	must(t, err)
	if len(records) != members {
		t.Fatalf("records: %+v", records)
	}
	commit := ""
	for i, r := range records {
		member := committeeAgent(i + 1)
		if r.Round != 1 || r.Member != member || r.Revision != (shed.Pin{Spec: 1, Plan: 1}) || r.Turn != turns[i] || r.Failure != "" {
			t.Fatalf("record %d: %+v", i+1, r)
		}
		docs := f.documents(t, stream, shed.DocumentID(1, member))
		if len(docs) != 1 || docs[0].Path != "shed/round-1/"+member+".json" || docs[0].Actor != (trace.Actor{Kind: "agent", ID: member}) || docs[0].Cause != ops[0].Operation.ID {
			t.Fatalf("document of %s: %+v", member, docs)
		}
		if !strings.Contains(docs[0].Content, `"revision": {`) {
			t.Fatalf("%s: %q", docs[0].Path, docs[0].Content)
		}
		landed := documentCommit(t, f.s.current().Root, f.project, stream, docs[0].Path)
		if commit != "" && landed != commit {
			t.Fatalf("records landed in separate commits: %s %s", commit, landed)
		}
		commit = landed
	}
	if len(records[0].Objections) != 2 || len(records[0].Concessions) != 1 || len(records[1].Objections) != 1 || !records[2].Silent() {
		t.Fatalf("contributions: %+v", records)
	}
	var open []string
	for _, d := range shed.OpenDissent(records) {
		open = append(open, d.ID)
	}
	if want := []string{shed.ObjectionID(1, committeeAgent(1), 1), shed.ObjectionID(1, committeeAgent(2), 1)}; !slices.Equal(open, want) {
		t.Fatalf("open dissent %v, want %v", open, want)
	}
	var shedMoves []trace.Transition
	for _, tr := range f.transitions(t, stream) {
		if tr.Subject == shedSubject {
			shedMoves = append(shedMoves, tr)
		}
	}
	if len(shedMoves) != 5 || shedMoves[0].ID != "shed-round-1" || shedMoves[0].To != "round-1" || shedMoves[0].Cause != InShedState || shedMoves[0].Actor != shedActor ||
		shedMoves[1].ID != "shed-round-1-heard" || shedMoves[1].From != "round-1" || shedMoves[1].To != "heard-1" || shedMoves[1].Cause != ops[0].Operation.ID || shedMoves[1].Reason != ops[0].Result.Evidence {
		t.Fatalf("shed transitions: %+v", shedMoves)
	}

	d := &debate{S: f.s.intake().DebateDependencies(), Repository: f.repository()}
	must(t, d.Pass(ctx))
	same := func(got *coreadapter.OperationResult) bool {
		return got != nil && got.Outcome == ops[0].Result.Outcome && got.Evidence == ops[0].Result.Evidence
	}
	if observed, err := d.Inspect(ctx, ops[0].Operation); err != nil || observed.State != coreadapter.EffectCompleted || !same(observed.Result) {
		t.Fatalf("inspect after completion: %+v %v", observed, err)
	}
	if result, err := d.Apply(ctx, ops[0].Operation); err != nil || !same(&result) {
		t.Fatalf("apply after completion: %+v %v", result, err)
	}
	if again := f.roundOperations(t, stream); len(again) != 1 || len(f.runs()) != len(runs) {
		t.Fatalf("a second round ran: %+v %v", again, f.runs())
	}
	if docs := f.documents(t, stream, shed.DocumentID(1, committeeAgent(1))); len(docs) != 1 {
		t.Fatalf("retry duplicated records: %+v", docs)
	}

	root := f.opts.Config.Root
	if entries, err := os.ReadDir(filepath.Join(root, "views")); err != nil || len(entries) != 0 {
		t.Fatalf("views leaked: %v %v", entries, err)
	}
	for _, turn := range turns {
		if _, err := os.Lstat(filepath.Join(root, "shed", string(f.project), string(stream), turn, "workspace")); !os.IsNotExist(err) {
			t.Fatalf("staged workspace of %s retained", turn)
		}
	}
	if notes, ok := traceFile(t, f.trace, "notes/committee.md"); ok {
		t.Fatalf("committee notes: %q", notes)
	}
	for path, content := range snapshot(t, filepath.Join(root, "projects")) {
		if strings.Contains(content, demoSecret) {
			t.Fatalf("secret copied into %s", path)
		}
	}
}

// A service without a committee runner leaves sketched workstreams where
// they are. A round that a service stop interrupts stays pending in a service
// without a runner, without spending the member's attempt, and the next
// service with one runs only the member that had not finished.
func TestShedWaitsForACommitteeRunnerAndSurvivesRestart(t *testing.T) {
	t.Parallel()
	f := newShedFixture(t, 2)
	ctx := context.Background()
	runner := f.opts.Committee
	f.stop(t)
	f.opts.Committee = nil
	f.start(t)
	stream := f.handIn(t, "design", handedDesign)
	f.await(t, stream, sketched)
	must(t, (&debate{S: f.s.intake().DebateDependencies(), Repository: f.repository()}).Pass(ctx))
	if state, err := f.repository().Workflow(stream, trace.FeatureSubject); err != nil || state.Value != SketchedState {
		t.Fatalf("feature %+v %v", state, err)
	}
	if _, err := f.repository().Thread(stream, committeeAgent(1)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("committee thread without a runner: %v", err)
	}
	f.stop(t)

	first := shed.ObjectionID(1, committeeAgent(1), 1)
	one := f.member(1, 1, 1, func(ctx context.Context, _ agent.Request, _ *agent.Turn, tools *mcp.ClientSession) error {
		_, _, err := shedTool(ctx, tools, shed.ObjectTool, map[string]any{"kind": "fit", "part": "spec", "argument": "It restarts.", "citations": []string{"charter#1"}})
		return err
	})
	entered := make(chan struct{})
	f.member(1, 2, 1, func(ctx context.Context, _ agent.Request, _ *agent.Turn, tools *mcp.ClientSession) error {

		if _, _, err := shedTool(ctx, tools, shed.ObjectTool, map[string]any{"kind": "fit", "part": "plan", "argument": "Dropped.", "citations": []string{"charter#2"}}); err != nil {
			return err
		}
		close(entered)
		<-ctx.Done()
		return ctx.Err()
	})
	f.opts.Committee = runner
	f.start(t)
	select {
	case <-entered:
	case <-time.After(demoTimeout):
		t.Fatal("member 2 did not start")
	}
	deadline := time.Now().Add(demoTimeout)
	for {
		th, err := f.repository().Thread(stream, committeeAgent(1))
		must(t, err)
		if len(th.Turns) == 1 && th.Turns[0].Status() == "idle" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("member 1 did not finish: %+v", th)
		}
		time.Sleep(50 * time.Millisecond)
	}
	f.stop(t)

	f.opts.Committee = nil
	f.start(t)
	d := &debate{S: f.s.intake().DebateDependencies(), Repository: f.repository()}
	ops := f.roundOperations(t, stream)
	if len(ops) != 1 {
		t.Fatalf("operations: %+v", ops)
	}
	if observed, err := d.Inspect(ctx, ops[0].Operation); err != nil || observed.State != coreadapter.EffectAbsent {
		t.Fatalf("inspect an interrupted round: %+v %v", observed, err)
	}
	if _, err := d.Apply(ctx, ops[0].Operation); !errors.Is(err, errNoCommittee) {
		t.Fatalf("apply without a runner: %v", err)
	}
	th, err := f.repository().Thread(stream, committeeAgent(2))
	must(t, err)
	if len(th.Turns) != 1 || th.Turns[0].Status() != "interrupted" {
		t.Fatalf("a turn was queued without a runner: %+v", th.Turns)
	}
	if state, err := f.repository().Workflow(stream, shedSubject); err != nil || state.Value != "round-1" {
		t.Fatalf("shed %+v %v", state, err)
	}
	f.stop(t)

	two := f.member(1, 2, 2, func(context.Context, agent.Request, *agent.Turn, *mcp.ClientSession) error { return nil })
	f.opts.Committee = runner
	f.start(t)
	defer f.stop(t)
	f.awaitShed(t, stream, "heard-1")
	runs := f.runs()
	for _, turn := range []string{one, two} {
		if n := len(slices.DeleteFunc(slices.Clone(runs), func(r string) bool { return r != turn })); n != 1 {
			t.Fatalf("turn %s ran %d times: %v", turn, n, runs)
		}
	}
	records, err := shed.Records(f.repository(), stream)
	must(t, err)
	if len(records) != 2 || len(records[0].Objections) != 1 || records[0].Objections[0].ID != first || records[0].Turn != one || !records[1].Silent() || records[1].Turn != two {
		t.Fatalf("records: %+v", records)
	}
}

// A member whose turn fails is recorded with the failure and what it
// contributed before it; the failed turn accepts nothing, and the round
// still hears the others.
func TestCommitteeRoundRecordsAFailedMember(t *testing.T) {
	t.Parallel()
	f := newShedFixture(t, 2)
	defer f.stop(t)
	f.member(1, 1, 1, func(ctx context.Context, _ agent.Request, _ *agent.Turn, tools *mcp.ClientSession) error {
		if recorded, id, err := shedTool(ctx, tools, shed.ObjectTool, map[string]any{"kind": "acceptance", "part": "plan#dedupe", "argument": "A judgement shows nothing.", "citations": []string{"spec#2"}}); err != nil || !recorded {
			return fmt.Errorf("proof objection: %q %v", id, err)
		}
		return errors.New("the agent crashed")
	})
	f.member(1, 2, 1, func(context.Context, agent.Request, *agent.Turn, *mcp.ClientSession) error { return nil })
	stream := f.handIn(t, "design", handedDesign)
	f.awaitShed(t, stream, "heard-1")
	records, err := shed.Records(f.repository(), stream)
	must(t, err)
	if len(records) != 2 || !strings.Contains(records[0].Failure, "the agent crashed") || len(records[0].Objections) != 1 || records[0].Objections[0].Kind != shed.Acceptance || !records[1].Silent() {
		t.Fatalf("records: %+v", records)
	}
	ops := f.acknowledgedRoundOperations(t, stream)
	if len(ops) != 1 || ops[0].Result == nil || ops[0].Result.Outcome != "succeeded" || !strings.Contains(ops[0].Result.Evidence, "2 members heard, 1 objections, 0 concessions, 1 failed turns; 1 objections stand") {
		t.Fatalf("operations: %+v", ops)
	}
}

// A later round pins the revision it is given, lists the member's standing
// objections, shows the earlier rounds, and validates against the pinned
// revision rather than the latest one.
func TestLaterRoundPinsItsRevisionAndCarriesStandingObjections(t *testing.T) {
	t.Parallel()
	f := newShedFixture(t, 2)
	defer f.stop(t)
	ctx := context.Background()
	first := shed.ObjectionID(1, committeeAgent(1), 1)
	f.member(1, 1, 1, func(ctx context.Context, _ agent.Request, _ *agent.Turn, tools *mcp.ClientSession) error {
		_, _, err := shedTool(ctx, tools, shed.ObjectTool, map[string]any{"kind": "fit", "part": "spec#2", "argument": "The design never asks for it.", "citations": []string{"spec#2"}})
		return err
	})
	f.member(1, 2, 1, func(context.Context, agent.Request, *agent.Turn, *mcp.ClientSession) error { return nil })
	stream := f.handIn(t, "design", handedDesign)

	f.awaitShed(t, stream, "concluded-1")

	revised := strings.Replace(validSpec, "2. Acknowledged chunks are never sent again.\n", "", 1)
	for i, content := range []string{revised, validSpec} {
		must(t, f.repository().RecordDocuments(ctx, []trace.Document{{Header: trace.Header{Schema: "osmia.trace.document", Version: trace.Version, ID: plan.SpecDocument, Revision: i + 2, Project: f.project, Workstream: stream, At: f.clock.Now(), Actor: architectActor, Cause: "redraft"}, Path: plan.SpecPath, Content: content}}))
	}
	var problems []error
	var mu sync.Mutex
	report := func(format string, args ...any) {
		mu.Lock()
		defer mu.Unlock()
		problems = append(problems, fmt.Errorf(format, args...))
	}
	f.member(2, 1, 1, func(ctx context.Context, req agent.Request, _ *agent.Turn, tools *mcp.ClientSession) error {
		for _, want := range []string{"Round 2 of the shed", "spec.md revision 2 and plan.json revision 1", "Your objections that still stand", first + " (fit, spec#2, made against spec.md revision 1 and plan.json revision 1): The design never asks for it."} {
			if !strings.Contains(req.Prompt, want) {
				report("prompt lacks %q:\n%s", want, req.Prompt)
			}
		}
		if got, err := readTool(ctx, tools, "spec.md"); err != nil || got != revised {
			report("pinned spec: %q %v", got, err)
		}
		if got, err := readTool(ctx, tools, "shed/round-1/"+committeeAgent(1)+".json"); err != nil || !strings.Contains(got, first) {
			report("earlier round: %q %v", got, err)
		}
		if recorded, reason, err := shedTool(ctx, tools, shed.ObjectTool, map[string]any{"kind": "fit", "part": "spec#1", "argument": "x", "citations": []string{"spec#2"}}); err != nil || recorded || !strings.Contains(reason, "spec.md revision 2 has no acceptance criterion numbered 2") {
			report("a criterion of the latest revision only was cited: %v %q %v", recorded, reason, err)
		}
		return nil
	})
	f.member(2, 2, 1, func(_ context.Context, req agent.Request, _ *agent.Turn, _ *mcp.ClientSession) error {
		if strings.Contains(req.Prompt, "still stand") {
			report("a member without objections is told of some:\n%s", req.Prompt)
		}
		return nil
	})
	d := &debate{S: f.s.intake().DebateDependencies(), Repository: f.repository()}
	state, err := f.repository().Workflow(stream, shedSubject)
	must(t, err)
	must(t, d.Request(ctx, stream, state, roundInput{Round: 2, Spec: 2, Plan: 1}, "shed-concluded-1"))
	f.awaitShed(t, stream, "heard-2")
	if err := errors.Join(problems...); err != nil {
		t.Fatal(err)
	}
	records, err := shed.Records(f.repository(), stream)
	must(t, err)
	if len(records) != 4 || records[2].Revision != (shed.Pin{Spec: 2, Plan: 1}) || !records[2].Silent() {
		t.Fatalf("records: %+v", records)
	}

	if open := shed.OpenDissent(records); len(open) != 0 {
		t.Fatalf("open dissent: %+v", open)
	}
}

// A stop between the round's record and its transition leaves the files
// recorded: the next service records nothing again and only moves the shed,
// to heard even when the workstream was abandoned in between.
// A turn the stopped service captured is completed without running a member.
func TestRoundRecordedBeforeAStopIsNotRecordedAgain(t *testing.T) {
	t.Parallel()
	for _, crash := range []string{"captured", "recorded-then-abandoned"} {
		t.Run(crash, func(t *testing.T) {
			f := newShedFixture(t, 1)
			ctx := context.Background()
			member := committeeAgent(1)
			entered := make(chan struct{})
			f.member(1, 1, 1, func(ctx context.Context, _ agent.Request, _ *agent.Turn, _ *mcp.ClientSession) error {
				close(entered)
				<-ctx.Done()
				return ctx.Err()
			})
			stream := f.handIn(t, "design", handedDesign)
			select {
			case <-entered:
			case <-time.After(demoTimeout):
				t.Fatal("the round did not start")
			}
			f.stop(t)

			cfg, err := config.Load(f.opts.Config)
			must(t, err)
			repo, err := trace.Open(cfg.Root, config.Project{ID: f.project, Clone: f.clone})
			must(t, err)
			th, err := repo.Thread(stream, member)
			must(t, err)
			if len(th.Turns) != 1 || th.Turns[0].Status() != "interrupted" {
				t.Fatalf("interrupted turn: %+v", th)
			}
			ops, err := repo.Operations(stream)
			must(t, err)
			operation := ""
			for _, o := range ops {
				if o.Operation.Action == RoundAction {
					operation = o.Operation.ID
				}
			}
			second := th.Turns[0].Request
			second.TurnID = roundTurnID(1, member, 2)
			second.ID, second.At = "request_"+second.TurnID, f.clock.Now()
			_, err = repo.EnqueueTurn(ctx, second)
			must(t, err)
			directory := filepath.Join(cfg.Root.String(), "shed", string(f.project), string(stream), second.TurnID)
			claimed, err := repo.ClaimTurn(ctx, stream, member, "earlier-session", filepath.Join(directory, "session"), f.clock.Now())
			must(t, err)
			h := second.Header
			h.Schema, h.ID, h.At, h.Actor = "osmia.trace.turn-response", trace.EventID(second.ID, "response"), f.clock.Now(), trace.Actor{Kind: "service", ID: "thread-runner"}
			response := trace.TurnResponse{Header: h, AgentID: member, ThreadID: second.ThreadID, TurnID: second.TurnID, RequestID: second.ID, RequestRevision: second.Revision,
				Result: coreadapter.SessionResult{Session: coreadapter.BackendSession{Backend: second.Profile.Backend, ID: "session-2"}, SessionDirectory: claimed.Claim.SessionDirectory, StartedAt: claimed.Claim.At, Duration: time.Second, FinalResponse: "Round read"}}
			must(t, repo.CaptureTurn(ctx, "earlier-session", response))
			kept := shed.Record{Version: shed.Version, Round: 1, Member: member, Revision: shed.Pin{Spec: 1, Plan: 1}, Turn: second.TurnID,
				Objections: []shed.Objection{{ID: shed.ObjectionID(1, member, 1), Kind: shed.Fit, Part: "spec", Argument: "It restarts.", Citations: []string{"charter#1"}}}}
			data, err := shed.Encode(kept)
			must(t, err)
			must(t, os.MkdirAll(filepath.Join(directory, "output"), 0700))
			must(t, os.WriteFile(filepath.Join(directory, "output", "contributions.json"), data, 0600))
			if crash != "captured" {
				must(t, repo.CompleteTurn(ctx, stream, member, second.TurnID, "earlier-session", f.clock.Now()))
				must(t, repo.RecordDocuments(ctx, []trace.Document{{Header: trace.Header{Schema: "osmia.trace.document", Version: trace.Version, ID: shed.DocumentID(1, member), Revision: 1, Project: f.project, Workstream: stream, At: f.clock.Now(), Actor: trace.Actor{Kind: "agent", ID: member}, Cause: operation, Depth: 1},
					Path: shed.Path(1, member), Content: string(data)}}))

				h := trace.Header{Schema: "osmia.trace.transition", Version: trace.Version, ID: "abandoned", Revision: 1, Project: f.project, Workstream: stream, At: f.clock.Now(), Actor: ownerActor, Cause: "owner"}
				_, err := repo.SetFeatureState(ctx, h, AbandonedState, "the owner abandoned the workstream")
				must(t, err)
			}
			must(t, repo.Close())

			f.start(t)
			defer f.stop(t)
			f.awaitShed(t, stream, "heard-1")
			if runs := f.runs(); slices.Contains(runs, second.TurnID) || len(slices.DeleteFunc(runs, func(r string) bool { return r != roundTurnID(1, member, 1) })) != 1 {
				t.Fatalf("backend runs %v", f.runs())
			}
			docs := f.documents(t, stream, shed.DocumentID(1, member))
			if len(docs) != 1 || docs[0].Cause != operation || docs[0].Content != string(data) {
				t.Fatalf("records: %+v", docs)
			}
			round := f.acknowledgedRoundOperations(t, stream)
			if len(round) != 1 || round[0].Result == nil || round[0].Result.Outcome != "succeeded" || !strings.Contains(round[0].Result.Evidence, "1 members heard, 1 objections, 0 concessions, 0 failed turns; 1 objections stand") {
				t.Fatalf("operations: %+v", round)
			}
		})
	}
}

// A member whose every attempt a service stop interrupts is recorded as
// failed with the count, and the round still ends.
func TestMemberInterruptedEveryAttemptIsRecordedAsFailed(t *testing.T) {
	t.Parallel()
	f := newShedFixture(t, 2)
	defer f.stop(t)
	for attempt := 1; attempt <= maxRoundAttempts; attempt++ {
		f.member(1, 1, attempt, func(context.Context, agent.Request, *agent.Turn, *mcp.ClientSession) error {
			return errors.Join(context.Canceled, errors.New("connection dropped"))
		})
	}
	f.member(1, 2, 1, func(context.Context, agent.Request, *agent.Turn, *mcp.ClientSession) error { return nil })
	stream := f.handIn(t, "design", handedDesign)
	f.awaitShed(t, stream, "heard-1")
	records, err := shed.Records(f.repository(), stream)
	must(t, err)
	if want := fmt.Sprintf("the member's turn was interrupted %d times by service stops", maxRoundAttempts); len(records) != 2 || records[0].Failure != want || records[0].Turn != "" || !records[1].Silent() {
		t.Fatalf("records: %+v", records)
	}
	th, err := f.repository().Thread(stream, committeeAgent(1))
	must(t, err)
	if len(th.Turns) != maxRoundAttempts {
		t.Fatalf("attempts: %+v", th.Turns)
	}
}

// setCommittee rewrites capacity.committee in the fixture's config.toml.
func (f *shedFixture) setCommittee(t *testing.T, members int) {
	t.Helper()
	path := filepath.Join(f.opts.Config.Root, "config.toml")
	data, err := os.ReadFile(path)
	must(t, err)
	old := fmt.Sprintf("committee = %d\n", f.members)
	if !strings.Contains(string(data), old) {
		t.Fatalf("config.toml lacks %q", old)
	}
	must(t, os.WriteFile(path, []byte(strings.Replace(string(data), old, fmt.Sprintf("committee = %d\n", members), 1)), 0600))
	f.members = members
}

// A committee is fixed once its workstream is in the shed: a later
// capacity.committee neither adds a member nor widens a later round.
func TestCapacityChangeLeavesAnExistingCommitteeAlone(t *testing.T) {
	t.Parallel()
	f := newShedFixture(t, 2)
	ctx := context.Background()
	for round := 1; round <= 2; round++ {
		for i := 1; i <= 3; i++ {
			f.member(round, i, 1, func(context.Context, agent.Request, *agent.Turn, *mcp.ClientSession) error { return nil })
		}
	}
	stream := f.handIn(t, "design", handedDesign)
	f.awaitShed(t, stream, "concluded-1")
	f.stop(t)
	f.setCommittee(t, 3)
	f.start(t)
	defer f.stop(t)
	if got := f.s.current().Capacity.Committee; got != 3 {
		t.Fatalf("loaded capacity.committee %d", got)
	}
	d := &debate{S: f.s.intake().DebateDependencies(), Repository: f.repository()}

	if err := d.Pass(ctx); err != nil && !errors.Is(err, trace.ErrConflict) {
		t.Fatal(err)
	}
	state, err := f.repository().Workflow(stream, shedSubject)
	must(t, err)
	must(t, d.Request(ctx, stream, state, roundInput{Round: 2, Spec: 1, Plan: 1}, "shed-concluded-1"))
	f.awaitShed(t, stream, "heard-2")
	if _, err := f.repository().Thread(stream, committeeAgent(3)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the committee grew: %v", err)
	}
	records, err := shed.Records(f.repository(), stream)
	must(t, err)
	var second []string
	for _, r := range records {
		if r.Round == 2 {
			second = append(second, r.Member)
		}
	}
	if want := []string{committeeAgent(1), committeeAgent(2)}; !slices.Equal(second, want) {
		t.Fatalf("round 2 heard %v, want %v", second, want)
	}
	if runs := f.runs(); slices.Contains(runs, roundTurnID(2, committeeAgent(3), 1)) {
		t.Fatalf("a third member ran: %v", runs)
	}
}

// The in-shed transition counts the committee that runs. A stop after the
// committee's threads were created and before the transition, followed by a
// lower capacity.committee, leaves the larger committee: the reason names it.
func TestInShedReasonCountsTheCommitteeThatRuns(t *testing.T) {
	t.Parallel()
	f := newShedFixture(t, 3)
	ctx := context.Background()
	runner := f.opts.Committee
	f.stop(t)
	f.opts.Committee = nil
	f.start(t)
	stream := f.handIn(t, "design", handedDesign)
	f.await(t, stream, sketched)

	must(t, (&debate{S: f.s.intake().DebateDependencies(), Repository: f.repository()}).EnsureCommittee(ctx, stream, 3))
	f.stop(t)
	f.setCommittee(t, 2)
	all := newBarrier(3)
	for i := 1; i <= 3; i++ {
		f.member(1, i, 1, func(ctx context.Context, _ agent.Request, _ *agent.Turn, _ *mcp.ClientSession) error {
			return all.Wait(ctx)
		})
	}
	f.opts.Committee = runner
	f.start(t)
	defer f.stop(t)
	f.awaitShed(t, stream, "heard-1")
	var reason string
	for _, tr := range f.transitions(t, stream) {
		if tr.Subject == trace.FeatureSubject && tr.To == InShedState {
			reason = tr.Reason
		}
	}
	if want := "spec.md revision 1 and plan.json revision 1 enter the shed with a committee of 3"; reason != want {
		t.Fatalf("in-shed reason %q, want %q", reason, want)
	}
	if records, err := shed.Records(f.repository(), stream); err != nil || len(records) != 3 {
		t.Fatalf("records: %+v %v", records, err)
	}
}
