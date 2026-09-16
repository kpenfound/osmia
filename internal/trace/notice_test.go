package trace

import (
	"context"
	"errors"
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
