// Package recordspans derives the spans a trace commit closes and sends them
// through the telemetry sink: one span per workstream state, unit state, turn
// and check run, and the workstream's root span. It also sends the span of an
// exit the transition router decided.
//
// Every start and end is a time stored in the trace, and every ID is derived
// from the workstream and record IDs, so an interval that is open when the
// service restarts closes with the start it was recorded with and the IDs it
// would have had without the restart. No span carries content: names are built
// by the telemetry naming helpers and every attribute is allow-listed there.
package recordspans

import (
	"encoding/json"
	"path"
	"strings"
	"time"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/telemetry"
	"github.com/kpenfound/osmia/internal/trace"
)

// The kinds of check run, the values of the kind attribute of a check run span.
const (
	CheckUnit    = "unit"
	CheckFinal   = "final"
	CheckSession = "session"
)

// The outcomes of a turn, the values of the outcome attribute of a turn span.
const (
	TurnCompleted = "completed"
	TurnWaiting   = "waiting"
	TurnFailed    = "failed"
	TurnCancelled = "cancelled"
)

// How a workstream leaves the factory: the state its last state interval moves
// to and the cause of the root span's end.
const (
	Archived = "archived"
	Removed  = "removed"
)

// InitialState names the interval from a workstream's creation to its first
// recorded state.
const InitialState = "created"

// Recorder turns commit details into spans on its sink. The zero value, and a
// recorder with a disabled sink, sends nothing.
type Recorder struct {
	Sink *telemetry.Sink
}

// Observe is the span observer of a trace repository: pass it to
// Repository.ObserveSpans. It never blocks and never fails; spans the sink
// cannot take are dropped there.
func (r Recorder) Observe(d trace.CommitDetail) {
	if !r.Sink.Enabled() || d.Workstream == "" {
		return
	}
	for _, closed := range d.Closed {
		r.state(d, closed)
	}
	for _, turn := range d.Turns {
		r.turn(d, turn)
	}
	for _, doc := range d.Documents {
		r.check(d, doc)
	}
}

func (r Recorder) state(d trace.CommitDetail, closed trace.ClosedState) {
	t := closed.Transition
	start, entered, state := closed.Created, InitialState, t.From
	if closed.Entered != nil {
		start, entered = closed.Entered.At, closed.Entered.ID
	}
	if state == "" {
		state = InitialState
	}
	w := d.Workstream
	attrs := []telemetry.Attr{telemetry.Project(d.Project), telemetry.Workstream(w), telemetry.State(state), telemetry.NextState(t.To)}
	if t.Subject == trace.FeatureSubject {
		r.Sink.Emit(telemetry.Span{
			Name: telemetry.SpanName("workstream", "state"), Trace: telemetry.WorkstreamTrace(w),
			ID:     telemetry.WorkstreamSpan(w, "workstream-state", entered),
			Parent: telemetry.WorkstreamRoot(w), Start: start, End: t.At,
			Attrs: append(append(attrs, telemetry.Cause(FeatureCause(t))), routing(t)...),
		})
		return
	}
	r.Sink.Emit(telemetry.Span{
		Name: telemetry.SpanName("unit", "state"), Trace: telemetry.WorkstreamTrace(w),
		ID:     telemetry.WorkstreamSpan(w, "unit-state", t.Unit, entered),
		Parent: telemetry.WorkstreamRoot(w), Start: start, End: t.At,
		Attrs: append(append(attrs, telemetry.Unit(t.Unit), telemetry.Cause(UnitCause(t))), routing(t)...),
	})
}

func (r Recorder) turn(d trace.CommitDetail, turn trace.FinishedTurn) {
	w, res := d.Workstream, turn.Response.Result
	outcome, cause := TurnOutcome(turn.Response)
	attrs := []telemetry.Attr{
		telemetry.GenAIOperation(telemetry.GenAIOperationInvokeAgent),
		telemetry.GenAIAgentName(turn.Role), telemetry.GenAIAgentID(turn.Response.AgentID),
		telemetry.GenAIProvider(turn.Profile.Backend), telemetry.GenAIModel(turn.Profile.Model),
		telemetry.Role(turn.Role), telemetry.Backend(turn.Profile.Backend), telemetry.Model(turn.Profile.Model), telemetry.Profile(turn.Profile.Name),
		telemetry.Project(d.Project), telemetry.Workstream(w), telemetry.Record(turn.Response.ID),
		telemetry.Outcome(outcome), telemetry.SlotWait(SlotWait(turn)), telemetry.CostKnown(res.Usage.CostKnown),
	}
	if unit := turn.Request.Unit; unit != "" {
		attrs = append(attrs, telemetry.Unit(unit))
	}
	if res.Client != "" {
		attrs = append(attrs, telemetry.Client(res.Client))
	}
	if cause != "" {
		attrs = append(attrs, telemetry.Cause(cause))
	}
	if res.Usage.CostKnown {
		attrs = append(attrs, telemetry.CostUSD(res.Usage.CostUSD))
	}
	r.Sink.Emit(telemetry.Span{
		Name: telemetry.GenAIName(telemetry.GenAIOperationInvokeAgent, turn.Role), Trace: telemetry.WorkstreamTrace(w),
		ID:     telemetry.RecordSpan(w, turn.Response.ID, turn.Response.Revision),
		Parent: telemetry.WorkstreamRoot(w), Start: res.StartedAt, End: res.StartedAt.Add(res.Duration),
		Attrs: attrs, Failed: outcome == TurnFailed,
	})
}

// TurnOutcome classifies a recorded response as one of the turn outcomes, with,
// for a turn that was stopped or failed, the enumerated word that says how.
func TurnOutcome(r trace.TurnResponse) (outcome, cause string) {
	res := r.Result
	switch {
	case r.Stop != nil:
		return TurnCancelled, r.Stop.Cause
	case res.Cancelled:
		return TurnCancelled, ""
	case r.Failure != "" || res.IsError || res.TimedOut || res.ExitCode != 0 || res.Signal != 0:
		switch {
		case res.TimedOut:
			cause = "timeout"
		case r.FailureClass != "":
			cause = string(r.FailureClass)
		}
		return TurnFailed, cause
	case res.Outcome != nil && res.Outcome.Status == "waiting":
		return TurnWaiting, ""
	}
	return TurnCompleted, ""
}

// SlotWait is how long the turn waited between being queued and being claimed
// by a runner, as recorded: the request's time to the claim's time.
func SlotWait(turn trace.FinishedTurn) time.Duration {
	if turn.ClaimedAt.IsZero() || turn.ClaimedAt.Before(turn.Request.At) {
		return 0
	}
	return turn.ClaimedAt.Sub(turn.Request.At)
}

type checkRun struct {
	Status     string    `json:"status"`
	StartedAt  time.Time `json:"started_at"`
	FinishedAt time.Time `json:"finished_at"`
	Selection  *struct {
		Covered *int `json:"covered"`
	} `json:"selection"`
}

// CheckKind returns the kind of check run the document at docPath records and
// whether it records one: units/<unit>/checks-<n>.json, final/checks-<n>.json
// or session-checks/checks-<n>.json, and not their selection, output or start
// records.
func CheckKind(docPath string) (string, bool) {
	dir, file := path.Split(docPath)
	n, ok := strings.CutPrefix(file, "checks-")
	if !ok {
		return "", false
	}
	n, ok = strings.CutSuffix(n, ".json")
	if !ok || n == "" || strings.Trim(n, "0123456789") != "" {
		return "", false
	}
	switch parts := strings.Split(strings.TrimSuffix(dir, "/"), "/"); {
	case len(parts) == 2 && parts[0] == "units":
		return CheckUnit, true
	case len(parts) == 1 && parts[0] == "final":
		return CheckFinal, true
	case len(parts) == 1 && parts[0] == "session-checks":
		return CheckSession, true
	}
	return "", false
}

func (r Recorder) check(d trace.CommitDetail, doc trace.Document) {
	kind, ok := CheckKind(doc.Path)
	if !ok {
		return
	}
	var run checkRun
	if json.Unmarshal([]byte(doc.Content), &run) != nil || run.StartedAt.IsZero() {
		return
	}
	end := run.FinishedAt
	if end.Before(run.StartedAt) {
		end = run.StartedAt
	}
	w := d.Workstream
	attrs := []telemetry.Attr{telemetry.Project(d.Project), telemetry.Workstream(w), telemetry.CheckKind(kind), telemetry.Record(doc.ID), telemetry.Outcome(run.Status)}
	if kind == CheckUnit {
		attrs = append(attrs, telemetry.Unit(doc.Unit))
	}
	if run.Selection != nil && run.Selection.Covered != nil {
		attrs = append(attrs, telemetry.Covered(*run.Selection.Covered))
	}
	r.Sink.Emit(telemetry.Span{
		Name: telemetry.SpanName("check", "run"), Trace: telemetry.WorkstreamTrace(w),
		ID:     telemetry.RecordSpan(w, doc.ID, doc.Revision),
		Parent: telemetry.WorkstreamRoot(w), Start: run.StartedAt, End: end,
		Attrs: attrs, Failed: run.Status != "" && run.Status != "passed",
	})
}

// Ended sends the closing spans of a workstream that leaves the factory at end,
// archived or removed: the span of the last workstream state, which ends there,
// and the root span, which covers the workstream's whole life from its recorded
// creation. repository is read for the creation time and the last recorded
// state; a workstream whose trace cannot be read sends nothing.
func (r Recorder) Ended(repository *trace.Repository, stream config.WorkstreamID, end time.Time, how string) {
	if !r.Sink.Enabled() || repository == nil || end.IsZero() {
		return
	}
	created, err := repository.WorkstreamCreated(stream)
	if err != nil {
		return
	}
	transitions, err := trace.Read[trace.Transition](repository, stream)
	if err != nil {
		return
	}
	var last *trace.Transition
	for i := len(transitions) - 1; i >= 0 && last == nil; i-- {
		if transitions[i].Subject == trace.FeatureSubject {
			last = &transitions[i]
		}
	}
	r.endSpans(repository.Project(), stream, created, last, end, how)
}

func (r Recorder) endSpans(project config.ProjectID, w config.WorkstreamID, created time.Time, last *trace.Transition, end time.Time, how string) {
	if end.Before(created) {
		end = created
	}
	outcome := how
	if last != nil {
		if end.Before(last.At) {
			end = last.At
		}
		outcome = last.To
		r.Sink.Emit(telemetry.Span{
			Name: telemetry.SpanName("workstream", "state"), Trace: telemetry.WorkstreamTrace(w),
			ID:     telemetry.WorkstreamSpan(w, "workstream-state", last.ID),
			Parent: telemetry.WorkstreamRoot(w), Start: last.At, End: end,
			Attrs: []telemetry.Attr{telemetry.Project(project), telemetry.Workstream(w), telemetry.State(last.To), telemetry.NextState(how), telemetry.Cause(how)},
		})
	}
	r.Sink.Emit(telemetry.Span{
		Name: telemetry.SpanName("workstream"), Trace: telemetry.WorkstreamTrace(w), ID: telemetry.WorkstreamRoot(w),
		Start: created, End: end,
		Attrs: []telemetry.Attr{telemetry.Project(project), telemetry.Workstream(w), telemetry.Outcome(outcome), telemetry.Cause(how)},
	})
}
