package shed

import (
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/trace"
)

// chiefName is the file name, without its extension, of the chief of staff's
// decisions on the committee's objections of a round.
const chiefName = "chief"

// ChiefOfStaff is who disposed of an objection the chief of staff dismissed
// on the owner's behalf.
const ChiefOfStaff = "chief_of_staff"

// Decide is what the chief of staff decided about one committee objection.
type Decide string

const (
	// Dismiss settles the objection on the owner's behalf: the record answers
	// it, so it blocks no longer and is not put to the owner.
	Dismiss Decide = "dismiss"
	// Escalate puts the objection to the owner at the ratification gate.
	Escalate Decide = "escalate"
)

// ChiefDecision is the chief of staff's decision on one committee objection,
// with its note and the turn that decided.
type ChiefDecision struct {
	Objection string `json:"objection"`
	Decision  Decide `json:"decision"`
	Note      string `json:"note"`
	Turn      string `json:"turn,omitempty"`
}

// ChiefDecisions is what the chief of staff decided during one round, against
// the revision the documents were at.
type ChiefDecisions struct {
	Version   int             `json:"version"`
	Round     int             `json:"round"`
	Revision  Pin             `json:"revision"`
	Decisions []ChiefDecision `json:"decisions"`
}

// ChiefPath is the workstream path of the chief of staff's decisions of a
// round, and ChiefDocumentID its trace record ID.
func ChiefPath(round int) string       { return roundPath(round, chiefName) }
func ChiefDocumentID(round int) string { return roundDocumentID(round, chiefName) }

func (c ChiefDecisions) check() error {
	switch {
	case c.Version != Version:
		return fmt.Errorf("unsupported chief of staff decisions version %d", c.Version)
	case c.Round < 1:
		return errors.New("chief of staff decisions require a positive round")
	case c.Revision.Spec < 1 || c.Revision.Plan < 1:
		return errors.New("chief of staff decisions require the revision they were made against")
	}
	seen := map[string]bool{}
	for _, one := range c.Decisions {
		switch {
		case strings.TrimSpace(one.Objection) == "":
			return errors.New("a decision requires the objection it decides")
		case one.Decision != Dismiss && one.Decision != Escalate:
			return fmt.Errorf("unsupported decision %q", one.Decision)
		case strings.TrimSpace(one.Note) == "":
			return fmt.Errorf("the decision on objection %s requires a note", one.Objection)
		case seen[one.Objection]:
			return fmt.Errorf("objection %s is decided twice", one.Objection)
		}
		seen[one.Objection] = true
	}
	return nil
}

// EncodeChiefDecisions returns the file content of valid decisions.
func EncodeChiefDecisions(c ChiefDecisions) ([]byte, error) {
	if err := c.check(); err != nil {
		return nil, err
	}
	if c.Decisions == nil {
		c.Decisions = []ChiefDecision{}
	}
	return encodeFile(c)
}

// ParseChiefDecisions reads a decisions file's content, refusing unknown
// fields and invalid decisions.
func ParseChiefDecisions(data []byte) (ChiefDecisions, error) {
	var c ChiefDecisions
	if err := decodeFile(data, &c, "chief of staff decisions"); err != nil {
		return ChiefDecisions{}, err
	}
	if len(c.Decisions) == 0 {
		c.Decisions = nil
	}
	return c, c.check()
}

// AllChiefDecisions returns the chief of staff's decisions of the workstream,
// ordered by the round they were made in. A decisions file whose content
// disagrees with its path is an error.
func AllChiefDecisions(repository *trace.Repository, stream config.WorkstreamID) ([]ChiefDecisions, error) {
	latest, err := shedFiles(repository, stream, chiefName)
	if err != nil {
		return nil, err
	}
	var out []ChiefDecisions
	for path, d := range latest {
		c, err := ParseChiefDecisions([]byte(d.Content))
		if err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
		if ChiefPath(c.Round) != path {
			return nil, fmt.Errorf("%s: records the decisions of round %d", path, c.Round)
		}
		out = append(out, c)
	}
	slices.SortFunc(out, func(a, b ChiefDecisions) int { return a.Round - b.Round })
	return out, nil
}

// Undecided returns the committee objections of a dissent record that still
// need the chief of staff's decision before the owner sees the record: those
// neither the owner nor the chief of staff disposed of and the chief of staff
// did not escalate. The owner's own objections are the owner's.
func Undecided(entries []Entry) []Entry {
	return slices.DeleteFunc(slices.Clone(entries), func(e Entry) bool {
		return e.Member == OwnerMember || e.Disposition != "" || e.Escalated
	})
}

// ForOwner returns the entries of a dissent record the owner is shown: every
// entry but those the chief of staff dismissed.
func ForOwner(entries []Entry) []Entry {
	return slices.DeleteFunc(slices.Clone(entries), func(e Entry) bool { return e.By == ChiefOfStaff })
}
