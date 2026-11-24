package lifecycle_test

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	"github.com/kpenfound/osmia/internal/servicetest/journey"

	"github.com/kpenfound/osmia/internal/api"
	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/intake"
	"github.com/kpenfound/osmia/internal/lifecycle"
	"github.com/kpenfound/osmia/internal/servicetest/intakefixture"
	"github.com/kpenfound/osmia/internal/trace"
)

func holdDrift(t *testing.T, f *journey.ShedFixture, d journey.Drifter, stream config.WorkstreamID, op coreadapter.Operation, owners string) {
	t.Helper()
	journey.AwaitResolution(t, f.S, d, stream, op, "its mason's resolution")
	fixturekit.Must(t, os.WriteFile(filepath.Join(journey.ResolutionWorkspace(t, f, stream).Path, "CODEOWNERS"), []byte(owners), 0600))
	journey.CompleteDriftTurn(t, f, d.Repository, stream, intake.DriftMasonAgent, journey.ResolvedDone("Resolved"))
	journey.AwaitResolution(t, f.S, d, stream, op, "review 1")
	journey.CompleteDriftTurn(t, f, d.Repository, stream, intake.DriftReviewerAgent, journey.DriftVerdict(t, rejectedResolution))
	if result, err := journey.AttemptOperation(t, f.S, d.Repository, stream, op, d); err != nil || !strings.Contains(result.Evidence, "is held for the owner") {
		t.Fatalf("the drift rebase was not held: %+v %v", result, err)
	}
}

func TestChiefOfStaffHandsAHeldDriftRebaseBack(t *testing.T) {
	t.Parallel()
	f, stream, repository, _ := journey.NewFinalFixtureWith(t, "drift-handback", "")
	f.S.Cfg.Shed.MaxBounces = 1
	fm := cadenceForeman(f, repository, "1h")
	d := journey.Drifter{Foreman: fm}
	ctx := context.Background()
	_, _, op := journey.ConflictedDrift(t, f, d, stream)
	holdDrift(t, f, d, stream, op, "/internal/ @feature\n")

	controls := &journey.RuntimeControls{}
	controls.Service.Store(f.S)
	tool := controls.Lifecycle().HandBackDrift(repository, intakefixture.ClaimHeldDriftTurn(t, repository, f.Project, stream, f.Clock.Now, "events_1"), f.Clock.Now)
	handBack := func(input string) string {
		t.Helper()
		out, err := tool.Handle(ctx, json.RawMessage(input))
		fixturekit.Must(t, err)
		return string(out)
	}
	if out := handBack(`{"note":"Keep both owners.","owner_decided":true}`); !strings.Contains(out, "only for a handback the owner asked for") {
		t.Fatalf("an event turn recorded an owner handback: %s", out)
	}
	note := "Upstream already lists @upstream; keep it beside @feature."
	if out := handBack(fmt.Sprintf(`{"note":%q}`, note)); !strings.Contains(out, `"recorded":true`) || !strings.Contains(out, `"drift":2`) {
		t.Fatalf("hand_back_drift: %s", out)
	}
	if out := handBack(`{"note":"Again."}`); !strings.Contains(out, "drift rebase 2 is already asked for") {
		t.Fatalf("a second handback of one held drift rebase: %s", out)
	}
	statuses, err := repository.Statuses()
	fixturekit.Must(t, err)
	i := slices.IndexFunc(statuses, func(w trace.WorkstreamStatus) bool { return w.Workstream == stream })
	if decisions, err := journey.InspectForTest(f.S).OpenDecisions(ctx, repository, statuses[i]); err != nil || slices.ContainsFunc(decisions, func(e api.InboxEntry) bool { return e.Kind == api.InboxDrift }) {
		t.Fatalf("the inbox lists the drift rebase handed back: %+v %v", decisions, err)
	}
	later := f.S.Now().Add(48 * time.Hour)
	if why, err := fm.DriftDriver().DriftDue(stream, later); err != nil || !strings.HasPrefix(why, "the chief of staff asked for a drift rebase at ") {
		t.Fatalf("the handback does not make the workstream due: %q %v", why, err)
	}

	op = journey.RequestDrift(t, d, stream)
	journey.AwaitResolution(t, f.S, d, stream, op, "its mason's resolution")
	th, err := repository.Thread(stream, intake.DriftMasonAgent)
	fixturekit.Must(t, err)
	if resolve := th.Turns[len(th.Turns)-1]; resolve.Request.TurnID != lifecycle.DriftResolveTurnID(2, 1) || !strings.Contains(resolve.Request.Prompt, "Drift rebase 1 was held for the owner, and the chief of staff handed it back with this note, which this drift rebase answers: "+note) {
		t.Fatalf("the resolve turn %s:\n%s", resolve.Request.TurnID, resolve.Request.Prompt)
	}
	holdDrift(t, f, d, stream, op, "/internal/ @feature\n")
	if out := handBack(`{"note":"Keep @upstream this time."}`); !strings.Contains(out, `"drift":3`) {
		t.Fatalf("the second handback: %s", out)
	}
	op = journey.RequestDrift(t, d, stream)
	holdDrift(t, f, d, stream, op, "/internal/ @feature\n")
	if out := handBack(`{"note":"Try the verified conflict resolution."}`); !strings.Contains(out, `"drift":4`) {
		t.Fatalf("third engineering handback: %s", out)
	}
	op = journey.RequestDrift(t, d, stream)
	holdDrift(t, f, d, stream, op, "/internal/ @feature\n")
	if k, err := fm.DriftDriver().AskDrift(ctx, stream, f.S.Now()); err != nil || k != 5 {
		t.Fatalf("the owner's request answers drift rebase %d: %v", k, err)
	}
	if n, err := lifecycle.ChiefHandbacks(repository, stream); err != nil || n != 0 {
		t.Fatalf("chief handbacks after the owner's request: %d %v", n, err)
	}
}
