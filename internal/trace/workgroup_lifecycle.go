package trace

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"slices"
	"strings"

	"github.com/kpenfound/osmia/internal/config"
)

// ErrMemberRetired indicates a recoverable hold on an operation needing
// unavailable member history. Shared authority access does not satisfy it.
var ErrMemberRetired = errors.New("retired workgroup member history is unavailable; operation remains held")

type RetiredMemberError struct {
	Project    config.ProjectID
	Workstream config.WorkstreamID
}

func (e *RetiredMemberError) Error() string {
	return fmt.Sprintf("%s: project %s workstream %s", ErrMemberRetired, e.Project, e.Workstream)
}

func (e *RetiredMemberError) Unwrap() error { return ErrMemberRetired }

// WorkgroupMemberLifecycle checks durable retirement evidence for a member of
// the named authority. False means live, never merely absent. The callback must
// not read or lock member history: callers may already hold a member mutex.
// An unavailable or mismatched identity must return an error, not retirement.
type WorkgroupMemberLifecycle func(config.WorkgroupID, config.WorkstreamID) (bool, error)

// SetWorkgroupMemberLifecycle installs the service's evidence reader on an
// already open project handle. Every shared binding observes it per lookup.
func (r *Repository) SetWorkgroupMemberLifecycle(resolve WorkgroupMemberLifecycle) error {
	if r.stream != "" {
		return fmt.Errorf("workgroup lifecycle belongs to the project handle")
	}
	r.memberLifecycle.Store(&resolve)
	return nil
}

func (r *Repository) workgroupMemberRetired(group config.WorkgroupID, stream config.WorkstreamID) (bool, error) {
	if resolve := r.memberLifecycle.Load(); resolve != nil && *resolve != nil {
		return (*resolve)(group, stream)
	}
	return false, nil
}

// MemberRetired reads a binding's current lifecycle without opening its trace.
// Fixed membership, decisions and approval pins remain in the shared authority.
func (r *Repository) MemberRetired(stream config.WorkstreamID) (bool, error) {
	m, ok := r.memberBinding(stream)
	if !ok {
		return false, nil
	}
	group, ok := WorkgroupProjectID(r.Project())
	if !ok {
		return false, ErrConflict
	}
	return m.Project.workgroupMemberRetired(group, stream)
}

// memberReadError requires the resolved member's mutex. Lifecycle readers must
// use the shared receiver: the child may have been closed since resolution.
// The evidence callback does not lock or read the member's history.
func (r *Repository) memberReadError(stream config.WorkstreamID) error {
	retired, err := r.MemberRetired(stream)
	if err != nil {
		return err
	}
	if retired {
		m, _ := r.memberBinding(stream)
		return &RetiredMemberError{Project: m.Project.Project(), Workstream: stream}
	}
	return nil
}

// SetQuestionReadBoundary coordinates reads after member resolution and before
// locking. The callback holds no repository lock and must be cancellable.
func (r *Repository) SetQuestionReadBoundary(boundary func(config.WorkstreamID) error) error {
	if r.stream != "" {
		return ErrConflict
	}
	r.questionReadBoundary.Store(&boundary)
	return nil
}

// questionRead is lockQuestionRead for a read of committed records alone,
// which takes no lock: it never waits on a write in progress.
func (r *Repository) questionRead(stream config.WorkstreamID) (*Repository, error) {
	target, err := r.scoped(stream)
	if err != nil {
		return nil, err
	}
	if boundary := r.questionReadBoundary.Load(); boundary != nil && *boundary != nil {
		if err := (*boundary)(stream); err != nil {
			return nil, err
		}
	}
	if err := r.memberReadError(stream); err != nil {
		return nil, err
	}
	return target, nil
}

func (r *Repository) lockQuestionRead(stream config.WorkstreamID) (*Repository, error) {
	target, err := r.scoped(stream)
	if err != nil {
		return nil, err
	}
	if boundary := r.questionReadBoundary.Load(); boundary != nil && *boundary != nil {
		if err := (*boundary)(stream); err != nil {
			return nil, err
		}
	}
	target.mu.Lock()
	if err := r.memberReadError(stream); err != nil {
		target.mu.Unlock()
		return nil, err
	}
	return target, nil
}

func (r *Repository) memberBinding(stream config.WorkstreamID) (CrossRepoMember, bool) {
	r.membersMu.RLock()
	defer r.membersMu.RUnlock()
	m, ok := r.members[stream]
	return m, ok
}

func (r *Repository) memberBindings() map[config.WorkstreamID]CrossRepoMember {
	r.membersMu.RLock()
	defer r.membersMu.RUnlock()
	return maps.Clone(r.members)
}

// retiredRead verifies lifecycle evidence again when a live aggregate meets a
// retired binding. An error carrying the sentinel alone is not retirement.
func (r *Repository) retiredRead(stream config.WorkstreamID, readErr error) (bool, error) {
	if !errors.Is(readErr, ErrMemberRetired) {
		return false, readErr
	}
	retired, err := r.MemberRetired(stream)
	if err != nil {
		return false, err
	}
	if !retired {
		return false, readErr
	}
	return true, nil
}

func liveMemberRepository(m CrossRepoMember) (*Repository, error) {
	path, err := m.Project.root.WorkstreamTrace(m.Project.Project(), m.Workstream)
	if err != nil {
		return nil, err
	}
	if _, err := os.Lstat(path); err != nil {
		return nil, fmt.Errorf("live member %s trace: %w", m.Workstream, err)
	}
	return m.Project.scoped(m.Workstream)
}

func (r *Repository) bindMembers(members map[config.WorkstreamID]CrossRepoMember) error {
	if members == nil {
		return nil
	}
	r.membersMu.Lock()
	defer r.membersMu.Unlock()
	if r.members != nil {
		for stream, old := range r.members {
			next, ok := members[stream]
			if !ok || old.Project.Project() != next.Project.Project() {
				return fmt.Errorf("%w: shared member %s identity changed", ErrConflict, stream)
			}
		}
	}
	r.members = maps.Clone(members)
	return nil
}

// ErrMembershipChanged reports member bindings that differ from the
// workgroup's recorded membership: a member joined, or left, after the
// caller read it. Reading the membership again and binding it resolves it.
var ErrMembershipChanged = errors.New("workgroup membership differs from its bindings")

func (r *Repository) validateMemberBindings(id config.WorkgroupID, members map[config.WorkstreamID]CrossRepoMember) error {
	groups, _, err := Workgroups(r)
	if err != nil {
		return err
	}
	for _, group := range groups {
		if group.ID != id {
			continue
		}
		if len(group.Members) != len(members)+1 {
			return fmt.Errorf("%w: %w: workgroup %s requires its fixed membership", ErrConflict, ErrMembershipChanged, id)
		}
		for _, stream := range group.Members[1:] {
			if _, ok := members[stream]; !ok {
				return fmt.Errorf("%w: %w: missing member binding %s", ErrConflict, ErrMembershipChanged, stream)
			}
		}
		approval, _, found, err := ReadGroupApproval(r, group.Members[0])
		if err != nil {
			return err
		}
		if found {
			approved, err := ApprovedWorkgroupMembers(r, group, approval)
			if err != nil {
				return err
			}
			for stream, project := range approved {
				m, ok := members[stream]
				if !ok || m.Project.Project() != project {
					return fmt.Errorf("%w: member %s disagrees with approved project %s", ErrConflict, stream, project)
				}
			}
		}
		return nil
	}
	return fmt.Errorf("%w: workgroup %s has no recorded membership", ErrConflict, id)
}

// OpenWorkgroupAuthority opens the original group journal and host thread
// without requiring member history. It grants no member-side effect or journal
// completion; callers must separately reconcile each recorded prerequisite.
// Each successful open owns one Close, including overlapping opens.
func OpenWorkgroupAuthority(root config.Root, id config.WorkgroupID) (*Repository, error) {
	return openSharedWorkgroupTrace(root, id, nil)
}

// SetQuestionCommitBoundary installs service interruption coordination after
// durable member ruling/relay commits, outside repository locks.
func (r *Repository) SetQuestionCommitBoundary(boundary func(string) error) error {
	if r.stream != "" {
		return ErrConflict
	}
	r.questionCommitBoundary.Store(&boundary)
	return nil
}

func (r *Repository) afterQuestionCommit(stream config.WorkstreamID, stage string) error {
	project := r
	if member, ok := r.memberBinding(stream); ok {
		project = member.Project
	}
	if boundary := project.questionCommitBoundary.Load(); boundary != nil && *boundary != nil {
		return (*boundary)(stage)
	}
	return nil
}

// ApprovedWorkgroupMembers preserves the original approval and accepts only
// additions pinned by the owner's presented delta and its application journal.
// Pending allocation not yet in membership grants no binding.
func ApprovedWorkgroupMembers(r *Repository, g Workgroup, approval GroupApproval) (map[config.WorkstreamID]config.ProjectID, error) {
	projects := map[config.WorkstreamID]config.ProjectID{}
	if approval.Workgroup != g.ID {
		return nil, ErrConflict
	}
	for _, pin := range approval.Members {
		if !slices.Contains(g.Members[1:], pin.Workstream) {
			return nil, ErrConflict
		}
		if _, duplicate := projects[pin.Workstream]; duplicate {
			return nil, ErrConflict
		}
		projects[pin.Workstream] = pin.Project
	}
	if len(projects) == len(g.Members)-1 {
		return projects, nil
	}
	docs, err := Read[Document](r, g.Members[0])
	if err != nil {
		return nil, err
	}
	latest := map[string]Document{}
	for _, doc := range docs {
		if strings.HasPrefix(doc.ID, "workgroup-change-") && doc.Revision > latest[doc.ID].Revision {
			latest[doc.ID] = doc
		}
	}
	type journal struct {
		ID                              string              `json:"id"`
		Workgroup                       config.WorkgroupID  `json:"workgroup"`
		Host                            config.WorkstreamID `json:"host"`
		State                           string              `json:"state"`
		Before, After, Members, Request json.RawMessage
		Impact                          struct {
			Streams []struct {
				Workstream config.WorkstreamID
				Action     string
			}
		}
		Decision *struct {
			Revision int
			Decision string
		}
		Followups map[config.WorkstreamID]config.WorkstreamID
	}
	// Iteration permits a later approved delta to follow up an earlier addition.
	for changed := true; changed; {
		changed = false
		for _, doc := range latest {
			var c journal
			if err := json.Unmarshal([]byte(doc.Content), &c); err != nil {
				return nil, err
			}
			if len(c.Followups) == 0 {
				continue
			}
			if c.Workgroup != g.ID || c.Host != g.Members[0] || doc.ID != "workgroup-change-"+c.ID || c.Decision == nil || c.Decision.Decision != "approve" || c.Decision.Revision < 1 || (c.State != "approved" && c.State != "applying" && c.State != "releasing" && c.State != "applied") {
				return nil, ErrConflict
			}
			pinnedDoc, err := Get[Document](r, g.Members[0], doc.ID, c.Decision.Revision)
			if err != nil {
				return nil, err
			}
			var pinned journal
			if err := json.Unmarshal([]byte(pinnedDoc.Content), &pinned); err != nil {
				return nil, err
			}
			if pinned.State != "presented" || pinned.Workgroup != c.Workgroup || pinned.Host != c.Host || pinned.ID != c.ID || !equalJSON(c.Before, pinned.Before) || !equalJSON(c.After, pinned.After) || !equalJSON(c.Members, pinned.Members) || !equalJSON(c.Request, pinned.Request) {
				return nil, ErrConflict
			}
			var request struct{ Kind string }
			if err := json.Unmarshal(c.Request, &request); err != nil {
				return nil, err
			}
			if request.Kind != "amend" {
				return nil, ErrConflict
			}
			for parent, stream := range c.Followups {
				project, known := projects[parent]
				if !known {
					continue
				}
				affected := slices.ContainsFunc(pinned.Impact.Streams, func(row struct {
					Workstream config.WorkstreamID
					Action     string
				}) bool {
					return row.Workstream == parent && row.Action != "unchanged"
				})
				followup := slices.ContainsFunc(c.Impact.Streams, func(row struct {
					Workstream config.WorkstreamID
					Action     string
				}) bool {
					return row.Workstream == parent && row.Action == "follow-up"
				})
				data, _ := json.Marshal([]string{"handin", string(project), "followup-" + c.ID + "-" + string(parent)})
				sum := sha256.Sum256(data)
				expected := config.WorkstreamID("w_" + hex.EncodeToString(sum[:16]))
				if !affected || !followup || stream != expected {
					return nil, ErrConflict
				}
				if !slices.Contains(g.Members[1:], stream) {
					continue
				}
				if old, exists := projects[stream]; exists {
					if old != project {
						return nil, ErrConflict
					}
				} else {
					projects[stream] = project
					changed = true
				}
			}
		}
	}
	if len(projects) != len(g.Members)-1 {
		return nil, fmt.Errorf("%w: workgroup approval membership mismatch", ErrConflict)
	}
	return projects, nil
}
