// Package thread delivers durable requests through the prepared-turn adapter.
package thread

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"time"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/trace"
)

// Runner executes one queued request. Resources and outcome policy are supplied
// by the caller; thread identity, profile and messages come from the durable queue.
// No backend transcript is consulted. Now is required so callers control time.
type Runner struct {
	Store        *trace.Repository
	Turns        coreadapter.Turns
	Now          func() time.Time
	ReplayLimits ReplayLimits
	// Fallbacks maps a selected profile name to a service-approved fallback.
	Fallbacks map[string]coreadapter.Profile
	// MaxRetries bounds the same-profile retries of an infrastructure failure.
	MaxRetries        int
	Classifier        func(context.Context, coreadapter.PreparedTurn) (coreadapter.SessionResult, error)
	ClassifierProfile *coreadapter.Profile
}

// RunNext retries infrastructure failures within the turn, as execute
// describes, and records each attempt before and after it runs. Once claimed,
// persistence errors leave the turn reserved for reconciliation with its
// durable evidence.
// Callers must not share per-turn resource leases between competing invocations.
func (r Runner) RunNext(ctx context.Context, stream config.WorkstreamID, agent string, prepared coreadapter.PreparedTurn) (trace.QueuedTurn, error) {
	if r.Store == nil || r.Turns == nil || r.Now == nil {
		return trace.QueuedTurn{}, fmt.Errorf("thread runner requires store, turns and clock")
	}
	if r.MaxRetries < 0 || r.MaxRetries > 10 {
		return trace.QueuedTurn{}, fmt.Errorf("max retries must be between zero and ten")
	}
	q, err := r.Store.ClaimTurn(ctx, stream, agent, rand.Text(), prepared.SessionDirectory, r.Now())
	if err != nil {
		return q, err
	}

	// The claim serializes continuation selection against competing dispatchers.
	t, err := r.Store.Thread(stream, agent)
	if err != nil {
		return q, err
	}
	req := q.Request
	prepared.Scope = coreadapter.Scope{Project: string(req.Project), Workstream: string(stream), Unit: req.Unit, Thread: req.ThreadID, Turn: req.TurnID, Role: t.Identity.Role}
	prepared.Profile, prepared.SystemPrompt, prepared.Prompt = req.Profile, req.SystemPrompt, req.Prompt
	result, runErr, persistErr := r.execute(ctx, t, &q, prepared)
	if persistErr != nil && result.StartedAt.IsZero() {
		return q, errors.Join(runErr, persistErr)
	}
	h := req.Header
	h.Schema, h.ID, h.At, h.Actor = "osmia.trace.turn-response", trace.EventID(req.ID, "response"), r.Now(), trace.Actor{Kind: "service", ID: "thread-runner"}
	response := trace.TurnResponse{Header: h, AgentID: agent, ThreadID: req.ThreadID, TurnID: req.TurnID, RequestID: req.ID, RequestRevision: req.Revision, Result: result}
	var stop *Stop
	switch {
	case errors.As(runErr, &stop):
		response.Stop = &stop.TurnStop
	case len(q.Attempts) > 0:
		// The final attempt is the turn's result.
		last := q.Attempts[len(q.Attempts)-1]
		response.Failure, response.FailureClass = last.Failure, last.FailureClass
	case runErr != nil:
		response.Failure = runErr.Error()
	}
	if t.Identity.Role == "mason" {
		response.Classification = trace.ClassifyMasonTurn(result, response.Failure)
		if response.Classification != nil && r.Classifier != nil && r.ClassifierProfile != nil {
			r.classify(ctx, prepared, &response)
		}
	}
	q.Response = &response
	if persistErr != nil {
		return q, errors.Join(runErr, persistErr)
	}
	// Cancellation stops execution, not durable recording of its partial result.
	cleanup := context.WithoutCancel(ctx)
	if err := r.Store.CaptureTurn(cleanup, q.Claim.Token, response); err != nil {
		return q, errors.Join(runErr, err)
	}
	if err := r.costs(cleanup, t.Identity.Role, q); err != nil {
		return q, errors.Join(runErr, err)
	}
	completed := r.Now()
	q.CompletedAt = completed
	if err := r.Store.CompleteTurn(cleanup, stream, agent, req.TurnID, q.Claim.Token, completed); err != nil {
		return q, errors.Join(runErr, err)
	}
	return q, runErr
}
