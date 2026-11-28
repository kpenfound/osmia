// Package router commits workflow transitions. A caller raises an exit with
// its trigger, default target, legal targets and evidence; the router filters
// the targets through the hard prerequisites, takes Jev's judgments, decides
// the next state in code and commits it through the trace.
package router

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"time"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/jev"
	"github.com/kpenfound/osmia/internal/trace"
)

// Bar is the probability a target needs before the router departs from the
// default to it.
type Bar string

const (
	// BarModest is the bar of a retry or a send-back: a fixed constant of the
	// table (ModestBar).
	BarModest Bar = "modest"
	// BarStrict is the bar of a redraft, a reopen, another debate round, a
	// return to delivered and a dropped dependency: the global [jev]
	// decision_threshold.
	BarStrict Bar = "strict"
)

// Level names a probability a corroborating question has to reach.
type Level string

const (
	LevelModest Level = "modest"
	LevelStrict Level = "strict"
	// LevelHalf is the floor below which a proposition is judged false.
	LevelHalf Level = "half"
	// LevelLow is the ceiling at or under which a proposition is judged
	// unlikely.
	LevelLow Level = "low"
)

// Condition is one corroboration of a departure: the answer of one of the
// trigger's own questions has to be at least, or at most, a level.
type Condition struct {
	Question string
	AtLeast  Level
	AtMost   Level
}

// Target is one state an exit can enter.
type Target struct {
	Name string
	// Description is what the target does, one line, as Jev reads it.
	Description string
	// Reason is the transition's reason when the target is entered.
	Reason string
	// Bar is the bar for departing to the target from the default; the
	// default needs none. A target with no bar takes the modest one.
	Bar Bar
	// NeedsCandidate marks a target that works on a candidate (checks, review,
	// approval, landing): it is legal only while the subject has one. A target
	// that needs none, such as planned or ready, is never filtered by it.
	NeedsCandidate bool
	// Approval marks the approved target: legal only on an exit carrying the
	// authority of a satisfactory reviewer verdict for the current candidate
	// or an owner decision.
	Approval bool
	// Gate marks a target that is an owner gate (ratification, delivery
	// approval and publication, abandonment, a unit approved without
	// independent review, an intent change): legal only on an exit carrying an
	// owner decision or one of Jev's existing gate decisions.
	Gate bool
	// Works marks a target that starts agent work: a turn of a mason, a review,
	// a redraft, a round of debate. It is legal only while no
	// pause, spent budget or loop guard stops work on the subject's workstream,
	// whichever way the target is reached: a pinned owner or gate decision and
	// a default entered through its trigger's own records meet the same limits
	// as a pick.
	Works bool
	// Stay marks a target that leaves the subject waiting where it is. When
	// such a default is judged to be stuck, it is judged not to fit.
	Stay bool
	// State is the state of the subject the target enters when that is not
	// Name: Name is a fixed word the route span and Jev read ("redraft"), State
	// the value of the subject ("redraft-3"; "held-3" for a machine whose
	// states carry a number). A transition records the state it entered, so a
	// departure from the default shows in the trace.
	State string
	// NoMove marks a target whose entering moves nothing: the subject stays
	// where it is while the exit's records and effects are committed and the
	// occurrence is recorded. It is how a default of "wait" or "hold" is
	// followed.
	NoMove bool
	// Corroborate lists what the trigger's own answers must show for the
	// router to depart to the target.
	Corroborate []Condition
}

func (t Target) entered() string {
	if t.State != "" {
		return t.State
	}
	return t.Name
}

// Question is a question specific to the trigger, answered as a probability.
type Question struct {
	Name         string
	Instructions string
}

// Authority kinds. They are the DecidedBy values of the same names.
const (
	// AuthorityReviewer is a satisfactory reviewer verdict for the current
	// candidate. It makes approved legal, and only that.
	AuthorityReviewer = trace.DecidedByReviewer
	// AuthorityOwner is an owner decision: an owner card or command, or the
	// recovery card. The exit is a pinned move.
	AuthorityOwner = trace.DecidedByOwner
	// AuthorityJevGate is one of Jev's existing gate decisions. The exit is a
	// pinned move.
	AuthorityJevGate = trace.DecidedByJevGate
	// AuthorityChief is a move or ruling of the chief of staff, which works
	// outside the loop. The exit is a pinned move for every target but an
	// owner gate and approval, which it never authorises.
	AuthorityChief = trace.DecidedByManager
)

// Authority is the authority an exit carries. Only a reviewer verdict, the
// owner or one of Jev's existing gate decisions authorise a reserved target; a
// chief of staff's move pins any other target, and a decide-transition answer
// carries none.
type Authority struct {
	Kind string
	// Target is the target the authority authorises. A pinned move enters it
	// and no other; a reviewer verdict authorises approved.
	Target string
	// Judgment is the judgment record of a Jev gate decision.
	Judgment string
	// Ref names the record the authority rests on (a reviewer's verdict
	// document and revision, an owner decision), so the route journal records
	// what authorised the move.
	Ref string
	// Valid returns why the authority no longer holds when the target is
	// applied, or "" while it does: a verdict is still the latest and still
	// for the current candidate. A nil Valid always holds.
	Valid func(ctx context.Context) (string, error)
}

func (a *Authority) pinned() bool {
	return a != nil && (a.Kind == AuthorityOwner || a.Kind == AuthorityJevGate || a.Kind == AuthorityChief)
}

// Entry is one record of an exit's evidence: the kind of record, its ID and
// the text Jev reads. The router strips code from the text and caps it.
type Entry struct {
	Kind string `json:"kind"`
	ID   string `json:"id,omitempty"`
	Text string `json:"text"`
}

// Evidence is what Jev reads about an exit beyond the subject's own history,
// which the router reads from the trace. Facts hold numbers and words the
// caller computed in code (attempt counts, minutes since progress); none comes
// from a model.
type Evidence struct {
	// Intent is the sealed intent the subject serves.
	Intent  string
	Facts   map[string]any
	Records []Entry
	Sources []jev.Source
}

// Prerequisites are the hard prerequisites that depend on state outside the
// subject's own version: they can change without the subject moving, so the
// router evaluates them when it builds the request and again when it applies
// a target. Each returns why the target is not legal, or "" when it is; a
// non-nil error is a failure to read, and the router retries later instead of
// deciding.
type Prerequisites struct {
	// Candidate says whether the subject has a current candidate. It is asked
	// only for targets that need one; an exit with such a target and no
	// Candidate offers none of them.
	Candidate func(ctx context.Context) (string, error)
	// Operations reports a landing, rebase or publication in flight.
	Operations func(ctx context.Context, t Target) (string, error)
	// Limits reports an exhausted budget, a pause or the loop guard.
	Limits func(ctx context.Context, t Target) (string, error)
	// Other holds the machine's own prerequisites, such as merged only after
	// a landing.
	Other func(ctx context.Context, t Target) (string, error)
}

// Exit is one exit from a state of a routed machine.
type Exit struct {
	Repo       *trace.Repository
	Project    config.ProjectID
	Workstream config.WorkstreamID
	// Unit is the unit the exit belongs to, empty for a workstream exit.
	Unit string
	// Machine selects the shared effects of the targets: unit, ws, final,
	// contest, dep or drift.
	Machine string
	// Subject is the workflow subject whose state the exit changes, From its
	// current value and Version the version the caller read it at. A commit
	// with another version is refused, so the subject moves once.
	Subject string
	From    string
	Version uint64
	// Trigger is what raised the exit; it is machine.event, one of the triggers the route span lists.
	Trigger string
	// Cause identifies the record that raised the exit (a turn response, a
	// check run number, a review turn, a final review number), so that exits
	// that leave the state unchanged are different occurrences. HeaderCause,
	// when set, is the record the transition cites instead.
	Cause       string
	HeaderCause string
	// Actor is who the transition is attributed to.
	Actor trace.Actor
	// Note and Turn are the words of the owner or the chief of staff behind a
	// move and the turn that made it; the target's effect records them.
	Note string
	Turn string
	// Proposed is the target a proposal of the chief of staff asks for, which
	// may differ from the default when a policy sets the default to leave the
	// subject where it is. It is recorded, so a proposal recovered after a
	// restart is rebuilt with the target that was asked for.
	Proposed string
	// TransitionID, when set, names the transition into a target, for readers
	// that recognise a transition by its ID. It is stable for the exit.
	TransitionID func(Target) string
	// Enter, when set, builds how the trigger enters its default target: the
	// records that target has always had when the trigger raised it. A target
	// other than the default, however it was chosen, is entered through the
	// shared effect of its machine.
	Enter EffectFunc
	// Entering, when set, builds how this exit enters a target other than its
	// default, by the target's name, ahead of the machine's registered effect.
	// It serves a target whose effect depends on what the exit read when it was
	// raised, such as the evidence a dropped dependency rests on.
	Entering map[string]EffectFunc
	// Records, when set, builds the records of the trigger itself (the report
	// an exit cites, the failed check run it counts): they are written in the
	// commit of whichever target is entered, and in none when the exit is
	// held.
	Records EffectFunc
	// At is when the exit was raised; the zero value is the time it is routed.
	At time.Time
	// Default is the state today's code would enter. It is one of Targets.
	Default string
	// Targets are the states the exit can enter before the hard prerequisites
	// filter them.
	Targets   []Target
	Questions []Question
	Evidence  Evidence
	// Authority is the authority the exit carries, if any.
	Authority *Authority
	Prereq    Prerequisites
	// OwnerCard says, in a few words, where the owner decides a gate target the
	// exit proposes without the authority to enter it (the ratification card,
	// the delivery card); empty means the recovery card.
	OwnerCard string
}

// RecoveryCard names the card the owner decides a move on when it has no card of
// its own.
const RecoveryCard = "the recovery card"

// Pending reports whether routing the exit left the move as a pending owner
// decision: it belongs to the owner, and the owner decides it on a card.
func (r Result) Pending() bool { return r.Journal == StateOwner }

// Port is the router as a controller uses it: an exit that waits on a request
// is routed in place, and an exit a scheduling pass raises is handed to the
// router's worker, so Jev is never asked on the scheduling path.
type Port interface {
	Route(ctx context.Context, x Exit) (Result, error)
	Submit(x Exit, done func(Result, error)) bool
}

var triggerName = regexp.MustCompile(`^[a-z]+\.[a-z_]+$`)

// ValidTrigger reports whether trigger has the form of a trigger, machine.event.
func ValidTrigger(trigger string) bool { return triggerName.MatchString(trigger) }

// Actions of a verdict.
const (
	ActionCommit = "commit"
	ActionHold   = "hold"
	ActionOwner  = "owner"
)

// ErrCannotCarryOut marks an error of an effect that says the target cannot be
// carried out, as opposed to a failure to read or write that is retried. The
// router records it as a refusal and holds the subject.
var ErrCannotCarryOut = errors.New("the target cannot be carried out")

// Refusal returns an error saying the target cannot be carried out for reason.
func Refusal(reason string) error { return fmt.Errorf("%w: %s", ErrCannotCarryOut, reason) }

func (x Exit) validate() error {
	switch {
	case x.Repo == nil || x.Project == "" || x.Workstream == "":
		return errors.New("an exit needs a trace, a project and a workstream")
	case x.Subject == "" || x.Machine == "":
		return errors.New("an exit needs a subject and a machine")
	case !triggerName.MatchString(x.Trigger):
		return fmt.Errorf("%q is not a trigger", x.Trigger)
	case x.Default == "":
		return errors.New("an exit needs a default")
	}
	seen := map[string]bool{}
	for _, t := range x.Targets {
		if t.Name == "" || seen[t.Name] || t.Description == "" {
			return fmt.Errorf("target %q needs a name once and a description", t.Name)
		}
		seen[t.Name] = true
	}
	if !seen[x.Default] {
		return fmt.Errorf("the default %q is not among the targets", x.Default)
	}
	if a := x.Authority; a != nil {
		switch {
		case a.Kind != AuthorityReviewer && a.Kind != AuthorityOwner && a.Kind != AuthorityJevGate && a.Kind != AuthorityChief:
			return fmt.Errorf("%q is not an authority", a.Kind)
		case !seen[a.Target]:
			return fmt.Errorf("the authorised target %q is not among the targets", a.Target)
		case a.Kind == AuthorityJevGate && a.Judgment == "":
			return errors.New("a Jev gate decision names its judgment")
		case a.Kind == AuthorityReviewer && a.Target != x.Default:
			return errors.New("a reviewer verdict authorises the default approval")
		}
	}
	return nil
}

func (x Exit) defaultState() string {
	if t, ok := x.target(x.Default); ok {
		return t.entered()
	}
	return x.Default
}

func (x Exit) target(name string) (Target, bool) {
	for _, t := range x.Targets {
		if t.Name == name {
			return t, true
		}
	}
	return Target{}, false
}
