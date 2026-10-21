package service

import (
	context "context"
	json "encoding/json"
	errors "errors"
	fmt "fmt"
	os "os"
	filepath "path/filepath"
	reflect "reflect"
	strings "strings"
	atomic "sync/atomic"
	testing "testing"
	time "time"

	agent "github.com/kpenfound/agentcore/agent"
	config "github.com/kpenfound/osmia/internal/config"
	plan "github.com/kpenfound/osmia/internal/plan"
	shed "github.com/kpenfound/osmia/internal/shed"
	trace "github.com/kpenfound/osmia/internal/trace"
	mcp "github.com/modelcontextprotocol/go-sdk/mcp"
)

// productionSliceFixture drafts before enabling the committee. Both members
// then run a real round and architect reply, with no skipped debate or planted
// packet. The committee gates let the owner read between the two members' edits.
type productionSliceFixture struct {
	*crossRepoRatifyFixture
	group   config.WorkgroupID
	drafted trace.Document
	design  trace.ProjectSpec
	debate  [2]chan struct{}
	hook    atomic.Pointer[sliceBoundary]
}

type sliceBoundary struct {
	step      string
	reached   chan struct{}
	release   chan struct{}
	calls     atomic.Int32
	deferPass bool
}

func (f *productionSliceFixture) hold(step string, deferPass ...bool) *sliceBoundary {
	b := &sliceBoundary{step: step, reached: make(chan struct{}), release: make(chan struct{})}
	if len(deferPass) > 0 {
		b.deferPass = deferPass[0]
	}
	f.hook.Store(b)
	return b
}

func newProductionSliceFixture(t *testing.T, contract bool) *productionSliceFixture {
	t.Helper()
	f := &productionSliceFixture{debate: [2]chan struct{}{make(chan struct{}), make(chan struct{})}}
	f.crossRepoRatifyFixture = crossRepoFixture(t, false, func(o *Options) {
		file, err := os.OpenFile(filepath.Join(o.Config.Root, "config.toml"), os.O_APPEND|os.O_WRONLY, 0600)
		must(t, err)
		_, err = file.WriteString("[roles.committee]\nsandbox = \"container\"\nimage = \"fixture-image\"\n[capacity]\ncommittee = 1\n[shed]\nmax_rounds = 1\n")
		must(t, errors.Join(err, file.Close()))
		o.boundary = func(ctx context.Context, step string) error {
			b := f.hook.Load()
			if b == nil || b.step != step {
				return nil
			}
			if b.calls.Add(1) == 1 {
				close(b.reached)
			}
			if b.deferPass {
				select {
				case <-b.release:
					return nil
				default:
					return trace.ErrLocked
				}
			}
			select {
			case <-b.release:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		}
	})
	t.Cleanup(func() { f.stop(t) })
	id, api := f.s.DraftWorkgroup(context.Background(), []config.WorkstreamID{f.stream1, f.stream2})
	if api != nil {
		t.Fatal(api)
	}
	f.group = id
	f.awaitProjectSpec(t)
	f.stop(t)
	prepareSliceUpstreams(t, f.crossRepoRatifyFixture)
	f.drafted = refreshDocument(t, f.crossRepoRatifyFixture, id)
	wt, err := trace.OpenWorkgroupAuthority(f.s.current().Root, id)
	must(t, err)
	var found bool
	f.design, found, err = trace.ProjectSpecFor(wt, id)
	must(t, err)
	must(t, wt.Close())
	if !found {
		t.Fatal("initial project spec missing")
	}
	f.opts.Committee = &Committee{Engine: f.engine, Hosts: f.opts.Architect.Hosts}
	f.engine.Mu.Lock()
	f.engine.Turns[roundTurnID(1, committeeAgent(1), 1)] = func(ctx context.Context, req agent.Request, _ *agent.Turn, tools *mcp.ClientSession) (*agent.Result, error) {
		i := 0
		if strings.Contains(req.Workspace.Directory(), string(f.stream2)) {
			i = 1
		}
		select {
		case <-f.debate[i]:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		recorded, reason, err := shedTool(ctx, tools, shed.ObjectTool, map[string]any{"kind": "fit", "part": "spec#1", "argument": "Specify progress while preserving resumption.", "citations": []string{"spec#1"}})
		if err == nil && !recorded {
			err = fmt.Errorf("objection refused: %s", reason)
		}
		return &agent.Result{ClaudeID: "committee-" + req.Name, ResultText: "Review delivered", SessionDir: req.SessionDir, NumTurns: 1}, err
	}
	for attempt := 2; attempt <= 3; attempt++ {
		f.engine.Turns[roundTurnID(1, committeeAgent(1), attempt)] = f.engine.Turns[roundTurnID(1, committeeAgent(1), 1)]
	}

	f.engine.Turns[replyTurnID(1, 1)] = func(ctx context.Context, req agent.Request, _ *agent.Turn, tools *mcp.ClientSession) (*agent.Result, error) {
		i, spec, graph := 0, crossSpec1+"\nReport stored chunks.\n", crossPlan1+"\n"
		if strings.Contains(req.Workspace.Directory(), string(f.stream2)) {
			i, spec, graph = 1, crossSpec2+"\nReport upload progress.\n", crossPlan2+"\n"
		}
		if contract && i == 0 {
			spec = strings.Replace(crossSpec1, "chunk's offset.", "chunk's byte offset.", 1)
		}
		for _, doc := range []struct{ path, content string }{{plan.SpecPath, spec}, {plan.PlanPath, graph}} {
			got, err := callTool(ctx, tools, DraftTool, map[string]any{"path": doc.path, "content": doc.content})
			if err != nil {
				return nil, err
			}
			var status draftStatus
			if err := json.Unmarshal([]byte(got), &status); err != nil {
				return nil, err
			}
			if status.Verdict == draftInvalid && doc.path == plan.PlanPath {
				return nil, fmt.Errorf("reply plan refused: %s", got)
			}
		}
		recorded, reason, err := shedTool(ctx, tools, shed.ReplyTool, map[string]any{"objection": shed.ObjectionID(1, committeeAgent(1), 1), "answer": "Progress is explicit; the contract stays protected."})
		if err == nil && !recorded {
			err = fmt.Errorf("reply refused: %s", reason)
		}
		return &agent.Result{ClaudeID: "reply-" + req.Name, ResultText: "Debate revision delivered", SessionDir: req.SessionDir, NumTurns: 2}, err
	}
	f.engine.Mu.Unlock()
	f.start(t)
	return f
}

// prepareSliceUpstreams lets the real sealer fetch both bases from local
// bare repositories, including the configured upstream names it validates.
func prepareSliceUpstreams(t *testing.T, f *crossRepoRatifyFixture) {
	t.Helper()
	f.upstream(t)
	home := filepath.Dir(f.clone)
	clone := filepath.Join(home, "clone2")
	bare := filepath.Join(home, "remotes", "owner", "other.git")
	must(t, os.MkdirAll(filepath.Dir(bare), 0700))
	demoGit(t, home, "-C", clone, "branch", "-M", "main")
	demoGit(t, home, "clone", "--quiet", "--bare", "--no-hardlinks", clone, bare)
	demoGit(t, home, "-C", clone, "remote", "add", "upstream", bare)
}

func (f *productionSliceFixture) awaitPacket(t *testing.T, member config.WorkstreamID, revision int) PacketResponse {
	t.Helper()
	wait, closeWait := serviceChanges(t, f.s)
	defer closeWait()
	deadline := time.Now().Add(demoTimeout)
	for {
		packet, err := f.c.Packet(context.Background(), member)
		if err == nil && packet.ProjectSpec != nil && packet.ProjectSpecRevision == revision {
			_, _, repo, api := f.s.conversationTrace(string(member))
			if api != nil {
				t.Fatal(api)
			}
			pin, pinErr := latestPin(repo, member)
			must(t, pinErr)
			if packet.Packet.Revision == pin {
				return packet
			}
		}
		if time.Now().After(deadline) {
			_, _, repo, api := f.s.conversationTrace(string(member))
			if api != nil {
				t.Fatal(api)
			}
			feature, _ := repo.Workflow(member, trace.FeatureSubject)
			state, _ := repo.Workflow(member, shedSubject)
			threads, _ := repo.Threads(member)
			t.Fatalf("controller packet revision %d: packet=%+v error=%v feature=%s shed=%s threads=%+v", revision, packet, err, feature.Value, state.Value, threads)
		}
		wait()
	}
}

func (f *productionSliceFixture) awaitBoundary(t *testing.T, b *sliceBoundary) {
	t.Helper()
	eventually(t, b.step, func() bool {
		select {
		case <-b.reached:
			return true
		default:
			return false
		}
	})
}

func (f *productionSliceFixture) assertDebated(t *testing.T) {
	t.Helper()
	for i, member := range []config.WorkstreamID{f.stream1, f.stream2} {
		project := f.project
		if i == 1 {
			project = f.project2
		}
		repo := mustRepo(t, f.s, project)
		records, err := shed.Records(repo, member)
		must(t, err)
		replies, err := shed.Replies(repo, member)
		must(t, err)
		packet, err := f.c.Packet(context.Background(), member)
		must(t, err)
		if len(records) != 1 || records[0].Failure != "" || len(records[0].Objections) != 1 || len(replies) != 1 || replies[0].Failure != "" || replies[0].Redraft == nil || packet.Packet.Skipped || packet.Packet.Revision != (shed.Pin{Spec: 2, Plan: 2}) {
			t.Fatalf("member did not debate and revise through the shed: records=%+v replies=%+v packet=%+v", records, replies, packet)
		}
	}
}

func (f *productionSliceFixture) assertMechanical(t *testing.T, packet PacketResponse, members ...config.WorkstreamID) {
	t.Helper()
	expected := f.design
	expected.Slices = append([]trace.Slice(nil), expected.Slices...)
	for i := range expected.Slices {
		for _, member := range members {
			if expected.Slices[i].Workstream == member {
				_, _, repo, api := f.s.conversationTrace(string(member))
				if api != nil {
					t.Fatal(api)
				}
				spec, graph, err := memberSpecAndPlan(repo, member)
				must(t, err)
				expected.Slices[i].Spec, expected.Slices[i].Plan = spec, graph
			}
		}
	}
	if !reflect.DeepEqual(*packet.ProjectSpec, expected) {
		t.Fatalf("mechanical refresh changed design or missed slices: got=%+v want=%+v", packet.ProjectSpec, expected)
	}
	wt, err := trace.OpenWorkgroupAuthority(f.s.current().Root, f.group)
	must(t, err)
	defer wt.Close()
	old, err := trace.Get[trace.Document](wt, "", trace.ProjectSpecsDocument, f.drafted.Revision)
	must(t, err)
	if !reflect.DeepEqual(old, f.drafted) {
		t.Fatal("refresh rewrote project-spec history")
	}
}

func (f *productionSliceFixture) awaitApproval(t *testing.T, packet PacketResponse) {
	t.Helper()
	wait, closeWait := serviceChanges(t, f.s)
	defer closeWait()
	deadline := time.Now().Add(demoTimeout)
	for {
		decision, found, err := mustRepo(t, f.s, f.project).GroupApprovalOf(f.stream1)
		must(t, err)
		if found && decision.Complete {
			if decision.ProjectSpecRevision != packet.ProjectSpecRevision || !reflect.DeepEqual(decision.ProjectSpec, *packet.ProjectSpec) || len(decision.Members) != 2 {
				t.Fatalf("approval differs from displayed project spec: %+v", decision)
			}
			for _, member := range decision.Members {
				if member.Spec != 2 || member.Plan != 2 || member.Round != 1 {
					t.Fatalf("approval did not preserve debated member pins: %+v", member)
				}
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("group did not complete: found=%v decision=%+v", found, decision)
		}
		wait()
	}
}

func (f *productionSliceFixture) ratify(t *testing.T, packet PacketResponse) {
	t.Helper()
	_, err := f.c.Ratify(context.Background(), f.stream1, packet.Packet.Revision.Spec, packet.Packet.Revision.Plan, packet.ProjectSpecRevision)
	must(t, err)
	f.awaitApproval(t, packet)
}

func TestProjectSpecProductionDebateRefreshAndStaleOtherMember(t *testing.T) {
	t.Parallel()
	f := newProductionSliceFixture(t, false)
	close(f.debate[0])
	read := f.awaitPacket(t, f.stream1, f.drafted.Revision+1)
	f.assertMechanical(t, read, f.stream1)
	close(f.debate[1])
	packets := f.awaitDecisionPackets(t)
	fresh := f.awaitPacket(t, f.stream1, read.ProjectSpecRevision+1)
	f.assertDebated(t)
	f.assertMechanical(t, fresh, f.stream1, f.stream2)
	if read.Packet.Revision != fresh.Packet.Revision || packets[1].Packet.Revision.Spec != 2 {
		t.Fatal("stale read must change only the other member")
	}
	before := refreshDocument(t, f.crossRepoRatifyFixture, f.group)
	_, err := f.c.Ratify(context.Background(), f.stream1, read.Packet.Revision.Spec, read.Packet.Revision.Plan, read.ProjectSpecRevision)
	if !failed(err, Conflict) || !containsMessage(err, projectSpecCatchingUp) {
		t.Fatalf("old other-member read accepted: %v", err)
	}
	f.assertApprovalRefused(t, f.group, before)
	reread, err := f.c.Packet(context.Background(), f.stream1)
	must(t, err)
	f.ratify(t, reread)
}

func TestProjectSpecProductionContractDebateRedraft(t *testing.T) {
	t.Parallel()
	f := newProductionSliceFixture(t, true)
	// Settle the other member first, so the eventual owner decision can pin
	// the architect's redraft itself without an intervening mechanical refresh.
	close(f.debate[1])
	other := f.awaitPacket(t, f.stream2, f.drafted.Revision+1)
	f.assertMechanical(t, other, f.stream2)
	before := refreshDocument(t, f.crossRepoRatifyFixture, f.group)
	// Defer the redraft as a locked trace would, so the controller can finish
	// presenting the debate packet before the architect delivers its new contract.
	b := f.hold("project-spec-draft-requested", true)
	var calls atomic.Int32
	spec := strings.Replace(crossSpec1, "chunk's offset.", "chunk's byte offset.", 1)
	scriptGuardDraft(t, f.crossRepoRatifyFixture, "project-spec-draft-3", 2, spec, &calls, crossPlan1+"\n")
	close(f.debate[0])
	f.awaitBoundary(t, b)
	f.awaitPacket(t, f.stream1, before.Revision)
	assertGuardRefusal(t, f.crossRepoRatifyFixture, f.group, 2, "")
	if got := refreshDocument(t, f.crossRepoRatifyFixture, f.group); !reflect.DeepEqual(got, before) {
		t.Fatal("guard refreshed numbered contract text")
	}
	close(b.release)
	packet := f.awaitPacket(t, f.stream1, before.Revision+1)
	f.awaitDecisionPackets(t)
	slice, _ := trace.SliceFor(*packet.ProjectSpec, f.stream1)
	if slice.Spec != spec || !reflect.DeepEqual(slice.ContractCriteria, []int{2}) || calls.Load() != 1 {
		t.Fatalf("redraft lost architect's reviewed contract: %+v calls=%d", slice, calls.Load())
	}
	f.assertDebated(t)
	if !trace.ContractCitation(*packet.ProjectSpec, f.stream1, plan.Cite(2)) {
		t.Fatal("redraft dropped contract protection")
	}
	if doc := refreshDocument(t, f.crossRepoRatifyFixture, f.group); doc.Cause != "project-spec-draft-3" {
		t.Fatalf("packet must pin the architect's redraft: %+v", doc)
	}
	f.ratify(t, packet)
}

func TestProjectSpecProductionRestartRefresh(t *testing.T) {
	t.Parallel()
	for _, step := range []string{"project-spec-refresh-read", "project-spec-refresh-recorded"} {
		t.Run(step, func(t *testing.T) {
			t.Parallel()
			f := newProductionSliceFixture(t, false)
			b := f.hold(step)
			// Only one member changes at this boundary; the second is released later.
			close(f.debate[0])
			f.awaitBoundary(t, b)
			f.stop(t)
			got := refreshDocument(t, f.crossRepoRatifyFixture, f.group)
			want := f.drafted.Revision
			if step == "project-spec-refresh-recorded" {
				want++
			}
			if got.Revision != want || b.calls.Load() != 1 {
				t.Fatalf("interrupted boundary: revision=%d calls=%d", got.Revision, b.calls.Load())
			}
			f.hook.Store(nil)
			f.start(t)
			packet := f.awaitPacket(t, f.stream1, f.drafted.Revision+1)
			f.assertMechanical(t, packet, f.stream1)
			close(f.debate[1])
			f.awaitDecisionPackets(t)
			packet = f.awaitPacket(t, f.stream1, f.drafted.Revision+2)
			f.assertDebated(t)
			f.assertMechanical(t, packet, f.stream1, f.stream2)
			f.ratify(t, packet)
			if got := refreshDocument(t, f.crossRepoRatifyFixture, f.group); got.Revision != f.drafted.Revision+2 {
				t.Fatal("restart duplicated refresh")
			}
		})
	}
}

func TestProjectSpecProductionRepairsRecordedStaleSlices(t *testing.T) {
	t.Parallel()
	f, group, initial := projectSliceRefreshFixture(t)
	prepareSliceUpstreams(t, f)
	old := refreshDocument(t, f, group)
	var memberRecords [2][]trace.Record
	// Record member revisions after the original architect's project spec, while
	// controllers are stopped. Startup must repair this persisted unapproved group.
	for i, member := range []config.WorkstreamID{f.stream1, f.stream2} {
		project, spec, graph := f.project, crossSpec1+"\nReport stored chunks.\n", crossPlan1+"\n"
		if i == 1 {
			project, spec, graph = f.project2, crossSpec2+"\nReport upload progress.\n", crossPlan2+"\n"
		}
		repo := mustRepo(t, f.s, project)
		for _, change := range []struct{ id, content string }{{plan.SpecDocument, spec}, {plan.PlanDocument, graph}} {
			doc, err := trace.Get[trace.Document](repo, member, change.id, 1)
			must(t, err)
			doc.Revision, doc.Content, doc.Cause = 2, change.content, "member-revision"
			must(t, repo.RecordDocuments(context.Background(), []trace.Document{doc}))
		}
		// The persisted decision packet already follows these member revisions;
		// startup only needs to repair the shared slices.
		must(t, (&debate{S: f.s.intake().DebateDependencies(), Repository: repo}).PresentPacket(context.Background(), member, true))
		var err error
		memberRecords[i], err = trace.Read[trace.Record](repo, member)
		must(t, err)
	}
	wt, g, err := f.s.approvalTrace(context.Background(), mustRepo(t, f.s, f.project), group)
	must(t, err)
	before, err := trace.Read[trace.Record](wt, "")
	must(t, err)
	hostBefore, err := trace.Read[trace.Record](wt, g.Members[0])
	must(t, err)
	must(t, wt.Close())
	f.stop(t)
	var reads, appends atomic.Int32
	f.opts.boundary = func(_ context.Context, step string) error {
		if step == "project-spec-refresh-read" {
			reads.Add(1)
		}
		if step == "project-spec-refresh-recorded" {
			appends.Add(1)
		}
		return nil
	}
	f.start(t)
	p := &productionSliceFixture{crossRepoRatifyFixture: f, group: group, drafted: old, design: *initial[0].ProjectSpec}
	p.awaitPacket(t, f.stream1, old.Revision+1)
	p.awaitPacket(t, f.stream2, old.Revision+1)
	packets := f.awaitDecisionPackets(t)
	if packets[0].ProjectSpecRevision != old.Revision+1 || packets[1].ProjectSpecRevision != old.Revision+1 || reads.Load() != 1 || appends.Load() != 1 {
		t.Fatalf("stale group did not repair in one pass: packets=%+v reads=%d appends=%d", packets, reads.Load(), appends.Load())
	}
	wt, err = trace.OpenWorkgroupAuthority(f.s.current().Root, group)
	must(t, err)
	after, err := trace.Read[trace.Record](wt, "")
	must(t, err)
	hostAfter, err := trace.Read[trace.Record](wt, g.Members[0])
	must(t, err)
	fresh := projectSpecDocument(t, wt)
	must(t, wt.Close())
	if len(after) != len(before)+1 || !reflect.DeepEqual(after[:len(before)], before) || !reflect.DeepEqual(after[len(before)], fresh) || !reflect.DeepEqual(hostAfter, hostBefore) {
		t.Fatal("repair changed records beyond one appended project-spec revision")
	}
	for i, member := range []config.WorkstreamID{f.stream1, f.stream2} {
		project := f.project
		if i == 1 {
			project = f.project2
		}
		records, err := trace.Read[trace.Record](mustRepo(t, f.s, project), member)
		must(t, err)
		if !reflect.DeepEqual(records, memberRecords[i]) {
			t.Fatalf("repair rewrote or appended member %s's records", member)
		}
	}
	p.assertMechanical(t, packets[0], f.stream1, f.stream2)
	f.assertApprovalRefused(t, group, fresh)
	p.ratify(t, packets[0])
}

func TestProjectSpecProductionRecoversRefreshedMemberApprovals(t *testing.T) {
	t.Parallel()
	f := newProductionSliceFixture(t, false)
	close(f.debate[0])
	f.awaitPacket(t, f.stream1, f.drafted.Revision+1)
	close(f.debate[1])
	f.awaitDecisionPackets(t)
	packet := f.awaitPacket(t, f.stream1, f.drafted.Revision+2)
	f.assertDebated(t)
	f.assertMechanical(t, packet, f.stream1, f.stream2)
	// Persist the individual owner decisions without a group receipt, as an
	// interrupted approval can leave them. Only the restarted controller recovers
	// and completes the group; the owner sends no further ratify request.
	haltGuardControllers(t, f.crossRepoRatifyFixture)
	for _, member := range []config.WorkstreamID{f.stream1, f.stream2} {
		o, api := f.s.shedAction(string(member), InShedState, SketchedState)
		if api != nil {
			t.Fatal(api)
		}
		if _, api = f.s.ratifyMember(context.Background(), o, o.Pin); api != nil {
			t.Fatal(api)
		}
	}
	_, found, err := mustRepo(t, f.s, f.project).GroupApprovalOf(f.stream1)
	must(t, err)
	if found {
		t.Fatal("individual approvals unexpectedly recorded a group decision")
	}
	f.stop(t)
	f.start(t)
	f.awaitApproval(t, packet)
	if got := refreshDocument(t, f.crossRepoRatifyFixture, f.group); got.Revision != packet.ProjectSpecRevision {
		t.Fatal("recovery changed the displayed project spec")
	}
}
