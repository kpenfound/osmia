package trace

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"time"

	"github.com/kpenfound/osmia/internal/config"
)

// CrossRepoWorkgroupSubject is the workflow subject, recorded on a real
// member's own trace, naming the cross-repo workgroup it belongs to. It is a
// locator only: it never duplicates the workgroup's membership list or its
// project spec, which stay authoritative only in the workgroup's own trace
// (charter#2). It lets a caller holding just one member's own trace, and
// that member's workstream ID, find and read that workgroup's trace,
// without opening every member's repository first.
const CrossRepoWorkgroupSubject = "cross-repo-workgroup"

// PendingCrossRepoWorkgroup authenticates a member whose owner-recorded
// draft join retracted its solitary group before publishing its locator.
// It grants a joining hold only, never membership or a replacement group.
func (r *Repository) PendingCrossRepoWorkgroup(stream config.WorkstreamID) (config.WorkgroupID, error) {
	docs, err := Read[Document](r, "")
	if err != nil {
		return "", err
	}
	docs = slices.DeleteFunc(docs, func(d Document) bool { return d.ID != WorkgroupsDocument })
	slices.SortFunc(docs, func(a, b Document) int { return a.Revision - b.Revision })
	var previous Workgroup
	var retracted bool
	for _, doc := range docs {
		var file workgroupsFile
		if err := json.Unmarshal([]byte(doc.Content), &file); err != nil {
			return "", err
		}
		next, present := WorkgroupOf(file.Workgroups, stream)
		if present {
			retracted = false
		} else if len(previous.Members) > 0 {
			retracted = len(previous.Members) == 1 && doc.Actor.Kind == "owner" && doc.Cause == "draft-workgroup"
		}
		previous = next
	}
	if !retracted || len(previous.Members) > 0 {
		return "", nil
	}
	entries, err := os.ReadDir(filepath.Join(r.root.String(), "workgroups"))
	if err != nil {
		return "", err
	}
	var found config.WorkgroupID
	for _, entry := range entries {
		id, err := config.ParseWorkgroupID(entry.Name())
		if err != nil {
			continue
		}
		authority, err := OpenWorkgroupAuthority(r.root, id)
		if err != nil {
			if authority != nil {
				authority.Close()
			}
			return "", err
		}
		groups, _, err := Workgroups(authority)
		err = errors.Join(err, authority.Close())
		if err != nil {
			return "", err
		}
		group, member := WorkgroupOf(groups, stream)
		if !member {
			continue
		}
		if group.ID != id || len(group.Members) < 3 || !slices.Contains(group.Members[1:], stream) || found != "" {
			return "", fmt.Errorf("%w: ambiguous join authority for workstream %s", ErrConflict, stream)
		}
		found = id
	}
	if found == "" {
		return "", fmt.Errorf("workstream %s has an owner-retracted workgroup but no matching shared authority", stream)
	}
	return found, nil
}

// JoinCrossRepoWorkgroup publishes stream's shared locator and retracts its
// unused local chief in one member-trace commit. The project-level solitary
// workgroup can then be retracted: readers already route through the locator.
// Retrying the same join changes nothing; an existing conversation is refused.
func (r *Repository) JoinCrossRepoWorkgroup(ctx context.Context, stream config.WorkstreamID, id config.WorkgroupID, at time.Time, actor Actor, cause string) error {
	if err := config.CheckWorkgroupIDs(id); err != nil {
		return err
	}
	sr, err := r.scoped(stream)
	if err != nil {
		return err
	}
	sr.mu.Lock()
	defer sr.mu.Unlock()
	log, v, err := sr.loadWorkflow(stream)
	if err != nil {
		return err
	}
	state := v.states[CrossRepoWorkgroupSubject]
	if state.Value != "" && state.Value != string(id) {
		return refused("workstream %s already points to cross-repo workgroup %s", stream, state.Value)
	}
	chief, hasChief := log.Threads[ChiefOfStaff]
	if hasChief && len(chief.Turns) > 0 {
		return refused("workstream %s already has chief-of-staff turns of its own", stream)
	}
	if state.Value == string(id) && !hasChief {
		return nil
	}
	delete(log.Threads, ChiefOfStaff)
	var txs []Transaction
	if state.Value == "" {
		txs = append(txs, Transaction{ExpectedVersion: state.Version,
			Transition: Transition{Header: Header{Schema: "osmia.trace.transition", Version: Version, ID: "cross-repo-workgroup-" + string(id), Revision: 1, Project: r.Project(), Workstream: stream, At: at, Actor: actor, Cause: cause},
				Subject: CrossRepoWorkgroupSubject, From: state.Value, To: string(id), Reason: "workstream " + string(stream) + " joined cross-repo workgroup " + string(id)}})
	}
	_, err = sr.commitWorkflow(ctx, stream, log, v, txs...)
	return err
}

// RecordCrossRepoWorkgroup records, on stream's own trace, that it belongs
// to the cross-repo workgroup id. Calling it again with the same id changes
// nothing further; naming a different id is refused, since a workstream
// never belongs to two workgroups.
func (r *Repository) RecordCrossRepoWorkgroup(ctx context.Context, stream config.WorkstreamID, id config.WorkgroupID, at time.Time, actor Actor, cause string) error {
	existing, err := r.CrossRepoWorkgroupOf(stream)
	if err != nil {
		return err
	}
	if existing == id {
		return nil
	}
	if existing != "" {
		return refused("workstream %s already points to cross-repo workgroup %s", stream, existing)
	}
	state, err := r.Workflow(stream, CrossRepoWorkgroupSubject)
	if err != nil {
		return err
	}
	tx := Transaction{ExpectedVersion: state.Version,
		Transition: Transition{Header: Header{Schema: "osmia.trace.transition", Version: Version, ID: "cross-repo-workgroup-" + string(id), Revision: 1, Project: r.Project(), Workstream: stream, At: at, Actor: actor, Cause: cause},
			Subject: CrossRepoWorkgroupSubject, From: state.Value, To: string(id), Reason: "workstream " + string(stream) + " joined cross-repo workgroup " + string(id)}}
	_, err = r.Transact(ctx, tx)
	return err
}

// CrossRepoWorkgroupOf returns the cross-repo workgroup stream points to, or
// "" when it points to none.
func (r *Repository) CrossRepoWorkgroupOf(stream config.WorkstreamID) (config.WorkgroupID, error) {
	state, err := r.Workflow(stream, CrossRepoWorkgroupSubject)
	if err != nil {
		return "", err
	}
	return config.WorkgroupID(state.Value), nil
}

// CrossRepoProjectSpec reads the recorded Workgroup and project spec of a
// cross-repo workgroup directly from its own trace, reusing this
// process's own already open instance of it when one exists
// (openSharedWorkgroupTrace), or opening and closing one transiently for
// the read alone otherwise; either way it answers promptly even while a
// long call such as a dispatched chief-of-staff turn holds that same
// instance open elsewhere. It needs no real member's own repository open:
// a project spec and a workgroup's membership list are both project-level
// documents of the workgroup's own trace, never scoped to one workstream.
// r is used only to resolve where the service root is; the read never
// touches r's own trace. found is false when no trace exists yet for id.
func (r *Repository) CrossRepoProjectSpec(id config.WorkgroupID) (ProjectSpec, Workgroup, bool, error) {
	directory, err := r.root.WorkgroupTrace(id)
	if err != nil {
		return ProjectSpec{}, Workgroup{}, false, err
	}
	if _, err := os.Lstat(directory); os.IsNotExist(err) {
		return ProjectSpec{}, Workgroup{}, false, nil
	} else if err != nil {
		return ProjectSpec{}, Workgroup{}, false, err
	}
	wt, err := openSharedWorkgroupTrace(r.root, id, nil)
	if wt != nil && err != nil {
		wt.Close()
	}
	if err != nil {
		return ProjectSpec{}, Workgroup{}, false, err
	}
	defer wt.Close()
	groups, _, err := Workgroups(wt)
	if err != nil {
		return ProjectSpec{}, Workgroup{}, false, err
	}
	var g Workgroup
	found := false
	for _, candidate := range groups {
		if candidate.ID == id {
			g, found = candidate, true
			break
		}
	}
	if !found {
		return ProjectSpec{}, Workgroup{}, false, nil
	}
	// An accepted decision owns the project spec used by member context and
	// graph interpretation, even if newer draft documents appear during
	// recovery. Draft history stays readable in ProjectSpecFor.
	if len(g.Members) > 0 {
		revision, published, err := ApprovedWorkgroupRevision(wt, g.Members[0])
		if err != nil {
			return ProjectSpec{}, Workgroup{}, false, err
		}
		if published {
			return revision.Spec, g, true, nil
		}
		approval, _, decided, err := ReadGroupApproval(wt, g.Members[0])
		if err != nil {
			return ProjectSpec{}, Workgroup{}, false, err
		}
		if decided {
			return approval.ProjectSpec, g, true, nil
		}
	}
	spec, found, err := ProjectSpecFor(wt, id)
	if err != nil {
		return ProjectSpec{}, Workgroup{}, false, err
	}
	return spec, g, found, nil
}
