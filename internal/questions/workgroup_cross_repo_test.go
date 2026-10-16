package questions_test

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/questions"
	"github.com/kpenfound/osmia/internal/trace"
)

const (
	crossProject1 config.ProjectID    = "p_00000000000000000000000000000011"
	crossProject2 config.ProjectID    = "p_00000000000000000000000000000012"
	crossMember1  config.WorkstreamID = "w_00000000000000000000000000000011"
	crossMember2  config.WorkstreamID = "w_00000000000000000000000000000012"
)

// crossRepoFixture is two independent project repositories, each the
// trace of one repo of a multi-repo project, with one workstream of its
// own: the way a cross-repo workgroup's members each keep their own trace.
type crossRepoFixture struct {
	r1, r2 *trace.Repository
	root   config.Root
	p1, p2 config.Project
}

func setupCrossRepo(t *testing.T) crossRepoFixture {
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
	p1 := config.Project{ID: crossProject1, Clone: newClone("repo1")}
	p2 := config.Project{ID: crossProject2, Clone: newClone("repo2")}
	r1, err := trace.Create(ctx, root, p1, start, owner)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { r1.Close() })
	r2, err := trace.Create(ctx, root, p2, start, owner)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { r2.Close() })
	if err := r1.CreateWorkstream(ctx, crossMember1, start, owner); err != nil {
		t.Fatal(err)
	}
	if err := r2.CreateWorkstream(ctx, crossMember2, start, owner); err != nil {
		t.Fatal(err)
	}
	return crossRepoFixture{r1: r1, r2: r2, root: root, p1: p1, p2: p2}
}

// claimCrossRepoTurn creates agent's thread if needed, enqueues and claims
// a turn naming stream and unit, and returns its scope. The chief's own
// thread is never created here: CreateWorkgroupTrace already made it when
// it created the workgroup's host workstream.
func claimCrossRepoTurn(t *testing.T, r *trace.Repository, p config.ProjectID, stream config.WorkstreamID, agent, role, unit, turnID string) coreadapter.Scope {
	t.Helper()
	ctx := context.Background()
	threadID := agent + "_thread"
	if agent == trace.ChiefOfStaff {
		threadID = trace.ChiefOfStaff
	}
	header := func(kind, id string) trace.Header {
		return trace.Header{Schema: "osmia.trace." + kind, Version: trace.Version, ID: id, Revision: 1, Project: p, Workstream: stream, Unit: unit, At: start, Actor: trace.Actor{Kind: "service", ID: "controller"}, Cause: "test", Depth: 1}
	}
	if agent != trace.ChiefOfStaff {
		if err := r.CreateThread(ctx, trace.Agent{Header: header("agent", agent), Role: role, ThreadID: threadID}); err != nil {
			t.Fatal(err)
		}
	}
	req := trace.TurnRequest{Header: header("turn-request", "request_"+turnID), AgentID: agent, ThreadID: threadID, TurnID: turnID,
		Profile: coreadapter.Profile{Name: "default", Backend: "fake", Model: "test"}, SystemPrompt: "test", Prompt: "test"}
	if _, err := r.EnqueueTurn(ctx, req); err != nil {
		t.Fatal(err)
	}
	if _, err := r.ClaimTurn(ctx, stream, agent, "token_"+turnID, "/owned/"+turnID, start); err != nil {
		t.Fatal(err)
	}
	return coreadapter.Scope{Project: string(p), Workstream: string(stream), Unit: unit, Thread: threadID, Turn: turnID, Role: role}
}

// TestCrossRepoRouteAmendmentToolLandsOnAskingMemberAndResumes is the
// two-repo interface test spec#9 asks for: against the new authoritative
// workgroup trace, a qualified non-host plan amendment made through the
// chief of staff's actual route_amendment tool lands on the asking
// member's own trace, in its own repo, never the workgroup's host, and the
// asker resumes once the amendment is decided.
func TestCrossRepoRouteAmendmentToolLandsOnAskingMemberAndResumes(t *testing.T) {
	f := setupCrossRepo(t)
	ctx := context.Background()

	wg, g, err := trace.CreateWorkgroupTrace(ctx, f.root, []trace.CrossRepoMember{{Workstream: crossMember1, Project: f.r1}, {Workstream: crossMember2, Project: f.r2}}, start, owner, "test-workgroup")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { wg.Close() })
	host := g.Members[0]

	h := trace.Header{Schema: "osmia.trace.transition", Version: trace.Version, ID: "building", Revision: 1, Project: crossProject2, Workstream: crossMember2, At: start, Actor: owner, Cause: "test"}
	if _, err := f.r2.SetFeatureState(ctx, h, "building", "test"); err != nil {
		t.Fatal(err)
	}
	unit := trace.UnitSubject("resume")
	if _, err := f.r2.Transact(ctx, trace.Transaction{Transition: trace.Transition{Header: trace.Header{Schema: "osmia.trace.transition", Version: trace.Version, ID: "implementing", Revision: 1, Project: crossProject2, Workstream: crossMember2, Unit: "resume", At: start, Actor: owner, Cause: "test"}, Subject: unit, To: "implementing", Reason: "test"}}); err != nil {
		t.Fatal(err)
	}
	document := func(id, path, content string) trace.Document {
		return trace.Document{Header: trace.Header{Schema: "osmia.trace.document", Version: trace.Version, ID: id, Revision: 1, Project: crossProject2, Workstream: crossMember2, At: start, Actor: owner, Cause: "draft"}, Path: path, Content: content}
	}
	seal := trace.Document{Header: trace.Header{Schema: "osmia.trace.document", Version: trace.Version, ID: "seal", Revision: 1, Project: crossProject2, Workstream: crossMember2, At: start, Actor: owner, Cause: "test"}, Path: "seal.json", Content: `{"seal":1,"spec_hash":"sha256:test"}`}
	if err := f.r2.RecordDocuments(ctx, []trace.Document{
		document("spec", "spec.md", "# Retries\n\n## Acceptance criteria\n\n1. Retries use a bounded window.\n"),
		document("plan", "plan.json", `{"version":1,"units":[{"id":"resume","addresses":[{"criterion":"spec#1","proof":{"kind":"test","name":"TestResume"}}],"depends_on":[],"footprint":[]}]}`),
		seal,
	}); err != nil {
		t.Fatal(err)
	}

	workerScope := claimCrossRepoTurn(t, f.r2, crossProject2, crossMember2, "mason1", "mason", "resume", "build1")
	workerTools, err := questions.Tools(f.r2, "mason1", workerScope, func() time.Time { return start })
	if err != nil {
		t.Fatal(err)
	}
	var askTool coreadapter.Tool
	for _, tool := range workerTools {
		if tool.Name == questions.AskTool {
			askTool = tool
		}
	}
	askResult := callTool(t, askTool, `{"question":"May I widen the retry window?"}`)
	if !strings.Contains(askResult, `"question":"1"`) {
		t.Fatalf("ask result: %s", askResult)
	}

	chiefScope := claimCrossRepoTurn(t, wg, wg.Project(), host, trace.ChiefOfStaff, trace.ChiefOfStaff, "", "chief1")
	chiefTools, err := questions.Tools(wg, trace.ChiefOfStaff, chiefScope, func() time.Time { return start })
	if err != nil {
		t.Fatal(err)
	}
	var routeTool coreadapter.Tool
	for _, tool := range chiefTools {
		if tool.Name == questions.RouteAmendmentTool {
			routeTool = tool
		}
	}
	input := `{"question":"` + string(crossMember2) + `:1","citations":["plan#resume"],"change":"Widen the retry window","reason":"The reviewer found a gap"}`
	routeResult := callTool(t, routeTool, input)
	if !strings.Contains(routeResult, `"amendment":"1"`) {
		t.Fatalf("route result: %s", routeResult)
	}

	requests, err := trace.Read[trace.Amendment](f.r2, crossMember2)
	if err != nil || len(requests) != 1 || requests[0].Requester.ID != "mason1" {
		t.Fatalf("amendment landed on member2's own trace: %+v %v", requests, err)
	}
	if requests, err := trace.Read[trace.Amendment](f.r1, crossMember1); err != nil || len(requests) != 0 {
		t.Fatalf("amendment must not land on the other member: %+v %v", requests, err)
	}
	if requests, err := trace.Read[trace.Amendment](wg, host); err != nil || len(requests) != 0 {
		t.Fatalf("amendment must not land on the workgroup's host: %+v %v", requests, err)
	}

	state, err := f.r2.Workflow(crossMember2, unit)
	if err != nil || state.Value != "waiting" {
		t.Fatalf("member2's unit state %+v: %v", state, err)
	}

	// Once the amendment is decided, the owner resumes the unit directly:
	// resuming is ordinary per-workstream workflow mechanics, unaffected
	// by where route_amendment filed the amendment itself.
	if _, err := f.r2.Transact(ctx, trace.Transaction{ExpectedVersion: state.Version, Transition: trace.Transition{Header: trace.Header{Schema: "osmia.trace.transition", Version: trace.Version, ID: "resumed", Revision: 1, Project: crossProject2, Workstream: crossMember2, Unit: "resume", At: start, Actor: owner, Cause: "test"}, Subject: unit, From: "waiting", To: "implementing", Reason: "amendment decided"}}); err != nil {
		t.Fatal(err)
	}
	state, err = f.r2.Workflow(crossMember2, unit)
	if err != nil || state.Value != "implementing" {
		t.Fatalf("member2's unit did not resume: %+v: %v", state, err)
	}
}

func callTool(t *testing.T, tool coreadapter.Tool, input string) string {
	t.Helper()
	if tool.Handle == nil {
		t.Fatalf("tool %s has no handler", tool.Name)
	}
	out, err := tool.Handle(context.Background(), json.RawMessage(input))
	if err != nil {
		t.Fatalf("%s %s: %v", tool.Name, input, err)
	}
	return string(out)
}
