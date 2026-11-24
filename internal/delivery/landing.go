package delivery

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/kpenfound/osmia/internal/api"
	checkrun "github.com/kpenfound/osmia/internal/checks"
	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/execution"
	"github.com/kpenfound/osmia/internal/ghlog"
	"github.com/kpenfound/osmia/internal/intake"
	"github.com/kpenfound/osmia/internal/router"
	"github.com/kpenfound/osmia/internal/seal"
	"github.com/kpenfound/osmia/internal/trace"
	"github.com/kpenfound/osmia/internal/workspace"
)

const (
	// LandDeliveryAction is the repository-boundary operation action that lands
	// an assembled workstream's signed commit on the upstream base branch.
	LandDeliveryAction = "land-delivery"
	// LandingSubject is the workflow subject that tracks the delivery landings
	// of a workstream: requested-<n> once landing attempt n is asked for, then
	// landed-<n>, fell-back-<n>, base-moved-<n> or failed-<n>.
	LandingSubject = "delivery-landing"
	// LandingPath and LandingDocument are the workstream document that records
	// the landing, and LandingIntentPath and LandingIntentDocument the one that
	// records the intent to push before the push and the refusal after it.
	LandingPath           = "final/landing.json"
	LandingDocument       = "final-landing"
	LandingIntentPath     = "final/landing-intent.json"
	LandingIntentDocument = "final-landing-intent"
	// LandingCause is the cause of the records a landing writes.
	LandingCause = "landing"
	// LandingPushing and LandingRefused are the statuses of a landing intent:
	// the base branch is being moved, then the upstream refused the push.
	LandingPushing = "pushing"
	LandingRefused = "refused"
)

// The outcomes of a landing operation.
const (
	LandOutcomeLanded    = "landed"
	LandOutcomeFellBack  = "fell-back"
	LandOutcomeBaseMoved = "base-moved"
	LandOutcomeFailed    = "failed"
)

// DeliveryLanding is a workstream's recorded landing.
type DeliveryLanding = api.DeliveryLanding

// ErrLandingHeld is the error of a landing the router holds for now. Nothing
// was pushed; the landing is applied once the hold clears.
var ErrLandingHeld = errors.New("the router holds the landing")

// LandDecider is who decided to land: the owner, or Jev's delivery-route gate
// decision with the judgment it rests on. The zero value is the owner.
type LandDecider struct {
	By       string `json:"by,omitempty"`
	Judgment string `json:"judgment,omitempty"`
}

// Owner reports whether the owner decided.
func (d LandDecider) Owner() bool { return d.By == "" || d.By == trace.DecidedByOwner }

func (d LandDecider) name() string {
	if d.Owner() {
		return trace.DecidedByOwner
	}
	return d.By
}

func (d LandDecider) valid() bool {
	return d.Owner() && d.Judgment == "" || d.By == trace.DecidedByJev && d.Judgment != ""
}

func (d LandDecider) actor() trace.Actor {
	if d.By == trace.DecidedByJev {
		return trace.JevActor
	}
	return ownerActor
}

func (d LandDecider) approval() (by, judgment string) {
	if d.By == trace.DecidedByJev {
		return d.By, d.Judgment
	}
	return "", ""
}

// LandInput names one landing attempt: its number among the workstream's,
// the push it lands (the signed commit that push made, and the reviewed
// commit it signs), who decided and the blocking findings the owner accepted.
type LandInput struct {
	Landing  int         `json:"landing"`
	Push     int         `json:"push"`
	Review   int         `json:"review"`
	Reviewed string      `json:"reviewed"`
	Commit   string      `json:"commit"`
	Decider  LandDecider `json:"decider"`
	Accepted []string    `json:"accepted_blocking,omitempty"`
}

// LandingIntent is the durable record that a landing is about to push, written
// before the push, and, once the upstream refuses the push, the record of the
// refusal. BaseBefore is the commit the base branch was at when the landing
// began.
type LandingIntent struct {
	Landing    int         `json:"landing"`
	Push       int         `json:"push"`
	Review     int         `json:"review"`
	Operation  string      `json:"operation"`
	Branch     string      `json:"branch"`
	Commit     string      `json:"commit"`
	Reviewed   string      `json:"reviewed"`
	Remote     string      `json:"remote"`
	Upstream   string      `json:"upstream"`
	BaseBranch string      `json:"base_branch"`
	BaseBefore string      `json:"base_before"`
	Decider    LandDecider `json:"decider"`
	Status     string      `json:"status"`
	Refusal    string      `json:"refusal,omitempty"`
	At         time.Time   `json:"at"`
}

// LandIDs returns the IDs of the transition that asks for landing attempt n and
// of its run event.
func LandIDs(landing int) (transition, event string) {
	transition = fmt.Sprintf("%s-%d", LandingSubject, landing)
	return transition, trace.EventID(transition, "run")
}

// LandRequest asks to land an assembled workstream. AcceptBlocking accepts what
// the final review found blocking, as the owner's delivery approval does.
type LandRequest struct {
	Decider        LandDecider
	AcceptBlocking bool
}

// The outcomes a landing reports to its caller.
type LandingOutcome string

const (
	// Landed: the base branch is at the signed commit and the workstream is
	// delivered.
	Landed LandingOutcome = "landed"
	// LandingHeld: the router holds the landing move, so nothing was pushed. The
	// landing is recorded and applied once the hold clears.
	LandingHeld LandingOutcome = "held"
	// LandingFellBack: the upstream refused the push, so the draft pull request
	// was marked ready and the workstream delivered.
	LandingFellBack LandingOutcome = "fell-back"
	// LandingRefusedBeforePush: a precondition does not hold, so nothing was
	// pushed.
	LandingRefusedBeforePush LandingOutcome = "refused"
	// LandingBaseMoved: the upstream base branch moved, so nothing landed. The
	// rebase of the workstream was asked for; checks and review follow, and the
	// landing is asked for again on the new commit.
	LandingBaseMoved LandingOutcome = "base-moved"
	// LandingPending: the outcome of the push is not known. The landing is
	// reconciled against the base branch under the service's bounded recovery.
	LandingPending LandingOutcome = "pending"
)

// LandingResult is what landing a workstream did. Reason says why when the
// workstream did not land, and Commit is the signed commit.
type LandingResult struct {
	Outcome LandingOutcome
	Reason  string
	Commit  string
}

// LandWorkstream lands the assembled workstream of an opted-in project: the
// one entry point of the owner's "Approve and land" and of Jev's binding land
// decision. It refuses, with a reason and before anything is pushed, unless the
// project lands, the workstream is assembled with its final review current and
// nothing waits to merge before it, and checks passed on the exact signed
// commit of its current push. The landing is then recorded as an operation and
// applied through the router, which holds it while another operation of the
// workstream is in flight or work is paused or out of budget; a held landing
// pushes nothing and is applied once the hold clears. The result says which
// of landed, held, fell back, base moved, refused or pending it was.
func (s *Controller) LandWorkstream(ctx context.Context, repository *trace.Repository, stream config.WorkstreamID, req LandRequest) (LandingResult, error) {
	if !req.Decider.valid() {
		return LandingResult{}, errors.New("a landing is decided by the owner or by Jev's decision, which names its judgment")
	}
	lander := &Lander{S: s, Repository: repository}
	if _, input, outcome, found, err := lander.latest(stream); err != nil {
		return LandingResult{}, err
	} else if found && outcome != nil {
		switch outcome.Outcome {
		case LandOutcomeLanded:
			return LandingResult{Outcome: Landed, Reason: outcome.Evidence, Commit: input.Commit}, nil
		case LandOutcomeFellBack:
			return LandingResult{Outcome: LandingFellBack, Reason: outcome.Evidence, Commit: input.Commit}, nil
		}
	}
	cfg := s.About(repository)
	target, reason, err := s.landingTarget(ctx, repository, cfg, stream, req.AcceptBlocking, true)
	if err != nil {
		return LandingResult{}, err
	}
	if reason != "" {
		return LandingResult{Outcome: LandingRefusedBeforePush, Reason: reason}, nil
	}
	return s.landPush(ctx, repository, stream, req, target)
}

func (s *Controller) landPush(ctx context.Context, repository *trace.Repository, stream config.WorkstreamID, req LandRequest, target landingPush) (LandingResult, error) {
	lander := &Lander{S: s, Repository: repository}
	attempt, input, outcome, found, err := lander.latest(stream)
	if err != nil {
		return LandingResult{}, err
	}
	if found && outcome != nil && outcome.Outcome == LandOutcomeBaseMoved && input.Push == target.push.Number {
		return LandingResult{Outcome: LandingBaseMoved, Reason: outcome.Evidence, Commit: input.Commit}, nil
	}
	var op coreadapter.Operation
	if found && outcome == nil && input.Push == target.push.Number {
		if op, err = lander.operation(stream, attempt); err != nil {
			return LandingResult{}, err
		}
	} else {
		in := LandInput{Landing: attempt + 1, Push: target.push.Number, Review: target.push.Review, Reviewed: target.push.Reviewed, Commit: target.push.Commit, Decider: req.Decider}
		if req.AcceptBlocking {
			in.Accepted = ReportBlocking(target.report)
		}
		if op, err = lander.Request(ctx, stream, in); err != nil {
			return LandingResult{}, err
		}
	}
	result, err := lander.Apply(ctx, op)
	return lander.reported(op, result, err), nil
}

func (l *Lander) reported(op coreadapter.Operation, result coreadapter.OperationResult, err error) LandingResult {
	in, _ := DecodeLand(op)
	switch {
	case errors.Is(err, ErrLandingHeld):
		return LandingResult{Outcome: LandingHeld, Reason: err.Error(), Commit: in.Commit}
	case err != nil:
		return LandingResult{Outcome: LandingPending, Reason: err.Error(), Commit: in.Commit}
	}
	switch result.Outcome {
	case LandOutcomeLanded:
		return LandingResult{Outcome: Landed, Reason: result.Evidence, Commit: in.Commit}
	case LandOutcomeFellBack:
		return LandingResult{Outcome: LandingFellBack, Reason: result.Evidence, Commit: in.Commit}
	case LandOutcomeBaseMoved:
		return LandingResult{Outcome: LandingBaseMoved, Reason: result.Evidence, Commit: in.Commit}
	}
	return LandingResult{Outcome: LandingRefusedBeforePush, Reason: result.Evidence, Commit: in.Commit}
}

type landingPush struct {
	push   Push
	report FinalReport
}

func (s *Controller) landingTarget(ctx context.Context, repository *trace.Repository, cfg *config.Config, stream config.WorkstreamID, acceptBlocking, gate bool) (landingPush, string, error) {
	var target landingPush
	switch {
	case !cfg.HasProject() || cfg.Project.ID != repository.Project():
		return target, "the project is not active", nil
	case !cfg.Project.Lands():
		return target, fmt.Sprintf("the project's delivery is %q, so it delivers by pull request and never lands on its base branch; set delivery to %q to land", cfg.Project.Delivery, config.DeliveryJev), nil
	}
	feature, err := repository.Workflow(stream, trace.FeatureSubject)
	if err != nil {
		return target, "", err
	}
	if feature.Value != AssembledState {
		return target, fmt.Sprintf("the workstream is %s; only an assembled workstream lands", featureState(feature.Value)), nil
	}
	push, found, err := CurrentPush(repository, stream)
	if err != nil {
		return target, "", err
	}
	if !found {
		return target, "the workstream has no push of its branch to a pull request, so there is no signed commit to land", nil
	}
	if reason, err := landingChecks(repository, stream, push); err != nil || reason != "" {
		return target, reason, err
	}
	report, found, err := LatestFinalReport(repository, stream)
	if err != nil {
		return target, "", err
	}
	switch {
	case !found || report.Review != push.Review || report.Commit != push.Reviewed:
		return target, fmt.Sprintf("the current push signs reviewed commit %s of final review %d, which is not the latest final review's commit", push.Reviewed, push.Review), nil
	case report.Upstream == nil || report.Upstream.Workstream != "":
		return target, "the workstream's branch is based on another workstream's branch, which cannot land on the project's base branch", nil
	case !strings.EqualFold(push.Fork, push.Upstream) || !strings.EqualFold(push.Upstream, cfg.Project.Upstream) || push.Base != cfg.Project.BaseBranch:
		return target, fmt.Sprintf("the current push is a pull request against %s of %s from %s, not one against the project's base branch %s of %s", push.Base, push.Upstream, push.Fork, cfg.Project.BaseBranch, cfg.Project.Upstream), nil
	}
	target = landingPush{push: push, report: report}
	if !gate {
		return target, "", nil
	}
	if pending, err := s.UnresolvedWork(repository, stream); err != nil || pending {
		return target, "necessary discovered work or a plan revision remains unresolved", err
	}
	if wait, err := s.EdgePublicationWait(repository, stream); err != nil || wait != "" {
		return target, wait, err
	}
	if reference, waiting, err := s.UnresolvedMergeOrderEdge(repository, stream); err != nil || waiting {
		if reference == "" {
			reference = "a merge-order dependency"
		}
		return target, fmt.Sprintf("%s has not merged yet", reference), err
	}
	if _, reason, err := s.finalGate(ctx, repository, stream); err != nil || reason != "" {
		return target, reason, err
	}
	if blocking := ReportBlocking(report); len(blocking) > 0 && !acceptBlocking {
		return target, fmt.Sprintf("final review %d has blocking findings: %s; accept them as they are to land, or wait for the work that resolves them", report.Review, strings.Join(blocking, "; ")), nil
	}
	return target, "", nil
}

func landingChecks(repository *trace.Repository, stream config.WorkstreamID, push Push) (string, error) {
	_, latest, err := IncompleteFinalChecks(repository, stream, push.Review, push.Reviewed)
	if err != nil {
		return "", err
	}
	switch {
	case latest.Review == 0 || latest.Commit != push.Reviewed:
		return fmt.Sprintf("no check result is recorded for reviewed commit %s, so checks have not passed on the signed commit %s", push.Reviewed, push.Commit), nil
	case latest.Status != checkrun.ChecksPassed:
		return fmt.Sprintf("checks did not pass on the signed commit %s (the recorded run is %s)", push.Commit, latest.Status), nil
	case latest.PushedCommit != "" && latest.PushedCommit != push.Commit, latest.Push != 0 && latest.Push != push.Number:
		return fmt.Sprintf("the passing checks ran on pushed commit %s of push %d, not on the signed commit %s of push %d", latest.PushedCommit, latest.Push, push.Commit, push.Number), nil
	}
	return "", nil
}

// Lander is the delivery landing's reconciler. Its operations push the base
// branch, so they record their intent first and reconcile against the fetched
// base branch before they push again.
type Lander struct {
	S          *Controller
	Repository *trace.Repository
}

var _ coreadapter.Reconciler = (*Lander)(nil)

var landings sync.Map

func lockLanding(id string) func() {
	m, _ := landings.LoadOrStore(id, &sync.Mutex{})
	mu := m.(*sync.Mutex)
	mu.Lock()
	return mu.Unlock
}

// LandingAction reports whether op is a delivery landing.
func LandingAction(op coreadapter.Operation) bool {
	return op.Boundary == coreadapter.RepositoryBoundary && op.Action == LandDeliveryAction
}

// DecodeLand decodes the input of a delivery landing operation.
func DecodeLand(op coreadapter.Operation) (LandInput, error) {
	var in LandInput
	if !LandingAction(op) {
		return in, fmt.Errorf("unsupported repository operation %q", op.Action)
	}
	dec := json.NewDecoder(bytes.NewReader(op.Input))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&in); err != nil {
		return in, fmt.Errorf("invalid landing operation input: %w", err)
	}
	if in.Landing < 1 || in.Push < 1 || in.Commit == "" || in.Reviewed == "" || !in.Decider.valid() {
		return in, errors.New("a landing operation names its attempt, the push, the signed and reviewed commits and who decided")
	}
	return in, nil
}

// Request records landing attempt in.Landing as a durable operation, once.
func (l *Lander) Request(ctx context.Context, stream config.WorkstreamID, in LandInput) (coreadapter.Operation, error) {
	transition, event := LandIDs(in.Landing)
	op, err := l.operationFor(stream, event, in)
	if err != nil {
		return coreadapter.Operation{}, err
	}
	subject, err := l.Repository.Workflow(stream, LandingSubject)
	if err != nil {
		return coreadapter.Operation{}, err
	}
	h := trace.Header{Schema: "osmia.trace.transition", Version: trace.Version, ID: transition, Revision: 1, Project: l.Repository.Project(), Workstream: stream, At: l.S.Now(), Actor: foremanActor, Cause: LandingCause}
	reason := fmt.Sprintf("%s decided to land signed commit %s of push %d on the base branch by a fast-forward", in.Decider.name(), in.Commit, in.Push)
	tx := trace.Transaction{ExpectedVersion: subject.Version,
		Transition: trace.Transition{Header: h, Subject: LandingSubject, From: subject.Value, To: fmt.Sprintf("requested-%d", in.Landing), Reason: reason},
		Events:     []trace.Event{{ID: event, Kind: LandDeliveryAction, Body: fmt.Sprintf("Land %s on the base branch", in.Commit), Operation: &op}}}
	if _, err := l.Repository.Transact(ctx, tx); err != nil && !errors.Is(err, trace.ErrConflict) {
		return coreadapter.Operation{}, err
	}
	return op, nil
}

func (l *Lander) operationFor(stream config.WorkstreamID, event string, in LandInput) (coreadapter.Operation, error) {
	input, err := json.Marshal(in)
	if err != nil {
		return coreadapter.Operation{}, err
	}
	return coreadapter.Operation{ID: trace.OperationID(l.Repository.Project(), stream, event), Boundary: coreadapter.RepositoryBoundary, Action: LandDeliveryAction, Input: input}, nil
}

func (l *Lander) operation(stream config.WorkstreamID, n int) (coreadapter.Operation, error) {
	records, err := l.Repository.Operations(stream)
	if err != nil {
		return coreadapter.Operation{}, err
	}
	for _, r := range records {
		if in, err := DecodeLand(r.Operation); err == nil && in.Landing == n {
			return r.Operation, nil
		}
	}
	return coreadapter.Operation{}, fmt.Errorf("landing attempt %d of workstream %s is not recorded", n, stream)
}

func (l *Lander) latest(stream config.WorkstreamID) (int, LandInput, *coreadapter.OperationResult, bool, error) {
	records, err := l.Repository.Operations(stream)
	if err != nil {
		return 0, LandInput{}, nil, false, err
	}
	var latest LandInput
	found := false
	for _, r := range records {
		in, err := DecodeLand(r.Operation)
		if err != nil {
			continue
		}
		if !found || in.Landing > latest.Landing {
			latest, found = in, true
		}
	}
	if !found {
		return 0, LandInput{}, nil, false, nil
	}
	outcome, err := l.Outcome(stream, latest.Landing)
	return latest.Landing, latest, outcome, true, err
}

// Stream returns the workstream a landing operation belongs to: the one whose
// run event derives the operation ID.
func (l *Lander) Stream(op coreadapter.Operation, in LandInput) (config.WorkstreamID, error) {
	streams, err := l.Repository.Workstreams()
	if err != nil {
		return "", err
	}
	_, event := LandIDs(in.Landing)
	for _, stream := range streams {
		if trace.OperationID(l.Repository.Project(), stream, event) == op.ID {
			return stream, nil
		}
	}
	return "", fmt.Errorf("landing operation %s belongs to no workstream", op.ID)
}

// Outcome returns the recorded result of landing attempt n, nil before it has
// one: landed once the landing record and the delivery are recorded, fell back
// once the draft pull request was marked ready after a refused push, base moved
// once the workstream's rebase was asked for, or failed once it ended without
// either.
func (l *Lander) Outcome(stream config.WorkstreamID, n int) (*coreadapter.OperationResult, error) {
	transitions, err := trace.Read[trace.Transition](l.Repository, stream)
	if err != nil {
		return nil, err
	}
	transition, _ := LandIDs(n)
	for _, t := range transitions {
		for _, outcome := range []string{LandOutcomeLanded, LandOutcomeFellBack, LandOutcomeBaseMoved, LandOutcomeFailed} {
			if t.ID == transition+"-"+outcome {
				return &coreadapter.OperationResult{Outcome: outcome, Evidence: t.Reason}, nil
			}
		}
	}
	return nil, nil
}

// Inspect reads the recorded transitions: a recorded outcome completes the
// operation. Otherwise it is absent, and Apply fetches the base branch and
// settles what the earlier attempts left before it does anything else.
func (l *Lander) Inspect(_ context.Context, op coreadapter.Operation) (coreadapter.Observation, error) {
	in, err := DecodeLand(op)
	if err != nil {
		return coreadapter.Observation{}, err
	}
	stream, err := l.Stream(op, in)
	if err != nil {
		return coreadapter.Observation{}, err
	}
	result, err := l.Outcome(stream, in.Landing)
	if err != nil {
		return coreadapter.Observation{}, err
	}
	if result != nil {
		return coreadapter.Observation{State: coreadapter.EffectCompleted, Evidence: "landing " + result.Outcome, Result: result}, nil
	}
	return coreadapter.Observation{State: coreadapter.EffectAbsent, Evidence: fmt.Sprintf("landing attempt %d is not recorded; it fetches the base branch and checks whether it holds %s before pushing", in.Landing, in.Commit)}, nil
}

// LandingInFlight reports whether the workstream has a landing attempt that is
// not over, or whose refusal's fallback is under way: delivery by another
// route waits for its outcome.
func LandingInFlight(repository *trace.Repository, stream config.WorkstreamID) (bool, error) {
	l := &Lander{Repository: repository}
	attempt, _, outcome, found, err := l.latest(stream)
	if err != nil || !found || outcome != nil {
		return false, err
	}
	intent, recorded, err := ReadLandingIntent(repository, stream, attempt)
	if err != nil {
		return false, err
	}
	return !recorded || intent.Status != LandingRefused, nil
}

// ReadLandingIntent returns the latest record of landing attempt n's intent, and
// whether one is recorded.
func ReadLandingIntent(repository *trace.Repository, stream config.WorkstreamID, n int) (LandingIntent, bool, error) {
	docs, err := trace.Read[trace.Document](repository, stream)
	if err != nil {
		return LandingIntent{}, false, err
	}
	for _, d := range slices.Backward(docs) {
		if d.ID != LandingIntentDocument {
			continue
		}
		var intent LandingIntent
		if err := json.Unmarshal([]byte(d.Content), &intent); err != nil {
			return LandingIntent{}, false, fmt.Errorf("%s revision %d: %w", LandingIntentPath, d.Revision, err)
		}
		if intent.Landing == n {
			return intent, true, nil
		}
	}
	return LandingIntent{}, false, nil
}

// ReadDeliveryLanding returns the workstream's recorded landing, and whether
// one is recorded.
func ReadDeliveryLanding(repository *trace.Repository, stream config.WorkstreamID) (DeliveryLanding, bool, error) {
	docs, err := trace.Read[trace.Document](repository, stream)
	if err != nil {
		return DeliveryLanding{}, false, err
	}
	for _, d := range slices.Backward(docs) {
		if d.ID != LandingDocument {
			continue
		}
		var landing DeliveryLanding
		if err := json.Unmarshal([]byte(d.Content), &landing); err != nil {
			return DeliveryLanding{}, false, fmt.Errorf("%s revision %d: %w", LandingPath, d.Revision, err)
		}
		return landing, true, nil
	}
	return DeliveryLanding{}, false, nil
}

type landingRun struct {
	*Lander
	cfg    *config.Config
	stream config.WorkstreamID
	op     coreadapter.Operation
	in     LandInput
	g      workspace.Provider
	remote string
	branch string
}

// Apply lands the workstream, or settles what an earlier attempt left.
//
// It fetches the base branch first. A base branch that already holds the exact
// signed commit, even one that has moved on since, means the landing happened:
// the landing record and the delivery are completed once, with no push, rebase
// or marking ready. Otherwise the push must still be the workstream's current
// one with passed checks, and the router must admit the landing; a hold pushes
// nothing and returns ErrLandingHeld. The intent to push is recorded before the
// push. A push the upstream rejects as not a fast-forward, or a fetched base
// that no longer fits the reviewed one, asks for the existing rebase and ends
// the attempt. A push the upstream explicitly refuses is recorded and followed
// by marking the draft pull request ready. Any other failure is unknown and
// returned for the bounded recovery to retry, which reconciles first.
func (l *Lander) Apply(ctx context.Context, op coreadapter.Operation) (coreadapter.OperationResult, error) {
	in, err := DecodeLand(op)
	if err != nil {
		return coreadapter.OperationResult{}, err
	}
	stream, err := l.Stream(op, in)
	if err != nil {
		return coreadapter.OperationResult{}, err
	}
	ctx = ghlog.WithScope(ctx, ghlog.Scope{Project: string(l.Repository.Project()), Workstream: string(stream)})
	defer lockLanding(op.ID)()
	if result, err := l.Outcome(stream, in.Landing); err != nil || result != nil {
		if err != nil {
			return coreadapter.OperationResult{}, err
		}
		return *result, nil
	}
	cfg := l.S.About(l.Repository)
	if !cfg.HasProject() || cfg.Project.ID != l.Repository.Project() {
		return coreadapter.OperationResult{}, errors.New("the project is not active")
	}
	run := &landingRun{Lander: l, cfg: cfg, stream: stream, op: op, in: in, branch: cfg.Project.BaseBranch}
	return run.apply(ctx)
}

func (r *landingRun) apply(ctx context.Context) (coreadapter.OperationResult, error) {
	intent, recorded, err := ReadLandingIntent(r.Repository, r.stream, r.in.Landing)
	if err != nil {
		return coreadapter.OperationResult{}, err
	}
	if recorded && intent.Status == LandingRefused {
		return r.fallback(ctx, intent)
	}
	if !r.cfg.Project.Lands() {
		return r.fail(ctx, fmt.Sprintf("the project's delivery is %q, so it never lands on its base branch", r.cfg.Project.Delivery))
	}
	if r.g, err = featureWorkspaces(r.cfg, r.Repository).Of(r.stream); err != nil {
		return coreadapter.OperationResult{}, err
	}
	if r.remote, err = r.g.Remote(ctx, r.cfg.Project.Upstream); err != nil {
		return coreadapter.OperationResult{}, err
	}
	fetched, err := r.g.Fetch(ctx, r.remote, r.branch)
	if err != nil {
		return coreadapter.OperationResult{}, fmt.Errorf("cannot fetch %s of %s: %w", r.branch, r.cfg.Project.Upstream, err)
	}
	if landed, err := r.holds(ctx, fetched); err != nil {
		return coreadapter.OperationResult{}, err
	} else if landed {
		return r.complete(ctx, intent, recorded, fetched)
	}

	feature, err := r.Repository.Workflow(r.stream, trace.FeatureSubject)
	if err != nil {
		return coreadapter.OperationResult{}, err
	}
	target, reason, err := r.S.landingTarget(ctx, r.Repository, r.cfg, r.stream, true, false)
	switch {
	case err != nil:
		return coreadapter.OperationResult{}, err
	case reason != "":
		return r.fail(ctx, reason)
	case target.push.Number != r.in.Push || target.push.Commit != r.in.Commit || target.push.Reviewed != r.in.Reviewed:
		return r.fail(ctx, fmt.Sprintf("push %d is no longer the workstream's current push (it is push %d at %s)", r.in.Push, target.push.Number, target.push.Commit))
	}
	if blocking := ReportBlocking(target.report); !slices.Equal(blocking, r.in.Accepted) && len(blocking) > 0 {
		return r.fail(ctx, fmt.Sprintf("final review %d found blocking problems the decision to land does not accept", target.report.Review))
	}
	if err := r.admit(ctx, feature); err != nil {
		return coreadapter.OperationResult{}, err
	}

	if moved, err := r.baseMoved(ctx, fetched, target.report); err != nil {
		return coreadapter.OperationResult{}, err
	} else if moved != "" {
		return r.moved(ctx, moved)
	}

	if !recorded {
		intent = LandingIntent{Landing: r.in.Landing, Push: r.in.Push, Review: r.in.Review, Operation: r.op.ID, Branch: featureBranch(r.stream), Commit: r.in.Commit, Reviewed: r.in.Reviewed,
			Remote: r.remote, Upstream: r.cfg.Project.Upstream, BaseBranch: r.branch, BaseBefore: fetched, Decider: r.in.Decider, Status: LandingPushing, At: r.S.Now()}
		if err := r.recordIntent(ctx, intent); err != nil {
			return coreadapter.OperationResult{}, err
		}
	}
	if err := r.S.landingStep("land-recorded"); err != nil {
		return coreadapter.OperationResult{}, err
	}
	pushed, err := r.g.FastForward(ctx, r.remote, r.in.Commit, r.branch)
	switch pushed.Outcome {
	case workspace.FastForwardPushed:
	case workspace.FastForwardBaseMoved:
		return r.moved(ctx, fmt.Sprintf("the upstream rejected the push of %s to %s as not a fast-forward: %s", r.in.Commit, r.branch, oneLine(pushed.Output)))
	case workspace.FastForwardRefused:
		intent.Status, intent.Refusal, intent.At = LandingRefused, oneLine(pushed.Output), r.S.Now()
		if err := r.recordIntent(ctx, intent); err != nil {
			return coreadapter.OperationResult{}, err
		}
		if err := r.S.landingStep("land-refused"); err != nil {
			return coreadapter.OperationResult{}, err
		}
		return r.fallback(ctx, intent)
	default:
		if err == nil {
			err = errors.New(pushed.Output)
		}
		return coreadapter.OperationResult{}, err
	}
	if err := r.S.landingStep("land-pushed"); err != nil {
		return coreadapter.OperationResult{}, err
	}
	confirmed, err := r.g.Fetch(ctx, r.remote, r.branch)
	if err != nil {
		return coreadapter.OperationResult{}, fmt.Errorf("cannot confirm the landing against %s of %s: %w", r.branch, r.cfg.Project.Upstream, err)
	}
	if landed, err := r.holds(ctx, confirmed); err != nil {
		return coreadapter.OperationResult{}, err
	} else if !landed {
		return coreadapter.OperationResult{}, fmt.Errorf("the push of %s was accepted, but the fetched %s of %s at %s does not hold it", r.in.Commit, r.branch, r.cfg.Project.Upstream, confirmed)
	}
	return r.complete(ctx, intent, true, confirmed)
}

func (r *landingRun) holds(ctx context.Context, tip string) (bool, error) {
	known, err := r.g.Known(ctx, r.in.Commit)
	if err != nil || !known {
		return false, err
	}
	return r.g.Ancestor(ctx, r.in.Commit, tip)
}

func (r *landingRun) baseMoved(ctx context.Context, fetched string, report FinalReport) (string, error) {
	if known, err := r.g.Known(ctx, fetched); err != nil || !known {
		return "", fmt.Errorf("the clone does not hold the fetched %s of %s at %s", r.branch, r.cfg.Project.Upstream, fetched)
	}
	if report.Upstream != nil && report.Upstream.Commit != "" {
		if known, err := r.g.Known(ctx, report.Upstream.Commit); err != nil {
			return "", err
		} else if known {
			if contained, err := r.g.Ancestor(ctx, report.Upstream.Commit, fetched); err != nil {
				return "", err
			} else if !contained {
				return fmt.Sprintf("%s of %s at %s no longer holds the base %s that final review %d read", r.branch, r.cfg.Project.Upstream, fetched, report.Upstream.Commit, report.Review), nil
			}
		}
	}
	if ahead, err := r.g.Ancestor(ctx, fetched, r.in.Commit); err != nil {
		return "", err
	} else if !ahead {
		return fmt.Sprintf("%s of %s moved to %s, which %s does not contain", r.branch, r.cfg.Project.Upstream, fetched, r.in.Commit), nil
	}
	return "", nil
}

func (r *landingRun) admit(ctx context.Context, feature trace.WorkflowState) error {
	admitter, ok := r.S.Router.(router.Admitter)
	if !ok {
		return errors.New("routing the landing of a workstream is unavailable")
	}
	move := r.move(feature, nil, trace.Transaction{})
	x := move.Exit()
	x.Targets[0].Works = true
	x.Prereq.Operations = func(context.Context, router.Target) (string, error) { return r.inFlight() }
	adm, err := admitter.Admit(ctx, x)
	switch {
	case err != nil:
		return err
	case adm.Hold == router.HoldStale:
		return fmt.Errorf("the workstream changed while its landing was applied: %s", adm.Reason)
	case !adm.Admitted:
		return fmt.Errorf("%w (%s): %s", ErrLandingHeld, adm.Hold, adm.Reason)
	}
	return nil
}

var landingOperations = []string{execution.LandAction, execution.RebaseAction, execution.DriftAction, FinalReviewAction, deliveryReviewAction, PublishAction, publishUpstreamAction, LandDeliveryAction}

func (r *landingRun) inFlight() (string, error) { return LandingBusy(r.Repository, r.stream, r.op) }

// LandingBusy returns why another operation of the workstream holds the landing
// operation op, or "": a landing, rebase, drift rebase, final review or
// publication that has no result yet. An earlier landing attempt whose outcome
// is recorded is over, though its operation may not have its result yet.
func LandingBusy(repository *trace.Repository, stream config.WorkstreamID, op coreadapter.Operation) (string, error) {
	records, err := repository.Operations(stream)
	if err != nil {
		return "", err
	}
	lander := &Lander{Repository: repository}
	for _, rec := range records {
		if rec.Result != nil || rec.Operation.ID == op.ID || !slices.Contains(landingOperations, rec.Operation.Action) {
			continue
		}
		if LandingAction(rec.Operation) {
			if earlier, err := DecodeLand(rec.Operation); err == nil {
				if outcome, err := lander.Outcome(stream, earlier.Landing); err != nil {
					return "", err
				} else if outcome != nil {
					continue
				}
			}
		}
		return fmt.Sprintf("a %s operation of the workstream is in flight", rec.Operation.Action), nil
	}
	return "", nil
}

func (r *landingRun) move(feature trace.WorkflowState, docs []trace.Document, tx trace.Transaction, with ...trace.Transaction) intake.PinnedMove {
	deliveredID, err := deliveredTransitionID(r.Repository, r.stream, fmt.Sprintf("%s-%s", mustTransition(r.in.Landing), DeliveredState))
	if err != nil {
		deliveredID = DeliveredState
	}
	h := trace.Header{Schema: "osmia.trace.transition", Version: trace.Version, ID: deliveredID, Revision: 1, Project: r.Repository.Project(), Workstream: r.stream, At: r.S.Now(), Actor: r.in.Decider.actor(), Cause: r.op.ID}
	t := trace.Transaction{ExpectedVersion: feature.Version, Transition: trace.Transition{Header: h, Subject: trace.FeatureSubject, From: feature.Value, To: DeliveredState,
		Reason: fmt.Sprintf("the signed commit %s of push %d landed on %s of %s by a fast-forward, decided by %s", r.in.Commit, r.in.Push, r.branch, r.cfg.Project.Upstream, r.in.Decider.name())}}
	if len(tx.Events) > 0 {
		t.Events = tx.Events
	}
	return deliveredMove(r.Repository, r.stream, TriggerLanded, r.in.Decider, docs, t, with...)
}

func mustTransition(landing int) string {
	transition, _ := LandIDs(landing)
	return transition
}

func (r *landingRun) recordIntent(ctx context.Context, intent LandingIntent) error {
	content, err := json.MarshalIndent(intent, "", "  ")
	if err != nil {
		return err
	}
	revision, err := nextRevision(r.Repository, r.stream, LandingIntentDocument)
	if err != nil {
		return err
	}
	doc := trace.Document{Header: trace.Header{Schema: "osmia.trace.document", Version: trace.Version, ID: LandingIntentDocument, Revision: revision, Project: r.Repository.Project(), Workstream: r.stream, At: intent.At, Actor: foremanActor, Cause: r.op.ID},
		Path: LandingIntentPath, Content: string(content) + "\n"}
	return r.Repository.RecordDocuments(ctx, []trace.Document{doc})
}

func (r *landingRun) header(outcome string) trace.Header {
	return trace.Header{Schema: "osmia.trace.transition", Version: trace.Version, ID: mustTransition(r.in.Landing) + "-" + outcome, Revision: 1, Project: r.Repository.Project(), Workstream: r.stream, At: r.S.Now(), Actor: foremanActor, Cause: r.op.ID}
}

func (r *landingRun) finish(ctx context.Context, outcome, reason, notice string, docs ...trace.Document) (coreadapter.OperationResult, error) {
	subject, err := r.Repository.Workflow(r.stream, LandingSubject)
	if err != nil {
		return coreadapter.OperationResult{}, err
	}
	h := r.header(outcome)
	tx := trace.Transaction{ExpectedVersion: subject.Version,
		Transition: trace.Transition{Header: h, Subject: LandingSubject, From: subject.Value, To: fmt.Sprintf("%s-%d", outcome, r.in.Landing), Reason: reason},
		Events:     []trace.Event{trace.Notice(h.ID, "chief", notice)}}
	var recordErr error
	if len(docs) > 0 {
		_, recordErr = r.Repository.RecordDocumentsWith(ctx, docs, tx)
	} else {
		_, recordErr = r.Repository.Transact(ctx, tx)
	}
	if recordErr != nil {
		if errors.Is(recordErr, trace.ErrConflict) {
			if result, outErr := r.Outcome(r.stream, r.in.Landing); outErr == nil && result != nil {
				return *result, nil
			}
		}
		return coreadapter.OperationResult{}, recordErr
	}
	return coreadapter.OperationResult{Outcome: outcome, Evidence: reason}, nil
}

func (r *landingRun) fail(ctx context.Context, why string) (coreadapter.OperationResult, error) {
	reason := fmt.Sprintf("the landing of %s was not applied: %s", r.in.Commit, why)
	return r.finish(ctx, LandOutcomeFailed, reason, fmt.Sprintf("%s. Nothing was pushed to the base branch, and the workstream is not delivered.", reason))
}

func (r *landingRun) moved(ctx context.Context, why string) (coreadapter.OperationResult, error) {
	if r.S.RequestRebase != nil {
		if err := r.S.RequestRebase(ctx, r.Repository, r.stream); err != nil {
			return coreadapter.OperationResult{}, err
		}
	}
	reason := fmt.Sprintf("the base branch moved, so %s did not land: %s", r.in.Commit, why)
	route := LandingRoute{Landing: r.in.Landing, Push: r.in.Push, Review: r.in.Review, Commit: r.in.Commit, Decider: r.in.Decider, Status: LandingRoutePending, At: r.S.Now()}
	doc, err := r.routeDocument(r.stream, route, r.op.ID)
	if err != nil {
		return coreadapter.OperationResult{}, err
	}
	return r.finish(ctx, LandOutcomeBaseMoved, reason, fmt.Sprintf("%s. The workstream is rebased onto the new base branch and checked and reviewed again, and %s's decision to land is kept: it lands on the new commit once they pass.", reason, r.in.Decider.name()), doc)
}

func (r *landingRun) complete(ctx context.Context, intent LandingIntent, recorded bool, confirmed string) (coreadapter.OperationResult, error) {
	if err := r.S.landingStep("land-confirmed"); err != nil {
		return coreadapter.OperationResult{}, err
	}
	feature, err := r.Repository.Workflow(r.stream, trace.FeatureSubject)
	if err != nil {
		return coreadapter.OperationResult{}, err
	}
	before := intent.BaseBefore
	if !recorded {
		if report, found, err := LatestFinalReport(r.Repository, r.stream); err == nil && found && report.Upstream != nil {
			before = report.Upstream.Commit
		}
	}
	at := r.S.Now()
	by, judgment := r.in.Decider.name(), r.in.Decider.Judgment
	landing := DeliveryLanding{Push: r.in.Push, Review: r.in.Review, Branch: featureBranch(r.stream), Commit: r.in.Commit, Reviewed: r.in.Reviewed, Remote: r.remote, Upstream: r.cfg.Project.Upstream,
		BaseBranch: r.branch, BaseBefore: before, Confirmed: confirmed, At: at, DecidedBy: by, Judgment: judgment}
	content, err := json.MarshalIndent(landing, "", "  ")
	if err != nil {
		return coreadapter.OperationResult{}, err
	}
	revision, err := nextRevision(r.Repository, r.stream, LandingDocument)
	if err != nil {
		return coreadapter.OperationResult{}, err
	}
	docs := []trace.Document{{Header: trace.Header{Schema: "osmia.trace.document", Version: trace.Version, ID: LandingDocument, Revision: revision, Project: r.Repository.Project(), Workstream: r.stream, At: at, Actor: foremanActor, Cause: r.op.ID},
		Path: LandingPath, Content: string(content) + "\n"}}
	if _, merged, err := ReadDeliveryMerge(r.Repository, r.stream); err != nil {
		return coreadapter.OperationResult{}, err
	} else if !merged {
		merge, err := json.Marshal(DeliveryMerge{Branch: landing.Branch, Commit: r.in.Commit, Upstream: seal.Base{Remote: r.remote, Branch: r.branch, Commit: r.in.Commit}})
		if err != nil {
			return coreadapter.OperationResult{}, err
		}
		mergeRevision, err := nextRevision(r.Repository, r.stream, MergeDocument)
		if err != nil {
			return coreadapter.OperationResult{}, err
		}
		docs = append(docs, trace.Document{Header: trace.Header{Schema: "osmia.trace.document", Version: trace.Version, ID: MergeDocument, Revision: mergeRevision, Project: r.Repository.Project(), Workstream: r.stream, At: at, Actor: foremanActor, Cause: r.op.ID},
			Path: MergePath, Content: string(merge)})
	}
	reason := fmt.Sprintf("the signed commit %s of push %d landed on %s of %s by a fast-forward from %s, confirmed against the fetched base at %s, decided by %s", r.in.Commit, r.in.Push, r.branch, r.cfg.Project.Upstream, before, confirmed, by)
	subject, err := r.Repository.Workflow(r.stream, LandingSubject)
	if err != nil {
		return coreadapter.OperationResult{}, err
	}
	landedHeader := r.header(LandOutcomeLanded)
	landed := trace.Transaction{ExpectedVersion: subject.Version,
		Transition: trace.Transition{Header: landedHeader, Subject: LandingSubject, From: subject.Value, To: fmt.Sprintf("%s-%d", LandOutcomeLanded, r.in.Landing), Reason: reason}}
	done := coreadapter.OperationResult{Outcome: LandOutcomeLanded, Evidence: reason}
	if feature.Value != AssembledState {
		landed.Events = []trace.Event{trace.Notice(landedHeader.ID, "state", fmt.Sprintf("Workstream landed: %s landed on %s of %s at %s by a fast-forward, decided by %s, although the workstream was already delivered another way.", r.in.Commit, r.branch, r.cfg.Project.Upstream, confirmed, by))}
		if _, err := r.Repository.RecordDocumentsWith(ctx, docs, landed); err != nil {
			if errors.Is(err, trace.ErrConflict) {
				if result, outErr := r.Outcome(r.stream, r.in.Landing); outErr == nil && result != nil {
					return *result, nil
				}
			}
			return coreadapter.OperationResult{}, err
		}
		return done, nil
	}
	notice := fmt.Sprintf("Workstream delivered: %s landed on %s of %s at %s by a fast-forward, decided by %s. No pull request was merged.", r.in.Commit, r.branch, r.cfg.Project.Upstream, confirmed, by)
	if judgment != "" {
		notice += " Jev's judgment: " + judgment + "."
	}
	t := r.move(feature, docs, trace.Transaction{Events: []trace.Event{trace.Notice(DeliveredState, "state", notice)}}, landed)
	res, err := r.S.routeLanded(ctx, t)
	if err != nil {
		return coreadapter.OperationResult{}, err
	}
	if !res.Committed {
		if result, outErr := r.Outcome(r.stream, r.in.Landing); outErr == nil && result != nil {
			return *result, nil
		}
		return coreadapter.OperationResult{}, fmt.Errorf("the landing of workstream %s could not be recorded: %s", r.stream, res.Reason)
	}
	return done, nil
}

func (s *Controller) routeLanded(ctx context.Context, m intake.PinnedMove) (router.Result, error) {
	if s.Router == nil {
		return router.Result{}, errors.New("routing the delivery of a workstream is unavailable")
	}
	return s.Router.Route(ctx, m.Exit())
}

func (s *Controller) landingStep(name string) error {
	if s.Step == nil {
		return nil
	}
	return s.Step(name)
}

func oneLine(text string) string {
	return strings.Join(strings.Fields(text), " ")
}

func jsonMarshal(v any) (string, error) {
	content, err := json.MarshalIndent(v, "", "  ")
	return string(content) + "\n", err
}
