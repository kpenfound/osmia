package executionjourney

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/kpenfound/osmia/internal/delivery"
	"github.com/kpenfound/osmia/internal/intake"
	"github.com/kpenfound/osmia/internal/lifecycle"
	"github.com/kpenfound/osmia/internal/servicetest/journey"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/pulls"
	"github.com/kpenfound/osmia/internal/seal"
	"github.com/kpenfound/osmia/internal/service"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	"github.com/kpenfound/osmia/internal/trace"
)

func TestWorkgroupOwnPullRequestObservationWithRecordedBaseIntegration(t *testing.T) {
	t.Parallel()
	cfg, repo, host, refresh := journey.OwnPRFixture(t)
	journey.SealStream(t, repo)
	_, err := trace.CreateWorkgroup(context.Background(), repo, []config.WorkstreamID{fixturekit.Stream}, fixturekit.DemoStart, intake.OwnerActor, "fixture")
	fixturekit.Must(t, err)
	publication := journey.DeliverWithPullRequest(t, repo, cfg, 42)
	journey.ResolutionDocument(t, repo, fixturekit.Stream, lifecycle.BaseObservationDocument, "base-observation.json", lifecycle.BaseObservation{Base: seal.Base{Remote: "upstream", Branch: "main", Commit: "integrated"}})
	host.Prs = []pulls.PullRequest{{Number: 42, Merged: true, State: "closed", Head: publication.Branch, HeadRepository: publication.Fork}}
	fixturekit.Must(t, refresh.Pass(context.Background()))
	op := fixturekit.ActionOperation(t, repo, fixturekit.Stream, lifecycle.BaseRefreshAction)
	_, err = refresh.Apply(context.Background(), op)
	fixturekit.Must(t, err)
	outcome, revision, err := delivery.OwnPullRequestOutcomeAt(repo, fixturekit.Stream)
	fixturekit.Must(t, err)
	if host.Finds != 1 || revision != 1 || outcome.PullRequest != 42 || outcome.Outcome != delivery.OwnPullRequestOutcomeMerged {
		t.Fatalf("integrated base prevented own observation: finds=%d outcome=%+v revision=%d", host.Finds, outcome, revision)
	}
	_, revision, err = service.TestBaseObservationAt(repo, fixturekit.Stream)
	fixturekit.Must(t, err)
	if revision != 1 {
		t.Fatal("own observation changed recorded base integration")
	}
}

func TestWorkgroupOwnPullRequestObservationRetriesOnCadenceAndStopsWhenClosed(t *testing.T) {
	t.Parallel()
	for _, based := range []bool{false, true} {
		t.Run(fmt.Sprintf("with-base-%v", based), func(t *testing.T) {
			cfg, repo, host, refresh := journey.OwnPRFixture(t)
			if based {
				journey.SealStream(t, repo)
			}
			clock := &fixturekit.FixedClock{At: fixturekit.DemoStart}
			refresh.S.Options.Reconciliation.Now = clock.Now
			_, err := trace.CreateWorkgroup(context.Background(), repo, []config.WorkstreamID{fixturekit.Stream}, fixturekit.DemoStart, intake.OwnerActor, "fixture")
			fixturekit.Must(t, err)
			publication := journey.DeliverWithPullRequest(t, repo, cfg, 42)
			host.FindErr = errors.New("provider unavailable")
			ctx := context.Background()
			fixturekit.Must(t, refresh.Pass(ctx))
			op := fixturekit.ActionOperation(t, repo, fixturekit.Stream, lifecycle.BaseRefreshAction)
			_, err = refresh.Apply(ctx, op)
			fixturekit.Must(t, err)
			if _, revision, err := delivery.OwnPullRequestOutcomeAt(repo, fixturekit.Stream); err != nil || revision != 0 {
				t.Fatalf("failed lookup recorded an outcome: revision=%d err=%v", revision, err)
			}
			clock.Advance(10*time.Minute - time.Nanosecond)
			fixturekit.Must(t, refresh.Pass(ctx))
			state, err := repo.Workflow(fixturekit.Stream, lifecycle.BaseRefreshAction)
			fixturekit.Must(t, err)
			if state.Version != 2 || state.Value != "checked" {
				t.Fatal("own lookup retried before the polling interval")
			}
			clock.Advance(time.Nanosecond)
			host.FindErr = nil
			host.Prs = []pulls.PullRequest{{Number: 42, State: "closed", Head: publication.Branch, HeadRepository: publication.Fork}}
			fixturekit.Must(t, refresh.Pass(ctx))
			op = fixturekit.ActionOperation(t, repo, fixturekit.Stream, lifecycle.BaseRefreshAction)
			_, err = refresh.Apply(ctx, op)
			fixturekit.Must(t, err)
			outcome, revision, err := delivery.OwnPullRequestOutcomeAt(repo, fixturekit.Stream)
			fixturekit.Must(t, err)
			if host.Finds != 2 || revision != 1 || outcome.Outcome != delivery.OwnPullRequestOutcomeClosed {
				t.Fatalf("next cadence did not observe closure: finds=%d outcome=%+v revision=%d", host.Finds, outcome, revision)
			}
			clock.Advance(10 * time.Minute)
			fixturekit.Must(t, refresh.Pass(ctx))
			state, err = repo.Workflow(fixturekit.Stream, lifecycle.BaseRefreshAction)
			fixturekit.Must(t, err)
			if state.Version != 4 || state.Value != "checked" {
				t.Fatal("closed PR scheduled another lookup")
			}
			done, err := service.TestResolved(repo, fixturekit.Stream)
			fixturekit.Must(t, err)
			if done {
				t.Fatal("closed unmerged PR resolved the member")
			}
		})
	}
}

func TestBaseRefreshRecordsMergedOwnPullRequest(t *testing.T) {
	t.Parallel()
	cfg, repo, host, refresh := journey.OwnPRFixture(t)
	journey.SealStream(t, repo)
	publication := journey.DeliverWithPullRequest(t, repo, cfg, 42)
	host.Prs = []pulls.PullRequest{{Number: 42, Merged: true, State: "closed", Head: publication.Branch, HeadRepository: publication.Fork}}
	fixturekit.Must(t, refresh.Pass(context.Background()))
	op := fixturekit.ActionOperation(t, repo, fixturekit.Stream, lifecycle.BaseRefreshAction)
	result, err := refresh.Apply(context.Background(), op)
	fixturekit.Must(t, err)
	if result.Outcome != "succeeded" {
		t.Fatalf("base-refresh apply: %+v", result)
	}
	if host.Finds != 1 {
		t.Fatalf("own-PR lookup calls: %d", host.Finds)
	}
	outcome, revision, err := delivery.OwnPullRequestOutcomeAt(repo, fixturekit.Stream)
	fixturekit.Must(t, err)
	if revision != 1 || outcome.PullRequest != 42 || outcome.Outcome != delivery.OwnPullRequestOutcomeMerged {
		t.Fatalf("own-PR outcome %+v revision %d", outcome, revision)
	}
	if _, revision, err := service.TestBaseObservationAt(repo, fixturekit.Stream); err != nil || revision != 0 {
		t.Fatalf("the refresh made a further remote call: base-observation revision %d err %v", revision, err)
	}
	finished, err := delivery.OwnPullRequestFinished(repo, fixturekit.Stream)
	fixturekit.Must(t, err)
	if !finished {
		t.Fatal("the recorded merged outcome does not report the workstream's own PR finished")
	}
	records, err := delivery.Publications(repo, fixturekit.Stream)
	fixturekit.Must(t, err)
	if last := records[len(records)-1]; last.Status != delivery.PublicationOpened {
		t.Fatalf("recording the own-PR outcome changed the publication status to %s", last.Status)
	}
}

func TestBaseRefreshRecordsClosedOwnPullRequest(t *testing.T) {
	t.Parallel()
	cfg, repo, host, refresh := journey.OwnPRFixture(t)
	journey.SealStream(t, repo)
	publication := journey.DeliverWithPullRequest(t, repo, cfg, 42)
	host.Prs = []pulls.PullRequest{{Number: 42, State: "closed", Head: publication.Branch, HeadRepository: publication.Fork}}
	fixturekit.Must(t, refresh.Pass(context.Background()))
	op := fixturekit.ActionOperation(t, repo, fixturekit.Stream, lifecycle.BaseRefreshAction)
	result, err := refresh.Apply(context.Background(), op)
	fixturekit.Must(t, err)
	if result.Outcome != "succeeded" {
		t.Fatalf("base-refresh apply: %+v", result)
	}
	if host.Finds != 1 {
		t.Fatalf("own-PR lookup calls: %d", host.Finds)
	}
	outcome, revision, err := delivery.OwnPullRequestOutcomeAt(repo, fixturekit.Stream)
	fixturekit.Must(t, err)
	if revision != 1 || outcome.PullRequest != 42 || outcome.Outcome != delivery.OwnPullRequestOutcomeClosed {
		t.Fatalf("own-PR outcome %+v revision %d", outcome, revision)
	}
	if _, revision, err := service.TestBaseObservationAt(repo, fixturekit.Stream); err != nil || revision != 0 {
		t.Fatalf("the refresh made a further remote call: base-observation revision %d err %v", revision, err)
	}
	records, err := delivery.Publications(repo, fixturekit.Stream)
	fixturekit.Must(t, err)
	if last := records[len(records)-1]; last.Status != delivery.PublicationOpened {
		t.Fatalf("recording the own-PR outcome changed the publication status to %s", last.Status)
	}
}

func TestBaseRefreshOpenOwnPullRequestContinuesToExistingParentCheck(t *testing.T) {
	t.Parallel()
	cfg, repo, host, refresh := journey.OwnPRFixture(t)
	journey.SealStream(t, repo)
	publication := journey.DeliverWithPullRequest(t, repo, cfg, 42)
	host.Prs = []pulls.PullRequest{{Number: 42, State: "open", Head: publication.Branch, HeadRepository: publication.Fork}}
	fixturekit.Must(t, refresh.Pass(context.Background()))
	op := fixturekit.ActionOperation(t, repo, fixturekit.Stream, lifecycle.BaseRefreshAction)
	result, err := refresh.Apply(context.Background(), op)
	fixturekit.Must(t, err)
	if result.Outcome != "succeeded" {
		t.Fatalf("base-refresh apply: %+v", result)
	}
	if host.Finds != 1 {
		t.Fatalf("own-PR lookup calls: %d", host.Finds)
	}
	if _, revision, err := delivery.OwnPullRequestOutcomeAt(repo, fixturekit.Stream); err != nil || revision != 0 {
		t.Fatalf("an open pull request recorded an outcome: revision %d err %v", revision, err)
	}

	if _, revision, err := service.TestBaseObservationAt(repo, fixturekit.Stream); err != nil || revision != 1 {
		t.Fatalf("the refresh did not continue to its existing parent check: base-observation revision %d err %v", revision, err)
	}
}

func TestBaseRefreshWithNoOwnPullRequestMakesNoLookup(t *testing.T) {
	t.Parallel()
	_, repo, host, refresh := journey.OwnPRFixture(t)
	journey.SealStream(t, repo)
	ctx := context.Background()
	h := trace.Header{Schema: "osmia.trace.transition", Version: trace.Version, ID: intake.DeliveredState, Revision: 1, Project: fixturekit.Project, Workstream: fixturekit.Stream, At: fixturekit.DemoStart, Actor: intake.OwnerActor, Cause: "fixture"}
	_, err := repo.SetFeatureState(ctx, h, intake.DeliveredState, "delivered by merging outside the factory")
	fixturekit.Must(t, err)
	fixturekit.Must(t, refresh.Pass(ctx))
	op := fixturekit.ActionOperation(t, repo, fixturekit.Stream, lifecycle.BaseRefreshAction)
	result, err := refresh.Apply(ctx, op)
	fixturekit.Must(t, err)
	if result.Outcome != "succeeded" {
		t.Fatalf("base-refresh apply: %+v", result)
	}
	if host.Finds != 0 {
		t.Fatalf("a workstream delivered without a pull request made %d own-PR lookups", host.Finds)
	}
	if _, revision, err := service.TestBaseObservationAt(repo, fixturekit.Stream); err != nil || revision != 1 {
		t.Fatalf("the refresh did not continue: base-observation revision %d err %v", revision, err)
	}
}

func TestBaseRefreshRestartBetweenOwnPullRequestLookupAndRecordYieldsOneOutcome(t *testing.T) {
	t.Parallel()
	cfg, repo, host, refresh := journey.OwnPRFixture(t)
	journey.SealStream(t, repo)
	publication := journey.DeliverWithPullRequest(t, repo, cfg, 42)
	host.Prs = []pulls.PullRequest{{Number: 42, Merged: true, State: "closed", Head: publication.Branch, HeadRepository: publication.Fork}}
	fixturekit.Must(t, refresh.Pass(context.Background()))
	op := fixturekit.ActionOperation(t, repo, fixturekit.Stream, lifecycle.BaseRefreshAction)
	refresh.S.Boundary = func(step string) error {
		if step == "own-pr-outcome-found" {
			return errors.New("interrupted")
		}
		return nil
	}
	if _, err := refresh.Apply(context.Background(), op); err == nil {
		t.Fatal("expected the interruption")
	}
	if _, revision, err := delivery.OwnPullRequestOutcomeAt(repo, fixturekit.Stream); err != nil || revision != 0 {
		t.Fatalf("the interrupted attempt recorded an outcome: revision %d err %v", revision, err)
	}
	refresh.S.Boundary = nil

	repo.Close()
	reopened, err := trace.Open(cfg.Root, cfg.Project)
	fixturekit.Must(t, err)
	t.Cleanup(func() { reopened.Close() })
	refresh.Repository = reopened
	result, err := refresh.Apply(context.Background(), op)
	fixturekit.Must(t, err)
	if result.Outcome != "succeeded" {
		t.Fatalf("retry after restart: %+v", result)
	}
	if host.Finds != 2 {
		t.Fatalf("restart did not re-look-up the pull request: %d calls", host.Finds)
	}
	outcome, revision, err := delivery.OwnPullRequestOutcomeAt(reopened, fixturekit.Stream)
	fixturekit.Must(t, err)
	if revision != 1 || outcome.PullRequest != 42 || outcome.Outcome != delivery.OwnPullRequestOutcomeMerged {
		t.Fatalf("own-PR outcome after restart %+v revision %d, want exactly one", outcome, revision)
	}
}
