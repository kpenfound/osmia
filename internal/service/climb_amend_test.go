package service

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kpenfound/osmia/internal/amendment"
	"github.com/kpenfound/osmia/internal/api"
	"github.com/kpenfound/osmia/internal/checks"
	"github.com/kpenfound/osmia/internal/climb"
	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/execution"
	"github.com/kpenfound/osmia/internal/intake"
	kindreg "github.com/kpenfound/osmia/internal/kinds"
	"github.com/kpenfound/osmia/internal/seal"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	"github.com/kpenfound/osmia/internal/servicetest/intakefixture"
	"github.com/kpenfound/osmia/internal/trace"
)

// amendClimbSpec has the manager's amendment of the climb spec presented
// and approves it as the owner, which sets the running service resealing and
// applying it.
func (c *climbFixture) amendClimbSpec(t *testing.T, stream config.WorkstreamID, amended string) {
	t.Helper()
	ctx := context.Background()
	repository := c.repository()
	sealed, sealDoc, found, err := seal.Latest(repository, stream)
	fixturekit.Must(t, err)
	if !found {
		t.Fatal("the workstream has no seal")
	}
	at := time.Now().UTC()
	manager := trace.Actor{Kind: "agent", ID: trace.Manager}
	header := func(schema, id string, by trace.Actor) trace.Header {
		return trace.Header{Schema: schema, Version: trace.Version, ID: id, Revision: 1, Project: repository.Project(), Workstream: stream, At: at, Actor: by, Cause: "fixture"}
	}
	request := trace.Amendment{Header: header("osmia.trace.amendment", "1", manager), Requester: manager, Role: trace.Manager, Thread: trace.Manager, Turn: "events_1",
		Citations: []string{"climb#metric"}, Change: "Amend the climb spec", Reason: "The owner changed the climb", Seal: sealed.Seal, SealRevision: sealDoc.Revision, SpecHash: sealed.SpecHash}
	document := func(id, path, content string) trace.Document {
		return trace.Document{Header: header("osmia.trace.document", id, intake.ArchitectActor), Path: path, Content: content}
	}
	_, err = repository.RecordDocumentsWith(ctx, []trace.Document{
		document("amendment_1_spec", "amendments/1/"+kindreg.ClimbSpecPath, amended),
		document("amendment_1_affected", "amendments/1/affected.json", `{"criteria":[],"units":[]}`),
		document("amendment-1-presented-packet", intake.AmendmentPacketPath("1"), `{"round":1}`+"\n"),
	}, trace.Transaction{Transition: trace.Transition{Header: header("osmia.trace.transition", "amendment-1-presented", intake.ShedActor), Subject: intake.AmendmentSubject("1"), To: intake.AmendmentPresented, Reason: "presented"}})
	fixturekit.Must(t, err)
	fixturekit.Must(t, repository.Append(ctx, request))
	if _, err := c.c.DecideAmendment(ctx, stream, "1", api.AmendmentDecisionRequest{Decision: intake.AmendmentApprove, Note: "Yes.", Packet: 1}); err != nil {
		t.Fatal(err)
	}
}

func (c *climbFixture) awaitAmendmentApplied(t *testing.T, stream config.WorkstreamID) {
	t.Helper()
	within(t, "the amendment to be applied", func() bool {
		applications, err := amendment.Read(c.repository(), stream)
		return err == nil && len(applications) == 1 && applications[0].Applied != nil
	})
}

func (c *climbFixture) calibratedUnder(t *testing.T, stream config.WorkstreamID, revision int) bool {
	t.Helper()
	cal, _, found, err := climb.ReadCalibration(c.repository(), stream)
	fixturekit.Must(t, err)
	return found && cal.SpecRevision == revision && c.inner(t, stream) == climb.InnerAwaitingApproval
}

func (c *climbFixture) amendedMoves(t *testing.T, stream config.WorkstreamID) int {
	t.Helper()
	transitions, err := trace.Read[trace.Transition](c.repository(), stream)
	fixturekit.Must(t, err)
	n := 0
	for _, tr := range transitions {
		if tr.Subject == climb.InnerSubject && tr.To == climb.InnerCalibrating && strings.HasPrefix(tr.ID, "climb-amended-") {
			n++
		}
	}
	return n
}

func benchOutputOf(wall, rss float64) string {
	return fmt.Sprintf(`{"schema":1,"valid":true,"reason":"ok","metrics":{"wall_ms":{"value":%v,"unit":"ms","direction":"lower"},"peak_rss_mb":{"value":%v,"unit":"MB","direction":"lower"}}}`, wall, rss)
}

func TestAnAmendmentWhileTheApprovalCardIsOpenRetiresItAndOnlyTheOwnersNewApprovalStartsClimbing(t *testing.T) {
	t.Parallel()
	engine := newEvaluatorEngine(100, 101, 100)
	always := &gateJev{confidence: 0.999, pick: func(_ string, options []string) string {
		for _, want := range []string{"approve", "ratify"} {
			if slices.Contains(options, want) {
				return want
			}
		}
		return options[0]
	}}
	c := newClimbFixture(t, engine, true, always)
	defer c.stop(t)
	ctx := context.Background()
	stream := c.ratified(t)
	c.awaitInner(t, stream, climb.InnerAwaitingApproval)
	oldCard, open := c.card(t)
	if !open || approvalOf(oldCard).SpecRevision != 1 {
		t.Fatalf("the first card %+v", oldCard)
	}

	c.amendClimbSpec(t, stream, strings.Replace(climbBuildingSpec, `"testdata/corpus"]`, `"testdata/corpus", "testdata/extra"]`, 1))
	c.awaitAmendmentApplied(t, stream)
	within(t, "the evaluator to be calibrated again under climb spec revision 2", func() bool { return c.calibratedUnder(t, stream, 2) })

	if _, err := c.c.ApproveClimb(ctx, stream, approvalOf(oldCard)); !intakefixture.Failed(err, api.Conflict) {
		t.Fatalf("answering the retired card: %v", err)
	}
	if c.inner(t, stream) != climb.InnerAwaitingApproval {
		t.Fatalf("an answer to the retired card moved the climb to %q", c.inner(t, stream))
	}
	newCard, open := c.card(t)
	if !open || approvalOf(newCard).SpecRevision != 2 || approvalOf(newCard).Commit != approvalOf(oldCard).Commit {
		t.Fatalf("the new card %+v beside the old %+v: the same commit, a new climb spec revision", newCard, oldCard)
	}
	if !strings.Contains(newCard.Question, "calibrated again") || !strings.Contains(newCard.Question, "testdata/extra") {
		t.Fatalf("the new card does not say why it was raised:\n%s", newCard.Question)
	}
	for range 3 {
		awaitGateScans(t, c.scans)
	}
	if n, err := climb.LastStarted(c.repository(), stream); err != nil || n != 0 {
		t.Fatalf("%d attempts started before the owner approved the recalibrated evaluator: %v", n, err)
	}
	if _, ok, err := climb.ReadApproval(c.repository(), stream); err != nil || ok {
		t.Fatalf("an approval is recorded without the owner: %v %v", ok, err)
	}
	if c.inner(t, stream) != climb.InnerAwaitingApproval || engine.count() != 6 {
		t.Fatalf("the climb is %q after %d evaluator calls", c.inner(t, stream), engine.count())
	}
	for _, r := range always.Requests() {
		if state, _ := r.State.(string); strings.Contains(state, "Approve the evaluator") || strings.Contains(state, "Evaluator diff") {
			t.Fatalf("Jev was asked about the evaluator approval:\n%s", state)
		}
	}

	out, err := c.c.ApproveClimb(ctx, stream, approvalOf(newCard))
	fixturekit.Must(t, err)
	approval, ok, err := climb.ReadApproval(c.repository(), stream)
	fixturekit.Must(t, err)
	if !ok || out.SpecRevision != 2 || approval.SpecRevision != 2 || approval.Epoch != 1 || approval.Approved.SpecRevision != 2 || !slices.Contains(approval.Approved.Paths, "testdata/extra") || c.inner(t, stream) != climb.InnerClimbing {
		t.Fatalf("approval %+v, response %+v, inner %s", approval, out, c.inner(t, stream))
	}
	if _, open := c.card(t); open {
		t.Fatal("the card is still open after the owner's approval")
	}
}

// holdTheSecondMeasurement scripts the evaluator of a climb that calibrates on
// wall times 100, 101 and 100, measures attempt 1 at 90 and holds attempt 2's
// measurement, which is call 5, until release is closed. Later calls take their
// replies from after.
func holdTheSecondMeasurement(engine *evaluatorEngine, after func(n int) checks.CallResult) (blocked, release chan struct{}) {
	blocked, release = make(chan struct{}), make(chan struct{})
	engine.reply = func(n int) checks.CallResult {
		switch n {
		case 1, 3:
			return checks.CallResult{Stdout: benchOutputOf(100, 120)}
		case 2:
			return checks.CallResult{Stdout: benchOutputOf(101, 120)}
		case 4:
			return checks.CallResult{Stdout: benchOutputOf(90, 120)}
		}
		return after(n)
	}
	var once atomic.Bool
	prior := engine.gate
	engine.gate = func(ctx context.Context, n int) error {
		if n == 5 && once.CompareAndSwap(false, true) {
			close(blocked)
			select {
			case <-release:
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		if prior != nil {
			return prior(ctx, n)
		}
		return nil
	}
	return blocked, release
}

func waitFor(t *testing.T, what string, ch chan struct{}) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(90 * time.Second):
		t.Fatalf("timed out waiting for %s", what)
	}
}

func TestAnAmendmentThatAddsAProtectedFixtureMidClimbEndsTheOpenAttemptOnceRecalibratesAndSurvivesRestarts(t *testing.T) {
	t.Parallel()
	engine := newEvaluatorEngine()
	blocked, release := holdTheSecondMeasurement(engine, func(n int) checks.CallResult {
		if n >= 6 && n <= 8 {
			return checks.CallResult{Stdout: benchOutputOf([]float64{90, 91, 90}[n-6], 120)}
		}
		return checks.CallResult{Stdout: benchOutputOf(80, 120)}
	})
	hold6, blocked6 := make(chan struct{}), make(chan struct{})
	var held atomic.Bool
	first := engine.gate
	engine.gate = func(ctx context.Context, n int) error {
		if err := first(ctx, n); err != nil {
			return err
		}
		if n == 6 && held.CompareAndSwap(false, true) {
			close(blocked6)
			select {
			case <-hold6:
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		return nil
	}
	c := newClimbFixtureWith(t, engine, true, nil, climbBuildingSpec)
	defer c.stop(t)
	ctx := context.Background()
	c.playAttempt(1, map[string]string{"src/a1.go": "package src\n", "testdata/extra/a.txt": "extra v1\n"}, "cache the parse")
	c.playAttempt(2, map[string]string{"src/a2.go": "package src\n"}, "batch the reads")
	c.playAttempt(3, map[string]string{"src/a3.go": "package src\n", "testdata/extra/a.txt": "tampered\n"}, "tune the fixture")
	c.playAttempt(4, map[string]string{"src/a4.go": "package src\n"}, "skip a copy")
	stream := c.ratified(t)
	c.diagnose(t, stream)
	c.awaitInner(t, stream, climb.InnerAwaitingApproval)
	firstCard, _ := c.card(t)
	_, err := c.c.ApproveClimb(ctx, stream, approvalOf(firstCard))
	fixturekit.Must(t, err)
	c.awaitAttempts(t, stream, 1)
	waitFor(t, "attempt 2's measurement", blocked)
	if open, found, err := climb.OpenAttempt(c.repository(), stream); err != nil || !found || open.Number != 2 {
		t.Fatalf("open attempt %+v %v %v", open, found, err)
	}
	kept := c.attemptsRecorded(t, stream)[0]

	amended := strings.Replace(climbBuildingSpec, `"testdata/corpus"]`, `"testdata/corpus", "testdata/extra"]`, 1)
	amended = strings.Replace(amended, `"max_attempts": 20`, `"max_attempts": 4`, 1)
	c.amendClimbSpec(t, stream, amended)
	c.awaitAmendmentApplied(t, stream)
	close(release)
	waitFor(t, "the recalibration's first run", blocked6)

	attempts := c.attemptsRecorded(t, stream)
	if len(attempts) != 2 || attempts[1].Number != 2 || attempts[1].Outcome != climb.OutcomeDiscarded || attempts[1].Cause != climb.CauseAmended || attempts[1].SpecRevision != 1 || attempts[1].Epoch != 1 {
		t.Fatalf("recorded attempts %+v", attempts)
	}
	if got := c.inner(t, stream); got != climb.InnerCalibrating {
		t.Fatalf("the climb is %q while its recalibration runs", got)
	}
	if c.git(t, "rev-parse", intake.FeatureBranch(stream)) != kept.Commit {
		t.Fatalf("the climb branch is not at the last kept commit %s", kept.Commit)
	}

	// A restart after the open attempt ended and before recalibration completed
	// calibrates again on the same commit and records nothing twice.
	c.restart(t)
	within(t, "the evaluator to be calibrated again under climb spec revision 2", func() bool { return c.calibratedUnder(t, stream, 2) })
	cal, _, _, err := climb.ReadCalibration(c.repository(), stream)
	fixturekit.Must(t, err)
	if cal.Commit != kept.Commit || cal.Calibration.Baseline != 90 || cal.Recalibrated == nil || !slices.Equal(cal.Recalibrated.Fields, []string{climb.FieldProtected}) || !slices.Contains(cal.Protected, "testdata/extra") {
		t.Fatalf("recalibration %+v: it runs on the current best commit %s", cal, kept.Commit)
	}

	// A restart after recalibration and before the new approval keeps the climb
	// waiting on the new card.
	calls := engine.count()
	c.restart(t)
	c.awaitInner(t, stream, climb.InnerAwaitingApproval)
	awaitGateScans(t, c.scans)
	if engine.count() != calls || c.amendedMoves(t, stream) != 1 || len(c.attemptsRecorded(t, stream)) != 2 {
		t.Fatalf("after the restart: %d evaluator calls (was %d), %d moves to calibrating, %d attempts", engine.count(), calls, c.amendedMoves(t, stream), len(c.attemptsRecorded(t, stream)))
	}
	if n, err := climb.LastStarted(c.repository(), stream); err != nil || n != 2 {
		t.Fatalf("an attempt started before the owner approved: %d %v", n, err)
	}
	if _, err := c.c.ApproveClimb(ctx, stream, approvalOf(firstCard)); !intakefixture.Failed(err, api.Conflict) {
		t.Fatalf("the retired card revived: %v", err)
	}
	newCard, open := c.card(t)
	if !open || approvalOf(newCard).SpecRevision != 2 {
		t.Fatalf("the new card %+v", newCard)
	}

	_, err = c.c.ApproveClimb(ctx, stream, approvalOf(newCard))
	fixturekit.Must(t, err)
	c.awaitAttempts(t, stream, 4)
	attempts = c.attemptsRecorded(t, stream)
	a3, a4 := attempts[2], attempts[3]
	if a3.Cause != climb.CauseProtected || a3.Measurement != nil || !strings.Contains(a3.Reason, "testdata/extra/a.txt") || a3.Epoch != 2 || a3.SpecRevision != 2 || a3.Best != 90 {
		t.Fatalf("attempt 3 %+v: the new fixture is refused before measurement", a3)
	}
	if a4.Outcome != climb.OutcomeKept || a4.Best != 90 || a4.Epoch != 2 || a4.Parent != kept.Commit {
		t.Fatalf("attempt 4 %+v", a4)
	}
	calls3 := engine.recorded()
	if len(calls3) != 9 {
		t.Fatalf("the evaluator ran %d times, want calibration 3, attempt 1, attempt 2, recalibration 3 and attempt 4", len(calls3))
	}
	for i, call := range calls3[5:] {
		if call.files["testdata/extra/a.txt"] != "extra v1\n" {
			t.Errorf("call %d did not run with the approved copy of the new fixture restored: %q", i+6, call.files["testdata/extra/a.txt"])
		}
	}
	if c.amendedMoves(t, stream) != 1 {
		t.Fatalf("the climb moved to calibrating %d times for one amendment", c.amendedMoves(t, stream))
	}
	th, err := c.repository().Thread(stream, execution.ClimbMasonAgent)
	fixturekit.Must(t, err)
	ids := map[string]bool{}
	for _, q := range th.Turns {
		ids[q.Request.TurnID] = true
	}
	for n := 1; n <= 4; n++ {
		if !ids[fmt.Sprintf("climb-mason-%d-1", n)] {
			t.Errorf("attempt %d has no turn on the climb's one thread: %v", n, ids)
		}
	}
}

func TestAnAmendmentThatChangesThePrimaryMetricResetsTheBestAndTheChartDrawsOnlyTheNewSeries(t *testing.T) {
	t.Parallel()
	engine := newEvaluatorEngine()
	blocked, release := holdTheSecondMeasurement(engine, func(n int) checks.CallResult {
		switch {
		case n == 5:
			return checks.CallResult{Stdout: benchOutputOf(85, 120)}
		case n >= 6 && n <= 8:
			return checks.CallResult{Stdout: benchOutputOf(90, 120)}
		case n == 9:
			return checks.CallResult{Stdout: benchOutputOf(95, 110)}
		}
		return checks.CallResult{Stdout: benchOutputOf(60, 109)}
	})
	c := newClimbFixtureWith(t, engine, true, nil, climbBuildingSpec)
	defer c.stop(t)
	ctx := context.Background()
	c.playAttempt(1, map[string]string{"src/a1.go": "package src\n"}, "cache the parse")
	c.playAttempt(2, map[string]string{"src/a2.go": "package src\n"}, "batch the reads")
	c.playAttempt(3, map[string]string{"src/a3.go": "package src\n"}, "trim allocations")
	c.playAttempt(4, map[string]string{"src/a4.go": "package src\n"}, "trim more allocations")
	stream := c.ratified(t)
	c.diagnose(t, stream)
	c.awaitInner(t, stream, climb.InnerAwaitingApproval)
	card, _ := c.card(t)
	_, err := c.c.ApproveClimb(ctx, stream, approvalOf(card))
	fixturekit.Must(t, err)
	c.awaitAttempts(t, stream, 1)
	waitFor(t, "attempt 2's measurement", blocked)

	memory := `{
  "version": 1,
  "goal": "Make the parser faster on the corpus.",
  "evaluator": "parse-bench",
  "metric": {"name": "peak_rss_mb", "direction": "lower", "min_improvement": 3},
  "guardrails": {"limits": [{"metric": "wall_ms", "direction": "lower", "max": 1000}]},
  "protected": {"fixtures": ["testdata/corpus"]},
  "stop": {"max_attempts": 4}
}`
	c.amendClimbSpec(t, stream, memory)
	c.awaitAmendmentApplied(t, stream)
	close(release)
	within(t, "the evaluator to be calibrated again under climb spec revision 2", func() bool { return c.calibratedUnder(t, stream, 2) })

	// Until the owner approves, the panel draws nothing of the old series and
	// the new baseline is the memory the new metric measured.
	progress, apiErr := c.s.climbProgress(string(stream))
	if apiErr != nil {
		t.Fatal(apiErr)
	}
	if p := progress.Progress; p.Metric != "peak_rss_mb" || p.Epoch != 2 || p.Earlier != 2 || len(p.Points) != 0 || p.Baseline == nil || *p.Baseline != 120 || p.Unit != "MB" {
		t.Fatalf("progress while the climb waits for the new approval: %+v", p)
	}
	card, open := c.card(t)
	if !open || approvalOf(card).SpecRevision != 2 {
		t.Fatalf("the new card %+v", card)
	}
	_, err = c.c.ApproveClimb(ctx, stream, approvalOf(card))
	fixturekit.Must(t, err)
	c.awaitAttempts(t, stream, 4)

	attempts := c.attemptsRecorded(t, stream)
	a1, a2, a3, a4 := attempts[0], attempts[1], attempts[2], attempts[3]
	if a1.Outcome != climb.OutcomeKept || a1.Scores.Primary != 90 || a1.Epoch != 1 || a1.SpecRevision != 1 {
		t.Fatalf("attempt 1 %+v", a1)
	}
	if a2.Cause != climb.CauseAmended || a2.Epoch != 1 {
		t.Fatalf("attempt 2 %+v", a2)
	}
	if a3.Outcome != climb.OutcomeKept || a3.Epoch != 2 || a3.SpecRevision != 2 || a3.Best != 120 || a3.Scores.Primary != 110 || a3.Change == nil || *a3.Change != -10 {
		t.Fatalf("attempt 3 %+v: it is compared with the new baseline 120, not with the 90 of the old metric", a3)
	}
	if a4.Outcome != climb.OutcomeDiscarded || a4.Cause != climb.CauseNoImprovement || a4.Best != 110 || a4.Epoch != 2 {
		t.Fatalf("attempt 4 %+v", a4)
	}
	progress, apiErr = c.s.climbProgress(string(stream))
	if apiErr != nil {
		t.Fatal(apiErr)
	}
	p := progress.Progress
	if p.Epoch != 2 || p.Earlier != 2 || len(p.Points) != 2 || p.Points[0].Attempt != 3 || p.Baseline == nil || *p.Baseline != 120 || p.Best == nil || *p.Best != 110 || p.BestAttempt != 3 || p.Budgets.Attempts.Used != 4 {
		t.Fatalf("progress %+v: the attempts of the old metric are neither drawn nor compared", p)
	}
	log, apiErr := c.s.climbAttempts(string(stream))
	if apiErr != nil || len(log.Attempts) != 4 || log.Attempts[1].SpecRevision != 1 || log.Attempts[2].SpecRevision != 2 || log.Attempts[2].Epoch != 2 {
		t.Fatalf("attempt log %+v %v: every attempt carries the revision boundary", log, apiErr)
	}
}

func TestAnAmendmentThatChangesOnlyTheGoalOrTheStopRulesAppliesInPlaceToAClimbWaitingForApproval(t *testing.T) {
	t.Parallel()
	engine := newEvaluatorEngine(100, 101, 100)
	c := newClimbFixture(t, engine, true, nil)
	defer c.stop(t)
	ctx := context.Background()
	stream := c.ratified(t)
	c.awaitInner(t, stream, climb.InnerAwaitingApproval)
	card, _ := c.card(t)

	amended := strings.Replace(climbBuildingSpec, `"max_attempts": 20`, `"max_attempts": 30`, 1)
	amended = strings.Replace(amended, "Make the parser faster on the corpus.", "Make the parser much faster on the corpus.", 1)
	c.amendClimbSpec(t, stream, amended)
	c.awaitAmendmentApplied(t, stream)
	applications, err := amendment.Read(c.repository(), stream)
	fixturekit.Must(t, err)
	if effect := applications[0].Applied.Climb; effect == nil || len(effect.Recalibrate) != 0 || !slices.Equal(effect.InPlace, []string{climb.FieldGoal, climb.FieldStop}) {
		t.Fatalf("applied %+v", applications[0].Applied)
	}
	awaitGateScans(t, c.scans)
	if c.inner(t, stream) != climb.InnerAwaitingApproval || engine.count() != 3 || c.amendedMoves(t, stream) != 0 {
		t.Fatalf("an in-place amendment moved the climb to %q after %d calls", c.inner(t, stream), engine.count())
	}
	if open, ok := c.card(t); !ok || open.Answer.Body["spec_revision"] != card.Answer.Body["spec_revision"] {
		t.Fatalf("the card changed: %+v", open)
	}
	_, err = c.c.ApproveClimb(ctx, stream, approvalOf(card))
	fixturekit.Must(t, err)
	approval, _, err := climb.ReadApproval(c.repository(), stream)
	fixturekit.Must(t, err)
	if approval.SpecRevision != 1 || approval.Epoch != 1 || c.inner(t, stream) != climb.InnerClimbing {
		t.Fatalf("approval %+v", approval)
	}
}
