package lifecycle_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"
	"testing"

	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	"github.com/kpenfound/osmia/internal/servicetest/journey"

	"github.com/kpenfound/osmia/internal/api"
	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/intake"
	"github.com/kpenfound/osmia/internal/lifecycle"
	"github.com/kpenfound/osmia/internal/plan"
	"github.com/kpenfound/osmia/internal/runtime"
	"github.com/kpenfound/osmia/internal/servicetest/intakefixture"
	"github.com/kpenfound/osmia/internal/shed"
	"github.com/kpenfound/osmia/internal/trace"
)

func projectSliceRefreshFixture(t *testing.T) (*journey.CrossRepoRatifyFixture, config.WorkgroupID, [2]api.PacketResponse) {
	t.Helper()
	f := newCrossRepoRatifyFixture(t)
	t.Cleanup(func() { f.Stop(t) })
	id, apiFault := f.S.Lifecycle().DraftWorkgroup(context.Background(), []config.WorkstreamID{f.Stream1, f.Stream2})
	if apiFault != nil {
		t.Fatal(apiFault)
	}
	packets := awaitDecisionPackets(f, t)
	_, projects := f.S.Runtimes()
	for _, p := range projects {
		fixturekit.Must(t, f.S.Halt(p))
	}
	return f, id, packets
}
func reviseRefreshMember(t *testing.T, f *journey.CrossRepoRatifyFixture, member config.WorkstreamID, document, content string) {
	t.Helper()
	_, _, repo, apiFault := journey.InspectForTest(f.S).ConversationTrace(string(member))
	if apiFault != nil {
		t.Fatal(apiFault)
	}
	pin, err := intake.LatestPin(repo, member)
	fixturekit.Must(t, err)
	revision := pin.Spec
	if document == plan.PlanDocument {
		revision = pin.Plan
	}
	doc, err := trace.Get[trace.Document](repo, member, document, revision)
	fixturekit.Must(t, err)
	doc.Revision++
	doc.Content = content
	doc.Cause = "member-revision"
	fixturekit.Must(t, repo.RecordDocuments(context.Background(), []trace.Document{doc}))
	fixturekit.Must(t, (&intake.Debate{S: f.S.Intake().DebateDependencies(), Repository: repo}).PresentPacket(context.Background(), member, true))
}
func runRefresh(t *testing.T, f *journey.CrossRepoRatifyFixture) {
	t.Helper()
	for _, project := range []config.ProjectID{f.Project, f.Project2} {
		fixturekit.Must(t, f.S.Lifecycle().ProjectSpecRefreshPass(context.Background(), mustRepo(t, f.S, project)))
	}
}
func refreshDocument(t *testing.T, f *journey.CrossRepoRatifyFixture, id config.WorkgroupID) trace.Document {
	t.Helper()
	wt, err := trace.OpenWorkgroupAuthority(f.S.Current().Root, id)
	fixturekit.Must(t, err)
	defer wt.Close()
	return projectSpecDocument(t, wt)
}
func TestProjectSpecRefreshPreservesDesignAndBindsOwnerRead(t *testing.T) {
	t.Parallel()
	f, id, packets := projectSliceRefreshFixture(t)
	ctx := context.Background()
	old := refreshDocument(t, f, id)
	runRefresh(t, f)
	if got := refreshDocument(t, f, id); !reflect.DeepEqual(got, old) {
		t.Fatal("matching slices appended a revision")
	}
	reviseRefreshMember(t, f, f.Stream2, plan.SpecDocument, intakefixture.CrossSpec2+"\nUploads show progress.\n")
	reviseRefreshMember(t, f, f.Stream2, plan.PlanDocument, intakefixture.CrossPlan2+"\n")
	for _, revision := range []int{0, packets[0].ProjectSpecRevision} {
		_, err := f.C.Ratify(ctx, f.Stream1, packets[0].Packet.Revision.Spec, packets[0].Packet.Revision.Plan, revision)
		if !intakefixture.Failed(err, api.Conflict) || !containsMessage(err, intake.ProjectSpecCatchingUp) {
			t.Fatalf("behind slice / missing revision: %v", err)
		}
		assertApprovalRefused(f, t, id, old)
	}
	runRefresh(t, f)
	fresh := refreshDocument(t, f, id)
	if fresh.Revision != old.Revision+1 {
		t.Fatalf("refresh revisions: %d -> %d", old.Revision, fresh.Revision)
	}
	runRefresh(t, f)
	if got := refreshDocument(t, f, id); !reflect.DeepEqual(got, fresh) {
		t.Fatal("repeated refresh appended a revision")
	}
	packet, err := f.C.Packet(ctx, f.Stream1)
	fixturekit.Must(t, err)
	if packet.ProjectSpecRevision != fresh.Revision {
		t.Fatalf("packet revision %d != %d", packet.ProjectSpecRevision, fresh.Revision)
	}
	expected := *packets[0].ProjectSpec
	expected.Slices = append([]trace.Slice(nil), expected.Slices...)
	for i := range expected.Slices {
		if expected.Slices[i].Workstream == f.Stream2 {
			expected.Slices[i].Spec = intakefixture.CrossSpec2 + "\nUploads show progress.\n"
			expected.Slices[i].Plan = intakefixture.CrossPlan2 + "\n"
		}
	}
	if !reflect.DeepEqual(*packet.ProjectSpec, expected) {
		t.Fatalf("refresh changed design or missed member text: %+v", packet.ProjectSpec)
	}
	wt, err := trace.OpenWorkgroupAuthority(f.S.Current().Root, id)
	fixturekit.Must(t, err)
	history, err := trace.Get[trace.Document](wt, "", trace.ProjectSpecsDocument, old.Revision)
	fixturekit.Must(t, err)
	fixturekit.Must(t, wt.Close())
	if !reflect.DeepEqual(history, old) {
		t.Fatal("refresh changed history")
	}
	_, err = f.C.Ratify(ctx, f.Stream1, packets[0].Packet.Revision.Spec, packets[0].Packet.Revision.Plan, packets[0].ProjectSpecRevision)
	if !intakefixture.Failed(err, api.Conflict) || !containsMessage(err, intake.ProjectSpecCatchingUp) {
		t.Fatalf("stale other-member read: %v", err)
	}
	assertApprovalRefused(f, t, id, fresh)
	_, err = f.C.Ratify(ctx, f.Stream1, packet.Packet.Revision.Spec, packet.Packet.Revision.Plan, packet.ProjectSpecRevision)
	fixturekit.Must(t, err)
	repo := mustRepo(t, f.S, f.Project)
	approved, found, err := repo.GroupApprovalOf(f.Stream1)
	fixturekit.Must(t, err)
	if !found || approved.ProjectSpecRevision != fresh.Revision || !reflect.DeepEqual(approved.ProjectSpec, *packet.ProjectSpec) {
		t.Fatalf("approval did not pin displayed revision: %+v", approved)
	}
	effective, _, _, err := repo.ProjectSpecOf(f.Stream1)
	fixturekit.Must(t, err)
	reviseRefreshMember(t, f, f.Stream2, plan.SpecDocument, intakefixture.CrossSpec2+"\nAnother edit.\n")
	runRefresh(t, f)
	if got := refreshDocument(t, f, id); !reflect.DeepEqual(got, fresh) {
		t.Fatal("approved project spec refreshed")
	}
	after, _, _, err := repo.ProjectSpecOf(f.Stream1)
	fixturekit.Must(t, err)
	if !reflect.DeepEqual(after, effective) {
		t.Fatal("approved-revision read changed")
	}
	decision, _, err := repo.GroupApprovalOf(f.Stream1)
	fixturekit.Must(t, err)
	if !reflect.DeepEqual(decision, approved) {
		t.Fatal("refresh changed approval")
	}
}
func containsMessage(err error, message string) bool {
	return err != nil && strings.Contains(err.Error(), message)
}
func TestProjectSpecRefreshDefersHeldMembersAndTrace(t *testing.T) {
	t.Parallel()
	for _, hold := range []string{"locked", "joining", "retired", "canceled"} {
		t.Run(hold, func(t *testing.T) {
			t.Parallel()
			f, id, _ := projectSliceRefreshFixture(t)
			old := refreshDocument(t, f, id)
			reviseRefreshMember(t, f, f.Stream2, plan.SpecDocument, intakefixture.CrossSpec2+"\nProgress.\n")
			ctx := context.Background()
			unlock := func() {}
			switch hold {
			case "locked":
				fixturekit.Must(t, mustRepo(t, f.S, f.Project).ReleaseWorkgroupTraces())
				fixturekit.Must(t, mustRepo(t, f.S, f.Project2).ReleaseWorkgroupTraces())
				directory, err := f.S.Current().Root.WorkgroupTrace(id)
				fixturekit.Must(t, err)
				lock, err := os.OpenFile(filepath.Join(directory, "osmia.lock"), os.O_RDWR, 0600)
				fixturekit.Must(t, err)
				fixturekit.Must(t, syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB))
				unlock = func() { fixturekit.Must(t, syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)) }
				defer lock.Close()
			case "joining":
				_, err := trace.CreateWorkgroup(ctx, mustRepo(t, f.S, f.Project2), []config.WorkstreamID{f.Stream2}, f.S.Now(), intake.OwnerActor, "joining")
				fixturekit.Must(t, err)
			case "retired":
				fixturekit.Must(t, f.S.Store.SetArchived(runtime.Archive{Project: f.Project2, Workstream: f.Stream2, Workgroup: id, ArchivedAt: f.S.Now()}))
			case "canceled":
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			}
			for _, project := range []config.ProjectID{f.Project, f.Project2} {
				err := f.S.Lifecycle().ProjectSpecRefreshPass(ctx, mustRepo(t, f.S, project))
				if hold == "canceled" {
					if !errors.Is(err, context.Canceled) {
						t.Fatalf("cancellation: %v", err)
					}
				} else {
					fixturekit.Must(t, err)
				}
			}
			unlock()
			if got := refreshDocument(t, f, id); !reflect.DeepEqual(got, old) {
				t.Fatal("held member changed slices")
			}
		})
	}
}
func TestProjectSpecRefreshSerializesWithValidatedRatification(t *testing.T) {
	t.Parallel()
	f, id, packets := projectSliceRefreshFixture(t)
	old := refreshDocument(t, f, id)
	injected := false
	f.S.Boundary = func(step string) error {
		if step != "group-ratify-validated" {
			return nil
		}
		injected = true
		packet, err := f.C.Packet(context.Background(), f.Stream1)
		fixturekit.Must(t, err)
		if packet.ProjectSpecRevision != packets[0].ProjectSpecRevision {
			t.Fatal("packet read changed while ratification held the approval lock")
		}
		reviseRefreshMember(t, f, f.Stream2, plan.SpecDocument, intakefixture.CrossSpec2+"\nProgress.\n")
		runRefresh(t, f)
		return nil
	}
	defer func() { f.S.ClearBoundary() }()
	p := packets[0]
	_, err := f.C.Ratify(context.Background(), f.Stream1, p.Packet.Revision.Spec, p.Packet.Revision.Plan, p.ProjectSpecRevision)
	fixturekit.Must(t, err)
	if !injected {
		t.Fatal("validation boundary not reached")
	}
	runRefresh(t, f)
	if got := refreshDocument(t, f, id); !reflect.DeepEqual(got, old) {
		t.Fatal("racing refresh changed validated spec")
	}
	approval, found, err := mustRepo(t, f.S, f.Project).GroupApprovalOf(f.Stream1)
	fixturekit.Must(t, err)
	if !found || approval.ProjectSpecRevision != p.ProjectSpecRevision || !reflect.DeepEqual(approval.ProjectSpec, *p.ProjectSpec) {
		t.Fatalf("approval disagrees with validated revision: %+v", approval)
	}
	for _, m := range approval.Members {
		if m.Spec != 1 || m.Plan != 1 {
			t.Fatalf("approval pins were reread after validation: %+v", m)
		}
	}
}
func TestRecoverMemberApprovalsMatchesRefreshedSlices(t *testing.T) {
	t.Parallel()
	f, id, _ := projectSliceRefreshFixture(t)
	reviseRefreshMember(t, f, f.Stream2, plan.SpecDocument, intakefixture.CrossSpec2+"\nProgress.\n")
	runRefresh(t, f)
	for _, member := range []config.WorkstreamID{f.Stream1, f.Stream2} {
		o, apiFault := f.S.ShedAction(string(member), intake.InShedState, intake.SketchedState)
		if apiFault != nil {
			t.Fatal(apiFault)
		}
		_, apiFault = journey.InspectForTest(f.S).RatifyMember(context.Background(), o, o.Pin)
		if apiFault != nil {
			t.Fatal(apiFault)
		}
	}
	wt, g, err := journey.InspectForTest(f.S).ApprovalTrace(context.Background(), (mustRepo(t, f.S, f.Project)), id)
	fixturekit.Must(t, err)
	defer wt.Close()
	approval, found, err := journey.InspectForTest(f.S).RecoverMemberApprovals(context.Background(), wt, g)
	fixturekit.Must(t, err)
	if !found || approval.ProjectSpecRevision != 2 {
		t.Fatalf("refreshed approval not recovered: %+v, found=%v", approval, found)
	}
	for _, member := range approval.Members {
		repo, err := journey.InspectForTest(f.S).ApprovalMemberRepository(context.Background(), member.Project)
		fixturekit.Must(t, err)
		matches, err := intake.SliceMatchesPin(approval.ProjectSpec, repo, member.Workstream, shed.Pin{Spec: member.Spec, Plan: member.Plan})
		fixturekit.Must(t, err)
		if !matches {
			t.Fatal("recovery accepted mismatched slice")
		}
	}
}
func TestProjectSpecRefreshRetriesInterruptedAppend(t *testing.T) {
	t.Parallel()
	f, id, _ := projectSliceRefreshFixture(t)
	old := refreshDocument(t, f, id)
	reviseRefreshMember(t, f, f.Stream2, plan.SpecDocument, intakefixture.CrossSpec2+"\nProgress.\n")
	interrupted := errors.New("refresh interrupted after reading members")
	f.S.Boundary = func(step string) error {
		if step == "project-spec-refresh-read" {
			return interrupted
		}
		return nil
	}
	leader := lifecycle.LeastWorkstream([]config.WorkstreamID{f.Stream1, f.Stream2})
	_, _, repo, apiFault := journey.InspectForTest(f.S).ConversationTrace(string(leader))
	if apiFault != nil {
		t.Fatal(apiFault)
	}
	err := f.S.Lifecycle().ProjectSpecRefreshPass(context.Background(), repo)
	if !errors.Is(err, interrupted) {
		t.Fatalf("read boundary: %v", err)
	}
	f.S.ClearBoundary()
	if got := refreshDocument(t, f, id); !reflect.DeepEqual(got, old) {
		t.Fatal("interrupted refresh wrote a revision")
	}
	runRefresh(t, f)
	runRefresh(t, f)
	if got := refreshDocument(t, f, id); got.Revision != old.Revision+1 {
		t.Fatalf("retry appended %d revisions", got.Revision-old.Revision)
	}
}
func TestProjectSpecPacketRetainsApprovalWithoutExplicitRevisionPin(t *testing.T) {
	t.Parallel()
	f, id, packets := projectSliceRefreshFixture(t)
	repo := mustRepo(t, f.S, f.Project)
	wt, g, err := journey.InspectForTest(f.S).ApprovalTrace(context.Background(), repo, id)
	fixturekit.Must(t, err)
	defer wt.Close()
	decision := trace.GroupApproval{Workgroup: id, ProjectSpec: *packets[0].ProjectSpec}
	for i, member := range []config.WorkstreamID{f.Stream1, f.Stream2} {
		p := packets[i]
		record, err := shed.EncodeRatification(shed.Ratify(p.Packet.Round, p.Packet.Revision, nil))
		fixturekit.Must(t, err)
		decision.Members = append(decision.Members, trace.GroupMemberApproval{Project: p.Project, Workstream: member, Spec: p.Packet.Revision.Spec, Plan: p.Packet.Revision.Plan, Round: p.Packet.Round, Record: record})
	}
	fixturekit.Must(t, trace.RecordGroupApproval(context.Background(), wt, g.Members[0], decision, f.S.Now(), intake.OwnerActor))
	recorded, approvalDoc, found, err := trace.ReadGroupApproval(wt, g.Members[0])
	fixturekit.Must(t, err)
	if !found {
		t.Fatal("approval was not recorded")
	}
	later := decision.ProjectSpec
	later.Behavior = "A later unapproved draft."
	fixturekit.Must(t, trace.RecordProjectSpec(context.Background(), wt, g, later, f.S.Now(), intake.OwnerActor, "later-draft"))
	packet, err := f.C.Packet(context.Background(), f.Stream1)
	fixturekit.Must(t, err)
	if packet.ProjectSpecRevision != packets[0].ProjectSpecRevision || !reflect.DeepEqual(*packet.ProjectSpec, decision.ProjectSpec) {
		t.Fatalf("packet lost immutable approval's recorded revision: %+v", packet)
	}
	_, err = f.C.Ratify(context.Background(), f.Stream1, packet.Packet.Revision.Spec, packet.Packet.Revision.Plan, packet.ProjectSpecRevision)
	fixturekit.Must(t, err)
	after, afterDoc, found, err := trace.ReadGroupApproval(wt, g.Members[0])
	fixturekit.Must(t, err)
	if !found || !reflect.DeepEqual(after, recorded) || !reflect.DeepEqual(afterDoc, approvalDoc) {
		t.Fatal("retry changed existing approval")
	}
}
