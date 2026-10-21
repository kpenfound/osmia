package trace

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/kpenfound/osmia/internal/config"
)

// WorkgroupsPath is the project document holding the project's workgroups,
// and WorkgroupsDocument its record ID.
const (
	WorkgroupsPath     = "workgroups.json"
	WorkgroupsDocument = "workgroups"
)

// Workgroup is one feature request in flight in a project: one or more
// workstreams sharing a chief of staff. A workgroup wrapping exactly one
// workstream has no chief-of-staff thread of its own; that workstream's own
// existing thread serves as the workgroup's.
type Workgroup struct {
	ID           config.WorkgroupID    `json:"id"`
	Members      []config.WorkstreamID `json:"members"`
	Conversation *ConversationOrigin   `json:"conversation,omitempty"`
}

// workgroupsFile is the recorded shape of workgroups.json.
type workgroupsFile struct {
	Version    int         `json:"version"`
	Workgroups []Workgroup `json:"workgroups"`
}

// Workgroups returns every recorded workgroup of the project, in recorded
// order, with the document revision read, 0 when none is recorded.
func Workgroups(r *Repository) ([]Workgroup, int, error) {
	content, revision, err := latestWorkgroups(r)
	if err != nil {
		return nil, 0, err
	}
	if content == "" {
		return nil, 0, nil
	}
	var file workgroupsFile
	if err := json.Unmarshal([]byte(content), &file); err != nil {
		return nil, revision, err
	}
	return file.Workgroups, revision, nil
}

// WorkgroupOf returns the workgroup stream belongs to among groups, if one
// is recorded for it.
func WorkgroupOf(groups []Workgroup, stream config.WorkstreamID) (Workgroup, bool) {
	for _, g := range groups {
		for _, m := range g.Members {
			if m == stream {
				return g, true
			}
		}
	}
	return Workgroup{}, false
}

// EnsureWorkgroups records a new one-workstream workgroup, in one commit, for
// every member of missing not already in a recorded workgroup. A member
// already in one is left alone, so calling it again after a crash, or on
// every start, records nothing further and duplicates nothing: a workstream
// never gains a second workgroup.
func EnsureWorkgroups(ctx context.Context, r *Repository, missing []config.WorkstreamID, at time.Time, actor Actor, cause string) ([]Workgroup, error) {
	existing, revision, err := Workgroups(r)
	if err != nil {
		return nil, err
	}
	next := existing
	changed := false
	for _, stream := range missing {
		if pending, err := SessionDestroyPending(r.root, r.project, stream); err != nil {
			return nil, err
		} else if pending {
			continue
		}
		if _, ok := WorkgroupOf(existing, stream); ok {
			continue
		}
		id, err := config.NewWorkgroupID()
		if err != nil {
			return nil, err
		}
		next = append(next, Workgroup{ID: id, Members: []config.WorkstreamID{stream}})
		changed = true
	}
	if !changed {
		return existing, nil
	}
	data, err := json.Marshal(workgroupsFile{Version: 1, Workgroups: next})
	if err != nil {
		return nil, err
	}
	if err := r.Append(ctx, Document{
		Header:  Header{Schema: "osmia.trace.document", Version: Version, ID: WorkgroupsDocument, Revision: revision + 1, Project: r.Project(), At: at, Actor: actor, Cause: cause},
		Path:    WorkgroupsPath,
		Content: string(data),
	}); err != nil {
		return nil, err
	}
	return next, nil
}

// CreateWorkgroup records one new workgroup holding exactly members, who
// share its chief of staff from then on. It refuses, writing nothing, if
// members is empty, lists a workstream twice or names one that already
// belongs to a recorded workgroup. Until the architect's own graph exists,
// this is how a multi-workstream workgroup is made: directly by a test
// fixture or a service helper, as a fast-path or migrated one-workstream
// workgroup is by EnsureWorkgroups.
func CreateWorkgroup(ctx context.Context, r *Repository, members []config.WorkstreamID, at time.Time, actor Actor, cause string) (Workgroup, error) {
	if len(members) == 0 {
		return Workgroup{}, fmt.Errorf("a workgroup needs at least one member")
	}
	existing, revision, err := Workgroups(r)
	if err != nil {
		return Workgroup{}, err
	}
	seen := map[config.WorkstreamID]bool{}
	for _, m := range members {
		if seen[m] {
			return Workgroup{}, fmt.Errorf("workstream %s listed twice", m)
		}
		seen[m] = true
		if g, ok := WorkgroupOf(existing, m); ok {
			return Workgroup{}, fmt.Errorf("workstream %s already belongs to workgroup %s", m, g.ID)
		}
	}
	id, err := config.NewWorkgroupID()
	if err != nil {
		return Workgroup{}, err
	}
	g := Workgroup{ID: id, Members: slices.Clone(members)}
	next := append(slices.Clone(existing), g)
	data, err := json.Marshal(workgroupsFile{Version: 1, Workgroups: next})
	if err != nil {
		return Workgroup{}, err
	}
	if err := r.Append(ctx, Document{
		Header:  Header{Schema: "osmia.trace.document", Version: Version, ID: WorkgroupsDocument, Revision: revision + 1, Project: r.Project(), At: at, Actor: actor, Cause: cause},
		Path:    WorkgroupsPath,
		Content: string(data),
	}); err != nil {
		return Workgroup{}, err
	}
	return g, nil
}

// RetractWorkgroup removes stream's recorded one-workstream workgroup, if
// hand-in's own EnsureWorkgroups call already recorded one before stream
// joined a cross-repo workgroup. A new workstream gets that one-workstream
// workgroup at once (handin.go's own EnsureWorkgroups call), before
// anything can know it will later become a cross-repo workgroup's member;
// once RecordCrossRepoWorkgroup durably points stream at its real, shared
// workgroup in that workgroup's own trace, the earlier, solitary record on
// stream's own repository would otherwise make workgroup membership
// authoritative in two places (charter#2). It changes nothing when stream
// carries no recorded workgroup, and it refuses, changing nothing, when the
// recorded workgroup has more than stream as a member: retracting a real,
// same-repo workgroup is not this function's job. r must be the project's
// own repository, not an instance already scoped to one workstream.
func (r *Repository) RetractWorkgroup(ctx context.Context, stream config.WorkstreamID, at time.Time, actor Actor, cause string) error {
	existing, revision, err := Workgroups(r)
	if err != nil {
		return err
	}
	g, ok := WorkgroupOf(existing, stream)
	if !ok {
		return nil
	}
	if len(g.Members) != 1 {
		return fmt.Errorf("%w: workgroup %s of workstream %s has more than one member", ErrConflict, g.ID, stream)
	}
	next := make([]Workgroup, 0, len(existing))
	for _, candidate := range existing {
		if candidate.ID != g.ID {
			next = append(next, candidate)
		}
	}
	data, err := json.Marshal(workgroupsFile{Version: 1, Workgroups: next})
	if err != nil {
		return err
	}
	return r.Append(ctx, Document{
		Header:  Header{Schema: "osmia.trace.document", Version: Version, ID: WorkgroupsDocument, Revision: revision + 1, Project: r.Project(), At: at, Actor: actor, Cause: cause},
		Path:    WorkgroupsPath,
		Content: string(data),
	})
}

// ChiefOfStaffHost returns the workstream whose chief-of-staff thread serves
// the whole workgroup stream belongs to: the workgroup's first recorded
// member. A stream with no recorded workgroup is its own one-workstream
// workgroup and is its own host. r must be the project's own repository, not
// an instance already scoped to one workstream: workgroups.json is a
// project document.
func (r *Repository) ChiefOfStaffHost(stream config.WorkstreamID) (config.WorkstreamID, error) {
	groups, _, err := Workgroups(r)
	if err != nil {
		return "", err
	}
	return chiefOfStaffHost(groups, stream), nil
}

func chiefOfStaffHost(groups []Workgroup, stream config.WorkstreamID) config.WorkstreamID {
	if g, ok := WorkgroupOf(groups, stream); ok && len(g.Members) > 0 {
		return g.Members[0]
	}
	return stream
}

// WorkgroupMembers returns every workstream that shares stream's chief of
// staff: stream alone when no workgroup is recorded for it. r must be the
// project's own repository.
func (r *Repository) WorkgroupMembers(stream config.WorkstreamID) ([]config.WorkstreamID, error) {
	groups, _, err := Workgroups(r)
	if err != nil {
		return nil, err
	}
	if g, ok := WorkgroupOf(groups, stream); ok && len(g.Members) > 0 {
		return slices.Clone(g.Members), nil
	}
	return []config.WorkstreamID{stream}, nil
}

// ChiefQuestionTarget resolves which member workstream, and that member's
// own local question ID, a chief-of-staff tool's id argument names: id names
// a question of the turn's own workstream when it carries no qualifier, and
// "<workstream>:<id>" names one of any other member of the shared workgroup.
// r must be the project's own repository.
func (r *Repository) ChiefQuestionTarget(host config.WorkstreamID, id string) (config.WorkstreamID, string, error) {
	stream, local, ok := strings.Cut(id, ":")
	if !ok {
		return host, id, nil
	}
	members, err := r.WorkgroupMembers(host)
	if err != nil {
		return "", "", err
	}
	if !slices.Contains(members, config.WorkstreamID(stream)) {
		return "", "", refused("workstream %s does not share this chief of staff", stream)
	}
	return config.WorkstreamID(stream), local, nil
}

// ChiefMember resolves the workstream a chief-of-staff tool's workstream
// input names, and the project-level repository holding that workstream's
// trace: own on r itself when named is empty or own, or another member
// sharing own's chief of staff, on that member's own repo. A non-empty
// refusal names a workstream that does not share it. r must be the
// project's own repository.
func (r *Repository) ChiefMember(own, named config.WorkstreamID) (*Repository, config.WorkstreamID, string, error) {
	if named == "" || named == own {
		return r, own, "", nil
	}
	members, err := r.WorkgroupMembers(own)
	if err != nil {
		return nil, "", "", err
	}
	if !slices.Contains(members, named) {
		return nil, "", fmt.Sprintf("workstream %s does not share this chief of staff", named), nil
	}
	project, err := r.ProjectFor(named)
	if err != nil {
		return nil, "", "", err
	}
	return project, named, "", nil
}

// WorkgroupResolutionsPath is the project document holding every workgroup
// already recorded resolved, and WorkgroupResolutionsDocument its record ID.
const (
	WorkgroupResolutionsPath     = "workgroup-resolutions.json"
	WorkgroupResolutionsDocument = "workgroup-resolutions"
)

type workgroupResolutionsFile struct {
	Version  int                  `json:"version"`
	Resolved []config.WorkgroupID `json:"resolved"`
}

// ResolvedWorkgroups returns the IDs of every workgroup already recorded
// resolved, with the document revision, 0 when none is recorded. r must be
// the project's own repository.
func ResolvedWorkgroups(r *Repository) ([]config.WorkgroupID, int, error) {
	content, revision, err := latestDocument(r, WorkgroupResolutionsPath)
	if err != nil {
		return nil, 0, err
	}
	if content == "" {
		return nil, 0, nil
	}
	var file workgroupResolutionsFile
	if err := json.Unmarshal([]byte(content), &file); err != nil {
		return nil, revision, err
	}
	return file.Resolved, revision, nil
}

// MarkWorkgroupsResolved records, in one commit, every workgroup among
// groups that is not already recorded resolved and whose every member
// satisfies resolved (typically: has reached a resolved feature state,
// delivered or abandoned). It returns the IDs newly recorded resolved by
// this call. Calling it again, including after a crash mid-way, changes
// nothing further and marks nothing twice: a workgroup is never recorded
// resolved more than once, and resolving one workgroup never affects
// another's record.
//
// This is the distinct trigger other units retire workgroup-level documents
// on, such as a project spec, once every workstream of a workgroup is
// resolved; recording it neither resolves, deletes nor retires anything
// itself, and nothing here fires when only some of a workgroup's members
// are resolved.
func MarkWorkgroupsResolved(ctx context.Context, r *Repository, groups []Workgroup, resolved func(config.WorkstreamID) (bool, error), at time.Time, actor Actor, cause string) ([]config.WorkgroupID, error) {
	existing, revision, err := ResolvedWorkgroups(r)
	if err != nil {
		return nil, err
	}
	already := map[config.WorkgroupID]bool{}
	for _, id := range existing {
		already[id] = true
	}
	var newly []config.WorkgroupID
	for _, g := range groups {
		if already[g.ID] {
			continue
		}
		every := len(g.Members) > 0
		for _, m := range g.Members {
			ok, err := resolved(m)
			if err != nil {
				return nil, fmt.Errorf("workstream %s: %w", m, err)
			}
			if !ok {
				every = false
				break
			}
		}
		if every {
			newly = append(newly, g.ID)
		}
	}
	if len(newly) == 0 {
		return nil, nil
	}
	next := append(slices.Clone(existing), newly...)
	data, err := json.Marshal(workgroupResolutionsFile{Version: 1, Resolved: next})
	if err != nil {
		return nil, err
	}
	if err := r.Append(ctx, Document{
		Header:  Header{Schema: "osmia.trace.document", Version: Version, ID: WorkgroupResolutionsDocument, Revision: revision + 1, Project: r.Project(), At: at, Actor: actor, Cause: cause},
		Path:    WorkgroupResolutionsPath,
		Content: string(data),
	}); err != nil {
		return nil, err
	}
	return newly, nil
}

func latestDocument(r *Repository, path string) (string, int, error) {
	documents, err := Read[Document](r, "")
	if err != nil {
		return "", 0, err
	}
	content, revision := "", 0
	for _, d := range documents {
		if d.Path == path && d.Revision > revision {
			content, revision = d.Content, d.Revision
		}
	}
	return content, revision, nil
}

func latestWorkgroups(r *Repository) (string, int, error) {
	return latestDocument(r, WorkgroupsPath)
}
