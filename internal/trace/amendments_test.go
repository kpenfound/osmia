package trace

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
)

func TestContractRoutingUsesApprovedDeltaUnderMemberLock(t *testing.T) {
	for _, member := range []config.WorkstreamID{streamID, secondStream} {
		t.Run(string(member), func(t *testing.T) {
			r, _, _ := create(t)
			ctx := context.Background()
			if err := r.CreateWorkstream(ctx, secondStream, at, owner); err != nil {
				t.Fatal(err)
			}
			g, err := CreateWorkgroup(ctx, r, []config.WorkstreamID{streamID, secondStream}, at, owner, "test-workgroup")
			if err != nil {
				t.Fatal(err)
			}
			before := ProjectSpec{Behavior: "Shared contract", Slices: []Slice{
				{Workstream: streamID, ContractCriteria: []int{1}},
				{Workstream: secondStream, ContractCriteria: []int{1}},
			}}
			if err := RecordProjectSpec(ctx, r, g, before, at, owner, "ratified"); err != nil {
				t.Fatal(err)
			}
			after := ProjectSpec{Behavior: "Amended shared contract", Slices: []Slice{
				{Workstream: streamID, ContractCriteria: []int{2}},
				{Workstream: secondStream, ContractCriteria: []int{2}},
			}}
			if err := RecordWorkgroupRevision(ctx, r, g, "delta", after, at, owner); err != nil {
				t.Fatal(err)
			}
			building(t, r, member)
			seal(t, r, member, 1)
			implementing(t, r, member, "contract")
			scope := askerOn(t, r, member, "agent_mason", "mason", "build", "contract")
			q, err := r.Ask(ctx, "agent_mason", scope, "May the amended contract change?", at.Add(time.Second))
			if err != nil {
				t.Fatal(err)
			}
			chief := chiefThread(t, r, "chief_turn")
			outcome, err := r.FileAmendment(ctx, "chief", chief, AmendmentRequest{
				QuestionID: string(member) + ":" + q.ID, Citations: []string{"spec#2"},
				Change: "Extend the amended interface", Reason: "New evidence",
				Seal: 1, SealRevision: 1, SpecHash: "deadbeef",
			}, at.Add(2*time.Second))
			if err != nil || !outcome.ProjectLevel || outcome.ProjectAmendment.Origin != member {
				t.Fatalf("routing against approved delta: %+v err=%v", outcome, err)
			}
			local, err := Read[Amendment](r, member)
			if err != nil || len(local) != 0 {
				t.Fatalf("amended contract escaped into a local amendment: %+v err=%v", local, err)
			}
		})
	}
}

func askerOn(t *testing.T, r *Repository, stream config.WorkstreamID, agent, role, turn, unit string) coreadapter.Scope {
	t.Helper()
	ctx := context.Background()
	h := Header{Schema: "osmia.trace.agent", Version: Version, ID: agent, Revision: 1, Project: projectID, Workstream: stream, At: at, Actor: Actor{Kind: "service", ID: "controller"}, Cause: "owner-message-1", Depth: 2}
	if err := r.CreateThread(ctx, Agent{Header: h, Role: role, ThreadID: agent + "_thread"}); err != nil {
		t.Fatal(err)
	}
	reqHeader := h
	reqHeader.Schema, reqHeader.ID, reqHeader.Unit = "osmia.trace.turn-request", "request_"+turn, unit
	req := TurnRequest{Header: reqHeader, AgentID: agent, ThreadID: agent + "_thread", TurnID: turn,
		Profile: coreadapter.Profile{Name: "default", Backend: "fake", Model: "test"}, Prompt: "Build the unit"}
	if _, err := r.EnqueueTurn(ctx, req); err != nil {
		t.Fatal(err)
	}
	if _, err := r.ClaimTurn(ctx, stream, agent, "token_"+turn, "/owned/"+turn, at); err != nil {
		t.Fatal(err)
	}
	return coreadapter.Scope{Project: string(projectID), Workstream: string(stream), Unit: unit, Thread: agent + "_thread", Turn: turn, Role: role}
}

func implementing(t *testing.T, r *Repository, stream config.WorkstreamID, unit string) {
	t.Helper()
	h := Header{Schema: "osmia.trace.transition", Version: Version, ID: "test-implementing-" + string(stream) + "-" + unit, Revision: 1, Project: r.project, Workstream: stream, Unit: unit, At: at, Actor: owner, Cause: "test-setup", Depth: 1}
	tx := Transaction{Transition: Transition{Header: h, Subject: UnitSubject(unit), From: "", To: "implementing", Reason: "test setup"}}
	if _, err := r.Transact(context.Background(), tx); err != nil {
		t.Fatal(err)
	}
}

func seal(t *testing.T, r *Repository, stream config.WorkstreamID, revision int) {
	t.Helper()
	ctx := context.Background()
	h := Header{Schema: "osmia.trace.document", Version: Version, ID: "seal", Revision: revision, Project: r.project, Workstream: stream, At: at, Actor: owner, Cause: "test-seal"}
	if err := r.Append(ctx, Document{Header: h, Path: "seal.json", Content: "{}"}); err != nil {
		t.Fatal(err)
	}
}

func building(t *testing.T, r *Repository, stream config.WorkstreamID) {
	t.Helper()
	h := Header{Schema: "osmia.trace.transition", Version: Version, ID: "test-building-" + string(stream), Revision: 1, Project: r.project, Workstream: stream, At: at, Actor: owner, Cause: "test-setup", Depth: 1}
	if _, err := r.MoveFeatureState(context.Background(), h, "", "building", "test setup"); err != nil {
		t.Fatal(err)
	}
}

type contractCitationFixture struct {
	r     *Repository
	wg    Workgroup
	scope coreadapter.Scope
	chief coreadapter.Scope
}

func newContractCitationFixture(t *testing.T) *contractCitationFixture {
	t.Helper()
	r, _, _ := create(t)
	ctx := context.Background()
	if err := r.CreateWorkstream(ctx, secondStream, at, owner); err != nil {
		t.Fatal(err)
	}
	wg, err := CreateWorkgroup(ctx, r, []config.WorkstreamID{streamID, secondStream}, at, owner, "test-workgroup")
	if err != nil {
		t.Fatal(err)
	}
	spec := ProjectSpec{
		Behavior:  "Two repos exchange a fixed interface.",
		Contracts: []Contract{{Name: "upload-api", Text: "POST /v1/upload returns {id}."}},
		Slices: []Slice{
			{Workstream: streamID, Spec: "spec.md", Plan: "plan.json", ContractCriteria: []int{1}},
			{Workstream: secondStream, Spec: "spec.md", Plan: "plan.json", ContractCriteria: []int{1}},
		},
	}
	if err := RecordProjectSpec(ctx, r, wg, spec, at, owner, "owner-ratified"); err != nil {
		t.Fatal(err)
	}
	building(t, r, streamID)
	seal(t, r, streamID, 1)
	building(t, r, secondStream)
	seal(t, r, secondStream, 1)

	scope := askerOn(t, r, streamID, "agent_mason", "mason", "build", "unit-a")
	implementing(t, r, streamID, "unit-a")
	return &contractCitationFixture{r: r, wg: wg, scope: scope, chief: chiefThread(t, r, "chief_turn")}
}

func contractCitation(question, change, reason string) AmendmentRequest {
	return AmendmentRequest{QuestionID: question, Citations: []string{"spec#1"}, Change: change, Reason: reason, Seal: 1, SealRevision: 1, SpecHash: "deadbeef"}
}

func (f *contractCitationFixture) routeFirstMembersCitation(t *testing.T) (Question, AmendmentOutcome) {
	t.Helper()
	ctx := context.Background()
	q, err := f.r.Ask(ctx, "agent_mason", f.scope, "The caller also needs the upload's status; may the contract change?", at.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	outcome, err := f.r.FileAmendment(ctx, "chief", f.chief, contractCitation(q.ID, "Return {id, status}.", "The caller also needs the upload's status."), at.Add(2*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	return q, outcome
}

func (f *contractCitationFixture) routeSecondMembersCitation(t *testing.T) AmendmentOutcome {
	t.Helper()
	ctx := context.Background()
	askerB := askerOn(t, f.r, secondStream, "agent_b", "mason", "build_b", "unit-b")
	implementing(t, f.r, secondStream, "unit-b")
	qb, err := f.r.Ask(ctx, "agent_b", askerB, "The contract's id-only response is a problem here too; may it change?", at.Add(3*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	outcome, err := f.r.FileAmendment(ctx, "chief", f.chief, contractCitation(string(secondStream)+":"+qb.ID, "Return {id, status}.", "The second repo also needs the status."), at.Add(4*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	return outcome
}

func TestAMasonsOwnContractCitationIsRefusedAndRecordsNothing(t *testing.T) {
	f := newContractCitationFixture(t)
	_, err := f.r.FileAmendment(context.Background(), "agent_mason", f.scope, contractCitation("", "Return {id, status}.", "The caller also needs the upload's status."), at)
	if got := refusal(t, err); !strings.Contains(got, "spec#1") || !strings.Contains(got, "read-only inside a workstream") {
		t.Fatalf("mason's own contract citation: %s", got)
	}
	if amendments, err := Read[Amendment](f.r, streamID); err != nil || len(amendments) != 0 {
		t.Fatalf("a refused contract citation was recorded as an amendment: %+v err=%v", amendments, err)
	}
	if filed, _, err := ProjectAmendments(f.r); err != nil || len(filed) != 0 {
		t.Fatalf("a refused contract citation filed a project amendment: %+v err=%v", filed, err)
	}
}

func TestFileAmendmentRoutesAContractCitationToAProjectLevelAmendment(t *testing.T) {
	f := newContractCitationFixture(t)
	_, outcome := f.routeFirstMembersCitation(t)
	if !outcome.ProjectLevel || outcome.Amendment.ID != "" || outcome.ProjectAmendment.ID == "" {
		t.Fatalf("contract citation outcome: %+v", outcome)
	}
	if outcome.ProjectAmendment.Workgroup != f.wg.ID || outcome.ProjectAmendment.Origin != streamID {
		t.Fatalf("project amendment identity: %+v, want workgroup %s origin %s", outcome.ProjectAmendment, f.wg.ID, streamID)
	}
	if amendments, err := Read[Amendment](f.r, streamID); err != nil || len(amendments) != 0 {
		t.Fatalf("a routed contract citation was recorded as an ordinary amendment: %+v err=%v", amendments, err)
	}
	filed, _, err := ProjectAmendments(f.r)
	if err != nil || len(filed) != 1 || filed[0].ID != outcome.ProjectAmendment.ID {
		t.Fatalf("project amendments: %+v err=%v", filed, err)
	}
	if qs, err := f.r.Questions(streamID); err != nil || len(qs) != 1 || qs[0].State != QuestionRouted {
		t.Fatalf("question after routing: %+v err=%v", qs, err)
	}
}

func TestRepeatingARoutedContractCitationReusesItsProjectAmendment(t *testing.T) {
	f := newContractCitationFixture(t)
	q, outcome := f.routeFirstMembersCitation(t)
	again, err := f.r.FileAmendment(context.Background(), "chief", f.chief, contractCitation(q.ID, "Return {id, status}.", "The caller also needs the upload's status."), at.Add(2*time.Second))
	if err != nil || again.ProjectAmendment.ID != outcome.ProjectAmendment.ID {
		t.Fatalf("repeated request: %+v err=%v", again, err)
	}
	if filed, _, err := ProjectAmendments(f.r); err != nil || len(filed) != 1 {
		t.Fatalf("project amendments after a repeated request: %+v err=%v", filed, err)
	}
}

func TestANonHostMembersContractCitationIsItsOwnProjectAmendment(t *testing.T) {
	f := newContractCitationFixture(t)
	_, outcome := f.routeFirstMembersCitation(t)
	outcomeB := f.routeSecondMembersCitation(t)
	if !outcomeB.ProjectLevel || outcomeB.Amendment.ID != "" || outcomeB.ProjectAmendment.ID == "" {
		t.Fatalf("second-member contract citation outcome: %+v", outcomeB)
	}
	if outcomeB.ProjectAmendment.Workgroup != f.wg.ID || outcomeB.ProjectAmendment.Origin != secondStream {
		t.Fatalf("second-member project amendment identity: %+v, want workgroup %s origin %s", outcomeB.ProjectAmendment, f.wg.ID, secondStream)
	}
	if outcomeB.ProjectAmendment.ID == outcome.ProjectAmendment.ID {
		t.Fatalf("second member's project amendment reused the first member's: %+v", outcomeB.ProjectAmendment)
	}
	if amendments, err := Read[Amendment](f.r, secondStream); err != nil || len(amendments) != 0 {
		t.Fatalf("a routed contract citation from the non-host member was recorded as an ordinary amendment: %+v err=%v", amendments, err)
	}
	if amendments, err := Read[Amendment](f.r, streamID); err != nil || len(amendments) != 0 {
		t.Fatalf("a routed contract citation from the non-host member leaked onto the chief's host workstream: %+v err=%v", amendments, err)
	}
}

func TestANonContractCitationFromANonHostMemberStaysAnOrdinaryAmendmentOfItsStream(t *testing.T) {
	f := newContractCitationFixture(t)
	ctx := context.Background()
	askerB2 := askerOn(t, f.r, secondStream, "agent_b2", "mason", "build_b2", "unit-b2")
	implementing(t, f.r, secondStream, "unit-b2")
	qb2, err := f.r.Ask(ctx, "agent_b2", askerB2, "The upload size cap also needs documenting; may the plan note it?", at.Add(4200*time.Millisecond))
	if err != nil {
		t.Fatal(err)
	}
	outcomeB2, err := f.r.FileAmendment(ctx, "chief", f.chief, AmendmentRequest{
		QuestionID: string(secondStream) + ":" + qb2.ID, Citations: []string{"spec#2"}, Change: "Document the upload size cap.", Reason: "Operators asked where the limit is documented.",
		Seal: 1, SealRevision: 1, SpecHash: "deadbeef",
	}, at.Add(4400*time.Millisecond))
	if err != nil {
		t.Fatal(err)
	}
	if outcomeB2.ProjectLevel || outcomeB2.Amendment.ID == "" {
		t.Fatalf("non-contract qualified-routing outcome: %+v", outcomeB2)
	}
	if outcomeB2.Amendment.Workstream != secondStream {
		t.Fatalf("plan amendment from a qualified route landed on %s, want %s", outcomeB2.Amendment.Workstream, secondStream)
	}
	if amendments, err := Read[Amendment](f.r, secondStream); err != nil || len(amendments) != 1 || amendments[0].ID != outcomeB2.Amendment.ID {
		t.Fatalf("plan amendment after qualified routing: %+v err=%v", amendments, err)
	}
	if amendments, err := Read[Amendment](f.r, streamID); err != nil || len(amendments) != 0 {
		t.Fatalf("a routed plan amendment from the non-host member leaked onto the chief's host workstream: %+v err=%v", amendments, err)
	}
	if state, err := f.r.Workflow(secondStream, UnitSubject("unit-b2")); err != nil || state.Value != "waiting" {
		t.Fatalf("unit-b2 state after a routed plan amendment: %+v err=%v", state, err)
	}
}

func TestANonContractCitationFromTheHostMemberDoesNotChangeProjectAmendments(t *testing.T) {
	f := newContractCitationFixture(t)
	ctx := context.Background()
	f.routeFirstMembersCitation(t)
	f.routeSecondMembersCitation(t)
	scope2 := askerOn(t, f.r, streamID, "agent_mason2", "mason", "build2", "unit-d")
	implementing(t, f.r, streamID, "unit-d")
	outcome2, err := f.r.FileAmendment(ctx, "agent_mason2", scope2, AmendmentRequest{
		Citations: []string{"spec#2"}, Change: "Also validate the file size.", Reason: "Large uploads should be refused up front.",
		Seal: 1, SealRevision: 1, SpecHash: "deadbeef",
	}, at.Add(5*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if outcome2.ProjectLevel || outcome2.Amendment.ID == "" {
		t.Fatalf("non-contract citation outcome: %+v", outcome2)
	}
	amendments, err := Read[Amendment](f.r, streamID)
	if err != nil || len(amendments) != 1 || amendments[0].ID != outcome2.Amendment.ID {
		t.Fatalf("ordinary amendment after a non-contract citation: %+v err=%v", amendments, err)
	}
	if filed, _, err := ProjectAmendments(f.r); err != nil || len(filed) != 2 {
		t.Fatalf("a non-contract citation changed the project amendments: %+v err=%v", filed, err)
	}
}
