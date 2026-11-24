package service

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/intake"
	"github.com/kpenfound/osmia/internal/isolation"
	"github.com/kpenfound/osmia/internal/thread"
	"github.com/kpenfound/osmia/internal/trace"
)

const sessionRole = "session"
const sessionAgentID = sessionRole

const sessionRolePrompt = "You are in a direct session with the owner, working in a dedicated worktree of their project. " +
	"There is no factory around you: no spec, no plan, no units, no reviewer and no chief of staff. " +
	"The owner reviews your changes, commits them, runs checks on the committed work and pushes the branch themselves; you never commit, check or push. " +
	"Read, write and run commands in the worktree; use show_image(path) to attach a PNG or JPEG from your current worktree to the chat. " +
	"Answer the owner in plain chat."

type resumableEngine struct{ coreadapter.Engine }

func (e resumableEngine) SkillDirs() []string {
	if source, ok := e.Engine.(coreadapter.SkillSource); ok {
		return source.SkillDirs()
	}
	return nil
}

func (e resumableEngine) PrepareSkills(ctx context.Context, refs []string) ([]string, error) {
	if source, ok := e.Engine.(coreadapter.SkillPreparing); ok {
		return source.PrepareSkills(ctx, refs)
	}
	return nil, &coreadapter.UnsupportedError{Capability: "skills", Reason: "execution engine prepares no skills"}
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
	return coreadapter.WorkspaceLease{Workspace: coreadapter.Workspace{ID: string(stream), Directory: wt.Path, Access: req.Access}, Lease: intake.NoLease{}}, nil
}

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
