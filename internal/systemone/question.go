// Package systemone is a client for TypeSafe's System One API, which answers
// typed questions about a state: a Choice selects one supplied option, a Score
// rates against supplied levels, and a Noul is the probability that a
// proposition holds. It serves any TypeSafe-compatible endpoint, such as
// TypeSafe's own or OpenRouter's, and depends only on the standard library.
package systemone

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
)

// Kind is a question type.
type Kind string

const (
	KindChoice Kind = "choice"
	KindScore  Kind = "score"
	KindNoul   Kind = "noul"
)

// The API's request limits.
const (
	MaxChoiceOptions = 255
	MaxScoreLevels   = 10
)

// Option is one choice. Description is optional rubric text.
type Option struct {
	Name        string
	Description any
}

// Question is one typed question. Instructions and descriptions are strings
// or JSON values; a structured value can hold the question in one field and
// the data it refers to in others.
type Question struct {
	Kind         Kind
	Instructions any
	// Options are a Choice's options, in the order the model sees them.
	Options []Option
	// Levels are a Score's ordered level descriptions, lowest first.
	Levels []any
	// Yes and No optionally describe what each Noul answer means.
	Yes, No any
}

// Choice asks the model to select one of options.
func Choice(instructions any, options ...Option) Question {
	return Question{Kind: KindChoice, Instructions: instructions, Options: options}
}

// Score asks the model to rate the state on levels, lowest first.
func Score(instructions any, levels ...any) Question {
	return Question{Kind: KindScore, Instructions: instructions, Levels: levels}
}

// Noul asks the model for the probability that a proposition holds.
func Noul(instructions any) Question {
	return Question{Kind: KindNoul, Instructions: instructions}
}

func present(v any) bool {
	if v == nil {
		return false
	}
	if s, ok := v.(string); ok {
		return s != ""
	}
	return true
}

func (q Question) validate() error {
	if !present(q.Instructions) {
		return errors.New("instructions are required")
	}
	switch q.Kind {
	case KindChoice:
		if len(q.Options) < 2 || len(q.Options) > MaxChoiceOptions {
			return fmt.Errorf("a choice needs 2 to %d options", MaxChoiceOptions)
		}
		seen := map[string]bool{}
		for _, o := range q.Options {
			if o.Name == "" || seen[o.Name] {
				return errors.New("choice options need distinct non-empty names")
			}
			seen[o.Name] = true
		}
	case KindScore:
		if len(q.Levels) < 2 || len(q.Levels) > MaxScoreLevels {
			return fmt.Errorf("a score needs 2 to %d levels", MaxScoreLevels)
		}
		for _, l := range q.Levels {
			if !present(l) {
				return errors.New("score levels need descriptions")
			}
		}
	case KindNoul:
	default:
		return fmt.Errorf("unknown question kind %q", q.Kind)
	}
	if q.Kind != KindChoice && len(q.Options) > 0 || q.Kind != KindScore && len(q.Levels) > 0 || q.Kind != KindNoul && (q.Yes != nil || q.No != nil) {
		return errors.New("criteria do not match the question kind")
	}
	return nil
}

// MarshalJSON writes the API's wire form. A Choice's criteria keep the
// options' order.
func (q Question) MarshalJSON() ([]byte, error) {
	instructions, err := json.Marshal(q.Instructions)
	if err != nil {
		return nil, err
	}
	var criteria []byte
	switch q.Kind {
	case KindChoice:
		var b bytes.Buffer
		b.WriteByte('{')
		for i, o := range q.Options {
			if i > 0 {
				b.WriteByte(',')
			}
			name, _ := json.Marshal(o.Name)
			description, err := json.Marshal(o.Description)
			if err != nil {
				return nil, err
			}
			b.Write(name)
			b.WriteByte(':')
			b.Write(description)
		}
		b.WriteByte('}')
		criteria = b.Bytes()
	case KindScore:
		if criteria, err = json.Marshal(q.Levels); err != nil {
			return nil, err
		}
	case KindNoul:
		if q.Yes != nil || q.No != nil {
			c := map[string]any{}
			if q.Yes != nil {
				c["true"] = q.Yes
			}
			if q.No != nil {
				c["false"] = q.No
			}
			if criteria, err = json.Marshal(c); err != nil {
				return nil, err
			}
		}
	}
	return json.Marshal(struct {
		Type         Kind            `json:"type"`
		Instructions json.RawMessage `json:"instructions"`
		Criteria     json.RawMessage `json:"criteria,omitempty"`
	}{q.Kind, instructions, criteria})
}

// Request is a state and the questions asked of it, keyed by an ID the caller
// chooses. Every question is evaluated independently against the same state.
type Request struct {
	// State is a string or a JSON value.
	State     any
	Questions map[string]Question
}

// Validate checks the request against the API's limits, so a request that
// would be refused is never sent.
func (r Request) Validate() error {
	if !present(r.State) {
		return errors.New("state is required")
	}
	if len(r.Questions) == 0 {
		return errors.New("at least one question is required")
	}
	for id, q := range r.Questions {
		if id == "" {
			return errors.New("question IDs must not be empty")
		}
		if err := q.validate(); err != nil {
			return fmt.Errorf("question %s: %w", id, err)
		}
	}
	return nil
}

// Body is the JSON request body the API receives when model is asked r.
func (r Request) Body(model string) ([]byte, error) {
	return json.Marshal(struct {
		State     any                 `json:"state"`
		Model     string              `json:"model"`
		Questions map[string]Question `json:"questions"`
	}{r.State, model, r.Questions})
}

// Answer is one validated answer, in the API's own field names.
// Probabilities map each Choice option, or each Score level index as a
// decimal string, to its probability.
type Answer struct {
	Kind          Kind               `json:"type"`
	Choice        string             `json:"choice,omitempty"`
	Score         float64            `json:"score,omitempty"`
	Noul          float64            `json:"noul,omitempty"`
	Probabilities map[string]float64 `json:"probabilities,omitempty"`
	// Confidence is derived from the answer's distribution by the API. It
	// is not a verified probability of being correct.
	Confidence float64 `json:"confidence,omitempty"`
}

// Usage is what one request consumed. CostKnown says the endpoint reported
// CostUSD, as gateways such as OpenRouter may.
type Usage struct {
	InputTokens  int     `json:"input_tokens"`
	OutputTokens int     `json:"output_tokens"`
	CostUSD      float64 `json:"cost_usd,omitempty"`
	CostKnown    bool    `json:"cost_known,omitempty"`
}

// Response is a validated answer to every question of a request. Model is
// the versioned model that answered, which may differ from an alias asked for.
type Response struct {
	Model   string            `json:"model"`
	Answers map[string]Answer `json:"answers"`
	Usage   Usage             `json:"usage"`
}
