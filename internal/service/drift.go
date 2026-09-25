package service

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
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
	// rebased-<k> or skipped-<k>, through conflicted-<k> while a conflict
	// the replay left is resolved.
	driftSubject = "drift"
	// driftPath is the workstream document of drift rebases, and
	// driftDocument its trace record ID.
	driftPath     = "drift/rebase.json"
	driftDocument = "drift-rebase"
	// The outcomes a drift rebase document records: replayed once the
	// rebased commit exists and before the branch moves, then carrying
	// while unfinished units follow the branch, and rebased. A replay that
	// conflicts is recorded conflicted and resolved before it is replayed.
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
// From the upstream commit the seal's base named when it fetched, Upstream
// the upstream commit it rebased onto and Commit the rebased commit.
// Conflicts lists the paths a conflicted replay left, and Seal the seal and
// SealRevision the seal.json revision in force once the branch moved. A conflicted replay is resolved in rounds: Round numbers the
// replay's stops in the resolution workspace, Stop is the commit the
// latest stopped at, Candidate the resolved branch under review, Review
// the number of that review and Verdict the latest review's verdict.
type DriftRebase struct {
	Drift        int          `json:"drift"`
	Operation    string       `json:"operation"`
	Outcome      string       `json:"outcome"`
	Branch       string       `json:"branch"`
	Upstream     seal.Base    `json:"upstream"`
	From         string       `json:"from,omitempty"`
	Before       string       `json:"before"`
	Commit       string       `json:"commit,omitempty"`
	Conflicts    []string     `json:"conflicts,omitempty"`
	Round        int          `json:"round,omitempty"`
	Stop         string       `json:"stop,omitempty"`
	Candidate    string       `json:"candidate,omitempty"`
	Review       int          `json:"review,omitempty"`
	Verdict      *UnitVerdict `json:"verdict,omitempty"`
	Seal         int          `json:"seal,omitempty"`
	SealRevision int          `json:"seal_revision,omitempty"`
}

// move names the drift rebase and the upstream base it moves from and to.
func (r DriftRebase) move() trace.UpstreamMove {
	return trace.UpstreamMove{Drift: r.Drift, From: r.From, To: r.Upstream.Commit}
}

func driftIDs(k int) (transition, event string) {
	transition = fmt.Sprintf("%s-%d", driftSubject, k)
	return transition, trace.EventID(transition, "run")
}

// requestDrifts asks for a drift rebase of every building or assembled
// workstream of the project that is not paused and has no final review in
// flight, for which due returns why it takes one now and that has a seal,
// and returns the workstreams it asked for. Drift rebases share the project's
// lander with landings: while a landing or drift rebase of the project has
// no result, it asks for nothing.
func (f *foreman) requestDrifts(ctx context.Context, due func(config.WorkstreamID) (string, error)) ([]config.WorkstreamID, error) {
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
	state, _ := f.s.effective()
	var requested []config.WorkstreamID
	for _, stream := range streams {
		if stream == librarian || reviewing[stream] || scheduler.Paused(state.Pauses, f.cfg.Project.ID, stream) {
			continue
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if reason, err := (drifter{f}).building(stream); err != nil {
			return nil, fmt.Errorf("workstream %s drift: %w", stream, err)
		} else if reason != "" {
			continue
		}
		why, err := due(stream)
		if err != nil {
			return nil, fmt.Errorf("workstream %s drift: %w", stream, err)
		}
		if why == "" {
			continue
		}
		if _, _, found, err := seal.Latest(f.repository, stream); err != nil {
			return nil, fmt.Errorf("workstream %s drift: %w", stream, err)
		} else if !found {
			continue
		}
		if err := f.requestDrift(ctx, stream, why); err != nil {
			return nil, fmt.Errorf("workstream %s drift: %w", stream, err)
		}
		requested = append(requested, stream)
	}
	return requested, nil
}

// driftNumber returns the number of the drift rebase a value of the drift
// or drift request subject names, or 0 for the empty value.
func driftNumber(value string) (int, error) {
	if value == "" {
		return 0, nil
	}
	_, n, _ := strings.Cut(value, "-")
	var k int
	if _, err := fmt.Sscanf(n, "%d", &k); err != nil {
		return 0, fmt.Errorf("drift state %q names no drift rebase", value)
	}
	return k, nil
}

// requestDrift publishes the workstream's next drift rebase as a durable
// operation, with why it is asked for.
func (f *foreman) requestDrift(ctx context.Context, stream config.WorkstreamID, why string) error {
	state, err := f.repository.Workflow(stream, driftSubject)
	if err != nil {
		return err
	}
	k, err := driftNumber(state.Value)
	if err != nil {
		return err
	}
	k++
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
	reason := fmt.Sprintf("%s: drift rebase %d fetches %s of %s and rebases %s onto it; the seal's upstream base moves once the branch has", why, k, f.cfg.Project.BaseBranch, f.cfg.Project.Upstream, featureBranch(stream))
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
// is recorded rebased or skipped, nil before.
func (d drifter) outcome(stream config.WorkstreamID, k int) (*coreadapter.OperationResult, error) {
	transitions, err := trace.Read[trace.Transition](d.repository, stream)
	if err != nil {
		return nil, err
	}
	transition, _ := driftIDs(k)
	for _, t := range transitions {
		switch t.ID {
		case transition + "-" + driftRebased, transition + "-" + driftSkipped:
			return &coreadapter.OperationResult{Outcome: "succeeded", Evidence: t.Reason}, nil
		}
	}
	return nil, nil
}

// latest returns the latest record of drift rebase k, and whether one is.
func (d drifter) latest(stream config.WorkstreamID, k int) (DriftRebase, bool, error) {
	records, err := d.records(stream, k)
	if err != nil || len(records) == 0 {
		return DriftRebase{}, false, err
	}
	return records[len(records)-1], true, nil
}

// Inspect reads the recorded transitions, documents and the clone. A
// recorded outcome completes the operation; otherwise it is absent, with
// evidence of how far the recorded drift rebase got: a replay the branch has
// or has not moved to yet, a carry of unfinished units, a conflict
// resolution, or no replay at all. Apply continues from there without
// replaying again.
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
	r, found, err := d.latest(stream, in.Drift)
	if err != nil {
		return coreadapter.Observation{}, err
	}
	if found && resolving(r.Outcome) {
		return coreadapter.Observation{State: coreadapter.EffectAbsent, Evidence: fmt.Sprintf("drift rebase %d of %s onto %s is %s in its conflict resolution; the branch stays at %s until a reviewer approves the resolution", in.Drift, r.Before, r.Upstream.Commit, r.Outcome, r.Before)}, nil
	}
	branch := featureBranch(stream)
	tip, exists, err := featureWorkspaces(d.cfg).Branch(ctx, branch)
	if err != nil {
		return coreadapter.Observation{}, err
	}
	switch {
	case found && r.Outcome == driftCarrying:
		return coreadapter.Observation{State: coreadapter.EffectAbsent, Evidence: fmt.Sprintf("drift rebase %d moved feature branch %s from %s to %s and carries the unfinished units onto it; the outcome is recorded once they are current", in.Drift, branch, r.Before, r.Commit)}, nil
	case found && r.Outcome == driftReplayed && exists && tip == r.Commit && r.Commit != r.Before:
		return coreadapter.Observation{State: coreadapter.EffectAbsent, Evidence: fmt.Sprintf("feature branch %s is at %s, drift rebase %d's recorded replay of %s onto %s; the seal and outcome are recorded without replaying again", branch, tip, in.Drift, r.Before, r.Upstream.Commit)}, nil
	case found && r.Outcome == driftReplayed:
		return coreadapter.Observation{State: coreadapter.EffectAbsent, Evidence: fmt.Sprintf("drift rebase %d replayed %s onto %s as %s; the branch moves to it and the seal follows", in.Drift, r.Before, r.Upstream.Commit, r.Commit)}, nil
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
// seal: the conflicts are resolved and reviewed as resolve says, and the
// operation stays pending, holding the project's lander, until a reviewer
// approves the resolved branch, which is then the replay's commit. A
// workstream that stops building or being assembled while its conflicts
// are resolved is skipped, and its resolution workspace removed. A clean
// replay records drift/rebase.json with the rebased commit before
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
	rebase, found, err := d.latest(stream, in.Drift)
	if err != nil {
		return coreadapter.OperationResult{}, err
	}
	if found && rebase.Outcome == driftCarrying {
		return d.finishCarry(ctx, stream, rebase)
	}
	if found && resolving(rebase.Outcome) {
		if reason, err := d.building(stream); err != nil {
			return coreadapter.OperationResult{}, err
		} else if reason != "" {
			if err := d.release(ctx, stream); err != nil {
				return coreadapter.OperationResult{}, err
			}
			return d.skip(ctx, stream, in.Drift, op.ID, reason+"; its conflict resolution is dropped")
		}
		if rebase, err = d.resolve(ctx, stream, rebase); err != nil {
			return coreadapter.OperationResult{}, err
		}
	}
	replayed := found && rebase.Outcome == driftReplayed
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
		sealed, _, found, err := seal.Latest(d.repository, stream)
		if err != nil {
			return coreadapter.OperationResult{}, err
		}
		if !found {
			return coreadapter.OperationResult{}, fmt.Errorf("workstream %s has no seal", stream)
		}
		remote, err := g.Remote(ctx, d.cfg.Project.Upstream)
		if err != nil {
			return coreadapter.OperationResult{}, err
		}
		fetched, err := g.Fetch(ctx, remote, d.cfg.Project.BaseBranch)
		if err != nil {
			return coreadapter.OperationResult{}, fmt.Errorf("fetch %s of %s: %w", d.cfg.Project.BaseBranch, remote, err)
		}
		rebase = DriftRebase{Drift: in.Drift, Operation: op.ID, Branch: branch, Upstream: seal.Base{Remote: remote, Branch: d.cfg.Project.BaseBranch, Commit: fetched}, From: sealed.Base.Commit, Before: tip}
		if err := d.s.step("drift-replaying"); err != nil {
			return coreadapter.OperationResult{}, err
		}
		commit, conflicts, err := g.Replay(ctx, tip, fetched, requested)
		if err != nil {
			return coreadapter.OperationResult{}, err
		}
		if len(conflicts) > 0 {
			rebase.Outcome, rebase.Conflicts = driftConflicted, conflicts
			if err := d.conflict(ctx, stream, rebase); err != nil {
				return coreadapter.OperationResult{}, err
			}
			if rebase, err = d.resolve(ctx, stream, rebase); err != nil {
				return coreadapter.OperationResult{}, err
			}
		} else {
			rebase.Outcome, rebase.Commit = driftReplayed, commit
			if err := d.recordReplay(ctx, stream, rebase); err != nil {
				return coreadapter.OperationResult{}, err
			}
			if err := d.s.step("drift-replayed"); err != nil {
				return coreadapter.OperationResult{}, err
			}
		}
	}
	if !exists || (tip != rebase.Before && tip != rebase.Commit) {
		if !exists {
			tip = "nothing"
		}
		return d.skip(ctx, stream, in.Drift, op.ID, fmt.Sprintf("feature branch %s moved to %s during the drift rebase, which replayed %s", branch, tip, rebase.Before))
	}
	if rebase.Candidate != "" {
		if err := d.release(ctx, stream); err != nil {
			return coreadapter.OperationResult{}, err
		}
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
	visible := ""
	if moved != nil {
		if visible, err = d.invalidates(stream); err != nil {
			return coreadapter.OperationResult{}, err
		}
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
		if _, err := d.record(ctx, stream, rebase, moved, driftCarrying, reason, visible); err != nil {
			return coreadapter.OperationResult{}, err
		}
		return d.finishCarry(ctx, stream, rebase)
	}
	return d.record(ctx, stream, rebase, moved, driftRebased, reason, visible)
}

// invalidates returns what moving the seal's upstream base does to the
// workstream's latest final report: why it no longer authorises delivery
// when it is a completed final review on the base the seal names now, or ""
// when there is no such report.
func (d drifter) invalidates(stream config.WorkstreamID) (string, error) {
	report, found, err := latestFinalReport(d.repository, stream)
	if err != nil || !found || report.Outcome != finalReviewed {
		return "", err
	}
	if stale, err := finalReportBaseStale(d.repository, stream); err != nil || stale {
		return "", err
	}
	return fmt.Sprintf("final review %d read the feature branch on the old upstream base and no longer authorises delivery; a new final review is required", report.Review), nil
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
	return d.record(ctx, stream, rebase, nil, driftRebased, reason, "")
}

// eligible returns why the workstream takes no drift rebase now, or "" when
// it is building or assembled and not paused.
func (d drifter) eligible(stream config.WorkstreamID) (string, error) {
	if reason, err := d.building(stream); err != nil || reason != "" {
		return reason, err
	}
	state, _ := d.s.effective()
	if scheduler.Paused(state.Pauses, d.cfg.Project.ID, stream) {
		return "the workstream is paused", nil
	}
	return "", nil
}

// building returns why the workstream is neither building nor assembled, or
// "" when it is one of them.
func (d drifter) building(stream config.WorkstreamID) (string, error) {
	feature, err := d.repository.Workflow(stream, trace.FeatureSubject)
	if err != nil {
		return "", err
	}
	if feature.Value != BuildingState && feature.Value != AssembledState {
		return fmt.Sprintf("the workstream is %s, not building or assembled", featureState(feature.Value)), nil
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
// there is one, and the drift subject's move to the outcome, with an
// upstream moved event when visible says what the move does that is
// visible. An outcome already recorded returns its result and records
// nothing more.
func (d drifter) record(ctx context.Context, stream config.WorkstreamID, rebase DriftRebase, moved *trace.Document, outcome, reason, visible string) (coreadapter.OperationResult, error) {
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
	if visible != "" {
		tx.Events = []trace.Event{trace.UpstreamMoved(tx.Transition.ID, stream, rebase.move(), visible)}
	}
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
