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
	Store *trace.Repository
	Turns coreadapter.Turns
	Now   func() time.Time
}

// RunNext never retries execution. Once claimed, persistence errors leave the
// turn reserved; retry capture with the returned response and token.
// Callers must not share per-turn resource leases between competing invocations.
func (r Runner) RunNext(ctx context.Context, stream config.WorkstreamID, agent string, prepared coreadapter.PreparedTurn) (trace.QueuedTurn, error) {
	if r.Store == nil || r.Turns == nil || r.Now == nil {
		return trace.QueuedTurn{}, fmt.Errorf("thread runner requires store, turns and clock")
	}
	t, err := r.Store.Thread(stream, agent)
	if err != nil {
		return trace.QueuedTurn{}, err
	}
	q, err := r.Store.ClaimTurn(ctx, stream, agent, rand.Text(), prepared.SessionDirectory, r.Now())
	if err != nil {
		return q, err
	}
	req := q.Request
	prepared.Scope = coreadapter.Scope{Project: string(req.Project), Workstream: string(stream), Unit: req.Unit, Thread: req.ThreadID, Turn: req.TurnID, Role: t.Identity.Role}
	prepared.Profile, prepared.SystemPrompt, prepared.Prompt, prepared.History, prepared.Resume = req.Profile, req.SystemPrompt, req.Prompt, req.History, req.Resume
	result, runErr := r.Turns.Run(ctx, prepared)
	if result.StartedAt.IsZero() {
		result.StartedAt = q.Claim.At
	}
	result.SessionDirectory = q.Claim.SessionDirectory
	if errors.Is(runErr, context.Canceled) {
		result.Cancelled = true
	}
	h := req.Header
	h.Schema, h.ID, h.At, h.Actor = "osmia.trace.turn-response", trace.EventID(req.ID, "response"), r.Now(), trace.Actor{Kind: "service", ID: "thread-runner"}
	response := trace.TurnResponse{Header: h, AgentID: agent, ThreadID: req.ThreadID, TurnID: req.TurnID, RequestID: req.ID, RequestRevision: req.Revision, Result: result}
	if runErr != nil {
		response.Failure = runErr.Error()
	}
	q.Response = &response
	// Cancellation stops execution, not durable recording of its partial result.
	cleanup := context.WithoutCancel(ctx)
	if err := r.Store.CaptureTurn(cleanup, q.Claim.Token, response); err != nil {
		return q, errors.Join(runErr, err)
	}
	completed := r.Now()
	q.CompletedAt = completed
	if err := r.Store.CompleteTurn(cleanup, stream, agent, req.TurnID, q.Claim.Token, completed); err != nil {
		return q, errors.Join(runErr, err)
	}
	return q, runErr
}
