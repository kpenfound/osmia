package service

import (
	"context"
	"slices"
	"sync"
	"time"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/trace"
)

type pollTaskContext struct{}
type pollTaskKey struct {
	kind   string
	stream config.WorkstreamID
}

type networkPollTasks struct {
	mu      sync.Mutex
	running map[pollTaskKey]bool
	cursor  map[string]config.WorkstreamID
	stopped bool
	group   sync.WaitGroup
}

// pollTask admits an independent observer without workflow ordering. A full
// pool leaves cadence and evidence untouched. Outside the background driver,
// callers can reconcile an observer synchronously with the same evidence rules.
func (s *Service) pollTask(ctx context.Context, repository *trace.Repository, kind string, stream config.WorkstreamID, poll func(context.Context) error) error {
	tasks, _ := ctx.Value(pollTaskContext{}).(*networkPollTasks)
	if tasks == nil {
		return poll(ctx)
	}
	key := pollTaskKey{kind, stream}
	tasks.mu.Lock()
	defer tasks.mu.Unlock()
	if tasks.stopped || ctx.Err() != nil || tasks.running[key] {
		return nil
	}
	release, admitted := s.tryGitOperation(repository)
	if !admitted {
		return nil
	}
	tasks.running[key] = true
	tasks.cursor[kind] = stream
	tasks.group.Add(1)
	go func() {
		defer tasks.group.Done()
		defer func() {
			tasks.mu.Lock()
			delete(tasks.running, key)
			tasks.mu.Unlock()
			release()
		}()
		started := time.Now()
		err := poll(ctx)
		if elapsed := time.Since(started); elapsed >= trace.SlowThreshold {
			s.log().Warn("slow network observer", "project", string(repository.Project()), "workstream", string(stream), "poll", kind, "duration", elapsed)
		}
		if err != nil && ctx.Err() == nil {
			s.log().Error("network observer failed; will retry", "project", string(repository.Project()), "workstream", string(stream), "poll", kind, "error", err.Error())
		}
	}()
	return nil
}

// pollOrder resumes after the last admitted stream so a full pool cannot
// repeatedly admit only the beginning of a large scan.
func pollOrder[T any](ctx context.Context, kind string, items []T, stream func(T) config.WorkstreamID) []T {
	tasks, _ := ctx.Value(pollTaskContext{}).(*networkPollTasks)
	if tasks == nil {
		return items
	}
	tasks.mu.Lock()
	last := tasks.cursor[kind]
	tasks.mu.Unlock()
	i := slices.IndexFunc(items, func(item T) bool { return stream(item) == last })
	if i < 0 || i == len(items)-1 {
		return items
	}
	return append(slices.Clone(items[i+1:]), items[:i+1]...)
}

// runNetworkPolls keeps remote observation independent of scheduling. Each
// observer has one caller across reloads; durable evidence and cadence state
// decide whether a lookup is due. Shutdown stops admission and joins every
// observer before the project's trace closes.
func (s *Service) runNetworkPolls(ctx context.Context, repository *trace.Repository, p *pipeline) {
	tasks := &networkPollTasks{running: map[pollTaskKey]bool{}, cursor: map[string]config.WorkstreamID{}}
	ctx = context.WithValue(ctx, pollTaskContext{}, tasks)
	var group sync.WaitGroup
	for _, hook := range p.current.Load().polls {
		group.Add(1)
		go func(name string) {
			defer group.Done()
			ticker := time.NewTicker(time.Second)
			defer ticker.Stop()
			for {
				wake := s.conversationSignal.listen()
				if ctx.Err() != nil {
					return
				}
				current := p.conversation.Load()
				if current == nil {
					current = p.current.Load()
				}
				for _, poll := range current.polls {
					if poll.name != name {
						continue
					}
					var err error
					switch name {
					case "base-refresh", "edge-refresh":
						// These scans enqueue independent workstream observers.
						err = poll.pass(ctx)
					default:
						err = s.pollTask(ctx, repository, name, "", poll.pass)
					}
					if err != nil && ctx.Err() == nil {
						s.log().Error("network observer scan failed; will retry", "project", string(repository.Project()), "poll", name, "error", err.Error())
					}
					break
				}
				select {
				case <-ctx.Done():
					return
				case <-wake:
				case <-ticker.C:
				}
			}
		}(hook.name)
	}
	group.Wait()
	tasks.mu.Lock()
	tasks.stopped = true
	tasks.mu.Unlock()
	tasks.group.Wait()
}
