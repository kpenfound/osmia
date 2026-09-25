package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/kpenfound/busybees/core/vcs"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/isolation"
	"github.com/kpenfound/osmia/internal/plan"
	"github.com/kpenfound/osmia/internal/questions"
	"github.com/kpenfound/osmia/internal/seal"
	"github.com/kpenfound/osmia/internal/trace"
	"github.com/kpenfound/osmia/internal/workspace"
)

const (
	// driftMasonAgent is the agent and thread of a workstream's mason that
	// resolves the conflicts of its drift rebases, and driftReviewerAgent
	// the reviewer that reads each resolution. Neither name is one a unit's
	// agent can take.
	driftMasonAgent    = "drift-mason"
	driftReviewerAgent = "drift-reviewer"
	// driftsDirectory is the directory under the root holding the
	// resolution workspaces of drift rebases, by project and workstream.
	driftsDirectory = "drifts"
	// The outcomes drift/rebase.json records while a conflicted drift
	// rebase is resolved: resolved once the replay holds a candidate for
	// review, rejected once a review sent it back to the mason.
	driftResolved = "resolved"
	driftRejected = "rejected"
)

// errDriftAwaits wraps why a drift rebase in resolution waits: the
// operation stays pending, and holds the project's lander, until the
// resolution is approved.
var errDriftAwaits = errors.New("drift rebase awaits its conflict resolution")

// resolving reports whether a drift rebase record is one of a resolution
// in progress.
func resolving(outcome string) bool {
	return outcome == driftConflicted || outcome == driftResolved || outcome == driftRejected
}

// driftWorkspaces returns the provider of the resolution workspaces of the
// configured project's drift rebases: one Git worktree per workstream,
// <root>/drifts/<project>/<workstream>, on driftBranch.
func driftWorkspaces(cfg *config.Config) *workspace.Git {
	return &workspace.Git{Clone: cfg.Project.Clone, Directory: filepath.Join(cfg.Root.String(), driftsDirectory, string(cfg.Project.ID))}
}

// driftBranch names the branch drift rebase k of the workstream is
// resolved on.
func driftBranch(stream config.WorkstreamID, k int) string {
	return fmt.Sprintf("osmia-drift/%s/%d", stream, k)
}

func driftResolveTurnID(k, round int) string {
	return fmt.Sprintf("%s-%d-resolve-%d", driftMasonAgent, k, round)
}

func driftFixTurnID(k, review int) string {
	return fmt.Sprintf("%s-%d-review-%d", driftMasonAgent, k, review)
}

func driftReviewTurnID(k, review int) string {
	return fmt.Sprintf("%s-%d-%d", driftReviewerAgent, k, review)
}

// resolutions are the resolution workspaces of a project's drift rebases.
// They are also the workspaces a drift mason turn is lent: the turn works
// on a copy without VCS metadata, which capture copies back.
type resolutions struct{ git *workspace.Git }

// find returns the workstream's resolution workspace when the clone has one.
func (r resolutions) find(ctx context.Context, stream config.WorkstreamID) (workspace.Worktree, error) {
	w, found, err := r.git.Workspace(ctx, string(stream))
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
func (r resolutions) Acquire(ctx context.Context, req coreadapter.WorkspaceRequest) (coreadapter.WorkspaceLease, error) {
	if err := ctx.Err(); err != nil {
		return coreadapter.WorkspaceLease{}, err
	}
	scope := req.Scope
	if scope.Role != masonRole || scope.Thread != driftMasonAgent || scope.Workstream == "" {
		return coreadapter.WorkspaceLease{}, errors.New("a drift resolution workspace is lent to a drift mason turn alone")
	}
	stream := config.WorkstreamID(scope.Workstream)
	w, err := r.find(ctx, stream)
	if err != nil {
		return coreadapter.WorkspaceLease{}, err
	}
	return coreadapter.WorkspaceLease{Workspace: coreadapter.Workspace{ID: driftsDirectory + "/" + string(stream), Directory: w.Path, Access: req.Access}, Lease: noLease{}}, nil
}

// selection selects the whole of the resolution workspace, but its VCS
// metadata, as the drift mason turn's view, run with execution. The turn
// may read and write files, file an amendment and call done, and nothing
// else.
func (r resolutions) selection(ctx context.Context, scope coreadapter.Scope, execution coreadapter.ExecutionSettings) (isolation.Selection, error) {
	w, err := r.find(ctx, config.WorkstreamID(scope.Workstream))
	if err != nil {
		return isolation.Selection{}, err
	}
	paths, err := viewPaths(w.Path)
	return isolation.Selection{Paths: paths, Execution: execution, Narrow: &coreadapter.Capabilities{Tools: []string{"file_read", "file_write", questions.AmendTool, doneTool}, WriteFiles: true, Execute: true}}, err
}

// capture copies a drift mason turn's view back into its resolution
// workspace, whatever the turn's result, as a unit's capture does.
func (r resolutions) capture(ctx context.Context, scope coreadapter.Scope, view *isolation.FileView) error {
	w, err := r.find(ctx, config.WorkstreamID(scope.Workstream))
	if err != nil {
		return err
	}
	return mirror(view.Workspace().Directory, w.Path)
}

// records returns the recorded revisions of drift/rebase.json for drift
// rebase k, in order.
func (d drifter) records(stream config.WorkstreamID, k int) ([]DriftRebase, error) {
	return driftRebases(d.repository, stream, k)
}

// driftRebases returns the recorded revisions of drift/rebase.json for
// drift rebase k of the workstream, in order.
func driftRebases(repository *trace.Repository, stream config.WorkstreamID, k int) ([]DriftRebase, error) {
	docs, err := trace.Read[trace.Document](repository, stream)
	if err != nil {
		return nil, err
	}
	var out []DriftRebase
	for _, doc := range docs {
		if doc.ID != driftDocument {
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

// latestDriftRecord returns the latest record of the workstream's latest
// drift rebase, and whether one is.
func latestDriftRecord(repository *trace.Repository, stream config.WorkstreamID) (DriftRebase, bool, error) {
	state, err := repository.Workflow(stream, driftSubject)
	if err != nil {
		return DriftRebase{}, false, err
	}
	k, err := driftNumber(state.Value)
	if err != nil || k == 0 {
		return DriftRebase{}, false, err
	}
	records, err := driftRebases(repository, stream, k)
	if err != nil || len(records) == 0 {
		return DriftRebase{}, false, err
	}
	return records[len(records)-1], true, nil
}

// resolvingMove returns the drift rebase whose conflicts the workstream's
// drift mason resolves, and whether one is being resolved.
func resolvingMove(repository *trace.Repository, stream config.WorkstreamID) (trace.UpstreamMove, bool, error) {
	r, found, err := latestDriftRecord(repository, stream)
	if err != nil || !found || !resolving(r.Outcome) {
		return trace.UpstreamMove{}, false, err
	}
	return r.move(), true, nil
}

// carriedOnto returns the workstream's latest drift rebase when it moved
// the feature branch to onto, and whether it did: a unit rebased onto onto
// is carried by that drift rebase.
func carriedOnto(repository *trace.Repository, stream config.WorkstreamID, onto string) (trace.UpstreamMove, bool, error) {
	r, found, err := latestDriftRecord(repository, stream)
	if err != nil || !found || (r.Outcome != driftCarrying && r.Outcome != driftRebased) || r.Commit != onto {
		return trace.UpstreamMove{}, false, err
	}
	return r.move(), true, nil
}

// unitDrift returns the drift rebase whose carry produced the candidate the
// unit's latest report names, and whether one did: the unit's latest
// rebase, clean and onto the feature branch a drift rebase moved, made that
// candidate.
func unitDrift(repository *trace.Repository, stream config.WorkstreamID, unit string) (trace.UpstreamMove, bool, error) {
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
	return carriedOnto(repository, stream, rebased.Onto)
}

// conflicted returns every path a stop of drift rebase k left conflicted,
// sorted.
func (d drifter) conflicted(stream config.WorkstreamID, k int) ([]string, error) {
	records, err := d.records(stream, k)
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

// conflict records that the replay of drift rebase k conflicted: the next
// revision of drift/rebase.json and the drift subject's move to
// conflicted-<k>, with an upstream moved event, in one commit. The branch and the seal stay; the
// resolution starts from this record.
func (d drifter) conflict(ctx context.Context, stream config.WorkstreamID, rebase DriftRebase) error {
	doc, err := d.document(stream, rebase)
	if err != nil {
		return err
	}
	state, err := d.repository.Workflow(stream, driftSubject)
	if err != nil {
		return err
	}
	transition, _ := driftIDs(rebase.Drift)
	reason := fmt.Sprintf("feature branch %s does not rebase cleanly onto %s/%s at %s: %s conflicted; the branch stays at %s and the seal is unchanged until a mason's resolution of the conflicts against the sealed spec is approved by a reviewer", rebase.Branch, rebase.Upstream.Remote, rebase.Upstream.Branch, rebase.Upstream.Commit, strings.Join(rebase.Conflicts, ", "), rebase.Before)
	id := transition + "-" + driftConflicted
	visible := fmt.Sprintf("feature branch %s conflicts with upstream in %s; a mason resolves the conflicts against the sealed spec, and the branch and the seal stay until a reviewer approves the resolution", rebase.Branch, strings.Join(rebase.Conflicts, ", "))
	tx := trace.Transaction{ExpectedVersion: state.Version,
		Transition: trace.Transition{Header: d.header(id, stream, "", rebase.Operation, d.s.now()), Subject: driftSubject, From: state.Value, To: fmt.Sprintf("%s-%d", driftConflicted, rebase.Drift), Reason: reason},
		Events:     []trace.Event{trace.UpstreamMoved(id, stream, rebase.move(), visible)}}
	if _, err := d.repository.RecordDocumentsWith(ctx, []trace.Document{doc}, tx); err != nil && !errors.Is(err, trace.ErrConflict) {
		return err
	}
	return nil
}

// advance records the next step of a resolution as the next revision of
// drift/rebase.json and returns it.
func (d drifter) advance(ctx context.Context, stream config.WorkstreamID, next DriftRebase) (DriftRebase, error) {
	return next, d.recordReplay(ctx, stream, next)
}

// resolve drives the resolution of conflicted drift rebase k from its
// latest record, one step at a time, and returns the replayed record once
// a reviewer approved the resolved candidate. Until then it returns an
// error that wraps errDriftAwaits, and the feature branch and the seal stay
// where they are.
//
// The replay runs in the workstream's resolution workspace, created from the
// branch's old tip on its own branch. At each commit that conflicts it stops
// with the conflict markers in the workspace, the stop is recorded, and the
// drift mason gets one turn that names the conflicted paths and carries the
// sealed spec. Once the mason's turn ends with no marker left in those
// paths, the service stages the workspace and goes on with the replay. The
// replayed branch is the candidate a reviewer reads against the sealed
// spec: approval replays the feature branch to it, and material findings
// return it to the mason, whose work the service snapshots as the next
// candidate. The resolution workspace is removed once the feature branch
// moves. Every step reads the workspace and the threads first, so a
// retry or a restart goes on from where the last one stopped and queues no
// turn twice.
func (d drifter) resolve(ctx context.Context, stream config.WorkstreamID, rebase DriftRebase) (DriftRebase, error) {
	w, err := d.resolution(ctx, stream, rebase)
	if err != nil {
		return rebase, err
	}
	g := driftWorkspaces(d.cfg)
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
		case !replaying && head == rebase.Before && rebase.Outcome == driftConflicted:
			if err := d.s.step("drift-resolving"); err != nil {
				return rebase, err
			}
			if _, _, err := g.ReplayIn(ctx, w, rebase.Upstream.Commit, requested); err != nil {
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
			if stop != rebase.Stop || rebase.Outcome != driftConflicted {
				next := rebase
				next.Outcome, next.Round, next.Stop, next.Conflicts = driftConflicted, rebase.Round+1, stop, unmerged
				if rebase, err = d.advance(ctx, stream, next); err != nil {
					return rebase, err
				}
			}
			turn := driftResolveTurnID(rebase.Drift, rebase.Round)
			prompt, err := d.resolvePrompt(ctx, stream, rebase)
			if err != nil {
				return rebase, err
			}
			if done, err := d.masonDone(ctx, stream, w, turn, prompt, rebase.Conflicts); err != nil || !done {
				return rebase, awaiting(rebase, err, "its mason's resolution of %s", strings.Join(rebase.Conflicts, ", "))
			}
			if err := d.s.step("drift-staging"); err != nil {
				return rebase, err
			}
			if _, _, err := g.ContinueReplay(ctx, w, requested); err != nil {
				return rebase, err
			}
			continue
		}
		switch rebase.Outcome {
		case driftConflicted:
			next := rebase
			next.Outcome, next.Candidate, next.Review = driftResolved, head, rebase.Review+1
			if rebase, err = d.advance(ctx, stream, next); err != nil {
				return rebase, err
			}
			continue
		case driftRejected:
			paths, err := d.conflicted(stream, rebase.Drift)
			if err != nil {
				return rebase, err
			}
			prompt, err := d.fixPrompt(stream, rebase)
			if err != nil {
				return rebase, err
			}
			if done, err := d.masonDone(ctx, stream, w, driftFixTurnID(rebase.Drift, rebase.Review), prompt, paths); err != nil || !done {
				return rebase, awaiting(rebase, err, "its mason's answer to review %d", rebase.Review)
			}
			candidate, err := g.Snapshot(ctx, w, rebase.Upstream.Commit)
			if err != nil {
				return rebase, err
			}
			next := rebase
			next.Outcome, next.Candidate, next.Review, next.Verdict = driftResolved, candidate, rebase.Review+1, nil
			if rebase, err = d.advance(ctx, stream, next); err != nil {
				return rebase, err
			}
			continue
		}
		verdict, found, err := d.reviewed(ctx, stream, rebase)
		if err != nil || !found {
			return rebase, awaiting(rebase, err, "review %d of candidate %s", rebase.Review, rebase.Candidate)
		}
		next := rebase
		next.Verdict = &verdict
		if verdict.Decision != "satisfactory" {
			next.Outcome = driftRejected
			if rebase, err = d.advance(ctx, stream, next); err != nil {
				return rebase, err
			}
			continue
		}
		next.Outcome, next.Commit = driftReplayed, rebase.Candidate
		if rebase, err = d.advance(ctx, stream, next); err != nil {
			return rebase, err
		}
		return rebase, d.s.step("drift-approved")
	}
}

// awaiting returns err, or the errDriftAwaits a resolution step waiting on
// what returns.
func awaiting(rebase DriftRebase, err error, format string, args ...any) error {
	if err != nil {
		return err
	}
	return fmt.Errorf("drift rebase %d awaits %s: %w", rebase.Drift, fmt.Sprintf(format, args...), errDriftAwaits)
}

// resolution returns the workstream's resolution workspace for drift rebase
// k, creating it on driftBranch from the feature branch's old tip when the
// clone has none. One a drift rebase before this one left is removed first.
func (d drifter) resolution(ctx context.Context, stream config.WorkstreamID, rebase DriftRebase) (workspace.Worktree, error) {
	g := driftWorkspaces(d.cfg)
	branch := driftBranch(stream, rebase.Drift)
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

// release removes the workstream's resolution workspace when the clone has
// one. Its branch stays.
func (d drifter) release(ctx context.Context, stream config.WorkstreamID) error {
	g := driftWorkspaces(d.cfg)
	w, found, err := g.Workspace(ctx, string(stream))
	if err != nil || !found {
		return err
	}
	return g.Release(ctx, w)
}

// masonDone drives the drift mason turn that begins with turn and reports
// whether it is done: the thread's latest turn, turn or a follow-up of it,
// ended, and none of paths carries a conflict marker in the workspace. It
// queues turn when the thread does not have it. An interrupted turn has its
// surviving view copied back and one continuation queued. A turn that ended
// with markers left gets one reminder that names them.
func (d drifter) masonDone(ctx context.Context, stream config.WorkstreamID, w workspace.Worktree, turn, prompt string, paths []string) (bool, error) {
	if err := d.ensureThread(ctx, stream, driftMasonAgent, masonRole); err != nil {
		return false, err
	}
	if err := d.recoverView(ctx, stream, driftMasonAgent, func() (string, error) { return w.Path, nil }); err != nil {
		return false, err
	}
	th, err := d.repository.Thread(stream, driftMasonAgent)
	if err != nil {
		return false, err
	}
	i := slices.IndexFunc(th.Turns, func(q trace.QueuedTurn) bool { return q.Request.TurnID == turn })
	if i < 0 {
		profile, _, err := d.s.roleExecution(d.cfg, masonRole)
		if err != nil {
			return false, err
		}
		req := trace.TurnRequest{Header: trace.Header{Schema: "osmia.trace.turn-request", Version: trace.Version, ID: "request_" + turn, Revision: 1, Project: d.repository.Project(), Workstream: stream, At: d.s.now(), Actor: foremanActor, Cause: driftDocument, Depth: 1},
			AgentID: driftMasonAgent, ThreadID: driftMasonAgent, TurnID: turn, Profile: profile, SystemPrompt: driftMasonSystemPrompt(d.cfg.Project), Prompt: prompt}
		_, err = d.repository.EnqueueTurn(ctx, req)
		return false, err
	}
	last := th.Turns[len(th.Turns)-1]
	switch last.Status() {
	case "interrupted":
		return false, d.follow(ctx, stream, last, "recover", last.Request.Prompt+"\n\n"+interruption(last)+" Your view holds the files that turn left. Go on from them, and call done once the resolution is complete.")
	case "idle":
	default:
		return false, nil
	}
	marked, err := driftWorkspaces(d.cfg).MarkedFiles(w, paths)
	if err != nil || len(marked) == 0 {
		return err == nil, err
	}
	return false, d.follow(ctx, stream, last, "markers", fmt.Sprintf("Your last turn ended, and these files still carry conflict markers: %s. Resolve each conflict against the sealed spec, remove every marker, and call done again.", strings.Join(marked, ", ")))
}

// follow queues, once, the drift mason turn that follows turn last, named
// by kind and last's sequence, with prompt.
func (d drifter) follow(ctx context.Context, stream config.WorkstreamID, last trace.QueuedTurn, kind, prompt string) error {
	req := last.Request
	req.TurnID = fmt.Sprintf("%s-%s-%d", driftMasonAgent, kind, last.Sequence)
	req.ID = "request_" + req.TurnID
	req.At = d.s.now()
	if last.Response != nil {
		req.Cause = last.Response.ID
	}
	req.Prompt = prompt
	profile, _, err := d.s.roleExecution(d.cfg, masonRole)
	if err != nil {
		return err
	}
	req.Profile = profile
	th, err := d.repository.Thread(stream, driftMasonAgent)
	if err != nil {
		return err
	}
	if slices.ContainsFunc(th.Turns, func(q trace.QueuedTurn) bool { return q.Request.TurnID == req.TurnID }) {
		return nil
	}
	_, err = d.repository.EnqueueTurn(ctx, req)
	return err
}

// reviewed returns the verdict of review n of the resolved candidate, and
// whether its turn recorded one. It queues the review turn when the
// reviewer's thread does not have it, and one continuation of a review a
// stop interrupted.
func (d drifter) reviewed(ctx context.Context, stream config.WorkstreamID, rebase DriftRebase) (UnitVerdict, bool, error) {
	if err := d.ensureThread(ctx, stream, driftReviewerAgent, reviewerRole); err != nil {
		return UnitVerdict{}, false, err
	}
	th, err := d.repository.Thread(stream, driftReviewerAgent)
	if err != nil {
		return UnitVerdict{}, false, err
	}
	turn := driftReviewTurnID(rebase.Drift, rebase.Review)
	i := slices.IndexFunc(th.Turns, func(q trace.QueuedTurn) bool { return q.Request.TurnID == turn })
	if i < 0 {
		prompt, err := d.reviewPrompt(ctx, stream, rebase)
		if err != nil {
			return UnitVerdict{}, false, err
		}
		profile, _, err := d.s.roleExecution(d.cfg, reviewerRole)
		if err != nil {
			return UnitVerdict{}, false, err
		}
		req := trace.TurnRequest{Header: trace.Header{Schema: "osmia.trace.turn-request", Version: trace.Version, ID: "request_" + turn, Revision: 1, Project: d.repository.Project(), Workstream: stream, At: d.s.now(), Actor: foremanActor, Cause: driftDocument, Depth: 1},
			AgentID: driftReviewerAgent, ThreadID: driftReviewerAgent, TurnID: turn, Profile: profile, SystemPrompt: "You are the reviewer of a resolution of the conflicts a rebase onto upstream left in a feature branch. Read only the supplied evidence and call verdict with your decision. You cannot edit the candidate.", Prompt: prompt}
		_, err = d.repository.EnqueueTurn(ctx, req)
		return UnitVerdict{}, false, err
	}
	if th.Status == "interrupted" && th.Active != "" {
		if err := d.repository.AbandonTurn(ctx, stream, driftReviewerAgent, th.Active, d.s.now()); err != nil {
			return UnitVerdict{}, false, err
		}
		if th, err = d.repository.Thread(stream, driftReviewerAgent); err != nil {
			return UnitVerdict{}, false, err
		}
	}
	last := th.Turns[len(th.Turns)-1]
	switch last.Status() {
	case "interrupted":
		req := last.Request
		req.TurnID = fmt.Sprintf("%s-recover-%d", turn, last.Sequence)
		req.ID, req.At = "request_"+req.TurnID, d.s.now()
		if last.Response != nil {
			req.Cause = last.Response.ID
		}
		req.Prompt += "\n\nThe previous turn was interrupted. Review the same candidate and record a verdict."
		if slices.ContainsFunc(th.Turns, func(q trace.QueuedTurn) bool { return q.Request.TurnID == req.TurnID }) {
			return UnitVerdict{}, false, nil
		}
		_, err := d.repository.EnqueueTurn(ctx, req)
		return UnitVerdict{}, false, err
	case "idle":
	default:
		return UnitVerdict{}, false, nil
	}
	outcome := last.Response.Result.Outcome
	if outcome == nil || outcome.Status != verdictOutcome {
		return UnitVerdict{}, false, nil
	}
	var verdict UnitVerdict
	if err := json.Unmarshal([]byte(outcome.Report), &verdict); err != nil {
		return UnitVerdict{}, false, fmt.Errorf("the verdict of turn %s: %w", last.Request.TurnID, err)
	}
	return verdict, true, nil
}

// ensureThread creates the workstream's drift agent thread when it is
// missing.
func (d drifter) ensureThread(ctx context.Context, stream config.WorkstreamID, agent, role string) error {
	if _, err := d.repository.Thread(stream, agent); err == nil || !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return d.repository.CreateThread(ctx, trace.Agent{Header: trace.Header{Schema: "osmia.trace.agent", Version: trace.Version, ID: agent, Revision: 1, Project: d.repository.Project(), Workstream: stream, At: d.s.now(), Actor: foremanActor, Cause: driftDocument}, Role: role, ThreadID: agent})
}

func driftMasonSystemPrompt(p config.Project) string {
	return fmt.Sprintf("You are a mason of the %s project (%s). You resolve the conflicts the service's rebase of a feature branch onto upstream left, in a workspace of its own whose files are your view. You hold no version control tool: the service records your work and goes on with the rebase. Resolve against the sealed spec, change nothing the conflicts do not need, and call done when no conflict marker is left. When upstream's change alters what a sealed criterion means, call %s: the owner decides the spec, never your resolution.", p.Name, p.Upstream, questions.AmendTool)
}

// driftAmendGuidance tells a drift mason how to raise an upstream change to
// what a sealed criterion means.
func driftAmendGuidance(rebase DriftRebase) string {
	return fmt.Sprintf("If upstream's change alters what a sealed criterion means, do not settle that in the resolution: call %s with the spec#<n> criteria it changes, the change the spec needs and why. The request cites upstream commit %s and goes to the owner; resolve the conflicts against the sealed spec as it stands all the same.", questions.AmendTool, rebase.Upstream.Commit)
}

// sealedSpec returns the workstream's sealed spec rendered for a prompt.
func (d drifter) sealedSpec(stream config.WorkstreamID) (string, error) {
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

func (d drifter) resolvePrompt(ctx context.Context, stream config.WorkstreamID, rebase DriftRebase) (string, error) {
	spec, err := d.sealedSpec(stream)
	if err != nil {
		return "", err
	}
	c, err := driftWorkspaces(d.cfg).Commit(ctx, rebase.Stop)
	if err != nil {
		return "", err
	}
	subject, _, _ := strings.Cut(c.Message, "\n")
	return fmt.Sprintf(`Resolve the conflicts of feature branch %s with upstream.

The service is rebasing the workstream's feature branch %s from %s onto %s/%s at %s, one commit at a time, in a workspace of its own whose files are your view. Replaying commit %s (%q) conflicted, and these files of your view are conflicted:
- %s

A conflicted file carries conflict markers: the lines between "<<<<<<<" and "=======" are the branch as rebased onto upstream so far, and those between "=======" and ">>>>>>>" are the commit being replayed. Resolve every conflict against the sealed spec below, so that what upstream now holds and what the feature branch built both stand, and remove every marker. Change nothing the conflicts do not need. Then call done with the outcome of your resolution and end your turn. The service goes on with the rebase once no conflicted file carries a marker, and a reviewer reads the resolved branch against the sealed spec before the feature branch moves.

%s

%s`, rebase.Branch, rebase.Branch, rebase.Before, rebase.Upstream.Remote, rebase.Upstream.Branch, rebase.Upstream.Commit, rebase.Stop, subject, strings.Join(rebase.Conflicts, "\n- "), driftAmendGuidance(rebase), spec), nil
}

func (d drifter) fixPrompt(stream config.WorkstreamID, rebase DriftRebase) (string, error) {
	spec, err := d.sealedSpec(stream)
	if err != nil {
		return "", err
	}
	var findings []string
	if rebase.Verdict != nil {
		for _, f := range rebase.Verdict.Findings {
			findings = append(findings, fmt.Sprintf("- %s (%s): %s Action: %s", f.Criterion, f.Severity, f.Evidence, f.Action))
		}
	}
	return fmt.Sprintf(`The reviewer did not approve your resolution of the conflicts of feature branch %s with upstream. Their findings:
%s

Your view holds the resolved branch, candidate %s. Address every finding against the sealed spec below, keep what upstream holds and what the feature branch built, and leave no conflict marker. Then call done with the outcome of your changes and end your turn. The reviewer reads the resolution again before the feature branch moves.

%s

%s`, rebase.Branch, strings.Join(findings, "\n"), rebase.Candidate, driftAmendGuidance(rebase), spec), nil
}

func (d drifter) reviewPrompt(ctx context.Context, stream config.WorkstreamID, rebase DriftRebase) (string, error) {
	spec, err := d.sealedSpec(stream)
	if err != nil {
		return "", err
	}
	paths, err := d.conflicted(stream, rebase.Drift)
	if err != nil {
		return "", err
	}
	g := driftWorkspaces(d.cfg)
	base, err := g.MergeBase(ctx, rebase.Before, rebase.Upstream.Commit)
	if err != nil {
		return "", err
	}
	before, err := g.Diff(ctx, base, rebase.Before)
	if err != nil {
		return "", err
	}
	after, err := g.Diff(ctx, rebase.Upstream.Commit, rebase.Candidate)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf(`Review the resolution of the conflicts of feature branch %s with upstream against the sealed spec below.

The service rebased feature branch %s from %s onto %s/%s at %s. The rebase conflicted in:
- %s

A mason resolved the conflicts, and the resolved branch is candidate %s. The feature branch stays at %s until you approve it: a satisfactory verdict moves the feature branch and the seal to the candidate, and material findings return the resolution to the mason with your findings. Check that each conflicted file keeps both what upstream now holds and what the feature branch built, that nothing else of the feature branch's change was lost or altered, and that the sealed criteria still hold. Cite criteria as spec#<n> in your evidence and findings.

The feature branch's change before the rebase, from %s to %s:
%s

The candidate's change on upstream, from %s to %s:
%s

%s`, rebase.Branch, rebase.Branch, rebase.Before, rebase.Upstream.Remote, rebase.Upstream.Branch, rebase.Upstream.Commit, strings.Join(paths, "\n- "), rebase.Candidate, rebase.Before, base, rebase.Before, before, rebase.Upstream.Commit, rebase.Candidate, after, spec), nil
}
