package kinds

import (
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/kpenfound/osmia/internal/skills"
)

// HillClimb is the kind of a workstream that improves one measured number: a
// ratified climb spec, an evaluator the owner approves, and attempts that Go
// keeps or discards by arithmetic.
const HillClimb = "hill-climb"

// ClimbSpecID is the trace document ID of the document a Hill Climb
// workstream's architect drafts, its committee debates and its owner
// ratifies. It occupies the spec slot of a ratification, and the kind has no
// plan.
const ClimbSpecID = "spec"

// ClimbSpecPath is the workstream path of the climb spec.
const ClimbSpecPath = "climb-spec.json"

// EvaluatorUnit is the ID of the one unit a Hill Climb workstream builds before
// it calibrates: the unit whose mason and reviewer write and judge the
// evaluator.
const EvaluatorUnit = "evaluator"

// Turn names a turn on which a kind loads the skills its table configures.
type Turn string

const (
	// TurnArchitect is an architect turn that drafts, replies to the
	// committee about, or amends the kind's ratified document.
	TurnArchitect Turn = "architect"
	// TurnMetricRound is a committee turn of the climb spec's round.
	TurnMetricRound Turn = "metric-round"
	// TurnEvaluatorMason is the mason turn of the evaluator unit.
	TurnEvaluatorMason Turn = "evaluator-mason"
	// TurnEvaluatorReviewer is the reviewer turn of the evaluator unit.
	TurnEvaluatorReviewer Turn = "evaluator-reviewer"
)

// Hill Climb settings defaults and bounds.
const (
	DefaultCalibrationRuns    = 3
	MinCalibrationRuns        = 2
	MaxCalibrationRuns        = 25
	DefaultRepeats            = 1
	MaxRepeats                = 25
	DefaultMeasurementTimeout = 15 * time.Minute
	MinMeasurementTimeout     = time.Minute
	MaxMeasurementTimeout     = 24 * time.Hour
)

// The keys of the `[kinds.hill-climb]` table.
const (
	skillsKey      = "skills"
	calibrationKey = "calibration_runs"
	repeatsKey     = "repeats"
	timeoutKey     = "measurement_timeout"
)

// HillClimbSettings is the `[kinds.hill-climb]` table with its defaults
// applied.
type HillClimbSettings struct {
	// Skills are the skill references loaded on the turns that write or judge
	// the evaluator, and on no other.
	Skills []string
	// CalibrationRuns is how many times calibration runs the evaluator.
	CalibrationRuns int
	// Repeats is the default k of a climb spec that names none.
	Repeats int
	// MeasurementTimeout bounds one run of the evaluator.
	MeasurementTimeout time.Duration
}

// ParseHillClimb reads a `[kinds.hill-climb]` table. An absent table, or an
// absent key, takes the default. An unknown key, a skill reference that does
// not parse or a value outside its bounds is refused with the key it is in.
func ParseHillClimb(table map[string]any) (HillClimbSettings, error) {
	s := HillClimbSettings{CalibrationRuns: DefaultCalibrationRuns, Repeats: DefaultRepeats, MeasurementTimeout: DefaultMeasurementTimeout}
	known := []string{skillsKey, calibrationKey, repeatsKey, timeoutKey}
	for _, key := range slices.Sorted(maps.Keys(table)) {
		if !slices.Contains(known, key) {
			return s, fmt.Errorf("unknown key %q; the hill-climb kind accepts %s", key, strings.Join(known, ", "))
		}
	}
	if v, ok := table[skillsKey]; ok {
		refs, err := stringList(v)
		if err != nil {
			return s, fmt.Errorf("%s: %w", skillsKey, err)
		}
		for i, ref := range refs {
			if _, err := skills.Parse(ref); err != nil {
				return s, fmt.Errorf("%s[%d]: %w", skillsKey, i, err)
			}
		}
		s.Skills = refs
	}
	var err error
	if s.CalibrationRuns, err = boundedInt(table, calibrationKey, DefaultCalibrationRuns, MinCalibrationRuns, MaxCalibrationRuns); err != nil {
		return s, err
	}
	if s.Repeats, err = boundedInt(table, repeatsKey, DefaultRepeats, 1, MaxRepeats); err != nil {
		return s, err
	}
	if v, ok := table[timeoutKey]; ok {
		text, isText := v.(string)
		d, perr := time.ParseDuration(text)
		if !isText || perr != nil || d < MinMeasurementTimeout || d > MaxMeasurementTimeout {
			return s, fmt.Errorf("%s must be a duration between %s and %s, such as \"%s\"", timeoutKey, MinMeasurementTimeout, MaxMeasurementTimeout, DefaultMeasurementTimeout)
		}
		s.MeasurementTimeout = d
	}
	return s, nil
}

func stringList(v any) ([]string, error) {
	var items []any
	switch list := v.(type) {
	case []string:
		return validRefs(list)
	case []any:
		items = list
	default:
		return nil, fmt.Errorf("must be a list of skill references")
	}
	out := make([]string, 0, len(items))
	for _, item := range items {
		text, ok := item.(string)
		if !ok {
			return nil, fmt.Errorf("must be a list of skill references")
		}
		out = append(out, text)
	}
	return validRefs(out)
}

func validRefs(refs []string) ([]string, error) {
	for _, ref := range refs {
		if strings.TrimSpace(ref) == "" {
			return nil, fmt.Errorf("a skill reference is empty")
		}
	}
	return slices.Clone(refs), nil
}

func boundedInt(table map[string]any, key string, def, lo, hi int) (int, error) {
	v, ok := table[key]
	if !ok {
		return def, nil
	}
	var n int
	switch x := v.(type) {
	case int:
		n = x
	case int64:
		n = int(x)
	case float64:
		if x != float64(int(x)) {
			return 0, fmt.Errorf("%s must be a whole number from %d to %d", key, lo, hi)
		}
		n = int(x)
	default:
		return 0, fmt.Errorf("%s must be a whole number from %d to %d", key, lo, hi)
	}
	if n < lo || n > hi {
		return 0, fmt.Errorf("%s must be a whole number from %d to %d", key, lo, hi)
	}
	return n, nil
}

var climbStates = []string{"evaluator", "calibrating", "awaiting-approval", "climbing", "concluding"}

var hillClimb = Kind{
	Name:    HillClimb,
	UseWhen: "the work is to improve one measured number, such as a run time, a size or a score, by repeated attempts against an evaluator the owner approves, in one existing project.",
	Flags:   Flags{},
	Documents: Documents{
		Drafted: []string{ClimbSpecID}, Debated: []string{ClimbSpecID}, Ratified: []string{ClimbSpecID},
		SpecPath: ClimbSpecPath,
	},
	Rounds: 1,
	Loop:   Loop{Name: "climb", States: climbStates},
	PullRequestSection: func(pr PullRequest) string { return climbSection(pr.Climb) },
	Progress: func(r Reading) Progress {
		return Progress{Panel: "climb", Summary: climbSummary(r.Climb), Spark: climbSpark(r.Climb)}
	},
	SkillTurns: []Turn{TurnArchitect, TurnMetricRound, TurnEvaluatorMason, TurnEvaluatorReviewer},
	ChiefTools: []string{"climb_steer", "climb_conclude"},
	Settings: Settings{
		Validate: func(table map[string]any) error {
			_, err := ParseHillClimb(table)
			return err
		},
		Skills: func(table map[string]any) []string {
			s, err := ParseHillClimb(table)
			if err != nil {
				return nil
			}
			return s.Skills
		},
	},
}

// Climb phases an attempt in flight can be in.
const (
	PhaseMason      = "mason"
	PhaseGuardrails = "guardrails"
	PhaseMeasuring  = "measuring"
	PhaseReviewing  = "reviewing"
)

// PhaseWords says in words where an attempt in flight is, as the summary line
// and the climb panel show it.
func PhaseWords(phase string, run, runs int) string {
	switch phase {
	case PhaseMason:
		return "mason working"
	case PhaseGuardrails:
		return "guardrails"
	case PhaseMeasuring:
		if runs > 1 {
			return fmt.Sprintf("measuring %d/%d", max(run, 1), runs)
		}
		return "measuring"
	case PhaseReviewing:
		return "reviewing"
	}
	return phase
}

// SignedPercent writes a percentage with its sign and one decimal, using the
// minus sign rather than a hyphen.
func SignedPercent(pct float64) string {
	switch {
	case pct < 0:
		return fmt.Sprintf("\u2212%.1f%%", -pct)
	case pct > 0:
		return fmt.Sprintf("+%.1f%%", pct)
	}
	return "0.0%"
}

// climbSummary is the one line a Hill Climb workstream shows in the workstream
// list and in osmia status, such as "14 attempts · attempt 15 measuring 2/3 ·
// best −18.4%". It is empty for a workstream that has no climb to read.
func climbSummary(c *ClimbReading) string {
	if c == nil {
		return ""
	}
	noun := "attempts"
	if c.Attempts == 1 {
		noun = "attempt"
	}
	parts := []string{fmt.Sprintf("%d %s", c.Attempts, noun)}
	if c.Running > 0 && c.Phase != "" {
		words := PhaseWords(c.Phase, c.Run, c.Runs)
		if c.Phase == PhaseMason {
			words = "working"
		}
		parts = append(parts, fmt.Sprintf("attempt %d %s", c.Running, words))
	}
	switch {
	case c.ChangePercent == nil:
	case *c.ChangePercent == 0:
		parts = append(parts, "no improvement yet")
	default:
		parts = append(parts, "best "+SignedPercent(*c.ChangePercent))
	}
	return strings.Join(parts, " \u00b7 ")
}

func climbSpark(c *ClimbReading) []float64 {
	if c == nil || len(c.Spark) < 2 {
		return nil
	}
	return slices.Clone(c.Spark)
}
