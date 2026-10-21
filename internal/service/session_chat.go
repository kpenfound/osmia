package service

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"slices"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/thread"
	"github.com/kpenfound/osmia/internal/trace"
)

// sessionServiceActor is the provenance of every service-written record on
// a session's thread that is not the owner's own message: the working/idle
// state and a context-rebuilt entry.
var sessionServiceActor = trace.Actor{Kind: "service", ID: "session"}

// sessionTrace resolves raw to a session workstream of an active project and
// returns the project's open trace. An unknown workstream, one not in an
// active project or one that is not a session is refused with not_found.
func (s *Service) sessionTrace(raw string) (config.ProjectID, config.WorkstreamID, *trace.Repository, *APIError) {
	id, err := config.ParseWorkstreamID(raw)
	if err != nil {
		return "", "", nil, &APIError{Code: Validation, Message: "workstream must be a workstream ID: w_ followed by 32 lowercase hexadecimal digits"}
	}
	notFound := &APIError{Code: NotFound, Message: fmt.Sprintf("session %s is not in an active project; list sessions with GET %s/sessions", id, Prefix)}
	cfg, projects := s.runtimes()
	for _, active := range projects {
		if !cfg.Active(active.id) {
			continue
		}
		streams, err := active.repository.Workstreams()
		if err != nil {
			return "", "", nil, &APIError{Code: Internal, Message: fmt.Sprintf("cannot read the workstreams of project %s; check the trace repository", active.id)}
		}
		if !slices.Contains(streams, id) {
			continue
		}
		kind, err := active.repository.Kind(id)
		if err != nil {
			return "", "", nil, &APIError{Code: Internal, Message: fmt.Sprintf("cannot read the kind of workstream %s; check the trace repository", id)}
		}
		if kind != trace.SessionKind {
			return "", "", nil, notFound
		}
		return active.id, id, active.repository, nil
	}
	return "", "", nil, notFound
}

// sessionLockKey identifies one session's busy lock: a repository and the
// one of its workstreams that is the session, so every session of every
// project has its own lock.
type sessionLockKey struct {
	repository *trace.Repository
	stream     config.WorkstreamID
}

// sessionLocks is the Go guard against two concurrent calls to sessionMessage
// against the same session both finding it idle and both recording an owner
// message: at most one turn ever runs per session, and there is no
// message queue, so a message that cannot take its session's lock
// immediately is refused as busy rather than waiting for the turn in
// flight to finish.
type sessionGate struct {
	sync.Mutex
	controls atomic.Int64
}

var sessionLocks sync.Map // sessionLockKey -> *sessionGate

func sessionLock(repository *trace.Repository, stream config.WorkstreamID) *sessionGate {
	v, _ := sessionLocks.LoadOrStore(sessionLockKey{repository, stream}, &sessionGate{})
	return v.(*sessionGate)
}

// lastSessionRequestAt returns the request time of the thread's most recent
// turn, or the zero time when it has none yet.
func lastSessionRequestAt(th trace.Thread) time.Time {
	if n := len(th.Turns); n > 0 {
		return th.Turns[n-1].Request.At
	}
	return time.Time{}
}

// ensureSessionThread creates the session's one agent thread unless it
// already exists, and returns it. The first creation fixes the identity's
// timestamp; later calls leave it unchanged and never retry creation.
func ensureSessionThread(ctx context.Context, repository *trace.Repository, project config.ProjectID, stream config.WorkstreamID, at time.Time) (trace.Thread, error) {
	if th, err := repository.Thread(stream, sessionAgentID); err == nil {
		return th, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return trace.Thread{}, err
	}
	agent := trace.Agent{
		Header: trace.Header{Schema: "osmia.trace.agent", Version: trace.Version, ID: sessionAgentID, Revision: 1,
			Project: project, Workstream: stream, At: at, Actor: sessionServiceActor, Cause: "session-thread-create"},
		Role:     sessionRole,
		ThreadID: sessionAgentID,
	}
	if err := repository.CreateThread(ctx, agent); err != nil && !errors.Is(err, trace.ErrConflict) {
		return trace.Thread{}, err
	}
	return repository.Thread(stream, sessionAgentID)
}

// SessionEntry is one service entry appended to a session's thread: a
// commit made, a check result, or a notice that the agent's context was
// rebuilt from history, with when it was recorded.
type SessionEntry struct {
	ID   string    `json:"id"`
	Text string    `json:"text"`
	At   time.Time `json:"at"`
}

// sessionEntryPrefix is the per-workstream document path prefix every
// service entry is recorded under.
const sessionEntryPrefix = "session-entries/"

// recordSessionEntry appends a service entry to the session's thread, such
// as a commit made, a check result, or a context-rebuilt notice, so the
// next turn receives it. A retry with the same id records nothing more.
func recordSessionEntry(ctx context.Context, repository *trace.Repository, project config.ProjectID, stream config.WorkstreamID, id string, at time.Time, text string) error {
	data, err := json.Marshal(SessionEntry{ID: id, Text: text, At: at})
	if err != nil {
		return err
	}
	h := trace.Header{Schema: "osmia.trace.document", Version: trace.Version, ID: id, Revision: 1,
		Project: project, Workstream: stream, At: at, Actor: sessionServiceActor, Cause: id}
	doc := trace.Document{Header: h, Path: sessionEntryPrefix + id + ".json", Content: string(data) + "\n"}
	if err := repository.RecordDocuments(ctx, []trace.Document{doc}); err != nil && !errors.Is(err, trace.ErrConflict) {
		return err
	}
	return nil
}

// sessionEntriesSince returns the session's service entries recorded after
// cutoff, oldest first: each reaches exactly one turn, the first one whose
// request comes after it was recorded.
func sessionEntriesSince(repository *trace.Repository, stream config.WorkstreamID, cutoff time.Time) ([]SessionEntry, error) {
	docs, err := trace.Read[trace.Document](repository, stream)
	if err != nil {
		return nil, err
	}
	var out []SessionEntry
	for _, d := range docs {
		if !strings.HasPrefix(d.Path, sessionEntryPrefix) {
			continue
		}
		var e SessionEntry
		if err := json.Unmarshal([]byte(d.Content), &e); err != nil {
			return nil, fmt.Errorf("%s: %w", d.Path, err)
		}
		if e.At.After(cutoff) {
			out = append(out, e)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].At.Before(out[j].At) })
	return out, nil
}

// sessionPrompt is one turn's input: the owner's new message, preceded by
// any service entries recorded since the previous turn, so the agent
// learns what the owner did with its work.
func sessionPrompt(entries []SessionEntry, text string) string {
	if len(entries) == 0 {
		return text
	}
	var b strings.Builder
	b.WriteString("Since your last turn:\n")
	for _, e := range entries {
		fmt.Fprintf(&b, "- %s\n", e.Text)
	}
	b.WriteString("\n")
	b.WriteString(text)
	return b.String()
}

// setSessionState moves the session's SessionSubject workflow value to to,
// recording why, unless it is already there. A retry with the same id is
// idempotent.
func setSessionState(ctx context.Context, repository *trace.Repository, project config.ProjectID, stream config.WorkstreamID, at time.Time, id, cause, to string) error {
	// Each effect belongs to one turn even if a later turn changed the state.
	// A retry after its durable write must not change the later turn's state.
	transitions, err := trace.Read[trace.Transition](repository, stream)
	if err != nil {
		return err
	}
	for _, tr := range transitions {
		if tr.ID == id {
			return nil
		}
	}
	state, err := repository.Workflow(stream, trace.SessionSubject)
	if err != nil {
		return err
	}
	if state.Value == to {
		return nil
	}
	h := trace.Header{Schema: "osmia.trace.transition", Version: trace.Version, ID: id, Revision: 1,
		Project: project, Workstream: stream, At: at, Actor: sessionServiceActor, Cause: cause}
	_, err = repository.Transact(ctx, trace.Transaction{ExpectedVersion: state.Version,
		Transition: trace.Transition{Header: h, Subject: trace.SessionSubject, From: state.Value, To: to, Reason: cause}})
	return err
}

// sessionConversationEntry converts a session's queued turn into a
// ConversationEntry, the same shape the chief-of-staff conversation uses,
// distinguishing a turn the owner stopped from one that failed.
func sessionConversationEntry(q trace.QueuedTurn) ConversationEntry {
	state := TurnQueued
	switch {
	case q.Claim == nil:
	case q.CompletedAt.IsZero():
		state = TurnRunning
	case q.Response != nil && q.Response.Stop != nil:
		state = TurnStopped
	case q.Status() == "failed" || q.Status() == "interrupted":
		state = TurnFailed
	default:
		state = TurnDone
	}
	req := q.Request
	return ConversationEntry{Turn: req.TurnID, Kind: "message", Text: req.Prompt, At: req.At, State: state, QueuedReason: queuedReason(trace.Thread{Turns: []trace.QueuedTurn{q}}, q)}
}

// sessionMessage records text as the next owner message on an idle
// session's thread and starts exactly one agent turn on the session's
// profile. It is refused, recording nothing, when the message is empty,
// the session's profile is no longer one the service can run, or a turn is
// already running.
func (s *Service) sessionMessage(ctx context.Context, raw string, req SessionMessageRequest) (ConversationEntry, *APIError) {
	project, stream, repository, api := s.sessionTrace(raw)
	if api != nil {
		return ConversationEntry{}, api
	}
	if err := s.step("session-message-looked-up"); err != nil {
		return ConversationEntry{}, &APIError{Code: Internal, Message: err.Error()}
	}
	text := strings.TrimSpace(req.Text)
	if text == "" {
		return ConversationEntry{}, &APIError{Code: Validation, Message: "text must not be empty"}
	}
	view, api := s.sessionRead(string(stream))
	if api != nil {
		return ConversationEntry{}, api
	}
	cfg := s.current().For(project)
	profile, _, err := cfg.Execution(masonRole, view.Profile)
	if err != nil {
		return ConversationEntry{}, &APIError{Code: Validation, Message: fmt.Sprintf("profile %q is not one the service can run; open a new session: %v", view.Profile, err)}
	}
	mu := sessionLock(repository, stream)
	if !mu.TryLock() {
		return ConversationEntry{}, &APIError{Code: Conflict, Message: "a turn is running for this session; try again once it finishes"}
	}
	defer func() { mu.Unlock(); s.conversationSignal.wake() }()
	if _, _, _, api := s.sessionTrace(raw); api != nil {
		return ConversationEntry{}, api
	}
	// End and destroy share this admission guard.
	if ended, err := sessionEnded(repository, stream); err != nil {
		return ConversationEntry{}, &APIError{Code: Internal, Message: fmt.Sprintf("cannot read the state of session %s; check the trace repository", stream)}
	} else if ended {
		return ConversationEntry{}, &APIError{Code: Conflict, Message: "the session has ended"}
	}
	at := s.now()
	th, err := ensureSessionThread(ctx, repository, project, stream, at)
	if err != nil {
		return ConversationEntry{}, &APIError{Code: Internal, Message: fmt.Sprintf("cannot record the message for session %s; check the trace repository", stream)}
	}
	if unfinishedSessionTurn(th) || view.State == trace.SessionWorking {
		return ConversationEntry{}, &APIError{Code: Conflict, Message: "a turn is running for this session; try again once it finishes"}
	}
	entries, err := sessionEntriesSince(repository, stream, lastSessionRequestAt(th))
	if err != nil {
		return ConversationEntry{}, &APIError{Code: Internal, Message: fmt.Sprintf("cannot read the service entries of session %s; check the trace repository", stream)}
	}
	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return ConversationEntry{}, &APIError{Code: Internal, Message: "cannot generate a message ID"}
	}
	id := hex.EncodeToString(nonce[:])
	treq := trace.TurnRequest{
		Header: trace.Header{Schema: "osmia.trace.turn-request", Version: trace.Version, ID: "request_" + id, Revision: 1,
			Project: project, Workstream: stream, At: at, Actor: ownerActor, Cause: "owner-message"},
		AgentID: sessionAgentID, ThreadID: sessionAgentID, TurnID: "message_" + id,
		Profile: profile, SystemPrompt: sessionRolePrompt, Prompt: sessionPrompt(entries, text),
	}
	q, err := repository.EnqueueTurn(ctx, treq)
	if err != nil {
		return ConversationEntry{}, &APIError{Code: Internal, Message: fmt.Sprintf("cannot record the message for session %s; check the trace repository", stream)}
	}
	if err := setSessionState(ctx, repository, project, stream, at, "session-working-"+q.Request.TurnID, "session-turn-started", trace.SessionWorking); err != nil {
		return ConversationEntry{}, &APIError{Code: Internal, Message: fmt.Sprintf("cannot record session %s as working; check the trace repository", stream)}
	}
	if err := dispatchSessionTurn(ctx, repository, project, stream, q.Request.TurnID, at); err != nil {
		return ConversationEntry{}, &APIError{Code: Internal, Message: fmt.Sprintf("cannot dispatch the turn for session %s; check the trace repository", stream)}
	}
	return sessionConversationEntry(q), nil
}

// dispatchSessionTurn wakes reconciliation of the durable session queue. The
// conversation path publishes and reconciles the turn's operation.
func dispatchSessionTurn(ctx context.Context, repository *trace.Repository, project config.ProjectID, stream config.WorkstreamID, turn string, at time.Time) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	repository.WakeWorkflow()
	return nil
}

// sessionStop stops a session's running turn: it is recorded as stopped,
// its worktree changes already made are kept, and the session returns to
// idle. Stopping an idle session does nothing.
func (s *Service) sessionStop(ctx context.Context, raw string) (SessionView, *APIError) {
	_, stream, _, api := s.sessionTrace(raw)
	if api != nil {
		return SessionView{}, api
	}
	stop := &thread.Stop{TurnStop: trace.TurnStop{Cause: trace.TurnStopOwner, Scope: string(stream), Source: "owner", Reason: "the owner stopped the turn"}}
	s.turns.stopStream(stream, stop)
	return s.sessionRead(string(stream))
}
