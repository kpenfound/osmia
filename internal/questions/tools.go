package questions

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/trace"
)

const (
	AskTool            = "ask"
	AnswerTool         = "answer"
	EscalateTool       = "escalate"
	RouteAmendmentTool = "route_amendment"
	ProposeCharterTool = "propose_charter"
)

// Reserved is the reason route_amendment and propose_charter return.
const Reserved = "reserved until amendments and standing rulings (M4)"

// Guidance tells the chief of staff what to do with a question. It belongs
// in the system prompt of every turn that may carry one.
const Guidance = "When a service event says a question is open, choose exactly once for it. " +
	"Call answer when the project context below or the workstream's spec and plan already settle it, citing what settles it: " + CitationForms + ". " +
	"Call escalate when answering would be a new decision, or when your answer would contradict an earlier ruling: never answer against a ruling, escalate instead. " +
	"Escalate several open questions that need the same decision as one batch. Never answer for the owner because time is passing; a question waits as long as it needs."

// ChiefTools names the chief of staff's question tools.
var ChiefTools = []string{AnswerTool, EscalateTool, RouteAmendmentTool, ProposeCharterTool}

type refusal struct {
	Recorded bool   `json:"recorded"`
	Reason   string `json:"reason"`
}

// Tools returns the question tools of the claimed turn scope names: ask for
// every role but the chief of staff, and answer, escalate, route_amendment
// and propose_charter for the chief of staff. A request the trace refuses is
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
		return json.Marshal(struct {
			Recorded bool   `json:"recorded"`
			Question string `json:"question"`
			Next     string `json:"next"`
		}{true, q.ID, "End your turn now. The answer arrives as your next turn on this thread."})
	}
	return []coreadapter.Tool{ask}, nil
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
					return json.Marshal(refusal{Reason: unresolved.Error()})
				}
				return nil, err
			}
		}
		ruling, err := repository.AnswerQuestion(ctx, agent, scope, input.Question, input.Text, input.Citations, now())
		if err != nil {
			return refuse(err)
		}
		return json.Marshal(struct {
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
		return json.Marshal(struct {
			Recorded  bool     `json:"recorded"`
			Batch     string   `json:"batch"`
			Questions []string `json:"questions"`
		}{true, batch, input.Questions})
	}
	tools := []coreadapter.Tool{answer, escalate}
	for _, reserved := range []struct{ name, description string }{
		{RouteAmendmentTool, "Route a question as an amendment to the sealed spec or plan. " + Reserved + "."},
		{ProposeCharterTool, "Propose a charter amendment for an answer that is a standing rule. " + Reserved + "."},
	} {
		tools = append(tools, coreadapter.Tool{Name: reserved.name, Effect: coreadapter.ToolMemory, Description: reserved.description,
			InputSchema: json.RawMessage(`{"type":"object"}`),
			Handle: func(context.Context, json.RawMessage) (json.RawMessage, error) {
				return json.Marshal(refusal{Reason: Reserved})
			}})
	}
	return tools
}

// refuse turns a refusal into the tool's result and passes other errors on.
func refuse(err error) (json.RawMessage, error) {
	var refused *trace.QuestionRefused
	if errors.As(err, &refused) {
		return json.Marshal(refusal{Reason: refused.Reason})
	}
	return nil, err
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
