package climb

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/kpenfound/osmia/internal/api"
	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/trace"
)

// ConcludePhase is how far a climb's conclusion has got. Each phase is one
// commit of the trace, together with the conclusion record it rests on, so a
// restart reads the phase back and carries on from it.
type ConcludePhase string

const (
	// ConcludePinned: the upstream commit the branch is rebased onto, and the
	// branch commit that rebase starts from, are recorded.
	ConcludePinned ConcludePhase = "pinned"
	// ConcludeConflicted: the replay onto the pinned commit conflicted, and the
	// conclusion waits for the owner.
	ConcludeConflicted ConcludePhase = "conflicted"
	// ConcludePrepared: the rebased commit is made and recorded, and the climb
	// branch has not moved to it yet.
	ConcludePrepared ConcludePhase = "prepared"
	// ConcludeMoved: the climb branch is at the rebased commit.
	ConcludeMoved ConcludePhase = "moved"
	// ConcludeMeasured: the rebased commit is measured and both finals are
	// recorded. The workstream is ready to be assembled.
	ConcludeMeasured ConcludePhase = "measured"
)

// Target is the upstream commit a climb branch is rebased onto, pinned when
// concluding begins.
type Target struct {
	Workstream config.WorkstreamID `json:"workstream,omitempty"`
	Remote     string              `json:"remote"`
	Branch     string              `json:"branch"`
	Commit     string              `json:"commit"`
}

// Final is a climb's final result as measured on one commit: the best score
// the climb reached before the rebase, or the score measured again on the
// rebased commit.
type Final struct {
	// Commit is the commit the result belongs to, and MeasuredOn the commit
	// the evaluator was run on. They differ for a kept attempt, which was
	// measured on its candidate and landed as a squashed commit of the same
	// tree.
	Commit     string `json:"commit"`
	MeasuredOn string `json:"measured_on"`
	// Attempt is the number of the attempt that made the best, zero for the
	// baseline.
	Attempt int `json:"attempt"`
	// Valid is false for a measurement that failed or was invalid; Reason says
	// why, and Score is then unset.
	Valid       bool               `json:"valid"`
	Reason      string             `json:"reason,omitempty"`
	Score       float64            `json:"score"`
	Metrics     map[string]float64 `json:"metrics,omitempty"`
	Units       map[string]string  `json:"units,omitempty"`
	Measurement *Measurement       `json:"measurement,omitempty"`
}

// Conclusion is climb/conclusion.json: the one rebase of the climb branch and
// the finals measured before and after it. Concluding pins Target once, so a
// restart never rebases onto a newer upstream, and the post-rebase final is
// measured on Rebased, the commit the branch holds and delivery publishes.
type Conclusion struct {
	Phase     ConcludePhase `json:"phase"`
	Evaluator string        `json:"evaluator"`
	Primary   string        `json:"primary"`
	Branch    string        `json:"branch"`
	// Base is the upstream commit the branch sat on when concluding began, the
	// boundary of the replay, and Before the branch tip it replays.
	Base   string `json:"base"`
	Before string `json:"before"`
	// Target is the pinned conclusion target and At the time the conclusion
	// was pinned, which fixes the rebased commit.
	Target Target    `json:"target"`
	At     time.Time `json:"at"`
	// Baseline is the calibrated score on BaselineCommit, the evaluator
	// commit.
	Baseline       float64 `json:"baseline"`
	BaselineCommit string  `json:"baseline_commit"`
	Unit           string  `json:"unit,omitempty"`
	// Attempts counts the recorded attempts and Kept the numbers of those that
	// landed.
	Attempts  int      `json:"attempts"`
	Kept      []int    `json:"kept,omitempty"`
	Pre       Final    `json:"pre"`
	Rebased   string   `json:"rebased,omitempty"`
	Conflicts []string `json:"conflicts,omitempty"`
	Post      *Final   `json:"post,omitempty"`
}

// ReadConclusion returns the climb's conclusion record, and whether concluding
// has begun.
func ReadConclusion(r *trace.Repository, stream config.WorkstreamID) (Conclusion, bool, error) {
	v, _, found, err := readRecord[Conclusion](r, stream, trace.ClimbConclusionID)
	return v, found, err
}

// Concluded reports whether the conclusion has recorded both finals.
func (c Conclusion) Concluded() bool { return c.Phase == ConcludeMeasured && c.Post != nil }

// KeptNone says the climb kept no attempt, so its branch holds only the
// evaluator.
func (c Conclusion) KeptNone() bool { return len(c.Kept) == 0 }

// ConcludeHost is what concluding asks of the service's machinery: the
// climb's clone, the upstream, and the one rebase.
type ConcludeHost interface {
	Source
	// Branch returns the commit a branch points at.
	Branch(ctx context.Context, name string) (commit string, exists bool, err error)
	// Target resolves the upstream commit the climb branch is rebased onto, and
	// the commit the branch sits on now, given its tip.
	Target(ctx context.Context, tip string) (target Target, base string, err error)
	// Replay makes the commit that replays what the branch holds after base
	// onto onto. The same arguments make the same commit, and no branch moves.
	Replay(ctx context.Context, base, before, onto string, at time.Time) (commit string, conflicts []string, err error)
	// Move moves the climb branch from one commit to another, which is a
	// no-op when it is already there.
	Move(ctx context.Context, key, from, to string) error
}

// Concluder carries a concluding Hill Climb workstream from the end of its
// climb to the start of its delivery: it pins the upstream commit to rebase
// onto, rebases the climb branch once, and measures the rebased commit again.
// Every step is recorded before the next begins and reconciled against the
// branch and the trace on a restart, so the branch is rebased at most once and
// the post-rebase measurement belongs to the commit that is delivered.
type Concluder struct {
	Repository *trace.Repository
	// Spec returns the sealed climb spec and the revision that was sealed.
	Spec func(stream config.WorkstreamID) (Spec, int, error)
	// Branch names the climb branch.
	Branch func(stream config.WorkstreamID) string
	// Host returns the machinery of the workstream.
	Host func(stream config.WorkstreamID) (ConcludeHost, error)
	// Runner returns the measurement runner that exports commits from source.
	Runner func(stream config.WorkstreamID, source Source) (Runner, error)
	// Publish announces a climb event to the clients of the event stream.
	Publish func(kind api.EventKind, stream config.WorkstreamID)
	Now     func() time.Time
	// Step is a hook between a side effect and its record; a failure aborts
	// the pass as a crash would.
	Step func(name string) error
}

// Reconcile moves the conclusion one step: pinning, replaying, moving the
// branch or measuring. It does nothing for a workstream that is not building,
// whose climb is not concluding, or whose last attempt has not settled.
func (c *Concluder) Reconcile(ctx context.Context, stream config.WorkstreamID) error {
	feature, err := c.Repository.Workflow(stream, trace.FeatureSubject)
	if err != nil || feature.Value != featureBuilding {
		return err
	}
	inner, err := ReadInner(c.Repository, stream)
	if err != nil || inner.Value != InnerConcluding {
		return err
	}
	if _, open, err := OpenAttempt(c.Repository, stream); err != nil || open {
		return err
	}
	state, err := c.Repository.Workflow(stream, trace.ClimbConclusionSubject)
	if err != nil {
		return err
	}
	var rec Conclusion
	if state.Value != "" {
		var found bool
		if rec, found, err = ReadConclusion(c.Repository, stream); err != nil {
			return err
		} else if !found {
			return fmt.Errorf("workstream %s is %s without a conclusion record", stream, state.Value)
		}
	}
	switch ConcludePhase(state.Value) {
	case "":
		err = c.pin(ctx, stream, state)
	case ConcludePinned:
		err = c.prepare(ctx, stream, state, rec)
	case ConcludePrepared:
		err = c.move(ctx, stream, state, rec)
	case ConcludeMoved:
		err = c.measure(ctx, stream, state, rec)
	}
	if errors.Is(err, errMoved) {
		return nil
	}
	return err
}

func (c *Concluder) now() time.Time {
	if c.Now != nil {
		return c.Now().UTC()
	}
	return time.Now().UTC()
}

func (c *Concluder) hook(name string) error {
	if c.Step == nil {
		return nil
	}
	return c.Step(name)
}

func (c *Concluder) publish(stream config.WorkstreamID) {
	if c.Publish != nil {
		c.Publish(api.EventClimbPhase, stream)
	}
}

func (c *Concluder) header(id string, stream config.WorkstreamID, cause string, at time.Time) trace.Header {
	return trace.Header{Schema: "osmia.trace.transition", Version: trace.Version, ID: id, Revision: 1, Project: c.Repository.Project(), Workstream: stream, At: at, Actor: InnerActor, Cause: cause}
}

// record stores rec as the next revision of the conclusion document in the
// commit that moves the conclusion to its next phase.
func (c *Concluder) record(ctx context.Context, stream config.WorkstreamID, state trace.WorkflowState, rec Conclusion, reason string, event trace.Event) error {
	revision, err := nextRevision(c.Repository, stream, trace.ClimbConclusionID)
	if err != nil {
		return err
	}
	at := c.now()
	doc, err := jsonDocument(c.Repository, stream, trace.ClimbConclusionID, trace.ClimbConclusionPath, revision, at, fmt.Sprintf("climb-conclusion-%s", rec.Phase), rec)
	if err != nil {
		return err
	}
	id := fmt.Sprintf("%s-%s", trace.ClimbConclusionSubject, rec.Phase)
	tx := trace.Transaction{ExpectedVersion: state.Version,
		Transition: trace.Transition{Header: c.header(id, stream, trace.ClimbConclusionID, at), Subject: trace.ClimbConclusionSubject, From: state.Value, To: string(rec.Phase), Reason: reason},
		Events:     []trace.Event{event}}
	if err := recordWith(ctx, c.Repository, []trace.Document{doc}, tx); err != nil {
		return err
	}
	c.publish(stream)
	return nil
}

func (c *Concluder) pin(ctx context.Context, stream config.WorkstreamID, state trace.WorkflowState) error {
	spec, _, err := c.Spec(stream)
	if err != nil {
		return err
	}
	approval, ok, err := ReadApproval(c.Repository, stream)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("workstream %s is concluding without the owner's approval record", stream)
	}
	attempts, err := ReadAttempts(c.Repository, stream)
	if err != nil {
		return err
	}
	host, err := c.Host(stream)
	if err != nil {
		return err
	}
	branch := c.Branch(stream)
	tip, exists, err := host.Branch(ctx, branch)
	if err != nil {
		return err
	}
	if !exists {
		return fmt.Errorf("the clone has no climb branch %s", branch)
	}
	target, base, err := host.Target(ctx, tip)
	if err != nil {
		return err
	}
	if err := c.hook("climb-conclusion-resolved"); err != nil {
		return err
	}
	rec := Conclusion{Phase: ConcludePinned, Evaluator: spec.Evaluator, Primary: spec.Metric.Name, Branch: branch, Base: base, Before: tip, Target: target, At: c.now(),
		Baseline: approval.Baseline, BaselineCommit: approval.Commit, Unit: approval.Units[spec.Metric.Name], Attempts: len(attempts)}
	rec.Pre = Final{Commit: tip, MeasuredOn: approval.Commit, Valid: true, Score: approval.Baseline, Metrics: approval.Metrics, Units: approval.Units}
	for _, a := range attempts {
		if a.Outcome != OutcomeKept || a.Scores == nil {
			continue
		}
		rec.Kept = append(rec.Kept, a.Number)
		rec.Pre.Attempt, rec.Pre.MeasuredOn = a.Number, a.Candidate
		rec.Pre.Score, rec.Pre.Metrics = a.Scores.Primary, a.Scores.Metrics
	}
	reason := fmt.Sprintf("the climb concluded with %d attempts recorded and %d kept; its branch %s, at %s, is rebased once onto %s/%s at %s, pinned now", rec.Attempts, len(rec.Kept), branch, tip, target.Remote, target.Branch, target.Commit)
	body := fmt.Sprintf("The climb concludes with %d attempts recorded and %d kept. Its branch is rebased once onto %s/%s at %s, and measured again on the rebased commit. Final before the rebase: %v.", rec.Attempts, len(rec.Kept), target.Remote, target.Branch, target.Commit, rec.Pre.Score)
	return c.record(ctx, stream, state, rec, reason, trace.Progress("climb-conclusion-pinned", "chief", body))
}

func (c *Concluder) prepare(ctx context.Context, stream config.WorkstreamID, state trace.WorkflowState, rec Conclusion) error {
	host, err := c.Host(stream)
	if err != nil {
		return err
	}
	commit, conflicts, err := host.Replay(ctx, rec.Base, rec.Before, rec.Target.Commit, rec.At)
	if err != nil {
		return err
	}
	if err := c.hook("climb-conclusion-replayed"); err != nil {
		return err
	}
	if len(conflicts) > 0 {
		rec.Phase, rec.Conflicts = ConcludeConflicted, conflicts
		reason := fmt.Sprintf("the climb branch %s does not rebase cleanly onto %s/%s at %s: %s conflicted; the branch is left at %s", rec.Branch, rec.Target.Remote, rec.Target.Branch, rec.Target.Commit, strings.Join(conflicts, ", "), rec.Before)
		return c.record(ctx, stream, state, rec, reason, trace.Notice("climb-conclusion-conflicted", "chief", "The climb cannot be concluded: "+reason+". Its kept attempts are on the branch, which stays where it is until the owner decides."))
	}
	rec.Phase, rec.Rebased = ConcludePrepared, commit
	reason := fmt.Sprintf("the climb branch %s replays onto %s/%s at %s as %s; the branch moves to it next", rec.Branch, rec.Target.Remote, rec.Target.Branch, rec.Target.Commit, commit)
	return c.record(ctx, stream, state, rec, reason, trace.Progress("climb-conclusion-prepared", "chief", "The climb branch was replayed onto the pinned upstream commit as "+commit+"."))
}

func (c *Concluder) move(ctx context.Context, stream config.WorkstreamID, state trace.WorkflowState, rec Conclusion) error {
	host, err := c.Host(stream)
	if err != nil {
		return err
	}
	tip, exists, err := host.Branch(ctx, rec.Branch)
	if err != nil {
		return err
	}
	if !exists {
		return fmt.Errorf("the clone has no climb branch %s", rec.Branch)
	}
	if tip != rec.Rebased {
		if tip != rec.Before {
			return fmt.Errorf("the climb branch %s is at %s, which is neither the commit %s it was rebased from nor the rebased commit %s", rec.Branch, tip, rec.Before, rec.Rebased)
		}
		if err := host.Move(ctx, string(stream)+"/conclusion", rec.Before, rec.Rebased); err != nil {
			return err
		}
	}
	if err := c.hook("climb-conclusion-moved"); err != nil {
		return err
	}
	rec.Phase = ConcludeMoved
	reason := fmt.Sprintf("the climb branch %s moved from %s to the rebased commit %s", rec.Branch, rec.Before, rec.Rebased)
	return c.record(ctx, stream, state, rec, reason, trace.Progress("climb-conclusion-moved", "chief", "The climb branch moved to the rebased commit "+rec.Rebased+". The evaluator measures it next."))
}

func (c *Concluder) measure(ctx context.Context, stream config.WorkstreamID, state trace.WorkflowState, rec Conclusion) error {
	spec, _, err := c.Spec(stream)
	if err != nil {
		return err
	}
	approval, ok, err := ReadApproval(c.Repository, stream)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("workstream %s is concluding without the owner's approval record", stream)
	}
	host, err := c.Host(stream)
	if err != nil {
		return err
	}
	tip, exists, err := host.Branch(ctx, rec.Branch)
	if err != nil {
		return err
	}
	if !exists || tip != rec.Rebased {
		return fmt.Errorf("the climb branch %s is at %q, not at the rebased commit %s the final is measured on", rec.Branch, tip, rec.Rebased)
	}
	runner, err := c.Runner(stream, host)
	if err != nil {
		return err
	}
	m, err := runner.Measure(ctx, Request{Commit: rec.Rebased, Function: spec.Evaluator, Approved: approval.Approved, Expect: spec.Expectations().WithUnits(approval.Units), Repeats: spec.Repeats})
	if err != nil {
		return err
	}
	if err := c.hook("climb-conclusion-measured"); err != nil {
		return err
	}
	post := Final{Commit: rec.Rebased, MeasuredOn: rec.Rebased, Attempt: rec.Pre.Attempt, Valid: m.Valid, Reason: m.Reason, Measurement: &m}
	if m.Valid {
		post.Score, post.Metrics, post.Units = m.Score, m.Metrics, m.Units
	}
	rec.Phase, rec.Post = ConcludeMeasured, &post
	reason := fmt.Sprintf("the evaluator measured the rebased commit %s again: %s", rec.Rebased, finalWords(post))
	body := fmt.Sprintf("The climb is concluded. Final before the rebase: %v on %s. Final after the rebase: %s on %s. The branch is assembled for the final review next.", rec.Pre.Score, rec.Pre.Commit, finalWords(post), rec.Rebased)
	event := trace.Progress("climb-conclusion-measured", "chief", body)
	if !post.Valid {
		event = trace.Notice("climb-conclusion-measured", "chief", body)
	}
	return c.record(ctx, stream, state, rec, reason, event)
}

func finalWords(f Final) string {
	if !f.Valid {
		return "the measurement is invalid: " + strings.TrimSpace(f.Reason)
	}
	return fmt.Sprintf("score %v", f.Score)
}
