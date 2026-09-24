package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/kb"
	"github.com/kpenfound/osmia/internal/plan"
	"github.com/kpenfound/osmia/internal/runtime"
	"github.com/kpenfound/osmia/internal/scheduler"
	"github.com/kpenfound/osmia/internal/trace"
)

// The decisions of the mason controller on a ready unit.
const (
	DispatchStarted  = "started"
	DispatchDeferred = "deferred"
)

// The reasons a ready unit is deferred, in the order they take precedence.
const (
	// DeferPaused: a runtime pause covers the unit's workstream.
	DeferPaused = "paused"
	// DeferBlocked: the mason controller is blocked on a unit of the
	// workstream, the deferred unit itself or an implementing one.
	DeferBlocked = "blocked"
	// DeferEntangled: the unit is entangled with units in flight in its
	// workstream.
	DeferEntangled = "entangled"
	// DeferWorkstreamCap: capacity.per_workstream of the workstream's units
	// are implementing.
	DeferWorkstreamCap = "workstream-cap"
	// DeferPriority: every mason slot is taken and workstreams earlier in the
	// project's priority order have a unit to start first.
	DeferPriority = "priority"
	// DeferCapacity: every mason slot is taken.
	DeferCapacity = "capacity"
)

// UnitDispatch is the document units/<unit>/dispatch.json: the mason
// controller's latest decision on a ready unit. Version is the version of the
// unit's workflow state the decision was taken on, so a decision on a unit
// that has since moved no longer applies. Reason is set on a deferral, with
// Blockers for an entanglement, Blocked naming the blocked unit, Pause the
// pause in force, Workstreams the higher-priority workstreams waiting to
// start a unit, and Limit the capacity in force. Message says the same in
// plain language.
type UnitDispatch struct {
	Unit        string                `json:"unit"`
	Version     uint64                `json:"version"`
	Decision    string                `json:"decision"`
	Reason      string                `json:"reason,omitempty"`
	Blockers    []plan.StartBlocker   `json:"blockers,omitempty"`
	Blocked     string                `json:"blocked,omitempty"`
	Pause       *runtime.Pause        `json:"pause,omitempty"`
	Workstreams []config.WorkstreamID `json:"workstreams,omitempty"`
	Limit       int                   `json:"limit,omitempty"`
	Message     string                `json:"message"`
}

func dispatchDocument(unit string) string { return trace.UnitSubject(unit) + "-dispatch" }

func dispatchPath(unit string) string { return fmt.Sprintf("units/%s/dispatch.json", unit) }

// startedDispatch is the decision to start the unit from the given state.
func startedDispatch(unit string, state trace.WorkflowState) UnitDispatch {
	return UnitDispatch{Unit: unit, Version: state.Version, Decision: DispatchStarted, Message: fmt.Sprintf("Unit %s started implementing.", unit)}
}

// dispatchMessage returns the plain-language message of a deferral.
func dispatchMessage(d UnitDispatch) string {
	switch d.Reason {
	case DeferPaused:
		return fmt.Sprintf("Waits while a %s pause is in force, set by %s.", d.Pause.Target.Scope, d.Pause.Source)
	case DeferBlocked:
		if d.Blocked == d.Unit {
			return "Cannot start: the mason controller is blocked on this unit and tries again on every pass."
		}
		return fmt.Sprintf("Waits while the mason controller is blocked on unit %s of this workstream.", d.Blocked)
	case DeferEntangled:
		var parts []string
		for _, b := range d.Blockers {
			why := "their footprints overlap"
			switch b.Reason {
			case plan.DependencyReason:
				why = "one depends on the other"
			case plan.MappingReason:
				why = "a footprint does not resolve through the entity map"
			}
			parts = append(parts, fmt.Sprintf("%s (%s)", b.Unit, why))
		}
		return "Waits for units in flight it is entangled with: " + strings.Join(parts, ", ") + "."
	case DeferWorkstreamCap:
		return fmt.Sprintf("Waits for a slot in its workstream: %d of its units are implementing, the per-workstream cap.", d.Limit)
	case DeferPriority:
		ids := make([]string, len(d.Workstreams))
		for i, w := range d.Workstreams {
			ids[i] = string(w)
		}
		return fmt.Sprintf("Waits for a mason slot: all %d are in use, and higher-priority workstreams start first: %s.", d.Limit, strings.Join(ids, ", "))
	case DeferCapacity:
		return fmt.Sprintf("Waits for a mason slot: all %d are in use.", d.Limit)
	}
	return ""
}

// dispatchPass holds what the mason controller's pass ends with, from which
// it decides why each ready unit it did not start waits.
type dispatchPass struct {
	cfg      *config.Config
	pauses   []runtime.Pause
	rank     func(config.WorkstreamID) int
	entities kb.Map
	// candidates are the workstreams that may start a unit, as the pass
	// left them.
	candidates []building
	// held names, by workstream, the unit the controller is blocked on.
	held map[config.WorkstreamID]string
}

// deferral decides why a ready unit of the workstream waits. A unit of a
// workstream neither paused nor held that is entangled with nothing in flight
// and under its workstream's cap waits for a mason slot: the pass stopped
// starting units only once every slot was taken.
func (p dispatchPass) deferral(b building, unit string) (UnitDispatch, error) {
	d := UnitDispatch{Unit: unit, Version: b.states[trace.UnitSubject(unit)].Version, Decision: DispatchDeferred}
	pause, paused := scheduler.Pausing(p.pauses, p.cfg.Project.ID, b.stream)
	blocked, held := p.held[b.stream]
	switch {
	case paused:
		d.Reason, d.Pause = DeferPaused, &pause
	case held:
		d.Reason, d.Blocked = DeferBlocked, blocked
	default:
		decision, err := plan.DecideStart(b.plan, p.entities, unit, b.inFlight)
		if err != nil {
			return UnitDispatch{}, err
		}
		switch {
		case !decision.CanStart:
			d.Reason, d.Blockers = DeferEntangled, decision.Blockers
		case b.implementing >= p.cfg.Project.Capacity.PerWorkstream:
			d.Reason, d.Limit = DeferWorkstreamCap, p.cfg.Project.Capacity.PerWorkstream
		default:
			d.Reason, d.Limit = DeferCapacity, p.cfg.Capacity.Masons
			for _, c := range p.candidates {
				if c.stream == b.stream || p.rank(c.stream) >= p.rank(b.stream) || c.implementing >= p.cfg.Project.Capacity.PerWorkstream {
					continue
				}
				if _, ok, err := nextReady(c, p.entities); err != nil {
					return UnitDispatch{}, err
				} else if ok {
					d.Reason, d.Workstreams = DeferPriority, append(d.Workstreams, c.stream)
				}
			}
		}
	}
	d.Message = dispatchMessage(d)
	return d, nil
}

// latestDispatches returns the latest units/<unit>/dispatch.json revision of
// each unit of the workstream, by unit.
func latestDispatches(repository *trace.Repository, stream config.WorkstreamID) (map[string]trace.Document, error) {
	documents, err := trace.Read[trace.Document](repository, stream)
	if err != nil {
		return nil, err
	}
	out := map[string]trace.Document{}
	for _, d := range documents {
		if d.Unit != "" && d.ID == dispatchDocument(d.Unit) {
			out[d.Unit] = d
		}
	}
	return out, nil
}

// dispatchRevision returns the next revision of the unit's dispatch.json
// holding the decision, or false when its latest revision already holds it.
func (m *masons) dispatchRevision(stream config.WorkstreamID, previous trace.Document, d UnitDispatch) (trace.Document, bool, error) {
	data, err := json.MarshalIndent(d, "", "  ")
	if err != nil {
		return trace.Document{}, false, err
	}
	content := string(data) + "\n"
	if previous.Revision > 0 && previous.Content == content {
		return trace.Document{}, false, nil
	}
	h := trace.Header{Schema: "osmia.trace.document", Version: trace.Version, ID: dispatchDocument(d.Unit), Revision: previous.Revision + 1, Project: m.repository.Project(), Workstream: stream, Unit: d.Unit, At: m.s.now(), Actor: masonActor, Cause: trace.UnitSubject(d.Unit) + "-" + UnitReady}
	return trace.Document{Header: h, Path: dispatchPath(d.Unit), Content: content}, true, nil
}

// recordDeferrals records, in one commit per workstream, why each ready unit
// of the given workstreams waits, for each unit whose latest decision says
// otherwise. A write another writer got to first is left to the next pass.
func (m *masons) recordDeferrals(ctx context.Context, p dispatchPass, streams []building) error {
	for _, b := range streams {
		var docs []trace.Document
		var latest map[string]trace.Document
		for _, u := range dependencyOrder(b.plan) {
			if b.states[trace.UnitSubject(u.ID)].Value != UnitReady {
				continue
			}
			if latest == nil {
				var err error
				if latest, err = latestDispatches(m.repository, b.stream); err != nil {
					return fmt.Errorf("workstream %s: %w", b.stream, err)
				}
			}
			d, err := p.deferral(b, u.ID)
			if err != nil {
				return fmt.Errorf("workstream %s unit %s: %w", b.stream, u.ID, err)
			}
			doc, changed, err := m.dispatchRevision(b.stream, latest[u.ID], d)
			if err != nil {
				return err
			}
			if changed {
				docs = append(docs, doc)
			}
		}
		if len(docs) == 0 {
			continue
		}
		if err := m.repository.RecordDocuments(ctx, docs); err != nil && !errors.Is(err, trace.ErrConflict) {
			return fmt.Errorf("workstream %s: %w", b.stream, err)
		}
	}
	return nil
}

// currentDeferral returns the deferral a unit's latest dispatch.json records
// while it still applies: the unit is ready in the state the decision was
// taken on.
func currentDeferral(document trace.Document, state trace.WorkflowState) (*UnitDispatch, error) {
	if document.Revision == 0 || state.Value != UnitReady {
		return nil, nil
	}
	var d UnitDispatch
	if err := json.Unmarshal([]byte(document.Content), &d); err != nil {
		return nil, fmt.Errorf("unit %s dispatch: %w", document.Unit, err)
	}
	if d.Decision != DispatchDeferred || d.Version != state.Version {
		return nil, nil
	}
	return &d, nil
}
