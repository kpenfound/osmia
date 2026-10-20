package service

import (
	"context"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/ghlog"
	"github.com/kpenfound/osmia/internal/runtime"
)

// observeArchivedPullRequests observes the archive's immutable publication even
// when no dependent observes it and its member trace has been cleanup-deleted.
// Only the runtime archive is updated; retained or deleted history stays intact.
func (b *baseRefresher) observeArchivedPullRequests(ctx context.Context) error {
	client := b.s.options.PullRequests
	if client == nil {
		return nil
	}
	for _, a := range pollOrder(ctx, archivePoll, b.s.memberArchives(), func(a runtime.Archive) config.WorkstreamID { return a.Workstream }) {
		if a.Project != b.repository.Project() {
			continue
		}
		if err := b.s.pollTask(ctx, b.repository, archivePoll, a.Workstream, func(ctx context.Context) error {
			return b.observeArchivedPullRequest(ctx, a)
		}); err != nil {
			return err
		}
	}
	return nil
}

func (b *baseRefresher) observeArchivedPullRequest(ctx context.Context, a runtime.Archive) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if a.Project != b.repository.Project() {
		return nil
	}
	if a.State != DeliveredState || archiveResolved(a) || a.Publication.PullRequest <= 0 {
		b.s.polls.forget(b.repository.Project(), a.Workstream, archivePoll)
		return nil
	}
	now := b.s.now()
	if attempt, checked := b.s.polls.last(b.repository.Project(), a.Workstream, archivePoll); checked && now.Sub(attempt.at) < b.s.about(b.repository).Project.PullRequestPollInterval() {
		return nil
	}
	b.s.polls.record(b.repository.Project(), a.Workstream, archivePoll, pullRequestAttempt{at: now})
	p := a.Publication
	scope := ghlog.WithScope(ctx, ghlog.Scope{Project: string(a.Project), Workstream: string(a.Workstream)})
	found, err := b.s.options.PullRequests.Find(scope, p.Upstream, p.Fork, p.Branch)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		// A failed read supplies no evidence and retries on the next cadence.
		return nil
	}
	for _, pr := range found {
		if pr.Number != p.PullRequest || !pr.Merged {
			continue
		}
		if err := b.s.step("archive-own-pr-merge-observed"); err != nil {
			return err
		}
		if err := b.s.store.RecordArchivePullRequestMerge(a.Project, a.Workstream, p, pr.MergeCommit); err != nil {
			return err
		}
		b.s.polls.forget(b.repository.Project(), a.Workstream, archivePoll)
		break
	}
	return nil
}
