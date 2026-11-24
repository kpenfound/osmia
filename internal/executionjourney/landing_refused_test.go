package executionjourney

import (
	"context"
	"github.com/kpenfound/osmia/internal/execution"
	"github.com/kpenfound/osmia/internal/intake"
	"github.com/kpenfound/osmia/internal/service"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	"strconv"
	"strings"
	"testing"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/jev/jevfixture"
	"github.com/kpenfound/osmia/internal/reconcile"
	"github.com/kpenfound/osmia/internal/router"
	"github.com/kpenfound/osmia/internal/trace"
)

type refusingLanding struct {
	*foreman
	reason string
}

func (r refusingLanding) Apply(ctx context.Context, op coreadapter.Operation) (coreadapter.OperationResult, error) {
	in, err := execution.DecodeLand(op)
	if err != nil {
		return coreadapter.OperationResult{}, err
	}
	stream, err := r.foreman.driver().Stream(op, in)
	if err != nil {
		return coreadapter.OperationResult{}, err
	}
	return r.foreman.driver().Refuse(ctx, stream, in, r.reason)
}

func settleLandings(t *testing.T, f *shedFixture, repo *trace.Repository, adapter coreadapter.Reconciler) {
	t.Helper()
	controller, err := reconcile.New(repo, reconcile.Options{Worker: "landing-refused-fixture",
		Concurrent: service.TestConcurrentOperation, Class: service.TestOperationClass, Limit: InspectForTest(f.s).GitOperationLimit(repo, nil),
		Adapters: map[coreadapter.OperationBoundary]coreadapter.Reconciler{coreadapter.RepositoryBoundary: adapter}})
	fixturekit.Must(t, err)
	fixturekit.Must(t, controller.Pass(context.Background()))
	fixturekit.Must(t, controller.Wait())
}

func TestARefusedLandingOfAStaleApprovalReturnsTheUnitToChecking(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f, stream, repo := newApprovedFixture(t, "refused-stale")
	defer repo.Close()
	p := &jevfixture.Transitions{}
	routeThrough(f.s, p)
	lands := &foreman{masons: newMasonController(f.s, repo)}
	review, result := approvedReview(t, repo, stream, "resume")

	fixturekit.DemoGit(t, f.clone, "-C", f.clone, "update-ref", "refs/heads/"+execution.UnitBranch(stream, "resume"), result.Identity.Candidate.BaseRevision)
	fixturekit.Must(t, lands.Pass(ctx))
	settleLandings(t, f, repo, lands)
	ops := landOperations(t, repo, stream)
	if len(ops) != 1 || ops[0].Result == nil || ops[0].Result.Outcome != "failed" {
		t.Fatalf("the refused landing %+v", ops)
	}
	if got := workflowOf(t, repo, stream, "resume"); got.Value != intake.UnitApproved {
		t.Fatalf("the refusal itself moved the unit to %s", got.Value)
	}

	fixturekit.Must(t, lands.Pass(ctx))

	if got := workflowOf(t, repo, stream, "resume"); got.Value != execution.UnitChecking {
		t.Fatalf("a refused landing of a stale approval left the unit %s, want checking", got.Value)
	}
	moved := latestOf(t, repo, stream, trace.UnitSubject("resume"))
	wantID := trace.UnitSubject("resume") + "-checking-landing-" + strconv.Itoa(review.Revision) + "-1"
	if moved.ID != wantID || moved.To != execution.UnitChecking || moved.DefaultTo != execution.UnitChecking || moved.DecidedBy != trace.DecidedByCode || moved.Departed() {
		t.Fatalf("the return to checking %+v, want %s", moved, wantID)
	}
	rec := routeOf(t, repo, stream, "unit.landing_refused")
	if rec.State != router.StateCommitted || rec.Outcome != router.OutcomeFollowed || rec.Default != execution.UnitChecking || rec.Target != execution.UnitChecking || rec.Authority != "" {
		t.Fatalf("route %+v", rec)
	}
	for _, op := range landOperations(t, repo, stream) {
		in, err := execution.DecodeLand(op.Operation)
		fixturekit.Must(t, err)
		if in.Unit == "resume" && op.Result == nil {
			t.Fatalf("a second landing of the refused approval was asked for: %+v", in)
		}
		if in.Unit == "resume" && in.Attempt > 1 {
			t.Fatalf("a refused landing of a stale approval was asked for again: %+v", in)
		}
	}
}

func TestAGivenUpLandingIsLandedAgainAsTheNextAttempt(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	f, _ := newMasonFixtureOn(t, config.WorkspacesGit, "masons = 1\n", independentPlan)
	base := strings.TrimSpace(fixturekit.DemoGit(t, f.clone, "-C", f.clone, "rev-parse", "HEAD"))
	stream, repo := seedBuild(t, f, "refused-gave-up", independentPlan, config.WorkspacesGit, base)
	seedReview(t, f, repo, stream, "resume", resumeReport)
	approveDirectly(t, f.s, repo, stream, "resume", "spec#1")
	defer repo.Close()
	p := &jevfixture.Transitions{}
	routeThrough(f.s, p)
	lands := &foreman{masons: newMasonController(f.s, repo)}
	review, result := approvedReview(t, repo, stream, "resume")

	fixturekit.Must(t, lands.Pass(ctx))
	settleLandings(t, f, repo, refusingLanding{foreman: lands, reason: "gave up after repeated failures"})
	if got := workflowOf(t, repo, stream, "resume"); got.Value != intake.UnitApproved {
		t.Fatalf("the unit is %s", got.Value)
	}

	fixturekit.Must(t, lands.Pass(ctx))
	again := latestOf(t, repo, stream, trace.UnitSubject("resume"))
	if again.To != intake.UnitApproved || again.DefaultTo != intake.UnitApproved || again.DecidedBy != trace.DecidedByReviewer || !strings.Contains(again.ID, "-approved-again-") {
		t.Fatalf("the decision to land again %+v", again)
	}
	rec := routeOf(t, repo, stream, "unit.landing_refused")
	if rec.Authority != router.AuthorityReviewer || rec.Outcome != router.OutcomeFollowed || rec.Target != intake.UnitApproved {
		t.Fatalf("route %+v", rec)
	}
	fixturekit.Must(t, lands.Pass(ctx))
	ops := landOperations(t, repo, stream)
	if len(ops) != 2 {
		t.Fatalf("%d landings, want the refused one and its second attempt: %+v", len(ops), ops)
	}
	second, err := execution.DecodeLand(ops[1].Operation)
	fixturekit.Must(t, err)
	if second.Attempt != 2 || second.Unit != "resume" || second.Review != review.Revision || second.Candidate != result.Identity.Candidate.Revision {
		t.Fatalf("the second attempt %+v", second)
	}
	if ops[0].Operation.ID == ops[1].Operation.ID {
		t.Fatalf("both attempts share the operation %s", ops[0].Operation.ID)
	}

	settleLandings(t, f, repo, lands)
	if got := workflowOf(t, repo, stream, "resume"); got.Value != intake.UnitMerged {
		t.Fatalf("the second attempt left the unit %s", got.Value)
	}
	if commits := f.landedCommits(t, stream, result.Identity.Candidate.BaseRevision); len(commits) != 1 {
		t.Fatalf("the feature branch holds %v", commits)
	}
}
