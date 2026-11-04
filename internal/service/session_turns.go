package service

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/isolation"
	"github.com/kpenfound/osmia/internal/thread"
	"github.com/kpenfound/osmia/internal/trace"
)

// sessionRole and sessionAgentID both name the session's one agent: there is
// exactly one agent, and one durable thread, per session.
const sessionRole = "session"
const sessionAgentID = sessionRole

// sessionRolePrompt opens the system prompt of every session turn: the agent
// is told it is in a direct session with the owner, that the owner alone
// commits, checks and pushes its work, and that it answers in plain chat.
const sessionRolePrompt = "You are in a direct session with the owner, working in a dedicated worktree of their project. " +
	"There is no factory around you: no spec, no plan, no units, no reviewer and no chief of staff. " +
	"The owner reviews your changes, commits them, runs checks on the committed work and pushes the branch themselves; you never commit, check or push. " +
	"Read, write and run commands in the worktree; use show_image(path) to attach a PNG or JPEG from your current worktree to the chat. " +
	"Answer the owner in plain chat."

// resumableEngine makes a session's backend model session resumable: it
// defers to the engine's own CheckResume when it has one, as a fake engine
// used for testing does, so tests can fake both a resumable and an
// unresumable saved session, exactly as any other role's CheckResume call
// would (this wrapper changes nothing for them). Only when the engine has no
// CheckResume of its own does it reach for a heuristic: a same-backend,
// same-model, same-effort match, trusted because agentcore offers
// no documented way to verify a saved session's availability and
// compatibility before starting it (TODO(upstream): a documented
// resumability check would let this verify more and trust less). That
// heuristic answers only when thread.ResumeFallbackAllowed(ctx) is true,
// which thread.Runner sets only for a role thread.Runner.ResumableRoles
// grants (the session role alone in production), so it never reaches a
// turn of another role even though this same wrapper runs every role's
// CheckResume call. A session that in fact cannot be resumed fails visibly
// at launch, the same way any other infrastructure failure does, and the
// thread runner's own retry and fallback handle it.
type resumableEngine struct{ coreadapter.Engine }

func (e resumableEngine) SkillDirs() []string {
	if source, ok := e.Engine.(coreadapter.SkillSource); ok {
		return source.SkillDirs()
	}
	return nil
}

func (e resumableEngine) CheckResume(ctx context.Context, previous, next coreadapter.Profile, session coreadapter.BackendSession) error {
	if checker, ok := e.Engine.(coreadapter.ResumeChecker); ok {
		return checker.CheckResume(ctx, previous, next, session)
	}
	if !thread.ResumeFallbackAllowed(ctx) {
		return &coreadapter.UnsupportedError{Capability: "resume", Reason: "execution engine cannot verify saved session compatibility and availability"}
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if previous.Backend != next.Backend || session.Backend != next.Backend || previous.Model != next.Model || previous.Effort != next.Effort {
		return coreadapter.ErrResumeUnavailable
	}
	return nil
}

// sessionWorkspaces lends a session turn its session's existing worktree,
// created when the session opened and reused unchanged by every later
// turn, so the agent's changes persist into the next turn.
type sessionWorkspaces struct{ cfg *config.Config }

var _ coreadapter.Workspaces = sessionWorkspaces{}

func (w sessionWorkspaces) Acquire(ctx context.Context, req coreadapter.WorkspaceRequest) (coreadapter.WorkspaceLease, error) {
	if req.Scope.Role != sessionRole || req.Scope.Workstream == "" {
		return coreadapter.WorkspaceLease{}, errors.New("a session workspace is lent to a session turn alone")
	}
	stream := config.WorkstreamID(req.Scope.Workstream)
	g := workspaces(w.cfg, sessionsDirectory, config.WorkspacesGit)
	wt, found, err := g.Workspace(ctx, string(stream))
	if err != nil {
		return coreadapter.WorkspaceLease{}, err
	}
	if !found {
		return coreadapter.WorkspaceLease{}, fmt.Errorf("session %s has no worktree", stream)
	}
	return coreadapter.WorkspaceLease{Workspace: coreadapter.Workspace{ID: string(stream), Directory: wt.Path, Access: req.Access}, Lease: noLease{}}, nil
}

// sessionSelection selects the whole of a session turn's worktree, but its
// VCS metadata, as its view: the workspace tools a mason sandbox has, with
// the show_image attachment tool.
func sessionSelection(ctx context.Context, cfg *config.Config, scope coreadapter.Scope, execution coreadapter.ExecutionSettings) (isolation.Selection, error) {
	stream := config.WorkstreamID(scope.Workstream)
	g := workspaces(cfg, sessionsDirectory, config.WorkspacesGit)
	wt, found, err := g.Workspace(ctx, string(stream))
	if err != nil {
		return isolation.Selection{}, err
	}
	if !found {
		return isolation.Selection{}, fmt.Errorf("session %s has no worktree", stream)
	}
	paths, err := viewPaths(wt.Path)
	return isolation.Selection{Paths: paths, Execution: execution}, err
}

// capture copies a session turn's view back into its session's worktree,
// whatever the turn's result, as a unit's or a drift mason's capture does,
// so a file the agent wrote persists into the session's next turn.
func (w sessionWorkspaces) capture(ctx context.Context, scope coreadapter.Scope, view *isolation.FileView) error {
	stream := config.WorkstreamID(scope.Workstream)
	g := workspaces(w.cfg, sessionsDirectory, config.WorkspacesGit)
	wt, found, err := g.Workspace(ctx, string(stream))
	if err != nil {
		return err
	}
	if !found {
		return fmt.Errorf("session %s has no worktree", stream)
	}
	return mirror(view.Workspace().Directory, wt.Path)
}

// sessionRelay runs the part of the thread dispatcher's Relay hook that
// applies to a session's completed turn: when the turn's last attempt
// replayed history because its model session could not be resumed, it
// records a service entry saying so before returning the session to idle,
// so the entry already exists by the time any reader sees the session go
// idle, and reaches the next turn exactly once. A retry of either effect
// records nothing more.
func sessionRelay(ctx context.Context, repository *trace.Repository, now func() time.Time, th trace.Thread, q trace.QueuedTurn) error {
	stream := config.WorkstreamID(q.Request.Workstream)
	project := repository.Project()
	at := now()
	if len(q.Attempts) > 0 {
		last := q.Attempts[len(q.Attempts)-1]
		if last.Path == "replay" && last.SourceSession.ID != "" {
			if err := recordSessionEntry(ctx, repository, project, stream, "context-rebuilt-"+q.Request.TurnID, at,
				"Your earlier model session could not be resumed, so this turn's context was rebuilt from the thread's history."); err != nil {
				return err
			}
		}
	}
	return setSessionState(ctx, repository, project, stream, at, "session-idle-"+q.Request.TurnID, "session-turn-completed", trace.SessionIdle)
}
