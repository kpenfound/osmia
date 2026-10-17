package service

import (
	"context"
	"errors"
	"fmt"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/runtime"
	"github.com/kpenfound/osmia/internal/trace"
)

// migrationActor records the startup migration that wraps existing
// workstreams in workgroups.
var migrationActor = trace.Actor{Kind: "service", ID: "osmia"}

// ensureWorkgroupsInterrupt lets tests simulate a crash partway through
// ensureWorkgroups, right before a given project's workgroups would be
// recorded, the same way migrationInterrupt tests the trace-layout
// migration.
var ensureWorkgroupsInterrupt func(config.ProjectID) error

// ensureWorkgroups wraps every existing non-session workstream of each
// project's own default repo in its own one-workstream workgroup, whose
// chief of staff is that workstream's existing chief-of-staff thread: no new
// thread is created. It runs once on every start, before the project
// schedules any work, and is resumable and idempotent. A workstream already
// in a recorded workgroup is left alone, so running it again, or resuming
// it after a crash mid-way, changes nothing further and duplicates nothing.
// Exploratory sessions are never put in a workgroup. A workstream that
// already points to a cross-repo workgroup (recorded in a different trace,
// outside this project) is left alone too: wrapping it in a same-project
// one-workstream workgroup here would make its membership authoritative in
// two places (charter#2) and give it a second, bogus chief-of-staff host.
func (s *Service) ensureWorkgroups(ctx context.Context, cfg *config.Config, projects []*activeProject, archives []runtime.Archive) error {
	repos := map[config.ProjectID]config.Project{}
	for _, p := range cfg.Projects {
		repos[p.ID] = p
	}
	for _, p := range projects {
		repo, ok := repos[p.id]
		if !ok || !repo.IsDefaultRepo() {
			continue
		}
		streams, err := p.repository.Workstreams()
		if err != nil {
			return err
		}
		librarian := librarianWorkstream(p.id)
		present := make(map[config.WorkstreamID]bool, len(streams))
		var members []config.WorkstreamID
		for _, stream := range streams {
			present[stream] = true
			if stream == librarian {
				// The librarian's own workstream tracks knowledge-base
				// extraction; it is service bookkeeping, not a feature
				// workstream, and never belongs to a workgroup.
				continue
			}
			session, err := isSession(p.repository, stream)
			if err != nil {
				return err
			}
			if session {
				continue
			}
			if id, err := p.repository.CrossRepoWorkgroupOf(stream); err != nil {
				return err
			} else if id != "" {
				continue
			}
			if pending, err := p.repository.PendingCrossRepoWorkgroup(stream); err != nil {
				return err
			} else if pending != "" {
				continue
			}
			members = append(members, stream)
		}
		// Runtime retains archived feature identities after trace cleanup. Read
		// only their mapping here: deleted history supplies no new lifecycle or
		// resolution evidence, and a pinned cross-repo group stays authoritative.
		for _, a := range archives {
			if a.Project != p.id || present[a.Workstream] || a.Workstream == librarian || a.Workgroup != "" {
				continue
			}
			members = append(members, a.Workstream)
		}
		if len(members) == 0 {
			continue
		}
		if ensureWorkgroupsInterrupt != nil {
			if err := ensureWorkgroupsInterrupt(p.id); err != nil {
				return err
			}
		}
		if _, err := trace.EnsureWorkgroups(ctx, p.repository, members, s.now(), migrationActor, "migrate-workgroups"); err != nil {
			return err
		}
	}
	return nil
}

// resolved requires cancellation or recorded merge evidence for delivery.
func resolved(repository *trace.Repository, stream config.WorkstreamID) (bool, error) {
	state, err := repository.Workflow(stream, trace.FeatureSubject)
	if err != nil {
		return false, err
	}
	evidence, err := readMemberEvidence(repository, stream)
	return evidence.resolution(state.Value) != "unresolved", err
}

// workgroupResolver records, once every member of a workgroup is resolved,
// that the workgroup itself has resolved: the distinct trigger other units
// retire workgroup-level documents on, such as a project spec. Resolving
// one member never resolves the workgroup and never fires this trigger;
// only resolving every member does, and only once.
type workgroupResolver struct {
	s          *Service
	repository *trace.Repository
}

// Pass marks every workgroup whose every member is now resolved. It is
// resumable and idempotent: a workgroup already recorded resolved is left
// alone, and nothing here deletes, archives or otherwise retires anything,
// a member's own cleanup included. It also resolves every cross-repo
// workgroup one of repository's own workstreams belongs to, recording that
// resolution in the workgroup's own trace rather than repository's: every
// member's own pass that finds the same workgroup tries the same idempotent
// record, so none of them needs to be the sole owner of it.
func (w *workgroupResolver) Pass(ctx context.Context) error {
	if err := w.reconcileArchivedEdgeMerges(ctx); err != nil {
		return err
	}
	groups, _, err := trace.Workgroups(w.repository)
	if err != nil {
		return err
	}
	if len(groups) > 0 {
		streams, err := w.repository.Workstreams()
		if err != nil {
			return err
		}
		present := make(map[config.WorkstreamID]bool, len(streams))
		for _, stream := range streams {
			present[stream] = true
		}
		_, err = trace.MarkWorkgroupsResolved(ctx, w.repository, groups, func(stream config.WorkstreamID) (bool, error) {
			for _, a := range w.s.memberArchives() {
				if present[stream] && a.Project == w.repository.Project() && a.Workstream == stream && archiveResolved(a) {
					return true, nil
				}
			}
			if !present[stream] {
				for _, a := range w.s.memberArchives() {
					if a.Project == w.repository.Project() && a.Workstream == stream && (!a.CleanupStartedAt.IsZero() || !a.CleanedAt.IsZero()) {
						return archiveResolved(a), nil
					}
				}
				return false, fmt.Errorf("member %s is missing without verified resolution and cleanup evidence", stream)
			}
			return resolved(w.repository, stream)
		}, w.s.now(), migrationActor, "workgroup-resolved")
		if err != nil {
			return err
		}
	}
	return w.resolveCrossRepoWorkgroups(ctx)
}

// reconcileArchivedEdgeMerges consumes durable edge merge receipts before
// deciding group resolution. A receipt records a verified merge even if an
// amendment subsequently removes the edge. Its PR must still match the archive's
// exact pinned publication; no upstream history is needed for restart replay.
func (w *workgroupResolver) reconcileArchivedEdgeMerges(ctx context.Context) error {
	archives := w.s.memberArchives()
	if len(archives) == 0 {
		return nil
	}
	streams, err := w.repository.Workstreams()
	if err != nil {
		return err
	}
	for _, stream := range streams {
		if err := ctx.Err(); err != nil {
			return err
		}
		if a, archived := w.s.archivedWorkstream(string(stream)); archived && (!a.CleanupStartedAt.IsZero() || !a.CleanedAt.IsZero()) {
			continue
		}
		observed, _, _, err := edgeObservationAt(w.repository, stream)
		if err != nil {
			if a, archived := w.s.archivedWorkstream(string(stream)); archived && (!a.CleanupStartedAt.IsZero() || !a.CleanedAt.IsZero()) {
				continue
			}
			return err
		}
		for _, status := range observed.Edges {
			if !status.Merged || status.Type != trace.EdgeHardBlock && status.Type != trace.EdgeMergeOrder {
				continue
			}
			for _, a := range archives {
				if a.Workstream != status.Upstream || a.State != DeliveredState || archiveResolved(a) || a.Publication.PullRequest <= 0 || status.Reference != fmt.Sprintf("%s#%d", a.Publication.Upstream, a.Publication.PullRequest) {
					continue
				}
				if err := w.s.step("archive-edge-merge-resolution"); err != nil {
					return err
				}
				if err := w.s.store.RecordArchivePullRequestMerge(a.Project, a.Workstream, a.Publication, status.Commit); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

// resolveCrossRepoWorkgroups marks resolved, in its own trace, every
// cross-repo workgroup one of repository's own workstreams points to, once
// every one of its real members (wherever its own repository is) has
// reached a resolved state or has matching durable archive/resolution evidence.
// Absence and API failure never prove resolution.
func (w *workgroupResolver) resolveCrossRepoWorkgroups(ctx context.Context) error {
	streams, err := w.repository.Workstreams()
	if err != nil {
		return err
	}
	seen := map[config.WorkgroupID]bool{}
	for _, a := range w.s.memberArchives() {
		if a.Project != w.repository.Project() || a.Workgroup == "" || seen[a.Workgroup] {
			continue
		}
		seen[a.Workgroup] = true
		if err := w.resolveCrossRepoWorkgroup(ctx, a.Workgroup); err != nil {
			return err
		}
	}
	for _, stream := range streams {
		if err := ctx.Err(); err != nil {
			// A resolution interrupted by the service stopping is not a
			// failure of this pass; it resumes on the next start or pass.
			return err
		}
		id, err := w.repository.CrossRepoWorkgroupOf(stream)
		if err != nil {
			return fmt.Errorf("workstream %s cross-repo workgroup: %w", stream, err)
		}
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		if err := w.resolveCrossRepoWorkgroup(ctx, id); err != nil {
			if errors.Is(err, trace.ErrLocked) || errors.Is(err, trace.ErrMembershipChanged) {
				// The workgroup trace is busy with another member's own
				// pass, or a member joined since the membership was read;
				// try again next pass.
				continue
			}
			return fmt.Errorf("workgroup %s: %w", id, err)
		}
	}
	return nil
}

func (w *workgroupResolver) resolveCrossRepoWorkgroup(ctx context.Context, id config.WorkgroupID) error {
	lock := w.s.groupApprovalLock(id)
	if !lock.TryLock() {
		return nil
	}
	defer lock.Unlock()
	_, g, _, err := w.repository.CrossRepoProjectSpec(id)
	if err != nil {
		return err
	}
	if len(g.Members) < 2 {
		return nil
	}
	members, err := w.s.crossRepoMemberRepos(ctx, g)
	if errors.Is(err, errWorkgroupJoining) {
		return nil
	}
	if err != nil {
		return err
	}
	wt, err := trace.OpenWorkgroupTrace(w.s.current().Root, id, members)
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
			return w.retiredMemberResolved(id, stream)
		}
		if err := w.s.step(fmt.Sprintf("workgroup-member-resolution-read:%s:%s", w.repository.Project(), stream)); err != nil {
			return false, err
		}
		// Retain the shared binding through workflow and publication reads.
		// Another repo's cleanup controller can retire this member meanwhile.
		done, readErr := resolved(wt, stream)
		if readErr == nil {
			if done && stream == real.Members[len(real.Members)-1] {
				if err := w.s.step("workgroup-resolution-ready"); err != nil {
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
			return w.retiredMemberResolved(id, stream)
		}
		return false, readErr
	}, w.s.now(), migrationActor, "workgroup-resolved")
	return err
}

func (w *workgroupResolver) retiredMemberResolved(id config.WorkgroupID, stream config.WorkstreamID) (bool, error) {
	for _, a := range w.s.memberArchives() {
		if a.Workgroup == id && a.Workstream == stream && archiveResolved(a) {
			return true, nil
		}
	}
	// Retirement without resolution evidence is an expected hold, presented
	// on the archived member's pane. It must not stop the survivor's controller.
	return false, nil
}
