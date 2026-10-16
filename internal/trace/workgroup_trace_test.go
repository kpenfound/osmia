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

// crossRepoFixture builds two independent project repositories, the way
// two repos of one multi-repo project each keep their own trace, outside
// any notion of a workgroup. Neither repository is told about the other.
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

// createCrossRepoWorkgroup builds the workgroup-level trace sharing
// crossMember1 (in r1) and crossMember2 (in r2): the way a multi-repo
// workgroup's members must join before either gets a chief of staff.
func createCrossRepoWorkgroup(t *testing.T, root config.Root, r1, r2 *Repository) (*Repository, Workgroup) {
	t.Helper()
	wg, g, err := CreateWorkgroupTrace(context.Background(), root, []CrossRepoMember{{crossMember1, r1}, {crossMember2, r2}}, at, owner, "test-workgroup")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { wg.Close() })
	return wg, g
}

// reopenCrossRepoWorkgroup simulates a restart: it closes every repository
// the test still holds open and reopens all three, rebuilding the
// workgroup trace's member map from the freshly reopened member projects,
// the way the service would after a crash or stop.
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

// claimCrossRepoTurn creates agent's thread on r if needed, enqueues and
// claims a turn naming stream and unit, and returns its scope. project is
// the identity the turn's records are stamped with: the workgroup trace's
// own synthetic identity for the shared chief-of-staff thread, or a
// member's own real project identity for an ordinary worker turn. The
// chief's own thread is never created here: CreateWorkgroupTrace already
// made it, with its own cause, when it created the workgroup's host
// workstream.
func claimCrossRepoTurn(t *testing.T, r *Repository, project config.ProjectID, stream config.WorkstreamID, agent, role, unit, turnID string) coreadapter.Scope {
	t.Helper()
	ctx := context.Background()
	threadID := agent + "_thread"
	if agent == ChiefOfStaff {
		threadID = ChiefOfStaff
	}
	header := func(kind, id string) Header {
		return Header{Schema: "osmia.trace." + kind, Version: Version, ID: id, Revision: 1, Project: project, Workstream: stream, Unit: unit, At: at, Actor: Actor{Kind: "service", ID: "controller"}, Cause: "test", Depth: 1}
	}
	if agent != ChiefOfStaff {
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

// TestCrossRepoWorkgroupSharesOneChiefOfStaffThreadAcrossMembersAndARestart
// covers spec#4's cross-repo extension: questions from roles in both repos
// reach one chief-of-staff thread, hosted on the workgroup's own trace, and
// each answer resumes the right asker in its own repo's trace. It then
// simulates a restart and shows the same holds after reopening.
func TestCrossRepoWorkgroupSharesOneChiefOfStaffThreadAcrossMembersAndARestart(t *testing.T) {
	root, r1, r2, p1, p2 := crossRepoFixture(t)
	wg, g := createCrossRepoWorkgroup(t, root, r1, r2)
	ctx := context.Background()

	host := g.Members[0]
	if host == crossMember1 || host == crossMember2 {
		t.Fatalf("the workgroup's host %s must not be a real member", host)
	}
	for _, m := range []config.WorkstreamID{crossMember1, crossMember2} {
		h, err := wg.ChiefOfStaffHost(m)
		if err != nil || h != host {
			t.Fatalf("host of %s: %s %v, want %s", m, h, err, host)
		}
		members, err := wg.WorkgroupMembers(m)
		if err != nil || len(members) != 3 || members[0] != host || members[1] != crossMember1 || members[2] != crossMember2 {
			t.Fatalf("members of %s: %+v %v", m, members, err)
		}
	}
	if _, err := Get[Agent](wg, host, ChiefOfStaff, 1); err != nil {
		t.Fatalf("shared chief-of-staff identity: %v", err)
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

	chiefScope := claimCrossRepoTurn(t, wg, wg.Project(), host, ChiefOfStaff, ChiefOfStaff, "", "chief1")
	if _, err := wg.AnswerQuestion(ctx, ChiefOfStaff, chiefScope, string(crossMember1)+":"+q1.ID, "member1's answer", []string{"spec#1"}, at); err != nil {
		t.Fatal(err)
	}
	if _, err := wg.AnswerQuestion(ctx, ChiefOfStaff, chiefScope, string(crossMember2)+":"+q2.ID, "member2's answer", []string{"spec#1"}, at); err != nil {
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
		h, err := wg.ChiefOfStaffHost(m)
		if err != nil || h != host {
			t.Fatalf("host of %s after restart: %s %v, want %s", m, h, err, host)
		}
	}
	checkAnswered(r1, crossMember1, q1.ID, "member1's answer")
	checkAnswered(r2, crossMember2, q2.ID, "member2's answer")
}

// setBuilding puts stream's feature state at building and unit's at
// implementing, and records a seal, so a route_amendment on it is
// eligible, as the single-repo questions tests do.
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

// TestCrossRepoRouteAmendmentLandsOnAskingMemberAndResumes covers spec#9's
// routing extension: a qualified non-host plan amendment through
// FileAmendment (what the route_amendment tool calls) lands on the asking
// member's own trace, never the workgroup's host, and the member's parked
// unit resumes once the amendment is decided.
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

	chiefScope := claimCrossRepoTurn(t, wg, wg.Project(), host, ChiefOfStaff, ChiefOfStaff, "", "chief1")
	outcome, err := wg.FileAmendment(ctx, ChiefOfStaff, chiefScope, AmendmentRequest{
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

	// Once the amendment is decided the owner resumes the unit directly,
	// exactly as a single-repo workstream's own unit resumes after its
	// amendment is applied: resuming is ordinary per-workstream workflow
	// mechanics, unaffected by where the amendment itself was filed.
	if _, err := r2.Transact(ctx, Transaction{ExpectedVersion: state.Version, Transition: Transition{Header: Header{Schema: "osmia.trace.transition", Version: Version, ID: "resumed", Revision: 1, Project: crossProject2, Workstream: crossMember2, Unit: "resume", At: at, Actor: owner, Cause: "test"}, Subject: UnitSubject("resume"), From: "waiting", To: "implementing", Reason: "amendment decided"}}); err != nil {
		t.Fatal(err)
	}
	state, err = r2.Workflow(crossMember2, UnitSubject("resume"))
	if err != nil || state.Value != "implementing" {
		t.Fatalf("member2's unit did not resume: %+v: %v", state, err)
	}
}

// TestCrossRepoContractChangeBecomesProjectLevelAmendment covers spec#10's
// cross-repo extension: a question that would change a contract section of
// a member's slice becomes a project-level amendment recorded with the
// workgroup's own trace, never against the workgroup's host workstream or
// the asking member's own trace.
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

	chiefScope := claimCrossRepoTurn(t, wg, wg.Project(), host, ChiefOfStaff, ChiefOfStaff, "", "chief1")
	outcome, err := wg.FileAmendment(ctx, ChiefOfStaff, chiefScope, AmendmentRequest{
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

// TestCrossRepoContractCitingRouteAmendmentReturnsWithinBoundedTime is a
// regression guard for the self-deadlock FileAmendment's second
// contractTouch once caused on this exact path (a chief-of-staff
// route_amendment, on the workgroup trace itself, citing a contract
// section of the asking member's slice): ProjectSpecOf -> CrossRepoWorkgroupOf
// -> Workflow -> scoped(member) re-locked the member's own scoped instance
// FileAmendment already holds. A caller that regresses this hangs forever
// on that mutex, so the call runs on its own goroutine and the test fails
// fast, rather than hanging out to the suite's own timeout, if it ever
// returns.
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
	chiefScope := claimCrossRepoTurn(t, wg, wg.Project(), host, ChiefOfStaff, ChiefOfStaff, "", "chief1")

	type result struct {
		outcome AmendmentOutcome
		err     error
	}
	done := make(chan result, 1)
	t.Cleanup(func() { close(done) })
	go func() {
		outcome, err := wg.FileAmendment(ctx, ChiefOfStaff, chiefScope, AmendmentRequest{
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

// TestCrossRepoConsolidatedEscalationSurvivesRestartAndRelayResumesBothOrigins
// covers spec#17 and spec#18's cross-repo extension: one consolidated
// escalation spanning both repos' questions is still one inbox entry
// numbered from the workgroup's own trace, survives a restart, and the
// relayed ruling resumes all and only the affected origins in both repos
// while an unaffected question on either member stays untouched.
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
	// A second question of the same workstream must come from a separate
	// turn on a thread with no turn still active: a different agent's
	// thread, since the first turn's own thread is still active.
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

	chiefScope := claimCrossRepoTurn(t, wg, wg.Project(), host, ChiefOfStaff, ChiefOfStaff, "", "chief1")
	batch, err := wg.EscalateQuestions(ctx, ChiefOfStaff, chiefScope, EscalationRequest{
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

	// A real restart recovers the chief's own interrupted turn before any
	// new one claims its thread, the way the service's own startup does.
	if err := wg.AbandonTurn(ctx, host, ChiefOfStaff, "chief1", at); err != nil {
		t.Fatal(err)
	}
	chiefScope2 := claimCrossRepoTurn(t, wg, wg.Project(), host, ChiefOfStaff, ChiefOfStaff, "", "chief2")
	relayed, err := wg.RelayRuling(ctx, ChiefOfStaff, chiefScope2, string(crossMember1)+":"+affected1.ID, "Apply the shared retry budget", ScopeLocal, at)
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
