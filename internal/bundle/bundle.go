// Package bundle assembles the project context a turn receives: the charter,
// footprint entities and trace decisions.
package bundle

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/kpenfound/osmia/internal/charter"
	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/kb"
	"github.com/kpenfound/osmia/internal/plan"
	"github.com/kpenfound/osmia/internal/trace"
)

// Mode names where a provider's context comes from.
type Mode string

// ModeFile is context read from the project's local files and trace. It is a
// supported operating mode, not a degraded one.
const ModeFile Mode = "file"

// Provider assembles bundles. Callers depend only on this interface.
type Provider interface {
	Mode(config.ProjectID) Mode
	Assemble(context.Context, config.ProjectID, Scope) (Bundle, error)
}

// Scope narrows a bundle. Entities are footprint entity IDs or aliases; none
// means the whole project. A Workstream limits decisions to its own rulings.
type Scope struct {
	Role       string              `json:"role,omitempty"`
	Entities   []string            `json:"entities,omitempty"`
	Workstream config.WorkstreamID `json:"workstream,omitempty"`
}

// Bundle is one assembled context. Every part names the path and record it
// was read from.
type Bundle struct {
	Memory         []json.RawMessage     `json:"memory,omitempty"`
	MemoryProblem  string                `json:"memory_problem,omitempty"`
	Project        config.ProjectID      `json:"project"`
	Mode           Mode                  `json:"mode"`
	Scope          Scope                 `json:"scope"`
	MemberCharters []trace.MemberCharter `json:"member_charters,omitempty"`
	Charter        Charter               `json:"charter"`
	Entities       Entities              `json:"entities"`
	Decisions      []Decision            `json:"decisions"`
	Notices        []Notice              `json:"notices"`
	// CharterNotices are the charter rules the owner ratified from rulings,
	// in the order they were recorded in the charter.
	CharterNotices []CharterNotice `json:"charter_notices"`
	// CharterEdits identify the owner's edits so in-flight roles notice revised policy.
	CharterEdits   []trace.Document `json:"charter_edits"`
	ProjectNotices []ProjectNotice  `json:"project_notices"`
	// ProjectSpec is the scope's workstream's workgroup's project spec,
	// marked as a draft until group sealing completes. It stays
	// available to the chief of staff and every unresolved member,
	// including a dependent still waiting on or repairing after an
	// upstream merge, and leaves the bundle only once every member of the
	// workgroup is resolved.
	ProjectSpec  *ProjectSpecView      `json:"project_spec,omitempty"`
	Dependencies []DependencyReference `json:"dependencies,omitempty"`
}

type DependencyReference struct {
	Upstream config.WorkstreamID `json:"upstream"`
	Type     string              `json:"type"`
	Merged   bool                `json:"merged"`
	// Waived says this workstream dropped the dependency with a waiver in its
	// own trace: it no longer waits on it, and the upstream has not merged.
	Waived      bool   `json:"waived,omitempty"`
	Reference   string `json:"reference,omitempty"`
	Commit      string `json:"commit,omitempty"`
	BuildBranch string `json:"build_branch,omitempty"`
}

// ProjectSpecView is the project spec of the scope's workstream's
// workgroup, and the scope's workstream's own slice of it, when one is
// recorded for it.
type ProjectSpecView struct {
	Source    string             `json:"source"`
	Record    string             `json:"record"`
	Workgroup config.WorkgroupID `json:"workgroup"`
	Draft     bool               `json:"draft,omitempty"`
	Behavior  string             `json:"behavior"`
	Contracts []trace.Contract   `json:"contracts,omitempty"`
	Graph     []trace.Edge       `json:"graph,omitempty"`
	// Dropped is the part of Graph that the scope's workstream dropped with a
	// waiver in its own trace. The spec still names those edges.
	Dropped []trace.Edge `json:"dropped,omitempty"`
	Slice   *trace.Slice `json:"slice,omitempty"`
}

const edgeWaiverDocument = "edge-waivers"

type droppedDependency struct {
	Upstream config.WorkstreamID `json:"upstream"`
	Type     string              `json:"type"`
}

func droppedDependencies(documents []trace.Document) ([]droppedDependency, error) {
	for _, document := range slices.Backward(documents) {
		if document.ID != edgeWaiverDocument {
			continue
		}
		var waivers struct {
			Waivers []droppedDependency `json:"waivers"`
		}
		if err := json.Unmarshal([]byte(document.Content), &waivers); err != nil {
			return nil, fmt.Errorf("dependency waivers: %w", err)
		}
		return waivers.Waivers, nil
	}
	return nil, nil
}

type ProjectNotice struct {
	Source   string    `json:"source"`
	Record   string    `json:"record"`
	Revision int       `json:"revision"`
	At       time.Time `json:"at"`
	trace.ProjectNotice
}

// Charter is the recorded charter revision, its guidance as charter.Guidance
// reads it, and its citable rules.
type Charter struct {
	Source      string               `json:"source"`
	Record      string               `json:"record"`
	Revision    int                  `json:"revision"`
	Guidance    string               `json:"guidance"`
	Rules       []charter.Rule       `json:"rules"`
	Diagnostics []charter.Diagnostic `json:"diagnostics"`
}

// Entities are the relevant entities of the recorded entity map.
type Entities struct {
	Source     string   `json:"source"`
	Record     string   `json:"record"`
	Revision   int      `json:"revision"`
	Entities   []Entity `json:"entities"`
	Unresolved []string `json:"unresolved"`
}

type Entity struct {
	ID    string   `json:"id"`
	Name  string   `json:"name"`
	Paths []string `json:"paths"`
}

// Decision is the latest revision of one ruling recorded in the trace.
type Decision struct {
	Source           string              `json:"source"`
	Workstream       config.WorkstreamID `json:"workstream"`
	Record           string              `json:"record"`
	Revision         int                 `json:"revision"`
	QuestionID       string              `json:"question_id"`
	QuestionRevision int                 `json:"question_revision"`
	At               time.Time           `json:"at"`
	Decision         string              `json:"decision"`
	OwnerResponse    string              `json:"owner_response,omitempty"`
	ReturnedAnswer   string              `json:"returned_answer"`
	Scope            string              `json:"scope,omitempty"`
}

// Notice is an owner ruling the chief of staff relayed with scope notify. It
// applies to the whole project, so every bundle on the project carries it,
// whatever workstream recorded it and whatever the bundle's scope. A ruling
// relayed to a batch of questions is one notice.
type Notice struct {
	Source         string              `json:"source"`
	Workstream     config.WorkstreamID `json:"workstream"`
	Record         string              `json:"record"`
	Revision       int                 `json:"revision"`
	At             time.Time           `json:"at"`
	OwnerResponse  string              `json:"owner_response"`
	ReturnedAnswer string              `json:"returned_answer"`
}

// CharterNotice is a rule the owner ratified from a ruling and the service
// recorded in the charter. Like a notify ruling it applies to the whole
// project, so every bundle on the project carries it. Source, Record and
// Revision name the proposal's charter.json revision that records the rule's
// number and the charter revision that holds it; Ruling and RulingRevision
// name the ruling it came from and OwnerResponse the owner's words in it.
type CharterNotice struct {
	Source         string              `json:"source"`
	Workstream     config.WorkstreamID `json:"workstream"`
	Record         string              `json:"record"`
	Revision       int                 `json:"revision"`
	At             time.Time           `json:"at"`
	Number         int                 `json:"number"`
	Rule           string              `json:"rule"`
	Charter        int                 `json:"charter"`
	Ruling         string              `json:"ruling"`
	RulingRevision int                 `json:"ruling_revision"`
	OwnerResponse  string              `json:"owner_response"`
}

// Files is the provider that reads only the project's trace. It keeps
// nothing between calls, so edits to the charter apply
// to the next assembly.
type Files struct {
	Provider Provider
	Role     string
	// Repository returns the open trace of an active project. Files never
	// closes it.
	Repository func(config.ProjectID) (*trace.Repository, error)
	// Unplanned returns the unit a workstream of a kind with no sealed plan
	// builds, from the sealed document of its spec slot. It is called only for
	// such a workstream.
	Unplanned func(spec trace.Document, unit string) (plan.Unit, error)
}

// UnplannedDocument and UnplannedPath name the one-unit plan a workstream of a
// kind with no sealed plan builds from; the bundle renders it where a plan
// would be.
const (
	UnplannedDocument = "unit-source"
	UnplannedPath     = "unit-source.json"
)

var _ Provider = Files{}

func (Files) Mode(config.ProjectID) Mode { return ModeFile }

// Assemble reads the latest charter, then the entity map and the rulings, in
// that order. The notices are the project's rulings with scope
// notify, ordered as the decisions are, and the charter rules ratified from
// rulings, ordered by when the charter recorded them.
func (f Files) Assemble(ctx context.Context, project config.ProjectID, scope Scope) (Bundle, error) {
	if f.Repository == nil {
		return Bundle{}, errors.New("file context provider has no trace")
	}
	repo, err := f.Repository(project)
	if err != nil {
		return Bundle{}, err
	}
	b := Bundle{Project: project, Mode: ModeFile, Scope: scope, Decisions: []Decision{}, Notices: []Notice{}, CharterNotices: []CharterNotice{}}
	doc, err := repo.Charter(ctx)
	if err != nil {
		return Bundle{}, fmt.Errorf("charter: %w", err)
	}
	c := charter.Parse(doc.Content)
	b.Charter = Charter{Source: doc.Path, Record: doc.ID, Revision: doc.Revision, Guidance: charter.Guidance(doc.Content), Rules: c.Rules, Diagnostics: c.Diagnostics}

	if scope.Workstream != "" {
		members, err := repo.WorkgroupCharters(ctx, scope.Workstream)
		if err != nil {
			return Bundle{}, fmt.Errorf("workgroup charters: %w", err)
		}
		if len(members) > 1 {
			b.MemberCharters = members
		}
	}

	m, revision, err := kb.LoadRevision(repo)
	if err != nil {
		return Bundle{}, fmt.Errorf("entity map: %w", err)
	}
	b.Entities = Entities{Source: trace.EntitiesPath, Record: trace.EntitiesDocument, Revision: revision, Entities: []Entity{}, Unresolved: []string{}}
	if len(scope.Entities) == 0 {
		f.whole(m, &b)
	} else {
		f.scoped(m, scope.Entities, &b)
	}
	if b.Decisions, err = decisions(repo, scope.Workstream); err != nil {
		return Bundle{}, fmt.Errorf("decisions: %w", err)
	}
	if scope.Workstream != "" {
		if view, err := projectSpecView(repo, scope.Workstream); err != nil {
			return Bundle{}, fmt.Errorf("project spec: %w", err)
		} else if view != nil {
			b.ProjectSpec = view
		}
		documents, err := trace.Read[trace.Document](repo, scope.Workstream)
		if err != nil {
			return Bundle{}, err
		}
		dropped, err := droppedDependencies(documents)
		if err != nil {
			return Bundle{}, err
		}
		if b.ProjectSpec != nil {
			for _, e := range b.ProjectSpec.Graph {
				if e.To == scope.Workstream && slices.ContainsFunc(dropped, func(d droppedDependency) bool { return d.Upstream == e.From && d.Type == e.Type }) {
					b.ProjectSpec.Dropped = append(b.ProjectSpec.Dropped, e)
				}
			}
		}
		for _, document := range slices.Backward(documents) {
			if document.ID != "edge-observation" {
				continue
			}
			var observation struct {
				Edges []DependencyReference `json:"edges"`
			}
			if err := json.Unmarshal([]byte(document.Content), &observation); err != nil {
				return Bundle{}, fmt.Errorf("dependency references: %w", err)
			}
			b.Dependencies = observation.Edges
			for i, d := range b.Dependencies {
				b.Dependencies[i].Waived = slices.ContainsFunc(dropped, func(w droppedDependency) bool { return w.Upstream == d.Upstream && w.Type == d.Type })
			}
			break
		}
	}
	all, err := decisions(repo, "")
	if err != nil {
		return Bundle{}, fmt.Errorf("notices: %w", err)
	}
	for _, d := range all {
		same := func(n Notice) bool {
			return n.Workstream == d.Workstream && n.At.Equal(d.At) && n.OwnerResponse == d.OwnerResponse && n.ReturnedAnswer == d.ReturnedAnswer
		}
		if d.Scope == trace.ScopeNotify && !slices.ContainsFunc(b.Notices, same) {
			b.Notices = append(b.Notices, Notice{Source: d.Source, Workstream: d.Workstream, Record: d.Record, Revision: d.Revision, At: d.At, OwnerResponse: d.OwnerResponse, ReturnedAnswer: d.ReturnedAnswer})
		}
	}
	proposals, err := repo.CharterProposals()
	if err != nil {
		return Bundle{}, fmt.Errorf("charter notices: %w", err)
	}
	for _, p := range proposals {
		if p.State.Value != trace.CharterChartered {
			continue
		}
		b.CharterNotices = append(b.CharterNotices, CharterNotice{Source: "workstreams/" + string(p.Workstream) + "/" + p.Latest.Path, Workstream: p.Workstream, Record: p.Latest.ID, Revision: p.Latest.Revision, At: p.Latest.At,
			Number: p.Proposal.Number, Rule: p.Proposal.Rule, Charter: p.Proposal.Charter, Ruling: p.Proposal.Ruling, RulingRevision: p.Proposal.RulingRevision, OwnerResponse: p.Proposal.OwnerResponse})
	}
	documents, err := repo.ProjectDocuments()
	if err != nil {
		return Bundle{}, err
	}
	for _, d := range documents {
		if d.Path == "charter.md" && d.Cause == "owner-edit" && d.Revision > 1 {
			b.CharterEdits = append(b.CharterEdits, d)
		}
		if strings.HasPrefix(d.Path, "notices/") {
			var n trace.ProjectNotice
			if err := json.Unmarshal([]byte(d.Content), &n); err != nil {
				return Bundle{}, err
			}
			b.ProjectNotices = append(b.ProjectNotices, ProjectNotice{Source: d.Path, Record: d.ID, Revision: d.Revision, At: d.At, ProjectNotice: n})
		}
	}
	slices.SortStableFunc(b.CharterNotices, func(x, y CharterNotice) int { return x.At.Compare(y.At) })
	return b, nil
}

func (f Files) whole(m kb.Map, b *Bundle) {
	for _, e := range m.Entities {
		b.Entities.Entities = append(b.Entities.Entities, entity(e))
	}
	slices.SortFunc(b.Entities.Entities, func(x, y Entity) int { return strings.Compare(x.ID, y.ID) })
}

func (f Files) scoped(m kb.Map, names []string, b *Bundle) {
	fp := m.ResolveEntities(names)
	b.Entities.Unresolved = fp.Unresolved
	byID := map[string]kb.Entity{}
	for _, e := range m.Entities {
		byID[e.ID] = e
	}
	for _, id := range fp.Entities {
		b.Entities.Entities = append(b.Entities.Entities, entity(byID[id]))
	}
}

func entity(e kb.Entity) Entity {
	return Entity{ID: e.ID, Name: e.Name, Paths: append([]string{}, e.Paths...)}
}

func projectSpecView(repo *trace.Repository, stream config.WorkstreamID) (*ProjectSpecView, error) {
	spec, workgroup, found, err := repo.ProjectSpecOf(stream)
	if err != nil || !found {
		return nil, err
	}
	active, err := trace.ProjectSpecActive(repo, workgroup)
	if err != nil || !active {
		return nil, err
	}
	var slicePtr *trace.Slice
	if slice, ok := trace.SliceFor(spec, stream); ok {
		active, err := trace.MemberDocumentsActive(repo, stream)
		if err != nil {
			return nil, err
		}
		if active {
			slicePtr = &slice
		}
	}
	approved, err := repo.GroupApproved(stream)
	if err != nil {
		return nil, err
	}
	return &ProjectSpecView{Source: trace.ProjectSpecsPath, Record: trace.ProjectSpecsDocument, Workgroup: workgroup, Draft: !approved, Behavior: spec.Behavior, Contracts: spec.Contracts, Graph: spec.Graph, Slice: slicePtr}, nil
}

func decisions(repo *trace.Repository, stream config.WorkstreamID) ([]Decision, error) {
	streams := []config.WorkstreamID{stream}
	if stream == "" {
		var err error
		if streams, err = repo.Workstreams(); err != nil {
			return nil, err
		}
	}
	out := []Decision{}
	for _, ws := range streams {
		rulings, err := trace.Read[trace.Ruling](repo, ws)
		if err != nil {
			return nil, err
		}
		latest := map[string]trace.Ruling{}
		for _, r := range rulings {
			latest[r.ID] = r
		}
		for _, r := range latest {
			out = append(out, Decision{Source: trace.RecordPath(r), Workstream: ws, Record: r.ID, Revision: r.Revision, QuestionID: r.QuestionID, QuestionRevision: r.QuestionRevision, At: r.At, Decision: r.Decision, OwnerResponse: r.OwnerResponse, ReturnedAnswer: r.ReturnedAnswer, Scope: r.Scope})
		}
	}
	slices.SortFunc(out, func(x, y Decision) int {
		if c := strings.Compare(string(x.Workstream), string(y.Workstream)); c != 0 {
			return c
		}
		if c := x.At.Compare(y.At); c != 0 {
			return c
		}
		return strings.Compare(x.Record, y.Record)
	})
	return out, nil
}
