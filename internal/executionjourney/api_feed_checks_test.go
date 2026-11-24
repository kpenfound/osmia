package executionjourney

import (
	context "context"
	json "encoding/json"
	fmt "fmt"
	filepath "path/filepath"
	reflect "reflect"
	slices "slices"
	strings "strings"
	testing "testing"
	time "time"

	"github.com/kpenfound/osmia/internal/api"
	"github.com/kpenfound/osmia/internal/delivery"
	"github.com/kpenfound/osmia/internal/execution"
	"github.com/kpenfound/osmia/internal/intake"
	"github.com/kpenfound/osmia/internal/service"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	"github.com/kpenfound/osmia/internal/servicetest/journey"

	checkrun "github.com/kpenfound/osmia/internal/checks"
	coreadapter "github.com/kpenfound/osmia/internal/coreadapter"
	trace "github.com/kpenfound/osmia/internal/trace"
)

func TestFeedUnitCheckRunShowsTheSelectionThatRanAfterARefusal(t *testing.T) {
	t.Parallel()
	_, cfg := journey.ConversationFixture(t, "fur-")
	repo, err := trace.Open(cfg.Root, cfg.Project)
	fixturekit.Must(t, err)
	defer func() { repo.Close() }()
	started, finished := fixturekit.DemoStart, fixturekit.DemoStart.Add(time.Minute)
	selected := api.CheckSelection{Mode: checkrun.SelectionSelected, Links: []string{"dag://a"}, Candidates: 3}
	full := checkrun.FullSelection(checkrun.SelectionRefused, "no checks matched pattern")
	fixturekit.FeedDocument(t, repo, fixturekit.Stream, execution.CheckSelectionDocument("upload", 1), execution.CheckSelectionPath("upload", 1), 1, started, execution.UnitCheckSelection{Unit: "upload", Run: 1, Selection: selected, StartedAt: started})
	fixturekit.FeedDocument(t, repo, fixturekit.Stream, execution.CheckDocumentID("upload", 1), "units/upload/checks-1.json", 1, finished, execution.UnitCheckRun{Unit: "upload", Run: 1, Status: checkrun.ChecksPassed, Selection: &full, StartedAt: started, FinishedAt: finished})
	entries, err := service.FeedEntries(repo, repo, fixturekit.Stream)
	fixturekit.Must(t, err)
	if len(entries) != 1 || !reflect.DeepEqual(entries[0].UnitChecks.Selection, service.FeedSelection(&full)) {
		t.Fatalf("entries: %+v", entries)
	}
}

func TestFeedUnitCheckRunsKeepSelectionAndRecordedResultsAcrossRestart(t *testing.T) {
	t.Parallel()
	for _, status := range []string{checkrun.ChecksPassed, checkrun.ChecksFailed, checkrun.ChecksIncomplete, checkrun.ChecksSuperseded} {
		t.Run(status, func(t *testing.T) {
			_, cfg := journey.ConversationFixture(t, "fuc-")
			repo, err := trace.Open(cfg.Root, cfg.Project)
			fixturekit.Must(t, err)
			defer func() { repo.Close() }()
			listed := 115
			selection := api.CheckSelection{Mode: checkrun.SelectionSelected, Links: []string{"dag://a", "dag://b", "dag://c"}, Candidates: 113, Listed: &listed, FallbackPicks: 3}
			if status == checkrun.ChecksSuperseded {
				selection = checkrun.FullSelection("unconfigured", "OPENROUTER_API_KEY is not set")
			}
			started, chosen, finished := fixturekit.DemoStart, fixturekit.DemoStart.Add(time.Minute), fixturekit.DemoStart.Add(2*time.Minute)
			fixturekit.FeedDocument(t, repo, fixturekit.Stream, execution.CheckSelectionDocument("upload", 1), execution.CheckSelectionPath("upload", 1), 1, chosen, execution.UnitCheckSelection{Unit: "upload", Run: 1, Selection: selection, StartedAt: started})
			read := func() []api.FeedEntry {
				t.Helper()
				entries, err := service.FeedEntries(repo, repo, fixturekit.Stream)
				fixturekit.Must(t, err)
				return entries
			}
			before := read()
			if status != checkrun.ChecksSuperseded && (before[0].UnitChecks.Selection.FallbackPicks != 3 || !strings.Contains(before[0].UnitChecks.Selection.Description, "highest-scored checks ran")) {
				t.Fatalf("picks feed: %+v", before[0].UnitChecks.Selection)
			}
			if len(before) != 1 || before[0].At != chosen || before[0].UnitChecks.State != "running" || before[0].UnitChecks.FinishedAt != nil || !reflect.DeepEqual(before[0].UnitChecks.Selection, service.FeedSelection(&selection)) {
				t.Fatalf("running: %+v", before)
			}
			fixturekit.Must(t, repo.Close())
			repo, err = trace.Open(cfg.Root, cfg.Project)
			fixturekit.Must(t, err)
			if after := read(); !reflect.DeepEqual(after, before) {
				t.Fatalf("restart changed running entry: %+v", after)
			}
			run := execution.UnitCheckRun{Unit: "upload", Run: 1, Status: status, Selection: &selection, StartedAt: started, FinishedAt: finished, Output: "captured output is private to trace"}
			if status == checkrun.ChecksPassed {
				run.Flaky = []string{"dag://c"}
			}
			if status == checkrun.ChecksFailed {
				run.Failed = []string{"dag://b"}
			}
			if status == checkrun.ChecksIncomplete {
				run.Error = "runner did not finish"
			}
			if status == checkrun.ChecksSuperseded {
				run.Error = "candidate replaced"
			}
			fixturekit.FeedDocument(t, repo, fixturekit.Stream, execution.CheckDocumentID("upload", 1), "units/upload/checks-1.json", 1, finished, run)
			want := api.FeedEntry{Kind: "unit_checks", At: chosen, UnitChecks: &api.FeedUnitChecks{Unit: "upload", Run: 1, State: status, Selection: service.FeedSelection(&selection), Failed: run.Failed, Flaky: run.Flaky, Error: run.Error, StartedAt: started, FinishedAt: &finished}}
			if after := read(); !reflect.DeepEqual(after, []api.FeedEntry{want}) {
				t.Fatalf("finished: %s; want %+v", describeFeed(after), want.UnitChecks)
			}
			data, err := json.Marshal(read())
			fixturekit.Must(t, err)
			if strings.Contains(string(data), "captured output") {
				t.Fatal("feed contains captured output")
			}
		})
	}
}

func TestFeedUnitCheckSelectionWithoutAListedTotalShowsNone(t *testing.T) {
	t.Parallel()
	_, cfg := journey.ConversationFixture(t, "flu-")
	repo, err := trace.Open(cfg.Root, cfg.Project)
	fixturekit.Must(t, err)
	defer repo.Close()

	selection := api.CheckSelection{Mode: checkrun.SelectionSelected, Links: []string{"dag://a"}, Candidates: 113}
	fixturekit.FeedDocument(t, repo, fixturekit.Stream, execution.CheckDocumentID("upload", 1), "units/upload/checks-1.json", 1, fixturekit.DemoStart, execution.UnitCheckRun{Unit: "upload", Run: 1, Status: checkrun.ChecksPassed, Selection: &selection, StartedAt: fixturekit.DemoStart, FinishedAt: fixturekit.DemoStart})
	fixturekit.FeedDocument(t, repo, fixturekit.Stream, execution.CheckDocumentID("upload", 2), "units/upload/checks-2.json", 1, fixturekit.DemoStart.Add(time.Minute), execution.UnitCheckRun{Unit: "upload", Run: 2, Status: checkrun.ChecksFailed, Failed: []string{"dag://b"}, StartedAt: fixturekit.DemoStart, FinishedAt: fixturekit.DemoStart.Add(time.Minute)})
	entries, err := service.FeedEntries(repo, repo, fixturekit.Stream)
	fixturekit.Must(t, err)
	if len(entries) != 2 {
		t.Fatalf("entries: %+v", entries)
	}
	got := entries[0].UnitChecks.Selection
	if got == nil || got.Selected != 1 || got.Judged != 113 || got.Listed != nil || entries[1].UnitChecks.Selection != nil {
		t.Fatalf("selection without a listed total: %+v", entries)
	}
	data, err := json.Marshal(entries)
	fixturekit.Must(t, err)
	if strings.Contains(string(data), `"listed"`) {
		t.Fatalf("invented total: %s", data)
	}
}

func TestFeedFinalChecksShowEachRecordedResult(t *testing.T) {
	t.Parallel()
	_, cfg := journey.ConversationFixture(t, "ffc-")
	repo, err := trace.Open(cfg.Root, cfg.Project)
	fixturekit.Must(t, err)
	defer func() { repo.Close() }()
	at := func(n int) time.Time { return fixturekit.DemoStart.Add(time.Duration(n) * time.Minute) }
	read := func() []api.FeedEntry {
		t.Helper()
		entries, err := service.FeedEntries(repo, repo, fixturekit.Stream)
		fixturekit.Must(t, err)
		return slices.DeleteFunc(entries, func(e api.FeedEntry) bool { return e.Kind != "final_checks" })
	}
	if entries := read(); len(entries) != 0 {
		t.Fatal("a repository without a check record produced a final check entry")
	}
	result := func(review, revision, minute int, state, reason string) {
		t.Helper()
		run := delivery.FinalCheckRun{Review: review, Status: state, Error: reason, StartedAt: at(minute - 1), FinishedAt: at(minute)}
		if state == checkrun.ChecksFailed {
			run.Failed = []string{"dag://failed"}
			run.Flaky = []string{"dag://flaky"}
		}
		fixturekit.FeedDocument(t, repo, fixturekit.Stream, delivery.FinalChecksDocument(review), delivery.FinalChecksPath(review), revision, at(minute), run)
	}
	result(1, 1, 2, checkrun.ChecksIncomplete, "engine unavailable")
	result(1, 2, 13, checkrun.ChecksPassed, "")
	before := read()
	if len(before) != 2 || before[0].FinalChecks.State != checkrun.ChecksIncomplete || before[0].FinalChecks.Error != "engine unavailable" || before[1].FinalChecks.State != checkrun.ChecksPassed {
		t.Fatalf("retry: %+v", before)
	}
	fixturekit.Must(t, repo.Close())
	repo, err = trace.Open(cfg.Root, cfg.Project)
	fixturekit.Must(t, err)
	if entries := read(); !reflect.DeepEqual(entries, before) {
		t.Fatalf("restart changed the entries: %+v", entries)
	}
	result(2, 1, 14, checkrun.ChecksIncomplete, "export failed")
	result(2, 2, 15, checkrun.ChecksFailed, "")
	got := read()
	states := []string{}
	for _, e := range got {
		states = append(states, e.FinalChecks.State)
	}
	if !reflect.DeepEqual(states, []string{checkrun.ChecksIncomplete, checkrun.ChecksPassed, checkrun.ChecksIncomplete, checkrun.ChecksFailed}) {
		t.Fatalf("result states: %v", states)
	}
	for _, e := range got {
		if e.FinalChecks.Attempt != 0 || e.FinalChecks.StartedAt != nil {
			t.Fatalf("invented start: %+v", e.FinalChecks)
		}
	}
	if !reflect.DeepEqual(got[3].FinalChecks.Failed, []string{"dag://failed"}) || !reflect.DeepEqual(got[3].FinalChecks.Flaky, []string{"dag://flaky"}) {
		t.Fatalf("failed and flaky links: %+v", got[3])
	}
}

type claimedRolesFeed struct {
	entries      []api.FeedEntry
	conversation []api.ConversationEntry
	before       []api.ConversationEntry
	reviewed     api.FinalReport
}

func feedWithEveryClaimedRoleAndTwoFinalReports(t *testing.T) claimedRolesFeed {
	t.Helper()
	_, cfg := journey.ConversationFixture(t, "ffr-")
	repo, err := trace.Open(cfg.Root, cfg.Project)
	fixturekit.Must(t, err)
	defer repo.Close()
	ctx := context.Background()
	for i, role := range []string{intake.MasonRole, intake.CommitteeRole, intake.ArchitectRole, "foreman"} {
		id := fmt.Sprintf("agent_feed_%d", i)
		fixturekit.QueueRoleTurn(t, repo, fixturekit.Stream, id, role, "", fixturekit.DemoStart)
		_, err := repo.ClaimTurn(ctx, fixturekit.Stream, id, "token_"+id, filepath.Join(t.TempDir(), id), fixturekit.DemoStart)
		fixturekit.Must(t, err)
	}
	_, err = repo.EnsureChiefOfStaff(ctx, fixturekit.Stream, fixturekit.DemoStart, intake.ServiceActor)
	fixturekit.Must(t, err)
	_, err = repo.EnqueueTurn(ctx, trace.TurnRequest{Header: trace.Header{Schema: "osmia.trace.turn-request", Version: trace.Version, ID: "owner_turn", Revision: 1, Project: fixturekit.Project, Workstream: fixturekit.Stream, At: fixturekit.DemoStart, Actor: intake.OwnerActor, Cause: "owner-message"}, AgentID: trace.ChiefOfStaff, ThreadID: trace.ChiefOfStaff, TurnID: "message", Profile: coreadapter.Profile{Name: "chief", Backend: "fake", Model: "test"}, Prompt: "Review progress?"})
	fixturekit.Must(t, err)
	_, err = repo.ClaimTurn(ctx, fixturekit.Stream, trace.ChiefOfStaff, "token_chief", filepath.Join(t.TempDir(), "chief"), fixturekit.DemoStart)
	fixturekit.Must(t, err)
	before, err := service.ConversationEntries(repo, fixturekit.Stream)
	fixturekit.Must(t, err)
	reviewed := api.FinalReport{Review: 1, Outcome: delivery.FinalReviewed, Reader: "agent_feed_1", Turn: "agent_feed_1-turn", Findings: []api.FinalFinding{{Severity: delivery.FindingBlocking}, {Severity: delivery.FindingAdvisory}, {Severity: delivery.FindingBlocking}}, Criteria: []api.FinalCriterion{{Criterion: "spec#1", Gap: "uncovered"}, {Criterion: "spec#2", Evidence: "upload"}}}
	fixturekit.FeedDocument(t, repo, fixturekit.Stream, delivery.FinalReportDocument, delivery.FinalReportPath, 1, fixturekit.DemoStart.Add(time.Minute), reviewed)
	fixturekit.FeedDocument(t, repo, fixturekit.Stream, delivery.FinalReportDocument, delivery.FinalReportPath, 2, fixturekit.DemoStart.Add(2*time.Minute), api.FinalReport{Review: 2, Outcome: delivery.FinalFailed, Failure: "rebase failed"})
	entries, err := service.FeedEntries(repo, repo, fixturekit.Stream)
	fixturekit.Must(t, err)
	conversation := []api.ConversationEntry{}
	for _, e := range entries {
		if e.Kind == "message" || e.Kind == "response" || e.Kind == "action" {
			conversation = append(conversation, api.ConversationEntry{Kind: e.Kind, At: e.At, Turn: e.Turn, Text: e.Text, State: e.State})
		}
	}
	return claimedRolesFeed{entries: entries, conversation: conversation, before: before, reviewed: reviewed}
}

func TestFeedShowsASessionForEveryClaimedRole(t *testing.T) {
	t.Parallel()
	feed := feedWithEveryClaimedRoleAndTwoFinalReports(t)
	sessions := []api.FeedSession{}
	for _, e := range feed.entries {
		if e.Kind == "session" {
			sessions = append(sessions, *e.Session)
		}
	}
	if len(sessions) != 5 {
		t.Fatalf("claimed sessions: %+v", sessions)
	}
	for _, s := range sessions {
		if s.Profile == "" || s.Role == "" || s.State != "running" {
			t.Fatalf("session: %+v", s)
		}
	}
	if !reflect.DeepEqual(feed.conversation, feed.before) {
		t.Fatalf("conversation changed: %+v; before %+v", feed.conversation, feed.before)
	}
}

func TestFeedFinalReviewOutcomesNameTheirReader(t *testing.T) {
	t.Parallel()
	feed := feedWithEveryClaimedRoleAndTwoFinalReports(t)
	outcomes := []api.FeedFinalReview{}
	sessions := []api.FeedSession{}
	for _, e := range feed.entries {
		switch e.Kind {
		case "session":
			sessions = append(sessions, *e.Session)
		case "final_review":
			outcomes = append(outcomes, *e.FinalReview)
		}
	}
	want := []api.FeedFinalReview{{Review: 1, State: delivery.FinalReviewed, Reader: feed.reviewed.Reader, Turn: feed.reviewed.Turn, Blocking: 2, Gaps: 1}, {Review: 2, State: delivery.FinalFailed, Failure: "rebase failed"}}
	if !reflect.DeepEqual(outcomes, want) {
		t.Fatalf("outcomes: %+v; want %+v", outcomes, want)
	}
	if !slices.ContainsFunc(sessions, func(s api.FeedSession) bool {
		return s.Agent == outcomes[0].Reader && s.Turn == outcomes[0].Turn && s.Role == intake.CommitteeRole
	}) {
		t.Fatal("review reader has no session entry")
	}
}

func TestEventStreamAnnouncesFeedCheckAndReviewRecords(t *testing.T) {
	t.Parallel()
	s, c := journey.EventTraceFixture(t)
	events, _ := fixturekit.EventStream(t, c)
	if e := fixturekit.NextEvent(t, events); e.Kind != api.EventResync {
		t.Fatalf("initial event: %+v", e)
	}
	repo := s.Sole().Repository
	at := fixturekit.DemoStart
	for _, record := range []struct {
		id, path string
		revision int
		value    any
	}{
		{execution.CheckSelectionDocument("upload", 1), execution.CheckSelectionPath("upload", 1), 1, execution.UnitCheckSelection{Unit: "upload", Run: 1, Selection: checkrun.FullSelection("unconfigured", "OPENROUTER_API_KEY is not set"), StartedAt: at}},
		{execution.CheckDocumentID("upload", 1), "units/upload/checks-1.json", 1, execution.UnitCheckRun{Unit: "upload", Run: 1, Status: checkrun.ChecksPassed, StartedAt: at, FinishedAt: at}},
		{delivery.FinalChecksDocument(1), delivery.FinalChecksPath(1), 1, delivery.FinalCheckRun{Review: 1, Status: checkrun.ChecksIncomplete, Error: "timeout", StartedAt: at, FinishedAt: at}},
		{delivery.FinalChecksDocument(1), delivery.FinalChecksPath(1), 2, delivery.FinalCheckRun{Review: 1, Status: checkrun.ChecksPassed, StartedAt: at, FinishedAt: at}},
		{delivery.FinalReportDocument, delivery.FinalReportPath, 1, api.FinalReport{Review: 1, Outcome: delivery.FinalReviewed}},
	} {
		fixturekit.FeedDocument(t, repo, fixturekit.Stream, record.id, record.path, record.revision, at, record.value)

		fixturekit.AwaitEvents(t, events, api.Event{Kind: api.EventWorkstream, Project: fixturekit.Project, Workstream: fixturekit.Stream}, api.Event{Kind: api.EventInbox, Project: fixturekit.Project, Workstream: fixturekit.Stream})
		at = at.Add(time.Minute)
	}
}

func TestFeedIncludesEveryWorkflowSubject(t *testing.T) {
	t.Parallel()
	_, cfg := journey.ConversationFixture(t, "fts-")
	repo, err := trace.Open(cfg.Root, cfg.Project)
	fixturekit.Must(t, err)
	defer repo.Close()
	subjects := []string{trace.FeatureSubject, trace.UnitSubject("upload"), execution.ChecksSubject("upload"), execution.CheckBlockedSubject("upload"), delivery.FinalReviewSubject, trace.QuestionSubject("1"), "drift-rebase", "seal", "amendment", "publication", "future-subject"}
	var want []api.FeedEntry
	for i, subject := range subjects {
		unit := ""
		if i >= 1 && i <= 3 {
			unit = "upload"
		}
		for j, to := range []string{"pending", "finished"} {
			at := fixturekit.DemoStart.Add(time.Duration(2*i+j) * time.Minute)
			state, err := repo.Workflow(fixturekit.Stream, subject)
			fixturekit.Must(t, err)
			h := trace.Header{Schema: "osmia.trace.transition", Version: trace.Version, ID: fmt.Sprintf("feed-transition-%d-%d", i, j), Revision: 1, Project: fixturekit.Project, Workstream: fixturekit.Stream, Unit: unit, At: at, Actor: intake.ServiceActor, Cause: "feed-test"}
			reason := "reason for " + subject + " " + to
			_, err = repo.Transact(context.Background(), trace.Transaction{ExpectedVersion: state.Version, Transition: trace.Transition{Header: h, Subject: subject, From: state.Value, To: to, Reason: reason}})
			fixturekit.Must(t, err)
			want = append(want, api.FeedEntry{Kind: "transition", At: at, Transition: &api.FeedTransition{Subject: subject, Unit: unit, From: state.Value, To: to, Actor: "service:osmia", Reason: reason}})
		}
	}
	got, err := service.FeedEntries(repo, repo, fixturekit.Stream)
	fixturekit.Must(t, err)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("feed: %s; want: %s", describeFeed(got), describeFeed(want))
	}
}
