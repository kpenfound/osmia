package service

import (
	"context"

	"github.com/kpenfound/osmia/internal/ghlog"
)

// observeArchivedPullRequests observes the archive's immutable publication even
// when no dependent observes it and its member trace has been cleanup-deleted.
// Only the runtime archive is updated; retained or deleted history stays intact.
func (b *baseRefresher) observeArchivedPullRequests(ctx context.Context) error {
	client := b.s.options.PullRequests
	if client == nil {
		return nil
	}
	for _, a := range b.s.memberArchives() {
		if err := ctx.Err(); err != nil {
			return err
		}
		if a.Project != b.repository.Project() {
			continue
		}
		if a.State != DeliveredState || archiveResolved(a) || a.Publication.PullRequest <= 0 {
			b.s.polls.forget(b.repository.Project(), a.Workstream, archivePoll)
			continue
		}
		now := b.s.now()
		if attempt, checked := b.s.polls.last(b.repository.Project(), a.Workstream, archivePoll); checked && now.Sub(attempt.at) < b.s.about(b.repository).Project.PullRequestPollInterval() {
			continue
		}
		b.s.polls.record(b.repository.Project(), a.Workstream, archivePoll, pullRequestAttempt{at: now})
		p := a.Publication
		scope := ghlog.WithScope(ctx, ghlog.Scope{Project: string(a.Project), Workstream: string(a.Workstream)})
		found, err := client.Find(scope, p.Upstream, p.Fork, p.Branch)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			// A failed read supplies no evidence and retries on the next cadence.
			continue
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
	}
	return nil
}
