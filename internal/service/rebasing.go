package service

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/kpenfound/osmia/internal/bundle"
	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/trace"
	"github.com/kpenfound/osmia/internal/workspace"
)

// RebaseAction is the repository-boundary operation action that rebases one
// unit's workspace onto the tip of its workstream's feature branch.
const RebaseAction = "rebase"

// rebaseSnapshotTrailer is the rebase commit message trailer that names the
// snapshot of the unit's workspace the rebase replayed.
const rebaseSnapshotTrailer = "Osmia-Snapshot"

// rebaseInput names a rebase: the unit, the number k of the unit's rebase
// and the feature branch commit its workspace is rebased onto.
type rebaseInput struct {
	Unit   string `json:"unit"`
	Rebase int    `json:"rebase"`
	Onto   string `json:"onto"`
}

// UnitRebase is the document units/<unit>/rebase.json: one rebase of a
// unit's workspace. Snapshot is the commit that held the workspace, Base the
// feature branch commit it descended from, Onto the feature branch tip it was
// rebased onto and Commit the rebased commit the workspace's branch is on.
// Conflicts lists the paths the rebase left conflicted, and Report the
// revision of units/<unit>/report.json that is current after the rebase.
type UnitRebase struct {
	Unit      string   `json:"unit"`
	Rebase    int      `json:"rebase"`
	Operation string   `json:"operation"`
	State     string   `json:"state"`
	Branch    string   `json:"branch"`
	Base      string   `json:"base"`
	Onto      string   `json:"onto"`
	Snapshot  string   `json:"snapshot"`
	Commit    string   `json:"commit"`
	Conflicts []string `json:"conflicts"`
	Report    int      `json:"report"`
}

// rebaseSubject is the workflow subject that tracks the rebases of one
// unit's workspace: requested-<k> once rebase k is asked for, then
// rebased-<k>, conflicted-<k> or refused-<k>.
func rebaseSubject(unit string) string {
	return "rebase" + strings.TrimPrefix(trace.UnitSubject(unit), "unit")
}

func rebaseDocument(unit string) string { return trace.UnitSubject(unit) + "-rebase" }

func rebaseIDs(unit string, k int) (transition, event string) {
	transition = fmt.Sprintf("%s-%d", rebaseSubject(unit), k)
	return transition, trace.EventID(transition, "run")
}

// resolveTurnID is the mason turn that resolves the conflicts rebase k left.
func resolveTurnID(unit string, k int) string {
	return fmt.Sprintf("%s-rebase-%d", masonAgent(unit), k)
}

// refresh keeps the unfinished units of a building or assembled workstream on the tip of
// its feature branch. A unit whose rebases left conflicts is routed to its
// mason first. A unit whose workspace does not descend from the tip is
// rebased once no mason turn of it is claimed or dispatched: its rebase is
// published as a durable operation. rebasing names the units with a rebase
// in flight, dispatched the thread turns an operation names and rebased the
// feature branch commits each unit's finished rebases were onto. refresh
// reports whether every unit is current, so a landing may be asked for.
func (f *foreman) refresh(ctx context.Context, b building, rebasing map[string]bool, dispatched map[string]bool, rebased map[string][]string) (bool, error) {
	units := newUnitWorkspaces(f.cfg, f.repository)
	g, err := units.of(b.stream)
	if err != nil {
		return false, err
	}
	tip, exists, err := g.Branch(ctx, featureBranch(b.stream))
	if err != nil || !exists {
		return true, err
	}
	current := true
	for _, u := range b.plan.Units {
		state := b.states[trace.UnitSubject(u.ID)]
		if state.Value == "" || state.Value == UnitPlanned || state.Value == UnitMerged {
			continue
		}
		if moved, err := f.resolve(ctx, b.stream, u.ID, state); err != nil {
			return false, fmt.Errorf("unit %s: %w", u.ID, err)
		} else if moved {
			current = false
		}
		behind, err := units.behind(ctx, b.stream, u.ID)
		if err != nil || !behind {
			if err != nil {
				return false, err
			}
			continue
		}
		if rebasing[u.ID] {
			current = false
			continue
		}
		if slices.Contains(rebased[u.ID], tip) {
			continue
		}
		current = false
		if writing, err := f.writing(b.stream, u.ID, dispatched); err != nil || writing {
			if err != nil {
				return false, err
			}
			continue
		}
		if err := f.requestRebase(ctx, b.stream, u.ID, tip); err != nil {
			return false, fmt.Errorf("unit %s rebase: %w", u.ID, err)
		}
	}
	return current, nil
}

// writing reports whether a turn of the unit's mason is claimed, running or
// interrupted with its view not yet copied back, or dispatched and not
// completed.
func (f *foreman) writing(stream config.WorkstreamID, unit string, dispatched map[string]bool) (bool, error) {
	th, err := f.repository.Thread(stream, masonAgent(unit))
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return slices.ContainsFunc(th.Turns, func(q trace.QueuedTurn) bool {
		return q.CompletedAt.IsZero() && (q.Claim != nil || dispatched[masonAgent(unit)+"/"+q.Request.TurnID])
	}), nil
}

// requestRebase publishes the next rebase of the unit's workspace onto the
// feature branch commit onto as a durable operation.
func (f *foreman) requestRebase(ctx context.Context, stream config.WorkstreamID, unit, onto string) error {
	subject := rebaseSubject(unit)
	state, err := f.repository.Workflow(stream, subject)
	if err != nil {
		return err
	}
	k := 1
	if state.Value != "" {
		_, n, _ := strings.Cut(state.Value, "-")
		if _, err := fmt.Sscanf(n, "%d", &k); err != nil {
			return fmt.Errorf("subject %s is %q", subject, state.Value)
		}
		k++
	}
	transition, event := rebaseIDs(unit, k)
	input, err := json.Marshal(rebaseInput{Unit: unit, Rebase: k, Onto: onto})
	if err != nil {
		return err
	}
	cause, err := f.landedBy(stream, onto)
	if err != nil {
		return err
	}
	op := coreadapter.Operation{ID: trace.OperationID(f.repository.Project(), stream, event), Boundary: coreadapter.RepositoryBoundary, Action: RebaseAction, Input: input}
	reason := fmt.Sprintf("feature branch %s moved to %s; unit %s's workspace on %s does not descend from it and no mason turn holds it, so the service rebases it onto %s", featureBranch(stream), onto, unit, unitBranch(stream, unit), onto)
	tx := trace.Transaction{ExpectedVersion: state.Version,
		Transition: trace.Transition{Header: f.header(transition, stream, unit, cause, f.s.now()), Subject: subject, From: state.Value, To: fmt.Sprintf("requested-%d", k), Reason: reason},
		Events:     []trace.Event{{ID: event, Kind: RebaseAction, Body: fmt.Sprintf("Rebase unit %s", unit), Operation: &op}}}
	if _, err := f.repository.Transact(ctx, tx); err != nil && !errors.Is(err, trace.ErrConflict) {
		return err
	}
	return nil
}

// landedBy returns the landing operation whose commit is the feature branch
// commit, or the feature branch when no landing made it.
func (f *foreman) landedBy(stream config.WorkstreamID, commit string) (string, error) {
	docs, err := trace.Read[trace.Document](f.repository, stream)
	if err != nil {
		return "", err
	}
	for _, d := range slices.Backward(docs) {
		var landing UnitLanding
		if strings.HasPrefix(d.Path, "units/") && strings.HasSuffix(d.Path, "/landing.json") && json.Unmarshal([]byte(d.Content), &landing) == nil && landing.Commit == commit {
			return landing.Operation, nil
		}
	}
	return featureBranch(stream), nil
}

// conflicts returns the paths the unit's rebases left conflicted since its
// latest report was recorded, sorted, and the latest of those rebases. A
// report recorded after a conflicted rebase resolves it.
func (m *masons) conflicts(stream config.WorkstreamID, unit string) ([]string, UnitRebase, error) {
	docs, err := trace.Read[trace.Document](m.repository, stream)
	if err != nil {
		return nil, UnitRebase{}, err
	}
	report := 0
	var rebases []UnitRebase
	for _, d := range docs {
		switch d.ID {
		case reportDocument(unit):
			report = max(report, d.Revision)
		case rebaseDocument(unit):
			var r UnitRebase
			if err := json.Unmarshal([]byte(d.Content), &r); err != nil {
				return nil, UnitRebase{}, fmt.Errorf("%s: %w", d.Path, err)
			}
			rebases = append(rebases, r)
		}
	}
	var paths []string
	var latest UnitRebase
	for _, r := range rebases {
		if r.Report != report || len(r.Conflicts) == 0 {
			continue
		}
		latest = r
		for _, p := range r.Conflicts {
			if !slices.Contains(paths, p) {
				paths = append(paths, p)
			}
		}
	}
	slices.Sort(paths)
	return paths, latest, nil
}

// resolve routes a unit whose rebases left conflicts to its mason: a unit
// reviewing or approved returns to implementing, and an implementing unit's
// mason gets one turn to resolve the conflicts of its latest conflicted
// rebase. A unit in any other state is left until it is in one of those.
// resolve reports whether the unit's state moved.
func (f *foreman) resolve(ctx context.Context, stream config.WorkstreamID, unit string, state trace.WorkflowState) (bool, error) {
	if state.Value != UnitImplementing && state.Value != UnitReviewing && state.Value != UnitApproved {
		return false, nil
	}
	paths, latest, err := f.conflicts(stream, unit)
	if err != nil || len(paths) == 0 {
		return false, err
	}
	if state.Value != UnitImplementing {
		subject := trace.UnitSubject(unit)
		id := fmt.Sprintf("%s-%s-rebase-%d", subject, UnitImplementing, latest.Rebase)
		transition, _ := rebaseIDs(unit, latest.Rebase)
		reason := fmt.Sprintf("unit %s returns to implementing from %s: rebasing its workspace onto %s left %s conflicted; its mason resolves the conflict markers against the sealed spec before the unit is reviewed again", unit, state.Value, latest.Onto, strings.Join(paths, ", "))
		tx := trace.Transaction{ExpectedVersion: state.Version,
			Transition: trace.Transition{Header: f.header(id, stream, unit, transition+"-conflicted", f.s.now()), Subject: subject, From: state.Value, To: UnitImplementing, Reason: reason},
			Events:     []trace.Event{trace.Notice(id, "unit", fmt.Sprintf("Unit %s returns to implementing: rebasing it onto %s left %s conflicted.", unit, latest.Onto, strings.Join(paths, ", ")))}}
		if _, err := f.repository.Transact(ctx, tx); errors.Is(err, trace.ErrConflict) {
			return true, nil
		} else if err != nil {
			return false, err
		}
	}
	return state.Value != UnitImplementing, f.enqueueResolve(ctx, stream, unit, paths, latest)
}

// enqueueResolve queues the mason turn that resolves the conflicts of a
// rebase, with the unit's bundle, which holds the sealed spec, unless the
// mason's thread has it. A unit whose spec no longer matches its seal gets
// no turn, and why is recorded as a block.
func (m *masons) enqueueResolve(ctx context.Context, stream config.WorkstreamID, unit string, paths []string, rebase UnitRebase) error {
	agent := masonAgent(unit)
	th, err := m.repository.Thread(stream, agent)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	turn := resolveTurnID(unit, rebase.Rebase)
	if slices.ContainsFunc(th.Turns, func(q trace.QueuedTurn) bool { return q.Request.TurnID == turn }) {
		return nil
	}
	transition, _ := rebaseIDs(unit, rebase.Rebase)
	cause := transition + "-conflicted"
	mason, err := m.bundle(ctx, stream, unit)
	if errors.Is(err, bundle.ErrStaleSpec) {
		return m.block(ctx, stream, unit, cause, fmt.Sprintf("unit %s's rebase conflicts reach no mason: its mason bundle cannot be assembled: %v", unit, err))
	}
	if err != nil {
		return err
	}
	profile, _, err := m.s.roleExecution(m.cfg, masonRole)
	if err != nil {
		return err
	}
	backend, err := m.repository.Workspaces(stream)
	if err != nil {
		return err
	}
	req := trace.TurnRequest{Header: trace.Header{Schema: "osmia.trace.turn-request", Version: trace.Version, ID: "request_" + turn, Revision: 1, Project: m.repository.Project(), Workstream: stream, Unit: unit, At: m.s.now(), Actor: foremanActor, Cause: cause, Depth: 1},
		AgentID: agent, ThreadID: agent, TurnID: turn, Profile: profile, SystemPrompt: masonSystemPrompt(m.cfg.Project), Prompt: resolvePrompt(unit, paths, rebase, mason, backend)}
	_, err = m.repository.EnqueueTurn(ctx, req)
	return err
}

// resolvePrompt is the prompt of the mason turn that resolves the conflicts
// a rebase of the unit's workspace, on the given backend, left in paths.
func resolvePrompt(unit string, paths []string, rebase UnitRebase, m bundle.Mason, backend string) string {
	markers := fmt.Sprintf(`A conflicted file carries conflict markers: the lines between "<<<<<<< %s" and "=======" are the feature branch's, and those between "=======" and ">>>>>>> %s" are your unit's. A file one side deleted and the other changed is kept as the side that changed it.`, rebase.Onto, rebase.Snapshot)
	if backend == config.WorkspacesJujutsu {
		markers = jujutsuMarkers("the feature branch's", "your unit's")
	}
	return fmt.Sprintf(`Resolve the conflicts of unit %s.

The workstream's feature branch moved to %s, and the service rebased your unit's workspace onto it. These files of your view are conflicted:
%s

%s Resolve every conflict against the sealed spec below, so that what the feature branch holds and what your unit builds both stand, and remove every marker. Then check that each of the unit's criteria holds and its proof is in place and passing, and call done with a report on every criterion of the unit. The service sends the unit to review only once no conflicted file carries a marker.

%s`, unit, rebase.Onto, "- "+strings.Join(paths, "\n- "), markers, m.Render())
}

// jujutsuMarkers explains the conflict markers of a file a Jujutsu rebase
// left conflicted, whose sides are onto, the side rebased onto, and change,
// the side rebased.
func jujutsuMarkers(onto, change string) string {
	return fmt.Sprintf(`A conflicted file carries conflict markers: the lines between "<<<<<<<" and "|||||||" are %s, those between "|||||||" and "=======" are what both sides started from, and those between "=======" and ">>>>>>>" are %s. A side that deleted the file holds no lines.`, onto, change)
}

// remind queues one turn of the unit's mason, after its done turn, that
// names the conflicted files still carrying markers, unless the thread has
// it.
func (m *masons) remind(ctx context.Context, stream config.WorkstreamID, unit string, done trace.QueuedTurn, marked []string) error {
	th, err := m.repository.Thread(stream, masonAgent(unit))
	if err != nil {
		return err
	}
	req := done.Request
	req.TurnID = fmt.Sprintf("%s-markers-%d", masonAgent(unit), done.Sequence)
	if slices.ContainsFunc(th.Turns, func(q trace.QueuedTurn) bool { return q.Request.TurnID == req.TurnID }) {
		return nil
	}
	req.ID = "request_" + req.TurnID
	req.At = m.s.now()
	req.Cause = done.Response.ID
	req.Prompt = fmt.Sprintf("You reported unit %s done, and these files still carry conflict markers from rebasing its workspace onto the feature branch: %s. Resolve each conflict against the sealed spec, remove every marker, check the unit's criteria and proofs, and call done again.", unit, strings.Join(marked, ", "))
	_, err = m.repository.EnqueueTurn(ctx, req)
	return err
}

// rebaser runs the rebase operations of the foreman.
type rebaser struct{ *foreman }

var _ coreadapter.Reconciler = rebaser{}

func decodeRebase(op coreadapter.Operation) (rebaseInput, error) {
	var in rebaseInput
	if op.Boundary != coreadapter.RepositoryBoundary || op.Action != RebaseAction {
		return in, fmt.Errorf("unsupported repository operation %q", op.Action)
	}
	dec := json.NewDecoder(bytes.NewReader(op.Input))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&in); err != nil {
		return in, fmt.Errorf("invalid rebase operation input: %w", err)
	}
	if in.Unit == "" || in.Rebase < 1 || in.Onto == "" {
		return in, errors.New("rebase operation requires a unit, a positive rebase number and a commit to rebase onto")
	}
	return in, nil
}

// stream returns the workstream a rebase operation belongs to: the one whose
// run event derives the operation ID.
func (r rebaser) stream(op coreadapter.Operation, in rebaseInput) (config.WorkstreamID, error) {
	streams, err := r.repository.Workstreams()
	if err != nil {
		return "", err
	}
	_, event := rebaseIDs(in.Unit, in.Rebase)
	for _, stream := range streams {
		if trace.OperationID(r.repository.Project(), stream, event) == op.ID {
			return stream, nil
		}
	}
	return "", fmt.Errorf("rebase operation %s belongs to no workstream", op.ID)
}

// outcome returns the recorded result of a rebase: succeeded once it is
// recorded rebased or conflicted, failed once its refusal is recorded, nil
// before either.
func (r rebaser) outcome(stream config.WorkstreamID, in rebaseInput) (*coreadapter.OperationResult, error) {
	transitions, err := trace.Read[trace.Transition](r.repository, stream)
	if err != nil {
		return nil, err
	}
	transition, _ := rebaseIDs(in.Unit, in.Rebase)
	for _, t := range transitions {
		switch t.ID {
		case transition + "-rebased", transition + "-conflicted":
			return &coreadapter.OperationResult{Outcome: "succeeded", Evidence: t.Reason}, nil
		case transition + "-refused":
			return &coreadapter.OperationResult{Outcome: "failed", Evidence: t.Reason}, nil
		}
	}
	return nil, nil
}

// Inspect reads the recorded transitions and the clone. A recorded outcome
// completes the operation; otherwise it is absent, with evidence of whether
// the unit branch already holds this operation's rebased commit, which Apply
// then records without snapshotting or rebasing again.
func (r rebaser) Inspect(ctx context.Context, op coreadapter.Operation) (coreadapter.Observation, error) {
	in, err := decodeRebase(op)
	if err != nil {
		return coreadapter.Observation{}, err
	}
	stream, err := r.stream(op, in)
	if err != nil {
		return coreadapter.Observation{}, err
	}
	result, err := r.outcome(stream, in)
	if err != nil {
		return coreadapter.Observation{}, err
	}
	if result != nil {
		return coreadapter.Observation{State: coreadapter.EffectCompleted, Evidence: "rebase " + result.Outcome, Result: result}, nil
	}
	branch := unitBranch(stream, in.Unit)
	g, err := newUnitWorkspaces(r.cfg, r.repository).of(stream)
	if err != nil {
		return coreadapter.Observation{}, err
	}
	tip, exists, err := g.Branch(ctx, branch)
	if err != nil {
		return coreadapter.Observation{}, err
	}
	if !exists {
		return coreadapter.Observation{State: coreadapter.EffectAbsent, Evidence: fmt.Sprintf("the clone has no unit branch %s", branch)}, nil
	}
	if snapshot, err := rebasedSnapshot(ctx, g, tip, in, op.ID); err != nil {
		return coreadapter.Observation{}, err
	} else if snapshot != "" {
		return coreadapter.Observation{State: coreadapter.EffectAbsent, Evidence: fmt.Sprintf("unit branch %s is at %s, this operation's rebase of snapshot %s onto %s; the rebase records it without rebasing again", branch, tip, snapshot, in.Onto)}, nil
	}
	return coreadapter.Observation{State: coreadapter.EffectAbsent, Evidence: fmt.Sprintf("unit branch %s is at %s and holds no rebase of this operation; the rebase checks the feature branch before snapshotting", branch, tip)}, nil
}

// rebasedSnapshot returns the snapshot the rebase operation replayed when
// head is the commit it made: its only parent is the commit the operation
// rebases onto and its message names the operation. It returns "" for any
// other commit.
func rebasedSnapshot(ctx context.Context, g workspace.Provider, head string, in rebaseInput, operation string) (string, error) {
	if head == in.Onto {
		return "", nil
	}
	c, err := g.Commit(ctx, head)
	if err != nil {
		return "", err
	}
	lines := strings.Split(c.Message, "\n")
	if !slices.Equal(c.Parents, []string{in.Onto}) || !slices.Contains(lines, landingTrailer+": "+operation) {
		return "", nil
	}
	for _, line := range lines {
		if value, ok := strings.CutPrefix(line, rebaseSnapshotTrailer+": "); ok {
			return value, nil
		}
	}
	return "", nil
}

// Apply rebases the unit's workspace onto the feature branch commit the
// operation names. A unit branch whose tip is this operation's commit on that
// commit is an interrupted rebase: its snapshot is read from the commit's
// trailer, the tip is the rebased commit, and nothing is snapshotted or
// committed again. Otherwise the
// feature branch must still be at that commit and the workspace must not
// descend from it; the workspace is snapshotted, and the change the snapshot
// holds since its base is merged onto the commit as one rebased commit,
// conflicts included: as markers on Git, stored on Jujutsu, whose workspace
// materializes them as markers. On Jujutsu the rebased commit carries the
// change ID the trace records for the unit. The workspace's branch, index and files then
// move to the rebased commit, and the rebase is recorded. A workspace that
// is missing, already current or behind a feature branch that moved again
// is refused with the reason as its result, and nothing changes. Each
// attempt runs between checkpoints of the unit workspaces' state, which a
// service restarted after the attempt was cut short restores before the
// rebase is retried.
func (r rebaser) Apply(ctx context.Context, op coreadapter.Operation) (coreadapter.OperationResult, error) {
	in, err := decodeRebase(op)
	if err != nil {
		return coreadapter.OperationResult{}, err
	}
	stream, err := r.stream(op, in)
	if err != nil {
		return coreadapter.OperationResult{}, err
	}
	if result, err := r.outcome(stream, in); err != nil || result != nil {
		if err != nil {
			return coreadapter.OperationResult{}, err
		}
		return *result, nil
	}
	g, err := newUnitWorkspaces(r.cfg, r.repository).of(stream)
	if err != nil {
		return coreadapter.OperationResult{}, err
	}
	requested, err := r.requestedAt(stream, op.ID)
	if err != nil {
		return coreadapter.OperationResult{}, err
	}
	return checkpointed(ctx, op.ID, requested, []workspace.Provider{g}, func() (coreadapter.OperationResult, error) {
		return r.rebase(ctx, op.ID, stream, in, g, requested)
	})
}

// rebase is one attempt of the rebase Apply describes, on the unit
// workspaces g.
func (r rebaser) rebase(ctx context.Context, operation string, stream config.WorkstreamID, in rebaseInput, g workspace.Provider, requested time.Time) (coreadapter.OperationResult, error) {
	w, found, err := g.Workspace(ctx, unitName(stream, in.Unit))
	if err != nil {
		return coreadapter.OperationResult{}, err
	}
	if !found {
		return r.refuse(ctx, stream, in, fmt.Sprintf("unit %s has no workspace", in.Unit))
	}
	head, _, err := g.Branch(ctx, w.Branch)
	if err != nil {
		return coreadapter.OperationResult{}, err
	}
	snapshot, err := rebasedSnapshot(ctx, g, head, in, operation)
	if err != nil {
		return coreadapter.OperationResult{}, err
	}
	rebased := ""
	if snapshot != "" {
		rebased = head
	} else {
		tip, _, err := g.Branch(ctx, featureBranch(stream))
		if err != nil {
			return coreadapter.OperationResult{}, err
		}
		if tip != in.Onto {
			return r.refuse(ctx, stream, in, fmt.Sprintf("feature branch %s moved on to %s", featureBranch(stream), tip))
		}
		if current, err := g.Ancestor(ctx, in.Onto, head); err != nil {
			return coreadapter.OperationResult{}, err
		} else if current {
			return r.refuse(ctx, stream, in, fmt.Sprintf("unit %s's workspace already descends from %s", in.Unit, in.Onto))
		}
		base, err := g.MergeBase(ctx, in.Onto, head)
		if err != nil {
			return coreadapter.OperationResult{}, err
		}
		if err := r.s.step("rebase-snapshotting"); err != nil {
			return coreadapter.OperationResult{}, err
		}
		if snapshot, err = g.Snapshot(ctx, w, base); err != nil {
			return coreadapter.OperationResult{}, err
		}
		if err := r.s.step("rebase-snapshotted"); err != nil {
			return coreadapter.OperationResult{}, err
		}
	}
	base, err := g.MergeBase(ctx, in.Onto, snapshot)
	if err != nil {
		return coreadapter.OperationResult{}, err
	}
	driftBase := ""
	if drift, found, err := carryDrift(r.repository, stream, in.Onto); err != nil {
		return coreadapter.OperationResult{}, err
	} else if found {
		if descends, err := g.Ancestor(ctx, drift.Before, snapshot); err != nil {
			return coreadapter.OperationResult{}, err
		} else if descends {
			base = drift.Before
			driftBase = base
		}
	}
	message := fmt.Sprintf("Rebase unit %s onto %s\n\nOsmia-Workstream: %s\nOsmia-Unit: %s\n%s: %s\nOsmia-Base: %s\nOsmia-Onto: %s\n%s: %s\n", in.Unit, in.Onto, stream, in.Unit, rebaseSnapshotTrailer, snapshot, base, in.Onto, landingTrailer, operation)
	change, err := unitChange(r.repository, stream, in.Unit)
	if err != nil {
		return coreadapter.OperationResult{}, err
	}
	var commit string
	var conflicts []string
	if rebased != "" {
		commit = rebased
		conflicts, err = rebasedConflicts(ctx, g, driftBase, in.Onto, snapshot, rebased, message, change, requested)
	} else {
		commit, conflicts, err = rebaseCarrying(ctx, g, driftBase, in.Onto, snapshot, message, change, requested)
	}
	if err != nil {
		return coreadapter.OperationResult{}, err
	}
	if err := g.Move(ctx, w, snapshot, commit); err != nil {
		return coreadapter.OperationResult{}, err
	}
	if err := r.s.step("rebase-moved"); err != nil {
		return coreadapter.OperationResult{}, err
	}
	return r.record(ctx, stream, in, operation, UnitRebase{Unit: in.Unit, Rebase: in.Rebase, Operation: operation, Branch: w.Branch, Base: base, Onto: in.Onto, Snapshot: snapshot, Commit: commit, Conflicts: conflicts})
}

// rebaseCarrying rebases snapshot as RebaseFrom does and returns the rebased
// commit carrying the unit's change ID, when the unit has one, with the
// paths the rebase left conflicted.
func rebaseCarrying(ctx context.Context, g workspace.Provider, base, onto, snapshot, message, change string, at time.Time) (string, []string, error) {
	commit, conflicts, err := g.RebaseFrom(ctx, base, onto, snapshot, message, at)
	if err != nil || change == "" {
		return commit, conflicts, err
	}
	commit, err = g.Carry(ctx, commit, change)
	return commit, conflicts, err
}

// rebasedConflicts returns the paths the rebase of snapshot that made commit
// rebased left conflicted. A commit that holds stored conflicts holds those
// paths, and one Jujutsu made from a snapshot that held stored conflicts
// holds no other. Any other commit is the one Git's merge makes from the
// rebase's arguments, carrying the unit's change ID when it has one, which
// is made again to read its conflicted paths.
func rebasedConflicts(ctx context.Context, g workspace.Provider, base, onto, snapshot, rebased, message, change string, at time.Time) ([]string, error) {
	stored, err := g.StoredConflicts(ctx, rebased)
	if err != nil || len(stored) != 0 {
		return stored, err
	}
	if held, err := g.StoredConflicts(ctx, snapshot); err != nil || len(held) != 0 {
		return nil, err
	}
	commit, conflicts, err := rebaseCarrying(ctx, g, base, onto, snapshot, message, change, at)
	if err != nil {
		return nil, err
	}
	if commit != rebased {
		return nil, fmt.Errorf("the rebase of snapshot %s onto %s makes %s, not the unit branch's %s", snapshot, onto, commit, rebased)
	}
	return conflicts, nil
}

// record records, in one commit, units/<unit>/rebase.json and the rebase's
// outcome. A clean rebase of the unit's recorded candidate also records the
// next report revision, naming the rebased candidate and the new base; an
// approved unit returns to reviewing, and a reviewing unit's review starts
// again. A conflicted rebase tells the chief of staff; the next pass routes
// the conflicts to the unit's mason. When a drift rebase moved the feature
// branch the unit is rebased onto, an approval sent back to review and a
// conflict each also raise an upstream moved event. A rebase already
// recorded returns its result and records nothing more.
func (r rebaser) record(ctx context.Context, stream config.WorkstreamID, in rebaseInput, operation string, rebase UnitRebase) (coreadapter.OperationResult, error) {
	subject := trace.UnitSubject(in.Unit)
	state, err := r.repository.Workflow(stream, subject)
	if err != nil {
		return coreadapter.OperationResult{}, err
	}
	docs, err := trace.Read[trace.Document](r.repository, stream)
	if err != nil {
		return coreadapter.OperationResult{}, err
	}
	var report, previous trace.Document
	for _, d := range docs {
		switch d.ID {
		case reportDocument(in.Unit):
			report = d
		case rebaseDocument(in.Unit):
			previous = d
		}
	}
	move, drifted, err := carriedOnto(r.repository, stream, in.Onto)
	if err != nil {
		return coreadapter.OperationResult{}, err
	}
	at := r.s.now()
	rebase.State, rebase.Report = state.Value, report.Revision
	if rebase.Conflicts == nil {
		rebase.Conflicts = []string{}
	}
	var records []trace.Document
	var txs []trace.Transaction
	transition, _ := rebaseIDs(in.Unit, in.Rebase)
	var recorded UnitReport
	if len(rebase.Conflicts) == 0 && report.Revision > 0 && json.Unmarshal([]byte(report.Content), &recorded) == nil && recorded.Candidate == rebase.Snapshot {
		recorded.Base, recorded.Candidate = rebase.Onto, rebase.Commit
		content, err := json.MarshalIndent(recorded, "", "  ")
		if err != nil {
			return coreadapter.OperationResult{}, err
		}
		h := report.Header
		h.Revision++
		h.At, h.Actor, h.Cause = at, foremanActor, operation
		records = append(records, trace.Document{Header: h, Path: report.Path, Content: string(content) + "\n"})
		rebase.Report = h.Revision
		if state.Value == UnitApproved || state.Value == UnitReviewing {
			id := fmt.Sprintf("%s-%s-rebase-%d", subject, UnitReviewing, in.Rebase)
			reason := fmt.Sprintf("unit %s's candidate %s from %s was rebased onto %s as candidate %s, recorded in %s revision %d; the rebased candidate is reviewed before it lands", in.Unit, rebase.Snapshot, rebase.Base, rebase.Onto, rebase.Commit, report.Path, h.Revision)
			var events []trace.Event
			if state.Value == UnitApproved {
				reason = "the approval no longer holds: " + reason
				events = append(events, trace.Notice(id, "unit", fmt.Sprintf("Unit %s returns to review: its approved candidate was rebased onto %s.", in.Unit, rebase.Onto)))
				if drifted {
					events = append(events, trace.UpstreamMoved(id, stream, move, fmt.Sprintf("unit %s's approval no longer holds: its candidate was rebased onto the feature branch at %s and returns to review", in.Unit, rebase.Onto)))
				}
			}
			txs = append(txs, trace.Transaction{ExpectedVersion: state.Version,
				Transition: trace.Transition{Header: r.header(id, stream, in.Unit, operation, at), Subject: subject, From: state.Value, To: UnitReviewing, Reason: reason}, Events: events})
		}
	}
	content, err := json.MarshalIndent(rebase, "", "  ")
	if err != nil {
		return coreadapter.OperationResult{}, err
	}
	records = append(records, trace.Document{Header: trace.Header{Schema: "osmia.trace.document", Version: trace.Version, ID: rebaseDocument(in.Unit), Revision: previous.Revision + 1, Project: r.repository.Project(), Workstream: stream, Unit: in.Unit, At: at, Actor: foremanActor, Cause: operation},
		Path: fmt.Sprintf("units/%s/rebase.json", in.Unit), Content: string(content) + "\n"})
	outcome, to := "-rebased", fmt.Sprintf("rebased-%d", in.Rebase)
	reason := fmt.Sprintf("unit %s's workspace on %s was rebased from %s onto %s: snapshot %s is now %s", in.Unit, rebase.Branch, rebase.Base, rebase.Onto, rebase.Snapshot, rebase.Commit)
	var events []trace.Event
	if len(rebase.Conflicts) != 0 {
		outcome, to = "-conflicted", fmt.Sprintf("conflicted-%d", in.Rebase)
		reason += "; conflicted: " + strings.Join(rebase.Conflicts, ", ")
		events = append(events, trace.Notice(transition+outcome, "chief", fmt.Sprintf("Unit %s conflicts with feature branch %s at %s in %s. Its mason resolves the conflict markers against the sealed spec; the unit is reviewed again before it lands.", in.Unit, featureBranch(stream), rebase.Onto, strings.Join(rebase.Conflicts, ", "))))
		if drifted {
			events = append(events, trace.UpstreamMoved(transition+outcome, stream, move, fmt.Sprintf("unit %s conflicts with the feature branch at %s in %s; its mason resolves the conflicts against the sealed spec, and the unit is reviewed again before it lands", in.Unit, rebase.Onto, strings.Join(rebase.Conflicts, ", "))))
		}
	}
	rebaseState, err := r.repository.Workflow(stream, rebaseSubject(in.Unit))
	if err != nil {
		return coreadapter.OperationResult{}, err
	}
	txs = append(txs, trace.Transaction{ExpectedVersion: rebaseState.Version,
		Transition: trace.Transition{Header: r.header(transition+outcome, stream, in.Unit, operation, at), Subject: rebaseSubject(in.Unit), From: rebaseState.Value, To: to, Reason: reason}, Events: events})
	if _, err := r.repository.RecordDocumentsWith(ctx, records, txs...); err != nil {
		if errors.Is(err, trace.ErrConflict) {
			if done, outcomeErr := r.outcome(stream, in); outcomeErr == nil && done != nil {
				return *done, nil
			}
		}
		return coreadapter.OperationResult{}, err
	}
	if err := r.s.step("rebase-recorded"); err != nil {
		return coreadapter.OperationResult{}, err
	}
	return coreadapter.OperationResult{Outcome: "succeeded", Evidence: reason}, nil
}

// refuse records why the rebase was refused and returns the refusal as the
// operation's result. The workspace is unchanged.
func (r rebaser) refuse(ctx context.Context, stream config.WorkstreamID, in rebaseInput, reason string) (coreadapter.OperationResult, error) {
	transition, _ := rebaseIDs(in.Unit, in.Rebase)
	subject := rebaseSubject(in.Unit)
	state, err := r.repository.Workflow(stream, subject)
	if err != nil {
		return coreadapter.OperationResult{}, err
	}
	recorded := fmt.Sprintf("unit %s was not rebased onto %s: %s", in.Unit, in.Onto, reason)
	tx := trace.Transaction{ExpectedVersion: state.Version,
		Transition: trace.Transition{Header: r.header(transition+"-refused", stream, in.Unit, transition, r.s.now()), Subject: subject, From: state.Value, To: fmt.Sprintf("refused-%d", in.Rebase), Reason: recorded}}
	if _, err := r.repository.Transact(ctx, tx); err != nil {
		return coreadapter.OperationResult{}, err
	}
	return coreadapter.OperationResult{Outcome: "failed", Evidence: recorded}, nil
}
