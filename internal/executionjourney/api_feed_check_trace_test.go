package executionjourney

import (
	"encoding/json"
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
	must(t, err)
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
			started, finished := demoStart, demoStart.Add(time.Minute)
			selection := CheckSelection{Mode: checkrun.SelectionSelected, Links: []string{"dag://a"}, Candidates: 1}
			feedDocument(t, repo, stream, checkSelectionDocument("upload", 1), checkSelectionPath("upload", 1), 1, started, UnitCheckSelection{Unit: "upload", Run: 1, Selection: selection, StartedAt: started, TraceURL: startTrace})
			entries, err := feedEntries(repo, repo, stream)
			must(t, err)
			if len(entries) != 1 || entries[0].UnitChecks.State != "running" || entries[0].UnitChecks.TraceURL != startTrace {
				t.Fatalf("running: %+v", entries)
			}
			feedDocument(t, repo, stream, checkDocumentID("upload", 1), "units/upload/checks-1.json", 1, finished, UnitCheckRun{Unit: "upload", Run: 1, Status: ChecksPassed, Selection: &selection, TraceURL: tc.result, StartedAt: started, FinishedAt: finished})
			entries, err = feedEntries(repo, repo, stream)
			must(t, err)
			if len(entries) != 1 || entries[0].UnitChecks.State != ChecksPassed || entries[0].UnitChecks.TraceURL != tc.want {
				t.Fatalf("finished: %+v", entries)
			}
		})
	}
}

func TestFeedUnitChecksWithoutAnyTraceLinkShowNone(t *testing.T) {
	t.Parallel()
	repo := feedFixtureRepo(t, "fun-")
	started := demoStart
	feedDocument(t, repo, stream, checkSelectionDocument("upload", 1), checkSelectionPath("upload", 1), 1, started, UnitCheckSelection{Unit: "upload", Run: 1, StartedAt: started})
	feedDocument(t, repo, stream, checkDocumentID("upload", 1), "units/upload/checks-1.json", 1, started, UnitCheckRun{Unit: "upload", Run: 1, Status: ChecksPassed, StartedAt: started, FinishedAt: started})
	entries, err := feedEntries(repo, repo, stream)
	must(t, err)
	data, err := json.Marshal(entries)
	must(t, err)
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
			at := demoStart
			feedDocument(t, repo, stream, finalCheckAttemptDocument(1, 1), finalCheckAttemptPath(1, 1), 1, at, FinalCheckAttempt{Review: 1, Attempt: 1, StartedAt: at, TraceURL: startTrace})
			entries, err := feedEntries(repo, repo, stream)
			must(t, err)
			if len(entries) != 1 || entries[0].FinalChecks.State != "running" || entries[0].FinalChecks.TraceURL != startTrace {
				t.Fatalf("running: %+v", entries)
			}
			feedDocument(t, repo, stream, finalChecksDocument(1), finalChecksPath(1), 1, at.Add(time.Minute), FinalCheckRun{Review: 1, Attempt: 1, Status: ChecksPassed, TraceURL: tc.result, StartedAt: at, FinishedAt: at.Add(time.Minute)})
			entries, err = feedEntries(repo, repo, stream)
			must(t, err)
			if len(entries) != 1 || entries[0].FinalChecks.State != ChecksPassed || entries[0].FinalChecks.TraceURL != tc.want {
				t.Fatalf("finished: %+v", entries)
			}
		})
	}
}

func TestFeedFinalChecksWithoutAnyTraceLinkShowNone(t *testing.T) {
	t.Parallel()
	repo := feedFixtureRepo(t, "ffn-")
	at := demoStart
	feedDocument(t, repo, stream, finalCheckAttemptDocument(1, 1), finalCheckAttemptPath(1, 1), 1, at, FinalCheckAttempt{Review: 1, Attempt: 1, StartedAt: at})
	feedDocument(t, repo, stream, finalChecksDocument(1), finalChecksPath(1), 1, at, FinalCheckRun{Review: 1, Attempt: 1, Status: ChecksPassed, StartedAt: at, FinishedAt: at})
	entries, err := feedEntries(repo, repo, stream)
	must(t, err)
	data, err := json.Marshal(entries)
	must(t, err)
	if len(entries) != 1 || strings.Contains(string(data), "trace_url") {
		t.Fatalf("entries: %s", data)
	}
}

func TestFeedSessionChecksShowTheResultTraceLink(t *testing.T) {
	t.Parallel()
	repo := feedFixtureRepo(t, "fsl-")
	at := demoStart
	feedDocument(t, repo, stream, "session-checks-1", "session-checks/checks-1.json", 1, at, SessionCheckRun{Run: 1, Kind: "full", Status: ChecksPassed, TraceURL: resultTrace, StartedAt: at, FinishedAt: at})
	feedDocument(t, repo, stream, "session-checks-2", "session-checks/checks-2.json", 1, at, SessionCheckRun{Run: 2, Kind: "full", Status: ChecksPassed, StartedAt: at, FinishedAt: at})
	entries, err := feedEntries(repo, repo, stream)
	must(t, err)
	if len(entries) != 2 || entries[0].SessionChecks.TraceURL != resultTrace || entries[1].SessionChecks.TraceURL != "" {
		t.Fatalf("entries: %+v", entries)
	}
}
