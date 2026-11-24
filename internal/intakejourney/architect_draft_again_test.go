package intakejourney

import (
	"context"
	"fmt"
	"github.com/kpenfound/osmia/internal/api"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	"github.com/kpenfound/osmia/internal/servicetest/intakefixture"
	"github.com/kpenfound/osmia/internal/servicetest/journey"
	"strings"
	"testing"

	"github.com/kpenfound/osmia/internal/intake"
	"github.com/kpenfound/osmia/internal/lifecycle"
	"github.com/kpenfound/osmia/internal/plan"
	"github.com/kpenfound/osmia/internal/service"
	"github.com/kpenfound/osmia/internal/trace"
)

func TestAMoveToSketchedDraftsAStoppedWorkstreamAgain(t *testing.T) {
	t.Parallel()
	f := journey.NewArchitectFixture(t)
	defer f.Stop(t)
	ctx := context.Background()
	for n := 1; n <= 3*intake.MaxDrafts; n++ {
		f.Script(fmt.Sprintf("draft-%d-1", n), map[string]string{plan.SpecPath: fixturekit.ValidSpec, plan.PlanPath: fixturekit.CyclicPlan}, nil)
	}
	f.Script(fmt.Sprintf("draft-%d-1", 2*intake.MaxDrafts+1), map[string]string{plan.SpecPath: fixturekit.ValidSpec, plan.PlanPath: fixturekit.ValidPlan}, nil)
	stream := f.HandIn(t, "design", fixturekit.HandedDesign)
	f.Await(t, stream, intakefixture.DraftAt("exhausted"))

	_, err := f.C.MoveWorkstream(ctx, stream, intake.InShedState, "Debate it.")
	if !intakefixture.Failed(err, api.Conflict) || !strings.Contains(err.Error(), "drafting stopped after 3 refused drafts") || !strings.Contains(err.Error(), "move it to sketched with a note") {
		t.Fatalf("in-shed move on a stopped workstream: %v", err)
	}

	moves := service.InspectForTest(f.S.Core()).LifecycleController()
	chief := trace.Actor{Kind: "agent", ID: trace.ChiefOfStaff}
	note := "Drop the dependency between the units; they share no state."
	out, apiErr := moves.MoveWorkstream(ctx, f.Repository(), stream, lifecycle.WorkstreamMoveRequest{To: intake.SketchedState, Note: note}, chief, "chief-turn-1")
	if apiErr != nil {
		t.Fatal(apiErr)
	}
	if out.State != intake.HandedState || !strings.Contains(out.Detail, "new run of up to 3 drafts") {
		t.Fatalf("chief's move: %+v", out)
	}
	last := fmt.Sprintf("draft-%d-exhausted", 2*intake.MaxDrafts)
	f.Await(t, stream, f.TransitionRecorded(t, stream, last))
	if runs := f.Runs(); len(runs) != 2*intake.MaxDrafts {
		t.Fatalf("backend runs %v", runs)
	}
	byID := map[string]trace.Transition{}
	for _, tr := range f.Transitions(t, stream) {
		byID[tr.ID] = tr
	}
	first := byID[fmt.Sprintf("draft-%d", intake.MaxDrafts+1)]
	if first.Actor != chief || first.Cause != intake.DraftAgainCause || first.From != "exhausted" || !strings.Contains(first.Reason, "the chief of staff asked for another run of 3 drafts: "+note) {
		t.Fatalf("first draft of the new run: %+v", first)
	}
	if stopped := byID[last]; stopped.From != fmt.Sprintf("invalid-%d", 2*intake.MaxDrafts) || stopped.To != "exhausted" || !strings.Contains(stopped.Reason, "none of the architect's 3 drafts") {
		t.Fatalf("second stop: %+v", stopped)
	}
	th := f.ArchitectThread(t, stream)
	if len(th.Turns) != 2*intake.MaxDrafts {
		t.Fatalf("thread: %d turns", len(th.Turns))
	}
	for i := intake.MaxDrafts; i < 2*intake.MaxDrafts; i++ {
		if !strings.Contains(th.Turns[i].Request.Prompt, note) {
			t.Fatalf("draft %d prompt lacks the note: %s", i+1, th.Turns[i].Request.Prompt)
		}
	}
	if p := th.Turns[intake.MaxDrafts].Request.Prompt; !strings.Contains(p, fmt.Sprintf("Draft %d was not accepted", intake.MaxDrafts)) || !strings.Contains(p, "dependency cycle") {
		t.Fatalf("first draft of the run lacks the last refusal: %s", p)
	}
	if strings.Contains(th.Turns[0].Request.Prompt, note) {
		t.Fatal("a draft before the move carries its note")
	}
	outbox, err := f.Repository().Outbox(stream)
	fixturekit.Must(t, err)
	notices := map[string]int{}
	for _, e := range outbox {
		if e.Event.Operation == nil && intakefixture.ChiefEvent(e.Event) {
			notices[e.TransitionID]++
		}
	}
	if notices["draft-exhausted"] != 1 || notices[last] != 1 {
		t.Fatalf("notices: %v", notices)
	}

	note = "Keep one unit."
	if _, err := f.C.MoveWorkstream(ctx, stream, intake.SketchedState, note); err != nil {
		t.Fatal(err)
	}
	f.Await(t, stream, intakefixture.Sketched)
	if tr := f.Transitions(t, stream); !containsTransition(tr, func(tr trace.Transition) bool {
		return tr.ID == fmt.Sprintf("draft-%d", 2*intake.MaxDrafts+1) && tr.Actor == intake.OwnerActor && tr.Cause == intake.DraftAgainCause
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

func TestTheOwnersRedraftDraftsAStoppedWorkstreamAgain(t *testing.T) {
	t.Parallel()
	f := journey.NewArchitectFixture(t)
	defer f.Stop(t)
	ctx := context.Background()
	for n := 1; n <= intake.MaxDrafts; n++ {
		f.Script(fmt.Sprintf("draft-%d-1", n), map[string]string{plan.SpecPath: fixturekit.ValidSpec, plan.PlanPath: fixturekit.CyclicPlan}, nil)
	}
	f.Script(fmt.Sprintf("draft-%d-1", intake.MaxDrafts+1), map[string]string{plan.SpecPath: fixturekit.ValidSpec, plan.PlanPath: fixturekit.ValidPlan}, nil)
	stream := f.HandIn(t, "design", fixturekit.HandedDesign)
	f.Await(t, stream, intakefixture.DraftAt("exhausted"))

	if _, err := f.C.ShedObject(ctx, stream, "No."); !intakefixture.Failed(err, api.Conflict) || !strings.Contains(err.Error(), "has no accepted spec and plan in the shed yet") || !strings.Contains(err.Error(), "drafting stopped after 3 refused drafts") {
		t.Fatalf("shed objection on a stopped workstream: %v", err)
	}
	if _, err := f.C.ShedRedraft(ctx, stream, " "); !intakefixture.Failed(err, api.Validation) {
		t.Fatalf("redraft without a note: %v", err)
	}
	note := "Drop the dependency between the units."
	if _, err := f.C.ShedRedraft(ctx, stream, note); err != nil {
		t.Fatal(err)
	}
	f.Await(t, stream, intakefixture.Sketched)
	if tr := f.Transitions(t, stream); !containsTransition(tr, func(tr trace.Transition) bool {
		return tr.ID == fmt.Sprintf("draft-%d", intake.MaxDrafts+1) && tr.Actor == intake.OwnerActor && tr.Cause == intake.DraftAgainCause && strings.Contains(tr.Reason, note)
	}) {
		t.Fatalf("owner's run not recorded: %+v", tr)
	}
	if _, err := f.C.ShedRedraft(ctx, stream, note); err == nil || strings.Contains(err.Error(), "has no accepted spec and plan") {
		t.Fatalf("a sketched workstream's redraft took the handed path: %v", err)
	}
}
