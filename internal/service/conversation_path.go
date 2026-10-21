package service

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/reconcile"
	"github.com/kpenfound/osmia/internal/scheduler"
	"github.com/kpenfound/osmia/internal/thread"
	"github.com/kpenfound/osmia/internal/trace"
)

// conversationSignal broadcasts coalesced hints independently of the main
// controller's consumed wakeups. Callbacks only signal; the queues are durable.
type conversationSignal struct {
	mu      sync.Mutex
	changed chan struct{}
}

func (w *conversationSignal) listen() <-chan struct{} {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.changed == nil {
		w.changed = make(chan struct{})
	}
	return w.changed
}
func (w *conversationSignal) wake() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.changed != nil {
		close(w.changed)
	}
	w.changed = make(chan struct{})
}
func conversationRole(role string) bool { return role == trace.ChiefOfStaff || role == sessionRole }
func conversationOperation(op coreadapter.Operation) bool {
	in, err := thread.DecodeTurn(op)
	return err == nil && conversationRole(in.Agent)
}

// conversationPath owns admission and reconciliation for the project's local
// conversations and the shared chiefs led by one of its live members.
type conversationPath struct {
	s           *Service
	repository  *trace.Repository
	pipeline    *pipeline
	drain       chan struct{}
	once        sync.Once
	mu          sync.Mutex
	controllers []*reconcile.Controller
}

func (p *conversationPath) Drain() {
	if p == nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.once.Do(func() { close(p.drain); p.s.conversationSignal.wake() })
	for _, controller := range p.controllers {
		controller.Drain()
	}
}

type conversationTarget struct {
	repository *trace.Repository
	controller *reconcile.Controller
	close      func()
}

func (p *conversationPath) run(ctx context.Context) (err error) {
	if p.s.options.Threads == nil {
		return nil
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	local, err := p.target(p.repository, nil)
	if err != nil {
		return err
	}
	targets := map[config.WorkgroupID]*conversationTarget{"": local}
	defer func() {
		if err != nil {
			cancel()
		}
		for _, t := range targets {
			t.controller.Drain()
		}
		for _, t := range targets {
			err = errors.Join(err, t.controller.Wait())
			if t.close != nil {
				t.close()
			}
		}
	}()
	// Ticks recover due retries and changes to runtime gates. Enqueue and result
	// writes wake immediately, independently of scheduling or its tick source.
	ticks := p.s.options.conversationTicks
	if ticks == nil {
		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()
		ticks = ticker.C
	}
	for {
		wake := p.s.conversationSignal.listen()
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-p.drain:
			return nil
		default:
		}
		if err := p.discover(ctx, targets); err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			p.s.log().Warn("conversation discovery failed; will retry", "project", string(p.repository.Project()), "error", err.Error())
		}
		for _, t := range targets {
			if err := p.queue(ctx, t.repository); err != nil {
				if ctx.Err() != nil {
					return ctx.Err()
				}
				p.s.log().Warn("conversation queue scan failed; will retry", "project", string(p.repository.Project()), "error", err.Error())
				continue
			}
			if err := t.controller.Pass(ctx); err != nil {
				if ctx.Err() != nil {
					return ctx.Err()
				}
				p.s.log().Warn("conversation reconciliation scan failed; will retry", "project", string(p.repository.Project()), "error", err.Error())
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-p.drain:
			return nil
		case <-wake:
		case <-ticks:
		}
	}
}

func (p *conversationPath) target(repository *trace.Repository, closeTarget func()) (*conversationTarget, error) {
	s := p.s
	// Local adapters follow the pipeline across reloads. A shared chief uses
	// the same production binding over its workgroup's authority.
	var adapter coreadapter.Reconciler
	if repository == p.repository {
		adapter = conversationAdapter{p.pipeline}
	} else {
		adapter = sharedConversationAdapter{s: s, repository: repository}
	}

	options := s.options.Reconciliation
	options.Worker = "conversations"
	options.Adapters = map[coreadapter.OperationBoundary]coreadapter.Reconciler{coreadapter.RunnerBoundary: adapter}
	options.Schedule = nil
	options.Workstreams = nil
	if repository != p.repository {
		options.Workstreams = func() ([]config.WorkstreamID, error) { return p.streams(repository) }
	}
	hold := options.Hold
	options.Hold = func(stream config.WorkstreamID, op coreadapter.Operation) bool {
		return (hold != nil && hold(stream, op)) || p.holds(repository, stream, op)
	}
	options.Concurrent = func(coreadapter.Operation) bool { return true }
	options.Class = func(coreadapter.Operation) trace.OperationClass { return trace.ConversationOperation }
	options.Limit = nil
	controller, err := reconcile.New(repository, options)
	if err != nil {
		return nil, err
	}
	p.mu.Lock()
	select {
	case <-p.drain:
		controller.Drain()
	default:
	}
	p.controllers = append(p.controllers, controller)
	p.mu.Unlock()
	return &conversationTarget{repository: repository, controller: controller, close: closeTarget}, nil
}

// conversationAdapter binds each call to the latest accepted configuration;
// an Apply already running keeps the adapter it started with.
type conversationAdapter struct{ pipeline *pipeline }

func (a conversationAdapter) Inspect(ctx context.Context, op coreadapter.Operation) (coreadapter.Observation, error) {
	return a.pipeline.conversation.Load().runner.turns.Inspect(ctx, op)
}

func (a conversationAdapter) Apply(ctx context.Context, op coreadapter.Operation) (coreadapter.OperationResult, error) {
	return a.pipeline.conversation.Load().runner.turns.Apply(ctx, op)
}

// queue publishes at most one outstanding operation per thread, including
// completion hooks. A completed turn's operation must settle before its
// successor is admitted, so a delayed hook cannot mutate a newer turn's state.
func (p *conversationPath) queue(ctx context.Context, repository *trace.Repository) error {
	streams, err := p.streams(repository)
	if err != nil {
		return err
	}
	for _, stream := range streams {
		threads, err := repository.Threads(stream)
		if err != nil {
			p.scanError(repository, stream, err)
			continue
		}
		if err := p.s.step("conversation-queue-read:" + string(stream)); err != nil {
			return err
		}
		records, err := repository.Operations(stream)
		if err != nil {
			p.scanError(repository, stream, err)
			continue
		}
		for _, th := range threads {
			if !conversationRole(th.Identity.Role) {
				continue
			}
			if pendingConversation(records, th, "") {
				continue
			}
			for _, q := range th.Turns {
				if !q.CompletedAt.IsZero() {
					continue
				}
				c := scheduler.Candidate{Project: repository.Project(), Workstream: stream, Thread: th, Turn: q}
				if err := p.publish(ctx, repository, c); err != nil && !errors.Is(err, trace.ErrConflict) {
					p.scanError(repository, stream, err)
				}

				break
			}
		}
	}
	return nil
}

func (p *conversationPath) scanError(repository *trace.Repository, stream config.WorkstreamID, err error) {
	p.s.log().Warn("conversation thread scan failed; will retry", "project", string(repository.Project()), "workstream", string(stream), "error", err.Error())
}

func pendingConversation(records []trace.OperationRecord, th trace.Thread, before string) bool {
	pending := map[string]bool{}
	for _, record := range records {
		in, err := thread.DecodeTurn(record.Operation)
		if err != nil || in.Agent != th.Identity.ID || record.Acknowledged {
			continue
		}
		pending[in.Turn] = true
	}
	// Operation IDs are hashes, so their scan order says nothing about the
	// thread's turn order. Recovery can find several published intents.
	for _, q := range th.Turns {
		if q.Request.TurnID == before {
			return false
		}
		if pending[q.Request.TurnID] {
			return true
		}
	}
	return len(pending) > 0
}

func (p *conversationPath) holds(repository *trace.Repository, stream config.WorkstreamID, op coreadapter.Operation) bool {
	if !conversationOperation(op) || (repository != p.repository && !p.leads(repository)) {
		return true
	}
	in, _ := thread.DecodeTurn(op)
	th, err := repository.Thread(stream, in.Agent)
	if err != nil {
		return true
	}
	records, err := repository.Operations(stream)
	if err != nil || pendingConversation(records, th, in.Turn) {
		return true
	}
	for _, q := range th.Turns {
		if q.Request.TurnID == in.Turn {
			// Completion and hook recovery do not start a model and must finish
			// even when the provider or the workstream is held.
			if !q.CompletedAt.IsZero() || q.Response != nil {
				return false
			}
			if !p.s.admitRole(th.Identity.Role) {
				return true
			}
			if th.Identity.Role == sessionRole {
				mu := sessionLock(repository, stream)
				if !mu.TryLock() {
					return true
				}
				mu.Unlock()
				return false
			}
			if pending, err := repository.PendingCrossRepoWorkgroup(stream); err != nil || pending != "" {
				return true
			}
			if id, err := repository.CrossRepoWorkgroupOf(stream); err != nil || (id != "" && repository == p.repository) {
				return true
			}
			held, err := p.s.holds(repository.Project(), librarianWorkstream(repository.Project()), repository, scheduler.Candidate{Project: repository.Project(), Workstream: stream, Thread: th, Turn: q})
			return err != nil || held
		}
		if q.CompletedAt.IsZero() {
			return true
		}
	}
	return true
}

func (p *conversationPath) discover(ctx context.Context, targets map[config.WorkgroupID]*conversationTarget) error {
	ids, err := p.s.crossRepoWorkgroupIDs(ctx, p.repository)
	if err != nil {
		return err
	}
	for _, id := range ids {
		_, g, _, err := p.repository.CrossRepoProjectSpec(id)
		if err != nil {
			return err
		}
		leader, err := p.s.liveWorkgroupLeader(g)
		if err != nil {
			return err
		}
		streams, err := p.repository.Workstreams()
		if err != nil {
			return err
		}
		owns := false
		for _, stream := range streams {
			owns = owns || stream == leader
		}
		if !owns {
			continue
		}
		members, err := p.s.crossRepoMemberRepos(ctx, g)
		if errors.Is(err, errWorkgroupJoining) {
			continue
		}
		if err != nil {
			return err
		}
		wt, err := trace.OpenWorkgroupTrace(p.s.current().Root, id, members)
		if err != nil {
			if wt != nil {
				wt.Close()
			}
			return err
		}
		wt.Observe(p.s.observeSharedConversation)
		if targets[id] != nil {
			wt.Close()
			continue
		}
		t, err := p.target(wt, func() { wt.Close() })
		if err != nil {
			wt.Close()
			return fmt.Errorf("workgroup chief: %w", err)
		}
		targets[id] = t
	}
	return nil
}

// leads is read at admission as membership can change while a target is open.
func (p *conversationPath) leads(repository *trace.Repository) bool {
	id, ok := trace.WorkgroupProjectID(repository.Project())
	if !ok {
		return false
	}
	_, g, _, err := p.repository.CrossRepoProjectSpec(id)
	if err != nil {
		return false
	}
	leader, err := p.s.liveWorkgroupLeader(g)
	if err != nil {
		return false
	}
	streams, err := p.repository.Workstreams()
	if err != nil {
		return false
	}
	for _, stream := range streams {
		if stream == leader {
			return true
		}
	}
	return false
}

// Shared bindings use current configuration and validated live members on each
// attempt, while operations already running retain their own bound adapter.
type sharedConversationAdapter struct {
	s          *Service
	repository *trace.Repository
}

func (a sharedConversationAdapter) Inspect(ctx context.Context, op coreadapter.Operation) (coreadapter.Observation, error) {
	bound, err := a.s.options.Threads(a.repository, a.s.current())
	if err != nil {
		return coreadapter.Observation{}, err
	}
	return bound.Inspect(ctx, op)
}
func (a sharedConversationAdapter) Apply(ctx context.Context, op coreadapter.Operation) (coreadapter.OperationResult, error) {
	bound, err := a.s.options.Threads(a.repository, a.s.current())
	if err != nil {
		return coreadapter.OperationResult{}, err
	}
	return (abandonable{Reconciler: bound, s: a.s, repository: a.repository}).Apply(ctx, op)
}

// A shared chief has addresses in several member views. Resync invalidates
// those views without reading member repositories under the trace write lock.
func (s *Service) observeSharedConversation(trace.Commit) {
	s.conversationSignal.wake()
	s.hub.Publish(Event{Kind: EventResync})
}

// A bound workgroup aggregate lists real members; its conversation belongs to
// the synthetic host, which is deliberately absent from that aggregate.
func (p *conversationPath) streams(repository *trace.Repository) ([]config.WorkstreamID, error) {
	if repository == p.repository {
		return repository.Workstreams()
	}
	id, _ := trace.WorkgroupProjectID(repository.Project())
	groups, _, err := trace.Workgroups(repository)
	if err != nil {
		return nil, err
	}
	for _, group := range groups {
		if group.ID == id && len(group.Members) > 0 {
			return []config.WorkstreamID{group.Members[0]}, nil
		}
	}
	return nil, fmt.Errorf("shared chief has no workgroup host")
}

func (p *conversationPath) publish(ctx context.Context, repository *trace.Repository, c scheduler.Candidate) error {
	if c.Thread.Identity.Role == sessionRole {
		mu := sessionLock(repository, c.Workstream)
		// The message handler owns the working transition until it releases this
		// short lock. Its unlock signals another scan if acceptance is still busy.
		if !mu.TryLock() {
			return nil
		}
		defer mu.Unlock()
		q := c.Turn
		// The queue's thread snapshot may precede completion and acknowledgment.
		// Only first admission owns the working transition; a stale scan must
		// never restore working after the completion hook has recorded idle.
		records, err := repository.Operations(c.Workstream)
		if err != nil {
			return err
		}
		for _, record := range records {
			in, err := thread.DecodeTurn(record.Operation)
			if err == nil && in.Agent == c.Thread.Identity.ID && in.Turn == q.Request.TurnID {
				return nil
			}
		}
		if err := setSessionState(ctx, repository, repository.Project(), c.Workstream, p.s.now(), "session-working-"+q.Request.TurnID, "session-turn-started", trace.SessionWorking); err != nil {
			return err
		}
	}
	return scheduler.Dispatch(ctx, repository, c, p.s.now())
}
