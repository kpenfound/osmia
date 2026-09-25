package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/seal"
	"github.com/kpenfound/osmia/internal/shed"
	"github.com/kpenfound/osmia/internal/trace"
)

// inbox lists the active project's open owner decisions, oldest first, or
// none when no project or trace is active. Decisions of abandoned workstreams
// are left out.
func (s *Service) inbox(ctx context.Context) (InboxResponse, *APIError) {
	s.mu.Lock()
	active, cfg := s.active, s.cfg
	s.mu.Unlock()
	out := InboxResponse{Entries: []InboxEntry{}}
	if !cfg.HasProject() || active == nil {
		return out, nil
	}
	failed := &APIError{Internal, fmt.Sprintf("cannot read the inbox of project %s; check the trace repository", cfg.Project.ID)}
	repository := active.repository
	statuses, err := repository.Statuses()
	if err != nil {
		return InboxResponse{}, failed
	}
	escalations, err := repository.Inbox()
	if err != nil {
		return InboxResponse{}, failed
	}
	gone := map[config.WorkstreamID]bool{}
	for _, w := range statuses {
		gone[w.Workstream] = w.State == AbandonedState
	}
	for _, e := range escalations {
		if e.State == trace.QuestionEscalated && !gone[e.Workstream] {
			out.Entries = append(out.Entries, inboxView(e))
		}
	}
	for _, w := range statuses {
		if gone[w.Workstream] || w.Workstream == librarianWorkstream(cfg.Project.ID) {
			continue
		}
		entries, err := s.openDecisions(ctx, repository, w)
		if err != nil {
			return InboxResponse{}, failed
		}
		out.Entries = append(out.Entries, entries...)
	}
	slices.SortStableFunc(out.Entries, func(a, b InboxEntry) int { return a.OpenedAt.Compare(b.OpenedAt) })
	return out, nil
}

func inboxView(e trace.InboxEntry) InboxEntry {
	out := InboxEntry{Kind: InboxEscalation, Number: e.Number, Workstream: e.Workstream, Batch: e.Batch, Question: e.Rephrasing, Blocked: e.Blocked,
		Options: append([]string{}, e.Options...), Recommendation: e.Recommendation, OpenedAt: e.EscalatedAt, Asked: []InboxQuestion{},
		Answer: InboxAnswer{Method: http.MethodPost, Path: fmt.Sprintf("%s/inbox/%d", Prefix, e.Number), Body: map[string]any{}}}
	if e.State == trace.QuestionEscalated && len(e.Questions) > 0 && eligibleQuickReply(e.Recommendation) {
		out.QuickReply = e.Recommendation
	}
	for _, q := range e.Questions {
		out.Asked = append(out.Asked, InboxQuestion{ID: q.Asked.ID, AskedBy: q.Asked.AskedBy.ID, Unit: q.Asked.Unit, Question: q.Asked.Question})
	}
	return out
}

// decision starts an inbox entry of a kind other than an escalation.
func decision(kind string, stream config.WorkstreamID, opened time.Time, path string, body map[string]any) InboxEntry {
	return InboxEntry{Kind: kind, Workstream: stream, OpenedAt: opened, Options: []string{}, Asked: []InboxQuestion{},
		Answer: InboxAnswer{Method: http.MethodPost, Path: Prefix + path, Body: body}}
}

// openDecisions lists a workstream's open decisions other than its
// escalations: its ratification packet, contested units, presented
// amendments and delivery approval.
func (s *Service) openDecisions(ctx context.Context, repository *trace.Repository, w trace.WorkstreamStatus) ([]InboxEntry, error) {
	var out []InboxEntry
	add := func(e InboxEntry, open bool, err error) error {
		if open {
			out = append(out, e)
		}
		return err
	}
	if w.State == InShedState || w.State == SketchedState {
		if err := add(ratificationEntry(repository, w.Workstream)); err != nil {
			return nil, err
		}
	}
	for _, gate := range w.Gates {
		if gate.Kind != UnitContested {
			continue
		}
		if err := add(s.contestedEntry(repository, w.Workstream, gate.Reference)); err != nil {
			return nil, err
		}
	}
	if w.State == BuildingState || w.State == AssembledState {
		var ids []int
		for subject, state := range w.Subjects {
			id, ok := strings.CutPrefix(subject, amendmentSubject(""))
			if n, err := strconv.Atoi(id); ok && err == nil && state.Value == amendmentPresented {
				ids = append(ids, n)
			}
		}
		slices.Sort(ids)
		for _, id := range ids {
			if err := add(s.amendmentEntry(repository, w.Workstream, strconv.Itoa(id))); err != nil {
				return nil, err
			}
		}
	}
	if w.State == AssembledState {
		if err := add(s.deliveryEntry(ctx, repository, w.Workstream)); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// ratificationEntry is the workstream's ratification packet while debate
// rests at the decision it presents and the owner has not ratified it. It
// offers ratify only while no objection blocks it.
func ratificationEntry(repository *trace.Repository, stream config.WorkstreamID) (InboxEntry, bool, error) {
	packet, doc, found, err := shed.LatestPacket(repository, stream)
	if err != nil || !found {
		return InboxEntry{}, false, err
	}
	state, err := repository.Workflow(stream, shedSubject)
	if err != nil {
		return InboxEntry{}, false, err
	}
	if kind, round, ok := shedState(state.Value); !packet.Skipped && (!ok || kind != "concluded" || round != packet.Round) {
		return InboxEntry{}, false, nil
	}
	ratified, recorded, err := latestRatification(repository, stream)
	if err != nil {
		return InboxEntry{}, false, err
	}
	if recorded && ratified.Round == packet.Round && ratified.Revision == packet.Revision {
		return InboxEntry{}, false, nil
	}
	entries, err := Dissent(repository, stream)
	if err != nil {
		return InboxEntry{}, false, err
	}
	e := decision(InboxRatification, stream, doc.At, "/ratify/"+string(stream), map[string]any{"spec": packet.Revision.Spec, "plan": packet.Revision.Plan})
	e.Revision = doc.Revision
	e.Question = fmt.Sprintf("Ratify %s? Debate ended after round %d: %s", packet.Revision, packet.Round, packet.Conclusion)
	if packet.Skipped {
		e.Question = fmt.Sprintf("Ratify %s? Debate was skipped: %s", packet.Revision, packet.Conclusion)
	}
	e.Blocked = "Sealing the spec and plan, and building the workstream."
	if len(shed.Blocked(entries)) == 0 {
		e.Options = []string{"ratify"}
	}
	e.Recommendation = packet.Recommendation
	return e, true, nil
}

// contestedEntry is a contested unit that has no ruling yet, with the rulings
// its contest takes.
func (s *Service) contestedEntry(repository *trace.Repository, stream config.WorkstreamID, unit string) (InboxEntry, bool, error) {
	state, err := repository.Workflow(stream, trace.UnitSubject(unit))
	if err != nil || state.Value != UnitContested {
		return InboxEntry{}, false, err
	}
	contest, mason, err := masonContest(repository, stream, unit)
	if err != nil {
		return InboxEntry{}, false, err
	}
	options := []string{"review", "revise"}
	switch {
	case failedReview(contest, unit):
		options = []string{"review"}
	case mason:
		options = []string{"revise"}
	default:
		r := &reviewers{masons: &masons{s: s, cfg: s.current(), repository: repository}}
		result, ok, err := r.storedResult(stream, unit, state)
		if err != nil {
			return InboxEntry{}, false, err
		}
		if ok {
			if _, ruled, err := latestContestedRuling(repository, stream, unit, result.Bounces); err != nil || ruled {
				return InboxEntry{}, false, err
			}
		}
	}
	e := decision(InboxContested, stream, contest.At, "/contested/"+string(stream)+"/"+unit, map[string]any{})
	e.Unit = unit
	e.Question = fmt.Sprintf("Unit %s is contested: %s", unit, contest.Reason)
	e.Blocked = fmt.Sprintf("Unit %s.", unit)
	e.Options = options
	return e, true, nil
}

// amendmentEntry is a presented amendment at its latest packet revision, with
// the decisions that revision takes.
func (s *Service) amendmentEntry(repository *trace.Repository, stream config.WorkstreamID, id string) (InboxEntry, bool, error) {
	c, found, err := readAmendment(repository, stream, id)
	if err != nil || !found || c.state.Value != amendmentPresented || c.packet.Revision == 0 {
		return InboxEntry{}, false, err
	}
	var packet struct {
		Recommendation string `json:"recommendation"`
	}
	if err := json.Unmarshal([]byte(c.packet.Content), &packet); err != nil {
		return InboxEntry{}, false, fmt.Errorf("%s revision %d: %w", c.packet.Path, c.packet.Revision, err)
	}
	records, err := amendmentRecords(repository, stream, id)
	if err != nil {
		return InboxEntry{}, false, err
	}
	current, _, sealed, err := seal.Latest(repository, stream)
	if err != nil {
		return InboxEntry{}, false, err
	}
	matching := false
	if sealed {
		if matching, err = amendmentSealMatches(repository, stream, c.request.SealRevision, current); err != nil {
			return InboxEntry{}, false, err
		}
	}
	entries := amendmentDissent(records)
	var options []string
	if matching && len(shed.Blocked(entries)) == 0 {
		options = append(options, AmendmentApprove)
	}
	if matching && len(entries) > 0 {
		options = append(options, AmendmentOverrule)
	}
	options = append(options, AmendmentReject)
	if c.round < s.current().Shed.MaxRounds {
		options = append(options, AmendmentRound)
	}
	e := decision(InboxAmendment, stream, c.packet.At, "/amendment/"+string(stream)+"/"+id, map[string]any{"packet": c.packet.Revision})
	e.Amendment, e.Unit, e.Revision = id, c.request.Unit, c.packet.Revision
	e.Question = fmt.Sprintf("Amend the sealed spec and plan after debate round %d? Change: %s Reason: %s", c.round, c.request.Change, c.request.Reason)
	e.Blocked = "The sealed spec and plan stay in force until the amendment is decided."
	if c.request.Unit != "" {
		e.Blocked = fmt.Sprintf("Unit %s waits for the decision.", c.request.Unit)
	}
	e.Options = options
	e.Recommendation = packet.Recommendation
	return e, true, nil
}

// deliveryEntry is an assembled workstream's final report while it can be
// approved and no approval of it stands: none is recorded, it is stale, or
// its publication was refused.
func (s *Service) deliveryEntry(ctx context.Context, repository *trace.Repository, stream config.WorkstreamID) (InboxEntry, bool, error) {
	p, api := s.presentDelivery(ctx, repository.Project(), stream, repository)
	switch {
	case api != nil && api.Code == Conflict:
		return InboxEntry{}, false, nil
	case api != nil:
		return InboxEntry{}, false, errors.New(api.Message)
	case p.Delivered:
		return InboxEntry{}, false, nil
	}
	for _, c := range p.Report.Criteria {
		if c.Gap != "" {
			return InboxEntry{}, false, nil
		}
	}
	review, prior, _, err := deliveryDocuments(repository, stream)
	if err != nil {
		return InboxEntry{}, false, err
	}
	if p.Approval != nil {
		if refused, err := publicationRefused(repository, stream, prior.Revision); err != nil || !refused {
			return InboxEntry{}, false, err
		}
	}
	e := decision(InboxDelivery, stream, review.At, "/delivery/"+string(stream), map[string]any{"review": p.Report.Review, "review_revision": p.ReviewRevision, "commit": p.Report.Commit, "draft_hash": p.DraftHash})
	e.Revision = p.ReviewRevision
	title := strings.TrimSpace(p.Report.Summary)
	if title == "" {
		title = "workstream " + string(stream)
	}
	e.Question = fmt.Sprintf("Deliver %s? Final review %d of commit %s shows evidence for every criterion.", title, p.Report.Review, p.Report.Commit)
	e.Blocked = "Publishing the pull request."
	e.Options = []string{"approve"}
	return e, true, nil
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
