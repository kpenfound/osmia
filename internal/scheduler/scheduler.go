// Package scheduler dispatches queued workstream turns from durable trace state.
package scheduler

import (
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/runtime"
	"github.com/kpenfound/osmia/internal/thread"
	"github.com/kpenfound/osmia/internal/trace"
)

// Actor is the provenance of dispatch transitions.
var Actor = trace.Actor{Kind: "service", ID: "scheduler"}

// Candidate is the next turn of one thread, ready to be dispatched.
type Candidate struct {
	Project    config.ProjectID
	Workstream config.WorkstreamID
	Thread     trace.Thread
	Turn       trace.QueuedTurn
}

type Options struct {
	Now func() time.Time
	// Admit is the dispatch gate after Capacity: a candidate it declines, or one
	// without a free slot, stays queued and is offered again on a later pass.
	// Nil admits every candidate that fits.
	Admit func(context.Context, Candidate) (bool, error)
	// Capacity bounds the turns in flight. Masons, reviewers and committee
	// members share their role kind's slots across workstreams; every other
	// role runs one turn at a time per workstream. Each workstream also runs at
	// most PerWorkstream turns. Chief-of-staff turns take no slot. Nil leaves
	// dispatch unbounded.
	Capacity *config.Capacity
	// Priorities returns the runtime priority order of the workstreams of
	// every project, read once per pass. Nil gives every workstream the same
	// priority.
	Priorities func() []runtime.Ranked
	// Shared is the pool of role slots this scheduler shares with the
	// schedulers of other projects. Nil keeps the slots to this trace.
	Shared *Shared
}

// Shared is one pool of role slots that the schedulers of several projects,
// one per trace, draw from. Their passes run one at a time, and each counts
// the turns in flight on every trace Traces returns against the role slots of
// Capacity, so the limits hold across projects. PerWorkstream and the one
// turn per workstream of the other roles stay per workstream.
//
// Each pass also orders the queued turns of every project together and
// leaves a slot free for another project's turn that goes before its own,
// which that project's pass then dispatches. A turn the other project's own
// gate declined on its latest pass is left out until that project passes
// again.
type Shared struct {
	mu sync.Mutex
	// Traces returns the open traces of every project that draws on the
	// pool. A scheduler's own trace may be among them.
	Traces func() []*trace.Repository
	// perWorkstream is each project's capacity.per_workstream, as its
	// latest pass used it.
	perWorkstream map[config.ProjectID]int
	// declined holds the turns each project's gate declined on its latest
	// pass.
	declined map[offer]bool
}

// offer names one queued turn of one project.
type offer struct {
	project    config.ProjectID
	workstream config.WorkstreamID
	agent      string
	turn       string
}

func offerOf(c Candidate) offer {
	return offer{c.Project, c.Workstream, c.Thread.Identity.ID, c.Turn.Request.TurnID}
}

// begin records the project's per-workstream limit and forgets what its gate
// declined, before a pass of the project offers its turns again.
func (p *Shared) begin(project config.ProjectID, limits *config.Capacity) {
	if p.perWorkstream == nil {
		p.perWorkstream, p.declined = map[config.ProjectID]int{}, map[offer]bool{}
	}
	delete(p.perWorkstream, project)
	if limits != nil {
		p.perWorkstream[project] = limits.PerWorkstream
	}
	for o := range p.declined {
		if o.project == project {
			delete(p.declined, o)
		}
	}
}

// Hold runs fn while no pass of a scheduler drawing on the pool is in
// progress, so a trace fn takes out of Traces is not read once Hold returns.
func (p *Shared) Hold(fn func()) {
	p.mu.Lock()
	defer p.mu.Unlock()
	fn()
}

// Scheduler publishes the intent to run each thread's next queued turn. The
// reconciliation controller delivers that intent through the thread
// dispatcher, so a turn in flight at shutdown is recovered, not repeated.
//
// While the scheduler runs, callers accept turns with EnqueueTurn alone. A
// caller that publishes its own turn operation must do so before the service
// opens the trace or while an earlier turn of the same thread is in flight;
// otherwise the turn can end up with two operations, which the dispatcher
// still runs once.
//
// A turn holds its slots while it is in flight, so a slot is free again once
// the turn completes, whatever its outcome, and once the turn is interrupted
// by a restart. Slots are counted from the trace on each pass, and passes of
// one Scheduler, or of the schedulers sharing one pool, run one at a time;
// passes of a second scheduler on the same trace can run concurrently with
// them and overbook.
type Scheduler struct {
	mu         sync.Mutex
	repository *trace.Repository
	options    Options
}

// lock serializes the passes of the scheduler, or of every scheduler of its
// shared pool.
func (s *Scheduler) lock() func() {
	mu := &s.mu
	if s.options.Shared != nil {
		mu = &s.options.Shared.mu
	}
	mu.Lock()
	return mu.Unlock
}

func New(repository *trace.Repository, options Options) (*Scheduler, error) {
	if repository == nil {
		return nil, errors.New("scheduler requires a trace repository")
	}
	if options.Now == nil {
		options.Now = time.Now
	}
	return &Scheduler{repository: repository, options: options}, nil
}

// Pass reads every workstream's threads, then its turn operations, and
// dispatches the next turn of each thread that has none in flight. A thread's
// turn is in flight while it is claimed or while a turn operation names it and
// the turn has not completed. Pass makes no model call.
//
// Candidates are offered one at a time, by stage first: review, then
// implementation, then debate, then drafting, then every other role. Within a
// stage the candidate of the highest-priority workstream goes first. Among
// equal priorities the project whose last turn of that stage was dispatched
// least recently goes first, and within it the workstream whose last turn of
// that stage was, so equal projects take the stage's slots in turn however
// many workstreams each has ready, and so do equal workstreams of a project.
// The last dispatch of each stage is read from the trace's turn operations,
// so the rotation continues across restarts. A candidate that finds no free
// slot or that Admit declines is not dispatched and leaves its place in the
// rotation unchanged.
//
// With a shared pool the candidates of every project in it are ordered
// together. A slot that goes to another project's candidate is left free
// for that project's pass, and the candidate takes its place in the rotation
// for the rest of this pass.
func (s *Scheduler) Pass(ctx context.Context) error {
	defer s.lock()()
	project, shared := s.repository.Project(), s.options.Shared
	if shared != nil {
		shared.begin(project, s.options.Capacity)
	}
	r, err := s.read()
	if err != nil {
		return err
	}
	candidates, used, order := r.candidates, r.used, s.order(r.served)
	if shared != nil {
		candidates = slices.DeleteFunc(candidates, func(c Candidate) bool { return c.Project != project && shared.declined[offerOf(c)] })
	}
	for len(candidates) > 0 {
		slices.SortStableFunc(candidates, order)
		c := candidates[0]
		candidates = candidates[1:]
		role := c.Thread.Identity.Role
		if refusal(s.limits(c.Project), used, c) != "" {
			continue
		}
		if c.Project != project {
			used.add(c.Project, c.Workstream, role)
			r.served.serve(c, s.options.Now())
			continue
		}
		if s.options.Admit != nil {
			admitted, err := s.options.Admit(ctx, c)
			if err != nil {
				return err
			}
			if !admitted {
				if shared != nil {
					shared.declined[offerOf(c)] = true
				}
				continue
			}
		}
		at := s.options.Now()
		if err := s.dispatch(ctx, c, at); err != nil {
			return fmt.Errorf("workstream %s: %w", c.Workstream, err)
		}
		used.add(c.Project, c.Workstream, role)
		r.served.serve(c, at)
	}
	return nil
}

// limits returns the capacity that holds the project's turns: the
// scheduler's own, with the per-workstream limit of the project's latest
// pass when the project is another one of the shared pool.
func (s *Scheduler) limits(project config.ProjectID) *config.Capacity {
	shared := s.options.Shared
	if s.options.Capacity == nil || shared == nil || project == s.repository.Project() {
		return s.options.Capacity
	}
	n, ok := shared.perWorkstream[project]
	if !ok {
		return s.options.Capacity
	}
	limits := *s.options.Capacity
	limits.PerWorkstream = n
	return &limits
}

// The reasons a queued turn finds no free slot.
const (
	// WaitCapacity: every slot of the turn's role kind is taken, or, for a
	// role without shared slots, its workstream already runs a turn of it.
	WaitCapacity = "capacity"
	// WaitWorkstreamCap: the workstream runs capacity.per_workstream turns.
	WaitWorkstreamCap = "workstream-cap"
)

// Wait is a queued turn that finds no free slot, and why.
type Wait struct {
	Candidate
	Reason string
}

// Slots is the scheduler's slot accounting at one read of the trace.
type Slots struct {
	// Used counts the turns in flight by role, chief-of-staff turns aside.
	Used map[string]int
	// Waiting lists, in the order a pass offers them, the queued turns that
	// hold would admit and that a pass would find no free slot for.
	Waiting []Wait
}

// Slots reads the trace as Pass does and reports the slots in use and the
// queued turns waiting for one, without dispatching anything. It offers the
// candidates to the free slots in Pass's order and takes a slot for each one
// that fits and that holds does not hold, as Pass would if Admit let it
// through. holds is the part of the dispatch gate that declines a candidate
// whatever the capacity, such as a pause; a candidate it holds is not waiting
// for a slot.
func (s *Scheduler) Slots(holds func(Candidate) (bool, error)) (Slots, error) {
	return SlotsOf([]Gated{{s, holds}})
}

// Gated is a scheduler and the part of its dispatch gate that declines a
// candidate whatever the capacity.
type Gated struct {
	Scheduler *Scheduler
	Holds     func(Candidate) (bool, error)
}

// SlotsOf reports the slots of schedulers that draw on one shared pool as
// Slots does for one, offering the candidates of all of them to the free
// slots in one order, the order their passes follow together. Each
// candidate is held by its own scheduler's Holds and bounded by its own
// scheduler's capacity. Used counts the turns in flight before any of them.
func SlotsOf(gated []Gated) (Slots, error) {
	out := Slots{}
	if len(gated) == 0 {
		return out, nil
	}
	defer gated[0].Scheduler.lock()()
	of := map[config.ProjectID]Gated{}
	var traces []*trace.Repository
	for _, g := range gated {
		of[g.Scheduler.repository.Project()] = g
		for _, t := range g.Scheduler.traces() {
			if !slices.Contains(traces, t) {
				traces = append(traces, t)
			}
		}
	}
	r, err := readTraces(traces)
	if err != nil {
		return Slots{}, err
	}
	out.Used = maps.Clone(r.used.roles)
	candidates := slices.DeleteFunc(r.candidates, func(c Candidate) bool { _, ok := of[c.Project]; return !ok })
	used, order, at := r.used, gated[0].Scheduler.order(r.served), gated[0].Scheduler.options.Now()
	for len(candidates) > 0 {
		slices.SortStableFunc(candidates, order)
		c := candidates[0]
		candidates = candidates[1:]
		g := of[c.Project]
		held, err := g.Holds(c)
		if err != nil {
			return Slots{}, err
		}
		if held {
			continue
		}
		if reason := refusal(g.Scheduler.options.Capacity, used, c); reason != "" {
			out.Waiting = append(out.Waiting, Wait{c, reason})
			continue
		}
		used.add(c.Project, c.Workstream, c.Thread.Identity.Role)
		r.served.serve(c, at)
	}
	return out, nil
}

// reading is what a pass reads from the traces before it offers slots: the
// turns in flight, the queued turns and each stage's last dispatch.
type reading struct {
	used       usage
	candidates []Candidate
	served     served
}

// traces returns the scheduler's trace, then, with a shared pool, the
// pool's other traces.
func (s *Scheduler) traces() []*trace.Repository {
	out := []*trace.Repository{s.repository}
	if s.options.Shared != nil && s.options.Shared.Traces != nil {
		for _, other := range s.options.Shared.Traces() {
			if other != s.repository {
				out = append(out, other)
			}
		}
	}
	return out
}

// read reads the scheduler's traces.
func (s *Scheduler) read() (reading, error) {
	return readTraces(s.traces())
}

// readTraces reads every workstream's threads of each trace, then its turn
// operations, and returns the turns in flight on all of them, their queued
// turns and each stage's last dispatch.
func readTraces(traces []*trace.Repository) (reading, error) {
	r := reading{used: usage{roles: map[string]int{}, streams: map[streamKey]int{}, local: map[localKey]int{}}, served: served{}}
	for i, repository := range traces {
		l, err := load(repository)
		if err != nil {
			if i > 0 {
				err = fmt.Errorf("project %s: %w", repository.Project(), err)
			}
			return reading{}, err
		}
		l.count(r.used)
		for _, stream := range l.streams {
			for _, t := range l.threads[stream] {
				q, ok := next(t)
				if !ok || l.dispatched[turnKey{stream, t.Identity.ID, q.Request.TurnID}] {
					continue
				}
				r.candidates = append(r.candidates, Candidate{Project: l.project, Workstream: stream, Thread: t, Turn: q})
			}
		}
		for k, at := range l.served {
			r.served.at(k, at)
		}
	}
	return r, nil
}

// loaded is one trace's workstreams, their threads, the turns its turn
// operations name and each stage's last dispatch.
type loaded struct {
	project    config.ProjectID
	streams    []config.WorkstreamID
	threads    map[config.WorkstreamID][]trace.Thread
	dispatched map[turnKey]bool
	served     served
}

// load reads every workstream's threads of repository, then its turn
// operations.
func load(repository *trace.Repository) (loaded, error) {
	project := repository.Project()
	streams, err := repository.Workstreams()
	if err != nil {
		return loaded{}, err
	}
	threads := map[config.WorkstreamID][]trace.Thread{}
	roles := map[localKey]string{}
	for _, stream := range streams {
		if threads[stream], err = repository.Threads(stream); err != nil {
			return loaded{}, fmt.Errorf("workstream %s: %w", stream, err)
		}
		for _, t := range threads[stream] {
			roles[localKey{project, stream, t.Identity.ID}] = t.Identity.Role
		}
	}
	dispatched := map[turnKey]bool{}
	served := served{}
	for _, stream := range streams {
		records, err := repository.Operations(stream)
		if err != nil {
			return loaded{}, fmt.Errorf("workstream %s: %w", stream, err)
		}
		for _, record := range records {
			// The dispatcher refuses input it cannot decode, and the controller
			// records that refusal as a retry, so such an operation runs no turn.
			in, err := thread.DecodeTurn(record.Operation)
			if err != nil {
				continue
			}
			dispatched[turnKey{in.Workstream, in.Agent, in.Turn}] = true
			if role, ok := roles[localKey{project, in.Workstream, in.Agent}]; ok {
				served.dispatched(project, in.Workstream, role, record.Transition.At)
			}
		}
	}
	return loaded{project, streams, threads, dispatched, served}, nil
}

// count adds the trace's turns in flight to used.
func (l loaded) count(used usage) {
	for _, stream := range l.streams {
		for _, t := range l.threads[stream] {
			if inFlight(t, stream, l.dispatched) {
				used.add(l.project, stream, t.Identity.Role)
			}
		}
	}
}

// order is the order candidates are offered in, given each stage's last
// dispatch. Ties keep trace order, for stable admission across passes.
func (s *Scheduler) order(served served) func(a, b Candidate) int {
	rank := Rank(nil)
	if s.options.Priorities != nil {
		rank = Rank(s.options.Priorities())
	}
	return func(a, b Candidate) int {
		ra, rb := a.Thread.Identity.Role, b.Thread.Identity.Role
		return cmp.Or(cmp.Compare(stage(ra), stage(rb)),
			cmp.Compare(rank(a.Project, a.Workstream), rank(b.Project, b.Workstream)),
			served[rotationKey(a.Project, "", ra)].Compare(served[rotationKey(b.Project, "", rb)]),
			served[rotationKey(a.Project, a.Workstream, ra)].Compare(served[rotationKey(b.Project, b.Workstream, rb)]),
			strings.Compare(string(a.Project), string(b.Project)),
			strings.Compare(string(a.Workstream), string(b.Workstream)))
	}
}

// served holds the last dispatch of each stage, by workstream and, under
// the empty workstream ID, by project.
type served map[rotation]time.Time

// at moves the last dispatch of key to at when at is later.
func (s served) at(key rotation, at time.Time) {
	if at.After(s[key]) {
		s[key] = at
	}
}

// dispatched records a dispatch of a turn of role in the project's
// workstream at the given time.
func (s served) dispatched(project config.ProjectID, stream config.WorkstreamID, role string, at time.Time) {
	s.at(rotationKey(project, stream, role), at)
	s.at(rotationKey(project, "", role), at)
}

// serve records the candidate's dispatch at the given time.
func (s served) serve(c Candidate, at time.Time) {
	s.dispatched(c.Project, c.Workstream, c.Thread.Identity.Role, at)
}

// stage orders the roles whose turns compete for a freed slot, so the factory
// finishes work before it widens it.
func stage(role string) int {
	switch role {
	case "reviewer":
		return 0
	case "mason":
		return 1
	case "committee":
		return 2
	case "architect":
		return 3
	}
	return 4
}

type usage struct {
	roles   map[string]int
	streams map[streamKey]int
	local   map[localKey]int
}

type streamKey struct {
	project    config.ProjectID
	workstream config.WorkstreamID
}

// localKey names a role or an agent within one workstream of one project.
type localKey struct {
	project    config.ProjectID
	workstream config.WorkstreamID
	name       string
}

// rotation names one workstream's place in a stage's rotation, or with an
// empty workstream, one project's.
type rotation struct {
	project    config.ProjectID
	workstream config.WorkstreamID
	stage      int
}

func rotationKey(project config.ProjectID, stream config.WorkstreamID, role string) rotation {
	return rotation{project, stream, stage(role)}
}

// add counts a turn in flight. Role kinds share their slots across every
// workstream; workstream and per-workstream role counts are kept per project.
func (u usage) add(project config.ProjectID, stream config.WorkstreamID, role string) {
	if role == trace.ChiefOfStaff {
		return
	}
	u.roles[role]++
	u.streams[streamKey{project, stream}]++
	u.local[localKey{project, stream, role}]++
}

// refusal returns why the candidate's turn has no free slot within limits,
// or "" when it has one. Nil limits leave every turn a slot.
func refusal(limits *config.Capacity, used usage, c Candidate) string {
	role := c.Thread.Identity.Role
	if limits == nil || role == trace.ChiefOfStaff {
		return ""
	}
	if used.streams[streamKey{c.Project, c.Workstream}] >= limits.PerWorkstream {
		return WaitWorkstreamCap
	}
	fits := used.local[localKey{c.Project, c.Workstream, role}] < 1
	switch role {
	case "mason":
		fits = used.roles[role] < limits.Masons
	case "reviewer":
		fits = used.roles[role] < limits.Reviewers
	case "committee":
		fits = used.roles[role] < limits.Committee
	}
	if !fits {
		return WaitCapacity
	}
	return ""
}

// inFlight reports whether the thread's oldest unfinished turn is claimed or
// named by a turn operation. A claim a restart interrupted runs nowhere.
func inFlight(t trace.Thread, stream config.WorkstreamID, dispatched map[turnKey]bool) bool {
	for _, q := range t.Turns {
		if q.CompletedAt.IsZero() {
			if q.Claim != nil {
				return t.Status != "interrupted"
			}
			return dispatched[turnKey{stream, t.Identity.ID, q.Request.TurnID}]
		}
	}
	return false
}

type turnKey struct {
	workstream config.WorkstreamID
	agent      string
	turn       string
}

// next returns the thread's oldest unfinished turn unless it is claimed. A
// parked thread has no unfinished turn, so it is not offered to Admit. Turns
// are claimed in sequence, so a claim is always on the oldest unfinished turn
// and no later turn is eligible.
func next(t trace.Thread) (trace.QueuedTurn, bool) {
	for _, q := range t.Turns {
		if q.CompletedAt.IsZero() {
			return q, q.Claim == nil
		}
	}
	return trace.QueuedTurn{}, false
}

// Subject is the workflow subject that records a thread's dispatched turns.
func Subject(agent string) string {
	return fmt.Sprintf("dispatch_%x", sha256.Sum256([]byte(agent)))[:49]
}

func (s *Scheduler) dispatch(ctx context.Context, c Candidate, at time.Time) error {
	agent, req := c.Thread.Identity.ID, c.Turn.Request
	subject := Subject(agent)
	state, err := s.repository.Workflow(c.Workstream, subject)
	if err != nil {
		return err
	}
	data, _ := json.Marshal([]string{agent, req.TurnID})
	id := fmt.Sprintf("dispatch_%x", sha256.Sum256(data))
	event := trace.EventID(id, "turn")
	project := c.Project
	op, err := thread.TurnOperation(project, event, thread.TurnInput{Workstream: c.Workstream, Agent: agent, Turn: req.TurnID})
	if err != nil {
		return err
	}
	h := trace.Header{Schema: "osmia.trace.transition", Version: trace.Version, ID: id, Revision: 1, Project: project, Workstream: c.Workstream, Unit: req.Unit, At: at, Actor: Actor, Cause: req.ID, Depth: req.Depth}
	tx := trace.Transaction{ExpectedVersion: state.Version, Transition: trace.Transition{Header: h, Subject: subject, From: state.Value, To: req.TurnID,
		Reason: fmt.Sprintf("Queued turn %s dispatched on thread %s", req.TurnID, req.ThreadID)},
		Events: []trace.Event{{ID: event, Kind: "turn", Body: "Run turn " + req.TurnID, Operation: &op}}}
	_, err = s.repository.Transact(ctx, tx)
	return err
}
