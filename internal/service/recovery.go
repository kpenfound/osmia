package service

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/kpenfound/agentcore/agent/procs"
	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/execution"
	"github.com/kpenfound/osmia/internal/ghlog"
	"github.com/kpenfound/osmia/internal/runtime"
	"github.com/kpenfound/osmia/internal/trace"
)

func (s *Service) holdsTurn(turn string) bool {
	return s.clients != nil && s.clients.Held(turn)
}

func (s *Service) recoverSessions(ctx context.Context, cfg *config.Config, repository *trace.Repository) error {
	if s.clients != nil {
		repository.HoldTurns(s.holdsTurn)
		if err := s.clients.HoldFromTrace(repository.Project(), repository); err != nil {
			return err
		}
	}
	streams, err := repository.Workstreams()
	if err != nil {
		return err
	}
	for _, stream := range streams {
		threads, err := repository.Threads(stream)
		if err != nil {
			return err
		}
		for _, th := range threads {
			if err := s.recoverThread(ctx, cfg, repository, stream, th); err != nil {
				return err
			}
		}
	}
	return nil
}

func (s *Service) recoverThread(ctx context.Context, cfg *config.Config, repository *trace.Repository, stream config.WorkstreamID, th trace.Thread) error {
	for _, q := range th.Turns {
		if !q.CompletedAt.IsZero() || q.Response != nil {
			continue
		}
		if s.holdsTurn(q.Request.TurnID) {
			continue
		}
		if q.Claim != nil {
			if th.Identity.Role == masonRole {
				continue
			}
			if err := repository.AbandonTurn(ctx, stream, th.Identity.ID, q.Request.TurnID, s.now()); err != nil {
				return fmt.Errorf("recover %s turn %s: %w", th.Identity.ID, q.Request.TurnID, err)
			}
		} else {
			if th.Active != "" && th.Identity.Role == masonRole {
				continue
			}
			dir := ""
			for _, candidate := range sessionDirectories(cfg, repository.Project(), stream, th.Identity, q.Request.TurnID) {
				info, err := os.Lstat(candidate)
				if errors.Is(err, os.ErrNotExist) {
					continue
				}
				if err != nil {
					return err
				}
				if !info.IsDir() {
					return fmt.Errorf("turn session %s is not a directory", candidate)
				}
				if dir != "" {
					return fmt.Errorf("turn %s has multiple session directories", q.Request.TurnID)
				}
				dir = candidate
			}
			if dir == "" {
				continue
			}
			if err := repository.RecoverUnclaimedTurn(ctx, stream, th.Identity.ID, q.Request.TurnID, dir, s.now()); err != nil {
				return fmt.Errorf("recover unclaimed %s turn %s: %w", th.Identity.ID, q.Request.TurnID, err)
			}
		}
	}
	if trace.ManagerLike(th.Identity.Role) && s.options.Threads != nil {
		if err := s.recoverContinuations(ctx, repository, stream, th.Identity.ID, func(turn string) error {
			return s.recoverChief(ctx, repository, stream, th.Identity.Role, th.Identity.ID, turn)
		}); err != nil {
			return err
		}
	}
	if th.Identity.Role == sessionRole && s.options.Threads != nil {
		if err := s.recoverContinuations(ctx, repository, stream, th.Identity.ID, func(turn string) error {
			return s.recoverSession(ctx, cfg, repository, stream, th.Identity.ID, turn)
		}); err != nil {
			return err
		}
	}
	return nil
}

func (s *Service) recoverWorkgroupChiefSessions(ctx context.Context, projects []*activeProject) error {
	seen := map[config.WorkgroupID]bool{}
	for _, p := range projects {
		ids, err := s.crossRepoWorkgroupIDs(ctx, p.repository)
		if err != nil {
			return err
		}
		for _, id := range ids {
			if seen[id] {
				continue
			}
			seen[id] = true
			if err := s.recoverWorkgroupChiefSession(ctx, id); err != nil {
				return fmt.Errorf("recover workgroup %s chief-of-staff thread: %w", id, err)
			}
		}
	}
	return nil
}

func (s *Service) recoverWorkgroupChiefSession(ctx context.Context, id config.WorkgroupID) error {
	_, projects := s.runtimes()
	if len(projects) == 0 {
		return nil
	}
	g, _, err := projects[0].repository.CrossRepoWorkgroup(id)
	if err != nil {
		return err
	}
	if len(g.Members) < 2 {
		return nil
	}
	members, err := s.crossRepoMemberRepos(ctx, g)
	if errors.Is(err, errWorkgroupJoining) {
		return nil
	}
	if err != nil {
		return err
	}
	wt, err := trace.OpenWorkgroupTrace(s.current().Root, id, members)
	if wt != nil && err != nil {
		wt.Close()
	}
	if err != nil {
		return err
	}
	defer wt.Close()
	host := g.Members[0]
	th, err := wt.Thread(host, trace.Manager)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if err := s.recoverThread(ctx, s.current(), wt, host, th); err != nil {
		return err
	}
	if g.Conversation != nil {
		for _, q := range th.Turns {
			if q.Response != nil && q.Status() == "interrupted" && q.Response.Result.Cancelled && q.Response.Stop == nil {
				if err := s.recoverChief(ctx, wt, host, trace.Manager, trace.Manager, q.Request.TurnID); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

func (s *Service) recoverWorkspaces(ctx context.Context, cfg *config.Config) error {
	for _, directory := range []string{branchesDirectory, unitsDirectory, driftsDirectory} {
		operations, err := workspaces(cfg, directory, config.WorkspacesJujutsu).Recover(ctx)
		if err != nil {
			return err
		}
		for _, operation := range operations {
			ghlog.Logger(ctx).Info("restored interrupted workspace operation", "directory", directory, "project", string(cfg.Project.ID), "operation", operation)
		}
	}
	return nil
}

func reapSandboxes(ctx context.Context, cfg *config.Config, sbxBin string) {
	root, project := cfg.Root.String(), string(cfg.Project.ID)
	patterns := []string{
		filepath.Join(root, "threads", project, "*", "*", "*", procs.SandboxNameFile),
		filepath.Join(root, "architect", project, "*", "*", "session", procs.SandboxNameFile),
		filepath.Join(root, "shed", project, "*", "*", "session", procs.SandboxNameFile),
		filepath.Join(root, "final", project, "*", "*", "session", procs.SandboxNameFile),
	}
	var dirs []string
	for _, pattern := range patterns {
		records, err := filepath.Glob(pattern)
		if err != nil {
			ghlog.Logger(ctx).Warn("cannot look for sandboxes interrupted sessions left", "project", project, "error", err.Error())
			continue
		}
		for _, record := range records {
			dirs = append(dirs, filepath.Dir(record))
		}
	}
	if len(dirs) == 0 {
		return
	}
	if err := procs.CleanSandboxDirs(ctx, sbxBin, dirs); err != nil {
		ghlog.Logger(ctx).Warn("cannot remove sandboxes interrupted sessions left", "project", project, "error", err.Error())
	}
}

func sessionDirectories(cfg *config.Config, project config.ProjectID, stream config.WorkstreamID, agent trace.Agent, turn string) []string {
	root := cfg.Root.String()
	switch agent.Role {
	case architectRole:
		return []string{filepath.Join(root, "architect", string(project), string(stream), turn, "session")}
	case committeeRole:
		return []string{
			filepath.Join(root, "shed", string(project), string(stream), turn, "session"),
			filepath.Join(root, "final", string(project), string(stream), turn, "session"),
		}
	case sessionRole:
		return []string{filepath.Join(runtime.ModelSessionDirectory(root, project, stream), turn)}
	default:
		return []string{filepath.Join(root, "threads", string(project), string(stream), agent.ID, turn)}
	}
}

func sessionProfile(repository *trace.Repository, stream config.WorkstreamID) (string, error) {
	list, err := repository.Statuses()
	if err != nil {
		return "", err
	}
	for _, w := range list {
		if w.Workstream == stream && w.Kind == trace.SessionKind {
			return w.Profile, nil
		}
	}
	return "", fmt.Errorf("session %s not found", stream)
}

func (s *Service) recoverSession(ctx context.Context, cfg *config.Config, repository *trace.Repository, stream config.WorkstreamID, agent, turn string) error {
	profileName, err := sessionProfile(repository, stream)
	if err != nil {
		return err
	}
	profile, _, err := cfg.Execution(masonRole, profileName)
	if err != nil {
		return err
	}
	next, err := s.recoverContinuation(ctx, repository, stream, agent, turn, profile)
	if err != nil || next == "" {
		return err
	}
	return dispatchSessionTurn(ctx, repository, repository.Project(), stream, next, s.now())
}

func (s *Service) recoverContinuations(ctx context.Context, repository *trace.Repository, stream config.WorkstreamID, agent string, recover func(turn string) error) error {
	settled, err := repository.Thread(stream, agent)
	if err != nil {
		return err
	}
	for _, q := range settled.Turns {
		if q.Response != nil && q.Status() == "interrupted" && q.Response.Actor == recoveryActor {
			if err := recover(q.Request.TurnID); err != nil {
				return err
			}
		}
	}
	return nil
}

func (s *Service) recoverContinuation(ctx context.Context, repository *trace.Repository, stream config.WorkstreamID, agent, turn string, profile coreadapter.Profile) (string, error) {
	th, err := repository.Thread(stream, agent)
	if err != nil {
		return "", err
	}
	for _, q := range th.Turns {
		if q.Request.TurnID != turn {
			continue
		}
		if q.Response == nil || q.Status() != "interrupted" {
			return "", nil
		}
		if execution.InterruptedInARow(th) >= execution.MaxRecoveries {
			return "", nil
		}
		req := q.Request
		req.Project = repository.Project()
		req.ID = fmt.Sprintf("request_%s-recover-%d", agent, q.Sequence)
		req.TurnID = fmt.Sprintf("%s-recover-%d", agent, q.Sequence)
		for _, existing := range th.Turns {
			if existing.Request.TurnID == req.TurnID {
				return "", nil
			}
		}
		req.At, req.Actor, req.Cause = s.now(), recoveryActor, q.Response.ID
		req.Prompt += "\n\nThe service stopped during your previous turn. Continue this work from the durable thread state."
		req.Profile = profile
		if _, err := repository.EnqueueTurn(ctx, req); err != nil {
			return "", err
		}
		return req.TurnID, nil
	}
	return "", nil
}

func (s *Service) recoverChief(ctx context.Context, repository *trace.Repository, stream config.WorkstreamID, role, agent, turn string) error {
	profile, _, err := s.roleExecution(s.current(), role)
	if err != nil {
		return err
	}
	_, err = s.recoverContinuation(ctx, repository, stream, agent, turn, profile)
	return err
}
