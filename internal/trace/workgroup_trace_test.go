package trace

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
)

const (
	crossProject1 config.ProjectID    = "p_00000000000000000000000000000021"
	crossProject2 config.ProjectID    = "p_00000000000000000000000000000022"
	crossMember1  config.WorkstreamID = "w_00000000000000000000000000000021"
	crossMember2  config.WorkstreamID = "w_00000000000000000000000000000022"
)

func crossRepoFixture(t *testing.T) (root config.Root, r1, r2 *Repository, p1, p2 config.Project) {
	t.Helper()
	ctx := context.Background()
	base := t.TempDir()
	root, err := config.ResolveRoot(filepath.Join(base, "osmia"), "")
	if err != nil {
		t.Fatal(err)
	}
	newClone := func(name string) string {
		clone := filepath.Join(base, name)
		if err := os.Mkdir(clone, 0700); err != nil {
			t.Fatal(err)
		}
		cmd := exec.Command("git", "init", "--quiet", clone)
		cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_TEMPLATE_DIR="}
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git init: %v: %s", err, out)
		}
		return clone
	}
	p1 = config.Project{ID: crossProject1, Clone: newClone("repo1")}
	p2 = config.Project{ID: crossProject2, Clone: newClone("repo2")}
	r1, err = Create(ctx, root, p1, at, owner)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { r1.Close() })
	r2, err = Create(ctx, root, p2, at, owner)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { r2.Close() })
	if err := r1.CreateWorkstream(ctx, crossMember1, at, owner); err != nil {
		t.Fatal(err)
	}
	if err := r2.CreateWorkstream(ctx, crossMember2, at, owner); err != nil {
		t.Fatal(err)
	}
	return root, r1, r2, p1, p2
}

func createCrossRepoWorkgroup(t *testing.T, root config.Root, r1, r2 *Repository) (*Repository, Workgroup) {
	t.Helper()
	wg, g, err := CreateWorkgroupTrace(context.Background(), root, []CrossRepoMember{{crossMember1, r1}, {crossMember2, r2}}, at, owner, "test-workgroup")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { wg.Close() })
	return wg, g
}

func reopenCrossRepoWorkgroup(t *testing.T, root config.Root, p1, p2 config.Project, wg, r1, r2 *Repository, group config.WorkgroupID) (*Repository, *Repository, *Repository) {
	t.Helper()
	if err := wg.Close(); err != nil {
		t.Fatal(err)
	}
	if err := r1.Close(); err != nil {
		t.Fatal(err)
	}
	if err := r2.Close(); err != nil {
		t.Fatal(err)
	}
	nr1, err := Open(root, p1)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { nr1.Close() })
	nr2, err := Open(root, p2)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { nr2.Close() })
	nwg, err := OpenWorkgroupTrace(root, group, []CrossRepoMember{{crossMember1, nr1}, {crossMember2, nr2}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { nwg.Close() })
	return nwg, nr1, nr2
}

func claimCrossRepoTurn(t *testing.T, r *Repository, project config.ProjectID, stream config.WorkstreamID, agent, role, unit, turnID string) coreadapter.Scope {
	t.Helper()
	ctx := context.Background()
	threadID := agent + "_thread"
	if agent == Manager {
		threadID = Manager
	}
	header := func(kind, id string) Header {
		return Header{Schema: "osmia.trace." + kind, Version: Version, ID: id, Revision: 1, Project: project, Workstream: stream, Unit: unit, At: at, Actor: Actor{Kind: "service", ID: "controller"}, Cause: "test", Depth: 1}
	}
	if agent != Manager {
		if err := r.CreateThread(ctx, Agent{Header: header("agent", agent), Role: role, ThreadID: threadID}); err != nil {
			t.Fatal(err)
		}
	}
	req := TurnRequest{Header: header("turn-request", "request_"+turnID), AgentID: agent, ThreadID: threadID, TurnID: turnID,
		Profile: coreadapter.Profile{Name: "default", Backend: "fake", Model: "test"}, SystemPrompt: "test", Prompt: "test"}
	if _, err := r.EnqueueTurn(ctx, req); err != nil {
		t.Fatal(err)
	}
	if _, err := r.ClaimTurn(ctx, stream, agent, "token_"+turnID, "/owned/"+turnID, at); err != nil {
		t.Fatal(err)
	}
	return coreadapter.Scope{Project: string(project), Workstream: string(stream), Unit: unit, Thread: threadID, Turn: turnID, Role: role}
}

func TestCrossRepoWorkgroupSharesOneManagerThreadAcrossMembersAndARestart(t *testing.T) {
	root, r1, r2, p1, p2 := crossRepoFixture(t)
	wg, g := createCrossRepoWorkgroup(t, root, r1, r2)
	ctx := context.Background()

	host := g.Members[0]
	if host == crossMember1 || host == crossMember2 {
		t.Fatalf("the workgroup's host %s must not be a real member", host)
	}
	for _, m := range []config.WorkstreamID{crossMember1, crossMember2} {
		h, err := wg.ManagerHost(m)
		if err != nil || h != host {
			t.Fatalf("host of %s: %s %v, want %s", m, h, err, host)
		}
		members, err := wg.WorkgroupMembers(m)
		if err != nil || len(members) != 3 || members[0] != host || members[1] != crossMember1 || members[2] != crossMember2 {
			t.Fatalf("members of %s: %+v %v", m, members, err)
		}
	}
	if _, err := Get[Agent](wg, host, Manager, 1); err != nil {
		t.Fatalf("shared manager identity: %v", err)
	}

	workerScope1 := claimCrossRepoTurn(t, r1, crossProject1, crossMember1, "mason1", "mason", "", "build1")
	q1, err := r1.Ask(ctx, "mason1", workerScope1, "Member1's own question", at)
	if err != nil {
		t.Fatal(err)
	}
	workerScope2 := claimCrossRepoTurn(t, r2, crossProject2, crossMember2, "mason2", "mason", "", "build2")
	q2, err := r2.Ask(ctx, "mason2", workerScope2, "Member2's own question", at)
	if err != nil {
		t.Fatal(err)
	}

	managerScope := claimCrossRepoTurn(t, wg, wg.Project(), host, Manager, Manager, "", "chief1")
	if _, err := wg.AnswerQuestion(ctx, Manager, managerScope, string(crossMember1)+":"+q1.ID, "member1's answer", []string{"spec#1"}, at); err != nil {
		t.Fatal(err)
	}
	if _, err := wg.AnswerQuestion(ctx, Manager, managerScope, string(crossMember2)+":"+q2.ID, "member2's answer", []string{"spec#1"}, at); err != nil {
		t.Fatal(err)
	}

	checkAnswered := func(r *Repository, stream config.WorkstreamID, id, want string) {
		t.Helper()
		qs, err := r.Questions(stream)
		if err != nil {
			t.Fatal(err)
		}
		for _, q := range qs {
			if q.Asked.ID == id {
				if q.State != QuestionAnswered || q.Ruling == nil || q.Ruling.ReturnedAnswer != want {
					t.Fatalf("question %s of %s: %+v", id, stream, q)
				}
				return
			}
		}
		t.Fatalf("question %s not found on %s", id, stream)
	}
	checkAnswered(r1, crossMember1, q1.ID, "member1's answer")
	checkAnswered(r2, crossMember2, q2.ID, "member2's answer")

	wg, r1, r2 = reopenCrossRepoWorkgroup(t, root, p1, p2, wg, r1, r2, g.ID)
	for _, m := range []config.WorkstreamID{crossMember1, crossMember2} {
		h, err := wg.ManagerHost(m)
		if err != nil || h != host {
			t.Fatalf("host of %s after restart: %s %v, want %s", m, h, err, host)
		}
	}
	checkAnswered(r1, crossMember1, q1.ID, "member1's answer")
	checkAnswered(r2, crossMember2, q2.ID, "member2's answer")
}

func setBuilding(t *testing.T, r *Repository, stream config.WorkstreamID, project config.ProjectID, unit string) {
	t.Helper()
	ctx := context.Background()
	h := Header{Schema: "osmia.trace.transition", Version: Version, ID: "building", Revision: 1, Project: project, Workstream: stream, At: at, Actor: owner, Cause: "test"}
	if _, err := r.SetFeatureState(ctx, h, "building", "test"); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Transact(ctx, Transaction{Transition: Transition{Header: Header{Schema: "osmia.trace.transition", Version: Version, ID: "implementing", Revision: 1, Project: project, Workstream: stream, Unit: unit, At: at, Actor: owner, Cause: "test"}, Subject: UnitSubject(unit), To: "implementing", Reason: "test"}}); err != nil {
		t.Fatal(err)
	}
	seal := Document{Header: Header{Schema: "osmia.trace.document", Version: Version, ID: "seal", Revision: 1, Project: project, Workstream: stream, At: at, Actor: owner, Cause: "test"}, Path: "seal.json", Content: `{"seal":1,"spec_hash":"sha256:test"}`}
	if err := r.RecordDocuments(ctx, []Document{seal}); err != nil {
		t.Fatal(err)
	}
}

func TestCrossRepoRouteAmendmentLandsOnAskingMemberAndResumes(t *testing.T) {
	root, r1, r2, _, _ := crossRepoFixture(t)
	wg, g := createCrossRepoWorkgroup(t, root, r1, r2)
	host := g.Members[0]
	ctx := context.Background()

	setBuilding(t, r2, crossMember2, crossProject2, "resume")
	workerScope := claimCrossRepoTurn(t, r2, crossProject2, crossMember2, "mason1", "mason", "resume", "build1")
	q, err := r2.Ask(ctx, "mason1", workerScope, "May I widen the retry window?", at)
	if err != nil {
		t.Fatal(err)
	}

	managerScope := claimCrossRepoTurn(t, wg, wg.Project(), host, Manager, Manager, "", "chief1")
	outcome, err := wg.FileAmendment(ctx, Manager, managerScope, AmendmentRequest{
		QuestionID: string(crossMember2) + ":" + q.ID, Citations: []string{"plan#resume"}, Change: "Widen the retry window", Reason: "The reviewer found a gap",
		Seal: 1, SealRevision: 1, SpecHash: "sha256:test",
	}, at)
	if err != nil {
		t.Fatal(err)
	}
	if outcome.ProjectLevel || outcome.Amendment.Workstream != crossMember2 || outcome.Amendment.ID != "1" {
		t.Fatalf("amendment outcome: %+v", outcome)
	}

	requests, err := Read[Amendment](r2, crossMember2)
	if err != nil || len(requests) != 1 || requests[0].Requester.ID != "mason1" {
		t.Fatalf("amendment landed on member2's own trace: %+v %v", requests, err)
	}
	if requests, err := Read[Amendment](r1, crossMember1); err != nil || len(requests) != 0 {
		t.Fatalf("amendment must not land on the other member: %+v %v", requests, err)
	}

	state, err := r2.Workflow(crossMember2, UnitSubject("resume"))
	if err != nil || state.Value != "waiting" {
		t.Fatalf("member2's unit state %+v: %v", state, err)
	}

	if _, err := r2.Transact(ctx, Transaction{ExpectedVersion: state.Version, Transition: Transition{Header: Header{Schema: "osmia.trace.transition", Version: Version, ID: "resumed", Revision: 1, Project: crossProject2, Workstream: crossMember2, Unit: "resume", At: at, Actor: owner, Cause: "test"}, Subject: UnitSubject("resume"), From: "waiting", To: "implementing", Reason: "amendment decided"}}); err != nil {
		t.Fatal(err)
	}
	state, err = r2.Workflow(crossMember2, UnitSubject("resume"))
	if err != nil || state.Value != "implementing" {
		t.Fatalf("member2's unit did not resume: %+v: %v", state, err)
	}
}

func TestCrossRepoContractChangeBecomesProjectLevelAmendment(t *testing.T) {
	root, r1, r2, _, _ := crossRepoFixture(t)
	wg, g := createCrossRepoWorkgroup(t, root, r1, r2)
	host := g.Members[0]
	ctx := context.Background()

	spec := ProjectSpec{Behavior: "Cross-repo contract test", Slices: []Slice{
		{Workstream: crossMember1, Spec: "# Member1\n", Plan: "{}", ContractCriteria: []int{5}},
		{Workstream: crossMember2, Spec: "# Member2\n", Plan: "{}"},
	}}
	if err := RecordProjectSpec(ctx, wg, g, spec, at, owner, "test-project-spec"); err != nil {
		t.Fatal(err)
	}

	setBuilding(t, r1, crossMember1, crossProject1, "resume")
	workerScope := claimCrossRepoTurn(t, r1, crossProject1, crossMember1, "mason1", "mason", "resume", "build1")
	q, err := r1.Ask(ctx, "mason1", workerScope, "May I change the retry contract?", at)
	if err != nil {
		t.Fatal(err)
	}

	managerScope := claimCrossRepoTurn(t, wg, wg.Project(), host, Manager, Manager, "", "chief1")
	outcome, err := wg.FileAmendment(ctx, Manager, managerScope, AmendmentRequest{
		QuestionID: string(crossMember1) + ":" + q.ID, Citations: []string{"spec#5"}, Change: "Change the retry contract", Reason: "The contract no longer fits",
		Seal: 1, SealRevision: 1, SpecHash: "sha256:test",
	}, at)
	if err != nil {
		t.Fatal(err)
	}
	if !outcome.ProjectLevel || outcome.ProjectAmendment.Workgroup != g.ID || outcome.ProjectAmendment.Origin != crossMember1 {
		t.Fatalf("outcome: %+v", outcome)
	}

	pas, _, err := ProjectAmendments(wg)
	if err != nil || len(pas) != 1 {
		t.Fatalf("project amendments on the workgroup trace: %+v %v", pas, err)
	}
	if requests, err := Read[Amendment](r1, crossMember1); err != nil || len(requests) != 0 {
		t.Fatalf("a contract change must never be recorded against the asking member: %+v %v", requests, err)
	}
	if hostRequests, err := Read[Amendment](wg, host); err != nil || len(hostRequests) != 0 {
		t.Fatalf("a contract change must never be recorded against the workgroup's host: %+v %v", hostRequests, err)
	}

	qs, err := r1.Questions(crossMember1)
	if err != nil {
		t.Fatal(err)
	}
	if len(qs) != 1 || qs[0].State != QuestionRouted {
		t.Fatalf("member1's question was not routed: %+v", qs)
	}
}

func TestCrossRepoContractCitingRouteAmendmentReturnsWithinBoundedTime(t *testing.T) {
	root, r1, r2, _, _ := crossRepoFixture(t)
	wg, g := createCrossRepoWorkgroup(t, root, r1, r2)
	host := g.Members[0]
	ctx := context.Background()

	spec := ProjectSpec{Behavior: "Cross-repo contract test", Slices: []Slice{
		{Workstream: crossMember1, Spec: "# Member1\n", Plan: "{}", ContractCriteria: []int{5}},
		{Workstream: crossMember2, Spec: "# Member2\n", Plan: "{}"},
	}}
	if err := RecordProjectSpec(ctx, wg, g, spec, at, owner, "test-project-spec"); err != nil {
		t.Fatal(err)
	}

	setBuilding(t, r1, crossMember1, crossProject1, "resume")
	workerScope := claimCrossRepoTurn(t, r1, crossProject1, crossMember1, "mason1", "mason", "resume", "build1")
	q, err := r1.Ask(ctx, "mason1", workerScope, "May I change the retry contract?", at)
	if err != nil {
		t.Fatal(err)
	}
	managerScope := claimCrossRepoTurn(t, wg, wg.Project(), host, Manager, Manager, "", "chief1")

	type result struct {
		outcome AmendmentOutcome
		err     error
	}
	done := make(chan result, 1)
	t.Cleanup(func() { close(done) })
	go func() {
		outcome, err := wg.FileAmendment(ctx, Manager, managerScope, AmendmentRequest{
			QuestionID: string(crossMember1) + ":" + q.ID, Citations: []string{"spec#5"}, Change: "Change the retry contract", Reason: "The contract no longer fits",
			Seal: 1, SealRevision: 1, SpecHash: "sha256:test",
		}, at)
		select {
		case done <- result{outcome, err}:
		default:
		}
	}()
	select {
	case r := <-done:
		if r.err != nil {
			t.Fatal(r.err)
		}
		if !r.outcome.ProjectLevel || r.outcome.ProjectAmendment.Workgroup != g.ID || r.outcome.ProjectAmendment.Origin != crossMember1 {
			t.Fatalf("outcome: %+v", r.outcome)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a contract-citing route_amendment on the workgroup trace did not return within 5s: it is self-deadlocking on the member's own locked scoped instance")
	}
}

func TestCrossRepoConsolidatedEscalationSurvivesRestartAndRelayResumesBothOrigins(t *testing.T) {
	root, r1, r2, p1, p2 := crossRepoFixture(t)
	wg, g := createCrossRepoWorkgroup(t, root, r1, r2)
	host := g.Members[0]
	ctx := context.Background()

	worker1 := claimCrossRepoTurn(t, r1, crossProject1, crossMember1, "mason1", "mason", "", "build1")
	affected1, err := r1.Ask(ctx, "mason1", worker1, "Member1's affected question", at)
	if err != nil {
		t.Fatal(err)
	}
	worker1b := claimCrossRepoTurn(t, r1, crossProject1, crossMember1, "mason1b", "mason", "", "build1b")
	unaffected, err := r1.Ask(ctx, "mason1b", worker1b, "Member1's unrelated question", at)
	if err != nil {
		t.Fatal(err)
	}
	worker2 := claimCrossRepoTurn(t, r2, crossProject2, crossMember2, "mason2", "mason", "", "build2")
	affected2, err := r2.Ask(ctx, "mason2", worker2, "Member2's affected question", at)
	if err != nil {
		t.Fatal(err)
	}

	managerScope := claimCrossRepoTurn(t, wg, wg.Project(), host, Manager, Manager, "", "chief1")
	batch, err := wg.EscalateQuestions(ctx, Manager, managerScope, EscalationRequest{
		Questions:  []string{string(crossMember1) + ":" + affected1.ID, string(crossMember2) + ":" + affected2.ID},
		Rephrasing: "Does the shared retry budget apply to both repos?", Blocked: "both units", Recommendation: "Apply it to both",
	}, at)
	if err != nil {
		t.Fatal(err)
	}

	entries, err := wg.Inbox()
	if err != nil || len(entries) != 1 {
		t.Fatalf("inbox entries: %+v %v", entries, err)
	}
	entry := entries[0]
	if entry.Batch != batch || len(entry.Questions) != 2 {
		t.Fatalf("consolidated entry: %+v", entry)
	}

	unaffectedStates, err := r1.Questions(crossMember1)
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range unaffectedStates {
		if q.Asked.ID == unaffected.ID && q.State != QuestionOpen {
			t.Fatalf("unaffected question changed state: %+v", q)
		}
	}

	wg, r1, r2 = reopenCrossRepoWorkgroup(t, root, p1, p2, wg, r1, r2, g.ID)

	entries, err = wg.Inbox()
	if err != nil || len(entries) != 1 || entries[0].Number != entry.Number {
		t.Fatalf("inbox entry after restart: %+v %v", entries, err)
	}
	if _, err := wg.Rule(ctx, entry.Number, "Yes, apply the shared retry budget to both", owner, at); err != nil {
		t.Fatal(err)
	}

	if err := wg.AbandonTurn(ctx, host, Manager, "chief1", at); err != nil {
		t.Fatal(err)
	}
	managerScope2 := claimCrossRepoTurn(t, wg, wg.Project(), host, Manager, Manager, "", "chief2")
	relayed, err := wg.RelayRuling(ctx, Manager, managerScope2, string(crossMember1)+":"+affected1.ID, "Apply the shared retry budget", ScopeLocal, at)
	if err != nil {
		t.Fatal(err)
	}
	if len(relayed) != 2 {
		t.Fatalf("relayed batch: %+v", relayed)
	}

	checkAnswered := func(r *Repository, stream config.WorkstreamID, id string) {
		t.Helper()
		qs, err := r.Questions(stream)
		if err != nil {
			t.Fatal(err)
		}
		for _, q := range qs {
			if q.Asked.ID == id {
				if q.State != QuestionAnswered || q.Ruling == nil || q.Ruling.ReturnedAnswer != "Apply the shared retry budget" {
					t.Fatalf("question %s of %s: %+v", id, stream, q)
				}
				return
			}
		}
		t.Fatalf("question %s not found on %s", id, stream)
	}
	checkAnswered(r1, crossMember1, affected1.ID)
	checkAnswered(r2, crossMember2, affected2.ID)

	qs, err := r1.Questions(crossMember1)
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range qs {
		if q.Asked.ID == unaffected.ID && (q.State != QuestionOpen || q.Ruling != nil) {
			t.Fatalf("unaffected question was touched by the relay: %+v", q)
		}
	}
}
