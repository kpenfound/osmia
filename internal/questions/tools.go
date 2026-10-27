package questions

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/kpenfound/osmia/internal/charter"
	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/plan"
	"github.com/kpenfound/osmia/internal/trace"
)

const (
	AskTool            = "ask"
	AnswerTool         = "answer"
	EscalateTool       = "escalate"
	RelayRulingTool    = "relay_ruling"
	RouteAmendmentTool = "route_amendment"
	AmendTool          = "amend"
	ProposeCharterTool = "propose_charter"
	LogAssumptionTool  = "log_assumption"
)

// Guidance tells the chief of staff what to do with a question. It belongs
// in the system prompt of every turn that may carry one.
const Guidance = "When a service event says a question is open, choose exactly once for it. " +
	"Call answer for engineering decisions within the approved intent, including equivalent evidence and necessary supporting changes within an assignment. Cite the requirements and evidence supporting your decision: " + CitationForms + ". " +
	"Call escalate only when the answer would contradict or extend the agreed design: it changes owner intent, relaxes a constraint, contradicts an owner ruling, or needs an owner-reserved tradeoff. Missing context calls for investigation, not an intent decision. " +
	"Structure every escalation: section cites the spec or plan passage in tension, conflict states the tension itself, options are the choices and recommendation is what you would decide. " +
	"When a genuine gap affects more than one workstream or unit of your workgroup, escalate every affected question together as one consolidated batch, naming each with its own workstream when it is not your own: " +
	"the owner's one answer resumes every one of them, and work unaffected by the gap is never held up for it. " +
	"Call route_amendment for revised assignments, dependencies or newly discovered work. Plan-only revisions preserving approved intent are factory decisions; spec changes remain owner decisions. " +
	"A question that would change a contract section of a member's slice is filed as a project-level amendment instead of a plan amendment, and is never applied inside that workstream; route_amendment does this for you. " +
	"Log a judgment call you make within stated intent, rather than escalate or stay silent about it, with log_assumption: it appears in the affected workstreams' drafted pull request descriptions. Name affected members with workstreams; omit it for a workgroup-wide judgment. " +
	"Never answer for the owner because time is passing; a question waits as long as it needs. " +
	"Your workgroup may hold more than one workstream; a question number alone names one of your own workstream, and \"<workstream>:<number>\" names one of another member sharing you as its chief of staff. " +
	"The tools that read or act on one workstream's records, units or code, such as factory_context, record_discovery, inspect_code, move_workstream, move_unit, resolve_contested, decide_amendment, resolve_objection and hand_back_drift, take workstream to name the member they concern; without it they act on this conversation's own workstream. " +
	"When a service event says the owner ruled on an inbox entry, call relay_ruling exactly once for it, naming one of its questions: rephrase the ruling for the askers without changing what it decides, " +
	"and choose scope local when it matters only to them, or notify when it applies across the project. " +
	"When the owner's ruling is a standing rule for the project rather than a decision about this feature, also call propose_charter once for it with the rule as the charter should state it; " +
	"For an amendment to another member charter, set amends to that project's qualified rule citation; the proposal is recorded and decided in that project. " +
	"The owner ratifies or declines the proposal, so make it the attention of your status until they do."

// ChiefTools names the chief of staff's question tools.
var ChiefTools = []string{AnswerTool, EscalateTool, RelayRulingTool, RouteAmendmentTool, ProposeCharterTool, LogAssumptionTool}

type refusal struct {
	Recorded bool   `json:"recorded"`
	Reason   string `json:"reason"`
}

// Tools returns the question tools of the claimed turn scope names: ask for
// every role but the chief of staff, and answer, escalate, relay_ruling,
// route_amendment and propose_charter for the chief of staff. A request the trace refuses is
// an ordinary result, {"recorded":false,"reason":...}, so the agent reads why.
func Tools(repository *trace.Repository, agent string, scope coreadapter.Scope, now func() time.Time) ([]coreadapter.Tool, error) {
	return tools(repository, agent, scope, now, nil)
}

// DriftTools returns the ask and amend tools of the mason turn that
// resolves the feature branch's conflicts with upstream after drift rebase
// move: ask reaches the workstream's chief of staff about the drift rebase
// it resolves, and an amendment it files cites move's upstream commit and
// parks nothing.
func DriftTools(repository *trace.Repository, agent string, scope coreadapter.Scope, now func() time.Time, move trace.UpstreamMove) ([]coreadapter.Tool, error) {
	if scope.Role != "mason" || scope.Unit != "" {
		return nil, errors.New("only the drift mason turn reads a drift rebase")
	}
	return tools(repository, agent, scope, now, &move)
}

func tools(repository *trace.Repository, agent string, scope coreadapter.Scope, now func() time.Time, move *trace.UpstreamMove) ([]coreadapter.Tool, error) {
	if repository == nil || now == nil {
		return nil, errors.New("question tools require a trace and a clock")
	}
	if scope.Role == trace.ChiefOfStaff {
		return chiefTools(repository, agent, scope, now), nil
	}
	ask := coreadapter.Tool{Name: AskTool, Effect: coreadapter.ToolMemory,
		Description: "Ask the chief of staff a question you cannot answer from your context. Say what you need decided and what it blocks. For a conflict between member charters, supply charter_conflict with both project-qualified rules; Go sends it directly to the owner. The question is recorded and your turn ends; end it as soon as this returns. The answer arrives as your next turn on this thread, however long it takes.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"question":{"type":"string"},"charter_conflict":{"type":"array","items":{"type":"string"},"minItems":2}},"required":["question"],"additionalProperties":false}`)}
	ask.Handle = func(ctx context.Context, raw json.RawMessage) (json.RawMessage, error) {
		var input struct {
			Question        string   `json:"question"`
			CharterConflict []string `json:"charter_conflict"`
		}
		if err := decodeInput(raw, &input); err != nil {
			return nil, err
		}
		q, err := repository.AskCharterConflict(ctx, agent, scope, input.Question, input.CharterConflict, now())
		if err != nil {
			return refuse(err)
		}
		return encode(struct {
			Recorded bool   `json:"recorded"`
			Question string `json:"question"`
			Next     string `json:"next"`
		}{true, q.ID, "End your turn now. The answer arrives as your next turn on this thread."})
	}
	if move != nil {
		return []coreadapter.Tool{ask, amendmentTool(repository, agent, scope, now, false, move)}, nil
	}
	return []coreadapter.Tool{ask}, nil
}

func amendmentTool(repository *trace.Repository, agent string, scope coreadapter.Scope, now func() time.Time, routed bool, move *trace.UpstreamMove) coreadapter.Tool {
	name := AmendTool
	if routed {
		name = RouteAmendmentTool
	}
	properties := `"citations":{"type":"array","items":{"type":"string"}},"change":{"type":"string"},"reason":{"type":"string"}`
	required := `"citations","change","reason"`
	if routed {
		properties = `"question":{"type":"string"},` + properties
	}
	description := "Request a revised plan or spec against spec#<n> and/or plan#<unit> citations. A chief may omit question to commission replanning or bounded investigation for discovered work. Give the proposed change and reason. The request is recorded; end this turn when accepted."
	next := "End your turn now. The request is with the chief of staff."
	if move != nil {
		description = fmt.Sprintf("File an amendment request when upstream's change from %s to %s alters what a sealed criterion means. Cite the sealed spec#<n> and/or plan#<unit> it changes, and give the proposed change and reason; the request cites upstream commit %s and goes to the owner. Nothing waits for it: finish the resolution against the sealed spec as it stands.", move.From, move.To, move.To)
		next = "The request is with the chief of staff. Finish the resolution against the sealed spec as it stands, then call done."
	}
	tool := coreadapter.Tool{Name: name, Effect: coreadapter.ToolMemory,
		Description: description,
		InputSchema: json.RawMessage(`{"type":"object","properties":{` + properties + `},"required":[` + required + `],"additionalProperties":false}`)}
	tool.Handle = func(ctx context.Context, raw json.RawMessage) (json.RawMessage, error) {
		var input struct {
			Question  string   `json:"question"`
			Citations []string `json:"citations"`
			Change    string   `json:"change"`
			Reason    string   `json:"reason"`
		}
		if err := decodeInput(raw, &input); err != nil {
			return nil, err
		}
		stream := config.WorkstreamID(scope.Workstream)
		// A route_amendment naming a question resolves it the same way
		// every other chief-of-staff question tool does: against the
		// asking member's own workstream, which is where its citations,
		// its seal and the amendment itself (unless it becomes a
		// project-level amendment instead) belong, never against the
		// chief's host workstream by default.
		target := stream
		if routed && input.Question != "" {
			member, _, err := repository.ChiefQuestionTarget(stream, input.Question)
			if err != nil {
				return refuse(err)
			}
			target = member
		}
		if len(input.Citations) == 0 {
			return encode(refusal{Reason: "at least one spec#<n> or plan#<unit> citation is required"})
		}
		for _, c := range input.Citations {
			if _, ok := plan.ParseCitation(c); !ok && !strings.HasPrefix(c, "plan#") {
				return encode(refusal{Reason: "amendments cite only spec#<n> or plan#<unit>"})
			}
			if err := Resolve(ctx, repository, target, c); err != nil {
				var unresolved *Unresolved
				if errors.As(err, &unresolved) {
					return encode(refusal{Reason: unresolved.Error()})
				}
				return nil, err
			}
		}
		documents, err := trace.Read[trace.Document](repository, target)
		if err != nil {
			return nil, err
		}
		var document trace.Document
		for _, d := range documents {
			if d.Path == "seal.json" {
				document = d
			}
		}
		if document.ID == "" {
			return encode(refusal{Reason: "this workstream has no seal"})
		}
		var sealed struct {
			Seal     int    `json:"seal"`
			SpecHash string `json:"spec_hash"`
		}
		if err := json.Unmarshal([]byte(document.Content), &sealed); err != nil {
			return nil, err
		}
		outcome, err := repository.FileAmendment(ctx, agent, scope, trace.AmendmentRequest{QuestionID: input.Question, Citations: input.Citations, Change: input.Change, Reason: input.Reason, Seal: sealed.Seal, SealRevision: document.Revision, SpecHash: sealed.SpecHash, Upstream: move}, now())
		if err != nil {
			return refuse(err)
		}
		if outcome.ProjectLevel {
			return encode(struct {
				Recorded         bool   `json:"recorded"`
				ProjectAmendment string `json:"project_amendment"`
				Workgroup        string `json:"workgroup"`
				Next             string `json:"next"`
			}{true, outcome.ProjectAmendment.ID, string(outcome.ProjectAmendment.Workgroup),
				"A contract section is read-only inside a workstream; this was filed as a project-level amendment. " + next})
		}
		return encode(struct {
			Recorded  bool   `json:"recorded"`
			Amendment string `json:"amendment"`
			Next      string `json:"next"`
		}{true, outcome.Amendment.ID, next})
	}
	return tool
}

func chiefTools(repository *trace.Repository, agent string, scope coreadapter.Scope, now func() time.Time) []coreadapter.Tool {
	stream := config.WorkstreamID(scope.Workstream)
	answer := coreadapter.Tool{Name: AnswerTool, Effect: coreadapter.ToolMemory,
		Description: "Resolve an open question, including an engineering decision within approved intent. The record need not prescribe the implementation choice: explain your judgment and cite the scope, constraints or evidence it rests on. question: its number. text: the answer and reasoning the asker receives. citations: at least one of " + CitationForms + "; each must exist. An answer that would contradict an earlier ruling must be escalated instead.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"question":{"type":"string"},"text":{"type":"string"},"citations":{"type":"array","items":{"type":"string"}}},"required":["question","text","citations"],"additionalProperties":false}`)}
	answer.Handle = func(ctx context.Context, raw json.RawMessage) (json.RawMessage, error) {
		var input struct {
			Question  string   `json:"question"`
			Text      string   `json:"text"`
			Citations []string `json:"citations"`
		}
		if err := decodeInput(raw, &input); err != nil {
			return nil, err
		}
		member, _, err := repository.ChiefQuestionTarget(stream, input.Question)
		if err != nil {
			return refuse(err)
		}
		for _, c := range input.Citations {
			if err := Resolve(ctx, repository, member, c); err != nil {
				var unresolved *Unresolved
				if errors.As(err, &unresolved) {
					return encode(refusal{Reason: unresolved.Error()})
				}
				return nil, err
			}
		}
		ruling, err := repository.AnswerQuestion(ctx, agent, scope, input.Question, input.Text, input.Citations, now())
		if err != nil {
			return refuse(err)
		}
		return encode(struct {
			Recorded bool   `json:"recorded"`
			Question string `json:"question"`
			Next     string `json:"next"`
		}{true, ruling.QuestionID, "The answer is delivered to the asker as its next turn."})
	}
	escalate := coreadapter.Tool{Name: EscalateTool, Effect: coreadapter.ToolMemory,
		Description: "Send one or several open questions to the owner as one consolidated batch: a genuine gap affecting several workstreams or units of your workgroup is escalated once, naming every affected question, " +
			"not once per workstream, and the owner's one answer resumes every one of them while unaffected work keeps running. questions: their numbers, qualified \"<workstream>:<number>\" for any question not of your own workstream. " +
			"section: the spec#<n> or plan#<unit> passage in tension. conflict: the tension itself. rephrasing: the decision the owner must make, in one or two plain sentences naming the unit and criterion. blocked: what waits on it, in one sentence. options: the choices, one sentence each saying what happens if chosen. recommendation: the option you would choose and its main reason, in one or two sentences. " +
			"An escalated question can no longer be answered by you.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"questions":{"type":"array","items":{"type":"string"}},"rephrasing":{"type":"string"},"section":{"type":"string"},"conflict":{"type":"string"},"blocked":{"type":"string"},"options":{"type":"array","items":{"type":"string"}},"recommendation":{"type":"string"}},"required":["questions","rephrasing","blocked","options","recommendation"],"additionalProperties":false}`)}
	escalate.Handle = func(ctx context.Context, raw json.RawMessage) (json.RawMessage, error) {
		var input struct {
			Questions      []string `json:"questions"`
			Rephrasing     string   `json:"rephrasing"`
			Section        string   `json:"section"`
			Conflict       string   `json:"conflict"`
			Blocked        string   `json:"blocked"`
			Options        []string `json:"options"`
			Recommendation string   `json:"recommendation"`
		}
		if err := decodeInput(raw, &input); err != nil {
			return nil, err
		}
		batch, err := repository.EscalateQuestions(ctx, agent, scope, trace.EscalationRequest{Questions: input.Questions, Rephrasing: input.Rephrasing, Section: input.Section, Conflict: input.Conflict, Blocked: input.Blocked, Options: input.Options, Recommendation: input.Recommendation}, now())
		if err != nil {
			return refuse(err)
		}
		return encode(struct {
			Recorded  bool     `json:"recorded"`
			Batch     string   `json:"batch"`
			Questions []string `json:"questions"`
		}{true, batch, input.Questions})
	}
	relay := coreadapter.Tool{Name: RelayRulingTool, Effect: coreadapter.ToolMemory,
		Description: "Send the owner's ruling on an escalation back to everyone who asked. question: the number of one question the owner ruled on; every question of its escalation gets the same text. text: the ruling rephrased for the askers, deciding nothing the owner did not. scope: local, for the askers only, or notify, which also makes it a notice in every later context on this project.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"question":{"type":"string"},"text":{"type":"string"},"scope":{"type":"string","enum":["local","notify"]}},"required":["question","text","scope"],"additionalProperties":false}`)}
	relay.Handle = func(ctx context.Context, raw json.RawMessage) (json.RawMessage, error) {
		var input struct {
			Question string `json:"question"`
			Text     string `json:"text"`
			Scope    string `json:"scope"`
		}
		if err := decodeInput(raw, &input); err != nil {
			return nil, err
		}
		relayed, err := repository.RelayRuling(ctx, agent, scope, input.Question, input.Text, input.Scope, now())
		if err != nil {
			return refuse(err)
		}
		return encode(struct {
			Recorded  bool     `json:"recorded"`
			Questions []string `json:"questions"`
			Scope     string   `json:"scope"`
			Next      string   `json:"next"`
		}{true, relayed, input.Scope, "The ruling is delivered to each asker as its next turn."})
	}
	propose := coreadapter.Tool{Name: ProposeCharterTool, Effect: coreadapter.ToolMemory,
		Description: "Propose the owner's ruling on a question as a standing charter rule, when it applies to the project beyond this feature. question: the number of a question the owner ruled on; one proposal per ruling. rule: the rule as the charter should state it, one line. amends: optional charter citation, qualified with the target project ID when amending another member charter; the proposal is recorded and decided in that project. " +
			"The owner ratifies or declines it. A ratified rule is appended to the charter as its next number, recording the ruling, and becomes a notice in every bundle on the project.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"question":{"type":"string"},"rule":{"type":"string"},"amends":{"type":"string"}},"required":["question","rule"],"additionalProperties":false}`)}
	propose.Handle = func(ctx context.Context, raw json.RawMessage) (json.RawMessage, error) {
		var input struct {
			Question string `json:"question"`
			Rule     string `json:"rule"`
			Amends   string `json:"amends"`
		}
		if err := decodeInput(raw, &input); err != nil {
			return nil, err
		}
		next := func(content string) int { return charter.Parse(content).Next() }
		proposal, err := repository.ProposeCharterFor(ctx, agent, scope, input.Question, input.Rule, input.Amends, next, now())
		if err != nil {
			return refuse(err)
		}
		return encode(struct {
			Recorded bool   `json:"recorded"`
			Question string `json:"question"`
			Number   int    `json:"number"`
			Next     string `json:"next"`
		}{true, proposal.Question, proposal.Number, "The owner ratifies or declines the proposal; make it the attention of your status until they do."})
	}
	logAssumption := coreadapter.Tool{Name: LogAssumptionTool, Effect: coreadapter.ToolMemory,
		Description: "Log a judgment call made within stated intent, rather than ask or escalate it. text: the assumption as it should read in the drafted pull request description. workstreams: optional affected member workstream IDs; omit for a judgment applying to the whole workgroup.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"text":{"type":"string"},"workstreams":{"type":"array","minItems":1,"uniqueItems":true,"items":{"type":"string"}}},"required":["text"],"additionalProperties":false}`)}
	logAssumption.Handle = func(ctx context.Context, raw json.RawMessage) (json.RawMessage, error) {
		var input struct {
			Text        string                `json:"text"`
			Workstreams []config.WorkstreamID `json:"workstreams"`
		}
		if err := decodeInput(raw, &input); err != nil {
			return nil, err
		}
		if input.Workstreams != nil && len(input.Workstreams) == 0 {
			return encode(refusal{Reason: "workstreams must name at least one affected member, or be omitted for the whole workgroup"})
		}
		a, err := repository.LogAssumption(ctx, agent, scope, input.Text, now(), input.Workstreams...)
		if err != nil {
			return refuse(err)
		}
		return encode(struct {
			Recorded bool   `json:"recorded"`
			Logged   string `json:"logged"`
		}{true, a.ID})
	}
	return []coreadapter.Tool{answer, escalate, relay, amendmentTool(repository, agent, scope, now, true, nil), propose, logAssumption}
}

// refuse turns a refusal into the tool's result and passes other errors on.
func refuse(err error) (json.RawMessage, error) {
	var refused *trace.QuestionRefused
	if errors.As(err, &refused) {
		return encode(refusal{Reason: refused.Reason})
	}
	return nil, err
}

// encode writes a tool result without escaping the angle brackets of the
// citation forms.
func encode(v any) (json.RawMessage, error) {
	var b bytes.Buffer
	e := json.NewEncoder(&b)
	e.SetEscapeHTML(false)
	if err := e.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimSpace(b.Bytes()), nil
}

func decodeInput(raw json.RawMessage, into any) error {
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if err := d.Decode(into); err != nil {
		return fmt.Errorf("tool input: %w", err)
	}
	if err := d.Decode(new(any)); err != io.EOF {
		return errors.New("tool input must be one object")
	}
	return nil
}
