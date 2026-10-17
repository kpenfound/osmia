package trace

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"time"

	"github.com/kpenfound/osmia/internal/config"
)

const SealPreparedState = "seal-prepared"

const GroupApprovalDocument = "workgroup-approval"
const GroupApprovalPath = "tools/workgroup-approval.json"

// GroupMemberApproval pins the owner decision and its member scope. Record is
// the encoded ratification, including the committee dispositions at approval.
type GroupMemberApproval struct {
	Project    config.ProjectID    `json:"project"`
	Workstream config.WorkstreamID `json:"workstream"`
	Spec       int                 `json:"spec"`
	Plan       int                 `json:"plan"`
	Round      int                 `json:"round"`
	Record     json.RawMessage     `json:"record"`
}

// GroupApproval is the sole effective approval of a cross-repo draft. Member
// decisions and seals are preparations until this record is complete.
type GroupApproval struct {
	Workgroup   config.WorkgroupID    `json:"workgroup"`
	ProjectSpec ProjectSpec           `json:"project_spec"`
	Members     []GroupMemberApproval `json:"members"`
	Complete    bool                  `json:"complete"`
}

func ReadGroupApproval(r *Repository, host config.WorkstreamID) (GroupApproval, Document, bool, error) {
	docs, err := Read[Document](r, host)
	if err != nil {
		return GroupApproval{}, Document{}, false, err
	}
	for _, d := range slices.Backward(docs) {
		if d.ID == GroupApprovalDocument {
			var decision GroupApproval
			err := json.Unmarshal([]byte(d.Content), &decision)
			return decision, d, true, err
		}
	}
	return GroupApproval{}, Document{}, false, nil
}

// RecordGroupApproval permits a single immutable decision followed by its
// completion, recorded at its tools path.
func RecordGroupApproval(ctx context.Context, r *Repository, host config.WorkstreamID, decision GroupApproval, at time.Time, actor Actor) error {
	old, doc, found, err := ReadGroupApproval(r, host)
	if err != nil {
		return err
	}
	if found {
		immutable := decision
		immutable.Complete = old.Complete
		if !equalJSON(old, immutable) || old.Complete && !decision.Complete {
			return ErrConflict
		}
		if old.Complete == decision.Complete {
			return nil
		}
	}
	if len(decision.Members) == 0 {
		return fmt.Errorf("group approval requires members")
	}
	data, err := json.Marshal(decision)
	if err != nil {
		return err
	}
	return r.RecordDocuments(ctx, []Document{{Header: Header{Schema: "osmia.trace.document", Version: Version, ID: GroupApprovalDocument, Revision: doc.Revision + 1, Project: r.Project(), Workstream: host, At: at, Actor: actor, Cause: GroupApprovalDocument}, Path: GroupApprovalPath, Content: string(data)}})
}

// GroupApproved consults the shared authority without holding a member mutex.
// Ordinary workstreams retain their existing individual ratification behavior.
func (r *Repository) GroupApproved(stream config.WorkstreamID) (bool, error) {
	id, err := r.CrossRepoWorkgroupOf(stream)
	if err != nil || id == "" {
		return id == "", err
	}
	return r.groupApproved(id)
}

func (r *Repository) GroupApprovalOf(stream config.WorkstreamID) (GroupApproval, bool, error) {
	id, err := r.CrossRepoWorkgroupOf(stream)
	if err != nil || id == "" {
		return GroupApproval{}, false, err
	}
	decision, found, _, err := r.groupApprovalState(id)
	return decision, found, err
}

// GroupApprovalRequired distinguishes a shared conversation with one selected
// workstream from a group that must seal several workstreams together.
func (r *Repository) GroupApprovalRequired(stream config.WorkstreamID) (bool, error) {
	id, err := r.CrossRepoWorkgroupOf(stream)
	if err != nil || id == "" {
		return false, err
	}
	_, _, required, err := r.groupApprovalState(id)
	return required, err
}

func (r *Repository) groupApproved(id config.WorkgroupID) (bool, error) {
	decision, found, required, err := r.groupApprovalState(id)
	return !required || found && decision.Complete, err
}

func (r *Repository) groupApproval(id config.WorkgroupID) (GroupApproval, bool, error) {
	decision, found, _, err := r.groupApprovalState(id)
	return decision, found, err
}

func (r *Repository) groupApprovalState(id config.WorkgroupID) (GroupApproval, bool, bool, error) {
	project := r
	if r.parent != nil {
		project = r.parent
	}
	var wt *Repository
	var err error
	if project.Project() == crossRepoProject(id) {
		wt = project
	} else {
		wt, err = openSharedWorkgroupTrace(r.root, id, nil)
		if wt != nil {
			defer wt.Close()
		}
		if err != nil {
			return GroupApproval{}, false, true, err
		}
	}
	groups, _, err := Workgroups(wt)
	if err != nil {
		return GroupApproval{}, false, true, err
	}
	for _, g := range groups {
		if g.ID == id && len(g.Members) > 0 {
			decision, _, found, err := ReadGroupApproval(wt, g.Members[0])
			return decision, found, !(g.Conversation != nil && len(g.Members) == 2), err
		}
	}
	return GroupApproval{}, false, true, fmt.Errorf("workgroup %s has no authority", id)
}
