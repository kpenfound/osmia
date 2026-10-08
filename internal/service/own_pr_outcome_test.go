package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/pulls"
	"github.com/kpenfound/osmia/internal/seal"
	"github.com/kpenfound/osmia/internal/shed"
	"github.com/kpenfound/osmia/internal/trace"
)

// ownPRFixture opens a fresh trace repository holding workstreams stream
// and quiet, and a fake pull request host counting its own calls.
func ownPRFixture(t *testing.T) (*config.Config, *trace.Repository, *fakePulls, *baseRefresher) {
	t.Helper()
	_, cfg := conversationFixture(t, "own-pr-")
	repo, err := trace.Open(cfg.Root, cfg.Project)
	must(t, err)
	t.Cleanup(func() { repo.Close() })
	host := &fakePulls{}
	s := &Service{cfg: cfg, projects: []*activeProject{runtimeFor(repo)}, options: Options{PullRequests: host}}
	return cfg, repo, host, &baseRefresher{s: s, repository: repo}
}

// sealStream records stream sealed and depending on quiet, so Pass
// schedules its base-refresh.
func sealStream(t *testing.T, repo *trace.Repository) {
	t.Helper()
	ctx := context.Background()
	_, err := repo.SetWorkstreamBase(ctx, stream, quiet, 0, demoStart)
	must(t, err)
	sealed := seal.Seal{Version: 1, Seal: 1, Round: 1, Revision: shed.Pin{Spec: 1, Plan: 1}, SpecHash: seal.SpecHash("spec"), Base: seal.Base{Remote: "upstream", Branch: "main", Commit: "upstream-sha"}, Branch: featureBranch(stream), Workspace: "/owned/feature"}
	data, err := seal.Encode(sealed)
	must(t, err)
	h := trace.Header{Schema: "osmia.trace.document", Version: trace.Version, ID: seal.DocumentID, Revision: 1, Project: project, Workstream: stream, At: demoStart, Actor: foremanActor, Cause: "seal"}
	must(t, repo.RecordDocuments(ctx, []trace.Document{{Header: h, Path: seal.Path, Content: string(data)}}))
}

// deliverWithPullRequest records stream delivered with one open pull
// request of its own, at the given number.
func deliverWithPullRequest(t *testing.T, repo *trace.Repository, cfg *config.Config, pr int) DeliveryPublication {
	t.Helper()
	ctx := context.Background()
	record := DeliveryPublication{Status: publicationOpened, Upstream: cfg.Project.Upstream, Fork: cfg.Project.PushRepository(), Base: cfg.Project.BaseBranch, Branch: featureBranch(stream), Commit: "feature-tip", PullRequest: pr, URL: fmt.Sprintf("https://example.invalid/pull/%d", pr)}
	data, err := json.Marshal(record)
	must(t, err)
	h := trace.Header{Schema: "osmia.trace.document", Version: trace.Version, ID: publicationDocument, Revision: 1, Project: repo.Project(), Workstream: stream, At: demoStart, Actor: foremanActor, Cause: "fixture"}
	must(t, repo.RecordDocuments(ctx, []trace.Document{{Header: h, Path: publicationPath, Content: string(data)}}))
	h.Schema, h.ID = "osmia.trace.transition", DeliveredState
	_, err = repo.SetFeatureState(ctx, h, DeliveredState, "delivered for fixture")
	must(t, err)
	return record
}

func TestBaseRefreshRecordsMergedOwnPullRequest(t *testing.T) {
	t.Parallel()
	cfg, repo, host, refresh := ownPRFixture(t)
	sealStream(t, repo)
	publication := deliverWithPullRequest(t, repo, cfg, 42)
	host.prs = []pulls.PullRequest{{Number: 42, Merged: true, State: "closed", Head: publication.Branch, HeadRepository: publication.Fork}}
	must(t, refresh.Pass(context.Background()))
	op := actionOperation(t, repo, stream, baseRefreshAction)
	result, err := refresh.Apply(context.Background(), op)
	must(t, err)
	if result.Outcome != "succeeded" {
		t.Fatalf("base-refresh apply: %+v", result)
	}
	if host.finds != 1 {
		t.Fatalf("own-PR lookup calls: %d", host.finds)
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
	host.prs = []pulls.PullRequest{{Number: 42, State: "closed", Head: publication.Branch, HeadRepository: publication.Fork}}
	must(t, refresh.Pass(context.Background()))
	op := actionOperation(t, repo, stream, baseRefreshAction)
	result, err := refresh.Apply(context.Background(), op)
	must(t, err)
	if result.Outcome != "succeeded" {
		t.Fatalf("base-refresh apply: %+v", result)
	}
	if host.finds != 1 {
		t.Fatalf("own-PR lookup calls: %d", host.finds)
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
	host.prs = []pulls.PullRequest{{Number: 42, State: "open", Head: publication.Branch, HeadRepository: publication.Fork}}
	must(t, refresh.Pass(context.Background()))
	op := actionOperation(t, repo, stream, baseRefreshAction)
	result, err := refresh.Apply(context.Background(), op)
	must(t, err)
	if result.Outcome != "succeeded" {
		t.Fatalf("base-refresh apply: %+v", result)
	}
	if host.finds != 1 {
		t.Fatalf("own-PR lookup calls: %d", host.finds)
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
	if host.finds != 0 {
		t.Fatalf("a workstream delivered without a pull request made %d own-PR lookups", host.finds)
	}
	if _, revision, err := baseObservationAt(repo, stream); err != nil || revision != 1 {
		t.Fatalf("the refresh did not continue: base-observation revision %d err %v", revision, err)
	}
}

func TestOwnPullRequestOutcomeIgnoresAnEarlierPullRequestNumber(t *testing.T) {
	t.Parallel()
	cfg, repo, _, _ := ownPRFixture(t)
	deliverWithPullRequest(t, repo, cfg, 7)
	must(t, recordOwnPullRequestOutcome(context.Background(), repo, demoStart, stream, "fixture", 3, ownPullRequestOutcomeClosed))
	finished, err := ownPullRequestFinished(repo, stream)
	must(t, err)
	if finished {
		t.Fatal("an outcome recorded for an earlier pull request counted for the workstream's current pull request")
	}
}

func TestBaseRefreshRestartBetweenOwnPullRequestLookupAndRecordYieldsOneOutcome(t *testing.T) {
	t.Parallel()
	cfg, repo, host, refresh := ownPRFixture(t)
	sealStream(t, repo)
	publication := deliverWithPullRequest(t, repo, cfg, 42)
	host.prs = []pulls.PullRequest{{Number: 42, Merged: true, State: "closed", Head: publication.Branch, HeadRepository: publication.Fork}}
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
	if host.finds != 2 {
		t.Fatalf("restart did not re-look-up the pull request: %d calls", host.finds)
	}
	outcome, revision, err := ownPullRequestOutcomeAt(reopened, stream)
	must(t, err)
	if revision != 1 || outcome.PullRequest != 42 || outcome.Outcome != ownPullRequestOutcomeMerged {
		t.Fatalf("own-PR outcome after restart %+v revision %d, want exactly one", outcome, revision)
	}
}
