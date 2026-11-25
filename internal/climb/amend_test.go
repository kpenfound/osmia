package climb

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	"github.com/kpenfound/osmia/internal/trace"
)

func TestCompareClassifiesEveryChangedFieldAsCalibrationDependentOrInPlace(t *testing.T) {
	t.Parallel()
	limit := func(metric string, dir Direction, max float64) []SecondaryLimit {
		return []SecondaryLimit{{Metric: metric, Direction: dir, Max: &max}}
	}
	cases := []struct {
		name      string
		change    func(*Spec)
		calibrate []string
		inPlace   []string
	}{
		{"nothing", func(*Spec) {}, nil, nil},
		{"the evaluator function", func(s *Spec) { s.Evaluator = "measure-cpu" }, []string{FieldEvaluator}, nil},
		{"the primary metric name", func(s *Spec) { s.Metric.Name = "cpu_ms" }, []string{FieldMetricName}, nil},
		{"the direction", func(s *Spec) { s.Metric.Direction = Higher }, []string{FieldMetricDir}, nil},
		{"the minimum improvement", func(s *Spec) { s.Metric.MinImprovement = 9 }, []string{FieldMinImprovement}, nil},
		{"the repeats", func(s *Spec) { s.Repeats = 3 }, []string{FieldRepeats}, nil},
		{"a secondary metric", func(s *Spec) { s.Guardrails.Limits = limit("alloc_mb", Lower, 5) }, []string{FieldSecondary}, nil},
		{"the direction of a secondary metric", func(s *Spec) { s.Guardrails.Limits = limit("peak_rss_mb", Higher, 200) }, []string{FieldSecondary}, nil},
		{"a protected fixture", func(s *Spec) { s.Protected.Fixtures = []string{"fixtures", "testdata/extra"} }, []string{FieldProtected}, nil},
		{"an added protected path", func(s *Spec) { s.Protected.Add = []string{"scripts"} }, []string{FieldProtected}, nil},
		{"a fixture moved to the additions", func(s *Spec) { s.Protected.Fixtures, s.Protected.Add = nil, []string{"./fixtures"} }, nil, nil},
		{"the goal", func(s *Spec) { s.Goal = "Make the parser much faster." }, nil, []string{FieldGoal}},
		{"the stop rules", func(s *Spec) { s.Stop.MaxAttempts = 40; s.Stop.Plateau = 5 }, nil, []string{FieldStop}},
		{"the guardrail checks", func(s *Spec) { s.Guardrails.Checks = []string{"go:test"} }, nil, []string{FieldChecks}},
		{"the bound of a limit", func(s *Spec) { s.Guardrails.Limits = limit("peak_rss_mb", Lower, 150) }, nil, []string{FieldBounds}},
		{"both kinds at once", func(s *Spec) { s.Metric.MinImprovement = 9; s.Goal = "Else." }, []string{FieldMinImprovement}, []string{FieldGoal}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			old := climbSpec()
			next := climbSpec()
			tc.change(&next)
			got := Compare(old, next)
			if !slices.Equal(got.Calibration, tc.calibrate) || !slices.Equal(got.InPlace, tc.inPlace) {
				t.Fatalf("Compare = %+v, want calibration %v in place %v", got, tc.calibrate, tc.inPlace)
			}
			if got.Recalibrates() != (len(tc.calibrate) > 0) || got.None() != (len(tc.calibrate)+len(tc.inPlace) == 0) {
				t.Fatalf("Recalibrates/None disagree with %+v", got)
			}
		})
	}
}

func TestDriftReadsTheOlderRevisionOnlyWhenTheRecordWasMadeUnderAnother(t *testing.T) {
	t.Parallel()
	old, current := climbSpec(), climbSpec()
	current.Metric.Name = "cpu_ms"
	read := func(_ config.WorkstreamID, revision int) (Spec, error) {
		if revision != 1 {
			return Spec{}, fmt.Errorf("no revision %d", revision)
		}
		return old, nil
	}
	if change, err := Drift(nil, fixturekit.Stream, 2, current, 2); err != nil || !change.None() {
		t.Fatalf("a record made under the sealed revision drifted: %+v %v", change, err)
	}
	if change, err := Drift(nil, fixturekit.Stream, 0, current, 2); err != nil || !change.None() {
		t.Fatalf("a record that pins no revision drifted: %+v %v", change, err)
	}
	if _, err := Drift(nil, fixturekit.Stream, 1, current, 2); !errors.Is(err, ErrNoSpecReader) {
		t.Fatalf("a drift with no reader: %v", err)
	}
	if change, err := Drift(read, fixturekit.Stream, 1, current, 2); err != nil || !slices.Equal(change.Calibration, []string{FieldMetricName}) {
		t.Fatalf("drift %+v %v", change, err)
	}
	if _, err := Drift(read, fixturekit.Stream, 7, current, 8); err == nil {
		t.Fatal("an unreadable revision was ignored")
	}
}

func (r *machineRig) approveAt(commit string, revision int) (ApprovalRecord, error) {
	return r.machine.Approve(context.Background(), fixturekit.Stream, commit, revision, fixturekit.OwnerActor)
}

func (r *machineRig) calibrated() {
	r.t.Helper()
	r.reconcile()
	r.reconcile()
	if got := r.inner(); got != InnerAwaitingApproval {
		r.t.Fatalf("inner state %q, want awaiting-approval", got)
	}
}

func addFixture(path string) func(*Spec) {
	return func(s *Spec) { s.Protected.Fixtures = append(slices.Clone(s.Protected.Fixtures), path) }
}

func TestAnAmendmentThatChangesTheCalibrationRetiresTheOpenCardAndRequiresAFreshOwnerApproval(t *testing.T) {
	t.Parallel()
	r := newMachineRig(t, true, scored(100), scored(101), scored(100))
	r.calibrated()
	first, _, _, err := ReadCalibration(r.repo, fixturekit.Stream)
	fixturekit.Must(t, err)
	if first.SpecRevision != 1 || first.Recalibrated != nil {
		t.Fatalf("the first calibration %+v", first)
	}

	r.amend(addFixture("testdata/extra"))
	r.reply(scored(90), scored(91), scored(92))
	r.reconcile()
	if got := r.inner(); got != InnerCalibrating || r.calls() != 0 {
		t.Fatalf("the amendment left the climb %q after %d calls, want calibrating before any run", got, r.calls())
	}
	if _, err := r.approveAt(first.Commit, 1); !errors.Is(err, ErrNotAwaiting) {
		t.Fatalf("answering the retired card: %v", err)
	}
	if _, ok, err := ReadApproval(r.repo, fixturekit.Stream); err != nil || ok {
		t.Fatalf("an approval was recorded: %v %v", ok, err)
	}

	r.reconcile()
	if got := r.inner(); got != InnerAwaitingApproval || r.calls() != 3 {
		t.Fatalf("recalibration left %q after %d calls", got, r.calls())
	}
	second, _, _, err := ReadCalibration(r.repo, fixturekit.Stream)
	fixturekit.Must(t, err)
	if second.SpecRevision != 2 || second.Recalibrated == nil || second.Recalibrated.From != 1 || !slices.Equal(second.Recalibrated.Fields, []string{FieldProtected}) ||
		!slices.Contains(second.Protected, "testdata/extra") || second.Calibration.Baseline != 91 || second.Commit != first.Commit {
		t.Fatalf("the recalibration %+v", second)
	}
	if _, err := r.approveAt(first.Commit, 1); !errors.Is(err, ErrStaleApproval) {
		t.Fatalf("answering the retired card after the new calibration: %v", err)
	}
	if got := r.inner(); got != InnerAwaitingApproval {
		t.Fatalf("a refused answer moved the climb to %q", got)
	}
	rec, err := r.approveAt(second.Commit, 2)
	fixturekit.Must(t, err)
	if rec.SpecRevision != 2 || rec.Epoch != 1 || rec.Approved.SpecRevision != 2 || rec.Baseline != 91 || !slices.Contains(rec.Approved.Paths, "testdata/extra") || r.inner() != InnerClimbing {
		t.Fatalf("approval %+v, inner %s", rec, r.inner())
	}
	notices := r.notices(trace.NoticeKind)
	if len(notices) != 1 || !strings.Contains(notices[0], "revision 2") || !strings.Contains(notices[0], FieldProtected) {
		t.Fatalf("notices %v", notices)
	}
}

func TestAnAmendmentThatDoesNotDependOnCalibrationKeepsTheOpenCardAndTheApproval(t *testing.T) {
	t.Parallel()
	r := newMachineRig(t, true, scored(100), scored(101), scored(100))
	r.calibrated()
	r.amend(func(s *Spec) {
		s.Goal = "Make the parser faster, and say why."
		s.Stop.MaxAttempts = 30
		s.Stop.Plateau = 6
	})
	r.reconcile()
	if got := r.inner(); got != InnerAwaitingApproval || r.calls() != 3 {
		t.Fatalf("an in-place amendment moved the climb to %q after %d calls", got, r.calls())
	}
	rec, err := r.approveAt("base0001", 1)
	fixturekit.Must(t, err)
	if rec.SpecRevision != 1 || r.inner() != InnerClimbing {
		t.Fatalf("approval %+v, inner %s", rec, r.inner())
	}
	r.amend(func(s *Spec) { s.Stop.MaxAttempts = 50 })
	r.reconcile()
	if r.inner() != InnerClimbing || r.calls() != 3 {
		t.Fatalf("an in-place amendment of a climbing climb moved it to %q after %d calls", r.inner(), r.calls())
	}
}

func (r *machineRig) openAttempt(n int, phase Phase) {
	r.t.Helper()
	ctx := context.Background()
	j := Journal{Number: n, Parent: "base0001", Unit: AttemptUnit(n), Phase: phase, SpecRevision: 1, Epoch: 1, StartedAt: fixturekit.DemoStart}
	doc, err := journalDocument(r.repo, fixturekit.Stream, j, fixturekit.DemoStart, "fixture")
	fixturekit.Must(r.t, err)
	h := trace.Header{Schema: "osmia.trace.transition", Version: trace.Version, ID: fmt.Sprintf("fixture-attempt-%d-%s", n, phase), Revision: 1, Project: r.repo.Project(), Workstream: fixturekit.Stream, At: fixturekit.DemoStart, Actor: fixturekit.OwnerActor, Cause: "fixture"}
	_, err = r.repo.RecordDocumentsWith(ctx, []trace.Document{doc}, trace.Transaction{Transition: trace.Transition{Header: h, Subject: trace.ClimbAttemptSubject(n), To: string(phase), Reason: "fixture"}})
	fixturekit.Must(r.t, err)
}

func (r *machineRig) cleanAttempt(n int) {
	r.t.Helper()
	state, err := r.repo.Workflow(fixturekit.Stream, trace.ClimbAttemptSubject(n))
	fixturekit.Must(r.t, err)
	h := trace.Header{Schema: "osmia.trace.transition", Version: trace.Version, ID: fmt.Sprintf("fixture-attempt-%d-cleaned", n), Revision: 1, Project: r.repo.Project(), Workstream: fixturekit.Stream, At: fixturekit.DemoStart, Actor: fixturekit.OwnerActor, Cause: "fixture"}
	_, err = r.repo.Transact(context.Background(), trace.Transaction{ExpectedVersion: state.Version, Transition: trace.Transition{Header: h, Subject: trace.ClimbAttemptSubject(n), From: state.Value, To: string(PhaseCleaned), Reason: "fixture"}})
	fixturekit.Must(r.t, err)
}

func (r *machineRig) transitionsInto(subject, to string) []string {
	r.t.Helper()
	all, err := trace.Read[trace.Transition](r.repo, fixturekit.Stream)
	fixturekit.Must(r.t, err)
	var ids []string
	for _, tr := range all {
		if tr.Subject == subject && tr.To == to {
			ids = append(ids, tr.ID)
		}
	}
	return ids
}

func TestAnAmendmentDuringClimbingWaitsForTheOpenAttemptThenCalibratesOnTheCurrentBestAndSurvivesARestart(t *testing.T) {
	t.Parallel()
	r := newMachineRig(t, true, scored(100), scored(101), scored(100))
	r.calibrated()
	first, err := r.approveAt("base0001", 1)
	fixturekit.Must(t, err)
	r.openAttempt(1, PhaseStarted)

	r.amend(addFixture("testdata/extra"))
	r.reply(scored(80), scored(81), scored(82))
	r.reconcile()
	if r.inner() != InnerClimbing || r.calls() != 0 {
		t.Fatalf("the machine moved a climbing climb with an attempt in flight: %q after %d calls", r.inner(), r.calls())
	}

	r.cleanAttempt(1)
	r.clone.land("best0002", "diff --git a/src/work.go b/src/work.go\n+tuned\n")
	crashed := false
	r.machine.Step = func(name string) error {
		if name == "climb-recalibrating" && !crashed {
			crashed = true
			return errors.New("crash after the move to calibrating")
		}
		return nil
	}
	if err := r.machine.Reconcile(context.Background(), fixturekit.Stream); err == nil || !crashed {
		t.Fatalf("the crash hook did not fire: %v", err)
	}
	if r.inner() != InnerCalibrating || r.calls() != 0 {
		t.Fatalf("after the crash the climb is %q with %d calls", r.inner(), r.calls())
	}

	r.reconcile()
	if r.inner() != InnerAwaitingApproval || r.calls() != 3 {
		t.Fatalf("after the restart the climb is %q with %d calls", r.inner(), r.calls())
	}
	amended := 0
	for _, id := range r.transitionsInto(InnerSubject, InnerCalibrating) {
		if strings.HasPrefix(id, "climb-amended-") {
			amended++
		}
	}
	if amended != 1 {
		t.Fatalf("the amendment moved the climb to calibrating %d times", amended)
	}
	cal, _, _, err := ReadCalibration(r.repo, fixturekit.Stream)
	fixturekit.Must(t, err)
	if cal.Commit != "best0002" || cal.EvaluatorCommit != "base0001" || cal.Calibration.Baseline != 81 || cal.SpecRevision != 2 || !slices.Contains(r.clone.exports, "best0002") {
		t.Fatalf("recalibration %+v, exports %v", cal, r.clone.exports)
	}
	if _, err := r.approveAt("base0001", 1); !errors.Is(err, ErrStaleApproval) {
		t.Fatalf("the first approval's card answered again: %v", err)
	}
	second, err := r.approveAt("best0002", 2)
	fixturekit.Must(t, err)
	if second.Epoch != 2 || second.SpecRevision != 2 || second.Baseline != 81 || !second.StartedAt().Equal(first.StartedAt()) {
		t.Fatalf("second approval %+v after %+v", second, first)
	}
	if latest, ok, err := ReadApproval(r.repo, fixturekit.Stream); err != nil || !ok || latest.Epoch != 2 {
		t.Fatalf("the latest approval %+v %v %v", latest, ok, err)
	}
	if again, err := r.approveAt("best0002", 2); err != nil || again.Epoch != 2 {
		t.Fatalf("answering the new card twice: %+v %v", again, err)
	}
}

func (r *loopRig) amend(change func(*Spec)) {
	r.t.Helper()
	next := r.spec
	next.Guardrails.Checks = slices.Clone(r.spec.Guardrails.Checks)
	next.Guardrails.Limits = slices.Clone(r.spec.Guardrails.Limits)
	change(&next)
	r.revision++
	r.specs[r.revision] = next
	r.spec = next
}

// recalibrate is what the inner machine and the owner do after the loop has
// settled an amendment: calibrate on the current best and approve again with
// the new baseline.
func (r *loopRig) recalibrate(baseline float64) {
	r.t.Helper()
	ctx := context.Background()
	state, err := ReadInner(r.repo, fixturekit.Stream)
	fixturekit.Must(r.t, err)
	h := func(id string) trace.Header {
		return trace.Header{Schema: "osmia.trace.transition", Version: trace.Version, ID: id, Revision: 1, Project: r.repo.Project(), Workstream: fixturekit.Stream, At: fixturekit.DemoStart, Actor: fixturekit.OwnerActor, Cause: "fixture"}
	}
	_, err = r.repo.Transact(ctx, trace.Transaction{ExpectedVersion: state.Version, Transition: trace.Transition{Header: h("fixture-calibrating"), Subject: InnerSubject, From: InnerClimbing, To: InnerCalibrating, Reason: "fixture"}})
	fixturekit.Must(r.t, err)
	state, err = ReadInner(r.repo, fixturekit.Stream)
	fixturekit.Must(r.t, err)
	next := r.approval
	next.Baseline, next.SpecRevision, next.Epoch, next.Started = baseline, r.revision, r.approval.EpochNumber()+1, r.approval.StartedAt()
	m := &Machine{Repository: r.repo, Now: func() time.Time { return fixturekit.DemoStart }}
	doc, err := m.document(fixturekit.Stream, trace.ClimbApprovalID, trace.ClimbApprovalPath, next.Epoch, "fixture", next)
	fixturekit.Must(r.t, err)
	_, err = r.repo.RecordDocumentsWith(ctx, []trace.Document{doc}, trace.Transaction{ExpectedVersion: state.Version, Transition: trace.Transition{Header: h("fixture-climbing-again"), Subject: InnerSubject, From: InnerCalibrating, To: InnerClimbing, Reason: "fixture"}})
	fixturekit.Must(r.t, err)
	r.approval = next
}

func (r *loopRig) started() int {
	r.t.Helper()
	n, err := LastStarted(r.repo, fixturekit.Stream)
	fixturekit.Must(r.t, err)
	return n
}

func TestAnAmendmentOfTheCalibrationEndsTheAttemptInFlightOnceAndStartsNoOther(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		crash string
		phase string
	}{
		"while its mason works":        {"", string(PhaseStarted)},
		"after its candidate is taken": {"climb:captured", string(PhaseCaptured)},
	} {
		t.Run(name, func(t *testing.T) {
			r := newLoopRig(t, nil)
			r.host.script(1, script{pending: tc.crash == "", changed: edits("src/work.go"), report: good("cache the parse")})
			r.host.script(2, script{changed: edits("src/work.go"), report: good("another idea")})
			r.score(1, 90, 120)
			r.score(2, 80, 120)
			if tc.crash == "" {
				r.reconcile()
			} else {
				r.crash(tc.crash)
			}
			if r.phase(1) != tc.phase {
				t.Fatalf("attempt 1 is %q, want %q", r.phase(1), tc.phase)
			}

			r.amend(addFixture("testdata/extra"))
			r.reconcile()
			a := r.attempt(1)
			if a.Outcome != OutcomeDiscarded || a.Cause != CauseAmended || a.SpecRevision != 1 || a.Epoch != 1 || a.Measurement != nil || a.Scores != nil || r.phase(1) != string(PhaseCleaned) {
				t.Fatalf("attempt 1 %+v is %q", a, r.phase(1))
			}
			if !strings.Contains(a.Reason, FieldProtected) {
				t.Fatalf("the reason does not name the changed field: %q", a.Reason)
			}
			for range 3 {
				r.reconcile()
			}
			if got := len(r.attempts()); got != 1 || r.started() != 1 {
				t.Fatalf("%d attempts recorded and %d started after the amendment", got, r.started())
			}
			if len(r.engine.calls) != 0 || len(r.host.guarded) != 0 || len(r.host.reviews) != 0 {
				t.Fatalf("an attempt was measured, checked or reviewed under the stale approval: %d %v %v", len(r.engine.calls), r.host.guarded, r.host.reviews)
			}
			if r.tip() != "base0001" || r.host.dropped[0] != 1 || len(r.host.dropped) != 1 || len(r.host.workspaces) != 0 {
				t.Fatalf("tip %s, dropped %v, workspaces %v", r.tip(), r.host.dropped, r.host.workspaces)
			}
			if inner, err := ReadInner(r.repo, fixturekit.Stream); err != nil || inner.Value != InnerClimbing {
				t.Fatalf("the loop moved the inner state to %q: %v", inner.Value, err)
			}
		})
	}
}

func TestAnAttemptThatAlreadyMovedTheClimbBranchIsPromotedAndRecordedBeforeTheClimbCalibratesAgain(t *testing.T) {
	t.Parallel()
	r := newLoopRig(t, nil)
	r.host.script(1, script{changed: edits("src/work.go"), report: good("cache the parse")})
	r.host.script(2, script{changed: edits("src/work.go"), report: good("another idea")})
	r.score(1, 90, 120)
	r.score(2, 80, 120)
	r.crash("climb:promoting")
	if r.phase(1) != string(PhasePromoting) {
		t.Fatalf("attempt 1 is %q", r.phase(1))
	}
	r.amend(func(s *Spec) { s.Metric.MinImprovement = 3 })
	r.reconcile()
	r.reconcile()
	a := r.attempt(1)
	if a.Outcome != OutcomeKept || a.SpecRevision != 1 || r.tip() != a.Commit || r.phase(1) != string(PhaseCleaned) {
		t.Fatalf("attempt 1 %+v, tip %s, phase %s", a, r.tip(), r.phase(1))
	}
	if len(r.attempts()) != 1 || r.started() != 1 || len(r.engine.calls) != 1 {
		t.Fatalf("%d attempts recorded, %d started, %d measurements", len(r.attempts()), r.started(), len(r.engine.calls))
	}
}

func TestLoopRefusesToMeasureOrStartAnAttemptUnderAnApprovalOfAnOlderClimbSpecRevision(t *testing.T) {
	t.Parallel()
	r := newLoopRig(t, nil)
	r.host.script(1, script{changed: edits("src/work.go"), report: good("cache the parse")})
	r.score(1, 90, 120)
	r.amend(func(s *Spec) { s.Metric.MinImprovement = 9 })
	c, err := r.loop.begin(fixturekit.Stream, false)
	fixturekit.Must(t, err)
	if !c.stale.Recalibrates() || c.revision != 2 || c.approval.SpecRevision != 1 {
		t.Fatalf("cycle %+v", c.stale)
	}
	ctx := context.Background()
	if err := r.loop.start(ctx, c, nil); !errors.Is(err, ErrApprovalStale) {
		t.Fatalf("start: %v", err)
	}
	if _, err := r.loop.measure(ctx, c, Journal{Number: 1, Parent: "base0001", Candidate: "cand0001", Phase: PhaseCaptured}); !errors.Is(err, ErrApprovalStale) {
		t.Fatalf("measure: %v", err)
	}
	r.reconcile()
	if r.started() != 0 || len(r.engine.calls) != 0 || len(r.host.threads) != 0 {
		t.Fatalf("started %d attempts, measured %d times, asked the mason about %v", r.started(), len(r.engine.calls), r.host.threads)
	}
	concluding, err := r.loop.begin(fixturekit.Stream, true)
	fixturekit.Must(t, err)
	if concluding.stale.Recalibrates() {
		t.Fatal("a concluding climb is held for recalibration")
	}
}

func TestAnAmendmentThatDoesNotDependOnCalibrationAppliesInPlaceToTheClimbingLoop(t *testing.T) {
	t.Parallel()
	r := newLoopRig(t, func(s *Spec) { s.Stop.MaxAttempts = 2 })
	r.host.script(1, script{pending: true, changed: edits("src/work.go"), report: good("cache the parse")})
	r.host.script(2, script{changed: edits("src/work.go"), report: good("batch the reads")})
	r.host.script(3, script{changed: edits("src/work.go"), report: good("skip a copy")})
	r.score(1, 90, 120)
	r.score(2, 80, 120)
	r.score(3, 70, 120)
	r.reconcile()
	r.amend(func(s *Spec) {
		s.Goal = "Make the parser faster still."
		s.Stop.MaxAttempts = 3
	})
	r.reconcile()
	attempts := r.attempts()
	if len(attempts) != 3 {
		t.Fatalf("%d attempts recorded, want the raised budget of 3", len(attempts))
	}
	for i, a := range attempts {
		wantRevision := 2
		if i == 0 {
			wantRevision = 1
		}
		if a.Outcome != OutcomeKept || a.Epoch != 1 || a.SpecRevision != wantRevision || a.Best != []float64{100, 90, 80}[i] {
			t.Errorf("attempt %d %+v, want revision %d", a.Number, a, wantRevision)
		}
	}
	if latest, ok, err := ReadApproval(r.repo, fixturekit.Stream); err != nil || !ok || latest.Epoch != 1 || latest.SpecRevision != 1 {
		t.Fatalf("the approval changed: %+v %v %v", latest, ok, err)
	}
}

func TestAfterARecalibrationAttemptsAreComparedOnlyWithTheNewBaselineAndTheStopRulesStartOver(t *testing.T) {
	t.Parallel()
	r := newLoopRig(t, func(s *Spec) { s.Stop.MaxAttempts = 10; s.Stop.Plateau = 2 })
	r.host.script(1, script{changed: edits("src/work.go"), report: good("cache the parse")})
	r.host.script(2, script{changed: edits("src/work.go"), report: good("batch the reads")})
	r.host.script(3, script{pending: true, changed: edits("src/work.go"), report: good("skip a copy")})
	r.score(1, 90, 120)
	r.score(2, 480, 120)
	r.crash("climb:cleaned")
	if r.attempt(1).Outcome != OutcomeKept {
		t.Fatalf("attempt 1 %+v", r.attempt(1))
	}
	r.amend(func(s *Spec) { s.Metric.MinImprovement = 3 })
	r.reconcile()
	if r.started() != 1 {
		t.Fatalf("an attempt started under the stale approval")
	}
	r.recalibrate(500)
	r.reconcile()
	a1, a2 := r.attempt(1), r.attempt(2)
	if a1.Epoch != 1 || a2.Epoch != 2 || a2.SpecRevision != 2 || a2.Best != 500 || a2.Outcome != OutcomeKept || a2.Parent != a1.Commit {
		t.Fatalf("attempt 1 %+v, attempt 2 %+v: the score 480 is an improvement on the new baseline 500, and the 90 of the old measurement is not its best", a1, a2)
	}
	brief := r.host.briefs[2]
	if brief.Best != 500 || brief.Baseline != 500 || brief.Earlier != 1 || len(brief.Log) != 0 || !strings.Contains(brief.Render(), "1 earlier attempt ran before the climb spec changed") {
		t.Fatalf("brief of attempt 2: %+v", brief)
	}
	if len(r.stops) != 0 {
		t.Fatalf("a plateau fired after %d attempts of which one is of the new epoch: %+v", len(r.attempts()), r.stops)
	}
	progress := BuildProgress(ProgressInput{Spec: r.spec, Attempts: r.attempts(), Epoch: 2, SpecRevision: 2, Now: r.now})
	if progress.Earlier != 1 || len(progress.Points) != 1 || progress.Points[0].Attempt != 2 || progress.Best == nil || *progress.Best != 480 || progress.Baseline == nil || *progress.Baseline != 500 || progress.Budgets.Attempts.Used != 2 {
		t.Fatalf("progress %+v", progress)
	}
}

func TestStopRulesCountTheBudgetsOverEveryAttemptAndTheBestOverTheCurrentEpoch(t *testing.T) {
	t.Parallel()
	kept := func(n, epoch int, score float64) Attempt {
		return Attempt{Number: n, Epoch: epoch, Outcome: OutcomeKept, Scores: &Scores{Primary: score}}
	}
	missed := func(n, epoch int) Attempt {
		return Attempt{Number: n, Epoch: epoch, Outcome: OutcomeDiscarded, Cause: CauseNoImprovement}
	}
	attempts := []Attempt{kept(1, 1, 90), missed(2, 1), missed(3, 1), missed(4, 2)}
	in := StopInput{Rules: StopRules{MaxAttempts: 4, Plateau: 3}, Direction: Lower, Baseline: 500, Attempts: attempts, Epoch: 2}
	got := Rules(EvaluateStops(in))
	if !slices.Equal(got, []string{RuleAttempts}) {
		t.Fatalf("firing rules %v: the budget counts every attempt and the plateau only the attempts since the new baseline", got)
	}
	target := 100.0
	in.Rules = StopRules{MaxAttempts: 10, Target: &target}
	if got := Rules(EvaluateStops(in)); len(got) != 0 {
		t.Fatalf("the best of the old measurement reached the target of the new one: %v", got)
	}
}
