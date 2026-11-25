package climb

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"slices"
	"strings"
	"time"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/trace"
)

// Outcome is how an attempt ended.
type Outcome string

const (
	// OutcomeKept is an attempt Go kept and its reviewer approved. It became
	// the new best.
	OutcomeKept Outcome = "kept"
	// OutcomeDiscarded is every other end of an attempt; its Cause says why.
	OutcomeDiscarded Outcome = "discarded"
)

// Actor is the provenance of attempt records.
var Actor = trace.Actor{Kind: "service", ID: "climb"}

// DiffSummary says what an attempt changed.
type DiffSummary struct {
	Files   []string `json:"files"`
	Summary string   `json:"summary,omitempty"`
}

// Scores are the primary-metric values of a measured attempt: the median Go
// compared, the value of each run, and the median of every required metric.
type Scores struct {
	Primary float64            `json:"primary"`
	Runs    []float64          `json:"runs,omitempty"`
	Metrics map[string]float64 `json:"metrics,omitempty"`
}

// Attempt is the terminal record of one attempt, stored as
// climb/attempts/<number>.json in the workstream's trace.
type Attempt struct {
	Number int `json:"number"`
	// Parent is the commit of the best the attempt started from, and Best its
	// primary score.
	Parent string  `json:"parent"`
	Best   float64 `json:"best"`
	// Candidate is the commit the mason's work was captured as, and Branch
	// the branch that keeps a discarded attempt inspectable. Either is empty
	// when it does not exist.
	Candidate string `json:"candidate,omitempty"`
	Branch    string `json:"branch,omitempty"`
	// Commit is the squashed commit a kept attempt landed on the climb branch.
	Commit string `json:"commit,omitempty"`
	// Hypothesis, ChangeSummary and Expected are the mason's report of the
	// idea, what it changed and the effect it expects.
	Hypothesis    string      `json:"hypothesis"`
	ChangeSummary string      `json:"change_summary,omitempty"`
	Expected      string      `json:"expected_effect,omitempty"`
	Diff          DiffSummary `json:"diff"`
	// Scores is set when the attempt was measured validly. Change is the
	// score minus the best and Improvement the same distance in the metric's
	// direction, positive when better.
	Scores      *Scores  `json:"scores,omitempty"`
	Change      *float64 `json:"change,omitempty"`
	ChangePct   *float64 `json:"change_percent,omitempty"`
	Improvement *float64 `json:"improvement,omitempty"`
	Outcome     Outcome  `json:"outcome"`
	Cause       Cause    `json:"cause,omitempty"`
	Reason      string   `json:"reason,omitempty"`
	// ReviewNotes are the reviewer's notes on a rejected attempt.
	ReviewNotes string `json:"review_notes,omitempty"`
	// Guardrails records the guardrail checks' result.
	Guardrails *Guardrails `json:"guardrails,omitempty"`
	// Measurement holds the full output and parsed values of every run.
	Measurement *Measurement `json:"measurement,omitempty"`
	StartedAt   time.Time    `json:"started_at"`
	FinishedAt  time.Time    `json:"finished_at"`
}

// Guardrails records whether the guardrail checks passed.
type Guardrails struct {
	Green  bool   `json:"green"`
	Detail string `json:"detail,omitempty"`
}

// Measured fills the attempt's scores and changes from a valid measurement and
// the decision made on it, and keeps the measurement itself.
func (a *Attempt) Measured(m Measurement, d Decision) {
	a.Measurement = &m
	if !m.Valid {
		return
	}
	a.Scores = &Scores{Primary: m.Score, Runs: m.Scores, Metrics: m.Metrics}
	change, improvement := d.Change, d.Improvement
	a.Change, a.Improvement = &change, &improvement
	if a.Best != 0 {
		pct := change / absolute(a.Best) * 100
		a.ChangePct = &pct
	}
}

func absolute(v float64) float64 {
	if v < 0 {
		return -v
	}
	return v
}

// Validate reports a record that could not be a faithful account of an
// attempt.
func (a Attempt) Validate() error {
	switch {
	case a.Number < 1:
		return fmt.Errorf("attempt number %d is not positive", a.Number)
	case strings.TrimSpace(a.Parent) == "":
		return errors.New("an attempt records the commit it started from")
	case a.Outcome != OutcomeKept && a.Outcome != OutcomeDiscarded:
		return fmt.Errorf("outcome %q is neither kept nor discarded", a.Outcome)
	case a.Outcome == OutcomeDiscarded && a.Cause == "":
		return errors.New("a discarded attempt records why")
	case a.Outcome == OutcomeKept && (a.Scores == nil || a.Measurement == nil || !a.Measurement.Valid || a.Commit == ""):
		return errors.New("a kept attempt records a valid measurement and the commit it landed")
	case a.Outcome == OutcomeKept && a.Cause != "":
		return errors.New("a kept attempt has no discard cause")
	}
	return nil
}

// RecordAttempt stores the attempt as the workstream document
// climb/attempts/<number>.json. The record is immutable: recording the same
// attempt again is a no-op, which makes a retry after a restart safe, and
// recording a different one under the same number is trace.ErrConflict.
func RecordAttempt(ctx context.Context, r *trace.Repository, stream config.WorkstreamID, a Attempt) error {
	doc, recorded, err := AttemptDocument(r, stream, a)
	if err != nil || recorded {
		return err
	}
	return r.RecordDocuments(ctx, []trace.Document{doc})
}

// AttemptDocument returns the document that records the attempt, for a caller
// that records it in the same commit as the move that ends the attempt. It
// reports recorded when the same record is already there, and refuses a
// different record under the same number with trace.ErrConflict.
func AttemptDocument(r *trace.Repository, stream config.WorkstreamID, a Attempt) (doc trace.Document, recorded bool, err error) {
	if err := a.Validate(); err != nil {
		return trace.Document{}, false, err
	}
	data, err := json.MarshalIndent(a, "", "  ")
	if err != nil {
		return trace.Document{}, false, err
	}
	content := string(data) + "\n"
	path := trace.ClimbAttemptPath(a.Number)
	if existing, found, err := latestAttempt(r, stream, path); err != nil {
		return trace.Document{}, false, err
	} else if found {
		if existing.Content == content {
			return existing, true, nil
		}
		return trace.Document{}, false, fmt.Errorf("%w: attempt %d is already recorded differently", trace.ErrConflict, a.Number)
	}
	at := a.FinishedAt
	if at.IsZero() {
		at = time.Now()
	}
	return trace.Document{
		Header: trace.Header{Schema: "osmia.trace.document", Version: trace.Version, ID: trace.ClimbAttemptID(a.Number), Revision: 1, Project: r.Project(), Workstream: stream, At: at.UTC(), Actor: Actor, Cause: fmt.Sprintf("climb-attempt-%d", a.Number)},
		Path:   path, Content: content,
	}, false, nil
}

// ReadAttempt returns attempt n, and whether it is recorded.
func ReadAttempt(r *trace.Repository, stream config.WorkstreamID, n int) (Attempt, bool, error) {
	doc, found, err := latestAttempt(r, stream, trace.ClimbAttemptPath(n))
	if err != nil || !found {
		return Attempt{}, false, err
	}
	a, err := decodeAttempt(doc)
	return a, err == nil, err
}

// ReadAttempts returns every recorded attempt of the workstream, in attempt
// order.
func ReadAttempts(r *trace.Repository, stream config.WorkstreamID) ([]Attempt, error) {
	docs, err := trace.Read[trace.Document](r, stream)
	if err != nil {
		return nil, err
	}
	latest := map[string]trace.Document{}
	for _, d := range docs {
		if strings.HasPrefix(d.Path, trace.ClimbAttemptsDir+"/") && d.Revision >= latest[d.Path].Revision {
			latest[d.Path] = d
		}
	}
	out := make([]Attempt, 0, len(latest))
	for _, d := range latest {
		a, err := decodeAttempt(d)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	slices.SortFunc(out, func(a, b Attempt) int { return a.Number - b.Number })
	return out, nil
}

func latestAttempt(r *trace.Repository, stream config.WorkstreamID, path string) (trace.Document, bool, error) {
	docs, err := trace.Read[trace.Document](r, stream)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return trace.Document{}, false, nil
		}
		return trace.Document{}, false, err
	}
	var latest trace.Document
	for _, d := range docs {
		if d.Path == path && d.Revision >= latest.Revision {
			latest = d
		}
	}
	return latest, latest.Revision > 0, nil
}

func decodeAttempt(d trace.Document) (Attempt, error) {
	var a Attempt
	if err := json.Unmarshal([]byte(d.Content), &a); err != nil {
		return Attempt{}, fmt.Errorf("%s: %w", d.Path, err)
	}
	if d.Path != trace.ClimbAttemptPath(a.Number) {
		return Attempt{}, fmt.Errorf("%s holds attempt %d", d.Path, a.Number)
	}
	return a, nil
}
