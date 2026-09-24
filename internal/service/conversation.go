package service

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/trace"
)

// ownerActor is the provenance of messages the owner sends.
var ownerActor = trace.Actor{Kind: "owner", ID: "local"}

// chiefPrompt opens the system prompt of every owner message turn; the
// priority guidance and the workstream's rendered bundle follow it.
const chiefPrompt = "You are the chief of staff for workstream %s. The prompt is a message from the owner. The project context below was assembled when the message was accepted."

// now is the service clock: the reconciliation clock when one is supplied.
func (s *Service) now() time.Time {
	if s.options.Reconciliation.Now != nil {
		return s.options.Reconciliation.Now().UTC()
	}
	return time.Now().UTC()
}

// conversationTrace resolves raw to a workstream of the active project and
// returns the project's open trace. The librarian's workstream is unknown
// here as it is in status: its chief of staff never gets a turn.
func (s *Service) conversationTrace(raw string) (config.ProjectID, config.WorkstreamID, *trace.Repository, *APIError) {
	id, err := config.ParseWorkstreamID(raw)
	if err != nil {
		return "", "", nil, &APIError{Validation, "workstream must be a workstream ID: w_ followed by 32 lowercase hexadecimal digits"}
	}
	s.mu.Lock()
	active, cfg := s.active, s.cfg
	s.mu.Unlock()
	if !cfg.HasProject() {
		return "", "", nil, &APIError{NoProject, "no project is configured; add one with osmia project add"}
	}
	unknown := &APIError{Validation, fmt.Sprintf("workstream %s is not in the active project; list workstreams with osmia status", id)}
	if active == nil {
		return "", "", nil, unknown
	}
	streams, err := active.repository.Workstreams()
	if err != nil {
		return "", "", nil, &APIError{Internal, fmt.Sprintf("cannot read the workstreams of project %s; check the trace repository", cfg.Project.ID)}
	}
	if !slices.Contains(streams, id) || id == librarianWorkstream(cfg.Project.ID) {
		return "", "", nil, unknown
	}
	return cfg.Project.ID, id, active.repository, nil
}

// send accepts an owner message as the next turn of the workstream's
// chief-of-staff thread. It returns only after the request is durable.
func (s *Service) send(ctx context.Context, raw string, req SendRequest) (ConversationEntry, *APIError) {
	project, stream, repository, api := s.conversationTrace(raw)
	if api != nil {
		return ConversationEntry{}, api
	}
	if strings.TrimSpace(req.Text) == "" {
		return ConversationEntry{}, &APIError{Validation, "text must not be empty"}
	}
	cfg := s.current()
	name, profile, err := s.chiefOverride(cfg)
	if err != nil {
		return ConversationEntry{}, &APIError{Internal, fmt.Sprintf("role %s has no usable profile %q; check osmia profiles", trace.ChiefOfStaff, name)}
	}
	context, err := chiefContext(ctx, s.Context(), repository, project, stream)
	if err != nil {
		return ConversationEntry{}, &APIError{Internal, fmt.Sprintf("cannot assemble the context of workstream %s; check the charter, the knowledge base and the trace repository", stream)}
	}
	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return ConversationEntry{}, &APIError{Internal, "cannot generate a message ID"}
	}
	id := hex.EncodeToString(nonce[:])
	at := s.now()
	failed := &APIError{Internal, fmt.Sprintf("cannot record the message for workstream %s; check the trace repository", stream)}
	if _, err := repository.EnsureChiefOfStaff(ctx, stream, at, serviceActor); err != nil {
		return ConversationEntry{}, failed
	}
	q, err := repository.EnqueueTurn(ctx, trace.TurnRequest{
		Header:       trace.Header{Schema: "osmia.trace.turn-request", Version: trace.Version, ID: "request_" + id, Revision: 1, Project: project, Workstream: stream, At: at, Actor: ownerActor, Cause: "owner-message"},
		AgentID:      trace.ChiefOfStaff,
		ThreadID:     trace.ChiefOfStaff,
		TurnID:       "message_" + id,
		Profile:      profile,
		SystemPrompt: fmt.Sprintf(chiefPrompt, stream) + "\n\n" + priorityGuidance + "\n\n" + context,
		Prompt:       req.Text,
	})
	if err != nil {
		return ConversationEntry{}, failed
	}
	return conversation(q)[0], nil
}

// conversationList reads the owner's messages to the workstream's chief of
// staff, and its final responses to them, from the thread's turn log.
func (s *Service) conversationList(raw string) (ConversationResponse, *APIError) {
	_, stream, repository, api := s.conversationTrace(raw)
	if api != nil {
		return ConversationResponse{}, api
	}
	out := ConversationResponse{Workstream: stream, Entries: []ConversationEntry{}}
	t, err := repository.ChiefOfStaffThread(stream)
	if errors.Is(err, os.ErrNotExist) {
		return out, nil
	}
	if err != nil {
		return ConversationResponse{}, &APIError{Internal, fmt.Sprintf("cannot read the conversation of workstream %s; check the trace repository", stream)}
	}
	for _, q := range t.Turns {
		if q.Request.Actor != ownerActor {
			continue
		}
		entries := conversation(q)
		// A claim an earlier service session left without a result never
		// completes and is never retried.
		if t.Status == "interrupted" && t.Active == q.Request.TurnID {
			for i := range entries {
				entries[i].State = TurnFailed
			}
		}
		out.Entries = append(out.Entries, entries...)
	}
	return out, nil
}

// conversation returns a turn's message and, once the turn completed with a
// final response, that response.
func conversation(q trace.QueuedTurn) []ConversationEntry {
	state := TurnQueued
	switch {
	case q.Claim == nil:
	case q.CompletedAt.IsZero():
		state = TurnRunning
	case q.Status() == "failed" || q.Status() == "interrupted":
		state = TurnFailed
	default:
		state = TurnDone
	}
	req := q.Request
	out := []ConversationEntry{{Turn: req.TurnID, Kind: "message", Text: req.Prompt, At: req.At, State: state}}
	if !q.CompletedAt.IsZero() && q.Response != nil && q.Response.Result.FinalResponse != "" {
		out = append(out, ConversationEntry{Turn: req.TurnID, Kind: "response", Text: q.Response.Result.FinalResponse, At: q.Response.At, State: state})
	}
	return out
}
