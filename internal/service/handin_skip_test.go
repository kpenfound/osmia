package service

import (
	"context"
	"strings"
	"testing"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/shed"
	"github.com/kpenfound/osmia/internal/trace"
)

// handInSkipping hands the design in with debate skipped.
func (f *shedFixture) handInSkipping(t *testing.T, key string) config.WorkstreamID {
	t.Helper()
	content := handedDesign
	out, err := f.c.HandIn(context.Background(), HandInRequest{Project: f.project, Key: key, Stdin: &content, SkipDebate: true})
	must(t, err)
	if !out.SkipDebate || out.State != HandedState {
		t.Fatalf("hand-in %+v", out)
	}
	return out.Workstream
}

// debatedNothing checks that the workstream has no committee, and that no
// round, reply or committee turn was ever asked for or run.
func (f *shedFixture) debatedNothing(t *testing.T, stream config.WorkstreamID) {
	t.Helper()
	if ops := f.roundOperations(t, stream); len(ops) != 0 {
		t.Fatalf("round operations %+v", ops)
	}
	if ops := f.replyOperations(t, stream); len(ops) != 0 {
		t.Fatalf("reply operations %+v", ops)
	}
	if moves := f.shedMoves(t, stream); len(moves) != 0 {
		t.Fatalf("shed moves %v", moves)
	}
	threads, err := f.repository().Threads(stream)
	must(t, err)
	for _, th := range threads {
		if th.Identity.Role == committeeRole {
			t.Fatalf("committee thread %+v", th.Identity)
		}
	}
	for turn := range f.ran() {
		if strings.HasPrefix(turn, "shed-") {
			t.Fatalf("turn %s ran", turn)
		}
	}
}

// featureMoves lists the feature transitions of the workstream as ID:state.
func (f *shedFixture) featureMoves(t *testing.T, stream config.WorkstreamID) []string {
	t.Helper()
	var moves []string
	for _, tr := range f.transitions(t, stream) {
		if tr.Subject == trace.FeatureSubject {
			moves = append(moves, tr.ID+":"+tr.To)
		}
	}
	return moves
}

// A hand-in that skips debate, to a service with a committee, gets the
// architect's draft and no debate: the workstream enters the shed without a
// committee, the packet asks the owner to ratify, and the ratification seals.
func TestHandInSkippingDebateIsRatifiedAndSeals(t *testing.T) {
	t.Parallel()
	f := newDebateFixture(t, 2, 1)
	defer f.stop(t)
	ctx := context.Background()
	f.upstream(t)
	stream := f.handInSkipping(t, "design")

	skip := f.transition(t, stream, skipTransition)
	if skip.Subject != ownerSubject || skip.From != "" || skip.To != skippedValue || skip.Actor != ownerActor || skip.Cause != handInTransition ||
		skip.Reason != "the owner skipped debate at hand-in; the workstream still needs the owner's ratification of the spec and the plan" {
		t.Fatalf("skip transition %+v", skip)
	}
	if reason := f.transition(t, stream, handInTransition).Reason; reason != "the owner handed in handed/stdin from stdin and skipped debate" {
		t.Fatalf("hand-in reason %q", reason)
	}
	// A retry of the key must ask for what the hand-in did.
	content := handedDesign
	_, err := f.c.HandIn(ctx, HandInRequest{Project: f.project, Key: "design", Stdin: &content})
	if !failed(err, Conflict) || !strings.Contains(err.Error(), "key design already handed in workstream "+string(stream)+" skipping debate; use a new key") {
		t.Fatalf("retry without the skip: %v", err)
	}
	if again := f.handInSkipping(t, "design"); again != stream {
		t.Fatalf("retry handed in %s, want %s", again, stream)
	}

	packet := f.awaitPacket(t, stream, "ratify: no objection stands")
	if !packet.Skipped || packet.Round != 1 || packet.Revision != (shed.Pin{Spec: 1, Plan: 1}) || len(packet.Dissent) != 0 || packet.Conclusion != skip.Reason {
		t.Fatalf("the packet of a debate skipped at hand-in %+v", packet)
	}
	f.awaitFeature(t, stream, InShedState)
	enter := f.transition(t, stream, InShedState)
	want := "spec.md revision 1 and plan.json revision 1 enter the shed without a committee: the owner skipped debate"
	if enter.From != SketchedState || enter.Actor != shedActor || enter.Cause != skipTransition || enter.Reason != want {
		t.Fatalf("entering the shed %+v", enter)
	}
	// The chief of staff is asked to present the packet once there is one.
	if notice := f.notice(t, stream, InShedState); notice != "Workstream state changed from sketched to in-shed: "+want+".\n"+presentation("ratify: no objection stands") {
		t.Fatalf("the notice of entering the shed %q", notice)
	}
	if _, err := f.c.ShedSkip(ctx, stream); !failed(err, Conflict) || !strings.Contains(err.Error(), "debate on workstream "+string(stream)+" is already skipped") {
		t.Fatalf("skipping again: %v", err)
	}

	ratified, err := f.c.Ratify(ctx, stream, 1, 1)
	must(t, err)
	if ratified.Sealing != "requested" || ratified.Round != 1 || ratified.Spec != 1 || ratified.Plan != 1 {
		t.Fatalf("ratification %+v", ratified)
	}
	f.awaitFeature(t, stream, RatifiedState)
	ops := awaitAcknowledged(t, func(t *testing.T) []trace.OperationRecord { return f.sealOperations(t, stream) })
	if len(ops) != 1 || ops[0].Result == nil || ops[0].Result.Outcome != "succeeded" {
		t.Fatalf("seal operations %+v", ops)
	}
	drafts := f.acknowledgedDraftOperations(t, stream)
	if len(drafts) != 1 || drafts[0].Result == nil || drafts[0].Result.Outcome != "succeeded" {
		t.Fatalf("draft operations %+v", drafts)
	}
	if moves := f.featureMoves(t, stream); strings.Join(moves, " ") != "handin:handed sketched:sketched in-shed:in-shed ratified:ratified" {
		t.Fatalf("feature moves %v", moves)
	}
	f.debatedNothing(t, stream)

	// A hand-in without the skip is debated as ever.
	p := &faults{}
	f.member(1, 1, 1, concedes(p))
	f.member(1, 2, 1, concedes(p))
	debated := f.handIn(t, "debated", handedDesign)
	f.awaitShed(t, debated, "concluded-1")
	p.check(t)
	if ops := f.acknowledgedRoundOperations(t, debated); len(ops) != 1 {
		t.Fatalf("round operations %+v", ops)
	}
	if reason := f.transition(t, debated, handInTransition).Reason; reason != "the owner handed in handed/stdin from stdin" {
		t.Fatalf("hand-in reason %q", reason)
	}
	_, err = f.c.HandIn(ctx, HandInRequest{Project: f.project, Key: "debated", Stdin: &content, SkipDebate: true})
	if !failed(err, Conflict) || !strings.Contains(err.Error(), "key debated already handed in workstream "+string(debated)+" without skipping debate; use a new key") {
		t.Fatalf("retry with the skip: %v", err)
	}
}

// The skip is on the record: a service that could not run a committee and a
// later one that can both honour it.
func TestHandInSkipSurvivesARestart(t *testing.T) {
	t.Parallel()
	f := newDebateFixture(t, 1, 1)
	committee := f.opts.Committee
	f.stop(t)
	f.opts.Committee = nil
	f.start(t)
	stream := f.handInSkipping(t, "design")
	f.awaitPacket(t, stream, "ratify: no objection stands")
	f.awaitFeature(t, stream, InShedState)
	f.stop(t)

	f.opts.Committee = committee
	f.start(t)
	defer f.stop(t)
	f.upstream(t)
	if _, err := f.c.Ratify(context.Background(), stream, 1, 1); err != nil {
		t.Fatal(err)
	}
	f.awaitFeature(t, stream, RatifiedState)
	awaitAcknowledged(t, func(t *testing.T) []trace.OperationRecord { return f.sealOperations(t, stream) })
	f.debatedNothing(t, stream)
}

// A skip through the shed after a hand-in without one is not part of the
// hand-in: a retry of the key returns the same workstream.
func TestHandInRetryAfterAShedSkip(t *testing.T) {
	t.Parallel()
	f := newDebateFixture(t, 1, 1)
	ctx := context.Background()
	f.stop(t)
	f.opts.Committee = nil
	f.start(t)
	defer f.stop(t)
	stream := f.handIn(t, "design", handedDesign)
	f.await(t, stream, sketched)
	if _, err := f.c.ShedSkip(ctx, stream); err != nil {
		t.Fatal(err)
	}
	content := handedDesign
	out, err := f.c.HandIn(ctx, HandInRequest{Project: f.project, Key: "design", Stdin: &content})
	must(t, err)
	if out.Workstream != stream || out.SkipDebate {
		t.Fatalf("retry %+v", out)
	}
	_, err = f.c.HandIn(ctx, HandInRequest{Project: f.project, Key: "design", Stdin: &content, SkipDebate: true})
	if !failed(err, Conflict) || !strings.Contains(err.Error(), "key design already handed in workstream "+string(stream)+" without skipping debate; use a new key") {
		t.Fatalf("retry with the skip: %v", err)
	}
}
