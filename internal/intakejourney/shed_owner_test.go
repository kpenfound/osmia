package intakejourney

import (
	context "context"

	intakefixture "github.com/kpenfound/osmia/internal/servicetest/intakefixture"

	fmt "fmt"
	slices "slices"
	strings "strings"
	testing "testing"
	time "time"

	agent "github.com/kpenfound/agentcore/agent"
	config "github.com/kpenfound/osmia/internal/config"
	shed "github.com/kpenfound/osmia/internal/shed"
	trace "github.com/kpenfound/osmia/internal/trace"
	mcp "github.com/modelcontextprotocol/go-sdk/mcp"
)

var held = intakefixture.Held

var awaitStart = intakefixture.AwaitStart

// ownerRecord returns the owner's objections recorded in a round.
func (f *shedFixture) ownerRecord(t *testing.T, stream config.WorkstreamID, round int) shed.Record {
	t.Helper()
	docs := f.documents(t, stream, shed.DocumentID(round, shed.OwnerMember))
	if len(docs) == 0 {
		t.Fatalf("round %d holds no record of the owner", round)
	}
	latest := docs[len(docs)-1]
	if latest.Path != shed.Path(round, shed.OwnerMember) || latest.Actor != ownerActor || latest.Cause != "owner-shed" {
		t.Fatalf("owner record %+v", latest)
	}
	record, err := shed.Parse([]byte(latest.Content))
	must(t, err)
	return record
}

// ownerMoves lists the states the workstream's owner subject went through.
func (f *shedFixture) ownerMoves(t *testing.T, stream config.WorkstreamID) []string {
	t.Helper()
	return intakefixture.OwnerMoves(t, f.transitions(t, stream))
}

func (f *shedFixture) dissent(t *testing.T, stream config.WorkstreamID) []shed.Entry {
	t.Helper()
	entries, err := Dissent(f.repository(), stream)
	must(t, err)
	return entries
}

// The owner's objection is recorded as the owner's under the round it was
// made in, stands and blocks, and the architect answers it like a member's.
func TestOwnerObjectionIsAnsweredLikeAMembers(t *testing.T) {
	t.Parallel()
	f := newDebateFixture(t, 1, 1)
	defer f.stop(t)
	ctx := context.Background()
	p := &faults{}
	started, release := make(chan struct{}), make(chan struct{})
	f.member(1, 1, 1, held(started, release))
	var answered []string
	f.script(replyTurnID(1, 1), nil, func(ctx context.Context, req agent.Request, _ *agent.Turn, tools *mcp.ClientSession) error {

		if want := "- owner-r1-1 (owner, blocking, by owner in round 1): The plan never names the retry budget.\n"; !strings.Contains(req.Prompt, want) {
			p.Report("the reply prompt lacks %q:\n%s", want, req.Prompt)
		}
		if recorded, reason, err := shedTool(ctx, tools, shed.ReplyTool, map[string]any{"objection": "owner-r1-1", "answer": "Named in unit resume."}); err != nil || !recorded {
			p.Report("answer to the owner: %q %v", reason, err)
		} else {
			answered = append(answered, "owner-r1-1")
		}
		return nil
	})
	stream := f.handIn(t, "design", handedDesign)
	awaitStart(t, started)

	out, err := f.c.ShedObject(ctx, stream, "The plan never names the retry budget.")
	must(t, err)
	if out.Objection != "owner-r1-1" || out.Round != 1 || out.Action != "objected" || out.Workstream != stream || out.Project != f.project {
		t.Fatalf("objection %+v", out)
	}
	record := f.ownerRecord(t, stream, 1)
	if len(record.Objections) != 1 || record.Objections[0].Kind != shed.Owner || record.Revision != (shed.Pin{Spec: 1, Plan: 1}) || record.Objections[0].Argument != "The plan never names the retry budget." {
		t.Fatalf("owner record %+v", record)
	}
	if moves := f.ownerMoves(t, stream); !slices.Equal(moves, []string{"objected-1"}) {
		t.Fatalf("owner subject went %v", moves)
	}
	close(release)

	f.awaitShed(t, stream, "concluded-1")
	entries := f.dissent(t, stream)
	if len(entries) != 1 || entries[0].ID != "owner-r1-1" || !entries[0].Blocking || entries[0].Member != shed.OwnerMember || entries[0].Round != 1 {
		t.Fatalf("dissent %+v", entries)
	}
	if len(answered) != 1 {
		t.Fatalf("the architect answered %v", answered)
	}
	reply := f.reply(t, stream, 1)
	if len(reply.Answers) != 1 || reply.Answers[0].Objection != "owner-r1-1" {
		t.Fatalf("reply %+v", reply)
	}

	docs := f.documents(t, stream, shed.DocumentID(1, committeeAgent(1)))
	if len(docs) != 1 {
		t.Fatalf("the round recorded %d member files", len(docs))
	}
	heard, err := shed.Parse([]byte(docs[0].Content))
	must(t, err)
	if !heard.Silent() || heard.Owned() {
		t.Fatalf("member record %+v", heard)
	}
	if reason := f.transition(t, stream, "shed-round-1-heard").Reason; !strings.Contains(reason, "1 members heard, 0 objections") {
		t.Fatalf("round 1 %q", reason)
	}
	p.Check(t)

	second, err := f.c.ShedObject(ctx, stream, "The proof is a manual check.")
	must(t, err)
	if second.Objection != "owner-r1-2" {
		t.Fatalf("second objection %+v", second)
	}
	if record := f.ownerRecord(t, stream, 1); len(record.Objections) != 2 {
		t.Fatalf("owner record %+v", record)
	}
	if got := len(f.dissent(t, stream)); got != 2 {
		t.Fatalf("dissent entries %d", got)
	}
	if _, err := f.c.ShedObject(ctx, stream, "  "); !failed(err, Validation) {
		t.Fatalf("empty argument: %v", err)
	}
}

// The owner's objection is not a committee record: a round abandoned before
// any member is heard still fails, and records nothing of the committee.
func TestAnAbandonedRoundFailsThoughTheOwnerObjected(t *testing.T) {
	t.Parallel()
	f := newDebateFixture(t, 1, 1)
	defer f.stop(t)
	ctx := context.Background()
	started := make(chan struct{})
	f.member(1, 1, 1, func(ctx context.Context, _ agent.Request, _ *agent.Turn, _ *mcp.ClientSession) error {
		close(started)
		<-ctx.Done()
		return ctx.Err()
	})
	stream := f.handIn(t, "design", handedDesign)
	awaitStart(t, started)
	if _, err := f.c.ShedObject(ctx, stream, "The plan never names the retry budget."); err != nil {
		t.Fatal(err)
	}
	if _, err := f.c.Abandon(ctx, stream, "no longer needed"); err != nil {
		t.Fatal(err)
	}
	f.awaitShed(t, stream, "failed-1", "heard-1")
	records, err := shed.Records(f.repository(), stream)
	must(t, err)
	if len(records) != 1 || !records[0].Owned() {
		t.Fatalf("records of an abandoned round: %+v", records)
	}
	if reason := f.transition(t, stream, "shed-round-1-failed").Reason; !strings.Contains(reason, "the committee is not heard") {
		t.Fatalf("failure %q", reason)
	}
	ops := f.acknowledgedRoundOperations(t, stream)
	if len(ops) != 1 || ops[0].Result == nil || ops[0].Result.Outcome != "failed" || !strings.Contains(ops[0].Result.Evidence, "the workstream was abandoned") {
		t.Fatalf("operations: %+v", ops)
	}
}

// A dismissed objection stays in the dissent record with the owner's
// disposition and no longer blocks; a sustained one blocks whatever its kind.
func TestOwnerRulingDisposesOfAnObjection(t *testing.T) {
	t.Parallel()
	f := newDebateFixture(t, 2, 1)
	defer f.stop(t)
	ctx := context.Background()
	p := &faults{}
	fit := shed.ObjectionID(1, committeeAgent(1), 1)
	veto := shed.ObjectionID(1, committeeAgent(2), 1)
	f.member(1, 1, 1, objects(p, shed.Fit, "spec#1", "spec#1"))
	f.member(1, 2, 1, objects(p, shed.Charter, "plan#resume", "charter#1"))
	f.script(replyTurnID(1, 1), nil, answers(p, "Kept as it is.", fit, veto))
	stream := f.handIn(t, "design", handedDesign)
	f.awaitShed(t, stream, "concluded-1")
	p.Check(t)

	out, err := f.c.ShedRule(ctx, stream, veto, "dismiss", "I accept the risk.")
	must(t, err)
	if out.Action != string(shed.Dismissed) || out.Objection != veto || out.Round != 1 || !strings.Contains(out.Detail, "I accept the risk.") {
		t.Fatalf("ruling %+v", out)
	}
	if _, err := f.c.ShedRule(ctx, stream, fit, "sustain", ""); err != nil {
		t.Fatal(err)
	}
	want := map[string]struct {
		blocking    bool
		disposition shed.Disposition
	}{fit: {true, shed.Sustained}, veto: {false, shed.Dismissed}}
	entries := f.dissent(t, stream)
	if len(entries) != 2 {
		t.Fatalf("dissent %+v", entries)
	}
	for _, e := range entries {
		if e.Blocking != want[e.ID].blocking || e.Disposition != want[e.ID].disposition {
			t.Fatalf("entry %+v, want %+v", e, want[e.ID])
		}
	}
	if moves := f.ownerMoves(t, stream); !slices.Equal(moves, []string{"ruled-1", "ruled-1"}) {
		t.Fatalf("owner subject went %v", moves)
	}

	if _, err := f.c.ShedRule(ctx, stream, veto, "sustain", "I changed my mind."); err != nil {
		t.Fatal(err)
	}
	docs := f.documents(t, stream, shed.RulingsDocumentID(1))
	if len(docs) != 3 || docs[2].Revision != 3 || docs[2].Path != shed.RulingsPath(1) || docs[2].Actor != ownerActor {
		t.Fatalf("rulings documents %+v", docs)
	}
	rulings, err := shed.ParseRulings([]byte(docs[2].Content))
	must(t, err)
	if len(rulings.Rulings) != 2 {
		t.Fatalf("rulings %+v", rulings)
	}
	for _, e := range f.dissent(t, stream) {
		if !e.Blocking || e.Disposition != shed.Sustained {
			t.Fatalf("entry after the second ruling %+v", e)
		}
	}
	for name, tc := range map[string]struct {
		objection, disposition string
		code                   Code
	}{
		"an objection that does not stand": {"agent_committee_1-r1-9", "dismiss", NotFound},
		"an unknown disposition":           {fit, "overrule", Validation},
		"the recorded disposition":         {fit, string(shed.Sustained), Validation},
	} {
		if _, err := f.c.ShedRule(ctx, stream, tc.objection, tc.disposition, ""); !failed(err, tc.code) {
			t.Fatalf("%s: %v", name, err)
		}
	}
}

// Dismissing the dissent that stands ends the debate where it is: the next
// step concludes instead of running another round, and says why.
func TestDismissingEveryObjectionConcludesTheDebate(t *testing.T) {
	t.Parallel()
	f := newDebateFixture(t, 1, 2)
	defer f.stop(t)
	ctx := context.Background()
	p := &faults{}
	objection := shed.ObjectionID(1, committeeAgent(1), 1)
	f.member(1, 1, 1, objects(p, shed.Size, "plan#resume", "spec#1"))
	replying, release := make(chan struct{}), make(chan struct{})
	f.script(replyTurnID(1, 1), nil, func(ctx context.Context, _ agent.Request, _ *agent.Turn, _ *mcp.ClientSession) error {
		close(replying)
		select {
		case <-release:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	})
	stream := f.handIn(t, "design", handedDesign)
	select {
	case <-replying:
	case <-time.After(demoTimeout):
		t.Fatal("the reply did not start")
	}
	if _, err := f.c.ShedRule(ctx, stream, objection, "dismiss", "The unit is small enough."); err != nil {
		t.Fatal(err)
	}
	close(release)

	f.awaitShed(t, stream, "concluded-1", "round-2")
	end := f.transition(t, stream, "shed-concluded-1")
	if end.Reason != "debate concluded after round 1: the owner disposed of every objection that stood" {
		t.Fatalf("conclusion %q", end.Reason)
	}

	entries := f.dissent(t, stream)
	if len(entries) != 1 || entries[0].ID != objection || entries[0].Blocking || entries[0].Disposition != shed.Dismissed {
		t.Fatalf("dissent %+v", entries)
	}
	line := fmt.Sprintf("\n- %s (size, dismissed, advisory, by %s in round 1 on plan#resume, against %s): It does not hold.", objection, committeeAgent(1), shed.Pin{Spec: 1, Plan: 1})
	if notice := f.concluded(t, stream, 1); !strings.Contains(notice.Event.Body, "Open dissent:"+line) {
		t.Fatalf("notice %q, want a line %q", notice.Event.Body, line)
	}
	if ran := f.ran(); ran[roundTurnID(2, committeeAgent(1), 1)] != 0 {
		t.Fatalf("round 2 ran: %v", ran)
	}
	p.Check(t)
}

// Skipping debate runs no round, in this service and in the next, and leaves
// the workstream in the shed for ratification. The skip is the recorded
// transition, not the owner subject's latest value: a ruling moves the
// subject on and does not resume the debate.
func TestOwnerSkipsDebateAcrossARestart(t *testing.T) {
	t.Parallel()
	passed, prepare := countingSchedule()
	f := newDebateFixtureWith(t, 1, 1, "", prepare)
	ctx := context.Background()
	runner := f.opts.Committee
	f.stop(t)
	f.opts.Committee = nil
	f.start(t)
	stream := f.handIn(t, "design", handedDesign)
	f.await(t, stream, sketched)

	out, err := f.c.ShedSkip(ctx, stream)
	must(t, err)
	if out.Action != skippedValue || out.Workstream != stream || !strings.Contains(out.Detail, "ratification") {
		t.Fatalf("skip %+v", out)
	}
	if _, err := f.c.ShedSkip(ctx, stream); !failed(err, Conflict) {
		t.Fatalf("skipping twice: %v", err)
	}

	if state, err := f.repository().Workflow(stream, trace.FeatureSubject); err != nil || state.Value != InShedState {
		t.Fatalf("feature %+v %v", state, err)
	}
	if threads, err := f.repository().Threads(stream); err == nil {
		for _, th := range threads {
			if th.Identity.Role == committeeRole {
				t.Fatalf("a committee thread was created: %+v", th.Identity)
			}
		}
	}

	objection := shed.ObjectionID(1, committeeAgent(1), 1)
	record := shed.Record{Version: shed.Version, Round: 1, Member: committeeAgent(1), Revision: shed.Pin{Spec: 1, Plan: 1}, Turn: roundTurnID(1, committeeAgent(1), 1),
		Objections: []shed.Objection{{ID: objection, Kind: shed.Size, Part: "plan#resume", Argument: "It does too much.", Citations: []string{"spec#1"}}}}
	content, err := shed.Encode(record)
	must(t, err)
	must(t, f.repository().RecordDocuments(ctx, []trace.Document{{Header: trace.Header{Schema: "osmia.trace.document", Version: trace.Version, ID: shed.DocumentID(1, committeeAgent(1)), Revision: 1, Project: f.project, Workstream: stream, At: f.clock.Now(), Actor: trace.Actor{Kind: "agent", ID: committeeAgent(1)}, Cause: "planted"}, Path: shed.Path(1, committeeAgent(1)), Content: string(content)}}))
	if _, err := f.c.ShedRule(ctx, stream, objection, "dismiss", "Small enough."); err != nil {
		t.Fatal(err)
	}
	f.stop(t)

	f.opts.Committee = runner
	f.start(t)
	defer f.stop(t)
	awaitPasses(t, passed, 3)
	if moves := f.shedMoves(t, stream); len(moves) != 0 || len(f.roundOperations(t, stream)) != 0 {
		t.Fatalf("a skipped debate ran %v", moves)
	}
	if ran := f.ran(); ran[roundTurnID(1, committeeAgent(1), 1)] != 0 {
		t.Fatalf("a member ran: %v", ran)
	}
	threads, err := f.repository().Threads(stream)
	must(t, err)
	if slices.ContainsFunc(threads, func(th trace.Thread) bool { return th.Identity.Role == committeeRole }) {
		t.Fatalf("a committee was created for a skipped debate: %+v", threads)
	}
	if moves, want := f.ownerMoves(t, stream), []string{skippedValue, "ruled-1"}; !slices.Equal(moves, want) {
		t.Fatalf("owner subject went %v, want %v", moves, want)
	}
	f.stillInShed(t, stream)

	if _, err := f.c.ShedObject(ctx, stream, "Too late."); !failed(err, Conflict) {
		t.Fatalf("objecting after a skip: %v", err)
	}
	if _, err := f.c.ShedMore(ctx, stream, 1); !failed(err, Conflict) {
		t.Fatalf("more after a skip: %v", err)
	}
	_, err = f.c.ShedRedraft(ctx, stream, "Split it.")
	if !failed(err, Conflict) || !strings.Contains(err.Error(), "skipped debate") {
		t.Fatalf("a redraft after a skip: %v", err)
	}
}

// Skip is refused while a round is running, and on a workstream that is not
// in the shed at all.
func TestSkipIsRefusedWhileARoundRunsAndOutsideTheShed(t *testing.T) {
	t.Parallel()
	f := newDebateFixture(t, 1, 1)
	defer f.stop(t)
	ctx := context.Background()
	started, release := make(chan struct{}), make(chan struct{})
	f.member(1, 1, 1, held(started, release))
	stream := f.handIn(t, "design", handedDesign)
	awaitStart(t, started)
	if _, err := f.c.ShedSkip(ctx, stream); !failed(err, Conflict) {
		t.Fatalf("skipping a running round: %v", err)
	}

	_, err := f.c.ShedMore(ctx, stream, 1)
	if !failed(err, Conflict) || !strings.Contains(err.Error(), "has not concluded") {
		t.Fatalf("more while round 1 runs: %v", err)
	}
	_, err = f.c.ShedRedraft(ctx, stream, "Split it.")
	if !failed(err, Conflict) || !strings.Contains(err.Error(), "has not concluded") {
		t.Fatalf("a redraft while round 1 runs: %v", err)
	}
	close(release)
	f.awaitShed(t, stream, "concluded-1")

	if _, err := f.c.Abandon(ctx, stream, "Superseded."); err != nil {
		t.Fatal(err)
	}
	for name, call := range map[string]func() error{
		"skip":     func() error { _, err := f.c.ShedSkip(ctx, stream); return err },
		"object":   func() error { _, err := f.c.ShedObject(ctx, stream, "No."); return err },
		"rule":     func() error { _, err := f.c.ShedRule(ctx, stream, "owner-r1-1", "dismiss", ""); return err },
		"overrule": func() error { _, err := f.c.ShedOverrule(ctx, stream, "owner-r1-1", ""); return err },
		"more":     func() error { _, err := f.c.ShedMore(ctx, stream, 1); return err },
		"redraft":  func() error { _, err := f.c.ShedRedraft(ctx, stream, "Split it."); return err },
		"ratify":   func() error { _, err := f.c.Ratify(ctx, stream, 1, 1); return err },
	} {
		if err := call(); !failed(err, Conflict) || !strings.Contains(err.Error(), AbandonedState) {
			t.Fatalf("%s on an abandoned workstream: %v", name, err)
		}
	}
}

// A request for further rounds survives a restart and resumes the debate from
// its conclusion, up to the round it asked for.
func TestOwnerAsksForMoreRoundsAcrossARestart(t *testing.T) {
	t.Parallel()
	f := newDebateFixture(t, 1, 1)
	ctx := context.Background()
	p := &faults{}
	objection := shed.ObjectionID(1, committeeAgent(1), 1)
	f.member(1, 1, 1, objects(p, shed.Size, "plan#resume", "spec#1"))
	f.script(replyTurnID(1, 1), nil, answers(p, "Kept as one unit.", objection))
	stream := f.handIn(t, "design", handedDesign)
	f.awaitShed(t, stream, "concluded-1")
	p.Check(t)

	for name, tc := range map[string]struct {
		rounds int
		code   Code
	}{"none": {0, Validation}, "negative": {-1, Validation}, "beyond the cap": {2, Validation}} {
		if _, err := f.c.ShedMore(ctx, stream, tc.rounds); !failed(err, tc.code) {
			t.Fatalf("%s: %v", name, err)
		}
	}

	runner := f.opts.Committee
	f.stop(t)
	f.opts.Committee = nil
	f.start(t)
	out, err := f.c.ShedMore(ctx, stream, 1)
	must(t, err)
	if out.Action != "more" || out.Rounds != 1 || out.Round != 1 {
		t.Fatalf("more %+v", out)
	}

	if _, err := f.c.ShedMore(ctx, stream, 1); err != nil {
		t.Fatal(err)
	}
	if docs := f.documents(t, stream, shed.MoreDocumentID(1)); len(docs) != 2 || docs[1].Revision != 2 || docs[1].Path != shed.MorePath(1) {
		t.Fatalf("requests %+v", docs)
	}

	repo := f.repository()
	drivePass(t, f.s, repo, func() error { return (&debate{S: f.s.intake().DebateDependencies(), Repository: repo}).Pass(ctx) })
	if moves := f.shedMoves(t, stream); moves[len(moves)-1] != "concluded-1" {
		t.Fatalf("a round ran without a committee runner: %v", moves)
	}
	f.stop(t)

	f.opts.Committee = runner
	f.script(replyTurnID(2, 1), nil, nil)
	f.member(2, 1, 1, concedes(p, objection))
	f.start(t)
	defer f.stop(t)
	f.awaitShed(t, stream, "concluded-2")
	if moves, want := f.shedMoves(t, stream), []string{"round-1", "heard-1", "reply-1", "replied-1", "concluded-1", "round-2", "heard-2", "concluded-2"}; !slices.Equal(moves, want) {
		t.Fatalf("shed went %v, want %v", moves, want)
	}
	resumed := f.transition(t, stream, "shed-round-2")
	if resumed.From != "concluded-1" || resumed.Cause != shed.MoreDocumentID(1) {
		t.Fatalf("resumed round %+v", resumed)
	}
	if entries := f.dissent(t, stream); len(entries) != 0 {
		t.Fatalf("dissent after the concession %+v", entries)
	}

	repo = f.repository()
	drivePass(t, f.s, repo, func() error { return (&debate{S: f.s.intake().DebateDependencies(), Repository: repo}).Pass(ctx) })
	if moves := f.shedMoves(t, stream); moves[len(moves)-1] != "concluded-2" {
		t.Fatalf("the debate resumed without a request: %v", moves)
	}
	p.Check(t)
}

// awaitDocument waits until the workstream records that revision of a
// document.
func (f *shedFixture) awaitDocument(t *testing.T, stream config.WorkstreamID, id string, revision int) {
	t.Helper()
	deadline := time.Now().Add(demoTimeout)
	for {
		docs := f.documents(t, stream, id)
		if len(docs) > 0 && docs[len(docs)-1].Revision >= revision {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("workstream %s records %d revisions of %s, want %d", stream, len(docs), id, revision)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

var failed = intakefixture.Failed

// Every owner action of the shed refuses a workstream the active project does
// not hold, and reports no project when none is configured.
func TestShedActionsRefuseAnUnknownWorkstream(t *testing.T) {
	t.Parallel()
	f := newDebateFixture(t, 1, 1)
	defer f.stop(t)
	ctx := context.Background()
	unknown := config.WorkstreamID("w_00000000000000000000000000000009")
	calls := map[string]func(config.WorkstreamID) error{
		"object": func(id config.WorkstreamID) error { _, err := f.c.ShedObject(ctx, id, "No."); return err },
		"rule": func(id config.WorkstreamID) error {
			_, err := f.c.ShedRule(ctx, id, "owner-r1-1", "dismiss", "")
			return err
		},
		"overrule": func(id config.WorkstreamID) error {
			_, err := f.c.ShedOverrule(ctx, id, "owner-r1-1", "")
			return err
		},
		"skip":    func(id config.WorkstreamID) error { _, err := f.c.ShedSkip(ctx, id); return err },
		"more":    func(id config.WorkstreamID) error { _, err := f.c.ShedMore(ctx, id, 1); return err },
		"redraft": func(id config.WorkstreamID) error { _, err := f.c.ShedRedraft(ctx, id, "Split it."); return err },
		"packet":  func(id config.WorkstreamID) error { _, err := f.c.Packet(ctx, id); return err },
		"ratify":  func(id config.WorkstreamID) error { _, err := f.c.Ratify(ctx, id, 1, 1); return err },
	}
	for name, call := range calls {
		if err := call(unknown); !failed(err, Validation) {
			t.Fatalf("%s on an unknown workstream: %v", name, err)
		}
	}
	stream := f.handIn(t, "design", handedDesign)
	f.awaitShed(t, stream, "concluded-1", fmt.Sprintf("failed-%d", 1))
	if _, err := f.c.RemoveProject(ctx, f.project); err != nil {
		t.Fatal(err)
	}
	for name, call := range calls {
		if err := call(stream); !failed(err, NoProject) {
			t.Fatalf("%s with no project: %v", name, err)
		}
	}
}
