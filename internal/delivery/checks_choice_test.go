package delivery

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/kpenfound/osmia/internal/api"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/pulls"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	"github.com/kpenfound/osmia/internal/trace"
)

const secondPushedCommit = "4444444444444444444444444444444444444444"
const secondReviewedCommit = "3333333333333333333333333333333333333333"

// assemble moves the fixture workstream to assembled, where a final review
// and the pull request's checks are watched.
func (f *watchFixture) assemble() {
	f.t.Helper()
	_, err := f.repo.Transact(context.Background(), trace.Transaction{Transition: trace.Transition{
		Header:  trace.Header{Schema: "osmia.trace.transition", Version: trace.Version, ID: "fixture-assembled", Revision: 1, Project: f.repo.Project(), Workstream: stream, At: f.clock.Now(), Actor: ownerActor, Cause: "fixture"},
		Subject: trace.FeatureSubject, To: AssembledState, Reason: "fixture"}})
	fixturekit.Must(f.t, err)
}

// secondPush records push 2, which supersedes push 1, and returns the report
// of the final review that made it.
func (f *watchFixture) secondPush() FinalReport {
	f.t.Helper()
	publisher := &Publisher{S: f.reviewer.S, Repository: f.repo}
	fixturekit.Must(f.t, publisher.RecordPublication(context.Background(), stream, DeliveryPublication{Push: 2, Review: 2, Operation: "review-2", Status: PublicationOpened, Reviewed: secondReviewedCommit, Commit: secondPushedCommit, PullRequest: 7, URL: "https://github.com/acme/widgets/pull/7",
		Upstream: "acme/widgets", Fork: "me/widgets", Branch: featureBranch(stream), Base: "main", Draft: true, PushedAt: f.clock.Now()}))
	return FinalReport{Review: 2, Operation: "review-2", Commit: secondReviewedCommit}
}

// chief is a chief-of-staff turn's scope over the fixture workstream.
func (f *watchFixture) chief(turn string) coreadapter.Scope {
	f.t.Helper()
	if f.scope == nil {
		_, err := f.repo.EnsureChiefOfStaff(context.Background(), stream, fixturekit.DemoStart, fixturekit.ServiceActor)
		fixturekit.Must(f.t, err)
		scope := fixturekit.ClaimTurn(f.t, f.repo, f.t.TempDir(), stream, trace.ChiefOfStaff, trace.ChiefOfStaff, turn, fixturekit.DemoStart)
		f.scope = &scope
	}
	return *f.scope
}

func (f *watchFixture) call(tool coreadapter.Tool, input string) map[string]any {
	f.t.Helper()
	out, err := tool.Handle(context.Background(), json.RawMessage(input))
	fixturekit.Must(f.t, err)
	var v map[string]any
	fixturekit.Must(f.t, json.Unmarshal(out, &v))
	return v
}

func (f *watchFixture) load() func() *Controller {
	return func() *Controller { return f.reviewer.S }
}

func (f *watchFixture) entry() (InboxEntry, bool) {
	f.t.Helper()
	e, open, err := ChecksChoiceEntry(f.repo, trace.WorkstreamStatus{Workstream: stream})
	fixturekit.Must(f.t, err)
	return e, open
}

// offer has the chief of staff ask the owner about push n.
func (f *watchFixture) offer(push int, note string) map[string]any {
	f.t.Helper()
	tool := ChecksChoiceToolOf(f.load(), f.repo, f.chief("offer"))
	return f.call(tool, `{"push":`+itoa(push)+`,"note":"`+note+`"}`)
}

func itoa(n int) string { b, _ := json.Marshal(n); return string(b) }

// answer is the owner's answer to the entry the inbox lists, as a client sends it.
func (f *watchFixture) answer(e InboxEntry, decision string) (ChecksChoiceResponse, *api.APIError) {
	f.t.Helper()
	raw, err := json.Marshal(e.Answer.Body)
	fixturekit.Must(f.t, err)
	var req ChecksChoiceRequest
	fixturekit.Must(f.t, json.Unmarshal(raw, &req))
	req.Decision = decision
	return f.reviewer.S.AnswerChecksChoice(context.Background(), string(stream), req)
}

func TestChiefPollsNowAndRerunsOnlyTheCurrentPush(t *testing.T) {
	t.Parallel()
	f := newWatchFixture(t)
	f.assemble()
	f.api.set(f.loaded(), f.pending(2, 40*time.Second))
	poll := ChecksToolOf(f.load(), f.repo, f.chief("poll"))

	// The poll is made at once, inside the wait after the push.
	got := f.call(poll, `{"action":"poll","push":1}`)
	if got["done"] != true || got["outcome"] != "pending" || f.api.readCount() != 1 || f.api.commits[0] != "acme/widgets@"+pushedCommit {
		t.Fatalf("poll %v, reads %v", got, f.api.commits)
	}
	if len(f.runs()) != 0 {
		t.Fatalf("a poll recorded a result: %+v", f.runs())
	}

	got = f.call(poll, `{"action":"rerun","push":1}`)
	calls := f.rerunCalls()
	if got["done"] != true || len(calls) != 1 || !strings.HasPrefix(calls[0], "cloud checks rerun --all --pr 7\n") || !strings.Contains(calls[0], "token="+cloudToken+"\n") {
		t.Fatalf("rerun %v, calls %q", got, calls)
	}

	// Push 2 supersedes push 1: a request naming push 1 is refused, naming
	// the current push, and does nothing.
	f.clock.Advance(time.Hour)
	f.secondPush()
	reads := f.api.readCount()
	for _, action := range []string{"poll", "rerun"} {
		got = f.call(poll, `{"action":"`+action+`","push":1}`)
		reason, _ := got["reason"].(string)
		if got["done"] != false || !strings.Contains(reason, "push 1 is superseded") || !strings.Contains(reason, "the current push is push 2") || !strings.Contains(reason, secondPushedCommit) {
			t.Fatalf("%s of a superseded push: %v", action, got)
		}
	}
	if f.api.readCount() != reads || len(f.rerunCalls()) != 1 {
		t.Fatalf("a refused request acted: reads %d, reruns %d", f.api.readCount()-reads, len(f.rerunCalls()))
	}
	if got = f.call(poll, `{"action":"poll","push":2}`); got["done"] != true || f.api.commits[len(f.api.commits)-1] != "acme/widgets@"+secondPushedCommit {
		t.Fatalf("poll of the current push: %v", got)
	}
}

func TestChecksToolsAreRefusedForOtherRoles(t *testing.T) {
	t.Parallel()
	f := newWatchFixture(t)
	f.assemble()
	scope := f.chief("other")
	scope.Role = "mason"
	for _, tool := range []coreadapter.Tool{ChecksToolOf(f.load(), f.repo, scope), ChecksChoiceToolOf(f.load(), f.repo, scope)} {
		input := `{"action":"rerun","push":1}`
		if tool.Name == ChecksChoiceToolName {
			input = `{"push":1,"note":"nothing"}`
		}
		// A turn whose role is not the chief of staff's is denied before the
		// call reaches the service, or refused by it.
		out, err := tool.Handle(context.Background(), json.RawMessage(input))
		if err == nil {
			var got map[string]any
			fixturekit.Must(t, json.Unmarshal(out, &got))
			if got["done"] != false {
				t.Fatalf("%s: %v", tool.Name, got)
			}
		}
	}
	if f.api.readCount() != 0 || len(f.rerunCalls()) != 0 {
		t.Fatalf("a refused role acted: reads %d, reruns %v", f.api.readCount(), f.rerunCalls())
	}
	if _, open := f.entry(); open {
		t.Fatal("a refused role offered the choice")
	}
}

func TestOwnerSeesTheChoiceAndKeepsWaiting(t *testing.T) {
	t.Parallel()
	f := newWatchFixture(t)
	f.assemble()
	f.afterWait()
	f.api.set()
	f.mustWait()

	if got := f.offer(1, "Cloud+Checks+are+not+set+up+here"); got["done"] != true {
		t.Fatalf("offer %v", got)
	}
	e, open := f.entry()
	if !open || e.Kind != InboxChecksChoice || len(e.Options) != 2 || e.Options[0] != ChecksChoiceKeepWaiting || e.Options[1] != ChecksChoiceContinue ||
		e.Answer.Body["push"] != 1 || e.Answer.Body["pull_request"] != 7 || e.Answer.Body["pushed_commit"] != pushedCommit || e.Answer.Body["operation"] != "review" ||
		!strings.Contains(e.Question, "push 1") || !strings.Contains(e.Question, pushedCommit) || !strings.Contains(e.Question, "pull request #7") {
		t.Fatalf("entry %+v", e)
	}

	// The chief of staff alone does not continue the final review.
	f.clock.Advance(10 * time.Minute)
	f.mustWait()
	if len(f.runs()) != 0 {
		t.Fatalf("a result was recorded: %+v", f.runs())
	}

	reads := f.api.readCount()
	out, apiErr := f.answer(e, ChecksChoiceKeepWaiting)
	if apiErr != nil || out.Decision != ChecksChoiceKeepWaiting {
		t.Fatalf("answer %+v: %v", out, apiErr)
	}
	if _, open := f.entry(); open {
		t.Fatal("the entry stays after the answer")
	}
	// The watch still polls, and records no check result.
	f.clock.Advance(10 * time.Minute)
	f.mustWait()
	if f.api.readCount() != reads+1 || len(f.runs()) != 0 {
		t.Fatalf("reads %d (was %d), runs %+v", f.api.readCount(), reads, f.runs())
	}
	// The same answer again changes nothing; a different one is refused.
	if _, apiErr := f.answer(e, ChecksChoiceKeepWaiting); apiErr != nil {
		t.Fatalf("repeated answer: %v", apiErr)
	}
	if _, apiErr := f.answer(e, ChecksChoiceContinue); apiErr == nil || apiErr.Code != Conflict {
		t.Fatalf("answer after the question closed: %v", apiErr)
	}
	f.mustWait()
}

func TestOwnerContinuesWithoutCheckResultsForThatPushOnly(t *testing.T) {
	t.Parallel()
	f := newWatchFixture(t)
	f.assemble()
	f.afterWait()
	f.api.set()
	f.mustWait()
	f.offer(1, "Cloud+Checks+never+ran")
	e, _ := f.entry()
	out, apiErr := f.answer(e, ChecksChoiceContinue)
	if apiErr != nil || out.Decision != ChecksChoiceContinue || out.Push != 1 {
		t.Fatalf("answer %+v: %v", out, apiErr)
	}
	if _, open := f.entry(); open {
		t.Fatal("the entry stays after the answer")
	}

	f.restart()
	run := f.mustComplete()
	if run.Status != ChecksContinued || run.ContinuedBy != "owner" || run.Source != api.CheckSourcePullRequest || run.Push != 1 || run.PushedCommit != pushedCommit || run.Commit != reviewedCommit || run.Counts != nil || run.Run != nil {
		t.Fatalf("run %+v", run)
	}
	evidence := FinalCheckEvidence(run)
	for _, want := range []string{"No check results exist", "the owner chose to continue", "pull request #7", pushedCommit} {
		if !strings.Contains(evidence, want) {
			t.Fatalf("evidence lacks %q:\n%s", want, evidence)
		}
	}
	// A restart and another read return the same record, once.
	f.restart()
	if again := f.mustComplete(); again.Status != ChecksContinued || len(f.runs()) != 1 {
		t.Fatalf("again %+v, runs %+v", again, f.runs())
	}

	// Push 2 does not inherit the choice, before or after a restart.
	f.clock.Advance(time.Hour)
	second := f.secondPush()
	f.report = second
	for range 2 {
		f.mustWait()
		f.clock.Advance(FinalChecksWait)
		f.restart()
		f.mustWait()
	}
	if runs := f.runs(); len(runs) != 1 {
		t.Fatalf("push 2 recorded %+v", runs)
	}
}

func TestSupersededPushClosesTheChoiceAndRefusesItsAnswer(t *testing.T) {
	t.Parallel()
	f := newWatchFixture(t)
	f.assemble()
	f.offer(1, "Nothing+was+posted")
	e, open := f.entry()
	if !open {
		t.Fatal("no entry")
	}

	f.clock.Advance(time.Hour)
	second := f.secondPush()
	if _, open := f.entry(); open {
		t.Fatal("the entry about push 1 stays after push 2")
	}
	if _, apiErr := f.answer(e, ChecksChoiceContinue); apiErr == nil || apiErr.Code != Conflict || !strings.Contains(apiErr.Message, "push 2") {
		t.Fatalf("answer about a superseded push: %v", apiErr)
	}
	// The offer is closed as obsolete, and push 2's final review gets nothing from it.
	record, _, err := ReadChecksChoice(f.repo, stream, 1)
	fixturekit.Must(t, err)
	if record.State != ChecksChoiceObsolete {
		t.Fatalf("offer about push 1: %+v", record)
	}
	f.report = second
	f.clock.Advance(FinalChecksWait)
	f.mustWait()
	if len(f.runs()) != 0 {
		t.Fatalf("runs %+v", f.runs())
	}
	// Push 2 needs its own offer, and its own answer.
	if got := f.offer(1, "Old+push"); got["done"] != false {
		t.Fatalf("offer about push 1: %v", got)
	}
	if got := f.offer(2, "Still+nothing"); got["done"] != true {
		t.Fatalf("offer about push 2: %v", got)
	}
	if e2, open := f.entry(); !open || e2.Answer.Body["push"] != 2 || e2.Answer.Body["pushed_commit"] != secondPushedCommit {
		t.Fatalf("entry %+v", e2)
	}
	if _, apiErr := f.answer(e, ChecksChoiceContinue); apiErr == nil || apiErr.Code != Conflict {
		t.Fatalf("old entry answered after push 2 was offered: %v", apiErr)
	}
}

func TestChoiceSurvivesARestartOnceAndClosesWhenAResultCompletes(t *testing.T) {
	t.Parallel()
	f := newWatchFixture(t)
	f.assemble()
	f.afterWait()
	f.api.set()
	f.mustWait()
	f.offer(1, "Waiting")
	if got := f.offer(1, "Again"); got["done"] != false || !strings.Contains(got["reason"].(string), "already asked") {
		t.Fatalf("second offer %v", got)
	}
	f.restart()
	e, open := f.entry()
	if !open {
		t.Fatal("a restart lost the entry")
	}
	if got := f.offer(1, "Again"); got["done"] != false {
		t.Fatalf("offer after a restart %v", got)
	}

	// A completed result closes the question; answering it is then refused.
	f.clock.Advance(10 * time.Minute)
	f.api.set(f.loaded(), f.done(3, "success", 5*time.Minute, counts(3, 3, 0, 0)))
	if run := f.mustComplete(); run.Status != "passed" {
		t.Fatalf("run %+v", run)
	}
	if _, open := f.entry(); open {
		t.Fatal("the entry stays after a result completed")
	}
	if _, apiErr := f.answer(e, ChecksChoiceContinue); apiErr == nil || apiErr.Code != Conflict {
		t.Fatalf("answer after a result: %v", apiErr)
	}
	if got := f.offer(1, "Late"); got["done"] != false {
		t.Fatalf("offer after a result: %v", got)
	}
}

func TestAnswerNeedsTheEntrysPin(t *testing.T) {
	t.Parallel()
	f := newWatchFixture(t)
	f.assemble()
	f.offer(1, "Waiting")
	e, _ := f.entry()
	for name, change := range map[string]func(*ChecksChoiceRequest){
		"push":      func(r *ChecksChoiceRequest) { r.Push = 2 },
		"commit":    func(r *ChecksChoiceRequest) { r.PushedCommit = reviewedCommit },
		"operation": func(r *ChecksChoiceRequest) { r.Operation = "other" },
		"offer":     func(r *ChecksChoiceRequest) { r.Offer++ },
		"pull":      func(r *ChecksChoiceRequest) { r.PullRequest = 8 },
	} {
		raw, _ := json.Marshal(e.Answer.Body)
		var req ChecksChoiceRequest
		fixturekit.Must(t, json.Unmarshal(raw, &req))
		req.Decision = ChecksChoiceContinue
		change(&req)
		if _, apiErr := f.reviewer.S.AnswerChecksChoice(context.Background(), string(stream), req); apiErr == nil || apiErr.Code != Conflict {
			t.Fatalf("%s: %v", name, apiErr)
		}
	}
	if _, apiErr := f.reviewer.S.AnswerChecksChoice(context.Background(), string(stream), ChecksChoiceRequest{Decision: "approve"}); apiErr == nil || apiErr.Code != Validation {
		t.Fatalf("unknown decision: %v", apiErr)
	}
	f.afterWait()
	f.api.set()
	f.mustWait()
	if len(f.runs()) != 0 {
		t.Fatalf("runs %+v", f.runs())
	}
}

var _ = pulls.CommitStatus{}
