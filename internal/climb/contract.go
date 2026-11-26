package climb

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// SchemaVersion is the evaluator contract version this build reads. The
// evaluator unit's task states the same version; changing the contract means
// changing both together and bumping this number.
const SchemaVersion = 1

// Direction names which way of a metric is better.
type Direction string

const (
	// Higher means a larger value is better.
	Higher Direction = "higher"
	// Lower means a smaller value is better.
	Lower Direction = "lower"
)

// Valid reports whether d is one of the two directions.
func (d Direction) Valid() bool { return d == Higher || d == Lower }

// Improvement returns how much better value is than reference in the
// direction d: positive when better, negative when worse.
func (d Direction) Improvement(value, reference float64) float64 {
	if d == Lower {
		return reference - value
	}
	return value - reference
}

// Metric is one measured value the evaluator reports.
type Metric struct {
	Value     float64   `json:"value"`
	Unit      string    `json:"unit"`
	Direction Direction `json:"direction"`
}

// Output is a conforming evaluator result. Valid false means the run is
// invalid, not that it scored badly, and Reason then says why.
type Output struct {
	Schema  int               `json:"schema"`
	Valid   bool              `json:"valid"`
	Reason  string            `json:"reason"`
	Metrics map[string]Metric `json:"metrics"`
}

// ParseOutput reads the evaluator's return value. The value is one JSON
// object carrying schema 1, valid, reason and metrics, where every metric is
// {value, unit, direction}. A JSON-encoded string that holds such an object is
// read as the object it holds. Anything else is an error that names what is
// wrong, and the caller records the run as failed.
func ParseOutput(raw string) (Output, error) {
	text := strings.TrimSpace(raw)
	if text == "" {
		return Output{}, errors.New("the evaluator returned no output")
	}
	if text[0] == '"' {
		var inner string
		if err := json.Unmarshal([]byte(text), &inner); err != nil {
			return Output{}, fmt.Errorf("the evaluator output is not valid JSON: %v", err)
		}
		text = strings.TrimSpace(inner)
	}
	var top map[string]json.RawMessage
	decoder := json.NewDecoder(strings.NewReader(text))
	if err := decoder.Decode(&top); err != nil {
		return Output{}, fmt.Errorf("the evaluator output is not a JSON object: %v", err)
	}
	if decoder.More() {
		return Output{}, errors.New("the evaluator output has data after the JSON object")
	}
	if top == nil {
		return Output{}, errors.New("the evaluator output is not a JSON object")
	}
	var out Output
	schema, err := field(top, "schema")
	if err != nil {
		return Output{}, err
	}
	if string(schema) != "1" {
		return Output{}, fmt.Errorf("unsupported schema %s: this runner reads schema %d", schema, SchemaVersion)
	}
	out.Schema = SchemaVersion
	if err := decodeField(top, "valid", &out.Valid, '#'); err != nil {
		return Output{}, err
	}
	if err := decodeField(top, "reason", &out.Reason, '"'); err != nil {
		return Output{}, err
	}
	metrics, err := field(top, "metrics")
	if err != nil {
		return Output{}, err
	}
	var entries map[string]json.RawMessage
	if err := json.Unmarshal(metrics, &entries); err != nil || entries == nil {
		return Output{}, errors.New(`"metrics" must be an object of name → {value, unit, direction}`)
	}
	out.Metrics = make(map[string]Metric, len(entries))
	for name, entry := range entries {
		if name == "" {
			return Output{}, errors.New("a metric has an empty name")
		}
		m, err := parseMetric(entry)
		if err != nil {
			return Output{}, fmt.Errorf("metric %q: %w", name, err)
		}
		out.Metrics[name] = m
	}
	return out, nil
}

func parseMetric(raw json.RawMessage) (Metric, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil || fields == nil {
		return Metric{}, errors.New("is not an object of {value, unit, direction}")
	}
	var m Metric
	if err := decodeField(fields, "value", &m.Value, '0'); err != nil {
		return Metric{}, err
	}
	if err := decodeField(fields, "unit", &m.Unit, '"'); err != nil {
		return Metric{}, err
	}
	var direction string
	if err := decodeField(fields, "direction", &direction, '"'); err != nil {
		return Metric{}, err
	}
	m.Direction = Direction(direction)
	if !m.Direction.Valid() {
		return Metric{}, fmt.Errorf(`direction %q must be "higher" or "lower"`, direction)
	}
	return m, nil
}

func field(fields map[string]json.RawMessage, name string) (json.RawMessage, error) {
	raw, ok := fields[name]
	if !ok || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return nil, fmt.Errorf("the %q field is missing", name)
	}
	return bytes.TrimSpace(raw), nil
}

// decodeField decodes the named field into target, which must be JSON of the
// kind the marker names: '"' for a string, '0' for a number and '#' for a
// boolean. Loose conversions that encoding/json allows are not accepted.
func decodeField(fields map[string]json.RawMessage, name string, target any, marker byte) error {
	raw, err := field(fields, name)
	if err != nil {
		return err
	}
	first := raw[0]
	ok := false
	switch marker {
	case '"':
		ok = first == '"'
	case '0':
		ok = first == '-' || first >= '0' && first <= '9'
	case '#':
		ok = first == 't' || first == 'f'
	}
	if !ok {
		return fmt.Errorf("the %q field has the wrong type", name)
	}
	if err := json.Unmarshal(raw, target); err != nil {
		return fmt.Errorf("the %q field is invalid: %v", name, err)
	}
	return nil
}
