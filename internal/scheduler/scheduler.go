// Package scheduler dispatches queued workstream turns from durable trace state.
package scheduler

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/thread"
	"github.com/kpenfound/osmia/internal/trace"
)

// Actor is the provenance of dispatch transitions.
var Actor = trace.Actor{Kind: "service", ID: "scheduler"}

// Candidate is the next turn of one thread, ready to be dispatched.
type Candidate struct {
	Workstream config.WorkstreamID
	Thread     trace.Thread
	Turn       trace.QueuedTurn
}

type Options struct {
	Now func() time.Time
	// Admit is the single dispatch gate: a candidate it declines stays queued
	// and is offered again on a later pass. Nil admits every candidate.
	Admit func(context.Context, Candidate) (bool, error)
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
type Scheduler struct {
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
func (s *Scheduler) Pass(ctx context.Context) error {
	streams, err := s.repository.Workstreams()
	if err != nil {
		return err
	}
	threads := map[config.WorkstreamID][]trace.Thread{}
	for _, stream := range streams {
		if threads[stream], err = s.repository.Threads(stream); err != nil {
			return fmt.Errorf("workstream %s: %w", stream, err)
		}
	}
	dispatched := map[turnKey]bool{}
	for _, stream := range streams {
		records, err := s.repository.Operations(stream)
		if err != nil {
			return fmt.Errorf("workstream %s: %w", stream, err)
		}
		for _, record := range records {
			// The dispatcher refuses input it cannot decode, and the controller
			// records that refusal as a retry, so such an operation runs no turn.
			if in, err := thread.DecodeTurn(record.Operation); err == nil {
				dispatched[turnKey{in.Workstream, in.Agent, in.Turn}] = true
			}
		}
	}
	for _, stream := range streams {
		for _, t := range threads[stream] {
			q, ok := next(t)
			if !ok || dispatched[turnKey{stream, t.Identity.ID, q.Request.TurnID}] {
				continue
			}
			c := Candidate{Workstream: stream, Thread: t, Turn: q}
			if s.options.Admit != nil {
				admitted, err := s.options.Admit(ctx, c)
				if err != nil {
					return err
				}
				if !admitted {
					continue
				}
			}
			if err := s.dispatch(ctx, c); err != nil {
				return fmt.Errorf("workstream %s: %w", stream, err)
			}
		}
	}
	return nil
}

type turnKey struct {
	workstream config.WorkstreamID
	agent      string
	turn       string
}

// next returns the thread's oldest unfinished turn unless it is claimed. Turns
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

func (s *Scheduler) dispatch(ctx context.Context, c Candidate) error {
	agent, req := c.Thread.Identity.ID, c.Turn.Request
	subject := Subject(agent)
	state, err := s.repository.Workflow(c.Workstream, subject)
	if err != nil {
		return err
	}
	data, _ := json.Marshal([]string{agent, req.TurnID})
	id := fmt.Sprintf("dispatch_%x", sha256.Sum256(data))
	event := trace.EventID(id, "turn")
	project := s.repository.Project()
	op, err := thread.TurnOperation(project, event, thread.TurnInput{Workstream: c.Workstream, Agent: agent, Turn: req.TurnID})
	if err != nil {
		return err
	}
	h := trace.Header{Schema: "osmia.trace.transition", Version: trace.Version, ID: id, Revision: 1, Project: project, Workstream: c.Workstream, Unit: req.Unit, At: s.options.Now(), Actor: Actor, Cause: req.ID, Depth: req.Depth}
	tx := trace.Transaction{ExpectedVersion: state.Version, Transition: trace.Transition{Header: h, Subject: subject, From: state.Value, To: req.TurnID,
		Reason: fmt.Sprintf("Queued turn %s dispatched on thread %s", req.TurnID, req.ThreadID)},
		Events: []trace.Event{{ID: event, Kind: "turn", Body: "Run turn " + req.TurnID, Operation: &op}}}
	_, err = s.repository.Transact(ctx, tx)
	return err
}
