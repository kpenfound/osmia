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
func conversationRole(role string) bool { return trace.ManagerLike(role) || role == sessionRole }
func conversationOperation(op coreadapter.Operation) bool {
	in, err := thread.DecodeTurn(op)
	return err == nil && conversationRole(in.Agent)
}

type conversationPath struct {
	s           *Service
	repository  *trace.Repository
	pipeline    *pipeline
	drain       chan struct{}
	once        sync.Once
	scanning    sync.Mutex
	mu          sync.Mutex
	controllers []*reconcile.Controller
	followUpRan map[*trace.Repository]time.Time
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

func (p *conversationPath) draining() bool {
	select {
	case <-p.drain:
		return true
	default:
		return false
	}
}

func (p *conversationPath) Quiesce() {
	if p == nil {
		return
	}
	p.Drain()
	p.scanning.Lock()
	defer p.scanning.Unlock()
}

func (p *conversationPath) scan(ctx context.Context, targets map[config.WorkgroupID]*conversationTarget) error {
	p.scanning.Lock()
	defer p.scanning.Unlock()
	if p.draining() {
		return nil
	}
	if err := p.discover(ctx, targets); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		p.s.log().Warn("conversation discovery failed; will retry", "project", string(p.repository.Project()), "error", err.Error())
	}
	for _, t := range targets {
		if p.draining() {
			return nil
		}
		if err := p.followUps(ctx, t.repository); err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			p.s.log().Warn("follow-up scan failed; will retry", "project", string(p.repository.Project()), "error", err.Error())
		}
		if err := p.queue(ctx, t.repository); err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			p.s.log().Warn("conversation queue scan failed; will retry", "project", string(p.repository.Project()), "error", err.Error())
			continue
		}
		t.gate.begin()
		err := t.controller.Pass(ctx)
		t.gate.end()
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			p.s.log().Warn("conversation reconciliation scan failed; will retry", "project", string(p.repository.Project()), "error", err.Error())
		}
	}
	return nil
}

type conversationTarget struct {
	repository *trace.Repository
	controller *reconcile.Controller
	gate       *managerGate
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
		defer p.s.manager.drop(p)
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
		if err := p.scan(ctx, targets); err != nil {
			return err
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
	gate := s.manager.gate(p, repository.Project())
	hold := options.Hold
	options.Hold = func(stream config.WorkstreamID, op coreadapter.Operation) bool {
		return (hold != nil && hold(stream, op)) || p.holds(gate, repository, stream, op)
	}
	options.Concurrent = func(coreadapter.Operation) bool { return true }
	options.Class = func(coreadapter.Operation) trace.OperationClass { return trace.ConversationOperation }
	options.Limit = gate.limit
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
	return &conversationTarget{repository: repository, controller: controller, gate: gate, close: closeTarget}, nil
}

type conversationAdapter struct{ pipeline *pipeline }

func (a conversationAdapter) Inspect(ctx context.Context, op coreadapter.Operation) (coreadapter.Observation, error) {
	return a.pipeline.conversation.Load().runner.turns.Inspect(ctx, op)
}

func (a conversationAdapter) Apply(ctx context.Context, op coreadapter.Operation) (coreadapter.OperationResult, error) {
	return a.pipeline.conversation.Load().runner.turns.Apply(ctx, op)
}

func (p *conversationPath) queue(ctx context.Context, repository *trace.Repository) error {
	type queuedStream struct {
		stream  config.WorkstreamID
		threads []trace.Thread
		records []trace.OperationRecord
	}
	var snapshot []queuedStream
	type scanFailure struct {
		stream config.WorkstreamID
		err    error
	}
	var failures []scanFailure
	err := repository.WithWorkstreamDiscovery(func(streams []config.WorkstreamID) error {
		if repository != p.repository {
			var err error
			streams, err = p.streams(repository)
			if err != nil {
				return err
			}
		}
		for _, stream := range streams {
			threads, err := repository.Threads(stream)
			if err != nil {
				failures = append(failures, scanFailure{stream, err})
				continue
			}
			records, err := repository.Operations(stream)
			if err != nil {
				failures = append(failures, scanFailure{stream, err})
				continue
			}
			snapshot = append(snapshot, queuedStream{stream, threads, records})
		}
		return nil
	})
	if err != nil {
		return err
	}
	for _, failure := range failures {
		p.scanError(repository, failure.stream, failure.err)
	}
	for _, entry := range snapshot {
		stream, threads, records := entry.stream, entry.threads, entry.records
		if err := p.s.step("conversation-queue-read:" + string(stream)); err != nil {
			return err
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

func (p *conversationPath) holds(gate *managerGate, repository *trace.Repository, stream config.WorkstreamID, op coreadapter.Operation) bool {
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
			if !q.CompletedAt.IsZero() || q.Response != nil {
				return false
			}
			if !p.s.admitRole(th.Identity.Role) {
				return true
			}
			if th.Identity.Role == sessionRole {
				mu := SessionLock(repository, stream)
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
			held, err := p.s.holds(repository.Project(), repository, scheduler.Candidate{Project: repository.Project(), Workstream: stream, Thread: th, Turn: q})
			if err != nil || held {
				return true
			}
			return gate.hold(stream, th, q)
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
		g, _, err := p.repository.CrossRepoWorkgroup(id)
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
		if targets[id] != nil {
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
		t, err := p.target(wt, func() { wt.Close() })
		if err != nil {
			wt.Close()
			return fmt.Errorf("workgroup manager: %w", err)
		}
		targets[id] = t
	}
	return nil
}

func (p *conversationPath) leads(repository *trace.Repository) bool {
	id, ok := trace.WorkgroupProjectID(repository.Project())
	if !ok {
		return false
	}
	g, _, err := p.repository.CrossRepoWorkgroup(id)
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
	cfg := a.s.current()
	bound, err := a.s.options.Threads(a.repository, cfg)
	if err != nil {
		return coreadapter.OperationResult{}, err
	}
	return (abandonable{Reconciler: bound, s: a.s, repository: a.repository, cfg: cfg}).Apply(ctx, op)
}

func (s *Service) observeSharedConversation(trace.Commit) {
	s.conversationSignal.wake()
	s.hub.Publish(Event{Kind: EventResync})
}

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
	return nil, fmt.Errorf("shared manager has no workgroup host")
}

func (p *conversationPath) publish(ctx context.Context, repository *trace.Repository, c scheduler.Candidate) error {
	if c.Thread.Identity.Role == sessionRole {
		mu := SessionLock(repository, c.Workstream)
		if !mu.TryLock() {
			return nil
		}
		defer mu.Unlock()
		q := c.Turn
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
		if err := SetSessionState(ctx, repository, repository.Project(), c.Workstream, p.s.now(), "session-working-"+q.Request.TurnID, "session-turn-started", trace.SessionWorking); err != nil {
			return err
		}
	}
	if p.s.placement == nil {
		return scheduler.Dispatch(ctx, repository, c, p.s.now())
	}
	turn := scheduler.PlacementTurn(c, resumableRole)
	if p.s.placement.Take(turn).Client == "" {
		return nil
	}
	err := scheduler.Dispatch(ctx, repository, c, p.s.now())
	if err != nil {
		p.s.placement.Release(turn.ID)
	}
	return err
}
