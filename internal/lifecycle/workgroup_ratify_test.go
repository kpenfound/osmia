package lifecycle_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kpenfound/osmia/internal/api"
	"github.com/kpenfound/osmia/internal/bundle"
	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/intake"
	"github.com/kpenfound/osmia/internal/lifecycle"
	"github.com/kpenfound/osmia/internal/seal"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	"github.com/kpenfound/osmia/internal/servicetest/intakefixture"
	"github.com/kpenfound/osmia/internal/trace"
)

const (
	crossSpec1 = intakefixture.CrossSpec1
	crossPlan1 = intakefixture.CrossPlan1
	crossSpec2 = intakefixture.CrossSpec2
	crossPlan2 = intakefixture.CrossPlan2
)

type crossRepoRatifyFixture struct {
	*architectFixture
	project2         config.ProjectID
	stream1, stream2 config.WorkstreamID
}

func TestGroupApprovalRequiresLiveMemberTraceAndHonorsShutdown(t *testing.T) {
	t.Parallel()
	s, _ := start(t, fixture(t))
	decision := trace.GroupApproval{Members: []trace.GroupMemberApproval{{Project: fixturekit.Project, Workstream: fixturekit.Stream}}}
	if err := s.prepareGroupApproval(context.Background(), decision); !errors.Is(err, intake.ErrNoTrace) {
		t.Fatalf("live missing trace was not reported: %v", err)
	}
	fixturekit.Must(t, s.Close())
	if err := s.prepareGroupApproval(s.lifetime, decision); !errors.Is(err, context.Canceled) {
		t.Fatalf("shutdown resolution was not cancellation: %v", err)
	}
	if err := s.prepareGroupApproval(context.Background(), decision); !errors.Is(err, intake.ErrNoTrace) {
		t.Fatalf("uncanceled missing trace was hidden: %v", err)
	}
}
func newCrossRepoRatifyFixture(t *testing.T, configure ...func(*Options)) *crossRepoRatifyFixture {
	t.Helper()
	return crossRepoFixture(t, true, configure...)
}
func crossRepoFixture(t *testing.T, skipDebate bool, configure ...func(*Options)) *crossRepoRatifyFixture {
	t.Helper()
	draftsReady := make(chan struct{})
	configure = append(configure, func(opts *Options) {
		hold := opts.Reconciliation.Hold
		opts.Reconciliation.Hold = func(stream config.WorkstreamID, op coreadapter.Operation) bool {
			if op.Action == intake.DraftAction {
				select {
				case <-draftsReady:
				default:
					return true
				}
			}
			return hold != nil && hold(stream, op)
		}
	})
	f := newArchitectFixture(t, configure...)
	home := filepath.Dir(f.clone)
	clone2 := filepath.Join(home, "clone2")
	fixturekit.Must(t, os.Mkdir(clone2, 0700))
	fixturekit.DemoGit(t, home, "init", "--quiet", clone2)
	fixturekit.Must(t, os.MkdirAll(filepath.Join(clone2, "internal", "trace"), 0700))
	fixturekit.Must(t, os.WriteFile(filepath.Join(clone2, "internal", "trace", "git.go"), []byte("package trace\n"), 0600))
	fixturekit.DemoGit(t, home, "-C", clone2, "add", "internal")
	fixturekit.DemoGit(t, home, "-C", clone2, "-c", "user.name=Owner", "-c", "user.email=owner@example.invalid", "commit", "-qm", "base")
	added, err := f.c.AddProject(context.Background(), api.ProjectAddRequest{Name: "other", Upstream: "owner/other", Fork: "owner/other-fork", Clone: clone2})
	fixturekit.Must(t, err)
	writeCharter(t, f.c, added.Project.ID, "1. Keep changes small.\n")
	cf := &crossRepoRatifyFixture{architectFixture: f, project2: added.Project.ID}
	handIn := func(project config.ProjectID, key string) config.WorkstreamID {
		content := "Build " + key
		out, err := cf.c.HandIn(context.Background(), api.HandInRequest{Project: project, Key: key, Stdin: &content, SkipDebate: skipDebate})
		fixturekit.Must(t, err)
		return out.Workstream
	}
	cf.stream1 = intake.HandInWorkstream(cf.project, "upload-server")
	cf.stream2 = intake.HandInWorkstream(cf.project2, "upload-client")
	cf.scriptDrafts(t)
	if got := handIn(cf.project, "upload-server"); got != cf.stream1 {
		t.Fatalf("server hand-in workstream = %s, want %s", got, cf.stream1)
	}
	if got := handIn(cf.project2, "upload-client"); got != cf.stream2 {
		t.Fatalf("client hand-in workstream = %s, want %s", got, cf.stream2)
	}
	close(draftsReady)
	for _, project := range []config.ProjectID{cf.project, cf.project2} {
		repo, err := cf.s.repository(project)
		fixturekit.Must(t, err)
		repo.WakeWorkflow()
	}
	return cf
}

func (f *crossRepoRatifyFixture) scriptDrafts(t *testing.T) {
	t.Helper()
	intakefixture.ScriptCrossRepoDrafts(t, f.engine, func() (config.WorkstreamID, config.WorkstreamID) { return f.stream1, f.stream2 })
}

func (f *crossRepoRatifyFixture) awaitProjectSpec(t *testing.T) {
	t.Helper()
	wait, closeWait := serviceChanges(t, f.s)
	defer closeWait()
	deadline := time.Now().Add(fixturekit.DemoTimeout)
	for {
		repo1, err := f.s.repository(f.project)
		fixturekit.Must(t, err)
		repo2, err := f.s.repository(f.project2)
		fixturekit.Must(t, err)
		_, _, found, err := repo1.ProjectSpecOf(f.stream1)
		if err != nil && !errors.Is(err, trace.ErrLocked) {
			fixturekit.Must(t, err)
		}
		if found {
			return
		}
		feature1, err := repo1.Workflow(f.stream1, trace.FeatureSubject)
		fixturekit.Must(t, err)
		feature2, err := repo2.Workflow(f.stream2, trace.FeatureSubject)
		fixturekit.Must(t, err)
		if time.Now().After(deadline) {
			t.Fatalf("project spec not recorded; member states are %q and %q", feature1.Value, feature2.Value)
		}
		wait()
	}
}

func (f *crossRepoRatifyFixture) awaitDecisionPackets(t *testing.T) [2]api.PacketResponse {
	t.Helper()
	wait, closeWait := serviceChanges(t, f.s)
	defer closeWait()
	deadline := time.Now().Add(fixturekit.DemoTimeout)
	for {
		var packets [2]api.PacketResponse
		ready := true
		var diagnostic strings.Builder
		for i, member := range []config.WorkstreamID{f.stream1, f.stream2} {
			project := f.project
			if i == 1 {
				project = f.project2
			}
			repo, err := f.s.repository(project)
			fixturekit.Must(t, err)
			feature, err := repo.Workflow(member, trace.FeatureSubject)
			fixturekit.Must(t, err)
			packet, err := f.c.Packet(context.Background(), member)
			if err != nil && !intakefixture.Failed(err, api.NotFound) {
				t.Fatalf("member %s packet: %v", member, err)
			}
			fmt.Fprintf(&diagnostic, "member=%s state=%s packet=%+v error=%v\n", member, feature.Value, packet, err)
			if err != nil || packet.ProjectSpec == nil || feature.Value != intake.InShedState {
				ready = false
			} else {
				pin, err := intake.LatestPin(repo, member)
				fixturekit.Must(t, err)
				if packet.Project != project || packet.Workstream != member || packet.Revision < 1 || packet.Packet.Revision != pin {
					t.Fatalf("member %s packet does not pin the published draft: %+v; latest=%+v", member, packet, pin)
				}
				packets[i] = packet
			}
			threads, err := repo.Threads(member)
			fixturekit.Must(t, err)
			for _, th := range threads {
				if th.Identity.Role != intake.ArchitectRole && th.Identity.Role != intake.CommitteeRole {
					continue
				}
				for _, turn := range th.Turns {
					fmt.Fprintf(&diagnostic, "thread=%s turn=%s status=%s completed=%v\n", th.Identity.ThreadID, turn.Request.TurnID, turn.Status(), turn.CompletedAt)
					ready = ready && !turn.CompletedAt.IsZero()
				}
			}
			ops, err := repo.Operations(member)
			fixturekit.Must(t, err)
			for _, op := range ops {
				if op.Operation.Action == intake.DraftAction || op.Operation.Action == intake.RoundAction || op.Operation.Action == intake.ReplyAction {
					fmt.Fprintf(&diagnostic, "operation=%s action=%s acknowledged=%v result=%+v history=%+v\n", op.Operation.ID, op.Operation.Action, op.Acknowledged, op.Result, op.History)
					ready = ready && op.Acknowledged
				}
			}
		}
		if ready {
			repo, err := f.s.repository(f.project)
			fixturekit.Must(t, err)
			group, err := repo.CrossRepoWorkgroupOf(f.stream1)
			fixturekit.Must(t, err)
			wt, g, err := f.s.approvalTrace(context.Background(), repo, group)
			fixturekit.Must(t, err)
			state, err := wt.Workflow(g.Members[0], lifecycle.ProjectSpecDraftSubject)
			fixturekit.Must(t, err)
			th, err := wt.Thread(g.Members[0], intake.ArchitectAgent)
			fixturekit.Must(t, err)
			fixturekit.Must(t, wt.Close())
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

func (f *crossRepoRatifyFixture) assertApprovalRefused(t *testing.T, group config.WorkgroupID, defective trace.Document) {
	t.Helper()
	repo, err := f.s.repository(f.project)
	fixturekit.Must(t, err)
	wt, g, err := f.s.approvalTrace(context.Background(), repo, group)
	fixturekit.Must(t, err)
	defer wt.Close()
	if got := projectSpecDocument(t, wt); !intakefixture.EqualJSONTest(got, defective) {
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
		fixturekit.Must(t, err)
		if decision, found, err := intake.LatestRatification(repo, member); err != nil || found {
			t.Fatalf("refused draft ratified member %s: decision=%+v found=%v error=%v", member, decision, found, err)
		}
		if prepared, _, found, err := seal.PreparedLatest(repo, member); err != nil || found {
			t.Fatalf("refused draft prepared member %s's seal: seal=%+v found=%v error=%v", member, prepared, found, err)
		}
		state, err := repo.Workflow(member, intake.SealSubject)
		fixturekit.Must(t, err)
		ops, err := repo.Operations(member)
		fixturekit.Must(t, err)
		if state.Value != "" {
			t.Fatalf("refused draft requested member %s's seal: %+v", member, state)
		}
		for _, op := range ops {
			if op.Operation.Action == intake.SealAction {
				t.Fatalf("refused draft created member %s's seal operation: %+v", member, op)
			}
		}
	}
}
func projectSpecDocument(t *testing.T, wt *trace.Repository) trace.Document {
	t.Helper()
	docs, err := trace.Read[trace.Document](wt, "")
	fixturekit.Must(t, err)
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

func TestCrossRepoWorkgroupDraftAndRatify(t *testing.T) {
	t.Parallel()
	f := newCrossRepoRatifyFixture(t)
	defer f.stop(t)
	ctx := context.Background()
	id, apiFault := f.s.DraftWorkgroup(ctx, []config.WorkstreamID{f.stream1, f.stream2})
	if apiFault != nil {
		t.Fatalf("draft workgroup: %v", apiFault)
	}
	packets := f.awaitDecisionPackets(t)

	repo1, err := f.s.repository(f.project)
	fixturekit.Must(t, err)
	spec, workgroup, found, err := repo1.ProjectSpecOf(f.stream1)
	fixturekit.Must(t, err)
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

	packet := packets[0]
	if packet.ProjectSpec == nil || packet.ProjectSpec.Behavior != spec.Behavior {
		t.Fatalf("packet does not carry the project spec: %+v", packet)
	}

	out, err := f.c.Ratify(ctx, f.stream1, packet.Packet.Revision.Spec, packet.Packet.Revision.Plan, packet.ProjectSpecRevision)
	fixturekit.Must(t, err)
	if len(out.Members) != 2 {
		t.Fatalf("ratify did not seal every member: %+v", out)
	}
	for _, m := range []config.WorkstreamID{f.stream1, f.stream2} {
		var repo *trace.Repository
		if m == f.stream1 {
			repo = repo1
		} else {
			repo, err = f.s.repository(f.project2)
			fixturekit.Must(t, err)
		}
		if _, ratified, err := intake.LatestRatification(repo, m); err != nil || !ratified {
			t.Fatalf("workstream %s is not ratified: err=%v ratified=%v", m, err, ratified)
		}
	}

	b, err := f.s.Context().Assemble(ctx, f.project, bundle.Scope{Role: "chief_of_staff", Workstream: f.stream1})
	fixturekit.Must(t, err)
	if b.ProjectSpec == nil || b.ProjectSpec.Workgroup != id || b.ProjectSpec.Slice == nil || b.ProjectSpec.Slice.Workstream != f.stream1 {
		t.Fatalf("chief of staff's context of member1 does not carry the ratified project spec: %+v", b.ProjectSpec)
	}
	b2, err := f.s.Context().Assemble(ctx, f.project2, bundle.Scope{Role: "chief_of_staff", Workstream: f.stream2})
	fixturekit.Must(t, err)
	if b2.ProjectSpec == nil || b2.ProjectSpec.Workgroup != id || b2.ProjectSpec.Slice == nil || b2.ProjectSpec.Slice.Workstream != f.stream2 {
		t.Fatalf("chief of staff's context of member2 does not carry the ratified project spec: %+v", b2.ProjectSpec)
	}
}

func TestRatifyWorkgroupRefusesAnInvalidGraphWithNothingRecorded(t *testing.T) {
	t.Parallel()
	f := newCrossRepoRatifyFixture(t)
	defer f.stop(t)
	ctx := context.Background()
	_, apiFault := f.s.DraftWorkgroup(ctx, []config.WorkstreamID{f.stream1, f.stream2})
	if apiFault != nil {
		t.Fatalf("draft workgroup: %v", apiFault)
	}
	packets := f.awaitDecisionPackets(t)

	repo1, err := f.s.repository(f.project)
	fixturekit.Must(t, err)
	spec, workgroup, _, err := repo1.ProjectSpecOf(f.stream1)
	fixturekit.Must(t, err)
	spec.Graph = []trace.Edge{{From: f.stream1, To: f.stream2, Type: trace.EdgeContract}, {From: f.stream2, To: f.stream1, Type: trace.EdgeContract}}
	wt, err := trace.OpenWorkgroupTrace(f.s.current().Root, workgroup, []trace.CrossRepoMember{{Workstream: f.stream1, Project: repo1}, {Workstream: f.stream2, Project: mustRepo(t, f.s, f.project2)}})
	fixturekit.Must(t, err)
	valid := projectSpecDocument(t, wt)
	writeCorruptedProjectSpec(t, wt, workgroup, spec)
	defective := projectSpecDocument(t, wt)
	if defective.Revision != valid.Revision+1 {
		t.Fatalf("defective graph not recorded as the next revision: valid=%+v defective=%+v", valid, defective)
	}
	fixturekit.Must(t, wt.Close())

	if _, err := f.c.Ratify(ctx, f.stream1, packets[0].Packet.Revision.Spec, packets[0].Packet.Revision.Plan, defective.Revision); !intakefixture.Failed(err, api.Validation) || !strings.Contains(err.Error(), "project spec graph is invalid") {
		t.Fatalf("ratify did not refuse the defective graph: %v", err)
	}
	f.assertApprovalRefused(t, workgroup, defective)
}

func TestRatifyWorkgroupRefusesWhenAMemberHasNoSlice(t *testing.T) {
	t.Parallel()
	f := newCrossRepoRatifyFixture(t)
	defer f.stop(t)
	ctx := context.Background()
	_, apiFault := f.s.DraftWorkgroup(ctx, []config.WorkstreamID{f.stream1, f.stream2})
	if apiFault != nil {
		t.Fatalf("draft workgroup: %v", apiFault)
	}
	packets := f.awaitDecisionPackets(t)

	repo1, err := f.s.repository(f.project)
	fixturekit.Must(t, err)
	spec, workgroup, _, err := repo1.ProjectSpecOf(f.stream1)
	fixturekit.Must(t, err)
	g := trace.Workgroup{ID: workgroup, Members: []config.WorkstreamID{f.stream1, f.stream2}}
	spec.Slices = spec.Slices[:1]
	wt, err := trace.OpenWorkgroupTrace(f.s.current().Root, workgroup, []trace.CrossRepoMember{{Workstream: f.stream1, Project: repo1}, {Workstream: f.stream2, Project: mustRepo(t, f.s, f.project2)}})
	fixturekit.Must(t, err)
	valid := projectSpecDocument(t, wt)
	fixturekit.Must(t, trace.RecordProjectSpec(ctx, wt, g, spec, f.s.now(), intake.OwnerActor, "drop-slice"))
	defective := projectSpecDocument(t, wt)
	if defective.Revision != valid.Revision+1 {
		t.Fatalf("missing slice not recorded as the next revision: valid=%+v defective=%+v", valid, defective)
	}
	fixturekit.Must(t, wt.Close())

	current, _, found, err := repo1.ProjectSpecOf(f.stream1)
	fixturekit.Must(t, err)
	if !found || !intakefixture.EqualJSONTest(current, spec) || len(current.Slices) != 1 || current.Slices[0].Workstream != f.stream1 {
		t.Fatalf("ratify's project-spec lookup does not see the missing-slice revision: %+v", current)
	}
	if _, err := f.c.Ratify(ctx, f.stream1, packets[0].Packet.Revision.Spec, packets[0].Packet.Revision.Plan, defective.Revision); !intakefixture.Failed(err, api.Conflict) || !strings.Contains(err.Error(), "workstream "+string(f.stream2)+" has no slice") {
		t.Fatalf("ratify did not refuse the missing slice: %v", err)
	}
	f.assertApprovalRefused(t, workgroup, defective)
}

func TestRatifyWorkgroupInterruptedBetweenMembersCompletesOnRetry(t *testing.T) {
	t.Parallel()
	f := newProductionDesignFixture(t)
	defer f.stop(t)
	f.startOwner(t)
	packet := f.awaitPacket(t)
	hook := &intakefixture.DesignInterruption{Step: "group-member-decision-prepared", Nth: 1, Reached: make(chan struct{})}
	f.hook.Store(hook)
	finished := make(chan error, 1)
	go func() {
		_, err := f.c.Ratify(context.Background(), f.stream1, packet.Packet.Revision.Spec, packet.Packet.Revision.Plan, packet.ProjectSpecRevision)
		finished <- err
	}()
	select {
	case <-hook.Reached:
	case <-time.After(fixturekit.DemoTimeout):
		t.Fatal("member preparation was not interrupted")
	}
	f.assertBarrier(t)
	f.stop(t)
	<-finished
	f.start(t)
	f.awaitApproved(t)
	out, err := f.c.Ratify(context.Background(), f.stream1, packet.Packet.Revision.Spec, packet.Packet.Revision.Plan, packet.ProjectSpecRevision)
	fixturekit.Must(t, err)
	if len(out.Members) != 2 || out.Sealing != "succeeded" {
		t.Fatalf("retry: %+v", out)
	}
	f.awaitApproved(t)
}

func TestDraftWorkgroupRefusesReadinessWithNothingRecordedOnAnyMember(t *testing.T) {
	t.Parallel()
	f := newCrossRepoRatifyFixture(t)
	defer f.stop(t)
	ctx := context.Background()

	if _, err := f.c.Send(ctx, f.stream2, "Where do we stand?"); err != nil {
		t.Fatalf("send: %v", err)
	}

	if _, apiFault := f.s.DraftWorkgroup(ctx, []config.WorkstreamID{f.stream1, f.stream2}); apiFault == nil {
		t.Fatal("draft workgroup with a member that already has chief-of-staff turns was not refused")
	}

	repo1, err := f.s.repository(f.project)
	fixturekit.Must(t, err)
	repo2, err := f.s.repository(f.project2)
	fixturekit.Must(t, err)
	if id, err := repo1.CrossRepoWorkgroupOf(f.stream1); err != nil || id != "" {
		t.Fatalf("member1 was left pointing at a cross-repo workgroup after a refused readiness check: id=%q err=%v", id, err)
	}
	if id, err := repo2.CrossRepoWorkgroupOf(f.stream2); err != nil || id != "" {
		t.Fatalf("member2, which was ready, was still joined to a workgroup after the whole call was refused: id=%q err=%v", id, err)
	}
	groups1, _, err := trace.Workgroups(repo1)
	fixturekit.Must(t, err)
	if _, ok := trace.WorkgroupOf(groups1, f.stream1); !ok {
		t.Fatal("member1's own one-workstream workgroup was retracted despite the refused join")
	}
	groups2, _, err := trace.Workgroups(repo2)
	fixturekit.Must(t, err)
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

func TestDraftWorkgroupInterruptedBetweenMembersCompletesOnRetryWithNoSecondWorkgroup(t *testing.T) {
	t.Parallel()
	f := newCrossRepoRatifyFixture(t)
	defer f.stop(t)
	ctx := context.Background()
	repo1, err := f.s.repository(f.project)
	fixturekit.Must(t, err)
	repo2, err := f.s.repository(f.project2)
	fixturekit.Must(t, err)

	resolved := []trace.CrossRepoMember{{Workstream: f.stream1, Project: repo1}, {Workstream: f.stream2, Project: repo2}}
	f.s.boundary = func(name string) error {
		if name == "workgroup-member-joined" {
			return errors.New("interrupted between members")
		}
		return nil
	}
	_, apiFault := f.s.DraftWorkgroup(ctx, []config.WorkstreamID{f.stream1, f.stream2})
	if apiFault == nil {
		t.Fatal("join did not stop at the injected crash")
	}
	f.s.clearBoundary()
	joinedID, err := repo1.CrossRepoWorkgroupOf(f.stream1)
	fixturekit.Must(t, err)
	if joinedID == "" {
		t.Fatal("first member was not joined before interruption")
	}
	_, g, _, err := repo1.CrossRepoProjectSpec(joinedID)
	fixturekit.Must(t, err)
	if entries, apiFault := f.s.workgroupInbox(ctx, repo1, joinedID); apiFault != nil || len(entries) != 0 {
		t.Fatalf("half-joined workgroup inbox: %+v %v", entries, apiFault)
	}
	if _, err := f.s.crossRepoMemberRepos(ctx, g); !errors.Is(err, intake.ErrWorkgroupJoining) {
		t.Fatalf("half-joined workgroup became dispatchable: %v", err)
	}
	if ids, err := f.s.crossRepoWorkgroupIDs(ctx, repo1); err != nil || len(ids) != 0 {
		t.Fatalf("half-joined workgroup appeared in discovery: %v %v", ids, err)
	}
	if id, err := repo2.CrossRepoWorkgroupOf(f.stream2); err != nil || id != "" {
		t.Fatalf("member2 was already joined before the retry: id=%q err=%v", id, err)
	}

	id, apiFault := f.s.DraftWorkgroup(ctx, []config.WorkstreamID{f.stream1, f.stream2})
	if apiFault != nil {
		t.Fatalf("retry: %v", apiFault)
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
	fixturekit.Must(t, err)
	defer reopened.Close()
	groups, _, err := trace.Workgroups(reopened)
	fixturekit.Must(t, err)
	if len(groups) != 1 {
		t.Fatalf("the retry recorded more than one workgroup: %+v", groups)
	}
	entries, err := os.ReadDir(filepath.Join(f.s.current().Root.String(), "workgroups"))
	fixturekit.Must(t, err)
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
	if _, apiFault := f.s.DraftWorkgroup(context.Background(), members); apiFault == nil {
		t.Fatal("expected interruption")
	}
	f.s.clearBoundary()
	before, err := os.ReadDir(filepath.Join(f.s.current().Root.String(), "workgroups"))
	fixturekit.Must(t, err)
	if len(before) != 1 {
		t.Fatalf("workgroup traces: %v", before)
	}
	id, apiFault := f.s.DraftWorkgroup(context.Background(), members)
	if apiFault != nil || string(id) != before[0].Name() {
		t.Fatalf("retry did not reuse the pending trace: %s %v", id, apiFault)
	}
}

func TestCrossRepoWorkgroupSharesOneChiefOfStaffAcrossARestart(t *testing.T) {
	t.Parallel()
	f := newCrossRepoRatifyFixture(t)
	defer f.stop(t)
	ctx := context.Background()
	_, apiFault := f.s.DraftWorkgroup(ctx, []config.WorkstreamID{f.stream1, f.stream2})
	if apiFault != nil {
		t.Fatalf("draft workgroup: %v", apiFault)
	}
	f.awaitProjectSpec(t)

	sent1, err := f.c.Send(ctx, f.stream1, "Where does the acknowledgment live?")
	fixturekit.Must(t, err)

	repo1, err := f.s.repository(f.project)
	fixturekit.Must(t, err)
	if _, err := repo1.ChiefOfStaffThread(f.stream1); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("member1's own repository got a chief-of-staff thread of its own: %v", err)
	}
	repo2, err := f.s.repository(f.project2)
	fixturekit.Must(t, err)
	if _, err := repo2.ChiefOfStaffThread(f.stream2); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("member2's own repository got a chief-of-staff thread of its own: %v", err)
	}

	sent2, err := f.c.Send(ctx, f.stream2, "Does the client ever see the offset?")
	fixturekit.Must(t, err)
	checkSharedConversation := func(t *testing.T) {
		t.Helper()
		for _, m := range []config.WorkstreamID{f.stream1, f.stream2} {
			list, err := f.c.Conversation(ctx, m)
			fixturekit.Must(t, err)
			if len(list.Entries) != 2 || list.Entries[0].Turn != sent1.Turn || list.Entries[1].Turn != sent2.Turn {
				t.Fatalf("workstream %s's shared conversation: %+v", m, list.Entries)
			}
		}
	}
	checkSharedConversation(t)

	f.stop(t)
	f.start(t)
	checkSharedConversation(t)
	repo1, err = f.s.repository(f.project)
	fixturekit.Must(t, err)
	if _, err := repo1.ChiefOfStaffThread(f.stream1); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("member1's own repository got a chief-of-staff thread after restart: %v", err)
	}
	repo2, err = f.s.repository(f.project2)
	fixturekit.Must(t, err)
	if _, err := repo2.ChiefOfStaffThread(f.stream2); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("member2's own repository got a chief-of-staff thread after restart: %v", err)
	}
}
func mustRepo(t *testing.T, s *Service, project config.ProjectID) *trace.Repository {
	t.Helper()
	repo, err := s.repository(project)
	fixturekit.Must(t, err)
	return repo
}

func writeCorruptedProjectSpec(t *testing.T, wt *trace.Repository, workgroup config.WorkgroupID, spec trace.ProjectSpec) {
	t.Helper()
	fixturekit.CorruptProjectSpec(t, wt, workgroup, spec, intake.OwnerActor)
}
