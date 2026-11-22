package lifecycle_test

import (
	"context"
	"slices"
	"testing"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/intake"
	"github.com/kpenfound/osmia/internal/seal"
	"github.com/kpenfound/osmia/internal/trace"
)

// A workstream reopened for plan-only revisions keeps its seal, yet each
// reassembly records its own move to assembled: the second reopen under the
// same seal reassembles as the first did, and further passes over an
// assembled workstream record no
// second move for the same assembly.
func TestPlanOnlyReopensUnderOneSealEachReassemble(t *testing.T) {
	t.Parallel()
	f, stream, repository, a := newFinalFixture(t, "plan-only-reassembly")
	hostFinalReviews(t, f, stream)
	f.s.setSole(&activeProject{repository: repository})
	ctx := context.Background()
	_, op := assembleBoth(t, f, repository, a, stream,
		map[string]string{"internal/trace/resume.go": "package trace\n"},
		map[string]string{"internal/trace/dedupe.go": "package trace\n"})
	failFinalReview(t, f, repository, stream, a, 1, op)
	sealed, doc, found, err := seal.Latest(repository, stream)
	must(t, err)
	if !found {
		t.Fatal("the workstream has no seal")
	}

	assemblies := func() []string {
		t.Helper()
		transitions, err := trace.Read[trace.Transition](repository, stream)
		must(t, err)
		var ids []string
		for _, tr := range transitions {
			if tr.Subject == trace.FeatureSubject && tr.To == AssembledState {
				ids = append(ids, tr.ID)
			}
		}
		return ids
	}
	for k, want := range []string{"assembled-2", "assembled-3"} {
		if _, api := f.s.ownerMoveWorkstream(ctx, string(stream), WorkstreamMoveRequest{To: BuildingState, Note: "Reopen it for repair units."}); api != nil {
			t.Fatalf("reopen %d: %s", k+1, api.Message)
		}
		must(t, a.Pass(ctx))
		if feature, err := repository.Workflow(stream, trace.FeatureSubject); err != nil || feature.Value != BuildingState {
			t.Fatalf("reopen %d reassembled before any revision: %+v %v", k+1, feature, err)
		}
		recordPlanOnlyRevision(t, repository, stream, sealed, doc.Revision, k+1)
		must(t, a.Pass(ctx))
		if feature, err := repository.Workflow(stream, trace.FeatureSubject); err != nil || feature.Value != AssembledState {
			t.Fatalf("reopen %d did not reassemble: %+v %v", k+1, feature, err)
		}
		if moved := lastOn(t, repository, stream, trace.FeatureSubject); moved.ID != want || moved.From != BuildingState {
			t.Fatalf("reassembly %d recorded %+v", k+1, moved)
		}
		if body := noticeOf(t, repository, stream, want); body == "" {
			t.Fatalf("reassembly %d told the chief of staff nothing", k+1)
		}
		if again, _, _, err := seal.Latest(repository, stream); err != nil || again.Seal != sealed.Seal {
			t.Fatalf("a plan-only revision changed the seal: %+v %v", again, err)
		}
	}

	must(t, a.Pass(ctx))
	must(t, a.Pass(ctx))
	if ids := assemblies(); !slices.Equal(ids, []string{AssembledState, "assembled-2", "assembled-3"}) {
		t.Fatalf("moves to assembled %q", ids)
	}
}

// recordPlanOnlyRevision files an amendment under the current seal and
// records it applied, as a revision that only adds plan units does.
func recordPlanOnlyRevision(t *testing.T, repository *trace.Repository, stream config.WorkstreamID, sealed seal.Seal, sealRevision, k int) {
	t.Helper()
	ctx := context.Background()
	id := "repair_" + string(rune('0'+k))
	h := trace.Header{Schema: "osmia.trace.amendment", Version: trace.Version, ID: id, Revision: 1, Project: repository.Project(), Workstream: stream, At: demoStart, Actor: trace.Actor{Kind: "agent", ID: "chief"}, Cause: "fixture"}
	must(t, repository.Append(ctx, trace.Amendment{Header: h, Requester: h.Actor, Role: trace.ChiefOfStaff, Thread: "chief", Turn: "filing_" + id, Citations: []string{"spec#1"}, Change: "Add a repair unit.", Reason: "The final review found a gap.", Seal: sealed.Seal, SealRevision: sealRevision, SpecHash: sealed.SpecHash}))
	for i, to := range []string{"filed", intake.AmendmentApplied} {
		from := ""
		if i > 0 {
			from = "filed"
		}
		h.Schema, h.ID = "osmia.trace.transition", "amendment_"+id+"_"+to
		_, err := repository.Transact(ctx, trace.Transaction{ExpectedVersion: uint64(i), Transition: trace.Transition{Header: h, Subject: intake.AmendmentSubject(id), From: from, To: to, Reason: to}})
		must(t, err)
	}
}
