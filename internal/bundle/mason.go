package bundle

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/kpenfound/osmia/internal/amendment"
	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/envelope"
	"github.com/kpenfound/osmia/internal/followup"
	"github.com/kpenfound/osmia/internal/plan"
	"github.com/kpenfound/osmia/internal/seal"
	"github.com/kpenfound/osmia/internal/trace"
)

// Mason is the bundle a mason builds one unit of a sealed workstream from:
// the spec at its sealed hash, the unit's criteria and planned proofs, its
// footprint and dependencies, the notices of approved amendments that affect
// it, and the project context scoped to its footprint.
type Mason struct {
	Workstream config.WorkstreamID `json:"workstream"`
	Seal       int                 `json:"seal"`
	Spec       SealedDocument      `json:"spec"`
	Plan       SealedDocument      `json:"plan"`
	Unit       string              `json:"unit"`
	Title      string              `json:"title,omitempty"`
	Criteria   []UnitCriterion     `json:"criteria"`
	DependsOn  []string            `json:"depends_on"`
	Footprint  []string            `json:"footprint"`
	Followup   *followup.Unit      `json:"followup,omitempty"`
	// Amendments are the approved amendments that rework the unit or
	// change its plan entry, in the order they were applied.
	Amendments []AmendmentNotice `json:"amendments,omitempty"`
	// Context holds the charter and the knowledge-base context of the
	// unit's footprint.
	Context Bundle `json:"context"`
}

// SealedDocument is the revision of spec.md or plan.json the seal ratified.
// Hash is the spec's sealed hash; the plan has none.
type SealedDocument struct {
	Source   string `json:"source"`
	Record   string `json:"record"`
	Revision int    `json:"revision"`
	Hash     string `json:"hash,omitempty"`
	Content  string `json:"content"`
}

// UnitCriterion is one criterion the unit addresses, its text as the sealed
// spec states it, and the proof the plan names for it.
type UnitCriterion struct {
	Citation string     `json:"citation"`
	Text     string     `json:"text"`
	Proof    plan.Proof `json:"proof"`
}

// AmendmentNotice tells a unit's roles about an approved amendment that
// reworks the unit, because it changed the meaning of a criterion the unit
// addresses, or that changed the unit's plan entry while its criteria kept
// their meaning. It is read from the amendment's application record: the
// revisions it sealed, the changed criteria, the unit's affected proofs, and
// the request and the owner's note as they were recorded.
type AmendmentNotice struct {
	Source    string   `json:"source"`
	Amendment string   `json:"amendment"`
	Rework    bool     `json:"rework"`
	Seal      int      `json:"seal"`
	Spec      int      `json:"spec"`
	Plan      int      `json:"plan"`
	Criteria  []string `json:"criteria"`
	Proofs    []string `json:"proofs"`
	Citations []string `json:"citations"`
	Change    string   `json:"change"`
	Reason    string   `json:"reason"`
	Note      string   `json:"note,omitempty"`
}

// ErrStaleSpec is returned when the workstream's spec no longer matches the
// hash its seal records.
var ErrStaleSpec = errors.New("spec does not match its seal")

// Mason assembles the mason bundle of one unit of the workstream's latest
// seal. It first reads spec.md, recording any owner edit, and refuses the
// bundle when the spec's hash differs from the sealed one, so a unit is never
// built against a spec the owner did not ratify. The unit is read from the
// sealed plan revision or a final-review follow-up.
func (f Files) Mason(ctx context.Context, project config.ProjectID, stream config.WorkstreamID, unit string) (Mason, error) {
	if f.Repository == nil {
		return Mason{}, errors.New("file context provider has no trace")
	}
	repo, err := f.Repository(project)
	if err != nil {
		return Mason{}, err
	}
	s, _, ok, err := seal.Latest(repo, stream)
	if err != nil {
		return Mason{}, err
	}
	if !ok {
		return Mason{}, fmt.Errorf("workstream %s is not sealed", stream)
	}
	current, err := repo.OwnerDocuments(ctx, stream, f.now().UTC(), nil)
	if err != nil {
		return Mason{}, err
	}
	spec := current[plan.SpecDocument]
	if hash := seal.SpecHash(spec.Content); hash != s.SpecHash {
		return Mason{}, fmt.Errorf("%w: %s revision %d hashes to %s, seal %d records %s", ErrStaleSpec, plan.SpecPath, spec.Revision, hash, s.Seal, s.SpecHash)
	}
	planDoc, err := revision(repo, stream, plan.PlanDocument, s.Revision.Plan)
	if err != nil {
		return Mason{}, err
	}
	p, err := plan.Parse([]byte(planDoc.Content))
	if err != nil {
		return Mason{}, fmt.Errorf("%s revision %d: %w", plan.PlanPath, planDoc.Revision, err)
	}
	u, ok := p.Unit(unit)
	var added *followup.Unit
	if !ok {
		item, found, err := followup.Find(repo, stream, unit)
		if err != nil {
			return Mason{}, err
		}
		if !found {
			return Mason{}, fmt.Errorf("unit %q is not in %s revision %d", unit, plan.PlanPath, planDoc.Revision)
		}
		u, added = item.Unit, &item
	}
	parsed := plan.ParseSpec(spec.Content)
	m := Mason{
		Workstream: stream,
		Seal:       s.Seal,
		Spec:       SealedDocument{Source: spec.Path, Record: spec.ID, Revision: s.Revision.Spec, Hash: s.SpecHash, Content: spec.Content},
		Plan:       SealedDocument{Source: planDoc.Path, Record: planDoc.ID, Revision: planDoc.Revision, Content: planDoc.Content},
		Unit:       u.ID,
		Title:      u.Title,
		Criteria:   []UnitCriterion{},
		DependsOn:  append([]string{}, u.DependsOn...),
		Footprint:  append([]string{}, u.Footprint...),
		Followup:   added,
	}
	for _, a := range u.Addresses {
		n, _ := plan.ParseCitation(a.Criterion)
		c, ok := parsed.Criterion(n)
		if !ok {
			return Mason{}, fmt.Errorf("unit %q cites %s, which %s revision %d does not hold", u.ID, a.Criterion, plan.SpecPath, spec.Revision)
		}
		m.Criteria = append(m.Criteria, UnitCriterion{Citation: a.Criterion, Text: c.Text, Proof: a.Proof})
	}
	applications, err := amendment.Read(repo, stream)
	if err != nil {
		return Mason{}, err
	}
	for _, a := range applications {
		if !a.Names(u.ID) {
			continue
		}
		n := AmendmentNotice{Source: amendment.Path(a.Amendment), Amendment: a.Amendment, Rework: slices.Contains(a.Rework, u.ID), Seal: a.To.Seal, Spec: a.To.Spec, Plan: a.To.Plan,
			Criteria: slices.Clone(a.Criteria), Proofs: []string{}, Citations: slices.Clone(a.Citations), Change: a.Change, Reason: a.Reason, Note: a.Note}
		for _, proof := range a.Proofs {
			if strings.HasPrefix(proof, u.ID+":") {
				n.Proofs = append(n.Proofs, proof)
			}
		}
		m.Amendments = append(m.Amendments, n)
	}
	if m.Context, err = f.Assemble(ctx, project, Scope{Entities: m.Footprint, Workstream: stream}); err != nil {
		return Mason{}, err
	}
	return m, nil
}

func (f Files) now() time.Time {
	if f.Now != nil {
		return f.Now()
	}
	return time.Now()
}

// revision returns the given revision of a workstream document.
func revision(repo *trace.Repository, stream config.WorkstreamID, id string, rev int) (trace.Document, error) {
	documents, err := trace.Read[trace.Document](repo, stream)
	if err != nil {
		return trace.Document{}, err
	}
	i := slices.IndexFunc(documents, func(d trace.Document) bool { return d.ID == id && d.Revision == rev })
	if i < 0 {
		return trace.Document{}, fmt.Errorf("workstream %s records no revision %d of %s", stream, rev, id)
	}
	return documents[i], nil
}

// Render returns the mason bundle as the text a turn request carries: the
// unit, its criteria and proofs, its dependencies and footprint, its
// amendment notices, the sealed spec, and the rendered project context.
func (m Mason) Render() string {
	var w strings.Builder
	line := func(format string, args ...any) { fmt.Fprintf(&w, format+"\n", args...) }
	line("# Unit %s", m.Unit)
	line("")
	if m.Title != "" {
		line("title: %s", m.Title)
	}
	line("workstream: %s", m.Workstream)
	line("seal: %d", m.Seal)
	line("spec: %s (record %s revision %d, %s)", m.Spec.Source, m.Spec.Record, m.Spec.Revision, m.Spec.Hash)
	line("plan: %s (record %s revision %d)", m.Plan.Source, m.Plan.Record, m.Plan.Revision)
	if m.Followup != nil && m.Followup.Amendment != "" {
		line("follow-up: amendment %s, %s", m.Followup.Amendment, m.Followup.Criterion)
		line("gap: %s", m.Followup.Gap)
	} else if m.Followup != nil {
		line("follow-up: final/report.json revision %d, review %d, %s", m.Followup.Report, m.Followup.Review, m.Followup.Criterion)
		line("gap: %s", m.Followup.Gap)
	}
	line("")
	line("## Criteria")
	if len(m.Criteria) == 0 {
		line("The unit addresses no criteria.")
	}
	for _, c := range m.Criteria {
		line("- %s: %s", c.Citation, indent(c.Text))
		line("  proof: %s %s", c.Proof.Kind, indent(c.Proof.Name))
	}
	line("")
	line("## Depends on")
	if len(m.DependsOn) == 0 {
		line("No units.")
	}
	for _, d := range m.DependsOn {
		line("- plan#%s", d)
	}
	line("")
	line("## Footprint")
	for _, e := range m.Footprint {
		line("- %s", e)
	}
	if len(m.Amendments) > 0 {
		line("")
		w.WriteString(m.RenderAmendments())
	}
	line("")
	line("## Spec")
	w.WriteString(m.Spec.Content)
	if !strings.HasSuffix(m.Spec.Content, "\n") {
		w.WriteString("\n")
	}
	line("## end of %s", m.Spec.Source)
	line("")
	w.WriteString(m.Context.Render())
	return w.String()
}

// RenderAmendments returns the unit's amendment notices as text, each with
// the request and the owner's note in the shared envelope, or "" when the
// unit has none.
func (m Mason) RenderAmendments() string {
	if len(m.Amendments) == 0 {
		return ""
	}
	var w strings.Builder
	line := func(format string, args ...any) { fmt.Fprintf(&w, format+"\n", args...) }
	line("## Amendments")
	for _, n := range m.Amendments {
		line("- %s: the owner approved amendment %s; seal %d governs %s revision %d and %s revision %d", n.Source, n.Amendment, n.Seal, plan.SpecPath, n.Spec, plan.PlanPath, n.Plan)
		if n.Rework {
			line("  It changed the meaning of criteria this unit addresses, so the unit is built again against the amended spec.")
		} else {
			line("  It changed this unit's entry in the plan; the criteria the unit addresses keep their meaning.")
		}
		changed := "none"
		if len(n.Criteria) > 0 {
			changed = strings.Join(n.Criteria, ", ")
		}
		proofs := "none"
		if len(n.Proofs) > 0 {
			proofs = strings.Join(n.Proofs, ", ")
		}
		line("  changed criteria: %s", changed)
		line("  affected proofs of this unit: %s", proofs)
		sections := []envelope.Section{{Name: "question", Text: fmt.Sprintf("Citations: %s\nChange: %s\nReason: %s", strings.Join(n.Citations, ", "), n.Change, n.Reason)}}
		if n.Note != "" {
			sections = append(sections, envelope.Section{Name: "owner_response", Text: n.Note})
		}
		section, err := envelope.Render(sections...)
		if err != nil {
			panic(err)
		}
		w.WriteString(section)
	}
	return w.String()
}
