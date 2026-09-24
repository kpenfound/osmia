package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/events"
	"github.com/kpenfound/osmia/internal/questions"
	"github.com/kpenfound/osmia/internal/trace"
)

// upstreamMovedEvents returns the workstream's upstream moved events in the
// order they were raised.
func upstreamMovedEvents(t *testing.T, repository *trace.Repository, stream config.WorkstreamID) []trace.OutboxEntry {
	t.Helper()
	outbox, err := repository.Outbox(stream)
	must(t, err)
	outbox = slices.DeleteFunc(outbox, func(e trace.OutboxEntry) bool { return e.Event.Kind != trace.UpstreamMovedKind })
	slices.SortStableFunc(outbox, func(a, b trace.OutboxEntry) int { return a.At.Compare(b.At) })
	return outbox
}

// checkMoved fails unless an upstream moved event names the workstream, the
// drift rebase, the upstream commits and each of wants.
func checkMoved(t *testing.T, e trace.OutboxEntry, stream config.WorkstreamID, move trace.UpstreamMove, wants ...string) {
	t.Helper()
	for _, want := range append([]string{string(stream), fmt.Sprintf("drift rebase %d ", move.Drift), "from " + move.From, "to " + move.To}, wants...) {
		if !strings.Contains(e.Event.Body, want) {
			t.Fatalf("the upstream moved event of %s lacks %q: %s", e.TransitionID, want, e.Event.Body)
		}
	}
	if e.Event.ID != trace.EventID(e.TransitionID, trace.UpstreamMovedKey(move.Drift)) {
		t.Fatalf("the upstream moved event of %s has ID %s", e.TransitionID, e.Event.ID)
	}
}

// chiefDeliverer returns an event deliverer over the trace that delivers at
// once, creating the workstream's chief-of-staff thread when it is missing.
func chiefDeliverer(t *testing.T, f *shedFixture, repository *trace.Repository, stream config.WorkstreamID) *events.Deliverer {
	t.Helper()
	if _, err := repository.ChiefOfStaffThread(stream); errors.Is(err, os.ErrNotExist) {
		must(t, repository.CreateThread(context.Background(), trace.Agent{Header: trace.Header{Schema: "osmia.trace.agent", Version: trace.Version, ID: trace.ChiefOfStaff, Revision: 1, Project: repository.Project(), Workstream: stream, At: f.s.now(), Actor: foremanActor, Cause: "test"}, Role: trace.ChiefOfStaff, ThreadID: trace.ChiefOfStaff}))
	} else {
		must(t, err)
	}
	d, err := events.New(repository, events.Options{Now: f.s.now, Profile: func() (coreadapter.Profile, error) {
		return coreadapter.Profile{Name: "default", Backend: "fake", Model: "test"}, nil
	}})
	must(t, err)
	return d
}

// deliveredTurns counts the chief-of-staff turns whose prompt carries body.
func deliveredTurns(t *testing.T, repository *trace.Repository, stream config.WorkstreamID, body string) int {
	t.Helper()
	th, err := repository.ChiefOfStaffThread(stream)
	must(t, err)
	n := 0
	for _, q := range th.Turns {
		if strings.Contains(q.Request.Prompt, body) {
			n++
		}
	}
	return n
}

// handle runs a tool's handler with input and returns its result.
func handle(t *testing.T, tool coreadapter.Tool, input string) string {
	t.Helper()
	out, err := tool.Handle(context.Background(), json.RawMessage(input))
	must(t, err)
	return string(out)
}

// A drift rebase whose feature branch conflicts with upstream tells the
// chief of staff once, with the conflict: one upstream moved event, raised
// with drift-<k>-conflicted, delivered in one chief-of-staff turn that a
// restart does not deliver again. The drift mason resolving the conflict may
// file an amendment that cites the upstream commit: it raises a second
// event, parks nothing and waits for the owner, and the turn that recovers
// the mason's interrupted turn gets the request already filed. The approved
// resolution raises no event of its own, and status lists both.
func TestConflictedDriftTellsTheChiefOnceAndItsMasonMayAmend(t *testing.T) {
	t.Parallel()
	f, stream, repository, _ := newFinalFixture(t, "drift-moved")
	d := drifter{&foreman{masons: newMasonController(f.s, repository)}}
	ctx := context.Background()
	from := seals(t, repository, stream)[0].Base.Commit
	_, upstream, op := conflictedDrift(t, f, d, stream)
	move := trace.UpstreamMove{Drift: 1, From: from, To: upstream}
	awaitResolution(t, f.s, d, stream, op, "its mason's resolution")
	awaitResolution(t, f.s, d, stream, op, "its mason's resolution")
	moved := upstreamMovedEvents(t, repository, stream)
	if len(moved) != 1 || moved[0].TransitionID != "drift-1-conflicted" {
		t.Fatalf("upstream moved events of a conflicted drift rebase %+v", moved)
	}
	checkMoved(t, moved[0], stream, move, "feature branch "+featureBranch(stream)+" conflicts with upstream in CODEOWNERS")
	conflicted := moved[0].Event.Body
	must(t, chiefDeliverer(t, f, repository, stream).Pass(ctx))
	if n := deliveredTurns(t, repository, stream, conflicted); n != 1 {
		t.Fatalf("the conflict was delivered in %d turns", n)
	}

	// The drift mason files an amendment from its resolve turn.
	threads := filepath.Join(f.s.cfg.Root.String(), "threads", string(f.project), string(stream), driftMasonAgent)
	turn := driftResolveTurnID(1, 1)
	_, err := repository.ClaimTurn(ctx, stream, driftMasonAgent, "resolving", filepath.Join(threads, turn), f.clock.Now())
	must(t, err)
	scope := coreadapter.Scope{Project: string(f.project), Workstream: string(stream), Thread: driftMasonAgent, Turn: turn, Role: masonRole}
	tools, err := driftMasonTools(repository, scope, f.s.now)
	must(t, err)
	if len(tools) != 1 || tools[0].Name != questions.AmendTool {
		t.Fatalf("the drift mason's question tools %+v", tools)
	}
	input := `{"citations":["spec#1"],"change":"Name the upstream owner in the spec","reason":"Upstream moved ownership of internal/"}`
	if got := handle(t, tools[0], input); !strings.Contains(got, `"amendment":"1"`) {
		t.Fatalf("the drift mason's amend returned %s", got)
	}
	requests, err := trace.Read[trace.Amendment](repository, stream)
	must(t, err)
	if len(requests) != 1 || requests[0].Upstream == nil || *requests[0].Upstream != move || requests[0].Unit != "" || requests[0].Requester.ID != driftMasonAgent {
		t.Fatalf("the drift mason's amendment %+v", requests)
	}
	if state, err := repository.Workflow(stream, amendmentSubject("1")); err != nil || state.Value != "filed" {
		t.Fatalf("the amendment is %+v: %v", state, err)
	}
	transitions, err := trace.Read[trace.Transition](repository, stream)
	must(t, err)
	for _, tr := range transitions {
		if tr.Cause == "amendment_1_filed" {
			t.Fatalf("the drift mason's amendment parked %s", tr.Subject)
		}
	}
	moved = upstreamMovedEvents(t, repository, stream)
	if len(moved) != 2 || moved[1].TransitionID != "amendment_1_filed" {
		t.Fatalf("upstream moved events after the amendment %+v", moved)
	}
	checkMoved(t, moved[1], stream, move, "amendment 1 was filed by the mason", "spec#1")

	// A restart interrupts the turn; its recovery files nothing new, and
	// nothing is delivered twice.
	restart := func() {
		t.Helper()
		must(t, repository.Close())
		repository, err = trace.Open(f.s.cfg.Root, f.s.cfg.Project)
		must(t, err)
		d = drifter{&foreman{masons: newMasonController(f.s, repository)}}
	}
	restart()
	defer func() { repository.Close() }()
	awaitResolution(t, f.s, d, stream, op, "its mason's resolution")
	awaitResolution(t, f.s, d, stream, op, "its mason's resolution")
	recovered, err := repository.ClaimTurn(ctx, stream, driftMasonAgent, "recovering", filepath.Join(threads, "recover"), f.clock.Now())
	must(t, err)
	if recovered.Request.TurnID != driftMasonAgent+"-recover-1" {
		t.Fatalf("the recovering turn is %s", recovered.Request.TurnID)
	}
	scope.Turn = recovered.Request.TurnID
	tools, err = driftMasonTools(repository, scope, f.s.now)
	must(t, err)
	if got := handle(t, tools[0], input); !strings.Contains(got, `"amendment":"1"`) {
		t.Fatalf("the recovering turn's amend returned %s", got)
	}
	if requests, err := trace.Read[trace.Amendment](repository, stream); err != nil || len(requests) != 1 {
		t.Fatalf("amendments after recovery %+v: %v", requests, err)
	}
	if moved = upstreamMovedEvents(t, repository, stream); len(moved) != 2 {
		t.Fatalf("upstream moved events after a restart %+v", moved)
	}
	deliverer := chiefDeliverer(t, f, repository, stream)
	must(t, deliverer.Pass(ctx))
	must(t, deliverer.Pass(ctx))
	if n := deliveredTurns(t, repository, stream, conflicted); n != 1 {
		t.Fatalf("after a restart the conflict was delivered in %d turns", n)
	}
	if n := deliveredTurns(t, repository, stream, moved[1].Event.Body); n != 1 {
		t.Fatalf("the amendment's event was delivered in %d turns", n)
	}

	// The approved resolution moves the branch and raises nothing more.
	w := resolutionWorkspace(t, f, stream)
	must(t, os.WriteFile(filepath.Join(w.Path, "CODEOWNERS"), []byte("/internal/ @upstream @feature\n"), 0600))
	completeClaimedTurn(t, f, repository, stream, driftMasonAgent, recovered, resolvedDone("Kept both owners"))
	awaitResolution(t, f.s, d, stream, op, "review 1")
	completeDriftTurn(t, f, repository, stream, driftReviewerAgent, driftVerdict(t, approvedResolution))
	if result, err := attemptOperation(t, f.s, repository, stream, op, d); err != nil || result.Outcome != "succeeded" {
		t.Fatalf("the approved drift rebase %+v %v", result, err)
	}
	if again := upstreamMovedEvents(t, repository, stream); len(again) != 2 {
		t.Fatalf("upstream moved events after the drift rebase %+v", again)
	}
	if state, err := repository.Workflow(stream, amendmentSubject("1")); err != nil || state.Value != "filed" {
		t.Fatalf("the amendment is %+v after the drift rebase: %v", state, err)
	}
	states, err := repository.WorkflowStates(stream)
	must(t, err)
	st, err := latestDrift(repository, stream, states)
	must(t, err)
	if st == nil || st.Outcome != driftRebased || !reflect.DeepEqual(st.Moved, []string{conflicted, moved[1].Event.Body}) {
		t.Fatalf("status drift %+v", st)
	}
}

// A drift rebase that carries approved units onto the rebased feature
// branch sends their approvals back to review and tells the chief of staff
// with each: one upstream moved event per unit, raised with its return to
// review. The unit's reviewer then holds an amend that cites the upstream
// commit, is told so in its review prompt, and its request parks the unit
// for the owner once, however often it is repeated.
func TestDriftCarryReturnsApprovalsAndTheirReviewerMayAmend(t *testing.T) {
	t.Parallel()
	f, stream, repository := newApprovedFixture(t, "drift-approved")
	t.Cleanup(func() { repository.Close() })
	ctx := context.Background()
	from := seals(t, repository, stream)[0].Base.Commit
	upstream := advanceUpstream(t, f, map[string]string{"UPSTREAM.md": "upstream\n"})
	d := drifter{&foreman{masons: newMasonController(f.s, repository)}}
	op := requestDrift(t, d, stream)
	if _, err := d.Apply(ctx, op); err == nil || !strings.Contains(err.Error(), "awaits unit carryover") {
		t.Fatalf("the drift rebase did not carry the approved units: %v", err)
	}
	for _, unit := range []string{"resume", "dedupe"} {
		for _, o := range rebaseOperations(t, repository, stream, unit) {
			settleOperation(t, f.s, repository, stream, o.Operation, rebaser{d.foreman})
		}
	}
	if result := settleOperation(t, f.s, repository, stream, op, d); result.Outcome != "succeeded" {
		t.Fatalf("the drift rebase %+v", result)
	}
	tip := featureTip(t, f, stream)
	move := trace.UpstreamMove{Drift: 1, From: from, To: upstream}
	moved := upstreamMovedEvents(t, repository, stream)
	if len(moved) != 2 {
		t.Fatalf("upstream moved events of the carry %+v", moved)
	}
	for _, e := range moved {
		unit := strings.TrimSuffix(strings.TrimPrefix(e.TransitionID, trace.UnitSubject("")), "-reviewing-rebase-1")
		if e.TransitionID != trace.UnitSubject(unit)+"-reviewing-rebase-1" {
			t.Fatalf("an upstream moved event of %s", e.TransitionID)
		}
		checkMoved(t, e, stream, move, fmt.Sprintf("unit %s's approval no longer holds", unit), tip)
		if state, err := repository.Workflow(stream, trace.UnitSubject(unit)); err != nil || state.Value != UnitReviewing {
			t.Fatalf("unit %s is %+v: %v", unit, state, err)
		}
	}

	// dedupe's reviewer reads the carried candidate.
	r := &reviewers{masons: newMasonController(f.s, repository)}
	agent := reviewerAgent("dedupe")
	if th, err := repository.Thread(stream, agent); err == nil && th.Active != "" {
		must(t, repository.AbandonTurn(ctx, stream, agent, th.Active, f.s.now()))
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		must(t, err)
	}
	state, err := repository.Workflow(stream, trace.UnitSubject("dedupe"))
	must(t, err)
	must(t, r.one(ctx, stream, "dedupe", state, false))
	th, err := repository.Thread(stream, agent)
	must(t, err)
	review := th.Turns[len(th.Turns)-1]
	if want := fmt.Sprintf("drift rebase 1 moved its upstream base from %s to %s", from, upstream); !strings.Contains(review.Request.Prompt, want) {
		t.Fatalf("the review prompt lacks %q:\n%s", want, review.Request.Prompt)
	}
	claim := func(token string) coreadapter.Scope {
		t.Helper()
		q, err := repository.ClaimTurn(ctx, stream, agent, token, filepath.Join(f.s.cfg.Root.String(), "threads", token), f.clock.Now())
		must(t, err)
		return coreadapter.Scope{Project: string(f.project), Workstream: string(stream), Unit: "dedupe", Thread: agent, Turn: q.Request.TurnID, Role: reviewerRole}
	}
	scope := claim("reviewing")
	tools, err := reviewerQuestionTools(repository, scope, f.s.now)
	must(t, err)
	i := slices.IndexFunc(tools, func(tool coreadapter.Tool) bool { return tool.Name == questions.AmendTool })
	if i < 0 || !strings.Contains(tools[i].Description, upstream) {
		t.Fatalf("the reviewer's question tools %+v", tools)
	}
	input := `{"citations":["spec#2"],"change":"Skip chunks upstream now deduplicates","reason":"Upstream deduplicates chunks itself"}`
	if got := handle(t, tools[i], input); !strings.Contains(got, `"amendment":"1"`) {
		t.Fatalf("the reviewer's amend returned %s", got)
	}
	if state, err := repository.Workflow(stream, trace.UnitSubject("dedupe")); err != nil || state.Value != UnitWaiting {
		t.Fatalf("dedupe is %+v after its reviewer's amendment: %v", state, err)
	}
	moved = upstreamMovedEvents(t, repository, stream)
	if len(moved) != 3 || moved[2].TransitionID != "amendment_1_filed" {
		t.Fatalf("upstream moved events after the amendment %+v", moved)
	}
	checkMoved(t, moved[2], stream, move, "amendment 1 was filed by the reviewer")

	// A restart interrupts the review; the turn that recovers it files
	// nothing new.
	must(t, repository.Close())
	repository, err = trace.Open(f.s.cfg.Root, f.s.cfg.Project)
	must(t, err)
	must(t, repository.AbandonTurn(ctx, stream, agent, scope.Turn, f.s.now()))
	recover := review.Request
	recover.TurnID = scope.Turn + "-recover-1"
	recover.ID, recover.At = "request_"+recover.TurnID, f.s.now()
	_, err = repository.EnqueueTurn(ctx, recover)
	must(t, err)
	scope = claim("recovering")
	tools, err = reviewerQuestionTools(repository, scope, f.s.now)
	must(t, err)
	i = slices.IndexFunc(tools, func(tool coreadapter.Tool) bool { return tool.Name == questions.AmendTool })
	if got := handle(t, tools[i], input); !strings.Contains(got, `"amendment":"1"`) {
		t.Fatalf("the recovering review's amend returned %s", got)
	}
	requests, err := trace.Read[trace.Amendment](repository, stream)
	must(t, err)
	if len(requests) != 1 || requests[0].Upstream == nil || *requests[0].Upstream != move || requests[0].Unit != "dedupe" {
		t.Fatalf("amendments after recovery %+v", requests)
	}
	if state, err := repository.Workflow(stream, amendmentSubject("1")); err != nil || state.Value != "filed" {
		t.Fatalf("the amendment is %+v: %v", state, err)
	}
	if n := len(upstreamMovedEvents(t, repository, stream)); n != 3 {
		t.Fatalf("%d upstream moved events after recovery", n)
	}

	// resume's reviewer reads a carried candidate too.
	if _, drifted, err := unitDrift(repository, stream, "resume"); err != nil || !drifted {
		t.Fatalf("resume's carried candidate: %t %v", drifted, err)
	}
}

// A drift rebase that moves the seal's base under an assembled workstream's
// completed final report tells the chief of staff the report no longer
// authorises delivery, with the move.
func TestDriftUnderAFinalReportTellsTheChief(t *testing.T) {
	t.Parallel()
	f, stream, repository, report := deliveryFixture(t)
	ctx := context.Background()
	from := seals(t, repository, stream)[0].Base.Commit
	upstream := advanceUpstream(t, f, map[string]string{"UPSTREAM.md": "upstream\n"})
	d := drifter{&foreman{masons: newMasonController(f.s, repository)}}
	op := requestDrift(t, d, stream)
	if result := settleOperation(t, f.s, repository, stream, op, d); result.Outcome != "succeeded" {
		t.Fatalf("the drift rebase %+v", result)
	}
	moved := upstreamMovedEvents(t, repository, stream)
	if len(moved) != 1 || moved[0].TransitionID != "drift-1-rebased" {
		t.Fatalf("upstream moved events %+v", moved)
	}
	checkMoved(t, moved[0], stream, trace.UpstreamMove{Drift: 1, From: from, To: upstream}, fmt.Sprintf("final review %d read the feature branch on the old upstream base", report.Review))
	reviewer := &finalReviewer{s: f.s, repository: repository}
	if _, reason, err := reviewer.finalGate(ctx, stream); err != nil || reason == "" {
		t.Fatalf("the final gate after the drift rebase: %q %v", reason, err)
	}

	// Another drift rebase moves nothing a report reads, and says nothing.
	op = requestDrift(t, d, stream)
	settleOperation(t, f.s, repository, stream, op, d)
	if again := upstreamMovedEvents(t, repository, stream); len(again) != 1 {
		t.Fatalf("upstream moved events after a second drift rebase %+v", again)
	}
}
