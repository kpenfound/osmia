package trace

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"slices"
	"time"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
)

var ErrNoTurn = errors.New("thread has no eligible turn")

// Thread retains accepted requests in durable sequence order, including finished
// turns. Active is a turn ID. Interrupted active turns remain reserved on reopen.
type Thread struct {
	Identity Agent                      `json:"identity"`
	Session  coreadapter.BackendSession `json:"session"`
	Status   string                     `json:"status"`
	Active   string                     `json:"active,omitempty"`
	Turns    []QueuedTurn               `json:"turns,omitempty"`
}

// Parked reports whether the thread's last turn ended waiting and no turn has
// been queued since. A parked thread holds no turn and is not scheduled; the
// next queued turn unparks it.
func (t Thread) Parked() bool {
	if t.Status != "waiting" {
		return false
	}
	for _, q := range t.Turns {
		if q.CompletedAt.IsZero() {
			return false
		}
	}
	return true
}

type QueuedTurn struct {
	Attempts    []TurnAttempt `json:"attempts,omitempty"`
	Sequence    uint64        `json:"sequence"`
	Request     TurnRequest   `json:"request"`
	Claim       *TurnClaim    `json:"claim,omitempty"`
	Response    *TurnResponse `json:"response,omitempty"`
	CompletedAt time.Time     `json:"completed_at,omitempty"`
}
type TurnClaim struct {
	Token            string    `json:"token"`
	ServiceSession   string    `json:"service_session"`
	SessionDirectory string    `json:"session_directory"`
	At               time.Time `json:"at"`
}

// Status reports one turn's execution state: running, captured, or the thread
// status its completion retains.
func (t QueuedTurn) Status() string {
	if t.Response == nil {
		return "running"
	}
	if t.CompletedAt.IsZero() {
		return "captured"
	}
	r := t.Response
	switch {
	case r.Result.Cancelled:
		return "interrupted"
	case r.Failure != "" || r.Result.IsError || r.Result.TimedOut || r.Result.ExitCode != 0 || r.Result.Signal != 0:
		return "failed"
	case r.Result.Outcome != nil && r.Result.Outcome.Status == "waiting":
		return "waiting"
	default:
		return "idle"
	}
}

func validateThreads(threads map[string]Thread, records []Record, project config.ProjectID, stream config.WorkstreamID) error {
	expected := map[string]Record{}
	for id, t := range threads {
		a := t.Identity
		if id != a.ID || a.Project != project || a.Workstream != stream || a.Revision != 1 {
			return fmt.Errorf("invalid thread identity")
		}
		if err := validate(a); err != nil {
			return err
		}
		expected[recordKey(a)] = a
		status, active, session := "idle", "", a.Session
		seenTurns, seenTokens := map[string]bool{}, map[string]bool{}
		pending := false
		for i, q := range t.Turns {
			req := q.Request
			if err := validateAttempts(q); err != nil {
				return err
			}
			if err := validate(req); err != nil {
				return err
			}
			if req.Project != project || req.Workstream != stream || req.AgentID != id || req.ThreadID != a.ThreadID || req.Revision != 1 || q.Sequence != uint64(i+1) || seenTurns[req.TurnID] {
				return fmt.Errorf("invalid queued turn")
			}
			seenTurns[req.TurnID] = true
			if _, ok := expected[recordKey(req)]; ok {
				return ErrConflict
			}
			expected[recordKey(req)] = req
			if q.Claim == nil {
				if q.Response != nil || !q.CompletedAt.IsZero() {
					return fmt.Errorf("unclaimed turn has result")
				}
				pending = true
				continue
			}
			c := q.Claim
			if pending || active != "" || !key(c.Token) || !key(c.ServiceSession) || !present(c.SessionDirectory) || c.At.IsZero() || c.At.Before(req.At) || seenTokens[c.Token] {
				return fmt.Errorf("invalid turn claim or ordering")
			}
			seenTokens[c.Token] = true
			if q.Response != nil {
				res := *q.Response
				if err := validate(res); err != nil {
					return err
				}
				if res.Project != project || res.Workstream != stream || res.Unit != req.Unit || res.AgentID != id || res.ThreadID != a.ThreadID || res.TurnID != req.TurnID || res.RequestID != req.ID || res.RequestRevision != req.Revision || res.Revision != 1 || res.Cause != req.Cause || res.Depth != req.Depth || res.At.Before(c.At) || res.Result.SessionDirectory != c.SessionDirectory || (res.Result.Session.Backend != "" && res.Result.Session.Backend != responseProfile(q).Backend) {
					return fmt.Errorf("invalid captured response provenance")
				}
				if _, ok := expected[recordKey(res)]; ok {
					return ErrConflict
				}
				expected[recordKey(res)] = res
				if res.Result.Session != (coreadapter.BackendSession{}) {
					session = res.Result.Session
				}
			}
			if q.CompletedAt.IsZero() {
				active = req.TurnID
			} else if q.Response == nil || q.CompletedAt.Before(q.Response.At) {
				return fmt.Errorf("invalid turn completion")
			}
			status = q.Status()
		}
		if t.Active != active || t.Status != status || t.Session != session {
			return fmt.Errorf("thread state differs from turn history")
		}
	}
	found := map[string]bool{}
	for _, rec := range records {
		if rec.header().Workstream != stream {
			continue
		}
		var agentID string
		switch v := rec.(type) {
		case Agent:
			agentID = v.ID
		case TurnRequest:
			agentID = v.AgentID
		case TurnResponse:
			agentID = v.AgentID
		default:
			continue
		}
		if _, managed := threads[agentID]; !managed {
			continue
		}
		k := recordKey(rec)
		want, ok := expected[k]
		if !ok || found[k] || !equalJSON(want, rec) {
			return fmt.Errorf("owned thread log differs from workflow: %w", ErrConflict)
		}
		found[k] = true
	}
	if len(found) != len(expected) {
		return fmt.Errorf("owned thread record missing")
	}
	return nil
}

// CreateThread atomically records identity and an empty durable queue. An exact
// retry is harmless; an existing identity cannot be rebound to another role.
func (r *Repository) CreateThread(ctx context.Context, agent Agent) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := validate(agent); err != nil {
		return err
	}
	if agent.Project != r.project || agent.Revision != 1 {
		return ErrConflict
	}
	log, _, err := r.loadWorkflow(agent.Workstream)
	if err != nil {
		return err
	}
	if old, ok := log.Threads[agent.ID]; ok {
		if equalJSON(old.Identity, agent) {
			return nil
		}
		return ErrConflict
	}
	if log.Threads == nil {
		log.Threads = map[string]Thread{}
	}
	log.Threads[agent.ID] = Thread{Identity: agent, Session: agent.Session, Status: "idle"}
	return r.saveThread(ctx, agent.Workstream, log, agent)
}

// ChiefOfStaff is the agent and thread ID of a workstream's chief-of-staff
// thread.
const ChiefOfStaff = "chief_of_staff"

// EnsureChiefOfStaff creates the workstream's chief-of-staff thread unless it
// already exists, and returns the thread. The first creation fixes the identity's
// timestamp and actor; later calls leave it unchanged.
func (r *Repository) EnsureChiefOfStaff(ctx context.Context, stream config.WorkstreamID, at time.Time, actor Actor) (Thread, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.ensureChiefOfStaff(ctx, stream, at, actor)
}

func (r *Repository) ensureChiefOfStaff(ctx context.Context, stream config.WorkstreamID, at time.Time, actor Actor) (Thread, error) {
	log, _, err := r.loadWorkflow(stream)
	if err != nil {
		return Thread{}, err
	}
	if t, ok := log.Threads[ChiefOfStaff]; ok {
		if t.Identity.Role != ChiefOfStaff || t.Identity.ThreadID != ChiefOfStaff {
			return Thread{}, fmt.Errorf("%w: agent %s has another role", ErrConflict, ChiefOfStaff)
		}
		return r.snapshot(t), nil
	}
	agent := Agent{Header: Header{Schema: "osmia.trace.agent", Version: Version, ID: ChiefOfStaff, Revision: 1, Project: r.project, Workstream: stream, At: at, Actor: actor, Cause: "chief-of-staff-create"}, Role: ChiefOfStaff, ThreadID: ChiefOfStaff}
	if err := validate(agent); err != nil {
		return Thread{}, err
	}
	if log.Threads == nil {
		log.Threads = map[string]Thread{}
	}
	t := Thread{Identity: agent, Status: "idle"}
	log.Threads[ChiefOfStaff] = t
	if err := r.saveThread(ctx, stream, log, agent); err != nil {
		return Thread{}, err
	}
	return t, nil
}

// ChiefOfStaffThread returns a snapshot of the workstream's chief-of-staff
// thread, or os.ErrNotExist before it is created.
func (r *Repository) ChiefOfStaffThread(stream config.WorkstreamID) (Thread, error) {
	return r.Thread(stream, ChiefOfStaff)
}

// Thread returns a detached snapshot. A claim from a previous service session
// without a captured result is exposed as interrupted, never eligible for retry.
func (r *Repository) Thread(stream config.WorkstreamID, agent string) (Thread, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	log, _, err := r.loadWorkflow(stream)
	if err != nil {
		return Thread{}, err
	}
	t, ok := log.Threads[agent]
	if !ok {
		return Thread{}, os.ErrNotExist
	}
	return r.snapshot(t), nil
}

// Threads returns detached snapshots of every thread in the workstream, ordered
// by agent ID, with the same interruption view as Thread.
func (r *Repository) Threads(stream config.WorkstreamID) ([]Thread, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	log, _, err := r.loadWorkflow(stream)
	if err != nil {
		return nil, err
	}
	threads := make([]Thread, 0, len(log.Threads))
	for _, id := range slices.Sorted(maps.Keys(log.Threads)) {
		threads = append(threads, r.snapshot(log.Threads[id]))
	}
	return threads, nil
}

func (r *Repository) snapshot(t Thread) Thread {
	for _, q := range t.Turns {
		if q.Request.TurnID == t.Active && q.Response == nil && q.Claim.ServiceSession != r.session {
			t.Status = "interrupted"
		}
	}
	return t
}

// EnqueueTurn fixes the request and its profile at acceptance. Sequence numbers
// reflect serialized durable acceptance, not timestamps or goroutine launch order.
func (r *Repository) EnqueueTurn(ctx context.Context, req TurnRequest) (QueuedTurn, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	log, _, err := r.loadWorkflow(req.Workstream)
	if err != nil {
		return QueuedTurn{}, err
	}
	t, ok := log.Threads[req.AgentID]
	if !ok {
		return QueuedTurn{}, os.ErrNotExist
	}
	for _, q := range t.Turns {
		if q.Request.TurnID == req.TurnID {
			if equalJSON(q.Request, req) {
				return q, nil
			}
			return QueuedTurn{}, ErrConflict
		}
	}
	q := QueuedTurn{Sequence: uint64(len(t.Turns) + 1), Request: req}
	t.Turns = append(t.Turns, q)
	log.Threads[req.AgentID] = t
	if err := r.saveThread(ctx, req.Workstream, log, req); err != nil {
		return QueuedTurn{}, err
	}
	return q, nil
}

// ClaimTurn reserves the oldest pending request without an expiring lease. A
// repeated token returns its original claim; it cannot reserve a successor.
func (r *Repository) ClaimTurn(ctx context.Context, stream config.WorkstreamID, agent, token, directory string, at time.Time) (QueuedTurn, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	log, _, err := r.loadWorkflow(stream)
	if err != nil {
		return QueuedTurn{}, err
	}
	t, ok := log.Threads[agent]
	if !ok {
		return QueuedTurn{}, os.ErrNotExist
	}
	for _, q := range t.Turns {
		if q.Claim != nil && q.Claim.Token == token {
			if q.Claim.SessionDirectory != directory || !q.Claim.At.Equal(at) {
				return QueuedTurn{}, ErrConflict
			}
			if q.Claim.ServiceSession != r.session || !q.CompletedAt.IsZero() {
				return q, ErrClaim
			}
			return q, nil
		}
	}
	if t.Active != "" {
		return QueuedTurn{}, ErrClaimed
	}
	for i, q := range t.Turns {
		if q.Claim != nil {
			continue
		}
		q.Claim = &TurnClaim{Token: token, ServiceSession: r.session, SessionDirectory: directory, At: at}
		t.Turns[i], t.Active, t.Status = q, q.Request.TurnID, "running"
		log.Threads[agent] = t
		if err := r.saveThread(ctx, stream, log); err != nil {
			return q, err
		}
		return q, nil
	}
	return QueuedTurn{}, ErrNoTurn
}

// CaptureTurn persists the adapter's final/partial result before completion. The
// exact response is retryable even after reopening; changed evidence is rejected.
func (r *Repository) CaptureTurn(ctx context.Context, token string, response TurnResponse) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	log, _, err := r.loadWorkflow(response.Workstream)
	if err != nil {
		return err
	}
	t, ok := log.Threads[response.AgentID]
	if !ok {
		return os.ErrNotExist
	}
	for i, q := range t.Turns {
		if q.Request.TurnID != response.TurnID {
			continue
		}
		if q.Claim == nil || q.Claim.Token != token {
			return ErrClaim
		}
		if q.Response != nil {
			if equalJSON(*q.Response, response) {
				return nil
			}
			return ErrConflict
		}
		if q.Claim.ServiceSession != r.session || t.Active != response.TurnID {
			return ErrClaim
		}
		q.Response = &response
		t.Turns[i], t.Status = q, "captured"
		if response.Result.Session != (coreadapter.BackendSession{}) {
			t.Session = response.Result.Session
		}
		log.Threads[response.AgentID] = t
		return r.saveThread(ctx, response.Workstream, log, response)
	}
	return ErrClaim
}

// AbandonTurn completes a turn that a previous service session reserved and
// never captured a result for. The exclusive repository lock proves that
// session is gone, and the thread becomes eligible for its next request. A
// turn whose final attempt has no result, or that has no attempt, is recorded
// as interrupted with the claim's start time and session directory, and that
// result also completes the final attempt. A turn whose final attempt recorded
// a result is completed with that result and failure, and its status follows
// them. A turn reserved by this session, one with a captured result, and one
// that is not reserved are refused with ErrClaim.
func (r *Repository) AbandonTurn(ctx context.Context, stream config.WorkstreamID, agent, turn string, at time.Time) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	if at.IsZero() {
		return fmt.Errorf("timestamp required")
	}
	log, _, err := r.loadWorkflow(stream)
	if err != nil {
		return err
	}
	t, ok := log.Threads[agent]
	if !ok {
		return os.ErrNotExist
	}
	for i, q := range t.Turns {
		if q.Request.TurnID != turn {
			continue
		}
		if q.Claim == nil || q.Response != nil || q.Claim.ServiceSession == r.session || t.Active != turn {
			return ErrClaim
		}
		if at.Before(q.Claim.At) {
			at = q.Claim.At
		}
		req := q.Request
		h := req.Header
		h.Schema, h.ID, h.At, h.Actor = "osmia.trace.turn-response", EventID(req.ID, "response"), at, Actor{Kind: "service", ID: "thread-recovery"}
		response := TurnResponse{Header: h, AgentID: agent, ThreadID: req.ThreadID, TurnID: turn, RequestID: req.ID, RequestRevision: req.Revision,
			Result:  coreadapter.SessionResult{SessionDirectory: q.Claim.SessionDirectory, StartedAt: q.Claim.At, Cancelled: true, IsError: true, ErrorSubtype: "interrupted"},
			Failure: "the service stopped before the turn captured a result"}
		settleAttempt(&q, &response)
		q.Response, q.CompletedAt = &response, at
		t.Turns[i], t.Active, t.Status = q, "", q.Status()
		if response.Result.Session != (coreadapter.BackendSession{}) {
			t.Session = response.Result.Session
		}
		log.Threads[agent] = t
		return r.saveThread(ctx, stream, log, response)
	}
	return ErrClaim
}

// settleAttempt makes a recovered turn's response and its final attempt one
// value. A final attempt with a recorded result supplies the response's result
// and failure; one without receives the response's.
func settleAttempt(q *QueuedTurn, response *TurnResponse) {
	n := len(q.Attempts)
	if n == 0 {
		return
	}
	q.Attempts = slices.Clone(q.Attempts)
	last := &q.Attempts[n-1]
	if last.Result != nil {
		response.Result, response.Failure = *last.Result, last.Failure
		return
	}
	result := response.Result
	last.Result, last.Failure = &result, response.Failure
}

// CancelTurns completes every unfinished turn of the workstream that no runner
// of this repository session holds: queued turns and turns a previous session
// reserved without a captured result. Each is recorded as cancelled with the
// given actor and reason, and a reserved turn's final attempt without a result
// receives that result, except a reserved turn whose final attempt recorded a
// result: that turn keeps the attempt's result and failure. A turn this
// session reserved, or one with a captured result, is left to its runner, and
// later turns of its thread wait for another call. It returns the number of
// turns it completed.
func (r *Repository) CancelTurns(ctx context.Context, stream config.WorkstreamID, at time.Time, actor Actor, reason string) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	if at.IsZero() || !present(reason) {
		return 0, fmt.Errorf("timestamp and reason required")
	}
	log, _, err := r.loadWorkflow(stream)
	if err != nil {
		return 0, err
	}
	var responses []Record
	for _, agent := range slices.Sorted(maps.Keys(log.Threads)) {
		t := log.Threads[agent]
		changed := false
	turns:
		for i, q := range t.Turns {
			switch {
			case !q.CompletedAt.IsZero():
				continue
			case q.Response != nil, q.Claim != nil && q.Claim.ServiceSession == r.session:
				break turns
			}
			req := q.Request
			done := at
			if done.Before(req.At) {
				done = req.At
			}
			if q.Claim == nil {
				q.Claim = &TurnClaim{Token: EventID(req.ID, "cancel"), ServiceSession: r.session, SessionDirectory: "none", At: done}
			} else if done.Before(q.Claim.At) {
				done = q.Claim.At
			}
			h := req.Header
			h.Schema, h.ID, h.At, h.Actor = "osmia.trace.turn-response", EventID(req.ID, "response"), done, actor
			response := TurnResponse{Header: h, AgentID: agent, ThreadID: req.ThreadID, TurnID: req.TurnID, RequestID: req.ID, RequestRevision: req.Revision,
				Result:  coreadapter.SessionResult{SessionDirectory: q.Claim.SessionDirectory, StartedAt: q.Claim.At, Cancelled: true, IsError: true, ErrorSubtype: "cancelled"},
				Failure: reason}
			settleAttempt(&q, &response)
			q.Response, q.CompletedAt = &response, done
			t.Turns[i], t.Active, t.Status = q, "", q.Status()
			if response.Result.Session != (coreadapter.BackendSession{}) {
				t.Session = response.Result.Session
			}
			responses = append(responses, response)
			changed = true
		}
		if changed {
			log.Threads[agent] = t
		}
	}
	if len(responses) == 0 {
		return 0, nil
	}
	return len(responses), r.saveThread(ctx, stream, log, responses...)
}

// CompleteTurn releases the reservation only after result capture. It can finish
// a captured turn after restart without running the backend again.
func (r *Repository) CompleteTurn(ctx context.Context, stream config.WorkstreamID, agent, turn, token string, at time.Time) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	log, _, err := r.loadWorkflow(stream)
	if err != nil {
		return err
	}
	t, ok := log.Threads[agent]
	if !ok {
		return os.ErrNotExist
	}
	for i, q := range t.Turns {
		if q.Request.TurnID != turn {
			continue
		}
		if q.Claim == nil || q.Claim.Token != token || q.Response == nil {
			return ErrClaim
		}
		if !q.CompletedAt.IsZero() {
			if q.CompletedAt.Equal(at) {
				return nil
			}
			return ErrConflict
		}
		q.CompletedAt = at
		t.Turns[i], t.Active, t.Status = q, "", q.Status()
		log.Threads[agent] = t
		return r.saveThread(ctx, stream, log)
	}
	return ErrClaim
}

func (r *Repository) saveThread(ctx context.Context, stream config.WorkstreamID, log workflowLog, additions ...Record) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	records, _, err := r.scan()
	if err != nil {
		return err
	}
	records = append(records, additions...)
	if err := validateThreads(log.Threads, records, r.project, stream); err != nil {
		return err
	}
	data, err := json.MarshalIndent(log, "", "  ")
	if err != nil {
		return err
	}
	files := map[string][]byte{"workstreams/" + string(stream) + "/workflow.json": append(data, '\n')}
	if err := r.appendRecords(files, additions); err != nil {
		return err
	}
	if err := r.publish(ctx, files); err != nil {
		return err
	}
	_ = r.wake.Notify(context.Background())
	return nil
}

func responseProfile(q QueuedTurn) coreadapter.Profile {
	if len(q.Attempts) > 0 {
		return q.Attempts[len(q.Attempts)-1].Profile
	}
	return q.Request.Profile
}
