package intake

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/kpenfound/agentcore/vcs"
	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/kb"
	"github.com/kpenfound/osmia/internal/plan"
	"github.com/kpenfound/osmia/internal/seal"
	"github.com/kpenfound/osmia/internal/shed"
	"github.com/kpenfound/osmia/internal/trace"
	"github.com/kpenfound/osmia/internal/workspace"
)

// SealAction is the repository-boundary operation action that seals one
// ratification: it fetches upstream, records the seal and the footprints,
// creates the feature branch in the service's workspace and moves the
// workstream to ratified.
const SealAction = "seal"

const (
	// RatifiedState is the feature state of a workstream whose ratified spec
	// and plan are sealed and whose feature branch exists.
	RatifiedState = "ratified"
	// sealSubject is the workflow subject that tracks the sealings of one
	// workstream: sealing-<k> while sealing k, the latest asked for, runs
	// or waits to retry, and failed-<k> once it failed for a reason a retry
	// does not put right. A sealing that succeeds is recorded by the feature
	// state it moves, and one that fails after a later sealing was asked for
	// records its failure without moving the subject.
	SealSubject = "seal"
	// branchesDirectory is the directory under the root holding the feature
	// branch workspaces, by project and workstream.
	BranchesDirectory = "branches"
)

var SealingActor = trace.Actor{Kind: "service", ID: "sealing"}

// sealInput identifies the ratification a sealing seals: its number, the
// round the owner ratified in, the revisions they ratified and the revision
// of the ratification's record, which the owner records again to ask for a
// sealing after one failed.
type SealInput struct {
	Seal         int `json:"seal"`
	Round        int `json:"round"`
	Spec         int `json:"spec"`
	Plan         int `json:"plan"`
	Ratification int `json:"ratification"`
}

func (in SealInput) Pin() shed.Pin { return shed.Pin{Spec: in.Spec, Plan: in.Plan} }

// seals reports whether the sealing is of the given ratification record.
func (in SealInput) Seals(r Ratification) bool {
	return in.Round == r.Round && in.Pin() == r.Revision && in.Ratification == r.Recorded
}

// ratification is the owner's recorded ratification with the revision of
// its record.
type Ratification struct {
	shed.Ratification
	Recorded int
}

func SealIDs(k int) (transition, event string) {
	transition = fmt.Sprintf("seal-%d", k)
	return transition, trace.EventID(transition, "run")
}

// featureBranch names the feature branch of a workstream in the project's
// clone.
func FeatureBranch(stream config.WorkstreamID) string { return "osmia/" + string(stream) }

// sealer is the sealing controller. Its pass asks for the sealing of every
// ratification record no sealing was asked for: the record is the owner's
// request, and the controller alone publishes the operation. Its reconciler
// runs each sealing operation.
type Sealer struct {
	S          *SealerDependencies
	Repository *trace.Repository
}

var _ coreadapter.Reconciler = (*Sealer)(nil)

// Pass reconciles every workstream of the trace except the librarian's.
func (z *Sealer) Pass(ctx context.Context) error {
	streams, err := z.Repository.Workstreams()
	if err != nil {
		return err
	}
	librarian := LibrarianWorkstream(z.Repository.Project())
	for _, stream := range streams {
		if stream == librarian {
			continue
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := z.Reconcile(ctx, stream); err != nil {
			return fmt.Errorf("workstream %s sealing: %w", stream, err)
		}
	}
	return nil
}

// reconcile asks for the sealing of the latest ratification record of a
// workstream in the shed when no sealing of that record was asked for. A
// ratification whose sealing failed is recorded again by the owner to be
// sealed again, never asked for again here.
func (z *Sealer) Reconcile(ctx context.Context, stream config.WorkstreamID) error {
	feature, err := z.Repository.Workflow(stream, trace.FeatureSubject)
	if err != nil {
		return err
	}
	if feature.Value != InShedState && feature.Value != SketchedState {
		return nil
	}
	latest, ok, err := LatestRatification(z.Repository, stream)
	if err != nil || !ok {
		return err
	}
	_, requested, err := z.Sealing(stream, latest)
	if err != nil || requested {
		return err
	}
	_, err = z.Request(ctx, stream, latest)
	return err
}

// latestRatification returns the owner's latest ratification of the
// workstream, the one of the latest round at its latest recorded revision,
// and whether there is one.
func LatestRatification(repository *trace.Repository, stream config.WorkstreamID) (Ratification, bool, error) {
	documents, err := trace.Read[trace.Document](repository, stream)
	if err != nil {
		return Ratification{}, false, err
	}
	var latest trace.Document
	round := 0
	for _, d := range documents {
		if n, ok := shed.RatificationRound(d.Path); ok && n >= round {
			latest, round = d, n
		}
	}
	if round == 0 {
		return Ratification{}, false, nil
	}
	r, err := shed.ParseRatification([]byte(latest.Content))
	if err != nil {
		return Ratification{}, false, fmt.Errorf("%s: %w", latest.Path, err)
	}
	if r.Round != round {
		return Ratification{}, false, fmt.Errorf("%s: records the ratification of round %d", latest.Path, r.Round)
	}
	return Ratification{Ratification: r, Recorded: latest.Revision}, true, nil
}

// sealing returns the latest sealing operation of the given ratification
// record, and whether one was asked for.
func (z *Sealer) Sealing(stream config.WorkstreamID, r Ratification) (trace.OperationRecord, bool, error) {
	ops, err := z.Repository.Operations(stream)
	if err != nil {
		return trace.OperationRecord{}, false, err
	}
	var latest trace.OperationRecord
	found, n := false, 0
	for _, o := range ops {
		if o.Operation.Action != SealAction {
			continue
		}
		in, err := DecodeSeal(o.Operation)
		if err != nil {
			return trace.OperationRecord{}, false, err
		}
		if in.Seals(r) && in.Seal > n {
			latest, found, n = o, true, in.Seal
		}
	}
	return latest, found, nil
}

func (z *Sealer) Header(id string, stream config.WorkstreamID, cause string, at time.Time) trace.Header {
	return trace.Header{Schema: "osmia.trace.transition", Version: trace.Version, ID: id, Revision: 1, Project: z.Repository.Project(), Workstream: stream, At: at, Actor: SealingActor, Cause: cause}
}

// request publishes the next sealing of the workstream, of the given
// ratification record, as a durable operation, and returns its number: the
// one after the highest asked for, whatever the subject reads.
func (z *Sealer) Request(ctx context.Context, stream config.WorkstreamID, r Ratification) (int, error) {
	state, err := z.Repository.Workflow(stream, SealSubject)
	if err != nil {
		return 0, err
	}
	ops, err := z.Repository.Operations(stream)
	if err != nil {
		return 0, err
	}
	k := 1
	for _, o := range ops {
		if o.Operation.Action != SealAction {
			continue
		}
		in, err := DecodeSeal(o.Operation)
		if err != nil {
			return 0, err
		}
		k = max(k, in.Seal+1)
	}
	transition, event := SealIDs(k)
	input, err := json.Marshal(SealInput{Seal: k, Round: r.Round, Spec: r.Revision.Spec, Plan: r.Revision.Plan, Ratification: r.Recorded})
	if err != nil {
		return 0, err
	}
	op := coreadapter.Operation{ID: trace.OperationID(z.Repository.Project(), stream, event), Boundary: coreadapter.RepositoryBoundary, Action: SealAction, Input: input}
	reason := fmt.Sprintf("the owner ratified %s in round %d; sealing %d fetches upstream, records the seal and the footprints and creates the feature branch", r.Revision, r.Round, k)
	tx := trace.Transaction{ExpectedVersion: state.Version,
		Transition: trace.Transition{Header: z.Header(transition, stream, fmt.Sprintf("%s-%d", shed.RatificationDocumentID(r.Round), r.Recorded), z.S.Now()), Subject: SealSubject, From: state.Value, To: fmt.Sprintf("sealing-%d", k), Reason: reason},
		Events:     []trace.Event{{ID: event, Kind: "seal", Body: fmt.Sprintf("Seal %s of the workstream", r.Revision), Operation: &op}}}
	_, err = z.Repository.Transact(ctx, tx)
	return k, err
}

func DecodeSeal(op coreadapter.Operation) (SealInput, error) {
	var in SealInput
	if op.Boundary != coreadapter.RepositoryBoundary || op.Action != SealAction {
		return in, fmt.Errorf("unsupported repository operation %q", op.Action)
	}
	dec := json.NewDecoder(bytes.NewReader(op.Input))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&in); err != nil {
		return in, fmt.Errorf("invalid seal operation input: %w", err)
	}
	if in.Seal < 1 || in.Round < 1 || in.Spec < 1 || in.Plan < 1 || in.Ratification < 1 {
		return in, errors.New("seal operation requires positive sealing, round, revision and record numbers")
	}
	return in, nil
}

// stream returns the workstream a sealing operation belongs to: the one whose
// run event derives the operation ID.
func (z *Sealer) Stream(op coreadapter.Operation, k int) (config.WorkstreamID, error) {
	streams, err := z.Repository.Workstreams()
	if err != nil {
		return "", err
	}
	_, event := SealIDs(k)
	for _, stream := range streams {
		if trace.OperationID(z.Repository.Project(), stream, event) == op.ID {
			return stream, nil
		}
	}
	return "", fmt.Errorf("seal operation %s belongs to no workstream", op.ID)
}

// outcome returns the recorded terminal result of sealing k: succeeded when
// it moved the workstream to ratified, failed when its failure is recorded,
// nil before either.
func (z *Sealer) Outcome(stream config.WorkstreamID, k int, operation string) (*coreadapter.OperationResult, error) {
	transitions, err := trace.Read[trace.Transition](z.Repository, stream)
	if err != nil {
		return nil, err
	}
	transition, _ := SealIDs(k)
	for _, t := range transitions {
		switch {
		case t.Subject == trace.FeatureSubject && (t.To == RatifiedState || t.To == trace.SealPreparedState) && t.Cause == operation:
			return &coreadapter.OperationResult{Outcome: "succeeded", Evidence: t.Reason}, nil
		case t.Subject == SealSubject && t.ID == transition+"-failed":
			return &coreadapter.OperationResult{Outcome: "failed", Evidence: t.Reason}, nil
		}
	}
	return nil, nil
}

// git returns the provider of the workstream's feature branch workspace.
func (z *Sealer) Git(stream config.WorkstreamID) (workspace.Provider, error) {
	return FeatureWorkspaces(z.S.About(z.Repository), z.Repository).Of(stream)
}

// Inspect reads the recorded transitions and the clone. A recorded outcome
// completes the operation; otherwise it is absent, with what the clone holds
// as evidence, and Apply resumes from the branch and workspace that exist.
func (z *Sealer) Inspect(ctx context.Context, op coreadapter.Operation) (coreadapter.Observation, error) {
	in, err := DecodeSeal(op)
	if err != nil {
		return coreadapter.Observation{}, err
	}
	stream, err := z.Stream(op, in.Seal)
	if err != nil {
		return coreadapter.Observation{}, err
	}
	result, err := z.Outcome(stream, in.Seal, op.ID)
	if err != nil {
		return coreadapter.Observation{}, err
	}
	if result != nil {
		return coreadapter.Observation{State: coreadapter.EffectCompleted, Evidence: "sealing " + result.Outcome, Result: result}, nil
	}
	branch := FeatureBranch(stream)
	g, err := z.Git(stream)
	if err != nil {
		return coreadapter.Observation{}, err
	}
	_, exists, err := g.Branch(ctx, branch)
	if err != nil {
		return coreadapter.Observation{}, err
	}
	if exists {
		return coreadapter.Observation{State: coreadapter.EffectAbsent, Evidence: fmt.Sprintf("the clone has feature branch %s; the sealing resumes from it", branch)}, nil
	}
	return coreadapter.Observation{State: coreadapter.EffectAbsent, Evidence: fmt.Sprintf("the clone has no feature branch %s", branch)}, nil
}

// Apply seals the ratification: it fetches the upstream base branch, takes
// the footprints of the ratified plan, creates the feature branch from the
// fetched commit in the service's workspace, records seal.json and moves the
// workstream to ratified. A branch or workspace an earlier attempt created is
// used, never made again. A fetch or branch creation that fails leaves the
// operation pending for another attempt. An abandoned workstream, a
// ratification a later one superseded, a branch that is not on upstream and
// a footprint the entity map does not resolve are recorded failures.
func (z *Sealer) Apply(ctx context.Context, op coreadapter.Operation) (coreadapter.OperationResult, error) {
	in, err := DecodeSeal(op)
	if err != nil {
		return coreadapter.OperationResult{}, err
	}
	stream, err := z.Stream(op, in.Seal)
	if err != nil {
		return coreadapter.OperationResult{}, err
	}
	fail := func(reason string) (coreadapter.OperationResult, error) { return z.Fail(ctx, stream, in, reason) }
	feature, err := z.Repository.Workflow(stream, trace.FeatureSubject)
	if err != nil {
		return coreadapter.OperationResult{}, err
	}
	if feature.Value != InShedState && feature.Value != SketchedState {
		return fail(fmt.Sprintf("the workstream is %s, not in the shed", FeatureState(feature.Value)))
	}
	latest, ok, err := LatestRatification(z.Repository, stream)
	if err != nil {
		return coreadapter.OperationResult{}, err
	}
	if !ok {
		return fail("the workstream has no ratification on the record")
	}
	if !in.Seals(latest) {
		if latest.Round == in.Round && latest.Revision == in.Pin() {
			return fail(fmt.Sprintf("the owner ratified %s in round %d again after this sealing was asked for; the later record is sealed instead", in.Pin(), in.Round))
		}
		return fail(fmt.Sprintf("the owner's latest ratification is of %s in round %d, not of %s in round %d", latest.Revision, latest.Round, in.Pin(), in.Round))
	}
	spec, graph, err := z.Documents(stream, in.Pin())
	if err != nil {
		return coreadapter.OperationResult{}, err
	}
	p, err := plan.Parse([]byte(graph.Content))
	if err != nil {
		return fail("the ratified plan does not parse: " + err.Error())
	}
	entities, err := kb.Load(z.Repository)
	if err != nil {
		return coreadapter.OperationResult{}, err
	}
	footprints, unresolved := seal.Take(p, entities)
	if len(unresolved) > 0 {
		return fail("the plan's footprints name what the entity map does not resolve: " + strings.Join(unresolved, ", "))
	}
	g, err := z.Git(stream)
	if err != nil {
		return coreadapter.OperationResult{}, err
	}
	selected, err := z.SelectedBase(ctx, stream, in.Seal, op.ID, g)
	if err != nil {
		return coreadapter.OperationResult{}, err
	}
	remote, fetched := selected.Remote, selected.Commit
	branch := FeatureBranch(stream)
	base, exists, err := g.Branch(ctx, branch)
	if err != nil {
		return coreadapter.OperationResult{}, err
	}
	if exists {
		// A branch an earlier attempt created is the feature branch, and the
		// commit it was created from is the seal. Any other branch of that
		// name is not the service's to build on.
		if base != fetched {
			return fail(fmt.Sprintf("the clone has a branch %s at %s that is not on %s/%s; it is not the service's feature branch", branch, base, remote, selected.Branch))
		}
	} else {
		base = fetched
	}
	ws, err := g.Acquire(ctx, vcs.Request{Name: string(stream), Ref: base, Branch: branch})
	if err != nil {
		return coreadapter.OperationResult{}, fmt.Errorf("feature branch %s: %w", branch, err)
	}
	if err := z.S.Step("seal-branch-created"); err != nil {
		return coreadapter.OperationResult{}, err
	}
	selected.Commit = base
	record := seal.Seal{Version: seal.Version, Seal: in.Seal, Round: in.Round, Revision: in.Pin(), SpecHash: seal.SpecHash(spec.Content),
		Base: selected, Branch: branch, Workspace: ws.Directory(), Footprints: footprints}
	// The branch stays whatever happens next. An abandonment up to here
	// records nothing more; one that lands between this check and the
	// record leaves seal.json on the abandoned workstream, and the move to
	// ratified, which expects the state the attempt read, refuses it.
	if gone, err := Abandoned(z.Repository, stream); err != nil || gone {
		if err != nil {
			return coreadapter.OperationResult{}, err
		}
		return fail("the owner abandoned the workstream; its feature branch " + branch + " stays in the clone")
	}
	if err := z.Record(ctx, stream, record, op.ID); err != nil {
		return coreadapter.OperationResult{}, err
	}
	if err := z.S.Step("seal-recorded"); err != nil {
		return coreadapter.OperationResult{}, err
	}
	reason := fmt.Sprintf("sealed %s at %s of %s/%s (%s); feature branch %s is checked out in %s; the footprints of %s are recorded",
		in.Pin(), base, remote, selected.Branch, record.SpecHash, branch, ws.Directory(), Units(len(footprints)))
	to := RatifiedState
	group, err := z.Repository.GroupApprovalRequired(stream)
	if err != nil {
		return coreadapter.OperationResult{}, err
	}
	if group {
		to = trace.SealPreparedState
		reason += "; preparation awaits the durable completion of every group member"
	}
	h := z.Header(to, stream, op.ID, z.S.Now())
	_, err = z.Repository.MoveFeatureState(ctx, h, feature.Value, to, reason)
	if err == nil {
		return coreadapter.OperationResult{Outcome: "succeeded", Evidence: reason}, nil
	}
	if !errors.Is(err, trace.ErrConflict) {
		return coreadapter.OperationResult{}, err
	}
	// The state moved since it was read: to ratified by an earlier attempt
	// of this sealing, or elsewhere by the owner.
	result, err := z.Outcome(stream, in.Seal, op.ID)
	if err != nil {
		return coreadapter.OperationResult{}, err
	}
	if result != nil {
		return *result, nil
	}
	moved, err := z.Repository.Workflow(stream, trace.FeatureSubject)
	if err != nil {
		return coreadapter.OperationResult{}, err
	}
	return fail(fmt.Sprintf("the workstream is %s, not in the shed", FeatureState(moved.Value)))
}

// documents returns the ratified revisions of the spec and the plan.
func (z *Sealer) Documents(stream config.WorkstreamID, pin shed.Pin) (spec, graph trace.Document, err error) {
	docs, err := trace.Read[trace.Document](z.Repository, stream)
	if err != nil {
		return trace.Document{}, trace.Document{}, err
	}
	i := slices.IndexFunc(docs, func(d trace.Document) bool { return d.ID == plan.SpecDocument && d.Revision == pin.Spec })
	j := slices.IndexFunc(docs, func(d trace.Document) bool { return d.ID == plan.PlanDocument && d.Revision == pin.Plan })
	if i < 0 || j < 0 {
		return trace.Document{}, trace.Document{}, fmt.Errorf("workstream %s has no recorded %s", stream, pin)
	}
	return docs[i], docs[j], nil
}

// record records the seal as the next revision of seal.json, unless this
// sealing recorded it already.
func (z *Sealer) Record(ctx context.Context, stream config.WorkstreamID, record seal.Seal, cause string) error {
	latest, doc, found, err := seal.PreparedLatest(z.Repository, stream)
	if err != nil {
		return err
	}
	if found && latest.Seal == record.Seal {
		return nil
	}
	content, err := seal.Encode(record)
	if err != nil {
		return err
	}
	revision := 1
	if found {
		revision = doc.Revision + 1
	}
	return z.Repository.RecordDocuments(ctx, []trace.Document{{Header: trace.Header{Schema: "osmia.trace.document", Version: trace.Version, ID: seal.DocumentID, Revision: revision,
		Project: z.Repository.Project(), Workstream: stream, At: z.S.Now(), Actor: SealingActor, Cause: cause}, Path: seal.Path, Content: string(content)}})
}

// fail records why the sealing failed, tells the chief of staff, and returns
// the failure as the operation's result. The subject moves to failed-<k>
// only while it still reads sealing-<k>: a sealing a later one has replaced
// records its failure and leaves the subject to the later one.
func (z *Sealer) Fail(ctx context.Context, stream config.WorkstreamID, in SealInput, reason string) (coreadapter.OperationResult, error) {
	transition, _ := SealIDs(in.Seal)
	id := transition + "-failed"
	state, err := z.Repository.Workflow(stream, SealSubject)
	if err != nil {
		return coreadapter.OperationResult{}, err
	}
	to := state.Value
	if state.Value == fmt.Sprintf("sealing-%d", in.Seal) {
		to = fmt.Sprintf("failed-%d", in.Seal)
	}
	recorded := fmt.Sprintf("sealing %d of %s failed: %s", in.Seal, in.Pin(), reason)
	body := fmt.Sprintf("The sealing of %s failed and the workstream is not ratified: %s. Once that is put right, ratifying the same revisions again asks for the sealing again.", in.Pin(), reason)
	tx := trace.Transaction{ExpectedVersion: state.Version,
		Transition: trace.Transition{Header: z.Header(id, stream, transition, z.S.Now()), Subject: SealSubject, From: state.Value, To: to, Reason: recorded},
		Events:     []trace.Event{trace.Notice(id, "failed", body)}}
	if _, err := z.Repository.Transact(ctx, tx); err != nil {
		return coreadapter.OperationResult{}, err
	}
	return coreadapter.OperationResult{Outcome: "failed", Evidence: recorded}, nil
}

// units counts units for a message.
func Units(n int) string {
	if n == 1 {
		return "1 unit"
	}
	return strconv.Itoa(n) + " units"
}

// selectedBase commits the exact starting revision before a branch can exist.
// Restart recovery reuses it even if the dependency integrates meanwhile.
func (z *Sealer) SelectedBase(ctx context.Context, stream config.WorkstreamID, number int, operation string, g workspace.Provider) (seal.Base, error) {
	id := fmt.Sprintf("seal-base-%d", number)
	docs, err := trace.Read[trace.Document](z.Repository, stream)
	if err != nil {
		return seal.Base{}, err
	}
	for _, doc := range docs {
		if doc.ID == id {
			var base seal.Base
			if err := json.Unmarshal([]byte(doc.Content), &base); err != nil {
				return base, err
			}
			if doc.Cause != operation || base.Commit == "" || base.Branch == "" {
				return base, errors.New("sealing base intent differs from its operation")
			}
			return base, nil
		}
	}
	selected, err := z.S.ResolveBase(ctx, z.S.About(z.Repository), z.Repository, stream, g)
	if err != nil {
		return selected, err
	}
	data, err := json.Marshal(selected)
	if err != nil {
		return selected, err
	}
	h := z.Header(id, stream, operation, z.S.Now())
	h.Schema = "osmia.trace.document"
	err = z.Repository.RecordDocuments(ctx, []trace.Document{{Header: h, Path: fmt.Sprintf("sealing/base-%d.json", number), Content: string(data)}})
	return selected, err
}
