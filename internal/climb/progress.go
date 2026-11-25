package climb

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/kinds"
	"github.com/kpenfound/osmia/internal/trace"
)

// RunningPath is the workstream document that says which attempt is in flight
// and where it is.
const RunningPath = trace.ClimbRunningPath

// Running says which attempt is in flight and in which phase. The climbing
// loop records it as each attempt starts and moves between phases, and clears
// it, with an empty Phase, once the attempt's terminal record is written.
type Running struct {
	Attempt    int    `json:"attempt"`
	Hypothesis string `json:"hypothesis,omitempty"`
	// Phase is one of kinds.PhaseMason, PhaseGuardrails, PhaseMeasuring and
	// PhaseReviewing. Run is the measurement run in progress and Runs how many
	// the attempt repeats.
	Phase string `json:"phase"`
	Run   int    `json:"run,omitempty"`
	Runs  int    `json:"runs,omitempty"`
	// StartedAt is when the attempt started, and ClimbStartedAt when the climb
	// began its first attempt.
	StartedAt      time.Time `json:"started_at"`
	ClimbStartedAt time.Time `json:"climb_started_at,omitempty"`
}

// Active reports whether an attempt is in flight.
func (r Running) Active() bool { return r.Attempt > 0 && r.Phase != "" }

// RecordRunning stores the running state as a new revision of the workstream
// document climb/running.json. Recording the state it already holds is a
// no-op, which makes a retry after a restart safe.
func RecordRunning(ctx context.Context, r *trace.Repository, stream config.WorkstreamID, run Running, at time.Time) error {
	data, err := json.MarshalIndent(run, "", "  ")
	if err != nil {
		return err
	}
	content := string(data) + "\n"
	latest, _, err := latestAttempt(r, stream, RunningPath)
	if err != nil {
		return err
	}
	if latest.Revision > 0 && latest.Content == content {
		return nil
	}
	doc := trace.Document{
		Header: trace.Header{Schema: "osmia.trace.document", Version: trace.Version, ID: "climb-running", Revision: latest.Revision + 1, Project: r.Project(), Workstream: stream, At: at.UTC(), Actor: Actor, Cause: fmt.Sprintf("climb-running-%d", latest.Revision+1)},
		Path:   RunningPath, Content: content,
	}
	return r.RecordDocuments(ctx, []trace.Document{doc})
}

// ReadRunning returns the recorded running state, and whether one is
// recorded.
func ReadRunning(r *trace.Repository, stream config.WorkstreamID) (Running, bool, error) {
	var run Running
	found, err := readJSONDocument(r, stream, RunningPath, &run)
	return run, found, err
}

func readJSONDocument(r *trace.Repository, stream config.WorkstreamID, path string, into any) (bool, error) {
	doc, found, err := latestAttempt(r, stream, path)
	if err != nil || !found {
		return false, err
	}
	if err := json.Unmarshal([]byte(doc.Content), into); err != nil {
		return false, fmt.Errorf("%s: %w", path, err)
	}
	return true, nil
}

// Meter is how much of one climb budget is used. A Max of zero means the climb
// spec sets no limit.
type Meter struct {
	Used float64 `json:"used"`
	Max  float64 `json:"max,omitempty"`
}

// Budgets are the meters of the climb: the attempts, the cost in US dollars and
// the wall-clock seconds the climb has used, and the attempts since the last
// improvement against the plateau rule.
type Budgets struct {
	Attempts         Meter `json:"attempts"`
	CostUSD          Meter `json:"cost_usd"`
	WallClockSeconds Meter `json:"wall_clock_seconds"`
	SinceImprovement Meter `json:"since_improvement"`
}

// Class is how the chart draws an attempt: a kept attempt, one that was
// measured and discarded, or one that failed before it had a score.
type Class string

const (
	ClassKept      Class = "kept"
	ClassDiscarded Class = "discarded"
	ClassFailed    Class = "failed"
)

// Point is one attempt on the score chart. Y is the attempt's score, or for
// an attempt without one the best it started from. Best is the best score
// once the attempt was recorded.
type Point struct {
	Attempt int       `json:"attempt"`
	Class   Class     `json:"class"`
	Cause   Cause     `json:"cause,omitempty"`
	Scored  bool      `json:"scored"`
	Y       float64   `json:"y"`
	Best    float64   `json:"best"`
	At      time.Time `json:"at"`
}

// RunningView is the attempt in flight, as the climb panel shows it.
type RunningView struct {
	Attempt    int       `json:"attempt"`
	Hypothesis string    `json:"hypothesis,omitempty"`
	Phase      string    `json:"phase"`
	Words      string    `json:"words"`
	Run        int       `json:"run,omitempty"`
	Runs       int       `json:"runs,omitempty"`
	StartedAt  time.Time `json:"started_at"`
}

// Progress is the climb panel's data and, through Summary and Spark, the
// workstream list's and osmia status's.
type Progress struct {
	Goal      string `json:"goal"`
	Evaluator string `json:"evaluator"`
	Metric    string `json:"metric"`
	// Direction is the way the metric improves, higher or lower, and Unit the
	// unit calibration recorded for it, empty before calibration.
	Direction      Direction `json:"direction"`
	Unit           string    `json:"unit,omitempty"`
	MinImprovement float64   `json:"min_improvement"`
	Repeats        int       `json:"repeats"`
	// Baseline is the score calibration established and Best the best score
	// so far; either is nil until it exists. BestAttempt is the attempt that
	// set the best, zero for the baseline. Change and ChangePercent are the
	// best's distance from the baseline.
	Baseline      *float64 `json:"baseline,omitempty"`
	Best          *float64 `json:"best,omitempty"`
	BestAttempt   int      `json:"best_attempt"`
	Change        *float64 `json:"change,omitempty"`
	ChangePercent *float64 `json:"change_percent,omitempty"`
	Target        *float64 `json:"target,omitempty"`
	// NoiseBand is the run-to-run spread calibration measured.
	NoiseBand *float64     `json:"noise_band,omitempty"`
	Budgets   Budgets      `json:"budgets"`
	Running   *RunningView `json:"running,omitempty"`
	// Epoch numbers the calibration the baseline, the best and Points belong
	// to, and SpecRevision is the climb spec revision it was made under. An
	// amendment of a field the calibration depends on starts a new epoch with
	// a new baseline: the Earlier attempts before it ran against another
	// measurement, so their scores are neither drawn in Points nor compared
	// with the best.
	Epoch        int     `json:"epoch"`
	SpecRevision int     `json:"spec_revision,omitempty"`
	Earlier      int     `json:"earlier_attempts,omitempty"`
	Points       []Point `json:"points"`
	// Summary is the one line the list and osmia status show, and Spark the
	// points of the sparkline, oriented so that a rising line improves.
	Summary string    `json:"summary"`
	Spark   []float64 `json:"spark,omitempty"`
}

// ProgressInput is everything Progress is read from.
type ProgressInput struct {
	Spec        Spec
	Calibration *Calibration
	Running     *Running
	Attempts    []Attempt
	// Epoch is the calibration epoch the climb stands in, and SpecRevision the
	// climb spec revision that calibration was made under.
	Epoch        int
	SpecRevision int
	// CostUSD is the known spend of the workstream so far.
	CostUSD float64
	// Extension is what the owner's budget extensions add to the climb spec's
	// hard limits, as climb.ReadBudget records them.
	Extension Totals
	Now       time.Time
}

// BuildProgress reads the climb's progress. It is plain arithmetic over what
// the climb recorded.
func BuildProgress(in ProgressInput) Progress {
	spec := in.Spec
	p := Progress{
		Goal: spec.Goal, Evaluator: spec.Evaluator, Metric: spec.Metric.Name, Direction: spec.Metric.Direction,
		MinImprovement: spec.Metric.MinImprovement, Repeats: spec.Repeats, Target: spec.Stop.Target, Points: []Point{},
		Epoch: max(in.Epoch, 1), SpecRevision: in.SpecRevision,
	}
	current := InEpoch(in.Attempts, in.Epoch)
	p.Earlier = len(in.Attempts) - len(current)
	if in.Calibration != nil && (in.Calibration.Verdict == Calibrated || in.Calibration.Verdict == "") {
		c := in.Calibration
		baseline, spread := c.Baseline, c.Spread
		p.Baseline, p.NoiseBand = &baseline, &spread
		p.Unit = c.Units[spec.Metric.Name]
	}
	if p.Baseline == nil && len(current) > 0 {
		first := current[0].Best
		p.Baseline = &first
	}
	best := p.Baseline
	var sinceImprovement int
	spark := []float64{}
	if p.Baseline != nil {
		spark = append(spark, p.orient(*p.Baseline))
	}
	for _, a := range current {
		before := a.Best
		pt := Point{Attempt: a.Number, At: a.FinishedAt, Cause: a.Cause, Y: before, Class: ClassFailed}
		if a.Scores != nil {
			pt.Scored, pt.Y, pt.Class = true, a.Scores.Primary, ClassDiscarded
		}
		if a.Outcome == OutcomeKept && a.Scores != nil {
			pt.Class = ClassKept
			score := a.Scores.Primary
			best = &score
			p.BestAttempt = a.Number
			sinceImprovement = 0
		} else {
			sinceImprovement++
		}
		if best != nil {
			pt.Best = *best
			spark = append(spark, p.orient(*best))
		}
		p.Points = append(p.Points, pt)
	}
	if best != nil {
		b := *best
		p.Best = &b
	}
	if p.Baseline != nil && p.Best != nil {
		change := *p.Best - *p.Baseline
		p.Change = &change
		if *p.Baseline != 0 {
			pct := change / absolute(*p.Baseline) * 100
			p.ChangePercent = &pct
		}
	}
	p.Budgets = in.budgets(sinceImprovement)
	reading := kinds.ClimbReading{Attempts: len(in.Attempts), ChangePercent: p.ChangePercent, Spark: spark}
	if r := in.Running; r != nil && r.Active() {
		p.Running = &RunningView{Attempt: r.Attempt, Hypothesis: r.Hypothesis, Phase: r.Phase, Run: r.Run, Runs: r.Runs, StartedAt: r.StartedAt, Words: kinds.PhaseWords(r.Phase, r.Run, r.Runs)}
		reading.Running, reading.Phase, reading.Run, reading.Runs = r.Attempt, r.Phase, r.Run, r.Runs
	}
	if kind, ok := kinds.Lookup(kinds.HillClimb); ok {
		progress := kind.Progress(kinds.Reading{Climb: &reading})
		p.Summary, p.Spark = progress.Summary, progress.Spark
	}
	return p
}

func (p Progress) orient(score float64) float64 {
	if p.Direction == Lower {
		return -score
	}
	return score
}

func (in ProgressInput) budgets(sinceImprovement int) Budgets {
	stop := in.Spec.Stop
	limits := stop.Limits(in.Extension)
	return Budgets{
		Attempts:         Meter{Used: float64(len(in.Attempts)), Max: float64(limits.Attempts)},
		CostUSD:          Meter{Used: in.CostUSD, Max: limits.CostUSD},
		WallClockSeconds: Meter{Used: in.elapsed().Seconds(), Max: limits.WallClock.Seconds()},
		SinceImprovement: Meter{Used: float64(sinceImprovement), Max: float64(stop.Plateau)},
	}
}

func (in ProgressInput) elapsed() time.Duration {
	var start, end time.Time
	if len(in.Attempts) > 0 {
		start, end = in.Attempts[0].StartedAt, in.Attempts[len(in.Attempts)-1].FinishedAt
	}
	if r := in.Running; r != nil && r.Active() {
		if !r.ClimbStartedAt.IsZero() {
			start = r.ClimbStartedAt
		} else if start.IsZero() {
			start = r.StartedAt
		}
		end = in.Now
	}
	if start.IsZero() || end.Before(start) {
		return 0
	}
	return end.Sub(start)
}
