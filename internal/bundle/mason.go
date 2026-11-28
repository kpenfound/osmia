package bundle

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"slices"
	"strings"

	"github.com/kpenfound/osmia/internal/amendment"
	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/envelope"
	"github.com/kpenfound/osmia/internal/followup"
	"github.com/kpenfound/osmia/internal/plan"
	"github.com/kpenfound/osmia/internal/seal"
	"github.com/kpenfound/osmia/internal/trace"
	"github.com/kpenfound/osmia/internal/verification"
)

// Mason is the bundle a mason builds one unit of a sealed workstream from:
// the spec at its sealed hash, the unit's task and acceptance, the criteria
// it serves, its footprint and dependencies, the notices of approved amendments that affect
// it, and the project context scoped to its footprint.
type Mason struct {
	Workstream  config.WorkstreamID `json:"workstream"`
	Seal        int                 `json:"seal"`
	Spec        SealedDocument      `json:"spec"`
	Plan        SealedDocument      `json:"plan"`
	Unit        string              `json:"unit"`
	Title       string              `json:"title,omitempty"`
	Task        string              `json:"task"`
	Acceptance  []string            `json:"acceptance"`
	Constraints []string            `json:"constraints,omitempty"`
	Boundaries  []string            `json:"boundaries,omitempty"`
	Guidance    []string            `json:"guidance,omitempty"`
	Criteria    []UnitCriterion     `json:"criteria"`
	DependsOn   []string            `json:"depends_on"`
	Footprint   []string            `json:"footprint"`
	Followup    *followup.Unit      `json:"followup,omitempty"`
	// Amendments are the approved amendments that rework the unit or
	// change its plan entry, in the order they were applied.
	Amendments []AmendmentNotice `json:"amendments,omitempty"`
	// Verification names the feature-map files that apply to the unit. It is
	// nil for a project without the verification setting.
	Verification *Verification `json:"verification,omitempty"`
	// Context holds the charter and the entity map context of the unit's
	// footprint.
	Context Bundle `json:"context"`
}

// Verification is what a mason is told about the project's verification
// lever: the feature files that apply to its unit, found by plain glob
// matching, and what to keep current with its change.
type Verification struct {
	// MapDir is the project's feature-map directory.
	MapDir string `json:"map_dir"`
	// Features are the feature files whose paths globs match the unit's files.
	Features []FeaturePointer `json:"features,omitempty"`
}

// FeaturePointer names one feature file of the map.
type FeaturePointer struct {
	File    string   `json:"file"`
	Feature string   `json:"feature"`
	Paths   []string `json:"paths"`
	Flows   []string `json:"flows,omitempty"`
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

// UnitCriterion is one criterion the unit serves and its text as the sealed
// spec states it.
type UnitCriterion struct {
	Citation string `json:"citation"`
	Text     string `json:"text"`
}

// AmendmentNotice tells a unit's roles about an approved amendment that
// reworks the unit, because it changed the meaning of a criterion the unit
// serves, or that changed the unit's plan entry while its criteria kept
// their meaning. It is read from the amendment's application record: the
// revisions it sealed, the changed criteria, and the request and the decision
// note as they were recorded.
type AmendmentNotice struct {
	Actor     string   `json:"actor,omitempty"`
	Source    string   `json:"source"`
	Amendment string   `json:"amendment"`
	Rework    bool     `json:"rework"`
	Seal      int      `json:"seal"`
	Spec      int      `json:"spec"`
	Plan      int      `json:"plan"`
	Criteria  []string `json:"criteria"`
	Citations []string `json:"citations"`
	Change    string   `json:"change"`
	Reason    string   `json:"reason"`
	Note      string   `json:"note,omitempty"`
}

// ErrStaleSpec is returned when the workstream's spec no longer matches the
// hash its seal records.
var ErrStaleSpec = errors.New("spec does not match its seal")

// ErrResolvedWorkstream refuses active worker context after merge or cancel.
// Historical specs and plans remain available through the trace readers.
var ErrResolvedWorkstream = errors.New("workstream is resolved")

// Mason assembles the mason bundle of one unit of the workstream's latest
// seal. Resolved members have no active worker bundle. It reads the latest
// spec.md revision and refuses a spec whose hash differs from the seal,
// so a unit is never built against a spec the owner did not ratify. The unit is
// read from the sealed plan revision or a final-review follow-up.
func (f Files) Mason(ctx context.Context, project config.ProjectID, stream config.WorkstreamID, unit string) (Mason, error) {
	if f.Repository == nil {
		return Mason{}, errors.New("file context provider has no trace")
	}
	repo, err := f.Repository(project)
	if err != nil {
		return Mason{}, err
	}
	active, err := trace.MemberDocumentsActive(repo, stream)
	if err != nil {
		return Mason{}, err
	}
	if !active {
		return Mason{}, fmt.Errorf("%w: %s", ErrResolvedWorkstream, stream)
	}
	s, _, ok, err := seal.Latest(repo, stream)
	if err != nil {
		return Mason{}, err
	}
	if !ok {
		return Mason{}, fmt.Errorf("workstream %s is not sealed", stream)
	}
	spec, err := latest(repo, stream, plan.SpecDocument)
	if err != nil {
		return Mason{}, err
	}
	if hash := seal.SpecHash(spec.Content); hash != s.SpecHash {
		return Mason{}, fmt.Errorf("%w: %s revision %d hashes to %s, seal %d records %s", ErrStaleSpec, plan.SpecPath, spec.Revision, hash, s.Seal, s.SpecHash)
	}
	var planDoc trace.Document
	var u plan.Unit
	var added *followup.Unit
	if planned, err := repo.Planned(stream); err != nil {
		return Mason{}, err
	} else if !planned {
		if f.Unplanned == nil {
			return Mason{}, fmt.Errorf("workstream %s has no sealed plan and no source of its units", stream)
		}
		if u, err = f.Unplanned(spec, unit); err != nil {
			return Mason{}, err
		}
		data, err := json.MarshalIndent(plan.Plan{Version: plan.Version, Units: []plan.Unit{u}}, "", "  ")
		if err != nil {
			return Mason{}, err
		}
		planDoc = trace.Document{Header: trace.Header{ID: UnplannedDocument, Revision: s.Revision.Plan}, Path: UnplannedPath, Content: string(data) + "\n"}
	} else {
		if planDoc, err = revision(repo, stream, plan.PlanDocument, s.Revision.Plan); err != nil {
			return Mason{}, err
		}
		p, err := plan.Parse([]byte(planDoc.Content))
		if err != nil {
			return Mason{}, fmt.Errorf("%s revision %d: %w", plan.PlanPath, planDoc.Revision, err)
		}
		var ok bool
		u, ok = p.Unit(unit)
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
	}
	parsed := plan.ParseSpec(spec.Content)
	m := Mason{
		Workstream:  stream,
		Seal:        s.Seal,
		Spec:        SealedDocument{Source: spec.Path, Record: spec.ID, Revision: s.Revision.Spec, Hash: s.SpecHash, Content: spec.Content},
		Plan:        SealedDocument{Source: planDoc.Path, Record: planDoc.ID, Revision: planDoc.Revision, Content: planDoc.Content},
		Unit:        u.ID,
		Title:       u.Title,
		Task:        u.Task,
		Acceptance:  append([]string{}, u.Acceptance...),
		Constraints: append([]string{}, u.Constraints...), Boundaries: append([]string{}, u.Boundaries...), Guidance: append([]string{}, u.Guidance...),
		Criteria:  []UnitCriterion{},
		DependsOn: append([]string{}, u.DependsOn...),
		Footprint: append([]string{}, u.Footprint...),
		Followup:  added,
	}
	for _, citation := range u.Criteria {
		n, _ := plan.ParseCitation(citation)
		c, ok := parsed.Criterion(n)
		if !ok {
			return Mason{}, fmt.Errorf("unit %q cites %s, which %s revision %d does not hold", u.ID, citation, plan.SpecPath, spec.Revision)
		}
		m.Criteria = append(m.Criteria, UnitCriterion{Citation: citation, Text: c.Text})
	}
	applications, err := amendment.Read(repo, stream)
	if err != nil {
		return Mason{}, err
	}
	for _, a := range applications {
		if !a.Names(u.ID) {
			continue
		}
		n := AmendmentNotice{Actor: a.Actor, Source: amendment.Path(a.Amendment), Amendment: a.Amendment, Rework: slices.Contains(a.Rework, u.ID), Seal: a.To.Seal, Spec: a.To.Spec, Plan: a.To.Plan,
			Criteria: slices.Clone(a.Criteria), Citations: slices.Clone(a.Citations), Change: a.Change, Reason: a.Reason, Note: a.Note}
		m.Amendments = append(m.Amendments, n)
	}
	provider := f.Provider
	if provider == nil {
		provider = f
	}
	if f.Verification != nil {
		if m.Verification, err = f.Verification(ctx, stream, m.Unit, m.Footprint); err != nil {
			return Mason{}, err
		}
	}
	if m.Context, err = provider.Assemble(ctx, project, Scope{Role: f.Role, Entities: m.Footprint, Workstream: stream}); err != nil {
		return Mason{}, err
	}
	return m, nil
}

func latest(repo *trace.Repository, stream config.WorkstreamID, id string) (trace.Document, error) {
	documents, err := trace.Read[trace.Document](repo, stream)
	if err != nil {
		return trace.Document{}, err
	}
	for _, d := range slices.Backward(documents) {
		if d.ID == id {
			return d, nil
		}
	}
	return trace.Document{}, fmt.Errorf("workstream %s records no %s: %w", stream, id, fs.ErrNotExist)
}

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
// unit's task and acceptance, the criteria it serves, its dependencies and
// footprint, its
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
	w.WriteString(m.RenderTask())
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
	if m.Verification != nil {
		line("")
		w.WriteString(m.Verification.Render())
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
	w.WriteString(m.Context.Brief())
	return w.String()
}

// RenderTask returns the unit's task, its acceptance and the criteria it
// serves as text: what the mason builds and the reviewer verifies.
func (m Mason) RenderTask() string {
	var w strings.Builder
	line := func(format string, args ...any) { fmt.Fprintf(&w, format+"\n", args...) }
	line("## Task")
	line("%s", m.Task)
	line("")
	line("## Acceptance")
	for _, a := range m.Acceptance {
		line("- %s", indent(a))
	}
	line("")
	for _, section := range []struct {
		name  string
		items []string
	}{{"Constraints", m.Constraints}, {"Ownership boundaries", m.Boundaries}, {"Adaptable guidance", m.Guidance}} {
		if len(section.items) == 0 {
			continue
		}
		line("## %s", section.name)
		for _, item := range section.items {
			line("- %s", indent(item))
		}
		line("")
	}
	line("## Criteria served")
	if len(m.Criteria) == 0 {
		line("The unit cites no criteria.")
	}
	for _, c := range m.Criteria {
		line("- %s: %s", c.Citation, indent(c.Text))
	}
	return w.String()
}

// RenderAmendments returns the unit's amendment notices as text, each with
// the request and the decision note in the shared envelope, or "" when the
// unit has none.
func (m Mason) RenderAmendments() string {
	if len(m.Amendments) == 0 {
		return ""
	}
	var w strings.Builder
	line := func(format string, args ...any) { fmt.Fprintf(&w, format+"\n", args...) }
	line("## Amendments")
	for _, n := range m.Amendments {
		actor, section := "owner", "owner_response"
		if n.Actor == trace.Manager {
			actor, section = "manager", "answer"
		}
		line("- %s: the %s approved amendment %s; seal %d governs %s revision %d and %s revision %d", n.Source, actor, n.Amendment, n.Seal, plan.SpecPath, n.Spec, plan.PlanPath, n.Plan)
		if n.Rework {
			line("  It changed this unit's assignment or criteria, so the unit is built again against the amended task and acceptance.")
		} else {
			line("  It changed this unit's entry in the plan; the criteria the unit serves keep their meaning.")
		}
		changed := "none"
		if len(n.Criteria) > 0 {
			changed = strings.Join(n.Criteria, ", ")
		}
		line("  changed criteria: %s", changed)
		sections := []envelope.Section{{Name: "question", Text: fmt.Sprintf("Citations: %s\nChange: %s\nReason: %s", strings.Join(n.Citations, ", "), n.Change, n.Reason)}}
		if n.Note != "" {
			sections = append(sections, envelope.Section{Name: section, Text: n.Note})
		}
		rendered, err := envelope.Render(sections...)
		if err != nil {
			panic(err)
		}
		w.WriteString(rendered)
	}
	return w.String()
}

// Render returns the verification section of the mason bundle: the feature
// files that apply, and the reminder to keep the lever and the map current in
// the same change and to list the invocations the mason verified with.
func (v Verification) Render() string {
	var w strings.Builder
	line := func(format string, args ...any) { fmt.Fprintf(&w, format+"\n", args...) }
	line("## Verification")
	line("This project has a verification lever and a feature map in %s. Keep the lever and the map current in the same change as the behaviour they describe, and list the lever invocations you verified with in the done report's invocations field, one argument list each. The service reruns up to %d of them, in the order you give, on your candidate, and the reviewer judges those service runs, not your own.", v.MapDir, verification.MaxRerunsPerUnit)
	if len(v.Features) == 0 {
		line("No feature file's paths match this unit's footprint or files, so none applies yet. If your change adds behaviour a user reaches, add its feature file.")
		return w.String()
	}
	line("These feature files apply to this unit, found by matching its files against their paths globs:")
	for _, f := range v.Features {
		flows := "no flows"
		if len(f.Flows) > 0 {
			flows = "flows " + strings.Join(f.Flows, ", ")
		}
		line("- %s (%s; paths %s; %s)", f.File, f.Feature, strings.Join(f.Paths, ", "), flows)
	}
	return w.String()
}

// Pointers converts features of the map to the pointers a mason bundle names.
func Pointers(features []verification.Feature) []FeaturePointer {
	out := make([]FeaturePointer, 0, len(features))
	for _, f := range features {
		out = append(out, FeaturePointer{File: f.File, Feature: f.Name, Paths: slices.Clone(f.Paths), Flows: slices.Clone(f.Flows)})
	}
	return out
}
