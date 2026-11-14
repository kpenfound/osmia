package trace

import (
	"encoding/json"
	"slices"
	"strings"
	"time"

	"github.com/kpenfound/osmia/internal/config"
)

// WorkgroupChangeDocumentPrefix starts the record ID of every workgroup
// change journal.
const WorkgroupChangeDocumentPrefix = "workgroup-change-"

// PublishedWorkgroup is a workgroup of more than one member as seen from one
// of its members: its ID, its real members, and its project spec once the
// owner has approved it. Draft project specs are never carried.
type PublishedWorkgroup struct {
	ID      config.WorkgroupID
	Members []config.WorkstreamID
	// Spec is the owner-approved project spec, nil until one is approved.
	// ApprovedAt is when the owner first approved it, and UpdatedAt when its
	// latest approved form was recorded.
	Spec       *ProjectSpec
	ApprovedAt time.Time
	UpdatedAt  time.Time
	// Changes are every recorded revision of the workgroup's change journals,
	// the owner's requests to amend or cancel its project spec and what the
	// owner ruled on them, oldest first.
	Changes []Document
}

// PublishedWorkgroupOf reports the workgroup of more than one member that
// stream belongs to, and false for a workstream that is alone. For a
// cross-repo workgroup the members are the real workstreams of every project,
// not the workgroup trace's own host. r must be the project's own repository.
func (r *Repository) PublishedWorkgroupOf(stream config.WorkstreamID) (PublishedWorkgroup, bool, error) {
	id, err := r.CrossRepoWorkgroupOf(stream)
	if err != nil {
		return PublishedWorkgroup{}, false, err
	}
	if id != "" {
		wt, _, g, found, err := r.OpenCrossRepoProjectSpec(id)
		if wt != nil {
			defer wt.Close()
		}
		if err != nil || !found || len(g.Members) < 2 {
			return PublishedWorkgroup{}, false, err
		}
		out := PublishedWorkgroup{ID: g.ID, Members: slices.Clone(g.Members[1:])}
		if err := out.approve(wt, g); err != nil {
			return PublishedWorkgroup{}, false, err
		}
		return out, true, nil
	}
	groups, _, err := Workgroups(r)
	if err != nil {
		return PublishedWorkgroup{}, false, err
	}
	g, ok := WorkgroupOf(groups, stream)
	if !ok || len(g.Members) < 2 {
		return PublishedWorkgroup{}, false, nil
	}
	out := PublishedWorkgroup{ID: g.ID, Members: slices.Clone(g.Members)}
	if err := out.approve(r, g); err != nil {
		return PublishedWorkgroup{}, false, err
	}
	return out, true, nil
}

// approve fills the owner-approved project spec of g from the repository
// holding its host: the latest approved revision, or else the group approval
// of a cross-repo workgroup. ApprovedAt is the first approval record's time,
// so it does not move when a later revision is recorded.
func (p *PublishedWorkgroup) approve(host *Repository, g Workgroup) error {
	if len(g.Members) == 0 {
		return nil
	}
	docs, err := Read[Document](host, g.Members[0])
	if err != nil {
		return err
	}
	var revision, approval *Document
	for i, d := range docs {
		if strings.HasPrefix(d.ID, WorkgroupChangeDocumentPrefix) {
			p.Changes = append(p.Changes, d)
			continue
		}
		switch d.ID {
		case WorkgroupRevisionDocument:
			revision = &docs[i]
		case GroupApprovalDocument:
			approval = &docs[i]
		default:
			continue
		}
		if p.ApprovedAt.IsZero() || d.At.Before(p.ApprovedAt) {
			p.ApprovedAt = d.At
		}
	}
	switch {
	case revision != nil:
		published, ok, err := approvedWorkgroupRevision([]Document{*revision})
		if err != nil {
			return err
		}
		if ok {
			p.Spec, p.UpdatedAt = &published.Spec, revision.At
		}
	case approval != nil:
		var decision GroupApproval
		if err := json.Unmarshal([]byte(approval.Content), &decision); err != nil {
			return err
		}
		p.Spec, p.UpdatedAt = &decision.ProjectSpec, approval.At
	}
	return nil
}
