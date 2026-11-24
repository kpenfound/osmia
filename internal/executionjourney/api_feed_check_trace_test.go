package executionjourney

import (
	"encoding/json"
	"github.com/kpenfound/osmia/internal/api"
	"github.com/kpenfound/osmia/internal/delivery"
	"github.com/kpenfound/osmia/internal/execution"
	"github.com/kpenfound/osmia/internal/service"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	"strings"
	"testing"
	"time"

	checkrun "github.com/kpenfound/osmia/internal/checks"
	"github.com/kpenfound/osmia/internal/trace"
)

const (
	startTrace  = "https://dagger.cloud/example/traces/start111"
	resultTrace = "https://dagger.cloud/example/traces/result222"
)

func feedFixtureRepo(t *testing.T, prefix string) *trace.Repository {
	t.Helper()
	_, cfg := conversationFixture(t, prefix)
	repo, err := trace.Open(cfg.Root, cfg.Project)
	fixturekit.Must(t, err)
	t.Cleanup(func() { repo.Close() })
	return repo
}

func TestFeedUnitChecksCarryTheStartRecordTraceLinkUntilAResultHasOne(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct{ result, want string }{
		"start link kept": {"", startTrace},
		"result link won": {resultTrace, resultTrace},
	} {
		t.Run(name, func(t *testing.T) {
			repo := feedFixtureRepo(t, "fut-")
			started, finished := fixturekit.DemoStart, fixturekit.DemoStart.Add(time.Minute)
			selection := api.CheckSelection{Mode: checkrun.SelectionSelected, Links: []string{"dag://a"}, Candidates: 1}
			fixturekit.FeedDocument(t, repo, fixturekit.Stream, execution.CheckSelectionDocument("upload", 1), execution.CheckSelectionPath("upload", 1), 1, started, execution.UnitCheckSelection{Unit: "upload", Run: 1, Selection: selection, StartedAt: started, TraceURL: startTrace})
			entries, err := service.TestFeedEntries(repo, repo, fixturekit.Stream)
			fixturekit.Must(t, err)
			if len(entries) != 1 || entries[0].UnitChecks.State != "running" || entries[0].UnitChecks.TraceURL != startTrace {
				t.Fatalf("running: %+v", entries)
			}
			fixturekit.FeedDocument(t, repo, fixturekit.Stream, execution.CheckDocumentID("upload", 1), "units/upload/checks-1.json", 1, finished, execution.UnitCheckRun{Unit: "upload", Run: 1, Status: checkrun.ChecksPassed, Selection: &selection, TraceURL: tc.result, StartedAt: started, FinishedAt: finished})
			entries, err = service.TestFeedEntries(repo, repo, fixturekit.Stream)
			fixturekit.Must(t, err)
			if len(entries) != 1 || entries[0].UnitChecks.State != checkrun.ChecksPassed || entries[0].UnitChecks.TraceURL != tc.want {
				t.Fatalf("finished: %+v", entries)
			}
		})
	}
}

func TestFeedUnitChecksWithoutAnyTraceLinkShowNone(t *testing.T) {
	t.Parallel()
	repo := feedFixtureRepo(t, "fun-")
	started := fixturekit.DemoStart
	fixturekit.FeedDocument(t, repo, fixturekit.Stream, execution.CheckSelectionDocument("upload", 1), execution.CheckSelectionPath("upload", 1), 1, started, execution.UnitCheckSelection{Unit: "upload", Run: 1, StartedAt: started})
	fixturekit.FeedDocument(t, repo, fixturekit.Stream, execution.CheckDocumentID("upload", 1), "units/upload/checks-1.json", 1, started, execution.UnitCheckRun{Unit: "upload", Run: 1, Status: checkrun.ChecksPassed, StartedAt: started, FinishedAt: started})
	entries, err := service.TestFeedEntries(repo, repo, fixturekit.Stream)
	fixturekit.Must(t, err)
	data, err := json.Marshal(entries)
	fixturekit.Must(t, err)
	if len(entries) != 1 || strings.Contains(string(data), "trace_url") {
		t.Fatalf("entries: %s", data)
	}
}

func TestFeedFinalChecksCarryTheAttemptStartTraceLinkUntilAResultHasOne(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct{ result, want string }{
		"start link kept": {"", startTrace},
		"result link won": {resultTrace, resultTrace},
	} {
		t.Run(name, func(t *testing.T) {
			repo := feedFixtureRepo(t, "ffl-")
			at := fixturekit.DemoStart
			fixturekit.FeedDocument(t, repo, fixturekit.Stream, delivery.FinalCheckAttemptDocument(1, 1), delivery.FinalCheckAttemptPath(1, 1), 1, at, delivery.FinalCheckAttempt{Review: 1, Attempt: 1, StartedAt: at, TraceURL: startTrace})
			entries, err := service.TestFeedEntries(repo, repo, fixturekit.Stream)
			fixturekit.Must(t, err)
			if len(entries) != 1 || entries[0].FinalChecks.State != "running" || entries[0].FinalChecks.TraceURL != startTrace {
				t.Fatalf("running: %+v", entries)
			}
			fixturekit.FeedDocument(t, repo, fixturekit.Stream, delivery.FinalChecksDocument(1), delivery.FinalChecksPath(1), 1, at.Add(time.Minute), delivery.FinalCheckRun{Review: 1, Attempt: 1, Status: checkrun.ChecksPassed, TraceURL: tc.result, StartedAt: at, FinishedAt: at.Add(time.Minute)})
			entries, err = service.TestFeedEntries(repo, repo, fixturekit.Stream)
			fixturekit.Must(t, err)
			if len(entries) != 1 || entries[0].FinalChecks.State != checkrun.ChecksPassed || entries[0].FinalChecks.TraceURL != tc.want {
				t.Fatalf("finished: %+v", entries)
			}
		})
	}
}

func TestFeedFinalChecksWithoutAnyTraceLinkShowNone(t *testing.T) {
	t.Parallel()
	repo := feedFixtureRepo(t, "ffn-")
	at := fixturekit.DemoStart
	fixturekit.FeedDocument(t, repo, fixturekit.Stream, delivery.FinalCheckAttemptDocument(1, 1), delivery.FinalCheckAttemptPath(1, 1), 1, at, delivery.FinalCheckAttempt{Review: 1, Attempt: 1, StartedAt: at})
	fixturekit.FeedDocument(t, repo, fixturekit.Stream, delivery.FinalChecksDocument(1), delivery.FinalChecksPath(1), 1, at, delivery.FinalCheckRun{Review: 1, Attempt: 1, Status: checkrun.ChecksPassed, StartedAt: at, FinishedAt: at})
	entries, err := service.TestFeedEntries(repo, repo, fixturekit.Stream)
	fixturekit.Must(t, err)
	data, err := json.Marshal(entries)
	fixturekit.Must(t, err)
	if len(entries) != 1 || strings.Contains(string(data), "trace_url") {
		t.Fatalf("entries: %s", data)
	}
}

func TestFeedSessionChecksShowTheResultTraceLink(t *testing.T) {
	t.Parallel()
	repo := feedFixtureRepo(t, "fsl-")
	at := fixturekit.DemoStart
	fixturekit.FeedDocument(t, repo, fixturekit.Stream, "session-checks-1", "session-checks/checks-1.json", 1, at, api.SessionCheckRun{Run: 1, Kind: "full", Status: checkrun.ChecksPassed, TraceURL: resultTrace, StartedAt: at, FinishedAt: at})
	fixturekit.FeedDocument(t, repo, fixturekit.Stream, "session-checks-2", "session-checks/checks-2.json", 1, at, api.SessionCheckRun{Run: 2, Kind: "full", Status: checkrun.ChecksPassed, StartedAt: at, FinishedAt: at})
	entries, err := service.TestFeedEntries(repo, repo, fixturekit.Stream)
	fixturekit.Must(t, err)
	if len(entries) != 2 || entries[0].SessionChecks.TraceURL != resultTrace || entries[1].SessionChecks.TraceURL != "" {
		t.Fatalf("entries: %+v", entries)
	}
}

func TestFeedSessionChecksShowUnfinishedEvidenceUntilAResultSupersedesIt(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct{ result, want string }{
		"evidence link kept": {"", startTrace},
		"result link won":    {resultTrace, resultTrace},
	} {
		t.Run(name, func(t *testing.T) {
			repo := feedFixtureRepo(t, "fse-")
			at := fixturekit.DemoStart
			fixturekit.FeedDocument(t, repo, fixturekit.Stream, "session-check-1-started", "session-checks/checks-1-started.json", 1, at, map[string]any{"run": 1, "commit": "abc", "kind": "full", "trace_url": startTrace, "started_at": at})
			entries, err := service.TestFeedEntries(repo, repo, fixturekit.Stream)
			fixturekit.Must(t, err)
			if len(entries) != 1 || entries[0].SessionChecks.State != "running" || entries[0].SessionChecks.TraceURL != startTrace || entries[0].SessionChecks.FinishedAt != nil {
				t.Fatalf("unfinished: %+v", entries)
			}
			finished := at.Add(time.Minute)
			fixturekit.FeedDocument(t, repo, fixturekit.Stream, "session-check-1", "session-checks/checks-1.json", 1, finished, api.SessionCheckRun{Run: 1, Commit: "abc", Kind: "full", Status: checkrun.ChecksPassed, TraceURL: tc.result, StartedAt: at, FinishedAt: finished})
			entries, err = service.TestFeedEntries(repo, repo, fixturekit.Stream)
			fixturekit.Must(t, err)
			if len(entries) != 1 || entries[0].SessionChecks.State != checkrun.ChecksPassed || entries[0].SessionChecks.TraceURL != tc.want || entries[0].SessionChecks.FinishedAt == nil {
				t.Fatalf("finished: %+v", entries)
			}
		})
	}
}
