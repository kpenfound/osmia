package execution

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/intake"
	"github.com/kpenfound/osmia/internal/reconcile"
	"github.com/kpenfound/osmia/internal/trace"
	"github.com/kpenfound/osmia/internal/workspace"
)

// boundedLanding is a landing of the approved unit "resume" asked for and a
// controller that reconciles it, with every other operation held.
type boundedLanding struct {
	f          *buildFixture
	stream     config.WorkstreamID
	repository *trace.Repository
	lands      *foreman
	controller *reconcile.Controller
}

func newBoundedLanding(t *testing.T, key string, options reconcile.Options) *boundedLanding {
	t.Helper()
	f, stream, repository := newReviewFixture(t, key)
	approveDirectly(t, f.s, repository, stream, "resume", "spec#1")
	lands := &foreman{masons: newMasonController(f.s, repository)}
	must(t, lands.Pass(context.Background()))
	options.Worker, options.Now, options.RetryDelay, options.MaxRetryDelay = "landing-test", f.clock.Now, time.Minute, time.Minute
	options.Adapters = map[coreadapter.OperationBoundary]coreadapter.Reconciler{coreadapter.RepositoryBoundary: lands}
	options.Hold = func(_ config.WorkstreamID, op coreadapter.Operation) bool { return op.Action != LandAction }
	controller, err := reconcile.New(repository, options)
	must(t, err)
	return &boundedLanding{f, stream, repository, lands, controller}
}

// pass makes the given number of passes a minute apart.
func (b *boundedLanding) pass(t *testing.T, passes int) {
	t.Helper()
	for range passes {
		must(t, b.controller.Pass(context.Background()))
		jump(b.f.clock, b.f.clock.Now().Add(time.Minute))
	}
}

func (b *boundedLanding) operation(t *testing.T) trace.OperationRecord {
	t.Helper()
	ops := landOperations(t, b.repository, b.stream)
	if len(ops) != 1 {
		t.Fatalf("landing operations %+v", ops)
	}
	return ops[0]
}

// notices returns the bodies of the notices the chief of staff was sent that
// contain the words.
func (b *boundedLanding) notices(t *testing.T, words string) []string {
	t.Helper()
	entries, err := b.repository.Outbox(b.stream)
	must(t, err)
	var bodies []string
	for _, e := range entries {
		if e.Event.Kind == trace.NoticeKind && strings.Contains(e.Event.Body, words) {
			bodies = append(bodies, e.Event.Body)
		}
	}
	return bodies
}

func (b *boundedLanding) landedCommits(t *testing.T) []string {
	t.Helper()
	_, result := approvedReview(t, b.repository, b.stream, "resume")
	return b.f.landedCommits(t, b.stream, result.Identity.Candidate.BaseRevision)
}

func (b *boundedLanding) transitionsTo(t *testing.T, prefix string) []string {
	t.Helper()
	var ids []string
	for _, tr := range allTransitions(t, b.f.cfg.Root, b.f.cfg.Project.ID, b.stream) {
		if tr.Subject == landingSubject("resume") && strings.HasPrefix(tr.To, prefix) {
			ids = append(ids, tr.ID)
		}
	}
	return ids
}

// A landing that keeps failing with its commit confirmed absent ends, past the
// failure limit and the give-up window, in a refusal through the landing's own
// refuse path. The chief of staff is told once at the third failure and once of
// the refusal, the refusal says the failure was not about the approval, and
// nothing was committed.
func TestLandingThatKeepsFailingIsRefusedOnceItsCommitIsConfirmedAbsent(t *testing.T) {
	t.Parallel()
	b := newBoundedLanding(t, "give-up-absent", reconcile.Options{GiveUpWindow: 10 * time.Minute})
	var local atomic.Int32
	b.f.s.Step = func(name string) error {
		if name == "land-local-step:"+string(b.stream) {
			local.Add(1)
			return errors.New("the clone is unreachable")
		}
		return nil
	}
	// Past the failure limit, inside the window: still retrying.
	b.pass(t, reconcile.GiveUpFailures+2)
	if op := b.operation(t); op.Result != nil || reconcile.ConsecutiveFailures(op) != reconcile.GiveUpFailures+2 {
		t.Fatalf("after %d failures: %+v", reconcile.GiveUpFailures+2, op)
	}
	if got := b.notices(t, "has failed 3 times in a row"); len(got) != 1 || !strings.Contains(got[0], "The landing of unit resume") || !strings.Contains(got[0], "the clone is unreachable") {
		t.Fatalf("notices at the third failure: %q", got)
	}
	// The window passes: the operation is refused.
	b.pass(t, 8)
	op := b.operation(t)
	if op.Result == nil || op.Result.Outcome != "failed" || !op.Acknowledged {
		t.Fatalf("the landing ended as %+v", op)
	}
	for _, words := range []string{RefusedAfterRetries, "the landing is confirmed absent", "holds no commit of this landing", "the clone is unreachable"} {
		if !strings.Contains(op.Result.Evidence, words) {
			t.Fatalf("the refusal lacks %q: %s", words, op.Result.Evidence)
		}
	}
	if staleRefusal(op.Result.Evidence) {
		t.Fatalf("the refusal reads as a stale approval: %s", op.Result.Evidence)
	}
	attempts := local.Load()
	b.pass(t, 3)
	if local.Load() != attempts {
		t.Fatal("the landing was attempted again after it was given up")
	}
	if len(b.transitionsTo(t, "refused-")) != 1 || len(b.transitionsTo(t, "landed-")) != 0 {
		t.Fatalf("refused %v, landed %v", b.transitionsTo(t, "refused-"), b.transitionsTo(t, "landed-"))
	}
	if commits := b.landedCommits(t); len(commits) != 0 {
		t.Fatalf("the feature branch gained %v", commits)
	}
	if got := b.notices(t, "did not land"); len(got) != 1 || strings.Contains(got[0], "Nothing was committed") {
		t.Fatalf("notices of the refusal: %q", got)
	}
	if got := b.notices(t, "has failed"); len(got) != 1 {
		t.Fatalf("the failure streak notified the chief %d times: %q", len(got), got)
	}
}

// A landing whose commit already reached the feature branch is recorded
// through its own success path when it is given up, not refused, and the commit
// is not made again.
func TestLandingWhoseCommitIsOnItsTargetIsRecordedWhenGivenUp(t *testing.T) {
	t.Parallel()
	b := newBoundedLanding(t, "give-up-completed", reconcile.Options{GiveUpWindow: 10 * time.Minute})
	phase := 0
	b.f.s.Step = func(name string) error {
		switch {
		case phase == 0 && name == "land-advanced":
			phase = 1
			return errors.New("crash")
		case phase == 1 && name == "land-local-step:"+string(b.stream):
			return errors.New("the clone is unreachable")
		}
		return nil
	}
	b.pass(t, reconcile.GiveUpFailures+2)
	if op := b.operation(t); op.Result != nil || len(b.landedCommits(t)) != 1 {
		t.Fatalf("before the window: %+v, commits %v", op, b.landedCommits(t))
	}
	b.pass(t, 8)
	op := b.operation(t)
	if op.Result == nil || op.Result.Outcome != "succeeded" || !op.Acknowledged {
		t.Fatalf("the landing ended as %+v", op)
	}
	commits := b.landedCommits(t)
	if len(commits) != 1 {
		t.Fatalf("the feature branch gained %d commits: %v", len(commits), commits)
	}
	if len(b.transitionsTo(t, "landed-")) != 1 || len(b.transitionsTo(t, "refused-")) != 0 {
		t.Fatalf("landed %v, refused %v", b.transitionsTo(t, "landed-"), b.transitionsTo(t, "refused-"))
	}
	var merged int
	for _, tr := range allTransitions(t, b.f.cfg.Root, b.f.cfg.Project.ID, b.stream) {
		if tr.Subject == trace.UnitSubject("resume") && tr.To == UnitMerged {
			merged++
		}
	}
	if merged != 1 {
		t.Fatalf("the unit merged %d times", merged)
	}
	if got := b.notices(t, "its effect is already on its target"); len(got) != 1 {
		t.Fatalf("notices of the recorded success: %q", got)
	}
}

// A landing recorded durably before the controller wrote its result keeps its
// success, even past the failure limit and the window: giving up is considered
// only after inspection finds no durable outcome.
func TestDurableLandingIsKeptPastTheGiveUpThreshold(t *testing.T) {
	t.Parallel()
	b := newBoundedLanding(t, "give-up-durable", reconcile.Options{GiveUpWindow: 10 * time.Minute})
	mode := "unreachable"
	b.f.s.Step = func(name string) error {
		if mode == "unreachable" && name == "land-local-step:"+string(b.stream) || mode == "crash" && name == "land-recorded" {
			return errors.New("the service stopped")
		}
		return nil
	}
	b.pass(t, reconcile.GiveUpFailures+1)
	// The landing is recorded, and the service stops before the controller
	// writes the operation's result.
	mode = "crash"
	b.pass(t, 1)
	if op := b.operation(t); op.Result != nil || len(b.transitionsTo(t, "landed-")) != 1 {
		t.Fatalf("the landing was not recorded durably before its result: %+v", op)
	}
	// The streak is now past the limit and the window.
	mode = "unreachable"
	jump(b.f.clock, b.f.clock.Now().Add(time.Hour))
	b.pass(t, 1)
	op := b.operation(t)
	if op.Result == nil || op.Result.Outcome != "succeeded" || !op.Acknowledged {
		t.Fatalf("the recorded landing was not kept: %+v", op)
	}
	if len(b.transitionsTo(t, "refused-")) != 0 || len(b.landedCommits(t)) != 1 {
		t.Fatalf("refused %v, commits %v", b.transitionsTo(t, "refused-"), b.landedCommits(t))
	}
	if got := b.notices(t, "its effect is already on its target"); len(got) != 0 {
		t.Fatalf("a give-up notice was sent for a landing that recorded itself: %q", got)
	}
}

// An attempt blocked on a clone lock held past its deadline is a retry that
// names lock contention and changes nothing. When the landing is given up while
// the clone is still held, its outcome is recorded as one that could not be
// determined: never a refusal that says the commit is absent, and the chief of
// staff is told. The lock outlives the attempt; the operation does not.
func TestLandingGivenUpOnAHeldCloneSaysItsOutcomeCouldNotBeDetermined(t *testing.T) {
	t.Parallel()
	b := newBoundedLanding(t, "give-up-held", reconcile.Options{GiveUpWindow: 10 * time.Minute, AttemptDeadline: 100 * time.Millisecond, ReconcileDeadline: 100 * time.Millisecond})
	g, err := featureWorkspaces(b.f.cfg, b.repository).Of(b.stream)
	must(t, err)
	holding, release := make(chan struct{}), make(chan struct{})
	finished := make(chan error, 1)
	go func() {
		finished <- workspace.Quiesced(context.Background(), []workspace.Provider{g}, func(context.Context) error {
			close(holding)
			<-release
			return nil
		})
	}()
	<-holding
	released := false
	defer func() {
		if !released {
			close(release)
		}
	}()
	tip, _, err := g.Branch(context.Background(), featureBranch(b.stream))
	must(t, err)

	b.pass(t, 1)
	op := b.operation(t)
	retry := op.History[len(op.History)-1]
	if op.Result != nil || retry.Kind != reconcile.RetryAction || !strings.Contains(retry.Failure, "Clone lock contention") || !strings.Contains(retry.Failure, "locked by another owner") {
		t.Fatalf("the blocked attempt: %+v", op.History)
	}
	b.pass(t, reconcile.GiveUpFailures+8)
	op = b.operation(t)
	if op.Result == nil || op.Result.Outcome != "failed" || !op.Acknowledged {
		t.Fatalf("the landing ended as %+v", op)
	}
	for _, words := range []string{RefusedAfterRetries, RefusedUnknownOutcome, "a mutation may still be running"} {
		if !strings.Contains(op.Result.Evidence, words) {
			t.Fatalf("the result lacks %q: %s", words, op.Result.Evidence)
		}
	}
	if strings.Contains(op.Result.Evidence, "confirmed absent") || staleRefusal(op.Result.Evidence) {
		t.Fatalf("the result claims more than is known: %s", op.Result.Evidence)
	}
	if after, _, err := g.Branch(context.Background(), featureBranch(b.stream)); err != nil || after != tip {
		t.Fatalf("the feature branch moved from %s to %s: %v", tip, after, err)
	}
	if got := b.notices(t, "may or may not have landed"); len(got) != 1 {
		t.Fatalf("notices of the unknown outcome: %q", got)
	}
	// The operation is over, and the lock is not: a later step still gives up
	// at its own deadline instead of hanging.
	waited, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if err := workspace.Quiesced(waited, []workspace.Provider{g}, func(context.Context) error { return nil }); !workspace.IsContention(err) {
		t.Fatalf("a later step on the held clone: %v", err)
	}
	close(release)
	released = true
	must(t, <-finished)
	// Once the process is gone the clone is quiesced again and reads normally.
	must(t, workspace.Quiesced(context.Background(), []workspace.Provider{g}, func(context.Context) error { return nil }))
}

// A landing still pending when its workstream is abandoned ends in a refusal
// that says so, instead of staying in flight.
func TestPendingLandingOfAnAbandonedWorkstreamReachesARefusal(t *testing.T) {
	t.Parallel()
	b := newBoundedLanding(t, "abandoned-landing", reconcile.Options{})
	h := trace.Header{Schema: "osmia.trace.transition", Version: trace.Version, ID: intake.AbandonTransition, Revision: 1, Project: b.repository.Project(), Workstream: b.stream, At: b.f.clock.Now(), Actor: ownerActor, Cause: intake.AbandonTransition}
	_, err := b.repository.SetFeatureStateUnless(context.Background(), h, intake.AbandonedState, "Superseded", intake.AbandonedState)
	must(t, err)
	b.pass(t, 1)
	op := b.operation(t)
	if op.Result == nil || op.Result.Outcome != "failed" || !op.Acknowledged || !strings.Contains(op.Result.Evidence, "abandoned") {
		t.Fatalf("the landing of the abandoned workstream ended as %+v", op)
	}
	if commits := b.landedCommits(t); len(commits) != 0 {
		t.Fatalf("the feature branch gained %v", commits)
	}
}

// A landing whose clone is held by a process that outlived an earlier attempt
// keeps retrying while the clone is held and is never refused for it. Once the
// process is gone the clone is read again: the landing is recorded as landed
// once, whether its commit reached the feature branch before the hold or is
// made after it, and the commit is never made twice.
func TestLandingIsRecordedOnceWhenAHeldCloneIsReleased(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name      string
		committed bool
	}{
		{"commit reached the target", true},
		{"commit never made", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			b := newBoundedLanding(t, "released-"+strings.ReplaceAll(tc.name, " ", "-"), reconcile.Options{GiveUpWindow: 10 * time.Minute, AttemptDeadline: 100 * time.Millisecond, ReconcileDeadline: 100 * time.Millisecond})
			crashed := false
			b.f.s.Step = func(name string) error {
				if tc.committed && !crashed && name == "land-advanced" {
					crashed = true
					return errors.New("crash")
				}
				return nil
			}
			g, err := featureWorkspaces(b.f.cfg, b.repository).Of(b.stream)
			must(t, err)
			if tc.committed {
				// The first attempt makes the commit and stops before recording it.
				b.pass(t, 1)
				if op := b.operation(t); op.Result != nil || len(b.landedCommits(t)) != 1 {
					t.Fatalf("the first attempt: %+v, commits %v", op, b.landedCommits(t))
				}
			}
			holding, release := make(chan struct{}), make(chan struct{})
			finished := make(chan error, 1)
			go func() {
				finished <- workspace.Quiesced(context.Background(), []workspace.Provider{g}, func(context.Context) error {
					close(holding)
					<-release
					return nil
				})
			}()
			<-holding
			released := false
			defer func() {
				if !released {
					close(release)
				}
			}()
			b.pass(t, reconcile.GiveUpFailures+1)
			op := b.operation(t)
			if op.Result != nil || !strings.Contains(op.History[len(op.History)-1].Failure, "Clone lock contention") {
				t.Fatalf("while the clone is held: %+v", op)
			}
			close(release)
			released = true
			must(t, <-finished)
			b.pass(t, 6)
			op = b.operation(t)
			if op.Result == nil || !op.Acknowledged {
				t.Fatalf("the landing did not end: %+v", op)
			}
			if op.Result.Outcome != "succeeded" || len(b.transitionsTo(t, "landed-")) != 1 || len(b.transitionsTo(t, "refused-")) != 0 || len(b.landedCommits(t)) != 1 {
				t.Fatalf("the landing ended %+v, landed %v, refused %v, commits %v", op.Result, b.transitionsTo(t, "landed-"), b.transitionsTo(t, "refused-"), b.landedCommits(t))
			}
		})
	}
}
