package trace

import (
	"context"
	"fmt"
	"time"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
)

// PriorityID is the record ID of every priority change of a workstream.
const PriorityID = "priority"

// PriorityChange records a change the chief of staff made to the project's
// runtime workstream priority order at the owner's request. The actor is the
// owner whose message the turn answered; Agent is the chief of staff that
// made the change in Turn. Order is the project's order in force after it.
type PriorityChange struct {
	Header
	Agent string                `json:"agent"`
	Turn  string                `json:"turn"`
	Order []config.WorkstreamID `json:"order"`
}

func (PriorityChange) traceRecord() {}

func (v PriorityChange) valid() bool {
	return v.ID == PriorityID && v.Unit == "" && v.Actor.Kind == "owner" && key(v.Agent) && key(v.Turn) && v.Order != nil && config.CheckWorkstreamIDs(v.Order...) == nil
}

// PriorityRefused is returned by SetPriority for a change that cannot be
// made. Reason is written for the chief of staff.
type PriorityRefused struct{ Reason string }

func (e *PriorityRefused) Error() string { return "priority refused: " + e.Reason }

// SetPriority records a priority change made in the scope's turn. The scope
// must name this session's active, uncaptured turn of a chief-of-staff
// thread, and that turn must answer a message from the owner; otherwise it
// returns *PriorityRefused and apply is not called. apply makes the change
// and returns the order in force after it, which is recorded; an error from
// apply records nothing and is returned as it is. It returns the record.
func (r *Repository) SetPriority(ctx context.Context, agent string, scope coreadapter.Scope, at time.Time, apply func() ([]config.WorkstreamID, error)) (PriorityChange, error) {
	if at.IsZero() || apply == nil {
		return PriorityChange{}, fmt.Errorf("timestamp and apply required")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return PriorityChange{}, err
	}
	if scope.Role != ChiefOfStaff {
		return PriorityChange{}, fmt.Errorf("only a chief-of-staff turn may set the priority")
	}
	_, q, err := r.turnScope(agent, scope, true)
	if err != nil {
		return PriorityChange{}, err
	}
	if q.Request.Actor.Kind != "owner" {
		return PriorityChange{}, &PriorityRefused{Reason: "only the owner can change the priority order; this turn does not answer a message from the owner"}
	}
	stream := config.WorkstreamID(scope.Workstream)
	records, _, err := r.scan()
	if err != nil {
		return PriorityChange{}, err
	}
	revision := 1
	for _, v := range records {
		if p, ok := v.(PriorityChange); ok && p.Workstream == stream {
			revision = p.Revision + 1
		}
	}
	v := PriorityChange{Header: Header{Schema: "osmia.trace.priority", Version: Version, ID: PriorityID, Revision: revision, Project: r.project, Workstream: stream,
		At: at, Actor: q.Request.Actor, Cause: q.Request.ID, Depth: q.Request.Depth + 1}, Agent: agent, Turn: scope.Turn, Order: []config.WorkstreamID{}}
	if err := validate(v); err != nil {
		return PriorityChange{}, err
	}
	order, err := apply()
	if err != nil {
		return PriorityChange{}, err
	}
	v.Order = append(v.Order, order...)
	if err := r.append(ctx, v, true); err != nil {
		return PriorityChange{}, err
	}
	return v, nil
}
