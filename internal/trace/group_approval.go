package trace

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
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
	ProjectSpecRevision int                   `json:"project_spec_revision,omitempty"`
	Workgroup           config.WorkgroupID    `json:"workgroup"`
	ProjectSpec         ProjectSpec           `json:"project_spec"`
	Members             []GroupMemberApproval `json:"members"`
	Complete            bool                  `json:"complete"`
}

func ReadGroupApproval(r *Repository, host config.WorkstreamID) (GroupApproval, Document, bool, error) {
	d, err := r.latestDocumentByID(host, GroupApprovalDocument, GroupApprovalPath)
	if errors.Is(err, fs.ErrNotExist) {
		return GroupApproval{}, Document{}, false, nil
	}
	if err != nil {
		return GroupApproval{}, Document{}, false, err
	}
	r.approvalDecodes.Add(1)
	if r.shared != nil {
		r.shared.approvalDecodes.Add(1)
	}
	var decision GroupApproval
	err = json.Unmarshal([]byte(d.Content), &decision)
	return decision, d, true, err
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
	snap, err := r.groupApprovalSnapshot(id)
	if err != nil {
		return false, err
	}
	return !snap.required || snap.found && snap.decision.Complete, nil
}

func (r *Repository) groupApproval(id config.WorkgroupID) (GroupApproval, bool, error) {
	decision, found, _, err := r.groupApprovalState(id)
	return decision, found, err
}

// groupApprovalState returns the workgroup's approval, whether one is
// recorded, and whether group approval is required. The approval belongs to
// the caller.
func (r *Repository) groupApprovalState(id config.WorkgroupID) (GroupApproval, bool, bool, error) {
	snap, err := r.groupApprovalSnapshot(id)
	if err != nil {
		return GroupApproval{}, false, true, err
	}
	if !snap.found {
		return GroupApproval{}, false, snap.required, nil
	}
	return cloneGroupApproval(*snap.decision), true, snap.required, nil
}

// cloneGroupApproval returns a copy of a that shares no mutable data with it.
func cloneGroupApproval(a GroupApproval) GroupApproval {
	a.ProjectSpec = cloneProjectSpec(a.ProjectSpec)
	if a.Members != nil {
		members := make([]GroupMemberApproval, len(a.Members))
		for i, m := range a.Members {
			m.Record = slices.Clone(m.Record)
			members[i] = m
		}
		a.Members = members
	}
	return a
}

// cloneProjectSpec returns a copy of spec that shares no mutable data with it.
func cloneProjectSpec(spec ProjectSpec) ProjectSpec {
	spec.Contracts = slices.Clone(spec.Contracts)
	spec.Graph = slices.Clone(spec.Graph)
	if spec.Slices != nil {
		slicesCopy := make([]Slice, len(spec.Slices))
		for i, s := range spec.Slices {
			s.ContractCriteria = slices.Clone(s.ContractCriteria)
			slicesCopy[i] = s
		}
		spec.Slices = slicesCopy
	}
	return spec
}
