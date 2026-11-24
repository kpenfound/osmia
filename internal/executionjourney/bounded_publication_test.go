package executionjourney

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kpenfound/osmia/internal/intake"
	"github.com/kpenfound/osmia/internal/servicetest/journey"

	"github.com/kpenfound/osmia/internal/delivery"
	"github.com/kpenfound/osmia/internal/pulls"
	"github.com/kpenfound/osmia/internal/reconcile"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	"github.com/kpenfound/osmia/internal/trace"
)

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

type boundedPublication struct {
	d     *draftFixture
	pulls *lossyPulls
	retryPasses
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
	d.f.S.Options.PullRequests = lossy
	if _, apiErr := d.decide(d.present(t)); apiErr != nil {
		t.Fatal(apiErr)
	}
	fixturekit.Must(t, (&journey.Publisher{S: d.f.S, Repository: d.repository}).Pass(context.Background()))
	passes := newRetryPasses(t, d.repository, "publication-test", delivery.PublishAction, &delivery.Publisher{S: d.f.S.Delivery(), Repository: d.repository})
	return &boundedPublication{d, lossy, passes}
}

func (b *boundedPublication) operation(t *testing.T) trace.OperationRecord {
	t.Helper()
	ops, err := b.d.repository.Operations(b.d.stream)
	fixturekit.Must(t, err)
	for _, o := range ops {
		if o.Operation.Action == delivery.PublishAction {
			return o
		}
	}
	t.Fatal("no publication operation")
	return trace.OperationRecord{}
}

func (b *boundedPublication) notices(t *testing.T, words string) []string {
	t.Helper()
	entries, err := b.d.repository.Outbox(b.d.stream)
	fixturekit.Must(t, err)
	var bodies []string
	for _, e := range entries {
		if e.Event.Kind == trace.NoticeKind && strings.Contains(e.Event.Body, words) {
			bodies = append(bodies, e.Event.Body)
		}
	}
	return bodies
}

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

	b.clock.Advance(10 * time.Minute)
	b.pulls.set(func(p *lossyPulls) { p.unreadable = false })
	b.pass(t, 1)

	op := b.operation(t)
	if op.Result == nil || op.Result.Outcome != "succeeded" || !op.Acknowledged {
		t.Fatalf("the publication ended as %+v", op)
	}
	if b.d.pulls.Readies != 1 {
		t.Fatalf("the pull request was marked ready %d times", b.d.pulls.Readies)
	}
	if refused, err := delivery.PublicationRefused(b.d.repository, b.d.stream, 1); err != nil || refused {
		t.Fatalf("the publication was refused: %v, %v", refused, err)
	}
	if got := b.d.feature(t); got != intake.DeliveredState {
		t.Fatalf("the workstream is %s, want delivered", got)
	}
	ready, recorded, err := delivery.ReadyAt(b.d.repository, b.d.stream)
	fixturekit.Must(t, err)
	if !recorded || ready.Approval != 1 || ready.PullRequest != b.d.pulls.Prs[0].Number || ready.Draft {
		t.Fatalf("the ready record %+v", ready)
	}
	if got := b.notices(t, "its effect is already on the pull request"); len(got) != 1 {
		t.Fatalf("notices of the recorded success: %q", got)
	}
}

func TestPublicationGivenUpOnAnUnreadableRemoteSaysItsOutcomeCouldNotBeDetermined(t *testing.T) {
	t.Parallel()
	b := newBoundedPublication(t)
	b.pulls.set(func(p *lossyPulls) { p.unreadable = true })
	pushes, readies, updates, creates := b.d.pushCount(t), b.d.pulls.Readies, b.d.pulls.Updates, b.d.pulls.Creates
	b.pass(t, reconcile.GiveUpFailures+1)
	if op := b.operation(t); op.Result != nil {
		t.Fatalf("given up inside the window: %+v", op.Result)
	}
	b.clock.Advance(10 * time.Minute)
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
	if got := b.d.feature(t); got != intake.AssembledState {
		t.Fatalf("the workstream is %s, want assembled", got)
	}
}

func TestPublicationWhoseEffectIsConfirmedAbsentIsRefusedWhenGivenUp(t *testing.T) {
	t.Parallel()
	b := newBoundedPublication(t)
	b.pulls.set(func(p *lossyPulls) { p.noUpdate = true })
	b.pass(t, reconcile.GiveUpFailures+1)
	if op := b.operation(t); op.Result != nil || reconcile.ConsecutiveFailures(op) != reconcile.GiveUpFailures+1 {
		t.Fatalf("inside the window: %+v", op)
	}
	b.clock.Advance(10 * time.Minute)
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
	if refused, err := delivery.PublicationRefused(b.d.repository, b.d.stream, 1); err != nil || !refused {
		t.Fatalf("the publication was not refused: %v, %v", refused, err)
	}
	if b.d.pulls.Readies != 0 || !b.d.pulls.Prs[0].Draft || b.d.feature(t) != intake.AssembledState {
		t.Fatalf("the give-up changed the pull request: %d ready calls, %+v, %s", b.d.pulls.Readies, b.d.pulls.Prs[0], b.d.feature(t))
	}
	if got := b.notices(t, "not marked ready"); len(got) != 1 {
		t.Fatalf("notices of the refusal: %q", got)
	}
}
