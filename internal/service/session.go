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

	"github.com/kpenfound/agentcore/vcs"
	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/trace"
)

// sessionsDirectory is the directory under the root holding session
// worktrees, outside the target repository and every other workspace kind.
const sessionsDirectory = "sessions"

// sessionActor is the provenance of every record a session's opening makes.
var sessionActor = trace.Actor{Kind: "owner", ID: "local"}

// sessionBranch names the branch a session's worktree is created on.
func sessionBranch(stream config.WorkstreamID) string { return "osmia/session/" + string(stream) }

// pendingSession is the journal of one session open in progress. It is
// written before any other side effect and removed last, so a restart finds
// an interrupted open and finishes or discards it with the same identity:
// the result is one usable session or none, never a duplicate and never an
// orphaned worktree.
type pendingSession struct {
	Version    int                 `json:"version"`
	Project    config.ProjectID    `json:"project"`
	Workstream config.WorkstreamID `json:"workstream"`
	Profile    string              `json:"profile"`
	Title      string              `json:"title,omitempty"`
	BaseBranch string              `json:"base_branch"`
	Base       string              `json:"base"`
	Branch     string              `json:"branch"`
}

func (s *Service) writePendingSession(p pendingSession) error {
	path, err := s.current().Root.PendingSessionOpen()
	if err != nil {
		return err
	}
	data, err := json.MarshalIndent(p, "", "  ")
	if err != nil {
		return err
	}
	return writeFileSynced(path, append(data, '\n'))
}

func (s *Service) readPendingSession() (*pendingSession, error) {
	path, err := s.current().Root.PendingSessionOpen()
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var p pendingSession
	d := json.NewDecoder(strings.NewReader(string(data)))
	d.DisallowUnknownFields()
	if err := d.Decode(&p); err != nil {
		return nil, err
	}
	if p.Version != 1 || config.CheckProjectIDs(p.Project) != nil || config.CheckWorkstreamIDs(p.Workstream) != nil || p.Profile == "" || p.Base == "" || p.Branch == "" {
		return nil, errors.New("invalid session open journal")
	}
	return &p, nil
}

// errSessionDiscarded reports that an open's journal was discarded, so its
// replay must not create a worktree or trace.
var errSessionDiscarded = errors.New("the session open was discarded")

func sessionIncomplete(id config.WorkstreamID, err error) *APIError {
	reason := "storage failure"
	switch {
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		reason = "interrupted"
	case errors.Is(err, trace.ErrLocked):
		reason = "trace locked"
	}
	return &APIError{Code: Internal, Message: fmt.Sprintf("opening session %s is incomplete (%s); restart the service to finish or discard it", id, reason)}
}

// sameSession reports whether a pending open answers the given request.
func sameSession(p pendingSession, project config.ProjectID, req SessionOpenRequest) bool {
	return p.Project == project && p.Profile == req.Profile && p.Title == strings.TrimSpace(req.Title)
}

// openSession opens a session: it validates the project and the profile,
// which must be one the service can run for mason turns in that project,
// before any side effect, then records the workstream and creates its
// dedicated worktree at the default branch's head, through the existing
// workspace machinery under the service root. Session opens are serialized,
// so two concurrent opens on the same or different projects never race.
func (s *Service) openSession(ctx context.Context, req SessionOpenRequest) (SessionView, *APIError) {
	s.sessionMu.Lock()
	defer s.sessionMu.Unlock()
	pending, err := s.readPendingSession()
	if err != nil {
		return SessionView{}, &APIError{Code: Internal, Message: "the session open journal is unreadable; inspect session-open.json under the root"}
	}
	if pending != nil {
		view, err := s.completeSessionOpen(ctx, *pending)
		if errors.Is(err, errSessionDiscarded) {
			return SessionView{}, &APIError{Code: Conflict, Message: fmt.Sprintf("an earlier session open for project %s could not be finished and was discarded; retry", pending.Project)}
		}
		if err != nil {
			return SessionView{}, sessionIncomplete(pending.Workstream, err)
		}
		if project, api := s.projectFor(req.Project); api != nil || !sameSession(*pending, project, req) {
			return SessionView{}, &APIError{Code: Conflict, Message: fmt.Sprintf("an interrupted session open finished as %s; open another session with a new request", pending.Workstream)}
		}
		return view, nil
	}
	project, api := s.projectFor(req.Project)
	if api != nil {
		return SessionView{}, api
	}
	cfg := s.current().For(project)
	if _, _, err := cfg.Execution(masonRole, req.Profile); err != nil {
		return SessionView{}, &APIError{Code: Validation, Message: fmt.Sprintf("profile %q is not one the service can run for mason turns in project %s: %v", req.Profile, project, err)}
	}
	repository, err := s.repository(project)
	if err != nil {
		return SessionView{}, &APIError{Code: Internal, Message: fmt.Sprintf("project %s has no trace repository", project)}
	}
	streams, err := repository.Workstreams()
	if err != nil {
		return SessionView{}, &APIError{Code: Internal, Message: fmt.Sprintf("cannot read the workstreams of project %s; check the trace repository", project)}
	}
	stream, err := config.NewWorkstreamID(streams...)
	if err != nil {
		return SessionView{}, &APIError{Code: Internal, Message: "cannot generate a session identity"}
	}
	g := workspaces(cfg, sessionsDirectory, config.WorkspacesGit)
	remote, err := g.Remote(ctx, cfg.Project.Upstream)
	if err != nil {
		return SessionView{}, &APIError{Code: Internal, Message: fmt.Sprintf("cannot fetch %s from %s; check the clone's remotes: %v", cfg.Project.BaseBranch, cfg.Project.Upstream, err)}
	}
	base, err := g.Fetch(ctx, remote, cfg.Project.BaseBranch)
	if err != nil {
		return SessionView{}, &APIError{Code: Internal, Message: fmt.Sprintf("cannot fetch %s from %s: %v", cfg.Project.BaseBranch, cfg.Project.Upstream, err)}
	}
	p := pendingSession{Version: 1, Project: project, Workstream: stream, Profile: req.Profile, Title: strings.TrimSpace(req.Title), BaseBranch: cfg.Project.BaseBranch, Base: base, Branch: sessionBranch(stream)}
	if err := s.writePendingSession(p); err != nil {
		return SessionView{}, &APIError{Code: Internal, Message: "cannot write the session open journal"}
	}
	if err := s.step("session-journal-written"); err != nil {
		return SessionView{}, sessionIncomplete(stream, err)
	}
	view, err := s.completeSessionOpen(ctx, p)
	if err != nil {
		return SessionView{}, sessionIncomplete(stream, err)
	}
	return view, nil
}

// completeSessionOpen runs every step of a session open that is not yet
// done, in order: each inspects the disk or trace before acting, so running
// it again after an interruption neither repeats an effect nor skips one.
// The worktree is created before the workstream is recorded, so a
// completion that finds the worktree but no record always has one left to
// make, never one to undo. When the project is no longer active, the open
// is discarded instead: any worktree it made is released and the journal
// removed, leaving no session.
func (s *Service) completeSessionOpen(ctx context.Context, p pendingSession) (SessionView, error) {
	cfg := s.current().For(p.Project)
	if !cfg.HasProject() {
		return SessionView{}, s.discardPendingSession(ctx, p)
	}
	repository, err := s.repository(p.Project)
	if err != nil {
		return SessionView{}, s.discardPendingSession(ctx, p)
	}
	gate := sessionLock(repository, p.Workstream)
	gate.Lock()
	defer gate.Unlock()
	// A destroy can remove the journal after this replay first read it.
	// Revalidate under the same admission guard before acquiring a worktree.
	pending, err := s.readPendingSession()
	if err != nil {
		return SessionView{}, err
	}
	if pending == nil || pending.Project != p.Project || pending.Workstream != p.Workstream {
		return SessionView{}, errSessionDiscarded
	}
	if destroying, err := trace.SessionDestroyPending(cfg.Root, p.Project, p.Workstream); err != nil {
		return SessionView{}, err
	} else if destroying {
		return SessionView{}, errSessionDiscarded
	}
	g := workspaces(cfg, sessionsDirectory, config.WorkspacesGit)
	if _, err := g.Acquire(ctx, vcs.Request{Name: string(p.Workstream), Ref: p.Base, Branch: p.Branch}); err != nil {
		return SessionView{}, err
	}
	if err := s.step("session-worktree-created"); err != nil {
		return SessionView{}, err
	}
	streams, err := repository.Workstreams()
	if err != nil {
		return SessionView{}, err
	}
	if !slices.Contains(streams, p.Workstream) {
		if err := repository.CreateSessionWorkstream(ctx, p.Workstream, p.Profile, p.Base, p.Branch, p.Title, s.now(), sessionActor); err != nil {
			return SessionView{}, err
		}
	}
	if err := s.step("session-recorded"); err != nil {
		return SessionView{}, err
	}
	journal, err := s.current().Root.PendingSessionOpen()
	if err != nil {
		return SessionView{}, err
	}
	if err := os.Remove(journal); err != nil && !os.IsNotExist(err) {
		return SessionView{}, err
	}
	if err := s.step("session-journal-removed"); err != nil {
		return SessionView{}, err
	}
	view, api := s.sessionRead(string(p.Workstream))
	if api != nil {
		return SessionView{}, errors.New(api.Message)
	}
	return view, nil
}

// discardPendingSession removes a worktree an interrupted open may have made
// and the journal, leaving no session, and returns errSessionDiscarded. It
// is used only when the project the pending open names is no longer active,
// so the open cannot be finished. When the project's configuration is still
// known (it failed to open only its trace), the worktree is released
// through the ordinary git workspace machinery, which also deregisters it
// from the clone; a project removed from the active list entirely carries
// no clone path to do that with, so only the worktree's own directory under
// the root is removed, and the clone may keep a worktree registration for a
// directory that no longer exists until something prunes it.
func (s *Service) discardPendingSession(ctx context.Context, p pendingSession) error {
	root := s.current().Root
	cfg := s.current().For(p.Project)
	if cfg.HasProject() {
		g := workspaces(cfg, sessionsDirectory, config.WorkspacesGit)
		if w, found, err := g.Workspace(ctx, string(p.Workstream)); err != nil {
			return err
		} else if found {
			if err := g.Release(ctx, w); err != nil {
				return err
			}
		}
	} else {
		dir := filepath.Join(root.String(), sessionsDirectory, string(p.Project), string(p.Workstream))
		if err := os.RemoveAll(dir); err != nil {
			return err
		}
	}
	journal, err := root.PendingSessionOpen()
	if err != nil {
		return err
	}
	if err := os.Remove(journal); err != nil && !os.IsNotExist(err) {
		return err
	}
	return errSessionDiscarded
}

// recoverSessionOpen finishes or discards a session open a restart
// interrupted, as completeSessionOpen describes.
func (s *Service) recoverSessionOpen(ctx context.Context) error {
	s.sessionMu.Lock()
	defer s.sessionMu.Unlock()
	pending, err := s.readPendingSession()
	if err != nil || pending == nil {
		return err
	}
	_, err = s.completeSessionOpen(ctx, *pending)
	if errors.Is(err, errSessionDiscarded) {
		return nil
	}
	return err
}

// sessionView converts a workstream's status, already known to be a
// session, into the API's view of it.
func sessionView(project config.ProjectID, w trace.WorkstreamStatus) SessionView {
	return SessionView{Workstream: w.Workstream, Project: project, Profile: w.Profile, Base: w.Base, Branch: w.Branch, Title: w.Title, State: w.State, CreatedAt: w.CreatedAt}
}

// sessionList reports every session of every active project, or only those
// of the named project when it is not empty, newest first.
func (s *Service) sessionList(rawProject string) (SessionsResponse, *APIError) {
	var filter config.ProjectID
	if rawProject != "" {
		id, err := config.ParseProjectID(rawProject)
		if err != nil {
			return SessionsResponse{}, &APIError{Code: Validation, Message: "project must be a project ID: p_ followed by 32 lowercase hexadecimal digits"}
		}
		filter = id
	}
	cfg, projects := s.runtimes()
	out := []SessionView{}
	for _, active := range projects {
		if !cfg.Active(active.id) || (filter != "" && active.id != filter) {
			continue
		}
		list, err := active.repository.Statuses()
		if err != nil {
			return SessionsResponse{}, &APIError{Code: Internal, Message: fmt.Sprintf("cannot read the workstream status of project %s; check the trace repository", active.id)}
		}
		for _, w := range list {
			if w.Kind == trace.SessionKind {
				out = append(out, sessionView(active.id, w))
			}
		}
	}
	slices.SortFunc(out, func(a, b SessionView) int {
		if c := b.CreatedAt.Compare(a.CreatedAt); c != 0 {
			return c
		}
		return strings.Compare(string(a.Workstream), string(b.Workstream))
	})
	return SessionsResponse{Sessions: out}, nil
}

// sessionRead reports one session, by workstream ID, across every active
// project. A workstream that exists but is not a session is refused with
// not_found, the same as an unknown one.
func (s *Service) sessionRead(raw string) (SessionView, *APIError) {
	id, err := config.ParseWorkstreamID(raw)
	if err != nil {
		return SessionView{}, &APIError{Code: Validation, Message: "workstream must be a workstream ID: w_ followed by 32 lowercase hexadecimal digits"}
	}
	cfg, projects := s.runtimes()
	for _, active := range projects {
		if !cfg.Active(active.id) {
			continue
		}
		list, err := active.repository.Statuses()
		if err != nil {
			return SessionView{}, &APIError{Code: Internal, Message: fmt.Sprintf("cannot read the workstream status of project %s; check the trace repository", active.id)}
		}
		for _, w := range list {
			if w.Workstream != id {
				continue
			}
			if w.Kind != trace.SessionKind {
				return SessionView{}, &APIError{Code: NotFound, Message: fmt.Sprintf("session %s is not in an active project; list sessions with GET %s/sessions", id, Prefix)}
			}
			return sessionView(active.id, w), nil
		}
	}
	return SessionView{}, &APIError{Code: NotFound, Message: fmt.Sprintf("session %s is not in an active project; list sessions with GET %s/sessions", id, Prefix)}
}
