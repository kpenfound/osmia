package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/runtime"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	"github.com/kpenfound/osmia/internal/servicetest/intakefixture"
	"github.com/kpenfound/osmia/internal/trace"
)

type followUpKit struct {
	root    config.Root
	project config.Project
	repo    *trace.Repository
	stream  config.WorkstreamID
	at      time.Time
	turns   int
}

func newFollowUpKit(t *testing.T) *followUpKit {
	t.Helper()
	ctx := context.Background()
	base := t.TempDir()
	root, err := config.ResolveRoot(filepath.Join(base, "osmia"), "")
	fixturekit.Must(t, err)
	project := config.Project{ID: "p_00000000000000000000000000000031", Clone: filepath.Join(base, "clone")}
	fixturekit.DemoGit(t, base, "init", "-q", project.Clone)
	k := &followUpKit{root: root, project: project, stream: "w_00000000000000000000000000000031", at: time.Now().UTC().Truncate(time.Second)}
	k.repo, err = trace.Create(ctx, root, project, k.at, ownerActor)
	fixturekit.Must(t, err)
	t.Cleanup(func() { _ = k.repo.Close() })
	fixturekit.Must(t, k.repo.CreateWorkstream(ctx, k.stream, k.at, ownerActor))
	h := trace.Header{Schema: "osmia.trace.transition", Version: trace.Version, ID: "building", Revision: 1, Project: project.ID, Workstream: k.stream, At: k.at, Actor: ownerActor, Cause: "ratification"}
	_, err = k.repo.Transact(ctx, trace.Transaction{Transition: trace.Transition{Header: h, Subject: trace.FeatureSubject, To: BuildingState, Reason: "Owner ratified intent"}})
	fixturekit.Must(t, err)
	return k
}

func (k *followUpKit) now() time.Time { return k.at }

func (k *followUpKit) unit(t *testing.T, unit, state string) {
	t.Helper()
	st, err := k.repo.Workflow(k.stream, trace.UnitSubject(unit))
	fixturekit.Must(t, err)
	h := trace.Header{Schema: "osmia.trace.transition", Version: trace.Version, ID: "unit-" + unit + "-" + state, Revision: 1, Project: k.project.ID, Workstream: k.stream, Unit: unit, At: k.at, Actor: ownerActor, Cause: "test"}
	_, err = k.repo.Transact(context.Background(), trace.Transaction{ExpectedVersion: st.Version, Transition: trace.Transition{Header: h, Subject: trace.UnitSubject(unit), From: st.Value, To: state, Reason: "test"}})
	fixturekit.Must(t, err)
}

func (k *followUpKit) turn(t *testing.T, id, prompt string) coreadapter.Scope {
	t.Helper()
	k.turns++
	return intakefixture.ClaimChiefTurn(t, k.repo, k.project.ID, k.stream, k.now, intakefixture.ChiefTurnInput{
		Turn: id, Prompt: prompt, Actor: serviceActor, Cause: "test", Token: "token-" + id, Directory: id,
	})
}

func (k *followUpKit) finish(t *testing.T, id string) {
	t.Helper()
	fixturekit.AnswerChief(t, k.repo, k.stream, id, "Done.")
}

func (k *followUpKit) tools(scope coreadapter.Scope) (schedule, action coreadapter.Tool) {
	accepted := coreadapter.Tool{Name: moveUnitTool, Handle: func(context.Context, json.RawMessage) (json.RawMessage, error) {
		return json.RawMessage(`{"recorded":true,"outcome":"moved"}`), nil
	}}
	tools := (&runtimeControls{}).reportQueued(nil, k.repo, scope, k.now, []coreadapter.Tool{newFollowUpTool(k.repo, scope, k.now), accepted})
	return tools[0], tools[1]
}

func (k *followUpKit) pass(t *testing.T) {
	t.Helper()
	f := followUps{now: k.now, window: config.DefaultStallAfter, profile: func() (coreadapter.Profile, error) {
		return coreadapter.Profile{Name: "default", Backend: "claude", Model: "test"}, nil
	}}
	fixturekit.Must(t, f.pass(context.Background(), k.repo, []config.WorkstreamID{k.stream}))
}

func (k *followUpKit) pending(t *testing.T) []followUp {
	t.Helper()
	recs, err := readFollowUps(k.repo, k.stream)
	fixturekit.Must(t, err)
	return recs.pending()
}

func (k *followUpKit) followUpTurns(t *testing.T) []trace.QueuedTurn {
	t.Helper()
	th, err := k.repo.Thread(k.stream, trace.Manager)
	fixturekit.Must(t, err)
	var out []trace.QueuedTurn
	for _, q := range th.Turns {
		if strings.HasPrefix(q.Request.TurnID, followUpTurnPrefix) {
			out = append(out, q)
		}
	}
	return out
}

type queuedResult struct {
	Recorded       bool         `json:"recorded"`
	Queued         []queuedItem `json:"queued"`
	NothingWillRun string       `json:"nothing_will_run"`
	CheckBack      string       `json:"check_back"`
}

func decodeQueued(t *testing.T, raw string) queuedResult {
	t.Helper()
	var out queuedResult
	fixturekit.Must(t, json.Unmarshal([]byte(raw), &out))
	return out
}

func TestAFollowUpSurvivesARestartAndFiresOnce(t *testing.T) {
	t.Parallel()
	k := newFollowUpKit(t)
	scope := k.turn(t, "message_1", "Is the upload unit moving?")
	tool, _ := k.tools(scope)
	if got := intakefixture.Handle(t, tool, `{"after":"2m","reason":"too soon"}`); !strings.Contains(got, `"recorded":false`) {
		t.Fatalf("a follow-up under five minutes was recorded: %s", got)
	}
	got := intakefixture.Handle(t, tool, `{"after":"30m","reason":"Check that the upload unit landed."}`)
	if !strings.Contains(got, `"recorded":true`) {
		t.Fatalf("follow_up: %s", got)
	}
	k.finish(t, "message_1")

	fixturekit.Must(t, k.repo.Close())
	reopened, err := trace.Open(k.root, k.project)
	fixturekit.Must(t, err)
	k.repo = reopened
	t.Cleanup(func() { _ = reopened.Close() })
	pending := k.pending(t)
	if len(pending) != 1 || pending[0].Reason != "Check that the upload unit landed." || pending[0].By != "chief" || pending[0].Turn != "message_1" {
		t.Fatalf("pending follow-ups after reopening: %+v", pending)
	}
	context, err := renderFollowUps(k.repo, k.stream)
	fixturekit.Must(t, err)
	if !strings.Contains(context, "## Pending follow-ups") || !strings.Contains(context, "Check that the upload unit landed.") {
		t.Fatalf("project context: %s", context)
	}

	k.at = k.at.Add(29 * time.Minute)
	k.pass(t)
	if turns := k.followUpTurns(t); len(turns) != 0 {
		t.Fatalf("a follow-up fired before it was due: %+v", turns)
	}
	k.unit(t, "upload", UnitMerged)
	k.at = k.at.Add(2 * time.Minute)
	k.pass(t)
	turns := k.followUpTurns(t)
	if len(turns) != 1 {
		t.Fatalf("follow-up turns: %d", len(turns))
	}
	prompt := turns[0].Request.Prompt
	for _, part := range []string{"You scheduled this check-back", "Check that the upload unit landed.", "unit-upload", "-> merged", stallTurnRule} {
		if !strings.Contains(prompt, part) {
			t.Fatalf("follow-up prompt lacks %q:\n%s", part, prompt)
		}
	}
	if turns[0].Request.Actor != followUpActor || turns[0].Request.Workstream != k.stream {
		t.Fatalf("follow-up request %+v", turns[0].Request.Header)
	}
	if pending := k.pending(t); len(pending) != 0 {
		t.Fatalf("a fired follow-up is still pending: %+v", pending)
	}
	k.at = k.at.Add(time.Hour)
	k.pass(t)
	if turns := k.followUpTurns(t); len(turns) != 1 {
		t.Fatalf("a follow-up fired again: %d turns", len(turns))
	}
}

// A newer follow-up of the same workstream replaces an unfired one, so only
// the newer fires; one naming another workstream is kept beside it.
func TestANewerFollowUpReplacesAnUnfiredOne(t *testing.T) {
	t.Parallel()
	k := newFollowUpKit(t)
	scope := k.turn(t, "message_1", "Check back on the upload work.")
	tool, _ := k.tools(scope)
	intakefixture.Handle(t, tool, `{"after":"10m","reason":"first"}`)
	got := intakefixture.Handle(t, tool, `{"after":"1h","reason":"second"}`)
	if !strings.Contains(got, `"replaced":"first"`) {
		t.Fatalf("the replacing follow_up did not say what it replaced: %s", got)
	}
	intakefixture.Handle(t, tool, `{"after":"2h","reason":"named","workstream":"`+string(k.stream)+`"}`)
	pending := k.pending(t)
	if len(pending) != 2 || pending[0].Reason != "second" || pending[1].Reason != "named" {
		t.Fatalf("pending follow-ups: %+v", pending)
	}
	k.finish(t, "message_1")
	k.at = k.at.Add(15 * time.Minute)
	k.pass(t)
	if turns := k.followUpTurns(t); len(turns) != 0 {
		t.Fatalf("the replaced follow-up fired: %+v", turns)
	}
	k.at = k.at.Add(50 * time.Minute)
	k.pass(t)
	turns := k.followUpTurns(t)
	if len(turns) != 1 || !strings.Contains(turns[0].Request.Prompt, "Reason: second") {
		t.Fatalf("follow-up turns: %+v", turns)
	}
}

func TestAnActionResultReportsQueuedWorkAndAnInertOneSchedulesACheckBack(t *testing.T) {
	t.Parallel()
	k := newFollowUpKit(t)
	k.unit(t, "upload", UnitMerged)
	scope := k.turn(t, "events_1", "Every unit merged.")
	_, action := k.tools(scope)
	inert := decodeQueued(t, intakefixture.Handle(t, action, `{"unit":"upload","to":"ready"}`))
	if !inert.Recorded || len(inert.Queued) != 0 || !strings.Contains(inert.NothingWillRun, "Nothing will run until") || !strings.Contains(inert.NothingWillRun, "1 merged") || inert.CheckBack == "" {
		t.Fatalf("inert action result: %+v", inert)
	}
	intakefixture.Handle(t, action, `{"unit":"upload","to":"ready"}`)
	pending := k.pending(t)
	if len(pending) != 1 || pending[0].By != "service" || pending[0].Reason != autoFollowUpReason || pending[0].Turn != "events_1" || !pending[0].Due.Equal(k.at.Add(config.DefaultStallAfter)) {
		t.Fatalf("check-backs after two inert actions: %+v", pending)
	}
	k.finish(t, "events_1")

	k.unit(t, "resume", UnitReady)
	scope = k.turn(t, "events_2", "Unit resume is ready.")
	_, action = k.tools(scope)
	queued := decodeQueued(t, intakefixture.Handle(t, action, `{"unit":"resume","to":"ready"}`))
	if len(queued.Queued) != 1 || queued.Queued[0].Role != masonRole || queued.Queued[0].Unit != "resume" || queued.NothingWillRun != "" || queued.CheckBack != "" {
		t.Fatalf("queued action result: %+v", queued)
	}
	k.finish(t, "events_2")
	recs, err := readFollowUps(k.repo, k.stream)
	fixturekit.Must(t, err)
	if recs.turns["events_2"] {
		t.Fatal("an action that queued work scheduled a check-back")
	}

	other := newFollowUpKit(t)
	other.unit(t, "upload", UnitMerged)
	scope = other.turn(t, "events_1", "Every unit merged.")
	schedule, action := other.tools(scope)
	intakefixture.Handle(t, action, `{"unit":"upload","to":"ready"}`)
	intakefixture.Handle(t, schedule, `{"after":"2h","reason":"Check the final review started.","workstream":"`+string(other.stream)+`"}`)
	pending = other.pending(t)
	if len(pending) != 1 || pending[0].By != "chief" {
		t.Fatalf("pending after the chief's own follow_up: %+v", pending)
	}
}

// A completed turn on a stall notice that ended without queuing work, a
// follow-up or an attention item gets the service's check-back once; one
// whose action queued work gets none.
func TestAStallNoticeTurnThatEndsWithoutActionGetsACheckBack(t *testing.T) {
	t.Parallel()
	k := newFollowUpKit(t)
	k.unit(t, "upload", UnitContested)
	k.turn(t, "events_1", "Unit upload has made no progress for 50 minutes. "+stallTurnRule)
	k.finish(t, "events_1")
	k.pass(t)
	k.pass(t)
	pending := k.pending(t)
	if len(pending) != 1 || pending[0].Reason != stallFollowUpReason || pending[0].Turn != "events_1" || pending[0].By != "service" {
		t.Fatalf("check-backs after a silent stall turn: %+v", pending)
	}
	recs, err := readFollowUps(k.repo, k.stream)
	fixturekit.Must(t, err)
	if recs.revision[pending[0].ID] != 1 {
		t.Fatalf("the check-back was recorded %d times", recs.revision[pending[0].ID])
	}

	acted := newFollowUpKit(t)
	acted.unit(t, "upload", UnitReady)
	scope := acted.turn(t, "events_1", "Unit upload has made no progress for 50 minutes. "+stallTurnRule)
	_, action := acted.tools(scope)
	if got := decodeQueued(t, intakefixture.Handle(t, action, `{"unit":"upload","to":"ready"}`)); len(got.Queued) == 0 {
		t.Fatalf("the stall turn's action queued nothing: %+v", got)
	}
	acted.finish(t, "events_1")
	acted.pass(t)
	if pending := acted.pending(t); len(pending) != 0 {
		t.Fatalf("a stall turn that queued work got a check-back: %+v", pending)
	}
}

func followUpProfile() (coreadapter.Profile, error) {
	return coreadapter.Profile{Name: "default", Backend: "claude", Model: "test"}, nil
}

func (k *followUpKit) endFollowUpTurn(t *testing.T, turn string, cancelled bool) {
	t.Helper()
	ctx := context.Background()
	q, err := k.repo.ClaimTurn(ctx, k.stream, trace.Manager, "token-"+turn, filepath.Join(t.TempDir(), "session"), k.at)
	fixturekit.Must(t, err)
	if q.Request.TurnID != turn {
		t.Fatalf("claimed turn %s, want %s", q.Request.TurnID, turn)
	}
	h := q.Request.Header
	h.Schema, h.ID, h.At = "osmia.trace.turn-response", trace.EventID(q.Request.ID, "response"), k.at
	result := coreadapter.SessionResult{SessionDirectory: q.Claim.SessionDirectory, StartedAt: q.Claim.At, IsError: true, Cancelled: cancelled}
	fixturekit.Must(t, k.repo.CaptureTurn(ctx, q.Claim.Token, trace.TurnResponse{Header: h, AgentID: trace.Manager, ThreadID: q.Request.ThreadID, TurnID: turn, RequestID: q.Request.ID, RequestRevision: q.Request.Revision, Result: result, Failure: "the backend failed"}))
	fixturekit.Must(t, k.repo.CompleteTurn(ctx, k.stream, trace.Manager, turn, q.Claim.Token, k.at))
}

func TestAWholeWorkgroupFollowUpFiresPastAnArchivedMember(t *testing.T) {
	t.Parallel()
	for _, cross := range []bool{false, true} {
		t.Run(fmt.Sprint("cross=", cross), func(t *testing.T) {
			t.Parallel()
			wholeWorkgroupFollowUpPastArchivedMember(t, cross)
		})
	}
}

func wholeWorkgroupFollowUpPastArchivedMember(t *testing.T, cross bool) {
	s, chief, g := workgroupFeedFixture(t, cross, 2)
	ctx := context.Background()
	host, gone := g.Members[0], fixturekit.Quiet
	if host == gone {
		t.Fatalf("the archived member %s hosts the chief", gone)
	}
	member := s.projects[len(s.projects)-1].repository
	f := followUp{ID: followUpID(trace.Manager, ""), Reason: stallFollowUpReason, ScheduledAt: fixturekit.DemoStart, Due: fixturekit.DemoStart.Add(time.Minute), By: "service", Turn: "events_1", State: followUpPending}
	fixturekit.Must(t, recordFollowUp(ctx, chief, host, 1, f, followUpActor, "test", fixturekit.DemoStart))
	fixturekit.Must(t, s.store.SetArchived(runtime.Archive{Workgroup: g.ID, Project: member.Project(), Workstream: gone, State: AbandonedState, Title: "Archived member", Resolved: true, ResolutionEvidence: "cancelled", ArchivedAt: fixturekit.DemoStart, CleanupStartedAt: fixturekit.DemoStart, CleanedAt: fixturekit.DemoStart}))
	_, err := member.MarkWorkstreamTraceForDeletion(gone)
	fixturekit.Must(t, err)
	fixturekit.Must(t, trace.FinishWorkstreamTraceDeletion(s.cfg.Root, member.Project(), gone))

	now := fixturekit.DemoStart.Add(time.Hour)
	pass := func(archived func(string) (runtime.Archive, bool)) {
		t.Helper()
		fu := followUps{now: func() time.Time { return now }, window: config.DefaultStallAfter, profile: followUpProfile, archived: archived}
		fixturekit.Must(t, fu.pass(ctx, chief, []config.WorkstreamID{host}))
	}
	prompts := func() []string {
		t.Helper()
		th, err := chief.Thread(host, trace.Manager)
		fixturekit.Must(t, err)
		var out []string
		for _, q := range th.Turns {
			if strings.HasPrefix(q.Request.TurnID, followUpTurnPrefix) {
				out = append(out, q.Request.Prompt)
			}
		}
		return out
	}
	pass(s.archivedWorkstream)
	got := prompts()
	if len(got) != 1 || !strings.Contains(got[0], "Workstream "+string(gone)+": archived ("+AbandonedState+", cancelled).") || !strings.Contains(got[0], "Workstream "+string(host)+" is ") {
		t.Fatalf("whole-workgroup follow-up prompts: %q", got)
	}
	recs, err := readFollowUps(chief, host)
	fixturekit.Must(t, err)
	if len(recs.pending()) != 0 {
		t.Fatalf("the follow-up is still pending: %+v", recs.pending())
	}
	if !cross {
		return
	}

	named := followUp{ID: followUpID(trace.Manager, gone), Workstream: gone, Reason: "Check the archived member.", ScheduledAt: now, Due: now.Add(time.Minute), By: "chief", Turn: "message_1", State: followUpPending}
	fixturekit.Must(t, recordFollowUp(ctx, chief, host, 1, named, followUpActor, "test", now))
	now = now.Add(time.Hour)
	pass(nil)
	got = prompts()
	if len(got) != 2 || !strings.Contains(got[1], "Workstream "+string(gone)+": archived (retired from the workgroup).") {
		t.Fatalf("named follow-up prompts: %q", got)
	}

	fixturekit.Must(t, s.store.ClearArchived(gone))
	named.ScheduledAt, named.Due = now, now.Add(time.Minute)
	fixturekit.Must(t, recordFollowUp(ctx, chief, host, 3, named, followUpActor, "test", now))
	now = now.Add(time.Hour)
	pass(nil)
	got = prompts()
	if len(got) != 3 || !strings.Contains(got[2], "Workstream "+string(gone)+": archived (its trace was removed).") {
		t.Fatalf("follow-up prompts without an archive record: %q", got)
	}
}

// A follow-up that cannot fire does not hold back a later one of the same
// thread, and fires on a later pass.
func TestAFailingFollowUpDoesNotBlockALaterOne(t *testing.T) {
	t.Parallel()
	k := newFollowUpKit(t)
	scope := k.turn(t, "message_1", "Check back twice.")
	tool, _ := k.tools(scope)
	intakefixture.Handle(t, tool, `{"after":"10m","reason":"whole thread"}`)
	intakefixture.Handle(t, tool, `{"after":"20m","reason":"named","workstream":"`+string(k.stream)+`"}`)
	k.finish(t, "message_1")
	k.at = k.at.Add(time.Hour)
	calls := 0
	f := followUps{now: k.now, window: config.DefaultStallAfter, profile: func() (coreadapter.Profile, error) {
		calls++
		if calls == 1 {
			return coreadapter.Profile{}, errors.New("no profile")
		}
		return followUpProfile()
	}}
	if err := f.pass(context.Background(), k.repo, []config.WorkstreamID{k.stream}); err == nil || !strings.Contains(err.Error(), "no profile") {
		t.Fatalf("pass error: %v", err)
	}
	if turns, pending := k.followUpTurns(t), k.pending(t); len(turns) != 1 || len(pending) != 1 {
		t.Fatalf("after one failing follow-up: %d turns, pending %+v", len(turns), pending)
	}
	fixturekit.Must(t, f.pass(context.Background(), k.repo, []config.WorkstreamID{k.stream}))
	if turns, pending := k.followUpTurns(t), k.pending(t); len(turns) != 2 || len(pending) != 0 {
		t.Fatalf("after the retry: %d turns, pending %+v", len(turns), pending)
	}
}

// A follow-up whose turn ended failed or interrupted fires again after a
// growing delay, at most maxFollowUpRefires times; a pass that finds it
// already put back records nothing more.
func TestAFollowUpWhoseTurnFailedFiresAgainBounded(t *testing.T) {
	t.Parallel()
	k := newFollowUpKit(t)
	scope := k.turn(t, "message_1", "Check back.")
	tool, _ := k.tools(scope)
	intakefixture.Handle(t, tool, `{"after":"10m","reason":"Check the upload unit."}`)
	k.finish(t, "message_1")
	k.at = k.at.Add(11 * time.Minute)
	k.pass(t)
	for n := 1; n <= maxFollowUpRefires; n++ {
		turns := k.followUpTurns(t)
		if len(turns) != n {
			t.Fatalf("fire %d: %d follow-up turns", n, len(turns))
		}
		k.endFollowUpTurn(t, turns[n-1].Request.TurnID, n == 2)
		k.pass(t)
		k.pass(t)
		pending := k.pending(t)
		if len(pending) != 1 || pending[0].Refires != n || !pending[0].Due.Equal(k.at.Add(followUpRefireDelay(n))) || pending[0].Reason != "Check the upload unit." {
			t.Fatalf("after failed fire %d: pending %+v", n, pending)
		}
		k.at = k.at.Add(followUpRefireDelay(n) - time.Second)
		k.pass(t)
		if got := len(k.followUpTurns(t)); got != n {
			t.Fatalf("refire %d came early: %d turns", n, got)
		}
		k.at = k.at.Add(time.Second)
		k.pass(t)
	}
	turns := k.followUpTurns(t)
	if len(turns) != maxFollowUpRefires+1 || !strings.Contains(turns[len(turns)-1].Request.Prompt, "Reason: Check the upload unit.") {
		t.Fatalf("follow-up turns after the refires: %d", len(turns))
	}
	k.endFollowUpTurn(t, turns[len(turns)-1].Request.TurnID, false)
	k.at = k.at.Add(time.Hour)
	k.pass(t)
	if got, pending := len(k.followUpTurns(t)), k.pending(t); got != maxFollowUpRefires+1 || len(pending) != 0 {
		t.Fatalf("the follow-up fired past its bound: %d turns, pending %+v", got, pending)
	}
}
