// Package shed holds the committee's contributions to a shed round: the
// record of what one member objected to and conceded against one pinned
// revision of the spec and the plan, the object and concede tools that build
// it, and the open dissent computed from the records of a workstream.
package shed

import (
	"bytes"
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/trace"
)

// Version is the schema version of a recorded contribution file.
const Version = 1

// Kind is what an objection tests the draft against.
type Kind string

const (
	// Charter says the part violates a charter rule. It is a veto.
	Charter Kind = "charter"
	// Fit says the plan does not realise the handed design, or works against
	// a decision the knowledge base holds. It is advice to the owner.
	Fit Kind = "fit"
	// Size says a unit addresses too much and is to be split.
	Size Kind = "size"
	// Proof says the plan names no proof that can show a criterion holds.
	Proof Kind = "proof"
)

// Kinds lists the objection kinds in the order prompts and schemas give them.
var Kinds = []Kind{Charter, Fit, Size, Proof}

// Pin is the revision of spec.md and of plan.json a round runs against.
type Pin struct {
	Spec int `json:"spec"`
	Plan int `json:"plan"`
}

// After reports whether p is a later revision of the documents than q: some
// document is newer and none is older.
func (p Pin) After(q Pin) bool { return p != q && p.Spec >= q.Spec && p.Plan >= q.Plan }

func (p Pin) String() string {
	return fmt.Sprintf("spec.md revision %d and plan.json revision %d", p.Spec, p.Plan)
}

// Objection is one member's dissent on one part of the spec or the plan.
type Objection struct {
	ID        string   `json:"id"`
	Kind      Kind     `json:"kind"`
	Part      string   `json:"part"`
	Argument  string   `json:"argument"`
	Citations []string `json:"citations"`
}

// Concession withdraws or settles the member's earlier objection.
type Concession struct {
	Objection string `json:"objection"`
	Reason    string `json:"reason"`
}

// Record is what one member contributed to one round, and the revision the
// round was pinned to. The owner's own objections are a record of the same
// shape, under the owner's member name. A record without objections or concessions is a silent
// turn. Failure is why the member's turn did not end normally; what it
// contributed before failing is kept.
type Record struct {
	Version     int          `json:"version"`
	Round       int          `json:"round"`
	Member      string       `json:"member"`
	Revision    Pin          `json:"revision"`
	Turn        string       `json:"turn,omitempty"`
	Objections  []Objection  `json:"objections"`
	Concessions []Concession `json:"concessions"`
	Failure     string       `json:"failure,omitempty"`
}

// Silent reports whether the member's turn ended normally without objecting
// or conceding.
func (r Record) Silent() bool {
	return r.Failure == "" && len(r.Objections) == 0 && len(r.Concessions) == 0
}

var memberPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,63}$`)

// Path is the workstream path of a member's record of a round.
func Path(round int, member string) string {
	return "shed/round-" + strconv.Itoa(round) + "/" + member + ".json"
}

// DocumentID is the trace record ID of a member's record of a round.
func DocumentID(round int, member string) string {
	return "shed-round-" + strconv.Itoa(round) + "-" + member
}

// ObjectionID is the ID of the member's k-th objection of a round.
func ObjectionID(round int, member string, k int) string {
	return fmt.Sprintf("%s-r%d-%d", member, round, k)
}

func (r Record) check() error {
	switch {
	case r.Version != Version:
		return fmt.Errorf("unsupported shed record version %d", r.Version)
	case r.Round < 1:
		return errors.New("shed record requires a positive round")
	case !memberPattern.MatchString(r.Member) || reserved(r.Member):
		return fmt.Errorf("invalid shed member %q", r.Member)
	case r.Revision.Spec < 1 || r.Revision.Plan < 1:
		return errors.New("shed record requires the revision it was made against")
	case r.Owned() && (len(r.Concessions) > 0 || r.Failure != "" || r.Turn != ""):
		return errors.New("the owner's record holds objections alone: the owner runs no turn")
	}
	for i, o := range r.Objections {
		if o.ID != ObjectionID(r.Round, r.Member, i+1) {
			return fmt.Errorf("objection %d has ID %q, want %q", i+1, o.ID, ObjectionID(r.Round, r.Member, i+1))
		}
		if r.Owned() {
			if o.Kind != Owner || strings.TrimSpace(o.Argument) == "" {
				return fmt.Errorf("the owner's objection %s requires the %s kind and an argument", o.ID, Owner)
			}
			continue
		}
		if !slices.Contains(Kinds, o.Kind) || strings.TrimSpace(o.Part) == "" || strings.TrimSpace(o.Argument) == "" || len(o.Citations) == 0 {
			return fmt.Errorf("objection %s requires a kind, a part, an argument and a citation", o.ID)
		}
	}
	for _, c := range r.Concessions {
		if strings.TrimSpace(c.Objection) == "" || strings.TrimSpace(c.Reason) == "" {
			return errors.New("a concession requires an objection and a reason")
		}
	}
	return nil
}

// Owned reports whether the record holds the owner's own objections rather
// than one committee member's contribution.
func (r Record) Owned() bool { return r.Member == OwnerMember }

// Encode returns the file content of a valid record.
func Encode(r Record) ([]byte, error) {
	if err := r.check(); err != nil {
		return nil, err
	}
	if r.Objections == nil {
		r.Objections = []Objection{}
	}
	if r.Concessions == nil {
		r.Concessions = []Concession{}
	}
	var b bytes.Buffer
	e := json.NewEncoder(&b)
	e.SetEscapeHTML(false)
	e.SetIndent("", "  ")
	if err := e.Encode(r); err != nil {
		return nil, err
	}
	return b.Bytes(), nil
}

// Parse reads a record's file content, refusing unknown fields and invalid
// records.
func Parse(data []byte) (Record, error) {
	var r Record
	d := json.NewDecoder(bytes.NewReader(data))
	d.DisallowUnknownFields()
	if err := d.Decode(&r); err != nil {
		return Record{}, fmt.Errorf("shed record: %w", err)
	}
	if err := d.Decode(new(any)); err != io.EOF {
		return Record{}, errors.New("shed record must be one object")
	}
	if len(r.Objections) == 0 {
		r.Objections = nil
	}
	if len(r.Concessions) == 0 {
		r.Concessions = nil
	}
	return r, r.check()
}

// Records returns the latest revision of every member's contribution and of
// the owner's objections recorded under the workstream's shed/, ordered by
// round and member. A record whose
// content disagrees with its path is an error.
func Records(repository *trace.Repository, stream config.WorkstreamID) ([]Record, error) {
	documents, err := trace.Read[trace.Document](repository, stream)
	if err != nil {
		return nil, err
	}
	latest := map[string]trace.Document{}
	for _, d := range documents {
		if strings.HasPrefix(d.Path, "shed/") && !reservedPath(d.Path) {
			latest[d.Path] = d
		}
	}
	var out []Record
	for path, d := range latest {
		r, err := Parse([]byte(d.Content))
		if err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
		if Path(r.Round, r.Member) != path {
			return nil, fmt.Errorf("%s: records round %d of member %s", path, r.Round, r.Member)
		}
		out = append(out, r)
	}
	sortRecords(out)
	return out, nil
}

func sortRecords(records []Record) {
	slices.SortStableFunc(records, func(a, b Record) int {
		return cmp.Or(cmp.Compare(a.Round, b.Round), strings.Compare(a.Member, b.Member))
	})
}

// Dissent is an objection that still stands, with who raised it, in which
// round and against which revision.
type Dissent struct {
	Objection
	Member   string `json:"member"`
	Round    int    `json:"round"`
	Revision Pin    `json:"revision"`
}

// Blocking reports whether the dissent stands in the way of ratification: the
// owner's own objection, a charter veto, or a size or proof objection the
// architect has to settle. A fit objection is advice and never blocks. The
// owner's ruling on the objection overrides this.
func (d Dissent) Blocking() bool { return d.Kind != Fit }

// Entry is one line of the dissent record: an objection that stands, whether
// it blocks, and what the owner ruled about it.
type Entry struct {
	Dissent
	Blocking    bool        `json:"blocking"`
	Disposition Disposition `json:"disposition,omitempty"`
	Note        string      `json:"note,omitempty"`
}

// DissentRecord is the dissent that stands after the given records, each
// objection with its kind, member, part, the owner's ruling on it and whether
// it blocks. A sustained objection blocks whatever its kind; a dismissed one
// blocks no longer and is kept as the owner's recorded disposition.
func DissentRecord(records []Record, rulings []Rulings) []Entry {
	ruled := map[string]Ruling{}
	for _, one := range flatten(rulings) {
		ruled[one.Objection] = one
	}
	open := OpenDissent(records)
	entries := make([]Entry, len(open))
	for i, d := range open {
		entries[i] = Entry{Dissent: d, Blocking: d.Blocking()}
		if r, ok := ruled[d.ID]; ok {
			entries[i].Disposition, entries[i].Note = r.Disposition, r.Note
			entries[i].Blocking = r.Disposition == Sustained
		}
	}
	return entries
}

// Standing returns the entries of a dissent record the owner has not
// dismissed: what the architect still answers and the debate still runs for.
func Standing(entries []Entry) []Entry {
	return slices.DeleteFunc(slices.Clone(entries), func(e Entry) bool { return e.Disposition == Dismissed })
}

// OpenDissent computes the dissent that stands after the given records,
// including the owner's own objections, which no member's turn settles. An
// objection stands until its member concedes it, or until the member accepts
// a later revision of the documents: a turn that ends normally against the
// later revision without a new objection. A silent turn against the revision
// the objection was made on changes nothing, and neither does a failed turn,
// which accepts nothing. The result is ordered by round, member and objection.
func OpenDissent(records []Record) []Dissent {
	ordered := slices.Clone(records)
	sortRecords(ordered)
	var open []Dissent
	for _, r := range ordered {
		for _, o := range r.Objections {
			open = append(open, Dissent{Objection: o, Member: r.Member, Round: r.Round, Revision: r.Revision})
		}
		accepts := r.Failure == "" && len(r.Objections) == 0
		open = slices.DeleteFunc(open, func(d Dissent) bool {
			if d.Member != r.Member {
				return false
			}
			conceded := slices.ContainsFunc(r.Concessions, func(c Concession) bool { return c.Objection == d.ID })
			return conceded || accepts && r.Revision.After(d.Revision)
		})
	}
	return open
}
