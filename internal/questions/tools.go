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
)

// Guidance tells the chief of staff what to do with a question. It belongs
// in the system prompt of every turn that may carry one.
const Guidance = "When a service event says a question is open, choose exactly once for it. " +
	"Call answer when the project context below or the workstream's spec and plan already settle it, citing what settles it: " + CitationForms + ". " +
	"Call escalate when answering would be a new decision, or when your answer would contradict an earlier ruling: never answer against a ruling, escalate instead. " +
	"Call route_amendment when the honest answer changes the sealed spec or plan. " +
	"Escalate several open questions that need the same decision as one batch. Never answer for the owner because time is passing; a question waits as long as it needs. " +
	"When a service event says the owner ruled on an inbox entry, call relay_ruling exactly once for it, naming one of its questions: rephrase the ruling for the askers without changing what it decides, " +
	"and choose scope local when it matters only to them, or notify when it applies across the project. " +
	"When the owner's ruling is a standing rule for the project rather than a decision about this feature, also call propose_charter once for it with the rule as the charter should state it; " +
	"the owner ratifies or declines the proposal, so make it the attention of your status until they do."

// ChiefTools names the chief of staff's question tools.
var ChiefTools = []string{AnswerTool, EscalateTool, RelayRulingTool, RouteAmendmentTool, ProposeCharterTool}

type refusal struct {
	Recorded bool   `json:"recorded"`
	Reason   string `json:"reason"`
}

// Tools returns the question tools of the claimed turn scope names: ask for
// every role but the chief of staff, amend for masons and reviewers, and
// answer, escalate, relay_ruling, route_amendment and propose_charter for
// the chief of staff. A request the trace refuses is
// an ordinary result, {"recorded":false,"reason":...}, so the agent reads why.
func Tools(repository *trace.Repository, agent string, scope coreadapter.Scope, now func() time.Time) ([]coreadapter.Tool, error) {
	if repository == nil || now == nil {
		return nil, errors.New("question tools require a trace and a clock")
	}
	if scope.Role == trace.ChiefOfStaff {
		return chiefTools(repository, agent, scope, now), nil
	}
	ask := coreadapter.Tool{Name: AskTool, Effect: coreadapter.ToolMemory,
		Description: "Ask the chief of staff a question you cannot answer from your context. Say what you need decided and what it blocks. The question is recorded and your turn ends; end it as soon as this returns. The answer arrives as your next turn on this thread, however long it takes.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"question":{"type":"string"}},"required":["question"],"additionalProperties":false}`)}
	ask.Handle = func(ctx context.Context, raw json.RawMessage) (json.RawMessage, error) {
		var input struct {
			Question string `json:"question"`
		}
		if err := decodeInput(raw, &input); err != nil {
			return nil, err
		}
		q, err := repository.Ask(ctx, agent, scope, input.Question, now())
		if err != nil {
			return refuse(err)
		}
		return encode(struct {
			Recorded bool   `json:"recorded"`
			Question string `json:"question"`
			Next     string `json:"next"`
		}{true, q.ID, "End your turn now. The answer arrives as your next turn on this thread."})
	}
	if scope.Role == "mason" || scope.Role == "reviewer" {
		return []coreadapter.Tool{ask, amendmentTool(repository, agent, scope, now, false)}, nil
	}
	return []coreadapter.Tool{ask}, nil
}

func amendmentTool(repository *trace.Repository, agent string, scope coreadapter.Scope, now func() time.Time, routed bool) coreadapter.Tool {
	name := AmendTool
	if routed {
		name = RouteAmendmentTool
	}
	properties := `"citations":{"type":"array","items":{"type":"string"}},"change":{"type":"string"},"reason":{"type":"string"}`
	required := `"citations","change","reason"`
	if routed {
		properties = `"question":{"type":"string"},` + properties
		required = `"question",` + required
	}
	tool := coreadapter.Tool{Name: name, Effect: coreadapter.ToolMemory,
		Description: "File an amendment request against sealed spec#<n> and/or plan#<unit> citations. Give the proposed change and reason. The request is recorded and the requesting unit waits; end this turn when accepted.",
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
		if len(input.Citations) == 0 {
			return encode(refusal{Reason: "at least one spec#<n> or plan#<unit> citation is required"})
		}
		for _, c := range input.Citations {
			if _, ok := plan.ParseCitation(c); !ok && !strings.HasPrefix(c, "plan#") {
				return encode(refusal{Reason: "amendments cite only spec#<n> or plan#<unit>"})
			}
			if err := Resolve(ctx, repository, stream, c, now()); err != nil {
				var unresolved *Unresolved
				if errors.As(err, &unresolved) {
					return encode(refusal{Reason: unresolved.Error()})
				}
				return nil, err
			}
		}
		documents, err := trace.Read[trace.Document](repository, stream)
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
		a, err := repository.FileAmendment(ctx, agent, scope, trace.AmendmentRequest{QuestionID: input.Question, Citations: input.Citations, Change: input.Change, Reason: input.Reason, Seal: sealed.Seal, SealRevision: document.Revision, SpecHash: sealed.SpecHash}, now())
		if err != nil {
			return refuse(err)
		}
		return encode(struct {
			Recorded  bool   `json:"recorded"`
			Amendment string `json:"amendment"`
			Next      string `json:"next"`
		}{true, a.ID, "End your turn now. The request is with the chief of staff."})
	}
	return tool
}

func chiefTools(repository *trace.Repository, agent string, scope coreadapter.Scope, now func() time.Time) []coreadapter.Tool {
	stream := config.WorkstreamID(scope.Workstream)
	answer := coreadapter.Tool{Name: AnswerTool, Effect: coreadapter.ToolMemory,
		Description: "Answer an open question from the record. question: its number. text: the answer the asker receives. citations: at least one of " + CitationForms + "; each must exist. An answer that would contradict an earlier ruling must be escalated instead.",
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
		for _, c := range input.Citations {
			if err := Resolve(ctx, repository, stream, c, now()); err != nil {
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
		Description: "Send one or several open questions to the owner as one batch. questions: their numbers. rephrasing: the question as the owner should read it, naming the unit and criterion. blocked: what waits on the answer. options: the choices. recommendation: what you would decide. An escalated question can no longer be answered by you.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"questions":{"type":"array","items":{"type":"string"}},"rephrasing":{"type":"string"},"blocked":{"type":"string"},"options":{"type":"array","items":{"type":"string"}},"recommendation":{"type":"string"}},"required":["questions","rephrasing","blocked","options","recommendation"],"additionalProperties":false}`)}
	escalate.Handle = func(ctx context.Context, raw json.RawMessage) (json.RawMessage, error) {
		var input struct {
			Questions      []string `json:"questions"`
			Rephrasing     string   `json:"rephrasing"`
			Blocked        string   `json:"blocked"`
			Options        []string `json:"options"`
			Recommendation string   `json:"recommendation"`
		}
		if err := decodeInput(raw, &input); err != nil {
			return nil, err
		}
		batch, err := repository.EscalateQuestions(ctx, agent, scope, trace.EscalationRequest{Questions: input.Questions, Rephrasing: input.Rephrasing, Blocked: input.Blocked, Options: input.Options, Recommendation: input.Recommendation}, now())
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
		Description: "Propose the owner's ruling on a question as a standing charter rule, when it applies to the project beyond this feature. question: the number of a question the owner ruled on; one proposal per ruling. rule: the rule as the charter should state it, one line. " +
			"The owner ratifies or declines it. A ratified rule is appended to the charter as its next number, recording the ruling, and becomes a notice in every bundle on the project.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"question":{"type":"string"},"rule":{"type":"string"}},"required":["question","rule"],"additionalProperties":false}`)}
	propose.Handle = func(ctx context.Context, raw json.RawMessage) (json.RawMessage, error) {
		var input struct {
			Question string `json:"question"`
			Rule     string `json:"rule"`
		}
		if err := decodeInput(raw, &input); err != nil {
			return nil, err
		}
		next := func(content string) int { return charter.Parse(content).Next() }
		proposal, err := repository.ProposeCharter(ctx, agent, scope, input.Question, input.Rule, next, now())
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
	return []coreadapter.Tool{answer, escalate, relay, amendmentTool(repository, agent, scope, now, true), propose}
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
