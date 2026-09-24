package service

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"time"

	"github.com/kpenfound/osmia/internal/charter"
	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/trace"
)

// charterActor records the charter revisions that append ratified rules and
// the proposals' moves to chartered.
var charterActor = trace.Actor{Kind: "service", ID: "charter"}

// charterWriteAttempts bounds how often one pass rereads the charter after
// an owner edit it did not record refused the write of a ratified rule.
const charterWriteAttempts = 3

// CharterDecisionRequest is the owner's decision on a charter proposal:
// ratify or decline, with an optional note.
type CharterDecisionRequest struct {
	Decision string `json:"decision"`
	Note     string `json:"note,omitempty"`
}

// CharterProposalView is one charter proposal. Number is the number the rule
// would take when it was proposed, and the number it took once State is
// chartered; Charter is then the charter.md revision that records it. Ruling
// and RulingRevision name the owner's ruling it comes from.
type CharterProposalView struct {
	Workstream     config.WorkstreamID `json:"workstream"`
	Question       string              `json:"question"`
	State          string              `json:"state"`
	Rule           string              `json:"rule"`
	Number         int                 `json:"number"`
	Ruling         string              `json:"ruling"`
	RulingRevision int                 `json:"ruling_revision"`
	OwnerResponse  string              `json:"owner_response"`
	ProposedAt     time.Time           `json:"proposed_at"`
	Decision       string              `json:"decision,omitempty"`
	Note           string              `json:"note,omitempty"`
	Charter        int                 `json:"charter,omitempty"`
	Detail         string              `json:"detail,omitempty"`
}

// CharterProposalsResponse lists the charter proposals waiting for the
// owner's decision, oldest first.
type CharterProposalsResponse struct {
	Proposals []CharterProposalView `json:"proposals"`
}

func charterProposalView(p trace.CharterProposalState, detail string) CharterProposalView {
	return CharterProposalView{Workstream: p.Workstream, Question: p.Proposal.Question, State: p.State.Value, Rule: p.Proposal.Rule, Number: p.Proposal.Number,
		Ruling: p.Proposal.Ruling, RulingRevision: p.Proposal.RulingRevision, OwnerResponse: p.Proposal.OwnerResponse, ProposedAt: p.Proposed.At,
		Decision: p.Proposal.Decision, Note: p.Proposal.Note, Charter: p.Proposal.Charter, Detail: detail}
}

// findCharterProposal returns the charter proposal made from question of the
// workstream, and whether there is one.
func findCharterProposal(repository *trace.Repository, stream config.WorkstreamID, question string) (trace.CharterProposalState, bool, error) {
	proposals, err := repository.CharterProposals()
	if err != nil {
		return trace.CharterProposalState{}, false, err
	}
	i := slices.IndexFunc(proposals, func(p trace.CharterProposalState) bool {
		return p.Workstream == stream && p.Proposal.Question == question
	})
	if i < 0 {
		return trace.CharterProposalState{}, false, nil
	}
	return proposals[i], true, nil
}

// charterProposals lists the active project's charter proposals that wait
// for the owner, leaving out those of abandoned workstreams.
func (s *Service) charterProposals() (CharterProposalsResponse, *APIError) {
	s.mu.Lock()
	active, cfg := s.active, s.cfg
	s.mu.Unlock()
	out := CharterProposalsResponse{Proposals: []CharterProposalView{}}
	if !cfg.HasProject() || active == nil {
		return out, nil
	}
	failed := &APIError{Internal, fmt.Sprintf("cannot read the charter proposals of project %s; check the trace repository", cfg.Project.ID)}
	proposals, err := active.repository.CharterProposals()
	if err != nil {
		return CharterProposalsResponse{}, failed
	}
	for _, p := range proposals {
		if p.State.Value != trace.CharterProposed {
			continue
		}
		gone, err := abandoned(active.repository, p.Workstream)
		if err != nil {
			return CharterProposalsResponse{}, failed
		}
		if !gone {
			out.Proposals = append(out.Proposals, charterProposalView(p, ""))
		}
	}
	return out, nil
}

// charterProposal serves the charter proposal made from one question of a
// workstream, whatever its state.
func (s *Service) charterProposal(raw, question string) (CharterProposalView, *APIError) {
	_, stream, repository, api := s.conversationTrace(raw)
	if api != nil {
		return CharterProposalView{}, api
	}
	p, found, err := findCharterProposal(repository, stream, question)
	if err != nil {
		return CharterProposalView{}, &APIError{Internal, fmt.Sprintf("cannot read the charter proposal of question %s of workstream %s; check the trace repository", question, stream)}
	}
	if !found {
		return CharterProposalView{}, &APIError{NotFound, fmt.Sprintf("workstream %s has no charter proposal for question %s", stream, question)}
	}
	return charterProposalView(p, ""), nil
}

// decideCharter records the owner's decision on a charter proposal.
func (s *Service) decideCharter(ctx context.Context, raw, question string, req CharterDecisionRequest) (CharterProposalView, *APIError) {
	project, stream, repository, api := s.conversationTrace(raw)
	if api != nil {
		return CharterProposalView{}, api
	}
	return s.recordCharterDecision(ctx, project, stream, repository, question, req, ownerActor, "owner-charter")
}

// recordCharterDecision records one decision on a proposed charter rule, as
// the actor: the next revision of the proposal's charter.json with the
// decision, and its move to ratified or declined, in one commit. A declined
// proposal tells the chief of staff; a ratified one does when its rule is in
// the charter. Deciding a decided proposal the same way again returns it and
// records nothing; deciding it the other way is refused. Proposals of
// abandoned workstreams take no decision.
func (s *Service) recordCharterDecision(ctx context.Context, project config.ProjectID, stream config.WorkstreamID, repository *trace.Repository, question string, req CharterDecisionRequest, actor trace.Actor, cause string) (CharterProposalView, *APIError) {
	failed := &APIError{Internal, fmt.Sprintf("cannot record the decision on the charter proposal of question %s of workstream %s; check the trace repository", question, stream)}
	decision := strings.TrimSpace(req.Decision)
	if decision != trace.CharterRatify && decision != trace.CharterDecline {
		return CharterProposalView{}, &APIError{Validation, "a charter decision is ratify or decline"}
	}
	gone, err := abandoned(repository, stream)
	if err != nil {
		return CharterProposalView{}, failed
	}
	if gone {
		return CharterProposalView{}, &APIError{Conflict, fmt.Sprintf("workstream %s is abandoned and its charter proposals take no decision", stream)}
	}
	p, found, err := findCharterProposal(repository, stream, question)
	if err != nil {
		return CharterProposalView{}, failed
	}
	if !found {
		return CharterProposalView{}, &APIError{NotFound, fmt.Sprintf("workstream %s has no charter proposal for question %s", stream, question)}
	}
	if p.State.Value != trace.CharterProposed {
		if p.Proposal.Decision == decision {
			return charterProposalView(p, fmt.Sprintf("the owner's decision to %s the charter proposal of question %s is already recorded", decision, question)), nil
		}
		return CharterProposalView{}, &APIError{Conflict, fmt.Sprintf("the charter proposal of question %s is already decided: %s", question, featureState(p.Proposal.Decision))}
	}
	next := p.Proposal
	next.Decision, next.Note = decision, strings.TrimSpace(req.Note)
	to := trace.CharterRatified
	reason := fmt.Sprintf("the owner ratified the charter proposal of question %s; the rule is appended to the charter next", question)
	if decision == trace.CharterDecline {
		to = trace.CharterDeclined
		reason = fmt.Sprintf("the owner declined the charter proposal of question %s; the charter is unchanged", question)
	}
	if next.Note != "" {
		reason += ": " + next.Note
	}
	content, err := json.MarshalIndent(next, "", "  ")
	if err != nil {
		return CharterProposalView{}, failed
	}
	at := s.now()
	doc := trace.Document{Header: trace.Header{Schema: "osmia.trace.document", Version: trace.Version, ID: p.Latest.ID, Revision: p.Latest.Revision + 1, Project: project, Workstream: stream, At: at, Actor: actor, Cause: cause},
		Path: p.Latest.Path, Content: string(content) + "\n"}
	subject := trace.CharterSubject(question)
	id := subject + "_" + to
	tx := trace.Transaction{ExpectedVersion: p.State.Version,
		Transition: trace.Transition{Header: trace.Header{Schema: "osmia.trace.transition", Version: trace.Version, ID: id, Revision: 1, Project: project, Workstream: stream, At: at, Actor: actor, Cause: cause}, Subject: subject, From: trace.CharterProposed, To: to, Reason: reason}}
	if to == trace.CharterDeclined {
		tx.Events = []trace.Event{trace.Notice(id, "charter", reason)}
	}
	if _, err := repository.RecordDocumentsWith(ctx, []trace.Document{doc}, tx); err != nil {
		if errors.Is(err, trace.ErrConflict) {
			return CharterProposalView{}, &APIError{Conflict, fmt.Sprintf("the charter proposal of question %s changed while the decision was recorded; read it again with osmia charter and retry", question)}
		}
		return CharterProposalView{}, failed
	}
	p.Latest, p.Proposal, p.State = doc, next, trace.WorkflowState{Value: to, Version: p.State.Version + 1}
	return charterProposalView(p, reason), nil
}

// decideCharterTool records the owner's decision on a charter proposal that
// the owner gave the chief of staff in a message.
const decideCharterTool = "decide_charter"

// charterGuidance tells the chief of staff when and how to use
// decide_charter. It belongs in the system prompt of every owner message
// turn.
const charterGuidance = "When the owner's message ratifies or declines a charter rule you proposed, call decide_charter with the question the proposal came from, the owner's decision and the owner's own words as the note. " +
	"Never decide a proposal the owner has not decided in the message; the owner can also decide it with osmia charter."

// decideCharter returns the decide_charter tool of one claimed
// chief-of-staff turn. It records the same decision as POST
// /charter/<workstream>/<question> on the turn's workstream, with the owner
// whose message the turn answers as its actor. A decision the service
// refuses is an ordinary result, {"recorded":false,"reason":...}, and
// records nothing.
func (c *runtimeControls) decideCharter(repository *trace.Repository, scope coreadapter.Scope) coreadapter.Tool {
	tool := coreadapter.Tool{Name: decideCharterTool, Effect: coreadapter.ToolMemory,
		Description: "Record the owner's decision on a charter rule you proposed, only when the owner decides it in a message. question: the question whose ruling the proposal came from; decision: ratify or decline; note: the owner's words. " +
			"A ratified rule is appended to the charter as its next number and becomes a notice in every bundle on the project; a declined one changes nothing.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"question":{"type":"string"},"decision":{"type":"string","enum":["ratify","decline"]},"note":{"type":"string"}},"required":["question","decision"],"additionalProperties":false}`)}
	tool.Handle = func(ctx context.Context, raw json.RawMessage) (json.RawMessage, error) {
		var input struct {
			Question string `json:"question"`
			Decision string `json:"decision"`
			Note     string `json:"note"`
		}
		d := json.NewDecoder(bytes.NewReader(raw))
		d.DisallowUnknownFields()
		if err := d.Decode(&input); err != nil {
			return nil, fmt.Errorf("tool input: %w", err)
		}
		if err := d.Decode(new(any)); err != io.EOF {
			return nil, errors.New("tool input must be one object")
		}
		s := c.service.Load()
		if s == nil {
			return nil, errors.New("charter decisions are unavailable")
		}
		request, owner, err := repository.OwnerTurn(trace.ChiefOfStaff, scope)
		if err != nil {
			return nil, err
		}
		if !owner {
			return priorityRefusal("only the owner decides a charter proposal; this turn does not answer a message from the owner")
		}
		out, api := s.recordCharterDecision(ctx, repository.Project(), config.WorkstreamID(scope.Workstream), repository, input.Question,
			CharterDecisionRequest{Decision: input.Decision, Note: input.Note}, request.Actor, request.ID)
		if api != nil {
			if api.Code == Internal {
				return nil, errors.New(api.Message)
			}
			return priorityRefusal(api.Message)
		}
		return json.Marshal(struct {
			Recorded bool   `json:"recorded"`
			Question string `json:"question"`
			State    string `json:"state"`
			Detail   string `json:"detail"`
		}{true, out.Question, out.State, out.Detail})
	}
	return tool
}

// charterer appends ratified charter rules to the project's charter.
type charterer struct {
	s          *Service
	repository *trace.Repository
}

// charterSource is the provenance a ratified rule carries in the charter, and
// the cause of the charter revision that appends it: the proposal's path.
func charterSource(p trace.CharterProposalState) string {
	return "workstreams/" + string(p.Workstream) + "/" + p.Latest.Path
}

// Pass records the rule of every ratified proposal in the charter.
func (c charterer) Pass(ctx context.Context) error {
	proposals, err := c.repository.CharterProposals()
	if err != nil {
		return err
	}
	for _, p := range proposals {
		if p.State.Value != trace.CharterRatified {
			continue
		}
		if err := c.write(ctx, p); err != nil {
			return err
		}
	}
	return nil
}

// write appends a ratified proposal's rule to the charter as its next number,
// then records the number and the charter revision in the proposal and moves
// it to chartered, which tells the workstream's chief of staff. The charter
// revision's cause is the proposal's path, so a charter revision recorded
// before a stop is found again rather than appended twice. The charter is
// read through the trace, which records an owner edit first, and written
// through it, which refuses to overwrite an owner edit made since the read:
// the write then reads the charter again, up to charterWriteAttempts times in
// one pass, and later passes try again.
func (c charterer) write(ctx context.Context, p trace.CharterProposalState) error {
	source := charterSource(p)
	written, err := c.written(source)
	if err != nil {
		return err
	}
	for attempt := 0; written == nil && attempt < charterWriteAttempts; attempt++ {
		current, err := c.repository.Charter(ctx, c.s.now())
		if err != nil {
			return err
		}
		content := charter.AppendRule(current.Content, charter.Parse(current.Content).Next(), p.Proposal.Rule, fmt.Sprintf("ratified from %s revision %d", p.Proposal.Ruling, p.Proposal.RulingRevision))
		next := trace.Document{Header: trace.Header{Schema: "osmia.trace.document", Version: trace.Version, ID: current.ID, Revision: current.Revision + 1, Project: c.repository.Project(), At: c.s.now(), Actor: charterActor, Cause: source},
			Path: current.Path, Content: content}
		if err := c.s.step("charter-write"); err != nil {
			return err
		}
		err = c.repository.Append(ctx, next)
		if errors.Is(err, trace.ErrConflict) {
			continue
		}
		if err != nil {
			return err
		}
		written = &next
	}
	if written == nil {
		return nil
	}
	if err := c.s.step("charter-written"); err != nil {
		return err
	}
	// The appended rule has the highest number of the revision that appended
	// it.
	next := p.Proposal
	next.Number, next.Charter = charter.Parse(written.Content).Next()-1, written.Revision
	content, err := json.MarshalIndent(next, "", "  ")
	if err != nil {
		return err
	}
	at := c.s.now()
	question := p.Proposal.Question
	subject := trace.CharterSubject(question)
	id := subject + "_" + trace.CharterChartered
	reason := fmt.Sprintf("the owner's ruling on question %s is charter#%d, recorded in charter.md revision %d, and every bundle on the project carries it as a notice", question, next.Number, written.Revision)
	doc := trace.Document{Header: trace.Header{Schema: "osmia.trace.document", Version: trace.Version, ID: p.Latest.ID, Revision: p.Latest.Revision + 1, Project: c.repository.Project(), Workstream: p.Workstream, At: at, Actor: charterActor, Cause: id},
		Path: p.Latest.Path, Content: string(content) + "\n"}
	tx := trace.Transaction{ExpectedVersion: p.State.Version,
		Transition: trace.Transition{Header: trace.Header{Schema: "osmia.trace.transition", Version: trace.Version, ID: id, Revision: 1, Project: c.repository.Project(), Workstream: p.Workstream, At: at, Actor: charterActor, Cause: source},
			Subject: subject, From: trace.CharterRatified, To: trace.CharterChartered, Reason: reason},
		Events: []trace.Event{trace.Notice(id, "charter", fmt.Sprintf("Charter rule %d is recorded from the owner's ruling on question %s, and every bundle on the project carries it as a notice: %s", next.Number, question, p.Proposal.Rule))}}
	if _, err := c.repository.RecordDocumentsWith(ctx, []trace.Document{doc}, tx); err != nil && !errors.Is(err, trace.ErrConflict) {
		return err
	}
	return nil
}

// written returns the charter revision that appended the rule of the
// proposal at source, or nil when none did.
func (c charterer) written(source string) (*trace.Document, error) {
	docs, err := trace.Read[trace.Document](c.repository, "")
	if err != nil {
		return nil, err
	}
	for _, d := range docs {
		if d.Path == "charter.md" && d.Cause == source && d.Actor == charterActor {
			return &d, nil
		}
	}
	return nil, nil
}
