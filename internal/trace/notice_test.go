package trace

import (
	"context"
	"errors"
	"reflect"
	"slices"
	"strings"
	"testing"
)

func TestSetFeatureStateCommitsANotice(t *testing.T) {
	r, _, _ := create(t)
	ctx := context.Background()
	state, err := r.SetFeatureState(ctx, header("transition", "handed"), "handed", "Owner handed in a design")
	if err != nil || state != (WorkflowState{Version: 1, Value: "handed"}) {
		t.Fatalf("state %v: %v", state, err)
	}
	if again, err := r.SetFeatureState(ctx, header("transition", "handed"), "handed", "Owner handed in a design"); err != nil || again != state {
		t.Fatalf("retry %v: %v", again, err)
	}
	if _, err := r.SetFeatureState(ctx, header("transition", "handed"), "planning", "Another reason"); !errors.Is(err, ErrConflict) {
		t.Fatalf("changed retry: %v", err)
	}
	if _, err := r.SetFeatureState(ctx, header("transition", "planning"), "planning", "Plan drafting started"); err != nil {
		t.Fatal(err)
	}
	entries := ready(t, r, at)
	if len(entries) != 2 {
		t.Fatalf("entries %+v", entries)
	}
	bodies := map[string]string{}
	for _, e := range entries {
		if e.Event.Kind != NoticeKind || e.Event.Operation != nil || !e.At.Equal(at) {
			t.Fatalf("entry %+v", e)
		}
		bodies[e.TransitionID] = e.Event.Body
	}
	if bodies["handed"] != "Workstream state changed to handed: Owner handed in a design" || bodies["planning"] != "Workstream state changed from handed to planning: Plan drafting started" {
		t.Fatalf("bodies %v", bodies)
	}
	if got, err := r.Workflow(streamID, FeatureSubject); err != nil || got != (WorkflowState{Version: 2, Value: "planning"}) {
		t.Fatalf("feature state %v: %v", got, err)
	}
}

func TestSetFeatureStateFailureLeavesNoEvent(t *testing.T) {
	r, root, p := create(t)
	injected := errors.New("injected publication failure")
	r.failPublication = func(step string) error {
		if step == "before-ref" {
			return injected
		}
		return nil
	}
	if _, err := r.SetFeatureState(context.Background(), header("transition", "handed"), "handed", "Owner handed in a design"); !errors.Is(err, injected) {
		t.Fatalf("injection not reached: %v", err)
	}
	r.failPublication = nil
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	r, err := Open(root, p)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if got, err := r.Workflow(streamID, FeatureSubject); err != nil || got != (WorkflowState{}) {
		t.Fatalf("state after failure %v: %v", got, err)
	}
	if entries, err := r.Outbox(streamID); err != nil || len(entries) != 0 {
		t.Fatalf("outbox after failure %+v: %v", entries, err)
	}
}

func TestNoticeIdentity(t *testing.T) {
	e := Notice("handed", "state", "Body")
	if e.ID != EventID("handed", "state") || e.Kind != NoticeKind || e.Body != "Body" || e.Operation != nil || !strings.HasPrefix(e.ID, "event_") {
		t.Fatalf("notice %+v", e)
	}
}

func TestMoveFeatureStateRequiresTheExpectedState(t *testing.T) {
	r, _, _ := create(t)
	ctx := context.Background()
	if _, err := r.MoveFeatureState(ctx, header("transition", "sketched"), "handed", "sketched", "Draft accepted"); !errors.Is(err, ErrConflict) {
		t.Fatalf("move from an absent state: %v", err)
	}
	if _, err := r.SetFeatureState(ctx, header("transition", "handed"), "handed", "Owner handed in a design"); err != nil {
		t.Fatal(err)
	}
	if _, err := r.MoveFeatureState(ctx, header("transition", "sketched"), "planning", "sketched", "Draft accepted"); !errors.Is(err, ErrConflict) {
		t.Fatalf("move from another state: %v", err)
	}
	state, err := r.MoveFeatureState(ctx, header("transition", "sketched"), "handed", "sketched", "Draft accepted")
	if err != nil || state != (WorkflowState{Version: 2, Value: "sketched"}) {
		t.Fatalf("state %v: %v", state, err)
	}
	// The same transition again returns the committed state whatever the
	// expected state says, so a retry after a commit is not refused.
	if again, err := r.MoveFeatureState(ctx, header("transition", "sketched"), "handed", "sketched", "Draft accepted"); err != nil || again != state {
		t.Fatalf("retry %v: %v", again, err)
	}
	if _, err := r.MoveFeatureState(ctx, header("transition", "sketched"), "handed", "sketched", "Another reason"); !errors.Is(err, ErrConflict) {
		t.Fatalf("changed retry: %v", err)
	}
	if got, err := r.Workflow(streamID, FeatureSubject); err != nil || got != state {
		t.Fatalf("feature state %v: %v", got, err)
	}
	if entries := ready(t, r, at); len(entries) != 2 || !strings.Contains(entries[len(entries)-1].Event.Body+entries[0].Event.Body, "changed from handed to sketched: Draft accepted") {
		t.Fatalf("entries %+v", entries)
	}
}

func TestSetFeatureStateUnlessRefusesListedStates(t *testing.T) {
	ctx := context.Background()
	r, _, _ := create(t)
	h := header("transition", "deliver")
	if _, err := r.SetFeatureState(ctx, h, "delivered", "opened"); err != nil {
		t.Fatal(err)
	}
	h2 := header("transition", "abandon")
	state, err := r.SetFeatureStateUnless(ctx, h2, "abandoned", "gone", "abandoned", "delivered")
	if !errors.Is(err, ErrFeatureState) || state.Value != "delivered" {
		t.Fatalf("refusal: %+v %v", state, err)
	}
	if got, err := r.Workflow(streamID, FeatureSubject); err != nil || got.Value != "delivered" {
		t.Fatalf("state after refusal %+v %v", got, err)
	}
	if _, err := r.SetFeatureStateUnless(ctx, h2, "abandoned", "gone", "abandoned"); err != nil {
		t.Fatal(err)
	}
}

func TestMoveFeatureStateWithRecordsTheOthersInTheSameCommit(t *testing.T) {
	r, _, _ := create(t)
	ctx := context.Background()
	if _, err := r.SetFeatureState(ctx, header("transition", "ratified"), "ratified", "Sealed"); err != nil {
		t.Fatal(err)
	}
	unit := func(id, subject, from, to string, version uint64) Transaction {
		return Transaction{ExpectedVersion: version, Transition: Transition{Header: header("transition", id), Subject: subject, From: from, To: to, Reason: "unit " + to}}
	}
	a, b := UnitSubject("a"), UnitSubject("b")
	with := []Transaction{unit("unit-a-planned", a, "", "planned", 0), unit("unit-a-ready", a, "planned", "ready", 1), unit("unit-b-planned", b, "", "planned", 0)}

	// A feature transaction among the others is refused, and so is the
	// whole commit when one of them does not apply.
	if _, err := r.MoveFeatureStateWith(ctx, header("transition", "building"), "ratified", "building", "Built", unit("again", FeatureSubject, "ratified", "delivered", 1)); err == nil || !strings.Contains(err.Error(), "must be of another subject") {
		t.Fatalf("a feature transaction among the others: %v", err)
	}
	if _, err := r.MoveFeatureStateWith(ctx, header("transition", "building"), "ratified", "building", "Built", unit("unit-a-ready", a, "planned", "ready", 1)); !errors.Is(err, ErrConflict) {
		t.Fatalf("a transaction that does not apply: %v", err)
	}
	if states, err := r.WorkflowStates(streamID); err != nil || len(states) != 1 || states[FeatureSubject] != (WorkflowState{Version: 1, Value: "ratified"}) {
		t.Fatalf("states after the refusals %+v: %v", states, err)
	}

	state, err := r.MoveFeatureStateWith(ctx, header("transition", "building"), "ratified", "building", "Built", with...)
	if err != nil || state != (WorkflowState{Version: 2, Value: "building"}) {
		t.Fatalf("state %v: %v", state, err)
	}
	want := map[string]WorkflowState{FeatureSubject: state, a: {Version: 2, Value: "ready"}, b: {Version: 1, Value: "planned"}}
	if states, err := r.WorkflowStates(streamID); err != nil || !reflect.DeepEqual(states, want) {
		t.Fatalf("states %+v: %v", states, err)
	}
	// A retry of the feature transition records nothing more.
	if again, err := r.MoveFeatureStateWith(ctx, header("transition", "building"), "ratified", "building", "Built", with...); err != nil || again != state {
		t.Fatalf("retry %v: %v", again, err)
	}
	transitions, err := Read[Transition](r, streamID)
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, tr := range transitions {
		ids = append(ids, tr.ID)
	}
	if !slices.Equal(ids, []string{"ratified", "building", "unit-a-planned", "unit-a-ready", "unit-b-planned"}) {
		t.Fatalf("transitions %v", ids)
	}
	if entries := ready(t, r, at); len(entries) != 2 || !strings.Contains(entries[0].Event.Body+entries[1].Event.Body, "changed from ratified to building: Built") {
		t.Fatalf("entries %+v", entries)
	}
}

func TestUnitSubjectIsAKey(t *testing.T) {
	if got := UnitSubject("resume-read"); got != "unit-resume-read" {
		t.Fatalf("subject %q", got)
	}
	long := strings.Repeat("u", 128)
	got := UnitSubject(long)
	if !strings.HasPrefix(got, "unit_") || len(got) != 37 || !key(got+"-planned") || got == UnitSubject(long[:127]) {
		t.Fatalf("subject of a long ID %q", got)
	}
	if edge := UnitSubject(strings.Repeat("u", 64)); edge != "unit-"+strings.Repeat("u", 64) || !key(edge+"-planned") {
		t.Fatalf("subject of a 64-character ID %q", edge)
	}
}
