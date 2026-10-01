package systemone

import (
	"encoding/json"
	"fmt"
	"math"
	"strconv"
)

type wireAnswer struct {
	Type          Kind               `json:"type"`
	Choice        *string            `json:"choice"`
	Score         *float64           `json:"score"`
	Noul          *float64           `json:"noul"`
	Probabilities map[string]float64 `json:"probabilities"`
	Confidence    *float64           `json:"confidence"`
}

type wireResponse struct {
	Model   string                `json:"model"`
	Answers map[string]wireAnswer `json:"answers"`
	Usage   *struct {
		InputTokens  *int     `json:"input_tokens"`
		OutputTokens *int     `json:"output_tokens"`
		Cost         *float64 `json:"cost"`
	} `json:"usage"`
}

// probabilityTolerance absorbs rounding in a reported distribution.
const probabilityTolerance = 0.02

func unit(v float64) bool { return !math.IsNaN(v) && v >= 0 && v <= 1 }

// decode parses a provider response and checks it answers exactly the
// questions of r, each in the shape its kind requires. An answer that fails
// any check makes the whole response unusable.
func decode(data []byte, r Request) (Response, error) {
	var w wireResponse
	if err := json.Unmarshal(data, &w); err != nil {
		return Response{}, fmt.Errorf("response is not JSON: %w", err)
	}
	if w.Model == "" || len(w.Model) > 128 {
		return Response{}, fmt.Errorf("response names no model")
	}
	if w.Usage == nil || w.Usage.InputTokens == nil || *w.Usage.InputTokens < 0 || w.Usage.OutputTokens != nil && *w.Usage.OutputTokens < 0 {
		return Response{}, fmt.Errorf("response reports no usage")
	}
	out := Response{Model: w.Model, Answers: map[string]Answer{}, Usage: Usage{InputTokens: *w.Usage.InputTokens}}
	if w.Usage.OutputTokens != nil {
		out.Usage.OutputTokens = *w.Usage.OutputTokens
	}
	if c := w.Usage.Cost; c != nil {
		if math.IsNaN(*c) || math.IsInf(*c, 0) || *c < 0 {
			return Response{}, fmt.Errorf("response reports an invalid cost")
		}
		out.Usage.CostUSD, out.Usage.CostKnown = *c, true
	}
	if len(w.Answers) != len(r.Questions) {
		return Response{}, fmt.Errorf("response answers %d questions, asked %d", len(w.Answers), len(r.Questions))
	}
	for id, q := range r.Questions {
		a, ok := w.Answers[id]
		if !ok {
			return Response{}, fmt.Errorf("response does not answer %s", id)
		}
		answer, err := checkAnswer(q, a)
		if err != nil {
			return Response{}, fmt.Errorf("answer %s: %w", id, err)
		}
		out.Answers[id] = answer
	}
	return out, nil
}

func checkAnswer(q Question, a wireAnswer) (Answer, error) {
	if a.Type != q.Kind {
		return Answer{}, fmt.Errorf("type %q does not match question type %q", a.Type, q.Kind)
	}
	switch q.Kind {
	case KindNoul:
		if a.Noul == nil || !unit(*a.Noul) {
			return Answer{}, fmt.Errorf("noul must be between 0 and 1")
		}
		return Answer{Kind: KindNoul, Noul: *a.Noul}, nil
	case KindChoice:
		keys := make([]string, len(q.Options))
		for i, o := range q.Options {
			keys[i] = o.Name
		}
		if err := distribution(a, keys); err != nil {
			return Answer{}, err
		}
		if a.Choice == nil {
			return Answer{}, fmt.Errorf("choice is missing")
		}
		top, ok := a.Probabilities[*a.Choice]
		if !ok {
			return Answer{}, fmt.Errorf("choice is not an offered option")
		}
		for _, p := range a.Probabilities {
			if p > top+probabilityTolerance {
				return Answer{}, fmt.Errorf("choice is not the most probable option")
			}
		}
		return Answer{Kind: KindChoice, Choice: *a.Choice, Probabilities: a.Probabilities, Confidence: *a.Confidence}, nil
	case KindScore:
		keys := make([]string, len(q.Levels))
		for i := range q.Levels {
			keys[i] = strconv.Itoa(i)
		}
		if err := distribution(a, keys); err != nil {
			return Answer{}, err
		}
		if a.Score == nil || math.IsNaN(*a.Score) || *a.Score < 0 || *a.Score > float64(len(q.Levels)-1) {
			return Answer{}, fmt.Errorf("score must lie within the levels")
		}
		return Answer{Kind: KindScore, Score: *a.Score, Probabilities: a.Probabilities, Confidence: *a.Confidence}, nil
	}
	return Answer{}, fmt.Errorf("unknown question type")
}

// distribution checks a's probabilities cover exactly keys and sum to one,
// and that its confidence is a proportion.
func distribution(a wireAnswer, keys []string) error {
	if a.Confidence == nil || !unit(*a.Confidence) {
		return fmt.Errorf("confidence must be between 0 and 1")
	}
	if len(a.Probabilities) != len(keys) {
		return fmt.Errorf("probabilities must cover every option")
	}
	sum := 0.0
	for _, k := range keys {
		p, ok := a.Probabilities[k]
		if !ok || !unit(p) {
			return fmt.Errorf("probabilities must cover every option with values between 0 and 1")
		}
		sum += p
	}
	if math.Abs(sum-1) > probabilityTolerance {
		return fmt.Errorf("probabilities must sum to 1")
	}
	return nil
}
