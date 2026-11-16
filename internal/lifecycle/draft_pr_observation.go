package lifecycle

import (
	"context"
	"strings"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/ghlog"
	"github.com/kpenfound/osmia/internal/pulls"
	"github.com/kpenfound/osmia/internal/trace"
)

// ObserveDraftPullRequests looks up the draft pull request of every assembled
// workstream with a recorded push, no more often than the project's
// pull_request_poll interval, and records the workstream delivered by the
// owner when it finds the pull request open and no longer a draft: the owner
// marked it ready on the pull request host directly. A lookup that fails
// supplies no evidence and is retried on the next interval.
func (b *BaseRefresher) ObserveDraftPullRequests(ctx context.Context) error {
	client := b.S.Options.PullRequests
	if client == nil || !pulls.Credentialed(client) {
		return nil
	}
	streams, err := nonSessionWorkstreams(b.Repository)
	if err != nil {
		return err
	}
	for _, stream := range pollOrder(ctx, b.S, DraftPoll, streams, func(stream config.WorkstreamID) config.WorkstreamID { return stream }) {
		if _, archived := b.S.ArchivedWorkstream(string(stream)); archived {
			continue
		}
		due, err := b.draftPollDue(stream)
		if err != nil {
			return err
		}
		if !due {
			continue
		}
		if err := b.S.PollTask(ctx, b.Repository, DraftPoll, stream, func(ctx context.Context) error {
			return b.observeDraftPullRequest(ctx, client, stream)
		}); err != nil {
			return err
		}
	}
	return nil
}

// draftPollDue reports whether stream is assembled with a pull request of a
// recorded push whose lookup is due. It forgets the cadence of a workstream
// that needs no lookup.
func (b *BaseRefresher) draftPollDue(stream config.WorkstreamID) (bool, error) {
	project := b.Repository.Project()
	if attempt, checked := b.S.Polls.Last(project, stream, DraftPoll); checked && b.S.Now().Sub(attempt.At) < b.S.About(b.Repository).Project.PullRequestPollInterval() {
		return false, nil
	}
	feature, err := b.Repository.Workflow(stream, trace.FeatureSubject)
	if err != nil {
		return false, err
	}
	if feature.Value != AssembledState {
		b.S.Polls.Forget(project, stream, DraftPoll)
		return false, nil
	}
	_, found, err := currentPush(b.Repository, stream)
	return found, err
}

func (b *BaseRefresher) observeDraftPullRequest(ctx context.Context, client pulls.Client, stream config.WorkstreamID) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if due, err := b.draftPollDue(stream); err != nil || !due {
		return err
	}
	// An approval whose application is still to run marks the pull request
	// ready itself and records the delivery with the approval.
	if publication, err := b.Repository.Workflow(stream, publicationSubject); err != nil {
		return err
	} else if strings.HasPrefix(publication.Value, "requested-") {
		return nil
	}
	push, _, err := currentPush(b.Repository, stream)
	if err != nil {
		return err
	}
	b.S.Polls.Record(b.Repository.Project(), stream, DraftPoll, PullRequestAttempt{At: b.S.Now()})
	scope := ghlog.WithScope(ctx, ghlog.Scope{Project: string(b.Repository.Project()), Workstream: string(stream)})
	found, err := client.Find(scope, push.Upstream, push.Fork, push.Branch)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return nil
	}
	for _, pr := range found {
		if pr.Number != push.PullRequest || pr.Draft {
			continue
		}
		if err := b.S.Step("draft-ready-observed"); err != nil {
			return err
		}
		_, err := markedReadyOnHost(scope, b.Repository, stream, pr, b.S.Now())
		return err
	}
	return nil
}
