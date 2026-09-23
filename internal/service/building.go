package service

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
	"github.com/kpenfound/osmia/internal/followup"
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
	buildSubject = "build"
)

// The states of a plan unit.
const (
	// UnitPlanned is a unit of the plan with a dependency not yet merged.
	UnitPlanned = "planned"
	// UnitReady is a unit whose dependencies have all merged.
	UnitReady = "ready"
	// UnitImplementing is a unit a mason works on.
	UnitImplementing = "implementing"
	// UnitReviewing is a unit whose candidate is under review.
	UnitReviewing = "reviewing"
	// UnitApproved is a unit its reviewer approved, waiting to land.
	UnitApproved = "approved"
	// UnitMerged is a unit squashed onto the feature branch.
	UnitMerged = "merged"
	// UnitWaiting is a unit whose role asked a question.
	UnitWaiting = "waiting"
	// UnitContested is a unit whose bounces passed the threshold.
	UnitContested = "contested"
)

var buildingActor = trace.Actor{Kind: "service", ID: "building"}

// buildInput names the seal a build starts from.
type buildInput struct {
	Seal int `json:"seal"`
}

func buildIDs(k int) (transition, event string) {
	transition = fmt.Sprintf("build-%d", k)
	return transition, trace.EventID(transition, "run")
}

// builder is the building controller. Its pass asks to start building every
// ratified workstream whose latest seal no build was asked for, and its
// reconciler runs each build operation.
type builder struct {
	s          *Service
	repository *trace.Repository
}

var _ coreadapter.Reconciler = (*builder)(nil)

// Pass reconciles every workstream of the trace except the librarian's.
func (b *builder) Pass(ctx context.Context) error {
	streams, err := b.repository.Workstreams()
	if err != nil {
		return err
	}
	librarian := librarianWorkstream(b.repository.Project())
	for _, stream := range streams {
		if stream == librarian {
			continue
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := b.reconcile(ctx, stream); err != nil {
			return fmt.Errorf("workstream %s building: %w", stream, err)
		}
	}
	return nil
}

// reconcile asks to start building a ratified workstream on its latest seal
// when no build on that seal was asked for.
func (b *builder) reconcile(ctx context.Context, stream config.WorkstreamID) error {
	feature, err := b.repository.Workflow(stream, trace.FeatureSubject)
	if err != nil || feature.Value != RatifiedState {
		return err
	}
	latest, doc, found, err := seal.Latest(b.repository, stream)
	if err != nil || !found {
		return err
	}
	ops, err := b.repository.Operations(stream)
	if err != nil {
		return err
	}
	for _, o := range ops {
		if o.Operation.Action != BuildAction {
			continue
		}
		in, err := decodeBuild(o.Operation)
		if err != nil {
			return err
		}
		if in.Seal == latest.Seal {
			return nil
		}
	}
	state, err := b.repository.Workflow(stream, buildSubject)
	if err != nil {
		return err
	}
	transition, event := buildIDs(latest.Seal)
	input, err := json.Marshal(buildInput{Seal: latest.Seal})
	if err != nil {
		return err
	}
	op := coreadapter.Operation{ID: trace.OperationID(b.repository.Project(), stream, event), Boundary: coreadapter.RepositoryBoundary, Action: BuildAction, Input: input}
	reason := fmt.Sprintf("seal %d is recorded; the build records the state of every unit of the sealed plan and moves the workstream to building", latest.Seal)
	tx := trace.Transaction{ExpectedVersion: state.Version,
		Transition: trace.Transition{Header: b.header(transition, stream, fmt.Sprintf("%s-%d", seal.DocumentID, doc.Revision), b.s.now()), Subject: buildSubject, From: state.Value, To: fmt.Sprintf("requested-%d", latest.Seal), Reason: reason},
		Events:     []trace.Event{{ID: event, Kind: BuildAction, Body: fmt.Sprintf("Start building on seal %d", latest.Seal), Operation: &op}}}
	_, err = b.repository.Transact(ctx, tx)
	return err
}

func (b *builder) header(id string, stream config.WorkstreamID, cause string, at time.Time) trace.Header {
	return trace.Header{Schema: "osmia.trace.transition", Version: trace.Version, ID: id, Revision: 1, Project: b.repository.Project(), Workstream: stream, At: at, Actor: buildingActor, Cause: cause}
}

func decodeBuild(op coreadapter.Operation) (buildInput, error) {
	var in buildInput
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
func (b *builder) stream(op coreadapter.Operation, k int) (config.WorkstreamID, error) {
	streams, err := b.repository.Workstreams()
	if err != nil {
		return "", err
	}
	_, event := buildIDs(k)
	for _, stream := range streams {
		if trace.OperationID(b.repository.Project(), stream, event) == op.ID {
			return stream, nil
		}
	}
	return "", fmt.Errorf("build operation %s belongs to no workstream", op.ID)
}

// outcome returns the result of a build that moved the workstream to
// building, nil before it did.
func (b *builder) outcome(stream config.WorkstreamID, operation string) (*coreadapter.OperationResult, error) {
	transitions, err := trace.Read[trace.Transition](b.repository, stream)
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
func (b *builder) Inspect(ctx context.Context, op coreadapter.Operation) (coreadapter.Observation, error) {
	in, err := decodeBuild(op)
	if err != nil {
		return coreadapter.Observation{}, err
	}
	stream, err := b.stream(op, in.Seal)
	if err != nil {
		return coreadapter.Observation{}, err
	}
	result, err := b.outcome(stream, op.ID)
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
func (b *builder) Apply(ctx context.Context, op coreadapter.Operation) (coreadapter.OperationResult, error) {
	in, err := decodeBuild(op)
	if err != nil {
		return coreadapter.OperationResult{}, err
	}
	stream, err := b.stream(op, in.Seal)
	if err != nil {
		return coreadapter.OperationResult{}, err
	}
	fail := func(reason string) (coreadapter.OperationResult, error) {
		return coreadapter.OperationResult{Outcome: "failed", Evidence: fmt.Sprintf("building on seal %d failed: %s", in.Seal, reason)}, nil
	}
	if result, err := b.outcome(stream, op.ID); err != nil || result != nil {
		if err != nil {
			return coreadapter.OperationResult{}, err
		}
		return *result, nil
	}
	feature, err := b.repository.Workflow(stream, trace.FeatureSubject)
	if err != nil {
		return coreadapter.OperationResult{}, err
	}
	if feature.Value != RatifiedState {
		return fail(fmt.Sprintf("the workstream is %s, not ratified", featureState(feature.Value)))
	}
	latest, _, found, err := seal.Latest(b.repository, stream)
	if err != nil {
		return coreadapter.OperationResult{}, err
	}
	if !found || latest.Seal != in.Seal {
		return fail("it is not the latest seal of the workstream")
	}
	graph, err := sealedPlan(b.repository, stream, latest.Revision.Plan)
	if err != nil {
		return coreadapter.OperationResult{}, err
	}
	p, err := plan.Parse([]byte(graph.Content))
	if err != nil {
		return fail("the sealed plan does not parse: " + err.Error())
	}
	at := b.s.now()
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
		txs = append(txs, trace.Transaction{Transition: trace.Transition{Header: b.header(subject+"-"+UnitPlanned, stream, op.ID, at), Subject: subject, To: UnitPlanned, Reason: reason}})
		if len(u.DependsOn) > 0 {
			planned = append(planned, fmt.Sprintf("%s (waiting for %s)", u.ID, strings.Join(u.DependsOn, ", ")))
			continue
		}
		txs = append(txs, trace.Transaction{ExpectedVersion: 1,
			Transition: trace.Transition{Header: b.header(subject+"-"+UnitReady, stream, op.ID, at), Subject: subject, From: UnitPlanned, To: UnitReady, Reason: fmt.Sprintf("unit %s is ready: it depends on no unit", u.ID)}})
		ready = append(ready, u.ID)
	}
	reason := fmt.Sprintf("seal %d is recorded; the states of the %s of its plan are recorded: ready %s; planned %s", in.Seal, units(len(p.Units)), list(ready), list(planned))
	h := b.header(BuildingState, stream, op.ID, at)
	_, err = b.repository.MoveFeatureStateWith(ctx, h, RatifiedState, BuildingState, reason, txs...)
	if err == nil {
		if err := b.s.step("build-recorded"); err != nil {
			return coreadapter.OperationResult{}, err
		}
		return coreadapter.OperationResult{Outcome: "succeeded", Evidence: reason}, nil
	}
	if !errors.Is(err, trace.ErrConflict) {
		return coreadapter.OperationResult{}, err
	}
	// The workflow moved since it was read: the owner moved the workstream,
	// or a unit already has a state.
	moved, err := b.repository.Workflow(stream, trace.FeatureSubject)
	if err != nil {
		return coreadapter.OperationResult{}, err
	}
	return fail(fmt.Sprintf("the workflow changed while the build ran; the workstream is %s", featureState(moved.Value)))
}

// sealedPlan returns the given revision of the workstream's plan.
func sealedPlan(repository *trace.Repository, stream config.WorkstreamID, revision int) (trace.Document, error) {
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
func list(entries []string) string {
	if len(entries) == 0 {
		return "none"
	}
	return strings.Join(entries, ", ")
}

// UnitStatus is the recorded state, latest card and landing of one unit of
// the sealed plan or a final-review follow-up.
type UnitStatus struct {
	Unit    string            `json:"unit"`
	State   string            `json:"state"`
	Card    *coreadapter.Card `json:"card,omitempty"`
	Landing *UnitLanding      `json:"landing,omitempty"`
}

// unitStates returns states, latest cards and landings, taken from the given
// workflow states, reports and landing documents, of the units of the
// workstream's sealed plan and final-review follow-ups, in order: none before
// their unit states are recorded.
func unitStates(repository *trace.Repository, stream config.WorkstreamID, states map[string]trace.WorkflowState) ([]UnitStatus, error) {
	latest, _, found, err := seal.Latest(repository, stream)
	if err != nil || !found {
		return nil, err
	}
	graph, err := sealedPlan(repository, stream, latest.Revision.Plan)
	if err != nil {
		return nil, err
	}
	p, err := plan.Parse([]byte(graph.Content))
	if err != nil {
		return nil, err
	}
	added, err := followup.Read(repository, stream)
	if err != nil {
		return nil, err
	}
	for _, u := range added {
		p.Units = append(p.Units, u.Unit)
	}
	documents, err := trace.Read[trace.Document](repository, stream)
	if err != nil {
		return nil, err
	}
	cards := map[string]*coreadapter.Card{}
	landings := map[string]*UnitLanding{}
	for _, document := range documents {
		if document.Unit != "" && document.ID == landingDocument(document.Unit) {
			var landing UnitLanding
			if err := json.Unmarshal([]byte(document.Content), &landing); err != nil {
				return nil, fmt.Errorf("unit %s landing: %w", document.Unit, err)
			}
			landings[document.Unit] = &landing
			continue
		}
		if document.Unit == "" || document.ID != reportDocument(document.Unit) {
			continue
		}
		var report UnitReport
		if err := json.Unmarshal([]byte(document.Content), &report); err != nil {
			return nil, fmt.Errorf("unit %s report: %w", document.Unit, err)
		}
		if report.Card != nil {
			cards[document.Unit] = report.Card
		}
	}
	var out []UnitStatus
	for _, u := range p.Units {
		if state := states[trace.UnitSubject(u.ID)].Value; state != "" {
			out = append(out, UnitStatus{Unit: u.ID, State: state, Card: cards[u.ID], Landing: landings[u.ID]})
		}
	}
	return out, nil
}
