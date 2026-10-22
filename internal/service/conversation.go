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
	"github.com/kpenfound/osmia/internal/readview"
	"github.com/kpenfound/osmia/internal/shed"
	"github.com/kpenfound/osmia/internal/trace"
)

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
		return "", "", nil, &APIError{Code: Validation, Message: "workstream must be a workstream ID: w_ followed by 32 lowercase hexadecimal digits"}
	}
	if _, ok := s.archivedWorkstream(raw); ok {
		return "", "", nil, &APIError{Code: Conflict, Message: "workstream is permanently archived; its history is unavailable"}
	}
	cfg, projects := s.runtimes()
	if len(cfg.Projects) == 0 {
		return "", "", nil, &APIError{Code: NoProject, Message: "no project is configured; add one with osmia project add"}
	}
	for _, active := range projects {
		if !cfg.Active(active.id) || id == librarianWorkstream(active.id) {
			continue
		}
		streams, err := active.repository.Workstreams()
		if err != nil {
			return "", "", nil, &APIError{Code: Internal, Message: fmt.Sprintf("cannot read the workstreams of project %s; check the trace repository", active.id)}
		}
		if slices.Contains(streams, id) {
			return active.id, id, active.repository, nil
		}
	}
	return "", "", nil, &APIError{Code: Validation, Message: fmt.Sprintf("workstream %s is not in an active project; list workstreams with osmia status", id)}
}

// send accepts an owner message as the next turn of the workstream's
// chief-of-staff thread. It returns only after the request is durable.
func (s *Service) send(ctx context.Context, raw string, req SendRequest) (ConversationEntry, *APIError) {
	_, stream, repository, api := s.conversationTrace(raw)
	if api != nil {
		return ConversationEntry{}, api
	}
	if design, err := isDesignWorkstream(repository, stream); err != nil {
		return ConversationEntry{}, &APIError{Code: Internal, Message: fmt.Sprintf("cannot read the kind of workstream %s; check the trace repository", stream)}
	} else if design {
		return ConversationEntry{}, &APIError{Code: Validation, Message: fmt.Sprintf("workstream %s is a design session; send messages with POST %s/design/%s/message", stream, Prefix, stream)}
	}
	if blocked, err := s.designStartBlocksDraft(repository, stream); err != nil {
		return ConversationEntry{}, &APIError{Code: Internal, Message: err.Error()}
	} else if blocked {
		state, err := repository.Workflow(stream, designStartSubject)
		if err != nil || (state.Value != "active" && state.Value != "complete" && state.Value != "invalid") {
			return ConversationEntry{}, &APIError{Code: Conflict, Message: "the design workgroup is starting; retry once conversation adoption completes"}
		}
	}
	return s.sendChief(ctx, stream, req, func() (*trace.Repository, config.WorkstreamID, config.WorkstreamID, func(), error) {
		target, host, bundle, close, err := s.chiefRepository(ctx, repository, stream)
		if err != nil {
			return nil, "", "", close, apiErrAsError{Api: &APIError{Code: Internal, Message: fmt.Sprintf("cannot resolve the workgroup of workstream %s; check the trace repository", stream)}}
		}
		return target, host, bundle, close, nil
	})
}

// sendChief assembles context, releases its read handle and resolves again for
// the durable enqueue. An empty address sends to the shared host itself.
func (s *Service) sendChief(ctx context.Context, stream config.WorkstreamID, req SendRequest, resolve func() (*trace.Repository, config.WorkstreamID, config.WorkstreamID, func(), error)) (ConversationEntry, *APIError) {
	wholeGroup := stream == ""
	if strings.TrimSpace(req.Text) == "" {
		return ConversationEntry{}, &APIError{Code: Validation, Message: "text must not be empty"}
	}
	cfg := s.current()
	name, profile, err := s.chiefOverride(cfg)
	if err != nil {
		return ConversationEntry{}, &APIError{Code: Internal, Message: fmt.Sprintf("role %s has no usable profile %q; check osmia profiles", trace.ChiefOfStaff, name)}
	}
	target, host, bundleStream, closeTarget, err := resolve()
	if err != nil {
		return ConversationEntry{}, workgroupAuthorityError(err)
	}
	if stream == "" {
		stream = host
	}
	project := target.Project()
	bundleProject, err := target.ProjectFor(bundleStream)
	if err != nil {
		closeTarget()
		return ConversationEntry{}, &APIError{Code: Internal, Message: err.Error()}
	}
	if bundleProject.Project() != target.Project() {
		project = bundleProject.Project()
	}
	context, err := chiefContext(ctx, s.Context(), target, closeTarget, project, bundleStream)
	if err != nil {
		if wholeGroup {
			return ConversationEntry{}, workgroupAuthorityError(err)
		}
		return ConversationEntry{}, &APIError{Code: Internal, Message: fmt.Sprintf("cannot assemble the context of workstream %s; check the charter, the knowledge base and the trace repository", stream)}
	}
	note := ""
	if host != stream {
		note = fmt.Sprintf(" The owner addressed workstream %s, a member of your workgroup; answer or escalate one of its questions as \"%s:<number>\".", stream, stream)
	}
	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return ConversationEntry{}, &APIError{Code: Internal, Message: "cannot generate a message ID"}
	}
	id := hex.EncodeToString(nonce[:])
	at := s.now()
	failed := &APIError{Code: Internal, Message: fmt.Sprintf("cannot record the message for workstream %s; check the trace repository", stream)}
	// chiefContext already closed the first resolution once it finished
	// reading from it, so the durable write below reopens the target fresh,
	// exactly as chiefRepository documents: transient, never held open past
	// one call.
	target, _, _, closeTarget, err = resolve()
	if err != nil {
		return ConversationEntry{}, workgroupAuthorityError(err)
	}
	defer closeTarget()
	if _, err := target.EnsureChiefOfStaff(ctx, stream, at, serviceActor); err != nil {
		if wholeGroup {
			return ConversationEntry{}, workgroupAuthorityError(err)
		}
		return ConversationEntry{}, failed
	}
	q, err := target.EnqueueTurn(ctx, trace.TurnRequest{
		Header:       trace.Header{Schema: "osmia.trace.turn-request", Version: trace.Version, ID: "request_" + id, Revision: 1, Project: target.Project(), Workstream: stream, At: at, Actor: ownerActor, Cause: "owner-message"},
		AgentID:      trace.ChiefOfStaff,
		ThreadID:     trace.ChiefOfStaff,
		TurnID:       "message_" + id,
		Profile:      profile,
		SystemPrompt: fmt.Sprintf(chiefPrompt, host) + note + "\n\n" + chiefDocumentsGuidance + "\n\n" + priorityGuidance + "\n\n" + pauseGuidance + "\n\n" + changeGuidance + "\n\n" + amendmentGuidance + "\n\n" + charterGuidance + "\n\n" + contestGuidance + "\n\n" + objectionGuidance + "\n\n" + driftHandbackGuidance + "\n\n" + ownerWritingGuidance + "\n\n" + context,
		Prompt:       req.Text,
	})
	if err != nil {
		if wholeGroup {
			return ConversationEntry{}, workgroupAuthorityError(err)
		}
		return ConversationEntry{}, failed
	}
	entry := conversation(q)[0]
	if th, err := target.Thread(q.Request.Workstream, q.Request.AgentID); err == nil {
		entry.QueuedReason = queuedReason(th, q)
	}
	return entry, nil
}

// conversationList reads the owner's messages to the workstream's chief of
// staff, and its final responses to them, from the thread's turn log.
func (s *Service) conversationList(raw string) (ConversationResponse, *APIError) {
	_, stream, repository, api := s.conversationTrace(raw)
	if api != nil {
		return ConversationResponse{}, api
	}
	target, _, _, closeTarget, err := s.chiefRepository(context.Background(), repository, stream)
	if err != nil {
		return ConversationResponse{}, &APIError{Code: Internal, Message: fmt.Sprintf("cannot resolve the workgroup of workstream %s; check the trace repository", stream)}
	}
	defer closeTarget()
	entries, err := readview.Consistent(func() ([]ConversationEntry, error) { return conversationEntries(target, stream) }, target.Generation)
	if err != nil {
		return ConversationResponse{}, &APIError{Code: Internal, Message: fmt.Sprintf("cannot read the conversation of workstream %s; check the trace repository", stream)}
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
		entries[0].QueuedReason = queuedReason(t, q)
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
// contests, questions and committee objections it raised to the owner, the
// owner's rulings on those questions, the objections it dismissed and the
// units it moved.
func chiefActions(repository *trace.Repository, stream config.WorkstreamID) ([]ConversationEntry, error) {
	return chiefActionsWithIdentity(repository, stream, nil)
}

func chiefActionsWithIdentity(repository *trace.Repository, stream config.WorkstreamID, identify func(ConversationEntry, config.WorkstreamID)) ([]ConversationEntry, error) {
	docs, err := trace.Read[trace.Document](repository, stream)
	if err != nil {
		return nil, err
	}
	var out []ConversationEntry
	emit := func(entry ConversationEntry, member config.WorkstreamID) {
		out = append(out, entry)
		if identify != nil {
			identify(entry, member)
		}
	}
	members, err := repository.WorkgroupMembers(stream)
	if err != nil {
		return nil, err
	}
	escalations, err := repository.Inbox()
	if err != nil {
		return nil, err
	}
	for _, escalation := range escalations {
		if !slices.ContainsFunc(escalation.Questions, func(question trace.QuestionState) bool {
			return slices.Contains(members, question.Asked.Workstream)
		}) {
			continue
		}
		member := config.WorkstreamID("")
		for i, q := range escalation.Questions {
			if i == 0 {
				member = q.Asked.Workstream
			} else if member != q.Asked.Workstream {
				member = ""
				break
			}
		}
		text := "Question for you: " + escalation.Rephrasing
		if escalation.Hold != "" {
			text += "\nRecoverable hold: " + escalation.Hold
		}
		if escalation.Blocked != "" {
			text += "\nBlocked: " + escalation.Blocked
		}
		if len(escalation.Options) > 0 {
			text += "\nOptions: " + strings.Join(escalation.Options, "; ")
		}
		if escalation.Recommendation != "" {
			text += "\nRecommendation: " + escalation.Recommendation
		}
		emit(ConversationEntry{Turn: escalation.Batch, Kind: "action", Text: text, At: escalation.EscalatedAt, State: TurnDone}, member)
		// One consolidated escalation has one owner answer. Read its original
		// ruling revision so relaying it cannot change its text or timestamp.
		// Background chief turns stay out of the owner's conversation.
		if escalation.State != trace.QuestionEscalated && len(escalation.Questions) > 0 {
			q := escalation.Questions[0]
			if q.Ruling != nil && q.Ruling.Decision == trace.DecisionRuling {
				ruling, err := repository.QuestionRuling(q.Asked.Workstream, q.Ruling.ID, 1)
				if err != nil {
					return nil, err
				}
				emit(ConversationEntry{Turn: "ruling_" + escalation.Batch, Kind: "action", Text: "You ruled: " + ruling.OwnerResponse, At: ruling.At, State: TurnDone}, member)
			}
		}
	}
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
		emit(ConversationEntry{Turn: decision.Turn, Kind: "action", Text: text, At: d.At, State: TurnDone}, stream)
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
		emit(ConversationEntry{Turn: move.Turn, Kind: "action", Text: text, At: d.At, State: TurnDone}, stream)
	}
	for _, d := range docs {
		if !strings.HasPrefix(d.ID, workstreamMovePrefix) {
			continue
		}
		var move WorkstreamMove
		if err := json.Unmarshal([]byte(d.Content), &move); err != nil {
			return nil, fmt.Errorf("%s: %w", d.Path, err)
		}
		if move.By != rulerName(chiefActor) {
			continue
		}
		text := fmt.Sprintf("Moved the workstream from %s to %s. %s", move.From, move.To, move.Note)
		if move.From == move.To {
			text = fmt.Sprintf("Asked for a fresh final review of the %s workstream. %s", move.To, move.Note)
		}
		emit(ConversationEntry{Turn: move.Turn, Kind: "action", Text: text, At: d.At, State: TurnDone}, stream)
	}
	// Each revision of a round's decisions adds one decision, shown at the
	// time that revision was recorded.
	decided := map[string]bool{}
	for _, d := range docs {
		if !strings.HasPrefix(d.Path, "shed/") || !strings.HasSuffix(d.Path, "/chief.json") {
			continue
		}
		decisions, err := shed.ParseChiefDecisions([]byte(d.Content))
		if err != nil {
			return nil, fmt.Errorf("%s: %w", d.Path, err)
		}
		for _, c := range decisions.Decisions {
			if decided[c.Objection] {
				continue
			}
			decided[c.Objection] = true
			text := fmt.Sprintf("Dismissed objection %s on your behalf: %s", c.Objection, c.Note)
			if c.Decision == shed.Escalate {
				text = fmt.Sprintf("Raised objection %s to you: %s", c.Objection, c.Note)
			}
			emit(ConversationEntry{Turn: c.Turn, Kind: "action", Text: text, At: d.At, State: TurnDone}, stream)
		}
	}

	host, err := repository.ChiefOfStaffHost(stream)
	if err != nil {
		return nil, err
	}
	changes, err := readChanges(repository, host)
	if err != nil {
		return nil, err
	}
	for _, c := range changes {
		if c.State == "failed" {
			emit(ConversationEntry{Turn: "change-" + c.ID + "-failed", Kind: "action", Text: c.Failure, State: TurnDone}, "")
			continue
		}
		if c.State == "drafting" || c.State == "debating" || c.State == "answering" || c.State == "presenting" {
			continue
		}
		text := c.Summary()
		if c.Decision == nil {
			text += fmt.Sprintf("\nApprove or reject this impact summary (revision %d).", c.Revision)
		}
		if c.Request.Kind == "cancel" && c.Decision == nil {
			text += "\nChoose whether open PRs are closed or left as drafts. Merged work is untouched."
		}
		for _, d := range c.Debate {
			text += "\nCommittee: " + d.Note
			for _, objection := range d.Objections {
				text += "\nObjection: " + objection
			}
		}
		if c.Reply != "" {
			text += "\nArchitect: " + c.Reply
		}
		if c.Decision != nil {
			text += "\nOwner decision: " + c.Decision.Decision
		}
		docs, err := trace.Read[trace.Document](repository, host)
		if err != nil {
			return nil, err
		}
		var at time.Time
		for _, d := range docs {
			if d.ID == workgroupChangeDocument(c.ID) {
				at = d.At
				break
			}
		}
		emit(ConversationEntry{Turn: "change-" + c.ID + "-impact", Kind: "action", Text: text, At: at, State: TurnDone}, "")
	}
	return out, nil
}
