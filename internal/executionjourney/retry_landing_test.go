package executionjourney

import (
	"context"
	"errors"
	"github.com/kpenfound/osmia/internal/intake"
	"github.com/kpenfound/osmia/internal/servicetest/intakefixture"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kpenfound/osmia/internal/api"
	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/delivery"
	"github.com/kpenfound/osmia/internal/jev"
	"github.com/kpenfound/osmia/internal/pulls"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	"github.com/kpenfound/osmia/internal/trace"
)

type baseMover struct {
	t       *testing.T
	ready   chan struct{}
	once    sync.Once
	scratch string
	file    string

	moved string
	mu    sync.Mutex
	err   error
}

func newBaseMover(t *testing.T, file string) *baseMover {
	return &baseMover{t: t, ready: make(chan struct{}), file: file}
}

func (m *baseMover) boundary(ctx context.Context, step string) error {
	if step != "land-recorded" {
		return nil
	}
	select {
	case <-m.ready:
	case <-ctx.Done():
		return ctx.Err()
	}
	var err error
	m.once.Do(func() {
		out, pushErr := exec.CommandContext(ctx, "git", "-C", m.scratch, "push", "--quiet", "origin", "HEAD:main").CombinedOutput()
		if pushErr != nil {
			err = errors.New(string(out))
			m.mu.Lock()
			m.err = err
			m.mu.Unlock()
		}
	})
	return err
}

func (m *baseMover) arm(w *routeWorld, content string) {
	m.t.Helper()
	home := filepath.Dir(w.f.clone)
	m.scratch = filepath.Join(home, "someone-else")
	fixturekit.DemoGit(m.t, home, "clone", "--quiet", w.upstream, m.scratch)
	fixturekit.Must(m.t, os.MkdirAll(filepath.Dir(filepath.Join(m.scratch, m.file)), 0755))
	fixturekit.Must(m.t, os.WriteFile(filepath.Join(m.scratch, m.file), []byte(content), 0600))
	fixturekit.DemoGit(m.t, home, "-C", m.scratch, "add", m.file)
	fixturekit.DemoGit(m.t, home, "-C", m.scratch, "-c", "user.name=Someone", "-c", "user.email=someone@example.invalid", "commit", "--quiet", "-m", "someone else's change")
	m.moved = strings.TrimSpace(fixturekit.DemoGit(m.t, home, "-C", m.scratch, "rev-parse", "HEAD"))
	close(m.ready)
}

func (m *baseMover) failed() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.err
}

func (w *routeWorld) landingRoute() (delivery.LandingRoute, bool) {
	w.t.Helper()
	route, found, err := delivery.ReadLandingRoute(w.repository(), w.stream)
	fixturekit.Must(w.t, err)
	return route, found
}

func (w *routeWorld) deliveryApprovals() int {
	w.t.Helper()
	return len(intakefixture.StreamDocuments(w.t, w.repository(), w.stream, delivery.DeliveryDocument))
}

func (w *routeWorld) awaitRebasedPush(after delivery.Push) delivery.Push {
	w.t.Helper()
	var push delivery.Push
	w.await("the rebased push", 0, func() bool {
		current, found, err := delivery.CurrentPush(w.repository(), w.stream)
		if err != nil || !found || current.Number <= after.Number {
			return false
		}
		push = current
		return true
	})
	return push
}

func (w *routeWorld) requireRetriedLanding(first, rebased delivery.Push, moved, decidedBy string) {
	w.t.Helper()
	t := w.t
	w.await("the workstream to be delivered", 0, func() bool { return w.feature() == intake.DeliveredState })
	if rebased.Commit == first.Commit {
		t.Fatalf("the rebased push signed the commit %s that did not land", first.Commit)
	}
	if got := w.rev("refs/heads/main"); got != rebased.Commit {
		t.Fatalf("the base branch is at %s, want the new signed commit %s", got, rebased.Commit)
	}
	if merges := strings.TrimSpace(fixturekit.DemoGit(t, filepath.Dir(w.f.clone), "-C", w.upstream, "rev-list", "--merges", moved+".."+rebased.Commit)); merges != "" {
		t.Fatalf("landing created merge commits: %s", merges)
	}
	if first.Commit != "" {
		if err := exec.Command("git", "-C", w.upstream, "merge-base", "--is-ancestor", first.Commit, "refs/heads/main").Run(); err == nil {
			t.Fatalf("the earlier signed commit %s reached the base branch", first.Commit)
		}
	}

	if n := w.mainUpdates(); n != 2 {
		t.Fatalf("the base branch was updated %d times, want someone else's update and the one landing", n)
	}
	landing, found := w.landed()
	if !found || landing.Commit != rebased.Commit || landing.Push != rebased.Number || landing.DecidedBy != decidedBy || landing.BaseBefore != moved {
		t.Fatalf("the landing record is %+v (found %v)", landing, found)
	}
	if creates, readies, _ := w.hostCounts(); creates != 1 || readies != 0 {
		t.Fatalf("landing opened %d pull requests and marked %d ready", creates, readies)
	}
	w.noPullRequestWasMerged()
	if n := w.landings(); n != 2 {
		t.Fatalf("%d landing attempts, want the one that found the base moved and the retry", n)
	}
	w.await("the downstream workstream to be released", 11*time.Minute, func() bool { return w.edgeWait() == "available" })

	w.advanceThroughPasses()
	if w.landings() != 2 || w.mainUpdates() != 2 || w.feature() != intake.DeliveredState {
		t.Fatalf("later polls changed the outcome: %d landings, %d base updates, %s", w.landings(), w.mainUpdates(), w.feature())
	}
	route, found := w.landingRoute()
	if !found || route.Landing != 1 {
		t.Fatalf("the route is %+v", route)
	}
}

func TestTheServiceRetriesTheOwnersLandingOnTheRebasedCommitAfterTheBaseMoved(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	mover := newBaseMover(t, "docs/notes.md")
	w := newRouteWorld(t, routeOptions{delivery: config.DeliveryJev, boundary: mover.boundary})
	mover.arm(w, "someone else's notes\n")
	first := w.awaitDraft()
	card := w.awaitCard()
	if strings.Join(card.Options, ",") != "approve,land" {
		t.Fatalf("the delivery card is %+v", card)
	}
	presented, err := w.f.c.Delivery(ctx, w.stream)
	fixturekit.Must(t, err)
	response, err := w.f.c.ApproveAndLand(ctx, w.stream, api.DeliveryLandDecision{Review: presented.Report.Review, ReviewRevision: presented.ReviewRevision, Commit: presented.Report.Commit, DraftHash: presented.DraftHash})
	fixturekit.Must(t, err)
	if response.Outcome != api.LandOutcomeBaseMoved {
		t.Fatalf("the response is %+v, want the base moved", response)
	}
	intakefixture.AwaitFurtherPasses(t, w.passes, 1)
	cards := w.cards.Load()
	rebased := w.awaitRebasedPush(first)
	w.requireRetriedLanding(first, rebased, mover.moved, trace.DecidedByOwner)
	fixturekit.Must(t, mover.failed())

	if d := w.delivered(); d.DecidedBy != trace.DecidedByOwner {
		t.Fatalf("the workstream was delivered by %+v", d)
	}
	if n := w.deliveryApprovals(); n != 1 {
		t.Fatalf("the owner approved %d times", n)
	}
	if n := w.cards.Load(); n != cards {
		t.Fatalf("the delivery card came back: %d looks at it after the approval, %d before the retry landed", n, cards)
	}
	if n := len(finalOperations(t, w.repository(), w.stream)); n < 2 {
		t.Fatalf("%d final reviews ran: the rebased workstream was not reviewed again", n)
	}
}

func TestTheServiceRetriesJevsLandingOnTheRebasedCommitAfterTheBaseMoved(t *testing.T) {
	t.Parallel()
	mover := newBaseMover(t, "docs/notes.md")
	p := confident("land", 0.97)
	w := newRouteWorld(t, routeOptions{jev: p, delivery: config.DeliveryJev, boundary: mover.boundary})
	mover.arm(w, "someone else's notes\n")
	first := w.awaitDraft()

	rebased := w.awaitRebasedPush(first)
	w.requireRetriedLanding(first, rebased, mover.moved, trace.DecidedByJev)
	fixturekit.Must(t, mover.failed())
	if d := w.delivered(); d.DecidedBy != trace.DecidedByJevGate || d.Judgment == "" {
		t.Fatalf("the workstream was delivered by %+v", d)
	}
	if n := routeDecisions(p); n != 1 {
		t.Fatalf("Jev was asked %d times", n)
	}
	route, _ := w.landingRoute()
	if route.Decider.By != trace.DecidedByJev {
		t.Fatalf("the route is %+v", route)
	}
	res, err := jev.Recorded(w.repository(), w.stream, jev.KindDeliveryRoute, jev.DeliveryRouteKey(w.stream, first.Review, first.Commit))
	fixturekit.Must(t, err)
	if res.Status != jev.Binding || res.Refused != "" {
		t.Fatalf("Jev's pick is %+v: the retry is the pick applied, not a refusal", res)
	}
	w.requireNoCards()
}

func TestARebasedPushWithBlockingFindingsDropsTheRouteAndShowsTheCard(t *testing.T) {
	t.Parallel()
	blocking := []any{map[string]any{"severity": "blocking", "finding": "The rebase broke the resume."}}
	for name, decideByJev := range map[string]bool{"the owner decided": false, "Jev decided": true} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			mover := newBaseMover(t, "docs/notes.md")
			o := routeOptions{delivery: config.DeliveryJev, boundary: mover.boundary, later: blocking}
			var p = confident("land", 0.97)
			if decideByJev {
				o.jev = p
			}
			w := newRouteWorld(t, o)
			mover.arm(w, "someone else's notes\n")
			first := w.awaitDraft()
			if !decideByJev {
				w.awaitCard()
				presented, err := w.f.c.Delivery(ctx, w.stream)
				fixturekit.Must(t, err)
				_, err = w.f.c.ApproveAndLand(ctx, w.stream, api.DeliveryLandDecision{Review: presented.Report.Review, ReviewRevision: presented.ReviewRevision, Commit: presented.Report.Commit, DraftHash: presented.DraftHash})
				fixturekit.Must(t, err)
			}
			rebased := w.awaitRebasedPush(first)

			var card api.InboxEntry
			w.await("the route to be dropped and the delivery card to appear", 0, func() bool {
				route, found := w.landingRoute()
				if !found || route.Status != delivery.LandingRouteDropped {
					return false
				}
				inbox, err := w.f.c.Inbox(ctx)
				if err != nil {
					return false
				}
				for _, e := range inbox.Entries {
					if e.Kind == api.InboxDelivery && e.Workstream == w.stream {
						card = e
						return true
					}
				}
				return false
			})
			if !strings.Contains(card.Question, "The rebase broke the resume") || card.Answer.Body["accept_blocking"] != true {
				t.Fatalf("the delivery card is %+v", card)
			}
			if got := w.rev("refs/heads/main"); got != mover.moved {
				t.Fatalf("the base branch is at %s, want it left at someone else's commit %s", got, mover.moved)
			}
			if got := w.rev("refs/heads/" + intake.FeatureBranch(w.stream)); got != rebased.Commit {
				t.Fatalf("the feature branch is at %s", got)
			}
			if w.feature() != intake.AssembledState || w.landings() != 1 {
				t.Fatalf("the workstream is %s after %d landing attempts", w.feature(), w.landings())
			}
			if creates, readies, _ := w.hostCounts(); creates != 1 || readies != 0 || !w.pull().Draft {
				t.Fatalf("%d pull requests opened, %d marked ready, draft %v", creates, readies, w.pull().Draft)
			}
			if decideByJev {
				if n := routeDecisions(p); n != 1 {
					t.Fatalf("Jev was asked %d times", n)
				}
			}
			w.advanceThroughPasses()
			if w.landings() != 1 || w.mainUpdates() != 1 || w.feature() != intake.AssembledState {
				t.Fatalf("a dropped route landed: %d landings, %d base updates, %s", w.landings(), w.mainUpdates(), w.feature())
			}
		})
	}
}

func (w *routeWorld) approveAndLand() api.DeliveryLandResponse {
	w.t.Helper()
	ctx := context.Background()
	presented, err := w.f.c.Delivery(ctx, w.stream)
	fixturekit.Must(w.t, err)
	response, err := w.f.c.ApproveAndLand(ctx, w.stream, api.DeliveryLandDecision{Review: presented.Report.Review, ReviewRevision: presented.ReviewRevision, Commit: presented.Report.Commit, DraftHash: presented.DraftHash})
	fixturekit.Must(w.t, err)
	return response
}

func (w *routeWorld) requireNotLanded(moved string) {
	w.t.Helper()
	t := w.t
	w.advanceThroughPasses()
	if got := w.rev("refs/heads/main"); got != moved || w.mainUpdates() != 1 {
		t.Fatalf("the base branch is at %s after %d updates, want it left at someone else's commit %s", got, w.mainUpdates(), moved)
	}
	if w.feature() != intake.AssembledState || w.landings() != 1 {
		t.Fatalf("the workstream is %s after %d landing attempts", w.feature(), w.landings())
	}
	if _, found := w.landed(); found {
		t.Fatal("a landing is recorded")
	}
	if creates, readies, closes := w.hostCounts(); creates != 1 || readies != 0 || closes != 0 || !w.pull().Draft {
		t.Fatalf("%d pull requests opened, %d marked ready, %d closed, draft %v", creates, readies, closes, w.pull().Draft)
	}
}

func TestARebaseConflictAfterTheBaseMovedTakesTheRepairPath(t *testing.T) {
	t.Parallel()

	mover := newBaseMover(t, "internal/trace/resume.go")
	w := newRouteWorld(t, routeOptions{delivery: config.DeliveryJev, boundary: mover.boundary})
	mover.arm(w, "package trace\n\n// Resume is someone else's.\n")
	w.awaitDraft()
	w.awaitCard()
	if response := w.approveAndLand(); response.Outcome != api.LandOutcomeBaseMoved {
		t.Fatalf("the response is %+v, want the base moved", response)
	}
	w.await("the conflicted rebase to take the repair path", 0, func() bool {
		for _, tr := range w.f.transitions(t, w.stream) {
			if tr.Subject == "drift" && strings.HasPrefix(tr.To, "conflicted-") {
				return true
			}
		}
		return false
	})
	w.requireNotLanded(mover.moved)
	if pushes, err := delivery.Pushes(w.repository(), w.stream); err != nil || len(pushes) != 1 {
		t.Fatalf("the conflicted workstream was pushed again: %v %v", pushes, err)
	}
	fixturekit.Must(t, mover.failed())
}

func TestFailingChecksAfterTheBaseMovedLandNothingAndShowTheCard(t *testing.T) {
	t.Parallel()
	mover := newBaseMover(t, "docs/notes.md")
	w := newRouteWorld(t, routeOptions{delivery: config.DeliveryJev, boundary: mover.boundary})
	mover.arm(w, "someone else's notes\n")
	first := w.awaitDraft()
	w.awaitCard()

	w.pulls.Mu.Lock()
	w.pulls.Statuses = func(_ context.Context, repository, commit string) (pulls.CommitStatuses, error) {
		if commit == first.Commit {
			return fixturekit.PassingStatuses(repository, commit), nil
		}
		at := fixturekit.DemoStart
		return pulls.CommitStatuses{Repository: repository, Commit: commit, Statuses: []pulls.CommitStatus{{ID: 1, Context: "load", State: "failure", Description: "module did not load", CreatedAt: at, UpdatedAt: at}}}, nil
	}
	w.pulls.Mu.Unlock()
	if response := w.approveAndLand(); response.Outcome != api.LandOutcomeBaseMoved {
		t.Fatalf("the response is %+v, want the base moved", response)
	}
	w.await("the route to be dropped", 0, func() bool {
		route, found := w.landingRoute()
		return found && route.Status == delivery.LandingRouteDropped
	})
	card := w.awaitCard()
	if !strings.Contains(card.Question, "Checks failed") {
		t.Fatalf("the delivery card is %+v", card)
	}
	w.requireNotLanded(mover.moved)
	fixturekit.Must(t, mover.failed())
}
