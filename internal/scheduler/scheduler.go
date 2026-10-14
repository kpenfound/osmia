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
// A project's turns enter flight only in its own passes, so its published
// count is never below its turns in flight. A turn that completes holds its
// slot for the other projects until its own project's next pass, which its
// completion wakes.
type Shared struct {
	mu sync.Mutex
	// usageMu guards usage alone and is never held while taking another
	// lock, so a project joins or leaves the pool whatever its caller holds.
	usageMu sync.Mutex
	// usage is what each project in the pool last published.
	usage map[config.ProjectID]published
	// Changed, when set, runs once a project's published turns in flight or
	// waiting turns change, or the project leaves the pool. It must not call
	// back into the pool.
	Changed func()
}

// published is one project's turns in flight by role kind, as its own
// scheduler last counted them, and the queued turns its last pass found no
// free slot for.
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

// same reports whether u and other count the same turns in flight and list
// the same waiting turns, for the same reasons, in the same order.
func (u published) same(other published) bool {
	return maps.Equal(u.roles, other.roles) && slices.EqualFunc(u.waiting, other.waiting, func(a, b Wait) bool {
		return a.Project == b.Project && a.Workstream == b.Workstream && a.Thread.Identity.ID == b.Thread.Identity.ID && a.Turn.Request.TurnID == b.Turn.Request.TurnID && a.Reason == b.Reason
	})
}

// others returns the published counts by role kind of every project in the
// pool but project.
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
// stage the candidate of the highest-priority workstream goes first, and among
// workstreams of equal priority the one whose last turn of that stage was
// dispatched least recently, so equal workstreams take the stage's slots in
// turn. The last dispatch of each stage is read from the trace's turn
// operations, so the rotation continues across restarts. A candidate that
// Holds holds, that finds no free slot or that Admit declines is not
// dispatched and leaves its workstream's place in the rotation unchanged.
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
	if s.options.Shared != nil {
		defer func() { s.options.Shared.publish(s.repository.Project(), r.own.roles, waiting) }()
	}
	candidates, used, order := r.candidates, r.used, s.order(r.served)
	for len(candidates) > 0 {
		slices.SortStableFunc(candidates, order)
		c := candidates[0]
		candidates = candidates[1:]
		role := c.Thread.Identity.Role
		if s.options.Holds != nil {
			held, err := s.options.Holds(c)
			if err != nil {
				return err
			}
			if held {
				continue
			}
		}
		if reason := s.refusal(used, c); reason != "" {
			waiting = append(waiting, Wait{c, reason})
			continue
		}
		if s.options.Admit != nil {
			admitted, err := s.options.Admit(ctx, c)
			if err != nil {
				return err
			}
			if !admitted {
				continue
			}
		}
		at := s.options.Now()
		if err := s.dispatch(ctx, c, at); err != nil {
			return fmt.Errorf("workstream %s: %w", c.Workstream, err)
		}
		used.add(c.Project, c.Workstream, role)
		r.own.add(c.Project, c.Workstream, role)
		r.served[rotationKey(c.Project, c.Workstream, role)] = at
	}
	return nil
}

// The reasons a queued turn finds no free slot.
const (
	// WaitCapacity: every slot of the turn's role kind is taken, or, for a
	// role without shared slots, its workstream already runs a turn of it.
	WaitCapacity = "capacity"
	// WaitWorkstreamCap: the workstream runs capacity.per_workstream turns.
	WaitWorkstreamCap = "workstream-cap"
)

// Wait is a queued turn a pass found no free slot for, and why.
type Wait struct {
	Candidate
	Reason string
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

// reading is what a pass reads before it offers slots: the turns in flight,
// the queued turns and each stage's last dispatch.
type reading struct {
	// used counts the turns in flight of every project in the pool.
	used usage
	// own counts the turns in flight of the scheduler's own project.
	own        usage
	candidates []Candidate
	served     map[rotation]time.Time
}

// read reads every workstream's threads, then its turn operations. With a
// shared pool it adds the counts the pool's other projects published, and
// never reads their traces.
func (s *Scheduler) read() (reading, error) {
	project := s.repository.Project()
	l, err := load(s.repository)
	if err != nil {
		return reading{}, err
	}
	own := newUsage()
	l.count(own)
	used := newUsage()
	used.merge(own)
	if s.options.Shared != nil {
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
	return reading{used: used, own: own, candidates: candidates, served: l.served}, nil
}

// loaded is one trace's workstreams, their threads, the turns its turn
// operations name and each stage's last dispatch.
type loaded struct {
	project    config.ProjectID
	streams    []config.WorkstreamID
	threads    map[config.WorkstreamID][]trace.Thread
	dispatched map[turnKey]bool
	served     map[rotation]time.Time
}

// load reads every workstream's threads of repository, then its turn
// operations. A session workstream is excluded: it has no spec, plan or unit
// for this scheduler's roles to serve, and its own turns are dispatched
// directly, never through this scheduler, so it never takes a slot or a
// place in the rotation of chief-of-staff, mason, reviewer or committee
// turns.
func load(repository *trace.Repository) (loaded, error) {
	project := repository.Project()
	all, err := repository.Workstreams()
	if err != nil {
		return loaded{}, err
	}
	streams := make([]config.WorkstreamID, 0, len(all))
	for _, stream := range all {
		kind, err := repository.Kind(stream)
		if err != nil {
			return loaded{}, fmt.Errorf("workstream %s: %w", stream, err)
		}
		if kind != trace.SessionKind {
			streams = append(streams, stream)
		}
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
	served := map[rotation]time.Time{}
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
				key := rotationKey(project, in.Workstream, role)
				if at := record.Transition.At; at.After(served[key]) {
					served[key] = at
				}
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

func newUsage() usage {
	return usage{roles: map[string]int{}, streams: map[streamKey]int{}, local: map[localKey]int{}}
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

// rotation names one workstream's place in a stage's rotation.
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

// merge adds the counts of other to u.
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

// refusal returns why the candidate's turn has no free slot, or "" when it
// has one.
func (s *Scheduler) refusal(used usage, c Candidate) string {
	limits, role := s.options.Capacity, c.Thread.Identity.Role
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
