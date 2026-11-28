package execution

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

	"github.com/kpenfound/osmia/internal/api"
	checkrun "github.com/kpenfound/osmia/internal/checks"
	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/router"
	"github.com/kpenfound/osmia/internal/scheduler"
	"github.com/kpenfound/osmia/internal/trace"
)

// CheckAction is the runner operation that runs the checks of one unit
// candidate before review.
const CheckAction = "unit-checks"

const ChecksRole = "checks"

var ChecksActor = trace.Actor{Kind: "service", ID: ChecksRole}

const CheckRetryDelay = 10 * time.Minute

const MaxIncompleteChecks = 3

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
// the checks it selected, the command, its exit status, diagnostic excerpt
// and the path of its captured output. Failed lists the check links Dagger's
// report shows failed, and Flaky those that failed and then passed on their
// rerun. Engines says which engine ran each invocation and why a cloud run
// did; a record without it ran locally. Evidence is what the run shows about
// how it ran: the checks that ran together, their parallelism, the engine, the
// captured output and the failure reports of the flaky checks; FlakeEvidence
// returns it for any record, including one written without it.
type UnitCheckRun struct {
	Unit               string                `json:"unit"`
	Run                int                   `json:"run"`
	Operation          string                `json:"operation"`
	Candidate          string                `json:"candidate"`
	Base               string                `json:"base"`
	DiffSHA256         string                `json:"diff_sha256"`
	Report             int                   `json:"report"`
	Status             string                `json:"status"`
	Selection          *CheckSelection       `json:"selection,omitempty"`
	Command            []string              `json:"command,omitempty"`
	ExitCode           int                   `json:"exit_code"`
	Output             string                `json:"output,omitempty"`
	OutputRevision     int                   `json:"output_revision,omitempty"`
	OutputPath         string                `json:"output_path,omitempty"`
	Truncated          bool                  `json:"truncated,omitempty"`
	Failed             []string              `json:"failed,omitempty"`
	Flaky              []string              `json:"flaky,omitempty"`
	TraceURL           string                `json:"trace_url,omitempty"`
	CIIdentityFallback string                `json:"ci_identity_fallback,omitempty"`
	Engines            *api.CheckAttribution `json:"engines,omitempty"`
	Evidence           *api.CheckEvidence    `json:"evidence,omitempty"`
	Error              string                `json:"error,omitempty"`
	StartedAt          time.Time             `json:"started_at"`
	FinishedAt         time.Time             `json:"finished_at"`
}

// FlakeEvidence converts the record to the neutral evidence of its run, with
// the run's flaky checks. A record written without evidence yields what its
// other fields hold, and marks the parallelism and any failure report it did
// not keep as unavailable, to be read in the record.
func (r UnitCheckRun) FlakeEvidence() api.CheckEvidence {
	var evidence api.CheckEvidence
	if r.Evidence != nil {
		evidence = *r.Evidence
	} else {
		evidence = checkrun.RecordedEvidence(checkrun.RecordedLocalRun{Selection: r.Selection, Engines: r.Engines, Output: r.Output, OutputPath: r.OutputPath, OutputRevision: r.OutputRevision, TraceURL: r.TraceURL, Record: CheckPath(r.Unit, r.Run)}, r.Flaky)
	}
	evidence.Engines, evidence.Flaky = r.Engines, slices.Clone(r.Flaky)
	return evidence
}

func (r UnitCheckRun) Matches(identity UnitReviewIdentity) bool {
	return r.Candidate == identity.Candidate.Revision && r.Base == identity.Candidate.BaseRevision && r.DiffSHA256 == identity.DiffSHA256
}

func unitReport(repo *trace.Repository, stream config.WorkstreamID, unit string) (UnitReport, int, error) {
	docs, err := trace.Read[trace.Document](repo, stream)
	if err != nil {
		return UnitReport{}, 0, err
	}
	var latest trace.Document
	for _, d := range docs {
		if d.ID == ReportDocument(unit) && d.Revision > latest.Revision {
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

func CheckDocumentID(unit string, run int) string {
	return fmt.Sprintf("%s-checks-%d", trace.UnitSubject(unit), run)
}

func checkRequestID(unit string, run int) string { return CheckDocumentID(unit, run) + "-requested" }

func CheckPath(unit string, run int) string { return fmt.Sprintf("units/%s/checks-%d.json", unit, run) }

var CheckPathPattern = regexp.MustCompile(`^units/[^/]+/checks-[0-9]+\.json$`)

func ChecksSubject(unit string) string { return trace.UnitSubject(unit) + "-checks" }

func checksThread(unit string) string {
	return ChecksRole + strings.TrimPrefix(trace.UnitSubject(unit), "unit")
}

func (c *checkers) Scope(in checkInput) coreadapter.Scope {
	return coreadapter.Scope{Project: string(c.repository.Project()), Workstream: string(in.Workstream), Unit: in.Unit, Thread: checksThread(in.Unit), Turn: CheckDocumentID(in.Unit, in.Run), Role: ChecksRole}
}

type checkers struct{ *masons }

var _ coreadapter.Reconciler = (*checkers)(nil)

func (c *checkers) Pass(ctx context.Context) error {
	streams, err := nonSessionWorkstreams(c.repository)
	if err != nil {
		return err
	}
	state, _ := c.s.Effective()
	for _, stream := range streams {
		if session, err := isSession(c.repository, stream); err != nil {
			return err
		} else if session {
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
		runs, err := CheckRuns(c.repository, stream)
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
			return false, c.Fail(ctx, stream, unit, state, *latest)
		case ChecksIncomplete:
			if err := c.Incomplete(ctx, stream, *latest); err != nil {
				return false, err
			}
			if n, err := c.IncompleteSinceEntry(stream, unit, *latest, runs); err != nil {
				return false, err
			} else if n >= MaxIncompleteChecks {
				return false, c.contestIncomplete(ctx, stream, unit, state, *latest, n)
			}
			if c.s.Now().Before(latest.FinishedAt.Add(CheckRetryDelay)) {
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

func sinceMove(transitions []trace.Transition, runs []UnitCheckRun, unit string) []UnitCheckRun {
	moved := -1
	for i, t := range transitions {
		if IsMove(t, unit) && t.To == UnitChecking {
			moved = i
		}
	}
	if moved < 0 {
		return runs
	}
	requested := map[string]bool{}
	for _, t := range transitions[moved+1:] {
		if t.Subject == ChecksSubject(unit) {
			requested[t.ID] = true
		}
	}
	return slices.DeleteFunc(slices.Clone(runs), func(r UnitCheckRun) bool {
		return r.Unit == unit && !requested[checkRequestID(unit, r.Run)]
	})
}

func (c *checkers) request(ctx context.Context, stream config.WorkstreamID, in checkInput) error {
	data, err := json.Marshal(in)
	if err != nil {
		return err
	}
	subject, err := c.repository.Workflow(stream, ChecksSubject(in.Unit))
	if err != nil {
		return err
	}
	id := checkRequestID(in.Unit, in.Run)
	event := trace.EventID(id, "run")
	op := coreadapter.Operation{ID: trace.OperationID(c.repository.Project(), stream, event), Boundary: coreadapter.RunnerBoundary, Action: CheckAction, Input: data}
	h := trace.Header{Schema: "osmia.trace.transition", Version: trace.Version, ID: id, Revision: 1, Project: c.repository.Project(), Workstream: stream, Unit: in.Unit, At: c.s.Now(), Actor: ChecksActor, Cause: ReportDocument(in.Unit)}
	reason := fmt.Sprintf("run %d checks candidate %s of unit %s, from %s, before review", in.Run, in.Candidate, in.Unit, in.Base)
	_, err = c.repository.Transact(ctx, trace.Transaction{ExpectedVersion: subject.Version,
		Transition: trace.Transition{Header: h, Subject: ChecksSubject(in.Unit), From: subject.Value, To: fmt.Sprintf("requested-%d", in.Run), Reason: reason},
		Events:     []trace.Event{{ID: event, Kind: CheckAction, Body: fmt.Sprintf("Run the checks of unit %s", in.Unit), Operation: &op}}})
	if errors.Is(err, trace.ErrConflict) {
		return nil
	}
	return err
}

func (c *checkers) pass(ctx context.Context, stream config.WorkstreamID, unit string, state trace.WorkflowState, run UnitCheckRun) error {
	id := fmt.Sprintf("%s-%s-checks-%d-%d", trace.UnitSubject(unit), UnitReviewing, run.Run, state.Version)
	reason := fmt.Sprintf("check run %d passed on candidate %s from %s, recorded in %s: %s", run.Run, run.Candidate, run.Base, CheckPath(unit, run.Run), checkrun.DescribeSelectionRun(*run.Selection, run.Engines))
	if flaky := checkrun.DescribeFlaky(run.Flaky); flaky != "" {
		reason += "; " + flaky
	}
	if open := c.openFlakeFixes(run); open != "" {
		reason += "; " + open
	}
	_, err := c.s.raise(ctx, true, UnitExitInput{
		Repo: c.repository, Stream: stream, Unit: unit, State: state, Trigger: TriggerChecksPassed, Cause: CheckDocumentID(unit, run.Run),
		Default: UnitReviewing, Targets: []string{UnitReviewing, UnitChecking}, Actor: ChecksActor,
		ID: id, HeaderCause: CheckDocumentID(unit, run.Run), Reason: reason, Events: []trace.Event{trace.Progress(id, "unit", fmt.Sprintf("Unit %s is reviewing: %s.", unit, reason))},
		Note:     reason,
		Evidence: router.Evidence{Facts: map[string]any{"run": run.Run, "flaky": len(run.Flaky) > 0}, Records: []router.Entry{{Kind: "check run", ID: CheckDocumentID(unit, run.Run), Text: reason}}},
	})
	return err
}

func (c *checkers) Fail(ctx context.Context, stream config.WorkstreamID, unit string, state trace.WorkflowState, run UnitCheckRun) error {
	_, identity, err := c.CandidateEvidence(ctx, stream, unit)
	if err != nil {
		if ctx.Err() != nil {
			return err
		}
		return c.block(ctx, stream, unit, fmt.Sprintf("unit %s stays checking: its checks failed, and its candidate's evidence cannot be read: %v", unit, err))
	}
	if !run.Matches(identity) {
		return c.block(ctx, stream, unit, fmt.Sprintf("unit %s stays checking: check run %d does not match its candidate's diff", unit, run.Run))
	}
	docs, err := trace.Read[trace.Document](c.repository, stream)
	if err != nil {
		return err
	}
	result := UnitReviewResult{Identity: identity, Turn: CheckDocumentID(unit, run.Run), Checks: run.Run, Verdict: checkVerdict(run)}
	var latest trace.Document
	for _, d := range docs {
		if d.ID == ReviewDocument(unit) {
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
	at := c.s.Now()
	doc := trace.Document{Header: trace.Header{Schema: "osmia.trace.document", Version: trace.Version, ID: ReviewDocument(unit), Revision: latest.Revision + 1, Project: c.repository.Project(), Workstream: stream, Unit: unit, At: at, Actor: ChecksActor, Cause: CheckDocumentID(unit, run.Run)},
		Path: fmt.Sprintf("units/%s/review.json", unit), Content: string(content) + "\n"}
	to := UnitImplementing
	if result.Bounces >= c.cfg.Shed.MaxBounces {
		to = UnitContested
	}
	id := fmt.Sprintf("%s-%s-checks-%d-%d", trace.UnitSubject(unit), to, run.Run, state.Version)
	reason := fmt.Sprintf("check run %d failed on candidate %s from %s, recorded in %s: %s failed; %s", run.Run, run.Candidate, run.Base, CheckPath(unit, run.Run), checkrun.CheckExcerpt(strings.Join(run.Failed, ", "), 2048), checkrun.DescribeSelectionRun(*run.Selection, run.Engines))
	if flaky := checkrun.DescribeFlaky(run.Flaky); flaky != "" {
		reason += "; " + flaky
	}
	if to == UnitContested {
		reason += fmt.Sprintf("; %d send-backs reached shed.max_bounces; the owner must rule review or revise before the unit moves", result.Bounces)
	}
	res, err := c.s.raise(ctx, true, UnitExitInput{
		Repo: c.repository, Stream: stream, Unit: unit, State: state, Trigger: TriggerChecksFailed, Cause: CheckDocumentID(unit, run.Run),
		Default: to, Targets: []string{UnitImplementing, UnitChecking, UnitContested}, Actor: ChecksActor,
		ID: id, HeaderCause: ReviewDocument(unit), Reason: reason, Events: []trace.Event{trace.Notice(id, "unit", fmt.Sprintf("Unit %s checks failed: %s.", unit, reason))},
		Note: reason, Documents: []trace.Document{doc},
		Evidence: router.Evidence{Facts: map[string]any{"run": run.Run, "send_backs": result.Bounces, "max_send_backs": c.cfg.Shed.MaxBounces, "flaky": len(run.Flaky) > 0},
			Records: []router.Entry{{Kind: "check run", ID: CheckDocumentID(unit, run.Run), Text: reason}}},
	})
	if err == nil && sentBack(res) {
		return (&reviewers{masons: c.masons}).enqueueFindings(ctx, stream, unit, result)
	}
	return err
}

func checkVerdict(run UnitCheckRun) UnitVerdict {
	v := UnitVerdict{Decision: "material_findings", Summary: fmt.Sprintf("Check run %d failed on candidate %s: %s.", run.Run, run.Candidate, checkrun.DescribeSelectionRun(*run.Selection, run.Engines))}
	for _, link := range run.Failed {
		v.Findings = append(v.Findings, ReviewFinding{Severity: "blocking", Evidence: fmt.Sprintf("check %s failed in %s", checkrun.CheckLinkLabel(link), CheckPath(run.Unit, run.Run)), Action: fmt.Sprintf("make %s pass", checkrun.CheckLinkLabel(link))})
	}
	return v
}

func (c *checkers) IncompleteSinceEntry(stream config.WorkstreamID, unit string, latest UnitCheckRun, runs []UnitCheckRun) (int, error) {
	transitions, err := trace.Read[trace.Transition](c.repository, stream)
	if err != nil {
		return 0, err
	}
	entered := -1
	for i, t := range transitions {
		if t.Subject == trace.UnitSubject(unit) && t.To == UnitChecking && t.From != UnitChecking {
			entered = i
		}
	}
	requested := map[string]bool{}
	for _, t := range transitions[entered+1:] {
		if t.Subject == ChecksSubject(unit) {
			requested[t.ID] = true
		}
	}
	n := 0
	for _, r := range runs {
		if r.Unit == unit && r.Status == ChecksIncomplete && r.Candidate == latest.Candidate && r.Base == latest.Base && requested[checkRequestID(unit, r.Run)] {
			n++
		}
	}
	return n, nil
}

func (c *checkers) contestIncomplete(ctx context.Context, stream config.WorkstreamID, unit string, state trace.WorkflowState, latest UnitCheckRun, n int) error {
	id := CheckDocumentID(unit, latest.Run) + "-contested"
	reason := fmt.Sprintf("%d check runs in a row on candidate %s of unit %s did not complete, the latest because %s", n, latest.Candidate, unit, latest.Error)
	if cloud := checkrun.DescribeAttribution(latest.Engines); cloud != "" {
		reason += "; " + strings.TrimSuffix(cloud, ".")
	}
	reason += "; the unit is contested instead of its checks running again"
	_, err := c.s.raise(ctx, true, UnitExitInput{
		Repo: c.repository, Stream: stream, Unit: unit, State: state, Trigger: TriggerChecksIncomplete, Cause: CheckDocumentID(unit, latest.Run),
		Default: UnitContested, Targets: []string{UnitContested}, Actor: ChecksActor,
		ID: id, HeaderCause: CheckDocumentID(unit, latest.Run), Reason: reason, Events: []trace.Event{trace.Notice(id, "unit", fmt.Sprintf("Unit %s is contested: %s.", unit, reason))},
		Note:     reason,
		Evidence: router.Evidence{Facts: map[string]any{"incomplete_runs": n, "max_incomplete_runs": MaxIncompleteChecks}, Records: []router.Entry{{Kind: "check run", ID: CheckDocumentID(unit, latest.Run), Text: reason}}},
	})
	return err
}

func (c *checkers) Incomplete(ctx context.Context, stream config.WorkstreamID, run UnitCheckRun) error {
	subject, err := c.repository.Workflow(stream, ChecksSubject(run.Unit))
	if err != nil {
		return err
	}
	to := fmt.Sprintf("incomplete-%d", run.Run)
	if subject.Value != fmt.Sprintf("requested-%d", run.Run) {
		return nil
	}
	id := CheckDocumentID(run.Unit, run.Run) + "-incomplete"
	reason := fmt.Sprintf("check run %d on candidate %s of unit %s did not complete: %s", run.Run, run.Candidate, run.Unit, run.Error)
	if cloud := checkrun.DescribeAttribution(run.Engines); cloud != "" {
		reason += "; " + strings.TrimSuffix(cloud, ".")
	}
	reason += fmt.Sprintf("; the unit stays checking and its checks run again after %s", CheckRetryDelay)
	h := trace.Header{Schema: "osmia.trace.transition", Version: trace.Version, ID: id, Revision: 1, Project: c.repository.Project(), Workstream: stream, Unit: run.Unit, At: c.s.Now(), Actor: ChecksActor, Cause: CheckDocumentID(run.Unit, run.Run)}
	_, err = c.repository.Transact(ctx, trace.Transaction{ExpectedVersion: subject.Version,
		Transition: trace.Transition{Header: h, Subject: ChecksSubject(run.Unit), From: subject.Value, To: to, Reason: reason},
		Events:     []trace.Event{trace.Notice(id, trace.ManagerKey, "Unit "+run.Unit+" is held in checking: "+reason+".")}})
	if errors.Is(err, trace.ErrConflict) {
		return nil
	}
	return err
}

func CheckBlockedSubject(unit string) string { return "blocked-" + checksThread(unit) }

func (c *checkers) block(ctx context.Context, stream config.WorkstreamID, unit, reason string) error {
	subject := CheckBlockedSubject(unit)
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
	h := trace.Header{Schema: "osmia.trace.transition", Version: trace.Version, ID: id, Revision: 1, Project: c.repository.Project(), Workstream: stream, Unit: unit, At: c.s.Now(), Actor: ChecksActor, Cause: ReportDocument(unit)}
	_, err = c.repository.Transact(ctx, trace.Transaction{ExpectedVersion: state.Version,
		Transition: trace.Transition{Header: h, Subject: subject, From: state.Value, To: fmt.Sprintf("blocked-%d", k), Reason: reason},
		Events:     []trace.Event{trace.Notice(id, trace.ManagerKey, "The unit checks are blocked: "+reason+".")}})
	if errors.Is(err, trace.ErrConflict) {
		return nil
	}
	return err
}

func (c *checkers) Decode(op coreadapter.Operation) (checkInput, error) {
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

func (c *checkers) Inspect(_ context.Context, op coreadapter.Operation) (coreadapter.Observation, error) {
	in, err := c.Decode(op)
	if err != nil {
		return coreadapter.Observation{}, err
	}
	runs, err := CheckRuns(c.repository, in.Workstream)
	if err != nil {
		return coreadapter.Observation{}, err
	}
	for _, r := range runs {
		if r.Unit == in.Unit && r.Run == in.Run {
			result := checkResult(r)
			return coreadapter.Observation{State: coreadapter.EffectCompleted, Evidence: result.Evidence, Result: &result}, nil
		}
	}
	return coreadapter.Observation{State: coreadapter.EffectAbsent, Evidence: "check run " + CheckPath(in.Unit, in.Run) + " is not recorded"}, nil
}

func (c *checkers) Apply(ctx context.Context, op coreadapter.Operation) (coreadapter.OperationResult, error) {
	in, err := c.Decode(op)
	if err != nil {
		return coreadapter.OperationResult{}, err
	}
	var captured string
	run := UnitCheckRun{Unit: in.Unit, Run: in.Run, Operation: op.ID, Candidate: in.Candidate, Base: in.Base, Report: in.Report, ExitCode: -1, StartedAt: c.s.Now()}
	if _, err := c.restoreSelection(in, &run); err != nil {
		return coreadapter.OperationResult{}, err
	}
	incomplete := func(err error) (coreadapter.OperationResult, error) {
		if ctx.Err() != nil {
			return coreadapter.OperationResult{}, ctx.Err()
		}
		run.Status, run.Error = ChecksIncomplete, err.Error()
		return c.record(ctx, in, run, captured)
	}
	if reason, err := c.superseded(in); err != nil {
		return coreadapter.OperationResult{}, err
	} else if reason != "" {
		run.Status, run.Error = ChecksSuperseded, reason
		return c.record(ctx, in, run, captured)
	}
	checks, cloud := c.s.Checks, c.s.CloudChecks
	if checks == nil {
		return incomplete(errors.New("candidate check runner is unavailable"))
	}
	g, err := NewUnitWorkspaces(c.cfg, c.repository).Of(in.Workstream)
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
	release, err := c.s.AcquireCheckSlot(ctx, c.repository.Project(), in.Workstream, in.Unit)
	if err != nil {
		return coreadapter.OperationResult{}, err
	}
	defer release()
	if reason, err := c.superseded(in); err != nil {
		return coreadapter.OperationResult{}, err
	} else if reason != "" {
		run.Status, run.Error = ChecksSuperseded, reason
		return c.record(ctx, in, run, captured)
	}
	run.StartedAt = c.s.Now()
	selection, err := c.selection(ctx, in, &run, dir, diff, checks)
	if err != nil {
		return coreadapter.OperationResult{}, err
	}
	run.Selection = &selection
	run.Command = checkrun.Command(selection.Links)
	timeout := c.s.About(c.repository).Project.CheckTimeout()
	var remembered checkrun.Remembered
	if cloud != nil {
		if remembered, err = checkrun.RememberedIn(c.repository, in.Workstream); err != nil {
			return coreadapter.OperationResult{}, err
		}
	}
	traces := c.watchTraces(ctx, in, run)
	result, ran, engines, err := checkrun.RunSelectionWithFallback(traces.observed(ctx), checks, cloud, timeout, remembered, dir, c.checkIdentity(in), selection)
	traces.stop()
	if ctx.Err() != nil {
		return coreadapter.OperationResult{}, ctx.Err()
	}
	if errors.Is(err, context.DeadlineExceeded) {
		err = fmt.Errorf("checks did not finish within checks_timeout %s", timeout)
	}
	run.Selection, run.Command, run.Engines = &ran, checkrun.ResultCommand(result, ran.Links), engines
	if result.TraceURL != "" {
		run.TraceURL = result.TraceURL
	}
	run.CIIdentityFallback = result.CIIdentityFallback
	run.Flaky = result.Flaky
	captured, run.ExitCode = result.Output, result.ExitCode
	run.Output, run.Truncated = checkrun.SummarizeCheckOutput(result.Output, result.ExitCode)
	run.Truncated = run.Truncated || result.Truncated
	run.OutputPath = checkrun.CheckOutputPath(CheckPath(in.Unit, in.Run))
	run.OutputRevision = 1
	evidence := checkrun.LocalEvidence(ran, engines, result.Output, run.OutputPath, run.OutputRevision, run.TraceURL)
	evidence.Engines = nil
	run.Evidence = &evidence
	if run.Status, run.Failed, err = checkrun.ClassifyChecks(result, err); run.Status == ChecksIncomplete {
		return incomplete(err)
	}
	if run.Status == ChecksPassed {
		if err := c.rerunDeclared(ctx, in, g); err != nil && ctx.Err() != nil {
			return coreadapter.OperationResult{}, ctx.Err()
		}
	}
	return c.record(ctx, in, run, captured)
}

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

func (c *checkers) record(ctx context.Context, in checkInput, run UnitCheckRun, captured string) (coreadapter.OperationResult, error) {
	run.FinishedAt = c.s.Now()
	data, err := json.MarshalIndent(run, "", "  ")
	if err != nil {
		return coreadapter.OperationResult{}, err
	}
	h := trace.Header{Schema: "osmia.trace.document", Version: trace.Version, ID: CheckDocumentID(in.Unit, in.Run), Revision: 1, Project: c.repository.Project(), Workstream: in.Workstream, Unit: in.Unit, At: run.FinishedAt, Actor: ChecksActor, Cause: checkRequestID(in.Unit, in.Run)}
	docs := []trace.Document{{Header: h, Path: CheckPath(in.Unit, in.Run), Content: string(data) + "\n"}}
	if run.OutputPath != "" {
		h.ID += "-output"
		docs = append(docs, trace.Document{Header: h, Path: run.OutputPath, Content: captured})
	}
	err = c.repository.RecordDocuments(context.WithoutCancel(ctx), docs)
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
	evidence := fmt.Sprintf("check run %d on candidate %s %s, recorded in %s", run.Run, run.Candidate, run.Status, CheckPath(run.Unit, run.Run))
	if run.Error != "" {
		evidence += ": " + run.Error
	}
	return coreadapter.OperationResult{Outcome: run.Status, Evidence: evidence}
}

func CheckRuns(repo *trace.Repository, stream config.WorkstreamID) ([]UnitCheckRun, error) {
	docs, err := trace.Read[trace.Document](repo, stream)
	if err != nil {
		return nil, err
	}
	latest := map[string]trace.Document{}
	var order []string
	for _, d := range docs {
		if !CheckPathPattern.MatchString(d.Path) {
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

func LatestRunFor(repo *trace.Repository, stream config.WorkstreamID, unit string, identity UnitReviewIdentity) (UnitCheckRun, bool, error) {
	runs, err := CheckRuns(repo, stream)
	if err != nil {
		return UnitCheckRun{}, false, err
	}
	var latest UnitCheckRun
	for _, r := range runs {
		if r.Unit == unit && (r.Status == ChecksPassed || r.Status == ChecksFailed) && r.Matches(identity) && r.Run > latest.Run {
			latest = r
		}
	}
	return latest, latest.Run > 0, nil
}

func checkEvidence(run UnitCheckRun) string {
	out := fmt.Sprintf("The service ran the project's Dagger checks on this exact candidate before review; you do not run them. Check run %d %s: %s.\nCommand: %s", run.Run, run.Status, checkrun.DescribeSelectionRun(*run.Selection, run.Engines), checkrun.CheckExcerpt(strings.Join(QuoteLinks(run.Command), " "), 2048))
	if len(run.Failed) > 0 {
		out += "\nFailed: " + checkrun.CheckExcerpt(strings.Join(run.Failed, ", "), 2048)
	}
	if flaky := checkrun.DescribeFlaky(run.Flaky); flaky != "" {
		out += "\n" + flaky
	}
	return out + checkrun.OutputEvidence(run.Output, run.OutputPath, run.OutputRevision, run.Truncated)
}

func QuoteLinks(args []string) []string {
	out := make([]string, len(args))
	for i, a := range args {
		if a != "&&" && strings.ContainsAny(a, "?&") {
			a = strconv.Quote(a)
		}
		out[i] = a
	}
	return out
}
