package executionjourney

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/delivery"
	"github.com/kpenfound/osmia/internal/pulls"
	"github.com/kpenfound/osmia/internal/reconcile"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	"github.com/kpenfound/osmia/internal/trace"
)

// lossyPulls is the fake pull request host with a host that can lose the
// response of marking a pull request ready, and that can refuse to answer or
// to update a pull request.
type lossyPulls struct {
	*fixturekit.FakePulls
	mu         sync.Mutex
	loseReady  bool
	unreadable bool
	noUpdate   bool
}

func (p *lossyPulls) set(change func(*lossyPulls)) {
	p.mu.Lock()
	defer p.mu.Unlock()
	change(p)
}

func (p *lossyPulls) Find(ctx context.Context, repository, headRepository, branch string) ([]pulls.PullRequest, error) {
	p.mu.Lock()
	unreadable := p.unreadable
	p.mu.Unlock()
	if unreadable {
		return nil, errors.New("the host is unreachable")
	}
	return p.FakePulls.Find(ctx, repository, headRepository, branch)
}

func (p *lossyPulls) Update(ctx context.Context, repository string, number int, change pulls.Update) (pulls.PullRequest, error) {
	p.mu.Lock()
	refuse := p.noUpdate
	p.mu.Unlock()
	if refuse {
		return pulls.PullRequest{}, errors.New("the host refused the update")
	}
	return p.FakePulls.Update(ctx, repository, number, change)
}

// Ready marks the pull request ready on the host and loses the response, as a
// connection reset after the host acted does.
func (p *lossyPulls) Ready(ctx context.Context, repository string, number int) (pulls.PullRequest, error) {
	pr, err := p.FakePulls.Ready(ctx, repository, number)
	p.mu.Lock()
	lose := p.loseReady
	p.mu.Unlock()
	if err == nil && lose {
		return pulls.PullRequest{}, errors.New("connection reset")
	}
	return pr, err
}

// publicationClock is the controller's clock: a minute passes between passes.
type publicationClock struct {
	mu sync.Mutex
	at time.Time
}

func (c *publicationClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.at
}

func (c *publicationClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.at = c.at.Add(d)
}

// boundedPublication is an approved "Mark ready for review" asked for as a
// publication operation, and a controller that reconciles it.
type boundedPublication struct {
	d          *draftFixture
	pulls      *lossyPulls
	clock      *publicationClock
	controller *reconcile.Controller
}

func newBoundedPublication(t *testing.T) *boundedPublication {
	t.Helper()
	d := newDraftFixture(t, "commit-per-unit")
	d.read(1)
	if result := d.settle(t, d.first); result.Outcome != "succeeded" {
		t.Fatalf("final review %+v", result)
	}
	d.recordBrief(t, "Restart the service")
	lossy := &lossyPulls{FakePulls: d.pulls}
	d.f.s.options.PullRequests = lossy
	if _, apiErr := d.decide(d.present(t)); apiErr != nil {
		t.Fatal(apiErr)
	}
	must(t, (&publisher{s: d.f.s, repository: d.repository}).Pass(context.Background()))
	clock := &publicationClock{at: time.Now().UTC()}
	controller, err := reconcile.New(d.repository, reconcile.Options{
		Worker: "publication-test", Now: clock.Now, RetryDelay: time.Minute, MaxRetryDelay: time.Minute, GiveUpWindow: 10 * time.Minute,
		Adapters: map[coreadapter.OperationBoundary]coreadapter.Reconciler{coreadapter.RepositoryBoundary: &delivery.Publisher{S: d.f.s.delivery(), Repository: d.repository}},
		Hold:     func(_ config.WorkstreamID, op coreadapter.Operation) bool { return op.Action != PublishAction },
	})
	must(t, err)
	return &boundedPublication{d, lossy, clock, controller}
}

// pass makes the given number of passes a minute apart.
func (b *boundedPublication) pass(t *testing.T, passes int) {
	t.Helper()
	for range passes {
		must(t, b.controller.Pass(context.Background()))
		b.clock.advance(time.Minute)
	}
}

func (b *boundedPublication) operation(t *testing.T) trace.OperationRecord {
	t.Helper()
	ops, err := b.d.repository.Operations(b.d.stream)
	must(t, err)
	for _, o := range ops {
		if o.Operation.Action == PublishAction {
			return o
		}
	}
	t.Fatal("no publication operation")
	return trace.OperationRecord{}
}

func (b *boundedPublication) notices(t *testing.T, words string) []string {
	t.Helper()
	entries, err := b.d.repository.Outbox(b.d.stream)
	must(t, err)
	var bodies []string
	for _, e := range entries {
		if e.Event.Kind == trace.NoticeKind && strings.Contains(e.Event.Body, words) {
			bodies = append(bodies, e.Event.Body)
		}
	}
	return bodies
}

// The host marks the pull request ready and the response is lost; every later
// attempt cannot read the host. Past the limit and the window the publication
// is recorded as delivered-ready through its own success path: no second ready
// call is made and nothing is refused.
func TestPublicationWhoseReadyResponseWasLostIsRecordedWhenGivenUp(t *testing.T) {
	t.Parallel()
	b := newBoundedPublication(t)
	b.pulls.set(func(p *lossyPulls) { p.loseReady = true })
	b.pass(t, 1)
	if op := b.operation(t); op.Result != nil || reconcile.ConsecutiveFailures(op) != 1 || b.d.pulls.Readies != 1 || b.d.pulls.Prs[0].Draft {
		t.Fatalf("after the lost response: %d ready calls, %+v", b.d.pulls.Readies, b.operation(t))
	}
	b.pulls.set(func(p *lossyPulls) { p.unreadable = true })
	b.pass(t, reconcile.GiveUpFailures+1)
	if op := b.operation(t); op.Result != nil {
		t.Fatalf("given up inside the window: %+v", op.Result)
	}
	// The window passes and the host answers again.
	b.clock.advance(10 * time.Minute)
	b.pulls.set(func(p *lossyPulls) { p.unreadable = false })
	b.pass(t, 1)

	op := b.operation(t)
	if op.Result == nil || op.Result.Outcome != "succeeded" || !op.Acknowledged {
		t.Fatalf("the publication ended as %+v", op)
	}
	if b.d.pulls.Readies != 1 {
		t.Fatalf("the pull request was marked ready %d times", b.d.pulls.Readies)
	}
	if refused, err := publicationRefused(b.d.repository, b.d.stream, 1); err != nil || refused {
		t.Fatalf("the publication was refused: %v, %v", refused, err)
	}
	if got := b.d.feature(t); got != DeliveredState {
		t.Fatalf("the workstream is %s, want delivered", got)
	}
	ready, recorded, err := delivery.ReadyAt(b.d.repository, b.d.stream)
	must(t, err)
	if !recorded || ready.Approval != 1 || ready.PullRequest != b.d.pulls.Prs[0].Number || ready.Draft {
		t.Fatalf("the ready record %+v", ready)
	}
	if got := b.notices(t, "its effect is already on the pull request"); len(got) != 1 {
		t.Fatalf("notices of the recorded success: %q", got)
	}
}

// The remote cannot be read at the threshold: the publication is ended saying
// its outcome could not be determined, never that the pull request was not
// marked ready, the chief of staff is told once, and nothing is pushed,
// marked ready or updated.
func TestPublicationGivenUpOnAnUnreadableRemoteSaysItsOutcomeCouldNotBeDetermined(t *testing.T) {
	t.Parallel()
	b := newBoundedPublication(t)
	b.pulls.set(func(p *lossyPulls) { p.unreadable = true })
	pushes, readies, updates, creates := b.d.pushCount(t), b.d.pulls.Readies, b.d.pulls.Updates, b.d.pulls.Creates
	b.pass(t, reconcile.GiveUpFailures+1)
	if op := b.operation(t); op.Result != nil {
		t.Fatalf("given up inside the window: %+v", op.Result)
	}
	b.clock.advance(10 * time.Minute)
	b.pass(t, 1)

	op := b.operation(t)
	if op.Result == nil || op.Result.Outcome != "failed" || !op.Acknowledged {
		t.Fatalf("the publication ended as %+v", op)
	}
	for _, words := range []string{delivery.RefusedAfterRetries, delivery.RefusedUnknownOutcome, "the host is unreachable"} {
		if !strings.Contains(op.Result.Evidence, words) {
			t.Fatalf("the result lacks %q: %s", words, op.Result.Evidence)
		}
	}
	if strings.Contains(op.Result.Evidence, "not marked ready") || strings.Contains(op.Result.Evidence, "confirmed absent") {
		t.Fatalf("the result claims more than is known: %s", op.Result.Evidence)
	}
	if got := b.notices(t, "could not be determined"); len(got) != 1 || strings.Contains(got[0], "was not marked ready") {
		t.Fatalf("notices of the unknown outcome: %q", got)
	}
	if got := b.notices(t, "not marked ready"); len(got) != 0 {
		t.Fatalf("the chief of staff was told the pull request was not marked ready: %q", got)
	}
	if b.d.pushCount(t) != pushes || b.d.pulls.Readies != readies || b.d.pulls.Updates != updates || b.d.pulls.Creates != creates {
		t.Fatalf("the give-up changed the remote: %d pushes (was %d), %d ready calls (was %d), %d updates (was %d), %d creates (was %d)", b.d.pushCount(t), pushes, b.d.pulls.Readies, readies, b.d.pulls.Updates, updates, b.d.pulls.Creates, creates)
	}
	if got := b.d.feature(t); got != AssembledState {
		t.Fatalf("the workstream is %s, want assembled", got)
	}
}

// A publication whose effect is confirmed absent is refused through the
// publication's own refusal, with the last failure as its reason and one
// notice to the chief of staff; the pull request is left as it was.
func TestPublicationWhoseEffectIsConfirmedAbsentIsRefusedWhenGivenUp(t *testing.T) {
	t.Parallel()
	b := newBoundedPublication(t)
	b.pulls.set(func(p *lossyPulls) { p.noUpdate = true })
	b.pass(t, reconcile.GiveUpFailures+1)
	if op := b.operation(t); op.Result != nil || reconcile.ConsecutiveFailures(op) != reconcile.GiveUpFailures+1 {
		t.Fatalf("inside the window: %+v", op)
	}
	b.clock.advance(10 * time.Minute)
	b.pass(t, 1)

	op := b.operation(t)
	if op.Result == nil || op.Result.Outcome != "failed" || !op.Acknowledged {
		t.Fatalf("the publication ended as %+v", op)
	}
	for _, words := range []string{delivery.RefusedAfterRetries, "the effect is confirmed absent", "still a draft"} {
		if !strings.Contains(op.Result.Evidence, words) {
			t.Fatalf("the refusal lacks %q: %s", words, op.Result.Evidence)
		}
	}
	if refused, err := publicationRefused(b.d.repository, b.d.stream, 1); err != nil || !refused {
		t.Fatalf("the publication was not refused: %v, %v", refused, err)
	}
	if b.d.pulls.Readies != 0 || !b.d.pulls.Prs[0].Draft || b.d.feature(t) != AssembledState {
		t.Fatalf("the give-up changed the pull request: %d ready calls, %+v, %s", b.d.pulls.Readies, b.d.pulls.Prs[0], b.d.feature(t))
	}
	if got := b.notices(t, "not marked ready"); len(got) != 1 {
		t.Fatalf("notices of the refusal: %q", got)
	}
}
