package router

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/trace"
)

// The states of an occurrence's route journal. The journal is the workflow
// subject route-<digest> and the document routes/<digest>.json, which one
// commit writes together: the document says what was decided, the subject's
// value says how far it was carried out, and the subject's transitions carry
// the notices to the chief of staff.
const (
	// StateDecided: Jev's answers were turned into a target that is recorded
	// but not yet applied. A restart applies it, without asking again.
	StateDecided = "decided"
	// StateCommitted: the target was entered; the move's transition and this
	// state were recorded together.
	StateCommitted = "committed"
	// StateHeld: the subject was held without moving.
	StateHeld = "held"
	// StateOwner: the move belongs to the owner and awaits their decision.
	StateOwner = "owner"
	// StateRefused: the target could not be applied; Refuse is recorded, nothing
	// was committed and the subject is held.
	StateRefused = "refused"
	// StateStale: the subject moved on since the exit was raised.
	StateStale = "stale"
)

// routeActor is who the router's own records are attributed to.
var routeActor = trace.Actor{Kind: "service", ID: "router"}

// Record is the content of an occurrence's route journal document.
type Record struct {
	Occurrence string `json:"occurrence"`
	Project    string `json:"project"`
	Workstream string `json:"workstream"`
	Unit       string `json:"unit,omitempty"`
	Machine    string `json:"machine"`
	Subject    string `json:"subject"`
	Trigger    string `json:"trigger"`
	Cause      string `json:"cause,omitempty"`
	From       string `json:"from"`
	Version    uint64 `json:"version"`
	Default    string `json:"default"`
	// Actor, Note, Turn and HeaderCause are what the exit's move is attributed
	// to, so a pick recovered after a restart is entered as the exit would
	// have entered it.
	Actor       trace.Actor `json:"actor,omitempty"`
	Note        string      `json:"note,omitempty"`
	Turn        string      `json:"turn,omitempty"`
	HeaderCause string      `json:"header_cause,omitempty"`
	// Offered are the legal targets Jev was offered, or the one target the
	// default was committed for when it was the only one.
	Offered []string `json:"offered,omitempty"`
	// Authority is the authority the exit carried: reviewer, owner or jev_gate.
	// AuthorityRef names the record it rests on.
	Authority    string `json:"authority,omitempty"`
	AuthorityRef string `json:"authority_ref,omitempty"`
	// Table and Rule say which version of the code table and which row chose
	// the state.
	Table string `json:"table,omitempty"`
	Rule  string `json:"rule,omitempty"`
	State string `json:"state"`
	// Outcome is the route span's outcome: followed, departed, held or
	// fallback.
	Outcome string `json:"outcome,omitempty"`
	// Target is the state entered, or for a held exit the move that was
	// held back or sent to the owner.
	Target    string `json:"target,omitempty"`
	DecidedBy string `json:"decided_by,omitempty"`
	// Judgment is the judgment record behind the decision; JevRequested says
	// Jev was asked, and Fallback how the request failed.
	Judgment     string `json:"judgment,omitempty"`
	JevRequested bool   `json:"jev_requested,omitempty"`
	Fallback     string `json:"fallback,omitempty"`
	Confidence   string `json:"confidence,omitempty"`
	// Hold is why the subject is held; Reason a sentence built by code on the
	// answers; Refusal why a target could not be applied.
	Hold    string `json:"hold,omitempty"`
	Reason  string `json:"reason,omitempty"`
	Refusal string `json:"refusal,omitempty"`
	// Transition is the ID of the move's transition once committed, and Entered
	// the state the subject was in once it was: the state of the target, or the
	// state it was left in when the target moves nothing.
	Transition string    `json:"transition,omitempty"`
	Entered    string    `json:"entered,omitempty"`
	Raised     time.Time `json:"raised"`
	Decided    time.Time `json:"decided,omitempty"`
}

// final reports whether the record is a finished occurrence.
func (r Record) final() bool {
	switch r.State {
	case StateCommitted, StateHeld, StateOwner, StateRefused, StateStale:
		return true
	}
	return false
}

// Finished reports whether the occurrence was decided: committed, held, sent to
// the owner, refused or found stale. A pass that raised an exit does not raise
// it again.
func (r Record) Finished() bool { return r.final() }

// OccurrenceKey is the key of an exit's occurrence: its subject, the version of
// the subject's state, the trigger and the record that raised it. The cause
// separates exits that leave the state unchanged. A cause that is long or has
// characters a key cannot hold is shortened to a digest.
func OccurrenceKey(x Exit) string {
	cause := x.Cause
	if len(cause) > 96 || strings.ContainsFunc(cause, func(r rune) bool { return r < ' ' || r == 0x7f }) {
		cause = fmt.Sprintf("%x", sha256.Sum256([]byte(cause)))[:16]
	}
	return fmt.Sprintf("%s@%d/%s/%s", x.Subject, x.Version, x.Trigger, cause)
}

// journalName is the digest the journal's subject, document and transitions
// are named by.
func journalName(key string) string {
	return fmt.Sprintf("%x", sha256.Sum256([]byte(key)))[:32]
}

func journalSubject(name string) string { return "route-" + name }

// IsJournal reports whether subject is the workflow subject of a route journal,
// whose transitions are the router's own bookkeeping and not a state change of
// the work.
func IsJournal(subject string) bool  { return strings.HasPrefix(subject, "route-") }
func journalPath(name string) string { return "routes/" + name + ".json" }

// readRecord returns the latest route record named name, its revision and the
// journal subject's workflow state. A missing record is nil.
func readRecord(repo *trace.Repository, stream config.WorkstreamID, name string) (*Record, int, trace.WorkflowState, error) {
	state, err := repo.Workflow(stream, journalSubject(name))
	if err != nil {
		return nil, 0, state, err
	}
	docs, err := trace.Read[trace.Document](repo, stream)
	if err != nil {
		return nil, 0, state, err
	}
	var latest trace.Document
	for _, d := range docs {
		if d.ID == journalSubject(name) && d.Revision > latest.Revision {
			latest = d
		}
	}
	if latest.Revision == 0 {
		return nil, 0, state, nil
	}
	var rec Record
	if err := json.Unmarshal([]byte(latest.Content), &rec); err != nil {
		return nil, 0, state, fmt.Errorf("route record %s is damaged: %w", name, err)
	}
	return &rec, latest.Revision, state, nil
}

// Lookup returns the route record of an exit's occurrence, nil when the exit
// was never routed.
func Lookup(repo *trace.Repository, stream config.WorkstreamID, x Exit) (*Record, error) {
	rec, _, _, err := readRecord(repo, stream, journalName(OccurrenceKey(x)))
	return rec, err
}

// Unfinished returns the route records of the workstream whose subject is held:
// held, sent to the owner or refused, in the order they were first written.
// They are the holds the stall watchdog and the recovery card pick up.
func Unfinished(repo *trace.Repository, stream config.WorkstreamID) ([]Record, error) {
	docs, err := trace.Read[trace.Document](repo, stream)
	if err != nil {
		return nil, err
	}
	latest := map[string]trace.Document{}
	var order []string
	for _, d := range docs {
		if !strings.HasPrefix(d.Path, "routes/") {
			continue
		}
		if prior, ok := latest[d.ID]; !ok {
			order = append(order, d.ID)
			latest[d.ID] = d
		} else if d.Revision > prior.Revision {
			latest[d.ID] = d
		}
	}
	var out []Record
	for _, id := range order {
		var rec Record
		if json.Unmarshal([]byte(latest[id].Content), &rec) != nil {
			continue
		}
		if rec.State == StateHeld || rec.State == StateOwner || rec.State == StateRefused {
			out = append(out, rec)
		}
	}
	return out, nil
}

// journalWrite is one commit of the journal: the next revision of the record,
// the journal subject moving to a state, and anything recorded with it.
type journalWrite struct {
	rec    Record
	rev    int
	from   trace.WorkflowState
	to     string
	reason string
	events []trace.Event
	// docs and txs are recorded in the same commit: the target's effects and
	// the move's own transaction.
	docs []trace.Document
	txs  []trace.Transaction
}

func (r *Router) headerFor(x Exit, schema, id string, revision int, at time.Time, cause string) trace.Header {
	return trace.Header{Schema: schema, Version: trace.Version, ID: id, Revision: revision, Project: x.Project, Workstream: x.Workstream, Unit: x.Unit, At: at, Actor: routeActor, Cause: cause}
}

// writeJournal records w in one commit and reports the transition ID of the
// journal subject's move.
func (r *Router) writeJournal(ctx context.Context, x Exit, name string, w journalWrite) error {
	subject := journalSubject(name)
	at := r.now()
	cause := x.Cause
	if x.HeaderCause != "" {
		cause = x.HeaderCause
	}
	if cause == "" {
		cause = x.Trigger
	}
	data, err := json.MarshalIndent(w.rec, "", "  ")
	if err != nil {
		return err
	}
	doc := trace.Document{Header: r.headerFor(x, "osmia.trace.document", subject, w.rev+1, at, cause), Path: journalPath(name), Content: string(data) + "\n"}
	id := subject + "-" + w.to
	tx := trace.Transaction{ExpectedVersion: w.from.Version, Events: w.events,
		Transition: trace.Transition{Header: r.headerFor(x, "osmia.trace.transition", id, 1, at, cause), Subject: subject, From: w.from.Value, To: w.to, Reason: w.reason}}
	docs := append(append([]trace.Document{}, w.docs...), doc)
	txs := append(append([]trace.Transaction{}, w.txs...), tx)
	_, err = x.Repo.RecordDocumentsWith(ctx, docs, txs...)
	return err
}
