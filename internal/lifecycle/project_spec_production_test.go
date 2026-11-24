package lifecycle_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kpenfound/osmia/internal/servicetest/journey"

	"github.com/kpenfound/agentcore/agent"
	"github.com/kpenfound/osmia/internal/api"
	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/intake"
	"github.com/kpenfound/osmia/internal/lifecycle"
	"github.com/kpenfound/osmia/internal/plan"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	"github.com/kpenfound/osmia/internal/servicetest/intakefixture"
	"github.com/kpenfound/osmia/internal/shed"
	"github.com/kpenfound/osmia/internal/trace"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type productionSliceFixture struct {
	*journey.CrossRepoRatifyFixture
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
	f.CrossRepoRatifyFixture = crossRepoFixture(t, false, func(o *journey.Options) {
		file, err := os.OpenFile(filepath.Join(o.Config.Root, "config.toml"), os.O_APPEND|os.O_WRONLY, 0600)
		fixturekit.Must(t, err)
		_, err = file.WriteString("[roles.committee]\nsandbox = \"container\"\nimage = \"fixture-image\"\n[capacity]\ncommittee = 1\n[shed]\nmax_rounds = 1\n")
		fixturekit.Must(t, errors.Join(err, file.Close()))
		o.Boundary = func(ctx context.Context, step string) error {
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
	t.Cleanup(func() { f.Stop(t) })
	id, apiFault := f.S.Lifecycle().DraftWorkgroup(context.Background(), []config.WorkstreamID{f.Stream1, f.Stream2})
	if apiFault != nil {
		t.Fatal(apiFault)
	}
	f.group = id
	awaitProjectSpec(f.CrossRepoRatifyFixture, t)
	f.Stop(t)
	prepareSliceUpstreams(t, f.CrossRepoRatifyFixture)
	f.drafted = refreshDocument(t, f.CrossRepoRatifyFixture, id)
	wt, err := trace.OpenWorkgroupAuthority(f.S.Current().Root, id)
	fixturekit.Must(t, err)
	var found bool
	f.design, found, err = trace.ProjectSpecFor(wt, id)
	fixturekit.Must(t, err)
	fixturekit.Must(t, wt.Close())
	if !found {
		t.Fatal("initial project spec missing")
	}
	f.Opts.Committee = &intake.Committee{Engine: f.Engine, Hosts: f.Opts.Architect.Hosts}
	f.Engine.Mu.Lock()
	f.Engine.Turns[intake.RoundTurnID(1, intake.CommitteeAgent(1), 1)] = func(ctx context.Context, req agent.Request, _ *agent.Turn, tools *mcp.ClientSession) (*agent.Result, error) {
		i := 0
		if strings.Contains(req.Workspace.Directory(), string(f.Stream2)) {
			i = 1
		}
		select {
		case <-f.debate[i]:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		recorded, reason, err := intakefixture.ShedTool(ctx, tools, shed.ObjectTool, map[string]any{"kind": "fit", "part": "spec#1", "argument": "Specify progress while preserving resumption.", "citations": []string{"spec#1"}})
		if err == nil && !recorded {
			err = fmt.Errorf("objection refused: %s", reason)
		}
		return &agent.Result{ClaudeID: "committee-" + req.Name, ResultText: "Review delivered", SessionDir: req.SessionDir, NumTurns: 1}, err
	}
	for attempt := 2; attempt <= 3; attempt++ {
		f.Engine.Turns[intake.RoundTurnID(1, intake.CommitteeAgent(1), attempt)] = f.Engine.Turns[intake.RoundTurnID(1, intake.CommitteeAgent(1), 1)]
	}

	f.Engine.Turns[intake.ReplyTurnID(1, 1)] = func(ctx context.Context, req agent.Request, _ *agent.Turn, tools *mcp.ClientSession) (*agent.Result, error) {
		i, spec, graph := 0, intakefixture.CrossSpec1+"\nReport stored chunks.\n", intakefixture.CrossPlan1+"\n"
		if strings.Contains(req.Workspace.Directory(), string(f.Stream2)) {
			i, spec, graph = 1, intakefixture.CrossSpec2+"\nReport upload progress.\n", intakefixture.CrossPlan2+"\n"
		}
		if contract && i == 0 {
			spec = strings.Replace(intakefixture.CrossSpec1, "chunk's offset.", "chunk's byte offset.", 1)
		}
		for _, doc := range []struct{ path, content string }{{plan.SpecPath, spec}, {plan.PlanPath, graph}} {
			got, err := fixturekit.CallTool(ctx, tools, intake.DraftTool, map[string]any{"path": doc.path, "content": doc.content})
			if err != nil {
				return nil, err
			}
			var status intake.DraftStatus
			if err := json.Unmarshal([]byte(got), &status); err != nil {
				return nil, err
			}
			if status.Verdict == intake.DraftInvalid && doc.path == plan.PlanPath {
				return nil, fmt.Errorf("reply plan refused: %s", got)
			}
		}
		recorded, reason, err := intakefixture.ShedTool(ctx, tools, shed.ReplyTool, map[string]any{"objection": shed.ObjectionID(1, intake.CommitteeAgent(1), 1), "answer": "Progress is explicit; the contract stays protected."})
		if err == nil && !recorded {
			err = fmt.Errorf("reply refused: %s", reason)
		}
		return &agent.Result{ClaudeID: "reply-" + req.Name, ResultText: "Debate revision delivered", SessionDir: req.SessionDir, NumTurns: 2}, err
	}
	f.Engine.Mu.Unlock()
	f.Start(t)
	return f
}

func prepareSliceUpstreams(t *testing.T, f *journey.CrossRepoRatifyFixture) {
	t.Helper()
	f.Upstream(t)
	home := filepath.Dir(f.Clone)
	clone := filepath.Join(home, "clone2")
	bare := filepath.Join(home, "remotes", "owner", "other.git")
	fixturekit.Must(t, os.MkdirAll(filepath.Dir(bare), 0700))
	fixturekit.DemoGit(t, home, "-C", clone, "branch", "-M", "main")
	fixturekit.DemoGit(t, home, "clone", "--quiet", "--bare", "--no-hardlinks", clone, bare)
	fixturekit.DemoGit(t, home, "-C", clone, "remote", "add", "upstream", bare)
}
func (f *productionSliceFixture) awaitPacket(t *testing.T, member config.WorkstreamID, revision int) api.PacketResponse {
	t.Helper()
	wait, closeWait := journey.ServiceChanges(t, f.S)
	defer closeWait()
	deadline := time.Now().Add(fixturekit.DemoTimeout)
	for {
		packet, err := f.C.Packet(context.Background(), member)
		if err == nil && packet.ProjectSpec != nil && packet.ProjectSpecRevision == revision {
			_, _, repo, apiFault := journey.InspectForTest(f.S).ConversationTrace(string(member))
			if apiFault != nil {
				t.Fatal(apiFault)
			}
			pin, pinErr := intake.LatestPin(repo, member)
			fixturekit.Must(t, pinErr)
			if packet.Packet.Revision == pin {
				return packet
			}
		}
		if time.Now().After(deadline) {
			_, _, repo, apiFault := journey.InspectForTest(f.S).ConversationTrace(string(member))
			if apiFault != nil {
				t.Fatal(apiFault)
			}
			feature, _ := repo.Workflow(member, trace.FeatureSubject)
			state, _ := repo.Workflow(member, intake.ShedSubject)
			threads, _ := repo.Threads(member)
			t.Fatalf("controller packet revision %d: packet=%+v error=%v feature=%s shed=%s threads=%+v", revision, packet, err, feature.Value, state.Value, threads)
		}
		wait()
	}
}
func (f *productionSliceFixture) awaitBoundary(t *testing.T, b *sliceBoundary) {
	t.Helper()
	intakefixture.Eventually(t, b.step, func() bool {
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
	for i, member := range []config.WorkstreamID{f.Stream1, f.Stream2} {
		project := f.Project
		if i == 1 {
			project = f.Project2
		}
		repo := mustRepo(t, f.S, project)
		records, err := shed.Records(repo, member)
		fixturekit.Must(t, err)
		replies, err := shed.Replies(repo, member)
		fixturekit.Must(t, err)
		packet, err := f.C.Packet(context.Background(), member)
		fixturekit.Must(t, err)
		if len(records) != 1 || records[0].Failure != "" || len(records[0].Objections) != 1 || len(replies) != 1 || replies[0].Failure != "" || replies[0].Redraft == nil || packet.Packet.Skipped || packet.Packet.Revision != (shed.Pin{Spec: 2, Plan: 2}) {
			t.Fatalf("member did not debate and revise through the shed: records=%+v replies=%+v packet=%+v", records, replies, packet)
		}
	}
}
func (f *productionSliceFixture) assertMechanical(t *testing.T, packet api.PacketResponse, members ...config.WorkstreamID) {
	t.Helper()
	expected := f.design
	expected.Slices = append([]trace.Slice(nil), expected.Slices...)
	for i := range expected.Slices {
		for _, member := range members {
			if expected.Slices[i].Workstream == member {
				_, _, repo, apiFault := journey.InspectForTest(f.S).ConversationTrace(string(member))
				if apiFault != nil {
					t.Fatal(apiFault)
				}
				spec, graph, err := lifecycle.MemberSpecAndPlan(repo, member)
				fixturekit.Must(t, err)
				expected.Slices[i].Spec, expected.Slices[i].Plan = spec, graph
			}
		}
	}
	if !reflect.DeepEqual(*packet.ProjectSpec, expected) {
		t.Fatalf("mechanical refresh changed design or missed slices: got=%+v want=%+v", packet.ProjectSpec, expected)
	}
	wt, err := trace.OpenWorkgroupAuthority(f.S.Current().Root, f.group)
	fixturekit.Must(t, err)
	defer wt.Close()
	old, err := trace.Get[trace.Document](wt, "", trace.ProjectSpecsDocument, f.drafted.Revision)
	fixturekit.Must(t, err)
	if !reflect.DeepEqual(old, f.drafted) {
		t.Fatal("refresh rewrote project-spec history")
	}
}
func (f *productionSliceFixture) awaitApproval(t *testing.T, packet api.PacketResponse) {
	t.Helper()
	wait, closeWait := journey.ServiceChanges(t, f.S)
	defer closeWait()
	deadline := time.Now().Add(fixturekit.DemoTimeout)
	for {
		decision, found, err := mustRepo(t, f.S, f.Project).GroupApprovalOf(f.Stream1)
		fixturekit.Must(t, err)
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
func (f *productionSliceFixture) ratify(t *testing.T, packet api.PacketResponse) {
	t.Helper()
	_, err := f.C.Ratify(context.Background(), f.Stream1, packet.Packet.Revision.Spec, packet.Packet.Revision.Plan, packet.ProjectSpecRevision)
	fixturekit.Must(t, err)
	f.awaitApproval(t, packet)
}
func TestProjectSpecProductionDebateRefreshAndStaleOtherMember(t *testing.T) {
	t.Parallel()
	f := newProductionSliceFixture(t, false)
	close(f.debate[0])
	read := f.awaitPacket(t, f.Stream1, f.drafted.Revision+1)
	f.assertMechanical(t, read, f.Stream1)
	close(f.debate[1])
	packets := awaitDecisionPackets(f.CrossRepoRatifyFixture, t)
	fresh := f.awaitPacket(t, f.Stream1, read.ProjectSpecRevision+1)
	f.assertDebated(t)
	f.assertMechanical(t, fresh, f.Stream1, f.Stream2)
	if read.Packet.Revision != fresh.Packet.Revision || packets[1].Packet.Revision.Spec != 2 {
		t.Fatal("stale read must change only the other member")
	}
	before := refreshDocument(t, f.CrossRepoRatifyFixture, f.group)
	_, err := f.C.Ratify(context.Background(), f.Stream1, read.Packet.Revision.Spec, read.Packet.Revision.Plan, read.ProjectSpecRevision)
	if !intakefixture.Failed(err, api.Conflict) || !containsMessage(err, intake.ProjectSpecCatchingUp) {
		t.Fatalf("old other-member read accepted: %v", err)
	}
	assertApprovalRefused(f.CrossRepoRatifyFixture, t, f.group, before)
	reread, err := f.C.Packet(context.Background(), f.Stream1)
	fixturekit.Must(t, err)
	f.ratify(t, reread)
}
func TestProjectSpecProductionContractDebateRedraft(t *testing.T) {
	t.Parallel()
	f := newProductionSliceFixture(t, true)
	close(f.debate[1])
	other := f.awaitPacket(t, f.Stream2, f.drafted.Revision+1)
	f.assertMechanical(t, other, f.Stream2)
	before := refreshDocument(t, f.CrossRepoRatifyFixture, f.group)
	b := f.hold("project-spec-draft-requested", true)
	var calls atomic.Int32
	spec := strings.Replace(intakefixture.CrossSpec1, "chunk's offset.", "chunk's byte offset.", 1)
	scriptGuardDraft(t, f.CrossRepoRatifyFixture, "project-spec-draft-3", 2, spec, &calls, intakefixture.CrossPlan1+"\n")
	close(f.debate[0])
	f.awaitBoundary(t, b)
	f.awaitPacket(t, f.Stream1, before.Revision)
	assertGuardRefusal(t, f.CrossRepoRatifyFixture, f.group, 2, "")
	if got := refreshDocument(t, f.CrossRepoRatifyFixture, f.group); !reflect.DeepEqual(got, before) {
		t.Fatal("guard refreshed numbered contract text")
	}
	close(b.release)
	packet := f.awaitPacket(t, f.Stream1, before.Revision+1)
	awaitDecisionPackets(f.CrossRepoRatifyFixture, t)
	slice, _ := trace.SliceFor(*packet.ProjectSpec, f.Stream1)
	if slice.Spec != spec || !reflect.DeepEqual(slice.ContractCriteria, []int{2}) || calls.Load() != 1 {
		t.Fatalf("redraft lost architect's reviewed contract: %+v calls=%d", slice, calls.Load())
	}
	f.assertDebated(t)
	if !trace.ContractCitation(*packet.ProjectSpec, f.Stream1, plan.Cite(2)) {
		t.Fatal("redraft dropped contract protection")
	}
	if doc := refreshDocument(t, f.CrossRepoRatifyFixture, f.group); doc.Cause != "project-spec-draft-3" {
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
			close(f.debate[0])
			f.awaitBoundary(t, b)
			f.Stop(t)
			got := refreshDocument(t, f.CrossRepoRatifyFixture, f.group)
			want := f.drafted.Revision
			if step == "project-spec-refresh-recorded" {
				want++
			}
			if got.Revision != want || b.calls.Load() != 1 {
				t.Fatalf("interrupted boundary: revision=%d calls=%d", got.Revision, b.calls.Load())
			}
			f.hook.Store(nil)
			f.Start(t)
			packet := f.awaitPacket(t, f.Stream1, f.drafted.Revision+1)
			f.assertMechanical(t, packet, f.Stream1)
			close(f.debate[1])
			awaitDecisionPackets(f.CrossRepoRatifyFixture, t)
			packet = f.awaitPacket(t, f.Stream1, f.drafted.Revision+2)
			f.assertDebated(t)
			f.assertMechanical(t, packet, f.Stream1, f.Stream2)
			f.ratify(t, packet)
			if got := refreshDocument(t, f.CrossRepoRatifyFixture, f.group); got.Revision != f.drafted.Revision+2 {
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
	for i, member := range []config.WorkstreamID{f.Stream1, f.Stream2} {
		project, spec, graph := f.Project, intakefixture.CrossSpec1+"\nReport stored chunks.\n", intakefixture.CrossPlan1+"\n"
		if i == 1 {
			project, spec, graph = f.Project2, intakefixture.CrossSpec2+"\nReport upload progress.\n", intakefixture.CrossPlan2+"\n"
		}
		repo := mustRepo(t, f.S, project)
		for _, change := range []struct{ id, content string }{{plan.SpecDocument, spec}, {plan.PlanDocument, graph}} {
			doc, err := trace.Get[trace.Document](repo, member, change.id, 1)
			fixturekit.Must(t, err)
			doc.Revision, doc.Content, doc.Cause = 2, change.content, "member-revision"
			fixturekit.Must(t, repo.RecordDocuments(context.Background(), []trace.Document{doc}))
		}
		fixturekit.Must(t, (&intake.Debate{S: f.S.Intake().DebateDependencies(), Repository: repo}).PresentPacket(context.Background(), member, true))
		var err error
		memberRecords[i], err = trace.Read[trace.Record](repo, member)
		fixturekit.Must(t, err)
	}
	wt, g, err := journey.InspectForTest(f.S).ApprovalTrace(context.Background(), (mustRepo(t, f.S, f.Project)), group)
	fixturekit.Must(t, err)
	before, err := trace.Read[trace.Record](wt, "")
	fixturekit.Must(t, err)
	hostBefore, err := trace.Read[trace.Record](wt, g.Members[0])
	fixturekit.Must(t, err)
	fixturekit.Must(t, wt.Close())
	f.Stop(t)
	var reads, appends atomic.Int32
	f.Opts.Boundary = func(_ context.Context, step string) error {
		if step == "project-spec-refresh-read" {
			reads.Add(1)
		}
		if step == "project-spec-refresh-recorded" {
			appends.Add(1)
		}
		return nil
	}
	f.Start(t)
	p := &productionSliceFixture{CrossRepoRatifyFixture: f, group: group, drafted: old, design: *initial[0].ProjectSpec}
	p.awaitPacket(t, f.Stream1, old.Revision+1)
	p.awaitPacket(t, f.Stream2, old.Revision+1)
	packets := awaitDecisionPackets(f, t)
	if packets[0].ProjectSpecRevision != old.Revision+1 || packets[1].ProjectSpecRevision != old.Revision+1 || reads.Load() != 1 || appends.Load() != 1 {
		t.Fatalf("stale group did not repair in one pass: packets=%+v reads=%d appends=%d", packets, reads.Load(), appends.Load())
	}
	wt, err = trace.OpenWorkgroupAuthority(f.S.Current().Root, group)
	fixturekit.Must(t, err)
	after, err := trace.Read[trace.Record](wt, "")
	fixturekit.Must(t, err)
	hostAfter, err := trace.Read[trace.Record](wt, g.Members[0])
	fixturekit.Must(t, err)
	fresh := projectSpecDocument(t, wt)
	fixturekit.Must(t, wt.Close())
	if len(after) != len(before)+1 || !reflect.DeepEqual(after[:len(before)], before) || !reflect.DeepEqual(after[len(before)], fresh) || !reflect.DeepEqual(hostAfter, hostBefore) {
		t.Fatal("repair changed records beyond one appended project-spec revision")
	}
	for i, member := range []config.WorkstreamID{f.Stream1, f.Stream2} {
		project := f.Project
		if i == 1 {
			project = f.Project2
		}
		records, err := trace.Read[trace.Record](mustRepo(t, f.S, project), member)
		fixturekit.Must(t, err)
		if !reflect.DeepEqual(records, memberRecords[i]) {
			t.Fatalf("repair rewrote or appended member %s's records", member)
		}
	}
	p.assertMechanical(t, packets[0], f.Stream1, f.Stream2)
	assertApprovalRefused(f, t, group, fresh)
	p.ratify(t, packets[0])
}
func TestProjectSpecProductionRecoversRefreshedMemberApprovals(t *testing.T) {
	t.Parallel()
	f := newProductionSliceFixture(t, false)
	close(f.debate[0])
	f.awaitPacket(t, f.Stream1, f.drafted.Revision+1)
	close(f.debate[1])
	awaitDecisionPackets(f.CrossRepoRatifyFixture, t)
	packet := f.awaitPacket(t, f.Stream1, f.drafted.Revision+2)
	f.assertDebated(t)
	f.assertMechanical(t, packet, f.Stream1, f.Stream2)
	haltGuardControllers(t, f.CrossRepoRatifyFixture)
	for _, member := range []config.WorkstreamID{f.Stream1, f.Stream2} {
		o, apiFault := f.S.ShedAction(string(member), intake.InShedState, intake.SketchedState)
		if apiFault != nil {
			t.Fatal(apiFault)
		}
		if _, apiFault = journey.InspectForTest(f.S).RatifyMember(context.Background(), o, o.Pin); apiFault != nil {
			t.Fatal(apiFault)
		}
	}
	_, found, err := mustRepo(t, f.S, f.Project).GroupApprovalOf(f.Stream1)
	fixturekit.Must(t, err)
	if found {
		t.Fatal("individual approvals unexpectedly recorded a group decision")
	}
	f.Stop(t)
	f.Start(t)
	f.awaitApproval(t, packet)
	if got := refreshDocument(t, f.CrossRepoRatifyFixture, f.group); got.Revision != packet.ProjectSpecRevision {
		t.Fatal("recovery changed the displayed project spec")
	}
}
