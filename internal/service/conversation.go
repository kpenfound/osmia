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

// conversationTrace resolves raw to a workstream of an active project and
// returns the project's open trace. The librarian's workstream is unknown
// here as it is in status: its chief of staff never gets a turn.
func (s *Service) conversationTrace(raw string) (config.ProjectID, config.WorkstreamID, *trace.Repository, *APIError) {
	id, err := config.ParseWorkstreamID(raw)
	if err != nil {
		return "", "", nil, &APIError{Validation, "workstream must be a workstream ID: w_ followed by 32 lowercase hexadecimal digits"}
	}
	if _, ok := s.archivedWorkstream(raw); ok {
		return "", "", nil, &APIError{Conflict, "workstream is permanently archived; its history is unavailable"}
	}
	cfg, projects := s.runtimes()
	if len(cfg.Projects) == 0 {
		return "", "", nil, &APIError{NoProject, "no project is configured; add one with osmia project add"}
	}
	for _, active := range projects {
		if !cfg.Active(active.id) || id == librarianWorkstream(active.id) {
			continue
		}
		streams, err := active.repository.Workstreams()
		if err != nil {
			return "", "", nil, &APIError{Internal, fmt.Sprintf("cannot read the workstreams of project %s; check the trace repository", active.id)}
		}
		if slices.Contains(streams, id) {
			return active.id, id, active.repository, nil
		}
	}
	return "", "", nil, &APIError{Validation, fmt.Sprintf("workstream %s is not in an active project; list workstreams with osmia status", id)}
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
		SystemPrompt: fmt.Sprintf(chiefPrompt, stream) + "\n\n" + chiefDocumentsGuidance + "\n\n" + priorityGuidance + "\n\n" + pauseGuidance + "\n\n" + amendmentGuidance + "\n\n" + charterGuidance + "\n\n" + contestGuidance + "\n\n" + driftHandbackGuidance + "\n\n" + context,
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
	entries, err := consistentRead(repository, func() ([]ConversationEntry, error) { return conversationEntries(repository, stream) })
	if err != nil {
		return ConversationResponse{}, &APIError{Internal, fmt.Sprintf("cannot read the conversation of workstream %s; check the trace repository", stream)}
	}
	return ConversationResponse{Workstream: stream, Entries: entries}, nil
}

// conversationEntries returns the workstream's conversation with its chief of
// staff, oldest first: the owner's messages, the final responses to them and
// the actions the chief of staff took on the owner's behalf.
func conversationEntries(repository *trace.Repository, stream config.WorkstreamID) ([]ConversationEntry, error) {
	out := []ConversationEntry{}
	t, err := repository.ChiefOfStaffThread(stream)
	if errors.Is(err, os.ErrNotExist) {
		return out, nil
	}
	if err != nil {
		return nil, err
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
		out = append(out, entries...)
	}
	actions, err := chiefActions(repository, stream)
	if err != nil {
		return nil, err
	}
	return withActions(out, actions), nil
}

// withActions places each action before the first entry recorded after it,
// in the order they were recorded, so a message stays beside its response.
func withActions(entries, actions []ConversationEntry) []ConversationEntry {
	actions = slices.Clone(actions)
	sort.SliceStable(actions, func(i, j int) bool { return actions[i].At.Before(actions[j].At) })
	merged := make([]ConversationEntry, 0, len(entries)+len(actions))
	for _, e := range entries {
		for len(actions) > 0 && actions[0].At.Before(e.At) {
			merged, actions = append(merged, actions[0]), actions[1:]
		}
		merged = append(merged, e)
	}
	return append(merged, actions...)
}

// chiefActions returns what the chief of staff did on the owner's behalf in
// the workstream, as action entries: its rulings on contested units, the
// contests it raised to the owner and the units it moved.
func chiefActions(repository *trace.Repository, stream config.WorkstreamID) ([]ConversationEntry, error) {
	docs, err := trace.Read[trace.Document](repository, stream)
	if err != nil {
		return nil, err
	}
	var out []ConversationEntry
	for _, d := range docs {
		if d.Unit == "" || !strings.HasPrefix(d.Path, "units/"+trace.UnitSubject(d.Unit)+"/chief-") {
			continue
		}
		var decision ChiefContestDecision
		if err := json.Unmarshal([]byte(d.Content), &decision); err != nil {
			return nil, fmt.Errorf("%s: %w", d.Path, err)
		}
		text := fmt.Sprintf("Raised contested unit %s to you: %s", d.Unit, decision.Note)
		switch decision.Decision {
		case "review":
			text = fmt.Sprintf("Resolved contested unit %s: its reviewer reviews the candidate again. %s", d.Unit, decision.Note)
		case "revise":
			text = fmt.Sprintf("Resolved contested unit %s: its mason revises the unit. %s", d.Unit, decision.Note)
		}
		out = append(out, ConversationEntry{Turn: decision.Turn, Kind: "action", Text: text, At: d.At, State: TurnDone})
	}
	for _, d := range docs {
		if d.Unit == "" || !strings.HasPrefix(d.Path, movePrefix(d.Unit)) {
			continue
		}
		var move UnitMove
		if err := json.Unmarshal([]byte(d.Content), &move); err != nil {
			return nil, fmt.Errorf("%s: %w", d.Path, err)
		}
		if move.By != rulerName(chiefActor) {
			continue
		}
		text := fmt.Sprintf("Moved unit %s from %s to %s. %s", d.Unit, move.From, move.To, move.Note)
		if move.To == UnitContested {
			text = fmt.Sprintf("Held unit %s for you, from %s: %s", d.Unit, move.From, move.Note)
		}
		out = append(out, ConversationEntry{Turn: move.Turn, Kind: "action", Text: text, At: d.At, State: TurnDone})
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
