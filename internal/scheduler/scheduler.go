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
	"github.com/kpenfound/osmia/internal/coreadapter"
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

// Pass reads every workstream's threads and turn operations and dispatches the
// next turn of each thread that has none in flight. A thread's turn is in
// flight while it is claimed or while its operation exists and the turn has
// not completed. Pass makes no model call.
func (s *Scheduler) Pass(ctx context.Context) error {
	streams, err := s.repository.Workstreams()
	if err != nil {
		return err
	}
	for _, stream := range streams {
		if err := s.stream(ctx, stream); err != nil {
			return fmt.Errorf("workstream %s: %w", stream, err)
		}
	}
	return nil
}

func (s *Scheduler) stream(ctx context.Context, stream config.WorkstreamID) error {
	threads, err := s.repository.Threads(stream)
	if err != nil {
		return err
	}
	records, err := s.repository.Operations(stream)
	if err != nil {
		return err
	}
	dispatched := map[[2]string]bool{}
	for _, record := range records {
		op := record.Operation
		if op.Boundary != coreadapter.RunnerBoundary || op.Action != thread.TurnAction {
			continue
		}
		var in thread.TurnInput
		if err := json.Unmarshal(op.Input, &in); err != nil {
			return fmt.Errorf("turn operation %s: %w", record.EventID, err)
		}
		dispatched[[2]string{in.Agent, in.Turn}] = true
	}
	for _, t := range threads {
		q, ok := next(t)
		if !ok || dispatched[[2]string{t.Identity.ID, q.Request.TurnID}] {
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
			return err
		}
	}
	return nil
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
