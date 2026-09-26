package service

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/trace"
)

var recoveryActor = trace.Actor{Kind: "service", ID: "thread-recovery"}

// recoverSessions settles sessions left by an earlier service before the
// controller can dispatch work. Role passes then read the completed
// interruption from the trace and reconstruct their pending work. A directory
// without a claim covers the crash between Prepare and ClaimTurn.
func (s *Service) recoverSessions(ctx context.Context, cfg *config.Config, repository *trace.Repository) error {
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
			for _, q := range th.Turns {
				if !q.CompletedAt.IsZero() || q.Response != nil {
					continue
				}
				if q.Claim != nil {
					// Mason views must be copied into their workspace before
					// the claim is released. The mason and drift passes do
					// that reconciliation themselves.
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
			if th.Identity.Role == trace.ChiefOfStaff && s.options.Threads != nil {
				settled, err := repository.Thread(stream, th.Identity.ID)
				if err != nil {
					return err
				}
				for _, q := range settled.Turns {
					if q.Response != nil && q.Status() == "interrupted" && q.Response.Actor == recoveryActor {
						if err := s.recoverChief(ctx, repository, stream, th.Identity.ID, q.Request.TurnID); err != nil {
							return err
						}
					}
				}
			}
		}
	}
	return nil
}

// recoverWorkspaces restores the Jujutsu workspaces of the configured
// project that an earlier service stopped in the middle of a multi-step
// operation on, before any controller runs: each kind of workspace goes back
// to the operation-log entry recorded before the interrupted attempt, and
// the operation's retry reconciles what that attempt did to the clone.
func (s *Service) recoverWorkspaces(ctx context.Context, cfg *config.Config) error {
	for _, directory := range []string{branchesDirectory, unitsDirectory, driftsDirectory} {
		operations, err := workspaces(cfg, directory, config.WorkspacesJujutsu).Recover(ctx)
		if err != nil {
			return err
		}
		for _, operation := range operations {
			log.Printf("osmia: restored the %s workspaces of project %s to their state before operation %s was interrupted", directory, cfg.Project.ID, operation)
		}
	}
	return nil
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
	case librarianRole:
		return []string{filepath.Join(root, "librarian", string(project), turn, "session")}
	default:
		return []string{filepath.Join(root, "threads", string(project), string(stream), agent.ID, turn)}
	}
}

func (s *Service) recoverChief(ctx context.Context, repository *trace.Repository, stream config.WorkstreamID, agent, turn string) error {
	th, err := repository.Thread(stream, agent)
	if err != nil {
		return err
	}
	for _, q := range th.Turns {
		if q.Request.TurnID != turn {
			continue
		}
		if q.Response == nil || q.Status() != "interrupted" {
			return nil
		}
		req := q.Request
		req.ID = fmt.Sprintf("request_%s-recover-%d", agent, q.Sequence)
		req.TurnID = fmt.Sprintf("%s-recover-%d", agent, q.Sequence)
		for _, existing := range th.Turns {
			if existing.Request.TurnID == req.TurnID {
				return nil
			}
		}
		req.At, req.Actor, req.Cause = s.now(), recoveryActor, q.Response.ID
		req.Prompt += "\n\nThe service stopped during your previous turn. Continue this work from the durable thread state."
		profile, _, err := s.roleExecution(s.current(), trace.ChiefOfStaff)
		if err != nil {
			return err
		}
		req.Profile = profile
		_, err = repository.EnqueueTurn(ctx, req)
		return err
	}
	return nil
}
