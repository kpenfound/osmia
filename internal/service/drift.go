package service

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/kpenfound/busybees/core/vcs"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/scheduler"
	"github.com/kpenfound/osmia/internal/seal"
	"github.com/kpenfound/osmia/internal/thread"
	"github.com/kpenfound/osmia/internal/trace"
	"github.com/kpenfound/osmia/internal/workspace"
)

// DriftAction is the repository-boundary operation action that rebases a
// workstream's feature branch onto the configured upstream base branch and
// moves the seal's recorded upstream base with it.
const DriftAction = "drift"

const (
	// driftSubject is the workflow subject that tracks a workstream's drift
	// rebases: requested-<k> once drift rebase k is asked for, then
	// rebased-<k>, conflicted-<k> or skipped-<k>.
	driftSubject = "drift"
	// driftPath is the workstream document of drift rebases, and
	// driftDocument its trace record ID.
	driftPath     = "drift/rebase.json"
	driftDocument = "drift-rebase"
	// The outcomes a drift rebase document records: replayed once the
	// rebased commit exists and before the branch moves, then rebased or
	// conflicted.
	driftReplayed   = "replayed"
	driftCarrying   = "carrying"
	driftRebased    = "rebased"
	driftConflicted = "conflicted"
	driftSkipped    = "skipped"
)

// driftInput names one drift rebase of a workstream by its number.
type driftInput struct {
	Drift int `json:"drift"`
}

// DriftRebase is the document drift/rebase.json: one drift rebase of a
// workstream's feature branch. Before is the branch commit it rebased,
// Upstream the upstream commit it rebased onto and Commit the rebased
// commit. Conflicts lists the paths a conflicted replay left, and Seal the
// seal and SealRevision the seal.json revision in force once the branch
// moved.
type DriftRebase struct {
	Drift        int       `json:"drift"`
	Operation    string    `json:"operation"`
	Outcome      string    `json:"outcome"`
	Branch       string    `json:"branch"`
	Upstream     seal.Base `json:"upstream"`
	Before       string    `json:"before"`
	Commit       string    `json:"commit,omitempty"`
	Conflicts    []string  `json:"conflicts,omitempty"`
	Seal         int       `json:"seal,omitempty"`
	SealRevision int       `json:"seal_revision,omitempty"`
}

func driftIDs(k int) (transition, event string) {
	transition = fmt.Sprintf("%s-%d", driftSubject, k)
	return transition, trace.EventID(transition, "run")
}

// requestDrifts asks for a drift rebase of every building or assembled
// workstream of the project that is not paused and has no final review in
// flight, and returns the workstreams it asked for. Drift rebases share the
// project's lander with landings: while a landing or drift rebase of the
// project has no result, it asks for nothing.
func (f *foreman) requestDrifts(ctx context.Context) ([]config.WorkstreamID, error) {
	streams, err := f.repository.Workstreams()
	if err != nil {
		return nil, err
	}
	librarian := librarianWorkstream(f.repository.Project())
	reviewing := map[config.WorkstreamID]bool{}
	for _, stream := range streams {
		if stream == librarian {
			continue
		}
		ops, err := f.repository.Operations(stream)
		if err != nil {
			return nil, err
		}
		for _, o := range ops {
			if o.Result != nil {
				continue
			}
			switch o.Operation.Action {
			case LandAction, DriftAction:
				return nil, nil
			case FinalReviewAction:
				reviewing[stream] = true
			}
		}
	}
	state, _ := f.s.store.Effective()
	var requested []config.WorkstreamID
	for _, stream := range streams {
		if stream == librarian || reviewing[stream] || scheduler.Paused(state.Pauses, f.cfg.Project.ID, stream) {
			continue
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if _, found, err := f.read(stream); err != nil {
			return nil, fmt.Errorf("workstream %s drift: %w", stream, err)
		} else if !found {
			continue
		}
		if err := f.requestDrift(ctx, stream); err != nil {
			return nil, fmt.Errorf("workstream %s drift: %w", stream, err)
		}
		requested = append(requested, stream)
	}
	return requested, nil
}

// requestDrift publishes the workstream's next drift rebase as a durable
// operation.
func (f *foreman) requestDrift(ctx context.Context, stream config.WorkstreamID) error {
	state, err := f.repository.Workflow(stream, driftSubject)
	if err != nil {
		return err
	}
	k := 1
	if state.Value != "" {
		_, n, _ := strings.Cut(state.Value, "-")
		if _, err := fmt.Sscanf(n, "%d", &k); err != nil {
			return fmt.Errorf("subject %s is %q", driftSubject, state.Value)
		}
		k++
	}
	_, doc, found, err := seal.Latest(f.repository, stream)
	if err != nil {
		return err
	}
	if !found {
		return fmt.Errorf("workstream %s has no seal", stream)
	}
	transition, event := driftIDs(k)
	input, err := json.Marshal(driftInput{Drift: k})
	if err != nil {
		return err
	}
	op := coreadapter.Operation{ID: trace.OperationID(f.repository.Project(), stream, event), Boundary: coreadapter.RepositoryBoundary, Action: DriftAction, Input: input}
	reason := fmt.Sprintf("drift rebase %d fetches %s of %s and rebases %s onto it; the seal's upstream base moves once the branch has", k, f.cfg.Project.BaseBranch, f.cfg.Project.Upstream, featureBranch(stream))
	tx := trace.Transaction{ExpectedVersion: state.Version,
		Transition: trace.Transition{Header: f.header(transition, stream, "", fmt.Sprintf("%s-%d", seal.DocumentID, doc.Revision), f.s.now()), Subject: driftSubject, From: state.Value, To: fmt.Sprintf("requested-%d", k), Reason: reason},
		Events:     []trace.Event{{ID: event, Kind: DriftAction, Body: fmt.Sprintf("Rebase %s onto upstream", featureBranch(stream)), Operation: &op}}}
	if _, err := f.repository.Transact(ctx, tx); err != nil && !errors.Is(err, trace.ErrConflict) {
		return err
	}
	return nil
}

// drifter runs the drift rebase operations of the foreman.
type drifter struct{ *foreman }

var _ coreadapter.Reconciler = drifter{}

func decodeDrift(op coreadapter.Operation) (driftInput, error) {
	var in driftInput
	if op.Boundary != coreadapter.RepositoryBoundary || op.Action != DriftAction {
		return in, fmt.Errorf("unsupported repository operation %q", op.Action)
	}
	dec := json.NewDecoder(bytes.NewReader(op.Input))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&in); err != nil {
		return in, fmt.Errorf("invalid drift operation input: %w", err)
	}
	if in.Drift < 1 {
		return in, errors.New("drift operation requires a positive drift number")
	}
	return in, nil
}

// stream returns the workstream a drift operation belongs to: the one whose
// run event derives the operation ID.
func (d drifter) stream(op coreadapter.Operation, in driftInput) (config.WorkstreamID, error) {
	streams, err := d.repository.Workstreams()
	if err != nil {
		return "", err
	}
	_, event := driftIDs(in.Drift)
	for _, stream := range streams {
		if trace.OperationID(d.repository.Project(), stream, event) == op.ID {
			return stream, nil
		}
	}
	return "", fmt.Errorf("drift operation %s belongs to no workstream", op.ID)
}

// outcome returns the recorded result of drift rebase k: succeeded once it
// is recorded rebased, conflicted or skipped, nil before.
func (d drifter) outcome(stream config.WorkstreamID, k int) (*coreadapter.OperationResult, error) {
	transitions, err := trace.Read[trace.Transition](d.repository, stream)
	if err != nil {
		return nil, err
	}
	transition, _ := driftIDs(k)
	for _, t := range transitions {
		switch t.ID {
		case transition + "-" + driftRebased, transition + "-" + driftConflicted, transition + "-" + driftSkipped:
			return &coreadapter.OperationResult{Outcome: "succeeded", Evidence: t.Reason}, nil
		}
	}
	return nil, nil
}

// replayed returns the recorded replay of drift rebase k, and whether one is.
func (d drifter) replayed(stream config.WorkstreamID, k int) (DriftRebase, bool, error) {
	docs, err := trace.Read[trace.Document](d.repository, stream)
	if err != nil {
		return DriftRebase{}, false, err
	}
	for _, doc := range slices.Backward(docs) {
		if doc.ID != driftDocument {
			continue
		}
		var r DriftRebase
		if err := json.Unmarshal([]byte(doc.Content), &r); err != nil {
			return DriftRebase{}, false, fmt.Errorf("%s revision %d: %w", doc.Path, doc.Revision, err)
		}
		if r.Drift == k && r.Outcome == driftReplayed {
			return r, true, nil
		}
	}
	return DriftRebase{}, false, nil
}

func (d drifter) carrying(stream config.WorkstreamID, k int) (DriftRebase, bool, error) {
	docs, err := trace.Read[trace.Document](d.repository, stream)
	if err != nil {
		return DriftRebase{}, false, err
	}
	for _, doc := range slices.Backward(docs) {
		if doc.ID != driftDocument {
			continue
		}
		var r DriftRebase
		if err := json.Unmarshal([]byte(doc.Content), &r); err != nil {
			return DriftRebase{}, false, err
		}
		if r.Drift == k && r.Outcome == driftCarrying {
			return r, true, nil
		}
	}
	return DriftRebase{}, false, nil
}

// Inspect reads the recorded transitions and documents. A recorded outcome
// completes the operation; otherwise it is absent, with the recorded replay
// or the feature branch's tip as evidence, and Apply reconciles the branch
// before replaying.
func (d drifter) Inspect(ctx context.Context, op coreadapter.Operation) (coreadapter.Observation, error) {
	in, err := decodeDrift(op)
	if err != nil {
		return coreadapter.Observation{}, err
	}
	stream, err := d.stream(op, in)
	if err != nil {
		return coreadapter.Observation{}, err
	}
	result, err := d.outcome(stream, in.Drift)
	if err != nil {
		return coreadapter.Observation{}, err
	}
	if result != nil {
		return coreadapter.Observation{State: coreadapter.EffectCompleted, Evidence: "drift rebase " + result.Outcome, Result: result}, nil
	}
	if r, found, err := d.replayed(stream, in.Drift); err != nil {
		return coreadapter.Observation{}, err
	} else if found {
		return coreadapter.Observation{State: coreadapter.EffectAbsent, Evidence: fmt.Sprintf("drift rebase %d replayed %s onto %s as %s; the branch moves to it and the seal follows", in.Drift, r.Before, r.Upstream.Commit, r.Commit)}, nil
	}
	branch := featureBranch(stream)
	tip, exists, err := featureWorkspaces(d.cfg).Branch(ctx, branch)
	if err != nil {
		return coreadapter.Observation{}, err
	}
	if !exists {
		return coreadapter.Observation{State: coreadapter.EffectAbsent, Evidence: fmt.Sprintf("the clone has no feature branch %s", branch)}, nil
	}
	return coreadapter.Observation{State: coreadapter.EffectAbsent, Evidence: fmt.Sprintf("feature branch %s is at %s; the drift rebase has not replayed it", branch, tip)}, nil
}

// Apply runs drift rebase k. A replay this operation recorded is not
// replayed again: the branch moves to its commit. Otherwise the workstream
// must be building or assembled and not paused, and its feature branch must
// exist; the foreman fetches the configured upstream base branch and replays
// the feature branch onto it. A conflicted replay records the upstream
// commit and the conflicted paths and changes neither the branch nor the
// seal. A clean one records drift/rebase.json with the rebased commit before
// the branch moves, so a retry moves it to the same commit. Once the branch
// is at the rebased commit, one commit records the next revision of
// drift/rebase.json, the next revision of seal.json with the upstream commit
// as its base when the base changed, and the rebase's outcome. A workstream
// that no longer qualifies, or whose branch moved during the rebase, is
// skipped with the reason as its result. Storage, fetch and Git errors leave
// the operation pending for another attempt.
func (d drifter) Apply(ctx context.Context, op coreadapter.Operation) (coreadapter.OperationResult, error) {
	in, err := decodeDrift(op)
	if err != nil {
		return coreadapter.OperationResult{}, err
	}
	stream, err := d.stream(op, in)
	if err != nil {
		return coreadapter.OperationResult{}, err
	}
	if result, err := d.outcome(stream, in.Drift); err != nil || result != nil {
		if err != nil {
			return coreadapter.OperationResult{}, err
		}
		return *result, nil
	}
	if carrying, found, err := d.carrying(stream, in.Drift); err != nil {
		return coreadapter.OperationResult{}, err
	} else if found {
		return d.finishCarry(ctx, stream, carrying)
	}
	rebase, replayed, err := d.replayed(stream, in.Drift)
	if err != nil {
		return coreadapter.OperationResult{}, err
	}
	g := featureWorkspaces(d.cfg)
	branch := featureBranch(stream)
	tip, exists, err := g.Branch(ctx, branch)
	if err != nil {
		return coreadapter.OperationResult{}, err
	}
	if !replayed {
		if reason, err := d.eligible(stream); err != nil {
			return coreadapter.OperationResult{}, err
		} else if reason != "" {
			return d.skip(ctx, stream, in.Drift, op.ID, reason)
		}
		if !exists {
			return d.skip(ctx, stream, in.Drift, op.ID, fmt.Sprintf("the clone has no feature branch %s", branch))
		}
		requested, err := d.requestedAt(stream, op.ID)
		if err != nil {
			return coreadapter.OperationResult{}, err
		}
		remote, err := g.Remote(ctx, d.cfg.Project.Upstream)
		if err != nil {
			return coreadapter.OperationResult{}, err
		}
		fetched, err := g.Fetch(ctx, remote, d.cfg.Project.BaseBranch)
		if err != nil {
			return coreadapter.OperationResult{}, fmt.Errorf("fetch %s of %s: %w", d.cfg.Project.BaseBranch, remote, err)
		}
		rebase = DriftRebase{Drift: in.Drift, Operation: op.ID, Branch: branch, Upstream: seal.Base{Remote: remote, Branch: d.cfg.Project.BaseBranch, Commit: fetched}, Before: tip}
		if err := d.s.step("drift-replaying"); err != nil {
			return coreadapter.OperationResult{}, err
		}
		commit, conflicts, err := g.Replay(ctx, tip, fetched, requested)
		if err != nil {
			return coreadapter.OperationResult{}, err
		}
		if len(conflicts) > 0 {
			rebase.Outcome, rebase.Conflicts = driftConflicted, conflicts
			reason := fmt.Sprintf("feature branch %s does not rebase cleanly onto %s/%s at %s: %s conflicted; the branch stays at %s and the seal is unchanged", branch, remote, rebase.Upstream.Branch, fetched, strings.Join(conflicts, ", "), tip)
			return d.record(ctx, stream, rebase, nil, driftConflicted, reason)
		}
		rebase.Outcome, rebase.Commit = driftReplayed, commit
		if err := d.recordReplay(ctx, stream, rebase); err != nil {
			return coreadapter.OperationResult{}, err
		}
		if err := d.s.step("drift-replayed"); err != nil {
			return coreadapter.OperationResult{}, err
		}
	}
	if !exists || (tip != rebase.Before && tip != rebase.Commit) {
		if !exists {
			tip = "nothing"
		}
		return d.skip(ctx, stream, in.Drift, op.ID, fmt.Sprintf("feature branch %s moved to %s during the drift rebase, which replayed %s", branch, tip, rebase.Before))
	}
	if rebase.Commit != rebase.Before {
		acquired, err := g.Acquire(ctx, vcs.Request{Name: string(stream), Branch: branch})
		if err != nil {
			return coreadapter.OperationResult{}, fmt.Errorf("feature branch %s: %w", branch, err)
		}
		if err := g.Move(ctx, acquired.(workspace.Worktree), rebase.Before, rebase.Commit); err != nil {
			return coreadapter.OperationResult{}, err
		}
	}
	if err := d.s.step("drift-moved"); err != nil {
		return coreadapter.OperationResult{}, err
	}
	current, sealDoc, found, err := seal.Latest(d.repository, stream)
	if err != nil {
		return coreadapter.OperationResult{}, err
	}
	if !found {
		return coreadapter.OperationResult{}, fmt.Errorf("workstream %s has no seal", stream)
	}
	rebase.Outcome, rebase.Seal, rebase.SealRevision = driftRebased, current.Seal, sealDoc.Revision
	var moved *trace.Document
	resealed := fmt.Sprintf("seal %d keeps its base %s", current.Seal, current.Base.Commit)
	if current.Base != rebase.Upstream {
		next := current
		next.Base = rebase.Upstream
		content, err := seal.Encode(next)
		if err != nil {
			return coreadapter.OperationResult{}, err
		}
		rebase.SealRevision = sealDoc.Revision + 1
		moved = &trace.Document{Header: trace.Header{Schema: "osmia.trace.document", Version: trace.Version, ID: seal.DocumentID, Revision: rebase.SealRevision, Project: d.repository.Project(), Workstream: stream, At: d.s.now(), Actor: foremanActor, Cause: op.ID},
			Path: seal.Path, Content: string(content)}
		resealed = fmt.Sprintf("seal %d moves from base %s to %s in %s revision %d", current.Seal, current.Base.Commit, rebase.Upstream.Commit, seal.Path, rebase.SealRevision)
	}
	onto := fmt.Sprintf("%s/%s at %s", rebase.Upstream.Remote, rebase.Upstream.Branch, rebase.Upstream.Commit)
	reason := fmt.Sprintf("feature branch %s is rebased from %s onto %s as %s; %s", branch, rebase.Before, onto, rebase.Commit, resealed)
	if rebase.Commit == rebase.Before {
		reason = fmt.Sprintf("feature branch %s at %s already descends from %s; %s", branch, rebase.Before, onto, resealed)
	}
	b, found, err := d.read(stream)
	if err != nil {
		return coreadapter.OperationResult{}, err
	}
	needsCarry := false
	if found {
		units := newUnitWorkspaces(d.cfg)
		for _, unit := range b.plan.Units {
			state := b.states[trace.UnitSubject(unit.ID)].Value
			if state == "" || state == UnitPlanned || state == UnitMerged {
				continue
			}
			behind, err := units.behind(ctx, stream, unit.ID)
			if err != nil {
				return coreadapter.OperationResult{}, err
			}
			needsCarry = needsCarry || behind
		}
	}
	if needsCarry {
		rebase.Outcome = driftCarrying
		if _, err := d.record(ctx, stream, rebase, moved, driftCarrying, reason); err != nil {
			return coreadapter.OperationResult{}, err
		}
		return d.finishCarry(ctx, stream, rebase)
	}
	return d.record(ctx, stream, rebase, moved, driftRebased, reason)
}

// finishCarry uses the unit rebase path while the drift operation holds the
// project lander. Each requested rebase and conflict turn is durable, so a
// retry only asks for work that has not already been recorded.
func (d drifter) finishCarry(ctx context.Context, stream config.WorkstreamID, rebase DriftRebase) (coreadapter.OperationResult, error) {
	b, found, err := d.read(stream)
	if err != nil {
		return coreadapter.OperationResult{}, err
	}
	if !found {
		return coreadapter.OperationResult{}, fmt.Errorf("workstream %s disappeared during drift", stream)
	}
	ops, err := d.repository.Operations(stream)
	if err != nil {
		return coreadapter.OperationResult{}, err
	}
	rebasing, rebased, dispatched := map[string]bool{}, map[string][]string{}, map[string]bool{}
	for _, o := range ops {
		switch o.Operation.Action {
		case RebaseAction:
			in, err := decodeRebase(o.Operation)
			if err != nil {
				return coreadapter.OperationResult{}, err
			}
			if o.Result == nil {
				rebasing[in.Unit] = true
			} else if o.Result.Outcome == "succeeded" {
				rebased[in.Unit] = append(rebased[in.Unit], in.Onto)
			}
		default:
			if in, err := thread.DecodeTurn(o.Operation); err == nil {
				dispatched[in.Agent+"/"+in.Turn] = true
			}
		}
	}
	current, err := d.refresh(ctx, b, rebasing, dispatched, rebased)
	if err != nil {
		return coreadapter.OperationResult{}, err
	}
	if !current {
		return coreadapter.OperationResult{}, fmt.Errorf("drift rebase %d awaits unit carryover", rebase.Drift)
	}
	units := newUnitWorkspaces(d.cfg)
	for _, unit := range b.plan.Units {
		state := b.states[trace.UnitSubject(unit.ID)].Value
		if state == "" || state == UnitPlanned || state == UnitMerged {
			continue
		}
		behind, err := units.behind(ctx, stream, unit.ID)
		if err != nil {
			return coreadapter.OperationResult{}, err
		}
		if behind {
			return coreadapter.OperationResult{}, fmt.Errorf("drift rebase %d awaits unit %s's workspace at %s", rebase.Drift, unit.ID, rebase.Commit)
		}
	}
	rebase.Outcome = driftRebased
	reason := fmt.Sprintf("feature branch %s and every unfinished unit are current at %s, or a conflict turn is queued", rebase.Branch, rebase.Commit)
	return d.record(ctx, stream, rebase, nil, driftRebased, reason)
}

// eligible returns why the workstream takes no drift rebase now, or "" when
// it is building or assembled and not paused.
func (d drifter) eligible(stream config.WorkstreamID) (string, error) {
	feature, err := d.repository.Workflow(stream, trace.FeatureSubject)
	if err != nil {
		return "", err
	}
	if feature.Value != BuildingState && feature.Value != AssembledState {
		return fmt.Sprintf("the workstream is %s, not building or assembled", featureState(feature.Value)), nil
	}
	state, _ := d.s.store.Effective()
	if scheduler.Paused(state.Pauses, d.cfg.Project.ID, stream) {
		return "the workstream is paused", nil
	}
	return "", nil
}

// document returns the next revision of drift/rebase.json holding rebase.
func (d drifter) document(stream config.WorkstreamID, rebase DriftRebase) (trace.Document, error) {
	content, err := json.MarshalIndent(rebase, "", "  ")
	if err != nil {
		return trace.Document{}, err
	}
	revision, err := nextRevision(d.repository, stream, driftDocument)
	if err != nil {
		return trace.Document{}, err
	}
	return trace.Document{Header: trace.Header{Schema: "osmia.trace.document", Version: trace.Version, ID: driftDocument, Revision: revision, Project: d.repository.Project(), Workstream: stream, At: d.s.now(), Actor: foremanActor, Cause: rebase.Operation},
		Path: driftPath, Content: string(content) + "\n"}, nil
}

// recordReplay records the replay of a drift rebase before its branch moves.
func (d drifter) recordReplay(ctx context.Context, stream config.WorkstreamID, rebase DriftRebase) error {
	doc, err := d.document(stream, rebase)
	if err != nil {
		return err
	}
	return d.repository.RecordDocuments(ctx, []trace.Document{doc})
}

// record records, in one commit, drift/rebase.json, the moved seal when
// there is one, and the drift subject's move to the outcome. An outcome
// already recorded returns its result and records nothing more.
func (d drifter) record(ctx context.Context, stream config.WorkstreamID, rebase DriftRebase, moved *trace.Document, outcome, reason string) (coreadapter.OperationResult, error) {
	doc, err := d.document(stream, rebase)
	if err != nil {
		return coreadapter.OperationResult{}, err
	}
	docs := []trace.Document{doc}
	if moved != nil {
		docs = append(docs, *moved)
	}
	state, err := d.repository.Workflow(stream, driftSubject)
	if err != nil {
		return coreadapter.OperationResult{}, err
	}
	transition, _ := driftIDs(rebase.Drift)
	tx := trace.Transaction{ExpectedVersion: state.Version,
		Transition: trace.Transition{Header: d.header(transition+"-"+outcome, stream, "", rebase.Operation, d.s.now()), Subject: driftSubject, From: state.Value, To: fmt.Sprintf("%s-%d", outcome, rebase.Drift), Reason: reason}}
	if _, err := d.repository.RecordDocumentsWith(ctx, docs, tx); err != nil {
		if errors.Is(err, trace.ErrConflict) {
			if recorded, outcomeErr := d.outcome(stream, rebase.Drift); outcomeErr == nil && recorded != nil {
				return *recorded, nil
			}
		}
		return coreadapter.OperationResult{}, err
	}
	if err := d.s.step("drift-recorded"); err != nil {
		return coreadapter.OperationResult{}, err
	}
	return coreadapter.OperationResult{Outcome: "succeeded", Evidence: reason}, nil
}

// skip records that drift rebase k changed nothing, and why.
func (d drifter) skip(ctx context.Context, stream config.WorkstreamID, k int, operation, why string) (coreadapter.OperationResult, error) {
	state, err := d.repository.Workflow(stream, driftSubject)
	if err != nil {
		return coreadapter.OperationResult{}, err
	}
	transition, _ := driftIDs(k)
	reason := fmt.Sprintf("drift rebase %d changed nothing: %s", k, why)
	tx := trace.Transaction{ExpectedVersion: state.Version,
		Transition: trace.Transition{Header: d.header(transition+"-"+driftSkipped, stream, "", operation, d.s.now()), Subject: driftSubject, From: state.Value, To: fmt.Sprintf("%s-%d", driftSkipped, k), Reason: reason}}
	if _, err := d.repository.Transact(ctx, tx); err != nil {
		if errors.Is(err, trace.ErrConflict) {
			if recorded, outcomeErr := d.outcome(stream, k); outcomeErr == nil && recorded != nil {
				return *recorded, nil
			}
		}
		return coreadapter.OperationResult{}, err
	}
	return coreadapter.OperationResult{Outcome: "succeeded", Evidence: reason}, nil
}
