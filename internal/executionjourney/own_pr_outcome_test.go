package executionjourney

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/pulls"
	"github.com/kpenfound/osmia/internal/seal"
	"github.com/kpenfound/osmia/internal/service"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	"github.com/kpenfound/osmia/internal/trace"
)

func TestWorkgroupOwnPullRequestObservationWithRecordedBaseIntegration(t *testing.T) {
	cfg, repo, host, refresh := ownPRFixture(t)
	sealStream(t, repo)
	_, err := trace.CreateWorkgroup(context.Background(), repo, []config.WorkstreamID{stream}, demoStart, ownerActor, "fixture")
	must(t, err)
	publication := deliverWithPullRequest(t, repo, cfg, 42)
	resolutionDocument(t, repo, stream, baseObservationDocument, "base-observation.json", baseObservation{Base: seal.Base{Remote: "upstream", Branch: "main", Commit: "integrated"}})
	host.Prs = []pulls.PullRequest{{Number: 42, Merged: true, State: "closed", Head: publication.Branch, HeadRepository: publication.Fork}}
	must(t, refresh.Pass(context.Background()))
	op := actionOperation(t, repo, stream, baseRefreshAction)
	_, err = refresh.Apply(context.Background(), op)
	must(t, err)
	outcome, revision, err := ownPullRequestOutcomeAt(repo, stream)
	must(t, err)
	if host.Finds != 1 || revision != 1 || outcome.PullRequest != 42 || outcome.Outcome != ownPullRequestOutcomeMerged {
		t.Fatalf("integrated base prevented own observation: finds=%d outcome=%+v revision=%d", host.Finds, outcome, revision)
	}
	_, revision, err = baseObservationAt(repo, stream)
	must(t, err)
	if revision != 1 {
		t.Fatal("own observation changed recorded base integration")
	}
}

func TestWorkgroupOwnPullRequestObservationRetriesOnCadenceAndStopsWhenClosed(t *testing.T) {
	for _, based := range []bool{false, true} {
		t.Run(fmt.Sprintf("with-base-%v", based), func(t *testing.T) {
			cfg, repo, host, refresh := ownPRFixture(t)
			if based {
				sealStream(t, repo)
			}
			clock := &fixedClock{At: demoStart}
			refresh.s.options.Reconciliation.Now = clock.Now
			_, err := trace.CreateWorkgroup(context.Background(), repo, []config.WorkstreamID{stream}, demoStart, ownerActor, "fixture")
			must(t, err)
			publication := deliverWithPullRequest(t, repo, cfg, 42)
			host.FindErr = errors.New("provider unavailable")
			ctx := context.Background()
			must(t, refresh.Pass(ctx))
			op := actionOperation(t, repo, stream, baseRefreshAction)
			_, err = refresh.Apply(ctx, op)
			must(t, err)
			if _, revision, err := ownPullRequestOutcomeAt(repo, stream); err != nil || revision != 0 {
				t.Fatalf("failed lookup recorded an outcome: revision=%d err=%v", revision, err)
			}
			clock.Advance(10*time.Minute - time.Nanosecond)
			must(t, refresh.Pass(ctx))
			state, err := repo.Workflow(stream, baseRefreshAction)
			must(t, err)
			if state.Version != 2 || state.Value != "checked" {
				t.Fatal("own lookup retried before the polling interval")
			}
			clock.Advance(time.Nanosecond)
			host.FindErr = nil
			host.Prs = []pulls.PullRequest{{Number: 42, State: "closed", Head: publication.Branch, HeadRepository: publication.Fork}}
			must(t, refresh.Pass(ctx))
			op = actionOperation(t, repo, stream, baseRefreshAction)
			_, err = refresh.Apply(ctx, op)
			must(t, err)
			outcome, revision, err := ownPullRequestOutcomeAt(repo, stream)
			must(t, err)
			if host.Finds != 2 || revision != 1 || outcome.Outcome != ownPullRequestOutcomeClosed {
				t.Fatalf("next cadence did not observe closure: finds=%d outcome=%+v revision=%d", host.Finds, outcome, revision)
			}
			clock.Advance(10 * time.Minute)
			must(t, refresh.Pass(ctx))
			state, err = repo.Workflow(stream, baseRefreshAction)
			must(t, err)
			if state.Version != 4 || state.Value != "checked" {
				t.Fatal("closed PR scheduled another lookup")
			}
			done, err := resolved(repo, stream)
			must(t, err)
			if done {
				t.Fatal("closed unmerged PR resolved the member")
			}
		})
	}
}

// ownPRFixture opens a fresh trace repository holding workstreams stream
// and quiet, and a fake pull request host counting its own calls.
func ownPRFixture(t *testing.T) (*config.Config, *trace.Repository, *fakePulls, *baseRefresher) {
	t.Helper()
	_, cfg := conversationFixture(t, "own-pr-")
	repo, err := trace.Open(cfg.Root, cfg.Project)
	must(t, err)
	t.Cleanup(func() { repo.Close() })
	host := &fakePulls{}
	s := &Service{cfg: cfg, projects: []*activeProject{runtimeFor(repo)}, options: Options{Options: service.Options{PullRequests: host}}}
	return cfg, repo, host, &baseRefresher{s: s, repository: repo}
}

// sealStream records stream sealed and depending on quiet, so Pass
// schedules its base-refresh.
func sealStream(t *testing.T, repo *trace.Repository) {
	t.Helper()
	fixturekit.SealDependentStream(t, repo, stream, quiet, featureBranch(stream), foremanActor)
}

// deliverWithPullRequest records stream delivered with one open pull
// request of its own, at the given number.
func deliverWithPullRequest(t *testing.T, repo *trace.Repository, cfg *config.Config, pr int) DeliveryPublication {
	t.Helper()
	return fixturekit.RecordDeliveredPublication(t, repo, cfg, stream, featureBranch(stream), pr, foremanActor)
}

func TestBaseRefreshRecordsMergedOwnPullRequest(t *testing.T) {
	t.Parallel()
	cfg, repo, host, refresh := ownPRFixture(t)
	sealStream(t, repo)
	publication := deliverWithPullRequest(t, repo, cfg, 42)
	host.Prs = []pulls.PullRequest{{Number: 42, Merged: true, State: "closed", Head: publication.Branch, HeadRepository: publication.Fork}}
	must(t, refresh.Pass(context.Background()))
	op := actionOperation(t, repo, stream, baseRefreshAction)
	result, err := refresh.Apply(context.Background(), op)
	must(t, err)
	if result.Outcome != "succeeded" {
		t.Fatalf("base-refresh apply: %+v", result)
	}
	if host.Finds != 1 {
		t.Fatalf("own-PR lookup calls: %d", host.Finds)
	}
	outcome, revision, err := ownPullRequestOutcomeAt(repo, stream)
	must(t, err)
	if revision != 1 || outcome.PullRequest != 42 || outcome.Outcome != ownPullRequestOutcomeMerged {
		t.Fatalf("own-PR outcome %+v revision %d", outcome, revision)
	}
	if _, revision, err := baseObservationAt(repo, stream); err != nil || revision != 0 {
		t.Fatalf("the refresh made a further remote call: base-observation revision %d err %v", revision, err)
	}
	finished, err := ownPullRequestFinished(repo, stream)
	must(t, err)
	if !finished {
		t.Fatal("the recorded merged outcome does not report the workstream's own PR finished")
	}
	records, err := publications(repo, stream)
	must(t, err)
	if last := records[len(records)-1]; last.Status != publicationOpened {
		t.Fatalf("recording the own-PR outcome changed the publication status to %s", last.Status)
	}
}

func TestBaseRefreshRecordsClosedOwnPullRequest(t *testing.T) {
	t.Parallel()
	cfg, repo, host, refresh := ownPRFixture(t)
	sealStream(t, repo)
	publication := deliverWithPullRequest(t, repo, cfg, 42)
	host.Prs = []pulls.PullRequest{{Number: 42, State: "closed", Head: publication.Branch, HeadRepository: publication.Fork}}
	must(t, refresh.Pass(context.Background()))
	op := actionOperation(t, repo, stream, baseRefreshAction)
	result, err := refresh.Apply(context.Background(), op)
	must(t, err)
	if result.Outcome != "succeeded" {
		t.Fatalf("base-refresh apply: %+v", result)
	}
	if host.Finds != 1 {
		t.Fatalf("own-PR lookup calls: %d", host.Finds)
	}
	outcome, revision, err := ownPullRequestOutcomeAt(repo, stream)
	must(t, err)
	if revision != 1 || outcome.PullRequest != 42 || outcome.Outcome != ownPullRequestOutcomeClosed {
		t.Fatalf("own-PR outcome %+v revision %d", outcome, revision)
	}
	if _, revision, err := baseObservationAt(repo, stream); err != nil || revision != 0 {
		t.Fatalf("the refresh made a further remote call: base-observation revision %d err %v", revision, err)
	}
	records, err := publications(repo, stream)
	must(t, err)
	if last := records[len(records)-1]; last.Status != publicationOpened {
		t.Fatalf("recording the own-PR outcome changed the publication status to %s", last.Status)
	}
}

func TestBaseRefreshOpenOwnPullRequestContinuesToExistingParentCheck(t *testing.T) {
	t.Parallel()
	cfg, repo, host, refresh := ownPRFixture(t)
	sealStream(t, repo)
	publication := deliverWithPullRequest(t, repo, cfg, 42)
	host.Prs = []pulls.PullRequest{{Number: 42, State: "open", Head: publication.Branch, HeadRepository: publication.Fork}}
	must(t, refresh.Pass(context.Background()))
	op := actionOperation(t, repo, stream, baseRefreshAction)
	result, err := refresh.Apply(context.Background(), op)
	must(t, err)
	if result.Outcome != "succeeded" {
		t.Fatalf("base-refresh apply: %+v", result)
	}
	if host.Finds != 1 {
		t.Fatalf("own-PR lookup calls: %d", host.Finds)
	}
	if _, revision, err := ownPullRequestOutcomeAt(repo, stream); err != nil || revision != 0 {
		t.Fatalf("an open pull request recorded an outcome: revision %d err %v", revision, err)
	}
	// resolveBase runs and completes the refresh exactly as it did before
	// this check existed: it is the refresh's existing parent check.
	if _, revision, err := baseObservationAt(repo, stream); err != nil || revision != 1 {
		t.Fatalf("the refresh did not continue to its existing parent check: base-observation revision %d err %v", revision, err)
	}
}

func TestBaseRefreshWithNoOwnPullRequestMakesNoLookup(t *testing.T) {
	t.Parallel()
	_, repo, host, refresh := ownPRFixture(t)
	sealStream(t, repo)
	ctx := context.Background()
	h := trace.Header{Schema: "osmia.trace.transition", Version: trace.Version, ID: DeliveredState, Revision: 1, Project: project, Workstream: stream, At: demoStart, Actor: ownerActor, Cause: "fixture"}
	_, err := repo.SetFeatureState(ctx, h, DeliveredState, "delivered by merging outside the factory")
	must(t, err)
	must(t, refresh.Pass(ctx))
	op := actionOperation(t, repo, stream, baseRefreshAction)
	result, err := refresh.Apply(ctx, op)
	must(t, err)
	if result.Outcome != "succeeded" {
		t.Fatalf("base-refresh apply: %+v", result)
	}
	if host.Finds != 0 {
		t.Fatalf("a workstream delivered without a pull request made %d own-PR lookups", host.Finds)
	}
	if _, revision, err := baseObservationAt(repo, stream); err != nil || revision != 1 {
		t.Fatalf("the refresh did not continue: base-observation revision %d err %v", revision, err)
	}
}

func TestBaseRefreshRestartBetweenOwnPullRequestLookupAndRecordYieldsOneOutcome(t *testing.T) {
	t.Parallel()
	cfg, repo, host, refresh := ownPRFixture(t)
	sealStream(t, repo)
	publication := deliverWithPullRequest(t, repo, cfg, 42)
	host.Prs = []pulls.PullRequest{{Number: 42, Merged: true, State: "closed", Head: publication.Branch, HeadRepository: publication.Fork}}
	must(t, refresh.Pass(context.Background()))
	op := actionOperation(t, repo, stream, baseRefreshAction)
	refresh.s.boundary = func(step string) error {
		if step == "own-pr-outcome-found" {
			return errors.New("interrupted")
		}
		return nil
	}
	if _, err := refresh.Apply(context.Background(), op); err == nil {
		t.Fatal("expected the interruption")
	}
	if _, revision, err := ownPullRequestOutcomeAt(repo, stream); err != nil || revision != 0 {
		t.Fatalf("the interrupted attempt recorded an outcome: revision %d err %v", revision, err)
	}
	refresh.s.boundary = nil
	// A restart reopens the repository; the next attempt re-looks the pull
	// request up.
	repo.Close()
	reopened, err := trace.Open(cfg.Root, cfg.Project)
	must(t, err)
	t.Cleanup(func() { reopened.Close() })
	refresh.repository = reopened
	result, err := refresh.Apply(context.Background(), op)
	must(t, err)
	if result.Outcome != "succeeded" {
		t.Fatalf("retry after restart: %+v", result)
	}
	if host.Finds != 2 {
		t.Fatalf("restart did not re-look-up the pull request: %d calls", host.Finds)
	}
	outcome, revision, err := ownPullRequestOutcomeAt(reopened, stream)
	must(t, err)
	if revision != 1 || outcome.PullRequest != 42 || outcome.Outcome != ownPullRequestOutcomeMerged {
		t.Fatalf("own-PR outcome after restart %+v revision %d, want exactly one", outcome, revision)
	}
}
