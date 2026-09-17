package shed

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/kb"
	"github.com/kpenfound/osmia/internal/plan"
	"github.com/kpenfound/osmia/internal/questions"
	"github.com/kpenfound/osmia/internal/trace"
)

const (
	ObjectTool  = "object"
	ConcedeTool = "concede"
	// EntityCitation prefixes the citation of an entity of the entity map.
	EntityCitation = trace.EntitiesPath + "#"
	planCitation   = "plan#"
	// WholeSpec and WholePlan name a whole document as the part an objection
	// concerns.
	WholeSpec = "spec"
	WholePlan = "plan"
)

// CitationForms lists what an objection may cite, for prompts and refusals.
const CitationForms = "charter#<n> (a charter rule), spec#<n> (an acceptance criterion of the spec), plan#<unit> (a unit of the plan), kb/<subsystem>.md (a knowledge-base file) or " + EntityCitation + "<entity> (an entity of the entity map)"

// PartForms lists the parts an objection may concern.
const PartForms = "spec#<n> (an acceptance criterion), plan#<unit> (a unit), spec (the spec as a whole) or plan (the plan as a whole)"

// Turn is one member's turn of a round: what its tools validate against and
// where they keep what the member contributed.
type Turn struct {
	Repository *trace.Repository
	Stream     config.WorkstreamID
	// Record names the round, the member, the pinned revision and the turn;
	// the tools add the contributions.
	Record Record
	// Spec and Plan are the pinned revisions the member reads.
	Spec plan.Spec
	Plan plan.Plan
	// Earlier holds the workstream's records of earlier rounds.
	Earlier []Record
	Now     func() time.Time
	// Save keeps the turn's contributions so far. It is called with every
	// accepted contribution, before the tool reports success.
	Save func(Record) error
}

// Invalid is why a contribution is refused back to the member.
type Invalid struct{ Reason string }

func (e *Invalid) Error() string { return e.Reason }

func invalid(format string, args ...any) error {
	return &Invalid{Reason: fmt.Sprintf(format, args...)}
}

type refusal struct {
	Recorded bool   `json:"recorded"`
	Reason   string `json:"reason"`
}

// Tools returns the object and concede tools of one committee turn. A
// contribution that is invalid is an ordinary result,
// {"recorded":false,"reason":...}, so the member reads why within the turn.
func Tools(t Turn) ([]coreadapter.Tool, error) {
	if t.Repository == nil || t.Now == nil || t.Save == nil {
		return nil, errors.New("shed tools require a trace, a clock and a place to keep contributions")
	}
	t.Record.Version = Version
	t.Record.Objections, t.Record.Concessions = nil, nil
	if err := t.Record.check(); err != nil {
		return nil, err
	}
	var mu sync.Mutex
	kinds := make([]string, len(Kinds))
	for i, k := range Kinds {
		kinds[i] = string(k)
	}
	schema, err := json.Marshal(map[string]any{"type": "object", "additionalProperties": false, "required": []string{"kind", "part", "argument", "citations"},
		"properties": map[string]any{"kind": map[string]any{"type": "string", "enum": kinds}, "part": map[string]any{"type": "string"}, "argument": map[string]any{"type": "string"},
			"citations": map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "minItems": 1}}})
	if err != nil {
		return nil, err
	}
	object := coreadapter.Tool{Name: ObjectTool, Effect: coreadapter.ToolMemory, InputSchema: schema,
		Description: "Object to one part of the spec or the plan. kind: charter (the part violates a charter rule; a veto), fit (the plan does not realise the handed design or works against a recorded decision; advice), " +
			"size (a unit addresses too much and must be split) or proof (the plan names no proof that can show a criterion holds). part: " + PartForms + "; a size objection names a unit and a proof objection a criterion. " +
			"argument: why. citations: at least one of " + CitationForms + "; each must exist, and a charter objection must cite the charter rule."}
	object.Handle = func(ctx context.Context, raw json.RawMessage) (json.RawMessage, error) {
		var input struct {
			Kind      Kind     `json:"kind"`
			Part      string   `json:"part"`
			Argument  string   `json:"argument"`
			Citations []string `json:"citations"`
		}
		if err := decodeInput(raw, &input); err != nil {
			return nil, err
		}
		mu.Lock()
		defer mu.Unlock()
		o := Objection{ID: ObjectionID(t.Record.Round, t.Record.Member, len(t.Record.Objections)+1), Kind: input.Kind, Part: input.Part, Argument: input.Argument, Citations: input.Citations}
		if err := t.checkObjection(ctx, o); err != nil {
			return refuse(err)
		}
		next := t.Record
		next.Objections = append(slices.Clone(next.Objections), o)
		if err := t.Save(next); err != nil {
			return nil, err
		}
		t.Record = next
		return encode(struct {
			Recorded  bool   `json:"recorded"`
			Objection string `json:"objection"`
		}{true, o.ID})
	}
	concede := coreadapter.Tool{Name: ConcedeTool, Effect: coreadapter.ToolMemory,
		Description: "Withdraw or settle one of your own objections that still stands. objection: its ID. reason: why it no longer stands, such as the architect's reply or a later revision.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"objection":{"type":"string"},"reason":{"type":"string"}},"required":["objection","reason"],"additionalProperties":false}`)}
	concede.Handle = func(_ context.Context, raw json.RawMessage) (json.RawMessage, error) {
		var input Concession
		if err := decodeInput(raw, &input); err != nil {
			return nil, err
		}
		mu.Lock()
		defer mu.Unlock()
		if err := t.checkConcession(input); err != nil {
			return refuse(err)
		}
		next := t.Record
		next.Concessions = append(slices.Clone(next.Concessions), input)
		if err := t.Save(next); err != nil {
			return nil, err
		}
		t.Record = next
		return encode(struct {
			Recorded  bool   `json:"recorded"`
			Objection string `json:"objection"`
		}{true, input.Objection})
	}
	return []coreadapter.Tool{object, concede}, nil
}

// checkObjection returns *Invalid for an objection the round refuses.
func (t Turn) checkObjection(ctx context.Context, o Objection) error {
	if !slices.Contains(Kinds, o.Kind) {
		return invalid("kind %q is not one of charter, fit, size or proof", o.Kind)
	}
	if strings.TrimSpace(o.Argument) == "" {
		return invalid("an objection requires an argument")
	}
	if err := t.checkPart(o.Kind, o.Part); err != nil {
		return err
	}
	if len(o.Citations) == 0 {
		return invalid("an objection requires at least one citation: %s", CitationForms)
	}
	for _, c := range o.Citations {
		if err := t.Resolve(ctx, c); err != nil {
			return err
		}
	}
	if o.Kind == Charter && !slices.ContainsFunc(o.Citations, questions.IsCharter) {
		return invalid("a charter objection must cite the charter rule the part violates, as charter#<n>")
	}
	return nil
}

// checkPart requires part to name the pinned spec or plan, or a criterion or
// unit of them; a size objection concerns a unit and a proof objection a
// criterion.
func (t Turn) checkPart(kind Kind, part string) error {
	_, criterion := plan.ParseCitation(part)
	unit := strings.HasPrefix(part, planCitation)
	switch {
	case kind == Size && !unit:
		return invalid("a size objection concerns a unit: part must be plan#<unit>")
	case kind == Proof && !criterion:
		return invalid("a proof objection concerns a criterion: part must be spec#<n>")
	case part == WholeSpec || part == WholePlan:
		return nil
	case criterion || unit:
		if err := t.pinned(part); err != nil {
			return invalid("part %q names nothing in the pinned revision: %s", part, err.(*Invalid).Reason)
		}
		return nil
	}
	return invalid("part %q is not one of %s", part, PartForms)
}

// pinned resolves a spec#<n> or plan#<unit> reference against the pinned
// revision.
func (t Turn) pinned(reference string) error {
	if n, ok := plan.ParseCitation(reference); ok {
		if _, ok := t.Spec.Criterion(n); !ok {
			return invalid("spec.md revision %d has no acceptance criterion numbered %d exactly once", t.Record.Revision.Spec, n)
		}
		return nil
	}
	id := strings.TrimPrefix(reference, planCitation)
	if _, ok := t.Plan.Unit(id); !ok {
		return invalid("plan.json revision %d has no unit %s exactly once", t.Record.Revision.Plan, id)
	}
	return nil
}

// Resolve checks that citation names something that exists: a criterion or a
// unit of the pinned spec or plan, a rule of the latest charter, a
// knowledge-base file or an entity of the recorded entity map. It returns
// *Invalid for a citation that names nothing, and any other error for a trace
// that cannot be read.
func (t Turn) Resolve(ctx context.Context, citation string) error {
	_, criterion := plan.ParseCitation(citation)
	switch {
	case criterion || strings.HasPrefix(citation, planCitation):
		if err := t.pinned(citation); err != nil {
			return invalid("citation %q does not resolve: %s", citation, err.(*Invalid).Reason)
		}
		return nil
	case strings.HasPrefix(citation, EntityCitation):
		entities, err := kb.Load(t.Repository)
		if err != nil {
			return err
		}
		name := strings.TrimPrefix(citation, EntityCitation)
		if _, ok := entities.Lookup(name); !ok || name == "" {
			return invalid("citation %q does not resolve: the entity map has no entity %q", citation, name)
		}
		return nil
	case questions.IsCharter(citation) || questions.IsProse(citation):
		err := questions.Resolve(ctx, t.Repository, t.Stream, citation, t.Now())
		var unresolved *questions.Unresolved
		if errors.As(err, &unresolved) {
			return invalid("%s", unresolved.Error())
		}
		return err
	}
	return invalid("citation %q is not one of %s", citation, CitationForms)
}

// checkConcession requires the objection to be the member's own, made in an
// earlier round or earlier in this turn, and still standing.
func (t Turn) checkConcession(c Concession) error {
	if strings.TrimSpace(c.Reason) == "" {
		return invalid("a concession requires a reason")
	}
	records := append(slices.Clone(t.Earlier), t.Record)
	for _, r := range records {
		for _, o := range r.Objections {
			if o.ID == c.Objection && r.Member != t.Record.Member {
				return invalid("objection %s is %s's; you may concede only your own", c.Objection, r.Member)
			}
		}
	}
	made := slices.ContainsFunc(records, func(r Record) bool {
		return slices.ContainsFunc(r.Objections, func(o Objection) bool { return o.ID == c.Objection })
	})
	if !made {
		return invalid("no objection %s is recorded", c.Objection)
	}
	// The turn in progress has accepted nothing yet, so what stands is the
	// dissent of the earlier rounds and this turn's own objections, less
	// what this turn already conceded.
	stands := slices.ContainsFunc(OpenDissent(t.Earlier), func(d Dissent) bool { return d.ID == c.Objection }) ||
		slices.ContainsFunc(t.Record.Objections, func(o Objection) bool { return o.ID == c.Objection })
	if !stands || slices.ContainsFunc(t.Record.Concessions, func(old Concession) bool { return old.Objection == c.Objection }) {
		return invalid("objection %s no longer stands", c.Objection)
	}
	return nil
}

// refuse turns an invalid contribution into the tool's result and passes
// other errors on.
func refuse(err error) (json.RawMessage, error) {
	var refused *Invalid
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
