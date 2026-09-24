// Package amendment records how an approved amendment applies to the units
// of a sealed workstream, and answers whether a report or review recorded
// against an earlier seal still holds after the amendments applied since.
package amendment

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/plan"
	"github.com/kpenfound/osmia/internal/trace"
)

// Path returns the workstream path of an amendment's application record, and
// DocumentID its trace record ID.
func Path(id string) string       { return "amendments/" + id + "/application.json" }
func DocumentID(id string) string { return "amendment_" + id + "_application" }

// Pin names one revision of a workstream's sealed documents: the seal number,
// the revision of seal.json that records it, and the spec and plan revisions
// it seals.
type Pin struct {
	Seal         int `json:"seal"`
	SealRevision int `json:"seal_revision"`
	Spec         int `json:"spec"`
	Plan         int `json:"plan"`
}

// governs reports whether both pins name the same seal, spec and plan.
func (p Pin) governs(q Pin) bool { return p.Seal == q.Seal && p.Spec == q.Spec && p.Plan == q.Plan }

// Application is the document amendments/<n>/application.json. Its first
// revision is recorded with the resealing: the revisions the amendment moved
// the workstream From and To, the request and the owner's note, the changed
// criteria and affected proofs, and how the amendment's affected units are
// treated. Rework units address a changed criterion and return to
// implementing; Notify units had their plan entry changed while their
// criteria kept their meaning; Added units are new to the plan and Removed
// units left it. Its second revision adds Applied, what applying it recorded.
type Application struct {
	Amendment string   `json:"amendment"`
	From      Pin      `json:"from"`
	To        Pin      `json:"to"`
	Citations []string `json:"citations"`
	Change    string   `json:"change"`
	Reason    string   `json:"reason"`
	Note      string   `json:"note,omitempty"`
	Criteria  []string `json:"criteria"`
	Proofs    []string `json:"proofs"`
	Rework    []string `json:"rework"`
	Notify    []string `json:"notify"`
	Added     []string `json:"added"`
	Removed   []string `json:"removed"`
	Applied   *Applied `json:"applied,omitempty"`
}

// Applied is what applying an amendment recorded. Held units were waiting or
// contested and are reworked or notified once they leave that state;
// Followups are the follow-up units recorded for changed criteria of merged
// units; FinalReview is the final review whose report the amendment
// invalidated, zero when none.
type Applied struct {
	Held        []string `json:"held"`
	Followups   []string `json:"followups"`
	FinalReview int      `json:"final_review,omitempty"`
}

// Classify sorts an amendment's affected units into rework, notify, added
// and removed. A unit that addresses a changed criterion in either plan is
// reworked, even when the affected set does not list it; any other affected
// unit in both plans is notified.
func Classify(criteria, units []string, before, after plan.Plan) (rework, notify, added, removed []string) {
	rework, notify, added, removed = []string{}, []string{}, []string{}, []string{}
	changed := func(u plan.Unit) bool {
		return slices.ContainsFunc(u.Addresses, func(a plan.Address) bool { return slices.Contains(criteria, a.Criterion) })
	}
	names := slices.Clone(units)
	for _, p := range []plan.Plan{before, after} {
		for _, u := range p.Units {
			if changed(u) {
				names = append(names, u.ID)
			}
		}
	}
	slices.Sort(names)
	for _, id := range slices.Compact(names) {
		old, was := before.Unit(id)
		now, is := after.Unit(id)
		switch {
		case !was && is:
			added = append(added, id)
		case was && !is:
			removed = append(removed, id)
		case !was:
		case changed(old) || changed(now):
			rework = append(rework, id)
		default:
			notify = append(notify, id)
		}
	}
	return rework, notify, added, removed
}

// Names reports whether the application reworks or notifies the unit.
func (a Application) Names(unit string) bool {
	return slices.Contains(a.Rework, unit) || slices.Contains(a.Notify, unit)
}

// Read returns the latest revision of every recorded application of the
// workstream, in the order the amendments were resealed.
func Read(repository *trace.Repository, stream config.WorkstreamID) ([]Application, error) {
	docs, err := trace.Read[trace.Document](repository, stream)
	if err != nil {
		return nil, err
	}
	return FromDocuments(docs)
}

// FromDocuments is Read over documents already read from the trace.
func FromDocuments(docs []trace.Document) ([]Application, error) {
	latest := map[string]trace.Document{}
	for _, d := range docs {
		if id := idOf(d.Path); id != "" && d.ID == DocumentID(id) && d.Revision >= latest[d.ID].Revision {
			latest[d.ID] = d
		}
	}
	out := []Application{}
	for _, d := range latest {
		var a Application
		if err := json.Unmarshal([]byte(d.Content), &a); err != nil {
			return nil, fmt.Errorf("%s revision %d: %w", d.Path, d.Revision, err)
		}
		out = append(out, a)
	}
	slices.SortFunc(out, func(a, b Application) int { return a.To.SealRevision - b.To.SealRevision })
	return out, nil
}

// idOf returns the amendment an application path names, or "".
func idOf(path string) string {
	rest, ok := strings.CutPrefix(path, "amendments/")
	if !ok {
		return ""
	}
	id, ok := strings.CutSuffix(rest, "/application.json")
	if !ok || strings.Contains(id, "/") {
		return ""
	}
	return id
}

// CarriesReport reports whether a unit's report made under seal number
// reported still holds under seal number current: every seal between them
// was taken by an applied amendment, and none of those reworks the unit.
func CarriesReport(applications []Application, unit string, reported, current int) bool {
	for seal := reported; seal < current; seal++ {
		i := slices.IndexFunc(applications, func(a Application) bool { return a.From.Seal == seal && a.To.Seal == seal+1 })
		if i < 0 || slices.Contains(applications[i].Rework, unit) {
			return false
		}
	}
	return reported <= current
}

// CarriesReview reports whether a review of a unit against the reviewed seal,
// spec and plan still holds under the current ones: the amendments applied
// since lead from one to the other, and none of them names the unit or
// removes it. Pins are compared by seal, spec and plan only.
func CarriesReview(applications []Application, unit string, reviewed, current Pin) bool {
	// Every application moves the pin to later revisions, so the walk takes
	// at most one step per application.
	for range len(applications) {
		if reviewed.governs(current) {
			return true
		}
		i := slices.IndexFunc(applications, func(a Application) bool { return a.From.governs(reviewed) })
		if i < 0 {
			return false
		}
		a := applications[i]
		if a.Names(unit) || slices.Contains(a.Removed, unit) {
			return false
		}
		reviewed = a.To
	}
	return reviewed.governs(current)
}
