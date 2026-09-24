package service

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/kpenfound/busybees/core/agent"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/kpenfound/osmia/internal/amendment"
	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/plan"
	"github.com/kpenfound/osmia/internal/seal"
	"github.com/kpenfound/osmia/internal/shed"
	"github.com/kpenfound/osmia/internal/trace"
)

var (
	// amendedSpec changes the meaning of criterion 1 of validSpec.
	amendedSpec = strings.Replace(validSpec, "1. An interrupted upload resumes from the last acknowledged chunk.", "1. An interrupted upload resumes from a durable checkpoint.", 1)
	// amendedPlan renames the proof of unit resume in validPlan.
	amendedPlan = strings.Replace(validPlan, "TestResume", "TestResumeAfterRestart", 1)
)

// recordAmendmentSealRevision simulates a later seal revision while leaving
// amendment and owner workflow state untouched.
func recordAmendmentSealRevision(t *testing.T, f *shedFixture, repository *trace.Repository, stream config.WorkstreamID, change func(*seal.Seal)) seal.Seal {
	t.Helper()
	current, doc, found, err := seal.Latest(repository, stream)
	must(t, err)
	if !found {
		t.Fatal("the workstream has no seal")
	}
	before := current
	change(&current)
	content, err := seal.Encode(current)
	must(t, err)
	docs := []trace.Document{}
	if current.Revision.Spec != before.Revision.Spec {
		docs = append(docs, trace.Document{Header: trace.Header{Schema: "osmia.trace.document", Version: trace.Version, ID: plan.SpecDocument, Revision: current.Revision.Spec, Project: repository.Project(), Workstream: stream, At: f.clock.Now(), Actor: architectActor, Cause: "fixture-drift"}, Path: plan.SpecPath, Content: amendedSpec})
	}
	if current.Revision.Plan != before.Revision.Plan {
		docs = append(docs, trace.Document{Header: trace.Header{Schema: "osmia.trace.document", Version: trace.Version, ID: plan.PlanDocument, Revision: current.Revision.Plan, Project: repository.Project(), Workstream: stream, At: f.clock.Now(), Actor: architectActor, Cause: "fixture-drift"}, Path: plan.PlanPath, Content: validPlan})
	}
	doc.Revision++
	doc.At = f.clock.Now()
	doc.Actor = foremanActor
	doc.Cause = "fixture-drift"
	doc.Content = string(content)
	docs = append(docs, doc)
	must(t, repository.RecordDocuments(context.Background(), docs))
	return current
}

// builtForAmendment returns a fixture with a building workstream on seal 1,
// a committee of one and the given shed.max_rounds.
func builtForAmendment(t *testing.T, rounds int) (*shedFixture, config.WorkstreamID) {
	t.Helper()
	f := newDebateFixture(t, 1, rounds)
	f.upstream(t)
	stream, _ := f.built(t)
	return f, stream
}

// presentAmendment plants amendment 1 as the chief of staff presents it: the
// requester of the given role asked for it from its turn filing on unit
// resume, which waits for it, the architect proposed spec and graph, the
// given committee records make its first round, and packet.json revision 1
// is presented. It returns the requester's agent.
func presentAmendment(t *testing.T, f *shedFixture, stream config.WorkstreamID, role, spec, graph string, records ...shed.Record) string {
	t.Helper()
	ctx := context.Background()
	repo := f.repository()
	at := f.clock.Now()
	sealed, sealDoc, found, err := seal.Latest(repo, stream)
	must(t, err)
	if !found {
		t.Fatal("the workstream has no seal")
	}
	requester, actor, stage := masonAgent("resume"), masonActor, UnitImplementing
	if role == reviewerRole {
		requester, actor, stage = reviewerAgent("resume"), reviewerActor, UnitReviewing
	}
	header := func(schema, id string, by trace.Actor, cause string) trace.Header {
		return trace.Header{Schema: schema, Version: trace.Version, ID: id, Revision: 1, Project: f.project, Workstream: stream, Unit: "resume", At: at, Actor: by, Cause: cause}
	}
	must(t, repo.CreateThread(ctx, trace.Agent{Header: header("osmia.trace.agent", requester, actor, "fixture"), Role: role, ThreadID: requester}))
	filing := header("osmia.trace.turn-request", "request_filing", actor, "fixture")
	filing.Depth = 1
	_, err = repo.EnqueueTurn(ctx, trace.TurnRequest{Header: filing, AgentID: requester, ThreadID: requester, TurnID: "filing", Profile: coreadapter.Profile{Name: "default", Backend: "claude", Model: "test"}, SystemPrompt: "requester system", Prompt: "Work on the unit."})
	must(t, err)
	subject := trace.UnitSubject("resume")
	unit, err := repo.Workflow(stream, subject)
	must(t, err)
	_, err = repo.Transact(ctx, trace.Transaction{ExpectedVersion: unit.Version, Transition: trace.Transition{Header: header("osmia.trace.transition", "fixture-"+stage, ownerActor, "fixture"), Subject: subject, From: unit.Value, To: stage, Reason: "fixture"}})
	must(t, err)
	_, err = repo.Transact(ctx, trace.Transaction{ExpectedVersion: unit.Version + 1, Transition: trace.Transition{Header: header("osmia.trace.transition", subject+"_waiting_amendment_1", trace.Actor{Kind: "service", ID: role}, "amendment_1_filed"), Subject: subject, From: stage, To: UnitWaiting, Reason: "unit resume waits for amendment 1"}})
	must(t, err)
	request := trace.Amendment{Header: header("osmia.trace.amendment", "1", trace.Actor{Kind: "agent", ID: requester}, "request_filing"), Requester: trace.Actor{Kind: "agent", ID: requester}, Role: role, Thread: requester, Turn: "filing",
		Citations: []string{"spec#1"}, Change: "Resume from a durable checkpoint", Reason: "Acknowledgements are not durable", Seal: sealed.Seal, SealRevision: sealDoc.Revision, SpecHash: sealed.SpecHash}
	must(t, repo.Append(ctx, request))
	document := func(id, path, content string) trace.Document {
		return trace.Document{Header: trace.Header{Schema: "osmia.trace.document", Version: trace.Version, ID: id, Revision: 1, Project: f.project, Workstream: stream, At: at, Actor: architectActor, Cause: "fixture"}, Path: path, Content: content}
	}
	sealedPlan, err := plan.Parse([]byte(validPlan))
	must(t, err)
	proposedPlan, err := plan.Parse([]byte(graph))
	must(t, err)
	affected, err := json.Marshal(affectedRevision(validSpec, spec, sealedPlan, proposedPlan))
	must(t, err)
	docs := []trace.Document{document("amendment_1_spec", "amendments/1/spec.md", spec), document("amendment_1_plan", "amendments/1/plan.json", graph),
		document("amendment_1_affected", "amendments/1/affected.json", string(affected)),
		document("amendment-1-presented-packet", amendmentPacketPath("1"), `{"round":1}`+"\n")}
	for _, r := range records {
		data, err := shed.Encode(r)
		must(t, err)
		docs = append(docs, document(fmt.Sprintf("amendment_1_round_%d_%s", r.Round, r.Member), amendmentRoundPath("1", r.Round, r.Member), string(data)))
	}
	presented := header("osmia.trace.transition", "amendment-1-presented", shedActor, "fixture")
	presented.Unit = ""
	_, err = repo.RecordDocumentsWith(ctx, docs, trace.Transaction{Transition: trace.Transition{Header: presented, Subject: amendmentSubject("1"), To: amendmentPresented, Reason: "presented"}})
	must(t, err)
	return requester
}

// amendmentObjection is a committee record of round 1 of amendment 1 with one
// objection of the given kind.
func amendmentObjection(kind shed.Kind) shed.Record {
	citation, part := "spec#1", "plan"
	if kind == shed.Charter {
		citation, part = "charter#1", "spec#1"
	}
	member := committeeAgent(1)
	return shed.Record{Version: shed.Version, Round: 1, Member: member, Revision: shed.Pin{Spec: 1, Plan: 1}, Turn: "amend-1-round-1-" + member + "-1",
		Objections: []shed.Objection{{ID: shed.ObjectionID(1, member, 1), Kind: kind, Part: part, Argument: "The proposal needs owner attention.", Citations: []string{citation}}}}
}

// awaitAmendment waits until amendment 1 reaches the wanted state.
func (f *shedFixture) awaitAmendment(t *testing.T, stream config.WorkstreamID, want string) {
	t.Helper()
	deadline := time.Now().Add(demoTimeout)
	for {
		state, err := f.repository().Workflow(stream, amendmentSubject("1"))
		must(t, err)
		if state.Value == want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("amendment 1 stayed %q, want %q", state.Value, want)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// revisions lists the recorded revisions of one document.
func (f *shedFixture) revisions(t *testing.T, stream config.WorkstreamID, id string) []int {
	t.Helper()
	var out []int
	for _, d := range f.documents(t, stream, id) {
		out = append(out, d.Revision)
	}
	return out
}

// ruling returns the turns of the requester's thread that deliver the
// ruling on amendment 1.
func (f *shedFixture) ruling(t *testing.T, stream config.WorkstreamID, requester string) []trace.QueuedTurn {
	t.Helper()
	th, err := f.repository().Thread(stream, requester)
	must(t, err)
	var out []trace.QueuedTurn
	for _, q := range th.Turns {
		if q.Request.TurnID == amendmentRulingTurn("1") {
			out = append(out, q)
		}
	}
	return out
}

// resumed checks that unit resume left its amendment wait for stage through
// the recorded transition, and is there now.
func (f *shedFixture) resumed(t *testing.T, stream config.WorkstreamID, stage string) {
	t.Helper()
	moved := f.transition(t, stream, trace.UnitSubject("resume")+"_resumed_amendment_1")
	if moved.From != UnitWaiting || moved.To != stage || moved.Actor != amendmentActor {
		t.Fatalf("resumption %+v", moved)
	}
	if state, err := f.unitState(stream, "resume"); err != nil || state != stage {
		t.Fatalf("unit resume is %q: %v", state, err)
	}
}

// Approving an amendment that changes a criterion versions the spec, and the
// seal takes the next number and the new spec hash on the same base. The
// feature stays building, the mason that asked receives the ruling in the
// shared envelope on its thread, and its unit resumes implementing. The same
// decision again is answered from the record; another one on the same packet
// is refused.
func TestOwnerApprovesAnAmendmentAndTheSpecIsResealed(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f, stream := builtForAmendment(t, 1)
	defer f.stop(t)
	before, _, _, err := seal.Latest(f.repository(), stream)
	must(t, err)
	requester := presentAmendment(t, f, stream, masonRole, amendedSpec, validPlan)

	for name, tc := range map[string]struct {
		amendment string
		req       AmendmentDecisionRequest
		code      Code
	}{
		"decision":  {"1", AmendmentDecisionRequest{Decision: "accept", Packet: 1}, Validation},
		"packet":    {"1", AmendmentDecisionRequest{Decision: AmendmentApprove}, Validation},
		"stale":     {"1", AmendmentDecisionRequest{Decision: AmendmentApprove, Packet: 2}, Conflict},
		"unknown":   {"9", AmendmentDecisionRequest{Decision: AmendmentApprove, Packet: 1}, NotFound},
		"overrule":  {"1", AmendmentDecisionRequest{Decision: AmendmentOverrule, Packet: 1}, Conflict},
		"round cap": {"1", AmendmentDecisionRequest{Decision: AmendmentRound, Packet: 1}, Conflict},
	} {
		if _, err := f.c.DecideAmendment(ctx, stream, tc.amendment, tc.req); !failed(err, tc.code) {
			t.Fatalf("%s: %v, want %s", name, err, tc.code)
		}
	}
	if got := f.revisions(t, stream, amendmentDecisionID("1")); len(got) != 0 {
		t.Fatalf("refused decisions were recorded: %v", got)
	}

	out, err := f.c.DecideAmendment(ctx, stream, "1", AmendmentDecisionRequest{Decision: AmendmentApprove, Note: "Checkpoints are what we meant.", Packet: 1})
	must(t, err)
	if out.Decision == nil || out.Decision.Decision != AmendmentApprove || out.Decision.Packet != 1 || out.Decision.Spec != 1 || out.Decision.Plan != 1 || out.Decision.SealRevision != 1 || out.Decision.Round != 1 {
		t.Fatalf("decision %+v", out)
	}
	f.awaitAmendment(t, stream, amendmentRuled)

	if got := f.revisions(t, stream, plan.SpecDocument); !slices.Equal(got, []int{1, 2}) {
		t.Fatalf("spec.md revisions %v", got)
	}
	if spec := f.documents(t, stream, plan.SpecDocument)[1]; spec.Content != amendedSpec || spec.Actor != amendmentActor || spec.Cause != amendmentDecided("1", 1) {
		t.Fatalf("spec.md revision 2 %+v", spec)
	}
	if got := f.revisions(t, stream, plan.PlanDocument); !slices.Equal(got, []int{1}) {
		t.Fatalf("plan.json revisions %v", got)
	}
	after, doc, _, err := seal.Latest(f.repository(), stream)
	must(t, err)
	if doc.Revision != 2 || after.Seal != 2 || after.Revision != (shed.Pin{Spec: 2, Plan: 1}) || after.SpecHash != seal.SpecHash(amendedSpec) || after.Base != before.Base || after.Branch != before.Branch || after.Workspace != before.Workspace || !reflect.DeepEqual(after.Footprints, before.Footprints) {
		t.Fatalf("seal after approval %+v, before %+v", after, before)
	}
	if feature, err := f.repository().Workflow(stream, trace.FeatureSubject); err != nil || feature.Value != BuildingState {
		t.Fatalf("feature %q: %v", feature.Value, err)
	}
	turns := f.ruling(t, stream, requester)
	if len(turns) != 1 {
		t.Fatalf("ruling turns %+v", turns)
	}
	req := turns[0].Request
	for _, part := range []string{"Decision: approve", "spec.md revision 2", "seal 2", "call done", "<<< osmia:question | asker (copied by Osmia)", "| Change: Resume from a durable checkpoint", "<<< osmia:owner_response | owner (copied by Osmia)", "| Checkpoints are what we meant."} {
		if !strings.Contains(req.Prompt, part) {
			t.Errorf("ruling misses %q:\n%s", part, req.Prompt)
		}
	}
	if req.SystemPrompt != "requester system" || req.Unit != "resume" || req.Actor != amendmentActor || req.Cause != amendmentDecided("1", 1) {
		t.Fatalf("ruling request %+v", req)
	}
	f.resumed(t, stream, UnitImplementing)
	// The requester's unit addresses the changed criterion: it was held while
	// it waited, and is reworked once the ruling resumed it.
	rework := f.awaitTurn(t, stream, requester, amendmentMasonTurn("resume", "1"))
	if moved := f.transition(t, stream, amendmentUnitID("resume", "1", true)); moved.From != UnitImplementing || moved.To != UnitImplementing || rework.Request.Cause != moved.ID || rework.Sequence <= turns[0].Sequence {
		t.Fatalf("rework %+v after the ruling, turn %+v", moved, rework.Request)
	}

	again, err := f.c.DecideAmendment(ctx, stream, "1", AmendmentDecisionRequest{Decision: AmendmentApprove, Packet: 1})
	must(t, err)
	if again.State != amendmentRuled || !strings.Contains(again.Detail, "already recorded") {
		t.Fatalf("the same decision again %+v", again)
	}
	if _, err := f.c.DecideAmendment(ctx, stream, "1", AmendmentDecisionRequest{Decision: AmendmentReject, Packet: 1}); !failed(err, Conflict) {
		t.Fatalf("another decision on the same packet: %v", err)
	}
	if got := f.revisions(t, stream, amendmentDecisionID("1")); !slices.Equal(got, []int{1}) {
		t.Fatalf("decision revisions %v", got)
	}
	view, err := f.c.Amendment(ctx, stream, "1")
	must(t, err)
	if view.State != amendmentRuled || view.Revision != 1 || view.Round != 1 || view.Decision == nil || view.Decision.Note != "Checkpoints are what we meant." {
		t.Fatalf("view %+v", view)
	}
}

// Approving a plan-only amendment versions the plan alone. The seal keeps its
// number, spec hash and base and names the new plan revision and its
// footprints. The reviewer that asked receives the ruling on the unit's
// reviewer thread and the unit resumes reviewing.
func TestOwnerApprovesAPlanOnlyAmendment(t *testing.T) {
	t.Parallel()
	f, stream := builtForAmendment(t, 1)
	defer f.stop(t)
	before, _, _, err := seal.Latest(f.repository(), stream)
	must(t, err)
	requester := presentAmendment(t, f, stream, reviewerRole, validSpec, amendedPlan)
	if requester != reviewerAgent("resume") {
		t.Fatalf("requester %s", requester)
	}
	_, err = f.c.DecideAmendment(context.Background(), stream, "1", AmendmentDecisionRequest{Decision: AmendmentApprove, Packet: 1})
	must(t, err)
	f.awaitAmendment(t, stream, amendmentRuled)
	if got := f.revisions(t, stream, plan.SpecDocument); !slices.Equal(got, []int{1}) {
		t.Fatalf("spec.md revisions %v", got)
	}
	if got := f.revisions(t, stream, plan.PlanDocument); !slices.Equal(got, []int{1, 2}) {
		t.Fatalf("plan.json revisions %v", got)
	}
	after, doc, _, err := seal.Latest(f.repository(), stream)
	must(t, err)
	if doc.Revision != 2 || after.Seal != before.Seal || after.Revision != (shed.Pin{Spec: 1, Plan: 2}) || after.SpecHash != before.SpecHash || after.Base != before.Base || len(after.Footprints) != 2 {
		t.Fatalf("seal after a plan-only approval %+v, before %+v", after, before)
	}
	turns := f.ruling(t, stream, reviewerAgent("resume"))
	if len(turns) != 1 || !strings.Contains(turns[0].Request.Prompt, "Your review of the same candidate resumes") || !strings.Contains(turns[0].Request.Prompt, "plan.json revision 2") {
		t.Fatalf("reviewer ruling %+v", turns)
	}
	f.resumed(t, stream, UnitReviewing)
	// The plan change keeps the meaning of the unit's criterion: it was held
	// while it waited, and is notified in review once the ruling resumed it.
	f.awaitTransition(t, stream, amendmentUnitID("resume", "1", false), UnitReviewing, UnitReviewing)
	f.resumed(t, stream, UnitReviewing)
}

// Rejecting an amendment leaves the sealed spec, plan and seal in force. The
// requester still receives the ruling and its unit resumes.
func TestOwnerRejectsAnAmendment(t *testing.T) {
	t.Parallel()
	f, stream := builtForAmendment(t, 1)
	defer f.stop(t)
	requester := presentAmendment(t, f, stream, reviewerRole, amendedSpec, validPlan)
	out, err := f.c.DecideAmendment(context.Background(), stream, "1", AmendmentDecisionRequest{Decision: AmendmentReject, Note: "Keep the chunk semantics.", Packet: 1})
	must(t, err)
	if out.State != amendmentRejected {
		t.Fatalf("decision %+v", out)
	}
	f.awaitAmendment(t, stream, amendmentRuled)
	for id, want := range map[string][]int{plan.SpecDocument: {1}, plan.PlanDocument: {1}, seal.DocumentID: {1}} {
		if got := f.revisions(t, stream, id); !slices.Equal(got, want) {
			t.Fatalf("%s revisions %v after a rejection", id, got)
		}
	}
	turns := f.ruling(t, stream, requester)
	if len(turns) != 1 || !strings.Contains(turns[0].Request.Prompt, "Decision: reject") || !strings.Contains(turns[0].Request.Prompt, "stay in force") || !strings.Contains(turns[0].Request.Prompt, "| Keep the chunk semantics.") {
		t.Fatalf("ruling %+v", turns)
	}
	f.resumed(t, stream, UnitReviewing)
}

// A charter veto blocks approval; the owner overrules it to approve, and the
// decision records the objections it set aside.
func TestOwnerOverrulesAVetoToApproveAnAmendment(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f, stream := builtForAmendment(t, 1)
	defer f.stop(t)
	presentAmendment(t, f, stream, masonRole, amendedSpec, validPlan, amendmentObjection(shed.Charter))
	if _, err := f.c.DecideAmendment(ctx, stream, "1", AmendmentDecisionRequest{Decision: AmendmentApprove, Packet: 1}); !failed(err, Conflict) || !strings.Contains(err.Error(), "block amendment 1") {
		t.Fatalf("approving over a veto: %v", err)
	}
	out, err := f.c.DecideAmendment(ctx, stream, "1", AmendmentDecisionRequest{Decision: AmendmentOverrule, Note: "The charter rule does not apply here.", Packet: 1})
	must(t, err)
	if out.State != amendmentApproved || out.Decision == nil || !slices.Equal(out.Decision.Overruled, []string{shed.ObjectionID(1, committeeAgent(1), 1)}) {
		t.Fatalf("overrule %+v", out)
	}
	f.awaitAmendment(t, stream, amendmentRuled)
	if got := f.revisions(t, stream, seal.DocumentID); !slices.Equal(got, []int{1, 2}) {
		t.Fatalf("seal.json revisions %v", got)
	}
	f.resumed(t, stream, UnitImplementing)
}

func TestAmendmentDecisionAcrossSealRevisions(t *testing.T) {
	for _, decision := range []string{AmendmentApprove, AmendmentOverrule} {
		t.Run(decision+" base move", func(t *testing.T) {
			f, stream := builtForAmendment(t, 1)
			defer f.stop(t)
			if decision == AmendmentOverrule {
				presentAmendment(t, f, stream, masonRole, amendedSpec, validPlan, amendmentObjection(shed.Charter))
			} else {
				presentAmendment(t, f, stream, masonRole, amendedSpec, validPlan)
			}
			recordAmendmentSealRevision(t, f, f.repository(), stream, func(s *seal.Seal) { s.Base.Commit = "later-upstream-commit" })
			out, err := f.c.DecideAmendment(context.Background(), stream, "1", AmendmentDecisionRequest{Decision: decision, Packet: 1})
			must(t, err)
			if out.State != amendmentApproved || out.Decision == nil || out.Decision.SealRevision != 2 {
				t.Fatalf("decision %+v", out)
			}
		})
	}
	for name, change := range map[string]func(*seal.Seal){
		"spec": func(s *seal.Seal) { s.Revision.Spec++; s.SpecHash = seal.SpecHash(amendedSpec) },
		"plan": func(s *seal.Seal) { s.Revision.Plan++ },
	} {
		t.Run(name+" changed", func(t *testing.T) {
			f, stream := builtForAmendment(t, 1)
			defer f.stop(t)
			presentAmendment(t, f, stream, masonRole, amendedSpec, validPlan)
			recordAmendmentSealRevision(t, f, f.repository(), stream, change)
			if _, err := f.c.DecideAmendment(context.Background(), stream, "1", AmendmentDecisionRequest{Decision: AmendmentApprove, Packet: 1}); !failed(err, Conflict) || !strings.Contains(err.Error(), "sealed spec or plan changed") {
				t.Fatalf("decision with changed %s: %v", name, err)
			}
			if got := f.revisions(t, stream, amendmentDecisionID("1")); len(got) != 0 {
				t.Fatalf("refused decision recorded: %v", got)
			}
		})
	}
}

// Asking for another round returns the amendment to the bounded shed flow:
// the committee debates it again in round 2 and the architect answers once,
// and the chief of staff presents packet revision 2. A decision on the first
// packet is refused, a third round is past shed.max_rounds, and the owner
// then approves the second packet.
func TestOwnerAsksForAnotherAmendmentRound(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f, stream := builtForAmendment(t, 2)
	defer f.stop(t)
	member := committeeAgent(1)
	f.script(fmt.Sprintf("amend-1-round-2-%s-1", member), nil, nil)
	f.script("amend-1-round-2-reply-1", nil, func(ctx context.Context, _ agent.Request, _ *agent.Turn, tools *mcp.ClientSession) error {
		_, err := callTool(ctx, tools, shed.ReplyTool, map[string]any{"objection": shed.ObjectionID(1, member, 1), "answer": "Checkpoints keep the fit."})
		return err
	})
	presentAmendment(t, f, stream, masonRole, amendedSpec, validPlan, amendmentObjection(shed.Fit))
	out, err := f.c.DecideAmendment(ctx, stream, "1", AmendmentDecisionRequest{Decision: AmendmentRound, Note: "Debate the fit once more.", Packet: 1})
	must(t, err)
	if out.State != "proposed" || out.Round != 2 {
		t.Fatalf("round decision %+v", out)
	}
	f.awaitAmendment(t, stream, amendmentPresented)
	for _, id := range []string{amendmentDecided("1", 1), "amendment-1-debating-2", "amendment-1-heard-2", "amendment-1-answering-2", "amendment-1-answered-2", "amendment-1-presented-2"} {
		f.transition(t, stream, id)
	}
	if runs := f.runs(); !slices.Contains(runs, fmt.Sprintf("amend-1-round-2-%s-1", member)) || !slices.Contains(runs, "amend-1-round-2-reply-1") {
		t.Fatalf("turns %v", runs)
	}
	records, err := amendmentRecords(f.repository(), stream, "1")
	must(t, err)
	if len(records) != 2 || records[1].Round != 2 {
		t.Fatalf("records %+v", records)
	}
	view, err := f.c.Amendment(ctx, stream, "1")
	must(t, err)
	var packet struct {
		Round   int             `json:"round"`
		Dissent []shed.Entry    `json:"dissent"`
		Reply   json.RawMessage `json:"reply"`
	}
	must(t, json.Unmarshal(view.Packet, &packet))
	if view.Revision != 2 || view.Round != 2 || packet.Round != 2 || len(packet.Dissent) != 1 || !strings.Contains(string(packet.Reply), "Checkpoints keep the fit.") {
		t.Fatalf("second presentation %+v, packet %s", view, view.Packet)
	}
	if _, err := f.c.DecideAmendment(ctx, stream, "1", AmendmentDecisionRequest{Decision: AmendmentApprove, Packet: 1}); !failed(err, Conflict) {
		t.Fatalf("approving the first packet: %v", err)
	}
	if _, err := f.c.DecideAmendment(ctx, stream, "1", AmendmentDecisionRequest{Decision: AmendmentRound, Packet: 2}); !failed(err, Conflict) || !strings.Contains(err.Error(), "shed.max_rounds") {
		t.Fatalf("a third round: %v", err)
	}
	approved, err := f.c.DecideAmendment(ctx, stream, "1", AmendmentDecisionRequest{Decision: AmendmentApprove, Packet: 2})
	must(t, err)
	if approved.Decision == nil || approved.Decision.Round != 2 || approved.Decision.Packet != 2 {
		t.Fatalf("approval %+v", approved)
	}
	f.awaitAmendment(t, stream, amendmentRuled)
	if got := f.revisions(t, stream, amendmentDecisionID("1")); !slices.Equal(got, []int{1, 2}) {
		t.Fatalf("decision revisions %v", got)
	}
	if got := f.revisions(t, stream, seal.DocumentID); !slices.Equal(got, []int{1, 2}) {
		t.Fatalf("seal.json revisions %v", got)
	}
}

// A decision recorded before a stop is applied once by the next service: it
// reseals, delivers the ruling and resumes the unit without deciding again.
func TestAmendmentDecisionIsCompletedOnceAfterARestart(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f, stream := builtForAmendment(t, 1)
	requester := presentAmendment(t, f, stream, masonRole, amendedSpec, validPlan)
	f.stop(t)
	repo, err := trace.Open(f.s.cfg.Root, f.s.cfg.Project)
	must(t, err)
	f.s.active = &activeProject{repository: repo}
	out, api := f.s.decideAmendment(ctx, string(stream), "1", AmendmentDecisionRequest{Decision: AmendmentApprove, Packet: 1})
	if api != nil || out.State != amendmentApproved {
		t.Fatalf("decision %+v %v", out, api)
	}
	if got := f.revisions(t, stream, seal.DocumentID); !slices.Equal(got, []int{1}) {
		t.Fatalf("resealed before the restart: %v", got)
	}
	must(t, repo.Close())
	f.start(t)
	defer f.stop(t)
	f.awaitAmendment(t, stream, amendmentRuled)
	for id, want := range map[string][]int{plan.SpecDocument: {1, 2}, seal.DocumentID: {1, 2}, amendmentDecisionID("1"): {1}} {
		if got := f.revisions(t, stream, id); !slices.Equal(got, want) {
			t.Fatalf("%s revisions %v, want %v", id, got, want)
		}
	}
	if turns := f.ruling(t, stream, requester); len(turns) != 1 {
		t.Fatalf("ruling turns %+v", turns)
	}
	f.resumed(t, stream, UnitImplementing)
	a := amendmentDebate{&debate{s: f.s, repository: f.repository()}}
	state, err := f.repository().Workflow(stream, amendmentSubject("1"))
	must(t, err)
	requests, err := trace.Read[trace.Amendment](f.repository(), stream)
	must(t, err)
	// Resealing, applying and ruling again, as a pass that read the earlier
	// state would, records nothing more.
	must(t, a.reseal(ctx, stream, requests[0], trace.WorkflowState{Value: amendmentApproved, Version: state.Version - 3}))
	must(t, a.apply(ctx, stream, requests[0], trace.WorkflowState{Value: amendmentResealed, Version: state.Version - 2}))
	must(t, a.rule(ctx, stream, requests[0], trace.WorkflowState{Value: amendmentApplied, Version: state.Version - 1}))
	for id, want := range map[string][]int{seal.DocumentID: {1, 2}, amendment.DocumentID("1"): {1, 2}} {
		if got := f.revisions(t, stream, id); !slices.Equal(got, want) {
			t.Fatalf("%s revisions after a replay %v, want %v", id, got, want)
		}
	}
	if turns := f.ruling(t, stream, requester); len(turns) != 1 {
		t.Fatalf("ruling turns after a replay %+v", turns)
	}
}

// The chief of staff records the owner's decision given in a message with
// decide_amendment, with the owner as its actor; a turn that answers no
// message from the owner decides nothing.
func TestChiefOfStaffRecordsTheOwnersAmendmentDecision(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f, stream := builtForAmendment(t, 1)
	presentAmendment(t, f, stream, masonRole, amendedSpec, validPlan)
	f.stop(t)
	repo, err := trace.Open(f.s.cfg.Root, f.s.cfg.Project)
	must(t, err)
	defer func() { _ = repo.Close() }()
	f.s.active = &activeProject{repository: repo}
	_, err = repo.EnsureChiefOfStaff(ctx, stream, demoStart, serviceActor)
	must(t, err)
	profile := coreadapter.Profile{Name: "default", Backend: "claude", Model: "test"}
	for _, turn := range []struct {
		id    string
		actor trace.Actor
	}{{"events_1", serviceActor}, {"message_1", ownerActor}} {
		_, err = repo.EnqueueTurn(ctx, trace.TurnRequest{Header: trace.Header{Schema: "osmia.trace.turn-request", Version: trace.Version, ID: "request_" + turn.id, Revision: 1, Project: f.project, Workstream: stream, At: demoStart, Actor: turn.actor, Cause: "fixture"},
			AgentID: trace.ChiefOfStaff, ThreadID: trace.ChiefOfStaff, TurnID: turn.id, Profile: profile, SystemPrompt: "chief", Prompt: "Reject amendment 1."})
		must(t, err)
	}
	controls := &runtimeControls{}
	controls.service.Store(f.s)
	decide := func(turn string) string {
		t.Helper()
		scope := coreadapter.Scope{Project: string(f.project), Workstream: string(stream), Role: trace.ChiefOfStaff, Thread: trace.ChiefOfStaff, Turn: turn}
		out, err := controls.decideAmendment(repo, scope).Handle(ctx, json.RawMessage(`{"amendment":"1","packet":1,"decision":"reject","note":"Keep the chunk semantics."}`))
		must(t, err)
		return string(out)
	}
	_, err = repo.ClaimTurn(ctx, stream, trace.ChiefOfStaff, "token-events", filepath.Join(t.TempDir(), "events"), demoStart)
	must(t, err)
	if out := decide("events_1"); out != `{"recorded":false,"reason":"only the owner decides an amendment; this turn does not answer a message from the owner"}` {
		t.Fatalf("service turn: %s", out)
	}
	// A new session finds the service's turn abandoned and claims the owner's.
	must(t, repo.Close())
	repo, err = trace.Open(f.s.cfg.Root, f.s.cfg.Project)
	must(t, err)
	f.s.active = &activeProject{repository: repo}
	must(t, repo.AbandonTurn(ctx, stream, trace.ChiefOfStaff, "events_1", demoStart))
	_, err = repo.ClaimTurn(ctx, stream, trace.ChiefOfStaff, "token-message", filepath.Join(t.TempDir(), "message"), demoStart)
	must(t, err)
	var result struct {
		Recorded  bool   `json:"recorded"`
		Amendment string `json:"amendment"`
		State     string `json:"state"`
	}
	must(t, json.Unmarshal([]byte(decide("message_1")), &result))
	if !result.Recorded || result.Amendment != "1" || result.State != amendmentRejected {
		t.Fatalf("owner turn: %+v", result)
	}
	docs, err := trace.Read[trace.Document](repo, stream)
	must(t, err)
	i := slices.IndexFunc(docs, func(d trace.Document) bool { return d.ID == amendmentDecisionID("1") })
	if i < 0 || docs[i].Actor != ownerActor || docs[i].Cause != "request_message_1" {
		t.Fatalf("decision record %+v", docs)
	}
}
