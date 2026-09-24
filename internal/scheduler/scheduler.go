// Package scheduler dispatches queued workstream turns from durable trace state.
package scheduler

import (
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
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
	// Priorities returns the runtime priority order of workstreams, read once
	// per pass. Nil gives every workstream the same priority.
	Priorities func() []runtime.Priority
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
// one Scheduler run one at a time; passes of a second scheduler on the same
// trace can run concurrently with them and overbook.
type Scheduler struct {
	mu         sync.Mutex
	repository *trace.Repository
	options    Options
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
// finds no free slot or that Admit declines is not dispatched and leaves its
// workstream's place in the rotation unchanged.
func (s *Scheduler) Pass(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	project := s.repository.Project()
	streams, err := s.repository.Workstreams()
	if err != nil {
		return err
	}
	threads := map[config.WorkstreamID][]trace.Thread{}
	roles := map[localKey]string{}
	for _, stream := range streams {
		if threads[stream], err = s.repository.Threads(stream); err != nil {
			return fmt.Errorf("workstream %s: %w", stream, err)
		}
		for _, t := range threads[stream] {
			roles[localKey{project, stream, t.Identity.ID}] = t.Identity.Role
		}
	}
	dispatched := map[turnKey]bool{}
	served := map[rotation]time.Time{}
	for _, stream := range streams {
		records, err := s.repository.Operations(stream)
		if err != nil {
			return fmt.Errorf("workstream %s: %w", stream, err)
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
	used := usage{roles: map[string]int{}, streams: map[streamKey]int{}, local: map[localKey]int{}}
	var candidates []Candidate
	for _, stream := range streams {
		for _, t := range threads[stream] {
			if inFlight(t, stream, dispatched) {
				used.add(project, stream, t.Identity.Role)
			}
		}
	}
	for _, stream := range streams {
		for _, t := range threads[stream] {
			q, ok := next(t)
			if !ok || dispatched[turnKey{stream, t.Identity.ID, q.Request.TurnID}] {
				continue
			}
			candidates = append(candidates, Candidate{Project: project, Workstream: stream, Thread: t, Turn: q})
		}
	}
	rank := Rank(nil, project)
	if s.options.Priorities != nil {
		rank = Rank(s.options.Priorities(), project)
	}
	// Ties keep trace order, for stable admission across passes.
	order := func(a, b Candidate) int {
		ra, rb := a.Thread.Identity.Role, b.Thread.Identity.Role
		return cmp.Or(cmp.Compare(stage(ra), stage(rb)),
			cmp.Compare(rank(a.Workstream), rank(b.Workstream)),
			served[rotationKey(a.Project, a.Workstream, ra)].Compare(served[rotationKey(b.Project, b.Workstream, rb)]),
			strings.Compare(string(a.Project), string(b.Project)),
			strings.Compare(string(a.Workstream), string(b.Workstream)))
	}
	for len(candidates) > 0 {
		slices.SortStableFunc(candidates, order)
		c := candidates[0]
		candidates = candidates[1:]
		role := c.Thread.Identity.Role
		if !s.fits(used, c) {
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
		served[rotationKey(c.Project, c.Workstream, role)] = at
	}
	return nil
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

// fits reports whether the candidate's turn has a free slot.
func (s *Scheduler) fits(used usage, c Candidate) bool {
	limits, role := s.options.Capacity, c.Thread.Identity.Role
	if limits == nil || role == trace.ChiefOfStaff {
		return true
	}
	if used.streams[streamKey{c.Project, c.Workstream}] >= limits.PerWorkstream {
		return false
	}
	switch role {
	case "mason":
		return used.roles[role] < limits.Masons
	case "reviewer":
		return used.roles[role] < limits.Reviewers
	case "committee":
		return used.roles[role] < limits.Committee
	}
	return used.local[localKey{c.Project, c.Workstream, role}] < 1
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
