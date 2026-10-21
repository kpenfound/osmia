package lifecycle

import (
	"context"
	"errors"
	"fmt"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/runtime"
	"github.com/kpenfound/osmia/internal/trace"
)

// MigrationActor records the startup migration that wraps existing
// workstreams in workgroups.
var MigrationActor = trace.Actor{Kind: "service", ID: "osmia"}

// EnsureWorkgroupsInterrupt lets tests simulate a crash partway through
// EnsureWorkgroups, right before a given project's workgroups would be
// recorded, the same way migrationInterrupt tests the trace-layout
// migration.
var EnsureWorkgroupsInterrupt func(config.ProjectID) error

// EnsureWorkgroups wraps every existing non-session workstream of each
// Project's own default repo in its own one-workstream workgroup, whose
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
func (s *Controller) EnsureWorkgroups(ctx context.Context, cfg *config.Config, projects []*Project, archives []runtime.Archive) error {
	repos := map[config.ProjectID]config.Project{}
	for _, p := range cfg.Projects {
		repos[p.ID] = p
	}
	for _, p := range projects {
		repo, ok := repos[p.ID]
		if !ok || !repo.IsDefaultRepo() {
			continue
		}
		streams, err := p.Repository.Workstreams()
		if err != nil {
			return err
		}
		librarian := librarianWorkstream(p.ID)
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
		// Runtime retains archived feature identities after trace cleanup. Read
		// only their mapping here: deleted history supplies no new lifecycle or
		// resolution evidence, and a pinned cross-repo group stays authoritative.
		for _, a := range archives {
			if a.Project != p.ID || present[a.Workstream] || a.Workstream == librarian || a.Workgroup != "" {
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
	groups, _, err := trace.Workgroups(w.Repository)
	if err != nil {
		return err
	}
	if len(groups) > 0 {
		streams, err := w.Repository.Workstreams()
		if err != nil {
			return err
		}
		present := make(map[config.WorkstreamID]bool, len(streams))
		for _, stream := range streams {
			present[stream] = true
		}
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
	streams, err := w.Repository.Workstreams()
	if err != nil {
		return err
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
	streams, err := w.Repository.Workstreams()
	if err != nil {
		return err
	}
	seen := map[config.WorkgroupID]bool{}
	for _, a := range w.S.MemberArchives() {
		if a.Project != w.Repository.Project() || a.Workgroup == "" || seen[a.Workgroup] {
			continue
		}
		seen[a.Workgroup] = true
		if err := w.ResolveCrossRepoWorkgroup(ctx, a.Workgroup); err != nil {
			return err
		}
	}
	for _, stream := range streams {
		if err := ctx.Err(); err != nil {
			// A resolution interrupted by the service stopping is not a
			// failure of this pass; it resumes on the next start or pass.
			return err
		}
		id, err := w.Repository.CrossRepoWorkgroupOf(stream)
		if err != nil {
			return fmt.Errorf("workstream %s cross-repo workgroup: %w", stream, err)
		}
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		if err := w.ResolveCrossRepoWorkgroup(ctx, id); err != nil {
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
func (w *WorkgroupResolver) ResolveCrossRepoWorkgroup(ctx context.Context, id config.WorkgroupID) error {
	lock := w.S.GroupApprovalLock(id)
	if !lock.TryLock() {
		return nil
	}
	defer lock.Unlock()
	_, g, _, err := w.Repository.CrossRepoProjectSpec(id)
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
		// Retain the shared binding through workflow and publication reads.
		// Another repo's cleanup controller can retire this member meanwhile.
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
	// Retirement without resolution evidence is an expected hold, presented
	// on the archived member's pane. It must not stop the survivor's controller.
	return false, nil
}
