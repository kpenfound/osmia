package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kpenfound/agentcore/agent"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/kpenfound/osmia/internal/bundle"
	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/seal"
	"github.com/kpenfound/osmia/internal/trace"
)

const (
	crossSpec1 = "# Upload service\n\nThe upload service accepts chunks and acknowledges them.\n\n## Acceptance criteria\n\n1. A chunk is acknowledged once stored.\n2. The acknowledgment names the chunk's offset.\n"
	crossPlan1 = `{"version": 2, "units": [
  {"id": "accept", "task": "Accept and store chunks.", "acceptance": ["TestAccept passes"], "criteria": ["spec#1", "spec#2"], "depends_on": [], "footprint": ["internal.trace"]}
]}
`
	crossSpec2 = "# Upload client\n\nThe upload client resumes from the last acknowledged offset.\n\n## Acceptance criteria\n\n1. The client resumes from the last acknowledged offset.\n"
	crossPlan2 = `{"version": 2, "units": [
  {"id": "resume", "task": "Resume from the last acknowledged offset.", "acceptance": ["TestResume passes"], "criteria": ["spec#1"], "depends_on": [], "footprint": ["internal.trace"]}
]}
`
)

// crossRepoRatifyFixture is two active projects in one service, each with
// its own clone and charter, ready for the architect to draft a cross-repo
// workgroup's project spec and for one owner ratify to seal it.
type crossRepoRatifyFixture struct {
	*architectFixture
	project2         config.ProjectID
	stream1, stream2 config.WorkstreamID
}

func TestGroupApprovalRequiresLiveMemberTraceAndHonorsShutdown(t *testing.T) {
	t.Parallel()
	s, _ := start(t, fixture(t))
	decision := trace.GroupApproval{Members: []trace.GroupMemberApproval{{Project: project, Workstream: stream}}}
	if err := s.prepareGroupApproval(context.Background(), decision); !errors.Is(err, errNoTrace) {
		t.Fatalf("live missing trace was not reported: %v", err)
	}
	must(t, s.Close())
	if err := s.prepareGroupApproval(s.lifetime, decision); !errors.Is(err, context.Canceled) {
		t.Fatalf("shutdown resolution was not cancellation: %v", err)
	}
	if err := s.prepareGroupApproval(context.Background(), decision); !errors.Is(err, errNoTrace) {
		t.Fatalf("uncanceled missing trace was hidden: %v", err)
	}
}

func newCrossRepoRatifyFixture(t *testing.T, configure ...func(*Options)) *crossRepoRatifyFixture {
	t.Helper()
	return crossRepoFixture(t, true, configure...)
}

func crossRepoFixture(t *testing.T, skipDebate bool, configure ...func(*Options)) *crossRepoRatifyFixture {
	t.Helper()
	f := newArchitectFixture(t, configure...)
	home := filepath.Dir(f.clone)
	clone2 := filepath.Join(home, "clone2")
	must(t, os.Mkdir(clone2, 0700))
	demoGit(t, home, "init", "--quiet", clone2)
	must(t, os.MkdirAll(filepath.Join(clone2, "internal", "trace"), 0700))
	must(t, os.WriteFile(filepath.Join(clone2, "internal", "trace", "git.go"), []byte("package trace\n"), 0600))
	demoGit(t, home, "-C", clone2, "add", "internal")
	demoGit(t, home, "-C", clone2, "-c", "user.name=Owner", "-c", "user.email=owner@example.invalid", "commit", "-qm", "base")
	added, err := f.c.AddProject(context.Background(), ProjectAddRequest{Name: "other", Upstream: "owner/other", Fork: "owner/other-fork", Clone: clone2})
	must(t, err)
	writeCharter(t, f.c, added.Project.ID, "1. Keep changes small.\n")
	cf := &crossRepoRatifyFixture{architectFixture: f, project2: added.Project.ID}
	handIn := func(project config.ProjectID, key string) config.WorkstreamID {
		content := "Build " + key
		out, err := cf.c.HandIn(context.Background(), HandInRequest{Project: project, Key: key, Stdin: &content, SkipDebate: skipDebate})
		must(t, err)
		return out.Workstream
	}
	cf.stream1 = handIn(cf.project, "upload-server")
	cf.stream2 = handIn(cf.project2, "upload-client")
	cf.scriptDrafts(t)
	return cf
}

// scriptDrafts installs the fake architect's behaviour for both members'
// own draft turns, routed by the workspace path's workstream, and for the
// workgroup's project-spec draft turn.
func (f *crossRepoRatifyFixture) scriptDrafts(t *testing.T) {
	t.Helper()
	// verdict calls the tool and requires the verdict it returns.
	verdict := func(ctx context.Context, tools *mcp.ClientSession, name string, args map[string]any, want string) error {
		got, err := callTool(ctx, tools, name, args)
		if err != nil {
			return err
		}
		var status draftStatus
		if err := json.Unmarshal([]byte(got), &status); err != nil {
			return err
		}
		if status.Verdict != want {
			return fmt.Errorf("%s returned %s, want verdict %s", name, got, want)
		}
		return nil
	}
	deliver := func(ctx context.Context, tools *mcp.ClientSession, path, content, want string) error {
		return verdict(ctx, tools, DraftTool, map[string]any{"path": path, "content": content}, want)
	}
	f.engine.mu.Lock()
	f.engine.turns["draft-1-1"] = func(ctx context.Context, req agent.Request, _ *agent.Turn, tools *mcp.ClientSession) (*agent.Result, error) {
		dir := req.Workspace.Directory()
		var spec, plan string
		switch {
		case strings.Contains(dir, string(f.stream1)):
			spec, plan = crossSpec1, crossPlan1
		case strings.Contains(dir, string(f.stream2)):
			spec, plan = crossSpec2, crossPlan2
		default:
			return nil, fmt.Errorf("unexpected draft workspace %s", dir)
		}
		if err := deliver(ctx, tools, "spec.md", spec, draftInvalid); err != nil {
			return nil, err
		}
		if err := deliver(ctx, tools, "plan.json", plan, draftValid); err != nil {
			return nil, err
		}
		return &agent.Result{ClaudeID: "session-" + req.Name, ResultText: "Draft delivered", SessionDir: req.SessionDir, NumTurns: 2}, nil
	}
	f.engine.turns["project-spec-draft-1"] = func(ctx context.Context, req agent.Request, _ *agent.Turn, tools *mcp.ClientSession) (*agent.Result, error) {
		args := map[string]any{
			"behavior": "Chunks the server accepts are acknowledged with their offset; the client resumes uploads from the last acknowledged offset.",
			"contracts": []map[string]any{
				{"name": "acknowledgment", "text": "The server's acknowledgment of a stored chunk names its offset; the client reads that offset to resume."},
			},
			"graph": []map[string]any{
				{"from": string(f.stream1), "to": string(f.stream2), "type": "contract"},
			},
			"slices": []map[string]any{
				{"workstream": string(f.stream1), "contract_criteria": []int{2}},
				{"workstream": string(f.stream2), "contract_criteria": []int{1}},
			},
		}
		// A delivery without its intended behaviour is reported invalid and
		// replaced by the next one.
		behavior := args["behavior"]
		args["behavior"] = " "
		if err := verdict(ctx, tools, ProjectSpecDraftTool, args, draftInvalid); err != nil {
			return nil, err
		}
		args["behavior"] = behavior
		if err := verdict(ctx, tools, ProjectSpecDraftTool, args, draftValid); err != nil {
			return nil, err
		}
		return &agent.Result{ClaudeID: "session-" + req.Name, ResultText: "Project spec delivered", SessionDir: req.SessionDir, NumTurns: 1}, nil
	}
	f.engine.mu.Unlock()
}

// awaitProjectSpec waits for the workgroup's project spec to be recorded.
func (f *crossRepoRatifyFixture) awaitProjectSpec(t *testing.T) {
	t.Helper()
	wait, closeWait := serviceChanges(t, f.s)
	defer closeWait()
	deadline := time.Now().Add(demoTimeout)
	for {
		repo1, err := f.s.repository(f.project)
		must(t, err)
		repo2, err := f.s.repository(f.project2)
		must(t, err)
		// The workgroup trace is briefly held open while a background pass
		// dispatches the architect's draft turn; a read that collides with
		// that is transient, not a failure, so it waits and tries again.
		_, _, found, err := repo1.ProjectSpecOf(f.stream1)
		if err != nil && !errors.Is(err, trace.ErrLocked) {
			must(t, err)
		}
		if found {
			return
		}
		feature1, err := repo1.Workflow(f.stream1, trace.FeatureSubject)
		must(t, err)
		feature2, err := repo2.Workflow(f.stream2, trace.FeatureSubject)
		must(t, err)
		if time.Now().After(deadline) {
			t.Fatalf("project spec not recorded; member states are %q and %q", feature1.Value, feature2.Value)
		}
		wait()
	}
}

// awaitDecisionPackets waits for published member packets and completed
// drafting/debate work before ratification or changes to the project spec.
func (f *crossRepoRatifyFixture) awaitDecisionPackets(t *testing.T) [2]PacketResponse {
	t.Helper()
	wait, closeWait := serviceChanges(t, f.s)
	defer closeWait()
	deadline := time.Now().Add(demoTimeout)
	for {
		var packets [2]PacketResponse
		ready := true
		var diagnostic strings.Builder
		for i, member := range []config.WorkstreamID{f.stream1, f.stream2} {
			project := f.project
			if i == 1 {
				project = f.project2
			}
			repo, err := f.s.repository(project)
			must(t, err)
			feature, err := repo.Workflow(member, trace.FeatureSubject)
			must(t, err)
			packet, err := f.c.Packet(context.Background(), member)
			if err != nil && !failed(err, NotFound) {
				t.Fatalf("member %s packet: %v", member, err)
			}
			fmt.Fprintf(&diagnostic, "member=%s state=%s packet=%+v error=%v\n", member, feature.Value, packet, err)
			if err != nil || packet.ProjectSpec == nil || feature.Value != InShedState {
				ready = false
			} else {
				pin, err := latestPin(repo, member)
				must(t, err)
				if packet.Project != project || packet.Workstream != member || packet.Revision < 1 || packet.Packet.Revision != pin {
					t.Fatalf("member %s packet does not pin the published draft: %+v; latest=%+v", member, packet, pin)
				}
				packets[i] = packet
			}
			threads, err := repo.Threads(member)
			must(t, err)
			for _, th := range threads {
				if th.Identity.Role != architectRole && th.Identity.Role != committeeRole {
					continue
				}
				for _, turn := range th.Turns {
					fmt.Fprintf(&diagnostic, "thread=%s turn=%s status=%s completed=%v\n", th.Identity.ThreadID, turn.Request.TurnID, turn.Status(), turn.CompletedAt)
					ready = ready && !turn.CompletedAt.IsZero()
				}
			}
			ops, err := repo.Operations(member)
			must(t, err)
			for _, op := range ops {
				if op.Operation.Action == DraftAction || op.Operation.Action == RoundAction || op.Operation.Action == ReplyAction {
					fmt.Fprintf(&diagnostic, "operation=%s action=%s acknowledged=%v result=%+v history=%+v\n", op.Operation.ID, op.Operation.Action, op.Acknowledged, op.Result, op.History)
					ready = ready && op.Acknowledged
				}
			}
		}
		if ready {
			repo, err := f.s.repository(f.project)
			must(t, err)
			group, err := repo.CrossRepoWorkgroupOf(f.stream1)
			must(t, err)
			wt, g, err := f.s.approvalTrace(context.Background(), repo, group)
			must(t, err)
			state, err := wt.Workflow(g.Members[0], projectSpecDraftSubject)
			must(t, err)
			th, err := wt.Thread(g.Members[0], architectAgent)
			must(t, err)
			must(t, wt.Close())
			fmt.Fprintf(&diagnostic, "group=%s draft=%s thread=%+v\n", group, state.Value, th)
			ready = state.Value == "drafted" && th.Active == "" && len(th.Turns) > 0
			for _, turn := range th.Turns {
				ready = ready && !turn.CompletedAt.IsZero() && turn.Status() == "idle"
			}
			if ready {
				return packets
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("member decision packets and drafting/debate did not settle:\n%s", diagnostic.String())
		}
		wait()
	}
}

// assertApprovalRefused checks the exact defective project-spec revision
// remains current and no group decision or member seal preparation exists.
func (f *crossRepoRatifyFixture) assertApprovalRefused(t *testing.T, group config.WorkgroupID, defective trace.Document) {
	t.Helper()
	repo, err := f.s.repository(f.project)
	must(t, err)
	wt, g, err := f.s.approvalTrace(context.Background(), repo, group)
	must(t, err)
	defer wt.Close()
	if got := projectSpecDocument(t, wt); !equalJSONTest(got, defective) {
		t.Fatalf("ratify did not read the pinned defective project-spec revision: got=%+v want=%+v", got, defective)
	}
	if decision, _, found, err := trace.ReadGroupApproval(wt, g.Members[0]); err != nil || found {
		t.Fatalf("refused draft wrote a group approval: decision=%+v found=%v error=%v", decision, found, err)
	}
	for i, member := range []config.WorkstreamID{f.stream1, f.stream2} {
		project := f.project
		if i == 1 {
			project = f.project2
		}
		repo, err := f.s.repository(project)
		must(t, err)
		if decision, found, err := latestRatification(repo, member); err != nil || found {
			t.Fatalf("refused draft ratified member %s: decision=%+v found=%v error=%v", member, decision, found, err)
		}
		if prepared, _, found, err := seal.PreparedLatest(repo, member); err != nil || found {
			t.Fatalf("refused draft prepared member %s's seal: seal=%+v found=%v error=%v", member, prepared, found, err)
		}
		state, err := repo.Workflow(member, sealSubject)
		must(t, err)
		ops, err := repo.Operations(member)
		must(t, err)
		if state.Value != "" {
			t.Fatalf("refused draft requested member %s's seal: %+v", member, state)
		}
		for _, op := range ops {
			if op.Operation.Action == SealAction {
				t.Fatalf("refused draft created member %s's seal operation: %+v", member, op)
			}
		}
	}
}

func projectSpecDocument(t *testing.T, wt *trace.Repository) trace.Document {
	t.Helper()
	docs, err := trace.Read[trace.Document](wt, "")
	must(t, err)
	var latest trace.Document
	for _, doc := range docs {
		if doc.ID == trace.ProjectSpecsDocument && doc.Revision > latest.Revision {
			latest = doc
		}
	}
	if latest.Revision == 0 {
		t.Fatal("no project-spec document recorded")
	}
	return latest
}

// TestCrossRepoWorkgroupDraftAndRatify is the two-repo workgroup test: the
// architect drafts a project spec with contracts, a graph and slices, the
// project spec appears in the ratification packet, and one owner ratify
// seals the project spec and both members (spec#9).
func TestCrossRepoWorkgroupDraftAndRatify(t *testing.T) {
	t.Parallel()
	f := newCrossRepoRatifyFixture(t)
	defer f.stop(t)
	ctx := context.Background()
	id, api := f.s.DraftWorkgroup(ctx, []config.WorkstreamID{f.stream1, f.stream2})
	if api != nil {
		t.Fatalf("draft workgroup: %v", api)
	}
	packets := f.awaitDecisionPackets(t)

	repo1, err := f.s.repository(f.project)
	must(t, err)
	spec, workgroup, found, err := repo1.ProjectSpecOf(f.stream1)
	must(t, err)
	if !found || workgroup != id {
		t.Fatalf("project spec not recorded for workgroup %s: found=%v workgroup=%s", id, found, workgroup)
	}
	if spec.Behavior == "" || len(spec.Contracts) != 1 || len(spec.Graph) != 1 || len(spec.Slices) != 2 {
		t.Fatalf("project spec incomplete: %+v", spec)
	}
	if slice, ok := trace.SliceFor(spec, f.stream1); !ok || slice.Spec != crossSpec1 || slice.Plan != crossPlan1 {
		t.Fatalf("member1's slice is not its own recorded spec and plan: %+v", slice)
	}
	if slice, ok := trace.SliceFor(spec, f.stream2); !ok || slice.Spec != crossSpec2 || slice.Plan != crossPlan2 {
		t.Fatalf("member2's slice is not its own recorded spec and plan: %+v", slice)
	}

	// The ratification packet shows the project spec.
	packet := packets[0]
	if packet.ProjectSpec == nil || packet.ProjectSpec.Behavior != spec.Behavior {
		t.Fatalf("packet does not carry the project spec: %+v", packet)
	}

	// One owner ratify seals the project spec and both members.
	out, err := f.c.Ratify(ctx, f.stream1, packet.Packet.Revision.Spec, packet.Packet.Revision.Plan, packet.ProjectSpecRevision)
	must(t, err)
	if len(out.Members) != 2 {
		t.Fatalf("ratify did not seal every member: %+v", out)
	}
	for _, m := range []config.WorkstreamID{f.stream1, f.stream2} {
		var repo *trace.Repository
		if m == f.stream1 {
			repo = repo1
		} else {
			repo, err = f.s.repository(f.project2)
			must(t, err)
		}
		if _, ratified, err := latestRatification(repo, m); err != nil || !ratified {
			t.Fatalf("workstream %s is not ratified: err=%v ratified=%v", m, err, ratified)
		}
	}

	// The chief of staff's context carries the ratified project spec and
	// slices, for either member's own repo.
	b, err := f.s.Context().Assemble(ctx, f.project, bundle.Scope{Role: "chief_of_staff", Workstream: f.stream1})
	must(t, err)
	if b.ProjectSpec == nil || b.ProjectSpec.Workgroup != id || b.ProjectSpec.Slice == nil || b.ProjectSpec.Slice.Workstream != f.stream1 {
		t.Fatalf("chief of staff's context of member1 does not carry the ratified project spec: %+v", b.ProjectSpec)
	}
	b2, err := f.s.Context().Assemble(ctx, f.project2, bundle.Scope{Role: "chief_of_staff", Workstream: f.stream2})
	must(t, err)
	if b2.ProjectSpec == nil || b2.ProjectSpec.Workgroup != id || b2.ProjectSpec.Slice == nil || b2.ProjectSpec.Slice.Workstream != f.stream2 {
		t.Fatalf("chief of staff's context of member2 does not carry the ratified project spec: %+v", b2.ProjectSpec)
	}
}

// TestRatifyWorkgroupRefusesAnInvalidGraphWithNothingRecorded shows that a
// ratify of a workgroup whose recorded project spec has an invalid graph is
// refused, recording nothing: no ratification for either member.
func TestRatifyWorkgroupRefusesAnInvalidGraphWithNothingRecorded(t *testing.T) {
	t.Parallel()
	f := newCrossRepoRatifyFixture(t)
	defer f.stop(t)
	ctx := context.Background()
	_, api := f.s.DraftWorkgroup(ctx, []config.WorkstreamID{f.stream1, f.stream2})
	if api != nil {
		t.Fatalf("draft workgroup: %v", api)
	}
	packets := f.awaitDecisionPackets(t)

	// Corrupt the recorded graph directly, as workgroup-spec's validator
	// would refuse: a cycle between the two members. RecordProjectSpec
	// itself validates the graph it is given, so the corrupted spec is
	// written as a raw document instead, the way a storage-level defect
	// would leave it; the test is for ratify's own defensive re-validation.
	repo1, err := f.s.repository(f.project)
	must(t, err)
	spec, workgroup, _, err := repo1.ProjectSpecOf(f.stream1)
	must(t, err)
	spec.Graph = []trace.Edge{{From: f.stream1, To: f.stream2, Type: trace.EdgeContract}, {From: f.stream2, To: f.stream1, Type: trace.EdgeContract}}
	wt, err := trace.OpenWorkgroupTrace(f.s.current().Root, workgroup, []trace.CrossRepoMember{{Workstream: f.stream1, Project: repo1}, {Workstream: f.stream2, Project: mustRepo(t, f.s, f.project2)}})
	must(t, err)
	valid := projectSpecDocument(t, wt)
	writeCorruptedProjectSpec(t, wt, workgroup, spec)
	defective := projectSpecDocument(t, wt)
	if defective.Revision != valid.Revision+1 {
		t.Fatalf("defective graph not recorded as the next revision: valid=%+v defective=%+v", valid, defective)
	}
	must(t, wt.Close())

	if _, err := f.c.Ratify(ctx, f.stream1, packets[0].Packet.Revision.Spec, packets[0].Packet.Revision.Plan, defective.Revision); !failed(err, Validation) || !strings.Contains(err.Error(), "project spec graph is invalid") {
		t.Fatalf("ratify did not refuse the defective graph: %v", err)
	}
	f.assertApprovalRefused(t, workgroup, defective)
}

// TestRatifyWorkgroupRefusesWhenAMemberHasNoSlice shows that a ratify of a
// workgroup whose recorded project spec does not cover every member is
// refused, recording nothing.
func TestRatifyWorkgroupRefusesWhenAMemberHasNoSlice(t *testing.T) {
	t.Parallel()
	f := newCrossRepoRatifyFixture(t)
	defer f.stop(t)
	ctx := context.Background()
	_, api := f.s.DraftWorkgroup(ctx, []config.WorkstreamID{f.stream1, f.stream2})
	if api != nil {
		t.Fatalf("draft workgroup: %v", api)
	}
	packets := f.awaitDecisionPackets(t)

	repo1, err := f.s.repository(f.project)
	must(t, err)
	spec, workgroup, _, err := repo1.ProjectSpecOf(f.stream1)
	must(t, err)
	g := trace.Workgroup{ID: workgroup, Members: []config.WorkstreamID{f.stream1, f.stream2}}
	// Drop member2's slice: the project spec no longer covers every member.
	spec.Slices = spec.Slices[:1]
	wt, err := trace.OpenWorkgroupTrace(f.s.current().Root, workgroup, []trace.CrossRepoMember{{Workstream: f.stream1, Project: repo1}, {Workstream: f.stream2, Project: mustRepo(t, f.s, f.project2)}})
	must(t, err)
	valid := projectSpecDocument(t, wt)
	must(t, trace.RecordProjectSpec(ctx, wt, g, spec, f.s.now(), ownerActor, "drop-slice"))
	defective := projectSpecDocument(t, wt)
	if defective.Revision != valid.Revision+1 {
		t.Fatalf("missing slice not recorded as the next revision: valid=%+v defective=%+v", valid, defective)
	}
	must(t, wt.Close())

	current, _, found, err := repo1.ProjectSpecOf(f.stream1)
	must(t, err)
	if !found || !equalJSONTest(current, spec) || len(current.Slices) != 1 || current.Slices[0].Workstream != f.stream1 {
		t.Fatalf("ratify's project-spec lookup does not see the missing-slice revision: %+v", current)
	}
	if _, err := f.c.Ratify(ctx, f.stream1, packets[0].Packet.Revision.Spec, packets[0].Packet.Revision.Plan, defective.Revision); !failed(err, Conflict) || !strings.Contains(err.Error(), "workstream "+string(f.stream2)+" has no slice") {
		t.Fatalf("ratify did not refuse the missing slice: %v", err)
	}
	f.assertApprovalRefused(t, workgroup, defective)
}

// TestRatifyWorkgroupInterruptedBetweenMembersCompletesOnRetry verifies that
// retry of the production owner approval preserves one decision per member
// and exposes approval only after every pinned seal is complete.
func TestRatifyWorkgroupInterruptedBetweenMembersCompletesOnRetry(t *testing.T) {
	t.Parallel()
	f := newProductionDesignFixture(t)
	defer f.stop(t)
	f.startOwner(t)
	packet := f.awaitPacket(t)
	hook := &designInterruption{step: "group-member-decision-prepared", nth: 1, reached: make(chan struct{})}
	f.hook.Store(hook)
	finished := make(chan error, 1)
	go func() {
		_, err := f.c.Ratify(context.Background(), f.stream1, packet.Packet.Revision.Spec, packet.Packet.Revision.Plan, packet.ProjectSpecRevision)
		finished <- err
	}()
	select {
	case <-hook.reached:
	case <-time.After(demoTimeout):
		t.Fatal("member preparation was not interrupted")
	}
	f.assertBarrier(t)
	f.stop(t)
	<-finished
	f.start(t)
	f.awaitApproved(t)
	out, err := f.c.Ratify(context.Background(), f.stream1, packet.Packet.Revision.Spec, packet.Packet.Revision.Plan, packet.ProjectSpecRevision)
	must(t, err)
	if len(out.Members) != 2 || out.Sealing != "succeeded" {
		t.Fatalf("retry: %+v", out)
	}
	f.awaitApproved(t)
}

// TestDraftWorkgroupRefusesReadinessWithNothingRecordedOnAnyMember covers
// discovery-9f006008dae0b83a83240fde93f9bb0f3bfb3317e781febe7a66494699673a5b:
// DraftWorkgroup checks every member's readiness, including that its
// chief-of-staff thread carries no turn of its own, before it writes
// anything. A member that already has a real conversation, as a design
// session's own workstream legitimately does once the owner has been
// talking to it, must refuse the whole join rather than leave the other,
// ready member durably pointed at a workgroup the refused member never
// joined.
func TestDraftWorkgroupRefusesReadinessWithNothingRecordedOnAnyMember(t *testing.T) {
	t.Parallel()
	f := newCrossRepoRatifyFixture(t)
	defer f.stop(t)
	ctx := context.Background()

	// member2 already has a chief-of-staff turn of its own, exactly the
	// condition that made RetractChiefOfStaff refuse in production.
	if _, err := f.c.Send(ctx, f.stream2, "Where do we stand?"); err != nil {
		t.Fatalf("send: %v", err)
	}

	if _, api := f.s.DraftWorkgroup(ctx, []config.WorkstreamID{f.stream1, f.stream2}); api == nil {
		t.Fatal("draft workgroup with a member that already has chief-of-staff turns was not refused")
	}

	repo1, err := f.s.repository(f.project)
	must(t, err)
	repo2, err := f.s.repository(f.project2)
	must(t, err)
	if id, err := repo1.CrossRepoWorkgroupOf(f.stream1); err != nil || id != "" {
		t.Fatalf("member1 was left pointing at a cross-repo workgroup after a refused readiness check: id=%q err=%v", id, err)
	}
	if id, err := repo2.CrossRepoWorkgroupOf(f.stream2); err != nil || id != "" {
		t.Fatalf("member2, which was ready, was still joined to a workgroup after the whole call was refused: id=%q err=%v", id, err)
	}
	groups1, _, err := trace.Workgroups(repo1)
	must(t, err)
	if _, ok := trace.WorkgroupOf(groups1, f.stream1); !ok {
		t.Fatal("member1's own one-workstream workgroup was retracted despite the refused join")
	}
	groups2, _, err := trace.Workgroups(repo2)
	must(t, err)
	if _, ok := trace.WorkgroupOf(groups2, f.stream2); !ok {
		t.Fatal("member2's workgroup was retracted despite the refused join")
	}
	if _, err := repo1.ChiefOfStaffThread(f.stream1); err != nil {
		t.Fatalf("member1's chief thread changed: %v", err)
	}
	entries, err := os.ReadDir(filepath.Join(f.s.current().Root.String(), "workgroups"))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("refused join created a workgroup trace: %v", entries)
	}
}

// TestDraftWorkgroupInterruptedBetweenMembersCompletesOnRetryWithNoSecondWorkgroup
// covers the same discovery's crash-and-retry requirement: a crash that
// joined member1 alone, leaving member2 not yet pointing at the new
// workgroup, is completed by a retry naming the same members, and that
// retry resumes the workgroup the first attempt already created rather
// than minting a second one.
func TestDraftWorkgroupInterruptedBetweenMembersCompletesOnRetryWithNoSecondWorkgroup(t *testing.T) {
	t.Parallel()
	f := newCrossRepoRatifyFixture(t)
	defer f.stop(t)
	ctx := context.Background()
	repo1, err := f.s.repository(f.project)
	must(t, err)
	repo2, err := f.s.repository(f.project2)
	must(t, err)

	resolved := []trace.CrossRepoMember{{Workstream: f.stream1, Project: repo1}, {Workstream: f.stream2, Project: repo2}}
	f.s.boundary = func(name string) error {
		if name == "workgroup-member-joined" {
			return errors.New("interrupted between members")
		}
		return nil
	}
	_, api := f.s.DraftWorkgroup(ctx, []config.WorkstreamID{f.stream1, f.stream2})
	if api == nil {
		t.Fatal("join did not stop at the injected crash")
	}
	f.s.boundary = nil
	joinedID, err := repo1.CrossRepoWorkgroupOf(f.stream1)
	must(t, err)
	if joinedID == "" {
		t.Fatal("first member was not joined before interruption")
	}
	_, g, _, err := repo1.CrossRepoProjectSpec(joinedID)
	must(t, err)
	if entries, api := f.s.workgroupInbox(ctx, repo1, joinedID); api != nil || len(entries) != 0 {
		t.Fatalf("half-joined workgroup inbox: %+v %v", entries, api)
	}
	if _, err := f.s.crossRepoMemberRepos(ctx, g); !errors.Is(err, errWorkgroupJoining) {
		t.Fatalf("half-joined workgroup became dispatchable: %v", err)
	}
	if ids, err := f.s.crossRepoWorkgroupIDs(ctx, repo1); err != nil || len(ids) != 0 {
		t.Fatalf("half-joined workgroup appeared in discovery: %v %v", ids, err)
	}
	if id, err := repo2.CrossRepoWorkgroupOf(f.stream2); err != nil || id != "" {
		t.Fatalf("member2 was already joined before the retry: id=%q err=%v", id, err)
	}

	// The retry: the owner's same command, naming the same members.
	id, api := f.s.DraftWorkgroup(ctx, []config.WorkstreamID{f.stream1, f.stream2})
	if api != nil {
		t.Fatalf("retry: %v", api)
	}
	if id != g.ID {
		t.Fatalf("retry minted a second workgroup %s instead of resuming %s", id, g.ID)
	}
	if joined, err := repo2.CrossRepoWorkgroupOf(f.stream2); err != nil || joined != g.ID {
		t.Fatalf("member2 was not joined by the retry: joined=%q err=%v", joined, err)
	}
	if _, err := repo2.ChiefOfStaffThread(f.stream2); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("member2 still has its own chief-of-staff thread after joining: %v", err)
	}
	reopened, err := trace.OpenWorkgroupTrace(f.s.current().Root, g.ID, resolved)
	must(t, err)
	defer reopened.Close()
	groups, _, err := trace.Workgroups(reopened)
	must(t, err)
	if len(groups) != 1 {
		t.Fatalf("the retry recorded more than one workgroup: %+v", groups)
	}
	entries, err := os.ReadDir(filepath.Join(f.s.current().Root.String(), "workgroups"))
	must(t, err)
	if len(entries) != 1 {
		t.Fatalf("retry created another workgroup trace: %v", entries)
	}
}

func TestDraftWorkgroupRetryAfterCreationBeforeAnyMemberJoins(t *testing.T) {
	t.Parallel()
	f := newCrossRepoRatifyFixture(t)
	defer f.stop(t)
	f.s.boundary = func(name string) error {
		if name == "workgroup-created" {
			return errors.New("interrupted before first member")
		}
		return nil
	}
	members := []config.WorkstreamID{f.stream1, f.stream2}
	if _, api := f.s.DraftWorkgroup(context.Background(), members); api == nil {
		t.Fatal("expected interruption")
	}
	f.s.boundary = nil
	before, err := os.ReadDir(filepath.Join(f.s.current().Root.String(), "workgroups"))
	must(t, err)
	if len(before) != 1 {
		t.Fatalf("workgroup traces: %v", before)
	}
	id, api := f.s.DraftWorkgroup(context.Background(), members)
	if api != nil || string(id) != before[0].Name() {
		t.Fatalf("retry did not reuse the pending trace: %s %v", id, api)
	}
}

// TestCrossRepoWorkgroupSharesOneChiefOfStaffAcrossARestart shows that, in
// production, an owner message addressed to either member of a cross-repo
// workgroup reaches the one shared chief-of-staff thread held in the
// workgroup's own trace, never a thread of either member's own repo, and
// that this still holds after a service restart: the shared thread is
// found again, not recreated, and both members' messages are still read
// from it.
func TestCrossRepoWorkgroupSharesOneChiefOfStaffAcrossARestart(t *testing.T) {
	t.Parallel()
	f := newCrossRepoRatifyFixture(t)
	defer f.stop(t)
	ctx := context.Background()
	_, api := f.s.DraftWorkgroup(ctx, []config.WorkstreamID{f.stream1, f.stream2})
	if api != nil {
		t.Fatalf("draft workgroup: %v", api)
	}
	f.awaitProjectSpec(t)

	// A message addressed to member1 is durably queued. Nothing runs a
	// chief-of-staff turn in this fixture, so it stays queued, which is
	// enough to show where it was recorded.
	sent1, err := f.c.Send(ctx, f.stream1, "Where does the acknowledgment live?")
	must(t, err)

	repo1, err := f.s.repository(f.project)
	must(t, err)
	if _, err := repo1.ChiefOfStaffThread(f.stream1); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("member1's own repository got a chief-of-staff thread of its own: %v", err)
	}
	repo2, err := f.s.repository(f.project2)
	must(t, err)
	if _, err := repo2.ChiefOfStaffThread(f.stream2); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("member2's own repository got a chief-of-staff thread of its own: %v", err)
	}

	// A second message, addressed to the other member, reaches the same
	// shared thread: both members' own conversation views list both
	// messages, in order.
	sent2, err := f.c.Send(ctx, f.stream2, "Does the client ever see the offset?")
	must(t, err)
	checkSharedConversation := func(t *testing.T) {
		t.Helper()
		for _, m := range []config.WorkstreamID{f.stream1, f.stream2} {
			list, err := f.c.Conversation(ctx, m)
			must(t, err)
			if len(list.Entries) != 2 || list.Entries[0].Turn != sent1.Turn || list.Entries[1].Turn != sent2.Turn {
				t.Fatalf("workstream %s's shared conversation: %+v", m, list.Entries)
			}
		}
	}
	checkSharedConversation(t)

	// Restart: the shared thread, holding both queued messages, is found
	// again from either member, not recreated empty.
	f.stop(t)
	f.start(t)
	checkSharedConversation(t)
	repo1, err = f.s.repository(f.project)
	must(t, err)
	if _, err := repo1.ChiefOfStaffThread(f.stream1); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("member1's own repository got a chief-of-staff thread after restart: %v", err)
	}
	repo2, err = f.s.repository(f.project2)
	must(t, err)
	if _, err := repo2.ChiefOfStaffThread(f.stream2); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("member2's own repository got a chief-of-staff thread after restart: %v", err)
	}
}

func mustRepo(t *testing.T, s *Service, project config.ProjectID) *trace.Repository {
	t.Helper()
	repo, err := s.repository(project)
	must(t, err)
	return repo
}

// corruptedProjectSpecsFile and corruptedProjectSpecEntry mirror the JSON
// shape internal/trace's own, unexported project-specs document uses.
// writeCorruptedProjectSpec writes spec as workgroup's project spec as a raw
// document, bypassing trace.RecordProjectSpec's own validation: it exists so
// a test can simulate a project spec a storage-level defect left invalid,
// which trace.RecordProjectSpec itself would otherwise always refuse to
// write.
type corruptedProjectSpecsFile struct {
	Version int                         `json:"version"`
	Specs   []corruptedProjectSpecEntry `json:"specs"`
}
type corruptedProjectSpecEntry struct {
	Workgroup config.WorkgroupID `json:"workgroup"`
	Spec      trace.ProjectSpec  `json:"spec"`
}

func writeCorruptedProjectSpec(t *testing.T, wt *trace.Repository, workgroup config.WorkgroupID, spec trace.ProjectSpec) {
	t.Helper()
	documents, err := trace.Read[trace.Document](wt, "")
	must(t, err)
	revision := 0
	for _, d := range documents {
		if d.Path == trace.ProjectSpecsPath && d.Revision > revision {
			revision = d.Revision
		}
	}
	data, err := json.Marshal(corruptedProjectSpecsFile{Version: 1, Specs: []corruptedProjectSpecEntry{{Workgroup: workgroup, Spec: spec}}})
	must(t, err)
	must(t, wt.Append(context.Background(), trace.Document{
		Header:  trace.Header{Schema: "osmia.trace.document", Version: trace.Version, ID: trace.ProjectSpecsDocument, Revision: revision + 1, Project: wt.Project(), At: time.Now().UTC(), Actor: ownerActor, Cause: "corrupt-graph"},
		Path:    trace.ProjectSpecsPath,
		Content: string(data),
	}))
}
