// Package bundle assembles the project context a turn receives: the charter,
// knowledge-base prose, footprint entities and trace decisions.
package bundle

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/kpenfound/osmia/internal/charter"
	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/kb"
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
	Entities   []string            `json:"entities,omitempty"`
	Workstream config.WorkstreamID `json:"workstream,omitempty"`
}

// Bundle is one assembled context. Every part names the path and record it
// was read from.
type Bundle struct {
	Project   config.ProjectID `json:"project"`
	Mode      Mode             `json:"mode"`
	Scope     Scope            `json:"scope"`
	Charter   Charter          `json:"charter"`
	Knowledge []Prose          `json:"knowledge"`
	Missing   []MissingProse   `json:"missing"`
	Entities  Entities         `json:"entities"`
	Decisions []Decision       `json:"decisions"`
	Notices   []Notice         `json:"notices"`
}

// Charter is the recorded charter revision and its citable rules.
type Charter struct {
	Source      string               `json:"source"`
	Record      string               `json:"record"`
	Revision    int                  `json:"revision"`
	Rules       []charter.Rule       `json:"rules"`
	Diagnostics []charter.Diagnostic `json:"diagnostics"`
}

// Prose is one kb/<subsystem>.md file as read at assembly.
type Prose struct {
	Subsystem string `json:"subsystem"`
	Source    string `json:"source"`
	Content   string `json:"content"`
}

// MissingProse names a scope entity for which none of the files that could
// hold its prose, its own or an ancestor's, exists.
type MissingProse struct {
	Entity string   `json:"entity"`
	Looked []string `json:"looked"`
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

// Files is the provider that reads only the project's local files and trace.
// It keeps nothing between calls, so edits to the charter or the knowledge
// base apply to the next assembly.
type Files struct {
	// Repository returns the open trace of an active project. Files never
	// closes it.
	Repository func(config.ProjectID) (*trace.Repository, error)
	// Now timestamps a charter revision recorded for an owner edit.
	Now func() time.Time
}

var _ Provider = Files{}

func (Files) Mode(config.ProjectID) Mode { return ModeFile }

// Assemble reads the charter through the trace, which first records any
// unrecorded owner edit, then the knowledge base, the entity map and the
// rulings, in that order. The notices are the project's rulings with scope
// notify, ordered as the decisions are.
func (f Files) Assemble(ctx context.Context, project config.ProjectID, scope Scope) (Bundle, error) {
	if f.Repository == nil {
		return Bundle{}, errors.New("file context provider has no trace")
	}
	repo, err := f.Repository(project)
	if err != nil {
		return Bundle{}, err
	}
	b := Bundle{Project: project, Mode: ModeFile, Scope: scope, Knowledge: []Prose{}, Missing: []MissingProse{}, Decisions: []Decision{}, Notices: []Notice{}}
	doc, err := repo.Charter(ctx, f.now().UTC())
	if err != nil {
		return Bundle{}, fmt.Errorf("charter: %w", err)
	}
	c := charter.Parse(doc.Content)
	b.Charter = Charter{Source: doc.Path, Record: doc.ID, Revision: doc.Revision, Rules: c.Rules, Diagnostics: c.Diagnostics}

	m, revision, err := kb.LoadRevision(repo)
	if err != nil {
		return Bundle{}, fmt.Errorf("entity map: %w", err)
	}
	b.Entities = Entities{Source: trace.EntitiesPath, Record: trace.EntitiesDocument, Revision: revision, Entities: []Entity{}, Unresolved: []string{}}
	if len(scope.Entities) == 0 {
		err = f.whole(repo, m, &b)
	} else {
		err = f.scoped(repo, m, scope.Entities, &b)
	}
	if err != nil {
		return Bundle{}, err
	}
	if b.Decisions, err = decisions(repo, scope.Workstream); err != nil {
		return Bundle{}, fmt.Errorf("decisions: %w", err)
	}
	all, err := decisions(repo, "")
	if err != nil {
		return Bundle{}, fmt.Errorf("notices: %w", err)
	}
	for _, d := range all {
		// One relay gives every ruling of its batch the same text at the same
		// time; it is one notice, sourced from the first of them.
		same := func(n Notice) bool {
			return n.Workstream == d.Workstream && n.At.Equal(d.At) && n.OwnerResponse == d.OwnerResponse && n.ReturnedAnswer == d.ReturnedAnswer
		}
		if d.Scope == trace.ScopeNotify && !slices.ContainsFunc(b.Notices, same) {
			b.Notices = append(b.Notices, Notice{Source: d.Source, Workstream: d.Workstream, Record: d.Record, Revision: d.Revision, At: d.At, OwnerResponse: d.OwnerResponse, ReturnedAnswer: d.ReturnedAnswer})
		}
	}
	return b, nil
}

func (f Files) whole(repo *trace.Repository, m kb.Map, b *Bundle) error {
	for _, e := range m.Entities {
		b.Entities.Entities = append(b.Entities.Entities, entity(e))
	}
	slices.SortFunc(b.Entities.Entities, func(x, y Entity) int { return strings.Compare(x.ID, y.ID) })
	names, err := repo.Subsystems()
	if err != nil {
		return fmt.Errorf("knowledge base: %w", err)
	}
	for _, name := range names {
		p, ok, err := prose(repo, name)
		if err != nil {
			return err
		}
		if ok {
			b.Knowledge = append(b.Knowledge, p)
		}
	}
	return nil
}

func (f Files) scoped(repo *trace.Repository, m kb.Map, names []string, b *Bundle) error {
	fp := m.ResolveEntities(names)
	b.Entities.Unresolved = fp.Unresolved
	byID := map[string]kb.Entity{}
	for _, e := range m.Entities {
		byID[e.ID] = e
	}
	for _, id := range fp.Entities {
		b.Entities.Entities = append(b.Entities.Entities, entity(byID[id]))
	}
	// lineage returns an entity's ID and those of its part_of ancestors,
	// nearest first.
	lineage := func(id string) []string {
		var out []string
		seen := map[string]bool{}
		queue := []string{id}
		for len(queue) > 0 {
			next := queue[0]
			queue = queue[1:]
			if seen[next] {
				continue
			}
			seen[next] = true
			out = append(out, next)
			queue = append(queue, byID[next].PartOf...)
		}
		return out
	}
	read := map[string]bool{} // subsystem -> file exists
	candidates := map[string]bool{}
	for _, id := range fp.Entities {
		for _, name := range lineage(id) {
			candidates[name] = true
		}
	}
	for _, name := range slices.Sorted(maps.Keys(candidates)) {
		p, ok, err := prose(repo, name)
		if err != nil {
			return err
		}
		read[name] = ok
		if ok {
			b.Knowledge = append(b.Knowledge, p)
		}
	}
	unresolved := map[string]bool{}
	for _, name := range fp.Unresolved {
		unresolved[name] = true
	}
	noted := map[string]bool{}
	for _, name := range names {
		e, ok := m.Lookup(name)
		if !ok || unresolved[name] || noted[e.ID] {
			continue
		}
		noted[e.ID] = true
		missing := MissingProse{Entity: e.ID, Looked: []string{}}
		for _, id := range lineage(e.ID) {
			if read[id] {
				missing.Looked = nil
				break
			}
			missing.Looked = append(missing.Looked, trace.ProsePath(id))
		}
		if missing.Looked != nil {
			b.Missing = append(b.Missing, missing)
		}
	}
	return nil
}

func prose(repo *trace.Repository, subsystem string) (Prose, bool, error) {
	content, err := repo.Prose(subsystem)
	if errors.Is(err, fs.ErrNotExist) {
		return Prose{}, false, nil
	}
	if err != nil {
		return Prose{}, false, fmt.Errorf("knowledge base %s: %w", trace.ProsePath(subsystem), err)
	}
	return Prose{Subsystem: subsystem, Source: trace.ProsePath(subsystem), Content: content}, true, nil
}

func entity(e kb.Entity) Entity {
	return Entity{ID: e.ID, Name: e.Name, Paths: append([]string{}, e.Paths...)}
}

// decisions returns the latest revision of every ruling in the workstream, or
// in every workstream of the project when none is given, ordered by
// workstream, time and record ID.
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
		// Revisions of one ruling are read in increasing order.
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
