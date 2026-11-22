package intakejourney

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/kpenfound/osmia/internal/intake"
	"github.com/kpenfound/osmia/internal/lifecycle"
	"github.com/kpenfound/osmia/internal/plan"
	"github.com/kpenfound/osmia/internal/service"
	"github.com/kpenfound/osmia/internal/trace"
)

// A handed workstream whose drafting stopped after MaxDrafts refused drafts
// starts a new run of MaxDrafts drafts when the chief of staff or the owner
// moves it to sketched with a note. Every draft of the run carries the note,
// its first draft says why the last one was refused, the run stops by itself
// again, and a move to in-shed is refused with where drafting stands.
func TestAMoveToSketchedDraftsAStoppedWorkstreamAgain(t *testing.T) {
	t.Parallel()
	f := newArchitectFixture(t)
	defer f.stop(t)
	ctx := context.Background()
	for n := 1; n <= 3*maxDrafts; n++ {
		f.script(fmt.Sprintf("draft-%d-1", n), map[string]string{plan.SpecPath: validSpec, plan.PlanPath: cyclicPlan}, nil)
	}
	f.script(fmt.Sprintf("draft-%d-1", 2*maxDrafts+1), map[string]string{plan.SpecPath: validSpec, plan.PlanPath: validPlan}, nil)
	stream := f.handIn(t, "design", handedDesign)
	f.await(t, stream, draftAt("exhausted"))

	_, err := f.c.MoveWorkstream(ctx, stream, InShedState, "Debate it.")
	if !failed(err, Conflict) || !strings.Contains(err.Error(), "drafting stopped after 3 refused drafts") || !strings.Contains(err.Error(), "move it to sketched with a note") {
		t.Fatalf("in-shed move on a stopped workstream: %v", err)
	}

	moves := service.InspectForTest(f.s.core()).LifecycleController()
	chief := trace.Actor{Kind: "agent", ID: trace.ChiefOfStaff}
	note := "Drop the dependency between the units; they share no state."
	out, api := moves.MoveWorkstream(ctx, f.repository(), stream, lifecycle.WorkstreamMoveRequest{To: SketchedState, Note: note}, chief, "chief-turn-1")
	if api != nil {
		t.Fatal(api)
	}
	if out.State != HandedState || !strings.Contains(out.Detail, "new run of up to 3 drafts") {
		t.Fatalf("chief's move: %+v", out)
	}
	last := fmt.Sprintf("draft-%d-exhausted", 2*maxDrafts)
	f.await(t, stream, f.transitionRecorded(t, stream, last))
	if runs := f.runs(); len(runs) != 2*maxDrafts {
		t.Fatalf("backend runs %v", runs)
	}
	byID := map[string]trace.Transition{}
	for _, tr := range f.transitions(t, stream) {
		byID[tr.ID] = tr
	}
	first := byID[fmt.Sprintf("draft-%d", maxDrafts+1)]
	if first.Actor != chief || first.Cause != intake.DraftAgainCause || first.From != "exhausted" || !strings.Contains(first.Reason, "the chief of staff asked for another run of 3 drafts: "+note) {
		t.Fatalf("first draft of the new run: %+v", first)
	}
	if stopped := byID[last]; stopped.From != fmt.Sprintf("invalid-%d", 2*maxDrafts) || stopped.To != "exhausted" || !strings.Contains(stopped.Reason, "none of the architect's 3 drafts") {
		t.Fatalf("second stop: %+v", stopped)
	}
	th := f.architectThread(t, stream)
	if len(th.Turns) != 2*maxDrafts {
		t.Fatalf("thread: %d turns", len(th.Turns))
	}
	for i := maxDrafts; i < 2*maxDrafts; i++ {
		if !strings.Contains(th.Turns[i].Request.Prompt, note) {
			t.Fatalf("draft %d prompt lacks the note: %s", i+1, th.Turns[i].Request.Prompt)
		}
	}
	if p := th.Turns[maxDrafts].Request.Prompt; !strings.Contains(p, fmt.Sprintf("Draft %d was not accepted", maxDrafts)) || !strings.Contains(p, "dependency cycle") {
		t.Fatalf("first draft of the run lacks the last refusal: %s", p)
	}
	if strings.Contains(th.Turns[0].Request.Prompt, note) {
		t.Fatal("a draft before the move carries its note")
	}
	outbox, err := f.repository().Outbox(stream)
	must(t, err)
	notices := map[string]int{}
	for _, e := range outbox {
		if e.Event.Operation == nil && chiefEvent(e.Event) {
			notices[e.TransitionID]++
		}
	}
	if notices["draft-exhausted"] != 1 || notices[last] != 1 {
		t.Fatalf("notices: %v", notices)
	}

	note = "Keep one unit."
	if _, err := f.c.MoveWorkstream(ctx, stream, SketchedState, note); err != nil {
		t.Fatal(err)
	}
	f.await(t, stream, sketched)
	if tr := f.transitions(t, stream); !containsTransition(tr, func(tr trace.Transition) bool {
		return tr.ID == fmt.Sprintf("draft-%d", 2*maxDrafts+1) && tr.Actor == ownerActor && tr.Cause == intake.DraftAgainCause
	}) {
		t.Fatalf("owner's run not recorded: %+v", tr)
	}
}

func containsTransition(transitions []trace.Transition, match func(trace.Transition) bool) bool {
	for _, tr := range transitions {
		if match(tr) {
			return true
		}
	}
	return false
}
