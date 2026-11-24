package lifecycle

import (
	"context"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/delivery"
	"github.com/kpenfound/osmia/internal/ghlog"
	"github.com/kpenfound/osmia/internal/pulls"
	"github.com/kpenfound/osmia/internal/trace"
)

// ObserveLandedPullRequests looks up the own pull request of every delivered
// workstream that landed on its base branch, no more often than the project's
// pull_request_poll interval, while the pull request's outcome is not recorded.
// It closes a pull request it finds open with a comment naming the landed commit
// and base branch, and records the outcome of one the host already shows merged
// or closed without touching it. A lookup that fails supplies no evidence and is
// retried on the next interval.
func (b *BaseRefresher) ObserveLandedPullRequests(ctx context.Context) error {
	client := b.S.Options.PullRequests
	if client == nil || !pulls.Credentialed(client) {
		return nil
	}
	streams, err := nonSessionWorkstreams(b.Repository)
	if err != nil {
		return err
	}
	for _, stream := range pollOrder(ctx, b.S, LandedPoll, streams, func(stream config.WorkstreamID) config.WorkstreamID { return stream }) {
		if _, archived := b.S.ArchivedWorkstream(string(stream)); archived {
			continue
		}
		due, err := b.landedPollDue(stream)
		if err != nil {
			return err
		}
		if !due {
			continue
		}
		if err := b.S.PollTask(ctx, b.Repository, LandedPoll, stream, func(ctx context.Context) error {
			return b.observeLandedPullRequest(ctx, client, stream)
		}); err != nil {
			return err
		}
	}
	return nil
}

func (b *BaseRefresher) landedPollDue(stream config.WorkstreamID) (bool, error) {
	project := b.Repository.Project()
	if attempt, checked := b.S.Polls.Last(project, stream, LandedPoll); checked && b.S.Now().Sub(attempt.At) < b.S.About(b.Repository).Project.PullRequestPollInterval() {
		return false, nil
	}
	feature, err := b.Repository.Workflow(stream, trace.FeatureSubject)
	if err != nil {
		return false, err
	}
	if feature.Value != DeliveredState {
		b.S.Polls.Forget(project, stream, LandedPoll)
		return false, nil
	}
	_, _, pending, err := delivery.LandedPullRequest(b.Repository, stream)
	if err != nil || !pending {
		b.S.Polls.Forget(project, stream, LandedPoll)
	}
	return pending, err
}

func (b *BaseRefresher) observeLandedPullRequest(ctx context.Context, client pulls.Client, stream config.WorkstreamID) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if due, err := b.landedPollDue(stream); err != nil || !due {
		return err
	}
	b.S.Polls.Record(b.Repository.Project(), stream, LandedPoll, PullRequestAttempt{At: b.S.Now()})
	scope := ghlog.WithScope(ctx, ghlog.Scope{Project: string(b.Repository.Project()), Workstream: string(stream)})
	_, err := delivery.SettleLandedPullRequest(scope, client, b.Repository, b.S.Now(), stream, b.S.Step)
	return err
}
