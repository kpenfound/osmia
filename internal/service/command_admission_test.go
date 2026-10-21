package service

import (
	"context"
	"reflect"
	"testing"
	"time"

	"github.com/kpenfound/osmia/internal/trace"
)

// Approving delivery with a revision the client's view is now stale on
// (here, a draft hash that no longer matches the current presentation) is
// refused with a reason and records nothing: no new delivery-approval
// document, and the current presentation is unchanged. Ratify and amendment
// decisions are refused the same way on a stale revision
// (TestRatifyListsEveryReasonItIsRefused and the "approving the first
// packet" case in TestOwnerAsksForAnotherAmendmentRound), so this covers
// delivery, ratification and amendment decisions: three gated actions whose
// commands never pass on a stale view (spec#3, plan#command-admission
// acceptance item 3).
func TestDeliveryApprovalRefusedOnStaleRevisionLeavesStateUnchanged(t *testing.T) {
	t.Parallel()
	f, stream, repository, _ := deliveryFixture(t)
	defer repository.Close()
	ctx := context.Background()

	p, api := f.s.deliveryPresentation(ctx, string(stream))
	if api != nil {
		t.Fatal(api)
	}
	before := streamDocuments(t, repository, stream, deliveryDocument)

	stale := DeliveryDecision{Review: p.Report.Review, ReviewRevision: p.ReviewRevision, Commit: p.Report.Commit, DraftHash: "not-the-current-draft-hash"}
	if _, api := f.s.approveDelivery(ctx, string(stream), stale); !failed(api, Conflict) {
		t.Fatalf("approving with a stale draft hash: %v", api)
	}
	if after := streamDocuments(t, repository, stream, deliveryDocument); len(after) != len(before) {
		t.Fatalf("a refused approval recorded a document: before %+v, after %+v", before, after)
	}
	again, api := f.s.deliveryPresentation(ctx, string(stream))
	if api != nil || again.Approval != nil {
		t.Fatalf("the refusal left an approval recorded: %+v %v", again.Approval, api)
	}

	// The same decision, with the revision the current presentation
	// actually carries, is accepted.
	accepted, api := f.s.approveDelivery(ctx, string(stream), DeliveryDecision{Review: p.Report.Review, ReviewRevision: p.ReviewRevision, Commit: p.Report.Commit, DraftHash: p.DraftHash})
	if api != nil || accepted.Description != p.Draft {
		t.Fatalf("approving with the current revision: %+v %v", accepted, api)
	}
}

// An owner action on the very workstream a factory operation is blocked on
// still answers within the latency bound: it takes only the repository's
// r.mu, briefly, never the operationMu the blocked operation holds for its
// whole run (busy_reads_test.go, blockedFactoryOperation). This complements
// TestOwnerActionOnAnotherWorkstreamDoesNotWaitForBlockedFactoryWork, which
// shows the same for a different workstream, and together they cover
// spec#2's "with backend work stalled, each owner action gets a response
// within the asserted bound".
func TestOwnerActionOnTheBlockedWorkstreamRespondsWithinBound(t *testing.T) {
	t.Parallel()
	blocking := newBlockedFactoryOperation()
	_, c, repository := setUpBusyReadsService(t, blocking)
	requestFactoryOperation(t, repository, stream, "command-admission-blocked")

	select {
	case <-blocking.Started:
	case <-time.After(10 * time.Second):
		t.Fatal("the fake factory operation never started")
	}
	defer close(blocking.Release)

	ctx := context.Background()
	started := time.Now()
	if _, err := c.Abandon(ctx, stream, "owner action while this workstream's own factory work is blocked"); err != nil {
		t.Fatalf("owner action on the blocked workstream: %v", err)
	}
	if elapsed := time.Since(started); elapsed > coreReadBound {
		t.Fatalf("owner action on the blocked workstream took %s, want within %s", elapsed, coreReadBound)
	}
}

// An owner decision accepted and durably recorded before a restart, whose
// long-running follow-through (resealing, applying and ruling an amendment)
// has not yet completed when the service comes back, is reconciled exactly
// once; resubmitting the identical command, both before and after the
// restart, is answered from the record rather than applied again. This
// extends TestAmendmentDecisionIsCompletedOnceAfterARestart with the
// resubmission the owner's client would actually retry with, covering
// spec#2's restart and idempotency requirements together.
func TestAmendmentDecisionResubmittedAcrossARestartAppliesOnce(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f, stream := builtForAmendment(t, 1)
	requester := presentAmendment(t, f, stream, masonRole, amendedSpec, validPlan)
	f.stop(t)
	repo, err := trace.Open(f.s.cfg.Root, f.s.cfg.Project)
	must(t, err)
	f.s.setSole(&activeProject{repository: repo})

	decide := AmendmentDecisionRequest{Decision: AmendmentApprove, Packet: 1}
	accepted, api := f.s.decideAmendment(ctx, string(stream), "1", decide)
	if api != nil || accepted.State != amendmentApproved {
		t.Fatalf("accepted decision %+v %v", accepted, api)
	}
	// Resubmitting the same command before the restart, with no controller
	// running to act on it yet, is answered from the record alone: the same
	// decision, recorded once.
	resubmitted, api := f.s.decideAmendment(ctx, string(stream), "1", decide)
	if api != nil || !reflect.DeepEqual(resubmitted.Decision, accepted.Decision) {
		t.Fatalf("resubmission before restart %+v %v, want the recorded decision %+v", resubmitted, api, accepted.Decision)
	}
	if got := f.revisions(t, stream, amendmentDecisionID("1")); !reflect.DeepEqual(got, []int{1}) {
		t.Fatalf("decision revisions before restart %v, want exactly one", got)
	}

	must(t, repo.Close())
	f.start(t)
	defer f.stop(t)
	f.awaitAmendment(t, stream, amendmentRuled)

	// Resubmitting the same command again, now that the follow-through has
	// completed, is still answered from the record: it neither records a
	// second decision nor delivers a second ruling.
	again, err := f.c.DecideAmendment(ctx, stream, "1", decide)
	if err != nil || !reflect.DeepEqual(again.Decision, accepted.Decision) {
		t.Fatalf("resubmission after restart %+v %v, want the recorded decision %+v", again, err, accepted.Decision)
	}
	if got := f.revisions(t, stream, amendmentDecisionID("1")); !reflect.DeepEqual(got, []int{1}) {
		t.Fatalf("decision revisions after restart %v, want exactly one", got)
	}
	if turns := f.ruling(t, stream, requester); len(turns) != 1 {
		t.Fatalf("ruling turns %+v, want exactly one", turns)
	}
}
