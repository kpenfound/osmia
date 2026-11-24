package intakejourney

import (
	context "context"
	strings "strings"
	testing "testing"

	"github.com/kpenfound/osmia/internal/api"
	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/intake"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	"github.com/kpenfound/osmia/internal/servicetest/intakefixture"
	"github.com/kpenfound/osmia/internal/servicetest/journey"

	shed "github.com/kpenfound/osmia/internal/shed"
	trace "github.com/kpenfound/osmia/internal/trace"
)

func handedInSkippingDebate(t *testing.T) (*journey.ShedFixture, config.WorkstreamID) {
	t.Helper()
	f := journey.NewDebateFixture(t, 2, 1)
	t.Cleanup(func() { f.Stop(t) })
	f.Upstream(t)
	return f, f.HandInSkipping(t, "design")
}

func TestHandInSkippingDebateRecordsTheSkipAndPresentsAPacketToRatify(t *testing.T) {
	t.Parallel()
	f, stream := handedInSkippingDebate(t)

	skip := f.Transition(t, stream, intake.SkipTransition)
	if skip.Subject != intake.OwnerSubject || skip.From != "" || skip.To != intake.SkippedValue || skip.Actor != intake.OwnerActor || skip.Cause != intake.HandInTransition ||
		skip.Reason != "the owner skipped debate at hand-in; the workstream still needs the owner's ratification of the spec and the plan" {
		t.Fatalf("skip transition %+v", skip)
	}
	if reason := f.Transition(t, stream, intake.HandInTransition).Reason; reason != "the owner handed in handed/stdin from stdin and skipped debate" {
		t.Fatalf("hand-in reason %q", reason)
	}
	packet := f.AwaitPacket(t, stream, "ratify: no objection stands")
	if !packet.Skipped || packet.Round != 1 || packet.Revision != (shed.Pin{Spec: 1, Plan: 1}) || len(packet.Dissent) != 0 || packet.Conclusion != skip.Reason {
		t.Fatalf("the packet of a debate skipped at hand-in %+v", packet)
	}
	f.DebatedNothing(t, stream)
}

func TestHandInSkippingDebateEntersTheShedWithoutACommittee(t *testing.T) {
	t.Parallel()
	f, stream := handedInSkippingDebate(t)
	f.AwaitPacket(t, stream, "ratify: no objection stands")
	f.AwaitFeature(t, stream, intake.InShedState)
	enter := f.Transition(t, stream, intake.InShedState)
	want := "spec.md revision 1 and plan.json revision 1 enter the shed without a committee: the owner skipped debate"
	if enter.From != intake.SketchedState || enter.Actor != intake.ShedActor || enter.Cause != intake.SkipTransition || enter.Reason != want {
		t.Fatalf("entering the shed %+v", enter)
	}
	if notice := f.Notice(t, stream, intake.InShedState); notice != "Workstream state changed from sketched to in-shed: "+want+".\n"+intake.Presentation("ratify: no objection stands") {
		t.Fatalf("the notice of entering the shed %q", notice)
	}
}

func TestSkippingDebateAgainAfterAHandInSkipIsRefused(t *testing.T) {
	t.Parallel()
	f, stream := handedInSkippingDebate(t)
	f.AwaitFeature(t, stream, intake.InShedState)
	if _, err := f.C.ShedSkip(context.Background(), stream); !intakefixture.Failed(err, api.Conflict) || !strings.Contains(err.Error(), "debate on workstream "+string(stream)+" is already skipped") {
		t.Fatalf("skipping again: %v", err)
	}
}

func TestRatifyingAHandInThatSkippedDebateSealsTheDraft(t *testing.T) {
	t.Parallel()
	f, stream := handedInSkippingDebate(t)
	f.AwaitPacket(t, stream, "ratify: no objection stands")
	ratified, err := f.C.Ratify(context.Background(), stream, 1, 1)
	fixturekit.Must(t, err)
	if ratified.Sealing != "requested" || ratified.Round != 1 || ratified.Spec != 1 || ratified.Plan != 1 {
		t.Fatalf("ratification %+v", ratified)
	}
	f.AwaitFeature(t, stream, intake.BuildingState)
	if record := f.Ratification(t, stream, 1); record.Revision != (shed.Pin{Spec: 1, Plan: 1}) || len(record.Dissent) != 0 {
		t.Fatalf("the ratification %+v", record)
	}
	ops := fixturekit.AwaitAcknowledged(t, func(t *testing.T) []trace.OperationRecord { return f.SealOperations(t, stream) })
	if len(ops) != 1 || ops[0].Result == nil || ops[0].Result.Outcome != "succeeded" {
		t.Fatalf("seal operations %+v", ops)
	}
	drafts := f.AcknowledgedDraftOperations(t, stream)
	if len(drafts) != 1 || drafts[0].Result == nil || drafts[0].Result.Outcome != "succeeded" {
		t.Fatalf("draft operations %+v", drafts)
	}
	if moves := f.FeatureMoves(t, stream); strings.Join(moves, " ") != "handin:handed sketched:sketched in-shed:in-shed ratified:ratified building:building" {
		t.Fatalf("feature moves %v", moves)
	}
	f.DebatedNothing(t, stream)
}

func TestHandInKeyThatSkippedDebateRefusesARetryWithoutTheSkip(t *testing.T) {
	t.Parallel()
	f, stream := handedInSkippingDebate(t)
	content := fixturekit.HandedDesign
	_, err := f.C.HandIn(context.Background(), api.HandInRequest{Project: f.Project, Key: "design", Stdin: &content})
	if !intakefixture.Failed(err, api.Conflict) || !strings.Contains(err.Error(), "key design already handed in workstream "+string(stream)+" skipping debate; use a new key") {
		t.Fatalf("retry without the skip: %v", err)
	}
	if again := f.HandInSkipping(t, "design"); again != stream {
		t.Fatalf("retry handed in %s, want %s", again, stream)
	}
}

func TestHandInKeyThatWasDebatedRefusesARetryWithTheSkip(t *testing.T) {
	t.Parallel()
	f := journey.NewDebateFixture(t, 2, 1)
	t.Cleanup(func() { f.Stop(t) })
	content := fixturekit.HandedDesign
	p := &intakefixture.Faults{}
	f.Member(1, 1, 1, intakefixture.Concedes(p))
	f.Member(1, 2, 1, intakefixture.Concedes(p))
	debated := f.HandIn(t, "debated", fixturekit.HandedDesign)
	f.AwaitShed(t, debated, "concluded-1")
	p.Check(t)
	if ops := f.AcknowledgedRoundOperations(t, debated); len(ops) != 1 {
		t.Fatalf("round operations %+v", ops)
	}
	if reason := f.Transition(t, debated, intake.HandInTransition).Reason; reason != "the owner handed in handed/stdin from stdin" {
		t.Fatalf("hand-in reason %q", reason)
	}
	_, err := f.C.HandIn(context.Background(), api.HandInRequest{Project: f.Project, Key: "debated", Stdin: &content, SkipDebate: true})
	if !intakefixture.Failed(err, api.Conflict) || !strings.Contains(err.Error(), "key debated already handed in workstream "+string(debated)+" without skipping debate; use a new key") {
		t.Fatalf("retry with the skip: %v", err)
	}
}

func TestHandInSkipSurvivesARestart(t *testing.T) {
	t.Parallel()
	f := journey.NewDebateFixture(t, 1, 1)
	committee := f.Opts.Committee
	f.Stop(t)
	f.Opts.Committee = nil
	f.Start(t)
	stream := f.HandInSkipping(t, "design")
	f.AwaitPacket(t, stream, "ratify: no objection stands")
	f.AwaitFeature(t, stream, intake.InShedState)
	f.Stop(t)

	f.Opts.Committee = committee
	f.Start(t)
	defer f.Stop(t)
	f.Upstream(t)
	if _, err := f.C.Ratify(context.Background(), stream, 1, 1); err != nil {
		t.Fatal(err)
	}
	f.AwaitFeature(t, stream, intake.BuildingState)
	fixturekit.AwaitAcknowledged(t, func(t *testing.T) []trace.OperationRecord { return f.SealOperations(t, stream) })
	f.DebatedNothing(t, stream)
}
