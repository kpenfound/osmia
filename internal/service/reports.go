package service

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"
	"sync"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/plan"
	"github.com/kpenfound/osmia/internal/seal"
	"github.com/kpenfound/osmia/internal/trace"
)

const (
	// doneTool is the tool a mason ends its unit's work with.
	doneTool = "done"
	// masonDone is the outcome of a mason turn whose done the service
	// accepted. The outcome's report holds the mason's report as JSON.
	masonDone = "done"
	// reportPath is the path of a unit's report in its workstream's trace,
	// with the unit ID in place of %s.
	reportPath = "units/%s/report.json"
)

// MasonReport is what a mason reports with done: the outcome of its work
// and one entry for every criterion its unit addresses.
type MasonReport struct {
	Outcome  string            `json:"outcome"`
	Criteria []CriterionReport `json:"criteria"`
}

// CriterionReport is a mason's report on one criterion of its unit: what it
// did, the evidence that the criterion holds and where the proof lives.
type CriterionReport struct {
	Criterion string `json:"criterion"`
	Done      string `json:"done"`
	Evidence  string `json:"evidence"`
	Proof     string `json:"proof"`
}

// UnitReport is the document units/<unit>/report.json: the mason's report
// on a unit, the turn it came from, the seal the unit was built against,
// and the candidate the service made of the unit's workspace, a commit on
// Branch that descends from the feature branch at Base.
type UnitReport struct {
	Unit      string            `json:"unit"`
	Turn      string            `json:"turn"`
	Seal      int               `json:"seal"`
	Outcome   string            `json:"outcome"`
	Criteria  []CriterionReport `json:"criteria"`
	Branch    string            `json:"branch"`
	Base      string            `json:"base"`
	Candidate string            `json:"candidate"`
}

// reportDocument is the document ID of a unit's report.
func reportDocument(unit string) string { return trace.UnitSubject(unit) + "-report" }

// reviewingTransitionID returns the ID of the transition that moves a unit
// to reviewing on its report revision k.
func reviewingTransitionID(unit string, k int) string {
	return fmt.Sprintf("%s-%s-%d", trace.UnitSubject(unit), UnitReviewing, k)
}

// masonReports holds the report each running mason turn's done accepted,
// until the turn ends and reports it as its outcome.
type masonReports struct {
	mu       sync.Mutex
	accepted map[string]MasonReport
}

func turnKey(scope coreadapter.Scope) string {
	return scope.Workstream + "/" + scope.Thread + "/" + scope.Turn
}

// tool returns the done tool of the claimed mason turn scope names. Input
// its schema refuses is a tool error. A report the service refuses is an
// ordinary result, {"recorded":false,"reason":...}, so the mason reads why,
// fixes it and calls done again; its unit does not move.
func (r *masonReports) tool(repository *trace.Repository, scope coreadapter.Scope) coreadapter.Tool {
	done := coreadapter.Tool{Name: doneTool, Effect: coreadapter.ToolMemory,
		Description: "Report your unit's work done, once every criterion of the unit holds and the proof the plan names for it is in place and passing. Give the outcome of your work and, for every criterion of the unit, what you did, the evidence that it holds and where the proof lives. The service records the report, takes your workspace as the unit's candidate and sends it to review; end your turn as soon as this returns.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"outcome":{"type":"string"},"criteria":{"type":"array","items":{"type":"object","properties":{"criterion":{"type":"string"},"done":{"type":"string"},"evidence":{"type":"string"},"proof":{"type":"string"}},"required":["criterion","done","evidence","proof"],"additionalProperties":false}}},"required":["outcome","criteria"],"additionalProperties":false}`)}
	done.Handle = func(ctx context.Context, raw json.RawMessage) (json.RawMessage, error) {
		var report MasonReport
		d := json.NewDecoder(bytes.NewReader(raw))
		d.DisallowUnknownFields()
		if err := d.Decode(&report); err != nil {
			return nil, fmt.Errorf("tool input: %w", err)
		}
		if err := d.Decode(new(any)); err != io.EOF {
			return nil, errors.New("tool input must be one object")
		}
		state, err := repository.Workflow(config.WorkstreamID(scope.Workstream), trace.UnitSubject(scope.Unit))
		if err != nil {
			return nil, err
		}
		if state.Value != UnitImplementing {
			return refuseReport("unit %s is %s, not implementing", scope.Unit, state.Value)
		}
		unit, err := sealedUnit(repository, scope)
		if err != nil {
			return nil, err
		}
		if reason := checkReport(unit, report); reason != "" {
			return refuseReport("%s", reason)
		}
		r.mu.Lock()
		defer r.mu.Unlock()
		key := turnKey(scope)
		if _, ok := r.accepted[key]; ok {
			return refuseReport("this turn already reported its unit done; end the turn")
		}
		if r.accepted == nil {
			r.accepted = map[string]MasonReport{}
		}
		r.accepted[key] = report
		return json.Marshal(struct {
			Recorded bool   `json:"recorded"`
			Next     string `json:"next"`
		}{true, "End your turn now. The service takes your workspace as the unit's candidate and sends it to review."})
	}
	return done
}

func refuseReport(format string, args ...any) (json.RawMessage, error) {
	return json.Marshal(struct {
		Recorded bool   `json:"recorded"`
		Reason   string `json:"reason"`
	}{false, fmt.Sprintf(format, args...)})
}

// sealedUnit returns the unit of the latest sealed plan scope names.
func sealedUnit(repository *trace.Repository, scope coreadapter.Scope) (plan.Unit, error) {
	stream := config.WorkstreamID(scope.Workstream)
	latest, _, found, err := seal.Latest(repository, stream)
	if err != nil {
		return plan.Unit{}, err
	}
	if !found {
		return plan.Unit{}, fmt.Errorf("workstream %s has no seal", stream)
	}
	graph, err := sealedPlan(repository, stream, latest.Revision.Plan)
	if err != nil {
		return plan.Unit{}, err
	}
	p, err := plan.Parse([]byte(graph.Content))
	if err != nil {
		return plan.Unit{}, err
	}
	unit, ok := p.Unit(scope.Unit)
	if !ok {
		return plan.Unit{}, fmt.Errorf("the sealed plan of workstream %s has no unit %s", stream, scope.Unit)
	}
	return unit, nil
}

// checkReport returns why a report does not report on the unit: an empty
// outcome, a criterion the unit does not address or one reported twice, an
// entry with an empty field, or a criterion of the unit left out. It returns
// "" for a report with one complete entry for every criterion of the unit.
func checkReport(unit plan.Unit, report MasonReport) string {
	if strings.TrimSpace(report.Outcome) == "" {
		return "outcome is required: say what the unit's work now does"
	}
	var criteria []string
	for _, a := range unit.Addresses {
		if !slices.Contains(criteria, a.Criterion) {
			criteria = append(criteria, a.Criterion)
		}
	}
	reported := map[string]bool{}
	for _, c := range report.Criteria {
		if !slices.Contains(criteria, c.Criterion) {
			return fmt.Sprintf("unit %s does not address criterion %q; report on %s alone", unit.ID, c.Criterion, strings.Join(criteria, ", "))
		}
		if reported[c.Criterion] {
			return fmt.Sprintf("criterion %s is reported twice", c.Criterion)
		}
		reported[c.Criterion] = true
		for _, field := range []struct{ name, value string }{{"done", c.Done}, {"evidence", c.Evidence}, {"proof", c.Proof}} {
			if strings.TrimSpace(field.value) == "" {
				return fmt.Sprintf("criterion %s has no %s", c.Criterion, field.name)
			}
		}
	}
	missing := slices.DeleteFunc(slices.Clone(criteria), func(c string) bool { return reported[c] })
	if len(missing) != 0 {
		return fmt.Sprintf("the report misses %s: report on every criterion of unit %s", strings.Join(missing, ", "), unit.ID)
	}
	return ""
}

// reportingTurns ends every mason turn whose done the service accepted with
// the outcome masonDone and the report, whatever the agent reported.
type reportingTurns struct {
	Turns   coreadapter.Turns
	reports *masonReports
}

var _ coreadapter.Turns = (*reportingTurns)(nil)
var _ coreadapter.ResumeChecker = (*reportingTurns)(nil)

func (t *reportingTurns) Run(ctx context.Context, prepared coreadapter.PreparedTurn) (coreadapter.SessionResult, error) {
	result, err := t.Turns.Run(ctx, prepared)
	t.reports.mu.Lock()
	defer t.reports.mu.Unlock()
	key := turnKey(prepared.Scope)
	report, ok := t.reports.accepted[key]
	delete(t.reports.accepted, key)
	if ok {
		data, encodeErr := json.Marshal(report)
		if encodeErr != nil {
			return result, errors.Join(err, encodeErr)
		}
		result.Outcome = &coreadapter.Outcome{Status: masonDone, Report: string(data)}
	}
	return result, err
}

// CheckResume defers to the wrapped runner; one that cannot check resumes
// makes the turn replay.
func (t *reportingTurns) CheckResume(ctx context.Context, previous, next coreadapter.Profile, session coreadapter.BackendSession) error {
	if checker, ok := t.Turns.(coreadapter.ResumeChecker); ok {
		return checker.CheckResume(ctx, previous, next, session)
	}
	return coreadapter.ErrResumeUnavailable
}

// reported returns the report of the unit's mason when the last turn of its
// thread ended cleanly with done, and that turn.
func (m *masons) reported(stream config.WorkstreamID, unit string) (MasonReport, trace.QueuedTurn, bool, error) {
	th, err := m.repository.Thread(stream, masonAgent(unit))
	if errors.Is(err, os.ErrNotExist) || err == nil && len(th.Turns) == 0 {
		return MasonReport{}, trace.QueuedTurn{}, false, nil
	}
	if err != nil {
		return MasonReport{}, trace.QueuedTurn{}, false, err
	}
	last := th.Turns[len(th.Turns)-1]
	if last.Status() != "idle" || last.Response.Result.Outcome == nil || last.Response.Result.Outcome.Status != masonDone {
		return MasonReport{}, trace.QueuedTurn{}, false, nil
	}
	var report MasonReport
	if err := json.Unmarshal([]byte(last.Response.Result.Outcome.Report), &report); err != nil {
		return MasonReport{}, trace.QueuedTurn{}, false, fmt.Errorf("the report of turn %s: %w", last.Request.TurnID, err)
	}
	return report, last, true, nil
}

// finish moves an implementing unit whose mason reported done to reviewing:
// it snapshots the unit's workspace as its candidate, and records the report
// with the candidate as the next revision of units/<unit>/report.json and the
// move in one commit. It reports whether the unit moved. A unit whose
// candidate cannot be made stays implementing, and finish records why it is
// blocked. A unit whose state moved since it was read is left to the next
// pass.
func (m *masons) finish(ctx context.Context, b building, unit string) (moved, blocked bool, err error) {
	report, turn, found, err := m.reported(b.stream, unit)
	if err != nil || !found {
		return false, false, err
	}
	w, base, candidate, err := newUnitWorkspaces(m.cfg).snapshot(ctx, b.stream, unit)
	if err != nil {
		if ctx.Err() != nil {
			return false, false, err
		}
		return false, true, m.block(ctx, b.stream, unit, turn.Response.ID, fmt.Sprintf("unit %s stays implementing: its mason reported done, and its candidate cannot be made: %v", unit, err))
	}
	latest, _, _, err := seal.Latest(m.repository, b.stream)
	if err != nil {
		return false, false, err
	}
	docs, err := trace.Read[trace.Document](m.repository, b.stream)
	if err != nil {
		return false, false, err
	}
	k := 1
	for _, d := range docs {
		if d.ID == reportDocument(unit) {
			k = d.Revision + 1
		}
	}
	content, err := json.MarshalIndent(UnitReport{Unit: unit, Turn: turn.Request.TurnID, Seal: latest.Seal, Outcome: report.Outcome, Criteria: report.Criteria, Branch: w.Branch, Base: base, Candidate: candidate}, "", "  ")
	if err != nil {
		return false, false, err
	}
	at := m.s.now()
	h := trace.Header{Schema: "osmia.trace.document", Version: trace.Version, ID: reportDocument(unit), Revision: k, Project: m.repository.Project(), Workstream: b.stream, Unit: unit, At: at, Actor: masonActor, Cause: turn.Response.ID, Depth: turn.Request.Depth}
	doc := trace.Document{Header: h, Path: fmt.Sprintf(reportPath, unit), Content: string(content) + "\n"}
	subject := trace.UnitSubject(unit)
	h.Schema, h.ID, h.Revision = "osmia.trace.transition", reviewingTransitionID(unit, k), 1
	tr := trace.Transition{Header: h, Subject: subject, From: UnitImplementing, To: UnitReviewing,
		Reason: fmt.Sprintf("the mason of unit %s reported done on turn %s; its candidate is %s on %s, from %s at %s, and its report is %s revision %d", unit, turn.Request.TurnID, candidate, w.Branch, featureBranch(b.stream), base, doc.Path, k)}
	tx := trace.Transaction{ExpectedVersion: b.states[subject].Version, Transition: tr,
		Events: []trace.Event{trace.Notice(reviewingTransitionID(unit, k), "unit", fmt.Sprintf("Unit %s is reviewing: its mason reported done on turn %s; its report is %s revision %d.", unit, turn.Request.TurnID, doc.Path, k))}}
	if _, err := m.repository.RecordDocumentsWith(ctx, []trace.Document{doc}, tx); errors.Is(err, trace.ErrConflict) {
		return false, false, nil
	} else if err != nil {
		return false, false, err
	}
	return true, false, nil
}
