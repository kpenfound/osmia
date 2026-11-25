package climb

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/kinds"
	"github.com/kpenfound/osmia/internal/trace"
)

// SpecVersion is the only climb spec schema version.
const SpecVersion = 1

// SpecDocument is the trace document ID of the climb spec and SpecPath its
// workstream path. The climb spec occupies the spec slot of a ratification
// and the kind has no plan.
const (
	SpecDocument = kinds.ClimbSpecID
	SpecPath     = kinds.ClimbSpecPath
)

// MinWallClock is the shortest wall-clock stop rule a climb spec may set.
const MinWallClock = time.Minute

var (
	evaluatorName = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_-]{0,63}$`)
	metricName    = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,63}$`)
)

// Spec is the climb spec: what a Hill Climb workstream improves, how it is
// measured, what must not break, what is frozen and when the climb stops.
// The architect drafts it in place of a spec and a plan, the committee debates
// the metric once and the owner ratifies it.
type Spec struct {
	Version int `json:"version"`
	// Goal says in prose what the climb improves and why.
	Goal string `json:"goal"`
	// Evaluator is the name of the function in the dev module that measures a
	// run. Each climb may create its own.
	Evaluator  string        `json:"evaluator"`
	Metric     MetricSpec    `json:"metric"`
	Repeats    int           `json:"repeats,omitempty"`
	Guardrails GuardrailSpec `json:"guardrails"`
	Protected  Protected     `json:"protected"`
	Stop       StopRules     `json:"stop"`
}

// MetricSpec is the primary metric: its name in the evaluator's output, the
// direction that is better and the absolute improvement, in the metric's
// units, an attempt must reach over the best to be kept.
type MetricSpec struct {
	Name           string    `json:"name"`
	Direction      Direction `json:"direction"`
	MinImprovement float64   `json:"min_improvement"`
}

// GuardrailSpec is what an attempt must not break: project checks that must
// still pass and limits on secondary metrics of the evaluator's output.
type GuardrailSpec struct {
	Checks []string         `json:"checks,omitempty"`
	Limits []SecondaryLimit `json:"limits,omitempty"`
}

// SecondaryLimit bounds a secondary metric. Direction is the way the
// evaluator reports the metric as better, which a run must agree with.
type SecondaryLimit struct {
	Metric    string    `json:"metric"`
	Direction Direction `json:"direction"`
	Min       *float64  `json:"min,omitempty"`
	Max       *float64  `json:"max,omitempty"`
}

// Protected extends the default protected paths. The defaults are the dev
// module, dagger.toml, dagger.lock and the declared fixtures; Fixtures declares
// the files and directories the evaluator reads, and Add names any further
// path to freeze.
type Protected struct {
	Fixtures []string `json:"fixtures,omitempty"`
	Add      []string `json:"add,omitempty"`
}

// StopRules end a climb. MaxAttempts is required and, like MaxCostUSD and
// MaxWallClock, a hard limit. Plateau counts attempts since the last
// improvement and Target is a score that ends the climb once reached; both are
// soft.
type StopRules struct {
	MaxAttempts  int      `json:"max_attempts"`
	MaxCostUSD   float64  `json:"max_cost_usd,omitempty"`
	MaxWallClock string   `json:"max_wall_clock,omitempty"`
	Plateau      int      `json:"plateau,omitempty"`
	Target       *float64 `json:"target,omitempty"`
}

// Defaults are the settings a climb spec that omits a value takes.
type Defaults struct {
	// Repeats is the k of a climb spec that sets none.
	Repeats int
}

// DefaultsOf returns the defaults of the `[kinds.hill-climb]` settings.
func DefaultsOf(s kinds.HillClimbSettings) Defaults { return Defaults{Repeats: s.Repeats} }

// Check parses a climb spec and applies its defaults, and returns every
// problem it finds so the architect can correct them all at once. The spec it
// returns is meaningful only without problems.
func Check(data []byte, d Defaults) (Spec, []string) {
	var s Spec
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&s); err != nil {
		return Spec{}, []string{"the climb spec is not valid JSON of the climb spec schema: " + err.Error()}
	}
	if err := dec.Decode(new(any)); err != io.EOF {
		return Spec{}, []string{"the climb spec must be one JSON object"}
	}
	s = s.WithDefaults(d)
	return s, s.Problems()
}

// Parse parses a climb spec and applies its defaults. An invalid climb spec is
// refused with every problem listed.
func Parse(data []byte, d Defaults) (Spec, error) {
	s, problems := Check(data, d)
	if len(problems) > 0 {
		return Spec{}, errors.New(strings.Join(problems, "; "))
	}
	return s, nil
}

// WithDefaults fills the values a climb spec may omit: the repeats, one when
// the defaults name none, and the empty lists of a spec that holds none.
func (s Spec) WithDefaults(d Defaults) Spec {
	if s.Repeats == 0 {
		s.Repeats = d.Repeats
	}
	if s.Repeats == 0 {
		s.Repeats = kinds.DefaultRepeats
	}
	if s.Version == 0 {
		s.Version = SpecVersion
	}
	return s
}

// Encode returns the file content of a valid climb spec.
func Encode(s Spec) ([]byte, error) {
	if problems := s.Problems(); len(problems) > 0 {
		return nil, errors.New(strings.Join(problems, "; "))
	}
	var b bytes.Buffer
	e := json.NewEncoder(&b)
	e.SetEscapeHTML(false)
	e.SetIndent("", "  ")
	if err := e.Encode(s); err != nil {
		return nil, err
	}
	return b.Bytes(), nil
}

func finite(v float64) bool { return !math.IsNaN(v) && !math.IsInf(v, 0) }

// Problems lists everything wrong with the climb spec: a required field that
// is missing and a value that is not one the climb can run with.
func (s Spec) Problems() []string {
	var out []string
	add := func(format string, args ...any) { out = append(out, fmt.Sprintf(format, args...)) }
	if s.Version != SpecVersion {
		add("version must be %d", SpecVersion)
	}
	if strings.TrimSpace(s.Goal) == "" {
		add("goal is required: say in prose what the climb improves and why")
	}
	switch {
	case s.Evaluator == "":
		add("evaluator is required: name the function in the dev module that measures a run")
	case !evaluatorName.MatchString(s.Evaluator):
		add("evaluator %q is not a function name: use letters, digits, '-' or '_', starting with a letter", s.Evaluator)
	}
	switch {
	case s.Metric.Name == "":
		add("metric.name is required: name the primary metric as the evaluator reports it")
	case !metricName.MatchString(s.Metric.Name):
		add("metric.name %q is not a metric name: use letters, digits, '.', '-' or '_'", s.Metric.Name)
	}
	if !s.Metric.Direction.Valid() {
		add("metric.direction must be \"higher\" or \"lower\", not %q", string(s.Metric.Direction))
	}
	if !finite(s.Metric.MinImprovement) || !(s.Metric.MinImprovement > 0) {
		add("metric.min_improvement must be a number above zero, in the metric's units: it is the absolute improvement an attempt must reach to be kept")
	}
	if s.Repeats < 1 || s.Repeats > kinds.MaxRepeats {
		add("repeats must be a whole number from 1 to %d", kinds.MaxRepeats)
	}
	out = append(out, s.guardrailProblems()...)
	out = append(out, s.protectedProblems()...)
	out = append(out, s.Stop.problems()...)
	return out
}

func (s Spec) guardrailProblems() []string {
	var out []string
	add := func(format string, args ...any) { out = append(out, fmt.Sprintf(format, args...)) }
	seenChecks := map[string]bool{}
	for _, c := range s.Guardrails.Checks {
		switch {
		case strings.TrimSpace(c) == "":
			add("guardrails.checks holds an empty check name")
		case seenChecks[c]:
			add("guardrails.checks names %q twice", c)
		}
		seenChecks[c] = true
	}
	seen := map[string]bool{}
	for i, l := range s.Guardrails.Limits {
		switch {
		case l.Metric == "" || !metricName.MatchString(l.Metric):
			add("guardrails.limits[%d].metric %q is not a metric name", i, l.Metric)
		case l.Metric == s.Metric.Name:
			add("guardrails.limits[%d] limits the primary metric %q: the minimum improvement already governs it, so limit a secondary metric", i, l.Metric)
		case seen[l.Metric]:
			add("guardrails.limits[%d] limits %q a second time", i, l.Metric)
		}
		seen[l.Metric] = true
		if !l.Direction.Valid() {
			add("guardrails.limits[%d].direction must be \"higher\" or \"lower\", not %q", i, string(l.Direction))
		}
		switch {
		case l.Min == nil && l.Max == nil:
			add("guardrails.limits[%d] needs a min or a max", i)
		case l.Min != nil && !finite(*l.Min), l.Max != nil && !finite(*l.Max):
			add("guardrails.limits[%d] has a bound that is not a number", i)
		case l.Min != nil && l.Max != nil && *l.Min > *l.Max:
			add("guardrails.limits[%d] has a min above its max", i)
		}
	}
	return out
}

func (s Spec) protectedProblems() []string {
	var out []string
	for name, paths := range map[string][]string{"fixtures": s.Protected.Fixtures, "add": s.Protected.Add} {
		for _, p := range paths {
			if _, err := CleanPath(p); err != nil {
				out = append(out, fmt.Sprintf("protected.%s: %v", name, err))
			}
		}
	}
	slices.Sort(out)
	return out
}

func (r StopRules) problems() []string {
	var out []string
	add := func(format string, args ...any) { out = append(out, fmt.Sprintf(format, args...)) }
	if r.MaxAttempts < 1 {
		add("stop.max_attempts is required and must be at least 1: the climb needs a hard limit on its attempts")
	}
	if r.MaxCostUSD != 0 && (!finite(r.MaxCostUSD) || r.MaxCostUSD < 0) {
		add("stop.max_cost_usd must be a number above zero")
	}
	if r.MaxWallClock != "" {
		if d, err := time.ParseDuration(r.MaxWallClock); err != nil || d < MinWallClock {
			add("stop.max_wall_clock must be a duration of at least %s, such as \"6h\"", MinWallClock)
		}
	}
	if r.Plateau < 0 {
		add("stop.plateau must be a whole number of attempts without improvement, at least 1")
	}
	if r.Target != nil && !finite(*r.Target) {
		add("stop.target must be a number")
	}
	return out
}

// Expectations returns the metrics every run must report: the primary metric
// and each secondary metric a guardrail limits, with the directions the climb
// spec fixes.
func (s Spec) Expectations() Expectations {
	e := Expectations{Primary: Expectation{Name: s.Metric.Name, Direction: s.Metric.Direction}}
	for _, l := range s.Guardrails.Limits {
		e.Secondary = append(e.Secondary, Expectation{Name: l.Metric, Direction: l.Direction})
	}
	return e
}

// Limits returns the secondary limits the keep/discard decision holds a run to.
func (s Spec) Limits() []Limit {
	var out []Limit
	for _, l := range s.Guardrails.Limits {
		out = append(out, Limit{Metric: l.Metric, Min: l.Min, Max: l.Max})
	}
	return out
}

// ProtectedPaths returns the paths the climb freezes, from the project's
// dagger.toml: the dev module, dagger.toml, dagger.lock, the declared fixtures
// and the paths the climb spec adds.
func (s Spec) ProtectedPaths(daggerToml []byte) ([]string, error) {
	dev, err := DevModuleSource(daggerToml)
	if err != nil {
		return nil, err
	}
	return ProtectedPaths(dev, s.Protected.Fixtures, s.Protected.Add)
}

// WallClock returns the wall-clock stop rule, and whether the climb spec sets
// one.
func (s Spec) WallClock() (time.Duration, bool) {
	if s.Stop.MaxWallClock == "" {
		return 0, false
	}
	d, err := time.ParseDuration(s.Stop.MaxWallClock)
	return d, err == nil
}

// SealedSpec returns the climb spec of the given recorded revision of the
// workstream's spec slot, as the building loop reads it. Defaults are applied.
func SealedSpec(repository *trace.Repository, stream config.WorkstreamID, revision int, d Defaults) (Spec, trace.Document, error) {
	docs, err := trace.Read[trace.Document](repository, stream)
	if err != nil {
		return Spec{}, trace.Document{}, err
	}
	i := slices.IndexFunc(docs, func(doc trace.Document) bool { return doc.ID == SpecDocument && doc.Revision == revision })
	if i < 0 {
		return Spec{}, trace.Document{}, fmt.Errorf("workstream %s has no recorded climb spec revision %d", stream, revision)
	}
	s, err := Parse([]byte(docs[i].Content), d)
	if err != nil {
		return Spec{}, docs[i], fmt.Errorf("the sealed climb spec does not parse: %w", err)
	}
	return s, docs[i], nil
}
