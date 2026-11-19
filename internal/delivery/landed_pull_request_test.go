package delivery

import (
	"context"
	"strings"
	"testing"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	"github.com/kpenfound/osmia/internal/trace"
)

func (f *landFixture) settle() bool {
	f.t.Helper()
	closed, err := SettleLandedPullRequest(context.Background(), f.pulls, f.repo, f.clock.Now(), stream, nil)
	fixturekit.Must(f.t, err)
	return closed
}

func (f *landFixture) hostWrites() (closes, comments int) {
	f.pulls.Mu.Lock()
	defer f.pulls.Mu.Unlock()
	return f.pulls.Closes, len(f.pulls.Comments)
}

// A landed workstream whose draft pull request is still open has it closed by
// one poll, with one comment naming the landed commit and the base branch. Later
// polls and a restart, which remembers only the trace, post nothing more.
func TestALandedWorkstreamsOpenPullRequestIsClosedOnceWithACommentNamingTheLandedCommit(t *testing.T) {
	t.Parallel()
	f := newLandFixture(t)
	if f.settle() {
		t.Fatal("a workstream that has not landed had its pull request closed")
	}
	if closes, comments := f.hostWrites(); closes != 0 || comments != 0 {
		t.Fatalf("the host was written before the landing: %d closes, %d comments", closes, comments)
	}
	if result := f.land(byOwner()); result.Outcome != Landed {
		t.Fatalf("the landing is %+v", result)
	}
	if closes, comments := f.hostWrites(); closes != 0 || comments != 0 {
		t.Fatalf("the landing wrote to the host: %d closes, %d comments", closes, comments)
	}

	if !f.settle() {
		t.Fatal("the poll left the open pull request of a landed workstream open")
	}
	f.pulls.Mu.Lock()
	state, comments := f.pulls.Prs[0].State, append([]fixturekit.PullComment(nil), f.pulls.Comments...)
	f.pulls.Mu.Unlock()
	if state != "closed" || len(comments) != 1 || comments[0].Number != 7 {
		t.Fatalf("the pull request is %s with comments %+v", state, comments)
	}
	for _, want := range []string{f.signed, "main of acme/widgets"} {
		if !strings.Contains(comments[0].Text, want) {
			t.Fatalf("the comment %q does not name %q", comments[0].Text, want)
		}
	}
	outcome, revision, err := OwnPullRequestOutcomeAt(f.repo, stream)
	fixturekit.Must(t, err)
	if revision == 0 || outcome.PullRequest != 7 || outcome.Outcome != OwnPullRequestOutcomeClosed {
		t.Fatalf("the recorded outcome is %+v at revision %d", outcome, revision)
	}

	// Later polls and a restart find nothing to do and write nothing.
	f.pulls.Mu.Lock()
	finds := f.pulls.Finds
	f.pulls.Mu.Unlock()
	for range 3 {
		if f.settle() {
			t.Fatal("a later poll closed the pull request again")
		}
	}
	f.pulls.Mu.Lock()
	again := f.pulls.Finds
	f.pulls.Mu.Unlock()
	if again != finds {
		t.Fatalf("polls looked the recorded pull request up %d more times", again-finds)
	}
	if closes, comments := f.hostWrites(); closes != 1 || comments != 1 {
		t.Fatalf("the host was written %d closes and %d comments", closes, comments)
	}
	if f.feature() != DeliveredState || len(f.transitions(DeliveredState)) != 1 {
		t.Fatalf("the workstream is %s after the poll", f.feature())
	}
}

// A close whose response is lost reached the host: the next poll finds the pull
// request closed, records it and comments no more.
func TestALostCloseResponseIsNotRepeatedByTheNextPoll(t *testing.T) {
	t.Parallel()
	f := newLandFixture(t)
	f.land(byOwner())
	f.pulls.Mu.Lock()
	f.pulls.Lose = true
	f.pulls.Mu.Unlock()
	if f.settle() {
		t.Fatal("a close that failed was reported as done")
	}
	if _, revision, err := OwnPullRequestOutcomeAt(f.repo, stream); err != nil || revision != 0 {
		t.Fatalf("an outcome was recorded for a close that was not confirmed: %d %v", revision, err)
	}
	if f.settle() {
		t.Fatal("the poll closed a pull request the host already shows closed")
	}
	if closes, comments := f.hostWrites(); closes != 1 || comments != 1 {
		t.Fatalf("the host was written %d closes and %d comments", closes, comments)
	}
	if outcome, _, err := OwnPullRequestOutcomeAt(f.repo, stream); err != nil || outcome.Outcome != OwnPullRequestOutcomeClosed {
		t.Fatalf("the recorded outcome is %+v %v", outcome, err)
	}
}

// A pull request the host already shows merged or closed after the landing is
// left alone, and the workstream's delivered and resolved state is the
// landing's, unchanged by the poll.
func TestAPullRequestAlreadyMergedOrClosedAfterTheLandingIsLeftAlone(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name    string
		merged  bool
		outcome string
	}{
		{"merged", true, OwnPullRequestOutcomeMerged},
		{"closed", false, OwnPullRequestOutcomeClosed},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			f := newLandFixture(t)
			f.land(byJev())
			before := f.feature()
			transitions, err := trace.Read[trace.Transition](f.repo, stream)
			fixturekit.Must(t, err)
			f.pulls.Mu.Lock()
			f.pulls.Prs[0].State, f.pulls.Prs[0].Merged = "closed", c.merged
			if c.merged {
				f.pulls.Prs[0].MergeCommit = f.signed
			}
			f.pulls.Mu.Unlock()

			if f.settle() {
				t.Fatal("the poll closed a pull request that was already finished")
			}
			if closes, comments := f.hostWrites(); closes != 0 || comments != 0 {
				t.Fatalf("the host was written %d closes and %d comments", closes, comments)
			}
			outcome, _, err := OwnPullRequestOutcomeAt(f.repo, stream)
			fixturekit.Must(t, err)
			if outcome.PullRequest != 7 || outcome.Outcome != c.outcome {
				t.Fatalf("the recorded outcome is %+v", outcome)
			}
			after, err := trace.Read[trace.Transition](f.repo, stream)
			fixturekit.Must(t, err)
			if f.feature() != before || len(after) != len(transitions) {
				t.Fatalf("the poll changed the workstream: %s, %d transitions after %d", f.feature(), len(after), len(transitions))
			}
			docs, err := trace.Read[trace.Document](f.repo, stream)
			fixturekit.Must(t, err)
			evidence, err := trace.MemberResolutionFrom(docs)
			fixturekit.Must(t, err)
			if got := evidence.Resolution(f.feature()); got != "merged" {
				t.Fatalf("the workstream resolves as %q", got)
			}
			if finished, err := OwnPullRequestFinished(f.repo, stream); err != nil || !finished {
				t.Fatalf("the pull request is not finished: %v %v", finished, err)
			}
			if merge, merged, err := ReadDeliveryMerge(f.repo, stream); err != nil || !merged || merge.Upstream.Commit != f.signed {
				t.Fatalf("the merge evidence is %+v %v %v", merge, merged, err)
			}
		})
	}
}

// A workstream delivered by a pull request, with the pull request closed by
// the owner, is not touched: only a landed workstream has its pull request
// settled.
func TestAWorkstreamThatDidNotLandHasItsPullRequestLeftAlone(t *testing.T) {
	t.Parallel()
	f := newLandFixture(t)
	f.transition("delivered", AssembledState, DeliveredState)
	if f.settle() {
		t.Fatal("a workstream that did not land had its pull request closed")
	}
	if closes, comments := f.hostWrites(); closes != 0 || comments != 0 {
		t.Fatalf("the host was written %d closes and %d comments", closes, comments)
	}
}

// The delivery presentation of a landed workstream reports its landing, with
// the landed commit, and not a merge or a ready pull request.
func TestTheDeliveryPresentationOfALandedWorkstreamReportsTheLanding(t *testing.T) {
	t.Parallel()
	f := newLandFixture(t)
	f.land(byOwner())
	c := f.controller()
	c.UpstreamDeliveryPending = func(*trace.Repository, *config.Config, config.WorkstreamID) (bool, error) { return false, nil }
	presented, api := c.PresentDelivery(context.Background(), f.repo.Project(), stream, f.repo)
	if api != nil {
		t.Fatalf("%+v", api)
	}
	l := presented.Landing
	if !presented.Delivered || l == nil || l.Commit != f.signed || l.BaseBranch != "main" || l.Upstream != "acme/widgets" || l.DecidedBy != trace.DecidedByOwner {
		t.Fatalf("the presentation is %+v with landing %+v", presented, l)
	}
	if presented.Merge != nil || presented.Ready != nil || presented.Maintenance {
		t.Fatalf("a landing is presented as a merge %+v, ready %+v or maintenance %v", presented.Merge, presented.Ready, presented.Maintenance)
	}
}
