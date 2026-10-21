package service

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/kpenfound/agentcore/vcs"
	"github.com/kpenfound/osmia/internal/bundle"
	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/followup"
	"github.com/kpenfound/osmia/internal/isolation"
	"github.com/kpenfound/osmia/internal/plan"
	reviewtools "github.com/kpenfound/osmia/internal/review"
	"github.com/kpenfound/osmia/internal/scheduler"
	"github.com/kpenfound/osmia/internal/seal"
	"github.com/kpenfound/osmia/internal/thread"
	"github.com/kpenfound/osmia/internal/trace"
	"github.com/kpenfound/osmia/internal/workspace"
)

// AssembledState is the feature state of a workstream whose planned units
// have all merged.
const AssembledState = "assembled"

// FinalReviewAction is the runner-boundary operation action that rebases an
// assembled workstream's feature branch onto upstream one last time and runs
// one committee member's read of the resulting branch against the sealed spec
// and the charter.
const FinalReviewAction = "final-review"

// FinalReportTool is the tool the final reader records its report with.
const FinalReportTool = "final_report"

const (
	// finalReviewSubject is the workflow subject that tracks a workstream's
	// final reviews: requested-<k> once review k is asked for, then
	// reviewed-<k> or failed-<k>.
	finalReviewSubject = "final-review"
	// finalRebasePath and finalReportPath are the workstream documents of
	// the final rebase and the final report, with their trace record IDs.
	finalRebasePath     = "final/rebase.json"
	finalRebaseDocument = "final-rebase"
	finalReportPath     = "final/report.json"
	finalReportDocument = "final-report"
	// finalReviewed and finalFailed are the outcomes of a final report.
	finalReviewed = "reviewed"
	finalFailed   = "failed"
)

var finalReviewActor = trace.Actor{Kind: "service", ID: "final-review"}

// finalReviewInput names one final review: its number and the inputs it was
// asked for, the feature branch commit and the seal, spec, plan and charter
// revisions that governed the workstream then.
type finalReviewInput struct {
	Maintenance bool `json:"maintenance,omitempty"`
	BaseCheck   int  `json:"base_check,omitempty"`
	// EdgeResolved is workgroupMergeOrderResolved's summary of the
	// workstream's merged incoming merge_order edges, read when the review
	// was asked for. It is "" for a workstream with no such edge, or
	// whose edges have not merged yet. A review whose EdgeResolved differs
	// from the current summary is stale: a workgroup dependency merged
	// since, and a fresh review and owner approval are required (spec#13).
	EdgeResolved string `json:"edge_resolved,omitempty"`
	// Move counts the workstream's moves to building or assembled when the
	// review was asked for. Each such move asks for a fresh review of the
	// branch, even when nothing else it reads changed.
	Move    int    `json:"move,omitempty"`
	Review  int    `json:"review"`
	Commit  string `json:"commit"`
	Seal    int    `json:"seal"`
	Spec    int    `json:"spec"`
	Plan    int    `json:"plan"`
	Charter int    `json:"charter"`
}

// FinalRebase is the document final/rebase.json: the rebase of the feature
// branch that final review Review reads. Before is the branch commit the
// review was asked for, Upstream the upstream commit it was rebased onto and
// Commit the rebased branch commit.
type FinalRebase struct {
	Review    int       `json:"review"`
	Operation string    `json:"operation"`
	Branch    string    `json:"branch"`
	Upstream  seal.Base `json:"upstream"`
	Before    string    `json:"before"`
	Commit    string    `json:"commit"`
}

// The severities of a final finding. A blocking finding stops delivery until
// work resolves it or the owner accepts it; an advisory one is shown to the
// owner and stops nothing.
const (
	FindingBlocking = "blocking"
	FindingAdvisory = "advisory"
)

// maxFinalRounds is how many final reviews of one workstream may find
// blocking problems before further revisions of its assembled plan are the
// owner's decision rather than the chief of staff's.
const maxFinalRounds = 3

// reportBlocking returns what stops the reviewed report from authorising delivery
// on its own: each criterion no merged unit serves and each blocking finding.
func reportBlocking(r FinalReport) []string {
	var out []string
	for _, c := range r.Criteria {
		if c.Gap != "" {
			out = append(out, c.Criterion+": "+c.Gap)
		}
	}
	for _, f := range r.Findings {
		if f.Severity == FindingBlocking {
			out = append(out, f.Describe())
		}
	}
	return out
}

// finalReportInput is what the final reader's tool call records.
type finalReportInput struct {
	Summary  string         `json:"summary,omitempty"`
	Findings []FinalFinding `json:"findings"`
}

func finalReviewIDs(k int) (transition, event string) {
	transition = fmt.Sprintf("%s-%d", finalReviewSubject, k)
	return transition, trace.EventID(transition, "run")
}

func finalTurnPrefix(k int, member string) string { return fmt.Sprintf("final-%d-%s-", k, member) }

func finalTurnID(k int, member string, attempt int) string {
	return fmt.Sprintf("%s%d", finalTurnPrefix(k, member), attempt)
}

// finalReviewer is the assembly controller. Its pass moves a building
// workstream whose every planned unit has merged to assembled, and asks for
// a final review of an assembled workstream once all its units have merged
// and its latest review does not read its current branch and governing
// documents; its reconciler runs each final
// review operation.
type finalReviewer struct {
	s          *Service
	repository *trace.Repository
}

var _ coreadapter.Reconciler = (*finalReviewer)(nil)

// Pass reconciles every workstream of the trace except the librarian's.
func (a *finalReviewer) Pass(ctx context.Context) error {
	streams, err := a.repository.Workstreams()
	if err != nil {
		return err
	}
	librarian := librarianWorkstream(a.repository.Project())
	for _, stream := range streams {
		if held, err := changeHeld(a.repository, stream); err != nil {
			return err
		} else if held {
			continue
		}
		if stream == librarian {
			continue
		}
		if session, err := isSession(a.repository, stream); err != nil {
			return fmt.Errorf("workstream %s final review: %w", stream, err)
		} else if session {
			continue
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		feature, err := a.repository.Workflow(stream, trace.FeatureSubject)
		if err != nil {
			return err
		}
		switch feature.Value {
		case BuildingState:
			err = a.assemble(ctx, stream)
		case AssembledState:
			err = a.request(ctx, stream)
		case DeliveredState:
			var pending bool
			pending, err = upstreamDeliveryPending(a.repository, a.s.about(a.repository), stream)
			if err == nil && pending {
				err = a.request(ctx, stream)
			}
		}
		if err != nil {
			return fmt.Errorf("workstream %s final review: %w", stream, err)
		}
	}
	return nil
}

// assemble moves a building workstream to assembled once every unit of its
// sealed plan has merged, and does nothing while any has not, while work
// remains unresolved, or while a move to building holds it for a revision.
func (a *finalReviewer) assemble(ctx context.Context, stream config.WorkstreamID) error {
	b, found, err := (&masons{s: a.s, cfg: a.s.about(a.repository), repository: a.repository}).readSealed(stream)
	if err != nil || !found {
		return err
	}
	if !allMerged(b) {
		return nil
	}
	if pending, err := unresolvedWork(a.repository, stream); err != nil || pending {
		return err
	}
	if held, err := reopenHeld(a.repository, stream); err != nil || held {
		return err
	}
	transitions, err := trace.Read[trace.Transition](a.repository, stream)
	if err != nil {
		return err
	}
	var ids []string
	subjects := map[string]bool{}
	for _, u := range b.plan.Units {
		ids = append(ids, u.ID)
		subjects[trace.UnitSubject(u.ID)] = true
	}
	// The last unit's move to merged caused the assembly.
	cause := BuildingState
	for _, t := range transitions {
		if t.To == UnitMerged && subjects[t.Subject] {
			cause = t.ID
		}
	}
	reason := fmt.Sprintf("every unit of the sealed plan has merged onto %s: %s; the feature branch is rebased onto upstream and read against the sealed spec and the charter next", featureBranch(stream), strings.Join(ids, ", "))
	id := AssembledState
	for _, t := range transitions {
		if t.ID == AssembledState {
			sealed, _, _, err := seal.Latest(a.repository, stream)
			if err != nil {
				return err
			}
			id = fmt.Sprintf("assembled-seal-%d", sealed.Seal)
			break
		}
	}
	h := trace.Header{Schema: "osmia.trace.transition", Version: trace.Version, ID: id, Revision: 1, Project: a.repository.Project(), Workstream: stream, At: a.s.now(), Actor: foremanActor, Cause: cause}
	_, err = a.repository.MoveFeatureState(ctx, h, BuildingState, AssembledState, reason)
	if errors.Is(err, trace.ErrConflict) {
		return nil
	}
	return err
}

// allMerged reports whether the building workstream's plan has units and
// every one of them has merged.
func allMerged(b building) bool {
	for _, u := range b.plan.Units {
		if b.states[trace.UnitSubject(u.ID)].Value != UnitMerged {
			return false
		}
	}
	return len(b.plan.Units) > 0
}

// errNoFeatureBranch is a workstream whose feature branch the clone lacks.
var errNoFeatureBranch = errors.New("the clone has no feature branch")

// governing returns the inputs a final review of the workstream would read
// now: the feature branch tip and the latest seal with its spec and plan
// revisions and the latest charter revision.
func (a *finalReviewer) governing(ctx context.Context, stream config.WorkstreamID) (finalReviewInput, seal.Seal, error) {
	latest, _, found, err := seal.Latest(a.repository, stream)
	if err != nil {
		return finalReviewInput{}, seal.Seal{}, err
	}
	if !found {
		return finalReviewInput{}, seal.Seal{}, fmt.Errorf("workstream %s has no seal", stream)
	}
	g, err := featureWorkspaces(a.s.about(a.repository), a.repository).of(stream)
	if err != nil {
		return finalReviewInput{}, seal.Seal{}, err
	}
	tip, exists, err := g.Branch(ctx, featureBranch(stream))
	if err != nil {
		return finalReviewInput{}, seal.Seal{}, err
	}
	if !exists {
		return finalReviewInput{}, seal.Seal{}, fmt.Errorf("%w %s", errNoFeatureBranch, featureBranch(stream))
	}
	charter, err := a.repository.Charter(ctx)
	if err != nil {
		return finalReviewInput{}, seal.Seal{}, err
	}
	_, baseCheck, err := baseObservationAt(a.repository, stream)
	if err != nil {
		return finalReviewInput{}, seal.Seal{}, err
	}
	feature, err := a.repository.Workflow(stream, trace.FeatureSubject)
	if err != nil {
		return finalReviewInput{}, seal.Seal{}, err
	}
	edgeResolved, err := workgroupMergeOrderResolved(a.repository, stream)
	if err != nil {
		return finalReviewInput{}, seal.Seal{}, err
	}
	moves, err := reviewMoves(a.repository, stream)
	if err != nil {
		return finalReviewInput{}, seal.Seal{}, err
	}
	return finalReviewInput{BaseCheck: baseCheck, Maintenance: feature.Value == DeliveredState, EdgeResolved: edgeResolved, Move: len(moves), Commit: tip, Seal: latest.Seal, Spec: latest.Revision.Spec, Plan: latest.Revision.Plan, Charter: charter.Revision}, latest, nil
}

// reads reports whether a review asked for with in, or recorded as report,
// reads the governing inputs now.
func (in finalReviewInput) reads(now finalReviewInput) bool {
	in.Review, now.Review = 0, 0
	return in == now
}

func reportInput(r FinalReport) finalReviewInput {
	return finalReviewInput{Maintenance: r.Maintenance, BaseCheck: r.BaseCheck, EdgeResolved: r.EdgeResolved, Move: r.Move, Commit: r.Commit, Seal: r.Seal, Spec: r.Spec, Plan: r.Plan, Charter: r.Charter}
}

// request asks for the next final review of an assembled workstream unless
// one has no result yet, the workstream is paused, the service has no
// committee runner, or a review was already asked for, or a report already
// reads, the current branch, seal, spec, plan and charter. A review that
// failed is asked for again only once one of them changes.
func (a *finalReviewer) request(ctx context.Context, stream config.WorkstreamID) error {
	observed, _, err := baseObservationAt(a.repository, stream)
	if err != nil || observed.Unavailable {
		return err
	}
	if a.s.options.Committee == nil {
		return nil
	}
	state, _ := a.s.effective()
	if scheduler.Paused(state.Pauses, a.repository.Project(), stream) {
		return nil
	}
	b, found, err := (&masons{s: a.s, cfg: a.s.about(a.repository), repository: a.repository}).readSealed(stream)
	if err != nil || !found {
		return err
	}
	if !allMerged(b) {
		return nil
	}
	if pending, err := unresolvedWork(a.repository, stream); err != nil || pending {
		return err
	}
	ops, err := a.repository.Operations(stream)
	if err != nil {
		return err
	}
	var asked []finalReviewInput
	for _, o := range ops {
		if o.Operation.Action == DriftAction && o.Result == nil {
			return nil
		}
		if o.Operation.Action != FinalReviewAction && o.Operation.Action != deliveryReviewAction {
			continue
		}
		if o.Result == nil {
			return nil
		}
		in, err := decodeFinalReview(o.Operation)
		if err != nil {
			return err
		}
		asked = append(asked, in)
	}
	now, _, err := a.governing(ctx, stream)
	if errors.Is(err, errNoFeatureBranch) {
		return nil
	}
	if err != nil {
		return err
	}
	report, found, err := latestFinalReport(a.repository, stream)
	if err != nil {
		return err
	}
	baseStale, err := finalReportBaseStale(a.repository, stream)
	if err != nil {
		return err
	}
	baseChanged, err := dependentBaseChanged(ctx, a.s.about(a.repository), a.repository, stream)
	if err != nil {
		return err
	}
	baseCurrent := found && !baseStale && !baseChanged
	if (!found || baseCurrent) && slices.ContainsFunc(asked, func(in finalReviewInput) bool { return in.reads(now) }) {
		return nil
	}
	if baseCurrent && report.Outcome == finalReviewed && reportInput(report).reads(now) {
		return nil
	}
	subject, err := a.repository.Workflow(stream, finalReviewSubject)
	if err != nil {
		return err
	}
	now.Review = len(asked) + 1
	transition, event := finalReviewIDs(now.Review)
	input, err := json.Marshal(now)
	if err != nil {
		return err
	}
	action := FinalReviewAction
	if now.Maintenance {
		action = deliveryReviewAction
	}
	op := coreadapter.Operation{ID: trace.OperationID(a.repository.Project(), stream, event), Boundary: coreadapter.RunnerBoundary, Action: action, Input: input}
	reason := fmt.Sprintf("the workstream is assembled; final review %d rebases %s, at %s, onto upstream and reads it against seal %d: spec revision %d, plan revision %d and charter revision %d", now.Review, featureBranch(stream), now.Commit, now.Seal, now.Spec, now.Plan, now.Charter)
	h := trace.Header{Schema: "osmia.trace.transition", Version: trace.Version, ID: transition, Revision: 1, Project: a.repository.Project(), Workstream: stream, At: a.s.now(), Actor: finalReviewActor, Cause: AssembledState}
	tx := trace.Transaction{ExpectedVersion: subject.Version,
		Transition: trace.Transition{Header: h, Subject: finalReviewSubject, From: subject.Value, To: fmt.Sprintf("requested-%d", now.Review), Reason: reason},
		Events:     []trace.Event{{ID: event, Kind: action, Body: fmt.Sprintf("Run final review %d", now.Review), Operation: &op}}}
	if _, err := a.repository.Transact(ctx, tx); err != nil && !errors.Is(err, trace.ErrConflict) {
		return err
	}
	return nil
}

func decodeFinalReview(op coreadapter.Operation) (finalReviewInput, error) {
	var in finalReviewInput
	if op.Boundary != coreadapter.RunnerBoundary || (op.Action != FinalReviewAction && op.Action != deliveryReviewAction) {
		return in, fmt.Errorf("unsupported runner operation %q", op.Action)
	}
	dec := json.NewDecoder(bytes.NewReader(op.Input))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&in); err != nil {
		return in, fmt.Errorf("invalid final review operation input: %w", err)
	}
	if in.Maintenance != (op.Action == deliveryReviewAction) {
		return in, errors.New("final review action differs from delivery maintenance intent")
	}
	if in.Review < 1 || in.Commit == "" || in.Seal < 1 || in.Spec < 1 || in.Plan < 1 || in.Charter < 1 {
		return in, errors.New("final review operation requires a positive review number, a commit and the seal, spec, plan and charter revisions")
	}
	return in, nil
}

// stream returns the workstream a final review operation belongs to: the one
// whose run event derives the operation ID.
func (a *finalReviewer) stream(op coreadapter.Operation, in finalReviewInput) (config.WorkstreamID, error) {
	streams, err := a.repository.Workstreams()
	if err != nil {
		return "", err
	}
	_, event := finalReviewIDs(in.Review)
	for _, stream := range streams {
		if trace.OperationID(a.repository.Project(), stream, event) == op.ID {
			return stream, nil
		}
	}
	return "", fmt.Errorf("final review operation %s belongs to no workstream", op.ID)
}

// outcome returns the recorded result of final review k: succeeded once its
// report is reviewed, failed once its failure is recorded, nil before either.
func (a *finalReviewer) outcome(stream config.WorkstreamID, k int) (*coreadapter.OperationResult, error) {
	transitions, err := trace.Read[trace.Transition](a.repository, stream)
	if err != nil {
		return nil, err
	}
	transition, _ := finalReviewIDs(k)
	for _, t := range transitions {
		switch t.ID {
		case transition + "-" + finalReviewed:
			return &coreadapter.OperationResult{Outcome: "succeeded", Evidence: t.Reason}, nil
		case transition + "-" + finalFailed:
			return &coreadapter.OperationResult{Outcome: "failed", Evidence: t.Reason}, nil
		}
	}
	return nil, nil
}

// Inspect reads the recorded transitions, the reader's thread and the clone:
// a recorded outcome completes the operation, a reader turn still running
// leaves it unknown, and otherwise it is absent, with evidence of whether
// the review's rebase is recorded and the feature branch has moved to it.
// Apply continues from a recorded rebase without replaying again.
func (a *finalReviewer) Inspect(ctx context.Context, op coreadapter.Operation) (coreadapter.Observation, error) {
	in, err := decodeFinalReview(op)
	if err != nil {
		return coreadapter.Observation{}, err
	}
	stream, err := a.stream(op, in)
	if err != nil {
		return coreadapter.Observation{}, err
	}
	result, err := a.outcome(stream, in.Review)
	if err != nil {
		return coreadapter.Observation{}, err
	}
	if result != nil {
		return coreadapter.Observation{State: coreadapter.EffectCompleted, Evidence: "final review " + result.Outcome, Result: result}, nil
	}
	t, err := a.repository.Thread(stream, committeeAgent(1))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return coreadapter.Observation{}, err
	}
	if err == nil {
		for _, q := range t.Turns {
			if strings.HasPrefix(q.Request.TurnID, finalTurnPrefix(in.Review, committeeAgent(1))) && q.Claim != nil && q.Response == nil && t.Status != "interrupted" {
				return coreadapter.Observation{State: coreadapter.EffectUnknown, Evidence: "final review turn " + q.Request.TurnID + " is running"}, nil
			}
		}
	}
	rebase, rebased, err := a.rebase(stream, in.Review)
	if err != nil {
		return coreadapter.Observation{}, err
	}
	if !rebased {
		return coreadapter.Observation{State: coreadapter.EffectAbsent, Evidence: fmt.Sprintf("final review %d is not recorded and has recorded no rebase", in.Review)}, nil
	}
	g, err := featureWorkspaces(a.s.about(a.repository), a.repository).of(stream)
	if err != nil {
		return coreadapter.Observation{}, err
	}
	tip, exists, err := g.Branch(ctx, rebase.Branch)
	if err != nil {
		return coreadapter.Observation{}, err
	}
	if exists && tip == rebase.Commit {
		return coreadapter.Observation{State: coreadapter.EffectAbsent, Evidence: fmt.Sprintf("final review %d is not recorded; feature branch %s is at %s, its recorded rebase of %s onto %s, and the review reads it without replaying again", in.Review, rebase.Branch, tip, rebase.Before, rebase.Upstream.Commit)}, nil
	}
	return coreadapter.Observation{State: coreadapter.EffectAbsent, Evidence: fmt.Sprintf("final review %d is not recorded; it recorded the rebase of %s onto %s as %s, and the branch moves to it without replaying again", in.Review, rebase.Before, rebase.Upstream.Commit, rebase.Commit)}, nil
}

// Apply runs final review k to a recorded report. The workstream must still
// be assembled and read the seal, spec, plan and charter the review was asked
// for, and its feature branch must still be at the commit it was asked for.
// The foreman then fetches the configured upstream base branch and replays
// the feature branch onto it; final/rebase.json records the result before
// the branch moves, so a retry moves it to the same commit. A replay that
// conflicts leaves the branch where it was. The service then runs the selected
// checks on the rebased commit, using the full suite on selection fallback,
// and records the run in final/checks-<k>.json, which a retry reads back.
// The first committee member then reads the rebased branch, the check run, the sealed spec and plan and
// the charter in a read-only view and records evidence or a gap for every
// sealed criterion.
// One commit records final/report.json with the review's move to reviewed or
// failed and a notice for the chief of staff. A review whose inputs changed,
// whose replay conflicted, or whose reader recorded no report fails with the
// reason in its report. Storage, fetch and Git errors, a missing committee
// runner and a pause covering the workstream leave the operation pending for
// another attempt; a reader's turn a hard pause stopped is continued within
// its attempt once the pause is lifted.
func (a *finalReviewer) Apply(ctx context.Context, op coreadapter.Operation) (coreadapter.OperationResult, error) {
	in, err := decodeFinalReview(op)
	if err != nil {
		return coreadapter.OperationResult{}, err
	}
	stream, err := a.stream(op, in)
	if err != nil {
		return coreadapter.OperationResult{}, err
	}
	if result, err := a.outcome(stream, in.Review); err != nil || result != nil {
		if err != nil {
			return coreadapter.OperationResult{}, err
		}
		return *result, nil
	}
	ctx = workspace.WithOperation(ctx, string(stream)+"/"+op.ID)
	cfg := a.s.about(a.repository)
	if !cfg.HasProject() || cfg.Project.ID != a.repository.Project() {
		return coreadapter.OperationResult{}, errors.New("the project is not active")
	}
	latest, _, found, err := seal.Latest(a.repository, stream)
	if err != nil {
		return coreadapter.OperationResult{}, err
	}
	if !found {
		return coreadapter.OperationResult{}, fmt.Errorf("workstream %s has no seal", stream)
	}
	report := FinalReport{Maintenance: in.Maintenance, BaseCheck: in.BaseCheck, EdgeResolved: in.EdgeResolved, Move: in.Move, Review: in.Review, Operation: op.ID, Outcome: finalFailed, Branch: featureBranch(stream), Before: in.Commit, Seal: in.Seal, SpecHash: latest.SpecHash, Spec: in.Spec, Plan: in.Plan, Charter: in.Charter, Criteria: []FinalCriterion{}}
	fail := func(reason string) (coreadapter.OperationResult, error) {
		report.Failure = reason
		return a.record(ctx, stream, report)
	}
	feature, err := a.repository.Workflow(stream, trace.FeatureSubject)
	if err != nil {
		return coreadapter.OperationResult{}, err
	}
	if feature.Value != AssembledState && !(feature.Value == DeliveredState && in.Maintenance) {
		return fail(fmt.Sprintf("the workstream is %s, not assembled", featureState(feature.Value)))
	}
	if in.Maintenance {
		pending, err := upstreamDeliveryPending(a.repository, cfg, stream)
		if err != nil {
			return coreadapter.OperationResult{}, err
		}
		if !pending {
			return fail("upstream delivery maintenance is not pending")
		}
	}
	now, _, err := a.governing(ctx, stream)
	if errors.Is(err, errNoFeatureBranch) {
		return fail(err.Error())
	}
	if err != nil {
		return coreadapter.OperationResult{}, err
	}
	rebase, rebased, err := a.rebase(stream, in.Review)
	if err != nil {
		return coreadapter.OperationResult{}, err
	}
	if now.Move != in.Move {
		return fail(fmt.Sprintf("the workstream was moved since the review was asked for; move %d asks for a fresh review", now.Move))
	}
	if now.BaseCheck != in.BaseCheck || now.EdgeResolved != in.EdgeResolved || now.Seal != in.Seal || now.Spec != in.Spec || now.Plan != in.Plan || now.Charter != in.Charter {
		return fail(fmt.Sprintf("the governing documents changed since the review was asked for: seal %d, spec revision %d, plan revision %d and charter revision %d are current", now.Seal, now.Spec, now.Plan, now.Charter))
	}
	g, err := featureWorkspaces(cfg, a.repository).of(stream)
	if err != nil {
		return coreadapter.OperationResult{}, err
	}
	if !rebased {
		if now.Commit != in.Commit {
			return fail(fmt.Sprintf("feature branch %s moved to %s since the review was asked for", report.Branch, now.Commit))
		}
		requested, err := (&foreman{masons: &masons{s: a.s, cfg: cfg, repository: a.repository}}).requestedAt(stream, op.ID)
		if err != nil {
			return coreadapter.OperationResult{}, err
		}
		upstream, err := a.s.resolveBase(ctx, cfg, a.repository, stream, g)
		if err != nil {
			return coreadapter.OperationResult{}, err
		}
		fetched := upstream.Commit
		if err := a.s.step("final-rebase-replaying"); err != nil {
			return coreadapter.OperationResult{}, err
		}
		currentBase, err := branchBase(ctx, a.repository, stream, g, in.Commit)
		if err != nil {
			return coreadapter.OperationResult{}, err
		}
		var conflicts []string
		err = workspace.LocalStep(ctx, string(stream)+"/"+op.ID+"/replay", requested, []workspace.Provider{g}, func(local context.Context) error {
			commit, paths, err := g.ReplayFrom(local, currentBase.Commit, in.Commit, fetched, requested)
			if err != nil {
				return err
			}
			conflicts = paths
			if len(conflicts) > 0 {
				return nil
			}
			rebase = FinalRebase{Review: in.Review, Operation: op.ID, Branch: report.Branch, Upstream: upstream, Before: in.Commit, Commit: commit}
			return a.recordRebase(local, stream, rebase)
		})
		if err != nil {
			return coreadapter.OperationResult{}, err
		}
		if len(conflicts) > 0 {
			report.Upstream, report.Conflicts = &upstream, conflicts
			return fail(fmt.Sprintf("feature branch %s does not rebase cleanly onto %s/%s at %s: %s conflicted; the branch is left at %s", report.Branch, upstream.Remote, upstream.Branch, fetched, strings.Join(conflicts, ", "), in.Commit))
		}
		if err := a.s.step("final-rebase-recorded"); err != nil {
			return coreadapter.OperationResult{}, err
		}
	}
	report.Upstream = &rebase.Upstream
	if now.Commit != rebase.Before && now.Commit != rebase.Commit {
		return fail(fmt.Sprintf("feature branch %s moved to %s during the review", report.Branch, now.Commit))
	}
	err = workspace.LocalStep(ctx, string(stream)+"/"+op.ID+"/move", a.s.now(), []workspace.Provider{g}, func(local context.Context) error {
		acquired, err := g.Acquire(local, vcs.Request{Name: string(stream), Branch: report.Branch})
		if err != nil {
			return fmt.Errorf("feature branch %s: %w", report.Branch, err)
		}
		return g.Move(local, acquired.(workspace.Worktree), rebase.Before, rebase.Commit)
	})
	if err != nil {
		return coreadapter.OperationResult{}, err
	}
	if err := a.s.step("final-rebase-moved"); err != nil {
		return coreadapter.OperationResult{}, err
	}
	report.Commit = rebase.Commit
	checks, err := a.finalChecks(ctx, cfg, stream, report)
	if err != nil {
		return coreadapter.OperationResult{}, err
	}
	return a.read(ctx, cfg, stream, in, report, checks)
}

// rebase returns the recorded rebase of final review k, and whether one is.
func (a *finalReviewer) rebase(stream config.WorkstreamID, k int) (FinalRebase, bool, error) {
	docs, err := trace.Read[trace.Document](a.repository, stream)
	if err != nil {
		return FinalRebase{}, false, err
	}
	for _, d := range slices.Backward(docs) {
		if d.ID != finalRebaseDocument {
			continue
		}
		var r FinalRebase
		if err := json.Unmarshal([]byte(d.Content), &r); err != nil {
			return FinalRebase{}, false, fmt.Errorf("%s revision %d: %w", d.Path, d.Revision, err)
		}
		if r.Review == k {
			return r, true, nil
		}
	}
	return FinalRebase{}, false, nil
}

// recordRebase records the next revision of final/rebase.json.
func (a *finalReviewer) recordRebase(ctx context.Context, stream config.WorkstreamID, rebase FinalRebase) error {
	content, err := json.MarshalIndent(rebase, "", "  ")
	if err != nil {
		return err
	}
	revision, err := nextRevision(a.repository, stream, finalRebaseDocument)
	if err != nil {
		return err
	}
	doc := trace.Document{Header: trace.Header{Schema: "osmia.trace.document", Version: trace.Version, ID: finalRebaseDocument, Revision: revision, Project: a.repository.Project(), Workstream: stream, At: a.s.now(), Actor: foremanActor, Cause: rebase.Operation},
		Path: finalRebasePath, Content: string(content) + "\n"}
	return a.repository.RecordDocuments(ctx, []trace.Document{doc})
}

// nextRevision returns the revision the next record of a workstream
// document takes.
func nextRevision(repository *trace.Repository, stream config.WorkstreamID, id string) (int, error) {
	docs, err := trace.Read[trace.Document](repository, stream)
	if err != nil {
		return 0, err
	}
	revision := 1
	for _, d := range docs {
		if d.ID == id && d.Revision >= revision {
			revision = d.Revision + 1
		}
	}
	return revision, nil
}

// read drives the first committee member's turn of the review to its end
// and records the report it left. It abandons a turn a previous service stop
// interrupted and starts a new turn, carrying checks, while attempts remain.
func (a *finalReviewer) read(ctx context.Context, cfg *config.Config, stream config.WorkstreamID, in finalReviewInput, report FinalReport, checks FinalCheckRun) (coreadapter.OperationResult, error) {
	if err := (&debate{s: a.s, repository: a.repository}).ensureCommittee(ctx, stream, 1); err != nil {
		return coreadapter.OperationResult{}, err
	}
	member := committeeAgent(1)
	report.Reader = member
	running, cancel := context.WithCancel(ctx)
	defer cancel()
	defer a.s.turns.add(stream, cancel)()
	for {
		if err := ctx.Err(); err != nil {
			return coreadapter.OperationResult{}, err
		}
		t, err := a.repository.Thread(stream, member)
		if err != nil {
			return coreadapter.OperationResult{}, err
		}
		// A turn a hard pause stopped is continued within its attempt, so
		// only the attempts' own turns count.
		var turns []trace.QueuedTurn
		tries := 0
		for _, q := range t.Turns {
			if strings.HasPrefix(q.Request.TurnID, finalTurnPrefix(in.Review, member)) {
				turns = append(turns, q)
				if !isContinuation(q.Request.TurnID) {
					tries++
				}
			}
		}
		var last, pending *trace.QueuedTurn
		if len(turns) > 0 {
			last = &turns[len(turns)-1]
		}
		if i := slices.IndexFunc(turns, func(q trace.QueuedTurn) bool { return q.CompletedAt.IsZero() }); i >= 0 {
			pending = &turns[i]
		}
		switch {
		case pending != nil && pending.Claim != nil && pending.Response == nil:
			if t.Status != "interrupted" {
				return coreadapter.OperationResult{}, errors.New("final review turn " + pending.Request.TurnID + " is still running")
			}
			if err := a.repository.AbandonTurn(ctx, stream, member, pending.Request.TurnID, a.s.now()); err != nil {
				return coreadapter.OperationResult{}, err
			}
		case pending != nil:
			gone, err := abandoned(a.repository, stream)
			if err != nil {
				return coreadapter.OperationResult{}, err
			}
			if gone && pending.Response == nil {
				if _, err := a.repository.CancelTurns(ctx, stream, a.s.now(), abandonActor, cancelReason); err != nil {
					return coreadapter.OperationResult{}, err
				}
				continue
			}
			turnCtx := running
			if pending.Response != nil {
				turnCtx = ctx
			} else if a.s.options.Committee == nil {
				return coreadapter.OperationResult{}, errNoCommittee
			} else if err := a.s.held(a.repository, stream); err != nil {
				return coreadapter.OperationResult{}, err
			}
			if _, err := a.dispatch(turnCtx, stream, in, report, member, pending.Request.TurnID); err != nil {
				return coreadapter.OperationResult{}, err
			}
			if err := os.RemoveAll(filepath.Join(a.turnDirectory(stream, pending.Request.TurnID), "workspace")); err != nil {
				return coreadapter.OperationResult{}, err
			}
		case last == nil || last.Status() == "interrupted":
			if gone, err := abandoned(a.repository, stream); err != nil || gone {
				if err != nil {
					return coreadapter.OperationResult{}, err
				}
				report.Failure = "the owner abandoned the workstream, so the committee member ran no turn"
				return a.record(ctx, stream, report)
			}
			if !stoppedTurn(last) && tries >= maxRoundAttempts {
				report.Failure = fmt.Sprintf("the committee member's turn was interrupted %d times by service stops", tries)
				return a.record(ctx, stream, report)
			}
			if a.s.options.Committee == nil {
				return coreadapter.OperationResult{}, errNoCommittee
			}
			if err := a.s.held(a.repository, stream); err != nil {
				return coreadapter.OperationResult{}, err
			}
			if stoppedTurn(last) {
				if _, err := a.s.continueStopped(ctx, a.repository, cfg, committeeRole, *last); err != nil {
					return coreadapter.OperationResult{}, err
				}
				continue
			}
			if err := a.enqueue(ctx, cfg, stream, in, report, checks, member, tries+1); err != nil {
				return coreadapter.OperationResult{}, err
			}
		default:
			report.Turn = last.Request.TurnID
			if last.Status() != "idle" {
				report.Failure = "the committee member's turn ended with status " + last.Status()
				if last.Response != nil && last.Response.Failure != "" {
					report.Failure += ": " + last.Response.Failure
				}
				return a.record(ctx, stream, report)
			}
			recorded, found, err := a.recorded(stream, continuedTurn(last.Request.TurnID))
			if err != nil {
				return coreadapter.OperationResult{}, err
			}
			if !found {
				report.Failure = "the committee member ended its turn without recording a report"
				return a.record(ctx, stream, report)
			}
			criteria, err := a.coverage(stream, report)
			if err != nil {
				return coreadapter.OperationResult{}, err
			}
			report.Outcome, report.Summary, report.Criteria, report.Findings = finalReviewed, recorded.Summary, criteria, recorded.Findings
			return a.record(ctx, stream, report)
		}
	}
}

// turnDirectory is the service-owned directory of one final review turn: its
// staged view, its backend session and the report its tool kept.
func (a *finalReviewer) turnDirectory(stream config.WorkstreamID, turn string) string {
	return filepath.Join(a.s.current().Root.String(), "final", string(a.repository.Project()), string(stream), turn)
}

func (a *finalReviewer) output(stream config.WorkstreamID, turn string) string {
	return filepath.Join(a.turnDirectory(stream, turn), "output", "report.json")
}

// recorded returns the report the turn's tool kept, and whether it kept one.
func (a *finalReviewer) recorded(stream config.WorkstreamID, turn string) (FinalReport, bool, error) {
	data, err := os.ReadFile(a.output(stream, turn))
	if errors.Is(err, fs.ErrNotExist) {
		return FinalReport{}, false, nil
	}
	if err != nil {
		return FinalReport{}, false, err
	}
	var report FinalReport
	if err := json.Unmarshal(data, &report); err != nil {
		return FinalReport{}, false, err
	}
	return report, true, nil
}

// enqueue accepts one attempt of the reader's turn, fixing its profile and
// prompts.
func (a *finalReviewer) enqueue(ctx context.Context, cfg *config.Config, stream config.WorkstreamID, in finalReviewInput, report FinalReport, checks FinalCheckRun, member string, attempt int) error {
	profile, err := a.s.agentProfile(cfg, committeeRole, member)
	if err != nil {
		return err
	}
	t, err := a.repository.Thread(stream, member)
	if err != nil {
		return err
	}
	notes, err := driftRebaseNotes(a.repository, stream)
	if err != nil {
		return err
	}
	if note, err := finalMoveNote(a.repository, stream, in.Move); err != nil {
		return err
	} else if note != "" {
		notes = append(notes, note)
	}
	coverage, err := a.coverage(stream, report)
	if err != nil {
		return err
	}
	previous, _, err := previousFinalReport(a.repository, stream, in.Review)
	if err != nil {
		return err
	}
	turn := finalTurnID(in.Review, member, attempt)
	transition, _ := finalReviewIDs(in.Review)
	req := trace.TurnRequest{Header: trace.Header{Schema: "osmia.trace.turn-request", Version: trace.Version, ID: "request_" + turn, Revision: 1, Project: a.repository.Project(), Workstream: stream, At: a.s.now(), Actor: finalReviewActor, Cause: transition, Depth: 1},
		AgentID: member, ThreadID: t.Identity.ThreadID, TurnID: turn, Profile: profile, SystemPrompt: finalSystemPrompt(cfg.Project), Prompt: finalPrompt(report, checks, notes, coverage, previous)}
	_, err = a.repository.EnqueueTurn(ctx, req)
	return err
}

// dispatch runs the turn through the thread dispatcher and runner, stopped by
// a hard pause covering the workstream.
func (a *finalReviewer) dispatch(ctx context.Context, stream config.WorkstreamID, in finalReviewInput, report FinalReport, member, turn string) (coreadapter.OperationResult, error) {
	ctx, release := a.s.stoppable(ctx, a.repository.Project(), stream)
	defer release()
	dispatcher := thread.Dispatcher{Runner: a.s.threadRunner(a.s.about(a.repository), a.repository, a.turns(stream, in, report), a.s.now), Prepare: func(_ context.Context, input thread.TurnInput) (coreadapter.PreparedTurn, error) {
		directory := filepath.Join(a.turnDirectory(input.Workstream, input.Turn), "session")
		return coreadapter.PreparedTurn{SessionDirectory: directory}, os.MkdirAll(directory, 0700)
	}}
	op, err := thread.TurnOperation(a.repository.Project(), "final-review-turn", thread.TurnInput{Workstream: stream, Agent: member, Turn: turn})
	if err != nil {
		return coreadapter.OperationResult{}, err
	}
	return dispatcher.Apply(ctx, op)
}

// turns is the final reader's isolated turn path: a read-only private view
// of the reviewed branch, the sealed spec and plan, the charter and the
// units' reports and landings, with reading, the branch's diff from upstream
// and reporting. It has no native write, shell, check or VCS capability.
func (a *finalReviewer) turns(stream config.WorkstreamID, in finalReviewInput, report FinalReport) *isolation.Turns {
	var engine coreadapter.Engine
	var hosts coreadapter.MCPHosts
	if c := a.s.options.Committee; c != nil {
		engine, hosts = c.Engine, c.Hosts
	}
	turns := &isolation.Turns{
		Workspaces: stagedWorkspaces{},
		Views:      isolation.Views{Directory: filepath.Join(a.s.current().Root.String(), "views")},
		Select: func(ctx context.Context, scope coreadapter.Scope) (isolation.Selection, error) {
			return a.selectView(ctx, scope, stream, report)
		},
		Grants: map[string]coreadapter.Capabilities{committeeRole: {Tools: []string{"file_read", FinalReportTool, reviewtools.DiffToolName}}},
		Scoped: func(_ context.Context, scope coreadapter.Scope) ([]coreadapter.Tool, error) {
			if scope.Role != committeeRole || scope.Workstream != string(stream) || scope.Project != string(a.repository.Project()) || !strings.HasPrefix(scope.Turn, finalTurnPrefix(in.Review, report.Reader)) {
				return nil, errors.New("turn scope denied")
			}
			// The turn that continues one a hard pause stopped replaces the
			// report that turn recorded.
			tool, err := a.tool(stream, continuedTurn(scope.Turn), report)
			if err != nil {
				return nil, err
			}
			cfg := a.s.about(a.repository)
			read := func(from, to string) func(ctx context.Context) (string, error) {
				return func(ctx context.Context) (string, error) {
					g, err := featureWorkspaces(cfg, a.repository).of(stream)
					if err != nil {
						return "", err
					}
					return g.Diff(ctx, from, to)
				}
			}
			branch := reviewtools.PinnedDiff{About: "the whole change the reviewed branch makes to the upstream commit it was rebased onto", From: report.Upstream.Commit, To: report.Commit, Read: read(report.Upstream.Commit, report.Commit)}
			previous, found, err := previousFinalReport(a.repository, stream, in.Review)
			if err != nil || !found {
				return []coreadapter.Tool{tool, reviewtools.DiffTool(branch)}, err
			}
			branch.Name = "branch"
			since := reviewtools.PinnedDiff{Name: "since", About: fmt.Sprintf("what changed since final review %d read %s, including any upstream change a rebase brought in", previous.Review, previous.Commit), From: previous.Commit, To: report.Commit, Read: read(previous.Commit, report.Commit)}
			return []coreadapter.Tool{tool, reviewtools.DiffTool(branch, since)}, nil
		},
		Hosts:  hosts,
		Engine: engine,
	}
	return memoryTurns(turns, a.s.about(a.repository), a.repository)
}

// sealedCriteria returns the criteria of the review's sealed spec revision,
// cited as spec#<n>, with their text.
func (a *finalReviewer) sealedCriteria(stream config.WorkstreamID, report FinalReport) ([]FinalCriterion, error) {
	spec, _, err := a.documents(stream, report)
	if err != nil {
		return nil, err
	}
	var criteria []FinalCriterion
	for _, c := range plan.ParseSpec(spec.Content).Criteria {
		criteria = append(criteria, FinalCriterion{Criterion: plan.Cite(c.Number), Text: strings.Join(strings.Fields(c.Text), " ")})
	}
	if len(criteria) == 0 {
		return nil, fmt.Errorf("spec.md revision %d holds no criteria", report.Spec)
	}
	return criteria, nil
}

// coverage returns every criterion of the review's sealed spec with the
// merged units that serve it, or a gap when none does.
func (a *finalReviewer) coverage(stream config.WorkstreamID, report FinalReport) ([]FinalCriterion, error) {
	criteria, err := a.sealedCriteria(stream, report)
	if err != nil {
		return nil, err
	}
	b, found, err := (&masons{s: a.s, cfg: a.s.about(a.repository), repository: a.repository}).readSealed(stream)
	if err != nil {
		return nil, err
	}
	for i, c := range criteria {
		var units []string
		if found {
			for _, u := range b.plan.Units {
				if slices.Contains(u.Criteria, c.Criterion) && b.states[trace.UnitSubject(u.ID)].Value == UnitMerged {
					units = append(units, u.ID)
				}
			}
		}
		if len(units) == 0 {
			criteria[i].Gap = "no merged unit serves it"
			continue
		}
		criteria[i].Evidence = "served by merged units " + strings.Join(units, ", ") + ", each reviewed against its acceptance"
	}
	return criteria, nil
}

// tool binds the report tool to the turn: a call whose findings each name a
// severity, say what is wrong and cite only criteria of the sealed spec is
// kept in the turn's output, replacing an earlier call's; any other is
// refused with the reason.
func (a *finalReviewer) tool(stream config.WorkstreamID, turn string, report FinalReport) (coreadapter.Tool, error) {
	sealed, err := a.sealedCriteria(stream, report)
	if err != nil {
		return coreadapter.Tool{}, err
	}
	file := a.output(stream, turn)
	tool := coreadapter.Tool{Name: FinalReportTool, Effect: coreadapter.ToolMemory,
		Description: "Record the final report on the whole branch: a summary and your findings. Each finding has a severity: blocking when the feature as a whole does not deliver the spec's intent, its units do not work together, or it breaks a charter rule; advisory for anything else worth the owner's attention. Give the criteria it concerns, cited as spec#<n>, what is wrong and the action it asks for. Record an empty list when there are no findings. A later call replaces an earlier one. End your turn once the report is recorded.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"summary":{"type":"string"},"findings":{"type":"array","items":{"type":"object","properties":{"severity":{"type":"string","enum":["blocking","advisory"]},"criteria":{"type":"array","items":{"type":"string"}},"finding":{"type":"string"},"action":{"type":"string"}},"required":["severity","finding"],"additionalProperties":false}}},"required":["summary","findings"],"additionalProperties":false}`)}
	tool.Handle = func(_ context.Context, raw json.RawMessage) (json.RawMessage, error) {
		var input finalReportInput
		if err := json.Unmarshal(raw, &input); err != nil {
			return nil, err
		}
		findings, reason := checkFindings(sealed, input.Findings)
		if reason != "" {
			return refuseReport("%s", reason)
		}
		data, err := json.Marshal(FinalReport{Summary: strings.TrimSpace(input.Summary), Findings: findings})
		if err != nil {
			return nil, err
		}
		if err := os.MkdirAll(filepath.Dir(file), 0700); err != nil {
			return nil, err
		}
		// A rename keeps the previous report whole if the write stops.
		if err := errors.Join(os.WriteFile(file+".tmp", data, 0600), os.Rename(file+".tmp", file)); err != nil {
			return nil, err
		}
		return json.Marshal(map[string]any{"recorded": true, "next": "End your turn now, or call again to replace the report."})
	}
	return tool, nil
}

// checkFindings returns the findings trimmed, or why one has no known
// severity, says nothing or cites what is not a criterion of the sealed spec.
func checkFindings(sealed []FinalCriterion, input []FinalFinding) ([]FinalFinding, string) {
	out := []FinalFinding{}
	for i, f := range input {
		f.Severity, f.Finding, f.Action = strings.TrimSpace(f.Severity), strings.TrimSpace(f.Finding), strings.TrimSpace(f.Action)
		if f.Severity != FindingBlocking && f.Severity != FindingAdvisory {
			return nil, fmt.Sprintf("finding %d has severity %q; use blocking or advisory", i+1, f.Severity)
		}
		if f.Finding == "" {
			return nil, fmt.Sprintf("finding %d says nothing; state what is wrong", i+1)
		}
		for _, c := range f.Criteria {
			if !slices.ContainsFunc(sealed, func(s FinalCriterion) bool { return s.Criterion == c }) {
				return nil, fmt.Sprintf("%q is not a criterion of the sealed spec; cite criteria as spec#<n>", c)
			}
		}
		out = append(out, f)
	}
	return out, ""
}

// documents returns the review's sealed spec and plan revisions.
func (a *finalReviewer) documents(stream config.WorkstreamID, report FinalReport) (spec, graph trace.Document, err error) {
	docs, err := trace.Read[trace.Document](a.repository, stream)
	if err != nil {
		return spec, graph, err
	}
	for _, doc := range docs {
		switch {
		case doc.ID == plan.SpecDocument && doc.Revision == report.Spec:
			spec = doc
		case doc.ID == plan.PlanDocument && doc.Revision == report.Plan:
			graph = doc
		}
	}
	if spec.Revision == 0 || graph.Revision == 0 {
		return spec, graph, fmt.Errorf("workstream %s does not record spec revision %d and plan revision %d", stream, report.Spec, report.Plan)
	}
	return spec, graph, nil
}

// selectView stages the reader's view for the claimed turn and selects all of
// it, read-only.
func (a *finalReviewer) selectView(ctx context.Context, scope coreadapter.Scope, stream config.WorkstreamID, report FinalReport) (isolation.Selection, error) {
	cfg := a.s.about(a.repository)
	if scope.Role != committeeRole || scope.Workstream != string(stream) || scope.Project != string(a.repository.Project()) || !cfg.HasProject() || cfg.Project.ID != a.repository.Project() {
		return isolation.Selection{}, errors.New("view selection denied")
	}
	_, settings, err := a.s.roleExecution(cfg, committeeRole)
	if err != nil {
		return isolation.Selection{}, err
	}
	if err := os.MkdirAll(filepath.Join(cfg.Root.String(), "views"), 0700); err != nil {
		return isolation.Selection{}, err
	}
	workspace := filepath.Join(a.turnDirectory(stream, scope.Turn), "workspace")
	paths, err := a.stage(ctx, cfg, stream, report, workspace)
	if err != nil {
		return isolation.Selection{}, err
	}
	return isolation.Selection{Workspace: coreadapter.WorkspaceRequest{SourceDirectory: workspace, Directory: workspace}, Paths: paths, Execution: settings}, nil
}

// stage builds the view: the reviewed commit's tracked files under branch/,
// the sealed spec.md and plan.json, the charter revision the review reads as
// charter.md, and each unit's latest report and landing under units/. It
// returns the paths to select.
func (a *finalReviewer) stage(ctx context.Context, cfg *config.Config, stream config.WorkstreamID, report FinalReport, workspace string) ([]string, error) {
	if err := os.RemoveAll(workspace); err != nil {
		return nil, err
	}
	g, err := featureWorkspaces(cfg, a.repository).of(stream)
	if err != nil {
		return nil, err
	}
	if err := g.Export(ctx, report.Commit, filepath.Join(workspace, "branch")); err != nil {
		return nil, err
	}
	spec, graph, err := a.documents(stream, report)
	if err != nil {
		return nil, err
	}
	project, err := a.repository.ProjectDocuments()
	if err != nil {
		return nil, err
	}
	i := slices.IndexFunc(project, func(d trace.Document) bool { return d.ID == "charter" && d.Revision == report.Charter })
	if i < 0 {
		return nil, fmt.Errorf("the project records no charter revision %d", report.Charter)
	}
	files := map[string]string{plan.SpecPath: spec.Content, plan.PlanPath: graph.Content, "charter.md": project[i].Content}
	paths := []string{"branch", plan.SpecPath, plan.PlanPath, "charter.md"}
	contextBundle, err := a.s.contextFor(a.repository).Assemble(ctx, cfg.Project.ID, bundle.Scope{Workstream: stream, Role: committeeRole})
	if err != nil {
		return nil, err
	}
	files["context.md"] = contextBundle.Render()
	paths = append(paths, "context.md")

	docs, err := trace.Read[trace.Document](a.repository, stream)
	if err != nil {
		return nil, err
	}
	for _, doc := range docs {
		if doc.Unit != "" && (doc.ID == reportDocument(doc.Unit) || doc.ID == landingDocument(doc.Unit)) {
			files[doc.Path] = doc.Content
			if !slices.Contains(paths, "units") {
				paths = append(paths, "units")
			}
		}
	}
	for name, content := range files {
		path := filepath.Join(workspace, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			return nil, err
		}
		if err := os.WriteFile(path, []byte(content), 0600); err != nil {
			return nil, err
		}
	}
	return paths, nil
}

// record commits the next revision of final/report.json with the review's
// move to reviewed or failed and a notice for the chief of staff, and
// returns the review's result. A review already recorded returns its result
// and records nothing more.
func (a *finalReviewer) record(ctx context.Context, stream config.WorkstreamID, report FinalReport) (coreadapter.OperationResult, error) {
	content, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		return coreadapter.OperationResult{}, err
	}
	revision, err := nextRevision(a.repository, stream, finalReportDocument)
	if err != nil {
		return coreadapter.OperationResult{}, err
	}
	at := a.s.now()
	doc := trace.Document{Header: trace.Header{Schema: "osmia.trace.document", Version: trace.Version, ID: finalReportDocument, Revision: revision, Project: a.repository.Project(), Workstream: stream, At: at, Actor: finalReviewActor, Cause: report.Operation},
		Path: finalReportPath, Content: string(content) + "\n"}
	transition, _ := finalReviewIDs(report.Review)
	id := transition + "-" + report.Outcome
	outcome := coreadapter.OperationResult{Outcome: "failed"}
	// Only a pending workgroup post-merge update permits maintenance
	// repairs on this branch. Historical edge resolutions do not grant
	// that permission to later same-repo base maintenance.
	edgePending := false
	if report.Maintenance {
		edgePending, err = workgroupEdgePending(a.repository, stream)
		if err != nil {
			return coreadapter.OperationResult{}, err
		}
	}
	var reason, body string
	if report.Outcome == finalReviewed {
		outcome.Outcome = "succeeded"
		blocking := reportBlocking(report)
		reason = fmt.Sprintf("final review %d read %s at %s against seal %d (spec revision %d, plan revision %d, charter revision %d): %d blocking and %d advisory; report %s revision %d",
			report.Review, report.Branch, report.Commit, report.Seal, report.Spec, report.Plan, report.Charter, len(blocking), len(report.Findings)-countSeverity(report.Findings, FindingBlocking), finalReportPath, revision)
		rounds, err := finalRoundsWithBlocking(a.repository, stream)
		if err != nil {
			return coreadapter.OperationResult{}, err
		}
		if len(blocking) > 0 && !report.Maintenance {
			rounds++
		}
		body = finalReviewNotice(report, blocking, rounds, edgePending)
	} else {
		reason = fmt.Sprintf("final review %d failed: %s; report %s revision %d", report.Review, report.Failure, finalReportPath, revision)
		body = fmt.Sprintf("The final review of the assembled branch failed: %s. The failed review authorises nothing; a new review runs once the branch or its governing documents change, or once the workstream is moved to assembled with %s.", report.Failure, moveWorkstreamTool)
	}
	outcome.Evidence = reason
	state, err := a.repository.Workflow(stream, finalReviewSubject)
	if err != nil {
		return coreadapter.OperationResult{}, err
	}
	h := trace.Header{Schema: "osmia.trace.transition", Version: trace.Version, ID: id, Revision: 1, Project: a.repository.Project(), Workstream: stream, At: at, Actor: finalReviewActor, Cause: report.Operation}
	tx := trace.Transaction{ExpectedVersion: state.Version,
		Transition: trace.Transition{Header: h, Subject: finalReviewSubject, From: state.Value, To: fmt.Sprintf("%s-%d", report.Outcome, report.Review), Reason: reason},
		Events:     []trace.Event{trace.Notice(id, "chief", body)}}
	documents := []trace.Document{doc}
	txs := []trace.Transaction{tx}
	if report.Outcome == finalReviewed && report.Maintenance && edgePending {
		added, found, err := a.followup(stream, report, revision)
		if err != nil {
			return coreadapter.OperationResult{}, err
		}
		if found {
			content, err := json.MarshalIndent([]followup.Unit{added}, "", "  ")
			if err != nil {
				return coreadapter.OperationResult{}, err
			}
			rev, err := nextRevision(a.repository, stream, followup.DocumentID)
			if err != nil {
				return coreadapter.OperationResult{}, err
			}
			documents = append(documents, trace.Document{Header: trace.Header{Schema: "osmia.trace.document", Version: trace.Version, ID: followup.DocumentID, Revision: rev, Project: a.repository.Project(), Workstream: stream, At: at, Actor: finalReviewActor, Cause: id}, Path: followup.Path, Content: string(content) + "\n"})
			subject := trace.UnitSubject(added.Unit.ID)
			planned := subject + "-" + UnitPlanned
			ready := subject + "-" + UnitReady
			makeHeader := func(key, cause string) trace.Header {
				return trace.Header{Schema: "osmia.trace.transition", Version: trace.Version, ID: key, Revision: 1, Project: a.repository.Project(), Workstream: stream, Unit: added.Unit.ID, At: at, Actor: finalReviewActor, Cause: cause}
			}
			txs = append(txs, trace.Transaction{Transition: trace.Transition{Header: makeHeader(planned, id), Subject: subject, To: UnitPlanned, Reason: fmt.Sprintf("final review %d of the post-merge dependency update found blocking problems: %s; report %s revision %d", report.Review, added.Gap, finalReportPath, revision)}})
			txs = append(txs, trace.Transaction{ExpectedVersion: 1, Transition: trace.Transition{Header: makeHeader(ready, planned), Subject: subject, From: UnitPlanned, To: UnitReady, Reason: fmt.Sprintf("follow-up %s is ready to repair the post-merge dependency update from final review %d", added.Unit.ID, report.Review)}})
		}
	}
	if _, err := a.repository.RecordDocumentsWith(ctx, documents, txs...); err != nil {
		if errors.Is(err, trace.ErrConflict) {
			if recorded, outcomeErr := a.outcome(stream, report.Review); outcomeErr == nil && recorded != nil {
				return *recorded, nil
			}
		}
		return coreadapter.OperationResult{}, err
	}
	return outcome, nil
}

func countSeverity(findings []FinalFinding, severity string) int {
	n := 0
	for _, f := range findings {
		if f.Severity == severity {
			n++
		}
	}
	return n
}

// finalReviewNotice is what the chief of staff hears of a reviewed final
// report: what blocks delivery, what is advisory, and what to do about it.
// rounds counts the workstream's final reviews with blocking problems,
// this one included, and edgePending says a delivered workstream's
// post-merge dependency update is pending.
func finalReviewNotice(report FinalReport, blocking []string, rounds int, edgePending bool) string {
	var b strings.Builder
	fmt.Fprintf(&b, "The final review of the assembled branch is recorded: %d blocking, %d advisory.", len(blocking), countSeverity(report.Findings, FindingAdvisory))
	for _, item := range blocking {
		b.WriteString("\nBlocking: " + item)
	}
	for _, f := range report.Findings {
		if f.Severity == FindingAdvisory {
			b.WriteString("\nAdvisory: " + f.Describe())
		}
	}
	switch {
	case len(blocking) == 0:
		b.WriteString("\nNothing blocks delivery; the owner decides it with the advisory findings in view.")
	case report.Maintenance && edgePending:
		b.WriteString("\nOne follow-up unit repairs the post-merge dependency update; the final review runs again once it lands.")
	case report.Maintenance:
		fmt.Fprintf(&b, "\nThe workstream is delivered with its pull request open. Triage them: to repair a finding that needs code, move the workstream to building with %s and then file one plan revision, through route_amendment, that groups related findings into as few units as fit; the open pull request is updated only after a fresh final review and the owner's delivery approval. A finding that is mistaken or not worth building is the owner's to accept at delivery, so tell the owner why.", moveWorkstreamTool)
	case rounds >= maxFinalRounds:
		fmt.Fprintf(&b, "\nFinal reviews have found blocking problems %d times. Further revisions of the assembled plan are the owner's decision: present these findings to the owner, who can accept them at delivery or approve the work that resolves them.", rounds)
	default:
		b.WriteString("\nTriage them: a finding that needs code is resolved by one plan revision, through route_amendment, that groups related findings into as few units as fit; a finding that is mistaken or not worth building is the owner's to accept at delivery, so tell the owner why.")
	}
	return b.String()
}

// followup returns the one unit that repairs the blocking problems of a
// reviewed post-merge dependency update, scoped to the sealed footprints of
// the units serving the criteria they concern, or of every unit when they
// name none, and whether there are any.
func (a *finalReviewer) followup(stream config.WorkstreamID, report FinalReport, revision int) (followup.Unit, bool, error) {
	blocking := reportBlocking(report)
	if len(blocking) == 0 {
		return followup.Unit{}, false, nil
	}
	_, graph, err := a.documents(stream, report)
	if err != nil {
		return followup.Unit{}, false, err
	}
	p, err := plan.Parse([]byte(graph.Content))
	if err != nil {
		return followup.Unit{}, false, err
	}
	var criteria []string
	for _, c := range report.Criteria {
		if c.Gap != "" {
			criteria = append(criteria, c.Criterion)
		}
	}
	for _, f := range report.Findings {
		if f.Severity != FindingBlocking {
			continue
		}
		for _, c := range f.Criteria {
			if !slices.Contains(criteria, c) {
				criteria = append(criteria, c)
			}
		}
	}
	var footprint []string
	for _, u := range p.Units {
		if len(criteria) > 0 && !slices.ContainsFunc(u.Criteria, func(c string) bool { return slices.Contains(criteria, c) }) {
			continue
		}
		for _, name := range u.Footprint {
			if !slices.Contains(footprint, name) {
				footprint = append(footprint, name)
			}
		}
	}
	if len(footprint) == 0 {
		return followup.Unit{}, false, fmt.Errorf("final review %d names no sealed footprint to repair; an owner-approved plan amendment is required", report.Review)
	}
	known := map[string]bool{}
	for _, u := range p.Units {
		known[u.ID] = true
	}
	previous, err := followup.Read(a.repository, stream)
	if err != nil {
		return followup.Unit{}, false, err
	}
	for _, u := range previous {
		known[u.Unit.ID] = true
	}
	base := fmt.Sprintf("final-%d", report.Review)
	id := base
	for n := 2; known[id]; n++ {
		id = fmt.Sprintf("%s-%d", base, n)
	}
	var acceptance []string
	for _, item := range blocking {
		acceptance = append(acceptance, "Resolved: "+item)
	}
	gap := strings.Join(blocking, "; ")
	first := ""
	if len(criteria) > 0 {
		first = criteria[0]
	}
	return followup.Unit{Review: report.Review, Report: revision, Criterion: first, Gap: gap, Unit: plan.Unit{ID: id, Title: "Repair the post-merge dependency update", Task: "Resolve what final review " + strconv.Itoa(report.Review) + " found blocking after the dependency update: " + gap, Acceptance: acceptance, Criteria: criteria, DependsOn: []string{}, Footprint: footprint}}, true, nil
}

// finalMoveNote is what the reader of a final review asked for by the
// workstream's move n to building or assembled is told of it, or "" when the
// review answers no move.
func finalMoveNote(repository *trace.Repository, stream config.WorkstreamID, n int) (string, error) {
	if n == 0 {
		return "", nil
	}
	moves, err := reviewMoves(repository, stream)
	if err != nil || len(moves) < n {
		return "", err
	}
	m := moves[n-1]
	if m.From == m.To {
		return fmt.Sprintf("The %s asked for this review with this note: %s", m.By, m.Note), nil
	}
	return fmt.Sprintf("The %s moved the workstream from %s to %s, which asked for this review, with this note: %s", m.By, m.From, m.To, m.Note), nil
}

// previousFinalReport returns the latest reviewed report of a final review
// before review k, and whether one is recorded.
func previousFinalReport(repository *trace.Repository, stream config.WorkstreamID, k int) (FinalReport, bool, error) {
	reports, err := finalReports(repository, stream)
	if err != nil {
		return FinalReport{}, false, err
	}
	for _, r := range slices.Backward(reports) {
		if r.Review < k && r.Outcome == finalReviewed && r.Commit != "" {
			return r, true, nil
		}
	}
	return FinalReport{}, false, nil
}

// finalRoundsWithBlocking counts the workstream's recorded final reviews of
// its assembled feature, outside delivery maintenance, that found something
// blocking.
func finalRoundsWithBlocking(repository *trace.Repository, stream config.WorkstreamID) (int, error) {
	reports, err := finalReports(repository, stream)
	if err != nil {
		return 0, err
	}
	n := 0
	for _, r := range reports {
		if r.Outcome == finalReviewed && !r.Maintenance && len(reportBlocking(r)) > 0 {
			n++
		}
	}
	return n, nil
}

// finalReports returns every recorded final report in the order recorded.
func finalReports(repository *trace.Repository, stream config.WorkstreamID) ([]FinalReport, error) {
	docs, err := trace.Read[trace.Document](repository, stream)
	if err != nil {
		return nil, err
	}
	var out []FinalReport
	for _, d := range docs {
		if d.ID != finalReportDocument {
			continue
		}
		var report FinalReport
		if err := json.Unmarshal([]byte(d.Content), &report); err != nil {
			return nil, fmt.Errorf("%s revision %d: %w", d.Path, d.Revision, err)
		}
		out = append(out, report)
	}
	return out, nil
}

// latestFinalReport returns the workstream's latest final report, and
// whether one is recorded.
func latestFinalReport(repository *trace.Repository, stream config.WorkstreamID) (FinalReport, bool, error) {
	docs, err := trace.Read[trace.Document](repository, stream)
	if err != nil {
		return FinalReport{}, false, err
	}
	for _, d := range slices.Backward(docs) {
		if d.ID != finalReportDocument {
			continue
		}
		var report FinalReport
		if err := json.Unmarshal([]byte(d.Content), &report); err != nil {
			return FinalReport{}, false, fmt.Errorf("%s revision %d: %w", d.Path, d.Revision, err)
		}
		return report, true, nil
	}
	return FinalReport{}, false, nil
}

// finalReportBaseStale reports whether the recorded seal's upstream base
// moved after the latest final report was written.
func finalReportBaseStale(repository *trace.Repository, stream config.WorkstreamID) (bool, error) {
	docs, err := trace.Read[trace.Document](repository, stream)
	if err != nil {
		return false, err
	}
	reportAt := -1
	for i, doc := range docs {
		if doc.ID == finalReportDocument {
			reportAt = i
		}
	}
	if reportAt < 0 {
		return false, nil
	}
	var before, after seal.Base
	for i, doc := range docs {
		if doc.ID != seal.DocumentID {
			continue
		}
		recorded, err := seal.Parse([]byte(doc.Content))
		if err != nil {
			return false, err
		}
		if i < reportAt {
			before = recorded.Base
		} else {
			after = recorded.Base
		}
	}
	return after != (seal.Base{}) && after != before, nil
}

// finalGate returns the workstream's latest final report and why it cannot
// be presented for approval or delivery: none is recorded, the latest
// failed, it no longer reads the workstream's feature branch tip, latest seal
// and its spec hash, latest spec and plan revisions and latest charter
// revision, or follow-up units have not landed. The reason is empty when it
// can. What the report itself finds blocking is the owner's to accept at
// approval; see FinalReport.blocking.
func (a *finalReviewer) finalGate(ctx context.Context, stream config.WorkstreamID) (FinalReport, string, error) {
	report, found, err := latestFinalReport(a.repository, stream)
	if err != nil || !found {
		return report, "no final review is recorded", err
	}
	if report.Outcome != finalReviewed {
		return report, fmt.Sprintf("final review %d failed: %s", report.Review, report.Failure), nil
	}
	now, latest, err := a.governing(ctx, stream)
	if errors.Is(err, errNoFeatureBranch) {
		return report, err.Error(), nil
	}
	if err != nil {
		return report, "", err
	}
	dependency, err := a.repository.WorkstreamBase(stream)
	if err != nil {
		return report, "", err
	}
	if dependency.Base != "" {
		cfg := a.s.about(a.repository)
		g, err := featureWorkspaces(cfg, a.repository).of(stream)
		if err != nil {
			return report, "", err
		}
		selected, err := a.s.resolveBase(ctx, cfg, a.repository, stream, g)
		if err != nil {
			return report, "base workstream is unavailable; publication waits", nil
		}
		if selected.Workstream != "" {
			parent, err := a.repository.Workflow(selected.Workstream, trace.FeatureSubject)
			if err != nil {
				return report, "", err
			}
			if parent.Value != DeliveredState {
				return report, "the base workstream must be delivered to the push repository before its dependent pull request can open", nil
			}
		}
		if report.Upstream == nil || *report.Upstream != selected {
			return report, "the base workstream moved or integrated upstream; a new final review and owner approval are required", nil
		}
	}
	docs, err := trace.Read[trace.Document](a.repository, stream)
	if err != nil {
		return report, "", err
	}
	revisions := map[string]int{}
	for _, d := range docs {
		revisions[d.ID] = max(revisions[d.ID], d.Revision)
	}
	for _, field := range []struct {
		name     string
		reviewed any
		current  any
	}{
		{"feature branch commit", report.Commit, now.Commit},
		{"dependency observation", report.BaseCheck, now.BaseCheck},
		{"delivery maintenance", report.Maintenance, now.Maintenance},
		{"workgroup edge resolution", report.EdgeResolved, now.EdgeResolved},
		{"workstream move", report.Move, now.Move},
		{"seal", report.Seal, now.Seal},
		{"spec hash", report.SpecHash, latest.SpecHash},
		{"spec revision", report.Spec, revisions[plan.SpecDocument]},
		{"plan revision", report.Plan, revisions[plan.PlanDocument]},
		{"charter revision", report.Charter, now.Charter},
	} {
		if field.reviewed != field.current {
			return report, fmt.Sprintf("final review %d is stale: it read %s %v, and %v is current; a new final review is required", report.Review, field.name, field.reviewed, field.current), nil
		}
	}
	if stale, err := finalReportBaseStale(a.repository, stream); err != nil {
		return report, "", err
	} else if stale {
		return report, fmt.Sprintf("final review %d is stale: its upstream base changed; a new final review is required", report.Review), nil
	}
	b, found, err := (&masons{s: a.s, cfg: a.s.about(a.repository), repository: a.repository}).readSealed(stream)
	if err != nil {
		return report, "", err
	}
	if !found || !allMerged(b) {
		return report, "final-review follow-up units have not all landed", nil
	}
	return report, "", nil
}

func finalSystemPrompt(p config.Project) string {
	return fmt.Sprintf("You are the committee member who gives an assembled feature its final read for the %s project (%s). Every planned unit has landed on the feature branch after its own reviewer verified it in detail against its task and acceptance, and the service ran checks on the exact commit you read, with their selection and result in your evidence. You read the branch as a whole, at a high level, before the owner decides whether it is delivered. You judge whether the feature delivers the sealed spec's intent: its units work together in the running product rather than only in their own tests, no unit undoes or contradicts another, every criterion is served, and no charter rule is broken. You do not review units again: their tests, edge cases, naming and internal structure were judged in their own reviews, and style, formatting and lint belong to the checks. Your view is read-only. You record your report with %s: a summary and your findings. A finding is blocking only when the feature as a whole fails the spec's intent or breaks a charter rule; anything else worth the owner's attention is advisory. The branch was rebased onto upstream: changes a rebase or a drift resolution needed so that both upstream's behaviour and the feature's stand are part of the feature branch whatever the sealed spec, plan or unit footprints name, and upstream's own changes are not the feature's. Neither is a finding on its own; judge them only where they break the feature or fail a criterion. Report nothing you cannot point at in the branch.", p.Name, p.Upstream, FinalReportTool) + checkGateReviewInstructions
}

func finalPrompt(report FinalReport, checks FinalCheckRun, driftNotes []string, coverage []FinalCriterion, previous FinalReport) string {
	notes := ""
	if len(driftNotes) > 0 {
		notes = "\n" + strings.Join(driftNotes, "\n") + "\n"
	}
	var served strings.Builder
	for _, c := range coverage {
		if c.Gap != "" {
			fmt.Fprintf(&served, "- %s: %s\n", c.Criterion, c.Gap)
		} else {
			fmt.Fprintf(&served, "- %s: %s\n", c.Criterion, c.Evidence)
		}
	}
	diff := fmt.Sprintf("Call %s to read the whole change the branch makes to upstream: files_only lists the changed files, and paths and lines narrow the diff.", reviewtools.DiffToolName)
	earlier := ""
	if previous.Review > 0 {
		diff = fmt.Sprintf("Call %s with change branch for the whole change the branch makes to upstream, or change since for what changed after final review %d: files_only lists the changed files, and paths and lines narrow the diff. The change since includes whatever upstream changed in between, which a rebase brought in: that is upstream's change, not the feature's, so judge it only where it breaks the feature.", reviewtools.DiffToolName, previous.Review)
		var b strings.Builder
		fmt.Fprintf(&b, "\nFinal review %d read %s and found:\n", previous.Review, previous.Commit)
		blocking := reportBlocking(previous)
		for _, item := range blocking {
			b.WriteString("- blocking: " + item + "\n")
		}
		for _, f := range previous.Findings {
			if f.Severity == FindingAdvisory {
				b.WriteString("- advisory: " + f.Describe() + "\n")
			}
		}
		if len(blocking) == 0 && len(previous.Findings) == 0 {
			b.WriteString("- nothing\n")
		}
		fmt.Fprintf(&b, "Start from what changed since then. Say in your summary whether each of its blocking findings is resolved. Raise a new blocking finding only where a change since review %d breaks the feature, or where the feature plainly fails a criterion; do not reopen what review %d accepted, and do not repeat its advisory findings.\n", previous.Review, previous.Review)
		earlier = b.String()
	}
	return fmt.Sprintf(`Final review %d of the assembled feature branch at commit %s, rebased onto %s/%s at %s. The spec is revision %d, the plan revision %d, sealed as seal %d; the charter is revision %d.

Your view holds:
- branch/: every tracked file of the reviewed commit.
- spec.md and plan.json: the sealed spec and plan. Cite a criterion as spec#<n>.
- charter.md: the owner's rules for contributing to this project.
- context.md: local decisions, knowledge and notices, plus optional scoped external memory.
- units/<unit>/report.json and landing.json: each unit's last report and how it landed.
%s
%s

Every criterion and the merged units that serve it:
%s%s
Read the branch as a whole, not unit by unit: follow how the units' work connects, from where the feature is entered to what it changes, and check it against the spec's intent and the charter. Then call %s once with your summary and findings, an empty list when there are none, and end your turn.

%s
`, report.Review, report.Commit, report.Upstream.Remote, report.Upstream.Branch, report.Upstream.Commit, report.Spec, report.Plan, report.Seal, report.Charter, notes, diff, served.String(), earlier, FinalReportTool, finalCheckEvidence(checks))
}
