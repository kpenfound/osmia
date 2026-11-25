package climb

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/kpenfound/osmia/internal/api"
	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/kinds"
	"github.com/kpenfound/osmia/internal/trace"
)

// MasonRequest asks the host for the state of attempt n's mason turns. The
// host queues the next turn on the climb's one mason thread when none is
// queued, and carries the brief in the turn's bundle.
type MasonRequest struct {
	Stream  config.WorkstreamID
	Journal Journal
	Brief   Brief
}

// AttemptReport is what the mason's done report says about an attempt.
type AttemptReport struct {
	Hypothesis string
	Change     string
	Expected   string
}

// MasonResult is how attempt n's mason turns stand: still working, done with a
// report, or out of clean turns without one, which discards the attempt as
// having no candidate.
type MasonResult struct {
	Pending     bool
	NoCandidate string
	Report      *AttemptReport
}

// ReviewRequest asks the host for the reviewer's verdict on a kept candidate.
type ReviewRequest struct {
	Stream  config.WorkstreamID
	Journal Journal
	Brief   Brief
	// Diff is the candidate's change against the parent.
	Diff string
}

// ReviewResult is how the review stands: still running, or the verdict.
type ReviewResult struct {
	Pending bool
	Review  Review
}

// GuardrailRequest asks the host to run the guardrail checks on a candidate.
type GuardrailRequest struct {
	Stream    config.WorkstreamID
	Number    int
	Candidate string
	Checks    []string
}

// Host is what the loop asks of the service's machinery: the climb's clone, the
// attempt workspaces, the mason and reviewer turns and the guardrail checks.
type Host interface {
	Source
	// Branch returns the commit a branch points at.
	Branch(ctx context.Context, name string) (commit string, exists bool, err error)
	// ChangedPaths lists the paths candidate changes against base, and Diff
	// renders the change.
	ChangedPaths(ctx context.Context, base, candidate string) ([]string, error)
	Diff(ctx context.Context, base, candidate string) (string, error)
	// Squash makes the one commit of candidate's tree on base. The same
	// arguments make the same commit and no branch moves.
	Squash(ctx context.Context, base, candidate, message string, at time.Time) (string, error)
	// Open builds the workspace of attempt n from parent, on the attempt's own
	// branch, and returns the branch. Opening it again changes nothing.
	Open(ctx context.Context, n int, parent string) (branch string, err error)
	// Capture takes the workspace of attempt n as a commit on its branch and
	// returns it. A workspace that holds no change returns parent.
	Capture(ctx context.Context, n int, parent string) (candidate string, err error)
	// Drop removes the workspace of attempt n and leaves its branch.
	Drop(ctx context.Context, n int) error
	// Advance moves the climb branch from one commit to another, which is a
	// no-op when it is already there.
	Advance(ctx context.Context, key, from, to string) error
	Mason(ctx context.Context, req MasonRequest) (MasonResult, error)
	Review(ctx context.Context, req ReviewRequest) (ReviewResult, error)
	Guardrails(ctx context.Context, req GuardrailRequest) (Guardrails, error)
}

// StopRequest asks the service to raise the climb.stop exit after attempt
// After, for the firing rules.
type StopRequest struct {
	Stream  config.WorkstreamID
	After   int
	Firings []Firing
	State   trace.WorkflowState
}

// StopOutcome is how a climb.stop exit ended.
type StopOutcome string

const (
	// StopContinue: the exit let the climb go on past the rules.
	StopContinue StopOutcome = "continue"
	// StopConclude: the exit moved the climb to concluding.
	StopConclude StopOutcome = "conclude"
	// StopWait: the exit was held, and the climb waits for a decision.
	StopWait StopOutcome = "wait"
)

// Loop runs the climbing state of a Hill Climb workstream, one attempt at a
// time. Every phase of an attempt is one commit of the workstream's trace that
// also holds the records the phase rests on, so a restart reads the phase back
// and reconciles it against the climb branch, the attempt's branch and the
// trace before it retries a side effect.
type Loop struct {
	Repository *trace.Repository
	// Spec returns the sealed climb spec and the revision that was sealed.
	Spec func(stream config.WorkstreamID) (Spec, int, error)
	// Branch names the climb branch.
	Branch func(stream config.WorkstreamID) string
	// Host returns the machinery of the workstream.
	Host func(stream config.WorkstreamID) (Host, error)
	// Runner returns the measurement runner that exports commits from source.
	Runner func(stream config.WorkstreamID, source Source) (Runner, error)
	// Spent returns what the climb cost since it started, in dollars.
	Spent func(stream config.WorkstreamID, since time.Time) (float64, error)
	// Allow returns why no attempt may start now, such as the daily budget or
	// the loop guard, or "" when one may.
	Allow func(ctx context.Context, stream config.WorkstreamID) (string, error)
	// Stop raises the climb.stop exit.
	Stop func(ctx context.Context, req StopRequest) (StopOutcome, error)
	// Publish announces a climb event to the clients of the event stream.
	Publish func(kind api.EventKind, stream config.WorkstreamID)
	Now     func() time.Time
	// Step is a hook between a side effect and its record; a failure aborts
	// the pass as a crash would.
	Step func(name string) error
}

// NoCandidateStall is how many attempts in a row may end without a candidate
// before the chief of staff is told the climb is stalled.
const NoCandidateStall = 3

const maxSteps = 64

type cycle struct {
	stream     config.WorkstreamID
	spec       Spec
	approval   ApprovalRecord
	host       Host
	runner     Runner
	branch     string
	concluding bool
}

// Reconcile moves the climb as far as it can without waiting: it finishes the
// open attempt from the phase the trace records, evaluates the stop rules after
// it, and starts the next attempt. It does nothing for a workstream that is not
// building, and for a climb that is concluding it only settles an attempt that
// was in flight.
func (l *Loop) Reconcile(ctx context.Context, stream config.WorkstreamID) error {
	feature, err := l.Repository.Workflow(stream, trace.FeatureSubject)
	if err != nil || feature.Value != featureBuilding {
		return err
	}
	inner, err := ReadInner(l.Repository, stream)
	if err != nil {
		return err
	}
	if inner.Value != InnerClimbing && inner.Value != InnerConcluding {
		return nil
	}
	c, err := l.begin(stream, inner.Value == InnerConcluding)
	if err != nil {
		return err
	}
	for range maxSteps {
		again, err := l.step(ctx, c)
		if errors.Is(err, errMoved) {
			return nil
		}
		if err != nil || !again {
			return err
		}
	}
	return nil
}

func (l *Loop) begin(stream config.WorkstreamID, concluding bool) (*cycle, error) {
	spec, _, err := l.Spec(stream)
	if err != nil {
		return nil, err
	}
	approval, ok, err := ReadApproval(l.Repository, stream)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, fmt.Errorf("workstream %s is climbing without the owner's approval record", stream)
	}
	host, err := l.Host(stream)
	if err != nil {
		return nil, err
	}
	runner, err := l.Runner(stream, host)
	if err != nil {
		return nil, err
	}
	return &cycle{stream: stream, spec: spec, approval: approval, host: host, runner: runner, branch: l.Branch(stream), concluding: concluding}, nil
}

func (l *Loop) step(ctx context.Context, c *cycle) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	open, found, err := OpenAttempt(l.Repository, c.stream)
	if err != nil {
		return false, err
	}
	if found {
		return l.advance(ctx, c, open)
	}
	if c.concluding {
		return false, nil
	}
	return l.next(ctx, c)
}

func (l *Loop) next(ctx context.Context, c *cycle) (bool, error) {
	attempts, err := ReadAttempts(l.Repository, c.stream)
	if err != nil {
		return false, err
	}
	if len(attempts) > 0 {
		if proceed, err := l.stops(ctx, c, attempts); err != nil || !proceed {
			return false, err
		}
	}
	if reason, err := l.allowed(ctx, c.stream); err != nil || reason != "" {
		return false, err
	}
	return true, l.start(ctx, c, attempts)
}

func (l *Loop) allowed(ctx context.Context, stream config.WorkstreamID) (string, error) {
	if l.Allow == nil {
		return "", nil
	}
	return l.Allow(ctx, stream)
}

func (l *Loop) stops(ctx context.Context, c *cycle, attempts []Attempt) (bool, error) {
	budget, err := ReadBudget(l.Repository, c.stream)
	if err != nil {
		return false, err
	}
	continued, err := ReadStops(l.Repository, c.stream)
	if err != nil {
		return false, err
	}
	var spent float64
	if l.Spent != nil {
		if spent, err = l.Spent(c.stream, c.approval.At); err != nil {
			return false, err
		}
	}
	in := StopInput{Rules: c.spec.Stop, Direction: c.spec.Metric.Direction, Baseline: c.approval.Baseline, Attempts: attempts, Extension: budget.Total(),
		Spent: spent, Started: c.approval.At, Now: l.now()}
	for _, rec := range continued {
		in.Continued = append(in.Continued, rec)
	}
	firings := EvaluateStops(in)
	if len(firings) == 0 {
		return true, nil
	}
	state, err := ReadInner(l.Repository, c.stream)
	if err != nil || state.Value != InnerClimbing {
		return false, err
	}
	after := attempts[len(attempts)-1].Number
	out, err := l.Stop(ctx, StopRequest{Stream: c.stream, After: after, Firings: firings, State: state})
	if err != nil {
		return false, err
	}
	return out == StopContinue, nil
}

func (l *Loop) start(ctx context.Context, c *cycle, attempts []Attempt) error {
	last, err := LastStarted(l.Repository, c.stream)
	if err != nil {
		return err
	}
	n := last + 1
	tip, exists, err := c.host.Branch(ctx, c.branch)
	if err != nil {
		return err
	}
	if !exists {
		return fmt.Errorf("the clone has no climb branch %s", c.branch)
	}
	steering, err := ReadSteering(l.Repository, c.stream)
	if err != nil {
		return err
	}
	best, _ := Best(c.approval.Baseline, attempts)
	j := Journal{Number: n, Parent: tip, Best: best, Unit: AttemptUnit(n), Phase: PhaseStarted, Steering: len(steering.Notes), StartedAt: l.now()}
	run := Running{Attempt: n, Phase: kinds.PhaseMason, StartedAt: j.StartedAt, ClimbStartedAt: c.approval.At}
	reason := fmt.Sprintf("attempt %d starts from the current best %s, whose primary score is %v; its mason works in a workspace built from it", n, tip, best)
	if err := l.move(ctx, c, &j, "", run, reason, nil); err != nil {
		return err
	}
	l.publish(api.EventClimbAttemptStarted, c.stream)
	return l.hook("climb:started")
}

func (l *Loop) move(ctx context.Context, c *cycle, j *Journal, from Phase, run Running, reason string, events []trace.Event, extra ...trace.Document) error {
	at := l.now()
	to := j.Phase
	subject := trace.ClimbAttemptSubject(j.Number)
	state, err := l.Repository.Workflow(c.stream, subject)
	if err != nil {
		return err
	}
	if state.Value != string(from) {
		return errMoved
	}
	jd, err := journalDocument(l.Repository, c.stream, *j, at, string(to))
	if err != nil {
		return err
	}
	rd, err := RunningDocument(l.Repository, c.stream, run, at)
	if err != nil {
		return err
	}
	id := fmt.Sprintf("%s-%s", subject, to)
	tx := trace.Transaction{ExpectedVersion: state.Version,
		Transition: trace.Transition{Header: trace.Header{Schema: "osmia.trace.transition", Version: trace.Version, ID: id, Revision: 1, Project: l.Repository.Project(), Workstream: c.stream, At: at, Actor: InnerActor, Cause: trace.ClimbJournalID(j.Number)},
			Subject: subject, From: state.Value, To: string(to), Reason: reason},
		Events: events}
	if err := recordWith(ctx, l.Repository, append([]trace.Document{jd, rd}, extra...), tx); err != nil {
		return err
	}
	l.publish(api.EventClimbPhase, c.stream)
	return nil
}

func (l *Loop) advance(ctx context.Context, c *cycle, j Journal) (bool, error) {
	if c.concluding && j.Phase != PhasePromoting && j.Phase != PhasePromoted && j.Phase != PhaseRecorded {
		return l.discard(ctx, c, j, Decision{Cause: CauseConcluded, Reason: "the climb was concluded while the attempt was in flight"})
	}
	switch j.Phase {
	case PhaseStarted:
		return l.working(ctx, c, j)
	case PhaseCaptured:
		return l.measure(ctx, c, j)
	case PhaseMeasured:
		return l.measured(ctx, c, j)
	case PhaseReviewed:
		return l.reviewed(ctx, c, j)
	case PhasePromoting:
		return l.promoting(ctx, c, j)
	case PhasePromoted:
		return l.promoted(ctx, c, j)
	case PhaseRecorded:
		return l.cleanup(ctx, c, j)
	}
	return false, fmt.Errorf("attempt %d is in the unknown phase %q", j.Number, j.Phase)
}

func (l *Loop) stale(ctx context.Context, c *cycle, j Journal) (bool, error) {
	tip, exists, err := c.host.Branch(ctx, c.branch)
	if err != nil {
		return false, err
	}
	return !exists || tip != j.Parent, nil
}

func (l *Loop) working(ctx context.Context, c *cycle, j Journal) (bool, error) {
	if stale, err := l.stale(ctx, c, j); err != nil || stale {
		if err != nil {
			return false, err
		}
		return l.discard(ctx, c, j, Decision{Cause: CauseStale, Reason: "the climb branch moved away from the commit the attempt started from"})
	}
	branch, err := c.host.Open(ctx, j.Number, j.Parent)
	if err != nil {
		return false, err
	}
	if branch != j.Branch {
		j.Branch = branch
	}
	brief, err := l.brief(c, j)
	if err != nil {
		return false, err
	}
	res, err := c.host.Mason(ctx, MasonRequest{Stream: c.stream, Journal: j, Brief: brief})
	if err != nil {
		return false, err
	}
	switch {
	case res.Pending:
		return false, nil
	case res.NoCandidate != "" || res.Report == nil:
		reason := res.NoCandidate
		if reason == "" {
			reason = "the mason ended its turns without a report"
		}
		return l.discard(ctx, c, j, Decision{Cause: CauseNoCandidate, Reason: reason})
	}
	candidate, err := c.host.Capture(ctx, j.Number, j.Parent)
	if err != nil {
		return false, err
	}
	if err := l.hook("climb:captured-unrecorded"); err != nil {
		return false, err
	}
	j.Hypothesis, j.ChangeSummary, j.Expected = res.Report.Hypothesis, res.Report.Change, res.Report.Expected
	if candidate == j.Parent {
		return l.discard(ctx, c, j, Decision{Cause: CauseNoCandidate, Reason: "the mason's workspace holds no change from the best it started from"})
	}
	changed, err := c.host.ChangedPaths(ctx, j.Parent, candidate)
	if err != nil {
		return false, err
	}
	diff, err := c.host.Diff(ctx, j.Parent, candidate)
	if err != nil {
		return false, err
	}
	slices.Sort(changed)
	j.Candidate, j.Changed, j.Diff = candidate, changed, DiffSummary{Files: changed, Summary: diffStat(diff)}
	j.Phase = PhaseCaptured
	run := l.running(c, j, kinds.PhaseGuardrails, 0)
	reason := fmt.Sprintf("the mason of attempt %d reported done; its workspace is the candidate %s on %s, changing %d paths from %s", j.Number, candidate, j.Branch, len(changed), j.Parent)
	if err := l.move(ctx, c, &j, PhaseStarted, run, reason, nil); err != nil {
		return false, err
	}
	return true, l.hook("climb:captured")
}

func (l *Loop) brief(c *cycle, j Journal) (Brief, error) {
	attempts, err := ReadAttempts(l.Repository, c.stream)
	if err != nil {
		return Brief{}, err
	}
	steering, err := ReadSteering(l.Repository, c.stream)
	if err != nil {
		return Brief{}, err
	}
	carried := 0
	if len(attempts) > 0 {
		if prior, found, err := ReadJournal(l.Repository, c.stream, attempts[len(attempts)-1].Number); err != nil {
			return Brief{}, err
		} else if found {
			carried = prior.Steering
		}
	}
	steering.Notes = steering.Notes[:min(len(steering.Notes), j.Steering)]
	return BriefOf(j.Number, j.Parent, c.spec, c.approval, attempts, steering, carried), nil
}

func (l *Loop) running(c *cycle, j Journal, phase string, run int) Running {
	r := Running{Attempt: j.Number, Hypothesis: j.Hypothesis, Phase: phase, StartedAt: j.StartedAt, ClimbStartedAt: c.approval.At}
	if phase == kinds.PhaseMeasuring {
		r.Run, r.Runs = run, c.spec.Repeats
	}
	return r
}

func (l *Loop) measure(ctx context.Context, c *cycle, j Journal) (bool, error) {
	if stale, err := l.stale(ctx, c, j); err != nil || stale {
		if err != nil {
			return false, err
		}
		return l.discard(ctx, c, j, Decision{Cause: CauseStale, Reason: "the climb branch moved away from the commit the attempt started from"})
	}
	if err := Gate(c.approval.Protected, j.Changed); err != nil {
		return l.decided(ctx, c, j, Decision{Cause: CauseProtected, Reason: err.Error() + "; the attempt is refused before it is measured"}, nil, nil)
	}
	green := &Guardrails{Green: true}
	if len(c.spec.Guardrails.Checks) > 0 {
		if err := RecordRunning(ctx, l.Repository, c.stream, l.running(c, j, kinds.PhaseGuardrails, 0), l.now()); err != nil {
			return false, err
		}
		res, err := c.host.Guardrails(ctx, GuardrailRequest{Stream: c.stream, Number: j.Number, Candidate: j.Candidate, Checks: c.spec.Guardrails.Checks})
		if err != nil {
			return false, err
		}
		green = &res
	}
	if !green.Green {
		detail := green.Detail
		if detail == "" {
			detail = "a guardrail check did not pass"
		}
		return l.decided(ctx, c, j, Decision{Cause: CauseGuardrails, Reason: detail}, green, nil)
	}
	req := Request{Commit: j.Candidate, Function: c.spec.Evaluator, Approved: c.approval.Approved, Expect: c.spec.Expectations().WithUnits(c.approval.Units), Repeats: c.spec.Repeats,
		Progress: func(run int) {
			_ = RecordRunning(ctx, l.Repository, c.stream, l.running(c, j, kinds.PhaseMeasuring, run), l.now())
			l.publish(api.EventClimbPhase, c.stream)
		}}
	m, err := c.runner.Measure(ctx, req)
	if err != nil {
		return false, err
	}
	if err := l.hook("climb:measuring"); err != nil {
		return false, err
	}
	d := Decide(Candidate{Measurement: m, Best: j.Best, Direction: c.spec.Metric.Direction, MinImprovement: c.spec.Metric.MinImprovement, ChecksGreen: green.Green, ChecksDetail: green.Detail, Limits: c.spec.Limits()})
	return l.decided(ctx, c, j, d, green, &m)
}

func (l *Loop) decided(ctx context.Context, c *cycle, j Journal, d Decision, g *Guardrails, m *Measurement) (bool, error) {
	j.Guardrails, j.Measurement, j.Decision = g, m, &d
	if !d.Keep {
		return l.finishDiscard(ctx, c, j, PhaseCaptured, d)
	}
	j.Phase = PhaseMeasured
	reason := fmt.Sprintf("attempt %d is kept by the arithmetic: score %v beats the best %v by %v, at least the minimum improvement %v, with green guardrails and every limit met; a reviewer reads it before the best moves", j.Number, m.Score, j.Best, d.Improvement, c.spec.Metric.MinImprovement)
	if err := l.move(ctx, c, &j, PhaseCaptured, l.running(c, j, kinds.PhaseReviewing, 0), reason, nil); err != nil {
		return false, err
	}
	return true, l.hook("climb:measured")
}

func (l *Loop) measured(ctx context.Context, c *cycle, j Journal) (bool, error) {
	if j.Decision == nil || !j.Decision.Keep {
		return l.finishDiscard(ctx, c, j, PhaseMeasured, derefDecision(j.Decision))
	}
	diff, err := c.host.Diff(ctx, j.Parent, j.Candidate)
	if err != nil {
		return false, err
	}
	brief, err := l.brief(c, j)
	if err != nil {
		return false, err
	}
	res, err := c.host.Review(ctx, ReviewRequest{Stream: c.stream, Journal: j, Brief: brief, Diff: diff})
	if err != nil {
		return false, err
	}
	if res.Pending {
		return false, nil
	}
	j.Review, j.Phase = &res.Review, PhaseReviewed
	verdict := "approved"
	if !res.Review.Approved {
		verdict = "rejected"
	}
	reason := fmt.Sprintf("the reviewer %s attempt %d", verdict, j.Number)
	if err := l.move(ctx, c, &j, PhaseMeasured, l.running(c, j, kinds.PhaseReviewing, 0), reason, nil); err != nil {
		return false, err
	}
	return true, l.hook("climb:reviewed")
}

func derefDecision(d *Decision) Decision {
	if d == nil {
		return Decision{Cause: CauseInvalid, Reason: "no decision was recorded"}
	}
	return *d
}

func (l *Loop) reviewed(ctx context.Context, c *cycle, j Journal) (bool, error) {
	if j.Review == nil || !j.Review.Approved {
		notes := ""
		if j.Review != nil {
			notes = strings.TrimSpace(j.Review.Notes)
		}
		reason := "the reviewer rejected the candidate"
		if notes != "" {
			reason += ": " + notes
		}
		return l.finishDiscard(ctx, c, j, PhaseReviewed, Decision{Cause: CauseRejected, Reason: reason})
	}
	if stale, err := l.stale(ctx, c, j); err != nil || stale {
		if err != nil {
			return false, err
		}
		return l.finishDiscard(ctx, c, j, PhaseReviewed, Decision{Cause: CauseStale, Reason: "the climb branch moved away from the commit the attempt started from"})
	}
	at := l.now()
	message := commitMessage(c.stream, j, c.spec)
	squashed, err := c.host.Squash(ctx, j.Parent, j.Candidate, message, at)
	if err != nil {
		return false, err
	}
	j.SquashAt, j.Message, j.Squashed, j.Phase = at, message, squashed, PhasePromoting
	reason := fmt.Sprintf("the approved candidate of attempt %d is prepared as the one squashed commit %s on %s; the climb branch moves to it next", j.Number, squashed, j.Parent)
	if err := l.move(ctx, c, &j, PhaseReviewed, l.running(c, j, kinds.PhaseReviewing, 0), reason, nil); err != nil {
		return false, err
	}
	return true, l.hook("climb:promoting")
}

func (l *Loop) promoting(ctx context.Context, c *cycle, j Journal) (bool, error) {
	tip, exists, err := c.host.Branch(ctx, c.branch)
	if err != nil {
		return false, err
	}
	switch {
	case !exists:
		return false, fmt.Errorf("the clone has no climb branch %s", c.branch)
	case tip == j.Squashed:
	case tip == j.Parent:
		if err := l.hook("climb:moving"); err != nil {
			return false, err
		}
		if err := c.host.Advance(ctx, fmt.Sprintf("%s/attempt-%d", c.stream, j.Number), j.Parent, j.Squashed); err != nil {
			return false, err
		}
		if err := l.hook("climb:moved"); err != nil {
			return false, err
		}
	default:
		return l.finishDiscard(ctx, c, j, PhasePromoting, Decision{Cause: CauseStale, Reason: fmt.Sprintf("the climb branch is at %s, neither the parent %s nor the prepared commit %s", tip, j.Parent, j.Squashed)})
	}
	j.Phase = PhasePromoted
	reason := fmt.Sprintf("the climb branch %s is at the squashed commit %s of attempt %d: it is the new best", c.branch, j.Squashed, j.Number)
	if err := l.move(ctx, c, &j, PhasePromoting, l.running(c, j, kinds.PhaseReviewing, 0), reason, nil); err != nil {
		return false, err
	}
	return true, l.hook("climb:promoted")
}

func (l *Loop) promoted(ctx context.Context, c *cycle, j Journal) (bool, error) {
	if j.Measurement == nil || !j.Measurement.Valid {
		return false, fmt.Errorf("attempt %d is promoted without a valid measurement", j.Number)
	}
	a := l.attempt(c, j, OutcomeKept, Decision{Keep: true})
	a.Commit = j.Squashed
	best := j.Measurement.Score
	body := fmt.Sprintf("Attempt %d was kept: %s. The current best is now %v on %s.", j.Number, strings.TrimSpace(oneLine(j.Hypothesis)), best, j.Squashed)
	events := []trace.Event{trace.Progress(fmt.Sprintf("%s-%s", trace.ClimbAttemptSubject(j.Number), PhaseRecorded), "chief", body)}
	return l.record(ctx, c, j, PhasePromoted, a, events, api.EventClimbKept)
}

func (l *Loop) finishDiscard(ctx context.Context, c *cycle, j Journal, from Phase, d Decision) (bool, error) {
	a := l.attempt(c, j, OutcomeDiscarded, d)
	var events []trace.Event
	if d.Cause == CauseNoCandidate {
		attempts, err := ReadAttempts(l.Repository, c.stream)
		if err != nil {
			return false, err
		}
		run := 1
		for i := len(attempts) - 1; i >= 0 && attempts[i].Cause == CauseNoCandidate; i-- {
			run++
		}
		if run%NoCandidateStall == 0 {
			id := fmt.Sprintf("%s-%s", trace.ClimbAttemptSubject(j.Number), PhaseRecorded)
			events = append(events, trace.Notice(id, "chief", fmt.Sprintf("The climb is stalled: %d attempts in a row ended without a candidate, the latest being attempt %d (%s). The mason produces nothing the service can measure; steer it with a note or conclude the climb.", run, j.Number, d.Reason)))
		}
	}
	return l.record(ctx, c, j, from, a, events, "")
}

func (l *Loop) discard(ctx context.Context, c *cycle, j Journal, d Decision) (bool, error) {
	return l.finishDiscard(ctx, c, j, j.Phase, d)
}

func (l *Loop) attempt(c *cycle, j Journal, outcome Outcome, d Decision) Attempt {
	a := Attempt{Number: j.Number, Parent: j.Parent, Best: j.Best, Candidate: j.Candidate, Branch: j.Branch,
		Hypothesis: j.Hypothesis, ChangeSummary: j.ChangeSummary, Expected: j.Expected, Diff: j.Diff, Outcome: outcome,
		Guardrails: j.Guardrails, StartedAt: j.StartedAt, FinishedAt: l.now()}
	if j.Measurement != nil {
		dec := d
		if j.Decision != nil {
			dec = *j.Decision
		}
		a.Measured(*j.Measurement, dec)
	}
	if outcome == OutcomeDiscarded {
		a.Cause, a.Reason = d.Cause, d.Reason
		if d.Cause == CauseRejected && j.Review != nil {
			a.ReviewNotes = j.Review.Notes
		}
	}
	return a
}

func (l *Loop) record(ctx context.Context, c *cycle, j Journal, from Phase, a Attempt, events []trace.Event, kept api.EventKind) (bool, error) {
	doc, recorded, err := AttemptDocument(l.Repository, c.stream, a)
	if err != nil {
		return false, err
	}
	var docs []trace.Document
	if !recorded {
		docs = append(docs, doc)
	}
	j.Phase = PhaseRecorded
	reason := fmt.Sprintf("attempt %d ends %s and is recorded at %s", j.Number, a.Outcome, trace.ClimbAttemptPath(j.Number))
	if a.Outcome == OutcomeDiscarded {
		reason = fmt.Sprintf("attempt %d is discarded (%s): %s", j.Number, a.Cause, a.Reason)
	}
	idle := Running{ClimbStartedAt: c.approval.At}
	if err := l.move(ctx, c, &j, from, idle, reason, events, docs...); err != nil {
		return false, err
	}
	l.publish(api.EventClimbAttemptRecorded, c.stream)
	if kept != "" {
		l.publish(kept, c.stream)
	}
	return true, l.hook("climb:recorded")
}

func (l *Loop) cleanup(ctx context.Context, c *cycle, j Journal) (bool, error) {
	if err := l.hook("climb:dropping"); err != nil {
		return false, err
	}
	if err := c.host.Drop(ctx, j.Number); err != nil {
		return false, err
	}
	j.Phase = PhaseCleaned
	reason := fmt.Sprintf("the workspace of attempt %d is dropped; its branch %s stays", j.Number, j.Branch)
	if err := l.move(ctx, c, &j, PhaseRecorded, Running{ClimbStartedAt: c.approval.At}, reason, nil); err != nil {
		return false, err
	}
	return true, l.hook("climb:cleaned")
}

func (l *Loop) publish(kind api.EventKind, stream config.WorkstreamID) {
	if l.Publish != nil {
		l.Publish(kind, stream)
	}
}

func (l *Loop) hook(name string) error {
	if l.Step == nil {
		return nil
	}
	return l.Step(name)
}

func (l *Loop) now() time.Time {
	if l.Now != nil {
		return l.Now().UTC()
	}
	return time.Now().UTC()
}

func commitMessage(stream config.WorkstreamID, j Journal, spec Spec) string {
	title := oneLine(j.Hypothesis)
	if r := []rune(title); len(r) > 60 {
		title = string(r[:60]) + "…"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Climb attempt %d: %s\n\n", j.Number, title)
	fmt.Fprintf(&b, "Hypothesis: %s\n\nChange: %s\n\nExpected effect: %s\n\n", strings.TrimSpace(j.Hypothesis), strings.TrimSpace(j.ChangeSummary), strings.TrimSpace(j.Expected))
	if j.Measurement != nil {
		fmt.Fprintf(&b, "Measured %s %v against the best %v on the parent.\n\n", spec.Metric.Name, j.Measurement.Score, j.Best)
	}
	fmt.Fprintf(&b, "Attempt %d of workstream %s, built on %s.", j.Number, stream, j.Parent)
	return b.String()
}

func diffStat(diff string) string {
	added, removed := 0, 0
	for _, line := range strings.Split(diff, "\n") {
		switch {
		case strings.HasPrefix(line, "+++"), strings.HasPrefix(line, "---"):
		case strings.HasPrefix(line, "+"):
			added++
		case strings.HasPrefix(line, "-"):
			removed++
		}
	}
	return fmt.Sprintf("+%d -%d", added, removed)
}
