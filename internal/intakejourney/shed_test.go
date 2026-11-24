package intakejourney

import (
	context "context"
	errors "errors"
	fmt "fmt"
	"github.com/kpenfound/osmia/internal/intake"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	"github.com/kpenfound/osmia/internal/servicetest/journey"
	os "os"
	filepath "path/filepath"
	slices "slices"
	strings "strings"
	sync "sync"
	testing "testing"
	time "time"

	agent "github.com/kpenfound/agentcore/agent"
	config "github.com/kpenfound/osmia/internal/config"
	coreadapter "github.com/kpenfound/osmia/internal/coreadapter"
	plan "github.com/kpenfound/osmia/internal/plan"
	"github.com/kpenfound/osmia/internal/projectstore"
	scheduler "github.com/kpenfound/osmia/internal/scheduler"
	intakefixture "github.com/kpenfound/osmia/internal/servicetest/intakefixture"
	shed "github.com/kpenfound/osmia/internal/shed"
	trace "github.com/kpenfound/osmia/internal/trace"
	mcp "github.com/modelcontextprotocol/go-sdk/mcp"
)

func newShedFixture(t *testing.T, members int) *journey.ShedFixture {
	t.Helper()
	return journey.NewDebateFixture(t, members, 1)
}

func newSketchedDebateFixture(t *testing.T, members, rounds int) *journey.ShedFixture {
	t.Helper()
	var committee *intake.Committee
	f := journey.NewDebateFixtureWith(t, members, rounds, "", func(o *journey.Options) {
		committee, o.Committee = o.Committee, nil
	})
	f.Opts.Committee = committee
	return f
}

type seen struct{ spec, plan, prompt string }

func TestCommitteeRoundRunsEveryMemberInParallelOnOnePinnedRevision(t *testing.T) {
	t.Parallel()
	const members = 3
	f := newShedFixture(t, members)
	defer f.Stop(t)
	ctx := context.Background()
	var architectProblems []error
	f.Script("draft-1-1", map[string]string{plan.SpecPath: fixturekit.ValidSpec, plan.PlanPath: fixturekit.ValidPlan},
		func(ctx context.Context, _ agent.Request, _ *agent.Turn, tools *mcp.ClientSession) error {
			for _, name := range []string{shed.ObjectTool, shed.ConcedeTool} {
				if _, err := fixturekit.CallTool(ctx, tools, name, map[string]any{"kind": "fit", "part": "spec", "argument": "x", "citations": []string{"charter#1"}, "objection": "x", "reason": "y"}); err == nil {
					architectProblems = append(architectProblems, fmt.Errorf("the architect called %s", name))
				}
			}
			return nil
		})
	all := intakefixture.NewBarrier(members)
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
		spec, err := intakefixture.ReadTool(ctx, tools, "spec.md")
		if err != nil {
			return err
		}
		graph, err := intakefixture.ReadTool(ctx, tools, "plan.json")
		mu.Lock()
		reads[i] = seen{spec, graph, req.Prompt}
		mu.Unlock()
		return err
	}
	var turns []string
	turns = append(turns, f.Member(1, 1, 1, func(ctx context.Context, req agent.Request, verified *agent.Turn, tools *mcp.ClientSession) error {
		if err := read(ctx, 1, req, tools); err != nil {
			return err
		}
		report(intakefixture.CheckCommitteeBoundary(ctx, req, verified, tools, f.Clone))

		if recorded, reason, err := intakefixture.ShedTool(ctx, tools, shed.ObjectTool, map[string]any{"kind": "charter", "part": "spec#2", "argument": "No test shows it.", "citations": []string{"spec#2"}}); err != nil || recorded || !strings.Contains(reason, "must cite the charter rule") {
			report(fmt.Errorf("charter objection without a charter citation: %v %q %v", recorded, reason, err))
		}
		if recorded, id, err := intakefixture.ShedTool(ctx, tools, shed.ObjectTool, map[string]any{"kind": "charter", "part": "spec#2", "argument": "No test shows it.", "citations": []string{"charter#2", "plan#dedupe"}}); err != nil || !recorded || id != shed.ObjectionID(1, intake.CommitteeAgent(1), 1) {
			report(fmt.Errorf("charter objection: %v %q %v", recorded, id, err))
		}
		recorded, id, err := intakefixture.ShedTool(ctx, tools, shed.ObjectTool, map[string]any{"kind": "size", "part": "plan#resume", "argument": "Too wide.", "citations": []string{"kb/entities.json#internal.trace"}})
		if err != nil || !recorded {
			report(fmt.Errorf("size objection: %v %q %v", recorded, id, err))
		}
		if recorded, reason, err := intakefixture.ShedTool(ctx, tools, shed.ConcedeTool, map[string]any{"objection": id, "reason": "One criterion is not wide."}); err != nil || !recorded {
			report(fmt.Errorf("concede: %q %v", reason, err))
		}
		return nil
	}))
	turns = append(turns, f.Member(1, 2, 1, func(ctx context.Context, req agent.Request, _ *agent.Turn, tools *mcp.ClientSession) error {
		if err := read(ctx, 2, req, tools); err != nil {
			return err
		}
		if recorded, reason, err := intakefixture.ShedTool(ctx, tools, shed.ConcedeTool, map[string]any{"objection": shed.ObjectionID(1, intake.CommitteeAgent(1), 1), "reason": "x"}); err != nil || recorded {
			report(fmt.Errorf("conceded for another member: %q %v", reason, err))
		}
		if recorded, id, err := intakefixture.ShedTool(ctx, tools, shed.ObjectTool, map[string]any{"kind": "fit", "part": "plan", "argument": "The design resumes; the plan restarts.", "citations": []string{"spec#1"}}); err != nil || !recorded || id != shed.ObjectionID(1, intake.CommitteeAgent(2), 1) {
			report(fmt.Errorf("fit objection: %v %q %v", recorded, id, err))
		}
		return nil
	}))
	turns = append(turns, f.Member(1, 3, 1, func(ctx context.Context, req agent.Request, _ *agent.Turn, tools *mcp.ClientSession) error {
		return read(ctx, 3, req, tools)
	}))
	stream := f.HandIn(t, "design", fixturekit.HandedDesign)
	f.AwaitShed(t, stream, "concluded-1")
	if err := errors.Join(append(problems, architectProblems...)...); err != nil {
		t.Fatal(err)
	}

	runs := f.Runs()
	t.Run("every member runs once and reads the same revision", func(t *testing.T) {
		for _, turn := range turns {
			if n := len(slices.DeleteFunc(slices.Clone(runs), func(r string) bool { return r != turn })); n != 1 {
				t.Fatalf("turn %s ran %d times: %v", turn, n, runs)
			}
		}
		if len(reads) != members {
			t.Fatalf("members that read the revision: %d", len(reads))
		}
		for i, got := range reads {
			if got != (seen{fixturekit.ValidSpec, fixturekit.ValidPlan, reads[1].prompt}) {
				t.Fatalf("member %d read another revision or prompt: %+v", i, got)
			}
		}
	})
	t.Run("the workstream enters the shed with the committee", func(t *testing.T) {
		var moves []trace.Transition
		for _, tr := range f.Transitions(t, stream) {
			if tr.Subject == trace.FeatureSubject {
				moves = append(moves, tr)
			}
		}
		if len(moves) != 3 || moves[2].ID != intake.InShedState || moves[2].From != intake.SketchedState || moves[2].To != intake.InShedState || moves[2].Actor != intake.ShedActor || moves[2].Cause != intake.SketchedState ||
			moves[2].Reason != "spec.md revision 1 and plan.json revision 1 enter the shed with a committee of 3" {
			t.Fatalf("feature transitions: %+v", moves)
		}
		if status, err := f.C.Status(ctx, stream); err != nil || status.State == nil || *status.State != intake.InShedState {
			t.Fatalf("status: %+v %v", status, err)
		}
	})

	t.Run("committee threads belong to the shed and the scheduler holds their turns", func(t *testing.T) {
		threads, err := f.Repository().Threads(stream)
		fixturekit.Must(t, err)
		var committee []string
		for _, th := range threads {
			if th.Identity.Role == intake.CommitteeRole {
				committee = append(committee, th.Identity.ID)
				if len(th.Turns) != 1 || th.Turns[0].Status() != "idle" || th.Identity.Actor != intake.ShedActor {
					t.Fatalf("committee thread: %+v", th)
				}
			}
		}
		if want := []string{intake.CommitteeAgent(1), intake.CommitteeAgent(2), intake.CommitteeAgent(3)}; !slices.Equal(committee, want) {
			t.Fatalf("committee %v, want %v", committee, want)
		}

		th, err := f.Repository().Thread(stream, intake.CommitteeAgent(1))
		fixturekit.Must(t, err)
		if held, err := f.S.DispatchHolds(f.S.Current(), f.Repository())(scheduler.Candidate{Workstream: stream, Thread: th}); err != nil || !held {
			t.Fatalf("the scheduler does not hold committee turns: %v %v", held, err)
		}
	})

	ops := f.AcknowledgedRoundOperations(t, stream)
	t.Run("the round records every member's contribution and the shed moves", func(t *testing.T) {
		if len(ops) != 1 || !ops[0].Acknowledged || ops[0].Result == nil || ops[0].Result.Outcome != "succeeded" ||
			ops[0].Result.Evidence != "round 1 against spec.md revision 1 and plan.json revision 1: 3 members heard, 3 objections, 1 concessions, 0 failed turns; 2 objections stand" {
			t.Fatalf("operations: %+v", ops)
		}
		records, err := shed.Records(f.Repository(), stream)
		fixturekit.Must(t, err)
		if len(records) != members {
			t.Fatalf("records: %+v", records)
		}
		commit := ""
		for i, r := range records {
			member := intake.CommitteeAgent(i + 1)
			if r.Round != 1 || r.Member != member || r.Revision != (shed.Pin{Spec: 1, Plan: 1}) || r.Turn != turns[i] || r.Failure != "" {
				t.Fatalf("record %d: %+v", i+1, r)
			}
			docs := f.Documents(t, stream, shed.DocumentID(1, member))
			if len(docs) != 1 || docs[0].Path != "shed/round-1/"+member+".json" || docs[0].Actor != (trace.Actor{Kind: "agent", ID: member}) || docs[0].Cause != ops[0].Operation.ID {
				t.Fatalf("document of %s: %+v", member, docs)
			}
			if !strings.Contains(docs[0].Content, `"revision": {`) {
				t.Fatalf("%s: %q", docs[0].Path, docs[0].Content)
			}
			landed := intakefixture.DocumentCommit(t, f.S.Current().Root, f.Project, stream, docs[0].Path)
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
		if want := []string{shed.ObjectionID(1, intake.CommitteeAgent(1), 1), shed.ObjectionID(1, intake.CommitteeAgent(2), 1)}; !slices.Equal(open, want) {
			t.Fatalf("open dissent %v, want %v", open, want)
		}
		var shedMoves []trace.Transition
		for _, tr := range f.Transitions(t, stream) {
			if tr.Subject == intake.ShedSubject {
				shedMoves = append(shedMoves, tr)
			}
		}
		if len(shedMoves) != 5 || shedMoves[0].ID != "shed-round-1" || shedMoves[0].To != "round-1" || shedMoves[0].Cause != intake.InShedState || shedMoves[0].Actor != intake.ShedActor ||
			shedMoves[1].ID != "shed-round-1-heard" || shedMoves[1].From != "round-1" || shedMoves[1].To != "heard-1" || shedMoves[1].Cause != ops[0].Operation.ID || shedMoves[1].Reason != ops[0].Result.Evidence {
			t.Fatalf("shed transitions: %+v", shedMoves)
		}
	})

	t.Run("another pass runs no second round and duplicates no record", func(t *testing.T) {
		d := &intake.Debate{S: f.S.Intake().DebateDependencies(), Repository: f.Repository()}
		fixturekit.Must(t, d.Pass(ctx))
		same := func(got *coreadapter.OperationResult) bool {
			return got != nil && got.Outcome == ops[0].Result.Outcome && got.Evidence == ops[0].Result.Evidence
		}
		if observed, err := d.Inspect(ctx, ops[0].Operation); err != nil || observed.State != coreadapter.EffectCompleted || !same(observed.Result) {
			t.Fatalf("inspect after completion: %+v %v", observed, err)
		}
		if result, err := d.Apply(ctx, ops[0].Operation); err != nil || !same(&result) {
			t.Fatalf("apply after completion: %+v %v", result, err)
		}
		if again := f.RoundOperations(t, stream); len(again) != 1 || len(f.Runs()) != len(runs) {
			t.Fatalf("a second round ran: %+v %v", again, f.Runs())
		}
		if docs := f.Documents(t, stream, shed.DocumentID(1, intake.CommitteeAgent(1))); len(docs) != 1 {
			t.Fatalf("retry duplicated records: %+v", docs)
		}
	})

	t.Run("the round leaves no views, workspaces or notes and copies no secret", func(t *testing.T) {
		root := f.Opts.Config.Root
		if entries, err := os.ReadDir(filepath.Join(root, "views")); err != nil || len(entries) != 0 {
			t.Fatalf("views leaked: %v %v", entries, err)
		}
		for _, turn := range turns {
			if _, err := os.Lstat(filepath.Join(root, "shed", string(f.Project), string(stream), turn, "workspace")); !os.IsNotExist(err) {
				t.Fatalf("staged workspace of %s retained", turn)
			}
		}
		if notes, ok := fixturekit.TraceFile(t, f.Trace, "notes/committee.md"); ok {
			t.Fatalf("committee notes: %q", notes)
		}
		for path, content := range intakefixture.Snapshot(t, filepath.Join(root, "projects")) {
			if strings.Contains(content, fixturekit.DemoSecret) {
				t.Fatalf("secret copied into %s", path)
			}
		}
	})
}

func TestShedWaitsForACommitteeRunnerAndSurvivesRestart(t *testing.T) {
	t.Parallel()
	f := newShedFixture(t, 2)
	ctx := context.Background()
	runner := f.Opts.Committee
	f.Stop(t)
	f.Opts.Committee = nil
	f.Start(t)
	stream := f.HandIn(t, "design", fixturekit.HandedDesign)
	f.Await(t, stream, intakefixture.Sketched)
	fixturekit.Must(t, (&intake.Debate{S: f.S.Intake().DebateDependencies(), Repository: f.Repository()}).Pass(ctx))
	if state, err := f.Repository().Workflow(stream, trace.FeatureSubject); err != nil || state.Value != intake.SketchedState {
		t.Fatalf("feature %+v %v", state, err)
	}
	if _, err := f.Repository().Thread(stream, intake.CommitteeAgent(1)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("committee thread without a runner: %v", err)
	}
	f.Stop(t)

	first := shed.ObjectionID(1, intake.CommitteeAgent(1), 1)
	one := f.Member(1, 1, 1, func(ctx context.Context, _ agent.Request, _ *agent.Turn, tools *mcp.ClientSession) error {
		_, _, err := intakefixture.ShedTool(ctx, tools, shed.ObjectTool, map[string]any{"kind": "fit", "part": "spec", "argument": "It restarts.", "citations": []string{"charter#1"}})
		return err
	})
	entered := make(chan struct{})
	f.Member(1, 2, 1, func(ctx context.Context, _ agent.Request, _ *agent.Turn, tools *mcp.ClientSession) error {

		if _, _, err := intakefixture.ShedTool(ctx, tools, shed.ObjectTool, map[string]any{"kind": "fit", "part": "plan", "argument": "Dropped.", "citations": []string{"charter#2"}}); err != nil {
			return err
		}
		close(entered)
		<-ctx.Done()
		return ctx.Err()
	})
	f.Opts.Committee = runner
	f.Start(t)
	select {
	case <-entered:
	case <-time.After(fixturekit.DemoTimeout):
		t.Fatal("member 2 did not start")
	}
	f.S.Until(t, "member 1 to finish", func() bool {
		th, err := f.Repository().Thread(stream, intake.CommitteeAgent(1))
		fixturekit.Must(t, err)
		return len(th.Turns) == 1 && th.Turns[0].Status() == "idle"
	})
	f.Stop(t)

	f.Opts.Committee = nil
	f.Start(t)
	d := &intake.Debate{S: f.S.Intake().DebateDependencies(), Repository: f.Repository()}
	ops := f.RoundOperations(t, stream)
	if len(ops) != 1 {
		t.Fatalf("operations: %+v", ops)
	}
	if observed, err := d.Inspect(ctx, ops[0].Operation); err != nil || observed.State != coreadapter.EffectAbsent {
		t.Fatalf("inspect an interrupted round: %+v %v", observed, err)
	}
	if _, err := d.Apply(ctx, ops[0].Operation); !errors.Is(err, intake.ErrNoCommittee) {
		t.Fatalf("apply without a runner: %v", err)
	}
	th, err := f.Repository().Thread(stream, intake.CommitteeAgent(2))
	fixturekit.Must(t, err)
	if len(th.Turns) != 1 || th.Turns[0].Status() != "interrupted" {
		t.Fatalf("a turn was queued without a runner: %+v", th.Turns)
	}
	if state, err := f.Repository().Workflow(stream, intake.ShedSubject); err != nil || state.Value != "round-1" {
		t.Fatalf("shed %+v %v", state, err)
	}
	f.Stop(t)

	two := f.Member(1, 2, 2, func(context.Context, agent.Request, *agent.Turn, *mcp.ClientSession) error { return nil })
	f.Opts.Committee = runner
	f.Start(t)
	defer f.Stop(t)
	f.AwaitShed(t, stream, "heard-1")
	runs := f.Runs()
	for _, turn := range []string{one, two} {
		if n := len(slices.DeleteFunc(slices.Clone(runs), func(r string) bool { return r != turn })); n != 1 {
			t.Fatalf("turn %s ran %d times: %v", turn, n, runs)
		}
	}
	records, err := shed.Records(f.Repository(), stream)
	fixturekit.Must(t, err)
	if len(records) != 2 || len(records[0].Objections) != 1 || records[0].Objections[0].ID != first || records[0].Turn != one || !records[1].Silent() || records[1].Turn != two {
		t.Fatalf("records: %+v", records)
	}
}

func TestCommitteeRoundRecordsAFailedMember(t *testing.T) {
	t.Parallel()
	f := newShedFixture(t, 2)
	defer f.Stop(t)
	f.Member(1, 1, 1, func(ctx context.Context, _ agent.Request, _ *agent.Turn, tools *mcp.ClientSession) error {
		if recorded, id, err := intakefixture.ShedTool(ctx, tools, shed.ObjectTool, map[string]any{"kind": "acceptance", "part": "plan#dedupe", "argument": "A judgement shows nothing.", "citations": []string{"spec#2"}}); err != nil || !recorded {
			return fmt.Errorf("proof objection: %q %v", id, err)
		}
		return errors.New("the agent crashed")
	})
	f.Member(1, 2, 1, func(context.Context, agent.Request, *agent.Turn, *mcp.ClientSession) error { return nil })
	stream := f.HandIn(t, "design", fixturekit.HandedDesign)
	f.AwaitShed(t, stream, "heard-1")
	records, err := shed.Records(f.Repository(), stream)
	fixturekit.Must(t, err)
	if len(records) != 2 || !strings.Contains(records[0].Failure, "the agent crashed") || len(records[0].Objections) != 1 || records[0].Objections[0].Kind != shed.Acceptance || !records[1].Silent() {
		t.Fatalf("records: %+v", records)
	}
	ops := f.AcknowledgedRoundOperations(t, stream)
	if len(ops) != 1 || ops[0].Result == nil || ops[0].Result.Outcome != "succeeded" || !strings.Contains(ops[0].Result.Evidence, "2 members heard, 1 objections, 0 concessions, 1 failed turns; 1 objections stand") {
		t.Fatalf("operations: %+v", ops)
	}
}

func TestLaterRoundPinsItsRevisionAndCarriesStandingObjections(t *testing.T) {
	t.Parallel()
	f := newShedFixture(t, 2)
	defer f.Stop(t)
	ctx := context.Background()
	first := shed.ObjectionID(1, intake.CommitteeAgent(1), 1)
	f.Member(1, 1, 1, func(ctx context.Context, _ agent.Request, _ *agent.Turn, tools *mcp.ClientSession) error {
		_, _, err := intakefixture.ShedTool(ctx, tools, shed.ObjectTool, map[string]any{"kind": "fit", "part": "spec#2", "argument": "The design never asks for it.", "citations": []string{"spec#2"}})
		return err
	})
	f.Member(1, 2, 1, func(context.Context, agent.Request, *agent.Turn, *mcp.ClientSession) error { return nil })
	stream := f.HandIn(t, "design", fixturekit.HandedDesign)

	f.AwaitShed(t, stream, "concluded-1")

	revised := strings.Replace(fixturekit.ValidSpec, "2. Acknowledged chunks are never sent again.\n", "", 1)
	for i, content := range []string{revised, fixturekit.ValidSpec} {
		fixturekit.Must(t, f.Repository().RecordDocuments(ctx, []trace.Document{{Header: trace.Header{Schema: "osmia.trace.document", Version: trace.Version, ID: plan.SpecDocument, Revision: i + 2, Project: f.Project, Workstream: stream, At: f.Clock.Now(), Actor: intake.ArchitectActor, Cause: "redraft"}, Path: plan.SpecPath, Content: content}}))
	}
	var problems []error
	var mu sync.Mutex
	report := func(format string, args ...any) {
		mu.Lock()
		defer mu.Unlock()
		problems = append(problems, fmt.Errorf(format, args...))
	}
	f.Member(2, 1, 1, func(ctx context.Context, req agent.Request, _ *agent.Turn, tools *mcp.ClientSession) error {
		for _, want := range []string{"Round 2 of the shed", "spec.md revision 2 and plan.json revision 1", "Your objections that still stand", first + " (fit, spec#2, made against spec.md revision 1 and plan.json revision 1): The design never asks for it."} {
			if !strings.Contains(req.Prompt, want) {
				report("prompt lacks %q:\n%s", want, req.Prompt)
			}
		}
		if got, err := intakefixture.ReadTool(ctx, tools, "spec.md"); err != nil || got != revised {
			report("pinned spec: %q %v", got, err)
		}
		if got, err := intakefixture.ReadTool(ctx, tools, "shed/round-1/"+intake.CommitteeAgent(1)+".json"); err != nil || !strings.Contains(got, first) {
			report("earlier round: %q %v", got, err)
		}
		if recorded, reason, err := intakefixture.ShedTool(ctx, tools, shed.ObjectTool, map[string]any{"kind": "fit", "part": "spec#1", "argument": "x", "citations": []string{"spec#2"}}); err != nil || recorded || !strings.Contains(reason, "spec.md revision 2 has no acceptance criterion numbered 2") {
			report("a criterion of the latest revision only was cited: %v %q %v", recorded, reason, err)
		}
		return nil
	})
	f.Member(2, 2, 1, func(_ context.Context, req agent.Request, _ *agent.Turn, _ *mcp.ClientSession) error {
		if strings.Contains(req.Prompt, "still stand") {
			report("a member without objections is told of some:\n%s", req.Prompt)
		}
		return nil
	})
	d := &intake.Debate{S: f.S.Intake().DebateDependencies(), Repository: f.Repository()}
	state, err := f.Repository().Workflow(stream, intake.ShedSubject)
	fixturekit.Must(t, err)
	fixturekit.Must(t, d.Request(ctx, stream, state, intake.RoundInput{Round: 2, Spec: 2, Plan: 1}, "shed-concluded-1"))
	f.AwaitShed(t, stream, "heard-2")
	if err := errors.Join(problems...); err != nil {
		t.Fatal(err)
	}
	records, err := shed.Records(f.Repository(), stream)
	fixturekit.Must(t, err)
	if len(records) != 4 || records[2].Revision != (shed.Pin{Spec: 2, Plan: 1}) || !records[2].Silent() {
		t.Fatalf("records: %+v", records)
	}

	if open := shed.OpenDissent(records); len(open) != 0 {
		t.Fatalf("open dissent: %+v", open)
	}
}

func TestRoundRecordedBeforeAStopIsNotRecordedAgain(t *testing.T) {
	t.Parallel()
	for _, crash := range []string{"captured", "recorded-then-abandoned"} {
		t.Run(crash, func(t *testing.T) {
			f := newShedFixture(t, 1)
			ctx := context.Background()
			member := intake.CommitteeAgent(1)
			entered := make(chan struct{})
			f.Member(1, 1, 1, func(ctx context.Context, _ agent.Request, _ *agent.Turn, _ *mcp.ClientSession) error {
				close(entered)
				<-ctx.Done()
				return ctx.Err()
			})
			stream := f.HandIn(t, "design", fixturekit.HandedDesign)
			select {
			case <-entered:
			case <-time.After(fixturekit.DemoTimeout):
				t.Fatal("the round did not start")
			}
			f.Stop(t)

			cfg, err := projectstore.LoadConfig(f.Opts.Config)
			fixturekit.Must(t, err)
			repo, err := trace.Open(cfg.Root, config.Project{ID: f.Project, Clone: f.Clone})
			fixturekit.Must(t, err)
			th, err := repo.Thread(stream, member)
			fixturekit.Must(t, err)
			if len(th.Turns) != 1 || th.Turns[0].Status() != "interrupted" {
				t.Fatalf("interrupted turn: %+v", th)
			}
			ops, err := repo.Operations(stream)
			fixturekit.Must(t, err)
			operation := ""
			for _, o := range ops {
				if o.Operation.Action == intake.RoundAction {
					operation = o.Operation.ID
				}
			}
			second := th.Turns[0].Request
			second.TurnID = intake.RoundTurnID(1, member, 2)
			second.ID, second.At = "request_"+second.TurnID, f.Clock.Now()
			_, err = repo.EnqueueTurn(ctx, second)
			fixturekit.Must(t, err)
			directory := filepath.Join(cfg.Root.String(), "shed", string(f.Project), string(stream), second.TurnID)
			claimed, err := repo.ClaimTurn(ctx, stream, member, "earlier-session", filepath.Join(directory, "session"), f.Clock.Now())
			fixturekit.Must(t, err)
			h := second.Header
			h.Schema, h.ID, h.At, h.Actor = "osmia.trace.turn-response", trace.EventID(second.ID, "response"), f.Clock.Now(), trace.Actor{Kind: "service", ID: "thread-runner"}
			response := trace.TurnResponse{Header: h, AgentID: member, ThreadID: second.ThreadID, TurnID: second.TurnID, RequestID: second.ID, RequestRevision: second.Revision,
				Result: coreadapter.SessionResult{Session: coreadapter.BackendSession{Backend: second.Profile.Backend, ID: "session-2"}, SessionDirectory: claimed.Claim.SessionDirectory, StartedAt: claimed.Claim.At, Duration: time.Second, FinalResponse: "Round read"}}
			fixturekit.Must(t, repo.CaptureTurn(ctx, "earlier-session", response))
			kept := shed.Record{Version: shed.Version, Round: 1, Member: member, Revision: shed.Pin{Spec: 1, Plan: 1}, Turn: second.TurnID,
				Objections: []shed.Objection{{ID: shed.ObjectionID(1, member, 1), Kind: shed.Fit, Part: "spec", Argument: "It restarts.", Citations: []string{"charter#1"}}}}
			data, err := shed.Encode(kept)
			fixturekit.Must(t, err)
			fixturekit.Must(t, os.MkdirAll(filepath.Join(directory, "output"), 0700))
			fixturekit.Must(t, os.WriteFile(filepath.Join(directory, "output", "contributions.json"), data, 0600))
			if crash != "captured" {
				fixturekit.Must(t, repo.CompleteTurn(ctx, stream, member, second.TurnID, "earlier-session", f.Clock.Now()))
				fixturekit.Must(t, repo.RecordDocuments(ctx, []trace.Document{{Header: trace.Header{Schema: "osmia.trace.document", Version: trace.Version, ID: shed.DocumentID(1, member), Revision: 1, Project: f.Project, Workstream: stream, At: f.Clock.Now(), Actor: trace.Actor{Kind: "agent", ID: member}, Cause: operation, Depth: 1},
					Path: shed.Path(1, member), Content: string(data)}}))

				h := trace.Header{Schema: "osmia.trace.transition", Version: trace.Version, ID: "abandoned", Revision: 1, Project: f.Project, Workstream: stream, At: f.Clock.Now(), Actor: intake.OwnerActor, Cause: "owner"}
				_, err := repo.SetFeatureState(ctx, h, intake.AbandonedState, "the owner abandoned the workstream")
				fixturekit.Must(t, err)
			}
			fixturekit.Must(t, repo.Close())

			f.Start(t)
			defer f.Stop(t)
			f.AwaitShed(t, stream, "heard-1")
			if runs := f.Runs(); slices.Contains(runs, second.TurnID) || len(slices.DeleteFunc(runs, func(r string) bool { return r != intake.RoundTurnID(1, member, 1) })) != 1 {
				t.Fatalf("backend runs %v", f.Runs())
			}
			docs := f.Documents(t, stream, shed.DocumentID(1, member))
			if len(docs) != 1 || docs[0].Cause != operation || docs[0].Content != string(data) {
				t.Fatalf("records: %+v", docs)
			}
			round := f.AcknowledgedRoundOperations(t, stream)
			if len(round) != 1 || round[0].Result == nil || round[0].Result.Outcome != "succeeded" || !strings.Contains(round[0].Result.Evidence, "1 members heard, 1 objections, 0 concessions, 0 failed turns; 1 objections stand") {
				t.Fatalf("operations: %+v", round)
			}
		})
	}
}

func TestMemberInterruptedEveryAttemptIsRecordedAsFailed(t *testing.T) {
	t.Parallel()
	f := newShedFixture(t, 2)
	defer f.Stop(t)
	for attempt := 1; attempt <= intake.MaxRoundAttempts; attempt++ {
		f.Member(1, 1, attempt, func(context.Context, agent.Request, *agent.Turn, *mcp.ClientSession) error {
			return errors.Join(context.Canceled, errors.New("connection dropped"))
		})
	}
	f.Member(1, 2, 1, func(context.Context, agent.Request, *agent.Turn, *mcp.ClientSession) error { return nil })
	stream := f.HandIn(t, "design", fixturekit.HandedDesign)
	f.AwaitShed(t, stream, "heard-1")
	records, err := shed.Records(f.Repository(), stream)
	fixturekit.Must(t, err)
	if want := fmt.Sprintf("the member's turn was interrupted %d times by service stops", intake.MaxRoundAttempts); len(records) != 2 || records[0].Failure != want || records[0].Turn != "" || !records[1].Silent() {
		t.Fatalf("records: %+v", records)
	}
	th, err := f.Repository().Thread(stream, intake.CommitteeAgent(1))
	fixturekit.Must(t, err)
	if len(th.Turns) != intake.MaxRoundAttempts {
		t.Fatalf("attempts: %+v", th.Turns)
	}
}

func TestCapacityChangeLeavesAnExistingCommitteeAlone(t *testing.T) {
	t.Parallel()
	f := newShedFixture(t, 2)
	ctx := context.Background()
	for round := 1; round <= 2; round++ {
		for i := 1; i <= 3; i++ {
			f.Member(round, i, 1, func(context.Context, agent.Request, *agent.Turn, *mcp.ClientSession) error { return nil })
		}
	}
	stream := f.HandIn(t, "design", fixturekit.HandedDesign)
	f.AwaitShed(t, stream, "concluded-1")
	f.Stop(t)
	f.SetCommittee(t, 3)
	f.Start(t)
	defer f.Stop(t)
	if got := f.S.Current().Capacity.Committee; got != 3 {
		t.Fatalf("loaded capacity.committee %d", got)
	}
	d := &intake.Debate{S: f.S.Intake().DebateDependencies(), Repository: f.Repository()}

	if err := d.Pass(ctx); err != nil && !errors.Is(err, trace.ErrConflict) {
		t.Fatal(err)
	}
	state, err := f.Repository().Workflow(stream, intake.ShedSubject)
	fixturekit.Must(t, err)
	fixturekit.Must(t, d.Request(ctx, stream, state, intake.RoundInput{Round: 2, Spec: 1, Plan: 1}, "shed-concluded-1"))
	f.AwaitShed(t, stream, "heard-2")
	if _, err := f.Repository().Thread(stream, intake.CommitteeAgent(3)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the committee grew: %v", err)
	}
	records, err := shed.Records(f.Repository(), stream)
	fixturekit.Must(t, err)
	var second []string
	for _, r := range records {
		if r.Round == 2 {
			second = append(second, r.Member)
		}
	}
	if want := []string{intake.CommitteeAgent(1), intake.CommitteeAgent(2)}; !slices.Equal(second, want) {
		t.Fatalf("round 2 heard %v, want %v", second, want)
	}
	if runs := f.Runs(); slices.Contains(runs, intake.RoundTurnID(2, intake.CommitteeAgent(3), 1)) {
		t.Fatalf("a third member ran: %v", runs)
	}
}

func TestInShedReasonCountsTheCommitteeThatRuns(t *testing.T) {
	t.Parallel()
	f := newShedFixture(t, 3)
	ctx := context.Background()
	runner := f.Opts.Committee
	f.Stop(t)
	f.Opts.Committee = nil
	f.Start(t)
	stream := f.HandIn(t, "design", fixturekit.HandedDesign)
	f.Await(t, stream, intakefixture.Sketched)

	fixturekit.Must(t, (&intake.Debate{S: f.S.Intake().DebateDependencies(), Repository: f.Repository()}).EnsureCommittee(ctx, stream, 3))
	f.Stop(t)
	f.SetCommittee(t, 2)
	all := intakefixture.NewBarrier(3)
	for i := 1; i <= 3; i++ {
		f.Member(1, i, 1, func(ctx context.Context, _ agent.Request, _ *agent.Turn, _ *mcp.ClientSession) error {
			return all.Wait(ctx)
		})
	}
	f.Opts.Committee = runner
	f.Start(t)
	defer f.Stop(t)
	f.AwaitShed(t, stream, "heard-1")
	var reason string
	for _, tr := range f.Transitions(t, stream) {
		if tr.Subject == trace.FeatureSubject && tr.To == intake.InShedState {
			reason = tr.Reason
		}
	}
	if want := "spec.md revision 1 and plan.json revision 1 enter the shed with a committee of 3"; reason != want {
		t.Fatalf("in-shed reason %q, want %q", reason, want)
	}
	if records, err := shed.Records(f.Repository(), stream); err != nil || len(records) != 3 {
		t.Fatalf("records: %+v %v", records, err)
	}
}
