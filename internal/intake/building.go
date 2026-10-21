package intake

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/plan"
	"github.com/kpenfound/osmia/internal/seal"
	"github.com/kpenfound/osmia/internal/trace"
)

// BuildAction is the repository-boundary operation action that starts
// building a sealed workstream: it records the state of every unit of the
// sealed plan and moves the workstream from ratified to building.
const BuildAction = "build"

const (
	// BuildingState is the feature state of a workstream whose units are
	// built.
	BuildingState = "building"
	// buildSubject is the workflow subject that tracks the requests to start
	// building a workstream: requested-<k> once building on seal k is asked
	// for. A build that succeeds is recorded by the feature state it moves.
	BuildSubject = "build"
)

// The states of a plan unit.
const (
	// UnitPlanned is a unit of the plan with a dependency not yet merged.
	UnitPlanned = "planned"
	// UnitReady is a unit whose dependencies have all merged.
	UnitReady = "ready"
	// UnitImplementing is a unit a mason works on.
	UnitImplementing = "implementing"
	// UnitChecking is a unit whose candidate the service runs the project's
	// checks on before review.
	UnitChecking = "checking"
	// UnitReviewing is a unit whose candidate is under review.
	UnitReviewing = "reviewing"
	// UnitApproved is a unit its reviewer approved, waiting to land.
	UnitApproved = "approved"
	// UnitMerged is a unit squashed onto the feature branch.
	UnitMerged = "merged"
	// UnitWaiting is a unit whose role asked a question.
	UnitWaiting = "waiting"
	// UnitContested is a unit awaiting an owner ruling after review or check
	// bounces or a mason clean-turn contest.
	UnitContested = "contested"
)

var BuildingActor = trace.Actor{Kind: "service", ID: "building"}

// buildInput names the seal a build starts from.
type BuildInput struct {
	Seal int `json:"seal"`
}

func BuildIDs(k int) (transition, event string) {
	transition = fmt.Sprintf("build-%d", k)
	return transition, trace.EventID(transition, "run")
}

// builder is the building controller. Its pass asks to start building every
// ratified workstream whose latest seal no build was asked for, and its
// reconciler runs each build operation.
type Builder struct {
	S          *BuilderDependencies
	Repository *trace.Repository
}

var _ coreadapter.Reconciler = (*Builder)(nil)

// Pass reconciles every workstream of the trace except the librarian's.
func (b *Builder) Pass(ctx context.Context) error {
	streams, err := b.Repository.Workstreams()
	if err != nil {
		return err
	}
	librarian := LibrarianWorkstream(b.Repository.Project())
	for _, stream := range streams {
		if stream == librarian {
			continue
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := b.Reconcile(ctx, stream); err != nil {
			return fmt.Errorf("workstream %s building: %w", stream, err)
		}
	}
	return nil
}

// reconcile asks to start building a ratified workstream on its latest seal
// when no build on that seal was asked for.
func (b *Builder) Reconcile(ctx context.Context, stream config.WorkstreamID) error {
	if held, err := b.S.ChangeHeld(b.Repository, stream); err != nil || held {
		return err
	}
	feature, err := b.Repository.Workflow(stream, trace.FeatureSubject)
	if err != nil || feature.Value != RatifiedState {
		return err
	}
	latest, doc, found, err := seal.Latest(b.Repository, stream)
	if err != nil || !found {
		return err
	}
	ops, err := b.Repository.Operations(stream)
	if err != nil {
		return err
	}
	for _, o := range ops {
		if o.Operation.Action != BuildAction {
			continue
		}
		in, err := DecodeBuild(o.Operation)
		if err != nil {
			return err
		}
		if in.Seal == latest.Seal {
			return nil
		}
	}
	state, err := b.Repository.Workflow(stream, BuildSubject)
	if err != nil {
		return err
	}
	transition, event := BuildIDs(latest.Seal)
	input, err := json.Marshal(BuildInput{Seal: latest.Seal})
	if err != nil {
		return err
	}
	op := coreadapter.Operation{ID: trace.OperationID(b.Repository.Project(), stream, event), Boundary: coreadapter.RepositoryBoundary, Action: BuildAction, Input: input}
	reason := fmt.Sprintf("seal %d is recorded; the build records the state of every unit of the sealed plan and moves the workstream to building", latest.Seal)
	tx := trace.Transaction{ExpectedVersion: state.Version,
		Transition: trace.Transition{Header: b.Header(transition, stream, fmt.Sprintf("%s-%d", seal.DocumentID, doc.Revision), b.S.Now()), Subject: BuildSubject, From: state.Value, To: fmt.Sprintf("requested-%d", latest.Seal), Reason: reason},
		Events:     []trace.Event{{ID: event, Kind: BuildAction, Body: fmt.Sprintf("Start building on seal %d", latest.Seal), Operation: &op}}}
	_, err = b.Repository.Transact(ctx, tx)
	return err
}

func (b *Builder) Header(id string, stream config.WorkstreamID, cause string, at time.Time) trace.Header {
	return trace.Header{Schema: "osmia.trace.transition", Version: trace.Version, ID: id, Revision: 1, Project: b.Repository.Project(), Workstream: stream, At: at, Actor: BuildingActor, Cause: cause}
}

func DecodeBuild(op coreadapter.Operation) (BuildInput, error) {
	var in BuildInput
	if op.Boundary != coreadapter.RepositoryBoundary || op.Action != BuildAction {
		return in, fmt.Errorf("unsupported repository operation %q", op.Action)
	}
	dec := json.NewDecoder(bytes.NewReader(op.Input))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&in); err != nil {
		return in, fmt.Errorf("invalid build operation input: %w", err)
	}
	if in.Seal < 1 {
		return in, errors.New("build operation requires a positive seal number")
	}
	return in, nil
}

// stream returns the workstream a build operation belongs to: the one whose
// run event derives the operation ID.
func (b *Builder) Stream(op coreadapter.Operation, k int) (config.WorkstreamID, error) {
	streams, err := b.Repository.Workstreams()
	if err != nil {
		return "", err
	}
	_, event := BuildIDs(k)
	for _, stream := range streams {
		if trace.OperationID(b.Repository.Project(), stream, event) == op.ID {
			return stream, nil
		}
	}
	return "", fmt.Errorf("build operation %s belongs to no workstream", op.ID)
}

// outcome returns the result of a build that moved the workstream to
// building, nil before it did.
func (b *Builder) Outcome(stream config.WorkstreamID, operation string) (*coreadapter.OperationResult, error) {
	transitions, err := trace.Read[trace.Transition](b.Repository, stream)
	if err != nil {
		return nil, err
	}
	for _, t := range transitions {
		if t.Subject == trace.FeatureSubject && t.To == BuildingState && t.Cause == operation {
			return &coreadapter.OperationResult{Outcome: "succeeded", Evidence: t.Reason}, nil
		}
	}
	return nil, nil
}

// Inspect reads the recorded transitions: a move to building by this
// operation completes it; otherwise it is absent.
func (b *Builder) Inspect(ctx context.Context, op coreadapter.Operation) (coreadapter.Observation, error) {
	in, err := DecodeBuild(op)
	if err != nil {
		return coreadapter.Observation{}, err
	}
	stream, err := b.Stream(op, in.Seal)
	if err != nil {
		return coreadapter.Observation{}, err
	}
	result, err := b.Outcome(stream, op.ID)
	if err != nil {
		return coreadapter.Observation{}, err
	}
	if result != nil {
		return coreadapter.Observation{State: coreadapter.EffectCompleted, Evidence: "the workstream moved to building", Result: result}, nil
	}
	return coreadapter.Observation{State: coreadapter.EffectAbsent, Evidence: "the workstream has not moved to building"}, nil
}

// Apply starts building: in one commit it records every unit of the sealed
// plan as planned, moves each unit that depends on no unit to ready, and
// moves the workstream from ratified to building. A build that
// moved the workstream already returns its result and records nothing more.
// A workstream no longer ratified, a seal a later one replaced, a sealed plan
// that does not parse and a workflow that changed under the build fail it
// with the reason as its result, recording nothing.
func (b *Builder) Apply(ctx context.Context, op coreadapter.Operation) (coreadapter.OperationResult, error) {
	in, err := DecodeBuild(op)
	if err != nil {
		return coreadapter.OperationResult{}, err
	}
	stream, err := b.Stream(op, in.Seal)
	if err != nil {
		return coreadapter.OperationResult{}, err
	}
	fail := func(reason string) (coreadapter.OperationResult, error) {
		return coreadapter.OperationResult{Outcome: "failed", Evidence: fmt.Sprintf("building on seal %d failed: %s", in.Seal, reason)}, nil
	}
	if result, err := b.Outcome(stream, op.ID); err != nil || result != nil {
		if err != nil {
			return coreadapter.OperationResult{}, err
		}
		return *result, nil
	}
	if held, err := b.S.ChangeRepositoryHeld(b.Repository, stream, op); err != nil || held {
		if err != nil {
			return coreadapter.OperationResult{}, err
		}
		return coreadapter.OperationResult{}, ErrChangeBoundary
	}
	feature, err := b.Repository.Workflow(stream, trace.FeatureSubject)
	if err != nil {
		return coreadapter.OperationResult{}, err
	}
	if feature.Value != RatifiedState {
		return fail(fmt.Sprintf("the workstream is %s, not ratified", FeatureState(feature.Value)))
	}
	latest, _, found, err := seal.Latest(b.Repository, stream)
	if err != nil {
		return coreadapter.OperationResult{}, err
	}
	if !found || latest.Seal != in.Seal {
		return fail("it is not the latest seal of the workstream")
	}
	graph, err := SealedPlan(b.Repository, stream, latest.Revision.Plan)
	if err != nil {
		return coreadapter.OperationResult{}, err
	}
	p, err := plan.Parse([]byte(graph.Content))
	if err != nil {
		return fail("the sealed plan does not parse: " + err.Error())
	}
	at := b.S.Now()
	var txs []trace.Transaction
	var ready, planned []string
	for _, u := range p.Units {
		// No unit has merged before the build, so a unit is ready exactly
		// when it depends on none.
		subject := trace.UnitSubject(u.ID)
		reason := fmt.Sprintf("unit %s is in the plan of seal %d", u.ID, in.Seal)
		if len(u.DependsOn) > 0 {
			reason += fmt.Sprintf(" and waits for %s to merge", strings.Join(u.DependsOn, ", "))
		}
		txs = append(txs, trace.Transaction{Transition: trace.Transition{Header: b.Header(subject+"-"+UnitPlanned, stream, op.ID, at), Subject: subject, To: UnitPlanned, Reason: reason}})
		if len(u.DependsOn) > 0 {
			planned = append(planned, fmt.Sprintf("%s (waiting for %s)", u.ID, strings.Join(u.DependsOn, ", ")))
			continue
		}
		txs = append(txs, trace.Transaction{ExpectedVersion: 1,
			Transition: trace.Transition{Header: b.Header(subject+"-"+UnitReady, stream, op.ID, at), Subject: subject, From: UnitPlanned, To: UnitReady, Reason: fmt.Sprintf("unit %s is ready: it depends on no unit", u.ID)}})
		ready = append(ready, u.ID)
	}
	reason := fmt.Sprintf("seal %d is recorded; the states of the %s of its plan are recorded: ready %s; planned %s", in.Seal, Units(len(p.Units)), List(ready), List(planned))
	h := b.Header(BuildingState, stream, op.ID, at)
	from := RatifiedState
	group, err := b.Repository.CrossRepoWorkgroupOf(stream)
	if err != nil {
		return coreadapter.OperationResult{}, err
	}
	if group != "" {
		if err := b.S.Step("build-workflow-checked:" + string(stream)); err != nil {
			return coreadapter.OperationResult{}, err
		}
		// Historical cross-repo seals may already have the individual
		// ratified transition. Use the actual preparation state for the
		// transaction; effective approval was checked above.
		transitions, err := trace.Read[trace.Transition](b.Repository, stream)
		if err != nil {
			return coreadapter.OperationResult{}, err
		}
		for _, t := range transitions {
			if t.Subject == trace.FeatureSubject {
				from = t.To
			}
		}
		if from != trace.SealPreparedState && from != RatifiedState {
			return fail(fmt.Sprintf("the workflow changed while the build ran; the workstream is %s", FeatureState(from)))
		}
	}
	_, err = b.Repository.MoveFeatureStateWith(ctx, h, from, BuildingState, reason, txs...)
	if err == nil {
		if err := b.S.Step("build-recorded"); err != nil {
			return coreadapter.OperationResult{}, err
		}
		return coreadapter.OperationResult{Outcome: "succeeded", Evidence: reason}, nil
	}
	if !errors.Is(err, trace.ErrConflict) {
		return coreadapter.OperationResult{}, err
	}
	// The workflow moved since it was read: the owner moved the workstream,
	// or a unit already has a state.
	moved, err := b.Repository.Workflow(stream, trace.FeatureSubject)
	if err != nil {
		return coreadapter.OperationResult{}, err
	}
	return fail(fmt.Sprintf("the workflow changed while the build ran; the workstream is %s", FeatureState(moved.Value)))
}

// sealedPlan returns the given revision of the workstream's plan.
func SealedPlan(repository *trace.Repository, stream config.WorkstreamID, revision int) (trace.Document, error) {
	docs, err := trace.Read[trace.Document](repository, stream)
	if err != nil {
		return trace.Document{}, err
	}
	i := slices.IndexFunc(docs, func(d trace.Document) bool { return d.ID == plan.PlanDocument && d.Revision == revision })
	if i < 0 {
		return trace.Document{}, fmt.Errorf("workstream %s has no recorded plan.json revision %d", stream, revision)
	}
	return docs[i], nil
}

// list joins the entries of a message, or says none.
func List(entries []string) string {
	if len(entries) == 0 {
		return "none"
	}
	return strings.Join(entries, ", ")
}
