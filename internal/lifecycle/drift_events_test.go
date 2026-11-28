package lifecycle_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	"github.com/kpenfound/osmia/internal/servicetest/journey"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/events"
	"github.com/kpenfound/osmia/internal/execution"
	"github.com/kpenfound/osmia/internal/intake"
	"github.com/kpenfound/osmia/internal/lifecycle"
	"github.com/kpenfound/osmia/internal/questions"
	"github.com/kpenfound/osmia/internal/service"
	"github.com/kpenfound/osmia/internal/servicetest/intakefixture"
	"github.com/kpenfound/osmia/internal/trace"
)

func upstreamMovedEvents(t *testing.T, repository *trace.Repository, stream config.WorkstreamID) []trace.OutboxEntry {
	t.Helper()
	outbox, err := repository.Outbox(stream)
	fixturekit.Must(t, err)
	outbox = slices.DeleteFunc(outbox, func(e trace.OutboxEntry) bool { return e.Event.Kind != trace.UpstreamMovedKind })
	slices.SortStableFunc(outbox, func(a, b trace.OutboxEntry) int { return a.At.Compare(b.At) })
	return outbox
}

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

func managerDeliverer(t *testing.T, f *journey.ShedFixture, repository *trace.Repository, stream config.WorkstreamID) *events.Deliverer {
	t.Helper()
	if _, err := repository.ManagerThread(stream); errors.Is(err, os.ErrNotExist) {
		fixturekit.Must(t, repository.CreateThread(context.Background(), trace.Agent{Header: trace.Header{Schema: "osmia.trace.agent", Version: trace.Version, ID: trace.Manager, Revision: 1, Project: repository.Project(), Workstream: stream, At: f.S.Now(), Actor: execution.ForemanActor, Cause: "test"}, Role: trace.Manager, ThreadID: trace.Manager}))
	} else {
		fixturekit.Must(t, err)
	}
	d, err := events.New(repository, events.Options{Now: f.S.Now, Profile: func() (coreadapter.Profile, error) {
		return coreadapter.Profile{Name: "default", Backend: "fake", Model: "test"}, nil
	}})
	fixturekit.Must(t, err)
	return d
}

func deliveredTurns(t *testing.T, repository *trace.Repository, stream config.WorkstreamID, body string) int {
	t.Helper()
	th, err := repository.ManagerThread(stream)
	fixturekit.Must(t, err)
	n := 0
	for _, q := range th.Turns {
		if strings.Contains(q.Request.Prompt, body) {
			n++
		}
	}
	return n
}

func TestConflictedDriftTellsTheManagerOnceAndItsMasonMayAmend(t *testing.T) {
	t.Parallel()
	f, stream, repository, _ := journey.NewFinalFixtureWith(t, "drift-moved", "")
	d := journey.Drifter{Foreman: &journey.Foreman{Masons: journey.NewMasonController(f.S, repository)}}
	ctx := context.Background()
	from := intakefixture.Seals(t, repository, stream)[0].Base.Commit
	_, upstream, op := journey.ConflictedDrift(t, f, d, stream)
	move := trace.UpstreamMove{Drift: 1, From: from, To: upstream}
	journey.AwaitResolution(t, f.S, d, stream, op, "its mason's resolution")
	journey.AwaitResolution(t, f.S, d, stream, op, "its mason's resolution")
	moved := upstreamMovedEvents(t, repository, stream)
	if len(moved) != 1 || moved[0].TransitionID != "drift-1-conflicted" {
		t.Fatalf("upstream moved events of a conflicted drift rebase %+v", moved)
	}
	checkMoved(t, moved[0], stream, move, "feature branch "+intake.FeatureBranch(stream)+" conflicts with upstream in CODEOWNERS")
	conflicted := moved[0].Event.Body
	fixturekit.Must(t, managerDeliverer(t, f, repository, stream).Pass(ctx))
	if n := deliveredTurns(t, repository, stream, conflicted); n != 1 {
		t.Fatalf("the conflict was delivered in %d turns", n)
	}

	threads := filepath.Join(f.S.Cfg.Root.String(), "threads", string(f.Project), string(stream), intake.DriftMasonAgent)
	turn := lifecycle.DriftResolveTurnID(1, 1)
	_, err := repository.ClaimTurn(ctx, stream, intake.DriftMasonAgent, "resolving", filepath.Join(threads, turn), f.Clock.Now())
	fixturekit.Must(t, err)
	scope := coreadapter.Scope{Project: string(f.Project), Workstream: string(stream), Thread: intake.DriftMasonAgent, Turn: turn, Role: intake.MasonRole}
	tools, err := service.TestDriftMasonTools(repository, scope, f.S.Now)
	fixturekit.Must(t, err)
	if len(tools) != 2 {
		t.Fatalf("the drift mason's question tools %+v", tools)
	}
	amend := intakefixture.AskTool(t, tools, questions.AmendTool)
	input := `{"citations":["spec#1"],"change":"Name the upstream owner in the spec","reason":"Upstream moved ownership of internal/"}`
	if got := intakefixture.Handle(t, amend, input); !strings.Contains(got, `"amendment":"1"`) {
		t.Fatalf("the drift mason's amend returned %s", got)
	}
	requests, err := trace.Read[trace.Amendment](repository, stream)
	fixturekit.Must(t, err)
	if len(requests) != 1 || requests[0].Upstream == nil || *requests[0].Upstream != move || requests[0].Unit != "" || requests[0].Requester.ID != intake.DriftMasonAgent {
		t.Fatalf("the drift mason's amendment %+v", requests)
	}
	if state, err := repository.Workflow(stream, intake.AmendmentSubject("1")); err != nil || state.Value != "filed" {
		t.Fatalf("the amendment is %+v: %v", state, err)
	}
	transitions, err := trace.Read[trace.Transition](repository, stream)
	fixturekit.Must(t, err)
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

	restart := func() {
		t.Helper()
		fixturekit.Must(t, repository.Close())
		repository, err = trace.Open(f.S.Cfg.Root, f.S.Cfg.Project)
		fixturekit.Must(t, err)
		d = journey.Drifter{Foreman: &journey.Foreman{Masons: journey.NewMasonController(f.S, repository)}}
	}
	restart()
	defer func() { repository.Close() }()
	journey.AwaitResolution(t, f.S, d, stream, op, "its mason's resolution")
	journey.AwaitResolution(t, f.S, d, stream, op, "its mason's resolution")
	recovered, err := repository.ClaimTurn(ctx, stream, intake.DriftMasonAgent, "recovering", filepath.Join(threads, "recover"), f.Clock.Now())
	fixturekit.Must(t, err)
	if recovered.Request.TurnID != intake.DriftMasonAgent+"-recover-1" {
		t.Fatalf("the recovering turn is %s", recovered.Request.TurnID)
	}
	scope.Turn = recovered.Request.TurnID
	tools, err = service.TestDriftMasonTools(repository, scope, f.S.Now)
	fixturekit.Must(t, err)
	amend = intakefixture.AskTool(t, tools, questions.AmendTool)
	if got := intakefixture.Handle(t, amend, input); !strings.Contains(got, `"amendment":"1"`) {
		t.Fatalf("the recovering turn's amend returned %s", got)
	}
	if requests, err := trace.Read[trace.Amendment](repository, stream); err != nil || len(requests) != 1 {
		t.Fatalf("amendments after recovery %+v: %v", requests, err)
	}
	if moved = upstreamMovedEvents(t, repository, stream); len(moved) != 2 {
		t.Fatalf("upstream moved events after a restart %+v", moved)
	}
	deliverer := managerDeliverer(t, f, repository, stream)
	fixturekit.Must(t, deliverer.Pass(ctx))
	if n := deliveredTurns(t, repository, stream, moved[1].Event.Body); n != 0 {
		t.Fatalf("the amendment's event was queued behind the conflict's turn in %d turns", n)
	}
	journey.CompleteDriftTurn(t, f, repository, stream, trace.Manager, nil)
	fixturekit.Must(t, deliverer.Pass(ctx))
	fixturekit.Must(t, deliverer.Pass(ctx))
	if n := deliveredTurns(t, repository, stream, conflicted); n != 1 {
		t.Fatalf("after a restart the conflict was delivered in %d turns", n)
	}
	if n := deliveredTurns(t, repository, stream, moved[1].Event.Body); n != 1 {
		t.Fatalf("the amendment's event was delivered in %d turns", n)
	}

	w := journey.ResolutionWorkspace(t, f, stream)
	fixturekit.Must(t, os.WriteFile(filepath.Join(w.Path, "CODEOWNERS"), []byte("/internal/ @upstream @feature\n"), 0600))
	journey.CompleteClaimedTurn(t, f, repository, stream, intake.DriftMasonAgent, recovered, journey.ResolvedDone("Kept both owners"))
	journey.AwaitResolution(t, f.S, d, stream, op, "review 1")
	journey.CompleteDriftTurn(t, f, repository, stream, intake.DriftReviewerAgent, journey.DriftVerdict(t, journey.ApprovedResolution))
	if result, err := journey.AttemptOperation(t, f.S, repository, stream, op, d); err != nil || result.Outcome != "succeeded" {
		t.Fatalf("the approved drift rebase %+v %v", result, err)
	}
	if again := upstreamMovedEvents(t, repository, stream); len(again) != 2 {
		t.Fatalf("upstream moved events after the drift rebase %+v", again)
	}
	if state, err := repository.Workflow(stream, intake.AmendmentSubject("1")); err != nil || state.Value != "filed" {
		t.Fatalf("the amendment is %+v after the drift rebase: %v", state, err)
	}
	states, err := repository.WorkflowStates(stream)
	fixturekit.Must(t, err)
	st, err := lifecycle.LatestDrift(repository, stream, states)
	fixturekit.Must(t, err)
	if st == nil || st.Outcome != lifecycle.DriftRebased || !reflect.DeepEqual(st.Moved, []string{conflicted, moved[1].Event.Body}) {
		t.Fatalf("status drift %+v", st)
	}
}

func TestDriftCarryReturnsApprovalsAndTheirReviewerMayAmend(t *testing.T) {
	t.Parallel()
	f, stream, repository := journey.NewApprovedFixture(t, "drift-approved")
	t.Cleanup(func() { repository.Close() })
	ctx := context.Background()
	from := intakefixture.Seals(t, repository, stream)[0].Base.Commit
	upstream := journey.AdvanceUpstream(t, f, map[string]string{"UPSTREAM.md": "upstream\n"})
	d := journey.Drifter{Foreman: &journey.Foreman{Masons: journey.NewMasonController(f.S, repository)}}
	op := journey.RequestDrift(t, d, stream)
	if _, err := d.Apply(ctx, op); err == nil || !strings.Contains(err.Error(), "awaits unit carryover") {
		t.Fatalf("the drift rebase did not carry the approved units: %v", err)
	}
	for _, unit := range []string{"resume", "dedupe"} {
		for _, o := range journey.RebaseOperations(t, repository, stream, unit) {
			journey.SettleOperation(t, f.S, repository, stream, o.Operation, journey.Rebaser(d))
		}
	}
	if result := journey.SettleOperation(t, f.S, repository, stream, op, d); result.Outcome != "succeeded" {
		t.Fatalf("the drift rebase %+v", result)
	}
	tip := journey.FeatureTip(t, f, stream)
	move := trace.UpstreamMove{Drift: 1, From: from, To: upstream}
	moved := upstreamMovedEvents(t, repository, stream)
	if len(moved) != 2 {
		t.Fatalf("upstream moved events of the carry %+v", moved)
	}
	for _, e := range moved {
		unit := strings.TrimSuffix(strings.TrimPrefix(e.TransitionID, trace.UnitSubject("")), "-checking-rebase-1")
		if e.TransitionID != trace.UnitSubject(unit)+"-checking-rebase-1" {
			t.Fatalf("an upstream moved event of %s", e.TransitionID)
		}
		checkMoved(t, e, stream, move, fmt.Sprintf("unit %s's approval no longer holds", unit), tip)
		if state, err := repository.Workflow(stream, trace.UnitSubject(unit)); err != nil || state.Value != execution.UnitChecking {
			t.Fatalf("unit %s is %+v: %v", unit, state, err)
		}
	}

	journey.RunChecks(t, f.S, repository, stream)
	r := &journey.Reviewers{Masons: journey.NewMasonController(f.S, repository)}
	agent := intake.ReviewerAgent("dedupe")
	if th, err := repository.Thread(stream, agent); err == nil && th.Active != "" {
		fixturekit.Must(t, repository.AbandonTurn(ctx, stream, agent, th.Active, f.S.Now()))
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		fixturekit.Must(t, err)
	}
	state, err := repository.Workflow(stream, trace.UnitSubject("dedupe"))
	fixturekit.Must(t, err)
	fixturekit.Must(t, r.One(ctx, stream, "dedupe", state, false))
	th, err := repository.Thread(stream, agent)
	fixturekit.Must(t, err)
	review := th.Turns[len(th.Turns)-1]
	if want := fmt.Sprintf("drift rebase 1 moved its upstream base from %s to %s", from, upstream); !strings.Contains(review.Request.Prompt, want) {
		t.Fatalf("the review prompt lacks %q:\n%s", want, review.Request.Prompt)
	}
	if !strings.Contains(review.Request.Prompt, "call "+questions.AskTool+" to raise it") || !strings.Contains(review.Request.Prompt, "citing upstream commit "+upstream) {
		t.Fatalf("the review prompt does not send upstream's change to the chief of staff:\n%s", review.Request.Prompt)
	}
	q, err := repository.ClaimTurn(ctx, stream, agent, "reviewing", filepath.Join(f.S.Cfg.Root.String(), "threads", "reviewing"), f.Clock.Now())
	fixturekit.Must(t, err)
	scope := coreadapter.Scope{Project: string(f.Project), Workstream: string(stream), Unit: "dedupe", Thread: agent, Turn: q.Request.TurnID, Role: intake.ReviewerRole}
	tools, err := questions.Tools(repository, agent, scope, f.S.Now)
	fixturekit.Must(t, err)
	if len(tools) != 1 || tools[0].Name != questions.AskTool {
		t.Fatalf("the reviewer's question tools %+v", tools)
	}

	if _, drifted, err := lifecycle.UnitDrift(repository, stream, "resume"); err != nil || !drifted {
		t.Fatalf("resume's carried candidate: %t %v", drifted, err)
	}
}

func TestDriftUnderAFinalReportTellsTheManager(t *testing.T) {
	t.Parallel()
	f, stream, repository, report := journey.DeliveryFixture(t)
	ctx := context.Background()
	from := intakefixture.Seals(t, repository, stream)[0].Base.Commit
	upstream := journey.AdvanceUpstream(t, f, map[string]string{"UPSTREAM.md": "upstream\n"})
	d := journey.Drifter{Foreman: &journey.Foreman{Masons: journey.NewMasonController(f.S, repository)}}
	op := journey.RequestDrift(t, d, stream)
	if result := journey.SettleOperation(t, f.S, repository, stream, op, d); result.Outcome != "succeeded" {
		t.Fatalf("the drift rebase %+v", result)
	}
	moved := upstreamMovedEvents(t, repository, stream)
	if len(moved) != 1 || moved[0].TransitionID != "drift-1-rebased" {
		t.Fatalf("upstream moved events %+v", moved)
	}
	checkMoved(t, moved[0], stream, trace.UpstreamMove{Drift: 1, From: from, To: upstream}, fmt.Sprintf("final review %d read the feature branch on the old upstream base", report.Review))
	reviewer := &journey.FinalReviewer{S: f.S, Repository: repository}
	if _, reason, err := reviewer.FinalGate(ctx, stream); err != nil || reason == "" {
		t.Fatalf("the final gate after the drift rebase: %q %v", reason, err)
	}

	op = journey.RequestDrift(t, d, stream)
	journey.SettleOperation(t, f.S, repository, stream, op, d)
	if again := upstreamMovedEvents(t, repository, stream); len(again) != 1 {
		t.Fatalf("upstream moved events after a second drift rebase %+v", again)
	}
}
