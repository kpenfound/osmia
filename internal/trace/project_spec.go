package trace

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/kpenfound/osmia/internal/config"
)

// The types a workgroup graph's edges carry. Only contract edges resolve
// here, when the contract they name is in the ratified project spec; hard
// block and merge order resolve on an upstream pull request's merge, which
// belongs to unit edges. Recording them here lets the architect propose a
// complete graph before that resolution exists.
const (
	EdgeContract   = "contract"
	EdgeHardBlock  = "hard_block"
	EdgeMergeOrder = "merge_order"
)

// Edge is one dependency of a workgroup's graph: a typed relationship from
// one member workstream to another.
type Edge struct {
	From config.WorkstreamID `json:"from"`
	To   config.WorkstreamID `json:"to"`
	Type string              `json:"type"`
}

func validEdgeType(t string) bool {
	return t == EdgeContract || t == EdgeHardBlock || t == EdgeMergeOrder
}

// Contract is one named cross-repo interface section of the project spec:
// the seam at which two or more member workstreams' repos meet.
type Contract struct {
	Name string `json:"name"`
	Text string `json:"text"`
}

// Slice is the part of the project spec that belongs to one member
// workstream's repo: its own spec and plan content, and which numbered
// criteria of the project spec's acceptance criteria are contract sections.
// A contract section is read-only to that workstream's own roles; a change
// to it is refused or routed to a project-level amendment, never applied
// inside the workstream.
type Slice struct {
	Workstream       config.WorkstreamID `json:"workstream"`
	Spec             string              `json:"spec"`
	Plan             string              `json:"plan"`
	ContractCriteria []int               `json:"contract_criteria,omitempty"`
}

// ProjectSpec is the committee-debated spec for a multi-repo workgroup: the
// intended behaviour, the cross-repo contracts at the seams between repos,
// the workgroup's workstream graph and each member's own slice. One owner
// ratification covers it and every member's slice at once.
type ProjectSpec struct {
	Behavior  string     `json:"behavior"`
	Contracts []Contract `json:"contracts,omitempty"`
	Graph     []Edge     `json:"graph,omitempty"`
	Slices    []Slice    `json:"slices"`
}

// SliceFor returns the member's own slice of spec, when one is recorded.
func SliceFor(spec ProjectSpec, stream config.WorkstreamID) (Slice, bool) {
	for _, s := range spec.Slices {
		if s.Workstream == stream {
			return s, true
		}
	}
	return Slice{}, false
}

// parseSpecCriterion returns the number a "spec#<n>" citation names, or
// false for any other form. internal/trace cannot import internal/plan,
// which already owns this citation form, without an import cycle through
// internal/kb, so the minimal parse is repeated here.
func parseSpecCriterion(citation string) (int, bool) {
	rest, ok := strings.CutPrefix(citation, "spec#")
	if !ok {
		return 0, false
	}
	n, err := strconv.Atoi(rest)
	if err != nil || n < 1 {
		return 0, false
	}
	return n, true
}

// ContractCitation reports whether citation names one of stream's own
// contract-criteria numbers in spec: a section its slice marks read-only.
func ContractCitation(spec ProjectSpec, stream config.WorkstreamID, citation string) bool {
	slice, ok := SliceFor(spec, stream)
	if !ok || len(slice.ContractCriteria) == 0 {
		return false
	}
	n, ok := parseSpecCriterion(citation)
	if !ok {
		return false
	}
	return slices.Contains(slice.ContractCriteria, n)
}

// ValidateGraph refuses a graph naming an edge type other than one of
// EdgeContract, EdgeHardBlock or EdgeMergeOrder, or an edge whose From or To
// is not one of members, or whose edges, of any type together, form a
// cycle.
func ValidateGraph(members []config.WorkstreamID, edges []Edge) error {
	known := make(map[config.WorkstreamID]bool, len(members))
	for _, m := range members {
		known[m] = true
	}
	adjacency := map[config.WorkstreamID][]config.WorkstreamID{}
	for _, e := range edges {
		if !validEdgeType(e.Type) {
			return fmt.Errorf("edge %s -> %s has an unknown type %q", e.From, e.To, e.Type)
		}
		if !known[e.From] {
			return fmt.Errorf("edge names unknown workstream %s", e.From)
		}
		if !known[e.To] {
			return fmt.Errorf("edge names unknown workstream %s", e.To)
		}
		adjacency[e.From] = append(adjacency[e.From], e.To)
	}
	const (
		unvisited = 0
		visiting  = 1
		done      = 2
	)
	state := map[config.WorkstreamID]int{}
	var stack []config.WorkstreamID
	var visit func(config.WorkstreamID) error
	visit = func(n config.WorkstreamID) error {
		switch state[n] {
		case done:
			return nil
		case visiting:
			stack = append(stack, n)
			return fmt.Errorf("the graph has a cycle: %s", cyclePath(stack))
		}
		state[n] = visiting
		stack = append(stack, n)
		for _, next := range adjacency[n] {
			if err := visit(next); err != nil {
				return err
			}
		}
		stack = stack[:len(stack)-1]
		state[n] = done
		return nil
	}
	for _, m := range members {
		if err := visit(m); err != nil {
			return err
		}
	}
	return nil
}

func cyclePath(stack []config.WorkstreamID) string {
	strs := make([]string, len(stack))
	for i, s := range stack {
		strs[i] = string(s)
	}
	return strings.Join(strs, " -> ")
}

func validSlice(s Slice) bool {
	if s.Workstream == "" || config.CheckWorkstreamIDs(s.Workstream) != nil {
		return false
	}
	for _, n := range s.ContractCriteria {
		if n < 1 {
			return false
		}
	}
	return true
}

func (p ProjectSpec) valid(members []config.WorkstreamID) error {
	if !present(p.Behavior) {
		return fmt.Errorf("a project spec requires its intended behaviour")
	}
	for _, c := range p.Contracts {
		if !present(c.Name) || !present(c.Text) {
			return fmt.Errorf("every contract needs a name and text")
		}
	}
	if err := ValidateGraph(members, p.Graph); err != nil {
		return err
	}
	seen := map[config.WorkstreamID]bool{}
	for _, s := range p.Slices {
		if !validSlice(s) {
			return fmt.Errorf("slice of %s is invalid", s.Workstream)
		}
		if seen[s.Workstream] {
			return fmt.Errorf("slice of %s is recorded twice", s.Workstream)
		}
		seen[s.Workstream] = true
	}
	return nil
}

// ValidateProjectSpec validates the shared text and graph before member writes.
func ValidateProjectSpec(p ProjectSpec, members []config.WorkstreamID) error {
	return p.valid(members)
}

// ProjectSpecsPath is the project document holding every workgroup's own
// project spec, and ProjectSpecsDocument its record ID.
const (
	ProjectSpecsPath     = "project-specs.json"
	ProjectSpecsDocument = "project-specs"
)

type projectSpecEntry struct {
	Workgroup config.WorkgroupID `json:"workgroup"`
	Spec      ProjectSpec        `json:"spec"`
}

type projectSpecsFile struct {
	Version int                `json:"version"`
	Specs   []projectSpecEntry `json:"specs"`
}

func latestProjectSpecs(r *Repository) (string, int, error) {
	return latestDocument(r, ProjectSpecsPath)
}

func readProjectSpecs(r *Repository) ([]projectSpecEntry, int, error) {
	content, revision, err := latestProjectSpecs(r)
	if err != nil {
		return nil, 0, err
	}
	if content == "" {
		return nil, 0, nil
	}
	var file projectSpecsFile
	if err := json.Unmarshal([]byte(content), &file); err != nil {
		return nil, revision, err
	}
	return file.Specs, revision, nil
}

// ProjectSpecFor returns the recorded project spec of workgroup, and whether
// one is recorded. r must be the project's own repository.
func ProjectSpecFor(r *Repository, workgroup config.WorkgroupID) (ProjectSpec, bool, error) {
	spec, _, found, err := ProjectSpecWithRevisionFor(r, workgroup)
	return spec, found, err
}

// ProjectSpecWithRevisionFor reads the spec and its trace document revision
// from the same snapshot.
func ProjectSpecWithRevisionFor(r *Repository, workgroup config.WorkgroupID) (ProjectSpec, int, bool, error) {
	entries, revision, err := readProjectSpecs(r)
	if err != nil {
		return ProjectSpec{}, 0, false, err
	}
	for _, e := range entries {
		if e.Workgroup == workgroup {
			return e.Spec, revision, true, nil
		}
	}
	return ProjectSpec{}, 0, false, nil
}

// RecordedProjectSpecRevision locates a recorded revision with exactly spec's
// content, including when an immutable approval predates explicit revision pins.
func RecordedProjectSpecRevision(r *Repository, workgroup config.WorkgroupID, spec ProjectSpec) (int, error) {
	docs, err := Read[Document](r, "")
	if err != nil {
		return 0, err
	}
	for _, doc := range slices.Backward(docs) {
		if doc.ID != ProjectSpecsDocument {
			continue
		}
		var file projectSpecsFile
		if err := json.Unmarshal([]byte(doc.Content), &file); err != nil {
			return 0, err
		}
		for _, entry := range file.Specs {
			if entry.Workgroup == workgroup && equalJSON(entry.Spec, spec) {
				return doc.Revision, nil
			}
		}
	}
	return 0, nil
}

// ProjectSpecOf returns the recorded project spec of the workgroup stream
// belongs to, and whether one is recorded for it: false both when stream has
// no recorded workgroup and when its workgroup has no recorded project
// spec. r must be the project's own repository. The cross-repo pointer
// (CrossRepoWorkgroupOf) is checked first: a member of a cross-repo
// workgroup is also recorded in r's own same-repo workgroups (today's
// EnsureWorkgroups wraps every non-session workstream into one before it
// learns of any cross-repo membership), so checking that first would always
// find that unrelated, spec-less entry and never reach the cross-repo one.
// When stream carries no cross-repo pointer, the project spec is read from r
// directly; when it does, it is read from that workgroup's own trace instead,
// never duplicated into r (charter#2).
func (r *Repository) ProjectSpecOf(stream config.WorkstreamID) (ProjectSpec, config.WorkgroupID, bool, error) {
	id, err := r.CrossRepoWorkgroupOf(stream)
	if err != nil {
		return ProjectSpec{}, "", false, err
	}
	if id != "" {
		spec, _, found, err := r.CrossRepoProjectSpec(id)
		return spec, id, found, err
	}
	groups, _, err := Workgroups(r)
	if err != nil {
		return ProjectSpec{}, "", false, err
	}
	if g, ok := WorkgroupOf(groups, stream); ok {
		revision, published, err := ApprovedWorkgroupRevision(r, g.Members[0])
		if err != nil {
			return ProjectSpec{}, g.ID, false, err
		}
		if published {
			return revision.Spec, g.ID, true, nil
		}
		spec, found, err := ProjectSpecFor(r, g.ID)
		return spec, g.ID, found, err
	}
	return ProjectSpec{}, "", false, nil
}

// projectSpecOfLocked is ProjectSpecOf's variant for a caller that already
// holds member's own scoped repository locked (target.mu), with v already
// loaded for member under that same lock: used by FileAmendment's second,
// post-lock contractTouch, it must never call Workflow, CrossRepoWorkgroupOf
// or ProjectSpecOf, since scoped(member) caches and returns that identical,
// already-locked instance, and a second r.mu.Lock() on it self-deadlocks.
// The cross-repo pointer is instead read directly from v. When project is
// itself the cross-repo workgroup's own trace (its project identity already
// matches the pointer), its approval or draft is read through the existing
// handle, without re-opening the trace or re-locking the asking member.
func projectSpecOfLocked(project *Repository, v *workflowView, member config.WorkstreamID, records []Record) (ProjectSpec, config.WorkgroupID, bool, error) {
	id := config.WorkgroupID(v.states[CrossRepoWorkgroupSubject].Value)
	if id != "" {
		if config.ProjectID(project.Project()) == crossRepoProject(id) {
			// The locked target is the real asking member. The shared
			// authority is a different scoped handle; use the existing
			// workgroup instance and never resolve or lock the member again.
			groups, _, err := Workgroups(project)
			if err != nil {
				return ProjectSpec{}, id, false, err
			}
			for _, g := range groups {
				if g.ID == id && len(g.Members) > 0 {
					revision, published, err := ApprovedWorkgroupRevision(project, g.Members[0])
					if err != nil {
						return ProjectSpec{}, id, false, err
					}
					if published {
						return revision.Spec, id, true, nil
					}
				}
			}
			decision, decided, err := project.groupApproval(id)
			if err != nil {
				return ProjectSpec{}, id, false, err
			}
			if decided {
				return decision.ProjectSpec, id, true, nil
			}
			spec, found, err := ProjectSpecFor(project, id)
			return spec, id, found, err
		}
		spec, _, found, err := project.CrossRepoProjectSpec(id)
		return spec, id, found, err
	}
	groups, _, err := Workgroups(project)
	if err != nil {
		return ProjectSpec{}, "", false, err
	}
	if g, ok := WorkgroupOf(groups, member); ok {
		var revision WorkgroupRevision
		var published bool
		if g.Members[0] == member {
			// FileAmendment already loaded this member under target.mu.
			// Read its revision from those records without re-locking it.
			var docs []Document
			for _, record := range records {
				if d, ok := record.(Document); ok && d.Workstream == member {
					docs = append(docs, d)
				}
			}
			revision, published, err = approvedWorkgroupRevision(docs)
		} else {
			revision, published, err = ApprovedWorkgroupRevision(project, g.Members[0])
		}
		if err != nil {
			return ProjectSpec{}, g.ID, false, err
		}
		if published {
			return revision.Spec, g.ID, true, nil
		}
		spec, found, err := ProjectSpecFor(project, g.ID)
		return spec, g.ID, found, err
	}
	return ProjectSpec{}, "", false, nil
}

// RecordProjectSpec validates spec against workgroup's members, refusing
// and writing nothing if a graph edge names an unknown member or an edge of
// any type forms a cycle, or a slice is invalid or repeated. Otherwise it
// appends spec as the next document revision of the workgroup's project
// spec. Earlier revisions remain readable. One owner ratification seals
// the project spec, its graph and every member's slice together.
// r must be the project's own repository.
func RecordProjectSpec(ctx context.Context, r *Repository, workgroup Workgroup, spec ProjectSpec, at time.Time, actor Actor, cause string) error {
	if err := spec.valid(workgroup.Members); err != nil {
		return err
	}
	entries, revision, err := readProjectSpecs(r)
	if err != nil {
		return err
	}
	next := make([]projectSpecEntry, 0, len(entries)+1)
	replaced := false
	for _, e := range entries {
		if e.Workgroup == workgroup.ID {
			next = append(next, projectSpecEntry{Workgroup: workgroup.ID, Spec: spec})
			replaced = true
			continue
		}
		next = append(next, e)
	}
	if !replaced {
		next = append(next, projectSpecEntry{Workgroup: workgroup.ID, Spec: spec})
	}
	data, err := json.Marshal(projectSpecsFile{Version: 1, Specs: next})
	if err != nil {
		return err
	}
	return r.Append(ctx, Document{
		Header:  Header{Schema: "osmia.trace.document", Version: Version, ID: ProjectSpecsDocument, Revision: revision + 1, Project: r.Project(), At: at, Actor: actor, Cause: cause},
		Path:    ProjectSpecsPath,
		Content: string(data),
	})
}

// ProjectSpecActive reports whether workgroup's project spec is still
// active: present, and the workgroup is not yet recorded resolved. A
// workgroup's project spec stays active for the chief of staff and every
// unresolved member, including a dependent still waiting on or repairing
// after an upstream merge, and leaves active state only once every member
// is resolved. r must be the project's own repository or the workgroup's
// authority. Cross-repo retirement is read from that authority, without
// requiring any member's retained history.
func ProjectSpecActive(r *Repository, workgroup config.WorkgroupID) (bool, error) {
	if config.ProjectID(r.Project()) != crossRepoProject(workgroup) {
		groups, _, err := Workgroups(r)
		if err != nil {
			return false, err
		}
		local := slices.ContainsFunc(groups, func(g Workgroup) bool { return g.ID == workgroup })
		if !local {
			authority, err := OpenWorkgroupAuthority(r.root, workgroup)
			if err != nil {
				if authority != nil {
					authority.Close()
				}
				return false, err
			}
			defer authority.Close()
			r = authority
		}
	}
	resolved, _, err := ResolvedWorkgroups(r)
	if err != nil {
		return false, err
	}
	return !slices.Contains(resolved, workgroup), nil
}
