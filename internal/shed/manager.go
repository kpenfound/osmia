package shed

import (
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/trace"
)

// Decide is what the manager decided about one committee objection.
type Decide string

const (
	// Dismiss settles the objection on the owner's behalf: the record answers
	// it, so it blocks no longer and is not put to the owner.
	Dismiss Decide = "dismiss"
	// Escalate puts the objection to the owner at the ratification gate.
	Escalate Decide = "escalate"
)

// ManagerDecision is the manager's decision on one committee objection,
// with its note and the turn that decided.
type ManagerDecision struct {
	Objection string `json:"objection"`
	Decision  Decide `json:"decision"`
	Note      string `json:"note"`
	Turn      string `json:"turn,omitempty"`
	// DecidedBy is empty for the manager's own decision, or
	// trace.DecidedByJev with the Jev judgment it comes from.
	DecidedBy string `json:"decided_by,omitempty"`
	Judgment  string `json:"judgment,omitempty"`
}

// ManagerDecisions is what the manager decided during one round, against
// the revision the documents were at.
type ManagerDecisions struct {
	Version   int               `json:"version"`
	Round     int               `json:"round"`
	Revision  Pin               `json:"revision"`
	Decisions []ManagerDecision `json:"decisions"`
}

// ManagerPath is the workstream path of the manager's decisions of a
// round, and ManagerDocumentID its trace record ID.
func ManagerPath(round int) string       { return roundPath(round, trace.ManagerKey) }
func ManagerDocumentID(round int) string { return roundDocumentID(round, trace.ManagerKey) }

// IsManagerPath reports whether path is the workstream path of the manager's
// decisions of some round.
func IsManagerPath(path string) bool {
	return strings.HasPrefix(path, "shed/") && strings.HasSuffix(path, "/"+trace.ManagerKey+".json")
}

func (c ManagerDecisions) check() error {
	switch {
	case c.Version != Version:
		return fmt.Errorf("unsupported chief of staff decisions version %d", c.Version)
	case c.Round < 1:
		return errors.New("chief of staff decisions require a positive round")
	case c.Revision.Spec < 1 || c.Revision.Plan < 0:
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
		case !trace.ValidDecider(one.DecidedBy, one.Judgment) || one.DecidedBy == trace.DecidedByOwner:
			return fmt.Errorf("the decision on objection %s names the chief of staff or Jev with its judgment as its decider", one.Objection)
		case seen[one.Objection]:
			return fmt.Errorf("objection %s is decided twice", one.Objection)
		}
		seen[one.Objection] = true
	}
	return nil
}

// EncodeManagerDecisions returns the file content of valid decisions.
func EncodeManagerDecisions(c ManagerDecisions) ([]byte, error) {
	if err := c.check(); err != nil {
		return nil, err
	}
	if c.Decisions == nil {
		c.Decisions = []ManagerDecision{}
	}
	return encodeFile(c)
}

// ParseManagerDecisions reads a decisions file's content, refusing unknown
// fields and invalid decisions.
func ParseManagerDecisions(data []byte) (ManagerDecisions, error) {
	var c ManagerDecisions
	if err := decodeFile(data, &c, "chief of staff decisions"); err != nil {
		return ManagerDecisions{}, err
	}
	if len(c.Decisions) == 0 {
		c.Decisions = nil
	}
	return c, c.check()
}

// AllManagerDecisions returns the manager's decisions of the workstream,
// ordered by the round they were made in. A decisions file whose content
// disagrees with its path is an error.
func AllManagerDecisions(repository *trace.Repository, stream config.WorkstreamID) ([]ManagerDecisions, error) {
	latest, err := shedFiles(repository, stream, trace.ManagerKey)
	if err != nil {
		return nil, err
	}
	var out []ManagerDecisions
	for path, d := range latest {
		c, err := ParseManagerDecisions([]byte(d.Content))
		if err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
		if ManagerPath(c.Round) != path {
			return nil, fmt.Errorf("%s: records the decisions of round %d", path, c.Round)
		}
		out = append(out, c)
	}
	slices.SortFunc(out, func(a, b ManagerDecisions) int { return a.Round - b.Round })
	return out, nil
}

// Undecided returns the committee objections of a dissent record that still
// need the manager's decision before the owner sees the record: those
// neither the owner nor the manager disposed of and the manager
// did not escalate. The owner's own objections are the owner's.
func Undecided(entries []Entry) []Entry {
	return slices.DeleteFunc(slices.Clone(entries), func(e Entry) bool {
		return e.Member == OwnerMember || e.Disposition != "" || e.Escalated
	})
}

// ForOwner returns the entries of a dissent record the owner is shown: every
// entry but those the manager dismissed.
func ForOwner(entries []Entry) []Entry {
	return slices.DeleteFunc(slices.Clone(entries), func(e Entry) bool { return e.By == trace.Manager })
}
