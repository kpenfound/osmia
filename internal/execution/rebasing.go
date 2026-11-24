package execution

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
	"github.com/kpenfound/osmia/internal/router"
	"github.com/kpenfound/osmia/internal/trace"
	"github.com/kpenfound/osmia/internal/workspace"
)

// RebaseAction is the repository-boundary operation action that rebases one
// unit's workspace onto the tip of its workstream's feature branch.
const RebaseAction = "rebase"

const rebaseSnapshotTrailer = "Osmia-Snapshot"

type RebaseInput struct {
	Unit   string `json:"unit"`
	Rebase int    `json:"rebase"`
	Onto   string `json:"onto"`
}

func RebaseSubject(unit string) string {
	return "rebase" + strings.TrimPrefix(trace.UnitSubject(unit), "unit")
}

func RebaseDocument(unit string) string { return trace.UnitSubject(unit) + "-rebase" }

func RebaseIDs(unit string, k int) (transition, event string) {
	transition = fmt.Sprintf("%s-%d", RebaseSubject(unit), k)
	return transition, trace.EventID(transition, "run")
}

func ResolveTurnID(unit string, k int) string {
	return fmt.Sprintf("%s-rebase-%d", masonAgent(unit), k)
}

func (f *foreman) Refresh(ctx context.Context, b building, rebasing map[string]bool, dispatched map[string]bool, rebased map[string][]string) (bool, error) {
	units := NewUnitWorkspaces(f.cfg, f.repository)
	g, err := units.Of(b.stream)
	if err != nil {
		return false, err
	}
	tip, exists, err := g.Branch(ctx, featureBranch(b.stream))
	if err != nil || !exists {
		return true, err
	}
	current := true
	var refusals refusedRebases
	for _, u := range b.plan.Units {
		state := b.states[trace.UnitSubject(u.ID)]
		if state.Value == "" || state.Value == UnitPlanned || state.Value == UnitMerged {
			continue
		}
		if moved, err := f.Resolve(ctx, b.stream, u.ID, state); err != nil {
			return false, fmt.Errorf("unit %s: %w", u.ID, err)
		} else if moved {
			current = false
		}
		behind, err := units.Behind(ctx, b.stream, u.ID)
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
		if refusals == nil {
			if refusals, err = f.refusedRebases(b); err != nil {
				return false, err
			}
		}
		if onTip := refusals.onto(u.ID, tip); len(onTip) >= MaxRefusedRebases {
			if state.Value != UnitContested {
				if err := f.contestRefusedRebases(ctx, b.stream, u.ID, state, tip, onTip); err != nil {
					return false, fmt.Errorf("unit %s: %w", u.ID, err)
				}
			}
			continue
		}
		current = false
		if writing, err := f.Writing(b.stream, u.ID, dispatched); err != nil || writing {
			if err != nil {
				return false, err
			}
			continue
		}
		if err := f.RequestRebase(ctx, b.stream, u.ID, tip); err != nil {
			return false, fmt.Errorf("unit %s rebase: %w", u.ID, err)
		}
	}
	return current, nil
}

const MaxRefusedRebases = 3

type refusedRebase struct {
	RebaseInput
	reason string
}

type refusedRebases map[string][]refusedRebase

func (r refusedRebases) onto(unit, tip string) []refusedRebase {
	var onTip []refusedRebase
	for _, in := range r[unit] {
		if in.Onto == tip {
			onTip = append(onTip, in)
		}
	}
	return onTip
}

func (f *foreman) refusedRebases(b building) (refusedRebases, error) {
	units := map[string]string{}
	for _, u := range b.plan.Units {
		units[trace.UnitSubject(u.ID)], units[RebaseSubject(u.ID)] = u.ID, u.ID
	}
	transitions, err := trace.Read[trace.Transition](f.repository, b.stream)
	if err != nil {
		return nil, err
	}
	asked, since := map[string]int{}, map[string]int{}
	for _, t := range transitions {
		unit, ok := units[t.Subject]
		if !ok {
			continue
		}
		if t.Subject == RebaseSubject(unit) {
			var k int
			if _, err := fmt.Sscanf(t.To, "requested-%d", &k); err == nil {
				asked[unit] = max(asked[unit], k)
			}
		} else if t.From == UnitContested && t.To != UnitContested {
			since[unit] = asked[unit]
		}
	}
	ops, err := f.repository.Operations(b.stream)
	if err != nil {
		return nil, err
	}
	refused := refusedRebases{}
	for _, o := range ops {
		if o.Operation.Action != RebaseAction || o.Result == nil || o.Result.Outcome != "failed" {
			continue
		}
		in, err := DecodeRebase(o.Operation)
		if err != nil {
			return nil, err
		}
		if in.Rebase > since[in.Unit] {
			refused[in.Unit] = append(refused[in.Unit], refusedRebase{in, o.Result.Evidence})
		}
	}
	for _, r := range refused {
		slices.SortFunc(r, func(a, b refusedRebase) int { return a.Rebase - b.Rebase })
	}
	return refused, nil
}

func (f *foreman) contestRefusedRebases(ctx context.Context, stream config.WorkstreamID, unit string, state trace.WorkflowState, tip string, refused []refusedRebase) error {
	last := refused[len(refused)-1]
	transition, _ := RebaseIDs(unit, last.Rebase)
	id := fmt.Sprintf("%s-%s-rebase-%d", trace.UnitSubject(unit), UnitContested, last.Rebase)
	reason := fmt.Sprintf("the service refused %d rebases of unit %s's workspace onto feature branch %s at %s, the latest because %s; the unit is contested instead of a rebase being asked for again, and a ruling or move that takes it out of the contest gets it a fresh rebase", len(refused), unit, featureBranch(stream), tip, last.reason)
	_, err := f.s.raise(ctx, true, UnitExitInput{
		Repo: f.repository, Stream: stream, Unit: unit, State: state, Trigger: TriggerRebaseRefused, Cause: transition + "-refused",
		Default: UnitContested, Targets: []string{UnitContested}, Actor: foremanActor,
		ID: id, HeaderCause: transition + "-refused", Reason: reason,
		Events: []trace.Event{trace.Notice(id, "unit", fmt.Sprintf("Unit %s is contested: %s.", unit, reason))},
		Note:   reason,
		Evidence: router.Evidence{Facts: map[string]any{"refused_rebases": len(refused), "max_refused_rebases": MaxRefusedRebases},
			Records: []router.Entry{{Kind: "rebase", ID: transition, Text: last.reason}}},
	})
	return err
}

func (f *foreman) Writing(stream config.WorkstreamID, unit string, dispatched map[string]bool) (bool, error) {
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

func (f *foreman) RequestRebase(ctx context.Context, stream config.WorkstreamID, unit, onto string) error {
	subject := RebaseSubject(unit)
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
	transition, event := RebaseIDs(unit, k)
	input, err := json.Marshal(RebaseInput{Unit: unit, Rebase: k, Onto: onto})
	if err != nil {
		return err
	}
	cause, err := f.landedBy(stream, onto)
	if err != nil {
		return err
	}
	op := coreadapter.Operation{ID: trace.OperationID(f.repository.Project(), stream, event), Boundary: coreadapter.RepositoryBoundary, Action: RebaseAction, Input: input}
	reason := fmt.Sprintf("feature branch %s moved to %s; unit %s's workspace on %s does not descend from it and no mason turn holds it, so the service rebases it onto %s", featureBranch(stream), onto, unit, UnitBranch(stream, unit), onto)
	tx := trace.Transaction{ExpectedVersion: state.Version,
		Transition: trace.Transition{Header: f.Header(transition, stream, unit, cause, f.s.Now()), Subject: subject, From: state.Value, To: fmt.Sprintf("requested-%d", k), Reason: reason},
		Events:     []trace.Event{{ID: event, Kind: RebaseAction, Body: fmt.Sprintf("Rebase unit %s", unit), Operation: &op}}}
	if _, err := f.repository.Transact(ctx, tx); err != nil && !errors.Is(err, trace.ErrConflict) {
		return err
	}
	return nil
}

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

func (m *masons) Conflicts(stream config.WorkstreamID, unit string) ([]string, UnitRebase, error) {
	docs, err := trace.Read[trace.Document](m.repository, stream)
	if err != nil {
		return nil, UnitRebase{}, err
	}
	report := 0
	var rebases []UnitRebase
	for _, d := range docs {
		switch d.ID {
		case ReportDocument(unit):
			report = max(report, d.Revision)
		case RebaseDocument(unit):
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

func (f *foreman) Resolve(ctx context.Context, stream config.WorkstreamID, unit string, state trace.WorkflowState) (bool, error) {
	if state.Value != UnitImplementing && state.Value != UnitChecking && state.Value != UnitReviewing && state.Value != UnitApproved {
		return false, nil
	}
	paths, latest, err := f.Conflicts(stream, unit)
	if err != nil || len(paths) == 0 {
		return false, err
	}
	if state.Value != UnitImplementing {
		subject := trace.UnitSubject(unit)
		id := fmt.Sprintf("%s-%s-rebase-%d", subject, UnitImplementing, latest.Rebase)
		transition, event := RebaseIDs(unit, latest.Rebase)
		reason := fmt.Sprintf("unit %s returns to implementing from %s: rebasing its workspace onto %s left %s conflicted; its mason resolves the conflict markers against the sealed spec before the unit is reviewed again", unit, state.Value, latest.Onto, strings.Join(paths, ", "))
		res, err := f.s.raise(ctx, true, UnitExitInput{
			Repo: f.repository, Stream: stream, Unit: unit, State: state, Trigger: TriggerRebaseConflicted, Cause: transition + "-conflicted",
			Default: UnitImplementing, Targets: []string{UnitImplementing}, Actor: foremanActor,
			Operation: trace.OperationID(f.repository.Project(), stream, event),
			ID:        id, HeaderCause: transition + "-conflicted", Reason: reason,
			Events: []trace.Event{trace.Notice(id, "unit", fmt.Sprintf("Unit %s returns to implementing: rebasing it onto %s left %s conflicted.", unit, latest.Onto, strings.Join(paths, ", ")))},
		})
		if err != nil {
			return true, err
		}
		if res.Committed && res.Target == UnitImplementing {
			return true, f.enqueueResolve(ctx, stream, unit, paths, latest)
		}
		return true, nil
	}
	return state.Value != UnitImplementing, f.enqueueResolve(ctx, stream, unit, paths, latest)
}

func (m *masons) enqueueResolve(ctx context.Context, stream config.WorkstreamID, unit string, paths []string, rebase UnitRebase) error {
	agent := masonAgent(unit)
	th, err := m.repository.Thread(stream, agent)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	turn := ResolveTurnID(unit, rebase.Rebase)
	if slices.ContainsFunc(th.Turns, func(q trace.QueuedTurn) bool { return q.Request.TurnID == turn }) {
		return nil
	}
	transition, _ := RebaseIDs(unit, rebase.Rebase)
	cause := transition + "-conflicted"
	mason, err := m.Bundle(ctx, stream, unit)
	if errors.Is(err, bundle.ErrStaleSpec) {
		return m.block(ctx, stream, unit, cause, fmt.Sprintf("unit %s's rebase conflicts reach no mason: its mason bundle cannot be assembled: %v", unit, err))
	}
	if err != nil {
		return err
	}
	profile, _, err := m.s.RoleExecution(m.cfg, masonRole)
	if err != nil {
		return err
	}
	backend, err := m.repository.Workspaces(stream)
	if err != nil {
		return err
	}
	req := trace.TurnRequest{Header: trace.Header{Schema: "osmia.trace.turn-request", Version: trace.Version, ID: "request_" + turn, Revision: 1, Project: m.repository.Project(), Workstream: stream, Unit: unit, At: m.s.Now(), Actor: foremanActor, Cause: cause, Depth: 1},
		AgentID: agent, ThreadID: agent, TurnID: turn, Profile: profile, SystemPrompt: masonSystemPrompt(m.cfg.Project), Prompt: ResolvePrompt(unit, paths, rebase, mason, backend)}
	_, err = m.repository.EnqueueTurn(ctx, req)
	return err
}

func ResolvePrompt(unit string, paths []string, rebase UnitRebase, m bundle.Mason, backend string) string {
	markers := fmt.Sprintf(`A conflicted file carries conflict markers: the lines between "<<<<<<< %s" and "=======" are the feature branch's, and those between "=======" and ">>>>>>> %s" are your unit's. A file one side deleted and the other changed is kept as the side that changed it.`, rebase.Onto, rebase.Snapshot)
	if backend == config.WorkspacesJujutsu {
		markers = JujutsuMarkers("the feature branch's", "your unit's")
	}
	return fmt.Sprintf(`Resolve the conflicts of unit %s.

The workstream's feature branch moved to %s, and the service rebased your unit's workspace onto it. These files of your view are conflicted:
%s

%s Resolve every conflict against the sealed spec below, so that what the feature branch holds and what your unit builds both stand, and remove every marker. Then check that the unit's acceptance still holds, and call done with the outcome. The service sends the unit to review only once no conflicted file carries a marker.

%s`, unit, rebase.Onto, "- "+strings.Join(paths, "\n- "), markers, m.Render())
}

func JujutsuMarkers(onto, change string) string {
	return fmt.Sprintf(`A conflicted file carries conflict markers: the lines between "<<<<<<<" and "|||||||" are %s, those between "|||||||" and "=======" are what both sides started from, and those between "=======" and ">>>>>>>" are %s. A side that deleted the file holds no lines.`, onto, change)
}

func (m *masons) remind(ctx context.Context, stream config.WorkstreamID, unit string, state trace.WorkflowState, done trace.QueuedTurn, marked []string) (bool, error) {
	th, err := m.repository.Thread(stream, masonAgent(unit))
	if err != nil {
		return false, err
	}
	req := done.Request
	req.TurnID = fmt.Sprintf("%s-markers-%d", masonAgent(unit), done.Sequence)
	if slices.ContainsFunc(th.Turns, func(q trace.QueuedTurn) bool { return q.Request.TurnID == req.TurnID }) {
		return false, nil
	}
	var reset uint64
	if ruling, found, err := LatestMasonRuling(m.repository, stream, unit); err != nil {
		return false, err
	} else if found {
		reset = ruling.ResetTurn
	}
	reminded := 0
	for _, q := range th.Turns {
		if q.Sequence > reset && strings.HasPrefix(q.Request.TurnID, masonAgent(unit)+"-markers-") {
			reminded++
		}
	}
	if reminded >= m.cfg.Mason.MaxCleanTurns {
		id := trace.EventID(done.Response.ID, "contested")
		reason := fmt.Sprintf("the mason reported done %d times with conflict markers left in %s, exhausting mason.max_clean_turns (%d)", reminded+1, strings.Join(marked, ", "), m.cfg.Mason.MaxCleanTurns)
		res, err := m.s.raise(ctx, true, UnitExitInput{
			Repo: m.repository, Stream: stream, Unit: unit, State: state, Trigger: TriggerMarkersLeft, Cause: done.Response.ID,
			Default: UnitContested, Targets: []string{UnitContested, UnitImplementing}, Actor: masonActor,
			ID: id, HeaderCause: done.Response.ID, Reason: reason, Events: []trace.Event{trace.Notice(id, "unit", fmt.Sprintf("Unit %s is contested: %s.", unit, reason))},
			Note: reason,
			Evidence: router.Evidence{Facts: map[string]any{"reminders": reminded, "max_clean_turns": m.cfg.Mason.MaxCleanTurns},
				Records: []router.Entry{{Kind: "mason turn", ID: done.Request.TurnID, Text: reason}}},
		})
		return err == nil && (res.Committed || res.Handed), err
	}
	req.ID = "request_" + req.TurnID
	req.At = m.s.Now()
	req.Cause = done.Response.ID
	req.Prompt = fmt.Sprintf("You reported unit %s done, and these files still carry conflict markers from rebasing its workspace onto the feature branch: %s. Resolve each conflict against the sealed spec, remove every marker, check the unit's acceptance, and call done again.", unit, strings.Join(marked, ", "))
	_, err = m.repository.EnqueueTurn(ctx, req)
	return false, err
}

type rebaser struct{ *foreman }

var _ coreadapter.Reconciler = rebaser{}

func DecodeRebase(op coreadapter.Operation) (RebaseInput, error) {
	var in RebaseInput
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

func (r rebaser) stream(op coreadapter.Operation, in RebaseInput) (config.WorkstreamID, error) {
	streams, err := r.repository.Workstreams()
	if err != nil {
		return "", err
	}
	_, event := RebaseIDs(in.Unit, in.Rebase)
	for _, stream := range streams {
		if trace.OperationID(r.repository.Project(), stream, event) == op.ID {
			return stream, nil
		}
	}
	return "", fmt.Errorf("rebase operation %s belongs to no workstream", op.ID)
}

func (r rebaser) outcome(stream config.WorkstreamID, in RebaseInput) (*coreadapter.OperationResult, error) {
	transitions, err := trace.Read[trace.Transition](r.repository, stream)
	if err != nil {
		return nil, err
	}
	transition, _ := RebaseIDs(in.Unit, in.Rebase)
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

func (r rebaser) Inspect(ctx context.Context, op coreadapter.Operation) (coreadapter.Observation, error) {
	in, err := DecodeRebase(op)
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
	g, err := NewUnitWorkspaces(r.cfg, r.repository).Of(stream)
	if err != nil {
		return coreadapter.Observation{}, err
	}
	var observed coreadapter.Observation
	err = workspace.Quiesced(ctx, []workspace.Provider{g}, func(ctx context.Context) error {
		var err error
		observed, err = r.observeRebase(ctx, g, UnitBranch(stream, in.Unit), in, op.ID)
		return err
	})
	if err != nil {
		return coreadapter.Observation{}, err
	}
	return observed, nil
}

func (r rebaser) observeRebase(ctx context.Context, g workspace.Provider, branch string, in RebaseInput, operation string) (coreadapter.Observation, error) {
	tip, exists, err := g.Branch(ctx, branch)
	if err != nil {
		return coreadapter.Observation{}, err
	}
	if !exists {
		return coreadapter.Observation{State: coreadapter.EffectAbsent, Evidence: fmt.Sprintf("the clone has no unit branch %s", branch)}, nil
	}
	if snapshot, err := rebasedSnapshot(ctx, g, tip, in, operation); err != nil {
		return coreadapter.Observation{}, err
	} else if snapshot != "" {
		return coreadapter.Observation{State: coreadapter.EffectAbsent, Evidence: fmt.Sprintf("unit branch %s is at %s, this operation's rebase of snapshot %s onto %s; the rebase records it without rebasing again", branch, tip, snapshot, in.Onto)}, nil
	}
	return coreadapter.Observation{State: coreadapter.EffectAbsent, Evidence: fmt.Sprintf("unit branch %s is at %s and holds no rebase of this operation; the rebase checks the feature branch before snapshotting", branch, tip)}, nil
}

func rebasedSnapshot(ctx context.Context, g workspace.Provider, head string, in RebaseInput, operation string) (string, error) {
	if head == in.Onto {
		return "", nil
	}
	c, err := g.Commit(ctx, head)
	if err != nil {
		return "", err
	}
	lines := strings.Split(c.Message, "\n")
	if !slices.Equal(c.Parents, []string{in.Onto}) || !slices.Contains(lines, LandingTrailer+": "+operation) {
		return "", nil
	}
	for _, line := range lines {
		if value, ok := strings.CutPrefix(line, rebaseSnapshotTrailer+": "); ok {
			return value, nil
		}
	}
	return "", nil
}

func (r rebaser) Apply(ctx context.Context, op coreadapter.Operation) (coreadapter.OperationResult, error) {
	in, err := DecodeRebase(op)
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
	g, err := NewUnitWorkspaces(r.cfg, r.repository).Of(stream)
	if err != nil {
		return coreadapter.OperationResult{}, err
	}
	requested, err := r.RequestedAt(stream, op.ID)
	if err != nil {
		return coreadapter.OperationResult{}, err
	}
	return checkpointed(ctx, string(stream)+"/"+op.ID, requested, []workspace.Provider{g}, func(ctx context.Context) (coreadapter.OperationResult, error) {
		return r.rebase(ctx, op.ID, stream, in, g, requested, recording{})
	})
}

func (r rebaser) rebase(ctx context.Context, operation string, stream config.WorkstreamID, in RebaseInput, g workspace.Provider, requested time.Time, rec recording) (coreadapter.OperationResult, error) {
	w, found, err := g.Workspace(ctx, UnitName(stream, in.Unit))
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
	} else if rec.existing {
		return coreadapter.OperationResult{}, errNothingToRecord
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
		if err := r.s.Step("rebase-snapshotting"); err != nil {
			return coreadapter.OperationResult{}, err
		}
		if snapshot, err = g.Snapshot(ctx, w, base); err != nil {
			return coreadapter.OperationResult{}, err
		}
		if err := r.s.Step("rebase-snapshotted"); err != nil {
			return coreadapter.OperationResult{}, err
		}
	}
	base, err := g.MergeBase(ctx, in.Onto, snapshot)
	if err != nil {
		return coreadapter.OperationResult{}, err
	}
	driftBase := ""
	if drift, found, err := r.s.CarryBase(r.repository, stream, in.Onto); err != nil {
		return coreadapter.OperationResult{}, err
	} else if found {
		if descends, err := g.Ancestor(ctx, drift, snapshot); err != nil {
			return coreadapter.OperationResult{}, err
		} else if descends {
			base = drift
			driftBase = base
		}
	}
	message := fmt.Sprintf("Rebase unit %s onto %s\n\nOsmia-Workstream: %s\nOsmia-Unit: %s\n%s: %s\nOsmia-Base: %s\nOsmia-Onto: %s\n%s: %s\n", in.Unit, in.Onto, stream, in.Unit, rebaseSnapshotTrailer, snapshot, base, in.Onto, LandingTrailer, operation)
	change, err := ReadUnitChange(r.repository, stream, in.Unit)
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
	if err := r.s.Step("rebase-moved"); err != nil {
		return coreadapter.OperationResult{}, err
	}
	return r.record(ctx, stream, in, operation, UnitRebase{Unit: in.Unit, Rebase: in.Rebase, Operation: operation, Branch: w.Branch, Base: base, Onto: in.Onto, Snapshot: snapshot, Commit: commit, Conflicts: conflicts}, rec.note)
}

func rebaseCarrying(ctx context.Context, g workspace.Provider, base, onto, snapshot, message, change string, at time.Time) (string, []string, error) {
	commit, conflicts, err := g.RebaseFrom(ctx, base, onto, snapshot, message, at)
	if err != nil || change == "" {
		return commit, conflicts, err
	}
	commit, err = g.Carry(ctx, commit, change)
	return commit, conflicts, err
}

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

func (r rebaser) record(ctx context.Context, stream config.WorkstreamID, in RebaseInput, operation string, rebase UnitRebase, note string) (coreadapter.OperationResult, error) {
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
		case ReportDocument(in.Unit):
			report = d
		case RebaseDocument(in.Unit):
			previous = d
		}
	}
	move, drifted, err := r.s.CarriedOnto(r.repository, stream, in.Onto)
	if err != nil {
		return coreadapter.OperationResult{}, err
	}
	at := r.s.Now()
	rebase.State, rebase.Report = state.Value, report.Revision
	if rebase.Conflicts == nil {
		rebase.Conflicts = []string{}
	}
	var records []trace.Document
	var txs []trace.Transaction
	var exit *UnitExitInput
	transition, _ := RebaseIDs(in.Unit, in.Rebase)
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
		if state.Value == UnitApproved || state.Value == UnitReviewing || state.Value == UnitChecking {
			id := fmt.Sprintf("%s-%s-rebase-%d", subject, UnitChecking, in.Rebase)
			reason := fmt.Sprintf("unit %s's candidate %s from %s was rebased onto %s as candidate %s, recorded in %s revision %d; the rebased candidate is checked and reviewed before it lands", in.Unit, rebase.Snapshot, rebase.Base, rebase.Onto, rebase.Commit, report.Path, h.Revision)
			var events []trace.Event
			if state.Value == UnitApproved {
				reason = "the approval no longer holds: " + reason
				events = append(events, trace.Progress(id, "unit", fmt.Sprintf("Unit %s returns to checking and review: its approved candidate was rebased onto %s.", in.Unit, rebase.Onto)))
				if drifted {
					events = append(events, trace.UpstreamMoved(id, stream, move, fmt.Sprintf("unit %s's approval no longer holds: its candidate was rebased onto the feature branch at %s and returns to checking and review", in.Unit, rebase.Onto)))
				}
			}
			exit = &UnitExitInput{
				Repo: r.repository, Stream: stream, Unit: in.Unit, State: state, Trigger: TriggerRebased, Cause: operation,
				Default: UnitChecking, Targets: []string{UnitChecking}, Actor: foremanActor,
				ID: id, HeaderCause: operation, Reason: reason, Events: events, Operation: operation,
				Candidate: func(context.Context) (string, error) { return "", nil },
			}
		}
	}
	content, err := json.MarshalIndent(rebase, "", "  ")
	if err != nil {
		return coreadapter.OperationResult{}, err
	}
	records = append(records, trace.Document{Header: trace.Header{Schema: "osmia.trace.document", Version: trace.Version, ID: RebaseDocument(in.Unit), Revision: previous.Revision + 1, Project: r.repository.Project(), Workstream: stream, Unit: in.Unit, At: at, Actor: foremanActor, Cause: operation},
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
	if note != "" {
		events = append(events, trace.Notice(transition+outcome, "gave-up", note))
	}
	rebaseState, err := r.repository.Workflow(stream, RebaseSubject(in.Unit))
	if err != nil {
		return coreadapter.OperationResult{}, err
	}
	txs = append(txs, trace.Transaction{ExpectedVersion: rebaseState.Version,
		Transition: trace.Transition{Header: r.Header(transition+outcome, stream, in.Unit, operation, at), Subject: RebaseSubject(in.Unit), From: rebaseState.Value, To: to, Reason: reason}, Events: events})
	if exit != nil {
		exit.Documents, exit.With = records, txs
		res, err := r.s.raise(ctx, false, *exit)
		switch {
		case err != nil:
			return coreadapter.OperationResult{}, err
		case !res.Committed:
			if done, outcomeErr := r.outcome(stream, in); outcomeErr == nil && done != nil {
				return *done, nil
			}
			return coreadapter.OperationResult{}, fmt.Errorf("the rebase of unit %s is not recorded: the router held its move to checking (%s)", in.Unit, res.Hold)
		}
	} else if _, err := r.repository.RecordDocumentsWith(ctx, records, txs...); err != nil {
		if errors.Is(err, trace.ErrConflict) {
			if done, outcomeErr := r.outcome(stream, in); outcomeErr == nil && done != nil {
				return *done, nil
			}
		}
		return coreadapter.OperationResult{}, err
	}
	if err := r.s.Step("rebase-recorded"); err != nil {
		return coreadapter.OperationResult{}, err
	}
	return coreadapter.OperationResult{Outcome: "succeeded", Evidence: reason}, nil
}

func (r rebaser) refuse(ctx context.Context, stream config.WorkstreamID, in RebaseInput, reason string) (coreadapter.OperationResult, error) {
	return r.refuseWith(ctx, stream, in, reason, "")
}

func (r rebaser) refuseWith(ctx context.Context, stream config.WorkstreamID, in RebaseInput, reason, notice string) (coreadapter.OperationResult, error) {
	transition, _ := RebaseIDs(in.Unit, in.Rebase)
	subject := RebaseSubject(in.Unit)
	state, err := r.repository.Workflow(stream, subject)
	if err != nil {
		return coreadapter.OperationResult{}, err
	}
	recorded := fmt.Sprintf("unit %s was not rebased onto %s: %s", in.Unit, in.Onto, reason)
	tx := trace.Transaction{ExpectedVersion: state.Version,
		Transition: trace.Transition{Header: r.Header(transition+"-refused", stream, in.Unit, transition, r.s.Now()), Subject: subject, From: state.Value, To: fmt.Sprintf("refused-%d", in.Rebase), Reason: recorded}}
	if notice != "" {
		tx.Events = []trace.Event{trace.Notice(transition+"-refused", "chief", notice)}
	}
	if _, err := r.repository.Transact(ctx, tx); err != nil {
		return coreadapter.OperationResult{}, err
	}
	return coreadapter.OperationResult{Outcome: "failed", Evidence: recorded}, nil
}
