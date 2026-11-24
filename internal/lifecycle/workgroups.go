package lifecycle

import (
	"context"
	"errors"
	"fmt"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/intake"
	"github.com/kpenfound/osmia/internal/runtime"
	"github.com/kpenfound/osmia/internal/trace"
)

// MigrationActor is the actor of the workgroup records EnsureWorkgroups and
// workgroup resolution write.
var MigrationActor = trace.Actor{Kind: "service", ID: "osmia"}

// EnsureWorkgroupsInterrupt, when set, is called with each project's ID right
// before EnsureWorkgroups records that project's workgroups; its error stops
// the pass there.
var EnsureWorkgroupsInterrupt func(config.ProjectID) error

// EnsureWorkgroups wraps every non-session workstream of each project that is
// in no workgroup in its own one-workstream workgroup, whose chief of staff is
// the workstream's existing chief-of-staff thread. It runs on every start
// before the project schedules work, and is resumable and idempotent. A
// workstream that already belongs to a recorded workgroup, including a
// cross-repo workgroup recorded in another project's trace, is left alone.
func (s *Controller) EnsureWorkgroups(ctx context.Context, cfg *config.Config, projects []*Project, archives []runtime.Archive) error {
	repos := map[config.ProjectID]config.Project{}
	for _, p := range cfg.Projects {
		repos[p.ID] = p
	}
	for _, p := range projects {
		_, ok := repos[p.ID]
		if !ok {
			continue
		}
		streams, err := p.Repository.Workstreams()
		if err != nil {
			return err
		}
		present := make(map[config.WorkstreamID]bool, len(streams))
		var members []config.WorkstreamID
		for _, stream := range streams {
			present[stream] = true
			session, err := isSession(p.Repository, stream)
			if err != nil {
				return err
			}
			if session {
				continue
			}
			if id, err := p.Repository.CrossRepoWorkgroupOf(stream); err != nil {
				return err
			} else if id != "" {
				continue
			}
			if pending, err := p.Repository.PendingCrossRepoWorkgroup(stream); err != nil {
				return err
			} else if pending != "" {
				continue
			}
			members = append(members, stream)
		}
		for _, a := range archives {
			if a.Project != p.ID || present[a.Workstream] || a.Workgroup != "" {
				continue
			}
			members = append(members, a.Workstream)
		}
		if len(members) == 0 {
			continue
		}
		if EnsureWorkgroupsInterrupt != nil {
			if err := EnsureWorkgroupsInterrupt(p.ID); err != nil {
				return err
			}
		}
		if _, err := trace.EnsureWorkgroups(ctx, p.Repository, members, s.Now(), MigrationActor, "migrate-workgroups"); err != nil {
			return err
		}
	}
	return nil
}

// Resolved requires cancellation or recorded merge evidence for delivery.
func Resolved(repository *trace.Repository, stream config.WorkstreamID) (bool, error) {
	state, err := repository.Workflow(stream, trace.FeatureSubject)
	if err != nil {
		return false, err
	}
	evidence, err := ReadMemberEvidence(repository, stream)
	return evidence.Resolution(state.Value) != "unresolved", err
}

// WorkgroupResolver records, once every member of a workgroup is resolved,
// that the workgroup itself has resolved: the distinct trigger other units
// retire workgroup-level documents on, such as a project spec. Resolving
// one member never resolves the workgroup and never fires this trigger;
// only resolving every member does, and only once.
type WorkgroupResolver struct {
	S          *Controller
	Repository *trace.Repository
}

// Pass marks every workgroup whose every member is now resolved. It is
// resumable and idempotent: a workgroup already recorded resolved is left
// alone, and nothing here deletes, archives or otherwise retires anything,
// a member's own cleanup included. It also resolves every cross-repo
// workgroup one of repository's own workstreams belongs to, recording that
// Resolution in the workgroup's own trace rather than repository's: every
// member's own pass that finds the same workgroup tries the same idempotent
// Record, so none of them needs to be the sole owner of it.
func (w *WorkgroupResolver) Pass(ctx context.Context) error {
	if err := w.ReconcileArchivedEdgeMerges(ctx); err != nil {
		return err
	}
	var groups []trace.Workgroup
	present := map[config.WorkstreamID]bool{}
	err := w.Repository.WithWorkstreamDiscovery(func(streams []config.WorkstreamID) error {
		var err error
		groups, _, err = trace.Workgroups(w.Repository)
		if err != nil {
			return err
		}
		sessions := map[config.WorkstreamID]bool{}
		for _, stream := range streams {
			present[stream] = true
			session, err := intake.IsSession(w.Repository, stream)
			if err != nil {
				return err
			}
			design, err := intake.IsDesignWorkstream(w.Repository, stream)
			if err != nil {
				return err
			}
			sessions[stream] = session || design
		}
		var features []trace.Workgroup
		for _, group := range groups {
			if len(group.Members) == 1 && sessions[group.Members[0]] {
				continue
			}
			features = append(features, group)
		}
		groups = features
		return nil
	})
	if err != nil {
		return err
	}
	if w.S.Step != nil {
		if err := w.S.Step("workgroup-resolution-groups-discovered"); err != nil {
			return err
		}
	}
	if len(groups) > 0 {
		_, err = trace.MarkWorkgroupsResolved(ctx, w.Repository, groups, func(stream config.WorkstreamID) (bool, error) {
			if pending, err := trace.SessionDestroyPending(w.S.Current().Root, w.Repository.Project(), stream); err != nil || pending {
				return false, err
			}
			for _, a := range w.S.MemberArchives() {
				if present[stream] && a.Project == w.Repository.Project() && a.Workstream == stream && ArchiveResolved(a) {
					return true, nil
				}
			}
			if !present[stream] {
				for _, a := range w.S.MemberArchives() {
					if a.Project == w.Repository.Project() && a.Workstream == stream && (!a.CleanupStartedAt.IsZero() || !a.CleanedAt.IsZero()) {
						return ArchiveResolved(a), nil
					}
				}
				current, _, err := trace.Workgroups(w.Repository)
				if err != nil {
					return false, err
				}
				if _, present := trace.WorkgroupOf(current, stream); !present {
					return false, nil
				}
				return false, fmt.Errorf("member %s is missing without verified resolution and cleanup evidence", stream)
			}
			return Resolved(w.Repository, stream)
		}, w.S.Now(), MigrationActor, "workgroup-resolved")
		if err != nil {
			return err
		}
	}
	return w.ResolveCrossRepoWorkgroups(ctx)
}

// ReconcileArchivedEdgeMerges consumes durable edge merge receipts before
// deciding group resolution. A receipt records a verified merge even if an
// amendment subsequently removes the edge. Its PR must still match the archive's
// exact pinned publication; no upstream history is needed for restart replay.
func (w *WorkgroupResolver) ReconcileArchivedEdgeMerges(ctx context.Context) error {
	archives := w.S.MemberArchives()
	if len(archives) == 0 {
		return nil
	}
	streams, err := nonSessionWorkstreams(w.Repository)
	if err != nil {
		return err
	}
	if w.S.Step != nil {
		if err := w.S.Step("workgroup-resolution-edges-discovered"); err != nil {
			return err
		}
	}
	for _, stream := range streams {
		if err := ctx.Err(); err != nil {
			return err
		}
		if a, archived := w.S.ArchivedWorkstream(string(stream)); archived && (!a.CleanupStartedAt.IsZero() || !a.CleanedAt.IsZero()) {
			continue
		}
		observed, _, _, err := EdgeObservationAt(w.Repository, stream)
		if err != nil {
			if a, archived := w.S.ArchivedWorkstream(string(stream)); archived && (!a.CleanupStartedAt.IsZero() || !a.CleanedAt.IsZero()) {
				continue
			}
			return err
		}
		for _, status := range observed.Edges {
			if !status.Merged || status.Type != trace.EdgeHardBlock && status.Type != trace.EdgeMergeOrder {
				continue
			}
			for _, a := range archives {
				if a.Workstream != status.Upstream || a.State != DeliveredState || ArchiveResolved(a) || a.Publication.PullRequest <= 0 || status.Reference != fmt.Sprintf("%s#%d", a.Publication.Upstream, a.Publication.PullRequest) {
					continue
				}
				if err := w.S.Step("archive-edge-merge-resolution"); err != nil {
					return err
				}
				if err := w.S.Store().RecordArchivePullRequestMerge(a.Project, a.Workstream, a.Publication, status.Commit); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

// ResolveCrossRepoWorkgroups marks resolved, in its own trace, every
// cross-repo workgroup one of repository's own workstreams points to, once
// every one of its real members (wherever its own repository is) has
// reached a resolved state or has matching durable archive/resolution evidence.
// Absence and API failure never prove resolution.
func (w *WorkgroupResolver) ResolveCrossRepoWorkgroups(ctx context.Context) error {
	streams, err := nonSessionWorkstreams(w.Repository)
	if err != nil {
		return err
	}
	if w.S.Step != nil {
		if err := w.S.Step("workgroup-resolution-discovered"); err != nil {
			return err
		}
	}
	ids := make(map[config.WorkstreamID]config.WorkgroupID, len(streams))
	active := map[config.WorkgroupID]bool{}
	for _, stream := range streams {
		if err := ctx.Err(); err != nil {
			return err
		}
		id, err := w.Repository.CrossRepoWorkgroupOf(stream)
		if err != nil {
			return fmt.Errorf("workstream %s cross-repo workgroup: %w", stream, err)
		}
		ids[stream] = id
		if a, archived := w.S.ArchivedWorkstream(string(stream)); !archived || !ArchiveResolved(a) {
			active[id] = true
		}
	}
	archives := w.S.MemberArchives()
	for _, a := range archives {
		if a.Workgroup != "" && !ArchiveResolved(a) {
			active[a.Workgroup] = true
		}
	}
	seen := map[config.WorkgroupID]bool{}
	resolve := func(id config.WorkgroupID) error {
		if id == "" || seen[id] {
			return nil
		}
		seen[id] = true
		if !active[id] && trace.WorkgroupResolutionCurrent(id) {
			return nil
		}
		return w.ResolveCrossRepoWorkgroup(ctx, id)
	}
	for _, a := range archives {
		if a.Project != w.Repository.Project() {
			continue
		}
		if err := resolve(a.Workgroup); err != nil {
			return err
		}
	}
	for _, stream := range streams {
		if err := ctx.Err(); err != nil {
			return err
		}
		id := ids[stream]
		if err := resolve(id); err != nil {
			if errors.Is(err, trace.ErrLocked) || errors.Is(err, trace.ErrMembershipChanged) {
				continue
			}
			return fmt.Errorf("workgroup %s: %w", id, err)
		}
	}
	return nil
}
func (w *WorkgroupResolver) ResolveCrossRepoWorkgroup(ctx context.Context, id config.WorkgroupID) error {
	lock := w.S.GroupApprovalLock(id)
	if !lock.TryLock() {
		return nil
	}
	defer lock.Unlock()
	g, _, err := w.Repository.CrossRepoWorkgroup(id)
	if err != nil {
		return err
	}
	if len(g.Members) < 2 {
		return nil
	}
	members, err := w.S.CrossRepoMemberRepos(ctx, g)
	if errors.Is(err, errWorkgroupJoining) {
		return nil
	}
	if err != nil {
		return err
	}
	wt, err := trace.OpenWorkgroupTrace(w.S.Current().Root, id, members)
	if wt != nil && err != nil {
		wt.Close()
	}
	if err != nil {
		return err
	}
	defer wt.Close()
	real := trace.Workgroup{ID: g.ID, Members: g.Members[1:]}
	_, err = trace.MarkWorkgroupsResolved(ctx, wt, []trace.Workgroup{real}, func(stream config.WorkstreamID) (bool, error) {
		if err := ctx.Err(); err != nil {
			return false, err
		}
		retired, err := wt.MemberRetired(stream)
		if err != nil {
			return false, err
		}
		if retired {
			return w.RetiredMemberResolved(id, stream)
		}
		if err := w.S.Step(fmt.Sprintf("workgroup-member-resolution-read:%s:%s", w.Repository.Project(), stream)); err != nil {
			return false, err
		}
		done, readErr := Resolved(wt, stream)
		if readErr == nil {
			if done && stream == real.Members[len(real.Members)-1] {
				if err := w.S.Step("workgroup-resolution-ready"); err != nil {
					return false, err
				}
			}
			return done, nil
		}
		retired, err = wt.MemberRetired(stream)
		if err != nil {
			return false, err
		}
		if retired {
			return w.RetiredMemberResolved(id, stream)
		}
		return false, readErr
	}, w.S.Now(), MigrationActor, "workgroup-resolved")
	return err
}
func (w *WorkgroupResolver) RetiredMemberResolved(id config.WorkgroupID, stream config.WorkstreamID) (bool, error) {
	for _, a := range w.S.MemberArchives() {
		if a.Workgroup == id && a.Workstream == stream && ArchiveResolved(a) {
			return true, nil
		}
	}
	return false, nil
}
