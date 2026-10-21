package executionjourney

import (
	context "context"
	json "encoding/json"
	fmt "fmt"
	checkrun "github.com/kpenfound/osmia/internal/checks"
	coreadapter "github.com/kpenfound/osmia/internal/coreadapter"
	trace "github.com/kpenfound/osmia/internal/trace"
	filepath "path/filepath"
	reflect "reflect"
	slices "slices"
	strings "strings"
	testing "testing"
	time "time"
)

func TestFeedUnitCheckRunsKeepSelectionAndRecordedResultsAcrossRestart(t *testing.T) {
	t.Parallel()
	for _, status := range []string{ChecksPassed, ChecksFailed, ChecksIncomplete, ChecksSuperseded} {
		t.Run(status, func(t *testing.T) {
			_, cfg := conversationFixture(t, "fuc-")
			repo, err := trace.Open(cfg.Root, cfg.Project)
			must(t, err)
			defer func() { repo.Close() }()
			listed := 115
			selection := CheckSelection{Mode: checkrun.SelectionSelected, Links: []string{"dag://a", "dag://b", "dag://c"}, Candidates: 113, Listed: &listed}
			if status == ChecksSuperseded {
				selection = checkrun.FullSelection("disabled", "the boost is off")
			}
			started, chosen, finished := demoStart, demoStart.Add(time.Minute), demoStart.Add(2*time.Minute)
			feedDocument(t, repo, stream, checkSelectionDocument("upload", 1), checkSelectionPath("upload", 1), 1, chosen, UnitCheckSelection{Unit: "upload", Run: 1, Selection: selection, StartedAt: started})
			read := func() []FeedEntry {
				t.Helper()
				entries, err := feedEntries(repo, repo, stream)
				must(t, err)
				return entries
			}
			before := read()
			if len(before) != 1 || before[0].At != chosen || before[0].UnitChecks.State != "running" || before[0].UnitChecks.FinishedAt != nil || !reflect.DeepEqual(before[0].UnitChecks.Selection, feedSelection(&selection)) {
				t.Fatalf("running: %+v", before)
			}
			must(t, repo.Close())
			repo, err = trace.Open(cfg.Root, cfg.Project)
			must(t, err)
			if after := read(); !reflect.DeepEqual(after, before) {
				t.Fatalf("restart changed running entry: %+v", after)
			}
			run := UnitCheckRun{Unit: "upload", Run: 1, Status: status, Selection: &selection, StartedAt: started, FinishedAt: finished, Output: "captured output is private to trace"}
			if status == ChecksFailed {
				run.Failed = []string{"dag://b"}
			}
			if status == ChecksIncomplete {
				run.Error = "runner did not finish"
			}
			if status == ChecksSuperseded {
				run.Error = "candidate replaced"
			}
			feedDocument(t, repo, stream, checkDocumentID("upload", 1), "units/upload/checks-1.json", 1, finished, run)
			want := FeedEntry{Kind: "unit_checks", At: chosen, UnitChecks: &FeedUnitChecks{Unit: "upload", Run: 1, State: status, Selection: feedSelection(&selection), Failed: run.Failed, Error: run.Error, StartedAt: started, FinishedAt: &finished}}
			if after := read(); !reflect.DeepEqual(after, []FeedEntry{want}) {
				t.Fatalf("finished: %s; want %+v", describeFeed(after), want.UnitChecks)
			}
			data, err := json.Marshal(read())
			must(t, err)
			if strings.Contains(string(data), "captured output") {
				t.Fatal("feed contains captured output")
			}
		})
	}
}

func TestFeedLegacyUnitCheckRunsDoNotInventListedTotals(t *testing.T) {
	t.Parallel()
	_, cfg := conversationFixture(t, "flu-")
	repo, err := trace.Open(cfg.Root, cfg.Project)
	must(t, err)
	defer repo.Close()
	// These JSON records omit listed, as traces recorded before totals do.
	selection := CheckSelection{Mode: checkrun.SelectionSelected, Links: []string{"dag://a"}, Candidates: 113}
	feedDocument(t, repo, stream, checkDocumentID("upload", 1), "units/upload/checks-1.json", 1, demoStart, UnitCheckRun{Unit: "upload", Run: 1, Status: ChecksPassed, Selection: &selection, StartedAt: demoStart, FinishedAt: demoStart})
	feedDocument(t, repo, stream, checkDocumentID("upload", 2), "units/upload/checks-2.json", 1, demoStart.Add(time.Minute), UnitCheckRun{Unit: "upload", Run: 2, Status: ChecksFailed, Failed: []string{"dag://b"}, StartedAt: demoStart, FinishedAt: demoStart.Add(time.Minute)})
	entries, err := feedEntries(repo, repo, stream)
	must(t, err)
	if len(entries) != 2 {
		t.Fatalf("entries: %+v", entries)
	}
	got := entries[0].UnitChecks.Selection
	if got == nil || got.Selected != 1 || got.Judged != 113 || got.Listed != nil || entries[1].UnitChecks.Selection != nil {
		t.Fatalf("legacy selection: %+v", entries)
	}
	data, err := json.Marshal(entries)
	must(t, err)
	if strings.Contains(string(data), `"listed"`) {
		t.Fatalf("invented total: %s", data)
	}
}

func TestFeedFinalChecksShowEachStartedAttemptAndLegacyResult(t *testing.T) {
	t.Parallel()
	_, cfg := conversationFixture(t, "ffc-")
	repo, err := trace.Open(cfg.Root, cfg.Project)
	must(t, err)
	defer func() { repo.Close() }()
	at := func(n int) time.Time { return demoStart.Add(time.Duration(n) * time.Minute) }
	h := trace.Header{Schema: "osmia.trace.transition", Version: trace.Version, ID: "final-request", Revision: 1, Project: project, Workstream: stream, At: at(0), Actor: serviceActor, Cause: "feed-test"}
	_, err = repo.Transact(context.Background(), trace.Transaction{Transition: trace.Transition{Header: h, Subject: finalReviewSubject, To: "requested-1", Reason: "assembled"}})
	must(t, err)
	read := func() []FeedEntry {
		t.Helper()
		entries, err := feedEntries(repo, repo, stream)
		must(t, err)
		return slices.DeleteFunc(entries, func(e FeedEntry) bool { return e.Kind != "final_checks" })
	}
	if entries := read(); len(entries) != 0 {
		t.Fatal("request alone produced a check attempt")
	}
	start := func(review, attempt, minute int) {
		t.Helper()
		feedDocument(t, repo, stream, finalCheckAttemptDocument(review, attempt), finalCheckAttemptPath(review, attempt), 1, at(minute), FinalCheckAttempt{Review: review, Attempt: attempt, StartedAt: at(minute)})
	}
	result := func(review, attempt, revision, minute int, state, reason string) {
		t.Helper()
		run := FinalCheckRun{Review: review, Attempt: attempt, Status: state, Error: reason, StartedAt: at(minute - 1), FinishedAt: at(minute), Output: "output stays in trace"}
		if state == ChecksFailed {
			run.Failed = []string{"dag://failed"}
		}
		feedDocument(t, repo, stream, finalChecksDocument(review), finalChecksPath(review), revision, at(minute), run)
	}
	start(1, 1, 1)
	if entries := read(); len(entries) != 1 || entries[0].FinalChecks.State != "running" {
		t.Fatalf("start: %+v", entries)
	}
	result(1, 1, 1, 2, ChecksIncomplete, "engine unavailable")
	if entries := read(); len(entries) != 1 || entries[0].FinalChecks.State != ChecksIncomplete {
		t.Fatalf("waiting for retry: %+v", entries)
	}
	start(1, 2, 12)
	before := read()
	if len(before) != 2 || before[0].FinalChecks.State != ChecksIncomplete || before[0].FinalChecks.Error != "engine unavailable" || before[1].FinalChecks.State != "running" {
		t.Fatalf("retry: %+v", before)
	}
	must(t, repo.Close())
	repo, err = trace.Open(cfg.Root, cfg.Project)
	must(t, err)
	if entries := read(); !reflect.DeepEqual(entries, before) {
		t.Fatalf("restart changed attempts: %+v", entries)
	}
	result(1, 2, 2, 13, ChecksPassed, "")
	// Legacy revisions have no attempt start evidence. Every recorded result
	// remains visible; none can imply a running attempt.
	result(2, 0, 1, 14, ChecksIncomplete, "export failed")
	result(2, 0, 2, 15, ChecksFailed, "")
	start(3, 1, 16) // An abandoned start stays unfinished beside its retry.
	start(3, 2, 17)
	result(3, 2, 1, 18, ChecksFailed, "")
	got := read()
	states := []string{}
	for _, e := range got {
		states = append(states, e.FinalChecks.State)
	}
	if !reflect.DeepEqual(states, []string{ChecksIncomplete, ChecksPassed, ChecksIncomplete, ChecksFailed, "running", ChecksFailed}) {
		t.Fatalf("attempt states: %v", states)
	}
	if got[0].At != at(1) || got[1].At != at(12) || got[1].FinalChecks.Attempt != 2 || got[1].FinalChecks.FinishedAt == nil {
		t.Fatalf("attempt identity: %+v", got)
	}
	for _, i := range []int{2, 3} {
		if got[i].FinalChecks.Attempt != 0 || got[i].FinalChecks.StartedAt != nil {
			t.Fatalf("invented start: %+v", got[i].FinalChecks)
		}
	}
	if !reflect.DeepEqual(got[3].FinalChecks.Failed, []string{"dag://failed"}) {
		t.Fatalf("failed links: %+v", got[3])
	}
	data, err := json.Marshal(got)
	must(t, err)
	if strings.Contains(string(data), "output stays") {
		t.Fatal("feed contains captured output")
	}
}

func TestFeedFinalReviewOutcomesAndEveryClaimedRole(t *testing.T) {
	t.Parallel()
	_, cfg := conversationFixture(t, "ffr-")
	repo, err := trace.Open(cfg.Root, cfg.Project)
	must(t, err)
	defer repo.Close()
	ctx := context.Background()
	for i, role := range []string{masonRole, committeeRole, architectRole, "foreman", librarianRole} {
		id := fmt.Sprintf("agent_feed_%d", i)
		queueRoleTurn(t, repo, stream, id, role, "", demoStart)
		_, err := repo.ClaimTurn(ctx, stream, id, "token_"+id, filepath.Join(t.TempDir(), id), demoStart)
		must(t, err)
	}
	_, err = repo.EnsureChiefOfStaff(ctx, stream, demoStart, serviceActor)
	must(t, err)
	_, err = repo.EnqueueTurn(ctx, trace.TurnRequest{Header: trace.Header{Schema: "osmia.trace.turn-request", Version: trace.Version, ID: "owner_turn", Revision: 1, Project: project, Workstream: stream, At: demoStart, Actor: ownerActor, Cause: "owner-message"}, AgentID: trace.ChiefOfStaff, ThreadID: trace.ChiefOfStaff, TurnID: "message", Profile: coreadapter.Profile{Name: "chief", Backend: "fake", Model: "test"}, Prompt: "Review progress?"})
	must(t, err)
	_, err = repo.ClaimTurn(ctx, stream, trace.ChiefOfStaff, "token_chief", filepath.Join(t.TempDir(), "chief"), demoStart)
	must(t, err)
	before, err := conversationEntries(repo, stream)
	must(t, err)
	reviewed := FinalReport{Review: 1, Outcome: finalReviewed, Reader: "agent_feed_1", Turn: "agent_feed_1-turn", Findings: []FinalFinding{{Severity: FindingBlocking}, {Severity: FindingAdvisory}, {Severity: FindingBlocking}}, Criteria: []FinalCriterion{{Criterion: "spec#1", Gap: "uncovered"}, {Criterion: "spec#2", Evidence: "upload"}}}
	feedDocument(t, repo, stream, finalReportDocument, finalReportPath, 1, demoStart.Add(time.Minute), reviewed)
	feedDocument(t, repo, stream, finalReportDocument, finalReportPath, 2, demoStart.Add(2*time.Minute), FinalReport{Review: 2, Outcome: finalFailed, Failure: "rebase failed"})
	entries, err := feedEntries(repo, repo, stream)
	must(t, err)
	sessions := []FeedSession{}
	outcomes := []FeedFinalReview{}
	conversation := []ConversationEntry{}
	for _, e := range entries {
		switch e.Kind {
		case "session":
			sessions = append(sessions, *e.Session)
		case "final_review":
			outcomes = append(outcomes, *e.FinalReview)
		case "message", "response", "action":
			conversation = append(conversation, ConversationEntry{Kind: e.Kind, At: e.At, Turn: e.Turn, Text: e.Text, State: e.State})
		}
	}
	if len(sessions) != 6 {
		t.Fatalf("claimed sessions: %+v", sessions)
	}
	for _, s := range sessions {
		if s.Profile == "" || s.Role == "" || s.State != "running" {
			t.Fatalf("session: %+v", s)
		}
	}
	if !reflect.DeepEqual(conversation, before) {
		t.Fatalf("conversation changed: %+v; before %+v", conversation, before)
	}
	want := []FeedFinalReview{{Review: 1, State: finalReviewed, Reader: reviewed.Reader, Turn: reviewed.Turn, Blocking: 2, Gaps: 1}, {Review: 2, State: finalFailed, Failure: "rebase failed"}}
	if !reflect.DeepEqual(outcomes, want) {
		t.Fatalf("outcomes: %+v; want %+v", outcomes, want)
	}
	if !slices.ContainsFunc(sessions, func(s FeedSession) bool {
		return s.Agent == outcomes[0].Reader && s.Turn == outcomes[0].Turn && s.Role == committeeRole
	}) {
		t.Fatal("review reader has no session entry")
	}
}

func TestEventStreamAnnouncesFeedCheckAndReviewRecords(t *testing.T) {
	t.Parallel()
	s, c := eventTraceFixture(t)
	events, _ := eventStream(t, c)
	if e := nextEvent(t, events); e.Kind != EventResync {
		t.Fatalf("initial event: %+v", e)
	}
	repo := s.sole().repository
	at := demoStart
	for _, record := range []struct {
		id, path string
		revision int
		value    any
	}{
		{checkSelectionDocument("upload", 1), checkSelectionPath("upload", 1), 1, UnitCheckSelection{Unit: "upload", Run: 1, Selection: checkrun.FullSelection("disabled", "boost off"), StartedAt: at}},
		{checkDocumentID("upload", 1), "units/upload/checks-1.json", 1, UnitCheckRun{Unit: "upload", Run: 1, Status: ChecksPassed, StartedAt: at, FinishedAt: at}},
		{finalCheckAttemptDocument(1, 1), finalCheckAttemptPath(1, 1), 1, FinalCheckAttempt{Review: 1, Attempt: 1, StartedAt: at}},
		{finalChecksDocument(1), finalChecksPath(1), 1, FinalCheckRun{Review: 1, Attempt: 1, Status: ChecksIncomplete, Error: "timeout", StartedAt: at, FinishedAt: at}},
		{finalCheckAttemptDocument(1, 2), finalCheckAttemptPath(1, 2), 1, FinalCheckAttempt{Review: 1, Attempt: 2, StartedAt: at}},
		{finalChecksDocument(1), finalChecksPath(1), 2, FinalCheckRun{Review: 1, Attempt: 2, Status: ChecksPassed, StartedAt: at, FinishedAt: at}},
		{finalReportDocument, finalReportPath, 1, FinalReport{Review: 1, Outcome: finalReviewed}},
	} {
		feedDocument(t, repo, stream, record.id, record.path, record.revision, at, record.value)
		// Drain both notifications before the next commit, proving each record
		// reaches the open HTTP event stream rather than a coalesced earlier event.
		awaitEvents(t, events, Event{Kind: EventWorkstream, Project: project, Workstream: stream}, Event{Kind: EventInbox, Project: project, Workstream: stream})
		at = at.Add(time.Minute)
	}
}

func TestFeedIncludesEveryWorkflowSubject(t *testing.T) {
	t.Parallel()
	_, cfg := conversationFixture(t, "fts-")
	repo, err := trace.Open(cfg.Root, cfg.Project)
	must(t, err)
	defer repo.Close()
	subjects := []string{trace.FeatureSubject, trace.UnitSubject("upload"), checksSubject("upload"), checkBlockedSubject("upload"), finalReviewSubject, trace.QuestionSubject("1"), "drift-rebase", "seal", "amendment", "publication", "future-subject"}
	var want []FeedEntry
	for i, subject := range subjects {
		unit := ""
		if i >= 1 && i <= 3 {
			unit = "upload"
		}
		for j, to := range []string{"pending", "finished"} {
			at := demoStart.Add(time.Duration(2*i+j) * time.Minute)
			state, err := repo.Workflow(stream, subject)
			must(t, err)
			h := trace.Header{Schema: "osmia.trace.transition", Version: trace.Version, ID: fmt.Sprintf("feed-transition-%d-%d", i, j), Revision: 1, Project: project, Workstream: stream, Unit: unit, At: at, Actor: serviceActor, Cause: "feed-test"}
			reason := "reason for " + subject + " " + to
			_, err = repo.Transact(context.Background(), trace.Transaction{ExpectedVersion: state.Version, Transition: trace.Transition{Header: h, Subject: subject, From: state.Value, To: to, Reason: reason}})
			must(t, err)
			want = append(want, FeedEntry{Kind: "transition", At: at, Transition: &FeedTransition{Subject: subject, Unit: unit, From: state.Value, To: to, Actor: "service:osmia", Reason: reason}})
		}
	}
	got, err := feedEntries(repo, repo, stream)
	must(t, err)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("feed: %s; want: %s", describeFeed(got), describeFeed(want))
	}
}
