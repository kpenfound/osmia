package executionjourney

import (
	context "context"
	"github.com/kpenfound/osmia/internal/api"
	"github.com/kpenfound/osmia/internal/delivery"
	"github.com/kpenfound/osmia/internal/intake"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	"github.com/kpenfound/osmia/internal/servicetest/intakefixture"
	reflect "reflect"
	testing "testing"
	time "time"

	trace "github.com/kpenfound/osmia/internal/trace"
)

func TestDeliveryApprovalRefusedOnStaleRevisionLeavesStateUnchanged(t *testing.T) {
	t.Parallel()
	f, stream, repository, _ := deliveryFixture(t)
	defer repository.Close()
	ctx := context.Background()

	p, apiErr := f.s.deliveryPresentation(ctx, string(stream))
	if apiErr != nil {
		t.Fatal(apiErr)
	}
	before := intakefixture.StreamDocuments(t, repository, stream, delivery.DeliveryDocument)

	stale := api.DeliveryDecision{Review: p.Report.Review, ReviewRevision: p.ReviewRevision, Commit: p.Report.Commit, DraftHash: "not-the-current-draft-hash"}
	if _, apiErr := f.s.approveDelivery(ctx, string(stream), stale); !intakefixture.Failed(apiErr, api.Conflict) {
		t.Fatalf("approving with a stale draft hash: %v", apiErr)
	}
	if after := intakefixture.StreamDocuments(t, repository, stream, delivery.DeliveryDocument); len(after) != len(before) {
		t.Fatalf("a refused approval recorded a document: before %+v, after %+v", before, after)
	}
	again, apiErr := f.s.deliveryPresentation(ctx, string(stream))
	if apiErr != nil || again.Approval != nil {
		t.Fatalf("the refusal left an approval recorded: %+v %v", again.Approval, apiErr)
	}

	accepted, apiErr := f.s.approveDelivery(ctx, string(stream), api.DeliveryDecision{Review: p.Report.Review, ReviewRevision: p.ReviewRevision, Commit: p.Report.Commit, DraftHash: p.DraftHash})
	if apiErr != nil || accepted.Description != p.Draft {
		t.Fatalf("approving with the current revision: %+v %v", accepted, apiErr)
	}
}

func TestOwnerActionOnTheBlockedWorkstreamRespondsWithinBound(t *testing.T) {
	t.Parallel()
	blocking := fixturekit.NewBlockedFactoryOperation()
	_, c, repository := setUpBusyReadsService(t, blocking)
	fixturekit.RequestFactoryOperation(t, repository, fixturekit.Stream, "command-admission-blocked")

	select {
	case <-blocking.Started:
	case <-time.After(10 * time.Second):
		t.Fatal("the fake factory operation never started")
	}
	defer close(blocking.Release)

	ctx := context.Background()
	started := time.Now()
	if _, err := c.Abandon(ctx, fixturekit.Stream, "owner action while this workstream's own factory work is blocked"); err != nil {
		t.Fatalf("owner action on the blocked workstream: %v", err)
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("owner action on the blocked workstream took %s, want within %s", elapsed, time.Second)
	}
}

func TestAmendmentDecisionResubmittedAcrossARestartAppliesOnce(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f, stream := builtForAmendment(t, 1)
	requester := presentAmendment(t, f, stream, intake.MasonRole, amendedSpec, fixturekit.ValidPlan)
	f.stop(t)
	repo, err := trace.Open(f.s.cfg.Root, f.s.cfg.Project)
	fixturekit.Must(t, err)
	f.s.setSole(&activeProject{repository: repo})

	decide := api.AmendmentDecisionRequest{Decision: intake.AmendmentApprove, Packet: 1}
	accepted, apiErr := f.s.decideAmendment(ctx, string(stream), "1", decide)
	if apiErr != nil || accepted.State != intake.AmendmentApproved {
		t.Fatalf("accepted decision %+v %v", accepted, apiErr)
	}

	resubmitted, apiErr := f.s.decideAmendment(ctx, string(stream), "1", decide)
	if apiErr != nil || !reflect.DeepEqual(resubmitted.Decision, accepted.Decision) {
		t.Fatalf("resubmission before restart %+v %v, want the recorded decision %+v", resubmitted, apiErr, accepted.Decision)
	}
	if got := f.revisions(t, stream, intake.AmendmentDecisionID("1")); !reflect.DeepEqual(got, []int{1}) {
		t.Fatalf("decision revisions before restart %v, want exactly one", got)
	}

	fixturekit.Must(t, repo.Close())
	f.start(t)
	defer f.stop(t)
	f.awaitAmendment(t, stream, intake.AmendmentRuled)

	again, err := f.c.DecideAmendment(ctx, stream, "1", decide)
	if err != nil || !reflect.DeepEqual(again.Decision, accepted.Decision) {
		t.Fatalf("resubmission after restart %+v %v, want the recorded decision %+v", again, err, accepted.Decision)
	}
	if got := f.revisions(t, stream, intake.AmendmentDecisionID("1")); !reflect.DeepEqual(got, []int{1}) {
		t.Fatalf("decision revisions after restart %v, want exactly one", got)
	}
	if turns := f.ruling(t, stream, requester); len(turns) != 1 {
		t.Fatalf("ruling turns %+v, want exactly one", turns)
	}
}
