package service

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/trace"
)

// logAssumption claims a chief-of-staff turn on stream and logs text as an
// assumption through it, the way the chief's log_assumption tool does.
func logAssumption(t *testing.T, repository *trace.Repository, stream config.WorkstreamID, text string, at time.Time) trace.Assumption {
	t.Helper()
	ctx := context.Background()
	_, err := repository.EnsureChiefOfStaff(ctx, stream, at, serviceActor)
	must(t, err)
	turn := fmt.Sprintf("assumption-%d", at.UnixNano())
	h := trace.Header{Schema: "osmia.trace.turn-request", Version: trace.Version, ID: "request-" + turn, Revision: 1, Project: repository.Project(), Workstream: stream, At: at, Actor: serviceActor, Cause: "fixture"}
	_, err = repository.EnqueueTurn(ctx, trace.TurnRequest{Header: h, AgentID: trace.ChiefOfStaff, ThreadID: trace.ChiefOfStaff, TurnID: turn, Profile: coreadapter.Profile{Name: "fake", Backend: "fake", Model: "fake"}, Prompt: "Review the status"})
	must(t, err)
	_, err = repository.ClaimTurn(ctx, stream, trace.ChiefOfStaff, turn+"-token", t.TempDir(), at)
	must(t, err)
	a, err := repository.LogAssumption(ctx, trace.ChiefOfStaff, coreadapter.Scope{Project: string(repository.Project()), Workstream: string(stream), Role: trace.ChiefOfStaff, Thread: trace.ChiefOfStaff, Turn: turn}, text, at)
	must(t, err)
	return a
}

// TestDraftDeliveryIncludesLoggedAssumptions shows that a judgment call the
// chief of staff logged within the workstream's stated intent appears in its
// drafted pull request body (spec#17), and that calling the same tool again
// from the same turn neither duplicates the logged entry nor the body text.
func TestDraftDeliveryIncludesLoggedAssumptions(t *testing.T) {
	t.Parallel()
	f, stream, repository, report := deliveryFixture(t)
	defer repository.Close()
	at := time.Now().UTC()
	logAssumption(t, repository, stream, "Used the existing retry backoff rather than adding a new one.", at)

	_, body, err := draftDelivery(f.s.about(repository), repository, stream, report)
	must(t, err)
	if !strings.Contains(body, "Assumptions:") || !strings.Contains(body, "- Used the existing retry backoff rather than adding a new one.") {
		t.Fatalf("drafted body does not carry the logged assumption:\n%s", body)
	}
}

// TestDraftDeliveryHasNoAssumptionsSectionWithoutAny shows the body carries
// no "Assumptions:" heading for a workstream that logged none, so an empty
// log never adds visible clutter to the drafted description.
func TestDraftDeliveryHasNoAssumptionsSectionWithoutAny(t *testing.T) {
	t.Parallel()
	f, stream, repository, report := deliveryFixture(t)
	defer repository.Close()
	_, body, err := draftDelivery(f.s.about(repository), repository, stream, report)
	must(t, err)
	if strings.Contains(body, "Assumptions:") {
		t.Fatalf("drafted body has an assumptions section with none logged:\n%s", body)
	}
}
