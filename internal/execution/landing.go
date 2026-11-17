package execution

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/kpenfound/agentcore/vcs"
	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/plan"
	"github.com/kpenfound/osmia/internal/router"
	"github.com/kpenfound/osmia/internal/scheduler"
	"github.com/kpenfound/osmia/internal/seal"
	"github.com/kpenfound/osmia/internal/thread"
	"github.com/kpenfound/osmia/internal/trace"
	"github.com/kpenfound/osmia/internal/workspace"
)

// LandAction is the repository-boundary operation action that lands one
// approved unit: it squashes the approved candidate onto the feature branch
// as one commit and records the unit merged.
const LandAction = "land"

// landingTrailer is the commit message trailer that names the operation
// that made a landing commit.
const landingTrailer = "Osmia-Operation"

var foremanActor = trace.Actor{Kind: "service", ID: "foreman"}

// landInput names the approval a landing lands: the unit, the revision of
// its units/<unit>/review.json that approved it, and the candidate and base
// that revision records.
type landInput struct {
	Unit      string `json:"unit"`
	Review    int    `json:"review"`
	Candidate string `json:"candidate"`
	Base      string `json:"base"`
	// Attempt numbers the landings of one approval: the first is attempt one,
	// and a landing that was refused is asked for again, as the next attempt,
	// when the router decides to land it again. Zero is the first.
	Attempt int `json:"attempt,omitempty"`
}

// attempt is the number of the landing, from one.
func (in landInput) attempt() int { return max(in.Attempt, 1) }

// ids returns the IDs of the landing's transition and its run event. The first
// attempt keeps the IDs of the approval, and each later one has its own.
func (in landInput) ids() (transition, event string) {
	return landAttemptIDs(in.Unit, in.Review, in.attempt())
}

// landingSubject is the workflow subject that tracks the landings of one
// unit: requested-<k> once landing the approval of review revision k is
// asked for, then landed-<k> or refused-<k>.
func landingSubject(unit string) string {
	return "landing" + strings.TrimPrefix(trace.UnitSubject(unit), "unit")
}

func landingDocument(unit string) string { return trace.UnitSubject(unit) + "-landing" }

func landIDs(unit string, review int) (transition, event string) {
	return landAttemptIDs(unit, review, 1)
}

// landAttemptIDs returns the IDs of the transition and the run event of the
// attempt-th landing of the approval review records.
func landAttemptIDs(unit string, review, attempt int) (transition, event string) {
	transition = fmt.Sprintf("%s-%d", landingSubject(unit), review)
	if attempt > 1 {
		transition = fmt.Sprintf("%s-attempt-%d", transition, attempt)
	}
	return transition, trace.EventID(transition, "run")
}

// landAgainID is the ID of the transition by which a refused landing of an
// approval that still holds is asked for again as the given attempt.
func landAgainID(unit string, review, attempt int) string {
	return fmt.Sprintf("%s-%s-again-%d-%d", trace.UnitSubject(unit), UnitApproved, review, attempt)
}

// foreman is the landing controller. Its pass asks to land one approved unit
// at a time per workstream and to rebase the unit workspaces a landing left
// behind; its reconciler runs each landing operation, and rebaser each
// rebase operation.
type foreman struct {
	*masons
}

var _ coreadapter.Reconciler = (*foreman)(nil)

// Pass keeps each workstream's unfinished units on its feature branch and
// asks to land its first approved unit once its own rebases have settled.
// A pending landing or drift holds only its workstream.
func (f *foreman) Pass(ctx context.Context) error {
	streams, err := nonSessionWorkstreams(f.repository)
	if err != nil {
		return err
	}
	state, _ := f.s.Effective()
	librarian := librarianWorkstream(f.repository.Project())
	requested := map[config.WorkstreamID][]landInput{}
	rebasing := map[config.WorkstreamID]map[string]bool{}
	rebased := map[config.WorkstreamID]map[string][]string{}
	dispatched := map[config.WorkstreamID]map[string]bool{}
	drifting := map[config.WorkstreamID]bool{}
	landing := map[config.WorkstreamID]bool{}
	var candidates []building
	for _, stream := range streams {
		if stream == librarian {
			continue
		}
		if held, err := f.s.ChangeHeld(f.repository, stream); err != nil {
			return err
		} else if held {
			continue
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		ops, err := f.repository.Operations(stream)
		if err != nil {
			return err
		}
		rebasing[stream], rebased[stream], dispatched[stream] = map[string]bool{}, map[string][]string{}, map[string]bool{}
		for _, o := range ops {
			switch o.Operation.Action {
			case DriftAction:
				if o.Result == nil {
					drifting[stream] = true
				}
			case LandAction:
				if o.Result == nil {
					landing[stream] = true
				}
				in, err := decodeLand(o.Operation)
				if err != nil {
					return err
				}
				requested[stream] = append(requested[stream], in)
			case RebaseAction:
				in, err := decodeRebase(o.Operation)
				if err != nil {
					return err
				}
				if o.Result == nil {
					rebasing[stream][in.Unit] = true
				} else {
					rebased[stream][in.Unit] = append(rebased[stream][in.Unit], in.Onto)
				}
			default:
				if in, err := thread.DecodeTurn(o.Operation); err == nil {
					dispatched[stream][in.Agent+"/"+in.Turn] = true
				}
			}
		}
		b, found, err := f.read(stream)
		if err != nil {
			return fmt.Errorf("workstream %s landing: %w", stream, err)
		}
		if found && !landing[stream] && !drifting[stream] && !scheduler.Paused(state.Pauses, f.cfg.Project.ID, stream) {
			candidates = append(candidates, b)
		}
	}
	settled := map[config.WorkstreamID]bool{}
	for _, b := range candidates {
		current, err := f.refresh(ctx, b, rebasing[b.stream], dispatched[b.stream], rebased[b.stream])
		if err != nil {
			return fmt.Errorf("workstream %s rebase: %w", b.stream, err)
		}
		settled[b.stream] = current && len(rebasing[b.stream]) == 0
	}
	for _, b := range startOrder(candidates, state.Priorities, f.cfg.Project.ID) {
		if !settled[b.stream] {
			continue
		}
		for _, u := range dependencyOrder(b.plan) {
			if b.states[trace.UnitSubject(u.ID)].Value != UnitApproved {
				continue
			}
			review, result, ok, err := f.approval(b.stream, u.ID, 0)
			if err != nil {
				return err
			}
			if !ok {
				continue
			}
			var latest *landInput
			for i, in := range requested[b.stream] {
				if in.Unit == u.ID && in.Review == review.Revision && (latest == nil || in.attempt() > latest.attempt()) {
					latest = &requested[b.stream][i]
				}
			}
			attempt := 1
			if latest != nil {
				// A landing that was asked for is followed by its refusal's
				// exit; only a decision to land again asks for another.
				again, err := f.afterRefusal(ctx, b, u.ID, review, result, *latest)
				if err != nil {
					return fmt.Errorf("workstream %s unit %s landing: %w", b.stream, u.ID, err)
				}
				if again == 0 {
					continue
				}
				attempt = again
			}
			if err := f.request(ctx, b.stream, u.ID, review, result, attempt); err != nil {
				return fmt.Errorf("workstream %s unit %s landing: %w", b.stream, u.ID, err)
			}
			break
		}
	}
	return nil
}

// The words of a refusal that was not about the approval: a landing an
// operation gave up on after repeated failures, and one whose outcome could not
// be read. Any other refusal says the approval no longer holds.
const (
	RefusedAfterRetries   = "gave up after repeated failures"
	RefusedUnknownOutcome = "its outcome could not be determined"
)

// staleRefusal reports whether a landing was refused because the approval no
// longer holds, as opposed to a failure of the landing itself.
func staleRefusal(reason string) bool {
	return !strings.Contains(reason, RefusedAfterRetries) && !strings.Contains(reason, RefusedUnknownOutcome)
}

// afterRefusal follows a landing that was asked for, for an approval that still
// stands. A landing whose refusal is recorded raises the unit's landing_refused
// exit, whose default is to check and review the unit again when the approval
// no longer holds and to land again when the refusal was not about it. It
// returns the number of the attempt to ask for when the router decided to land
// again, and zero otherwise.
func (f *foreman) afterRefusal(ctx context.Context, b building, unit string, review trace.Document, result UnitReviewResult, last landInput) (int, error) {
	outcome, err := f.outcome(b.stream, last)
	if err != nil || outcome == nil || outcome.Outcome != "failed" {
		return 0, err
	}
	next := last.attempt() + 1
	transitions, err := trace.Read[trace.Transition](f.repository, b.stream)
	if err != nil {
		return 0, err
	}
	if slices.ContainsFunc(transitions, func(t trace.Transition) bool { return t.ID == landAgainID(unit, review.Revision, next) }) {
		return next, nil
	}
	subject := trace.UnitSubject(unit)
	transition, event := last.ids()
	refused := transition + "-refused"
	reviews := &reviewers{masons: f.masons}
	holds, err := reviews.verdictHolds(b.stream, unit, result)(ctx)
	if err != nil {
		return 0, err
	}
	stale := staleRefusal(outcome.Evidence) || holds != ""
	in := UnitExitInput{
		Repo: f.repository, Stream: b.stream, Unit: unit, State: b.states[subject], Trigger: TriggerLandingRefused, Cause: refused, Actor: foremanActor,
		Note: outcome.Evidence,
		Evidence: router.Evidence{Facts: map[string]any{"attempt": last.attempt(), "approval_holds": holds == "", "refusal_about_the_approval": staleRefusal(outcome.Evidence)},
			Records: []router.Entry{{Kind: "landing refusal", ID: refused, Text: outcome.Evidence}}},
		HeaderCause: refused,
		// The refused landing recorded its outcome, so it no longer holds the unit.
		Operation: trace.OperationID(f.repository.Project(), b.stream, event),
	}
	if stale {
		id := fmt.Sprintf("%s-%s-landing-%d-%d", subject, UnitChecking, last.Review, last.attempt())
		in.Default, in.Targets = UnitChecking, []string{UnitChecking, UnitReviewing, UnitImplementing, UnitContested}
		in.ID, in.Reason = id, outcome.Evidence+"; the unit returns to checking and review so that a current approval is recorded"
		in.Events = []trace.Event{trace.Notice(id, "unit", fmt.Sprintf("Unit %s returns to checking: its landing was refused. %s", unit, outcome.Evidence))}
	} else {
		id := landAgainID(unit, review.Revision, next)
		in.Default, in.Targets = UnitApproved, []string{UnitApproved, UnitChecking, UnitReviewing, UnitContested}
		in.ID, in.Reason = id, fmt.Sprintf("%s; the approval still holds, so the foreman lands unit %s again as attempt %d", outcome.Evidence, unit, next)
		in.Events = []trace.Event{trace.Progress(id, "unit", fmt.Sprintf("Unit %s is landed again as attempt %d: its approval still holds.", unit, next))}
		in.Authority = &router.Authority{Kind: router.AuthorityReviewer, Target: UnitApproved, Ref: fmt.Sprintf("%s revision %d", review.Path, review.Revision), Valid: reviews.verdictHolds(b.stream, unit, result)}
	}
	_, err = f.s.raise(ctx, true, in)
	return 0, err
}

// approval returns the given revision of the unit's review.json, its latest
// when revision is zero, with the review result it records, and whether that
// is a satisfactory verdict.
func (f *foreman) approval(stream config.WorkstreamID, unit string, revision int) (trace.Document, UnitReviewResult, bool, error) {
	docs, err := trace.Read[trace.Document](f.repository, stream)
	if err != nil {
		return trace.Document{}, UnitReviewResult{}, false, err
	}
	var review trace.Document
	for _, d := range docs {
		if d.ID == reviewDocument(unit) && (revision == 0 || d.Revision == revision) {
			review = d
		}
	}
	var result UnitReviewResult
	if review.Revision == 0 || json.Unmarshal([]byte(review.Content), &result) != nil {
		return review, result, false, nil
	}
	return review, result, result.Turn != "" && result.Verdict.Decision == "satisfactory", nil
}

func (f *foreman) header(id string, stream config.WorkstreamID, unit, cause string, at time.Time) trace.Header {
	return trace.Header{Schema: "osmia.trace.transition", Version: trace.Version, ID: id, Revision: 1, Project: f.repository.Project(), Workstream: stream, Unit: unit, At: at, Actor: foremanActor, Cause: cause}
}

// request publishes the landing of the approval that review records as a
// durable operation.
func (f *foreman) request(ctx context.Context, stream config.WorkstreamID, unit string, review trace.Document, result UnitReviewResult, attempt int) error {
	subject := landingSubject(unit)
	state, err := f.repository.Workflow(stream, subject)
	if err != nil {
		return err
	}
	transition, event := landAttemptIDs(unit, review.Revision, attempt)
	candidate := result.Identity.Candidate
	in := landInput{Unit: unit, Review: review.Revision, Candidate: candidate.Revision, Base: candidate.BaseRevision}
	if attempt > 1 {
		in.Attempt = attempt
	}
	input, err := json.Marshal(in)
	if err != nil {
		return err
	}
	op := coreadapter.Operation{ID: trace.OperationID(f.repository.Project(), stream, event), Boundary: coreadapter.RepositoryBoundary, Action: LandAction, Input: input}
	reason := fmt.Sprintf("unit %s is approved by %s revision %d; landing squashes candidate %s onto %s at %s", unit, review.Path, review.Revision, candidate.Revision, featureBranch(stream), candidate.BaseRevision)
	tx := trace.Transaction{ExpectedVersion: state.Version,
		Transition: trace.Transition{Header: f.header(transition, stream, unit, fmt.Sprintf("%s-%d", reviewDocument(unit), review.Revision), f.s.Now()), Subject: subject, From: state.Value, To: fmt.Sprintf("requested-%d", review.Revision), Reason: reason},
		Events:     []trace.Event{{ID: event, Kind: LandAction, Body: fmt.Sprintf("Land unit %s", unit), Operation: &op}}}
	if _, err := f.repository.Transact(ctx, tx); err != nil && !errors.Is(err, trace.ErrConflict) {
		return err
	}
	return nil
}

func decodeLand(op coreadapter.Operation) (landInput, error) {
	var in landInput
	if op.Boundary != coreadapter.RepositoryBoundary || op.Action != LandAction {
		return in, fmt.Errorf("unsupported repository operation %q", op.Action)
	}
	dec := json.NewDecoder(bytes.NewReader(op.Input))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&in); err != nil {
		return in, fmt.Errorf("invalid land operation input: %w", err)
	}
	if in.Unit == "" || in.Review < 1 || in.Candidate == "" || in.Base == "" {
		return in, errors.New("land operation requires a unit, a positive review revision, a candidate and a base")
	}
	return in, nil
}

// stream returns the workstream a landing operation belongs to: the one whose
// run event derives the operation ID.
func (f *foreman) stream(op coreadapter.Operation, in landInput) (config.WorkstreamID, error) {
	streams, err := f.repository.Workstreams()
	if err != nil {
		return "", err
	}
	_, event := in.ids()
	for _, stream := range streams {
		if trace.OperationID(f.repository.Project(), stream, event) == op.ID {
			return stream, nil
		}
	}
	return "", fmt.Errorf("land operation %s belongs to no workstream", op.ID)
}

// outcome returns the recorded result of a landing: succeeded once it
// recorded the unit merged, failed once its refusal is recorded, nil before
// either.
func (f *foreman) outcome(stream config.WorkstreamID, in landInput) (*coreadapter.OperationResult, error) {
	transitions, err := trace.Read[trace.Transition](f.repository, stream)
	if err != nil {
		return nil, err
	}
	transition, _ := in.ids()
	for _, t := range transitions {
		switch t.ID {
		case transition + "-landed":
			return &coreadapter.OperationResult{Outcome: "succeeded", Evidence: t.Reason}, nil
		case transition + "-refused":
			return &coreadapter.OperationResult{Outcome: "failed", Evidence: t.Reason}, nil
		}
	}
	return nil, nil
}

// Inspect reads the recorded transitions and the clone. A recorded outcome
// completes the operation; otherwise it is absent, with evidence of whether
// the feature branch already holds this operation's landing commit, which
// Apply then records without committing again.
func (f *foreman) Inspect(ctx context.Context, op coreadapter.Operation) (coreadapter.Observation, error) {
	in, err := decodeLand(op)
	if err != nil {
		return coreadapter.Observation{}, err
	}
	stream, err := f.stream(op, in)
	if err != nil {
		return coreadapter.Observation{}, err
	}
	result, err := f.outcome(stream, in)
	if err != nil {
		return coreadapter.Observation{}, err
	}
	if result != nil {
		return coreadapter.Observation{State: coreadapter.EffectCompleted, Evidence: "landing " + result.Outcome, Result: result}, nil
	}
	branch := featureBranch(stream)
	g, err := featureWorkspaces(f.cfg, f.repository).Of(stream)
	if err != nil {
		return coreadapter.Observation{}, err
	}
	tip, exists, err := g.Branch(ctx, branch)
	if err != nil {
		return coreadapter.Observation{}, err
	}
	if !exists {
		return coreadapter.Observation{State: coreadapter.EffectAbsent, Evidence: fmt.Sprintf("the clone has no feature branch %s", branch)}, nil
	}
	if landed, err := landingCommit(ctx, g, tip, in, op.ID); err != nil {
		return coreadapter.Observation{}, err
	} else if landed {
		return coreadapter.Observation{State: coreadapter.EffectAbsent, Evidence: fmt.Sprintf("feature branch %s is at %s, this operation's landing commit on %s; the landing records it without committing again", branch, tip, in.Base)}, nil
	}
	return coreadapter.Observation{State: coreadapter.EffectAbsent, Evidence: fmt.Sprintf("feature branch %s is at %s and holds no commit of this landing; the landing checks its approval before committing", branch, tip)}, nil
}

// landingCommit reports whether tip is the commit the landing operation
// made: its only parent is the approved base and its message names the
// operation.
func landingCommit(ctx context.Context, g workspace.Provider, tip string, in landInput, operation string) (bool, error) {
	if tip == in.Base {
		return false, nil
	}
	c, err := g.Commit(ctx, tip)
	if err != nil {
		return false, err
	}
	return slices.Equal(c.Parents, []string{in.Base}) && slices.Contains(strings.Split(c.Message, "\n"), landingTrailer+": "+operation), nil
}

// Apply lands the approved unit. A feature branch whose tip is this
// operation's commit on the approved base is an interrupted landing: the
// commit is recorded and not made again. Otherwise the approval must still be
// current: the unit approved by this review revision, and its candidate,
// base, report, seal, spec and plan unchanged since. The candidate's tree is
// then committed once on the base with a message derived from the criteria
// the unit serves, and the feature branch and its workspace move to it.
// One commit then records units/<unit>/landing.json, the unit's move to
// merged and every dependent unit whose dependencies have all merged moving
// to ready. A stale approval is refused with the reason as its result, and
// nothing is committed. Its local step and durable result share a
// clone-locked checkpoint. Cancellation restores it before unlocking, while
// a process crash leaves it for startup recovery before the landing retries.
func (f *foreman) Apply(ctx context.Context, op coreadapter.Operation) (coreadapter.OperationResult, error) {
	in, err := decodeLand(op)
	if err != nil {
		return coreadapter.OperationResult{}, err
	}
	stream, err := f.stream(op, in)
	if err != nil {
		return coreadapter.OperationResult{}, err
	}
	approved, err := f.repository.GroupApproved(stream)
	if err != nil {
		return coreadapter.OperationResult{}, err
	}
	if !approved {
		return coreadapter.OperationResult{Outcome: "failed", Evidence: "workgroup seal preparations are pending"}, nil
	}
	if result, err := f.outcome(stream, in); err != nil || result != nil {
		if err != nil {
			return coreadapter.OperationResult{}, err
		}
		return *result, nil
	}
	if held, err := f.s.ChangeRepositoryHeld(f.repository, stream, op); err != nil || held {
		if err != nil {
			return coreadapter.OperationResult{}, err
		}
		return coreadapter.OperationResult{}, errChangeBoundary
	}
	requested, err := f.requestedAt(stream, op.ID)
	if err != nil {
		return coreadapter.OperationResult{}, err
	}
	g, err := featureWorkspaces(f.cfg, f.repository).Of(stream)
	if err != nil {
		return coreadapter.OperationResult{}, err
	}
	if err := f.s.Step("land-local-step:" + string(stream)); err != nil {
		return coreadapter.OperationResult{}, err
	}
	return checkpointed(ctx, string(stream)+"/"+op.ID, requested, []workspace.Provider{g}, func(ctx context.Context) (coreadapter.OperationResult, error) {
		return f.land(ctx, op.ID, stream, in, g, requested)
	})
}

// land is one attempt of the landing Apply describes, on the feature branch
// workspaces g.
func (f *foreman) land(ctx context.Context, operation string, stream config.WorkstreamID, in landInput, g workspace.Provider, requested time.Time) (coreadapter.OperationResult, error) {
	review, result, satisfactory, err := f.approval(stream, in.Unit, in.Review)
	if err != nil {
		return coreadapter.OperationResult{}, err
	}
	if !satisfactory || result.Identity.Candidate.Revision != in.Candidate || result.Identity.Candidate.BaseRevision != in.Base {
		return coreadapter.OperationResult{}, fmt.Errorf("%s revision %d does not record the approval of candidate %s from %s", reviewDocument(in.Unit), in.Review, in.Candidate, in.Base)
	}
	branch := featureBranch(stream)
	acquired, err := g.Acquire(ctx, vcs.Request{Name: string(stream), Branch: branch})
	if err != nil {
		return coreadapter.OperationResult{}, fmt.Errorf("feature branch %s: %w", branch, err)
	}
	w := acquired.(workspace.Worktree)
	tip, _, err := g.Branch(ctx, branch)
	if err != nil {
		return coreadapter.OperationResult{}, err
	}
	commit := ""
	if landed, err := landingCommit(ctx, g, tip, in, operation); err != nil {
		return coreadapter.OperationResult{}, err
	} else if landed {
		commit = tip
	}
	if commit == "" {
		if reason, err := f.current(ctx, stream, in, result); err != nil {
			return coreadapter.OperationResult{}, err
		} else if reason != "" {
			return f.refuse(ctx, stream, in, reason)
		}
		message, err := f.message(stream, in, review, result, operation)
		if err != nil {
			return coreadapter.OperationResult{}, err
		}
		if err := f.s.Step("land-committing"); err != nil {
			return coreadapter.OperationResult{}, err
		}
		if commit, err = g.Squash(ctx, in.Base, in.Candidate, message, requested); err != nil {
			return coreadapter.OperationResult{}, err
		}
		if err := f.s.Step("land-committed"); err != nil {
			return coreadapter.OperationResult{}, err
		}
		if err := g.Advance(ctx, w, in.Base, commit); err != nil {
			return coreadapter.OperationResult{}, err
		}
		if err := f.s.Step("land-advanced"); err != nil {
			return coreadapter.OperationResult{}, err
		}
	}
	return f.record(ctx, stream, in, operation, review, result, commit)
}

// requestedAt returns when the operation was asked for, the time the commit
// it makes is stamped with.
func (f *foreman) requestedAt(stream config.WorkstreamID, operation string) (time.Time, error) {
	ops, err := f.repository.Operations(stream)
	if err != nil {
		return time.Time{}, err
	}
	i := slices.IndexFunc(ops, func(o trace.OperationRecord) bool { return o.Operation.ID == operation })
	if i < 0 {
		return time.Time{}, fmt.Errorf("operation %s is not recorded", operation)
	}
	return ops[i].Transition.At, nil
}

// current returns why the approval can no longer land, or "" when the
// workstream is building or assembled, the unit is approved by this review
// revision, its candidate holds no stored conflict and every reviewed input
// is current.
func (f *foreman) current(ctx context.Context, stream config.WorkstreamID, in landInput, result UnitReviewResult) (string, error) {
	feature, err := f.repository.Workflow(stream, trace.FeatureSubject)
	if err != nil {
		return "", err
	}
	maintenance := false
	if feature.Value == DeliveredState {
		maintenance, err = f.s.WorkgroupEdgePending(f.repository, stream)
		if err != nil {
			return "", err
		}
	}
	if feature.Value != BuildingState && feature.Value != AssembledState && !maintenance {
		return fmt.Sprintf("the workstream is %s, not building or assembled", featureState(feature.Value)), nil
	}
	state, err := f.repository.Workflow(stream, trace.UnitSubject(in.Unit))
	if err != nil {
		return "", err
	}
	if state.Value != UnitApproved {
		return fmt.Sprintf("the unit is %s, not approved", state.Value), nil
	}
	latest, _, _, err := f.approval(stream, in.Unit, 0)
	if err != nil {
		return "", err
	}
	if latest.Revision != in.Review {
		return fmt.Sprintf("%s revision %d replaced the approval", latest.Path, latest.Revision), nil
	}
	g, err := newUnitWorkspaces(f.cfg, f.repository).Of(stream)
	if err != nil {
		return "", err
	}
	if stored, err := g.StoredConflicts(ctx, in.Candidate); err != nil {
		return "", err
	} else if len(stored) != 0 {
		return fmt.Sprintf("candidate %s holds unresolved conflicts in %s", in.Candidate, strings.Join(stored, ", ")), nil
	}
	reason, err := f.staleInputs(ctx, stream, in.Unit, result.Identity)
	if err != nil || reason == "" {
		return "", err
	}
	return "stale approval: " + reason, nil
}

// message returns the landing commit's message: its subject is the unit's
// title, or else the text of the criteria it serves taken from the sealed
// spec, its body lists each criterion, and its trailers name the workstream,
// unit, candidate, base, approval and operation.
func (f *foreman) message(stream config.WorkstreamID, in landInput, review trace.Document, result UnitReviewResult, operation string) (string, error) {
	unit, err := sealedUnit(f.repository, coreadapter.Scope{Workstream: string(stream), Unit: in.Unit})
	if err != nil {
		return "", err
	}
	latest, _, found, err := seal.Latest(f.repository, stream)
	if err != nil {
		return "", err
	}
	if !found {
		return "", fmt.Errorf("workstream %s has no seal", stream)
	}
	docs, err := trace.Read[trace.Document](f.repository, stream)
	if err != nil {
		return "", err
	}
	i := slices.IndexFunc(docs, func(d trace.Document) bool { return d.ID == plan.SpecDocument && d.Revision == latest.Revision.Spec })
	if i < 0 {
		return "", fmt.Errorf("workstream %s has no recorded spec.md revision %d", stream, latest.Revision.Spec)
	}
	return landingMessage(stream, unit, plan.ParseSpec(docs[i].Content), in, fmt.Sprintf("%s revision %d", review.Path, review.Revision), operation), nil
}

// subjectLength is the longest landing commit subject, in runes.
const subjectLength = 72

func landingMessage(stream config.WorkstreamID, unit plan.Unit, spec plan.Spec, in landInput, approval, operation string) string {
	var texts, lines []string
	for _, c := range landingCriteria(unit) {
		text := c
		if n, ok := plan.ParseCitation(c); ok {
			if criterion, ok := spec.Criterion(n); ok {
				text = strings.Join(strings.Fields(criterion.Text), " ")
			}
		}
		texts = append(texts, strings.TrimSuffix(text, "."))
		lines = append(lines, fmt.Sprintf("- %s: %s", c, text))
	}
	subject := []rune(strings.Join(texts, "; "))
	if unit.Title != "" {
		subject = []rune(strings.Join(strings.Fields(unit.Title), " "))
	} else if len(subject) == 0 {
		subject = []rune(strings.Join(strings.Fields(unit.Task), " "))
	}
	if len(subject) > subjectLength {
		subject = append(subject[:subjectLength-1], '…')
	}
	title := unit.ID
	if unit.Title != "" {
		title += " (" + unit.Title + ")"
	}
	return fmt.Sprintf("%s\n\nUnit %s of workstream %s serves:\n%s\n\nOsmia-Workstream: %s\nOsmia-Unit: %s\nOsmia-Candidate: %s\nOsmia-Base: %s\nOsmia-Approval: %s\n%s: %s\n",
		string(subject), title, stream, strings.Join(lines, "\n"), stream, unit.ID, in.Candidate, in.Base, approval, landingTrailer, operation)
}

// record records, in one commit, the landing document, the unit's move to
// merged with a notice for the chief of staff, the landing's outcome and
// every planned unit whose dependencies have now all merged moving to ready.
// A landing already recorded returns its result and records nothing more.
func (f *foreman) record(ctx context.Context, stream config.WorkstreamID, in landInput, operation string, review trace.Document, result UnitReviewResult, commit string) (coreadapter.OperationResult, error) {
	b, found, err := f.readActive(stream)
	if err != nil {
		return coreadapter.OperationResult{}, err
	}
	if !found {
		return coreadapter.OperationResult{}, fmt.Errorf("workstream %s is not building or assembled", stream)
	}
	unit, ok := b.plan.Unit(in.Unit)
	if !ok {
		return coreadapter.OperationResult{}, fmt.Errorf("the sealed plan and follow-ups of workstream %s have no unit %s", stream, in.Unit)
	}
	g, err := featureWorkspaces(f.cfg, f.repository).Of(stream)
	if err != nil {
		return coreadapter.OperationResult{}, err
	}
	landed, err := g.Commit(ctx, commit)
	if err != nil {
		return coreadapter.OperationResult{}, err
	}
	identity := result.Identity
	approval := fmt.Sprintf("%s revision %d", review.Path, review.Revision)
	criteria := landingCriteria(unit)
	branch := featureBranch(stream)
	landing := UnitLanding{Unit: in.Unit, Operation: operation, Approval: approval, Turn: result.Turn, Candidate: in.Candidate, Base: in.Base,
		Spec: identity.Candidate.SpecRevision, Plan: identity.Candidate.PlanRevision, Seal: identity.Seal, Criteria: criteria, Branch: branch, Commit: commit, Message: landed.Message}
	content, err := json.MarshalIndent(landing, "", "  ")
	if err != nil {
		return coreadapter.OperationResult{}, err
	}
	docs, err := trace.Read[trace.Document](f.repository, stream)
	if err != nil {
		return coreadapter.OperationResult{}, err
	}
	revision := 1
	for _, d := range docs {
		if d.ID == landingDocument(in.Unit) {
			revision = d.Revision + 1
		}
	}
	at := f.s.Now()
	doc := trace.Document{Header: trace.Header{Schema: "osmia.trace.document", Version: trace.Version, ID: landingDocument(in.Unit), Revision: revision, Project: f.repository.Project(), Workstream: stream, Unit: in.Unit, At: at, Actor: foremanActor, Cause: operation},
		Path: fmt.Sprintf("units/%s/landing.json", in.Unit), Content: string(content) + "\n"}

	subject := trace.UnitSubject(in.Unit)
	merged := subject + "-" + UnitMerged
	reason := fmt.Sprintf("unit %s landed as %s on %s: %s approved candidate %s from %s, spec %s, plan %s, seal %d; criteria %s",
		in.Unit, commit, branch, approval, in.Candidate, in.Base, identity.Candidate.SpecRevision, identity.Candidate.PlanRevision, identity.Seal, strings.Join(criteria, ", "))
	transition, _ := in.ids()
	landingState := b.states[landingSubject(in.Unit)]
	txs := []trace.Transaction{{ExpectedVersion: landingState.Version,
		Transition: trace.Transition{Header: f.header(transition+"-landed", stream, in.Unit, operation, at), Subject: landingSubject(in.Unit), From: landingState.Value, To: fmt.Sprintf("landed-%d", in.Review), Reason: reason}}}
	for _, u := range newlyReady(b.plan, b.states, in.Unit) {
		state := b.states[trace.UnitSubject(u.ID)]
		txs = append(txs, trace.Transaction{ExpectedVersion: state.Version,
			Transition: trace.Transition{Header: f.header(trace.UnitSubject(u.ID)+"-"+UnitReady, stream, u.ID, operation, at), Subject: trace.UnitSubject(u.ID), From: UnitPlanned, To: UnitReady,
				Reason: fmt.Sprintf("unit %s is ready: every unit it depends on has merged: %s", u.ID, strings.Join(u.DependsOn, ", ")),
				DecidedBy: trace.DecidedByCode, DefaultTo: UnitReady}})
	}
	// The unit's move to merged is routed: the landing is the commit it
	// records, with the landing's own records and the units it makes ready.
	res, err := f.s.raise(ctx, false, UnitExitInput{
		Repo: f.repository, Stream: stream, Unit: in.Unit, State: b.states[subject], Trigger: TriggerLanded, Cause: operation,
		Default: UnitMerged, Targets: []string{UnitMerged}, Actor: foremanActor, Operation: operation,
		ID: merged, HeaderCause: operation, Reason: reason,
		Events:    []trace.Event{trace.Progress(merged, "unit", fmt.Sprintf("Unit %s merged: it landed as %s on %s.", in.Unit, commit, branch))},
		Documents: []trace.Document{doc}, With: txs,
		Candidate: func(context.Context) (string, error) { return "", nil },
	})
	switch {
	case err != nil:
		return coreadapter.OperationResult{}, err
	case !res.Committed:
		if recorded, outcomeErr := f.outcome(stream, in); outcomeErr == nil && recorded != nil {
			return *recorded, nil
		}
		return coreadapter.OperationResult{}, fmt.Errorf("the landing of unit %s is not recorded: the router held its move to merged (%s)", in.Unit, res.Hold)
	}
	if err := f.s.Step("land-recorded"); err != nil {
		return coreadapter.OperationResult{}, err
	}
	return coreadapter.OperationResult{Outcome: "succeeded", Evidence: reason}, nil
}

// newlyReady returns the planned units of the plan, in plan order, that
// depend on the unit that merges and whose every other dependency has
// merged.
func newlyReady(p plan.Plan, states map[string]trace.WorkflowState, merging string) []plan.Unit {
	merged := func(id string) bool { return id == merging || states[trace.UnitSubject(id)].Value == UnitMerged }
	var out []plan.Unit
	for _, u := range p.Units {
		if states[trace.UnitSubject(u.ID)].Value == UnitPlanned && slices.Contains(u.DependsOn, merging) && !slices.ContainsFunc(u.DependsOn, func(d string) bool { return !merged(d) }) {
			out = append(out, u)
		}
	}
	return out
}

// refuse records why the landing was refused, tells the chief of staff, and
// returns the refusal as the operation's result. The unit stays approved and
// the feature branch is unchanged.
func (f *foreman) refuse(ctx context.Context, stream config.WorkstreamID, in landInput, reason string) (coreadapter.OperationResult, error) {
	transition, _ := in.ids()
	id := transition + "-refused"
	subject := landingSubject(in.Unit)
	state, err := f.repository.Workflow(stream, subject)
	if err != nil {
		return coreadapter.OperationResult{}, err
	}
	recorded := fmt.Sprintf("unit %s did not land: %s", in.Unit, reason)
	body := fmt.Sprintf("Unit %s did not land: %s. Nothing was committed to the feature branch; the unit lands once an approval of its current candidate, base, spec and plan is recorded.", in.Unit, reason)
	tx := trace.Transaction{ExpectedVersion: state.Version,
		Transition: trace.Transition{Header: f.header(id, stream, in.Unit, transition, f.s.Now()), Subject: subject, From: state.Value, To: fmt.Sprintf("refused-%d", in.Review), Reason: recorded},
		Events:     []trace.Event{trace.Notice(id, "chief", body)}}
	if _, err := f.repository.Transact(ctx, tx); err != nil {
		return coreadapter.OperationResult{}, err
	}
	return coreadapter.OperationResult{Outcome: "failed", Evidence: recorded}, nil
}
