package climb

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/trace"
)

// MaxNote bounds a steering note.
const MaxNote = 2000

// Note is one steering note: words the chief of staff leaves for the climb's
// next attempts.
type Note struct {
	Number int       `json:"number"`
	Text   string    `json:"text"`
	By     string    `json:"by"`
	At     time.Time `json:"at"`
}

// Steering is climb/steering.json, the notes in the order they were left.
type Steering struct {
	Notes []Note `json:"notes"`
}

// ReadSteering returns the steering notes left so far.
func ReadSteering(r *trace.Repository, stream config.WorkstreamID) (Steering, error) {
	v, _, _, err := readRecord[Steering](r, stream, trace.ClimbSteeringID)
	return v, err
}

// Steer leaves a steering note for the next attempt. A retry of the same
// words by the same actor in the same revision is not recorded twice.
func Steer(ctx context.Context, r *trace.Repository, stream config.WorkstreamID, text, by string, at time.Time) (Note, error) {
	text = strings.TrimSpace(text)
	switch {
	case text == "":
		return Note{}, errors.New("a steering note needs words: say what the next attempt should try or avoid")
	case len(text) > MaxNote:
		return Note{}, fmt.Errorf("a steering note is at most %d characters", MaxNote)
	}
	for range 3 {
		s, doc, _, err := readRecord[Steering](r, stream, trace.ClimbSteeringID)
		if err != nil {
			return Note{}, err
		}
		note := Note{Number: len(s.Notes) + 1, Text: text, By: by, At: at.UTC()}
		s.Notes = append(s.Notes, note)
		next, err := jsonDocument(r, stream, trace.ClimbSteeringID, trace.ClimbSteeringPath, doc.Revision+1, at, fmt.Sprintf("climb-steering-%d", note.Number), s)
		if err != nil {
			return Note{}, err
		}
		if err := r.RecordDocuments(ctx, []trace.Document{next}); err != nil {
			if errors.Is(err, trace.ErrConflict) {
				continue
			}
			return Note{}, err
		}
		return note, nil
	}
	return Note{}, trace.ErrConflict
}

// Extension is a budget extension the owner granted: more attempts, more
// cost, more wall-clock time, or any of them.
type Extension struct {
	Number    int       `json:"number"`
	Attempts  int       `json:"attempts,omitempty"`
	CostUSD   float64   `json:"cost_usd,omitempty"`
	WallClock string    `json:"wall_clock,omitempty"`
	Note      string    `json:"note,omitempty"`
	At        time.Time `json:"at"`
}

// Budget is climb/budget.json: the extensions the owner granted, in order.
type Budget struct {
	Extensions []Extension `json:"extensions"`
}

// Totals is what the extensions add to the climb spec's stop rules.
type Totals struct {
	Attempts  int
	CostUSD   float64
	WallClock time.Duration
}

// Total sums the extensions.
func (b Budget) Total() Totals {
	var t Totals
	for _, e := range b.Extensions {
		t.Attempts += e.Attempts
		t.CostUSD += e.CostUSD
		if d, err := time.ParseDuration(e.WallClock); err == nil {
			t.WallClock += d
		}
	}
	return t
}

// Problems lists what is wrong with an extension request.
func (e Extension) Problems() []string {
	var out []string
	if e.Attempts < 0 {
		out = append(out, "attempts must not be negative")
	}
	if !finite(e.CostUSD) || e.CostUSD < 0 {
		out = append(out, "cost_usd must be a number of dollars, zero or more")
	}
	if e.WallClock != "" {
		if d, err := time.ParseDuration(e.WallClock); err != nil || d <= 0 {
			out = append(out, "wall_clock must be a positive duration such as \"2h\"")
		}
	}
	if e.Attempts == 0 && e.CostUSD == 0 && e.WallClock == "" {
		out = append(out, "an extension adds attempts, cost_usd or wall_clock")
	}
	return out
}

// ReadBudget returns the extensions the owner granted.
func ReadBudget(r *trace.Repository, stream config.WorkstreamID) (Budget, error) {
	v, _, _, err := readRecord[Budget](r, stream, trace.ClimbBudgetID)
	return v, err
}

// BudgetDocument returns the next revision of climb/budget.json holding the
// extension, numbered after the ones already granted, and the extension as it
// is recorded. The caller records it with the move that grants it.
func BudgetDocument(r *trace.Repository, stream config.WorkstreamID, e Extension, at time.Time) (trace.Document, Extension, error) {
	b, doc, _, err := readRecord[Budget](r, stream, trace.ClimbBudgetID)
	if err != nil {
		return trace.Document{}, Extension{}, err
	}
	e.Number, e.At = len(b.Extensions)+1, at.UTC()
	b.Extensions = append(b.Extensions, e)
	d, err := jsonDocument(r, stream, trace.ClimbBudgetID, trace.ClimbBudgetPath, doc.Revision+1, at, fmt.Sprintf("climb-extension-%d", e.Number), b)
	return d, e, err
}

// StopRecord is climb/stops/<n>.json: a climb.stop exit after attempt n that
// let the climb go on, and which rules it let go on past.
type StopRecord struct {
	Attempt  int       `json:"attempt"`
	Rules    []string  `json:"rules"`
	Decision string    `json:"decision"`
	At       time.Time `json:"at"`
}

// ReadStops returns the stop records, by attempt number.
func ReadStops(r *trace.Repository, stream config.WorkstreamID) (map[int]StopRecord, error) {
	docs, err := trace.Read[trace.Document](r, stream)
	if err != nil {
		return nil, err
	}
	out := map[int]StopRecord{}
	latest := map[string]trace.Document{}
	for _, d := range docs {
		if strings.HasPrefix(d.Path, trace.ClimbStopsDir+"/") && d.Revision >= latest[d.Path].Revision {
			latest[d.Path] = d
		}
	}
	for _, d := range latest {
		var rec StopRecord
		if err := jsonUnmarshal(d.Content, &rec); err != nil {
			return nil, fmt.Errorf("%s: %w", d.Path, err)
		}
		out[rec.Attempt] = rec
	}
	return out, nil
}

// StopDocument returns the stop record that lets the climb go on after attempt
// n past the rules.
func StopDocument(r *trace.Repository, stream config.WorkstreamID, n int, rules []string, at time.Time) (trace.Document, error) {
	return jsonDocument(r, stream, trace.ClimbStopID(n), trace.ClimbStopPath(n), 1, at, fmt.Sprintf("climb-stop-%d", n),
		StopRecord{Attempt: n, Rules: rules, Decision: "continue", At: at.UTC()})
}
