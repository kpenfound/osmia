package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/kpenfound/osmia/internal/api"
	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/execution"
	"github.com/kpenfound/osmia/internal/intake"
	"github.com/kpenfound/osmia/internal/lifecycle"
	"github.com/kpenfound/osmia/internal/router"
	"github.com/kpenfound/osmia/internal/trace"
)

type (
	RecoveryMove     = api.RecoveryMove
	RecoveryRequest  = api.RecoveryRequest
	RecoveryResponse = api.RecoveryResponse
	FeedRecovery     = api.FeedRecovery
)

const InboxRecovery = api.InboxRecovery

// The states of a recovery card as the feed shows it.
const (
	recoveryOpen     = "open"
	recoveryApproved = "approved"
	recoveryStale    = "stale"
)

// recoveryRecord is the document recovery/<digest>.json: the recovery move a
// stalled subject's card pins, written when the card opens, and its approval.
// A card for a move that belongs to the owner is derived from the router's
// record of the occurrence; its approval is recorded here as well, so a move
// that changes no state still closes its card.
type recoveryRecord struct {
	api.RecoveryMove
	State    string    `json:"state"`
	Opened   time.Time `json:"opened"`
	Approved time.Time `json:"approved,omitempty"`
}

func recoveryDocument(occurrence string) string { return "recovery-" + router.Name(occurrence) }

func recoveryRecordPath(occurrence string) string {
	return "recovery/" + router.Name(occurrence) + ".json"
}

// recoveryCard is one recovery card: its move, when it was opened and where it
// stands. Current is the state of the move's subject now.
type recoveryCard struct {
	Move    api.RecoveryMove
	Opened  time.Time
	State   string
	Current trace.WorkflowState
}

// readRecoveryRecords returns the latest revision of each recovery record of
// the workstream by occurrence, with the revision, in the order opened.
func readRecoveryRecords(repo *trace.Repository, stream config.WorkstreamID) (map[string]recoveryRecord, map[string]int, []string, error) {
	docs, err := trace.Read[trace.Document](repo, stream)
	if err != nil {
		return nil, nil, nil, err
	}
	latest := map[string]trace.Document{}
	var order []string
	for _, d := range docs {
		if !strings.HasPrefix(d.Path, "recovery/") {
			continue
		}
		if prior, ok := latest[d.ID]; !ok {
			order = append(order, d.ID)
			latest[d.ID] = d
		} else if d.Revision > prior.Revision {
			latest[d.ID] = d
		}
	}
	records, revisions := map[string]recoveryRecord{}, map[string]int{}
	var occurrences []string
	for _, id := range order {
		var rec recoveryRecord
		if err := json.Unmarshal([]byte(latest[id].Content), &rec); err != nil {
			return nil, nil, nil, fmt.Errorf("recovery record %s is damaged: %w", id, err)
		}
		records[rec.Occurrence], revisions[rec.Occurrence] = rec, latest[id].Revision
		occurrences = append(occurrences, rec.Occurrence)
	}
	return records, revisions, occurrences, nil
}

// writeRecoveryRecord records rec as the next revision of its document.
func writeRecoveryRecord(ctx context.Context, repo *trace.Repository, stream config.WorkstreamID, rec recoveryRecord, revision int, actor trace.Actor, at time.Time) error {
	data, err := json.MarshalIndent(rec, "", "  ")
	if err != nil {
		return err
	}
	cause := "recovery-" + rec.State
	h := trace.Header{Schema: "osmia.trace.document", Version: trace.Version, ID: recoveryDocument(rec.Occurrence), Revision: revision + 1, Project: repo.Project(), Workstream: stream, Unit: rec.Unit, At: at, Actor: actor, Cause: cause}
	return repo.RecordDocuments(ctx, []trace.Document{{Header: h, Path: recoveryRecordPath(rec.Occurrence), Content: string(data) + "\n"}})
}

// recoveryWorkstreamStates maps the targets of the exits that can be sent to the owner
// to the feature state the owner's move names.
var recoveryWorkstreamStates = map[string]string{
	intake.TargetRedraft:        SketchedState,
	intake.TargetDebateRound:    InShedState,
	intake.TargetReopenBuilding: BuildingState,
	intake.TargetFreshReview:    AssembledState,
	BuildingState:               BuildingState,
	AssembledState:              AssembledState,
	DeliveredState:              DeliveredState,
	RatifiedState:               RatifiedState,
	AbandonedState:              AbandonedState,
}

// recoverable reports whether the owner has a move for a target of a machine:
// the owner's moves of units and workstreams, and the drop of a dependency.
func recoverable(machine, target string) bool {
	switch machine {
	case execution.UnitMachine:
		return target != "" && target != execution.TargetWait && target != UnitMerged
	case intake.WorkstreamMachine, intake.FinalMachine:
		_, ok := recoveryWorkstreamStates[target]
		return ok
	case lifecycle.DependencyMachine:
		return target == lifecycle.DependencyWaived || target == lifecycle.DependencyAdopted
	}
	return false
}

// dependencyWaitExit reports whether the trigger is one of the exits from a
// dependency wait that the owner clears by dropping the dependency.
func dependencyWaitExit(trigger string) bool {
	return trigger == lifecycle.TriggerUpstreamTerminal || trigger == lifecycle.TriggerUpstreamIntegrated || trigger == lifecycle.TriggerBaseUnavailable
}

// ownerBound reports whether the record is a move that belongs to the owner and
// waits on the recovery card: a proposal that carries no authority for the gate
// it needs and has no card of its own, or a move Jev judged would change what
// the owner approved.
func ownerBound(rec router.Record) bool {
	return rec.State == router.StateOwner && (rec.OwnerCard == "" || rec.OwnerCard == router.RecoveryCard) && recoverable(rec.Machine, rec.Target)
}

func recordMove(rec router.Record, target, origin, reason string) api.RecoveryMove {
	return api.RecoveryMove{Occurrence: rec.Occurrence, Machine: rec.Machine, Subject: rec.Subject, Unit: rec.Unit, From: rec.From, Version: rec.Version, Target: target, Origin: origin, Reason: reason}
}

// ownerBoundReason says why the owner decides the move.
func ownerBoundReason(rec router.Record) (string, string) {
	if rec.Hold == router.HoldIntent {
		return api.RecoveryIntent, fmt.Sprintf("Jev judged that moving %s to %s would change what you approved, so the move is yours to approve", rec.Subject, rec.Target)
	}
	return api.RecoveryOwner, fmt.Sprintf("moving %s to %s is an owner decision that a proposal cannot make", rec.Subject, rec.Target)
}

// recoveryCards lists every recovery card of the workstream, open or not, in the
// order they were opened. A card is open while the router's record or the stall
// it was opened for stands, the card is not approved, and its subject is still
// in the state and at the version the move pins; a subject that has moved on
// makes the card stale.
func (s *Service) recoveryCards(repo *trace.Repository, stream config.WorkstreamID) ([]recoveryCard, error) {
	records, err := router.Records(repo, stream)
	if err != nil {
		return nil, err
	}
	marks, _, order, err := readRecoveryRecords(repo, stream)
	if err != nil {
		return nil, err
	}
	window := s.about(repo).StallWindow()
	now := s.now()
	var out []recoveryCard
	seen := map[string]bool{}
	add := func(move api.RecoveryMove, opened time.Time) error {
		if seen[move.Occurrence] {
			return nil
		}
		seen[move.Occurrence] = true
		state, err := s.recoverySubjectState(repo, stream, move.Subject)
		if err != nil {
			return err
		}
		card := recoveryCard{Move: move, Opened: opened, State: recoveryOpen, Current: state}
		switch {
		case marks[move.Occurrence].State == recoveryApproved:
			card.State = recoveryApproved
		case state.Version != move.Version || state.Value != move.From:
			card.State = recoveryStale
		}
		out = append(out, card)
		return nil
	}
	for _, rec := range records {
		opened := rec.Decided
		if opened.IsZero() {
			opened = rec.Raised
		}
		switch {
		case ownerBound(rec):
			origin, reason := ownerBoundReason(rec)
			target := rec.Target
			if rec.Machine == lifecycle.DependencyMachine {
				target = lifecycle.DependencyWaived
			}
			if err := add(recordMove(rec, target, origin, reason), opened); err != nil {
				return nil, err
			}
		case rec.Machine == lifecycle.DependencyMachine && dependencyWaitExit(rec.Trigger) && window > 0 && (rec.State == router.StateHeld || rec.State == router.StateRefused) && now.Sub(opened) >= window:
			reason := fmt.Sprintf("the router held the exit from %s and neither Jev nor the chief of staff cleared it for %d minutes; dropping the dependency lets the workstream go on without waiting for it", rec.Subject, int(window/time.Minute))
			if err := add(recordMove(rec, lifecycle.DependencyWaived, api.RecoveryStall, reason), opened); err != nil {
				return nil, err
			}
		}
	}
	for _, occurrence := range order {
		mark := marks[occurrence]
		if err := add(mark.RecoveryMove, mark.Opened); err != nil {
			return nil, err
		}
	}
	slices.SortStableFunc(out, func(a, b recoveryCard) int { return a.Opened.Compare(b.Opened) })
	return out, nil
}

// recoveryEntries lists the recovery cards of the workstream that are open, as
// inbox entries. A card pins the exact move: the subject, the state and version
// it is in, the target and the reason; approving it applies that move and no
// other.
func (s *Service) recoveryEntries(repo *trace.Repository, w trace.WorkstreamStatus) ([]InboxEntry, error) {
	cards, err := s.recoveryCards(repo, w.Workstream)
	if err != nil {
		return nil, err
	}
	var out []InboxEntry
	for _, card := range cards {
		if card.State != recoveryOpen {
			continue
		}
		m := card.Move
		body := map[string]any{"occurrence": m.Occurrence, "subject": m.Subject, "from": m.From, "version": m.Version, "target": m.Target}
		if m.Unit != "" {
			body["unit"] = m.Unit
		}
		e := intake.Decision(InboxRecovery, w.Workstream, card.Opened, "/recovery/"+string(w.Workstream), body)
		e.Unit = m.Unit
		e.Recovery = &m
		e.Options = []string{api.RecoveryApprove}
		what := "workstream " + string(w.Workstream)
		if m.Unit != "" {
			what = "unit " + m.Unit
		} else if id, ok := changeOfSubject(m.Subject); ok {
			what = "workgroup change " + id
		}
		e.Question = fmt.Sprintf("Approve this recovery move: %s goes from %s (version %d) to %s. %s.", what, m.From, m.Version, m.Target, strings.ToUpper(m.Reason[:1])+m.Reason[1:])
		e.Blocked = strings.ToUpper(what[:1]) + what[1:] + "."
		if m.Origin == api.RecoveryStall {
			e.Blocked += " It has not moved since the stall was raised."
		}
		out = append(out, e)
	}
	return out, nil
}

// feedRecoveries is one feed entry for each recovery card of the workstream,
// placed where the card was opened.
func (s *Service) feedRecoveries(repo *trace.Repository, stream config.WorkstreamID) ([]FeedEntry, error) {
	cards, err := s.recoveryCards(repo, stream)
	if err != nil {
		return nil, err
	}
	out := make([]FeedEntry, 0, len(cards))
	for _, card := range cards {
		out = append(out, FeedEntry{Kind: "recovery", At: card.Opened, Recovery: &FeedRecovery{RecoveryMove: card.Move, State: card.State}})
	}
	return out, nil
}

// recoveryApproval serialises approvals, so a double click applies the move
// once.
var recoveryApproval sync.Mutex

// approveRecovery applies the move a recovery card pins as a pinned owner
// decision. The request must name the move the card shows, and the card must
// be open: a card whose subject has since changed state, or that was approved,
// is refused as a conflict and nothing is applied. The move is then applied
// through the owner's own path for its machine (the owner's unit and workstream
// moves, and the drop of a dependency), which submits it to the router as a pinned
// move: it asks Jev nothing, runs the apply-time check and the target's shared
// effects, and records the owner as the decider and the authority.
func (s *Service) approveRecovery(ctx context.Context, raw string, req RecoveryRequest) (RecoveryResponse, *APIError) {
	project, stream, repo, traceErr := s.conversationTrace(raw)
	if traceErr != nil {
		return RecoveryResponse{}, traceErr
	}
	failed := &APIError{Code: Internal, Message: fmt.Sprintf("cannot read the recovery cards of workstream %s; check the trace repository", stream)}
	recoveryApproval.Lock()
	defer recoveryApproval.Unlock()
	cards, err := s.recoveryCards(repo, stream)
	if err != nil {
		return RecoveryResponse{}, failed
	}
	i := slices.IndexFunc(cards, func(c recoveryCard) bool { return c.Move.Occurrence == req.Occurrence })
	if i < 0 {
		return RecoveryResponse{}, &APIError{Code: Conflict, Message: fmt.Sprintf("no recovery card of workstream %s names that move; read the inbox again", stream)}
	}
	card := cards[i]
	m := card.Move
	switch card.State {
	case recoveryApproved:
		return RecoveryResponse{}, &APIError{Code: Conflict, Message: fmt.Sprintf("the recovery move of %s to %s was approved already", m.Subject, m.Target)}
	case recoveryStale:
		return RecoveryResponse{}, &APIError{Code: Conflict, Message: fmt.Sprintf("the recovery move is stale: %s was %s at version %d when the card was opened and is %s at version %d now, so the move to %s was not applied; read the inbox again", m.Subject, m.From, m.Version, card.Current.Value, card.Current.Version, m.Target)}
	}
	if req.Subject != m.Subject || req.Unit != m.Unit || req.From != m.From || req.Version != m.Version || req.Target != m.Target {
		return RecoveryResponse{}, &APIError{Code: Conflict, Message: fmt.Sprintf("the request does not pin the move the recovery card shows (%s from %s at version %d to %s); read the inbox again", m.Subject, m.From, m.Version, m.Target)}
	}
	note := "Approved on the recovery card: " + m.Reason
	detail, apiErr := s.applyRecovery(ctx, project, stream, repo, m, note)
	if apiErr != nil {
		return RecoveryResponse{}, apiErr
	}
	// The move is recorded as approved so that a move that changes no state
	// closes its card too.
	at := s.now()
	marks, revisions, _, err := readRecoveryRecords(repo, stream)
	if err != nil {
		return RecoveryResponse{}, failed
	}
	rec, ok := marks[m.Occurrence]
	if !ok {
		rec = recoveryRecord{RecoveryMove: m, Opened: card.Opened}
	}
	rec.State, rec.Approved = recoveryApproved, at
	if err := writeRecoveryRecord(ctx, repo, stream, rec, revisions[m.Occurrence], ownerActor, at); err != nil && !errors.Is(err, trace.ErrConflict) {
		return RecoveryResponse{}, &APIError{Code: Internal, Message: fmt.Sprintf("the move was applied but cannot record its approval on workstream %s; check the trace repository", stream)}
	}
	return RecoveryResponse{Project: project, Workstream: stream, Occurrence: m.Occurrence, Subject: m.Subject, Unit: m.Unit, From: m.From, Target: m.Target, Detail: detail, At: at}, nil
}

// applyRecovery applies the pinned move through the owner's path for its
// machine and reports what it did.
func (s *Service) applyRecovery(ctx context.Context, project config.ProjectID, stream config.WorkstreamID, repo *trace.Repository, m api.RecoveryMove, note string) (string, *APIError) {
	if id, ok := changeOfSubject(m.Subject); ok {
		if m.Target != changeStateTarget {
			return "", &APIError{Code: Conflict, Message: fmt.Sprintf("the owner has no move for %s to %s", m.Subject, m.Target)}
		}
		if apiErr := s.lifecycle().EndStalledChange(ctx, stream, id, m.From, m.Version, note); apiErr != nil {
			return "", apiErr
		}
		return fmt.Sprintf("Ended workgroup change %s as failed; a new change can be requested.", id), nil
	}
	switch m.Machine {
	case execution.UnitMachine:
		out, apiErr := s.execution().MoveUnit(ctx, repo, stream, m.Unit, UnitMoveRequest{To: m.Target, Note: note}, ownerActor, "")
		if apiErr != nil {
			return "", apiErr
		}
		return fmt.Sprintf("Moved unit %s from %s to %s.", m.Unit, out.Move.From, out.Move.To), nil
	case intake.WorkstreamMachine, intake.FinalMachine:
		to, ok := recoveryWorkstreamStates[m.Target]
		if !ok {
			break
		}
		out, apiErr := s.lifecycle().MoveWorkstream(ctx, repo, stream, WorkstreamMoveRequest{To: to, Note: note}, ownerActor, "")
		if apiErr != nil {
			return "", apiErr
		}
		return fmt.Sprintf("Moved workstream %s to %s: %s", stream, out.State, out.Detail), nil
	case lifecycle.DependencyMachine:
		upstream, edge, ok := dependencyOf(m.Subject)
		if !ok {
			break
		}
		out, apiErr := s.lifecycle().DropDependencyOf(ctx, project, stream, repo, lifecycle.DropDependencyRequest{Upstream: upstream, Type: edge})
		if apiErr != nil {
			return "", apiErr
		}
		return fmt.Sprintf("Dropped the %s dependency of workstream %s on %s.", out.Type, out.Workstream, out.Upstream), nil
	}
	return "", &APIError{Code: Conflict, Message: fmt.Sprintf("the owner has no move for %s to %s", m.Subject, m.Target)}
}

// dependencyOf returns the upstream workstream and the dependency type a
// dependency decision's subject names.
func dependencyOf(subject string) (config.WorkstreamID, string, bool) {
	rest, ok := strings.CutPrefix(subject, "dependency-")
	if !ok {
		return "", "", false
	}
	upstream, edge, ok := strings.Cut(rest, "-")
	if !ok || upstream == "" || edge == "" {
		return "", "", false
	}
	return config.WorkstreamID(upstream), edge, true
}

// recoverySubjectState returns the state and version of the subject a recovery
// move pins: a workgroup change's state and journal revision, or the subject's
// workflow state.
func (s *Service) recoverySubjectState(repo *trace.Repository, stream config.WorkstreamID, subject string) (trace.WorkflowState, error) {
	if id, ok := changeOfSubject(subject); ok {
		return s.changeState(repo, stream, id)
	}
	return repo.RawWorkflow(stream, subject)
}

// recoveryTarget returns the state a recovery card moves a stalled subject to:
// the stage it is in again for a unit, a kick for a unit that has not started,
// a fresh final review for an assembled workstream, and assembled for a
// building workstream that has nothing left to build. A subject with no way to
// start again has no card.
func (w stallWatch) recoveryTarget(stream config.WorkstreamID, unit string, state string) string {
	if unit != "" {
		return execution.RecoveryTarget(state)
	}
	if t, ok := w.s.lifecycle().StalledRecovery(w.repository, stream, state); ok {
		return t.Name
	}
	return ""
}

// openRecovery opens the recovery card of a stall that neither Jev nor the chief
// of staff cleared: the router decided the stalled exit, the chief of staff was
// told, and the subject has not moved for a further stall window. The card pins
// the move that starts the stalled stage again. Opening is idempotent: the card
// of an occurrence is opened once.
func (w stallWatch) openRecovery(ctx context.Context, stream config.WorkstreamID, unit string, state trace.WorkflowState, rec router.Record, now time.Time, after time.Duration) error {
	if rec.State == router.StateOwner || rec.State == router.StateStale || rec.State == router.StateDecided {
		return nil
	}
	decided := rec.Decided
	if decided.IsZero() {
		decided = rec.Raised
	}
	if now.Sub(decided) < after || rec.Version != state.Version || rec.From != state.Value {
		return nil
	}
	target := w.recoveryTarget(stream, unit, state.Value)
	if target == "" {
		return nil
	}
	marks, revisions, _, err := readRecoveryRecords(w.repository, stream)
	if err != nil {
		return err
	}
	if _, opened := marks[rec.Occurrence]; opened {
		return nil
	}
	name := "workstream " + string(stream)
	if unit != "" {
		name = "unit " + unit
	}
	reason := fmt.Sprintf("%s has made no progress and neither Jev nor the chief of staff cleared the stall within a further %d minutes, so starting %s again is yours to approve", name, int(after/time.Minute), state.Value)
	if target != state.Value {
		reason = fmt.Sprintf("%s has made no progress and neither Jev nor the chief of staff cleared the stall within a further %d minutes, so moving it to %s is yours to approve", name, int(after/time.Minute), target)
	}
	move := recordMove(rec, target, api.RecoveryStall, reason)
	err = writeRecoveryRecord(ctx, w.repository, stream, recoveryRecord{RecoveryMove: move, State: recoveryOpen, Opened: now}, revisions[rec.Occurrence], stallActor, now)
	if errors.Is(err, trace.ErrConflict) {
		return nil
	}
	return err
}
