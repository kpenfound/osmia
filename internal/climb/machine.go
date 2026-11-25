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
	Now  func() time.Time
	// Step is a hook between a side effect and its record; a failure aborts
	// the pass as a crash would.
	Step func(name string) error
}

// Reconcile moves the workstream's inner state one step: starting it,
// noticing that the evaluator unit landed, or calibrating. It does nothing
// for a workstream that is not building. A calibration that cannot be carried
// out, such as one cancelled by a stopping service, records nothing and is
// carried out again by the next pass.
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
		Events:     []trace.Event{trace.Progress(id, "chief", body)}}}
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
		Events:     []trace.Event{trace.Progress(id, "chief", "The evaluator unit landed on the climb branch. Calibration starts.")}}
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
	rec := CalibrationRecord{Evaluator: spec.Evaluator, Primary: spec.Metric.Name, Commit: tip, Base: ev.Base, Protected: protected, SpecRevision: revision, Calibration: cal, At: m.now()}
	if ev.Base != tip {
		if rec.Diff, err = ws.Diff(ctx, ev.Base, tip); err != nil {
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
	event := trace.Notice(id, "chief", body)
	if cal.Verdict == Calibrated {
		event = trace.Progress(id, "chief", body)
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
	_, revision, err := m.Spec(stream)
	if err != nil {
		return err
	}
	cal, _, found, err := ReadCalibration(m.Repository, stream)
	if err != nil || !found || cal.SpecRevision == revision {
		return err
	}
	id := fmt.Sprintf("climb-recalibrating-%d", revision)
	tx := trace.Transaction{ExpectedVersion: state.Version,
		Transition: trace.Transition{Header: m.header(id, stream, id), Subject: InnerSubject, From: state.Value, To: InnerCalibrating, Reason: fmt.Sprintf("the climb spec changed to revision %d while the climb was held, so calibration runs again", revision)},
		Events:     []trace.Event{trace.Progress(id, "chief", fmt.Sprintf("The climb spec changed to revision %d while the climb was held. Calibration runs again.", revision))}}
	if _, err := m.Repository.TransactWhileFeature(ctx, tx, featureBuilding); err != nil && !errors.Is(err, trace.ErrConflict) {
		return err
	}
	return nil
}

// Approve records the owner's approval of the calibrated evaluator and starts
// climbing. commit pins the evaluator commit the owner saw on the card; an
// approval that pins another commit, or of a climb branch that has moved on
// from the calibrated commit, is refused. Approval snapshots the approved
// copy of the protected paths from that commit and freezes them in the same
// record that moves the climb to climbing, so climbing never starts without
// both. Only the owner's explicit decision calls it.
func (m *Machine) Approve(ctx context.Context, stream config.WorkstreamID, commit string, by trace.Actor) (ApprovalRecord, error) {
	state, err := ReadInner(m.Repository, stream)
	if err != nil {
		return ApprovalRecord{}, err
	}
	if state.Value == InnerClimbing {
		if rec, ok, err := ReadApproval(m.Repository, stream); err != nil || ok {
			return rec, err
		}
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
	rec := ApprovalRecord{Evaluator: cal.Evaluator, Commit: cal.Commit, Baseline: cal.Calibration.Baseline, Metrics: cal.Calibration.Metrics, Units: cal.Calibration.Units,
		Noise: cal.Calibration.Spread, Protected: slices.Clone(cal.Protected), Approved: approved, ApprovedBy: by.Kind, At: m.now()}
	doc, err := m.document(stream, trace.ClimbApprovalID, trace.ClimbApprovalPath, 1, "climb-approval", rec)
	if err != nil {
		return ApprovalRecord{}, err
	}
	id := "climb-approved"
	tx := trace.Transaction{ExpectedVersion: state.Version,
		Transition: trace.Transition{Header: trace.Header{Schema: "osmia.trace.transition", Version: trace.Version, ID: id, Revision: 1, Project: m.Repository.Project(), Workstream: stream, At: m.now(), Actor: by, Cause: trace.ClimbCalibrationID},
			Subject: InnerSubject, From: state.Value, To: InnerClimbing, Reason: fmt.Sprintf("the owner approved the calibrated evaluator %q at %s: baseline %v, spread %v, %d protected paths frozen", cal.Evaluator, cal.Commit, rec.Baseline, rec.Noise, len(rec.Protected)), DecidedBy: trace.DecidedByOwner},
		Events: []trace.Event{trace.Progress(id, "chief", fmt.Sprintf("The owner approved the evaluator %q at %s. The protected paths are frozen and the first attempt starts from baseline %v.", cal.Evaluator, cal.Commit, rec.Baseline))}}
	if _, err := m.Repository.RecordDocumentsWith(ctx, []trace.Document{doc}, tx); err != nil {
		if errors.Is(err, trace.ErrConflict) {
			if current, ok, rerr := ReadApproval(m.Repository, stream); rerr == nil && ok {
				return current, nil
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
