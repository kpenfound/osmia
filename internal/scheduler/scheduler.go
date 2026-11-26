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
	"github.com/kpenfound/osmia/internal/fleet"
	"github.com/kpenfound/osmia/internal/kinds"
	"github.com/kpenfound/osmia/internal/runtime"
	"github.com/kpenfound/osmia/internal/slotspans"
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
	// Holds is the part of the dispatch gate that declines a candidate
	// whatever the capacity, such as a pause. A candidate it holds takes no
	// slot and does not wait for one. Nil holds none.
	Holds func(Candidate) (bool, error)
	// Admit is the rest of the dispatch gate, after Holds and Capacity: a
	// candidate it declines, one Holds holds, or one without a free slot,
	// stays queued and is offered again on a later pass. Nil admits every
	// candidate that fits.
	Admit func(context.Context, Candidate) (bool, error)
	// Capacity bounds the turns in flight. Masons, reviewers and committee
	// members share their role kind's slots across workstreams; every other
	// role runs one turn at a time per workstream. Each workstream also runs at
	// most PerWorkstream turns. Chief-of-staff turns take no slot. Nil leaves
	// dispatch unbounded.
	Capacity *config.Capacity
	// Priorities returns the runtime priority order of workstreams, read once
	// per pass. Nil gives every workstream the same priority.
	Priorities func() []runtime.Priority
	// Shared is the pool of role slots this scheduler shares with the
	// schedulers of other projects. Nil keeps the slots to this trace.
	Shared *Shared
	// Placement places each dispatched turn on a client. A turn starts only
	// when its role has room under Capacity and a client has a free slot: it
	// checks the role limit first, so a turn it holds back does not move the
	// rotation. A turn that resumes a thread's saved session waits for the
	// client that holds it, owed that client's next free slot before any new
	// turn. Nil places nothing, and every turn runs where its runner puts it.
	Placement *fleet.Placement
	// Resumable is the thread runner's ResumableRoles, which tells what a turn
	// resumes. Nil is none.
	Resumable func(role string) bool
}

// Shared is one pool of role slots that the schedulers of several projects,
// one per trace, draw from. A scheduler reads only its own trace: it counts
// its own project's turns in flight there, and every other project's from
// the counts by role kind that project last published to the pool. Their
// passes run one at a time, and each publishes its project's count once it
// has dispatched, so the role slots of Capacity hold across projects.
// PerWorkstream and the one turn per workstream of the other roles stay per
// workstream.
//
// Workflow operations reserve capacity at admission through TryReserve;
// their runner turns use that reservation. Other turns enter flight in their
// project's passes. A scheduled turn that completes holds its slot for other
// projects until its own project's next pass, which its completion wakes.
type Shared struct {
	mu           sync.Mutex
	usageMu      sync.Mutex
	usage        map[config.ProjectID]published
	reservations map[reservationKey]int
	// Changed, when set, runs when published turns, waiting turns or workflow
	// reservations change, or the project leaves the pool. It must not call
	// back into the pool.
	Changed func()
	// Spans, when set, sends a span for each slot the pool holds and each wait
	// for one. It only observes: it never decides who gets a slot.
	Spans *slotspans.Recorder
	// Limits reports the limit of the pool that grants role's slots, or that it
	// has none, for the slots held when a project joins.
	Limits func(role string) (limit int, bounded bool)
}

type published struct {
	roles   map[string]int
	waiting []Wait
}

// Join counts the turns in flight on repository's own trace, such as those
// a restart left to recover, and publishes them as its project's count. A
// project joins before its scheduler's first pass, so the other projects'
// passes count its turns from the start. A nil pool joins nothing.
func (p *Shared) Join(repository *trace.Repository) error {
	if p == nil {
		return nil
	}
	l, err := load(repository)
	if err != nil {
		return err
	}
	used := newUsage()
	l.count(used)
	p.publish(l.project, used.roles, nil)
	p.syncHolds(l.project, used.flights, p.Limits, p.Spans.Now())
	return nil
}

// Leave removes project's count from the pool, freeing its slots for the
// other projects. A project leaves once its turns are no longer in flight,
// or when it is never started after joining. A nil pool has nothing to
// leave.
func (p *Shared) Leave(project config.ProjectID) {
	if p == nil {
		return
	}
	p.usageMu.Lock()
	_, ok := p.usage[project]
	delete(p.usage, project)
	p.usageMu.Unlock()
	p.endProject(project, p.Spans.Now())
	if ok && p.Changed != nil {
		p.Changed()
	}
}

func (p *Shared) publish(project config.ProjectID, roles map[string]int, waiting []Wait) {
	next := published{maps.Clone(roles), slices.Clone(waiting)}
	p.usageMu.Lock()
	if p.usage == nil {
		p.usage = map[config.ProjectID]published{}
	}
	previous, ok := p.usage[project]
	p.usage[project] = next
	p.usageMu.Unlock()
	if p.Changed != nil && (!ok || !next.same(previous)) {
		p.Changed()
	}
}

func (u published) same(other published) bool {
	return maps.Equal(u.roles, other.roles) && slices.EqualFunc(u.waiting, other.waiting, func(a, b Wait) bool {
		return a.Project == b.Project && a.Workstream == b.Workstream && a.Thread.Identity.ID == b.Thread.Identity.ID && a.Turn.Request.TurnID == b.Turn.Request.TurnID && a.Reason == b.Reason
	})
}

func (p *Shared) others(project config.ProjectID) map[string]int {
	p.usageMu.Lock()
	defer p.usageMu.Unlock()
	out := map[string]int{}
	for id, u := range p.usage {
		if id == project {
			continue
		}
		for role, n := range u.roles {
			out[role] += n
		}
	}
	for key, n := range p.reservations {
		if key.project != project {
			out[key.role] += n
		}
	}
	return out
}

// Slots reports the turns in flight of every project in the pool, and the
// queued turns of projects, in that order, that their last passes found no
// free slot for. It reads no trace: it is as current as the projects' last
// passes. A nil pool has no slot in use.
func (p *Shared) Slots(projects []config.ProjectID) Slots {
	out := Slots{Used: map[string]int{}}
	if p == nil {
		return out
	}
	p.usageMu.Lock()
	defer p.usageMu.Unlock()
	for _, u := range p.usage {
		for role, n := range u.roles {
			out.Used[role] += n
		}
	}
	for key, n := range p.reservations {
		out.Used[key.role] += n
	}
	for _, id := range projects {
		out.Waiting = append(out.Waiting, p.usage[id].waiting...)
	}
	return out
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
// stage the candidate of the highest-priority workstream goes first, and among
// workstreams of equal priority the one whose last turn of that stage was
// dispatched least recently, so equal workstreams take the stage's slots in
// turn. The last dispatch of each stage is read from the trace's turn
// operations, so the rotation continues across restarts. A candidate that
// Holds holds, that finds no free slot or that Admit declines is not
// dispatched and leaves its workstream's place in the rotation unchanged.
//
// With a Placement, a candidate that clears the role limits, the dispatch gate
// and Admit also needs a client: the pass places it with the placement, which
// takes a slot of the client it picks, and dispatches it there. A candidate that
// resumes a client's session waits for that client without a role slot, and is
// owed the client's next free slot before any new candidate. A candidate no
// client can take stays queued and leaves the placement's rotation unchanged.
//
// With a shared pool, the pass publishes its project's turns in flight and
// the queued turns it found no free slot for once it has dispatched, whether
// or not it fails part way.
func (s *Scheduler) Pass(ctx context.Context) error {
	defer s.lock()()
	r, err := s.read()
	if err != nil {
		return err
	}
	var waiting []Wait
	spans := s.options.Shared.spans()
	if s.options.Shared != nil {
		defer func() {
			s.options.Shared.publish(s.repository.Project(), r.own.roles, waiting)
			s.options.Shared.syncHolds(s.repository.Project(), r.own.flights, s.limits, s.options.Now())
		}()
	}
	touched := map[string]bool{}
	candidates, used, order := r.candidates, r.used, s.order(r.served)
	places := s.places(candidates)
	holds := map[turnKey]bool{}
	holdOf := func(c Candidate) (bool, error) {
		key := turnKey{c.Workstream, c.Thread.Identity.ID, c.Turn.Request.TurnID}
		if v, ok := holds[key]; ok || s.options.Holds == nil {
			return v, nil
		}
		v, err := s.options.Holds(c)
		holds[key] = v
		return v, err
	}
	if s.options.Placement != nil {
		if err := s.screen(places, candidates, used, holdOf); err != nil {
			return err
		}
		defer func() { s.options.Placement.Owe(string(s.repository.Project()), places.owed) }()
	}
	for len(candidates) > 0 {
		slices.SortStableFunc(candidates, order)
		c := candidates[0]
		candidates = candidates[1:]
		role := c.Thread.Identity.Role
		if _, pinned := places.pinned[turnOf(c)]; pinned {
			delete(places.owed, c.Turn.Request.TurnID)
			s.options.Placement.Owe(string(c.Project), places.owed)
		}
		if s.options.Holds != nil {
			held, err := holdOf(c)
			if err != nil {
				return err
			}
			if held {
				touched[s.waitInterval(c).Key()] = true
				spans.End(s.waitInterval(c), s.options.Now(), slotspans.Blocked)
				continue
			}
		}
		if reason := s.refusal(used, c); reason != "" {
			waiting = append(waiting, Wait{Candidate: c, Reason: reason})
			touched[s.waitInterval(c).Key()] = true
			s.refused(spans, c, reason, "")
			continue
		}
		if s.options.Admit != nil {
			admitted, err := s.options.Admit(ctx, c)
			if err != nil {
				return err
			}
			if !admitted {
				touched[s.waitInterval(c).Key()] = true
				spans.End(s.waitInterval(c), s.options.Now(), slotspans.Blocked)
				continue
			}
		}
		var taken *fleet.Turn
		client := ""
		if s.options.Placement != nil {
			id := c.Turn.Request.TurnID
			ft := places.turns[turnOf(c)]
			choice := s.options.Placement.Take(ft)
			if choice.Client == "" {
				if choice.Pinned != "" {
					places.owed[id] = choice.Pinned
				}
				s.options.Placement.Owe(string(c.Project), places.owed)
				waiting = append(waiting, Wait{c, choice.Reason, choice.Pinned})
				touched[s.waitInterval(c).Key()] = true
				s.refused(spans, c, choice.Reason, choice.Pinned)
				continue
			}
			taken, client = &ft, choice.Client
		}
		at := s.options.Now()
		if err := s.dispatch(ctx, c, at); err != nil {
			if taken != nil {
				s.options.Placement.Release(taken.ID)
			}
			return fmt.Errorf("workstream %s: %w", c.Workstream, err)
		}
		touched[s.waitInterval(c).Key()] = true
		spans.End(s.waitInterval(c), at, slotspans.Granted)
		used.add(c.Project, c.Workstream, role)
		r.own.add(c.Project, c.Workstream, role)
		r.own.addTurn(flightOf(c, at, client))
		r.served[rotationKey(c.Project, c.Workstream, role)] = at
	}
	s.endGoneWaits(spans, r.loaded, touched)
	return nil
}

// The reasons a queued turn finds no free slot.
const (
	// WaitCapacity: every slot of the turn's role kind is taken, or, for a
	// role without shared slots, its workstream already runs a turn of it.
	WaitCapacity = "capacity"
	// WaitWorkstreamCap: the workstream runs capacity.per_workstream turns.
	WaitWorkstreamCap = "workstream-cap"
	// WaitClient: the role has room, but the client the turn waits for has no
	// free slot, or no client with one can run its profile. A resumed turn waits
	// for the client that holds its session.
	WaitClient = fleet.WaitClient
	// WaitNoClient: no connected, non-draining client can run the turn's main
	// profile, so the turn is not dispatched.
	WaitNoClient = fleet.WaitNoClient
)

// Wait is a queued turn a pass found no free slot for, and why.
type Wait struct {
	Candidate
	Reason string
	// Client is the client a resumed turn waits for a slot of, and empty for
	// any other wait.
	Client string
}

// Slots is the slot accounting of a shared pool, as its projects' passes
// last published it.
type Slots struct {
	// Used counts the turns in flight by role, chief-of-staff turns aside.
	Used map[string]int
	// Waiting lists, in the order the passes offered them, the queued turns
	// that Holds did not hold and that found no free slot.
	Waiting []Wait
}

type reading struct {
	used       usage
	own        usage
	candidates []Candidate
	served     map[rotation]time.Time
	loaded     loaded
}

func (s *Scheduler) read() (reading, error) {
	project := s.repository.Project()
	l, err := load(s.repository)
	if err != nil {
		return reading{}, err
	}
	if s.options.Placement != nil {
		l.client = func(turn string) string {
			client, _ := s.options.Placement.PlacedOn(turn)
			return client
		}
	}
	own := newUsage()
	s.options.Shared.count(l, own)
	used := newUsage()
	used.merge(own)
	if s.options.Shared != nil {
		used.merge(s.options.Shared.reserved(project))
		for role, n := range s.options.Shared.others(project) {
			used.roles[role] += n
		}
	}
	var candidates []Candidate
	for _, stream := range l.streams {
		for _, t := range l.threads[stream] {
			q, ok := next(t)
			if !ok || l.dispatched[turnKey{stream, t.Identity.ID, q.Request.TurnID}] {
				continue
			}
			candidates = append(candidates, Candidate{Project: project, Workstream: stream, Thread: t, Turn: q})
		}
	}
	return reading{used: used, own: own, candidates: candidates, served: l.served, loaded: l}, nil
}

type loaded struct {
	project      config.ProjectID
	streams      []config.WorkstreamID
	threads      map[config.WorkstreamID][]trace.Thread
	dispatched   map[turnKey]bool
	dispatchedAt map[turnKey]time.Time
	served       map[rotation]time.Time
	// client names the client a turn was placed on, when placement knows.
	client func(turn string) string
}

func load(repository *trace.Repository) (loaded, error) {
	var result loaded
	err := repository.WithWorkstreamDiscovery(func(streams []config.WorkstreamID) error {
		var err error
		result, err = loadSnapshot(repository, streams)
		return err
	})
	return result, err
}

func loadSnapshot(repository *trace.Repository, all []config.WorkstreamID) (loaded, error) {
	project := repository.Project()
	out := loaded{project: project, threads: map[config.WorkstreamID][]trace.Thread{}, dispatched: map[turnKey]bool{}, dispatchedAt: map[turnKey]time.Time{}, served: map[rotation]time.Time{}}
	for _, stream := range all {
		kind, err := repository.Kind(stream)
		if err != nil {
			return loaded{}, fmt.Errorf("workstream %s: %w", stream, err)
		}
		if kind == trace.SessionKind {
			continue
		}
		threads, err := repository.Threads(stream)
		if err != nil {
			return loaded{}, fmt.Errorf("workstream %s: %w", stream, err)
		}
		if _, known := kinds.Lookup(kind); !known {
			threads = slices.DeleteFunc(threads, func(t trace.Thread) bool { return t.Identity.Role != trace.ChiefOfStaff })
		}
		records, err := repository.Operations(stream)
		if err != nil {
			return loaded{}, fmt.Errorf("workstream %s: %w", stream, err)
		}
		out.streams = append(out.streams, stream)
		out.threads[stream] = threads
		roles := map[string]string{}
		for _, t := range threads {
			roles[t.Identity.ID] = t.Identity.Role
		}
		for _, record := range records {
			in, err := thread.DecodeTurn(record.Operation)
			if err != nil {
				continue
			}
			key := turnKey{in.Workstream, in.Agent, in.Turn}
			out.dispatched[key] = true
			if at := record.Transition.At; out.dispatchedAt[key].IsZero() || at.Before(out.dispatchedAt[key]) {
				out.dispatchedAt[key] = at
			}
			if role, ok := roles[in.Agent]; ok {
				key := rotationKey(project, in.Workstream, role)
				if at := record.Transition.At; at.After(out.served[key]) {
					out.served[key] = at
				}
			}
		}
	}
	return out, nil
}

func (l loaded) count(used usage) {
	for _, stream := range l.streams {
		for _, t := range l.threads[stream] {
			if q, ok := inFlightTurn(t, stream, l.dispatched); ok {
				used.add(l.project, stream, t.Identity.Role)
				used.addTurn(l.flight(stream, t, q))
			}
		}
	}
}

func (s *Scheduler) order(served map[rotation]time.Time) func(a, b Candidate) int {
	project := s.repository.Project()
	rank := Rank(nil, project)
	if s.options.Priorities != nil {
		rank = Rank(s.options.Priorities(), project)
	}
	return func(a, b Candidate) int {
		ra, rb := a.Thread.Identity.Role, b.Thread.Identity.Role
		return cmp.Or(cmp.Compare(stage(ra), stage(rb)),
			cmp.Compare(rank(a.Workstream), rank(b.Workstream)),
			served[rotationKey(a.Project, a.Workstream, ra)].Compare(served[rotationKey(b.Project, b.Workstream, rb)]),
			strings.Compare(string(a.Project), string(b.Project)),
			strings.Compare(string(a.Workstream), string(b.Workstream)))
	}
}

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
	flights map[turnKey]flight
}

func newUsage() usage {
	return usage{roles: map[string]int{}, streams: map[streamKey]int{}, local: map[localKey]int{}, flights: map[turnKey]flight{}}
}

type streamKey struct {
	project    config.ProjectID
	workstream config.WorkstreamID
}

type localKey struct {
	project    config.ProjectID
	workstream config.WorkstreamID
	name       string
}

type rotation struct {
	project    config.ProjectID
	workstream config.WorkstreamID
	stage      int
}

func rotationKey(project config.ProjectID, stream config.WorkstreamID, role string) rotation {
	return rotation{project, stream, stage(role)}
}

func (u usage) add(project config.ProjectID, stream config.WorkstreamID, role string) {
	if trace.ChiefLike(role) {
		return
	}
	u.roles[role]++
	u.streams[streamKey{project, stream}]++
	u.local[localKey{project, stream, role}]++
}

func (u usage) merge(other usage) {
	for k, n := range other.roles {
		u.roles[k] += n
	}
	for k, n := range other.streams {
		u.streams[k] += n
	}
	for k, n := range other.local {
		u.local[k] += n
	}
}

func (s *Scheduler) refusal(used usage, c Candidate) string {
	limits, role := s.options.Capacity, c.Thread.Identity.Role
	if limits == nil || trace.ChiefLike(role) {
		return ""
	}
	if used.streams[streamKey{c.Project, c.Workstream}] >= limits.PerWorkstream {
		return WaitWorkstreamCap
	}
	return s.roleRefusal(used, c)
}

func (s *Scheduler) roleRefusal(used usage, c Candidate) string {
	limits, role := s.options.Capacity, c.Thread.Identity.Role
	if limits == nil || trace.ChiefLike(role) {
		return ""
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

func inFlightTurn(t trace.Thread, stream config.WorkstreamID, dispatched map[turnKey]bool) (trace.QueuedTurn, bool) {
	for _, q := range t.Turns {
		if q.CompletedAt.IsZero() {
			if q.Claim != nil {
				return q, t.Status != "interrupted"
			}
			return q, dispatched[turnKey{stream, t.Identity.ID, q.Request.TurnID}]
		}
	}
	return trace.QueuedTurn{}, false
}

type turnKey struct {
	workstream config.WorkstreamID
	agent      string
	turn       string
}

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
	return Dispatch(ctx, s.repository, c, at)
}

// Dispatch publishes a durable turn intent without capacity admission. Callers
// provide the thread's oldest eligible turn and serialize admission per thread.
func Dispatch(ctx context.Context, repository *trace.Repository, c Candidate, at time.Time) error {
	agent, req := c.Thread.Identity.ID, c.Turn.Request
	subject := Subject(agent)
	state, err := repository.Workflow(c.Workstream, subject)
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
	_, err = repository.Transact(ctx, tx)
	return err
}
