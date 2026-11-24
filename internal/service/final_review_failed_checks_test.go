package service

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/kpenfound/osmia/internal/api"
	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/delivery"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	"github.com/kpenfound/osmia/internal/servicetest/intakefixture"
	"github.com/kpenfound/osmia/internal/trace"
)

func recordFinalChecks(t *testing.T, repository *trace.Repository, stream config.WorkstreamID, k int, run delivery.FinalCheckRun) {
	t.Helper()
	run.Review, run.Operation = k, fmt.Sprintf("final-operation-%d", k)
	content, err := json.MarshalIndent(run, "", "  ")
	intakefixture.Must(t, err)
	intakefixture.Must(t, repository.RecordDocuments(context.Background(), []trace.Document{{Header: trace.Header{Schema: "osmia.trace.document", Version: trace.Version, ID: delivery.FinalChecksDocument(k), Revision: 1, Project: repository.Project(), Workstream: stream, At: fixturekit.DemoStart, Actor: serviceActor, Cause: run.Operation},
		Path: delivery.FinalChecksPath(k), Content: string(content) + "\n"}}))
}

func finalReviewNotice(t *testing.T, repository *trace.Repository, stream config.WorkstreamID, k int) string {
	t.Helper()
	entries, err := repository.Outbox(stream)
	intakefixture.Must(t, err)
	for _, e := range entries {
		if e.TransitionID == fmt.Sprintf("final-review-%d-reviewed", k) {
			return e.Event.Body
		}
	}
	t.Fatalf("no notice for final review %d", k)
	return ""
}

// The notice of a final review whose recorded check result failed lists each
// failed check with its command, whatever the reader's findings say; any other
// result adds nothing.
func TestFinalReviewNoticeListsTheFailedChecksOfTheRecordedResult(t *testing.T) {
	t.Parallel()
	f, stream, repository, report := deliveryFixture(t)
	const traceID = "4e3a3896a86e28f2204157939fb07b9f"
	recordFinalChecks(t, repository, stream, 2, delivery.FinalCheckRun{Commit: report.Commit, Source: api.CheckSourcePullRequest, PullRequest: 7, Push: 1, PushedCommit: "abc", Status: "failed", ExitCode: 1,
		Counts: &api.CheckCounts{Total: 9, Passed: 6, Failed: 3}, Failed: []string{"build", "load", "test:a"},
		FailedChecks: []api.CheckFailure{{Name: "build", TargetURL: "https://dagger.cloud/acme/checks/github.com/acme/widgets@abc?check=build"}, {Name: "load", TraceID: "loadtrace1"}, {Name: "test:a", TraceID: traceID}}})
	recordFinalReview(t, f, repository, stream, report, 2)
	notice := finalReviewNotice(t, repository, stream, 2)
	for _, want := range []string{"- test:a: `dagger trace " + traceID + "`", "- load: `dagger trace loadtrace1`", "- build: Dagger Cloud posted no trace id for this check; status: https://dagger.cloud/acme/checks/github.com/acme/widgets@abc?check=build"} {
		if !strings.Contains(notice, want) {
			t.Errorf("the notice lacks %q:\n%s", want, notice)
		}
	}

	recordFinalChecks(t, repository, stream, 3, delivery.FinalCheckRun{Commit: report.Commit, Source: api.CheckSourcePullRequest, PullRequest: 7, Push: 1, PushedCommit: "abc", Status: "passed"})
	recordFinalReview(t, f, repository, stream, report, 3)
	if notice := finalReviewNotice(t, repository, stream, 3); strings.Contains(notice, "dagger trace") || strings.Contains(notice, "failed, recorded in") {
		t.Errorf("a passed result added failed checks:\n%s", notice)
	}
}
