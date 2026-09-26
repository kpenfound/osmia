package trace

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
)

// StatusID is the record ID of every workstream status revision.
const StatusID = "status"

// StatusRole is the only role that may write a workstream status.
const StatusRole = "chief_of_staff"

// FeatureSubject is the workflow subject holding a workstream's feature state.
const FeatureSubject = "feature"

// UnitSubject returns the workflow subject holding the state of the plan unit
// with the given ID: unit-<id>, or unit_ and a hash of the ID for an ID longer
// than 64 characters, so that the subject and the IDs of its transitions stay
// keys.
func UnitSubject(unit string) string {
	if len(unit) <= 64 {
		return "unit-" + unit
	}
	sum := sha256.Sum256([]byte(unit))
	return "unit_" + hex.EncodeToString(sum[:16])
}

// StatusContent is what the chief of staff writes. Attention names an open
// owner gate, or is empty when there are none. Agents may be empty.
type StatusContent struct {
	Goal      string   `json:"goal"`
	Attention string   `json:"attention"`
	Note      string   `json:"note"`
	Agents    []string `json:"agents"`
}

func (c StatusContent) valid() bool {
	if !present(c.Goal) || !present(c.Note) || c.Agents == nil {
		return false
	}
	for _, a := range c.Agents {
		if !present(a) {
			return false
		}
	}
	return true
}

// StatusRejected is returned by SetStatus when the caller's check refuses
// the content. Reason is written for the chief of staff.
type StatusRejected struct{ Reason string }

func (e *StatusRejected) Error() string { return "status rejected: " + e.Reason }

// StatusCheck decides whether content may be stored. Known holds identifiers
// the trace records for the workstream: project, workstream, agent, thread,
// turn and backend session IDs, and the turn profile's model. Gates are the
// decisions currently waiting on the owner.
type StatusCheck func(content StatusContent, known []string, gates []OwnerGate) error

// OwnerGate identifies a decision currently waiting on the owner.
type OwnerGate struct {
	Kind      string `json:"kind"`
	Reference string `json:"reference"`
	Reason    string `json:"reason,omitempty"`
}

// SetStatus stores content as the next status revision of the scope's
// workstream. The scope must name this session's active, uncaptured turn of a
// chief-of-staff thread. A check error is returned as *StatusRejected and
// stores nothing. It returns the stored revision.
func (r *Repository) SetStatus(ctx context.Context, agent string, scope coreadapter.Scope, content StatusContent, at time.Time, check StatusCheck) (int, error) {
	if at.IsZero() || check == nil {
		return 0, fmt.Errorf("timestamp and status check required")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	if scope.Role != StatusRole {
		return 0, fmt.Errorf("only a chief-of-staff turn may set the status")
	}
	_, q, err := r.turnScope(agent, scope, true)
	if err != nil {
		return 0, err
	}
	stream := config.WorkstreamID(scope.Workstream)
	log, view, err := r.loadWorkflow(stream)
	if err != nil {
		return 0, err
	}
	known := []string{string(r.project), string(stream)}
	for id, t := range log.Threads {
		known = append(known, id, t.Identity.ThreadID, t.Session.ID)
		for _, turn := range t.Turns {
			known = append(known, turn.Request.TurnID, turn.Request.Profile.Model)
			if turn.Response != nil {
				known = append(known, turn.Response.Result.Session.ID)
			}
		}
	}
	records, _, err := r.scan()
	if err != nil {
		return 0, err
	}
	if err := check(content, known, ownerGates(records, stream, view)); err != nil {
		return 0, &StatusRejected{Reason: err.Error()}
	}
	revision := 1
	if latest := latestStatus(records, stream); latest != nil {
		revision = latest.Revision + 1
	}
	content.Agents = slices.Clone(content.Agents)
	v := Status{Header: Header{Schema: "osmia.trace.status", Version: Version, ID: StatusID, Revision: revision, Project: r.project, Workstream: stream,
		At: at, Actor: Actor{Kind: "agent", ID: agent}, Cause: q.Request.ID, Depth: q.Request.Depth + 1}, StatusContent: content}
	if err := r.append(ctx, v, true); err != nil {
		return 0, err
	}
	return revision, nil
}

func latestStatus(records []Record, stream config.WorkstreamID) *Status {
	var latest *Status
	for _, v := range records {
		if s, ok := v.(Status); ok && s.Workstream == stream {
			latest = &s
		}
	}
	return latest
}

// WorkstreamStatus is a workstream's latest status with the facts the trace
// owns. Status is nil until the chief of staff first writes one. State is the
// FeatureSubject workflow state, empty until one is recorded, and Subjects
// the state of every workflow subject that has one, read with it.
// Workspaces is the workspace backend the workstream was created on.
// OpenQuestions counts questions without a ruling that have not been routed
// to an amendment. Gates lists open owner
// decisions.
type WorkstreamStatus struct {
	Workstream    config.WorkstreamID
	Workspaces    string
	State         string
	Subjects      map[string]WorkflowState
	OpenQuestions int
	Gates         []OwnerGate
	Status        *Status
}

// Statuses reports every workstream in manifest order. It fails on any
// damaged record rather than report a partial view.
func (r *Repository) Statuses() ([]WorkstreamStatus, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	records, streams, err := r.scan()
	if err != nil {
		return nil, err
	}
	var out []WorkstreamStatus
	for _, stream := range streams {
		_, view, err := r.loadWorkflow(stream)
		if err != nil {
			return nil, err
		}
		asked, ruled := map[string]bool{}, map[string]bool{}
		for _, v := range records {
			if v.header().Workstream != stream {
				continue
			}
			switch q := v.(type) {
			case Question:
				asked[q.ID] = true
			case Ruling:
				ruled[q.QuestionID] = true
			}
		}
		open := 0
		for id := range asked {
			if !ruled[id] && view.states[QuestionSubject(id)].Value != QuestionRouted {
				open++
			}
		}
		workspaces, err := r.workspaces(stream)
		if err != nil {
			return nil, err
		}
		out = append(out, WorkstreamStatus{Workstream: stream, Workspaces: workspaces, State: view.states[FeatureSubject].Value, Subjects: view.states, OpenQuestions: open, Gates: ownerGates(records, stream, view), Status: latestStatus(records, stream)})
	}
	return out, nil
}

func ownerGates(records []Record, stream config.WorkstreamID, view *workflowView) []OwnerGate {
	gates := []OwnerGate{}
	states := view.states
	if states[FeatureSubject].Value != "abandoned" {
		for _, entry := range escalations(questions(records, view, stream)) {
			if entry.State == QuestionEscalated {
				gates = append(gates, OwnerGate{Kind: "escalation", Reference: fmt.Sprint(entry.Number)})
			}
		}
	}
	if states[FeatureSubject].Value == "in-shed" {
		pending := false
		for _, rec := range records {
			if d, ok := rec.(Document); ok && d.Workstream == stream {
				if d.Cause == "ratification-packet" {
					pending = true
				}
				if strings.HasSuffix(d.Path, "/ratification.json") {
					pending = false
				}
			}
		}
		if pending {
			gates = append(gates, OwnerGate{Kind: "ratification", Reference: string(stream)})
		}
	}
	if states[FeatureSubject].Value != "abandoned" {
		for subject, state := range states {
			if question, ok := strings.CutPrefix(subject, CharterSubject("")); ok && state.Value == CharterProposed {
				gates = append(gates, OwnerGate{Kind: "charter", Reference: question})
			}
		}
	}
	for subject, state := range states {
		if state.Value == "contested" && (strings.HasPrefix(subject, "unit-") || strings.HasPrefix(subject, "unit_")) {
			reference := strings.TrimPrefix(subject, "unit-")
			if strings.HasPrefix(subject, "unit_") {
				reference = subject
				for _, rec := range records {
					if transition, ok := rec.(Transition); ok && transition.Workstream == stream && transition.Subject == subject && transition.Unit != "" {
						reference = transition.Unit
					}
				}
			}
			gate := OwnerGate{Kind: "contested", Reference: reference}
			for _, rec := range records {
				if transition, ok := rec.(Transition); ok && transition.Workstream == stream && transition.Subject == subject && transition.To == "contested" {
					gate.Reason = ""
					if transition.From == "implementing" && transition.Actor.Kind == "service" && transition.Actor.ID == "mason" ||
						transition.From == "reviewing" && transition.Actor.Kind == "service" && transition.Actor.ID == "reviewer" && transition.Cause != subject+"-review" {
						gate.Reason = transition.Reason
					}
				}
			}
			gates = append(gates, gate)
		}
	}
	slices.SortFunc(gates, func(a, b OwnerGate) int {
		if a.Kind != b.Kind {
			return strings.Compare(a.Kind, b.Kind)
		}
		return strings.Compare(a.Reference, b.Reference)
	})
	return gates
}
