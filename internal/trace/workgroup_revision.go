package trace

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"slices"
	"time"

	"github.com/kpenfound/osmia/internal/config"
)

const WorkgroupRevisionDocument = "workgroup-revision"
const WorkgroupRevisionPath = "tools/workgroup-revision.json"

type WorkgroupRevision struct {
	Change string      `json:"change"`
	Spec   ProjectSpec `json:"spec"`
}

// ApprovedWorkgroupRevision is the published outcome of an owner-approved
// delta. The initial group approval remains immutable historical evidence.
func ApprovedWorkgroupRevision(r *Repository, host config.WorkstreamID) (WorkgroupRevision, bool, error) {
	d, err := r.latestDocumentByID(host, WorkgroupRevisionDocument, WorkgroupRevisionPath)
	if errors.Is(err, fs.ErrNotExist) {
		return WorkgroupRevision{}, false, nil
	}
	if err != nil {
		return WorkgroupRevision{}, false, err
	}
	return approvedWorkgroupRevision([]Document{d})
}

func approvedWorkgroupRevision(docs []Document) (WorkgroupRevision, bool, error) {
	for _, d := range slices.Backward(docs) {
		if d.ID == WorkgroupRevisionDocument {
			var revision WorkgroupRevision
			err := json.Unmarshal([]byte(d.Content), &revision)
			return revision, true, err
		}
	}
	return WorkgroupRevision{}, false, nil
}

func RecordWorkgroupRevision(ctx context.Context, r *Repository, g Workgroup, change string, spec ProjectSpec, at time.Time, actor Actor) error {
	if err := spec.valid(g.Members); err != nil {
		return err
	}
	old, found, err := ApprovedWorkgroupRevision(r, g.Members[0])
	if err != nil {
		return err
	}
	if found && old.Change == change {
		if !equalJSON(old.Spec, spec) {
			return ErrConflict
		}
		return nil
	}
	doc, err := r.latestDocumentByID(g.Members[0], WorkgroupRevisionDocument, WorkgroupRevisionPath)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	revision := doc.Revision + 1
	data, err := json.Marshal(WorkgroupRevision{Change: change, Spec: spec})
	if err != nil {
		return err
	}
	return r.RecordDocuments(ctx, []Document{{Header: Header{Schema: "osmia.trace.document", Version: Version, ID: WorkgroupRevisionDocument, Revision: revision, Project: r.Project(), Workstream: g.Members[0], At: at, Actor: actor, Cause: change}, Path: WorkgroupRevisionPath, Content: string(data)}})
}

// AddWorkgroupMember extends the same workgroup for follow-up work after a
// merged member. The original member and its merge remain historical facts.
func AddWorkgroupMember(ctx context.Context, r *Repository, id config.WorkgroupID, member config.WorkstreamID, at time.Time, actor Actor, cause string) error {
	defer r.cacheWorkgroupResolution(id, false)
	groups, revision, err := Workgroups(r)
	if err != nil {
		return err
	}
	for n := range groups {
		if groups[n].ID == id {
			if slices.Contains(groups[n].Members, member) {
				return nil
			}
			groups[n].Members = append(groups[n].Members, member)
		}
	}
	data, err := json.Marshal(workgroupsFile{Version: 1, Workgroups: groups})
	if err != nil {
		return err
	}
	return r.Append(ctx, Document{Header: Header{Schema: "osmia.trace.document", Version: Version, ID: WorkgroupsDocument, Revision: revision + 1, Project: r.Project(), At: at, Actor: actor, Cause: cause}, Path: WorkgroupsPath, Content: string(data)})
}

func ReopenWorkgroup(ctx context.Context, r *Repository, id config.WorkgroupID, at time.Time, actor Actor, cause string) error {
	defer r.cacheWorkgroupResolution(id, false)
	resolved, revision, err := ResolvedWorkgroups(r)
	if err != nil {
		return err
	}
	if !slices.Contains(resolved, id) {
		return nil
	}
	resolved = slices.DeleteFunc(resolved, func(g config.WorkgroupID) bool { return g == id })
	data, err := json.Marshal(workgroupResolutionsFile{Version: 1, Resolved: resolved})
	if err != nil {
		return err
	}
	return r.Append(ctx, Document{Header: Header{Schema: "osmia.trace.document", Version: Version, ID: WorkgroupResolutionsDocument, Revision: revision + 1, Project: r.Project(), At: at, Actor: actor, Cause: cause}, Path: WorkgroupResolutionsPath, Content: string(data)})
}
