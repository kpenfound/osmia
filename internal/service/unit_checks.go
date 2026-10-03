package service

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/scheduler"
	"github.com/kpenfound/osmia/internal/trace"
)

// CheckAction is the runner operation that runs the checks of one unit
// candidate before review.
const CheckAction = "unit-checks"

const checksRole = "checks"

var checksActor = trace.Actor{Kind: "service", ID: checksRole}

// checkRetryDelay is how long a unit whose checks did not complete waits
// before they run again.
const checkRetryDelay = 10 * time.Minute

// The statuses of a check run.
const (
	ChecksPassed = "passed"
	ChecksFailed = "failed"
	// ChecksIncomplete is a run that reported no result for its checks: the
	// export, the engine or Dagger failed, or the run timed out.
	ChecksIncomplete = "incomplete"
	// ChecksSuperseded is a run whose candidate was replaced, or whose unit
	// left checking, before it ran.
	ChecksSuperseded = "superseded"
)

// checkInput is a check operation's input: the run and the candidate it is
// bound to, from the unit's report revision Report.
type checkInput struct {
	Workstream config.WorkstreamID `json:"workstream"`
	Unit       string              `json:"unit"`
	Run        int                 `json:"run"`
	Candidate  string              `json:"candidate"`
	Base       string              `json:"base"`
	Report     int                 `json:"report"`
}

// UnitCheckRun is the document units/<unit>/checks-<run>.json: one run of a
// unit candidate's checks, bound to the candidate, its base and its diff, with
// the checks it selected, the command, its exit status and the end of its
// output. Failed lists the check links Dagger's report shows failed.
type UnitCheckRun struct {
	Unit       string          `json:"unit"`
	Run        int             `json:"run"`
	Operation  string          `json:"operation"`
	Candidate  string          `json:"candidate"`
	Base       string          `json:"base"`
	DiffSHA256 string          `json:"diff_sha256"`
	Report     int             `json:"report"`
	Status     string          `json:"status"`
	Selection  *CheckSelection `json:"selection,omitempty"`
	Command    []string        `json:"command,omitempty"`
	ExitCode   int             `json:"exit_code"`
	Output     string          `json:"output,omitempty"`
	Truncated  bool            `json:"truncated,omitempty"`
	Failed     []string        `json:"failed,omitempty"`
	Error      string          `json:"error,omitempty"`
	StartedAt  time.Time       `json:"started_at"`
	FinishedAt time.Time       `json:"finished_at"`
}

// matches reports whether the run checked the identity's candidate, base and
// diff.
func (r UnitCheckRun) matches(identity UnitReviewIdentity) bool {
	return r.Candidate == identity.Candidate.Revision && r.Base == identity.Candidate.BaseRevision && r.DiffSHA256 == identity.DiffSHA256
}

// unitReport returns the latest revision of the unit's report and its
// revision number, 0 when it has none.
func unitReport(repo *trace.Repository, stream config.WorkstreamID, unit string) (UnitReport, int, error) {
	docs, err := trace.Read[trace.Document](repo, stream)
	if err != nil {
		return UnitReport{}, 0, err
	}
	var latest trace.Document
	for _, d := range docs {
		if d.ID == reportDocument(unit) && d.Revision > latest.Revision {
			latest = d
		}
	}
	var report UnitReport
	if latest.Revision == 0 {
		return report, 0, nil
	}
	if err := json.Unmarshal([]byte(latest.Content), &report); err != nil {
		return report, 0, fmt.Errorf("%s: %w", latest.Path, err)
	}
	return report, latest.Revision, nil
}

func checkDocumentID(unit string, run int) string {
	return fmt.Sprintf("%s-checks-%d", trace.UnitSubject(unit), run)
}

func checkRequestID(unit string, run int) string { return checkDocumentID(unit, run) + "-requested" }

func checkPath(unit string, run int) string { return fmt.Sprintf("units/%s/checks-%d.json", unit, run) }

var checkPathPattern = regexp.MustCompile(`^units/[^/]+/checks-[0-9]+\.json$`)

// checksSubject is the workflow subject of a unit's check runs: requested-<k>
// once run k is asked for, and incomplete-<k> once run k did not complete.
func checksSubject(unit string) string { return trace.UnitSubject(unit) + "-checks" }

// checksThread names a unit's check runs in a Jev judgment's scope.
func checksThread(unit string) string {
	return checksRole + strings.TrimPrefix(trace.UnitSubject(unit), "unit")
}

// scope is the scope of a check run's Jev judgment.
func (c *checkers) scope(in checkInput) coreadapter.Scope {
	return coreadapter.Scope{Project: string(c.repository.Project()), Workstream: string(in.Workstream), Unit: in.Unit, Thread: checksThread(in.Unit), Turn: checkDocumentID(in.Unit, in.Run), Role: checksRole}
}

// checkers is the checks controller. Its pass runs each checking unit's
// candidate checks once, as an operation, and moves the unit on the result:
// passing checks to reviewing, failing checks back to implementing as a
// send-back, or contested once send-backs reach shed.max_bounces. A run that
// did not complete leaves the unit checking, tells the chief of staff, and
// runs again after checkRetryDelay. A workstream runs one check operation at a
// time.
type checkers struct{ *masons }

var _ coreadapter.Reconciler = (*checkers)(nil)

func (c *checkers) Pass(ctx context.Context) error {
	streams, err := c.repository.Workstreams()
	if err != nil {
		return err
	}
	state, _ := c.s.effective()
	for _, stream := range streams {
		if stream == librarianWorkstream(c.repository.Project()) {
			continue
		}
		b, found, err := c.read(stream)
		if err != nil {
			return err
		}
		if !found {
			continue
		}
		if gone, err := abandoned(c.repository, stream); err != nil || gone {
			if err != nil {
				return err
			}
			continue
		}
		runs, err := checkRuns(c.repository, stream)
		if err != nil {
			return err
		}
		ops, err := checkOperations(c.repository, stream)
		if err != nil {
			return err
		}
		transitions, err := trace.Read[trace.Transition](c.repository, stream)
		if err != nil {
			return err
		}
		hold := scheduler.Paused(state.Pauses, c.cfg.Project.ID, stream) || slices.ContainsFunc(ops, func(op checkOperation) bool { return !op.done })
		for _, u := range b.plan.Units {
			if b.states[trace.UnitSubject(u.ID)].Value != UnitChecking {
				continue
			}
			requested, err := c.one(ctx, stream, u.ID, b.states[trace.UnitSubject(u.ID)], sinceMove(transitions, runs, u.ID), ops, hold)
			if err != nil {
				return fmt.Errorf("workstream %s unit %s checks: %w", stream, u.ID, err)
			}
			hold = hold || requested
		}
	}
	return nil
}

// one moves a checking unit on the latest run of the candidate its report
// records, or requests a run unless hold. It reports whether it requested
// one. It reads recorded state alone; the run computes everything else.
func (c *checkers) one(ctx context.Context, stream config.WorkstreamID, unit string, state trace.WorkflowState, runs []UnitCheckRun, ops []checkOperation, hold bool) (bool, error) {
	report, revision, err := unitReport(c.repository, stream, unit)
	if err != nil {
		return false, err
	}
	if revision == 0 || report.Candidate == "" || report.Base == "" {
		return false, c.block(ctx, stream, unit, fmt.Sprintf("unit %s stays checking: it has no recorded candidate", unit))
	}
	var latest *UnitCheckRun
	for i, r := range runs {
		if r.Unit == unit && r.Status != ChecksSuperseded && r.Candidate == report.Candidate && r.Base == report.Base && (latest == nil || r.Run > latest.Run) {
			latest = &runs[i]
		}
	}
	if latest != nil {
		switch latest.Status {
		case ChecksPassed:
			return false, c.pass(ctx, stream, unit, state, *latest)
		case ChecksFailed:
			return false, c.fail(ctx, stream, unit, state, *latest)
		case ChecksIncomplete:
			if err := c.incomplete(ctx, stream, *latest); err != nil {
				return false, err
			}
			if c.s.now().Before(latest.FinishedAt.Add(checkRetryDelay)) {
				return false, nil
			}
		}
	}
	if hold {
		return false, nil
	}
	run := 1
	for _, op := range ops {
		if op.input.Unit == unit && op.input.Run >= run {
			run = op.input.Run + 1
		}
	}
	return true, c.request(ctx, stream, checkInput{Workstream: stream, Unit: unit, Run: run, Candidate: report.Candidate, Base: report.Base, Report: revision})
}

// sinceMove returns runs without the unit's runs requested before its latest
// move into checking, so a move into checking runs the checks again.
func sinceMove(transitions []trace.Transition, runs []UnitCheckRun, unit string) []UnitCheckRun {
	moved := -1
	for i, t := range transitions {
		if isMove(t, unit) && t.To == UnitChecking {
			moved = i
		}
	}
	if moved < 0 {
		return runs
	}
	requested := map[string]bool{}
	for _, t := range transitions[moved+1:] {
		if t.Subject == checksSubject(unit) {
			requested[t.ID] = true
		}
	}
	return slices.DeleteFunc(slices.Clone(runs), func(r UnitCheckRun) bool {
		return r.Unit == unit && !requested[checkRequestID(unit, r.Run)]
	})
}

// request records the operation that runs in's checks.
func (c *checkers) request(ctx context.Context, stream config.WorkstreamID, in checkInput) error {
	data, err := json.Marshal(in)
	if err != nil {
		return err
	}
	subject, err := c.repository.Workflow(stream, checksSubject(in.Unit))
	if err != nil {
		return err
	}
	id := checkRequestID(in.Unit, in.Run)
	event := trace.EventID(id, "run")
	op := coreadapter.Operation{ID: trace.OperationID(c.repository.Project(), stream, event), Boundary: coreadapter.RunnerBoundary, Action: CheckAction, Input: data}
	h := trace.Header{Schema: "osmia.trace.transition", Version: trace.Version, ID: id, Revision: 1, Project: c.repository.Project(), Workstream: stream, Unit: in.Unit, At: c.s.now(), Actor: checksActor, Cause: reportDocument(in.Unit)}
	reason := fmt.Sprintf("run %d checks candidate %s of unit %s, from %s, before review", in.Run, in.Candidate, in.Unit, in.Base)
	_, err = c.repository.Transact(ctx, trace.Transaction{ExpectedVersion: subject.Version,
		Transition: trace.Transition{Header: h, Subject: checksSubject(in.Unit), From: subject.Value, To: fmt.Sprintf("requested-%d", in.Run), Reason: reason},
		Events:     []trace.Event{{ID: event, Kind: CheckAction, Body: fmt.Sprintf("Run the checks of unit %s", in.Unit), Operation: &op}}})
	if errors.Is(err, trace.ErrConflict) {
		return nil
	}
	return err
}

// pass moves a unit whose candidate's checks passed to reviewing.
func (c *checkers) pass(ctx context.Context, stream config.WorkstreamID, unit string, state trace.WorkflowState, run UnitCheckRun) error {
	id := fmt.Sprintf("%s-%s-checks-%d-%d", trace.UnitSubject(unit), UnitReviewing, run.Run, state.Version)
	reason := fmt.Sprintf("check run %d passed on candidate %s from %s, recorded in %s: %s", run.Run, run.Candidate, run.Base, checkPath(unit, run.Run), run.Selection.describe())
	h := trace.Header{Schema: "osmia.trace.transition", Version: trace.Version, ID: id, Revision: 1, Project: c.repository.Project(), Workstream: stream, Unit: unit, At: c.s.now(), Actor: checksActor, Cause: checkDocumentID(unit, run.Run)}
	_, err := c.repository.Transact(ctx, trace.Transaction{ExpectedVersion: state.Version,
		Transition: trace.Transition{Header: h, Subject: trace.UnitSubject(unit), From: UnitChecking, To: UnitReviewing, Reason: reason},
		Events:     []trace.Event{trace.Notice(id, "unit", fmt.Sprintf("Unit %s is reviewing: %s.", unit, reason))}})
	if errors.Is(err, trace.ErrConflict) {
		return nil
	}
	return err
}

// fail records a unit's failed checks as a send-back in its review result,
// counted with the reviewer's toward shed.max_bounces, and moves the unit to
// implementing with the failures for its mason, or to contested once the
// send-backs reach the bound.
func (c *checkers) fail(ctx context.Context, stream config.WorkstreamID, unit string, state trace.WorkflowState, run UnitCheckRun) error {
	_, identity, err := c.candidateEvidence(ctx, stream, unit)
	if err != nil {
		if ctx.Err() != nil {
			return err
		}
		return c.block(ctx, stream, unit, fmt.Sprintf("unit %s stays checking: its checks failed, and its candidate's evidence cannot be read: %v", unit, err))
	}
	if !run.matches(identity) {
		return c.block(ctx, stream, unit, fmt.Sprintf("unit %s stays checking: check run %d does not match its candidate's diff", unit, run.Run))
	}
	docs, err := trace.Read[trace.Document](c.repository, stream)
	if err != nil {
		return err
	}
	result := UnitReviewResult{Identity: identity, Turn: checkDocumentID(unit, run.Run), Checks: run.Run, Verdict: checkVerdict(run)}
	var latest trace.Document
	for _, d := range docs {
		if d.ID == reviewDocument(unit) {
			latest = d
			var prior UnitReviewResult
			if json.Unmarshal([]byte(d.Content), &prior) == nil && prior.Verdict.Decision == "material_findings" && prior.Bounces > result.Bounces {
				result.Bounces = prior.Bounces
			}
		}
	}
	result.Bounces++
	content, err := json.MarshalIndent(result, "", "  ")
	if err != nil {
		return err
	}
	at := c.s.now()
	doc := trace.Document{Header: trace.Header{Schema: "osmia.trace.document", Version: trace.Version, ID: reviewDocument(unit), Revision: latest.Revision + 1, Project: c.repository.Project(), Workstream: stream, Unit: unit, At: at, Actor: checksActor, Cause: checkDocumentID(unit, run.Run)},
		Path: fmt.Sprintf("units/%s/review.json", unit), Content: string(content) + "\n"}
	to := UnitImplementing
	if result.Bounces >= c.cfg.Shed.MaxBounces {
		to = UnitContested
	}
	id := fmt.Sprintf("%s-%s-checks-%d-%d", trace.UnitSubject(unit), to, run.Run, state.Version)
	reason := fmt.Sprintf("check run %d failed on candidate %s from %s, recorded in %s: %s failed; %s", run.Run, run.Candidate, run.Base, checkPath(unit, run.Run), strings.Join(run.Failed, ", "), run.Selection.describe())
	if to == UnitContested {
		reason += fmt.Sprintf("; %d send-backs reached shed.max_bounces; the owner must rule review or revise before the unit moves", result.Bounces)
	}
	h := trace.Header{Schema: "osmia.trace.transition", Version: trace.Version, ID: id, Revision: 1, Project: c.repository.Project(), Workstream: stream, Unit: unit, At: at, Actor: checksActor, Cause: reviewDocument(unit)}
	tx := trace.Transaction{ExpectedVersion: state.Version, Transition: trace.Transition{Header: h, Subject: trace.UnitSubject(unit), From: UnitChecking, To: to, Reason: reason},
		Events: []trace.Event{trace.Notice(id, "unit", fmt.Sprintf("Unit %s checks failed: %s.", unit, reason))}}
	if _, err := c.repository.RecordDocumentsWith(ctx, []trace.Document{doc}, tx); errors.Is(err, trace.ErrConflict) {
		return nil
	} else if err != nil {
		return err
	}
	if to == UnitImplementing {
		return (&reviewers{masons: c.masons}).enqueueFindings(ctx, stream, unit, result)
	}
	return nil
}

// checkVerdict is the send-back a failed check run records: one finding for
// each failed check.
func checkVerdict(run UnitCheckRun) UnitVerdict {
	v := UnitVerdict{Decision: "material_findings", Summary: fmt.Sprintf("Check run %d failed on candidate %s: %s.", run.Run, run.Candidate, run.Selection.describe())}
	for _, link := range run.Failed {
		v.Findings = append(v.Findings, ReviewFinding{Severity: "blocking", Evidence: fmt.Sprintf("check %s failed in check run %d", link, run.Run), Action: fmt.Sprintf("make %s pass", link)})
	}
	return v
}

// incomplete tells the chief of staff, once, that a unit's check run did not
// complete.
func (c *checkers) incomplete(ctx context.Context, stream config.WorkstreamID, run UnitCheckRun) error {
	subject, err := c.repository.Workflow(stream, checksSubject(run.Unit))
	if err != nil {
		return err
	}
	to := fmt.Sprintf("incomplete-%d", run.Run)
	if subject.Value != fmt.Sprintf("requested-%d", run.Run) {
		return nil
	}
	id := checkDocumentID(run.Unit, run.Run) + "-incomplete"
	reason := fmt.Sprintf("check run %d on candidate %s of unit %s did not complete: %s; the unit stays checking and its checks run again after %s", run.Run, run.Candidate, run.Unit, run.Error, checkRetryDelay)
	h := trace.Header{Schema: "osmia.trace.transition", Version: trace.Version, ID: id, Revision: 1, Project: c.repository.Project(), Workstream: stream, Unit: run.Unit, At: c.s.now(), Actor: checksActor, Cause: checkDocumentID(run.Unit, run.Run)}
	_, err = c.repository.Transact(ctx, trace.Transaction{ExpectedVersion: subject.Version,
		Transition: trace.Transition{Header: h, Subject: checksSubject(run.Unit), From: subject.Value, To: to, Reason: reason},
		Events:     []trace.Event{trace.Notice(id, "chief", "Unit "+run.Unit+" is held in checking: "+reason+".")}})
	if errors.Is(err, trace.ErrConflict) {
		return nil
	}
	return err
}

func checkBlockedSubject(unit string) string { return "blocked-" + checksThread(unit) }

// block records, once for each reason, why a checking unit's checks cannot be
// requested, with a notice to the chief of staff.
func (c *checkers) block(ctx context.Context, stream config.WorkstreamID, unit, reason string) error {
	subject := checkBlockedSubject(unit)
	state, err := c.repository.Workflow(stream, subject)
	if err != nil {
		return err
	}
	transitions, err := trace.Read[trace.Transition](c.repository, stream)
	if err != nil {
		return err
	}
	if slices.ContainsFunc(transitions, func(t trace.Transition) bool {
		return t.Subject == subject && t.To == state.Value && t.Reason == reason
	}) {
		return nil
	}
	k := state.Version + 1
	id := fmt.Sprintf("%s-%d", subject, k)
	h := trace.Header{Schema: "osmia.trace.transition", Version: trace.Version, ID: id, Revision: 1, Project: c.repository.Project(), Workstream: stream, Unit: unit, At: c.s.now(), Actor: checksActor, Cause: reportDocument(unit)}
	_, err = c.repository.Transact(ctx, trace.Transaction{ExpectedVersion: state.Version,
		Transition: trace.Transition{Header: h, Subject: subject, From: state.Value, To: fmt.Sprintf("blocked-%d", k), Reason: reason},
		Events:     []trace.Event{trace.Notice(id, "chief", "The unit checks are blocked: "+reason+".")}})
	if errors.Is(err, trace.ErrConflict) {
		return nil
	}
	return err
}

func (c *checkers) decode(op coreadapter.Operation) (checkInput, error) {
	var in checkInput
	if op.Action != CheckAction || op.Boundary != coreadapter.RunnerBoundary {
		return in, errors.New("invalid check operation")
	}
	if err := json.Unmarshal(op.Input, &in); err != nil {
		return in, err
	}
	if in.Workstream == "" || in.Unit == "" || in.Run < 1 || in.Candidate == "" || in.Base == "" || in.Report < 1 {
		return in, errors.New("incomplete check operation")
	}
	return in, nil
}

// Inspect reports a check run recorded for the operation as completed.
func (c *checkers) Inspect(_ context.Context, op coreadapter.Operation) (coreadapter.Observation, error) {
	in, err := c.decode(op)
	if err != nil {
		return coreadapter.Observation{}, err
	}
	runs, err := checkRuns(c.repository, in.Workstream)
	if err != nil {
		return coreadapter.Observation{}, err
	}
	for _, r := range runs {
		if r.Unit == in.Unit && r.Run == in.Run {
			result := checkResult(r)
			return coreadapter.Observation{State: coreadapter.EffectCompleted, Evidence: result.Evidence, Result: &result}, nil
		}
	}
	return coreadapter.Observation{State: coreadapter.EffectAbsent, Evidence: "check run " + checkPath(in.Unit, in.Run) + " is not recorded"}, nil
}

// Apply runs the checks on a fresh export of the operation's candidate,
// bounded by the project's checks_timeout, and records the run. A failure to
// export or run them, or a run that outlasts the timeout, is an incomplete
// run; the service stopping leaves the operation to run again.
func (c *checkers) Apply(ctx context.Context, op coreadapter.Operation) (coreadapter.OperationResult, error) {
	in, err := c.decode(op)
	if err != nil {
		return coreadapter.OperationResult{}, err
	}
	run := UnitCheckRun{Unit: in.Unit, Run: in.Run, Operation: op.ID, Candidate: in.Candidate, Base: in.Base, Report: in.Report, ExitCode: -1, StartedAt: c.s.now()}
	incomplete := func(err error) (coreadapter.OperationResult, error) {
		if ctx.Err() != nil {
			return coreadapter.OperationResult{}, ctx.Err()
		}
		run.Status, run.Error = ChecksIncomplete, err.Error()
		return c.record(ctx, in, run)
	}
	if reason, err := c.superseded(in); err != nil {
		return coreadapter.OperationResult{}, err
	} else if reason != "" {
		run.Status, run.Error = ChecksSuperseded, reason
		return c.record(ctx, in, run)
	}
	checks := c.s.options.reviewChecks
	if checks == nil {
		return incomplete(errors.New("candidate check runner is unavailable"))
	}
	g, err := newUnitWorkspaces(c.cfg, c.repository).of(in.Workstream)
	if err != nil {
		return incomplete(err)
	}
	diff, err := g.Diff(ctx, in.Base, in.Candidate)
	if err != nil {
		return incomplete(fmt.Errorf("candidate diff: %w", err))
	}
	sum := sha256.Sum256([]byte(diff))
	run.DiffSHA256 = hex.EncodeToString(sum[:])
	base := filepath.Join(c.cfg.Root.String(), "checks")
	if err := os.MkdirAll(base, 0700); err != nil {
		return incomplete(err)
	}
	dir, err := os.MkdirTemp(base, "candidate-")
	if err != nil {
		return incomplete(err)
	}
	defer os.RemoveAll(dir)
	if err := g.Export(ctx, in.Candidate, dir); err != nil {
		return incomplete(fmt.Errorf("candidate export: %w", err))
	}
	selection := c.selectChecks(ctx, in, dir, diff, checks)
	run.Selection = &selection
	run.Command = append([]string{"dagger", "check", "--progress=report"}, selection.Links...)
	timeout := c.s.about(c.repository).Project.CheckTimeout()
	bounded, cancel := context.WithTimeout(ctx, timeout)
	result, err := checks.Check(bounded, dir, selection.Links)
	cancel()
	if ctx.Err() != nil {
		return coreadapter.OperationResult{}, ctx.Err()
	}
	if errors.Is(err, context.DeadlineExceeded) {
		err = fmt.Errorf("checks did not finish within checks_timeout %s", timeout)
	}
	run.ExitCode, run.Output, run.Truncated = result.ExitCode, result.Output, result.Truncated
	switch run.Failed = failedChecks(result.Output); {
	case err != nil:
		return incomplete(err)
	case result.ExitCode == 0:
		run.Status, run.Failed = ChecksPassed, nil
	case len(run.Failed) > 0:
		run.Status = ChecksFailed
	default:
		return incomplete(fmt.Errorf("dagger check exited %d without reporting a failed check", result.ExitCode))
	}
	return c.record(ctx, in, run)
}

// superseded returns why a run no longer checks its unit's candidate: the
// workstream was abandoned, the unit left checking, or its report names
// another candidate.
func (c *checkers) superseded(in checkInput) (string, error) {
	if gone, err := abandoned(c.repository, in.Workstream); err != nil || gone {
		if gone {
			return "the workstream is abandoned", nil
		}
		return "", err
	}
	state, err := c.repository.Workflow(in.Workstream, trace.UnitSubject(in.Unit))
	if err != nil {
		return "", err
	}
	if state.Value != UnitChecking {
		return fmt.Sprintf("unit %s is %s, not checking", in.Unit, state.Value), nil
	}
	report, _, err := unitReport(c.repository, in.Workstream, in.Unit)
	if err != nil {
		return "", err
	}
	if report.Candidate != in.Candidate || report.Base != in.Base {
		return fmt.Sprintf("unit %s's candidate is now %s from %s", in.Unit, report.Candidate, report.Base), nil
	}
	return "", nil
}

// record records the run as units/<unit>/checks-<run>.json and returns its
// operation result. A run already recorded is returned as it was recorded.
func (c *checkers) record(ctx context.Context, in checkInput, run UnitCheckRun) (coreadapter.OperationResult, error) {
	run.FinishedAt = c.s.now()
	data, err := json.MarshalIndent(run, "", "  ")
	if err != nil {
		return coreadapter.OperationResult{}, err
	}
	h := trace.Header{Schema: "osmia.trace.document", Version: trace.Version, ID: checkDocumentID(in.Unit, in.Run), Revision: 1, Project: c.repository.Project(), Workstream: in.Workstream, Unit: in.Unit, At: run.FinishedAt, Actor: checksActor, Cause: checkRequestID(in.Unit, in.Run)}
	err = c.repository.RecordDocuments(context.WithoutCancel(ctx), []trace.Document{{Header: h, Path: checkPath(in.Unit, in.Run), Content: string(data) + "\n"}})
	if errors.Is(err, trace.ErrConflict) {
		observed, err := c.Inspect(ctx, coreadapter.Operation{Boundary: coreadapter.RunnerBoundary, Action: CheckAction, Input: mustJSON(in)})
		if err != nil || observed.Result == nil {
			return coreadapter.OperationResult{}, errors.Join(errors.New("check run is recorded and unreadable"), err)
		}
		return *observed.Result, nil
	}
	if err != nil {
		return coreadapter.OperationResult{}, err
	}
	return checkResult(run), nil
}

func mustJSON(v any) json.RawMessage {
	data, _ := json.Marshal(v)
	return data
}

func checkResult(run UnitCheckRun) coreadapter.OperationResult {
	evidence := fmt.Sprintf("check run %d on candidate %s %s, recorded in %s", run.Run, run.Candidate, run.Status, checkPath(run.Unit, run.Run))
	if run.Error != "" {
		evidence += ": " + run.Error
	}
	return coreadapter.OperationResult{Outcome: run.Status, Evidence: evidence}
}

// checkRuns returns every check run recorded in the workstream, at its
// latest revision.
func checkRuns(repo *trace.Repository, stream config.WorkstreamID) ([]UnitCheckRun, error) {
	docs, err := trace.Read[trace.Document](repo, stream)
	if err != nil {
		return nil, err
	}
	latest := map[string]trace.Document{}
	var order []string
	for _, d := range docs {
		if !checkPathPattern.MatchString(d.Path) {
			continue
		}
		if _, seen := latest[d.ID]; !seen {
			order = append(order, d.ID)
		}
		if d.Revision > latest[d.ID].Revision {
			latest[d.ID] = d
		}
	}
	runs := make([]UnitCheckRun, 0, len(order))
	for _, id := range order {
		var run UnitCheckRun
		if err := json.Unmarshal([]byte(latest[id].Content), &run); err != nil {
			return nil, fmt.Errorf("%s: %w", latest[id].Path, err)
		}
		runs = append(runs, run)
	}
	return runs, nil
}

// checkOperation is a check operation of the workstream and whether its
// result is recorded.
type checkOperation struct {
	input checkInput
	done  bool
}

func checkOperations(repo *trace.Repository, stream config.WorkstreamID) ([]checkOperation, error) {
	records, err := repo.Operations(stream)
	if err != nil {
		return nil, err
	}
	var ops []checkOperation
	for _, r := range records {
		if r.Operation.Action != CheckAction {
			continue
		}
		var in checkInput
		if err := json.Unmarshal(r.Operation.Input, &in); err != nil {
			return nil, err
		}
		ops = append(ops, checkOperation{input: in, done: r.Result != nil})
	}
	return ops, nil
}

// latestRunFor returns the latest passed or failed check run of the
// identity's candidate, which is what its review receives.
func latestRunFor(repo *trace.Repository, stream config.WorkstreamID, unit string, identity UnitReviewIdentity) (UnitCheckRun, bool, error) {
	runs, err := checkRuns(repo, stream)
	if err != nil {
		return UnitCheckRun{}, false, err
	}
	var latest UnitCheckRun
	for _, r := range runs {
		if r.Unit == unit && (r.Status == ChecksPassed || r.Status == ChecksFailed) && r.matches(identity) && r.Run > latest.Run {
			latest = r
		}
	}
	return latest, latest.Run > 0, nil
}

// checkEvidence is how a review prompt carries a check run.
func checkEvidence(run UnitCheckRun) string {
	out := fmt.Sprintf("The service ran the project's Dagger checks on this exact candidate before review; you do not run them. Check run %d %s: %s.\nCommand: %s", run.Run, run.Status, run.Selection.describe(), strings.Join(quoteLinks(run.Command), " "))
	if len(run.Failed) > 0 {
		out += "\nFailed: " + strings.Join(run.Failed, ", ")
	}
	output := run.Output
	if over := len(output) - 16*1024; over > 0 {
		for over < len(output) && !utf8.RuneStart(output[over]) {
			over++
		}
		output = output[over:]
	}
	if output != "" {
		out += "\nThe end of its output:\n" + output
	}
	return out
}

// quoteLinks quotes the arguments of a command that a shell would split.
func quoteLinks(args []string) []string {
	out := make([]string, len(args))
	for i, a := range args {
		if strings.ContainsAny(a, "?&") {
			a = strconv.Quote(a)
		}
		out[i] = a
	}
	return out
}
