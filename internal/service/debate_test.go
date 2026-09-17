package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kpenfound/busybees/core/agent"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/plan"
	"github.com/kpenfound/osmia/internal/shed"
	"github.com/kpenfound/osmia/internal/thread"
	"github.com/kpenfound/osmia/internal/trace"
)

const (
	// splitPlan splits validPlan's resume unit in two.
	splitPlan = `{"version": 1, "units": [
  {"id": "resume-read", "title": "Find the last chunk", "addresses": [{"criterion": "spec#1", "proof": {"kind": "new-test", "name": "TestLastChunk"}}], "depends_on": [], "footprint": ["internal.trace"]},
  {"id": "resume-write", "title": "Resume from it", "addresses": [{"criterion": "spec#1", "proof": {"kind": "new-test", "name": "TestResume"}}], "depends_on": ["resume-read"], "footprint": ["internal.trace"]},
  {"id": "dedupe", "title": "Skip acknowledged chunks", "addresses": [{"criterion": "spec#2", "proof": {"kind": "new-test", "name": "TestNoChunkTwice"}}], "depends_on": ["resume-write"], "footprint": ["internal.trace"]}
]}
`
	cycleProblem = `unit "dedupe": dependency cycle dedupe -> resume -> dedupe`
)

// faults collects what the fakes of one test found wrong: an error a fake
// returns fails its turn instead of the test.
type faults struct {
	mu   sync.Mutex
	errs []error
}

func (p *faults) report(format string, args ...any) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.errs = append(p.errs, fmt.Errorf(format, args...))
}

func (p *faults) check(t *testing.T) {
	t.Helper()
	p.mu.Lock()
	defer p.mu.Unlock()
	if err := errors.Join(p.errs...); err != nil {
		t.Fatal(err)
	}
}

type fakeTurn = func(ctx context.Context, req agent.Request, verified *agent.Turn, tools *mcp.ClientSession) error

// objects makes a member's turn raise one objection.
func objects(p *faults, kind shed.Kind, part, citation string) fakeTurn {
	return func(ctx context.Context, _ agent.Request, _ *agent.Turn, tools *mcp.ClientSession) error {
		if recorded, reason, err := shedTool(ctx, tools, shed.ObjectTool, map[string]any{"kind": string(kind), "part": part, "argument": "It does not hold.", "citations": []string{citation}}); err != nil || !recorded {
			p.report("%s objection on %s: %q %v", kind, part, reason, err)
		}
		return nil
	}
}

// concedes makes a member's turn concede its objections.
func concedes(p *faults, ids ...string) fakeTurn {
	return func(ctx context.Context, _ agent.Request, _ *agent.Turn, tools *mcp.ClientSession) error {
		for _, id := range ids {
			if recorded, reason, err := shedTool(ctx, tools, shed.ConcedeTool, map[string]any{"objection": id, "reason": "The reply settles it."}); err != nil || !recorded {
				p.report("concede %s: %q %v", id, reason, err)
			}
		}
		return nil
	}
}

func silent(context.Context, agent.Request, *agent.Turn, *mcp.ClientSession) error { return nil }

// answers makes the architect's turn answer objections.
func answers(p *faults, text string, ids ...string) fakeTurn {
	return func(ctx context.Context, _ agent.Request, _ *agent.Turn, tools *mcp.ClientSession) error {
		for _, id := range ids {
			if recorded, reason, err := shedTool(ctx, tools, shed.ReplyTool, map[string]any{"objection": id, "answer": text}); err != nil || !recorded {
				p.report("reply to %s: %q %v", id, reason, err)
			}
		}
		return nil
	}
}

// shedMoves lists the states the workstream's shed went through.
func (f *shedFixture) shedMoves(t *testing.T, stream config.WorkstreamID) []string {
	t.Helper()
	var moves []string
	for _, tr := range f.transitions(t, stream) {
		if tr.Subject == shedSubject {
			moves = append(moves, tr.To)
		}
	}
	return moves
}

func (f *shedFixture) transition(t *testing.T, stream config.WorkstreamID, id string) trace.Transition {
	t.Helper()
	for _, tr := range f.transitions(t, stream) {
		if tr.ID == id {
			return tr
		}
	}
	t.Fatalf("transition %s is not recorded", id)
	return trace.Transition{}
}

func (f *shedFixture) replyOperations(t *testing.T, stream config.WorkstreamID) []trace.OperationRecord {
	t.Helper()
	ops, err := f.repository().Operations(stream)
	must(t, err)
	return slices.DeleteFunc(ops, func(o trace.OperationRecord) bool { return o.Operation.Action != ReplyAction })
}

// ran counts how often the backend ran each turn.
func (f *shedFixture) ran() map[string]int {
	out := map[string]int{}
	for _, turn := range f.runs() {
		out[turn]++
	}
	return out
}

func (f *shedFixture) reply(t *testing.T, stream config.WorkstreamID, round int) shed.Reply {
	t.Helper()
	docs := f.documents(t, stream, shed.ReplyDocumentID(round))
	if len(docs) != 1 || docs[0].Path != shed.ReplyPath(round) || docs[0].Actor != architectActor {
		t.Fatalf("reply documents of round %d: %+v", round, docs)
	}
	reply, err := shed.ParseReply([]byte(docs[0].Content))
	must(t, err)
	return reply
}

// stillInShed asserts that the debate's end moved the workstream nowhere.
func (f *shedFixture) stillInShed(t *testing.T, stream config.WorkstreamID) {
	t.Helper()
	if state, err := f.repository().Workflow(stream, trace.FeatureSubject); err != nil || state.Value != InShedState {
		t.Fatalf("feature %+v %v, want %s", state, err, InShedState)
	}
	if status, err := f.c.Status(context.Background(), stream); err != nil || status.State == nil || *status.State != InShedState {
		t.Fatalf("status: %+v %v", status, err)
	}
	for _, tr := range f.transitions(t, stream) {
		if tr.Subject == trace.FeatureSubject && tr.From == InShedState {
			t.Fatalf("the workstream left the shed: %+v", tr)
		}
	}
}

// concluded returns the notice the conclusion after round n told the chief of
// staff with.
func (f *shedFixture) concluded(t *testing.T, stream config.WorkstreamID, n int) trace.OutboxEntry {
	t.Helper()
	id := fmt.Sprintf("shed-concluded-%d", n)
	outbox, err := f.repository().Outbox(stream)
	must(t, err)
	for _, entry := range outbox {
		if entry.Event.ID == trace.EventID(id, "concluded") {
			if entry.TransitionID != id || entry.Event.Kind != trace.NoticeKind {
				t.Fatalf("conclusion notice: %+v", entry)
			}
			return entry
		}
	}
	t.Fatalf("the outbox holds no conclusion of round %d: %+v", n, outbox)
	return trace.OutboxEntry{}
}

// A round nobody objects in is consensus: the debate ends there, without a
// turn of the architect and without a second round.
func TestDebateConcludesByConsensusInOneRound(t *testing.T) {
	t.Parallel()
	f := newDebateFixture(t, 2, 3)
	defer f.stop(t)
	f.member(1, 1, 1, silent)
	f.member(1, 2, 1, silent)
	stream := f.handIn(t, "design", handedDesign)
	f.awaitShed(t, stream, "concluded-1")
	if moves, want := f.shedMoves(t, stream), []string{"round-1", "heard-1", "concluded-1"}; !slices.Equal(moves, want) {
		t.Fatalf("shed went %v, want %v", moves, want)
	}
	end := f.transition(t, stream, "shed-concluded-1")
	if end.Reason != "debate concluded by consensus after round 1: no objection stands" || end.Cause != "shed-round-1-heard" || end.Actor != shedActor || end.From != "heard-1" {
		t.Fatalf("conclusion: %+v", end)
	}
	if notice := f.concluded(t, stream, 1); notice.Event.Body != "Debate concluded: "+end.Reason+". The workstream stays in-shed until the owner rules." {
		t.Fatalf("notice %q", notice.Event.Body)
	}
	f.stillInShed(t, stream)
	if ran := f.ran(); ran[replyTurnID(1, 1)] != 0 || len(f.replyOperations(t, stream)) != 0 || len(f.roundOperations(t, stream)) != 1 {
		t.Fatalf("consensus ran more than its round: %v", ran)
	}
	if open, err := Dissent(f.repository(), stream); err != nil || len(open) != 0 {
		t.Fatalf("dissent %+v %v", open, err)
	}
	// Another pass derives the same state and writes nothing.
	must(t, (&debate{s: f.s, repository: f.repository()}).Pass(context.Background()))
	if moves := f.shedMoves(t, stream); len(moves) != 3 {
		t.Fatalf("a pass after the conclusion moved the shed: %v", moves)
	}
}

// checkReplyBoundary makes the assertions from inside the architect's reply
// turn: it reads the round and the draft, answers and redrafts, and holds
// nothing that contributes as a member, writes, runs or fetches.
func checkReplyBoundary(ctx context.Context, p *faults, req agent.Request, verified *agent.Turn, tools *mcp.ClientSession, clone string) {
	listed, err := tools.ListTools(ctx, nil)
	if err != nil {
		p.report("list tools: %v", err)
		return
	}
	var names []string
	for _, tool := range listed.Tools {
		names = append(names, tool.Name)
	}
	slices.Sort(names)
	if want := []string{DraftTool, "file_read", shed.ReplyTool}; !slices.Equal(names, want) {
		p.report("role tools %v, want %v", names, want)
	}
	for _, name := range []string{shed.ObjectTool, shed.ConcedeTool, "file_write", "shell", "fetch", "git_push", "notes_write", "set_status"} {
		if _, err := callTool(ctx, tools, name, map[string]any{"kind": "fit", "part": "spec", "argument": "x", "citations": []string{"charter#1"}, "objection": "x", "reason": "y"}); err == nil {
			p.report("the architect called %s", name)
		}
	}
	for path, want := range map[string]string{"spec.md": validSpec, "plan.json": validPlan, "handed/stdin": handedDesign, "charter.md": shedCharter} {
		if got, err := readTool(ctx, tools, path); err != nil || got != want {
			p.report("read %s: %q %v", path, got, err)
		}
	}
	for _, path := range []string{"repo/CODEOWNERS", "redraft/plan.json", ".git/HEAD", filepath.Join(clone, "CODEOWNERS")} {
		if _, err := readTool(ctx, tools, path); err == nil {
			p.report("the architect read %s", path)
		}
	}
	checkReadOnlyGrants(req, verified, clone, p.report)
	if req.Profile.VCSAccess || req.Profile.Name != architectRole {
		p.report("request carries VCS access or another role: %+v", req.Profile)
	}
}

// The architect answers the round once and redrafts; the committee debates
// the redraft, concedes, and the debate ends early by consensus.
func TestRedraftThenConcessionEndsTheDebate(t *testing.T) {
	t.Parallel()
	f := newDebateFixture(t, 2, 3)
	defer f.stop(t)
	p := &faults{}
	size, fit := shed.ObjectionID(1, committeeAgent(1), 1), shed.ObjectionID(1, committeeAgent(2), 1)
	f.member(1, 1, 1, objects(p, shed.Size, "plan#resume", "kb/entities.json#internal.trace"))
	f.member(1, 2, 1, objects(p, shed.Fit, "plan", "spec#1"))
	var stream config.WorkstreamID
	f.script(replyTurnID(1, 1), map[string]string{plan.PlanPath: splitPlan}, func(ctx context.Context, req agent.Request, verified *agent.Turn, tools *mcp.ClientSession) error {
		checkReplyBoundary(ctx, p, req, verified, tools, f.clone)
		for _, want := range []string{"Round 1 of the shed is heard", "The committee debated spec.md revision 1 and plan.json revision 1. 2 objections stand",
			"- " + size + " (size, blocking, by " + committeeAgent(1) + " in round 1 on plan#resume, citing kb/entities.json#internal.trace): It does not hold.",
			"- " + fit + " (fit, advisory, by " + committeeAgent(2) + " in round 1 on plan, citing spec#1): It does not hold.", "one reply to this round", shed.ReplyTool, DraftTool} {
			if !strings.Contains(req.Prompt, want) {
				p.report("reply prompt lacks %q:\n%s", want, req.Prompt)
			}
		}
		if strings.Contains(req.Prompt, "was not accepted") {
			p.report("the first reply turn is told of a returned redraft:\n%s", req.Prompt)
		}
		if got, err := readTool(ctx, tools, shed.Path(1, committeeAgent(1))); err != nil || !strings.Contains(got, size) {
			p.report("the round's record: %q %v", got, err)
		}
		// The committee holds no slot while the architect answers.
		threads, err := f.repository().Threads(stream)
		if err != nil {
			p.report("threads: %v", err)
		}
		for _, th := range threads {
			for _, q := range th.Turns {
				if th.Identity.Role == committeeRole && q.CompletedAt.IsZero() {
					p.report("committee turn %s is in flight during the architect's reply", q.Request.TurnID)
				}
			}
		}
		if recorded, reason, err := shedTool(ctx, tools, shed.ReplyTool, map[string]any{"objection": shed.ObjectionID(1, committeeAgent(1), 2), "answer": "x"}); err != nil || recorded || !strings.Contains(reason, "stands after round 1") {
			p.report("answered an objection nobody made: %v %q %v", recorded, reason, err)
		}
		return errors.Join(answers(p, "Split in two.", size)(ctx, req, verified, tools), answers(p, "The design resumes; so does the plan.", fit)(ctx, req, verified, tools))
	})
	f.member(2, 1, 1, func(ctx context.Context, req agent.Request, verified *agent.Turn, tools *mcp.ClientSession) error {
		if !strings.Contains(req.Prompt, "Every member reads the same revision: spec.md revision 1 and plan.json revision 2") || !strings.Contains(req.Prompt, size) {
			p.report("round 2 prompt:\n%s", req.Prompt)
		}
		if got, err := readTool(ctx, tools, "plan.json"); err != nil || got != splitPlan {
			p.report("round 2 debates %q %v", got, err)
		}
		if got, err := readTool(ctx, tools, shed.ReplyPath(1)); err != nil || !strings.Contains(got, "Split in two.") {
			p.report("the architect's reply in the view: %q %v", got, err)
		}
		return concedes(p, size)(ctx, req, verified, tools)
	})
	f.member(2, 2, 1, concedes(p, fit))
	stream = f.handIn(t, "design", handedDesign)
	f.awaitShed(t, stream, "concluded-2")
	p.check(t)
	if moves, want := f.shedMoves(t, stream), []string{"round-1", "heard-1", "reply-1", "replied-1", "round-2", "heard-2", "concluded-2"}; !slices.Equal(moves, want) {
		t.Fatalf("shed went %v, want %v", moves, want)
	}
	// One reply, recorded with the redraft in one commit, by the architect.
	ops := f.replyOperations(t, stream)
	if len(ops) != 1 || !ops[0].Acknowledged || ops[0].Result == nil || ops[0].Result.Outcome != "succeeded" ||
		ops[0].Result.Evidence != "the architect answered 2 objections after round 1 and redrafted: spec.md revision 1 and plan.json revision 2" {
		t.Fatalf("reply operations: %+v", ops)
	}
	if in, err := decodeShed(ops[0].Operation, ReplyAction); err != nil || in != (roundInput{1, 1, 1}) {
		t.Fatalf("reply input %+v %v", in, err)
	}
	asked, told := f.transition(t, stream, "shed-reply-1"), f.transition(t, stream, "shed-reply-1-replied")
	if asked.Cause != "shed-round-1-heard" || asked.Reason != "2 objections stand after round 1; the architect is asked for its reply" || told.Cause != ops[0].Operation.ID || told.Reason != ops[0].Result.Evidence {
		t.Fatalf("reply transitions: %+v %+v", asked, told)
	}
	reply := f.reply(t, stream, 1)
	if reply.Round != 1 || reply.Revision != (shed.Pin{Spec: 1, Plan: 1}) || reply.Turn != replyTurnID(1, 1) || reply.Redraft == nil || *reply.Redraft != (shed.Pin{Spec: 1, Plan: 2}) || reply.Failure != "" || len(reply.Problems) != 0 ||
		!slices.Equal(reply.Answers, []shed.Answer{{Objection: size, Answer: "Split in two."}, {Objection: fit, Answer: "The design resumes; so does the plan."}}) {
		t.Fatalf("reply: %+v", reply)
	}
	plans := f.documents(t, stream, plan.PlanDocument)
	if len(plans) != 2 || plans[1].Content != splitPlan || plans[1].Actor != architectActor || plans[1].Cause != ops[0].Operation.ID || len(f.documents(t, stream, plan.SpecDocument)) != 1 {
		t.Fatalf("plan revisions: %+v", plans)
	}
	streamDir := "workstreams/" + string(stream) + "/"
	landed := func(path string) string {
		return demoGit(t, filepath.Dir(f.clone), "-C", f.trace, "log", "-1", "--format=%H", "--", streamDir+path)
	}
	if landed(plan.PlanPath) != landed(shed.ReplyPath(1)) {
		t.Fatal("the redraft and the reply landed in separate commits")
	}
	rounds := f.roundOperations(t, stream)
	if len(rounds) != 2 {
		t.Fatalf("round operations: %+v", rounds)
	}
	for _, op := range rounds {
		if in, err := decodeRound(op.Operation); err != nil || in != (roundInput{1, 1, 1}) && in != (roundInput{2, 1, 2}) {
			t.Fatalf("round input %+v %v", in, err)
		}
	}
	if next := f.transition(t, stream, "shed-round-2"); next.Cause != "shed-reply-1-replied" {
		t.Fatalf("round 2 request: %+v", next)
	}
	if end := f.transition(t, stream, "shed-concluded-2"); end.Reason != "debate concluded by consensus after round 2: no objection stands" || end.Cause != "shed-round-2-heard" {
		t.Fatalf("conclusion: %+v", end)
	}
	f.concluded(t, stream, 2)
	f.stillInShed(t, stream)
	if ran := f.ran(); ran[replyTurnID(1, 1)] != 1 || ran[replyTurnID(1, 2)] != 0 || ran[replyTurnID(2, 1)] != 0 {
		t.Fatalf("architect turns: %v", ran)
	}
	if open, err := Dissent(f.repository(), stream); err != nil || len(open) != 0 {
		t.Fatalf("dissent %+v %v", open, err)
	}
	root := f.opts.Config.Root
	if _, err := os.Lstat(filepath.Join(root, "architect", string(f.project), string(stream), replyTurnID(1, 1), "workspace")); !os.IsNotExist(err) {
		t.Fatal("the reply's staged workspace is retained")
	}
	if entries, err := os.ReadDir(filepath.Join(root, "views")); err != nil || len(entries) != 0 {
		t.Fatalf("views leaked: %v %v", entries, err)
	}
}

// An invalid redraft is returned to the architect with its problems and is
// never recorded, so the committee only ever debates the corrected one. The
// returned turn is part of the same reply.
func TestInvalidRedraftGoesBackToTheArchitect(t *testing.T) {
	t.Parallel()
	f := newDebateFixture(t, 1, 2)
	defer f.stop(t)
	p := &faults{}
	proof := shed.ObjectionID(1, committeeAgent(1), 1)
	f.member(1, 1, 1, objects(p, shed.Proof, "spec#2", "plan#dedupe"))
	f.script(replyTurnID(1, 1), map[string]string{plan.PlanPath: cyclicPlan}, answers(p, "A test shows it now.", proof))
	f.script(replyTurnID(1, 2), map[string]string{plan.PlanPath: splitPlan}, func(ctx context.Context, req agent.Request, _ *agent.Turn, tools *mcp.ClientSession) error {
		for _, want := range []string{"Your redraft was not accepted, and the committee will not read it:", "- " + cycleProblem, "redraft/", "Your answers so far are kept", proof} {
			if !strings.Contains(req.Prompt, want) {
				p.report("returned redraft prompt lacks %q:\n%s", want, req.Prompt)
			}
		}
		if got, err := readTool(ctx, tools, "redraft/plan.json"); err != nil || got != cyclicPlan {
			p.report("the returned redraft: %q %v", got, err)
		}
		if got, err := readTool(ctx, tools, "plan.json"); err != nil || got != validPlan {
			p.report("the recorded plan: %q %v", got, err)
		}
		if _, err := readTool(ctx, tools, "redraft/spec.md"); err == nil {
			p.report("a file the architect did not deliver is in redraft/")
		}
		return nil
	})
	f.member(2, 1, 1, func(ctx context.Context, req agent.Request, verified *agent.Turn, tools *mcp.ClientSession) error {
		if got, err := readTool(ctx, tools, "plan.json"); err != nil || got != splitPlan {
			p.report("round 2 debates %q %v", got, err)
		}
		return concedes(p, proof)(ctx, req, verified, tools)
	})
	stream := f.handIn(t, "design", handedDesign)
	f.awaitShed(t, stream, "concluded-2")
	p.check(t)
	plans := f.documents(t, stream, plan.PlanDocument)
	if len(plans) != 2 || plans[0].Content != validPlan || plans[1].Content != splitPlan {
		t.Fatalf("plan revisions: %+v", plans)
	}
	for path, content := range snapshot(t, f.trace) {
		if strings.Contains(content, `"depends_on": [\"dedupe\"]`) || strings.Contains(content, `"depends_on": ["dedupe"]`) {
			t.Fatalf("the invalid redraft reached the trace in %s", path)
		}
	}
	reply := f.reply(t, stream, 1)
	if reply.Turn != replyTurnID(1, 2) || reply.Redraft == nil || *reply.Redraft != (shed.Pin{Spec: 1, Plan: 2}) || len(reply.Problems) != 0 || !slices.Equal(reply.Answers, []shed.Answer{{Objection: proof, Answer: "A test shows it now."}}) {
		t.Fatalf("reply: %+v", reply)
	}
	if ops := f.replyOperations(t, stream); len(ops) != 1 || ops[0].Result == nil || ops[0].Result.Outcome != "succeeded" {
		t.Fatalf("reply operations: %+v", ops)
	}
	if ran := f.ran(); ran[replyTurnID(1, 1)] != 1 || ran[replyTurnID(1, 2)] != 1 || ran[replyTurnID(1, 3)] != 0 {
		t.Fatalf("architect turns: %v", ran)
	}
	records, err := shed.Records(f.repository(), stream)
	must(t, err)
	if len(records) != 2 || records[1].Revision != (shed.Pin{Spec: 1, Plan: 2}) {
		t.Fatalf("records: %+v", records)
	}
	if moves, want := f.shedMoves(t, stream), []string{"round-1", "heard-1", "reply-1", "replied-1", "round-2", "heard-2", "concluded-2"}; !slices.Equal(moves, want) {
		t.Fatalf("shed went %v, want %v", moves, want)
	}
}

// A redraft that stays invalid is given up: the reply records why, the
// revision stays, and the debate goes on without it.
func TestRedraftGivenUpLeavesTheRevision(t *testing.T) {
	t.Parallel()
	f := newDebateFixture(t, 1, 1)
	defer f.stop(t)
	p := &faults{}
	size := shed.ObjectionID(1, committeeAgent(1), 1)
	f.member(1, 1, 1, objects(p, shed.Size, "plan#resume", "spec#1"))
	for k := 1; k <= maxRedrafts; k++ {
		f.script(replyTurnID(1, k), map[string]string{plan.PlanPath: cyclicPlan}, func(_ context.Context, req agent.Request, _ *agent.Turn, _ *mcp.ClientSession) error {
			if returned := strings.Contains(req.Prompt, "Your redraft was not accepted"); returned != (k > 1) {
				p.report("reply turn %d told of a returned redraft: %v", k, returned)
			}
			return nil
		})
	}
	stream := f.handIn(t, "design", handedDesign)
	f.awaitShed(t, stream, "concluded-1")
	p.check(t)
	reply := f.reply(t, stream, 1)
	if reply.Redraft != nil || !slices.Contains(reply.Problems, cycleProblem) || reply.Failure != "" || reply.Turn != replyTurnID(1, maxRedrafts) {
		t.Fatalf("reply: %+v", reply)
	}
	if plans := f.documents(t, stream, plan.PlanDocument); len(plans) != 1 {
		t.Fatalf("plan revisions: %+v", plans)
	}
	told := f.transition(t, stream, "shed-reply-1-replied")
	if !strings.HasPrefix(told.Reason, "the architect answered 0 objections after round 1; its redraft was given up as invalid and spec.md revision 1 and plan.json revision 1 stays:\n- ") || !strings.Contains(told.Reason, cycleProblem) {
		t.Fatalf("replied reason %q", told.Reason)
	}
	if ran := f.ran(); ran[replyTurnID(1, maxRedrafts)] != 1 || ran[replyTurnID(1, maxRedrafts+1)] != 0 {
		t.Fatalf("architect turns: %v", ran)
	}
	if open, err := Dissent(f.repository(), stream); err != nil || len(open) != 1 || open[0].ID != size {
		t.Fatalf("dissent %+v %v", open, err)
	}
}

// Reaching shed.max_rounds stops the debate with its dissent open: the veto
// still blocks, the workstream stays in the shed, nothing is approved, and
// the chief of staff is told what stands.
func TestCapReachedWithAnOpenVetoApprovesNothing(t *testing.T) {
	t.Parallel()
	f := newDebateFixture(t, 2, 2)
	defer f.stop(t)
	p := &faults{}
	veto, advice := shed.ObjectionID(1, committeeAgent(1), 1), shed.ObjectionID(1, committeeAgent(2), 1)
	f.member(1, 1, 1, objects(p, shed.Charter, "spec#2", "charter#2"))
	f.member(1, 2, 1, objects(p, shed.Fit, "plan", "spec#1"))
	f.script(replyTurnID(1, 1), nil, answers(p, "Criterion 2 has a test.", veto))
	// A silent turn on the revision the veto was made against settles nothing.
	f.member(2, 1, 1, silent)
	f.member(2, 2, 1, silent)
	f.script(replyTurnID(2, 1), nil, func(_ context.Context, req agent.Request, _ *agent.Turn, _ *mcp.ClientSession) error {
		if !strings.Contains(req.Prompt, "Round 2 of the shed is heard") || !strings.Contains(req.Prompt, veto+" (charter, blocking") {
			p.report("round 2 reply prompt:\n%s", req.Prompt)
		}
		return nil
	})
	stream := f.handIn(t, "design", handedDesign)
	f.awaitShed(t, stream, "concluded-2")
	p.check(t)
	if moves, want := f.shedMoves(t, stream), []string{"round-1", "heard-1", "reply-1", "replied-1", "round-2", "heard-2", "reply-2", "replied-2", "concluded-2"}; !slices.Equal(moves, want) {
		t.Fatalf("shed went %v, want %v", moves, want)
	}
	end := f.transition(t, stream, "shed-concluded-2")
	if end.Reason != "debate stopped after round 2, at the shed.max_rounds cap of 2, with 2 objections standing, 1 of them blocking; the cap approves nothing" || end.Cause != "shed-reply-2-replied" || end.From != "replied-2" {
		t.Fatalf("conclusion: %+v", end)
	}
	notice := f.concluded(t, stream, 2).Event.Body
	for _, want := range []string{"Debate concluded: " + end.Reason + ". The workstream stays in-shed until the owner rules.", "Open dissent:",
		"- " + veto + " (charter, blocking, by " + committeeAgent(1) + " in round 1 on spec#2, against spec.md revision 1 and plan.json revision 1): It does not hold.",
		"- " + advice + " (fit, advisory, by " + committeeAgent(2) + " in round 1 on plan, against spec.md revision 1 and plan.json revision 1): It does not hold."} {
		if !strings.Contains(notice, want) {
			t.Fatalf("notice lacks %q:\n%s", want, notice)
		}
	}
	f.stillInShed(t, stream)
	open, err := Dissent(f.repository(), stream)
	must(t, err)
	if len(open) != 2 || open[0].ID != veto || open[0].Kind != shed.Charter || open[0].Member != committeeAgent(1) || open[0].Part != "spec#2" || !open[0].Blocking ||
		open[1].ID != advice || open[1].Kind != shed.Fit || open[1].Member != committeeAgent(2) || open[1].Part != "plan" || open[1].Blocking {
		t.Fatalf("dissent record: %+v", open)
	}
	// The cap is the end of automatic debate: no third round, however many
	// passes follow.
	must(t, (&debate{s: f.s, repository: f.repository()}).Pass(context.Background()))
	if ran := f.ran(); ran[roundTurnID(3, committeeAgent(1), 1)] != 0 || len(f.roundOperations(t, stream)) != 2 || len(f.replyOperations(t, stream)) != 2 || ran[replyTurnID(1, 1)] != 1 || ran[replyTurnID(2, 1)] != 1 {
		t.Fatalf("turns after the cap: %v", ran)
	}
	if moves := f.shedMoves(t, stream); len(moves) != 9 {
		t.Fatalf("a pass after the conclusion moved the shed: %v", moves)
	}
}

// A failed architect turn is the round's reply all the same: it is recorded
// with the failure and what the architect answered before it, and the debate
// goes on.
func TestFailedReplyIsRecordedAndTheDebateGoesOn(t *testing.T) {
	t.Parallel()
	f := newDebateFixture(t, 1, 1)
	defer f.stop(t)
	p := &faults{}
	size := shed.ObjectionID(1, committeeAgent(1), 1)
	f.member(1, 1, 1, objects(p, shed.Size, "plan#resume", "spec#1"))
	f.script(replyTurnID(1, 1), map[string]string{plan.PlanPath: splitPlan}, func(ctx context.Context, req agent.Request, verified *agent.Turn, tools *mcp.ClientSession) error {
		return errors.Join(answers(p, "Half an answer.", size)(ctx, req, verified, tools), errors.New("the agent crashed"))
	})
	stream := f.handIn(t, "design", handedDesign)
	f.awaitShed(t, stream, "concluded-1")
	p.check(t)
	reply := f.reply(t, stream, 1)
	if !strings.Contains(reply.Failure, "the agent crashed") || reply.Redraft != nil || len(reply.Answers) != 1 || reply.Turn != replyTurnID(1, 1) {
		t.Fatalf("reply: %+v", reply)
	}
	if told := f.transition(t, stream, "shed-reply-1-replied"); !strings.Contains(told.Reason, "its turn failed: ") || !strings.Contains(told.Reason, "the agent crashed") {
		t.Fatalf("replied reason %q", told.Reason)
	}
	if ran := f.ran(); ran[replyTurnID(1, 1)] != 1 || ran[replyTurnID(1, 2)] != 0 || len(f.documents(t, stream, plan.PlanDocument)) != 1 {
		t.Fatalf("a failed turn was retried or its redraft recorded: %v", ran)
	}
}

// A restart in the middle of the architect's reply, and one in the middle of
// the next round, resume where the trace says the debate is: no member
// contributes twice and the round gets one reply.
func TestDebateResumesMidRoundAfterARestart(t *testing.T) {
	t.Parallel()
	f := newDebateFixture(t, 2, 2)
	p := &faults{}
	size := shed.ObjectionID(1, committeeAgent(1), 1)
	f.member(1, 1, 1, objects(p, shed.Size, "plan#resume", "spec#1"))
	f.member(1, 2, 1, silent)
	replying := make(chan struct{})
	f.script(replyTurnID(1, 1), nil, func(ctx context.Context, req agent.Request, verified *agent.Turn, tools *mcp.ClientSession) error {
		// What an interrupted turn answered is not the reply.
		answers(p, "Lost.", size)(ctx, req, verified, tools)
		close(replying)
		<-ctx.Done()
		return ctx.Err()
	})
	stream := f.handIn(t, "design", handedDesign)
	wait := func(entered chan struct{}, what string) {
		t.Helper()
		select {
		case <-entered:
		case <-time.After(demoTimeout):
			t.Fatal(what + " did not start")
		}
	}
	wait(replying, "the architect's reply")
	f.stop(t)

	// The next service starts the reply's second attempt and goes on to round
	// 2, where a stop interrupts member 1 after member 2 finished.
	f.script(replyTurnID(1, 2), nil, answers(p, "Kept.", size))
	debating := make(chan struct{})
	f.member(2, 1, 1, func(ctx context.Context, _ agent.Request, _ *agent.Turn, _ *mcp.ClientSession) error {
		close(debating)
		<-ctx.Done()
		return ctx.Err()
	})
	f.member(2, 2, 1, silent)
	f.start(t)
	wait(debating, "round 2")
	deadline := time.Now().Add(demoTimeout)
	for {
		th, err := f.repository().Thread(stream, committeeAgent(2))
		must(t, err)
		if len(th.Turns) == 2 && th.Turns[1].Status() == "idle" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("member 2 did not finish round 2: %+v", th.Turns)
		}
		time.Sleep(50 * time.Millisecond)
	}
	f.stop(t)

	f.member(2, 1, 2, concedes(p, size))
	f.start(t)
	defer f.stop(t)
	f.awaitShed(t, stream, "concluded-2")
	p.check(t)
	want := map[string]int{"draft-1-1": 1, roundTurnID(1, committeeAgent(1), 1): 1, roundTurnID(1, committeeAgent(2), 1): 1, replyTurnID(1, 1): 1, replyTurnID(1, 2): 1,
		roundTurnID(2, committeeAgent(1), 1): 1, roundTurnID(2, committeeAgent(2), 1): 1, roundTurnID(2, committeeAgent(1), 2): 1}
	for turn, n := range f.ran() {
		if want[turn] != n {
			t.Fatalf("turn %s ran %d times, want %d: %v", turn, n, want[turn], f.ran())
		}
	}
	if reply := f.reply(t, stream, 1); reply.Turn != replyTurnID(1, 2) || !slices.Equal(reply.Answers, []shed.Answer{{Objection: size, Answer: "Kept."}}) {
		t.Fatalf("reply: %+v", reply)
	}
	for round := 1; round <= 2; round++ {
		for i := 1; i <= 2; i++ {
			if docs := f.documents(t, stream, shed.DocumentID(round, committeeAgent(i))); len(docs) != 1 {
				t.Fatalf("round %d records of member %d: %+v", round, i, docs)
			}
		}
	}
	if len(f.replyOperations(t, stream)) != 1 || len(f.roundOperations(t, stream)) != 2 {
		t.Fatalf("operations: %+v %+v", f.replyOperations(t, stream), f.roundOperations(t, stream))
	}
	if moves, want := f.shedMoves(t, stream), []string{"round-1", "heard-1", "reply-1", "replied-1", "round-2", "heard-2", "concluded-2"}; !slices.Equal(moves, want) {
		t.Fatalf("shed went %v, want %v", moves, want)
	}
}

// A stop between the reply's record and its transition leaves the file
// recorded: the next service records nothing again and only moves the shed,
// to replied even when the workstream was abandoned in between.
func TestReplyRecordedBeforeAStopIsNotRecordedAgain(t *testing.T) {
	t.Parallel()
	for _, crash := range []string{"recorded", "recorded-then-abandoned"} {
		t.Run(crash, func(t *testing.T) {
			f := newDebateFixture(t, 1, 1)
			ctx := context.Background()
			p := &faults{}
			size := shed.ObjectionID(1, committeeAgent(1), 1)
			f.member(1, 1, 1, objects(p, shed.Size, "plan#resume", "spec#1"))
			entered := make(chan struct{})
			f.script(replyTurnID(1, 1), nil, func(ctx context.Context, _ agent.Request, _ *agent.Turn, _ *mcp.ClientSession) error {
				close(entered)
				<-ctx.Done()
				return ctx.Err()
			})
			stream := f.handIn(t, "design", handedDesign)
			select {
			case <-entered:
			case <-time.After(demoTimeout):
				t.Fatal("the reply did not start")
			}
			f.stop(t)
			p.check(t)

			// What the stopped service got to: a second attempt ran to the end
			// and the reply is recorded, but the shed has not moved.
			cfg, err := config.Load(f.opts.Config)
			must(t, err)
			repo, err := trace.Open(cfg.Root, config.Project{ID: f.project, Clone: f.clone})
			must(t, err)
			ops, err := repo.Operations(stream)
			must(t, err)
			operation := ""
			for _, o := range ops {
				if o.Operation.Action == ReplyAction {
					operation = o.Operation.ID
				}
			}
			th, err := repo.Thread(stream, architectAgent)
			must(t, err)
			first := th.Turns[len(th.Turns)-1]
			if first.Request.TurnID != replyTurnID(1, 1) || first.Status() != "interrupted" || first.Request.Cause != operation {
				t.Fatalf("interrupted turn: %+v", first)
			}
			second := first.Request
			second.TurnID = replyTurnID(1, 2)
			second.ID, second.At = "request_"+second.TurnID, f.clock.Now()
			_, err = repo.EnqueueTurn(ctx, second)
			must(t, err)
			directory := filepath.Join(cfg.Root.String(), "architect", string(f.project), string(stream), second.TurnID)
			claimed, err := repo.ClaimTurn(ctx, stream, architectAgent, "earlier-session", filepath.Join(directory, "session"), f.clock.Now())
			must(t, err)
			h := second.Header
			h.Schema, h.ID, h.At, h.Actor = "osmia.trace.turn-response", trace.EventID(second.ID, "response"), f.clock.Now(), trace.Actor{Kind: "service", ID: "thread-runner"}
			response := trace.TurnResponse{Header: h, AgentID: architectAgent, ThreadID: second.ThreadID, TurnID: second.TurnID, RequestID: second.ID, RequestRevision: second.Revision,
				Result: coreadapter.SessionResult{Session: coreadapter.BackendSession{Backend: second.Profile.Backend, ID: "session-2"}, SessionDirectory: claimed.Claim.SessionDirectory, StartedAt: claimed.Claim.At, Duration: time.Second, FinalResponse: "Replied"}}
			must(t, repo.CaptureTurn(ctx, "earlier-session", response))
			must(t, repo.CompleteTurn(ctx, stream, architectAgent, second.TurnID, "earlier-session", f.clock.Now()))
			data, err := shed.EncodeReply(shed.Reply{Version: shed.Version, Round: 1, Revision: shed.Pin{Spec: 1, Plan: 1}, Turn: second.TurnID, Answers: []shed.Answer{{Objection: size, Answer: "Recorded."}}})
			must(t, err)
			must(t, repo.RecordDocuments(ctx, []trace.Document{{Header: trace.Header{Schema: "osmia.trace.document", Version: trace.Version, ID: shed.ReplyDocumentID(1), Revision: 1, Project: f.project, Workstream: stream, At: f.clock.Now(), Actor: architectActor, Cause: operation, Depth: 1},
				Path: shed.ReplyPath(1), Content: string(data)}}))
			if crash == "recorded-then-abandoned" {
				h := trace.Header{Schema: "osmia.trace.transition", Version: trace.Version, ID: "abandoned", Revision: 1, Project: f.project, Workstream: stream, At: f.clock.Now(), Actor: ownerActor, Cause: "owner"}
				_, err := repo.SetFeatureState(ctx, h, AbandonedState, "the owner abandoned the workstream")
				must(t, err)
			}
			must(t, repo.Close())

			f.start(t)
			defer f.stop(t)
			f.awaitShed(t, stream, "replied-1")
			if ran := f.ran(); ran[replyTurnID(1, 1)] != 1 || ran[replyTurnID(1, 2)] != 0 || ran[replyTurnID(1, 3)] != 0 {
				t.Fatalf("architect turns: %v", ran)
			}
			if docs := f.documents(t, stream, shed.ReplyDocumentID(1)); len(docs) != 1 || docs[0].Content != string(data) {
				t.Fatalf("reply documents: %+v", docs)
			}
			replies := f.replyOperations(t, stream)
			if len(replies) != 1 || replies[0].Result == nil || replies[0].Result.Outcome != "succeeded" || replies[0].Result.Evidence != "the architect answered 1 objections after round 1 and left spec.md revision 1 and plan.json revision 1 as it is" {
				t.Fatalf("operations: %+v", replies)
			}
			if crash == "recorded" {
				f.awaitShed(t, stream, "concluded-1")
				return
			}
			// An abandoned workstream's debate is not concluded for anyone.
			must(t, (&debate{s: f.s, repository: f.repository()}).Pass(ctx))
			if moves := f.shedMoves(t, stream); moves[len(moves)-1] != "replied-1" {
				t.Fatalf("shed of an abandoned workstream went %v", moves)
			}
		})
	}
}

// Abandoning the workstream cancels the architect's running reply and fails
// it without a record.
func TestAbandonFailsARunningReply(t *testing.T) {
	t.Parallel()
	f := newDebateFixture(t, 1, 2)
	defer f.stop(t)
	p := &faults{}
	f.member(1, 1, 1, objects(p, shed.Size, "plan#resume", "spec#1"))
	entered := make(chan struct{})
	f.script(replyTurnID(1, 1), nil, func(ctx context.Context, _ agent.Request, _ *agent.Turn, _ *mcp.ClientSession) error {
		close(entered)
		<-ctx.Done()
		return ctx.Err()
	})
	stream := f.handIn(t, "design", handedDesign)
	select {
	case <-entered:
	case <-time.After(demoTimeout):
		t.Fatal("the reply never started")
	}
	_, err := f.c.Abandon(context.Background(), stream, "no longer needed")
	must(t, err)
	f.awaitShed(t, stream, "failed-1", "replied-1")
	p.check(t)
	if replies, err := shed.Replies(f.repository(), stream); err != nil || len(replies) != 0 {
		t.Fatalf("replies of an abandoned workstream: %+v %v", replies, err)
	}
	ops := f.replyOperations(t, stream)
	if len(ops) != 1 || ops[0].Result == nil || ops[0].Result.Outcome != "failed" || ops[0].Result.Evidence != "the reply to round 1 failed: the workstream was abandoned, so the architect runs no turn for it" {
		t.Fatalf("operations: %+v", ops)
	}
	if failed := f.transition(t, stream, "shed-reply-1-failed"); failed.From != "reply-1" || failed.Reason != ops[0].Result.Evidence {
		t.Fatalf("failed transition: %+v", failed)
	}
	if ran := f.ran(); ran[replyTurnID(1, 2)] != 0 || ran[roundTurnID(2, committeeAgent(1), 1)] != 0 {
		t.Fatalf("turns after abandonment: %v", ran)
	}
}

// A service without an architect runner asks for no reply, and leaves one
// already requested pending without spending an attempt.
func TestReplyWaitsForAnArchitectRunner(t *testing.T) {
	t.Parallel()
	f := newDebateFixture(t, 1, 1)
	ctx := context.Background()
	p := &faults{}
	runner := f.opts.Architect
	size := shed.ObjectionID(1, committeeAgent(1), 1)
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

	// The round is heard by a service that cannot run the architect.
	f.member(1, 1, 2, objects(p, shed.Size, "plan#resume", "spec#1"))
	f.opts.Architect = nil
	f.start(t)
	f.awaitShed(t, stream, "heard-1")
	d := &debate{s: f.s, repository: f.repository()}
	must(t, d.Pass(ctx))
	if moves := f.shedMoves(t, stream); moves[len(moves)-1] != "heard-1" || len(f.replyOperations(t, stream)) != 0 {
		t.Fatalf("a reply was requested without a runner: %v", moves)
	}
	f.stop(t)

	// With a runner the reply starts; a stop interrupts it.
	replying := make(chan struct{})
	f.script(replyTurnID(1, 1), nil, func(ctx context.Context, _ agent.Request, _ *agent.Turn, _ *mcp.ClientSession) error {
		close(replying)
		<-ctx.Done()
		return ctx.Err()
	})
	f.opts.Architect = runner
	f.start(t)
	select {
	case <-replying:
	case <-time.After(demoTimeout):
		t.Fatal("the reply did not start")
	}
	f.stop(t)

	f.opts.Architect = nil
	f.start(t)
	ops := f.replyOperations(t, stream)
	if len(ops) != 1 {
		t.Fatalf("operations: %+v", ops)
	}
	r := replier{&debate{s: f.s, repository: f.repository()}}
	if observed, err := r.Inspect(ctx, ops[0].Operation); err != nil || observed.State != coreadapter.EffectAbsent {
		t.Fatalf("inspect an interrupted reply: %+v %v", observed, err)
	}
	if _, err := r.Apply(ctx, ops[0].Operation); !errors.Is(err, errNoArchitect) {
		t.Fatalf("apply without a runner: %v", err)
	}
	th, err := f.repository().Thread(stream, architectAgent)
	must(t, err)
	if turns := replyTurns(th, 1); len(turns) != 1 || turns[0].Status() != "interrupted" {
		t.Fatalf("a turn was queued without a runner: %+v", turns)
	}
	f.stop(t)

	f.script(replyTurnID(1, 2), nil, answers(p, "Split.", size))
	f.opts.Architect = runner
	f.start(t)
	defer f.stop(t)
	f.awaitShed(t, stream, "concluded-1")
	p.check(t)
	if reply := f.reply(t, stream, 1); reply.Turn != replyTurnID(1, 2) || len(reply.Answers) != 1 {
		t.Fatalf("reply: %+v", reply)
	}
}

// An architect whose every reply turn a service stop interrupts is recorded
// as failed with the count, and the debate goes on.
func TestReplyInterruptedEveryAttemptIsRecordedAsFailed(t *testing.T) {
	t.Parallel()
	f := newDebateFixture(t, 1, 1)
	defer f.stop(t)
	p := &faults{}
	f.member(1, 1, 1, objects(p, shed.Size, "plan#resume", "spec#1"))
	for k := 1; k <= maxReplyAttempts; k++ {
		f.script(replyTurnID(1, k), nil, func(context.Context, agent.Request, *agent.Turn, *mcp.ClientSession) error {
			return errors.Join(context.Canceled, errors.New("connection dropped"))
		})
	}
	stream := f.handIn(t, "design", handedDesign)
	f.awaitShed(t, stream, "concluded-1")
	p.check(t)
	if reply, want := f.reply(t, stream, 1), fmt.Sprintf("the architect's turn was interrupted %d times by service stops", maxReplyAttempts); reply.Failure != want || reply.Turn != "" || len(reply.Answers) != 0 {
		t.Fatalf("reply: %+v", reply)
	}
	if ran := f.ran(); ran[replyTurnID(1, maxReplyAttempts)] != 1 || ran[replyTurnID(1, maxReplyAttempts+1)] != 0 {
		t.Fatalf("architect turns: %v", ran)
	}
}

func TestReplyOperationInputIsValidated(t *testing.T) {
	t.Parallel()
	for name, op := range map[string]coreadapter.Operation{
		"another action":   {Boundary: coreadapter.RunnerBoundary, Action: thread.TurnAction, Input: json.RawMessage(`{"round":1,"spec":1,"plan":1}`)},
		"a round":          {Boundary: coreadapter.RunnerBoundary, Action: RoundAction, Input: json.RawMessage(`{"round":1,"spec":1,"plan":1}`)},
		"another boundary": {Boundary: "vcs", Action: ReplyAction, Input: json.RawMessage(`{"round":1,"spec":1,"plan":1}`)},
		"unknown field":    {Boundary: coreadapter.RunnerBoundary, Action: ReplyAction, Input: json.RawMessage(`{"round":1,"spec":1,"plan":1,"extra":1}`)},
		"no round":         {Boundary: coreadapter.RunnerBoundary, Action: ReplyAction, Input: json.RawMessage(`{"round":0,"spec":1,"plan":1}`)},
		"no revision":      {Boundary: coreadapter.RunnerBoundary, Action: ReplyAction, Input: json.RawMessage(`{"round":1}`)},
	} {
		if _, err := decodeShed(op, ReplyAction); err == nil {
			t.Errorf("%s: decoded", name)
		}
	}
	if _, err := decodeRound(coreadapter.Operation{Boundary: coreadapter.RunnerBoundary, Action: ReplyAction, Input: json.RawMessage(`{"round":1,"spec":1,"plan":1}`)}); err == nil {
		t.Error("a reply decoded as a round")
	}
}
