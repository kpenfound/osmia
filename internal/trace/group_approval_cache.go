package trace

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sync"

	"github.com/kpenfound/osmia/internal/config"
)

// approvalSnapshot is the approval answer of one workgroup at a pair of
// document revisions. decision is nil unless found, and is shared with the
// cache: callers read it and never change or return it.
type approvalSnapshot struct {
	required bool
	found    bool
	decision *GroupApproval
}

// approvalCache holds the decoded approval answer of one workgroup, keyed by
// the stored sequence of the workgroups document and of the approval
// document it was decoded from. It belongs to the workgroup's registry entry
// and is dropped when the last holder of the workgroup trace closes. The
// durable trace stays authoritative: every read probes both sequences and
// decodes again when either changed.
type approvalCache struct {
	mu sync.Mutex

	groupsValid bool
	groupsSeq   int64
	host        config.WorkstreamID
	required    bool

	approvalValid bool
	approvalHost  config.WorkstreamID
	approvalSeq   int64
	approvalFound bool
	decision      *GroupApproval
}

func (c *approvalCache) reset() {
	c.mu.Lock()
	c.groupsValid, c.approvalValid, c.decision = false, false, nil
	c.mu.Unlock()
}

// documentSeq returns the stored sequence of the latest revision of the
// document id at path in scope's documents, without decoding it.
func (r *Repository) documentSeq(scope config.WorkstreamID, id, path string) (int64, bool, error) {
	target, err := r.scoped(scope)
	if err != nil {
		return 0, false, err
	}
	if target.reads == nil {
		return 0, false, fmt.Errorf("%w: trace repository is closed", ErrLocked)
	}
	var seq int64
	err = target.reads.QueryRowContext(context.Background(), "SELECT seq FROM records WHERE schema = 'osmia.trace.document' AND path = ? AND id = ? ORDER BY seq DESC LIMIT 1", scopePath(Header{Workstream: scope})+"documents.jsonl", id).Scan(&seq)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, false, nil
	}
	return seq, err == nil, err
}

// groupAuthority reads the host and approval requirement of workgroup id
// from the workgroups document of wt.
func groupAuthority(wt *Repository, id config.WorkgroupID) (config.WorkstreamID, bool, error) {
	groups, _, err := Workgroups(wt)
	if err != nil {
		return "", true, err
	}
	for _, g := range groups {
		if g.ID == id && len(g.Members) > 0 {
			return g.Members[0], !(g.Conversation != nil && len(g.Members) == 2), nil
		}
	}
	return "", true, fmt.Errorf("workgroup %s has no authority", id)
}

// groupApprovalSnapshot answers from state decoded at most once per revision
// of the workgroups and approval documents.
func (r *Repository) groupApprovalSnapshot(id config.WorkgroupID) (approvalSnapshot, error) {
	project := r
	if r.parent != nil {
		project = r.parent
	}
	wt := project
	if project.Project() != crossRepoProject(id) {
		var release func()
		var err error
		wt, release, err = project.workgroupTraceHandle(id)
		if err != nil {
			return approvalSnapshot{required: true}, err
		}
		defer release()
	}
	return wt.approvalSnapshotOf(id)
}

func (wt *Repository) approvalSnapshotOf(id config.WorkgroupID) (approvalSnapshot, error) {
	c := &approvalCache{}
	if wt.shared != nil {
		c = &wt.shared.approval
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	groupsSeq, groupsFound, err := wt.documentSeq("", WorkgroupsDocument, WorkgroupsPath)
	if err != nil || !groupsFound {
		c.groupsValid, c.approvalValid = false, false
		return wt.uncachedApprovalSnapshot(id)
	}
	if !c.groupsValid || c.groupsSeq != groupsSeq {
		host, required, err := groupAuthority(wt, id)
		if err != nil {
			c.groupsValid = false
			return approvalSnapshot{required: true}, err
		}
		c.groupsValid, c.groupsSeq, c.host, c.required = true, groupsSeq, host, required
	}
	approvalSeq, approvalFound, err := wt.documentSeq(c.host, GroupApprovalDocument, GroupApprovalPath)
	if err != nil {
		c.approvalValid = false
		return wt.uncachedApprovalSnapshot(id)
	}
	if !c.approvalValid || c.approvalHost != c.host || c.approvalSeq != approvalSeq || c.approvalFound != approvalFound {
		c.approvalValid = false
		var decision *GroupApproval
		if approvalFound {
			read, _, found, err := ReadGroupApproval(wt, c.host)
			if err != nil {
				return approvalSnapshot{required: true}, err
			}
			approvalFound = found
			if found {
				decision = &read
			}
		}
		c.approvalValid, c.approvalHost, c.approvalSeq, c.approvalFound, c.decision = true, c.host, approvalSeq, approvalFound, decision
	}
	return approvalSnapshot{required: c.required, found: c.approvalFound, decision: c.decision}, nil
}

// uncachedApprovalSnapshot reads the answer from the trace without keeping
// it. It also reports the errors a read of a damaged or absent document gives.
func (wt *Repository) uncachedApprovalSnapshot(id config.WorkgroupID) (approvalSnapshot, error) {
	host, required, err := groupAuthority(wt, id)
	if err != nil {
		return approvalSnapshot{required: true}, err
	}
	decision, _, found, err := ReadGroupApproval(wt, host)
	if err != nil {
		return approvalSnapshot{required: true}, err
	}
	snap := approvalSnapshot{required: required, found: found}
	if found {
		snap.decision = &decision
	}
	return snap, nil
}

// workgroupTraceHandle returns a handle on workgroup id's own trace for
// reading from this project's instance, and the function that gives it back.
// The project instance keeps one reference per workgroup until its own Close,
// so repeated reads share one open handle instead of acquiring the trace each
// time. After Close begins, the handle is opened for the one call.
func (r *Repository) workgroupTraceHandle(id config.WorkgroupID) (*Repository, func(), error) {
	r.groupTracesMu.Lock()
	if r.groupTracesClosed {
		r.groupTracesMu.Unlock()
		return r.transientWorkgroupTrace(id)
	}
	if wt := r.groupTraces[id]; wt != nil {
		r.groupTracesMu.Unlock()
		return wt, func() {}, nil
	}
	r.groupTracesMu.Unlock()
	wt, err := openSharedWorkgroupTrace(r.root, id, nil)
	if err != nil {
		if wt != nil {
			wt.Close()
		}
		return nil, nil, err
	}
	r.groupTracesMu.Lock()
	defer r.groupTracesMu.Unlock()
	if r.groupTracesClosed {
		return wt, func() { wt.Close() }, nil
	}
	if kept := r.groupTraces[id]; kept != nil {
		wt.Close()
		return kept, func() {}, nil
	}
	if r.groupTraces == nil {
		r.groupTraces = map[config.WorkgroupID]*Repository{}
	}
	r.groupTraces[id] = wt
	return wt, func() {}, nil
}

func (r *Repository) transientWorkgroupTrace(id config.WorkgroupID) (*Repository, func(), error) {
	wt, err := openSharedWorkgroupTrace(r.root, id, nil)
	if err != nil {
		if wt != nil {
			wt.Close()
		}
		return nil, nil, err
	}
	return wt, func() { wt.Close() }, nil
}

// releaseWorkgroupTraces gives back every workgroup trace reference this
// project instance kept for approval reads, and stops it keeping more.
func (r *Repository) releaseWorkgroupTraces() error {
	return r.dropWorkgroupTraces(true)
}

// ReleaseWorkgroupTraces gives back the workgroup trace references this
// project instance kept for approval reads. The next approval read opens the
// trace again, so a trace held elsewhere is not pinned by this instance.
func (r *Repository) ReleaseWorkgroupTraces() error {
	return r.dropWorkgroupTraces(false)
}

func (r *Repository) dropWorkgroupTraces(final bool) error {
	r.groupTracesMu.Lock()
	kept := r.groupTraces
	r.groupTraces = nil
	if final {
		r.groupTracesClosed = true
	}
	r.groupTracesMu.Unlock()
	var errs []error
	for _, wt := range kept {
		errs = append(errs, wt.Close())
	}
	return errors.Join(errs...)
}

// GroupApprovalDecodeCount reports approval document decodes on workgroup id's
// shared trace in this process.
func GroupApprovalDecodeCount(id config.WorkgroupID) uint64 {
	return sharedWorkgroupTraceFor(id).approvalDecodes.Load()
}

// ApprovalDecodeCount reports approval document decodes by this handle.
func (r *Repository) ApprovalDecodeCount() uint64 {
	return r.approvalDecodes.Load()
}

// GroupApprovalCached reports whether workgroup id's registry entry holds a
// decoded approval answer.
func GroupApprovalCached(id config.WorkgroupID) bool {
	c := &sharedWorkgroupTraceFor(id).approval
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.groupsValid || c.approvalValid
}
