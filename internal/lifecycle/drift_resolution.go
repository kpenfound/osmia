package lifecycle

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/kpenfound/agentcore/vcs"
	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/gitdiff"
	"github.com/kpenfound/osmia/internal/isolation"
	"github.com/kpenfound/osmia/internal/plan"
	"github.com/kpenfound/osmia/internal/questions"
	reviewtools "github.com/kpenfound/osmia/internal/review"
	"github.com/kpenfound/osmia/internal/seal"
	"github.com/kpenfound/osmia/internal/trace"
	"github.com/kpenfound/osmia/internal/workspace"
)

// ErrDriftAwaits wraps why a drift rebase in resolution waits: the
// operation stays pending, holding its workstream's landings and the
// Project's other drift rebases, until the resolution is approved or held.
var ErrDriftAwaits = errors.New("drift rebase awaits its conflict resolution")

// DriftHeldError reports why a drift rebase's conflict resolution stops and the
// Drift rebase is held for the owner: review sent the resolution back
// shed.max_bounces times, its mason left conflict markers after
// mason.max_clean_turns reminders, or maxRecoveries turns of its mason or
// reviewer in a row were interrupted.
type DriftHeldError struct{ Why string }

func (e DriftHeldError) Error() string {
	return "drift rebase is held for the owner: " + e.Why
}

// Resolving reports whether a drift rebase record is one of a resolution
// in progress.
func Resolving(outcome string) bool {
	return outcome == DriftConflicted || outcome == driftResolved || outcome == driftRejected
}

// DriftWorkspaces returns the provider of the resolution workspaces of the
// configured project's drift rebases: one workspace per workstream,
// <root>/drifts/<project>/<workstream>, on driftBranch and on the backend the
// Repository records for the workstream.
func DriftWorkspaces(cfg *config.Config, repository *trace.Repository) streamWorkspaces {
	return streamWorkspaces{Cfg: cfg, Repository: repository, Directory: driftsDirectory}
}

// DriftBranch names the branch drift rebase k of the workstream is
// Resolved on.
func DriftBranch(stream config.WorkstreamID, k int) string {
	return fmt.Sprintf("osmia-drift/%s/%d", stream, k)
}
func DriftResolveTurnID(k, round int) string {
	return fmt.Sprintf("%s-%d-resolve-%d", driftMasonAgent, k, round)
}
func DriftFixTurnID(k, review int) string {
	return fmt.Sprintf("%s-%d-review-%d", driftMasonAgent, k, review)
}
func DriftReviewTurnID(k, review int) string {
	return fmt.Sprintf("%s-%d-%d", driftReviewerAgent, k, review)
}

// Resolutions are the resolution workspaces of a project's drift rebases.
// They are also the workspaces a drift mason turn is lent: the turn works
// on a copy without VCS metadata, which capture copies back.
type Resolutions struct{ StreamWorkspaces }

// Find returns the workstream's resolution workspace when the clone has one.
func (r Resolutions) Find(ctx context.Context, stream config.WorkstreamID) (workspace.Worktree, error) {
	g, err := r.Of(stream)
	if err != nil {
		return workspace.Worktree{}, err
	}
	w, found, err := g.Workspace(ctx, string(stream))
	if err != nil {
		return workspace.Worktree{}, err
	}
	if !found {
		return workspace.Worktree{}, fmt.Errorf("workstream %s has no drift resolution workspace", stream)
	}
	return w, nil
}

// Acquire lends a drift mason turn its workstream's resolution workspace,
// which must exist.
func (r Resolutions) Acquire(ctx context.Context, req coreadapter.WorkspaceRequest) (coreadapter.WorkspaceLease, error) {
	if err := ctx.Err(); err != nil {
		return coreadapter.WorkspaceLease{}, err
	}
	scope := req.Scope
	if scope.Role != masonRole || scope.Thread != driftMasonAgent || scope.Workstream == "" {
		return coreadapter.WorkspaceLease{}, errors.New("a drift resolution workspace is lent to a drift mason turn alone")
	}
	stream := config.WorkstreamID(scope.Workstream)
	w, err := r.Find(ctx, stream)
	if err != nil {
		return coreadapter.WorkspaceLease{}, err
	}
	return coreadapter.WorkspaceLease{Workspace: coreadapter.Workspace{ID: driftsDirectory + "/" + string(stream), Directory: w.Path, Access: req.Access}, Lease: noLease{}}, nil
}

// Selection selects the whole of the resolution workspace, but its VCS
// metadata, as the drift mason turn's view, run with execution. The turn
// gets exactly masonGrant's tools over this view: it may read and write
// files, execute, ask the chief of staff, read factory context, record a
// discovery, file an amendment and call done.
func (r Resolutions) Selection(ctx context.Context, scope coreadapter.Scope, execution coreadapter.ExecutionSettings) (isolation.Selection, error) {
	w, err := r.Find(ctx, config.WorkstreamID(scope.Workstream))
	if err != nil {
		return isolation.Selection{}, err
	}
	paths, err := viewPaths(w.Path)
	return isolation.Selection{Paths: paths, Execution: execution}, err
}

// DriftReviewerSelection stages a read-only export of the resolved
// candidate that the drift review turn belongs to, resolved through
// DriftReviewOrigin from durable drift/rebase.json records rather than
// from the live resolution workspace, so it is the same across an answer
// continuation and after a restart. The turn gets exactly reviewerGrant's
// tools over this view: it may read files, ask the chief of staff, read
// factory context, record a discovery, read the diff and record its
// verdict; reviewerGrant holds neither WriteFiles nor Execute, so it
// writes nothing and executes nothing.
func DriftReviewerSelection(ctx context.Context, cfg *config.Config, repository *trace.Repository, scope coreadapter.Scope, execution coreadapter.ExecutionSettings) (isolation.Selection, error) {
	stream := config.WorkstreamID(scope.Workstream)
	rebase, err := DriftReviewOrigin(repository, stream, scope)
	if err != nil {
		return isolation.Selection{}, err
	}
	g, err := DriftWorkspaces(cfg, repository).Of(stream)
	if err != nil {
		return isolation.Selection{}, err
	}
	dir := filepath.Join(cfg.Root.String(), "workspaces", scope.Project, scope.Workstream, "review")
	if err := os.RemoveAll(dir); err != nil {
		return isolation.Selection{}, err
	}
	if err := g.Export(ctx, rebase.Candidate, dir); err != nil {
		return isolation.Selection{}, err
	}
	paths, err := viewPaths(dir)
	if err != nil {
		return isolation.Selection{}, err
	}
	return isolation.Selection{Workspace: coreadapter.WorkspaceRequest{SourceDirectory: dir, Directory: dir, BaseRevision: rebase.Candidate}, Paths: paths, Execution: execution}, nil
}

// Capture copies a drift mason turn's view back into its resolution
// workspace, whatever the turn's result, as a unit's capture does.
func (r Resolutions) Capture(ctx context.Context, scope coreadapter.Scope, view *isolation.FileView) error {
	w, err := r.Find(ctx, config.WorkstreamID(scope.Workstream))
	if err != nil {
		return err
	}
	g, err := r.Of(config.WorkstreamID(scope.Workstream))
	if err != nil {
		return err
	}
	return captureFiles(ctx, g, view.Workspace().Directory, w.Path)
}

// Records returns the recorded revisions of drift/rebase.json for drift
// rebase k, in order.
func (d Drifter) Records(stream config.WorkstreamID, k int) ([]DriftRebase,

	error) {
	return DriftRebases(d.repository, stream, k)
}

// DriftRebases returns the recorded revisions of drift/rebase.json for
// Drift rebase k of the workstream, in order.
func DriftRebases(repository *trace.Repository, stream config.WorkstreamID, k int) ([]DriftRebase, error) {
	docs, err := trace.Read[trace.Document](repository, stream)
	if err != nil {
		return nil, err
	}
	var out []DriftRebase
	for _, doc := range docs {
		if doc.ID != DriftDocument {
			continue
		}
		var r DriftRebase
		if err := json.Unmarshal([]byte(doc.Content), &r); err != nil {
			return nil, fmt.Errorf("%s revision %d: %w", doc.Path, doc.Revision, err)
		}
		if r.Drift == k {
			out = append(out, r)
		}
	}
	return out, nil
}

// LatestDriftRecord returns the latest record of the workstream's latest
// Drift rebase, and whether one is.
func LatestDriftRecord(repository *trace.Repository, stream config.WorkstreamID) (DriftRebase, bool, error) {
	state, err := repository.Workflow(stream, DriftSubject)
	if err != nil {
		return DriftRebase{}, false, err
	}
	k, err := DriftNumber(state.Value)
	if err != nil || k == 0 {
		return DriftRebase{}, false, err
	}
	records, err := DriftRebases(repository, stream, k)
	if err != nil || len(records) == 0 {
		return DriftRebase{}, false, err
	}
	return records[len(records)-1], true, nil
}

// ResolvingMove returns the drift rebase whose conflicts the workstream's
// Drift mason resolves, and whether one is being resolved.
func ResolvingMove(repository *trace.Repository, stream config.WorkstreamID) (trace.UpstreamMove, bool, error) {
	r, found, err := LatestDriftRecord(repository, stream)
	if err != nil || !found || !Resolving(r.Outcome) {
		return trace.UpstreamMove{}, false, err
	}
	return r.Move(), true, nil
}

// CarriedOnto returns the workstream's latest drift rebase when it moved
// the feature branch to onto, and whether it did: a unit rebased onto onto
// is carried by that drift rebase.
func CarriedOnto(repository *trace.Repository, stream config.WorkstreamID, onto string) (trace.UpstreamMove, bool, error) {
	r, found, err := CarryDrift(repository, stream, onto)
	if err != nil || !found {
		return trace.UpstreamMove{}, false, err
	}
	return r.Move(), true, nil
}

// CarryDrift identifies the drift whose approved result is onto.
func CarryDrift(repository *trace.Repository, stream config.WorkstreamID, onto string) (DriftRebase, bool, error) {
	r, found, err := LatestDriftRecord(repository, stream)
	if err != nil || !found || (r.Outcome != DriftCarrying && r.Outcome != DriftRebased) || r.Commit != onto {
		return DriftRebase{}, false, err
	}
	return r, true, nil
}

// UnitRebaseConflicts returns the paths the unit's latest rebase left
// Conflicted when that rebase was onto base, the feature branch commit the
// candidate under review descends from, or nil when it rebased cleanly or
// onto another commit.
func UnitRebaseConflicts(repository *trace.Repository, stream config.WorkstreamID, unit, base string) ([]string, error) {
	docs, err := trace.Read[trace.Document](repository, stream)
	if err != nil {
		return nil, err
	}
	var rebase trace.Document
	for _, d := range docs {
		if d.ID == rebaseDocument(unit) {
			rebase = d
		}
	}
	if rebase.Revision == 0 {
		return nil, nil
	}
	var rebased UnitRebase
	if err := json.Unmarshal([]byte(rebase.Content), &rebased); err != nil {
		return nil, err
	}
	if rebased.Onto != base {
		return nil, nil
	}
	return rebased.Conflicts, nil
}

// UnitDrift returns the drift rebase whose carry produced the candidate the
// unit's latest report names, and whether one did: the unit's latest
// rebase, clean and onto the feature branch a drift rebase moved, made that
// candidate.
func UnitDrift(repository *trace.Repository, stream config.WorkstreamID, unit string) (trace.UpstreamMove, bool, error) {
	docs, err := trace.Read[trace.Document](repository, stream)
	if err != nil {
		return trace.UpstreamMove{}, false, err
	}
	var report, rebase trace.Document
	for _, d := range docs {
		switch d.ID {
		case reportDocument(unit):
			report = d
		case rebaseDocument(unit):
			rebase = d
		}
	}
	if report.Revision == 0 || rebase.Revision == 0 {
		return trace.UpstreamMove{}, false, nil
	}
	var reported UnitReport
	var rebased UnitRebase
	if err := json.Unmarshal([]byte(report.Content), &reported); err != nil {
		return trace.UpstreamMove{}, false, err
	}
	if err := json.Unmarshal([]byte(rebase.Content), &rebased); err != nil {
		return trace.UpstreamMove{}, false, err
	}
	if len(rebased.Conflicts) != 0 || rebased.Commit != reported.Candidate {
		return trace.UpstreamMove{}, false, nil
	}
	return CarriedOnto(repository, stream, rebased.Onto)
}

// Conflicted returns every path a stop of drift rebase k left conflicted,
// sorted.
func (d Drifter) Conflicted(stream config.WorkstreamID, k int) ([]string, error) {
	records, err := d.Records(stream, k)
	if err != nil {
		return nil, err
	}
	var paths []string
	for _, r := range records {
		for _, p := range r.Conflicts {
			if !slices.Contains(paths, p) {
				paths = append(paths, p)
			}
		}
	}
	slices.Sort(paths)
	return paths, nil
}

// Conflict records that the replay of drift rebase k conflicted: the next
// revision of drift/rebase.json and the drift subject's move to
// Conflicted-<k>, with an upstream moved event, in one commit. The branch and the seal stay; the
// Resolution starts from this record.
func (d Drifter) Conflict(ctx context.Context, stream config.WorkstreamID, rebase DriftRebase) error {
	doc, err := d.Document(stream, rebase)
	if err != nil {
		return err
	}
	state, err := d.repository.Workflow(stream, DriftSubject)
	if err != nil {
		return err
	}
	transition, _ := DriftIDs(rebase.Drift)
	reason := fmt.Sprintf("feature branch %s does not rebase cleanly onto %s/%s at %s: %s conflicted; the branch stays at %s and the seal is unchanged until a mason's resolution of the conflicts against the sealed spec is approved by a reviewer", rebase.Branch, rebase.Upstream.Remote, rebase.Upstream.Branch, rebase.Upstream.Commit, strings.Join(rebase.Conflicts, ", "), rebase.Before)
	id := transition + "-" + DriftConflicted
	visible := fmt.Sprintf("feature branch %s conflicts with upstream in %s; a mason resolves the conflicts against the sealed spec, and the branch and the seal stay until a reviewer approves the resolution", rebase.Branch, strings.Join(rebase.Conflicts, ", "))
	tx := trace.Transaction{ExpectedVersion: state.Version, Transition: trace.Transition{Header: d.header(id, stream, "", rebase.Operation, d.s.Now()), Subject: DriftSubject, From: state.Value, To: fmt.Sprintf("%s-%d", DriftConflicted, rebase.Drift), Reason: reason}, Events: []trace.Event{trace.UpstreamMoved(id, stream, rebase.Move(), visible)}}
	if _, err := d.repository.RecordDocumentsWith(ctx, []trace.Document{doc}, tx); err != nil && !errors.Is(err, trace.ErrConflict) {
		return err
	}
	return nil
}

// Advance records the next step of a resolution as the next revision of
// Drift/rebase.json and returns it.
func (d Drifter) Advance(ctx context.Context, stream config.WorkstreamID, next DriftRebase) (DriftRebase, error) {
	return next, d.RecordReplay(ctx, stream, next)
}

// Resolve drives the resolution of conflicted drift rebase k from its
// Latest record, one step at a time, and returns the replayed record once
// a reviewer approved the resolved candidate. Until then it returns an
// error that wraps errDriftAwaits, and the feature branch and the seal stay
// where they are.
//
// The replay runs in the workstream's resolution workspace, created from the
// branch's old tip on its own branch. At each commit that conflicts it stops
// with the conflict markers in the workspace, the stop is recorded, and the
// Drift mason gets one turn that names the conflicted paths and carries the
// sealed spec. Once the mason's turn ends with no marker left in those
// paths, the service stages the workspace and goes on with the replay. The
// replayed branch is the candidate a reviewer reads against the sealed
// spec: approval replays the feature branch to it, and material findings
// return it to the mason, whose work the service snapshots as the next
// candidate. Once review has sent the resolution back shed.max_bounces
// times, or the mason has left markers after mason.max_clean_turns
// reminders, resolve returns a driftHeldError instead of asking the mason
// again.
// The resolution workspace is removed once the feature branch moves. Every step reads the workspace and the threads first, so a
// Retry or a restart goes on from where the last one stopped and queues no
// turn twice.
func (d Drifter) Resolve(ctx context.Context, stream config.WorkstreamID, rebase DriftRebase) (DriftRebase, error) {
	w, err := d.Resolution(ctx, stream, rebase)
	if err != nil {
		return rebase, err
	}
	g, err := DriftWorkspaces(d.cfg, d.repository).Of(stream)
	if err != nil {
		return rebase, err
	}
	requested, err := d.requestedAt(stream, rebase.Operation)
	if err != nil {
		return rebase, err
	}
	for {
		stop, unmerged, replaying, err := g.Replaying(ctx, w)
		if err != nil {
			return rebase, err
		}
		head, _, err := g.Branch(ctx, w.Branch)
		if err != nil {
			return rebase, err
		}
		switch {
		case !replaying && head == rebase.Before && rebase.Outcome == DriftConflicted:
			if err := d.s.Step("drift-resolving"); err != nil {
				return rebase, err
			}
			if _, _, err := g.ReplayInFrom(ctx, w, rebase.From, rebase.Upstream.Commit, requested); err != nil {
				return rebase, err
			}
			continue
		case replaying && len(unmerged) == 0:
			// A replay stopped for a resolution the service already staged,
			// or stopped by the service itself: it goes on.
			if _, _, err := g.ContinueReplay(ctx, w, requested); err != nil {
				return rebase, err
			}
			continue
		case replaying:
			if stop != rebase.Stop || rebase.Outcome != DriftConflicted {
				next := rebase
				next.Outcome, next.Round, next.Stop, next.Conflicts = DriftConflicted, rebase.Round+1, stop, unmerged
				if rebase, err = d.Advance(ctx, stream, next); err != nil {
					return rebase, err
				}
			}
			turn := DriftResolveTurnID(rebase.Drift, rebase.Round)
			prompt, err := d.ResolvePrompt(ctx, stream, rebase)
			if err != nil {
				return rebase, err
			}
			if done, err := d.MasonDone(ctx, stream, w, turn, prompt, rebase.Conflicts); err != nil || !done {
				return rebase, Awaiting(rebase, err, "its mason's resolution of %s", strings.Join(rebase.Conflicts, ", "))
			}
			if err := d.s.Step("drift-staging"); err != nil {
				return rebase, err
			}
			if _, _, err := g.ContinueReplay(ctx, w, requested); err != nil {
				return rebase, err
			}
			continue
		}
		switch rebase.Outcome {
		case DriftConflicted:
			next := rebase
			next.Outcome, next.Candidate, next.Review = driftResolved, head, rebase.Review+1
			if rebase, err = d.Advance(ctx, stream, next); err != nil {
				return rebase, err
			}
			continue
		case driftRejected:
			if rebase.Review >= d.cfg.Shed.MaxBounces {
				return rebase, DriftHeldError{fmt.Sprintf("review sent its resolution of the conflicts in %s back %d times, reaching shed.max_bounces", strings.Join(rebase.Conflicts, ", "), rebase.Review)}
			}
			paths, err := d.Conflicted(stream, rebase.Drift)
			if err != nil {
				return rebase, err
			}
			prompt, err := d.FixPrompt(stream, rebase)
			if err != nil {
				return rebase, err
			}
			if done, err := d.MasonDone(ctx, stream, w, DriftFixTurnID(rebase.Drift, rebase.Review), prompt, paths); err != nil || !done {
				return rebase, Awaiting(rebase, err, "its mason's answer to review %d", rebase.Review)
			}
			candidate, err := g.Snapshot(ctx, w, rebase.Upstream.Commit)
			if err != nil {
				return rebase, err
			}
			next := rebase
			next.Outcome, next.Candidate, next.Review, next.Verdict = driftResolved, candidate, rebase.Review+1, nil
			if rebase, err = d.Advance(ctx, stream, next); err != nil {
				return rebase, err
			}
			continue
		}
		verdict, found, err := d.Reviewed(ctx, stream, rebase)
		if err != nil || !found {
			return rebase, Awaiting(rebase, err, "review %d of candidate %s", rebase.Review, rebase.Candidate)
		}
		next := rebase
		next.Verdict = &verdict
		if verdict.Decision != "satisfactory" {
			next.Outcome = driftRejected
			if rebase, err = d.Advance(ctx, stream, next); err != nil {
				return rebase, err
			}
			continue
		}
		next.Outcome, next.Commit = DriftReplayed, rebase.Candidate
		if rebase, err = d.Advance(ctx, stream, next); err != nil {
			return rebase, err
		}
		return rebase, d.s.Step("drift-approved")
	}
}

// Awaiting returns err, or the errDriftAwaits a resolution step waiting on
// What returns.
func Awaiting(rebase DriftRebase, err error, format string, args ...any) error {
	if err != nil {
		return err
	}
	return fmt.Errorf("drift rebase %d awaits %s: %w", rebase.Drift, fmt.Sprintf(format, args...), ErrDriftAwaits)
}

// Resolution returns the workstream's resolution workspace for drift rebase
// k, creating it on driftBranch from the feature branch's old tip when the
// clone has none. One a drift rebase before this one left is removed first.
func (d Drifter) Resolution(ctx context.Context, stream config.WorkstreamID, rebase DriftRebase) (workspace.Worktree, error) {
	g, err := DriftWorkspaces(d.cfg, d.repository).Of(stream)
	if err != nil {
		return workspace.Worktree{}, err
	}
	branch := DriftBranch(stream, rebase.Drift)
	w, found, err := g.Workspace(ctx, string(stream))
	if err != nil {
		return workspace.Worktree{}, err
	}
	if found && w.Branch != branch {
		if err := g.Release(ctx, w); err != nil {
			return workspace.Worktree{}, err
		}
	}
	acquired, err := g.Acquire(ctx, vcs.Request{Name: string(stream), Ref: rebase.Before, Branch: branch})
	if err != nil {
		return workspace.Worktree{}, err
	}
	return acquired.(workspace.Worktree), nil
}

// Release removes the workstream's resolution workspace when the clone has
// one. Its branch stays.
func (d Drifter) Release(ctx context.Context, stream config.WorkstreamID) error {
	g, err := DriftWorkspaces(d.cfg, d.repository).Of(stream)
	if err != nil {
		return err
	}
	w, found, err := g.Workspace(ctx, string(stream))
	if err != nil || !found {
		return err
	}
	return g.Release(ctx, w)
}

// MasonDone drives the drift mason turn that begins with turn and reports
// whether it is done: the thread's latest turn, turn or a follow-up of it,
// ended, and none of paths carries a conflict marker in the workspace. It
// queues turn when the thread does not have it. An interrupted turn has its
// surviving view copied back and one continuation queued. A turn that ended
// with markers left gets one reminder that names them, up to
// mason.max_clean_turns reminders since turn; after that, or once
// maxRecoveries turns in a row were interrupted, it returns a
// DriftHeldError.
func (d Drifter) MasonDone(ctx context.Context, stream config.WorkstreamID, w workspace.Worktree, turn, prompt string, paths []string) (bool, error) {
	if err := d.EnsureThread(ctx, stream, driftMasonAgent, masonRole); err != nil {
		return false, err
	}
	th, err := d.repository.Thread(stream, driftMasonAgent)
	if err != nil {
		return false, err
	}
	i := slices.IndexFunc(th.Turns, func(q trace.QueuedTurn) bool {
		return q.Request.TurnID == turn
	})
	if i < 0 {
		profile, _, err := d.s.RoleExecution(d.cfg, masonRole)
		if err != nil {
			return false, err
		}
		req := trace.TurnRequest{Header: trace.Header{Schema: "osmia.trace.turn-request", Version: trace.Version, ID: "request_" + turn, Revision: 1, Project: d.repository.Project(), Workstream: stream, At: d.s.Now(), Actor: foremanActor, Cause: DriftDocument, Depth: 1}, AgentID: driftMasonAgent, ThreadID: driftMasonAgent, TurnID: turn, Profile: profile, SystemPrompt: DriftMasonSystemPrompt(d.cfg.Project), Prompt: prompt}
		_, err = d.repository.EnqueueTurn(ctx, req)
		return false, err
	}
	last := th.Turns[len(th.Turns)-1]
	switch last.Status() {
	case "interrupted":
		if n := interruptedInARow(th); n >= maxRecoveries {
			return false, DriftHeldError{fmt.Sprintf("its mason's latest %d turns were interrupted in a row", n)}
		}
		return false, d.Follow(ctx, stream, last, "recover", last.Request.Prompt+"\n\n"+interruption(last)+" Your view holds the files that turn left. Go on from them, and call done once the resolution is complete.")
	case "failed":
		return false, DriftHeldError{TurnFailure("mason", last)}
	case "idle":
	default:
		return false, nil
	}
	g, err := DriftWorkspaces(d.cfg, d.repository).Of(stream)
	if err != nil {
		return false, err
	}
	marked, err := g.MarkedFiles(w, paths)
	if err != nil || len(marked) == 0 {
		return err == nil, err
	}
	reminded := 0
	for _, q := range th.Turns[i:] {
		if strings.HasPrefix(q.Request.TurnID, driftMasonAgent+"-markers-") {
			reminded++
		}
	}
	if reminded >= d.cfg.Mason.MaxCleanTurns {
		return false, DriftHeldError{fmt.Sprintf("its mason left conflict markers in %s after %d reminders, exhausting mason.max_clean_turns", strings.Join(marked, ", "), reminded)}
	}
	return false, d.Follow(ctx, stream, last, "markers", fmt.Sprintf("Your last turn ended, and these files still carry conflict markers: %s. Resolve each conflict against the sealed spec, remove every marker, and call done again.", strings.Join(marked, ", ")))
}

// Follow queues, once, the drift mason turn that follows turn last, named
// by kind and last's sequence, with prompt.
func (d Drifter) Follow(ctx context.Context, stream config.WorkstreamID, last trace.QueuedTurn, kind, prompt string) error {
	req := last.Request
	req.TurnID = fmt.Sprintf("%s-%s-%d", driftMasonAgent, kind, last.Sequence)
	req.ID = "request_" + req.TurnID
	req.At = d.s.Now()
	if last.Response != nil {
		req.Cause = last.Response.ID
	}
	req.Prompt = prompt
	profile, _, err := d.s.RoleExecution(d.cfg, masonRole)
	if err != nil {
		return err
	}
	req.Profile = profile
	th, err := d.repository.Thread(stream, driftMasonAgent)
	if err != nil {
		return err
	}
	if slices.ContainsFunc(th.Turns, func(q trace.QueuedTurn) bool {
		return q.Request.TurnID == req.TurnID
	}) {
		return nil
	}
	_, err = d.repository.EnqueueTurn(ctx, req)
	return err
}

// Reviewed returns the verdict of review n of the resolved candidate, and
// whether its turn recorded one. It queues the review turn when the
// reviewer's thread does not have it, and one continuation of a review a
// stop interrupted, unless maxRecoveries turns in a row were interrupted,
// when it returns a driftHeldError.
func (d Drifter) Reviewed(ctx context.Context, stream config.WorkstreamID, rebase DriftRebase) (UnitVerdict, bool, error) {
	if err := d.EnsureThread(ctx, stream, driftReviewerAgent, reviewerRole); err != nil {
		return UnitVerdict{}, false, err
	}
	th, err := d.repository.Thread(stream, driftReviewerAgent)
	if err != nil {
		return UnitVerdict{}, false, err
	}
	turn := DriftReviewTurnID(rebase.Drift, rebase.Review)
	i := slices.IndexFunc(th.Turns, func(q trace.QueuedTurn) bool {
		return q.Request.TurnID == turn
	})
	if i < 0 {
		prompt, err := d.ReviewPrompt(ctx, stream, rebase)
		if err != nil {
			return UnitVerdict{}, false, err
		}
		profile, _, err := d.s.RoleExecution(d.cfg, reviewerRole)
		if err != nil {
			return UnitVerdict{}, false, err
		}
		req := trace.TurnRequest{Header: trace.Header{Schema: "osmia.trace.turn-request", Version: trace.Version, ID: "request_" + turn, Revision: 1, Project: d.repository.Project(), Workstream: stream, At: d.s.Now(), Actor: foremanActor, Cause: DriftDocument, Depth: 1}, AgentID: driftReviewerAgent, ThreadID: driftReviewerAgent, TurnID: turn, Profile: profile, SystemPrompt: DriftReviewerSystemPrompt(), Prompt: prompt}
		_, err = d.repository.EnqueueTurn(ctx, req)
		return UnitVerdict{}, false, err
	}
	if th.Status == "interrupted" && th.Active != "" {
		if err := d.repository.AbandonTurn(ctx, stream, driftReviewerAgent, th.Active, d.s.Now()); err != nil {
			return UnitVerdict{}, false, err
		}
		if th, err = d.repository.Thread(stream, driftReviewerAgent); err != nil {
			return UnitVerdict{}, false, err
		}
	}
	last := th.Turns[len(th.Turns)-1]
	switch last.Status() {
	case "interrupted":
		if n := interruptedInARow(th); n >= maxRecoveries {
			return UnitVerdict{}, false, DriftHeldError{fmt.Sprintf("its reviewer's latest %d turns were interrupted in a row", n)}
		}
		req := last.Request
		req.TurnID = fmt.Sprintf("%s-recover-%d", turn, last.Sequence)
		req.ID, req.At = "request_"+req.TurnID, d.s.Now()
		if last.Response != nil {
			req.Cause = last.Response.ID
		}
		req.Prompt += "\n\nThe previous turn was interrupted. Review the same candidate and record a verdict."
		if slices.ContainsFunc(th.Turns, func(q trace.QueuedTurn) bool {
			return q.Request.TurnID == req.TurnID
		}) {
			return UnitVerdict{}, false, nil
		}
		_, err := d.repository.EnqueueTurn(ctx, req)
		return UnitVerdict{}, false, err
	case "failed":
		return UnitVerdict{}, false, DriftHeldError{TurnFailure("reviewer", last)}
	case "idle":
	default:
		return UnitVerdict{}, false, nil
	}
	outcome := last.Response.Result.Outcome
	if outcome == nil || outcome.Status != verdictOutcome {
		return UnitVerdict{}, false, DriftHeldError{fmt.Sprintf("its reviewer's turn %s ended without recording a verdict", last.Request.TurnID)}
	}
	var verdict UnitVerdict
	if err := json.Unmarshal([]byte(outcome.Report), &verdict); err != nil {
		return UnitVerdict{}, false, fmt.Errorf("the verdict of turn %s: %w", last.Request.TurnID, err)
	}
	return verdict, true, nil
}

// TurnFailure says why a drift role's failed turn holds its drift rebase.
func TurnFailure(role string, last trace.QueuedTurn) string {
	why := fmt.Sprintf("its %s's turn %s failed", role, last.Request.TurnID)
	if exhausted(last) {
		return why + " on every retry and fallback profile: " + failureHistory(last)
	}
	if last.Response.Failure != "" {
		why += ": " + last.Response.Failure
	}
	return why
}

// EnsureThread creates the workstream's drift agent thread when it is
// missing.
func (d Drifter) EnsureThread(ctx context.Context, stream config.WorkstreamID, agent, role string) error {
	if _, err := d.repository.Thread(stream, agent); err == nil || !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return d.repository.CreateThread(ctx, trace.Agent{Header: trace.Header{Schema: "osmia.trace.agent", Version: trace.Version, ID: agent, Revision: 1, Project: d.repository.Project(), Workstream: stream, At: d.s.Now(), Actor: foremanActor, Cause: DriftDocument}, Role: role, ThreadID: agent})
}

// DriftScopeRule says that the conflicted paths of a drift rebase, and any
// change the rebased branch needs so that both upstream's and the feature
// branch's behaviour stand, are in scope whatever the workstream's sealed
// spec, plan or unit footprints name, and that a resolved path outside the
// workstream's original scope is not, on its own, a reason to reject the
// Resolution. It belongs in the drift mason's and drift reviewer's system
// prompts and in every resolution, fix and review prompt.
const DriftScopeRule = "Conflicted paths, and any other change the rebased branch needs so that both upstream's and the feature branch's behaviour stand, are in scope of this drift rebase, whatever the workstream's sealed spec, plan or unit footprints name. A resolved path outside the workstream's original scope is not, on its own, a reason to reject it."

// DriftReviewChecks names what the drift reviewer checks of a resolution:
// both sides kept, nothing of the feature branch's change lost or altered,
// no change beyond what the rebase needs, no conflict marker left, and the
// sealed criteria still holding.
const DriftReviewChecks = "Check that both sides are kept: nothing upstream now holds, and nothing the feature branch built, was lost or altered. Check that no change goes beyond what the rebase needs, that no conflict marker remains, and that the sealed criteria still hold. " + DriftScopeRule

func DriftMasonSystemPrompt(p config.Project) string {
	return fmt.Sprintf("You are a mason of the %s project (%s). You resolve the conflicts the service's rebase of a feature branch onto upstream left, in a workspace of its own whose files are your view. You hold no version control tool: the service records your work and goes on with the rebase. Resolve against the sealed spec, change nothing the conflicts do not need, and call done when no conflict marker is left. %s When upstream's change alters what a sealed criterion means, call %s: the owner decides the spec, never your resolution. When you need a decision you cannot make from your context, call %s to ask the workstream's chief of staff about the drift rebase you resolve; the answer arrives as your next turn on this thread.", p.Name, p.Upstream, DriftScopeRule, questions.AmendTool, questions.AskTool)
}

// DriftReviewerSystemPrompt is the system prompt of the drift reviewer
// turn that reads a resolution of a drift rebase's conflicts against the
// sealed spec.
func DriftReviewerSystemPrompt() string {
	return fmt.Sprintf("You are the reviewer of a resolution of the conflicts a rebase onto upstream left in a feature branch. Your view holds the resolved candidate's files, read-only: you cannot edit it. Read only the supplied evidence: call %s to read the feature branch's change and the resolution. %s Call %s with your decision. When you need a decision you cannot make from your context, call %s to ask the workstream's chief of staff about the drift rebase you review; the answer arrives as your next turn on this thread.", reviewtools.DiffToolName, DriftReviewChecks, verdictTool, questions.AskTool)
}

// DriftAmendGuidance tells a drift mason how to raise an upstream change to
// What a sealed criterion means.
func DriftAmendGuidance(rebase DriftRebase) string {
	return fmt.Sprintf("If upstream's change alters what a sealed criterion means, do not settle that in the resolution: call %s with the spec#<n> criteria it changes, the change the spec needs and why. The request cites upstream commit %s and goes to the owner; resolve the conflicts against the sealed spec as it stands all the same.", questions.AmendTool, rebase.Upstream.Commit)
}

// SealedSpec returns the workstream's sealed spec rendered for a prompt.
func (d Drifter) SealedSpec(stream config.WorkstreamID) (string, error) {
	current, _, found, err := seal.Latest(d.repository, stream)
	if err != nil {
		return "", err
	}
	if !found {
		return "", fmt.Errorf("workstream %s has no seal", stream)
	}
	docs, err := trace.Read[trace.Document](d.repository, stream)
	if err != nil {
		return "", err
	}
	i := slices.IndexFunc(docs, func(doc trace.Document) bool {
		return doc.ID == plan.SpecDocument && doc.Revision == current.Revision.Spec
	})
	if i < 0 {
		return "", fmt.Errorf("workstream %s has no recorded spec revision %d", stream, current.Revision.Spec)
	}
	content := docs[i].Content
	if !strings.HasSuffix(content, "\n") {
		content += "\n"
	}
	return fmt.Sprintf("## Sealed spec: %s revision %d, seal %d\n%s## end of %s\n", plan.SpecPath, current.Revision.Spec, current.Seal, content, plan.SpecPath), nil
}
func (d Drifter) ResolvePrompt(ctx context.Context, stream config.WorkstreamID, rebase DriftRebase) (string, error) {
	spec, err := d.SealedSpec(stream)
	if err != nil {
		return "", err
	}
	handback, err := DriftHandbackNote(d.repository, stream, rebase.Drift)
	if err != nil {
		return "", err
	}
	g, err := DriftWorkspaces(d.cfg, d.repository).Of(stream)
	if err != nil {
		return "", err
	}
	c, err := g.Commit(ctx, rebase.Stop)
	if err != nil {
		return "", err
	}
	subject, _, _ := strings.Cut(c.Message, "\n")
	backend, err := d.repository.Workspaces(stream)
	if err != nil {
		return "", err
	}
	markers := `A conflicted file carries conflict markers: the lines between "<<<<<<<" and "=======" are the branch as rebased onto upstream so far, and those between "=======" and ">>>>>>>" are the commit being replayed.`
	if backend == config.WorkspacesJujutsu {
		markers = jujutsuMarkers("the branch as rebased onto upstream so far", "the commit being replayed")
	}
	return fmt.Sprintf(`Resolve the conflicts of feature branch %s with upstream.

The service is rebasing the workstream's feature branch %s from %s onto %s/%s at %s, one commit at a time, in a workspace of its own whose files are your view. Replaying commit %s (%q) conflicted, and these files of your view are conflicted:
- %s

%s Resolve every conflict against the sealed spec below, so that what upstream now holds and what the feature branch built both stand, and remove every marker. Change nothing the conflicts do not need. Then call done with the outcome of your resolution and end your turn. The service goes on with the rebase once no conflicted file carries a marker, and a reviewer reads the resolved branch against the sealed spec before the feature branch moves. %s%s

%s

%s`, rebase.Branch, rebase.Branch, rebase.Before, rebase.Upstream.Remote, rebase.Upstream.Branch, rebase.Upstream.Commit, rebase.Stop, subject, strings.Join(rebase.Conflicts, "\n- "), markers, DriftScopeRule, handback, DriftAmendGuidance(rebase), spec), nil
}
func (d Drifter) FixPrompt(stream config.WorkstreamID, rebase DriftRebase) (string, error) {
	spec, err := d.SealedSpec(stream)
	if err != nil {
		return "", err
	}
	handback, err := DriftHandbackNote(d.repository, stream, rebase.Drift)
	if err != nil {
		return "", err
	}
	var findings []string
	if rebase.Verdict != nil {
		for _, f := range rebase.Verdict.Findings {
			findings = append(findings, fmt.Sprintf("- (%s) %s Action: %s", f.Severity, f.Evidence, f.Action))
		}
	}
	return fmt.Sprintf(`The reviewer did not approve your resolution of the conflicts of feature branch %s with upstream. Their findings:
%s

Your view holds the resolved branch, candidate %s. Address every finding against the sealed spec below, keep what upstream holds and what the feature branch built, and leave no conflict marker. Then call done with the outcome of your changes and end your turn. The reviewer reads the resolution again before the feature branch moves. %s%s

%s

%s`, rebase.Branch, strings.Join(findings, "\n"), rebase.Candidate, DriftScopeRule, handback, DriftAmendGuidance(rebase), spec), nil
}
func (d Drifter) ReviewPrompt(ctx context.Context, stream config.WorkstreamID, rebase DriftRebase) (string, error) {
	spec, err := d.SealedSpec(stream)
	if err != nil {
		return "", err
	}
	handback, err := DriftHandbackNote(d.repository, stream, rebase.Drift)
	if err != nil {
		return "", err
	}
	paths, err := d.Conflicted(stream, rebase.Drift)
	if err != nil {
		return "", err
	}
	diffs, err := DriftDiffs(ctx, d.cfg, d.repository, stream, rebase)
	if err != nil {
		return "", err
	}
	var changes []string
	for _, diff := range diffs {
		text, err := diff.Read(ctx)
		if err != nil {
			return "", err
		}
		changes = append(changes, fmt.Sprintf("Change %q is %s, from %s to %s:\n%s", diff.Name, diff.About, diff.From, diff.To, gitdiff.ChangedFiles(text)))
	}
	return fmt.Sprintf(`Review the resolution of the conflicts of feature branch %s with upstream against the sealed spec below.

The service rebased feature branch %s from %s onto %s/%s at %s. The rebase conflicted in:
- %s

A mason resolved the conflicts, and the resolved branch is candidate %s. The feature branch stays at %s until you approve it: a satisfactory verdict moves the feature branch and the seal to the candidate, and material findings return the resolution to the mason with your findings. %s Summarize what you checked in the verdict, citing criteria as spec#<n> where they matter.%s

Read both changes with %s, whole or by file and line range. Their changed files, with added and removed lines:

%s
%s`, rebase.Branch, rebase.Branch, rebase.Before, rebase.Upstream.Remote, rebase.Upstream.Branch, rebase.Upstream.Commit, strings.Join(paths, "\n- "), rebase.Candidate, rebase.Before, DriftReviewChecks, handback, reviewtools.DiffToolName, strings.Join(changes, "\n"), spec), nil
}

// DriftDiffs are the two changes a drift review reads: the feature branch's
// change before the rebase, from its merge base with upstream, and the
// Resolved candidate's change on upstream.
func DriftDiffs(ctx context.Context, cfg *config.Config, repository *trace.Repository, stream config.WorkstreamID, rebase DriftRebase) ([]reviewtools.PinnedDiff, error) {
	g, err := DriftWorkspaces(cfg, repository).Of(stream)
	if err != nil {
		return nil, err
	}
	base, err := g.MergeBase(ctx, rebase.Before, rebase.Upstream.Commit)
	if err != nil {
		return nil, err
	}
	read := func(from, to string) func(context.Context) (string, error) {
		return func(ctx context.Context) (string, error) {
			return g.Diff(ctx, from, to)
		}
	}
	return []reviewtools.PinnedDiff{{Name: "feature", About: "the feature branch's change before the rebase, from its merge base with upstream", From: base, To: rebase.Before, Read: read(base, rebase.Before)}, {Name: "resolution", About: "the resolved candidate's change on the upstream commit it was rebased onto", From: rebase.Upstream.Commit, To: rebase.Candidate, Read: read(rebase.Upstream.Commit, rebase.Candidate)}}, nil
}

// DriftOriginTurnID walks an agent thread's answer turn back, through
// however many chained asks, to the turn ID that began the attempt it
// continues: a drift resolve, fix or review turn. A turn that is not an
// answer turn is its own origin. This resolves an answer continuation to
// the same origin as the turn it continues, from durable question and
// thread records alone, so it survives a restart.
func DriftOriginTurnID(repository *trace.Repository, stream config.WorkstreamID, agent, turn string) (string, error) {
	for strings.HasPrefix(turn, questions.TurnID("")) {
		th, err := repository.Thread(stream, agent)
		if err != nil {
			return "", err
		}
		asked, err := repository.Questions(stream)
		if err != nil {
			return "", err
		}
		question := answerQuestionID(turn)
		i := slices.IndexFunc(asked, func(q trace.QuestionState) bool {
			return q.Asked.ID == question && q.Asked.Thread == agent
		})
		if i < 0 {
			return "", fmt.Errorf("turn %s names no recorded question %s on thread %s", turn, question, agent)
		}
		j := slices.IndexFunc(th.Turns, func(t trace.QueuedTurn) bool {
			return t.Request.TurnID == asked[i].Asked.Turn
		})
		if j < 0 {
			return "", fmt.Errorf("turn %s's asking turn %s is not recorded on thread %s", turn, asked[i].Asked.Turn, agent)
		}
		turn = th.Turns[j].Request.TurnID
	}
	return turn, nil
}

// DriftReviewOrigin resolves a drift reviewer turn, including an answer
// continuation of a chain of asks, to the drift rebase and review it
// belongs to: the origin turn's ID names drift rebase k and review n as
// DriftReviewTurnID does, and that review's recorded candidate is read
// from drift/rebase.json, not from the turn ID itself.
func DriftReviewOrigin(repository *trace.Repository, stream config.WorkstreamID, scope coreadapter.Scope) (DriftRebase, error) {
	origin, err := DriftOriginTurnID(repository, stream, driftReviewerAgent, scope.Turn)
	if err != nil {
		return DriftRebase{}, err
	}
	var k, review int
	if _, err := fmt.Sscanf(strings.TrimPrefix(origin, driftReviewerAgent+"-"), "%d-%d", &k, &review); err != nil {
		return DriftRebase{}, fmt.Errorf("turn %s is not a drift review", origin)
	}
	records, err := DriftRebases(repository, stream, k)
	if err != nil {
		return DriftRebase{}, err
	}
	for i := len(records) - 1; i >= 0; i-- {
		if records[i].Review == review && records[i].Candidate != "" {
			return records[i], nil
		}
	}
	return DriftRebase{}, fmt.Errorf("drift rebase %d records no candidate for review %d", k, review)
}

// DriftDiffTool serves the changes of the drift review a turn belongs to,
// Resolved through driftReviewOrigin so an answer continuation reads the
// same pinned diffs as the review turn it continues.
func DriftDiffTool(ctx context.Context, cfg *config.Config, repository *trace.Repository, scope coreadapter.Scope) (coreadapter.Tool, error) {
	stream := config.WorkstreamID(scope.Workstream)
	rebase, err := DriftReviewOrigin(repository, stream, scope)
	if err != nil {
		return coreadapter.Tool{}, err
	}
	diffs, err := DriftDiffs(ctx, cfg, repository, stream, rebase)
	if err != nil {
		return coreadapter.Tool{}, err
	}
	return reviewtools.DiffTool(diffs...), nil
}
