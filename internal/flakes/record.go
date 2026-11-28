package flakes

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/intake"
)

// Decision states of an occurrence.
const (
	// Pending: the manager has not acted.
	Pending = "pending"
	// Opening: a flake-fix workstream's hand-in key is reserved, and intake
	// may or may not have created the workstream.
	Opening = "opening"
	// Opened: the occurrence opened Workstream.
	Opened = "opened"
	// Joined: the group already had Workstream open.
	Joined = "joined"
	// Declined: the manager declined, for Reason.
	Declined = "declined"
)

// Decision is an occurrence's one durable decision. Once Opening, Opened,
// Joined or Declined it is never replaced by another; Opening only advances
// to Opened or Joined.
type Decision struct {
	State string    `json:"state"`
	At    time.Time `json:"at,omitzero"`
	// Generation and Key are the group generation and hand-in key reserved
	// when opening, which the occurrence keeps whatever the group does next.
	Generation int    `json:"generation,omitempty"`
	Key        string `json:"key,omitempty"`
	// Workstream is the workstream an Opening decision will create (the one
	// its key names), and the one an Opened or Joined decision names.
	Workstream config.WorkstreamID `json:"workstream,omitempty"`
	Reason     string              `json:"reason,omitempty"`
	// Notes are the manager's notes reserved with an Opening decision,
	// so an interrupted opening is completed with the notes it was given.
	Notes string `json:"notes,omitempty"`
}

// Final reports whether the decision can no longer change.
func (d Decision) Final() bool {
	return d.State == Opened || d.State == Joined || d.State == Declined
}

// Source says which run an occurrence comes from.
type Source struct {
	// Workstream is the workstream whose unit or final review ran the checks.
	Workstream config.WorkstreamID `json:"workstream"`
	// Unit is the unit that ran them; empty for a final review.
	Unit        string `json:"unit,omitempty"`
	FinalReview bool   `json:"final_review,omitempty"`
}

// Delivery marks a notice or surfacing as done.
type Delivery struct {
	At time.Time `json:"at"`
}

// Surfacing marks the occurrence's links as surfaced to an open workstream.
type Surfacing struct {
	Workstream config.WorkstreamID `json:"workstream"`
	At         time.Time           `json:"at"`
}

// Destinations of a decided occurrence's announcement. Each is delivered,
// marked and, if its workstream is archived, closed on its own.
const (
	// DestinationOrigin is the decision transition in the originating
	// workstream's trace, which its feed shows.
	DestinationOrigin = "origin"
	// DestinationTarget is the surfacing of a joined occurrence's links to the
	// open flake-fix workstream's manager. Surfaced marks it delivered.
	DestinationTarget = "target"
)

// Announcement marks the originating destination as written.
type Announcement struct {
	Destination string    `json:"destination"`
	At          time.Time `json:"at"`
}

// Moot records an announcement destination closed because its own workstream
// was archived, which deletes the trace and feed the announcement would go to.
// The occurrence's decision is unchanged.
type Moot struct {
	Destination string              `json:"destination"`
	Workstream  config.WorkstreamID `json:"workstream"`
	// ArchivedBy is who archived the workstream: "owner" or "chief_of_staff".
	ArchivedBy string    `json:"archived_by"`
	Note       string    `json:"note"`
	At         time.Time `json:"at"`
}

// Occurrence is the flaky checks of one run in one flake group.
type Occurrence struct {
	ID string `json:"id"`
	// Run is the run record's path in the originating workstream's trace.
	Run    string `json:"run"`
	Source Source `json:"source"`
	// Links are the flaky checks of the run that fall in this group.
	Links []string `json:"links"`
	// Evidence is the whole run's evidence.
	Evidence   Evidence  `json:"evidence"`
	RecordedAt time.Time `json:"recorded_at"`
	// FromFlakeFix is set when the run belongs to a flake-fix workstream.
	FromFlakeFix bool `json:"from_flake_fix,omitempty"`
	// Noticed marks the run's notice as sent.
	Noticed *Delivery `json:"noticed,omitempty"`
	// Surfaced marks the surfacings to open workstreams, one per workstream.
	Surfaced []Surfacing `json:"surfaced,omitempty"`
	// Announced marks the destinations of the decision's announcement that
	// were written, other than the surfacing Surfaced marks.
	Announced []Announcement `json:"announced,omitempty"`
	// Moot records the destinations closed because their workstream was
	// archived.
	Moot     []Moot   `json:"moot,omitempty"`
	Decision Decision `json:"decision"`
}

// IsAnnounced reports whether the originating destination was written.
func (o Occurrence) IsAnnounced(destination string) bool {
	return slices.ContainsFunc(o.Announced, func(a Announcement) bool { return a.Destination == destination })
}

// IsMoot reports whether the destination was closed as moot.
func (o Occurrence) IsMoot(destination string) bool {
	return slices.ContainsFunc(o.Moot, func(m Moot) bool { return m.Destination == destination })
}

// Closure is a flake-fix workstream the group has finished with.
type Closure struct {
	Workstream config.WorkstreamID `json:"workstream"`
	Generation int                 `json:"generation"`
	At         time.Time           `json:"at"`
}

// Open is a group's current open flake-fix workstream.
type Open struct {
	Workstream config.WorkstreamID `json:"workstream"`
	Generation int                 `json:"generation"`
	Key        string              `json:"key"`
	// Occurrence is the occurrence that opened it.
	Occurrence string    `json:"occurrence"`
	At         time.Time `json:"at"`
}

// Group is one flake group's record: its occurrences in the order they were
// recorded (its history), its generation and its open workstream.
type Group struct {
	Project    config.ProjectID `json:"project"`
	Group      string           `json:"group"`
	Unresolved bool             `json:"unresolved,omitempty"`
	// Generation advances each time the group's workstream is closed.
	Generation  int          `json:"generation"`
	Open        *Open        `json:"open,omitempty"`
	Closed      []Closure    `json:"closed,omitempty"`
	Occurrences []Occurrence `json:"occurrences"`
}

// Errors of the record.
var (
	// ErrUnknownOccurrence is an occurrence the group's record does not hold.
	ErrUnknownOccurrence = errors.New("unknown flake occurrence")
	// ErrNotReserved is the completion of a decision whose key was not reserved.
	ErrNotReserved = errors.New("the occurrence has no reserved hand-in key")
)

func (g *Group) find(run string) (*Occurrence, bool) {
	id := OccurrenceID(run, g.Group)
	for i := range g.Occurrences {
		if g.Occurrences[i].ID == id {
			return &g.Occurrences[i], true
		}
	}
	return nil, false
}

// Find returns a copy of the occurrence of run in the group.
func (g Group) Find(run string) (Occurrence, bool) {
	o, ok := g.find(run)
	if !ok {
		return Occurrence{}, false
	}
	return *o, true
}

// History returns the group's occurrences recorded before the one of run, in
// the order they were recorded. If run is not recorded, it is all of them.
func (g Group) History(run string) []Occurrence {
	id := OccurrenceID(run, g.Group)
	var out []Occurrence
	for _, o := range g.Occurrences {
		if o.ID == id {
			break
		}
		out = append(out, o)
	}
	return out
}

// OccurrenceInput is a run's flaky checks in one group.
type OccurrenceInput struct {
	// Group is the flake group's key, and Unresolved is set when it is a
	// Dagger Cloud name tied to no known check.
	Group        string
	Unresolved   bool
	Run          string
	Source       Source
	Links        []string
	Evidence     Evidence
	FromFlakeFix bool
}

func (g *Group) record(in OccurrenceInput, at time.Time) (Occurrence, bool, error) {
	if strings.TrimSpace(in.Run) == "" || in.Group != g.Group || len(in.Links) == 0 || in.Source.Workstream == "" {
		return Occurrence{}, false, fmt.Errorf("a flake occurrence needs its run record, its workstream and at least one flaky check")
	}
	if o, ok := g.find(in.Run); ok {
		return *o, false, nil
	}
	o := Occurrence{
		ID: OccurrenceID(in.Run, g.Group), Run: in.Run, Source: in.Source, Links: slices.Clone(in.Links),
		Evidence: in.Evidence, RecordedAt: at, FromFlakeFix: in.FromFlakeFix, Decision: Decision{State: Pending},
	}
	g.Occurrences = append(g.Occurrences, o)
	return o, true, nil
}

func (g *Group) reserve(run, notes string, at time.Time) (Decision, bool, error) {
	o, ok := g.find(run)
	if !ok {
		return Decision{}, false, ErrUnknownOccurrence
	}
	if o.Decision.State != Pending {
		return o.Decision, false, nil
	}
	if g.Open != nil {
		o.Decision = Decision{State: Joined, At: at, Workstream: g.Open.Workstream}
		return o.Decision, true, nil
	}
	key := HandInKey(g.Project, g.Group, g.Generation)
	o.Decision = Decision{State: Opening, At: at, Generation: g.Generation, Key: key, Workstream: intake.HandInWorkstream(g.Project, key), Notes: notes}
	return o.Decision, true, nil
}

func (g *Group) opened(run string, workstream config.WorkstreamID, at time.Time) (Decision, bool, error) {
	o, ok := g.find(run)
	if !ok {
		return Decision{}, false, ErrUnknownOccurrence
	}
	if o.Decision.State != Opening {
		if o.Decision.State == Pending {
			return Decision{}, false, ErrNotReserved
		}
		return o.Decision, false, nil
	}
	if workstream == "" {
		workstream = o.Decision.Workstream
	}
	d := o.Decision
	d.At, d.Workstream = at, workstream
	switch {
	case g.Open != nil && g.Open.Key == d.Key && g.Open.Occurrence != o.ID:
		d.State = Joined
	default:
		d.State = Opened
		if g.Open == nil && d.Generation == g.Generation {
			g.Open = &Open{Workstream: workstream, Generation: d.Generation, Key: d.Key, Occurrence: o.ID, At: at}
		}
	}
	o.Decision = d
	return d, true, nil
}

func (g *Group) join(run string, workstream config.WorkstreamID, at time.Time) (Decision, bool, error) {
	o, ok := g.find(run)
	if !ok {
		return Decision{}, false, ErrUnknownOccurrence
	}
	if o.Decision.State != Pending {
		return o.Decision, false, nil
	}
	if workstream == "" {
		return Decision{}, false, fmt.Errorf("joining needs a workstream")
	}
	o.Decision = Decision{State: Joined, At: at, Workstream: workstream}
	return o.Decision, true, nil
}

func (g *Group) decline(run, reason string, at time.Time) (Decision, bool, error) {
	o, ok := g.find(run)
	if !ok {
		return Decision{}, false, ErrUnknownOccurrence
	}
	if o.Decision.State != Pending {
		return o.Decision, false, nil
	}
	if strings.TrimSpace(reason) == "" {
		return Decision{}, false, fmt.Errorf("declining needs a reason")
	}
	o.Decision = Decision{State: Declined, At: at, Reason: strings.TrimSpace(reason)}
	return o.Decision, true, nil
}

func (g *Group) close(workstream config.WorkstreamID, at time.Time) bool {
	switch {
	case g.Open != nil && g.Open.Workstream == workstream:
		g.Closed = append(g.Closed, Closure{Workstream: workstream, Generation: g.Open.Generation, At: at})
		g.Generation = max(g.Generation, g.Open.Generation+1)
		g.Open = nil
		return true
	case g.Open == nil:
		for _, o := range g.Occurrences {
			if o.Decision.State == Opening && o.Decision.Generation == g.Generation && o.Decision.Workstream == workstream {
				g.Closed = append(g.Closed, Closure{Workstream: workstream, Generation: g.Generation, At: at})
				g.Generation++
				return true
			}
		}
	}
	return false
}

func (g *Group) markNoticed(run string, at time.Time) (bool, error) {
	o, ok := g.find(run)
	if !ok {
		return false, ErrUnknownOccurrence
	}
	if o.Noticed != nil {
		return false, nil
	}
	o.Noticed = &Delivery{At: at}
	return true, nil
}

func (g *Group) markSurfaced(run string, workstream config.WorkstreamID, at time.Time) (bool, error) {
	o, ok := g.find(run)
	if !ok {
		return false, ErrUnknownOccurrence
	}
	if slices.ContainsFunc(o.Surfaced, func(s Surfacing) bool { return s.Workstream == workstream }) {
		return false, nil
	}
	o.Surfaced = append(o.Surfaced, Surfacing{Workstream: workstream, At: at})
	return true, nil
}

func (g *Group) markAnnounced(run, destination string, at time.Time) (bool, error) {
	o, ok := g.find(run)
	if !ok {
		return false, ErrUnknownOccurrence
	}
	if o.IsAnnounced(destination) {
		return false, nil
	}
	o.Announced = append(o.Announced, Announcement{Destination: destination, At: at})
	return true, nil
}

func (g *Group) markMoot(run string, m Moot, at time.Time) (bool, error) {
	o, ok := g.find(run)
	if !ok {
		return false, ErrUnknownOccurrence
	}
	if o.IsMoot(m.Destination) || o.IsAnnounced(m.Destination) {
		return false, nil
	}
	m.At = at
	o.Moot = append(o.Moot, m)
	return true, nil
}
