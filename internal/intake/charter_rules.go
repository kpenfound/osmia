package intake

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"

	"github.com/kpenfound/osmia/internal/charter"
	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/trace"
)

// charterActor records the charter revisions that append ratified rules and
// the proposals' moves to chartered.
var CharterActor = trace.Actor{Kind: "service", ID: "charter"}

// charterWriteAttempts bounds how often one pass rereads the charter after
// the owner's concurrent edit took the revision a ratified rule was written
// against.
const CharterWriteAttempts = 3

func RenderCharterProposalView(p trace.CharterProposalState, detail string) CharterProposalView {
	return CharterProposalView{Workstream: p.Workstream, Question: p.Proposal.Question, State: p.State.Value, Rule: p.Proposal.Rule, Number: p.Proposal.Number,
		Ruling: p.Proposal.Ruling, RulingRevision: p.Proposal.RulingRevision, OwnerResponse: p.Proposal.OwnerResponse, ProposedAt: p.Proposed.At,
		Decision: p.Proposal.Decision, Note: p.Proposal.Note, Charter: p.Proposal.Charter, Detail: detail}
}

// findCharterProposal returns the charter proposal made from question of the
// workstream, and whether there is one.
func FindCharterProposal(repository *trace.Repository, stream config.WorkstreamID, question string) (trace.CharterProposalState, bool, error) {
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

// CharterProposalLocation resolves both a target proposal key and its proposing
// member's question, so owner decisions reach the same target after a restart.
func (s *Controller) CharterProposalLocation(repository *trace.Repository, stream config.WorkstreamID, question string) (*trace.Repository, config.WorkstreamID, string, error) {
	_, projects := s.Runtimes()
	for _, project := range projects {
		proposals, err := project.Repository.CharterProposals()
		if err != nil {
			return nil, "", "", err
		}
		for _, p := range proposals {
			if p.Workstream == stream && p.Proposal.Question == question || p.Proposal.SourceWorkstream == stream && p.Proposal.SourceQuestion == question {
				return project.Repository, p.Workstream, p.Proposal.Question, nil
			}
		}
	}
	return repository, stream, question, nil
}

// charterProposals lists the charter proposals of every active project that
// wait for the owner, leaving out those of abandoned workstreams.
func (s *Controller) CharterProposals() (CharterProposalsResponse, *APIError) {
	cfg, projects := s.Runtimes()
	out := CharterProposalsResponse{Proposals: []CharterProposalView{}}
	for _, active := range projects {
		if !cfg.Active(active.ID) {
			continue
		}
		failed := &APIError{Code: Internal, Message: fmt.Sprintf("cannot read the charter proposals of project %s; check the trace repository", active.ID)}
		proposals, err := active.Repository.CharterProposals()
		if err != nil {
			return CharterProposalsResponse{}, failed
		}
		for _, p := range proposals {
			if p.State.Value != trace.CharterProposed {
				continue
			}
			gone, err := Abandoned(active.Repository, p.Workstream)
			if err != nil {
				return CharterProposalsResponse{}, failed
			}
			if !gone {
				out.Proposals = append(out.Proposals, RenderCharterProposalView(p, ""))
			}
		}
	}
	return out, nil
}

// charterProposal serves the charter proposal made from one question of a
// workstream, whatever its state.
func (s *Controller) CharterProposal(raw, question string) (CharterProposalView, *APIError) {
	_, stream, repository, api := s.ConversationTrace(raw)
	if api != nil {
		return CharterProposalView{}, api
	}
	repository, stream, question, err := s.CharterProposalLocation(repository, stream, question)
	if err != nil {
		return CharterProposalView{}, &APIError{Code: Internal, Message: err.Error()}
	}
	p, found, err := FindCharterProposal(repository, stream, question)
	if err != nil {
		return CharterProposalView{}, &APIError{Code: Internal, Message: fmt.Sprintf("cannot read the charter proposal of question %s of workstream %s; check the trace repository", question, stream)}
	}
	if !found {
		return CharterProposalView{}, &APIError{Code: NotFound, Message: fmt.Sprintf("workstream %s has no charter proposal for question %s", stream, question)}
	}
	return RenderCharterProposalView(p, ""), nil
}

// decideCharter records the owner's decision on a charter proposal.
func (s *Controller) DecideCharter(ctx context.Context, raw, question string, req CharterDecisionRequest) (CharterProposalView, *APIError) {
	project, stream, repository, api := s.ConversationTrace(raw)
	if api != nil {
		return CharterProposalView{}, api
	}
	return s.RecordCharterDecision(ctx, project, stream, repository, question, req, OwnerActor, "owner-charter")
}

// recordCharterDecision records one decision on a proposed charter rule, as
// the actor: the next revision of the proposal's charter.json with the
// decision, and its move to ratified or declined, in one commit. A declined
// proposal tells the chief of staff; a ratified one does when its rule is in
// the charter. Deciding a decided proposal the same way again returns it and
// records nothing; deciding it the other way is refused. Proposals of
// abandoned workstreams take no decision.
func (s *Controller) RecordCharterDecision(ctx context.Context, _ config.ProjectID, stream config.WorkstreamID, repository *trace.Repository, question string, req CharterDecisionRequest, actor trace.Actor, cause string) (CharterProposalView, *APIError) {
	failed := &APIError{Code: Internal, Message: fmt.Sprintf("cannot record the decision on the charter proposal of question %s of workstream %s; check the trace repository", question, stream)}
	decision := strings.TrimSpace(req.Decision)
	if decision != trace.CharterRatify && decision != trace.CharterDecline {
		return CharterProposalView{}, &APIError{Code: Validation, Message: "a charter decision is ratify or decline"}
	}
	repository, stream, question, err := s.CharterProposalLocation(repository, stream, question)
	if err != nil {
		return CharterProposalView{}, failed
	}
	project := repository.Project()
	gone, err := Abandoned(repository, stream)
	if err != nil {
		return CharterProposalView{}, failed
	}
	if gone {
		return CharterProposalView{}, &APIError{Code: Conflict, Message: fmt.Sprintf("workstream %s is abandoned and its charter proposals take no decision", stream)}
	}
	p, found, err := FindCharterProposal(repository, stream, question)
	if err != nil {
		return CharterProposalView{}, failed
	}
	if !found {
		return CharterProposalView{}, &APIError{Code: NotFound, Message: fmt.Sprintf("workstream %s has no charter proposal for question %s", stream, question)}
	}
	if p.State.Value != trace.CharterProposed {
		if p.Proposal.Decision == decision {
			return RenderCharterProposalView(p, fmt.Sprintf("the owner's decision to %s the charter proposal of question %s is already recorded", decision, question)), nil
		}
		return CharterProposalView{}, &APIError{Code: Conflict, Message: fmt.Sprintf("the charter proposal of question %s is already decided: %s", question, FeatureState(p.Proposal.Decision))}
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
	at := s.Now()
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
			return CharterProposalView{}, &APIError{Code: Conflict, Message: fmt.Sprintf("the charter proposal of question %s changed while the decision was recorded; read it again with osmia charter and retry", question)}
		}
		return CharterProposalView{}, failed
	}
	p.Latest, p.Proposal, p.State = doc, next, trace.WorkflowState{Value: to, Version: p.State.Version + 1}
	return RenderCharterProposalView(p, reason), nil
}

// decideCharterTool records the owner's decision on a charter proposal that
// the owner gave the chief of staff in a message.
const DecideCharterTool = "decide_charter"

// charterGuidance tells the chief of staff when and how to use
// decide_charter. It belongs in the system prompt of every owner message
// turn.
const CharterGuidance = "When the owner's message ratifies or declines a charter rule you proposed, call decide_charter with the question the proposal came from, the owner's decision and the owner's own words as the note. " +
	"Never decide a proposal the owner has not decided in the message; the owner can also decide it with osmia charter."

// decideCharter returns the decide_charter tool of one claimed
// chief-of-staff turn. It records the same decision as POST
// /charter/<workstream>/<question> on the turn's workstream, with the owner
// whose message the turn answers as its actor. A decision the service
// refuses is an ordinary result, {"recorded":false,"reason":...}, and
// records nothing.
func (c *Controls) DecideCharter(repository *trace.Repository, scope coreadapter.Scope) coreadapter.Tool {
	tool := coreadapter.Tool{Name: DecideCharterTool, Effect: coreadapter.ToolMemory,
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
		s := c.Load()
		if s == nil {
			return nil, errors.New("charter decisions are unavailable")
		}
		request, owner, err := repository.OwnerTurn(trace.ChiefOfStaff, scope)
		if err != nil {
			return nil, err
		}
		if !owner {
			return PriorityRefusal("only the owner decides a charter proposal; this turn does not answer a message from the owner")
		}
		member, question, err := repository.ChiefQuestionTarget(config.WorkstreamID(scope.Workstream), input.Question)
		if err != nil {
			return PriorityRefusal(err.Error())
		}
		out, api := s.RecordCharterDecision(ctx, repository.Project(), member, repository, question,
			CharterDecisionRequest{Decision: input.Decision, Note: input.Note}, request.Actor, request.ID)
		if api != nil {
			if api.Code == Internal {
				return nil, errors.New(api.Message)
			}
			return PriorityRefusal(api.Message)
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
type Charterer struct {
	S          *ChartererDependencies
	Repository *trace.Repository
}

// charterSource is the provenance a ratified rule carries in the charter, and
// the cause of the charter revision that appends it: the proposal's path.
func CharterSource(p trace.CharterProposalState) string {
	return "workstreams/" + string(p.Workstream) + "/" + p.Latest.Path
}

// Pass records the rule of every ratified proposal in the charter.
func (c Charterer) Pass(ctx context.Context) error {
	var proposals []trace.CharterProposalState
	err := c.Repository.WithWorkstreamDiscovery(func([]config.WorkstreamID) error {
		var err error
		proposals, err = c.Repository.CharterProposals()
		return err
	})
	if err != nil {
		return err
	}
	for _, p := range proposals {
		if p.State.Value != trace.CharterRatified {
			continue
		}
		if err := c.Write(ctx, p); err != nil {
			return err
		}
	}
	return nil
}

// write appends a ratified proposal's rule to the charter as its next number,
// then records the number and the charter revision in the proposal and moves
// it to chartered, which tells the workstream's chief of staff. The charter
// revision's cause is the proposal's path, so a charter revision recorded
// before a stop is found again rather than appended twice. The write is the
// revision after the one read, so an owner edit recorded since the read
// refuses it: the write then reads the charter again, up to
// charterWriteAttempts times in one pass, and later passes try again.
func (c Charterer) Write(ctx context.Context, p trace.CharterProposalState) error {
	source := CharterSource(p)
	authority, err := c.Repository.CharterRepository()
	if err != nil {
		return err
	}
	written, err := c.Written(source)
	if err != nil {
		return err
	}
	for attempt := 0; written == nil && attempt < CharterWriteAttempts; attempt++ {
		current, err := c.Repository.Charter(ctx)
		if err != nil {
			return err
		}
		content := charter.AppendRule(current.Content, charter.Parse(current.Content).Next(), p.Proposal.Rule, fmt.Sprintf("ratified from %s revision %d", p.Proposal.Ruling, p.Proposal.RulingRevision))
		next := trace.Document{Header: trace.Header{Schema: "osmia.trace.document", Version: trace.Version, ID: current.ID, Revision: current.Revision + 1, Project: authority.Project(), At: c.S.Now(), Actor: CharterActor, Cause: source},
			Path: current.Path, Content: content}
		if err := c.S.Step("charter-write"); err != nil {
			return err
		}
		err = authority.Append(ctx, next)
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
	if err := c.S.Step("charter-written"); err != nil {
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
	at := c.S.Now()
	question := p.Proposal.Question
	subject := trace.CharterSubject(question)
	id := subject + "_" + trace.CharterChartered
	reason := fmt.Sprintf("the owner's ruling on question %s is charter#%d, recorded in charter.md revision %d, and every bundle on the project carries it as a notice", question, next.Number, written.Revision)
	doc := trace.Document{Header: trace.Header{Schema: "osmia.trace.document", Version: trace.Version, ID: p.Latest.ID, Revision: p.Latest.Revision + 1, Project: c.Repository.Project(), Workstream: p.Workstream, At: at, Actor: CharterActor, Cause: id},
		Path: p.Latest.Path, Content: string(content) + "\n"}
	tx := trace.Transaction{ExpectedVersion: p.State.Version,
		Transition: trace.Transition{Header: trace.Header{Schema: "osmia.trace.transition", Version: trace.Version, ID: id, Revision: 1, Project: c.Repository.Project(), Workstream: p.Workstream, At: at, Actor: CharterActor, Cause: source},
			Subject: subject, From: trace.CharterRatified, To: trace.CharterChartered, Reason: reason},
		Events: []trace.Event{trace.Notice(id, "charter", fmt.Sprintf("Charter rule %d is recorded from the owner's ruling on question %s, and every bundle on the project carries it as a notice: %s", next.Number, question, p.Proposal.Rule))}}
	if _, err := c.Repository.RecordDocumentsWith(ctx, []trace.Document{doc}, tx); err != nil && !errors.Is(err, trace.ErrConflict) {
		return err
	}
	return nil
}

// written returns the charter revision that appended the rule of the
// proposal at source, or nil when none did.
func (c Charterer) Written(source string) (*trace.Document, error) {
	docs, err := c.Repository.ProjectDocuments()
	if err != nil {
		return nil, err
	}
	for _, d := range docs {
		if d.Path == "charter.md" && d.Cause == source && d.Actor == CharterActor {
			return &d, nil
		}
	}
	return nil, nil
}
