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
	"github.com/kpenfound/osmia/internal/jev"
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
	// OnProviderLimit records a blocked provider report after its attempt is durable.
	OnProviderLimit func(coreadapter.Profile, coreadapter.ProviderLimit) error
	// AdmitRole checks a role pause before claiming a queued turn.
	AdmitRole func(string) bool
	// MaxRetries bounds the same-profile retries of an infrastructure failure.
	MaxRetries int
	// ResumableRoles reports whether a thread's role may look past an
	// interrupted turn with no explicit Stop to an earlier source, so a
	// session's restart recovery finds its last recorded model session, and
	// whether a CheckResume implementation with no verification of its own
	// may fall back to a role-blind heuristic (ResumeFallbackAllowed). It
	// never changes whether CheckResume is asked, or what a CheckResume that
	// answers for itself says. Nil means neither applies to any role.
	ResumableRoles func(string) bool
	// LegacyBaseline recovers the system prompt of a saved session whose
	// attempts did not record it. Nil, or an answer of false, replays the
	// turn into a new session instead of resuming with a guessed prompt.
	LegacyBaseline LegacyBaseline
	// Place chooses where a claimed turn runs, before it starts, and may wait for
	// a client slot. A turn whose thread's saved session is on another client
	// replays its log instead of resuming, and fallback profiles the client
	// cannot run are skipped. Nil runs every turn where Turns puts it, with
	// every profile.
	Place func(context.Context, trace.Thread, trace.QueuedTurn) (Placed, error)
	// Jev classifies clean mason responses first; a fallback, which is every
	// judgment while Jev is unconfigured, leaves them to the code rules. It
	// must not be nil.
	Jev *jev.Judge
	// Advise returns advisory text for a claimed turn, which the turn's
	// prompt ends with and its response records, or "" for none. It runs
	// inside turn execution, before the agent session starts, so it may ask
	// Jev. Nil adds nothing.
	Advise func(context.Context, trace.TurnRequest, coreadapter.Scope) string
}

// Placed is where a claimed turn runs.
type Placed struct {
	// Client is the ID of the client the turn runs on.
	Client string
	// Runs reports whether that client can run a profile. Nil means every
	// profile.
	Runs func(profile string) bool
	// Release gives back the client slot the turn took, when the turn ends. Nil
	// gives back nothing.
	Release func()
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
	if r.AdmitRole != nil {
		thread, err := r.Store.Thread(stream, agent)
		if err != nil {
			return trace.QueuedTurn{}, err
		}
		if !r.AdmitRole(thread.Identity.Role) {
			return trace.QueuedTurn{}, fmt.Errorf("provider usage limit pauses role %s", thread.Identity.Role)
		}
	}
	q, err := r.Store.ClaimTurn(ctx, stream, agent, rand.Text(), prepared.SessionDirectory, r.Now())
	if err != nil {
		return q, err
	}
	observeClaim(ctx, q)

	t, err := r.Store.Thread(stream, agent)
	if err != nil {
		return q, err
	}
	req := q.Request
	prepared.Scope = coreadapter.Scope{Project: string(r.Store.Project()), Workstream: string(stream), Unit: req.Unit, Thread: req.ThreadID, Turn: req.TurnID, Role: t.Identity.Role}
	prepared.Profile, prepared.SystemPrompt, prepared.Prompt = req.Profile, req.SystemPrompt, req.Prompt
	if req.Context != "" {
		prepared.Context = []string{req.Context}
	}
	var advice string
	if run := execution(ctx); r.Advise != nil && stopped(run) == nil {
		if advice = r.Advise(run, req, prepared.Scope); stopped(run) != nil {
			advice = ""
		}
		if advice != "" {
			prepared.Prompt += "\n\n" + advice
		}
	}
	result, runErr, persistErr := r.execute(ctx, t, &q, prepared)
	if persistErr != nil && result.StartedAt.IsZero() {
		return q, errors.Join(runErr, persistErr)
	}
	return r.settle(ctx, stream, agent, t, q, prepared.Scope, result, runErr, persistErr, advice)
}

func (r Runner) settle(ctx context.Context, stream config.WorkstreamID, agent string, t trace.Thread, q trace.QueuedTurn, scope coreadapter.Scope, result coreadapter.SessionResult, runErr, persistErr error, advice string) (trace.QueuedTurn, error) {
	req := q.Request
	h := req.Header
	h.Project = r.Store.Project()
	h.Schema, h.ID, h.At, h.Actor = "osmia.trace.turn-response", trace.EventID(req.ID, "response"), r.Now(), trace.Actor{Kind: "service", ID: "thread-runner"}
	response := trace.TurnResponse{Header: h, AgentID: agent, ThreadID: req.ThreadID, TurnID: req.TurnID, RequestID: req.ID, RequestRevision: req.Revision, Result: result, Advice: advice}
	var stop *Stop
	switch {
	case errors.As(runErr, &stop):
		response.Stop = &stop.TurnStop
	case len(q.Attempts) > 0:
		last := q.Attempts[len(q.Attempts)-1]
		response.Failure, response.FailureClass = last.Failure, last.FailureClass
	case runErr != nil:
		response.Failure = runErr.Error()
	}
	if t.Identity.Role == "mason" {
		response.Classification = trace.ClassifyMasonTurn(result, response.Failure)
		if response.Classification != nil {
			r.judgeClassification(ctx, req, scope, &response)
		}
	}
	q.Response = &response
	if persistErr != nil {
		return q, errors.Join(runErr, persistErr)
	}
	cleanup := context.WithoutCancel(ctx)
	err := func() error {
		if err := r.Store.CaptureTurn(cleanup, q.Claim.Token, response); err != nil {
			return err
		}
		if err := r.costs(cleanup, t.Identity.Role, q); err != nil {
			return err
		}
		completed := r.Now()
		q.CompletedAt = completed
		return r.Store.CompleteTurn(cleanup, stream, agent, req.TurnID, q.Claim.Token, completed)
	}()
	return q, errors.Join(runErr, err)
}

// Adopt completes a turn that a previous service session started on a client
// and that the client reattached after the restart, with the result the
// client returned. The turn becomes this session's, its attempt takes the
// result, and the turn's response is recorded as for an uninterrupted turn. It
// does not retry: a failed result is recorded as a failed turn.
func (r Runner) Adopt(ctx context.Context, stream config.WorkstreamID, agent, turn string, result coreadapter.SessionResult, runErr error) (trace.QueuedTurn, error) {
	if r.Store == nil || r.Now == nil {
		return trace.QueuedTurn{}, fmt.Errorf("thread runner requires store and clock")
	}
	q, err := r.Store.ReclaimTurn(ctx, stream, agent, turn)
	if err != nil {
		return q, err
	}
	t, err := r.Store.Thread(stream, agent)
	if err != nil {
		return q, err
	}
	req := q.Request
	scope := coreadapter.Scope{Project: string(r.Store.Project()), Workstream: string(stream), Unit: req.Unit, Thread: req.ThreadID, Turn: req.TurnID, Role: t.Identity.Role}
	if result.StartedAt.IsZero() {
		result.StartedAt = q.Claim.At
	}
	result.SessionDirectory = q.Claim.SessionDirectory
	if errors.Is(runErr, context.Canceled) {
		result.Cancelled = true
	}
	if !coreadapter.ValidSession(result.Session) {
		result.Session = coreadapter.BackendSession{}
		if runErr == nil {
			runErr = fmt.Errorf("the client returned no valid session reference")
		}
	}
	if n := len(q.Attempts); n > 0 {
		a := q.Attempts[n-1]
		if a.Result == nil {
			captured := result
			a.Result = &captured
			if failed(result, runErr) {
				a.FailureClass, a.Failure = coreadapter.Infrastructure, "the session failed on its client"
				if runErr != nil {
					a.Failure = runErr.Error()
				}
			}
			if err := r.Store.RecordAttempt(ctx, stream, agent, turn, q.Claim.Token, a); err != nil {
				return q, err
			}
			q.Attempts[n-1] = a
		}
	}
	return r.settle(ctx, stream, agent, t, q, scope, result, runErr, nil, "")
}
