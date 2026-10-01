package thread

import (
	"context"
	"fmt"
	"strings"
	"unicode"

	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/jev"
	"github.com/kpenfound/osmia/internal/systemone"
	"github.com/kpenfound/osmia/internal/trace"
)

// classificationTask is the Jev judgment that classifies a clean mason
// response. Bump classificationVersion whenever its questions, their
// interpretation or the thresholds below change.
const (
	classificationTask    = "mason-classification"
	classificationVersion = 1
)

// The probabilities a judgment's answers must reach to be accepted. A
// gave_up class contests the unit, so it needs more than the others, and
// every class but unclear needs a passage of the response supporting it.
const (
	classThreshold  = 0.7
	gaveUpThreshold = 0.9
	spanThreshold   = 0.5
)

// maxSpans bounds the passages offered as evidence; a longer response offers
// its last ones. maxSpanRunes bounds one passage, as it bounds a
// classifier's evidence.
const (
	maxSpans     = 64
	maxSpanRunes = 500
)

// noSpan is the evidence option that no passage supports a class.
const noSpan = "none"

// classOptions are the classes a judgment chooses from, as the model sees
// them.
var classOptions = []systemone.Option{
	{Name: "asked_in_prose", Description: "The agent asks a question, or asks for a decision or clarification, in its text instead of calling ask."},
	{Name: "claims_done", Description: "The agent states that the work is finished or that its acceptance holds."},
	{Name: "gave_up", Description: "The agent states that it cannot complete or continue the work, and stops."},
	{Name: "unclear", Description: "None of these: the agent reports progress or next steps, or the message is ambiguous."},
}

// evidenceQuestions ask, for each class but unclear, which passage of the
// response supports it.
var evidenceQuestions = map[string]string{
	"asked_in_prose": "Which passage of the message asks a question or asks for a decision or clarification?",
	"claims_done":    "Which passage of the message states that the work is finished or that its acceptance holds?",
	"gave_up":        "Which passage of the message states that the agent cannot complete or continue the work?",
}

func evidenceQuestion(class string) string { return "evidence-" + class }

// judgeClassification asks Jev to classify response's clean mason turn and
// reports whether it replaced the rule's classification. Any fallback leaves
// the classification as it was.
func (r Runner) judgeClassification(ctx context.Context, req trace.TurnRequest, scope coreadapter.Scope, response *trace.TurnResponse) bool {
	c := response.Classification
	spans := responseSpans(c.Window)
	if len(spans) == 0 {
		return false
	}
	state := map[string]any{"message": c.Window}
	if len(c.ToolCounts) > 0 {
		state["tool_calls"] = c.ToolCounts
	}
	request := systemone.Request{State: state, Questions: map[string]systemone.Question{
		"class": systemone.Choice("The state is the final message of a coding agent's turn on a unit of work. "+
			"The agent should have ended the turn by calling a tool: ask to put a question, or done when the work is finished. "+
			"It called neither. Which describes how the message ends?", classOptions...),
	}}
	options := make([]systemone.Option, 0, len(spans)+1)
	for i, span := range spans {
		options = append(options, systemone.Option{Name: spanOption(i), Description: span})
	}
	options = append(options, systemone.Option{Name: noSpan, Description: "No passage does."})
	for class, question := range evidenceQuestions {
		request.Questions[evidenceQuestion(class)] = systemone.Choice(question, options...)
	}
	decision := r.Jev.Evaluate(ctx, r.Store, jev.Judgment{
		Scope:   scope,
		Cause:   req.ID,
		Depth:   req.Depth,
		Task:    classificationTask,
		Version: classificationVersion,
		Sources: []jev.Source{{Kind: "turn-request", ID: req.ID, Revision: req.Revision}},
		Request: request,
		Accept: func(resp systemone.Response) string {
			_, _, reason := interpretClassification(resp, spans)
			return reason
		},
	})
	if decision.Outcome != jev.Accepted {
		return false
	}
	class, evidence, _ := interpretClassification(*decision.Response, spans)
	c.Class, c.Evidence, c.By, c.Judgment = class, evidence, trace.ClassifiedByJev, decision.ID
	return true
}

func spanOption(i int) string { return fmt.Sprintf("span-%d", i+1) }

// interpretClassification returns the class a response chose and its
// evidence: the supporting passage, or for unclear the class's probability.
// It returns why the answers are not accepted instead when the class falls
// below its threshold or no passage supports it.
func interpretClassification(resp systemone.Response, spans []string) (class, evidence, reason string) {
	a := resp.Answers["class"]
	class, p := a.Choice, a.Probabilities[a.Choice]
	threshold := classThreshold
	if class == "gave_up" {
		threshold = gaveUpThreshold
	}
	if p < threshold {
		return "", "", fmt.Sprintf("class %s has probability %.2f, below %.2f", class, p, threshold)
	}
	if class == "unclear" {
		return class, fmt.Sprintf("Jev found no question, completion claim or surrender (probability %.2f)", p), ""
	}
	e := resp.Answers[evidenceQuestion(class)]
	for i, span := range spans {
		if e.Choice == spanOption(i) && e.Probabilities[e.Choice] >= spanThreshold {
			return class, span, ""
		}
	}
	return "", "", fmt.Sprintf("no passage supports class %s with probability %.2f", class, spanThreshold)
}

// responseSpans splits a response into the passages offered as evidence:
// its sentences and lines, each at most maxSpanRunes, the last maxSpans of
// them.
func responseSpans(window string) []string {
	var spans []string
	add := func(s []rune) {
		for len(s) > 0 {
			n := min(len(s), maxSpanRunes)
			if span := strings.TrimSpace(string(s[:n])); span != "" {
				spans = append(spans, span)
			}
			s = s[n:]
		}
	}
	for line := range strings.SplitSeq(window, "\n") {
		runes := []rune(line)
		start := 0
		for i, r := range runes {
			if (r == '.' || r == '?' || r == '!') && (i+1 == len(runes) || unicode.IsSpace(runes[i+1])) {
				add(runes[start : i+1])
				start = i + 1
			}
		}
		add(runes[start:])
	}
	if len(spans) > maxSpans {
		spans = spans[len(spans)-maxSpans:]
	}
	return spans
}
