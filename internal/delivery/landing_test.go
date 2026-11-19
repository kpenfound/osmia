package delivery

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/reconcile"
	"github.com/kpenfound/osmia/internal/router"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	"github.com/kpenfound/osmia/internal/trace"
)

const judgmentKey = "judgment_ab12"

func byOwner() LandDecider { return LandDecider{} }
func byJev() LandDecider   { return LandDecider{By: trace.DecidedByJev, Judgment: judgmentKey} }

func TestLandingFastForwardsTheBaseToTheSignedCommitAndRecordsIt(t *testing.T) {
	t.Parallel()
	f := newLandFixture(t)
	result := f.land(byOwner())
	if result.Outcome != Landed || result.Commit != f.signed {
		t.Fatalf("the landing is %+v", result)
	}
	if got := f.upstreamMain(); got != f.signed {
		t.Fatalf("the base branch is at %s, want exactly the signed commit %s", got, f.signed)
	}
	if merges := f.git("-C", f.upstream, "rev-list", "--merges", f.base+".."+f.signed); merges != "" {
		t.Fatalf("the landing created merge commits: %s", merges)
	}
	if commits := strings.Fields(f.git("-C", f.upstream, "rev-list", f.base+".."+f.upstreamMain())); len(commits) != 2 {
		t.Fatalf("%d commits reached the base branch, want the feature branch's 2", len(commits))
	}
	if got := f.upstreamBranch(); got != f.signed {
		t.Fatalf("the feature branch of the push repository is at %s, want %s", got, f.signed)
	}
	if f.pulls.Creates != 0 || f.pulls.Updates != 0 || f.pulls.Readies != 0 {
		t.Fatalf("the pull request host was written: %d created, %d updated, %d marked ready", f.pulls.Creates, f.pulls.Updates, f.pulls.Readies)
	}

	landing, found, err := ReadDeliveryLanding(f.repo, stream)
	fixturekit.Must(t, err)
	if !found || landing.Commit != f.signed || landing.Reviewed != f.reviewed || landing.BaseBefore != f.base || landing.Confirmed != f.signed ||
		landing.Remote != "upstream" || landing.Upstream != "acme/widgets" || landing.BaseBranch != "main" || landing.Branch != f.branch || landing.DecidedBy != trace.DecidedByOwner || landing.At.IsZero() {
		t.Fatalf("the landing record is %+v", landing)
	}
	if f.feature() != DeliveredState || f.subject(LandingSubject) != "landed-1" {
		t.Fatalf("the workstream is %s and its landing %s", f.feature(), f.subject(LandingSubject))
	}
	// The landing resolves the workstream as a merge of its branch does: the
	// records every dependency edge, archive and presentation read.
	docs, err := trace.Read[trace.Document](f.repo, stream)
	fixturekit.Must(t, err)
	evidence, err := trace.MemberResolutionFrom(docs)
	fixturekit.Must(t, err)
	if got := evidence.Resolution(f.feature()); got != "merged" {
		t.Fatalf("the delivered workstream resolves as %q", got)
	}
	if merge, merged, err := ReadDeliveryMerge(f.repo, stream); err != nil || !merged || merge.Upstream.Commit != f.signed || merge.Upstream.Branch != "main" {
		t.Fatalf("the merge evidence is %+v %v %v", merge, merged, err)
	}
	if finished, err := OwnPullRequestFinished(f.repo, stream); err != nil || finished {
		t.Fatalf("a pull request merge was reported: %v %v", finished, err)
	}
	delivered := f.transitions(DeliveredState)
	if len(delivered) != 1 || delivered[0].DecidedBy != trace.DecidedByOwner || delivered[0].Subject != trace.FeatureSubject {
		t.Fatalf("the delivered transitions are %+v", delivered)
	}
	if f.notices("landed on main of acme/widgets") != 1 {
		t.Fatal("the chief of staff has no notice of the landing")
	}
}

func TestLandingDecidedByJevIsRecordedAsJevsGateDecision(t *testing.T) {
	t.Parallel()
	f := newLandFixture(t)
	if result := f.land(byJev()); result.Outcome != Landed {
		t.Fatalf("the landing is %+v", result)
	}
	delivered := f.transitions(DeliveredState)
	if len(delivered) != 1 || delivered[0].DecidedBy != trace.DecidedByJevGate || delivered[0].Judgment != judgmentKey {
		t.Fatalf("the delivered transitions are %+v", delivered)
	}
	landing, _, err := ReadDeliveryLanding(f.repo, stream)
	fixturekit.Must(t, err)
	if landing.DecidedBy != trace.DecidedByJev || landing.Judgment != judgmentKey {
		t.Fatalf("the landing record is %+v", landing)
	}
}

func TestLandingIsRefusedWithoutPassedChecksOnTheSignedCommitOrWithoutOptIn(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name    string
		options landOptions
		reason  string
	}{
		{"checks never ran", landOptions{checks: "none"}, "no check result is recorded"},
		{"checks failed", landOptions{checks: "failed"}, "did not pass"},
		{"checks passed on another commit", landOptions{checks: "other"}, "no check result is recorded"},
		{"the project delivers by pull request", landOptions{delivery: config.DeliveryPullRequest}, "delivers by pull request"},
		{"the project sets no delivery", landOptions{delivery: "x"}, "delivers by pull request"},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			f := newLandFixture(t, c.options)
			if c.options.delivery == "x" {
				f.cfg.Project.Delivery = ""
			}
			result, err := f.controller().LandWorkstream(context.Background(), f.repo, stream, LandRequest{Decider: byOwner(), AcceptBlocking: true})
			fixturekit.Must(t, err)
			if result.Outcome != LandingRefusedBeforePush || !strings.Contains(result.Reason, c.reason) {
				t.Fatalf("the landing is %+v, want it refused for %q", result, c.reason)
			}
			if got := f.upstreamMain(); got != f.base {
				t.Fatalf("the base branch moved to %s", got)
			}
			if ops, err := f.repo.Operations(stream); err != nil || len(ops) != 0 {
				t.Fatalf("a refused landing recorded operations %v %v", ops, err)
			}
			if f.feature() != AssembledState || f.stepCount("land-recorded") != 0 {
				t.Fatalf("the workstream is %s", f.feature())
			}
		})
	}
}

func TestAProjectThatDeliversByPullRequestNeverReachesTheLandingPush(t *testing.T) {
	t.Parallel()
	f := newLandFixture(t)
	op, err := f.lander().Request(context.Background(), stream, LandInput{Landing: 1, Push: 1, Review: 1, Reviewed: f.reviewed, Commit: f.signed, Decider: byOwner()})
	fixturekit.Must(t, err)
	f.cfg.Project.Delivery = config.DeliveryPullRequest
	result, err := f.lander().Apply(context.Background(), op)
	fixturekit.Must(t, err)
	if result.Outcome != LandOutcomeFailed || !strings.Contains(result.Evidence, "never lands") {
		t.Fatalf("the operation is %+v", result)
	}
	if f.upstreamMain() != f.base || f.stepCount("land-recorded") != 0 {
		t.Fatal("something was pushed to the base branch")
	}
}

func TestARouterHoldPushesNothingAndTheLandingLandsOnceItClears(t *testing.T) {
	t.Parallel()
	holds := map[string]func(f *landFixture) (clear func()){
		"a pause": func(f *landFixture) func() {
			f.router.Paused = func(context.Context, router.Exit) (string, error) { return "work is paused (owner): later", nil }
			return func() { f.router.Paused = nil }
		},
		"a spent budget": func(f *landFixture) func() {
			f.router.Stops = func(context.Context, router.Exit) (string, error) { return "the daily budget is spent", nil }
			return func() { f.router.Stops = nil }
		},
		"another operation in flight": func(f *landFixture) func() {
			f.busy()
			return f.finishBusy
		},
	}
	for name, hold := range holds {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f := newLandFixture(t)
			clear := hold(f)
			result := f.land(byOwner())
			if result.Outcome != LandingHeld {
				t.Fatalf("the landing is %+v, want it held", result)
			}
			if f.upstreamMain() != f.base || f.stepCount("land-recorded") != 0 {
				t.Fatal("a held landing pushed something")
			}
			if f.feature() != AssembledState {
				t.Fatalf("a held landing left the workstream %s", f.feature())
			}
			if f.documents(LandingIntentDocument) != 0 || f.pulls.Readies != 0 || f.notices("refused") != 0 {
				t.Fatal("a held landing was treated as a refusal")
			}

			clear()
			// The same landing is applied: the recorded operation, not a new one.
			again := f.land(byOwner())
			if again.Outcome != Landed || f.upstreamMain() != f.signed {
				t.Fatalf("after the hold cleared the landing is %+v", again)
			}
			if ops, err := f.repo.Operations(stream); err != nil || len(landingOperationsOf(ops)) != 1 {
				t.Fatalf("the operations are %v %v", ops, err)
			}
		})
	}
}

func TestABaseThatMovedIsNotAFallbackAndTheLandingIsRetriedOnTheRebasedCommit(t *testing.T) {
	t.Parallel()
	for name, move := range map[string]func(f *landFixture) string{
		// The base moved before the landing fetched it.
		"before the fetch": func(f *landFixture) string { return f.advanceUpstream("someone-else.txt") },
		// The base moved after the landing fetched it, so the upstream rejects
		// the push.
		"before the push": func(f *landFixture) string {
			var moved string
			f.hook("land-recorded", func() error { moved = f.advanceUpstream("someone-else.txt"); return nil })
			f.moved = func() string { return moved }
			return ""
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f := newLandFixture(t)
			moved := move(f)
			result := f.land(byOwner())
			if result.Outcome != LandingBaseMoved {
				t.Fatalf("the landing is %+v, want the base moved", result)
			}
			if f.moved != nil {
				moved = f.moved()
			}
			if got := f.upstreamMain(); got != moved {
				t.Fatalf("the base branch is at %s, want the other commit %s", got, moved)
			}
			if f.rebases != 1 {
				t.Fatalf("the existing rebase was asked for %d times", f.rebases)
			}
			if f.feature() != AssembledState || f.pulls.Readies != 0 || f.pulls.Creates != 0 || f.notices("refused") != 0 {
				t.Fatalf("a moved base was taken as a refusal: %s, %d ready", f.feature(), f.pulls.Readies)
			}
			if intent, _, err := ReadLandingIntent(f.repo, stream, 1); err != nil || intent.Status == LandingRefused {
				t.Fatalf("the intent is %+v %v", intent, err)
			}
			if f.subject(LandingSubject) != "base-moved-1" {
				t.Fatalf("the landing is %s", f.subject(LandingSubject))
			}

			// The rebase and the checks on its commit have run: the landing is
			// asked for again on the new push and lands the new commit.
			_, signed := f.rebased(2, moved)
			again := f.land(byOwner())
			if again.Outcome != Landed || again.Commit != signed {
				t.Fatalf("the retried landing is %+v", again)
			}
			if got := f.upstreamMain(); got != signed {
				t.Fatalf("the base branch is at %s, want the rebased commit %s", got, signed)
			}
			if merges := f.git("-C", f.upstream, "rev-list", "--merges", moved+".."+signed); merges != "" {
				t.Fatalf("merge commits reached the base branch: %s", merges)
			}
			if f.subject(LandingSubject) != "landed-2" || f.pulls.Readies != 0 {
				t.Fatalf("the landing is %s with %d pull requests marked ready", f.subject(LandingSubject), f.pulls.Readies)
			}
		})
	}
}

func TestABaseThatRewroteTheReviewedBaseIsMovedToo(t *testing.T) {
	t.Parallel()
	f := newLandFixture(t)
	// The base branch was force-moved to a commit that does not hold the base
	// the final review read.
	f.git("-C", f.scratch, "fetch", "--quiet", "origin")
	f.git("-C", f.scratch, "checkout", "--quiet", "--orphan", "rewritten")
	f.write(f.scratch, "other.txt", "other\n")
	f.commit(f.scratch, "rewritten history")
	f.git("-C", f.scratch, "push", "--quiet", "--force", "origin", "rewritten:main")
	moved := f.upstreamMain()
	if result := f.land(byOwner()); result.Outcome != LandingBaseMoved {
		t.Fatalf("the landing is %+v", result)
	}
	if f.upstreamMain() != moved || f.rebases != 1 || f.stepCount("land-recorded") != 0 {
		t.Fatal("the landing pushed over a base that no longer holds the reviewed base")
	}
}

func TestARefusedPushMarksTheDraftReadyAndDeliversWithoutLandingAgain(t *testing.T) {
	t.Parallel()
	for name, decider := range map[string]LandDecider{"the owner decided": byOwner(), "Jev decided": byJev()} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f := newLandFixture(t)
			f.refuseBase()
			result := f.land(decider)
			if result.Outcome != LandingFellBack || !strings.Contains(result.Reason, "Protected branch") {
				t.Fatalf("the landing is %+v", result)
			}
			if f.upstreamMain() != f.base {
				t.Fatalf("a refused push moved the base branch to %s", f.upstreamMain())
			}
			if f.pulls.Readies != 1 || f.pulls.Creates != 0 || f.pulls.Prs[0].Draft || len(f.pulls.Prs) != 1 {
				t.Fatalf("the draft pull request: %d marked ready, %d created, draft %v of %d", f.pulls.Readies, f.pulls.Creates, f.pulls.Prs[0].Draft, len(f.pulls.Prs))
			}
			if f.feature() != DeliveredState || f.subject(LandingSubject) != "fell-back-1" {
				t.Fatalf("the workstream is %s, its landing %s", f.feature(), f.subject(LandingSubject))
			}
			if f.notices("refused the push") != 1 || f.notices("Protected branch") < 1 {
				t.Fatal("no notice states why the workstream did not land")
			}
			intent, found, err := ReadLandingIntent(f.repo, stream, 1)
			fixturekit.Must(t, err)
			if !found || intent.Status != LandingRefused || !strings.Contains(intent.Refusal, "Protected branch") || intent.Commit != f.signed || intent.BaseBefore != f.base {
				t.Fatalf("the recorded intent is %+v", intent)
			}
			// The approval the marking rests on is the service's, authored for
			// the decider, and no landing is recorded.
			_, _, approval, err := DeliveryDocuments(f.repo, stream)
			fixturekit.Must(t, err)
			if approval == nil || approval.Commit != f.reviewed || approval.Description == "" {
				t.Fatalf("the approval is %+v", approval)
			}
			if decider.Owner() && approval.DecidedBy != "" || !decider.Owner() && (approval.DecidedBy != trace.DecidedByJev || approval.Judgment != judgmentKey) {
				t.Fatalf("the approval's decider is %q %q", approval.DecidedBy, approval.Judgment)
			}
			if _, landed, err := ReadDeliveryLanding(f.repo, stream); err != nil || landed {
				t.Fatalf("a landing is recorded for a refused push: %v %v", landed, err)
			}
			delivered := f.transitions(DeliveredState)
			if len(delivered) != 1 || (decider.Owner() && delivered[0].DecidedBy != trace.DecidedByOwner) || (!decider.Owner() && delivered[0].DecidedBy != trace.DecidedByJevGate) {
				t.Fatalf("the delivered transitions are %+v", delivered)
			}

			// A second attempt on the same delivery occurrence does nothing.
			pushes := f.stepCount("land-recorded")
			again, err := f.controller().LandWorkstream(context.Background(), f.repo, stream, LandRequest{Decider: decider, AcceptBlocking: true})
			fixturekit.Must(t, err)
			if again.Outcome != LandingFellBack {
				t.Fatalf("the second attempt is %+v", again)
			}
			if f.stepCount("land-recorded") != pushes || f.pulls.Readies != 1 || f.documents(LandingIntentDocument) != 2 || f.documents(DeliveryDocument) != 1 {
				t.Fatal("the second attempt pushed, marked ready or recorded again")
			}
		})
	}
}

func TestAPushThatLandedBeforeTheCrashIsCompletedOnceWithoutPushingAgain(t *testing.T) {
	t.Parallel()
	for name, afterwards := range map[string]func(f *landFixture) string{
		"the base is as it was left": func(f *landFixture) string { return f.signed },
		"the base advanced past the landed commit": func(f *landFixture) string {
			return f.advanceUpstream("later.txt")
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f := newLandFixture(t)
			// The upstream accepted the push, and the service went down before it
			// recorded anything of it.
			f.hook("land-pushed", func() error { return errors.New("the service stopped") })
			op := f.requestedLanding(byOwner())
			if _, err := f.lander().Apply(context.Background(), op); err == nil {
				t.Fatal("the landing reported a result though it stopped after the push")
			}
			if f.upstreamMain() != f.signed {
				t.Fatalf("the upstream is at %s, want the pushed %s", f.upstreamMain(), f.signed)
			}
			if f.documents(LandingDocument) != 0 || f.feature() != AssembledState {
				t.Fatal("the landing was recorded before the crash")
			}
			tip := afterwards(f)

			// A restart: recovery finds the exact signed commit on the base.
			result, err := f.lander().Apply(context.Background(), op)
			fixturekit.Must(t, err)
			if result.Outcome != LandOutcomeLanded {
				t.Fatalf("recovery is %+v", result)
			}
			if f.stepCount("land-recorded") != 1 {
				t.Fatalf("the landing pushed %d times", f.stepCount("land-recorded"))
			}
			if f.upstreamMain() != tip {
				t.Fatalf("recovery moved the base to %s, want %s", f.upstreamMain(), tip)
			}
			if f.rebases != 0 || f.pulls.Readies != 0 || f.pulls.Updates != 0 {
				t.Fatalf("recovery rebased %d times and marked %d ready", f.rebases, f.pulls.Readies)
			}
			landing, _, err := ReadDeliveryLanding(f.repo, stream)
			fixturekit.Must(t, err)
			if landing.Commit != f.signed || landing.Confirmed != tip || landing.BaseBefore != f.base {
				t.Fatalf("the landing record is %+v", landing)
			}
			if f.feature() != DeliveredState || f.documents(LandingDocument) != 1 || len(f.transitions(DeliveredState)) != 1 {
				t.Fatalf("the workstream is %s with %d landing records", f.feature(), f.documents(LandingDocument))
			}
			if merge, merged, err := ReadDeliveryMerge(f.repo, stream); err != nil || !merged || merge.Upstream.Commit != f.signed {
				t.Fatalf("the workstream does not resolve as merged: %+v %v %v", merge, merged, err)
			}

			// Recovering again completes nothing twice.
			again, err := f.lander().Apply(context.Background(), op)
			fixturekit.Must(t, err)
			if again.Outcome != LandOutcomeLanded || f.documents(LandingDocument) != 1 || f.documents(MergeDocument) != 1 || len(f.transitions(DeliveredState)) != 1 {
				t.Fatalf("a second recovery recorded again: %+v", again)
			}
		})
	}
}

func TestAPushThatSucceededButReturnedAnErrorIsSettledAgainstTheFetchedBase(t *testing.T) {
	t.Parallel()
	f := newLandFixture(t)
	// The fault comes after the upstream accepted the push: the response is lost.
	f.hook("land-confirmed", func() error { return errors.New("the connection dropped") })
	result := f.land(byOwner())
	if result.Outcome != LandingPending {
		t.Fatalf("the landing is %+v, want its outcome unknown", result)
	}
	if f.upstreamMain() != f.signed || f.documents(LandingDocument) != 0 || f.feature() != AssembledState {
		t.Fatal("the landing is half recorded: the record and the delivery come in one commit")
	}
	if f.pulls.Readies != 0 || f.notices("refused") != 0 || f.rebases != 0 {
		t.Fatal("an unknown outcome was taken as a refusal or a moved base")
	}
	again := f.land(byOwner())
	if again.Outcome != Landed || f.stepCount("land-recorded") != 1 || f.documents(LandingDocument) != 1 || f.feature() != DeliveredState {
		t.Fatalf("recovery is %+v after %d pushes", again, f.stepCount("land-recorded"))
	}
}

func TestATransportFailureThatNeverReachedTheUpstreamIsRetriedNotRefused(t *testing.T) {
	t.Parallel()
	f := newLandFixture(t)
	// The fetch reads the upstream and the push goes where nothing answers.
	f.git("-C", f.cfg.Project.Clone, "config", "remote.upstream.pushurl", f.upstream+".gone")
	result := f.land(byOwner())
	if result.Outcome != LandingPending {
		t.Fatalf("the landing is %+v, want its outcome unknown", result)
	}
	if f.upstreamMain() != f.base || f.feature() != AssembledState {
		t.Fatal("the failed push changed something")
	}
	if f.pulls.Readies != 0 || f.pulls.Prs[0].Draft != true || f.notices("refused") != 0 || f.rebases != 0 {
		t.Fatalf("the transport failure was treated as a refusal or a moved base: %d ready", f.pulls.Readies)
	}
	if intent, found, err := ReadLandingIntent(f.repo, stream, 1); err != nil || !found || intent.Status != LandingPushing {
		t.Fatalf("the intent is %+v %v %v", intent, found, err)
	}
	if outcome, err := f.lander().Outcome(stream, 1); err != nil || outcome != nil {
		t.Fatalf("the attempt has an outcome %+v %v", outcome, err)
	}

	// The upstream is reachable again: the bounded recovery's retry lands it.
	f.git("-C", f.cfg.Project.Clone, "config", "--unset", "remote.upstream.pushurl")
	again := f.land(byOwner())
	if again.Outcome != Landed || f.upstreamMain() != f.signed {
		t.Fatalf("the retry is %+v", again)
	}
	if f.pulls.Readies != 0 || f.stepCount("land-recorded") != 2 {
		t.Fatalf("the retry marked %d ready after %d push attempts", f.pulls.Readies, f.stepCount("land-recorded"))
	}
}

func TestAnUnreadableUpstreamIsRetriedAndGiveUpEndsItWithoutRefusing(t *testing.T) {
	t.Parallel()
	f := newLandFixture(t)
	f.git("-C", f.cfg.Project.Clone, "remote", "set-url", "upstream", filepath.Join(f.home, "gone", "acme", "widgets.git"))
	result := f.land(byOwner())
	if result.Outcome != LandingPending || f.stepCount("land-recorded") != 0 {
		t.Fatalf("the landing is %+v", result)
	}
	op := f.operation(1)
	ended, err := f.lander().GiveUp(context.Background(), op, reconcile.GiveUpReason{Status: reconcile.OperationStatus{Failures: 5}, Limit: reconcile.NewGiveUpLimit(0)})
	fixturekit.Must(t, err)
	if ended.Outcome != LandOutcomeFailed || !strings.Contains(ended.Evidence, RefusedUnknownOutcome) {
		t.Fatalf("giving up is %+v", ended)
	}
	if f.pulls.Readies != 0 || f.feature() != AssembledState || f.documents(LandingIntentDocument) != 0 {
		t.Fatal("giving up treated an unreadable upstream as a refusal")
	}
}

func TestGiveUpCompletesALandingTheBaseAlreadyHoldsAndFailsOneItConfirmsAbsent(t *testing.T) {
	t.Parallel()
	why := reconcile.GiveUpReason{Status: reconcile.OperationStatus{Failures: 5}, Limit: reconcile.NewGiveUpLimit(0)}
	t.Run("landed", func(t *testing.T) {
		t.Parallel()
		f := newLandFixture(t)
		f.hook("land-pushed", func() error { return errors.New("the service stopped") })
		op := f.requestedLanding(byOwner())
		if _, err := f.lander().Apply(context.Background(), op); err == nil {
			t.Fatal("no error")
		}
		result, err := f.lander().GiveUp(context.Background(), op, why)
		fixturekit.Must(t, err)
		if result.Outcome != LandOutcomeLanded || f.feature() != DeliveredState || f.stepCount("land-recorded") != 1 {
			t.Fatalf("giving up is %+v", result)
		}
	})
	t.Run("absent", func(t *testing.T) {
		t.Parallel()
		f := newLandFixture(t)
		f.git("-C", f.cfg.Project.Clone, "config", "remote.upstream.pushurl", f.upstream+".gone")
		op := f.requestedLanding(byOwner())
		if _, err := f.lander().Apply(context.Background(), op); err == nil {
			t.Fatal("no error")
		}
		result, err := f.lander().GiveUp(context.Background(), op, why)
		fixturekit.Must(t, err)
		if result.Outcome != LandOutcomeFailed || !strings.Contains(result.Evidence, "confirmed absent") || f.feature() != AssembledState || f.pulls.Readies != 0 {
			t.Fatalf("giving up is %+v", result)
		}
	})
}

func TestARestartAfterARecordedRefusalMarksReadyOnceAndNeverPushesAgain(t *testing.T) {
	t.Parallel()
	for name, crash := range map[string]string{
		"before the pull request was touched":     "land-marking-ready",
		"after the pull request was marked ready": "ready-marked",
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f := newLandFixture(t)
			f.refuseBase()
			f.hook(crash, func() error { return errors.New("the service stopped") })
			op := f.requestedLanding(byJev())
			if _, err := f.lander().Apply(context.Background(), op); err == nil {
				t.Fatal("the landing reported a result though it stopped")
			}
			intent, found, err := ReadLandingIntent(f.repo, stream, 1)
			fixturekit.Must(t, err)
			if !found || intent.Status != LandingRefused || f.feature() != AssembledState {
				t.Fatalf("the refusal is %+v %v, the workstream %s", intent, found, f.feature())
			}
			pushes := f.stepCount("land-recorded")

			result, err := f.lander().Apply(context.Background(), op)
			fixturekit.Must(t, err)
			if result.Outcome != LandOutcomeFellBack {
				t.Fatalf("recovery is %+v", result)
			}
			if f.stepCount("land-recorded") != pushes || f.upstreamMain() != f.base {
				t.Fatal("recovery pushed to the base branch again")
			}
			if f.pulls.Readies != 1 || f.pulls.Creates != 0 || f.pulls.Prs[0].Draft {
				t.Fatalf("recovery marked the draft ready %d times", f.pulls.Readies)
			}
			if f.feature() != DeliveredState || len(f.transitions(DeliveredState)) != 1 || f.documents(DeliveryDocument) != 1 {
				t.Fatalf("the workstream is %s", f.feature())
			}
			if again, err := f.lander().Apply(context.Background(), op); err != nil || again.Outcome != LandOutcomeFellBack || f.pulls.Readies != 1 {
				t.Fatalf("a second recovery is %+v %v with %d marked ready", again, err, f.pulls.Readies)
			}
		})
	}
}

func TestALandingFindsAWorkstreamDeliveredMeanwhileAndRecordsItWithoutMovingIt(t *testing.T) {
	t.Parallel()
	f := newLandFixture(t)
	f.hook("land-pushed", func() error {
		f.transition("delivered-elsewhere", AssembledState, DeliveredState)
		return nil
	})
	result := f.land(byOwner())
	if result.Outcome != Landed || f.upstreamMain() != f.signed || f.documents(LandingDocument) != 1 || f.subject(LandingSubject) != "landed-1" {
		t.Fatalf("the landing is %+v", result)
	}
	if f.feature() != DeliveredState || len(f.transitions(DeliveredState)) != 0 {
		t.Fatalf("the workstream was moved again: %s", f.feature())
	}
}

func landingOperationsOf(ops []trace.OperationRecord) []trace.OperationRecord {
	var out []trace.OperationRecord
	for _, o := range ops {
		if LandingAction(o.Operation) {
			out = append(out, o)
		}
	}
	return out
}

// requestedLanding records the landing of the fixture's push as an operation,
// as the entry point does before it applies it.
func (f *landFixture) requestedLanding(by LandDecider) coreadapter.Operation {
	f.t.Helper()
	op, err := f.lander().Request(context.Background(), stream, LandInput{Landing: 1, Push: 1, Review: 1, Reviewed: f.reviewed, Commit: f.signed, Decider: by})
	fixturekit.Must(f.t, err)
	return op
}
