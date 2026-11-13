package trace

import (
	"os"
	"slices"
	"sync"

	"github.com/kpenfound/osmia/internal/config"
)

// projectSpecSnapshot is the effective project spec of one workgroup at one
// set of source revisions, with the workgroup record it was read under. It is
// shared with the cache and never changed after it is built: readers copy
// what they hand out, or read it in place through a documented accessor.
type projectSpecSnapshot struct {
	group Workgroup
	spec  ProjectSpec
	// found reports whether a spec is recorded. The group is set whenever the
	// workgroup is recorded, whether or not a spec is.
	found bool
	// incoming lists each workstream's incoming graph edges, in graph order.
	incoming map[config.WorkstreamID][]Edge
}

func newProjectSpecSnapshot(g Workgroup, spec ProjectSpec, found bool) *projectSpecSnapshot {
	s := &projectSpecSnapshot{group: g, spec: spec, found: found}
	if len(spec.Graph) > 0 {
		s.incoming = make(map[config.WorkstreamID][]Edge)
		for _, e := range spec.Graph {
			s.incoming[e.To] = append(s.incoming[e.To], e)
		}
	}
	return s
}

// projectSpecKey names the inputs of the choice workgroupProjectSpec makes:
// the workgroups document, then the approved revision, then the immutable
// approval, then the latest draft. Only the inputs the choice reached are set,
// so a draft recorded after an approval does not change the key.
type projectSpecKey struct {
	groupsSeq int64
	// tier is 0 when the workgroup is not recorded, 1 for the approved
	// revision, 2 for the approval and 3 for the latest draft.
	tier int
	// seq is the stored sequence of the document tiers 1 and 3 read.
	seq int64
	// approval is the cached approval decision tier 2 read.
	approval *GroupApproval
}

// projectSpecCache holds the effective project spec of one workgroup. It
// belongs to the workgroup's registry entry and is dropped with the approval
// cache when the last holder of the workgroup trace closes. The durable trace
// stays authoritative: every read probes the sequence of each document the
// choice reached and decodes again when one changed.
type projectSpecCache struct {
	mu sync.Mutex

	groupsValid bool
	groupsSeq   int64
	group       Workgroup
	groupFound  bool

	valid bool
	key   projectSpecKey
	snap  *projectSpecSnapshot
}

func (c *projectSpecCache) reset() {
	c.mu.Lock()
	c.groupsValid, c.valid, c.snap = false, false, nil
	c.group = Workgroup{}
	c.mu.Unlock()
}

// projectSpecSnapshotOf returns the effective project spec of workgroup id
// read through wt, decoding each source document at most once per revision.
// The snapshot is shared: callers must not change it.
func (wt *Repository) projectSpecSnapshotOf(id config.WorkgroupID) (*projectSpecSnapshot, error) {
	c := &projectSpecCache{}
	if wt.shared != nil {
		c = &wt.shared.projectSpec
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	snap, err := wt.cachedProjectSpecSnapshot(c, id)
	if err != nil {
		c.valid = false
	}
	return snap, err
}

func (wt *Repository) cachedProjectSpecSnapshot(c *projectSpecCache, id config.WorkgroupID) (*projectSpecSnapshot, error) {
	groupsSeq, groupsFound, err := wt.documentSeq("", WorkgroupsDocument, WorkgroupsPath)
	if err != nil || !groupsFound {
		c.groupsValid, c.valid = false, false
		return wt.uncachedProjectSpecSnapshot(id)
	}
	if !c.groupsValid || c.groupsSeq != groupsSeq {
		c.groupsValid = false
		groups, _, err := Workgroups(wt)
		if err != nil {
			return nil, err
		}
		c.group, c.groupFound = Workgroup{}, false
		for _, candidate := range groups {
			if candidate.ID == id {
				c.group, c.groupFound = candidate, true
				break
			}
		}
		c.groupsValid, c.groupsSeq = true, groupsSeq
	}
	key := projectSpecKey{groupsSeq: groupsSeq}
	build := func(spec ProjectSpec, found bool) *projectSpecSnapshot {
		c.valid, c.key = true, key
		c.snap = newProjectSpecSnapshot(c.group, spec, found)
		return c.snap
	}
	hit := func() bool { return c.valid && c.key == key }
	if !c.groupFound {
		if hit() {
			return c.snap, nil
		}
		return build(ProjectSpec{}, false), nil
	}
	if len(c.group.Members) > 0 {
		host := c.group.Members[0]
		revSeq, revFound, err := wt.documentSeq(host, WorkgroupRevisionDocument, WorkgroupRevisionPath)
		if err != nil {
			c.valid = false
			return wt.uncachedProjectSpecSnapshot(id)
		}
		if revFound {
			key.tier, key.seq = 1, revSeq
			if hit() {
				return c.snap, nil
			}
			revision, published, err := ApprovedWorkgroupRevision(wt, host)
			if err != nil {
				return nil, err
			}
			if published {
				return build(revision.Spec, true), nil
			}
			key.tier, key.seq = 0, 0
		}
		approval, err := wt.approvalSnapshotOf(id)
		if err != nil {
			return nil, err
		}
		if approval.found {
			key.tier, key.seq, key.approval = 2, 0, approval.decision
			if hit() {
				return c.snap, nil
			}
			return build(approval.decision.ProjectSpec, true), nil
		}
	}
	specsSeq, _, err := wt.documentSeq("", ProjectSpecsDocument, ProjectSpecsPath)
	if err != nil {
		c.valid = false
		return wt.uncachedProjectSpecSnapshot(id)
	}
	key.tier, key.seq, key.approval = 3, specsSeq, nil
	if hit() {
		return c.snap, nil
	}
	spec, found, err := ProjectSpecFor(wt, id)
	if err != nil {
		return nil, err
	}
	return build(spec, found), nil
}

// uncachedProjectSpecSnapshot reads the effective spec from the trace without
// keeping it.
func (wt *Repository) uncachedProjectSpecSnapshot(id config.WorkgroupID) (*projectSpecSnapshot, error) {
	spec, g, found, err := wt.workgroupProjectSpec(id)
	if err != nil {
		return nil, err
	}
	return newProjectSpecSnapshot(g, spec, found), nil
}

// crossRepoProjectSpecSnapshot reads the effective project spec of cross-repo
// workgroup id through a handle on its trace that r's project instance keeps
// for the life of the instance. exists is false when no trace exists yet for
// id. The snapshot is shared: callers must not change it.
func (r *Repository) crossRepoProjectSpecSnapshot(id config.WorkgroupID) (snap *projectSpecSnapshot, exists bool, err error) {
	sharedWorkgroupTraceFor(id).projectSpecReads.Add(1)
	directory, err := r.root.WorkgroupTrace(id)
	if err != nil {
		return nil, false, err
	}
	if _, err := os.Lstat(directory); os.IsNotExist(err) {
		return nil, false, nil
	} else if err != nil {
		return nil, false, err
	}
	project := r
	if r.parent != nil {
		project = r.parent
	}
	wt := project
	if project.Project() != crossRepoProject(id) || project.shared == nil {
		var release func()
		wt, release, err = project.workgroupTraceHandle(id)
		if err != nil {
			return nil, false, err
		}
		defer release()
	}
	snap, err = wt.projectSpecSnapshotOf(id)
	return snap, err == nil, err
}

// CrossRepoProjectSpec reads the recorded Workgroup and project spec of a
// cross-repo workgroup directly from its own trace. The spec is the approved
// workgroup revision when one is published, else the spec embedded in the
// immutable approval, else the latest draft; a draft recorded after approval
// never replaces the accepted decision. Each source document is decoded at
// most once per revision, through a handle on the workgroup's trace that r's
// project instance keeps until it closes (or ReleaseWorkgroupTraces), so the
// read answers promptly even while a long call such as a dispatched
// chief-of-staff turn holds that same instance open elsewhere. It needs no
// real member's own repository open: a project spec and a workgroup's
// membership list are both project-level documents of the workgroup's own
// trace, never scoped to one workstream. r is used only to resolve where the
// service root is; the read never touches r's own trace. found is false when
// no trace exists yet for id, or no spec is recorded. The returned values
// belong to the caller.
func (r *Repository) CrossRepoProjectSpec(id config.WorkgroupID) (ProjectSpec, Workgroup, bool, error) {
	snap, exists, err := r.crossRepoProjectSpecSnapshot(id)
	if err != nil || !exists {
		return ProjectSpec{}, Workgroup{}, false, err
	}
	return cloneProjectSpec(snap.spec), cloneWorkgroup(snap.group), snap.found, nil
}

// IncomingEdgesOf returns the graph edges of the given types (every type when
// none is named) that point at stream, in graph order, from the effective
// project spec of the workgroup stream belongs to. found is false when stream
// belongs to no workgroup or its workgroup has no recorded spec, exactly as
// for ProjectSpecOf. The edges belong to the caller.
//
// For a member of a cross-repo workgroup the edges are read from an index
// built once per revision of the sources of the effective spec, so repeated
// calls at an unchanged revision decode nothing and copy only the edges that
// point at stream, never the spec. Scheduling-path callers that need the
// graph and not the whole spec should use this instead of ProjectSpecOf.
func (r *Repository) IncomingEdgesOf(stream config.WorkstreamID, types ...string) ([]Edge, bool, error) {
	id, err := r.CrossRepoWorkgroupOf(stream)
	if err != nil {
		return nil, false, err
	}
	var edges []Edge
	found := false
	if id != "" {
		snap, exists, err := r.crossRepoProjectSpecSnapshot(id)
		if err != nil || !exists {
			return nil, false, err
		}
		edges, found = snap.incoming[stream], snap.found
	} else {
		spec, _, ok, err := r.ProjectSpecOf(stream)
		if err != nil || !ok {
			return nil, false, err
		}
		found = true
		for _, e := range spec.Graph {
			if e.To == stream {
				edges = append(edges, e)
			}
		}
	}
	var out []Edge
	for _, e := range edges {
		if len(types) == 0 || slices.Contains(types, e.Type) {
			out = append(out, e)
		}
	}
	return out, found, nil
}

func cloneWorkgroup(g Workgroup) Workgroup {
	g.Members = slices.Clone(g.Members)
	if g.Conversation != nil {
		c := *g.Conversation
		g.Conversation = &c
	}
	return g
}

// ProjectSpecDecodeCount reports decodes of the project spec documents
// (project-specs and workgroup-revision) on workgroup id's shared trace in
// this process. Approval decodes are counted by GroupApprovalDecodeCount.
func ProjectSpecDecodeCount(id config.WorkgroupID) uint64 {
	return sharedWorkgroupTraceFor(id).projectSpecDecodes.Load()
}

// ProjectSpecCached reports whether workgroup id's registry entry holds a
// decoded effective project spec.
func ProjectSpecCached(id config.WorkgroupID) bool {
	c := &sharedWorkgroupTraceFor(id).projectSpec
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.valid
}

