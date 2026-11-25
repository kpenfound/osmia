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
	PullRequestSection: func(PullRequest) string {
		return ""
	},
	Progress: func(Units) Progress {
		return Progress{Panel: "climb"}
	},
	SkillTurns: []Turn{TurnArchitect, TurnMetricRound, TurnEvaluatorMason, TurnEvaluatorReviewer},
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
