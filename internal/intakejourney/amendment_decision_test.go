package intakejourney

import (
	context "context"
	json "encoding/json"
	fmt "fmt"
	"github.com/kpenfound/osmia/internal/api"
	"github.com/kpenfound/osmia/internal/execution"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	"github.com/kpenfound/osmia/internal/servicetest/journey"
	filepath "path/filepath"
	reflect "reflect"
	slices "slices"
	strings "strings"
	testing "testing"

	agent "github.com/kpenfound/agentcore/agent"
	amendment "github.com/kpenfound/osmia/internal/amendment"
	config "github.com/kpenfound/osmia/internal/config"
	coreadapter "github.com/kpenfound/osmia/internal/coreadapter"
	intake "github.com/kpenfound/osmia/internal/intake"
	plan "github.com/kpenfound/osmia/internal/plan"
	seal "github.com/kpenfound/osmia/internal/seal"
	intakefixture "github.com/kpenfound/osmia/internal/servicetest/intakefixture"
	shed "github.com/kpenfound/osmia/internal/shed"
	trace "github.com/kpenfound/osmia/internal/trace"
	mcp "github.com/modelcontextprotocol/go-sdk/mcp"
)

var (
	amendedPlan = strings.Replace(fixturekit.ValidPlan, "TestResume", "TestResumeAfterRestart", 1)
)

func recordAmendmentSealRevision(t *testing.T, f *journey.ShedFixture, repository *trace.Repository, stream config.WorkstreamID, change func(*seal.Seal)) seal.Seal {
	t.Helper()
	current, doc, found, err := seal.Latest(repository, stream)
	fixturekit.Must(t, err)
	if !found {
		t.Fatal("the workstream has no seal")
	}
	before := current
	change(&current)
	content, err := seal.Encode(current)
	fixturekit.Must(t, err)
	docs := []trace.Document{}
	if current.Revision.Spec != before.Revision.Spec {
		docs = append(docs, trace.Document{Header: trace.Header{Schema: "osmia.trace.document", Version: trace.Version, ID: plan.SpecDocument, Revision: current.Revision.Spec, Project: repository.Project(), Workstream: stream, At: f.Clock.Now(), Actor: intake.ArchitectActor, Cause: "fixture-drift"}, Path: plan.SpecPath, Content: journey.AmendedSpec})
	}
	if current.Revision.Plan != before.Revision.Plan {
		docs = append(docs, trace.Document{Header: trace.Header{Schema: "osmia.trace.document", Version: trace.Version, ID: plan.PlanDocument, Revision: current.Revision.Plan, Project: repository.Project(), Workstream: stream, At: f.Clock.Now(), Actor: intake.ArchitectActor, Cause: "fixture-drift"}, Path: plan.PlanPath, Content: fixturekit.ValidPlan})
	}
	doc.Revision++
	doc.At = f.Clock.Now()
	doc.Actor = execution.ForemanActor
	doc.Cause = "fixture-drift"
	doc.Content = string(content)
	docs = append(docs, doc)
	fixturekit.Must(t, repository.RecordDocuments(context.Background(), docs))
	return current
}

func TestRefusedAmendmentDecisionsRecordNothing(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f, stream := journey.BuiltForAmendment(t, 1)
	defer f.Stop(t)
	journey.PresentAmendment(t, f, stream, intake.MasonRole, journey.AmendedSpec, fixturekit.ValidPlan)
	for name, tc := range map[string]struct {
		amendment string
		req       api.AmendmentDecisionRequest
		code      api.Code
	}{
		"decision":  {"1", api.AmendmentDecisionRequest{Decision: "accept", Packet: 1}, api.Validation},
		"packet":    {"1", api.AmendmentDecisionRequest{Decision: intake.AmendmentApprove}, api.Validation},
		"stale":     {"1", api.AmendmentDecisionRequest{Decision: intake.AmendmentApprove, Packet: 2}, api.Conflict},
		"unknown":   {"9", api.AmendmentDecisionRequest{Decision: intake.AmendmentApprove, Packet: 1}, api.NotFound},
		"overrule":  {"1", api.AmendmentDecisionRequest{Decision: intake.AmendmentOverrule, Packet: 1}, api.Conflict},
		"round cap": {"1", api.AmendmentDecisionRequest{Decision: intake.AmendmentRound, Packet: 1}, api.Conflict},
	} {
		if _, err := f.C.DecideAmendment(ctx, stream, tc.amendment, tc.req); !intakefixture.Failed(err, tc.code) {
			t.Fatalf("%s: %v, want %s", name, err, tc.code)
		}
	}
	if got := f.Revisions(t, stream, intake.AmendmentDecisionID("1")); len(got) != 0 {
		t.Fatalf("refused decisions were recorded: %v", got)
	}
}

type approvedAmendment struct {
	f         *journey.ShedFixture
	stream    config.WorkstreamID
	requester string
	before    seal.Seal
}

func ownerApprovesAmendment(t *testing.T) *approvedAmendment {
	t.Helper()
	f, stream := journey.BuiltForAmendment(t, 1)
	t.Cleanup(func() { f.Stop(t) })
	before, _, _, err := seal.Latest(f.Repository(), stream)
	fixturekit.Must(t, err)
	requester := journey.PresentAmendment(t, f, stream, intake.MasonRole, journey.AmendedSpec, fixturekit.ValidPlan)
	out, err := f.C.DecideAmendment(context.Background(), stream, "1", api.AmendmentDecisionRequest{Decision: intake.AmendmentApprove, Note: "Checkpoints are what we meant.", Packet: 1})
	fixturekit.Must(t, err)
	if out.Decision == nil || out.Decision.Decision != intake.AmendmentApprove || out.Decision.Packet != 1 || out.Decision.Spec != 1 || out.Decision.Plan != 1 || out.Decision.SealRevision != 1 || out.Decision.Round != 1 {
		t.Fatalf("decision %+v", out)
	}
	f.AwaitAmendment(t, stream, intake.AmendmentRuled)
	return &approvedAmendment{f: f, stream: stream, requester: requester, before: before}
}

func TestApprovedAmendmentVersionsTheSpecAndResealsOnTheSameBase(t *testing.T) {
	t.Parallel()
	a := ownerApprovesAmendment(t)
	f, stream, before := a.f, a.stream, a.before
	if got := f.Revisions(t, stream, plan.SpecDocument); !slices.Equal(got, []int{1, 2}) {
		t.Fatalf("spec.md revisions %v", got)
	}
	if spec := f.Documents(t, stream, plan.SpecDocument)[1]; spec.Content != journey.AmendedSpec || spec.Actor != intake.AmendmentActor || spec.Cause != intake.AmendmentDecided("1", 1) {
		t.Fatalf("spec.md revision 2 %+v", spec)
	}
	if got := f.Revisions(t, stream, plan.PlanDocument); !slices.Equal(got, []int{1}) {
		t.Fatalf("plan.json revisions %v", got)
	}
	after, doc, _, err := seal.Latest(f.Repository(), stream)
	fixturekit.Must(t, err)
	if doc.Revision != 2 || after.Seal != 2 || after.Revision != (shed.Pin{Spec: 2, Plan: 1}) || after.SpecHash != seal.SpecHash(journey.AmendedSpec) || after.Base != before.Base || after.Branch != before.Branch || after.Workspace != before.Workspace || !reflect.DeepEqual(after.Footprints, before.Footprints) {
		t.Fatalf("seal after approval %+v, before %+v", after, before)
	}
	if feature, err := f.Repository().Workflow(stream, trace.FeatureSubject); err != nil || feature.Value != intake.BuildingState {
		t.Fatalf("feature %q: %v", feature.Value, err)
	}
}

func TestApprovedAmendmentDeliversTheRulingAndReworksTheRequestersUnit(t *testing.T) {
	t.Parallel()
	a := ownerApprovesAmendment(t)
	f, stream, requester := a.f, a.stream, a.requester
	turns := f.Ruling(t, stream, requester)
	if len(turns) != 1 {
		t.Fatalf("ruling turns %+v", turns)
	}
	req := turns[0].Request
	for _, part := range []string{"Decision: approve", "spec.md revision 2", "seal 2", "call done", "<<< osmia:question | asker (copied by Osmia)", "| Change: Resume from a durable checkpoint", "<<< osmia:owner_response | owner (copied by Osmia)", "| Checkpoints are what we meant."} {
		if !strings.Contains(req.Prompt, part) {
			t.Errorf("ruling misses %q:\n%s", part, req.Prompt)
		}
	}
	if req.SystemPrompt != "requester system" || req.Unit != "resume" || req.Actor != intake.AmendmentActor || req.Cause != intake.AmendmentDecided("1", 1) {
		t.Fatalf("ruling request %+v", req)
	}
	f.Resumed(t, stream, intake.UnitImplementing)
	rework := f.AwaitTurn(t, stream, requester, intake.AmendmentMasonTurn("resume", "1"))
	if moved := f.Transition(t, stream, intake.AmendmentUnitID("resume", "1", true)); moved.From != intake.UnitImplementing || moved.To != intake.UnitImplementing || rework.Request.Cause != moved.ID || rework.Sequence <= turns[0].Sequence {
		t.Fatalf("rework %+v after the ruling, turn %+v", moved, rework.Request)
	}
}

func TestAnAmendmentDecisionIsAnsweredFromItsRecordTheSecondTime(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	a := ownerApprovesAmendment(t)
	f, stream := a.f, a.stream
	again, err := f.C.DecideAmendment(ctx, stream, "1", api.AmendmentDecisionRequest{Decision: intake.AmendmentApprove, Packet: 1})
	fixturekit.Must(t, err)
	if again.State != intake.AmendmentRuled || !strings.Contains(again.Detail, "already recorded") {
		t.Fatalf("the same decision again %+v", again)
	}
	if _, err := f.C.DecideAmendment(ctx, stream, "1", api.AmendmentDecisionRequest{Decision: intake.AmendmentReject, Packet: 1}); !intakefixture.Failed(err, api.Conflict) {
		t.Fatalf("another decision on the same packet: %v", err)
	}
	if got := f.Revisions(t, stream, intake.AmendmentDecisionID("1")); !slices.Equal(got, []int{1}) {
		t.Fatalf("decision revisions %v", got)
	}
	view, err := f.C.Amendment(ctx, stream, "1")
	fixturekit.Must(t, err)
	if view.State != intake.AmendmentRuled || view.Revision != 1 || view.Round != 1 || view.Decision == nil || view.Decision.Note != "Checkpoints are what we meant." {
		t.Fatalf("view %+v", view)
	}
}

func TestOwnerApprovesAPlanOnlyAmendment(t *testing.T) {
	t.Parallel()
	f, stream := journey.BuiltForAmendment(t, 1)
	defer f.Stop(t)
	before, _, _, err := seal.Latest(f.Repository(), stream)
	fixturekit.Must(t, err)
	requester := journey.PresentAmendment(t, f, stream, intake.ReviewerRole, fixturekit.ValidSpec, amendedPlan)
	if requester != intake.ReviewerAgent("resume") {
		t.Fatalf("requester %s", requester)
	}
	_, err = f.C.DecideAmendment(context.Background(), stream, "1", api.AmendmentDecisionRequest{Decision: intake.AmendmentApprove, Packet: 1})
	fixturekit.Must(t, err)
	f.AwaitAmendment(t, stream, intake.AmendmentRuled)
	if got := f.Revisions(t, stream, plan.SpecDocument); !slices.Equal(got, []int{1}) {
		t.Fatalf("spec.md revisions %v", got)
	}
	if got := f.Revisions(t, stream, plan.PlanDocument); !slices.Equal(got, []int{1, 2}) {
		t.Fatalf("plan.json revisions %v", got)
	}
	after, doc, _, err := seal.Latest(f.Repository(), stream)
	fixturekit.Must(t, err)
	if doc.Revision != 2 || after.Seal != before.Seal || after.Revision != (shed.Pin{Spec: 1, Plan: 2}) || after.SpecHash != before.SpecHash || after.Base != before.Base || len(after.Footprints) != 2 {
		t.Fatalf("seal after a plan-only approval %+v, before %+v", after, before)
	}
	if turns := f.Ruling(t, stream, intake.ReviewerAgent("resume")); len(turns) != 0 {
		t.Fatalf("reviewer resumed before a new candidate: %+v", turns)
	}

	f.AwaitTransition(t, stream, intake.AmendmentUnitID("resume", "1", true), intake.UnitReviewing, intake.UnitImplementing)
	if st, err := f.Repository().Workflow(stream, trace.UnitSubject("resume")); err != nil || st.Value != intake.UnitImplementing {
		t.Fatalf("changed acceptance did not return to implementation: %+v %v", st, err)
	}
}

func TestOwnerRejectsAnAmendment(t *testing.T) {
	t.Parallel()
	f, stream := journey.BuiltForAmendment(t, 1)
	defer f.Stop(t)
	requester := journey.PresentAmendment(t, f, stream, intake.MasonRole, journey.AmendedSpec, fixturekit.ValidPlan)
	out, err := f.C.DecideAmendment(context.Background(), stream, "1", api.AmendmentDecisionRequest{Decision: intake.AmendmentReject, Note: "Keep the chunk semantics.", Packet: 1})
	fixturekit.Must(t, err)
	if out.State != intake.AmendmentRejected {
		t.Fatalf("decision %+v", out)
	}
	f.AwaitAmendment(t, stream, intake.AmendmentRuled)
	for id, want := range map[string][]int{plan.SpecDocument: {1}, plan.PlanDocument: {1}, seal.DocumentID: {1}} {
		if got := f.Revisions(t, stream, id); !slices.Equal(got, want) {
			t.Fatalf("%s revisions %v after a rejection", id, got)
		}
	}
	turns := f.Ruling(t, stream, requester)
	if len(turns) != 1 || !strings.Contains(turns[0].Request.Prompt, "Decision: reject") || !strings.Contains(turns[0].Request.Prompt, "stay in force") || !strings.Contains(turns[0].Request.Prompt, "| Keep the chunk semantics.") {
		t.Fatalf("ruling %+v", turns)
	}
	f.Resumed(t, stream, intake.UnitImplementing)
	f.Stop(t)
	f.Start(t)
	f.Resumed(t, stream, intake.UnitImplementing)
	if turns := f.Ruling(t, stream, requester); len(turns) != 1 {
		t.Fatalf("requester ruling after restart %+v", turns)
	}
}

func TestOwnerOverrulesAVetoToApproveAnAmendment(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f, stream := journey.BuiltForAmendment(t, 1)
	defer f.Stop(t)
	journey.PresentAmendment(t, f, stream, intake.MasonRole, journey.AmendedSpec, fixturekit.ValidPlan, intakefixture.AmendmentObjection(shed.Charter))
	if _, err := f.C.DecideAmendment(ctx, stream, "1", api.AmendmentDecisionRequest{Decision: intake.AmendmentApprove, Packet: 1}); !intakefixture.Failed(err, api.Conflict) || !strings.Contains(err.Error(), "block amendment 1") {
		t.Fatalf("approving over a veto: %v", err)
	}
	out, err := f.C.DecideAmendment(ctx, stream, "1", api.AmendmentDecisionRequest{Decision: intake.AmendmentOverrule, Note: "The charter rule does not apply here.", Packet: 1})
	fixturekit.Must(t, err)
	if out.State != intake.AmendmentApproved || out.Decision == nil || !slices.Equal(out.Decision.Overruled, []string{shed.ObjectionID(1, intake.CommitteeAgent(1), 1)}) {
		t.Fatalf("overrule %+v", out)
	}
	f.AwaitAmendment(t, stream, intake.AmendmentRuled)
	if got := f.Revisions(t, stream, seal.DocumentID); !slices.Equal(got, []int{1, 2}) {
		t.Fatalf("seal.json revisions %v", got)
	}
	f.Resumed(t, stream, intake.UnitImplementing)
}

func TestAmendmentDecisionAcrossSealRevisions(t *testing.T) {
	t.Parallel()
	t.Run("approve base move", func(t *testing.T) {
		f, stream := journey.BuiltForAmendment(t, 1)
		defer f.Stop(t)
		journey.PresentAmendment(t, f, stream, intake.MasonRole, journey.AmendedSpec, fixturekit.ValidPlan)
		beforeCard := f.InboxEntries(t, stream, api.InboxAmendment)
		recordAmendmentSealRevision(t, f, f.Repository(), stream, func(s *seal.Seal) { s.Base.Commit = "later-upstream-commit" })
		afterCard := f.InboxEntries(t, stream, api.InboxAmendment)
		if len(beforeCard) != 1 || len(afterCard) != 1 || !slices.Equal(beforeCard[0].Options, afterCard[0].Options) || beforeCard[0].Recommendation != afterCard[0].Recommendation {
			t.Fatalf("base move changed card: before %+v after %+v", beforeCard, afterCard)
		}

		out, err := f.C.DecideAmendment(context.Background(), stream, "1", api.AmendmentDecisionRequest{Decision: intake.AmendmentApprove, Packet: 1})
		fixturekit.Must(t, err)
		if out.State != intake.AmendmentApproved || out.Decision == nil || out.Decision.SealRevision != 2 {
			t.Fatalf("decision %+v", out)
		}
	})
	for name, change := range map[string]func(*seal.Seal){
		"spec": func(s *seal.Seal) { s.Revision.Spec++; s.SpecHash = seal.SpecHash(journey.AmendedSpec) },
		"plan": func(s *seal.Seal) { s.Revision.Plan++ },
	} {
		t.Run(name+" changed", func(t *testing.T) {
			f, stream := journey.BuiltForAmendment(t, 1)
			defer f.Stop(t)
			journey.PresentAmendment(t, f, stream, intake.MasonRole, journey.AmendedSpec, fixturekit.ValidPlan)
			recordAmendmentSealRevision(t, f, f.Repository(), stream, change)
			if _, err := f.C.DecideAmendment(context.Background(), stream, "1", api.AmendmentDecisionRequest{Decision: intake.AmendmentApprove, Packet: 1}); !intakefixture.Failed(err, api.Conflict) {
				t.Fatalf("decision with changed %s: %v", name, err)
			}
			f.AwaitAmendment(t, stream, intake.AmendmentRuled)
			decisions, err := intake.AmendmentDecisions(f.Repository(), stream, "1")
			fixturekit.Must(t, err)
			if len(decisions) != 1 || decisions[0].Actor != intake.AmendmentActor.ID || decisions[0].Decision != intake.AmendmentReject {
				t.Fatalf("retirement decisions %+v", decisions)
			}
		})
	}
}

func TestOwnerAsksForAnotherAmendmentRound(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f, stream := journey.BuiltForAmendment(t, 2)
	defer f.Stop(t)
	member := intake.CommitteeAgent(1)
	f.Script(fmt.Sprintf("amend-1-round-2-%s-1", member), nil, nil)
	f.Script("amend-1-round-2-reply-1", nil, func(ctx context.Context, _ agent.Request, _ *agent.Turn, tools *mcp.ClientSession) error {
		_, err := fixturekit.CallTool(ctx, tools, shed.ReplyTool, map[string]any{"objection": shed.ObjectionID(1, member, 1), "answer": "Checkpoints keep the fit."})
		return err
	})
	journey.PresentAmendment(t, f, stream, intake.MasonRole, journey.AmendedSpec, fixturekit.ValidPlan, intakefixture.AmendmentObjection(shed.Fit))
	if entries := f.InboxEntries(t, stream, api.InboxAmendment); len(entries) != 1 || entries[0].Amendment != "1" || entries[0].Revision != 1 || entries[0].Unit != "resume" ||
		!slices.Equal(entries[0].Options, []string{intake.AmendmentApprove, intake.AmendmentOverrule, intake.AmendmentReject, intake.AmendmentRound}) ||
		!reflect.DeepEqual(entries[0].Answer, api.InboxAnswer{Method: "POST", Path: "/v1/amendment/" + string(stream) + "/1", Body: map[string]any{"packet": 1.0}}) {
		t.Fatalf("inbox entries of the presented amendment %+v", entries)
	}
	out, err := f.C.DecideAmendment(ctx, stream, "1", api.AmendmentDecisionRequest{Decision: intake.AmendmentRound, Note: "Debate the fit once more.", Packet: 1})
	fixturekit.Must(t, err)

	for _, e := range f.InboxEntries(t, stream, api.InboxAmendment) {
		if e.Revision == 1 {
			t.Fatalf("inbox entry of the decided revision %+v", e)
		}
	}
	if out.State != "proposed" || out.Round != 2 {
		t.Fatalf("round decision %+v", out)
	}
	f.AwaitAmendment(t, stream, intake.AmendmentPresented)
	for _, id := range []string{intake.AmendmentDecided("1", 1), "amendment-1-debating-2", "amendment-1-heard-2", "amendment-1-answering-2", "amendment-1-answered-2", "amendment-1-presented-2"} {
		f.Transition(t, stream, id)
	}
	if runs := f.Runs(); !slices.Contains(runs, fmt.Sprintf("amend-1-round-2-%s-1", member)) || !slices.Contains(runs, "amend-1-round-2-reply-1") {
		t.Fatalf("turns %v", runs)
	}
	records, err := intake.AmendmentRecords(f.Repository(), stream, "1")
	fixturekit.Must(t, err)
	if len(records) != 2 || records[1].Round != 2 {
		t.Fatalf("records %+v", records)
	}
	view, err := f.C.Amendment(ctx, stream, "1")
	fixturekit.Must(t, err)
	var packet struct {
		Round   int             `json:"round"`
		Dissent []shed.Entry    `json:"dissent"`
		Reply   json.RawMessage `json:"reply"`
	}
	fixturekit.Must(t, json.Unmarshal(view.Packet, &packet))
	if view.Revision != 2 || view.Round != 2 || packet.Round != 2 || len(packet.Dissent) != 1 || !strings.Contains(string(packet.Reply), "Checkpoints keep the fit.") {
		t.Fatalf("second presentation %+v, packet %s", view, view.Packet)
	}
	if _, err := f.C.DecideAmendment(ctx, stream, "1", api.AmendmentDecisionRequest{Decision: intake.AmendmentApprove, Packet: 1}); !intakefixture.Failed(err, api.Conflict) {
		t.Fatalf("approving the first packet: %v", err)
	}

	if entries := f.InboxEntries(t, stream, api.InboxAmendment); len(entries) != 1 || entries[0].Revision != 2 || !slices.Equal(entries[0].Options, []string{intake.AmendmentApprove, intake.AmendmentOverrule, intake.AmendmentReject}) {
		t.Fatalf("inbox entries of the second presentation %+v", entries)
	}
	if _, err := f.C.DecideAmendment(ctx, stream, "1", api.AmendmentDecisionRequest{Decision: intake.AmendmentRound, Packet: 2}); !intakefixture.Failed(err, api.Conflict) || !strings.Contains(err.Error(), "shed.max_rounds") {
		t.Fatalf("a third round: %v", err)
	}
	approved, err := f.C.DecideAmendment(ctx, stream, "1", api.AmendmentDecisionRequest{Decision: intake.AmendmentApprove, Packet: 2})
	fixturekit.Must(t, err)
	if approved.Decision == nil || approved.Decision.Round != 2 || approved.Decision.Packet != 2 {
		t.Fatalf("approval %+v", approved)
	}
	if entries := f.InboxEntries(t, stream, api.InboxAmendment); len(entries) != 0 {
		t.Fatalf("inbox entries after the approval %+v", entries)
	}
	f.AwaitAmendment(t, stream, intake.AmendmentRuled)
	if got := f.Revisions(t, stream, intake.AmendmentDecisionID("1")); !slices.Equal(got, []int{1, 2}) {
		t.Fatalf("decision revisions %v", got)
	}
	if got := f.Revisions(t, stream, seal.DocumentID); !slices.Equal(got, []int{1, 2}) {
		t.Fatalf("seal.json revisions %v", got)
	}
}

func TestAmendmentDecisionIsCompletedOnceAfterARestart(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f, stream := journey.BuiltForAmendment(t, 1)
	requester := journey.PresentAmendment(t, f, stream, intake.MasonRole, journey.AmendedSpec, fixturekit.ValidPlan)
	f.Stop(t)
	repo, err := trace.Open(f.S.Current().Root, f.S.Current().Project)
	fixturekit.Must(t, err)
	f.S.SetSole(&journey.ActiveProject{Repository: repo})
	out, apiErr := f.S.DecideAmendment(ctx, string(stream), "1", api.AmendmentDecisionRequest{Decision: intake.AmendmentApprove, Packet: 1})
	if apiErr != nil || out.State != intake.AmendmentApproved {
		t.Fatalf("decision %+v %v", out, apiErr)
	}
	if got := f.Revisions(t, stream, seal.DocumentID); !slices.Equal(got, []int{1}) {
		t.Fatalf("resealed before the restart: %v", got)
	}
	fixturekit.Must(t, repo.Close())
	f.Start(t)
	defer f.Stop(t)
	f.AwaitAmendment(t, stream, intake.AmendmentRuled)
	for id, want := range map[string][]int{plan.SpecDocument: {1, 2}, seal.DocumentID: {1, 2}, intake.AmendmentDecisionID("1"): {1}} {
		if got := f.Revisions(t, stream, id); !slices.Equal(got, want) {
			t.Fatalf("%s revisions %v, want %v", id, got, want)
		}
	}
	if turns := f.Ruling(t, stream, requester); len(turns) != 1 {
		t.Fatalf("ruling turns %+v", turns)
	}
	f.Resumed(t, stream, intake.UnitImplementing)
	a := intake.AmendmentDebate{Debate: &intake.Debate{S: f.S.Intake().DebateDependencies(), Repository: f.Repository()}}
	state, err := f.Repository().Workflow(stream, intake.AmendmentSubject("1"))
	fixturekit.Must(t, err)
	requests, err := trace.Read[trace.Amendment](f.Repository(), stream)
	fixturekit.Must(t, err)

	fixturekit.Must(t, a.Reseal(ctx, stream, requests[0], trace.WorkflowState{Value: intake.AmendmentApproved, Version: state.Version - 3}))
	fixturekit.Must(t, a.ApplyAmendment(ctx, stream, requests[0], trace.WorkflowState{Value: intake.AmendmentResealed, Version: state.Version - 2}))
	fixturekit.Must(t, a.Rule(ctx, stream, requests[0], trace.WorkflowState{Value: intake.AmendmentApplied, Version: state.Version - 1}))
	for id, want := range map[string][]int{seal.DocumentID: {1, 2}, amendment.DocumentID("1"): {1, 2}} {
		if got := f.Revisions(t, stream, id); !slices.Equal(got, want) {
			t.Fatalf("%s revisions after a replay %v, want %v", id, got, want)
		}
	}
	if turns := f.Ruling(t, stream, requester); len(turns) != 1 {
		t.Fatalf("ruling turns after a replay %+v", turns)
	}
}

func TestChiefOfStaffRecordsTheOwnersAmendmentDecision(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f, stream := journey.BuiltForAmendment(t, 1)
	journey.PresentAmendment(t, f, stream, intake.MasonRole, journey.AmendedSpec, fixturekit.ValidPlan)
	f.Stop(t)
	repo, err := trace.Open(f.S.Current().Root, f.S.Current().Project)
	fixturekit.Must(t, err)
	defer func() { _ = repo.Close() }()
	f.S.SetSole(&journey.ActiveProject{Repository: repo})
	_, err = repo.EnsureChiefOfStaff(ctx, stream, fixturekit.DemoStart, intake.ServiceActor)
	fixturekit.Must(t, err)
	profile := coreadapter.Profile{Name: "default", Backend: "claude", Model: "test"}
	for _, turn := range []struct {
		id    string
		actor trace.Actor
	}{{"events_1", intake.ServiceActor}, {"message_1", intake.OwnerActor}} {
		_, err = repo.EnqueueTurn(ctx, trace.TurnRequest{Header: trace.Header{Schema: "osmia.trace.turn-request", Version: trace.Version, ID: "request_" + turn.id, Revision: 1, Project: f.Project, Workstream: stream, At: fixturekit.DemoStart, Actor: turn.actor, Cause: "fixture"},
			AgentID: trace.ChiefOfStaff, ThreadID: trace.ChiefOfStaff, TurnID: turn.id, Profile: profile, SystemPrompt: "chief", Prompt: "Reject amendment 1."})
		fixturekit.Must(t, err)
	}
	controls := &journey.RuntimeControls{}
	controls.Service.Store(f.S)
	decide := func(turn string) string {
		t.Helper()
		scope := coreadapter.Scope{Project: string(f.Project), Workstream: string(stream), Role: trace.ChiefOfStaff, Thread: trace.ChiefOfStaff, Turn: turn}
		out, err := controls.DecideAmendment(repo, scope).Handle(ctx, json.RawMessage(`{"amendment":"1","packet":1,"decision":"reject","note":"Keep the chunk semantics.","owner_decided":true}`))
		fixturekit.Must(t, err)
		return string(out)
	}
	_, err = repo.ClaimTurn(ctx, stream, trace.ChiefOfStaff, "token-events", filepath.Join(t.TempDir(), "events"), fixturekit.DemoStart)
	fixturekit.Must(t, err)
	if out := decide("events_1"); out != `{"recorded":false,"reason":"this turn does not answer an owner message"}` {
		t.Fatalf("service turn: %s", out)
	}

	fixturekit.Must(t, repo.Close())
	repo, err = trace.Open(f.S.Current().Root, f.S.Current().Project)
	fixturekit.Must(t, err)
	f.S.SetSole(&journey.ActiveProject{Repository: repo})
	fixturekit.Must(t, repo.AbandonTurn(ctx, stream, trace.ChiefOfStaff, "events_1", fixturekit.DemoStart))
	_, err = repo.ClaimTurn(ctx, stream, trace.ChiefOfStaff, "token-message", filepath.Join(t.TempDir(), "message"), fixturekit.DemoStart)
	fixturekit.Must(t, err)
	var result struct {
		Recorded  bool   `json:"recorded"`
		Amendment string `json:"amendment"`
		State     string `json:"state"`
	}
	fixturekit.Must(t, json.Unmarshal([]byte(decide("message_1")), &result))
	if !result.Recorded || result.Amendment != "1" || result.State != intake.AmendmentRejected {
		t.Fatalf("owner turn: %+v", result)
	}
	docs, err := trace.Read[trace.Document](repo, stream)
	fixturekit.Must(t, err)
	i := slices.IndexFunc(docs, func(d trace.Document) bool { return d.ID == intake.AmendmentDecisionID("1") })
	if i < 0 || docs[i].Actor != intake.OwnerActor || docs[i].Cause != "request_message_1" {
		t.Fatalf("decision record %+v", docs)
	}
}
