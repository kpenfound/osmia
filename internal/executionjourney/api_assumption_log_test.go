package executionjourney

import (
	context "context"
	fmt "fmt"
	strings "strings"
	testing "testing"
	time "time"

	"github.com/kpenfound/osmia/internal/intake"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	"github.com/kpenfound/osmia/internal/servicetest/journey"

	config "github.com/kpenfound/osmia/internal/config"
	coreadapter "github.com/kpenfound/osmia/internal/coreadapter"
	trace "github.com/kpenfound/osmia/internal/trace"
)

func logAssumption(t *testing.T, repository *trace.Repository, stream config.WorkstreamID, text string, at time.Time) trace.Assumption {
	t.Helper()
	ctx := context.Background()
	_, err := repository.EnsureManager(ctx, stream, at, intake.ServiceActor)
	fixturekit.Must(t, err)
	turn := fmt.Sprintf("assumption-%d", at.UnixNano())
	h := trace.Header{Schema: "osmia.trace.turn-request", Version: trace.Version, ID: "request-" + turn, Revision: 1, Project: repository.Project(), Workstream: stream, At: at, Actor: intake.ServiceActor, Cause: "fixture"}
	_, err = repository.EnqueueTurn(ctx, trace.TurnRequest{Header: h, AgentID: trace.Manager, ThreadID: trace.Manager, TurnID: turn, Profile: coreadapter.Profile{Name: "fake", Backend: "fake", Model: "fake"}, Prompt: "Review the status"})
	fixturekit.Must(t, err)
	_, err = repository.ClaimTurn(ctx, stream, trace.Manager, turn+"-token", t.TempDir(), at)
	fixturekit.Must(t, err)
	a, err := repository.LogAssumption(ctx, trace.Manager, coreadapter.Scope{Project: string(repository.Project()), Workstream: string(stream), Role: trace.Manager, Thread: trace.Manager, Turn: turn}, text, at)
	fixturekit.Must(t, err)
	return a
}

func TestDraftDeliveryIncludesLoggedAssumptions(t *testing.T) {
	t.Parallel()
	f, stream, repository, report := journey.DeliveryFixture(t)
	defer repository.Close()
	at := time.Now().UTC()
	logAssumption(t, repository, stream, "Used the existing retry backoff rather than adding a new one.", at)

	_, body, err := journey.DraftDelivery(f.S.About(repository), repository, stream, report)
	fixturekit.Must(t, err)
	if !strings.Contains(body, "Assumptions:") || !strings.Contains(body, "- Used the existing retry backoff rather than adding a new one.") {
		t.Fatalf("drafted body does not carry the logged assumption:\n%s", body)
	}
}

func TestDraftDeliveryHasNoAssumptionsSectionWithoutAny(t *testing.T) {
	t.Parallel()
	f, stream, repository, report := journey.DeliveryFixture(t)
	defer repository.Close()
	_, body, err := journey.DraftDelivery(f.S.About(repository), repository, stream, report)
	fixturekit.Must(t, err)
	if strings.Contains(body, "Assumptions:") {
		t.Fatalf("drafted body has an assumptions section with none logged:\n%s", body)
	}
}
