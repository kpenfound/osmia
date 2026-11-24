package intakejourney

import (
	context "context"
	json "encoding/json"
	errors "errors"
	"github.com/kpenfound/osmia/internal/api"
	"github.com/kpenfound/osmia/internal/delivery"
	"github.com/kpenfound/osmia/internal/execution"
	"github.com/kpenfound/osmia/internal/intake"
	"github.com/kpenfound/osmia/internal/service"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	"github.com/kpenfound/osmia/internal/servicetest/journey"
	slices "slices"
	strings "strings"
	testing "testing"

	amendment "github.com/kpenfound/osmia/internal/amendment"
	config "github.com/kpenfound/osmia/internal/config"
	coreadapter "github.com/kpenfound/osmia/internal/coreadapter"
	followup "github.com/kpenfound/osmia/internal/followup"
	plan "github.com/kpenfound/osmia/internal/plan"
	seal "github.com/kpenfound/osmia/internal/seal"
	intakefixture "github.com/kpenfound/osmia/internal/servicetest/intakefixture"
	trace "github.com/kpenfound/osmia/internal/trace"
	mcp "github.com/modelcontextprotocol/go-sdk/mcp"
)

const amendedIndependentPlan = `{"version": 2, "units": [
  {"id": "resume", "title": "Resume from the last chunk", "task": "Resume from the last chunk", "acceptance": ["spec#1 holds, shown by TestResume (new-test)"], "criteria": ["spec#1"], "depends_on": [], "footprint": ["internal.trace"]},
  {"id": "dedupe", "title": "Skip acknowledged chunks", "task": "Skip acknowledged chunks", "acceptance": ["spec#2 holds, shown by no chunk is sent twice (reviewer-judgement)"], "criteria": ["spec#2"], "depends_on": [], "footprint": ["internal"]},
  {"id": "audit", "title": "Record acknowledged chunks", "task": "Record acknowledged chunks", "acceptance": ["spec#2 holds, shown by acknowledgements are recorded (reviewer-judgement)"], "criteria": ["spec#2"], "depends_on": ["resume"], "footprint": ["internal.trace"]},
  {"id": "report", "title": "Report skipped chunks", "task": "Report skipped chunks", "acceptance": ["spec#2 holds, shown by skips are reported (reviewer-judgement)"], "criteria": ["spec#2"], "depends_on": [], "footprint": ["internal.trace"]}
]}
`

func approveAmendment(t *testing.T, f *journey.ShedFixture, repository *trace.Repository, stream config.WorkstreamID, spec, graph string) (intake.AmendmentDebate, trace.Amendment) {
	t.Helper()
	ctx := context.Background()
	sealed, sealDoc, found, err := seal.Latest(repository, stream)
	fixturekit.Must(t, err)
	if !found {
		t.Fatal("the workstream has no seal")
	}
	docs, err := trace.Read[trace.Document](repository, stream)
	fixturekit.Must(t, err)
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
	fixturekit.Must(t, err)
	after, err := plan.Parse([]byte(graph))
	fixturekit.Must(t, err)
	affected, err := json.Marshal(intake.AffectedRevision(sealedSpec, spec, before, after))
	fixturekit.Must(t, err)
	at := f.Clock.Now()
	chief := trace.Actor{Kind: "agent", ID: trace.ChiefOfStaff}
	header := func(schema, id string, by trace.Actor) trace.Header {
		return trace.Header{Schema: schema, Version: trace.Version, ID: id, Revision: 1, Project: repository.Project(), Workstream: stream, At: at, Actor: by, Cause: "fixture"}
	}
	request := trace.Amendment{Header: header("osmia.trace.amendment", "1", chief), Requester: chief, Role: trace.ChiefOfStaff, Thread: trace.ChiefOfStaff, Turn: "events_1",
		Citations: []string{"spec#1"}, Change: "Resume from a durable checkpoint", Reason: "Acknowledgements are not durable", Seal: sealed.Seal, SealRevision: sealDoc.Revision, SpecHash: sealed.SpecHash}
	fixturekit.Must(t, repository.Append(ctx, request))
	document := func(id, path, content string) trace.Document {
		return trace.Document{Header: header("osmia.trace.document", id, intake.ArchitectActor), Path: path, Content: content}
	}
	_, err = repository.RecordDocumentsWith(ctx, []trace.Document{
		document("amendment_1_spec", "amendments/1/spec.md", spec), document("amendment_1_plan", "amendments/1/plan.json", graph),
		document("amendment_1_affected", "amendments/1/affected.json", string(affected)), document("amendment-1-presented-packet", intake.AmendmentPacketPath("1"), `{"round":1}`+"\n"),
	}, trace.Transaction{Transition: trace.Transition{Header: header("osmia.trace.transition", "amendment-1-presented", intake.ShedActor), Subject: intake.AmendmentSubject("1"), To: intake.AmendmentPresented, Reason: "presented"}})
	fixturekit.Must(t, err)
	f.S.SetSole(&journey.ActiveProject{Repository: repository})
	out, apiErr := f.S.DecideAmendment(ctx, string(stream), "1", api.AmendmentDecisionRequest{Decision: intake.AmendmentApprove, Note: "Checkpoints are what we meant.", Packet: 1})
	if apiErr != nil || out.State != intake.AmendmentApproved {
		t.Fatalf("approval %+v %v", out, apiErr)
	}
	return intake.AmendmentDebate{Debate: &intake.Debate{S: f.S.Intake().DebateDependencies(), Repository: repository}}, request
}

func TestApprovedAmendmentUsesCurrentDriftBase(t *testing.T) {
	t.Parallel()
	f, stream, repository := journey.NewApprovedFixture(t, "drifted-amendment")
	defer repository.Close()
	a, req := approveAmendment(t, f, repository, stream, journey.AmendedSpec, journey.IndependentPlan)
	moved := recordAmendmentSealRevision(t, f, repository, stream, func(s *seal.Seal) { s.Base.Commit = "later-upstream-commit" })
	intakefixture.Step(t, a, stream, req, intake.AmendmentResealed)
	current, doc, found, err := seal.Latest(repository, stream)
	fixturekit.Must(t, err)
	if !found || doc.Revision != 3 || current.Base != moved.Base || current.Seal != moved.Seal+1 || current.SpecHash != seal.SpecHash(journey.AmendedSpec) {
		t.Fatalf("resealed %+v at revision %d, want current base %+v", current, doc.Revision, moved.Base)
	}
	applications, err := amendment.Read(repository, stream)
	fixturekit.Must(t, err)
	if len(applications) != 1 || applications[0].From.SealRevision != 2 || applications[0].To.SealRevision != 3 {
		t.Fatalf("application %+v", applications)
	}
}

func TestApprovedAmendmentWithChangedSealedDocumentsIsUnapplied(t *testing.T) {
	t.Parallel()
	for name, change := range map[string]func(*seal.Seal){
		"spec": func(s *seal.Seal) { s.Revision.Spec++; s.SpecHash = seal.SpecHash(journey.AmendedSpec) },
		"plan": func(s *seal.Seal) { s.Revision.Plan++ },
	} {
		t.Run(name, func(t *testing.T) {
			f, stream, repository := journey.NewApprovedFixture(t, "stale-"+name)
			defer repository.Close()
			a, req := approveAmendment(t, f, repository, stream, journey.AmendedSpec, journey.IndependentPlan)
			recordAmendmentSealRevision(t, f, repository, stream, change)
			intakefixture.Step(t, a, stream, req, intake.AmendmentUnapplied)
			if got := intakefixture.TransitionByID(t, repository, stream, "amendment-1-unapplied").Reason; !strings.Contains(got, "sealed spec or plan changed") {
				t.Fatalf("unapplied reason %q", got)
			}
			if docs := intakefixture.StreamDocuments(t, repository, stream, seal.DocumentID); len(docs) != 2 {
				t.Fatalf("unexpected reseal: %+v", docs)
			}
		})
	}
}

func TestApprovedAmendmentReworksItsUnitAndLandsUnaffectedApprovals(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f, stream, repository := journey.NewApprovedFixture(t, "amended")
	defer repository.Close()
	lands := &journey.Foreman{Masons: journey.NewMasonController(f.S, repository)}
	a, req := approveAmendment(t, f, repository, stream, journey.AmendedSpec, journey.IndependentPlan)

	intakefixture.Step(t, a, stream, req, intake.AmendmentResealed)
	if got := journey.LandingReason(t, lands, repository, stream, "resume"); !strings.HasPrefix(got, "stale approval: stale review inputs: ") || !strings.Contains(got, "report seal 1 is not the current seal") {
		t.Fatalf("the reworked unit's approval after the reseal: %q", got)
	}
	if got := journey.LandingReason(t, lands, repository, stream, "dedupe"); got != "" {
		t.Fatalf("the unaffected approval after the reseal: %q", got)
	}
	applications, err := amendment.Read(repository, stream)
	fixturekit.Must(t, err)
	if len(applications) != 1 || !slices.Equal(applications[0].Criteria, []string{"spec#1"}) || !slices.Equal(applications[0].Rework, []string{"resume"}) || len(applications[0].Notify) != 0 ||
		applications[0].From != (amendment.Pin{Seal: 1, SealRevision: 1, Spec: 1, Plan: 1}) || applications[0].To != (amendment.Pin{Seal: 2, SealRevision: 2, Spec: 2, Plan: 1}) || applications[0].Applied != nil {
		t.Fatalf("application %+v", applications)
	}

	f.S.Boundary = func(name string) error {
		if name == "amendment-applied" {
			return errors.New("the service stopped")
		}
		return nil
	}
	if err := a.One(ctx, stream, req); err == nil {
		t.Fatal("the interrupted application reported no error")
	}
	f.S.Boundary = nil
	if state, err := repository.Workflow(stream, trace.UnitSubject("resume")); err != nil || state.Value != intake.UnitImplementing {
		t.Fatalf("the reworked unit is %+v: %v", state, err)
	}
	moved := intakefixture.TransitionByID(t, repository, stream, intake.AmendmentUnitID("resume", "1", true))
	if moved.From != intake.UnitApproved || moved.To != intake.UnitImplementing || moved.Actor != intake.AmendmentActor || !strings.Contains(moved.Reason, "changed the meaning of spec#1") {
		t.Fatalf("rework %+v", moved)
	}
	if turns := intakefixture.MasonTurns(t, repository, stream, "resume", intake.AmendmentMasonTurn("resume", "1")); len(turns) != 0 {
		t.Fatalf("the mason turn was queued before the stop: %+v", turns)
	}

	intakefixture.Step(t, a, stream, req, intake.AmendmentRuled)
	intakefixture.Step(t, a, stream, req, intake.AmendmentRuled)
	turns := intakefixture.MasonTurns(t, repository, stream, "resume", intake.AmendmentMasonTurn("resume", "1"))
	if len(turns) != 1 || turns[0].Request.Cause != moved.ID || turns[0].Request.Actor != intake.AmendmentActor {
		t.Fatalf("mason turns %+v", turns)
	}
	for _, part := range []string{"returns to implementing", "## Amendments", "- amendments/1/application.json: the owner approved amendment 1; seal 2 governs spec.md revision 2",
		"changed criteria: spec#1", "<<< osmia:question | asker (copied by Osmia)", "| Change: Resume from a durable checkpoint",
		"<<< osmia:owner_response | owner (copied by Osmia)", "| Checkpoints are what we meant.", "- spec#1: An interrupted upload resumes from a durable checkpoint."} {
		if !strings.Contains(turns[0].Request.Prompt, part) {
			t.Errorf("the mason turn misses %q:\n%s", part, turns[0].Request.Prompt)
		}
	}
	if got := intakefixture.UnitMoves(t, repository, stream, "resume"); len(got) != 1 {
		t.Fatalf("resume's moves for the amendment %+v", got)
	}
	if got := intakefixture.UnitMoves(t, repository, stream, "dedupe"); len(got) != 0 {
		t.Fatalf("the unaffected unit moved %+v", got)
	}

	fixturekit.Must(t, lands.Pass(ctx))
	ops := journey.LandOperations(t, repository, stream)
	if len(ops) != 1 {
		t.Fatalf("landing operations %+v", ops)
	}
	if in, err := execution.DecodeLand(ops[0].Operation); err != nil || in.Unit != "dedupe" {
		t.Fatalf("landing of %+v: %v", in, err)
	}
	outcome, err := lands.Apply(ctx, ops[0].Operation)
	fixturekit.Must(t, err)
	if outcome.Outcome != "succeeded" {
		t.Fatalf("the unaffected approval did not land: %+v", outcome)
	}
	if state, err := repository.Workflow(stream, trace.UnitSubject("dedupe")); err != nil || state.Value != intake.UnitMerged {
		t.Fatalf("dedupe is %+v: %v", state, err)
	}
}

func TestApprovedPlanAmendmentNotifiesItsUnitAndAddsUnits(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f, stream, repository := journey.NewApprovedFixture(t, "notified")
	defer repository.Close()
	lands := &journey.Foreman{Masons: journey.NewMasonController(f.S, repository)}
	a, req := approveAmendment(t, f, repository, stream, fixturekit.ValidSpec, amendedIndependentPlan)

	intakefixture.Step(t, a, stream, req, intake.AmendmentResealed)
	if got := journey.LandingReason(t, lands, repository, stream, "dedupe"); got != "stale approval: stale plan revision; review the current candidate again" {
		t.Fatalf("the notified unit's approval after the reseal: %q", got)
	}
	if got := journey.LandingReason(t, lands, repository, stream, "resume"); got != "" {
		t.Fatalf("the unaffected approval after the reseal: %q", got)
	}
	intakefixture.Step(t, a, stream, req, intake.AmendmentApplied)
	moved := intakefixture.TransitionByID(t, repository, stream, intake.AmendmentUnitID("dedupe", "1", false))
	if moved.From != intake.UnitApproved || moved.To != intake.UnitReviewing || !strings.Contains(moved.Reason, "changed its plan entry, and the criteria it serves keep their meaning") {
		t.Fatalf("notice %+v", moved)
	}
	if body := intakefixture.NoticeOf(t, repository, stream, moved.ID); !strings.HasPrefix(body, "Unit dedupe: unit dedupe returns to review from approved") {
		t.Fatalf("notice body %q", body)
	}
	for unit, want := range map[string]string{"resume": intake.UnitApproved, "dedupe": intake.UnitReviewing, "audit": intake.UnitPlanned, "report": intake.UnitReady} {
		if state, err := repository.Workflow(stream, trace.UnitSubject(unit)); err != nil || state.Value != want {
			t.Fatalf("unit %s is %+v, want %s: %v", unit, state, want, err)
		}
	}
	applied := intakefixture.TransitionByID(t, repository, stream, "amendment-1-applied")
	if applied.Reason != "approved amendment 1 is applied to seal 1: reworked none; notified dedupe; held until they leave waiting or contested none; added audit planned (waiting for resume), report ready; follow-ups none" {
		t.Fatalf("applied %q", applied.Reason)
	}
	applications, err := amendment.Read(repository, stream)
	fixturekit.Must(t, err)
	if len(applications) != 1 || applications[0].Applied == nil || len(applications[0].Applied.Held) != 0 || !slices.Equal(applications[0].Added, []string{"audit", "report"}) {
		t.Fatalf("application %+v", applications)
	}

	m := journey.NewMasonController(f.S, repository)
	review, _, err := m.CandidateEvidence(ctx, stream, "dedupe")
	fixturekit.Must(t, err)
	i := slices.IndexFunc(review.Context, func(item coreadapter.ContextItem) bool { return item.Source == "amendment notices" })
	if i < 0 || !strings.Contains(review.Context[i].Content, "It changed this unit's entry in the plan; the criteria the unit serves keep their meaning.") {
		t.Fatalf("review evidence %+v", review.Context)
	}
	dedupe, err := m.Bundle(ctx, stream, "dedupe")
	fixturekit.Must(t, err)
	if len(dedupe.Amendments) != 1 || dedupe.Amendments[0].Rework || !strings.Contains(dedupe.Render(), "<<< osmia:question | asker (copied by Osmia)") {
		t.Fatalf("dedupe's bundle %+v", dedupe.Amendments)
	}
	if resume, err := m.Bundle(ctx, stream, "resume"); err != nil || len(resume.Amendments) != 0 || strings.Contains(resume.Render(), "## Amendments") {
		t.Fatalf("the unaffected unit's bundle %+v: %v", resume.Amendments, err)
	}

	fixturekit.Must(t, lands.Pass(ctx))
	ops := journey.LandOperations(t, repository, stream)
	if len(ops) != 1 {
		t.Fatalf("landing operations %+v", ops)
	}
	outcome, err := lands.Apply(ctx, ops[0].Operation)
	fixturekit.Must(t, err)
	if in, _ := execution.DecodeLand(ops[0].Operation); in.Unit != "resume" || outcome.Outcome != "succeeded" {
		t.Fatalf("landing of %s: %+v", in.Unit, outcome)
	}
	if state, err := repository.Workflow(stream, trace.UnitSubject("audit")); err != nil || state.Value != intake.UnitReady {
		t.Fatalf("audit after resume merged is %+v: %v", state, err)
	}
}

const followupID = "amendment-1-spec-1"

type finalReportAmendment struct {
	f          *journey.ShedFixture
	stream     config.WorkstreamID
	repository *trace.Repository
	finals     *journey.FinalReviewer
}

func approveAmendmentOverAFinalReport(t *testing.T) finalReportAmendment {
	t.Helper()
	ctx := context.Background()
	f, stream, repository, finals := journey.NewFinalFixture(t, "reviewed")
	_, op := journey.AssembleBoth(t, f, repository, finals, stream,
		map[string]string{"internal/trace/resume.go": "package trace\n"},
		map[string]string{"internal/trace/dedupe.go": "package trace\n"})
	f.FinalTurn(1, 1, func(ctx context.Context, tools *mcp.ClientSession) error {
		_, err := fixturekit.CallTool(ctx, tools, service.FinalReportTool, map[string]any{"summary": "Both hold.", "findings": []any{}})
		return err
	})
	if result, err := finals.Apply(ctx, op); err != nil || result.Outcome != "succeeded" {
		t.Fatalf("final review 1 %+v: %v", result, err)
	}
	if _, reason, err := finals.FinalGate(ctx, stream); err != nil || reason != "" {
		t.Fatalf("final review 1 does not authorise delivery: %q %v", reason, err)
	}
	a, req := approveAmendment(t, f, repository, stream, journey.AmendedSpec, fixturekit.ValidPlan)
	intakefixture.Step(t, a, stream, req, intake.AmendmentResealed)
	intakefixture.Step(t, a, stream, req, intake.AmendmentApplied)
	return finalReportAmendment{f: f, stream: stream, repository: repository, finals: finals}
}

func TestApprovedAmendmentInvalidatesTheFinalReportWithANotice(t *testing.T) {
	t.Parallel()
	a := approveAmendmentOverAFinalReport(t)
	applications, err := amendment.Read(a.repository, a.stream)
	fixturekit.Must(t, err)
	if len(applications) != 1 || applications[0].Applied == nil || applications[0].Applied.FinalReview != 1 {
		t.Fatalf("application %+v", applications)
	}
	outbox, err := a.repository.Outbox(a.stream)
	fixturekit.Must(t, err)
	if !slices.ContainsFunc(outbox, func(e trace.OutboxEntry) bool {
		return e.TransitionID == "amendment-1-applied" && e.Event.Body == "Final review 1 read seal 1, spec revision 1 and plan revision 1, which amendment 1 replaced: its report no longer authorises delivery. Final review runs again once every unit has merged."
	}) {
		t.Fatalf("no notice of the invalidated final report in %+v", outbox)
	}
	if _, reason, err := a.finals.FinalGate(context.Background(), a.stream); err != nil || !strings.HasPrefix(reason, "final review 1 is stale") {
		t.Fatalf("the invalidated final report: %q %v", reason, err)
	}
}

func TestApprovedAmendmentOverAMergedUnitAddsAFollowupInsteadOfReopeningIt(t *testing.T) {
	t.Parallel()
	a := approveAmendmentOverAFinalReport(t)
	added, err := followup.Read(a.repository, a.stream)
	fixturekit.Must(t, err)
	if len(added) != 1 || added[0].Unit.ID != followupID || added[0].Amendment != "1" || added[0].Criterion != "spec#1" || !strings.Contains(added[0].Gap, "after resume merged") || !slices.Equal(added[0].Unit.Footprint, []string{"internal.trace"}) {
		t.Fatalf("follow-ups %+v", added)
	}
	for unit, want := range map[string]string{"resume": intake.UnitMerged, "dedupe": intake.UnitMerged, followupID: intake.UnitReady} {
		if state, err := a.repository.Workflow(a.stream, trace.UnitSubject(unit)); err != nil || state.Value != want {
			t.Fatalf("unit %s is %+v, want %s: %v", unit, state, want, err)
		}
	}
	if got := intakefixture.UnitMoves(t, a.repository, a.stream, "resume"); len(got) != 0 {
		t.Fatalf("the merged unit was reopened: %+v", got)
	}
	applications, err := amendment.Read(a.repository, a.stream)
	fixturekit.Must(t, err)
	if len(applications) != 1 || applications[0].Applied == nil || !slices.Equal(applications[0].Applied.Followups, []string{followupID}) {
		t.Fatalf("application %+v", applications)
	}
	masonBundle, err := journey.NewMasonController(a.f.S, a.repository).Bundle(context.Background(), a.stream, followupID)
	fixturekit.Must(t, err)
	if !strings.Contains(masonBundle.Render(), "follow-up: amendment 1, spec#1\ngap: "+added[0].Gap) {
		t.Fatalf("the follow-up's bundle:\n%s", masonBundle.Render())
	}
}

func TestFinalReviewRunsAgainAgainstTheAmendedDocumentsOnceTheFollowupMerges(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	a := approveAmendmentOverAFinalReport(t)
	f, stream := a.f, a.stream
	fixturekit.Must(t, a.repository.Close())
	passed, prepare := journey.CountingSchedule()
	prepare(&f.Opts)
	f.Start(t)
	fixturekit.AwaitAcknowledged(t, func(t *testing.T) []trace.OperationRecord { return journey.FinalOperations(t, f.Repository(), stream) })
	intakefixture.AwaitFurtherPasses(t, passed, 3)
	f.Stop(t)
	repository, err := trace.Open(f.S.Current().Root, f.S.Current().Project)
	fixturekit.Must(t, err)
	defer repository.Close()
	finals := &journey.FinalReviewer{S: f.S, Repository: repository}
	fixturekit.Must(t, finals.Pass(ctx))
	if ops := journey.FinalOperations(t, repository, stream); len(ops) != 1 {
		t.Fatalf("final review asked for before the follow-up merged: %+v", ops)
	}
	journey.MergeDirectly(t, f, repository, stream, followupID, map[string]string{"internal/trace/checkpoint.go": "package trace\n"})
	fixturekit.Must(t, finals.Pass(ctx))
	ops := journey.FinalOperations(t, repository, stream)
	if len(ops) != 2 {
		t.Fatalf("final review operations %+v", ops)
	}
	in, err := delivery.DecodeFinalReview(ops[1].Operation)
	fixturekit.Must(t, err)
	if in.Review != 2 || in.Seal != 2 || in.Spec != 2 || in.Plan != 1 {
		t.Fatalf("final review 2 reads %+v", in)
	}
}

func TestFollowupsJoinTheBuildingPlan(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f, stream, repository, finals := journey.NewFinalFixture(t, "followed")
	journey.MergeDirectly(t, f, repository, stream, "resume", map[string]string{"internal/trace/resume.go": "package trace\n"})
	a, req := approveAmendment(t, f, repository, stream, journey.AmendedSpec, fixturekit.ValidPlan)
	intakefixture.Step(t, a, stream, req, intake.AmendmentResealed)
	intakefixture.Step(t, a, stream, req, intake.AmendmentApplied)
	id := "amendment-1-spec-1"
	b, found, err := journey.NewMasonController(f.S, repository).Read(stream)
	fixturekit.Must(t, err)
	if !found || !slices.ContainsFunc(b.Plan.Units, func(u plan.Unit) bool { return u.ID == id }) || b.States[trace.UnitSubject(id)].Value != intake.UnitReady {
		t.Fatalf("the building plan %+v, states %+v", b.Plan.Units, b.States)
	}
	journey.MergeDirectly(t, f, repository, stream, "dedupe", map[string]string{"internal/trace/dedupe.go": "package trace\n"})
	fixturekit.Must(t, finals.Pass(ctx))
	if feature, err := repository.Workflow(stream, trace.FeatureSubject); err != nil || feature.Value != intake.BuildingState {
		t.Fatalf("the workstream is %+v with its follow-up unmerged: %v", feature, err)
	}
	journey.MergeDirectly(t, f, repository, stream, id, map[string]string{"internal/trace/checkpoint.go": "package trace\n"})
	fixturekit.Must(t, finals.Pass(ctx))
	if feature, err := repository.Workflow(stream, trace.FeatureSubject); err != nil || feature.Value != intake.AssembledState {
		t.Fatalf("the workstream is %+v with every unit merged: %v", feature, err)
	}
}
