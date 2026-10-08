package service

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

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/trace"
)

// holdDrift runs drift operation op through one resolution its reviewer
// rejects, which holds it while shed.max_bounces is 1.
func holdDrift(t *testing.T, f *shedFixture, d drifter, stream config.WorkstreamID, op coreadapter.Operation, owners string) {
	t.Helper()
	awaitResolution(t, f.s, d, stream, op, "its mason's resolution")
	must(t, os.WriteFile(filepath.Join(resolutionWorkspace(t, f, stream).Path, "CODEOWNERS"), []byte(owners), 0600))
	completeDriftTurn(t, f, d.repository, stream, driftMasonAgent, resolvedDone("Resolved"))
	awaitResolution(t, f.s, d, stream, op, "review 1")
	completeDriftTurn(t, f, d.repository, stream, driftReviewerAgent, driftVerdict(t, rejectedResolution))
	if result, err := attemptOperation(t, f.s, d.repository, stream, op, d); err != nil || !strings.Contains(result.Evidence, "is held for the owner") {
		t.Fatalf("the drift rebase was not held: %+v %v", result, err)
	}
}

// chiefTurn claims a new chief-of-staff turn of the workstream and returns
// its scope.
func chiefTurn(t *testing.T, f *shedFixture, repository *trace.Repository, stream config.WorkstreamID, id string) coreadapter.Scope {
	t.Helper()
	ctx := context.Background()
	_, err := repository.EnsureChiefOfStaff(ctx, stream, f.clock.Now(), serviceActor)
	must(t, err)
	_, err = repository.EnqueueTurn(ctx, trace.TurnRequest{Header: trace.Header{Schema: "osmia.trace.turn-request", Version: trace.Version, ID: "request_" + id, Revision: 1, Project: f.project, Workstream: stream, At: f.clock.Now(), Actor: serviceActor, Cause: "fixture"},
		AgentID: trace.ChiefOfStaff, ThreadID: trace.ChiefOfStaff, TurnID: id, Profile: coreadapter.Profile{Name: "default", Backend: "claude", Model: "test"}, SystemPrompt: "chief", Prompt: "The drift rebase is held."})
	must(t, err)
	_, err = repository.ClaimTurn(ctx, stream, trace.ChiefOfStaff, "token-"+id, filepath.Join(t.TempDir(), id), f.clock.Now())
	must(t, err)
	return coreadapter.Scope{Project: string(f.project), Workstream: string(stream), Role: trace.ChiefOfStaff, Thread: trace.ChiefOfStaff, Turn: id}
}

// The chief of staff hands a held drift rebase back with a note: the next
// drift rebase is asked for, its mason and reviewer receive the note, and
// the inbox no longer lists the held one. Engineering recovery does not
// require an owner ruling after an arbitrary number of interventions.
func TestChiefOfStaffHandsAHeldDriftRebaseBack(t *testing.T) {
	t.Parallel()
	f, stream, repository, _ := newFinalFixture(t, "drift-handback")
	f.s.cfg.Shed.MaxBounces = 1
	fm := cadenceForeman(f, repository, "1h")
	d := drifter{fm}
	ctx := context.Background()
	_, _, op := conflictedDrift(t, f, d, stream)
	holdDrift(t, f, d, stream, op, "/internal/ @feature\n")

	controls := &runtimeControls{}
	controls.service.Store(f.s)
	tool := controls.handBackDrift(repository, chiefTurn(t, f, repository, stream, "events_1"), f.clock.Now)
	handBack := func(input string) string {
		t.Helper()
		out, err := tool.Handle(ctx, json.RawMessage(input))
		must(t, err)
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
	must(t, err)
	i := slices.IndexFunc(statuses, func(w trace.WorkstreamStatus) bool { return w.Workstream == stream })
	if decisions, err := f.s.openDecisions(ctx, repository, statuses[i]); err != nil || slices.ContainsFunc(decisions, func(e InboxEntry) bool { return e.Kind == InboxDrift }) {
		t.Fatalf("the inbox lists the drift rebase handed back: %+v %v", decisions, err)
	}
	later := f.s.now().Add(48 * time.Hour)
	if why, err := fm.driftDue(stream, later); err != nil || !strings.HasPrefix(why, "the chief of staff asked for a drift rebase at ") {
		t.Fatalf("the handback does not make the workstream due: %q %v", why, err)
	}

	// The next drift rebase's mason receives the note.
	op = requestDrift(t, d, stream)
	awaitResolution(t, f.s, d, stream, op, "its mason's resolution")
	th, err := repository.Thread(stream, driftMasonAgent)
	must(t, err)
	if resolve := th.Turns[len(th.Turns)-1]; resolve.Request.TurnID != driftResolveTurnID(2, 1) || !strings.Contains(resolve.Request.Prompt, "Drift rebase 1 was held for the owner, and the chief of staff handed it back with this note, which this drift rebase answers: "+note) {
		t.Fatalf("the resolve turn %s:\n%s", resolve.Request.TurnID, resolve.Request.Prompt)
	}
	holdDrift(t, f, d, stream, op, "/internal/ @feature\n")
	if out := handBack(`{"note":"Keep @upstream this time."}`); !strings.Contains(out, `"drift":3`) {
		t.Fatalf("the second handback: %s", out)
	}
	op = requestDrift(t, d, stream)
	holdDrift(t, f, d, stream, op, "/internal/ @feature\n")
	if out := handBack(`{"note":"Try the verified conflict resolution."}`); !strings.Contains(out, `"drift":4`) {
		t.Fatalf("third engineering handback: %s", out)
	}
	op = requestDrift(t, d, stream)
	holdDrift(t, f, d, stream, op, "/internal/ @feature\n")
	// Explicit owner requests retain their attribution.
	if k, err := fm.askDrift(ctx, stream, f.s.now()); err != nil || k != 5 {
		t.Fatalf("the owner's request answers drift rebase %d: %v", k, err)
	}
	if n, err := chiefHandbacks(repository, stream); err != nil || n != 0 {
		t.Fatalf("chief handbacks after the owner's request: %d %v", n, err)
	}
}
