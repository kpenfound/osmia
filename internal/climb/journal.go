package climb

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/trace"
)

// Phase is how far an attempt has got. The phase is the value of the workflow
// subject trace.ClimbAttemptSubject(n), so each move is one commit that also
// carries the documents it rests on, and a restart reads the phase back and
// carries on from it.
type Phase string

const (
	// PhaseStarted: the attempt exists, its parent is fixed, and its mason works
	// in a workspace built from the parent.
	PhaseStarted Phase = "started"
	// PhaseCaptured: the mason's workspace was taken as the attempt's
	// candidate, and its report is recorded.
	PhaseCaptured Phase = "captured"
	// PhaseMeasured: Go decided keep or discard, from the protected-path gate,
	// the guardrail checks and the measurement.
	PhaseMeasured Phase = "measured"
	// PhaseReviewed: the reviewer's verdict on a kept candidate is recorded.
	PhaseReviewed Phase = "reviewed"
	// PhasePromoting: the squashed commit is prepared and recorded, and the
	// climb branch is about to move to it.
	PhasePromoting Phase = "promoting"
	// PhasePromoted: the climb branch is at the squashed commit.
	PhasePromoted Phase = "promoted"
	// PhaseRecorded: the attempt's terminal record, climb/attempts/<n>.json,
	// exists.
	PhaseRecorded Phase = "recorded"
	// PhaseCleaned: the attempt's workspace is gone. Its branch stays.
	PhaseCleaned Phase = "cleaned"
)

// MasonAgent is the one mason thread of a climb. Every attempt's turns run on
// it, so the mason keeps what it learned from the attempts before.
const MasonAgent = "mason-climb"

// AttemptUnit is the unit ID of attempt n: the scope a mason turn, its
// workspace and its reviewer carry.
func AttemptUnit(n int) string { return fmt.Sprintf("climb-attempt-%d", n) }

// AttemptNumber returns the attempt a unit ID names.
func AttemptNumber(unit string) (int, bool) {
	rest, ok := strings.CutPrefix(unit, "climb-attempt-")
	if !ok {
		return 0, false
	}
	var n int
	if _, err := fmt.Sscanf(rest, "%d", &n); err != nil || n < 1 || fmt.Sprint(n) != rest {
		return 0, false
	}
	return n, true
}

// Review is the reviewer's verdict on a kept candidate.
type Review struct {
	Approved bool `json:"approved"`
	// Summary is how the reviewer verified the candidate and Notes what a
	// rejected candidate got wrong, which the mason's next turn reads.
	Summary string `json:"summary,omitempty"`
	Notes   string `json:"notes,omitempty"`
	Turn    string `json:"turn,omitempty"`
}

// Journal is climb/journal/<n>.json: everything the attempt has decided so
// far, rewritten as a new revision with every phase. Reading the latest
// revision and the phase subject is how a restart finds its place.
type Journal struct {
	Number int `json:"number"`
	// Parent is the commit of the current best the attempt started from, and
	// Best its primary score.
	Parent string  `json:"parent"`
	Best   float64 `json:"best"`
	Unit   string  `json:"unit"`
	// Branch is the branch of the attempt's workspace. It keeps the candidate
	// inspectable when the attempt is discarded.
	Branch string `json:"branch"`
	Phase  Phase  `json:"phase"`
	// Steering counts the steering notes the attempt's bundle carried.
	Steering  int       `json:"steering"`
	StartedAt time.Time `json:"started_at"`
	// SpecRevision and Epoch are the climb spec revision and the approval
	// epoch the attempt started under.
	SpecRevision int `json:"spec_revision,omitempty"`
	Epoch        int `json:"epoch,omitempty"`

	Hypothesis    string `json:"hypothesis,omitempty"`
	ChangeSummary string `json:"change_summary,omitempty"`
	Expected      string `json:"expected_effect,omitempty"`

	Candidate string      `json:"candidate,omitempty"`
	Changed   []string    `json:"changed,omitempty"`
	Diff      DiffSummary `json:"diff"`

	Guardrails  *Guardrails  `json:"guardrails,omitempty"`
	Measurement *Measurement `json:"measurement,omitempty"`
	Decision    *Decision    `json:"decision,omitempty"`
	Review      *Review      `json:"review,omitempty"`

	// SquashAt, Message and Squashed are the prepared promotion: the commit
	// the climb branch is moved to, fixed before the branch moves.
	SquashAt time.Time `json:"squash_at,omitempty"`
	Message  string    `json:"message,omitempty"`
	Squashed string    `json:"squashed,omitempty"`
}

// ReadJournal returns the latest revision of the journal of attempt n, and
// whether the attempt was started.
func ReadJournal(r *trace.Repository, stream config.WorkstreamID, n int) (Journal, bool, error) {
	j, _, found, err := readRecord[Journal](r, stream, trace.ClimbJournalID(n))
	return j, found, err
}

// OpenAttempt returns the journal of the attempt that has not been cleaned up
// yet. A climb has at most one.
func OpenAttempt(r *trace.Repository, stream config.WorkstreamID) (Journal, bool, error) {
	n, err := LastStarted(r, stream)
	if err != nil || n == 0 {
		return Journal{}, false, err
	}
	state, err := r.Workflow(stream, trace.ClimbAttemptSubject(n))
	if err != nil || state.Value == string(PhaseCleaned) {
		return Journal{}, false, err
	}
	j, found, err := ReadJournal(r, stream, n)
	if err != nil || !found {
		return Journal{}, false, err
	}
	j.Phase = Phase(state.Value)
	return j, true, nil
}

// LastStarted returns the number of the highest attempt that was started, or 0.
func LastStarted(r *trace.Repository, stream config.WorkstreamID) (int, error) {
	docs, err := trace.Read[trace.Document](r, stream)
	if err != nil {
		return 0, err
	}
	last := 0
	for _, d := range docs {
		if strings.HasPrefix(d.ID, "climb-journal-") {
			var n int
			if _, err := fmt.Sscanf(strings.TrimPrefix(d.ID, "climb-journal-"), "%d", &n); err == nil && n > last {
				last = n
			}
		}
	}
	return last, nil
}

func jsonDocument(r *trace.Repository, stream config.WorkstreamID, id, path string, revision int, at time.Time, cause string, v any) (trace.Document, error) {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return trace.Document{}, err
	}
	return trace.Document{
		Header: trace.Header{Schema: "osmia.trace.document", Version: trace.Version, ID: id, Revision: revision, Project: r.Project(), Workstream: stream, At: at.UTC(), Actor: Actor, Cause: cause},
		Path:   path, Content: string(data) + "\n",
	}, nil
}

func journalDocument(r *trace.Repository, stream config.WorkstreamID, j Journal, at time.Time, cause string) (trace.Document, error) {
	revision, err := nextRevision(r, stream, trace.ClimbJournalID(j.Number))
	if err != nil {
		return trace.Document{}, err
	}
	return jsonDocument(r, stream, trace.ClimbJournalID(j.Number), trace.ClimbJournalPath(j.Number), revision, at, cause, j)
}

// AttemptPhases are the phases of an attempt in order.
var AttemptPhases = []Phase{PhaseStarted, PhaseCaptured, PhaseMeasured, PhaseReviewed, PhasePromoting, PhasePromoted, PhaseRecorded, PhaseCleaned}

var errMoved = errors.New("the attempt moved on")

func isConflict(err error) bool { return errors.Is(err, trace.ErrConflict) }

func recordWith(ctx context.Context, r *trace.Repository, docs []trace.Document, tx trace.Transaction) error {
	if _, err := r.RecordDocumentsWith(ctx, docs, tx); err != nil {
		if isConflict(err) {
			return errMoved
		}
		return err
	}
	return nil
}

func jsonUnmarshal(content string, v any) error { return json.Unmarshal([]byte(content), v) }
