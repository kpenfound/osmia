package lifecycle

import (
	"context"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/ghlog"
	"github.com/kpenfound/osmia/internal/runtime"
)

// ObserveArchivedPullRequests observes the archive's immutable publication even
// when no dependent observes it and its member trace has been cleanup-deleted.
// Only the runtime archive is updated; retained or deleted history stays intact.
func (b *BaseRefresher) ObserveArchivedPullRequests(ctx context.Context) error {
	client := b.S.Options.PullRequests
	if client == nil {
		return nil
	}
	for _, a := range pollOrder(ctx, b.S, ArchivePoll, b.S.MemberArchives(), func(a runtime.Archive) config.WorkstreamID { return a.Workstream }) {
		if a.Project != b.Repository.Project() {
			continue
		}
		if err := b.S.PollTask(ctx, b.Repository, ArchivePoll, a.Workstream, func(ctx context.Context) error {
			return b.observeArchivedPullRequest(ctx, a)
		}); err != nil {
			return err
		}
	}
	return nil
}

func (b *BaseRefresher) observeArchivedPullRequest(ctx context.Context, a runtime.Archive) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if a.Project != b.Repository.Project() {
		return nil
	}
	if a.State != DeliveredState || ArchiveResolved(a) || a.Publication.PullRequest <= 0 {
		b.S.Polls.Forget(b.Repository.Project(), a.Workstream, ArchivePoll)
		return nil
	}
	now := b.S.Now()
	if attempt, checked := b.S.Polls.Last(b.Repository.Project(), a.Workstream, ArchivePoll); checked && now.Sub(attempt.At) < b.S.About(b.Repository).Project.PullRequestPollInterval() {
		return nil
	}
	b.S.Polls.Record(b.Repository.Project(), a.Workstream, ArchivePoll, PullRequestAttempt{At: now})
	p := a.Publication
	scope := ghlog.WithScope(ctx, ghlog.Scope{Project: string(a.Project), Workstream: string(a.Workstream)})
	found, err := b.S.Options.PullRequests.Find(scope, p.Upstream, p.Fork, p.Branch)
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
		if err := b.S.Step("archive-own-pr-merge-observed"); err != nil {
			return err
		}
		if err := b.S.Store().RecordArchivePullRequestMerge(a.Project, a.Workstream, p, pr.MergeCommit); err != nil {
			return err
		}
		b.S.Polls.Forget(b.Repository.Project(), a.Workstream, ArchivePoll)
		break
	}
	return nil
}
