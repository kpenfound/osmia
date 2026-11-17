package service

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kpenfound/osmia/internal/api"
	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/delivery"
	"github.com/kpenfound/osmia/internal/execution"
	"github.com/kpenfound/osmia/internal/flakes"
	"github.com/kpenfound/osmia/internal/intake"
	"github.com/kpenfound/osmia/internal/runtime"
	"github.com/kpenfound/osmia/internal/servicetest/intakefixture"
	"github.com/kpenfound/osmia/internal/trace"
)

// flakyCloudTest is the Dagger Cloud name of flakyTraceTest.
const flakyCloudTest = "test/internal/trace/TestFlaky"

// flakeFixLive is a running service on an opted-in project whose workstream's
// unit checks record a flaky go-test link, with fake agents. The service's
// own schedule records the flake and notices the chief of staff.
type flakeFixLive struct {
	*shedFixture
	stream config.WorkstreamID
	// group is the flake group of the flaky link.
	group string
}

func startFlakeFixLive(t *testing.T, key string, outcomes ...checkOutcome) *flakeFixLive {
	t.Helper()
	f, stream, repository := newFlakeFixture(t, key, outcomes...)
	must(t, repository.Close())
	f.start(t)
	t.Cleanup(func() { f.stop(t) })
	l := &flakeFixLive{shedFixture: f, stream: stream, group: flakes.GroupFlaky(nil, []string{flakyTraceTest})[0].Group}
	l.setting(t, true)
	return l
}

// setting turns the project's flake-fix setting on or off through the
// project-config API, which the service adopts and keeps across restarts.
func (l *flakeFixLive) setting(t *testing.T, on bool) {
	t.Helper()
	ctx := context.Background()
	got, err := l.c.ProjectConfig(ctx, l.project)
	must(t, err)
	saved, err := l.c.SaveProjectConfig(ctx, l.project, ProjectConfigEdit{Pin: got.Pin, Set: changes(func(v *ProjectConfigEditValues) { v.FlakeFixes = yes(on) })})
	must(t, err)
	if saved.Outcome != ProjectConfigApplied {
		t.Fatalf("saving flake_fixes = %v: %+v", on, saved)
	}
}

func (l *flakeFixLive) restart(t *testing.T) {
	t.Helper()
	l.stop(t)
	l.start(t)
}

// awaitFlake polls until cond holds.
func awaitFlake(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(60 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// awaitNoticeOf waits until the flake notice naming the occurrence is sent to
// stream's chief of staff, and returns every notice of stream. The notice is
// sent once.
func (l *flakeFixLive) awaitNoticeOf(t *testing.T, stream config.WorkstreamID, id string) []string {
	t.Helper()
	count := func() int {
		n := 0
		for _, body := range flakeNoticeBodies(t, l.repository(), stream) {
			if strings.Contains(body, id) {
				n++
			}
		}
		return n
	}
	awaitFlake(t, "the notice of occurrence "+id, func() bool {
		posted, _ := flakePosts(t, l.repository(), stream, "flake-notice-")
		return len(posted) > 0 && count() > 0
	})
	if n := count(); n != 1 {
		t.Fatalf("%d notices name occurrence %s", n, id)
	}
	return flakeNoticeBodies(t, l.repository(), stream)
}

// groupOf is the key of the flake group the occurrence is recorded in.
func (l *flakeFixLive) groupOf(t *testing.T, o flakes.Occurrence) string {
	t.Helper()
	groups, err := flakeStoreOf(l.repository()).Groups()
	must(t, err)
	for _, g := range groups {
		if _, ok := g.Find(o.Run); ok {
			return g.Group
		}
	}
	t.Fatalf("occurrence %s is in no group", o.ID)
	return ""
}

// occurrence is the occurrence of the run record at path of stream.
func (l *flakeFixLive) occurrence(t *testing.T, stream config.WorkstreamID, path string) flakes.Occurrence {
	t.Helper()
	var found flakes.Occurrence
	awaitFlake(t, "the occurrence of "+path, func() bool {
		for _, o := range flakeOccurrences(t, l.repository()) {
			if o.Run == flakes.RunKey(stream, path) {
				found = o
				return true
			}
		}
		return false
	})
	return found
}

// chief claims a chief-of-staff turn of stream and returns the flake_fix tool
// the service composes for it.
func (l *flakeFixLive) chief(t *testing.T, stream config.WorkstreamID, name string) coreadapter.Tool {
	t.Helper()
	repo := l.repository()
	ctx := context.Background()
	for attempt := 0; ; attempt++ {
		id := fmt.Sprintf("%s-%d", name, attempt)
		agentID, thread, turn := "fix-chief-"+id, "fix-chief-thread-"+id, "fix-chief-turn-"+id
		now := time.Now().UTC()
		h := func(schema, hid string) trace.Header {
			return trace.Header{Schema: "osmia.trace." + schema, Version: trace.Version, ID: hid, Revision: 1, Project: repo.Project(), Workstream: stream, At: now, Actor: ownerActor, Cause: "flake-fix-test"}
		}
		must(t, repo.CreateThread(ctx, trace.Agent{Header: h("agent", agentID), Role: trace.ChiefOfStaff, ThreadID: thread}))
		_, err := repo.EnqueueTurn(ctx, trace.TurnRequest{Header: h("turn-request", "request_"+turn), AgentID: agentID, ThreadID: thread, TurnID: turn, Profile: coreadapter.Profile{Name: "default", Backend: "fake", Model: "test"}, Prompt: "Work"})
		must(t, err)
		if _, err := repo.ClaimTurn(ctx, stream, agentID, "token_"+id, "/owned/"+id, now); err != nil {
			// The service's own runner claimed the turn first.
			if attempt < 5 {
				continue
			}
			t.Fatalf("claiming a chief-of-staff turn: %v", err)
		}
		controls := &runtimeControls{}
		controls.service.Store(l.s)
		return controls.flakeFix(repo, coreadapter.Scope{Project: string(repo.Project()), Workstream: string(stream), Thread: thread, Turn: turn, Role: trace.ChiefOfStaff})
	}
}

// flakeFixAnswer is what the tool answers.
type flakeFixAnswer struct {
	Recorded   *bool               `json:"recorded"`
	Reason     string              `json:"reason"`
	Decision   string              `json:"decision"`
	Group      string              `json:"group"`
	Workstream config.WorkstreamID `json:"workstream"`
}

func callFlakeFix(t *testing.T, tool coreadapter.Tool, input string) flakeFixAnswer {
	t.Helper()
	var out flakeFixAnswer
	must(t, json.Unmarshal([]byte(intakefixture.Handle(t, tool, input)), &out))
	return out
}

func (a flakeFixAnswer) refused() bool { return a.Recorded != nil && !*a.Recorded }

func openInput(occurrence, notes string) string {
	return fmt.Sprintf(`{"action":"open","occurrence":%q,"notes":%q}`, occurrence, notes)
}

// recordUnitFlakyRun records a unit check run, as the unit's checks do, that
// passed with the links flaky.
func recordUnitFlakyRun(t *testing.T, repository *trace.Repository, stream config.WorkstreamID, unit string, run int, links ...string) string {
	t.Helper()
	at := time.Now().UTC()
	output := "CHECKS  ✔ passed\n"
	for _, link := range links {
		output += "✔ " + link + " 1.0s OK\n"
	}
	record := UnitCheckRun{Unit: unit, Run: run, Status: ChecksPassed, Flaky: links, Output: output, StartedAt: at, FinishedAt: at,
		Evidence: &api.CheckEvidence{Engine: api.CheckEngineLocal, CoRun: links, Parallelism: 4}}
	data, err := json.MarshalIndent(record, "", "  ")
	must(t, err)
	path := execution.CheckPath(unit, run)
	h := trace.Header{Schema: "osmia.trace.document", Version: trace.Version, ID: fmt.Sprintf("%s-checks-%d", trace.UnitSubject(unit), run), Revision: 1, Project: repository.Project(), Workstream: stream, Unit: unit, At: at, Actor: trace.Actor{Kind: "service", ID: "checks"}, Cause: "fixture"}
	must(t, repository.RecordDocuments(context.Background(), []trace.Document{{Header: h, Path: path, Content: string(data) + "\n"}}))
	return path
}

// recordCloudFinalRun records the final-review check run of a Dagger Cloud
// result whose flaky checks are the names.
func recordCloudFinalRun(t *testing.T, repository *trace.Repository, stream config.WorkstreamID, review int, names ...string) string {
	t.Helper()
	at := time.Now().UTC()
	run := FinalCheckRun{Review: review, Operation: "final-review-op", Commit: "abc123", Source: api.CheckSourcePullRequest, PullRequest: 7, Push: 1, PushedCommit: "abc123", Status: ChecksPassed,
		Flaky: names, StartedAt: at, FinishedAt: at,
		Evidence: &api.CheckEvidence{Engine: api.CheckEngineCloud, CoRun: slices.Concat([]string{"lint"}, names),
			Traces: []api.CheckTraceRef{{Kind: api.CheckTraceFailed, Check: names[0], TraceID: "trace-final"}},
			Unavailable: []api.CheckUnavailable{
				{Item: api.CheckItemOutput, Engine: api.CheckEngineCloud, Where: "dagger trace trace-final"},
				{Item: api.CheckItemParallelism, Engine: api.CheckEngineCloud, Where: "dagger trace trace-final"},
			}}}
	data, err := json.MarshalIndent(run, "", "  ")
	must(t, err)
	h := trace.Header{Schema: "osmia.trace.document", Version: trace.Version, ID: delivery.FinalChecksDocument(review), Revision: 1, Project: repository.Project(), Workstream: stream, At: at, Actor: trace.Actor{Kind: "service", ID: "final-review"}, Cause: "final-review-op"}
	must(t, repository.RecordDocuments(context.Background(), []trace.Document{{Header: h, Path: delivery.FinalChecksPath(review), Content: string(data) + "\n"}}))
	return delivery.FinalChecksPath(review)
}

// handedText is the text the workstream was handed.
func handedText(t *testing.T, repository *trace.Repository, stream config.WorkstreamID) string {
	t.Helper()
	docs, err := trace.Read[trace.Document](repository, stream)
	must(t, err)
	for _, d := range docs {
		if d.ID == intake.HandedDocument {
			return d.Content
		}
	}
	t.Fatalf("workstream %s holds no handed document", stream)
	return ""
}

// flakeFixWorkstreams are the distinct workstreams the project's occurrences
// opened or joined.
func flakeFixWorkstreams(t *testing.T, repository *trace.Repository) []config.WorkstreamID {
	t.Helper()
	var out []config.WorkstreamID
	for _, o := range flakeOccurrences(t, repository) {
		if (o.Decision.State == flakes.Opened || o.Decision.State == flakes.Joined || o.Decision.State == flakes.Opening) && !slices.Contains(out, o.Decision.Workstream) {
			out = append(out, o.Decision.Workstream)
		}
	}
	return out
}

func expectedFlakeFixWorkstream(project config.ProjectID, group string, generation int) config.WorkstreamID {
	return intake.HandInWorkstream(project, flakes.HandInKey(project, group, generation))
}

func TestFlakeFixIsAChiefOfStaffToolOnly(t *testing.T) {
	t.Parallel()
	if !slices.Contains(chiefGrant.Tools, flakeFixToolName) || !slices.Contains(chiefOrdinaryNarrow.Tools, flakeFixToolName) {
		t.Fatalf("an ordinary chief-of-staff turn does not hold %s: %v", flakeFixToolName, chiefOrdinaryNarrow.Tools)
	}
	for name, tools := range map[string][]string{"design session": designNarrow.Tools, "mason": masonGrant.Tools, "unit mason": unitMasonGrant.Tools, "reviewer": reviewerGrant.Tools} {
		if slices.Contains(tools, flakeFixToolName) {
			t.Errorf("a %s turn holds %s", name, flakeFixToolName)
		}
	}
}

// A unit's check run records a flaky go-test link; the service notices the
// chief, whose tool opens a flake-fix workstream from the rendered evidence
// and its notes. Later flakes of the group, in a sibling test and in a final
// review's Dagger Cloud statuses, open nothing and reach the open workstream.
func TestFlakeFixOpensOneOrdinaryWorkstreamFromRealCheckRuns(t *testing.T) {
	t.Parallel()
	l := startFlakeFixLive(t, "flake-fix-e2e", flakyOutcome(flakyTraceTest))
	faults := &intakefixture.Faults{}
	l.member(1, 1, 1, intakefixture.Concedes(faults))
	repo := l.repository()
	const unitRun = "units/resume/checks-1.json"

	occ := l.occurrence(t, l.stream, unitRun)
	notices := l.awaitNoticeOf(t, l.stream, occ.ID)
	if i := slices.IndexFunc(notices, func(n string) bool { return strings.Contains(n, occ.ID) }); !strings.Contains(notices[i], "Awaiting your decision (1)") || !strings.Contains(notices[i], flakeFixToolName) {
		t.Fatalf("notices %q", notices)
	}
	if got := l.groupOf(t, occ); got != l.group {
		t.Fatalf("the occurrence is in group %q, want %q", got, l.group)
	}

	tool := l.chief(t, l.stream, "open")
	const notes = "Suspect shared fixture state between the trace tests."
	opened := callFlakeFix(t, tool, openInput(occ.ID, notes))
	want := expectedFlakeFixWorkstream(repo.Project(), l.group, 0)
	if opened.refused() || opened.Decision != flakes.Opened || opened.Workstream != want {
		t.Fatalf("open answered %+v, want an opened %s", opened, want)
	}

	// The workstream is an ordinary hand-in of the project: its handed
	// document is the rendered evidence plus the chief's notes.
	text := handedText(t, repo, want)
	for _, part := range []string{flakyTraceTest, unitRun, string(l.stream), "resume", notes, flakes.NotesHeading} {
		if !strings.Contains(text, part) {
			t.Fatalf("the handed document lacks %q:\n%s", part, text)
		}
	}
	if strings.Index(text, flakes.NotesHeading) < strings.Index(text, unitRun) || strings.Count(text, notes) != 1 {
		t.Fatalf("the chief's notes are not a separate section after the evidence:\n%s", text)
	}
	transitions, err := trace.Read[trace.Transition](repo, want)
	must(t, err)
	var handedIn bool
	for _, tr := range transitions {
		switch {
		case tr.ID == intake.HandInTransition:
			handedIn = true
			if tr.Actor != chiefActor || tr.To != intake.HandedState || !strings.Contains(tr.Reason, "chief of staff") || !strings.Contains(tr.Reason, string(l.stream)) || !strings.Contains(tr.Reason, occ.ID) {
				t.Fatalf("the hand-in transition: %+v", tr)
			}
		case tr.ID == intake.SkipTransition:
			t.Fatalf("debate was skipped: %+v", tr)
		}
	}
	if !handedIn {
		t.Fatalf("no hand-in transition: %+v", transitions)
	}
	if base, err := repo.WorkstreamBase(want); err != nil || base.Base != "" {
		t.Fatalf("base %+v: %v", base, err)
	}
	if st, _ := l.s.store.Snapshot(); slices.ContainsFunc(st.Priorities, func(p runtime.Priority) bool { return slices.Contains(p.Workstreams, want) }) {
		t.Fatalf("the workstream has a priority of its own: %+v", st.Priorities)
	}

	// It goes on to the architect, the committee and the owner's ratification;
	// nothing pre-ratifies it.
	l.awaitShed(t, want, "concluded-1")
	faults.Check(t)
	if docs := l.ratifications(t, want); len(docs) != 0 {
		t.Fatalf("the workstream was ratified without the owner: %+v", docs)
	}
	if entries := l.ratificationEntries(t, want); len(entries) != 1 {
		t.Fatalf("the ratification waits at the gate as %+v", entries)
	}

	// The originating workstream's feed names it.
	feed := readFeed(t, l.c, l.stream)
	if !slices.ContainsFunc(feed.Entries, func(e FeedEntry) bool {
		return e.Transition != nil && strings.HasPrefix(e.Transition.Subject, "flake-decision-") && strings.Contains(e.Transition.Reason, string(want)) && e.Transition.Actor == chiefActor.Kind+":"+chiefActor.ID
	}) {
		t.Fatalf("the feed does not name %s: %+v", want, feed.Entries)
	}

	// A later flake in a sibling test, and a final review's Cloud flake of a
	// test in the package, join the open workstream and open no other.
	streamsBefore, err := repo.Workstreams()
	must(t, err)
	siblingRun := recordUnitFlakyRun(t, repo, l.stream, "extra", 1, flakyTraceOther)
	cloudRun := recordCloudFinalRun(t, repo, l.stream, 1, flakyCloudTest)
	awaitFlake(t, "the later occurrences to be noticed", func() bool {
		for _, path := range []string{siblingRun, cloudRun} {
			o, ok := findRunOccurrence(flakeOccurrences(t, l.repository()), l.stream, path)
			if !ok || o.Noticed == nil {
				return false
			}
		}
		return true
	})
	for _, path := range []string{siblingRun, cloudRun} {
		o := l.occurrence(t, l.stream, path)
		if l.groupOf(t, o) != l.group || o.Decision.State != flakes.Joined || o.Decision.Workstream != want || len(o.Surfaced) != 1 {
			t.Fatalf("the occurrence of %s: %+v", path, o)
		}
	}
	surfacings := flakeSurfacings(t, l.repository(), want)
	if len(surfacings) != 2 || !strings.Contains(strings.Join(surfacings, "\n"), flakyTraceOther) || !strings.Contains(strings.Join(surfacings, "\n"), flakyCloudTest) {
		t.Fatalf("surfacings %q", surfacings)
	}
	if streamsAfter, err := l.repository().Workstreams(); err != nil || len(streamsAfter) != len(streamsBefore) {
		t.Fatalf("workstreams %v after %v: %v", streamsAfter, streamsBefore, err)
	}
	if got := flakeFixWorkstreams(t, l.repository()); !slices.Equal(got, []config.WorkstreamID{want}) {
		t.Fatalf("flake-fix workstreams %v", got)
	}

	// Asking again for the decided occurrence returns its decision.
	again := callFlakeFix(t, tool, openInput(occ.ID, "other notes"))
	if again.Decision != flakes.Opened || again.Workstream != want {
		t.Fatalf("a repeated open answered %+v", again)
	}
	if text2 := handedText(t, l.repository(), want); text2 != text {
		t.Fatalf("a repeated open changed the handed document")
	}
}

func findRunOccurrence(occurrences []flakes.Occurrence, stream config.WorkstreamID, path string) (flakes.Occurrence, bool) {
	for _, o := range occurrences {
		if o.Run == flakes.RunKey(stream, path) {
			return o, true
		}
	}
	return flakes.Occurrence{}, false
}

// Two chiefs, of two workstreams, open the same group at once: one workstream
// results, and the occurrence that lost the race joined it.
func TestFlakeFixConcurrentOpensOfOneGroupYieldOneWorkstream(t *testing.T) {
	t.Parallel()
	l := startFlakeFixLive(t, "flake-fix-concurrent", flakyOutcome(flakyTraceTest))
	repo := l.repository()
	other := config.WorkstreamID("w_ffffffffffffffffffffffffffffffff")
	must(t, repo.CreateWorkstream(context.Background(), other, time.Now(), trace.Actor{Kind: "service", ID: "fixture"}))
	otherRun := recordUnitFlakyRun(t, repo, other, "resume", 1, flakyTraceOther)
	first, second := l.occurrence(t, l.stream, "units/resume/checks-1.json"), l.occurrence(t, other, otherRun)
	l.awaitNoticeOf(t, l.stream, first.ID)
	l.awaitNoticeOf(t, other, second.ID)
	first, second = l.occurrence(t, l.stream, "units/resume/checks-1.json"), l.occurrence(t, other, otherRun)
	if l.groupOf(t, first) != l.groupOf(t, second) || first.Decision.State != flakes.Pending || second.Decision.State != flakes.Pending {
		t.Fatalf("occurrences %+v and %+v", first, second)
	}
	before, err := repo.Workstreams()
	must(t, err)

	tools := []coreadapter.Tool{l.chief(t, l.stream, "a"), l.chief(t, other, "b")}
	occurrences := []flakes.Occurrence{first, second}
	answers := make([]flakeFixAnswer, 2)
	var wg sync.WaitGroup
	for i := range tools {
		wg.Add(1)
		go func() {
			defer wg.Done()
			out, err := tools[i].Handle(context.Background(), json.RawMessage(openInput(occurrences[i].ID, "")))
			if err != nil {
				t.Errorf("open %d: %v", i, err)
				return
			}
			if err := json.Unmarshal(out, &answers[i]); err != nil {
				t.Errorf("open %d: %v", i, err)
			}
		}()
	}
	wg.Wait()
	want := expectedFlakeFixWorkstream(repo.Project(), l.group, 0)
	var opened, joined int
	for _, a := range answers {
		if a.Workstream != want {
			t.Fatalf("answers %+v, want workstream %s", answers, want)
		}
		switch a.Decision {
		case flakes.Opened:
			opened++
		case flakes.Joined:
			joined++
		}
	}
	if opened != 1 || joined != 1 {
		t.Fatalf("answers %+v: %d opened and %d joined", answers, opened, joined)
	}
	after, err := l.repository().Workstreams()
	must(t, err)
	if len(after) != len(before)+1 || !slices.Contains(after, want) {
		t.Fatalf("workstreams %v after %v", after, before)
	}
	// The one that joined was surfaced to the opened workstream's chief.
	awaitFlake(t, "the surfacing", func() bool { return len(flakeSurfacings(t, l.repository(), want)) == 1 })
}

// After the flake-fix workstream is abandoned and the service restarts,
// replaying the open returns the abandoned workstream and opens nothing; a new
// flake in the group opens the next workstream with the earlier history.
func TestFlakeFixReplayAfterAbandonAndRestartOpensNothingAndTheNextFlakeOpensTheNextGeneration(t *testing.T) {
	t.Parallel()
	l := startFlakeFixLive(t, "flake-fix-abandon", flakyOutcome(flakyTraceTest))
	const unitRun = "units/resume/checks-1.json"
	occ := l.occurrence(t, l.stream, unitRun)
	l.awaitNoticeOf(t, l.stream, occ.ID)
	first := callFlakeFix(t, l.chief(t, l.stream, "first"), openInput(occ.ID, ""))
	want := expectedFlakeFixWorkstream(l.repository().Project(), l.group, 0)
	if first.Decision != flakes.Opened || first.Workstream != want {
		t.Fatalf("open answered %+v", first)
	}

	_, err := l.c.Abandon(context.Background(), want, "the owner dropped it")
	must(t, err)
	l.restart(t)

	replay := callFlakeFix(t, l.chief(t, l.stream, "replay"), openInput(occ.ID, "replayed"))
	if replay.Decision != flakes.Opened || replay.Workstream != want {
		t.Fatalf("the replayed open answered %+v", replay)
	}
	if got := flakeFixWorkstreams(t, l.repository()); !slices.Equal(got, []config.WorkstreamID{want}) {
		t.Fatalf("flake-fix workstreams %v", got)
	}

	// The group's association ended with the abandonment, so a new flake opens
	// the next generation.
	awaitFlake(t, "the group to close", func() bool {
		g, found, err := flakeStoreOf(l.repository()).Group(l.group)
		must(t, err)
		return found && g.Open == nil && g.Generation == 1
	})
	nextRun := recordUnitFlakyRun(t, l.repository(), l.stream, "extra", 1, flakyTraceOther)
	next := l.occurrence(t, l.stream, nextRun)
	l.awaitNoticeOf(t, l.stream, next.ID)
	if next = l.occurrence(t, l.stream, nextRun); next.Decision.State != flakes.Pending {
		t.Fatalf("the new occurrence is %+v, want it awaiting a decision", next.Decision)
	}
	opened := callFlakeFix(t, l.chief(t, l.stream, "next"), openInput(next.ID, ""))
	wantNext := expectedFlakeFixWorkstream(l.repository().Project(), l.group, 1)
	if opened.Decision != flakes.Opened || opened.Workstream != wantNext || wantNext == want {
		t.Fatalf("the new flake answered %+v, want an opened %s", opened, wantNext)
	}
	text := handedText(t, l.repository(), wantNext)
	for _, part := range []string{"Earlier history of this flake group", string(want), unitRun, flakyTraceTest} {
		if !strings.Contains(text, part) {
			t.Fatalf("the next hand-in lacks %q:\n%s", part, text)
		}
	}
}

// An open interrupted after the keyed hand-in was created and before the
// decision was recorded completes after a restart to the same workstream.
func TestFlakeFixInterruptedOpenCompletesAfterRestart(t *testing.T) {
	t.Parallel()
	l := startFlakeFixLive(t, "flake-fix-interrupted", flakyOutcome(flakyTraceTest))
	occ := l.occurrence(t, l.stream, "units/resume/checks-1.json")
	l.awaitNoticeOf(t, l.stream, occ.ID)
	tool := l.chief(t, l.stream, "interrupted")
	l.s.setBoundary(func(name string) error {
		if name == "flake-fix-handed" {
			return errFlakeStop
		}
		return nil
	})
	if _, err := tool.Handle(context.Background(), json.RawMessage(openInput(occ.ID, "A note that survives."))); err == nil {
		t.Fatal("the interrupted open reported success")
	}
	want := expectedFlakeFixWorkstream(l.repository().Project(), l.group, 0)
	if streams, err := l.repository().Workstreams(); err != nil || !slices.Contains(streams, want) {
		t.Fatalf("the keyed hand-in was not made: %v %v", streams, err)
	}
	if o := l.occurrence(t, l.stream, "units/resume/checks-1.json"); o.Decision.State != flakes.Opening || o.Decision.Workstream != want {
		t.Fatalf("the decision is %+v, want it reserved", o.Decision)
	}

	l.restart(t)
	awaitFlake(t, "the reserved opening to complete", func() bool {
		o, ok := findRunOccurrence(flakeOccurrences(t, l.repository()), l.stream, "units/resume/checks-1.json")
		return ok && o.Decision.State == flakes.Opened && o.Decision.Workstream == want
	})
	if got := flakeFixWorkstreams(t, l.repository()); !slices.Equal(got, []config.WorkstreamID{want}) {
		t.Fatalf("flake-fix workstreams %v", got)
	}
	// Retrying the open finds the decision and opens nothing more.
	retry := callFlakeFix(t, l.chief(t, l.stream, "retry"), openInput(occ.ID, "A note that survives."))
	if retry.Decision != flakes.Opened || retry.Workstream != want {
		t.Fatalf("the retried open answered %+v", retry)
	}
	awaitFlake(t, "the decision to show in the originating feed", func() bool {
		feed := readFeed(t, l.c, l.stream)
		return slices.ContainsFunc(feed.Entries, func(e FeedEntry) bool {
			return e.Transition != nil && strings.HasPrefix(e.Transition.Subject, "flake-decision-") && strings.Contains(e.Transition.Reason, string(want))
		})
	})
}

func TestFlakeFixDeclineNeedsAReasonAndNotifiesAgainOnTheNextFlake(t *testing.T) {
	t.Parallel()
	l := startFlakeFixLive(t, "flake-fix-decline", flakyOutcome(flakyTraceTest))
	occ := l.occurrence(t, l.stream, "units/resume/checks-1.json")
	l.awaitNoticeOf(t, l.stream, occ.ID)
	tool := l.chief(t, l.stream, "decline")

	for _, input := range []string{
		fmt.Sprintf(`{"action":"decline","occurrence":%q}`, occ.ID),
		fmt.Sprintf(`{"action":"decline","occurrence":%q,"reason":"   "}`, occ.ID),
	} {
		if a := callFlakeFix(t, tool, input); !a.refused() || !strings.Contains(a.Reason, "reason") {
			t.Fatalf("a decline without a reason answered %+v", a)
		}
	}
	if o := l.occurrence(t, l.stream, "units/resume/checks-1.json"); o.Decision.State != flakes.Pending {
		t.Fatalf("a refused decline decided %+v", o.Decision)
	}

	const reason = "The flaky test was added by the unit itself."
	declined := callFlakeFix(t, tool, fmt.Sprintf(`{"action":"decline","occurrence":%q,"reason":%q}`, occ.ID, reason))
	if declined.refused() || declined.Decision != flakes.Declined || declined.Reason != reason {
		t.Fatalf("decline answered %+v", declined)
	}
	if o := l.occurrence(t, l.stream, "units/resume/checks-1.json"); o.Decision.State != flakes.Declined || o.Decision.Reason != reason {
		t.Fatalf("the decision is %+v", o.Decision)
	}
	// The reason is in the originating workstream's trace and feed.
	transitions, err := trace.Read[trace.Transition](l.repository(), l.stream)
	must(t, err)
	if !slices.ContainsFunc(transitions, func(tr trace.Transition) bool {
		return strings.HasPrefix(tr.Subject, "flake-decision-") && tr.To == "declined" && strings.Contains(tr.Reason, reason) && tr.Actor == chiefActor
	}) {
		t.Fatalf("the trace does not record the decline: %+v", transitions)
	}
	feed := readFeed(t, l.c, l.stream)
	if !slices.ContainsFunc(feed.Entries, func(e FeedEntry) bool {
		return e.Transition != nil && e.Transition.To == "declined" && strings.Contains(e.Transition.Reason, reason)
	}) {
		t.Fatalf("the feed does not show the decline: %+v", feed.Entries)
	}
	// Opening a declined occurrence returns the decline and opens nothing.
	if a := callFlakeFix(t, tool, openInput(occ.ID, "")); a.Decision != flakes.Declined || a.Reason != reason {
		t.Fatalf("an open after a decline answered %+v", a)
	}
	if got := flakeFixWorkstreams(t, l.repository()); len(got) != 0 {
		t.Fatalf("flake-fix workstreams %v", got)
	}

	// A later flake in the group notifies the chief of staff again.
	nextRun := recordUnitFlakyRun(t, l.repository(), l.stream, "extra", 1, flakyTraceOther)
	next := l.occurrence(t, l.stream, nextRun)
	notices := l.awaitNoticeOf(t, l.stream, next.ID)
	next = l.occurrence(t, l.stream, nextRun)
	i := slices.IndexFunc(notices, func(n string) bool { return strings.Contains(n, next.ID) })
	if next.Decision.State != flakes.Pending || i < 0 || !strings.Contains(notices[i], "Awaiting your decision (1)") {
		t.Fatalf("the later flake: %+v, notices %q", next, notices)
	}
}

func TestFlakeFixRefusals(t *testing.T) {
	t.Parallel()
	l := startFlakeFixLive(t, "flake-fix-refusals", flakyOutcome(flakyTraceTest))
	occ := l.occurrence(t, l.stream, "units/resume/checks-1.json")
	l.awaitNoticeOf(t, l.stream, occ.ID)
	tool := l.chief(t, l.stream, "refusals")
	refused := func(what, input, contains string) {
		t.Helper()
		if a := callFlakeFix(t, tool, input); !a.refused() || !strings.Contains(a.Reason, contains) {
			t.Fatalf("%s answered %+v, want a refusal mentioning %q", what, a, contains)
		}
	}

	refused("an unknown occurrence", openInput("0123456789abcdef", ""), "no recorded flake occurrence")
	refused("free-form content", fmt.Sprintf(`{"action":"open","occurrence":%q,"content":"# Do something else"}`, occ.ID), "cannot hand in other content")
	refused("a handed document", fmt.Sprintf(`{"action":"open","occurrence":%q,"stdin":"# Do something else"}`, occ.ID), "cannot hand in other content")
	refused("an unknown action", fmt.Sprintf(`{"action":"merge","occurrence":%q}`, occ.ID), "open or decline")
	refused("notes on a decline", fmt.Sprintf(`{"action":"decline","occurrence":%q,"reason":"r","notes":"n"}`, occ.ID), "notes belong to open")

	// An occurrence from a flake-fix workstream's own run.
	ownRun := flakes.RunKey(l.stream, "units/own/checks-1.json")
	_, _, err := flakeStoreOf(l.repository()).Record(context.Background(), flakes.OccurrenceInput{Group: l.group, Run: ownRun, Source: flakes.Source{Workstream: l.stream, Unit: "own"}, Links: []string{flakyTraceOther}, FromFlakeFix: true})
	must(t, err)
	own := flakes.OccurrenceID(ownRun, l.group)
	refused("an occurrence of a flake-fix run", openInput(own, ""), "flake-fix workstream's own run")

	// With the setting off, the tool refuses and nothing opens.
	l.setting(t, false)
	refused("a project with the setting off", openInput(occ.ID, ""), "off")
	refused("a decline on a project with the setting off", fmt.Sprintf(`{"action":"decline","occurrence":%q,"reason":"r"}`, occ.ID), "off")
	if got := flakeFixWorkstreams(t, l.repository()); len(got) != 0 {
		t.Fatalf("flake-fix workstreams %v", got)
	}
	if o := l.occurrence(t, l.stream, "units/resume/checks-1.json"); o.Decision.State != flakes.Pending {
		t.Fatalf("a refusal decided %+v", o.Decision)
	}
}
