package trace

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"slices"
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

// StatusContent is what the chief of staff writes. Attention may be empty;
// Agents holds one line per active agent and may be empty.
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
// turn and backend session IDs, and the turn profile's model.
type StatusCheck func(content StatusContent, known []string) error

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
	log, _, err := r.loadWorkflow(stream)
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
	if err := check(content, known); err != nil {
		return 0, &StatusRejected{Reason: err.Error()}
	}
	records, _, err := r.scan()
	if err != nil {
		return 0, err
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
// FeatureSubject workflow state, empty until one is recorded. OpenQuestions
// counts questions without a ruling.
type WorkstreamStatus struct {
	Workstream    config.WorkstreamID
	State         string
	OpenQuestions int
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
			if !ruled[id] {
				open++
			}
		}
		out = append(out, WorkstreamStatus{Workstream: stream, State: view.states[FeatureSubject].Value, OpenQuestions: open, Status: latestStatus(records, stream)})
	}
	return out, nil
}
