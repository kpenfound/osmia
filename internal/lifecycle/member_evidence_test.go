package lifecycle

import (
	"context"
	"testing"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/delivery"
	"github.com/kpenfound/osmia/internal/seal"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	"github.com/kpenfound/osmia/internal/trace"
)

func resolutionDocument(t *testing.T, repo *trace.Repository, stream config.WorkstreamID, id, path string, value any) {
	t.Helper()
	fixturekit.ResolutionDocument(t, repo, stream, id, path, value, demoStart, ownerActor)
}

func recordResolutionMerge(t *testing.T, repo *trace.Repository, stream config.WorkstreamID) {
	t.Helper()
	resolutionDocument(t, repo, stream, mergeDocument, mergePath, DeliveryMerge{Branch: featureBranch(stream), Commit: "merged-feature", Upstream: seal.Base{Remote: "upstream", Branch: "main", Commit: "merged-upstream"}})
}

func TestMemberResolutionRequiresMergeOrCancellation(t *testing.T) {
	for _, tc := range []struct {
		name, state  string
		pr           int
		draft, owner bool
		outcome      ownPullRequestOutcome
		want         bool
	}{
		{name: "abandoned", state: AbandonedState, want: true},
		{name: "cancelled", state: AbandonedState, pr: 42, want: true},
		{name: "owner merge", state: DeliveredState, owner: true, want: true},
		{name: "current merged PR", state: DeliveredState, pr: 42, outcome: ownPullRequestOutcome{PullRequest: 42, Outcome: "merged"}, want: true},
		{name: "ready", state: DeliveredState, pr: 42},
		{name: "draft", state: DeliveredState, pr: 42, draft: true},
		{name: "closed unmerged", state: DeliveredState, pr: 42, outcome: ownPullRequestOutcome{PullRequest: 42, Outcome: "closed"}},
		{name: "earlier PR", state: DeliveredState, pr: 43, outcome: ownPullRequestOutcome{PullRequest: 42, Outcome: "merged"}},
		{name: "no publication", state: DeliveredState},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repo := memberEvidenceFixture(t)
			h := trace.Header{Schema: "osmia.trace.transition", Version: trace.Version, ID: "terminal", Revision: 1, Project: repo.Project(), Workstream: stream, At: demoStart, Actor: ownerActor, Cause: "fixture"}
			_, err := repo.SetFeatureState(context.Background(), h, tc.state, "terminal fixture")
			must(t, err)
			if tc.pr > 0 {
				resolutionDocument(t, repo, stream, publicationDocument, publicationPath, DeliveryPublication{Status: publicationOpened, PullRequest: tc.pr, Draft: tc.draft})
			}
			if tc.owner {
				recordResolutionMerge(t, repo, stream)
			}
			if tc.outcome.Outcome != "" {
				resolutionDocument(t, repo, stream, ownPullRequestOutcomeDocument, ownPullRequestOutcomePath, tc.outcome)
			}
			got, err := Resolved(repo, stream)
			must(t, err)
			if got != tc.want {
				t.Fatalf("resolved=%v want %v", got, tc.want)
			}
			docs, err := trace.Read[trace.Document](repo, stream)
			must(t, err)
			evidence, err := MemberEvidenceFrom(docs)
			must(t, err)
			if (evidence.Resolution(tc.state) != "unresolved") != tc.want {
				t.Fatal("snapshot differs from live resolution")
			}
		})
	}
}

func TestMemberEvidenceUsesCurrentDocumentRevisions(t *testing.T) {
	docs := []trace.Document{
		{Header: trace.Header{ID: publicationDocument, Revision: 2}, Content: `{"status":"prepared"}`},
		{Header: trace.Header{ID: publicationDocument, Revision: 1}, Content: `{"status":"opened","pull_request":42}`},
		{Header: trace.Header{ID: ownPullRequestOutcomeDocument, Revision: 1}, Content: `{"pull_request":42,"outcome":"merged"}`},
	}
	evidence, err := MemberEvidenceFrom(docs)
	must(t, err)
	if evidence.Resolution(DeliveredState) != "unresolved" {
		t.Fatal("an earlier publication retired the member")
	}
	current, err := trace.MemberResolutionFrom(docs)
	must(t, err)
	if evidence.ResolutionEvidence() != current {
		t.Fatal("bundle and service resolution evidence differ")
	}
}

const stream = fixturekit.Stream

var demoStart = fixturekit.DemoStart
var must = fixturekit.Must

const mergePath = delivery.MergePath
const publicationPath = delivery.PublicationPath
const ownPullRequestOutcomePath = delivery.OwnPullRequestOutcomePath

func memberEvidenceFixture(t *testing.T) *trace.Repository {
	t.Helper()
	_, cfg := fixturekit.Conversation(t, "member-resolution-")
	repo, err := trace.Open(cfg.Root, cfg.Project)
	must(t, err)
	t.Cleanup(func() { repo.Close() })
	return repo
}
