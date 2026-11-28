package climb

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/kpenfound/osmia/internal/api"
	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/kinds"
	"github.com/kpenfound/osmia/internal/trace"
)

// ErrNotAwaiting refuses an approval of a climb that is not waiting for one.
var ErrNotAwaiting = errors.New("the climb is not waiting for the owner's approval of its evaluator")

// ErrStaleApproval refuses an approval that pins a commit other than the
// calibrated evaluator commit the climb branch holds.
var ErrStaleApproval = errors.New("the approval does not pin the calibrated evaluator commit")

const featureBuilding = "building"

// Workspace is the part of the climb branch's clone the inner machine reads.
type Workspace interface {
	Source
	// Branch returns the commit a branch points at.
	Branch(ctx context.Context, name string) (commit string, exists bool, err error)
	// Diff renders the change from base to candidate.
	Diff(ctx context.Context, base, candidate string) (string, error)
}

// Machine runs a Hill Climb workstream's inner machine up to the start of
// climbing: evaluator, calibrating, awaiting approval. Every move is recorded
// as one trace commit together with the documents it rests on, so a restart
// reads the inner state back and carries on from it without repeating a move
// that was recorded. The evaluator unit itself is a normal unit that the
// execution controllers run; the machine only decides whether it is needed
// and notices when it has landed.
type Machine struct {
	Repository *trace.Repository
	// Spec returns the sealed climb spec of the workstream and the revision of
	// it that was sealed.
	Spec func(stream config.WorkstreamID) (Spec, int, error)
	// SpecAt returns an earlier recorded revision of the climb spec, which the
	// machine compares with the sealed one to tell whether an amendment
	// changed what a calibration or an approval rests on.
	SpecAt SpecReader
	// Settings returns the `[kinds.hill-climb]` settings in effect.
	Settings func() kinds.HillClimbSettings
	// Workspace returns the workstream's clone, and Branch names the climb
	// branch in it.
	Workspace func(stream config.WorkstreamID) (Workspace, error)
	Branch    func(stream config.WorkstreamID) string
	// Runner returns the measurement runner that exports commits from source.
	Runner func(stream config.WorkstreamID, source Source) (Runner, error)
	// Root is the directory the machine makes its temporary exports in.
	Root string
	// Publish announces a climb event to the clients of the event stream, so
	// the panel redraws when an amendment sends the climb back to calibrating.
	Publish func(kind api.EventKind, stream config.WorkstreamID)
	Now     func() time.Time
	// Step is a hook between a side effect and its record; a failure aborts
	// the pass as a crash would.
	Step func(name string) error
}

// Reconcile moves the workstream's inner state one step: starting it,
// noticing that the evaluator unit landed, calibrating, or answering an
// amendment of the climb spec. It does nothing for a workstream that is not
// building. A calibration that cannot be carried out, such as one cancelled by
// a stopping service, records nothing and is carried out again by the next
// pass.
//
// An approved amendment that changes a field the calibration depends on sends
// a climb that waits for the owner's approval, or climbs, back to calibrating
// on the current best commit: the open approval card is retired, and a climb
// with an attempt in flight waits for the loop to end that attempt first. The
// machine never measures under an approval made under another revision of
// the climb spec; it calibrates again and the owner approves again.
func (m *Machine) Reconcile(ctx context.Context, stream config.WorkstreamID) error {
	feature, err := m.Repository.Workflow(stream, trace.FeatureSubject)
	if err != nil || feature.Value != featureBuilding {
		return err
	}
	state, err := ReadInner(m.Repository, stream)
	if err != nil {
		return err
	}
	switch state.Value {
	case "":
		return m.start(ctx, stream, state)
	case InnerEvaluator:
		return m.evaluated(ctx, stream, state)
	case InnerCalibrating:
		return m.calibrate(ctx, stream, state)
	case InnerHeldBroken, InnerHeldNoisy:
		return m.recalibrateAfterAmendment(ctx, stream, state)
	case InnerAwaitingApproval, InnerClimbing:
		return m.amended(ctx, stream, state)
	}
	return nil
}

func (m *Machine) export(ctx context.Context, ws Workspace, commit string) (string, func(), error) {
	root := m.Root
	if root == "" {
		root = os.TempDir()
	}
	if err := os.MkdirAll(root, 0o700); err != nil {
		return "", nil, err
	}
	dir, err := os.MkdirTemp(root, "climb-")
	if err != nil {
		return "", nil, err
	}
	if err := ws.Export(ctx, commit, dir); err != nil {
		os.RemoveAll(dir)
		return "", nil, fmt.Errorf("export %s: %w", commit, err)
	}
	return dir, func() { os.RemoveAll(dir) }, nil
}

func (m *Machine) tip(ctx context.Context, stream config.WorkstreamID, ws Workspace) (string, error) {
	branch := m.Branch(stream)
	tip, exists, err := ws.Branch(ctx, branch)
	if err != nil {
		return "", err
	}
	if !exists {
		return "", fmt.Errorf("the clone has no climb branch %s", branch)
	}
	return tip, nil
}

func (m *Machine) start(ctx context.Context, stream config.WorkstreamID, state trace.WorkflowState) error {
	spec, _, err := m.Spec(stream)
	if err != nil {
		return err
	}
	ws, err := m.Workspace(stream)
	if err != nil {
		return err
	}
	base, err := m.tip(ctx, stream, ws)
	if err != nil {
		return err
	}
	rec := EvaluatorRecord{Evaluator: spec.Evaluator, Base: base, At: m.now()}
	dir, cleanup, err := m.export(ctx, ws, base)
	if err != nil {
		return err
	}
	defer cleanup()
	toml, err := os.ReadFile(filepath.Join(dir, "dagger.toml"))
	if err == nil {
		rec.Module, err = DevModuleSource(toml)
	}
	switch {
	case err != nil:
		rec.Evidence = fmt.Sprintf("the project has no dev module to search: %v", err)
	default:
		found, evidence, ferr := FindEvaluator(filepath.Join(dir, filepath.FromSlash(rec.Module)), spec)
		if ferr != nil {
			return ferr
		}
		rec.Skipped, rec.Evidence = found, evidence
	}
	doc, err := m.document(stream, trace.ClimbEvaluatorID, trace.ClimbEvaluatorPath, 1, "climb-start", rec)
	if err != nil {
		return err
	}
	to, body := InnerEvaluator, fmt.Sprintf("The evaluator step of the climb starts: %s. A unit writes the evaluator function %q before calibration.", rec.Evidence, spec.Evaluator)
	reason := fmt.Sprintf("the climb spec names the evaluator %q and %s, so the evaluator unit runs", spec.Evaluator, rec.Evidence)
	if rec.Skipped {
		to = InnerCalibrating
		body = fmt.Sprintf("The evaluator step of the climb is skipped: %s. The function is reused and calibration starts.", rec.Evidence)
		reason = fmt.Sprintf("the project already has an evaluator matching the climb spec (%s), so the evaluator unit is skipped and calibration starts", rec.Evidence)
	}
	id := "climb-start"
	txs := []trace.Transaction{{ExpectedVersion: state.Version,
		Transition: trace.Transition{Header: m.header(id, stream, id), Subject: InnerSubject, From: state.Value, To: to, Reason: reason},
		Events:     []trace.Event{trace.Progress(id, trace.ManagerKey, body)}}}
	if !rec.Skipped {
		subject := trace.UnitSubject(EvaluatorUnitID)
		txs = append(txs,
			trace.Transaction{Transition: trace.Transition{Header: m.header(subject+"-planned", stream, id), Subject: subject, To: "planned", Reason: "the evaluator unit is the one unit of a Hill Climb workstream, made from its sealed climb spec"}},
			trace.Transaction{ExpectedVersion: 1, Transition: trace.Transition{Header: m.header(subject+"-ready", stream, id), Subject: subject, From: "planned", To: "ready", Reason: "the evaluator unit depends on no unit"}})
	}
	if _, err := m.Repository.RecordDocumentsWith(ctx, []trace.Document{doc}, txs...); err != nil {
		if errors.Is(err, trace.ErrConflict) {
			return nil
		}
		return err
	}
	return m.step("climb-started")
}

func (m *Machine) evaluated(ctx context.Context, stream config.WorkstreamID, state trace.WorkflowState) error {
	unit, err := m.Repository.Workflow(stream, trace.UnitSubject(EvaluatorUnitID))
	if err != nil || unit.Value != "merged" {
		return err
	}
	id := "climb-calibrating"
	tx := trace.Transaction{ExpectedVersion: state.Version,
		Transition: trace.Transition{Header: m.header(id, stream, trace.UnitSubject(EvaluatorUnitID)+"-merged"), Subject: InnerSubject, From: state.Value, To: InnerCalibrating, Reason: "the evaluator unit landed as the first commit on the climb branch, so calibration starts"},
		Events:     []trace.Event{trace.Progress(id, trace.ManagerKey, "The evaluator unit landed on the climb branch. Calibration starts.")}}
	if _, err := m.Repository.TransactWhileFeature(ctx, tx, featureBuilding); err != nil && !errors.Is(err, trace.ErrConflict) {
		return err
	}
	return nil
}

func (m *Machine) calibrate(ctx context.Context, stream config.WorkstreamID, state trace.WorkflowState) error {
	spec, revision, err := m.Spec(stream)
	if err != nil {
		return err
	}
	ev, found, err := ReadEvaluator(m.Repository, stream)
	if err != nil {
		return err
	}
	if !found {
		return fmt.Errorf("workstream %s is calibrating without an evaluator record", stream)
	}
	ws, err := m.Workspace(stream)
	if err != nil {
		return err
	}
	tip, err := m.tip(ctx, stream, ws)
	if err != nil {
		return err
	}
	dir, cleanup, err := m.export(ctx, ws, tip)
	if err != nil {
		return err
	}
	defer cleanup()
	toml, err := os.ReadFile(filepath.Join(dir, "dagger.toml"))
	if err != nil {
		return fmt.Errorf("the evaluator commit %s has no dagger.toml: %w", tip, err)
	}
	protected, err := spec.ProtectedPaths(toml)
	if err != nil {
		return err
	}
	approved, err := Snapshot(dir, protected)
	if err != nil {
		return err
	}
	approved.SpecRevision = revision
	runner, err := m.Runner(stream, ws)
	if err != nil {
		return err
	}
	runs := m.Settings().CalibrationRuns
	cal, err := runner.Calibrate(ctx, Request{Commit: tip, Function: spec.Evaluator, Approved: approved, Expect: spec.Expectations()}, runs, spec.Metric.MinImprovement)
	if err != nil {
		return err
	}
	if err := m.step("climb-calibrated"); err != nil {
		return err
	}
	rec := CalibrationRecord{Evaluator: spec.Evaluator, Primary: spec.Metric.Name, Commit: tip, EvaluatorCommit: tip, Base: ev.Base, Protected: protected, SpecRevision: revision, Calibration: cal, At: m.now()}
	prior, _, calibrated, err := ReadCalibration(m.Repository, stream)
	if err != nil {
		return err
	}
	if calibrated {
		if prior.EvaluatorCommit != "" {
			rec.EvaluatorCommit = prior.EvaluatorCommit
		} else {
			rec.EvaluatorCommit = prior.Commit
		}
		change, err := Drift(m.SpecAt, stream, prior.SpecRevision, spec, revision)
		if err != nil {
			return err
		}
		if change.Recalibrates() {
			rec.Recalibrated = &Recalibration{From: prior.SpecRevision, Fields: change.Calibration}
		}
	}
	if ev.Base != rec.EvaluatorCommit {
		if rec.Diff, err = ws.Diff(ctx, ev.Base, rec.EvaluatorCommit); err != nil {
			return err
		}
	}
	revisionOf, err := nextRevision(m.Repository, stream, trace.ClimbCalibrationID)
	if err != nil {
		return err
	}
	doc, err := m.document(stream, trace.ClimbCalibrationID, trace.ClimbCalibrationPath, revisionOf, fmt.Sprintf("climb-calibration-%d", revisionOf), rec)
	if err != nil {
		return err
	}
	to, body, reason := InnerAwaitingApproval, "", ""
	switch cal.Verdict {
	case Calibrated:
		body = fmt.Sprintf("Calibration of the evaluator %q ran %d times on %s: baseline %v %s, spread %v. The owner's approval card is open; no attempt starts before the owner approves.", spec.Evaluator, runs, tip, cal.Baseline, cal.Units[spec.Metric.Name], cal.Spread)
		if rec.Recalibrated != nil {
			body += fmt.Sprintf(" The climb spec changed to revision %d (%s), so this baseline replaces the earlier one and the attempts before it are no longer compared with it.", revision, strings.Join(rec.Recalibrated.Fields, ", "))
		}
		reason = fmt.Sprintf("calibration ran the evaluator %d times on %s and the spread %v is below the minimum improvement %v", runs, tip, cal.Spread, spec.Metric.MinImprovement)
	case BrokenMetric:
		to = InnerHeldBroken
		reason = "calibration found the metric broken: " + cal.Reason
		body = fmt.Sprintf("The climb is held: the metric, not the code, is broken. Calibration of the evaluator %q on %s failed: %s No attempt starts. Fix the evaluator or the climb spec by amendment; a changed climb spec calibrates again.", spec.Evaluator, tip, sentence(cal.Reason))
	case TooNoisy:
		to = InnerHeldNoisy
		reason = "calibration found the metric too noisy to climb: " + cal.Reason
		body = fmt.Sprintf("The climb is held: the metric is too noisy to climb. Calibration of the evaluator %q on %s: %s No attempt starts. Make the evaluator steadier, raise the repeats or the minimum improvement by amendment; a changed climb spec calibrates again.", spec.Evaluator, tip, sentence(cal.Reason))
	default:
		return fmt.Errorf("calibration ended with the verdict %q", cal.Verdict)
	}
	id := fmt.Sprintf("climb-calibration-%d", revisionOf)
	event := trace.Notice(id, trace.ManagerKey, body)
	if cal.Verdict == Calibrated {
		event = trace.Progress(id, trace.ManagerKey, body)
	}
	tx := trace.Transaction{ExpectedVersion: state.Version,
		Transition: trace.Transition{Header: m.header(id, stream, trace.ClimbEvaluatorID), Subject: InnerSubject, From: state.Value, To: to, Reason: reason},
		Events:     []trace.Event{event}}
	if _, err := m.Repository.RecordDocumentsWith(ctx, []trace.Document{doc}, tx); err != nil {
		if errors.Is(err, trace.ErrConflict) {
			return nil
		}
		return err
	}
	return m.step("climb-calibration-recorded")
}

func sentence(s string) string {
	s = strings.TrimSpace(s)
	if s == "" || strings.HasSuffix(s, ".") {
		return s
	}
	return s + "."
}

func (m *Machine) recalibrateAfterAmendment(ctx context.Context, stream config.WorkstreamID, state trace.WorkflowState) error {
	spec, revision, err := m.Spec(stream)
	if err != nil {
		return err
	}
	cal, _, found, err := ReadCalibration(m.Repository, stream)
	if err != nil || !found || cal.SpecRevision == revision {
		return err
	}
	change, err := Drift(m.SpecAt, stream, cal.SpecRevision, spec, revision)
	if err != nil || !change.Recalibrates() {
		return err
	}
	id := fmt.Sprintf("climb-recalibrating-%d", revision)
	tx := trace.Transaction{ExpectedVersion: state.Version,
		Transition: trace.Transition{Header: m.header(id, stream, id), Subject: InnerSubject, From: state.Value, To: InnerCalibrating, Reason: fmt.Sprintf("the climb spec changed to revision %d (%s) while the climb was held, so calibration runs again", revision, change.Words())},
		Events:     []trace.Event{trace.Progress(id, trace.ManagerKey, fmt.Sprintf("The climb spec changed to revision %d (%s) while the climb was held. Calibration runs again.", revision, change.Words()))}}
	if _, err := m.Repository.TransactWhileFeature(ctx, tx, featureBuilding); err != nil && !errors.Is(err, trace.ErrConflict) {
		return err
	}
	return nil
}

// amended answers an approved amendment of the climb spec for a climb that
// waits for the owner's approval or climbs. When a field the calibration
// depends on changed since the calibration, or the approval, was made, the
// climb returns to calibrating on the commit its branch holds now, which is
// the current best: the open approval card is retired by the move, since the
// card exists only while the climb waits for the approval, and the owner
// approves the recalibrated evaluator on a new card. A climb with an attempt
// in flight stays as it is until the loop has ended that attempt with its one
// terminal record, which is what makes the branch tip the best. A change that
// calibration does not depend on moves nothing.
func (m *Machine) amended(ctx context.Context, stream config.WorkstreamID, state trace.WorkflowState) error {
	spec, revision, err := m.Spec(stream)
	if err != nil {
		return err
	}
	pinned, what := 0, ""
	switch state.Value {
	case InnerAwaitingApproval:
		cal, _, found, err := ReadCalibration(m.Repository, stream)
		if err != nil || !found {
			return err
		}
		pinned, what = cal.SpecRevision, "the owner's approval card, which showed the evaluator calibrated under it, is retired"
	case InnerClimbing:
		approval, found, err := ReadApproval(m.Repository, stream)
		if err != nil || !found {
			return err
		}
		pinned, what = approval.SpecRevision, "the owner's approval no longer describes what the climb measures"
	}
	change, err := Drift(m.SpecAt, stream, pinned, spec, revision)
	if err != nil || !change.Recalibrates() {
		return err
	}
	if state.Value == InnerClimbing {
		if _, open, err := OpenAttempt(m.Repository, stream); err != nil || open {
			return err
		}
	}
	id := fmt.Sprintf("climb-amended-%d-%s", revision, state.Value)
	reason := fmt.Sprintf("the climb spec changed from revision %d to %d and %s changed, so %s and the evaluator is calibrated again on the current best commit; no attempt starts before the owner approves it again", pinned, revision, change.Words(), what)
	tx := trace.Transaction{ExpectedVersion: state.Version,
		Transition: trace.Transition{Header: m.header(id, stream, id), Subject: InnerSubject, From: state.Value, To: InnerCalibrating, Reason: reason},
		Events:     []trace.Event{trace.Notice(id, trace.ManagerKey, fmt.Sprintf("An approved amendment changed the climb spec to revision %d: %s. The evaluator is calibrated again on the current best commit and the owner approves it again before an attempt starts; the earlier baseline and best are retired.", revision, change.Words()))}}
	if _, err := m.Repository.TransactWhileFeature(ctx, tx, featureBuilding); err != nil {
		if errors.Is(err, trace.ErrConflict) {
			return nil
		}
		return err
	}
	if m.Publish != nil {
		m.Publish(api.EventClimbPhase, stream)
	}
	return m.step("climb-recalibrating")
}

// Approve records the owner's approval of the calibrated evaluator and starts
// climbing. commit and specRevision pin the evaluator commit and the climb
// spec revision the owner saw on the card; an approval that pins another
// commit or revision, a card of a calibration an amendment has retired, or an
// approval of a climb branch that has moved on from the calibrated commit, is
// refused. Approval snapshots the approved
// copy of the protected paths from that commit and freezes them in the same
// record that moves the climb to climbing, so climbing never starts without
// both. Only the owner's explicit decision calls it.
func (m *Machine) Approve(ctx context.Context, stream config.WorkstreamID, commit string, specRevision int, by trace.Actor) (ApprovalRecord, error) {
	state, err := ReadInner(m.Repository, stream)
	if err != nil {
		return ApprovalRecord{}, err
	}
	if state.Value == InnerClimbing {
		if rec, ok, err := ReadApproval(m.Repository, stream); err != nil {
			return ApprovalRecord{}, err
		} else if ok && rec.Commit == commit && rec.SpecRevision == specRevision {
			return rec, nil
		}
		return ApprovalRecord{}, ErrNotAwaiting
	}
	if state.Value != InnerAwaitingApproval {
		return ApprovalRecord{}, ErrNotAwaiting
	}
	cal, _, found, err := ReadCalibration(m.Repository, stream)
	if err != nil {
		return ApprovalRecord{}, err
	}
	if !found || cal.Calibration.Verdict != Calibrated {
		return ApprovalRecord{}, ErrNotAwaiting
	}
	if commit != cal.Commit {
		return ApprovalRecord{}, fmt.Errorf("%w: the card pins %q and the evaluator commit is %s", ErrStaleApproval, commit, cal.Commit)
	}
	if specRevision != cal.SpecRevision {
		return ApprovalRecord{}, fmt.Errorf("%w: the card pins climb spec revision %d and the evaluator was calibrated under revision %d", ErrStaleApproval, specRevision, cal.SpecRevision)
	}
	spec, revision, err := m.Spec(stream)
	if err != nil {
		return ApprovalRecord{}, err
	}
	change, err := Drift(m.SpecAt, stream, cal.SpecRevision, spec, revision)
	if err != nil {
		return ApprovalRecord{}, err
	}
	if change.Recalibrates() {
		return ApprovalRecord{}, fmt.Errorf("%w: the climb spec is at revision %d and %s changed since the evaluator was calibrated under revision %d", ErrStaleApproval, revision, change.Words(), cal.SpecRevision)
	}
	ws, err := m.Workspace(stream)
	if err != nil {
		return ApprovalRecord{}, err
	}
	tip, err := m.tip(ctx, stream, ws)
	if err != nil {
		return ApprovalRecord{}, err
	}
	if tip != cal.Commit {
		return ApprovalRecord{}, fmt.Errorf("%w: the climb branch is at %s and the evaluator commit is %s", ErrStaleApproval, tip, cal.Commit)
	}
	dir, cleanup, err := m.export(ctx, ws, cal.Commit)
	if err != nil {
		return ApprovalRecord{}, err
	}
	defer cleanup()
	approved, err := Snapshot(dir, cal.Protected)
	if err != nil {
		return ApprovalRecord{}, err
	}
	approved.SpecRevision = cal.SpecRevision
	revisionOf, err := nextRevision(m.Repository, stream, trace.ClimbApprovalID)
	if err != nil {
		return ApprovalRecord{}, err
	}
	rec := ApprovalRecord{Evaluator: cal.Evaluator, Commit: cal.Commit, Baseline: cal.Calibration.Baseline, Metrics: cal.Calibration.Metrics, Units: cal.Calibration.Units,
		Noise: cal.Calibration.Spread, Protected: slices.Clone(cal.Protected), Approved: approved, ApprovedBy: by.Kind, At: m.now(),
		SpecRevision: cal.SpecRevision, Epoch: revisionOf}
	if earlier, ok, err := ReadApproval(m.Repository, stream); err != nil {
		return ApprovalRecord{}, err
	} else if ok {
		rec.Started = earlier.StartedAt()
	}
	doc, err := m.document(stream, trace.ClimbApprovalID, trace.ClimbApprovalPath, revisionOf, fmt.Sprintf("climb-approval-%d", revisionOf), rec)
	if err != nil {
		return ApprovalRecord{}, err
	}
	id := "climb-approved"
	if revisionOf > 1 {
		id = fmt.Sprintf("climb-approved-%d", revisionOf)
	}
	tx := trace.Transaction{ExpectedVersion: state.Version,
		Transition: trace.Transition{Header: trace.Header{Schema: "osmia.trace.transition", Version: trace.Version, ID: id, Revision: 1, Project: m.Repository.Project(), Workstream: stream, At: m.now(), Actor: by, Cause: trace.ClimbCalibrationID},
			Subject: InnerSubject, From: state.Value, To: InnerClimbing, Reason: fmt.Sprintf("the owner approved the calibrated evaluator %q at %s: baseline %v, spread %v, %d protected paths frozen", cal.Evaluator, cal.Commit, rec.Baseline, rec.Noise, len(rec.Protected)), DecidedBy: trace.DecidedByOwner},
		Events: []trace.Event{trace.Progress(id, trace.ManagerKey, fmt.Sprintf("The owner approved the evaluator %q at %s. The protected paths are frozen and the first attempt starts from baseline %v.", cal.Evaluator, cal.Commit, rec.Baseline))}}
	if _, err := m.Repository.RecordDocumentsWith(ctx, []trace.Document{doc}, tx); err != nil {
		if errors.Is(err, trace.ErrConflict) {
			if now, rerr := ReadInner(m.Repository, stream); rerr == nil && now.Value == InnerClimbing {
				if current, ok, rerr := ReadApproval(m.Repository, stream); rerr == nil && ok && current.Commit == commit && current.SpecRevision == cal.SpecRevision {
					return current, nil
				}
			}
			return ApprovalRecord{}, ErrNotAwaiting
		}
		return ApprovalRecord{}, err
	}
	if err := m.step("climb-approved"); err != nil {
		return ApprovalRecord{}, err
	}
	return rec, nil
}
