package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/intake"
	"github.com/kpenfound/osmia/internal/runtime"
	"github.com/kpenfound/osmia/internal/trace"
)

func (s *Service) destroySession(ctx context.Context, raw string, design bool) (SessionDestroyResponse, *APIError) {
	if _, err := config.ParseWorkgroupID(raw); err == nil {
		return SessionDestroyResponse{}, &APIError{Code: Conflict, Message: "workgroups cannot be destroyed; only sessions can be destroyed"}
	}
	id, err := config.ParseWorkstreamID(raw)
	if err != nil {
		return SessionDestroyResponse{}, &APIError{Code: Validation, Message: "workstream must be a workstream ID"}
	}
	cfg, projects := s.runtimes()
	for _, p := range projects {
		if !cfg.Active(p.id) {
			continue
		}
		streams, err := p.repository.Workstreams()
		if err != nil {
			return SessionDestroyResponse{}, &APIError{Code: Internal, Message: err.Error()}
		}
		if !slices.Contains(streams, id) {
			continue
		}
		gate := sessionLock(p.repository, id)
		lock := &gate.Mutex
		if design {
			lock = designLock(p.repository, id)
		}
		if !lock.TryLock() {
			return SessionDestroyResponse{}, &APIError{Code: Conflict, Message: "the session is busy; try again once it finishes"}
		}
		defer lock.Unlock()
		// Re-resolve under the same lock every writer takes at admission.
		streams, err = p.repository.Workstreams()
		if err != nil {
			return SessionDestroyResponse{}, &APIError{Code: Internal, Message: err.Error()}
		}
		if !slices.Contains(streams, id) {
			break
		}
		kind, err := p.repository.Kind(id)
		if err != nil {
			return SessionDestroyResponse{}, &APIError{Code: Internal, Message: err.Error()}
		}
		allowed := kind == trace.SessionKind && !design
		if design && kind == "" {
			allowed, err = isDesignWorkstream(p.repository, id)
		}
		if err != nil {
			return SessionDestroyResponse{}, &APIError{Code: Internal, Message: err.Error()}
		}
		if !allowed || id == librarianWorkstream(p.id) {
			return SessionDestroyResponse{}, &APIError{Code: Conflict, Message: "only an open design session or an exploratory session can be destroyed"}
		}
		threads, err := p.repository.Threads(id)
		if err != nil {
			return SessionDestroyResponse{}, &APIError{Code: Internal, Message: err.Error()}
		}
		busy := gate.controls.Load() != 0
		for _, th := range threads {
			busy = busy || unfinishedSessionTurn(th)
		}
		// Completion includes relaying the turn and recording/acknowledging its
		// operation, which can outlive the thread's completion timestamp.
		operations, err := p.repository.Operations(id)
		if err != nil {
			return SessionDestroyResponse{}, &APIError{Code: Internal, Message: err.Error()}
		}
		for _, op := range operations {
			busy = busy || !op.Acknowledged
		}
		s.turns.mu.Lock()
		busy = busy || len(s.turns.streams[id]) != 0
		s.turns.mu.Unlock()
		if busy {
			return SessionDestroyResponse{}, &APIError{Code: Conflict, Message: "a turn or control is running for this session; try again once it finishes"}
		}
		intent := trace.SessionDestroy{Project: cfg.For(p.id).Project, Workstream: id, Kind: trace.SessionKind}
		if design {
			if _, pinned, err := intake.DesignStartIntentOf(p.repository, id); err != nil {
				return SessionDestroyResponse{}, &APIError{Code: Internal, Message: err.Error()}
			} else if pinned {
				return SessionDestroyResponse{}, &APIError{Code: Conflict, Message: "a workgroup start is pending for this design session"}
			}
			intent.Kind = "design"
			groups, _, err := trace.Workgroups(p.repository)
			if err != nil {
				return SessionDestroyResponse{}, &APIError{Code: Internal, Message: err.Error()}
			}
			if g, ok := trace.WorkgroupOf(groups, id); ok && len(g.Members) != 1 {
				return SessionDestroyResponse{}, &APIError{Code: Conflict, Message: "the design session belongs to a started workgroup"}
			}
		}

		err = p.repository.Serialize(func() error {
			if err := p.repository.BeginSessionDestroy(intent); err != nil {
				return err
			}
			s.forgetSessionStatus(p.id, id)
			if err := s.step("session-destroy-recorded"); err != nil {
				return err
			}
			return s.finishSessionDestroy(context.WithoutCancel(ctx), cfg, p.repository, s.store, intent)
		})
		if err != nil {
			return SessionDestroyResponse{}, &APIError{Code: Internal, Message: fmt.Sprintf("session destroy is pending recovery: %v", err)}
		}

		if err := s.refreshRuntime(); err != nil {
			return SessionDestroyResponse{}, &APIError{Code: Internal, Message: err.Error()}
		}
		s.hub.Publish(Event{Kind: EventRuntime})
		return SessionDestroyResponse{Project: p.id, Workstream: id, Destroyed: true}, nil
	}
	return SessionDestroyResponse{}, &APIError{Code: NotFound, Message: "session not found"}
}

func (s *Service) finishSessionDestroy(ctx context.Context, cfg *config.Config, repository *trace.Repository, store *runtime.Store, intent trace.SessionDestroy) error {
	project, stream := intent.Project.ID, intent.Workstream
	local := *cfg.For(project)
	local.Project = intent.Project
	if intent.Kind == trace.SessionKind {
		if err := forgetDestroyedSessionOpen(cfg.Root, project, stream); err != nil {
			return err
		}
	}
	if intent.Kind == "design" {
		if err := repository.RetractWorkgroup(ctx, stream, s.now(), ownerActor, "destroy-session"); err != nil {
			return err
		}
	} else {
		g := sessionGit(&local)
		if wt, found, err := g.Workspace(ctx, string(stream)); err != nil {
			return err
		} else if found {
			if err := g.Release(ctx, wt); err != nil {
				return err
			}
		}
	}
	if err := s.step("session-destroy-released"); err != nil {
		return err
	}
	if intent.Kind == trace.SessionKind {
		if err := sessionGit(&local).DeleteLocalBranch(ctx, sessionBranch(stream)); err != nil {
			return err
		}
	}
	dirs := append(slices.Clone(turnDirectories), "design", sessionsDirectory)
	for _, dir := range dirs {
		if err := os.RemoveAll(filepath.Join(cfg.Root.String(), dir, string(project), string(stream))); err != nil {
			return err
		}
	}
	if err := os.RemoveAll(runtime.ModelSessionDirectory(cfg.Root.String(), project, stream)); err != nil {
		return err
	}
	if _, err := repository.MarkWorkstreamTraceForDeletion(stream); err != nil {
		return err
	}
	if err := s.step("session-destroy-trace-moved"); err != nil {
		return err
	}
	if err := trace.FinishWorkstreamTraceDeletion(cfg.Root, project, stream); err != nil {
		return err
	}
	if err := store.ForgetSession(project, stream); err != nil {
		return err
	}
	s.activity.ForgetSession(project, stream)
	s.forgetSessionStatus(project, stream)
	return trace.FinishSessionDestroy(cfg.Root, project, stream)
}

func (s *Service) forgetSessionStatus(project config.ProjectID, stream config.WorkstreamID) {
	// A status read that already captured this session must not restore it
	// to the cache when its background refresh eventually finishes.
	s.statusViews.Invalidate(func(value StatusResponse) StatusResponse {
		value.Workstreams = slices.DeleteFunc(slices.Clone(value.Workstreams), func(w WorkstreamStatus) bool { return w.Project == project && w.Workstream == stream })
		return value
	})
}

func forgetDestroyedSessionOpen(root config.Root, project config.ProjectID, stream config.WorkstreamID) error {
	path, err := root.PendingSessionOpen()
	if err != nil {
		return err
	}
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	var pending pendingSession
	if err := json.Unmarshal(data, &pending); err != nil {
		return err
	}
	if pending.Project == project && pending.Workstream == stream {
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return err
		}
		dir, err := os.Open(filepath.Dir(path))
		if err != nil {
			return err
		}
		defer dir.Close()
		return dir.Sync()
	}
	return nil
}

// reconcileSessionDestroys runs before workgroup wrapping and scheduling.
func (s *Service) reconcileSessionDestroys(ctx context.Context, cfg *config.Config, projects []*activeProject, store *runtime.Store) error {
	for _, p := range projects {
		intents, err := trace.PendingSessionDestroys(cfg.Root, p.id)
		if err != nil {
			return err
		}
		for _, intent := range intents {
			if err := s.finishSessionDestroy(ctx, cfg, p.repository, store, intent); err != nil && !errors.Is(err, context.Canceled) {
				return fmt.Errorf("recover session destroy: %w", err)
			} else if err != nil {
				return err
			}
		}
	}
	return nil
}
