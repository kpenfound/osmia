package service

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"unicode"

	"github.com/kpenfound/osmia/internal/trace"
)

// inbox lists the active project's escalations that wait for the owner's
// ruling, by inbox number, or none when no project or trace is active.
// Escalations of abandoned workstreams are left out.
func (s *Service) inbox() (InboxResponse, *APIError) {
	s.mu.Lock()
	active, cfg := s.active, s.cfg
	s.mu.Unlock()
	out := InboxResponse{Entries: []InboxEntry{}}
	if !cfg.HasProject() || active == nil {
		return out, nil
	}
	failed := &APIError{Internal, fmt.Sprintf("cannot read the inbox of project %s; check the trace repository", cfg.Project.ID)}
	entries, err := active.repository.Inbox()
	if err != nil {
		return InboxResponse{}, failed
	}
	for _, e := range entries {
		if e.State != trace.QuestionEscalated {
			continue
		}
		gone, err := abandoned(active.repository, e.Workstream)
		if err != nil {
			return InboxResponse{}, failed
		}
		if !gone {
			out.Entries = append(out.Entries, inboxView(e))
		}
	}
	return out, nil
}

func inboxView(e trace.InboxEntry) InboxEntry {
	out := InboxEntry{Number: e.Number, Workstream: e.Workstream, Batch: e.Batch, Question: e.Rephrasing, Blocked: e.Blocked,
		Options: append([]string{}, e.Options...), Recommendation: e.Recommendation, EscalatedAt: e.EscalatedAt, Asked: []InboxQuestion{}}
	if e.State == trace.QuestionEscalated && len(e.Questions) > 0 && eligibleQuickReply(e.Recommendation) {
		out.QuickReply = e.Recommendation
	}
	for _, q := range e.Questions {
		out.Asked = append(out.Asked, InboxQuestion{ID: q.Asked.ID, AskedBy: q.Asked.AskedBy.ID, Unit: q.Asked.Unit, Question: q.Asked.Question})
	}
	return out
}

var prohibitedQuickReplyTokens = map[string]bool{
	"push": true, "pushes": true, "pushed": true, "pushing": true,
	"merge": true, "merges": true, "merged": true, "merging": true,
	"deliver": true, "delivers": true, "delivered": true, "delivering": true,
	"abandon": true, "abandons": true, "abandoned": true, "abandoning": true,
	"force": true, "forces": true, "forced": true, "forcing": true,
	"delete": true, "deletes": true, "deleted": true, "deleting": true,
	"rebase": true, "rebases": true, "rebased": true, "rebasing": true,
	"overrule": true, "overrules": true, "overruled": true, "overruling": true,
	"deploy": true, "deploys": true, "deployed": true, "deploying": true,
	"ratify": true, "ratifies": true, "ratified": true, "ratifying": true,
	"veto": true, "vetoes": true, "vetoed": true, "vetoing": true,
	"revert": true, "reverts": true, "reverted": true, "reverting": true,
	"reset": true, "resets": true, "resetting": true,
	"discard": true, "discards": true, "discarded": true, "discarding": true,
}

func eligibleQuickReply(recommendation string) bool {
	if strings.TrimSpace(recommendation) == "" {
		return false
	}
	for _, token := range strings.FieldsFunc(strings.ToLower(recommendation), func(r rune) bool { return !unicode.IsLetter(r) }) {
		if prohibitedQuickReplyTokens[token] {
			return false
		}
	}
	return true
}

// answer records the owner's ruling on the inbox entry raw numbers. It returns
// once the ruling and the chief of staff's notice of it are durable; relaying
// the ruling to the askers is the chief of staff's next turn.
func (s *Service) answer(ctx context.Context, raw string, req AnswerRequest) (AnswerResponse, *APIError) {
	number, err := strconv.Atoi(raw)
	if err != nil || number < 1 || strconv.Itoa(number) != raw {
		return AnswerResponse{}, &APIError{Validation, "inbox entry must be a number from osmia inbox"}
	}
	s.mu.Lock()
	active, cfg := s.active, s.cfg
	s.mu.Unlock()
	if !cfg.HasProject() {
		return AnswerResponse{}, &APIError{NoProject, "no project is configured; add one with osmia project add"}
	}
	unknown := &APIError{Validation, fmt.Sprintf("there is no inbox entry %d; list the entries with osmia inbox", number)}
	if active == nil {
		return AnswerResponse{}, unknown
	}
	if strings.TrimSpace(req.Text) == "" {
		return AnswerResponse{}, &APIError{Validation, "text must not be empty"}
	}
	failed := &APIError{Internal, fmt.Sprintf("cannot record the ruling on inbox entry %d; check the trace repository", number)}
	at := s.now()
	entry, err := active.repository.Rule(ctx, number, req.Text, ownerActor, at, AbandonedState)
	switch {
	case errors.Is(err, trace.ErrInboxEntry):
		return AnswerResponse{}, unknown
	case errors.Is(err, trace.ErrFeatureState):
		return AnswerResponse{}, &APIError{Conflict, fmt.Sprintf("inbox entry %d belongs to abandoned workstream %s and takes no ruling", number, entry.Workstream)}
	case errors.Is(err, trace.ErrRuled):
		return AnswerResponse{}, &APIError{Conflict, fmt.Sprintf("inbox entry %d is already answered", number)}
	case err != nil:
		return AnswerResponse{}, failed
	}
	out := AnswerResponse{Number: number, Workstream: entry.Workstream, Batch: entry.Batch, Questions: []string{}, Ruling: req.Text, At: at}
	for _, q := range entry.Questions {
		out.Questions = append(out.Questions, q.Asked.ID)
	}
	return out, nil
}
