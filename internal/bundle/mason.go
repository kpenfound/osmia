package bundle

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/plan"
	"github.com/kpenfound/osmia/internal/seal"
	"github.com/kpenfound/osmia/internal/trace"
)

// Mason is the bundle a mason builds one unit of a sealed workstream from:
// the spec at its sealed hash, the unit's criteria and planned proofs, its
// footprint and dependencies, and the project context scoped to its
// footprint.
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

// ErrStaleSpec is returned when the workstream's spec no longer matches the
// hash its seal records.
var ErrStaleSpec = errors.New("spec does not match its seal")

// Mason assembles the mason bundle of one unit of the workstream's latest
// seal. It first reads spec.md, recording any owner edit, and refuses the
// bundle when the spec's hash differs from the sealed one, so a unit is never
// built against a spec the owner did not ratify. The unit is read from the
// sealed plan revision.
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
	if !ok {
		return Mason{}, fmt.Errorf("unit %q is not in %s revision %d", unit, plan.PlanPath, planDoc.Revision)
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
	}
	for _, a := range u.Addresses {
		n, _ := plan.ParseCitation(a.Criterion)
		c, ok := parsed.Criterion(n)
		if !ok {
			return Mason{}, fmt.Errorf("unit %q cites %s, which %s revision %d does not hold", u.ID, a.Criterion, plan.SpecPath, spec.Revision)
		}
		m.Criteria = append(m.Criteria, UnitCriterion{Citation: a.Criterion, Text: c.Text, Proof: a.Proof})
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
// unit, its criteria and proofs, its dependencies and footprint, the sealed
// spec, and the rendered project context.
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
