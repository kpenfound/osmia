package climb

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"time"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/trace"
)

// InnerSubject is the workflow subject that holds the inner state of a Hill
// Climb workstream while it is building.
const InnerSubject = trace.ClimbSubject

// The inner states of a building Hill Climb workstream, in the order it passes
// through them. A workstream holds one of them from the moment the machine
// reads its sealed climb spec.
const (
	// InnerEvaluator is the state in which the evaluator unit writes the
	// evaluator function. It is skipped when the project already has one.
	InnerEvaluator = "evaluator"
	// InnerCalibrating is the state in which the service runs the evaluator
	// on the evaluator commit.
	InnerCalibrating = "calibrating"
	// InnerHeldBroken is a calibration that found the metric broken: a run
	// failed or was invalid. The code is not at fault.
	InnerHeldBroken = "held-broken-metric"
	// InnerHeldNoisy is a calibration whose spread reaches the minimum
	// improvement, so no improvement could be told from noise.
	InnerHeldNoisy = "held-too-noisy"
	// InnerAwaitingApproval is the state in which the owner's approval card
	// is open.
	InnerAwaitingApproval = "awaiting-approval"
	// InnerClimbing is the state the owner's approval starts. The attempt
	// loop runs in it.
	InnerClimbing = "climbing"
	// InnerConcluding is the state a firing stop rule, the owner or the chief
	// of staff moves a climb to. Concluding, the rebase and the delivery of the
	// branch belong to the delivery of the kind.
	InnerConcluding = "concluding"
)

// Held reports whether the inner state is one of the two holds.
func Held(state string) bool { return state == InnerHeldBroken || state == InnerHeldNoisy }

// InnerActor is the provenance of the inner machine's transitions.
var InnerActor = trace.Actor{Kind: "service", ID: "climb"}

// EvaluatorRecord is climb/evaluator.json: what the evaluator step decided
// when the machine first read the climb spec.
type EvaluatorRecord struct {
	// Evaluator is the evaluator function the climb spec names.
	Evaluator string `json:"evaluator"`
	// Base is the tip of the climb branch before the evaluator unit; the
	// evaluator diff is read against it.
	Base string `json:"base"`
	// Skipped says the project already had a function that matches the climb
	// spec, so no evaluator unit was run and the function is reused.
	Skipped bool `json:"skipped"`
	// Evidence says what was matched, or why nothing was.
	Evidence string `json:"evidence"`
	// Module is the dev module's source directory.
	Module string    `json:"module"`
	At     time.Time `json:"at"`
}

// CalibrationRecord is climb/calibration.json: what calibrating measured on
// the evaluator commit.
type CalibrationRecord struct {
	Evaluator string `json:"evaluator"`
	// Primary is the name of the primary metric the runs measured.
	Primary string `json:"primary"`
	// Commit is the evaluator commit the runs measured.
	Commit string `json:"commit"`
	// Base is the commit the evaluator diff is read against, and Diff is
	// that diff. The diff is empty when the evaluator unit was skipped.
	Base string `json:"base"`
	Diff string `json:"diff,omitempty"`
	// Protected lists the paths the owner's approval will freeze.
	Protected []string `json:"protected"`
	// SpecRevision is the revision of the sealed climb spec calibrated.
	SpecRevision int         `json:"spec_revision"`
	Calibration  Calibration `json:"calibration"`
	At           time.Time   `json:"at"`
}

// ApprovalRecord is climb/approval.json: the owner's approval of the
// calibrated evaluator, with everything the attempt loop starts from. Its
// presence is what lets climbing begin.
type ApprovalRecord struct {
	Evaluator string `json:"evaluator"`
	// Commit is the approved evaluator commit; the first attempt starts from
	// the climb branch at it.
	Commit string `json:"commit"`
	// Baseline, Units and Noise are what calibration established: the median
	// of the primary metric, the unit of each required metric, and the
	// run-to-run spread of the primary metric.
	Baseline float64            `json:"baseline"`
	Metrics  map[string]float64 `json:"metrics,omitempty"`
	Units    map[string]string  `json:"units,omitempty"`
	Noise    float64            `json:"noise"`
	// Protected lists the frozen paths and Approved is the frozen copy of
	// them that every measurement restores.
	Protected  []string     `json:"protected"`
	Approved   ApprovedCopy `json:"approved"`
	ApprovedBy string       `json:"approved_by"`
	At         time.Time    `json:"at"`
}

func latestDocument(r *trace.Repository, stream config.WorkstreamID, id string) (trace.Document, bool, error) {
	docs, err := trace.Read[trace.Document](r, stream)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return trace.Document{}, false, nil
		}
		return trace.Document{}, false, err
	}
	var latest trace.Document
	for _, d := range docs {
		if d.ID == id && d.Revision >= latest.Revision {
			latest = d
		}
	}
	return latest, latest.Revision > 0, nil
}

func readRecord[T any](r *trace.Repository, stream config.WorkstreamID, id string) (T, trace.Document, bool, error) {
	var v T
	doc, found, err := latestDocument(r, stream, id)
	if err != nil || !found {
		return v, trace.Document{}, false, err
	}
	if err := json.Unmarshal([]byte(doc.Content), &v); err != nil {
		return v, doc, false, fmt.Errorf("%s: %w", doc.Path, err)
	}
	return v, doc, true, nil
}

// ReadEvaluator returns the evaluator step's record, and whether it is
// recorded.
func ReadEvaluator(r *trace.Repository, stream config.WorkstreamID) (EvaluatorRecord, bool, error) {
	v, _, found, err := readRecord[EvaluatorRecord](r, stream, trace.ClimbEvaluatorID)
	return v, found, err
}

// ReadCalibration returns the latest calibration record, and whether there is
// one.
func ReadCalibration(r *trace.Repository, stream config.WorkstreamID) (CalibrationRecord, trace.Document, bool, error) {
	return readRecord[CalibrationRecord](r, stream, trace.ClimbCalibrationID)
}

// ReadApproval returns the owner's approval of the calibrated evaluator, and
// whether the owner has approved. The attempt loop reads the baseline, the
// units, the noise band and the frozen protected paths from it.
func ReadApproval(r *trace.Repository, stream config.WorkstreamID) (ApprovalRecord, bool, error) {
	v, _, found, err := readRecord[ApprovalRecord](r, stream, trace.ClimbApprovalID)
	return v, found, err
}

// ReadInner returns the workstream's inner state, empty before the machine
// has started.
func ReadInner(r *trace.Repository, stream config.WorkstreamID) (trace.WorkflowState, error) {
	return r.Workflow(stream, InnerSubject)
}

func nextRevision(r *trace.Repository, stream config.WorkstreamID, id string) (int, error) {
	doc, found, err := latestDocument(r, stream, id)
	if err != nil || !found {
		return 1, err
	}
	return doc.Revision + 1, nil
}

func (m *Machine) document(stream config.WorkstreamID, id, path string, revision int, cause string, v any) (trace.Document, error) {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return trace.Document{}, err
	}
	return trace.Document{
		Header: trace.Header{Schema: "osmia.trace.document", Version: trace.Version, ID: id, Revision: revision, Project: m.Repository.Project(), Workstream: stream, At: m.now(), Actor: InnerActor, Cause: cause},
		Path:   path, Content: string(data) + "\n",
	}, nil
}

func (m *Machine) header(id string, stream config.WorkstreamID, cause string) trace.Header {
	return trace.Header{Schema: "osmia.trace.transition", Version: trace.Version, ID: id, Revision: 1, Project: m.Repository.Project(), Workstream: stream, At: m.now(), Actor: InnerActor, Cause: cause}
}

func (m *Machine) now() time.Time {
	if m.Now != nil {
		return m.Now().UTC()
	}
	return time.Now().UTC()
}

func (m *Machine) step(name string) error {
	if m.Step == nil {
		return nil
	}
	return m.Step(name)
}
