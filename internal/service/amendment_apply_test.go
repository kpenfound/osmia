package service

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/kpenfound/osmia/internal/amendment"
	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/followup"
	"github.com/kpenfound/osmia/internal/plan"
	"github.com/kpenfound/osmia/internal/seal"
	"github.com/kpenfound/osmia/internal/trace"
)

// amendedIndependentPlan changes the footprint of unit dedupe of
// independentPlan, and adds unit audit, which depends on resume, and unit
// report, which depends on nothing.
const amendedIndependentPlan = `{"version": 1, "units": [
  {"id": "resume", "title": "Resume from the last chunk", "addresses": [{"criterion": "spec#1", "proof": {"kind": "new-test", "name": "TestResume"}}], "depends_on": [], "footprint": ["internal.trace"]},
  {"id": "dedupe", "title": "Skip acknowledged chunks", "addresses": [{"criterion": "spec#2", "proof": {"kind": "reviewer-judgement", "name": "no chunk is sent twice"}}], "depends_on": [], "footprint": ["internal"]},
  {"id": "audit", "title": "Record acknowledged chunks", "addresses": [{"criterion": "spec#2", "proof": {"kind": "reviewer-judgement", "name": "acknowledgements are recorded"}}], "depends_on": ["resume"], "footprint": ["internal.trace"]},
  {"id": "report", "title": "Report skipped chunks", "addresses": [{"criterion": "spec#2", "proof": {"kind": "reviewer-judgement", "name": "skips are reported"}}], "depends_on": [], "footprint": ["internal.trace"]}
]}
`

// approveAmendment plants amendment 1 of the workstream in a stopped
// service's open trace, filed by the chief of staff for no unit, with the
// given proposed spec and plan and the affected set drafting computes for
// them against the sealed ones, presents it, and records the owner's
// approval of it. It returns the amendment controller over the trace and the
// request.
func approveAmendment(t *testing.T, f *shedFixture, repository *trace.Repository, stream config.WorkstreamID, spec, graph string) (amendmentDebate, trace.Amendment) {
	t.Helper()
	ctx := context.Background()
	sealed, sealDoc, found, err := seal.Latest(repository, stream)
	must(t, err)
	if !found {
		t.Fatal("the workstream has no seal")
	}
	docs, err := trace.Read[trace.Document](repository, stream)
	must(t, err)
	var sealedSpec, sealedPlan string
	for _, d := range docs {
		switch {
		case d.ID == plan.SpecDocument && d.Revision == sealed.Revision.Spec:
			sealedSpec = d.Content
		case d.ID == plan.PlanDocument && d.Revision == sealed.Revision.Plan:
			sealedPlan = d.Content
		}
	}
	before, err := plan.Parse([]byte(sealedPlan))
	must(t, err)
	after, err := plan.Parse([]byte(graph))
	must(t, err)
	affected, err := json.Marshal(affectedRevision(sealedSpec, spec, before, after))
	must(t, err)
	at := f.clock.Now()
	chief := trace.Actor{Kind: "agent", ID: trace.ChiefOfStaff}
	header := func(schema, id string, by trace.Actor) trace.Header {
		return trace.Header{Schema: schema, Version: trace.Version, ID: id, Revision: 1, Project: repository.Project(), Workstream: stream, At: at, Actor: by, Cause: "fixture"}
	}
	request := trace.Amendment{Header: header("osmia.trace.amendment", "1", chief), Requester: chief, Role: trace.ChiefOfStaff, Thread: trace.ChiefOfStaff, Turn: "events_1",
		Citations: []string{"spec#1"}, Change: "Resume from a durable checkpoint", Reason: "Acknowledgements are not durable", Seal: sealed.Seal, SealRevision: sealDoc.Revision, SpecHash: sealed.SpecHash}
	must(t, repository.Append(ctx, request))
	document := func(id, path, content string) trace.Document {
		return trace.Document{Header: header("osmia.trace.document", id, architectActor), Path: path, Content: content}
	}
	_, err = repository.RecordDocumentsWith(ctx, []trace.Document{
		document("amendment_1_spec", "amendments/1/spec.md", spec), document("amendment_1_plan", "amendments/1/plan.json", graph),
		document("amendment_1_affected", "amendments/1/affected.json", string(affected)), document("amendment-1-presented-packet", amendmentPacketPath("1"), `{"round":1}`+"\n"),
	}, trace.Transaction{Transition: trace.Transition{Header: header("osmia.trace.transition", "amendment-1-presented", shedActor), Subject: amendmentSubject("1"), To: amendmentPresented, Reason: "presented"}})
	must(t, err)
	f.s.active = &activeProject{repository: repository}
	out, api := f.s.decideAmendment(ctx, string(stream), "1", AmendmentDecisionRequest{Decision: AmendmentApprove, Note: "Checkpoints are what we meant.", Packet: 1})
	if api != nil || out.State != amendmentApproved {
		t.Fatalf("approval %+v %v", out, api)
	}
	return amendmentDebate{&debate{s: f.s, repository: repository}}, request
}

func TestApprovedAmendmentUsesCurrentDriftBase(t *testing.T) {
	f, stream, repository := newApprovedFixture(t, "drifted-amendment")
	defer repository.Close()
	a, req := approveAmendment(t, f, repository, stream, amendedSpec, independentPlan)
	moved := recordAmendmentSealRevision(t, f, repository, stream, func(s *seal.Seal) { s.Base.Commit = "later-upstream-commit" })
	step(t, a, stream, req, amendmentResealed)
	current, doc, found, err := seal.Latest(repository, stream)
	must(t, err)
	if !found || doc.Revision != 3 || current.Base != moved.Base || current.Seal != moved.Seal+1 || current.SpecHash != seal.SpecHash(amendedSpec) {
		t.Fatalf("resealed %+v at revision %d, want current base %+v", current, doc.Revision, moved.Base)
	}
	applications, err := amendment.Read(repository, stream)
	must(t, err)
	if len(applications) != 1 || applications[0].From.SealRevision != 2 || applications[0].To.SealRevision != 3 {
		t.Fatalf("application %+v", applications)
	}
}

func TestApprovedAmendmentWithChangedSealedDocumentsIsUnapplied(t *testing.T) {
	for name, change := range map[string]func(*seal.Seal){
		"spec": func(s *seal.Seal) { s.Revision.Spec++; s.SpecHash = seal.SpecHash(amendedSpec) },
		"plan": func(s *seal.Seal) { s.Revision.Plan++ },
	} {
		t.Run(name, func(t *testing.T) {
			f, stream, repository := newApprovedFixture(t, "stale-"+name)
			defer repository.Close()
			a, req := approveAmendment(t, f, repository, stream, amendedSpec, independentPlan)
			recordAmendmentSealRevision(t, f, repository, stream, change)
			step(t, a, stream, req, amendmentUnapplied)
			if got := transitionByID(t, repository, stream, "amendment-1-unapplied").Reason; !strings.Contains(got, "sealed spec or plan changed") {
				t.Fatalf("unapplied reason %q", got)
			}
			if docs := streamDocuments(t, repository, stream, seal.DocumentID); len(docs) != 2 {
				t.Fatalf("unexpected reseal: %+v", docs)
			}
		})
	}
}

// step runs the amendment controller once on amendment 1 and checks the
// state it leaves it in.
func step(t *testing.T, a amendmentDebate, stream config.WorkstreamID, req trace.Amendment, want string) {
	t.Helper()
	must(t, a.one(context.Background(), stream, req))
	if state, err := a.repository.Workflow(stream, amendmentSubject(req.ID)); err != nil || state.Value != want {
		t.Fatalf("amendment %s is %+v, want %s: %v", req.ID, state, want, err)
	}
}

// awaitTransition waits until the running service records the transition,
// and checks the states it moves between.
func (f *shedFixture) awaitTransition(t *testing.T, stream config.WorkstreamID, id, from, to string) {
	t.Helper()
	deadline := time.Now().Add(demoTimeout)
	for {
		i := slices.IndexFunc(f.transitions(t, stream), func(tr trace.Transition) bool { return tr.ID == id })
		if i >= 0 {
			if tr := f.transitions(t, stream)[i]; tr.From != from || tr.To != to {
				t.Fatalf("transition %s %+v, want %s to %s", id, tr, from, to)
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("transition %s is never recorded", id)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// landingReason returns why the unit's latest approval cannot land, or "".
func landingReason(t *testing.T, lands *foreman, repository *trace.Repository, stream config.WorkstreamID, unit string) string {
	t.Helper()
	review, result := approvedReview(t, repository, stream, unit)
	in := landInput{Unit: unit, Review: review.Revision, Candidate: result.Identity.Candidate.Revision, Base: result.Identity.Candidate.BaseRevision}
	reason, err := lands.current(context.Background(), stream, in, result)
	must(t, err)
	return reason
}

// masonTurns returns the turns of the unit's mason thread with the given ID.
func masonTurns(t *testing.T, repository *trace.Repository, stream config.WorkstreamID, unit, turn string) []trace.QueuedTurn {
	t.Helper()
	th, err := repository.Thread(stream, masonAgent(unit))
	must(t, err)
	return slices.DeleteFunc(slices.Clone(th.Turns), func(q trace.QueuedTurn) bool { return q.Request.TurnID != turn })
}

// unitMoves returns the transitions of the unit's subject recorded for
// amendment 1.
func unitMoves(t *testing.T, repository *trace.Repository, stream config.WorkstreamID, unit string) []trace.Transition {
	t.Helper()
	transitions, err := trace.Read[trace.Transition](repository, stream)
	must(t, err)
	return slices.DeleteFunc(transitions, func(tr trace.Transition) bool {
		return tr.Subject != trace.UnitSubject(unit) || !strings.HasSuffix(tr.ID, "_amendment_1")
	})
}

// An approved amendment that changes a criterion invalidates the approval of
// the unit that addresses it from the resealing on: that approval is refused
// at landing, and applying the amendment returns the unit to implementing
// with a mason turn that carries the notice and the amended spec. The
// approval of the unit it does not affect lands on the new seal. A stop
// between the application's commit and the mason turn completes the turn
// once on the next pass.
func TestApprovedAmendmentReworksItsUnitAndLandsUnaffectedApprovals(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f, stream, repository := newApprovedFixture(t, "amended")
	defer repository.Close()
	lands := &foreman{masons: newMasonController(f.s, repository)}
	a, req := approveAmendment(t, f, repository, stream, amendedSpec, independentPlan)

	step(t, a, stream, req, amendmentResealed)
	if got := landingReason(t, lands, repository, stream, "resume"); !strings.HasPrefix(got, "stale approval: stale review inputs: ") || !strings.Contains(got, "report seal 1 is not the current seal") {
		t.Fatalf("the reworked unit's approval after the reseal: %q", got)
	}
	if got := landingReason(t, lands, repository, stream, "dedupe"); got != "" {
		t.Fatalf("the unaffected approval after the reseal: %q", got)
	}
	applications, err := amendment.Read(repository, stream)
	must(t, err)
	if len(applications) != 1 || !slices.Equal(applications[0].Criteria, []string{"spec#1"}) || !slices.Equal(applications[0].Rework, []string{"resume"}) || len(applications[0].Notify) != 0 ||
		applications[0].From != (amendment.Pin{Seal: 1, SealRevision: 1, Spec: 1, Plan: 1}) || applications[0].To != (amendment.Pin{Seal: 2, SealRevision: 2, Spec: 2, Plan: 1}) || applications[0].Applied != nil {
		t.Fatalf("application %+v", applications)
	}

	f.s.boundary = func(name string) error {
		if name == "amendment-applied" {
			return errors.New("the service stopped")
		}
		return nil
	}
	if err := a.one(ctx, stream, req); err == nil {
		t.Fatal("the interrupted application reported no error")
	}
	f.s.boundary = nil
	if state, err := repository.Workflow(stream, trace.UnitSubject("resume")); err != nil || state.Value != UnitImplementing {
		t.Fatalf("the reworked unit is %+v: %v", state, err)
	}
	moved := transitionByID(t, repository, stream, amendmentUnitID("resume", "1", true))
	if moved.From != UnitApproved || moved.To != UnitImplementing || moved.Actor != amendmentActor || !strings.Contains(moved.Reason, "changed the meaning of spec#1") {
		t.Fatalf("rework %+v", moved)
	}
	if turns := masonTurns(t, repository, stream, "resume", amendmentMasonTurn("resume", "1")); len(turns) != 0 {
		t.Fatalf("the mason turn was queued before the stop: %+v", turns)
	}

	step(t, a, stream, req, amendmentRuled)
	step(t, a, stream, req, amendmentRuled)
	turns := masonTurns(t, repository, stream, "resume", amendmentMasonTurn("resume", "1"))
	if len(turns) != 1 || turns[0].Request.Cause != moved.ID || turns[0].Request.Actor != amendmentActor {
		t.Fatalf("mason turns %+v", turns)
	}
	for _, part := range []string{"returns to implementing", "## Amendments", "- amendments/1/application.json: the owner approved amendment 1; seal 2 governs spec.md revision 2",
		"changed criteria: spec#1", "affected proofs of this unit: resume:spec#1", "<<< osmia:question | asker (copied by Osmia)", "| Change: Resume from a durable checkpoint",
		"<<< osmia:owner_response | owner (copied by Osmia)", "| Checkpoints are what we meant.", "- spec#1: An interrupted upload resumes from a durable checkpoint."} {
		if !strings.Contains(turns[0].Request.Prompt, part) {
			t.Errorf("the mason turn misses %q:\n%s", part, turns[0].Request.Prompt)
		}
	}
	if got := unitMoves(t, repository, stream, "resume"); len(got) != 1 {
		t.Fatalf("resume's moves for the amendment %+v", got)
	}
	if got := unitMoves(t, repository, stream, "dedupe"); len(got) != 0 {
		t.Fatalf("the unaffected unit moved %+v", got)
	}

	must(t, lands.Pass(ctx))
	ops := landOperations(t, repository, stream)
	if len(ops) != 1 {
		t.Fatalf("landing operations %+v", ops)
	}
	if in, err := decodeLand(ops[0].Operation); err != nil || in.Unit != "dedupe" {
		t.Fatalf("landing of %+v: %v", in, err)
	}
	outcome, err := lands.Apply(ctx, ops[0].Operation)
	must(t, err)
	if outcome.Outcome != "succeeded" {
		t.Fatalf("the unaffected approval did not land: %+v", outcome)
	}
	if state, err := repository.Workflow(stream, trace.UnitSubject("dedupe")); err != nil || state.Value != UnitMerged {
		t.Fatalf("dedupe is %+v: %v", state, err)
	}
}

// An approved plan amendment that changes a unit's footprint but not the
// meaning of its criteria returns that unit's approval to review, and its
// review evidence and mason bundle carry the amendment's notice. The approval
// of the other unit is carried to the new plan revision and lands. The units
// the plan adds enter planned or ready by their dependencies, and the one
// waiting for the unit that lands becomes ready when it merges.
func TestApprovedPlanAmendmentNotifiesItsUnitAndAddsUnits(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f, stream, repository := newApprovedFixture(t, "notified")
	defer repository.Close()
	lands := &foreman{masons: newMasonController(f.s, repository)}
	a, req := approveAmendment(t, f, repository, stream, validSpec, amendedIndependentPlan)

	step(t, a, stream, req, amendmentResealed)
	if got := landingReason(t, lands, repository, stream, "dedupe"); got != "stale approval: stale plan revision; review the current candidate again" {
		t.Fatalf("the notified unit's approval after the reseal: %q", got)
	}
	if got := landingReason(t, lands, repository, stream, "resume"); got != "" {
		t.Fatalf("the unaffected approval after the reseal: %q", got)
	}
	step(t, a, stream, req, amendmentApplied)
	moved := transitionByID(t, repository, stream, amendmentUnitID("dedupe", "1", false))
	if moved.From != UnitApproved || moved.To != UnitReviewing || !strings.Contains(moved.Reason, "changed its plan entry, and the criteria it addresses keep their meaning") {
		t.Fatalf("notice %+v", moved)
	}
	if body := noticeOf(t, repository, stream, moved.ID); !strings.HasPrefix(body, "Unit dedupe: unit dedupe returns to review from approved") {
		t.Fatalf("notice body %q", body)
	}
	for unit, want := range map[string]string{"resume": UnitApproved, "dedupe": UnitReviewing, "audit": UnitPlanned, "report": UnitReady} {
		if state, err := repository.Workflow(stream, trace.UnitSubject(unit)); err != nil || state.Value != want {
			t.Fatalf("unit %s is %+v, want %s: %v", unit, state, want, err)
		}
	}
	applied := transitionByID(t, repository, stream, "amendment-1-applied")
	if applied.Reason != "approved amendment 1 is applied to seal 1: reworked none; notified dedupe; held until they leave waiting or contested none; added audit planned (waiting for resume), report ready; follow-ups none" {
		t.Fatalf("applied %q", applied.Reason)
	}
	applications, err := amendment.Read(repository, stream)
	must(t, err)
	if len(applications) != 1 || applications[0].Applied == nil || len(applications[0].Applied.Held) != 0 || !slices.Equal(applications[0].Added, []string{"audit", "report"}) {
		t.Fatalf("application %+v", applications)
	}

	m := newMasonController(f.s, repository)
	review, _, err := m.candidateEvidence(ctx, stream, "dedupe")
	must(t, err)
	i := slices.IndexFunc(review.Context, func(item coreadapter.ContextItem) bool { return item.Source == "amendment notices" })
	if i < 0 || !strings.Contains(review.Context[i].Content, "It changed this unit's entry in the plan; the criteria the unit addresses keep their meaning.") || !strings.Contains(review.Context[i].Content, "affected proofs of this unit: dedupe:spec#2") {
		t.Fatalf("review evidence %+v", review.Context)
	}
	dedupe, err := m.bundle(ctx, stream, "dedupe")
	must(t, err)
	if len(dedupe.Amendments) != 1 || dedupe.Amendments[0].Rework || !strings.Contains(dedupe.Render(), "<<< osmia:question | asker (copied by Osmia)") {
		t.Fatalf("dedupe's bundle %+v", dedupe.Amendments)
	}
	if resume, err := m.bundle(ctx, stream, "resume"); err != nil || len(resume.Amendments) != 0 || strings.Contains(resume.Render(), "## Amendments") {
		t.Fatalf("the unaffected unit's bundle %+v: %v", resume.Amendments, err)
	}

	must(t, lands.Pass(ctx))
	ops := landOperations(t, repository, stream)
	if len(ops) != 1 {
		t.Fatalf("landing operations %+v", ops)
	}
	outcome, err := lands.Apply(ctx, ops[0].Operation)
	must(t, err)
	if in, _ := decodeLand(ops[0].Operation); in.Unit != "resume" || outcome.Outcome != "succeeded" {
		t.Fatalf("landing of %s: %+v", in.Unit, outcome)
	}
	if state, err := repository.Workflow(stream, trace.UnitSubject("audit")); err != nil || state.Value != UnitReady {
		t.Fatalf("audit after resume merged is %+v: %v", state, err)
	}
}

// Approving an amendment that changes a criterion a merged unit addresses
// never reopens the unit: the gap becomes a follow-up unit. The final report
// read against the replaced revisions is invalidated with a notice, and no
// longer authorises delivery; final review runs again, against the amended
// documents, once the follow-up has merged.
func TestApprovedAmendmentInvalidatesTheFinalReportAndAddsAFollowup(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f, stream, repository, finals := newFinalFixture(t, "reviewed")
	_, op := assembleBoth(t, f, repository, finals, stream,
		map[string]string{"internal/trace/resume.go": "package trace\n"},
		map[string]string{"internal/trace/dedupe.go": "package trace\n"})
	f.finalTurn(1, 1, func(ctx context.Context, tools *mcp.ClientSession) error {
		_, err := callTool(ctx, tools, FinalReportTool, map[string]any{"criteria": []any{
			map[string]any{"criterion": "spec#1", "evidence": "resume.go"},
			map[string]any{"criterion": "spec#2", "evidence": "dedupe.go"},
		}})
		return err
	})
	if result, err := finals.Apply(ctx, op); err != nil || result.Outcome != "succeeded" {
		t.Fatalf("final review 1 %+v: %v", result, err)
	}
	if _, reason, err := finals.finalGate(ctx, stream); err != nil || reason != "" {
		t.Fatalf("final review 1 does not authorise delivery: %q %v", reason, err)
	}
	a, req := approveAmendment(t, f, repository, stream, amendedSpec, validPlan)
	step(t, a, stream, req, amendmentResealed)
	step(t, a, stream, req, amendmentApplied)

	id := "amendment-1-spec-1"
	added, err := followup.Read(repository, stream)
	must(t, err)
	if len(added) != 1 || added[0].Unit.ID != id || added[0].Amendment != "1" || added[0].Criterion != "spec#1" || !strings.Contains(added[0].Gap, "after resume merged") || !slices.Equal(added[0].Unit.Footprint, []string{"internal.trace"}) {
		t.Fatalf("follow-ups %+v", added)
	}
	for unit, want := range map[string]string{"resume": UnitMerged, "dedupe": UnitMerged, id: UnitReady} {
		if state, err := repository.Workflow(stream, trace.UnitSubject(unit)); err != nil || state.Value != want {
			t.Fatalf("unit %s is %+v, want %s: %v", unit, state, want, err)
		}
	}
	if got := unitMoves(t, repository, stream, "resume"); len(got) != 0 {
		t.Fatalf("the merged unit was reopened: %+v", got)
	}
	applications, err := amendment.Read(repository, stream)
	must(t, err)
	if len(applications) != 1 || applications[0].Applied == nil || applications[0].Applied.FinalReview != 1 || !slices.Equal(applications[0].Applied.Followups, []string{id}) {
		t.Fatalf("application %+v", applications)
	}
	outbox, err := repository.Outbox(stream)
	must(t, err)
	if !slices.ContainsFunc(outbox, func(e trace.OutboxEntry) bool {
		return e.TransitionID == "amendment-1-applied" && e.Event.Body == "Final review 1 read seal 1, spec revision 1 and plan revision 1, which amendment 1 replaced: its report no longer authorises delivery. Final review runs again once every unit has merged."
	}) {
		t.Fatalf("no notice of the invalidated final report in %+v", outbox)
	}
	if _, reason, err := finals.finalGate(ctx, stream); err != nil || !strings.HasPrefix(reason, "final review 1 is stale") {
		t.Fatalf("the invalidated final report: %q %v", reason, err)
	}
	masonBundle, err := newMasonController(f.s, repository).bundle(ctx, stream, id)
	must(t, err)
	if !strings.Contains(masonBundle.Render(), "follow-up: amendment 1, spec#1\ngap: "+added[0].Gap) {
		t.Fatalf("the follow-up's bundle:\n%s", masonBundle.Render())
	}

	// The running service records final review 1's result and asks for no
	// other while the follow-up has not merged.
	must(t, repository.Close())
	f.start(t)
	awaitAcknowledged(t, func(t *testing.T) []trace.OperationRecord { return finalOperations(t, f.repository(), stream) })
	settle()
	f.stop(t)
	repository, err = trace.Open(f.s.cfg.Root, f.s.cfg.Project)
	must(t, err)
	defer repository.Close()
	finals = &finalReviewer{s: f.s, repository: repository}
	must(t, finals.Pass(ctx))
	if ops := finalOperations(t, repository, stream); len(ops) != 1 {
		t.Fatalf("final review asked for before the follow-up merged: %+v", ops)
	}
	mergeDirectly(t, f, repository, stream, id, map[string]string{"internal/trace/checkpoint.go": "package trace\n"})
	must(t, finals.Pass(ctx))
	ops := finalOperations(t, repository, stream)
	if len(ops) != 2 {
		t.Fatalf("final review operations %+v", ops)
	}
	in, err := decodeFinalReview(ops[1].Operation)
	must(t, err)
	if in.Review != 2 || in.Seal != 2 || in.Spec != 2 || in.Plan != 1 {
		t.Fatalf("final review 2 reads %+v", in)
	}
}

// A follow-up recorded while the workstream is building joins its effective
// plan, so the mason controller starts it and the workstream assembles only
// once it has merged too.
func TestFollowupsJoinTheBuildingPlan(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f, stream, repository, finals := newFinalFixture(t, "followed")
	mergeDirectly(t, f, repository, stream, "resume", map[string]string{"internal/trace/resume.go": "package trace\n"})
	a, req := approveAmendment(t, f, repository, stream, amendedSpec, validPlan)
	step(t, a, stream, req, amendmentResealed)
	step(t, a, stream, req, amendmentApplied)
	id := "amendment-1-spec-1"
	b, found, err := newMasonController(f.s, repository).read(stream)
	must(t, err)
	if !found || !slices.ContainsFunc(b.plan.Units, func(u plan.Unit) bool { return u.ID == id }) || b.states[trace.UnitSubject(id)].Value != UnitReady {
		t.Fatalf("the building plan %+v, states %+v", b.plan.Units, b.states)
	}
	mergeDirectly(t, f, repository, stream, "dedupe", map[string]string{"internal/trace/dedupe.go": "package trace\n"})
	must(t, finals.Pass(ctx))
	if feature, err := repository.Workflow(stream, trace.FeatureSubject); err != nil || feature.Value != BuildingState {
		t.Fatalf("the workstream is %+v with its follow-up unmerged: %v", feature, err)
	}
	mergeDirectly(t, f, repository, stream, id, map[string]string{"internal/trace/checkpoint.go": "package trace\n"})
	must(t, finals.Pass(ctx))
	if feature, err := repository.Workflow(stream, trace.FeatureSubject); err != nil || feature.Value != AssembledState {
		t.Fatalf("the workstream is %+v with every unit merged: %v", feature, err)
	}
}
