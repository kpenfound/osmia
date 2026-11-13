package trace

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/kpenfound/osmia/internal/config"
)

// approvalFixture is a cross-repo workgroup whose two members point at it
// and whose first member is at a feature state.
type approvalFixture struct {
	root     config.Root
	r1, r2   *Repository
	p1, p2   config.Project
	wt       *Repository
	g        Workgroup
	host     config.WorkstreamID
	approval GroupApproval
}

func newApprovalFixture(t *testing.T, feature string, size int) *approvalFixture {
	t.Helper()
	ctx := context.Background()
	root, r1, r2, p1, p2 := crossRepoFixture(t)
	wt, g := createCrossRepoWorkgroup(t, root, r1, r2)
	for _, m := range []struct {
		repo   *Repository
		stream config.WorkstreamID
	}{{r1, crossMember1}, {r2, crossMember2}} {
		if err := m.repo.RecordCrossRepoWorkgroup(ctx, m.stream, g.ID, at, owner, "join"); err != nil {
			t.Fatal(err)
		}
	}
	h := Header{Schema: "osmia.trace.transition", Version: Version, ID: feature, Revision: 1, Project: crossProject1, Workstream: crossMember1, At: at, Actor: owner, Cause: "test"}
	if _, err := r1.SetFeatureState(ctx, h, feature, "test"); err != nil {
		t.Fatal(err)
	}
	spec := ProjectSpec{
		Behavior:  strings.Repeat("Shared behavior. ", size),
		Contracts: []Contract{{Name: "api", Text: "contract"}},
		Graph:     []Edge{{From: crossMember1, To: crossMember2, Type: EdgeMergeOrder}},
		Slices:    []Slice{{Workstream: crossMember1, ContractCriteria: []int{1, 2}}, {Workstream: crossMember2}},
	}
	approval := GroupApproval{Workgroup: g.ID, ProjectSpec: spec, Members: []GroupMemberApproval{
		{Project: crossProject1, Workstream: crossMember1, Record: json.RawMessage(`{"a":1}`)},
		{Project: crossProject2, Workstream: crossMember2, Record: json.RawMessage(`{"b":2}`)},
	}}
	return &approvalFixture{root: root, r1: r1, r2: r2, p1: p1, p2: p2, wt: wt, g: g, host: g.Members[0], approval: approval}
}

func (f *approvalFixture) record(t *testing.T, complete bool) {
	t.Helper()
	f.approval.Complete = complete
	if err := RecordGroupApproval(context.Background(), f.wt, f.host, f.approval, at, owner); err != nil {
		t.Fatal(err)
	}
}

func feature(t *testing.T, r *Repository, stream config.WorkstreamID) string {
	t.Helper()
	states, err := r.WorkflowStates(stream)
	if err != nil {
		t.Fatal(err)
	}
	return states[FeatureSubject].Value
}

func TestRepeatedWorkflowStatesDecodeApprovalOncePerRevision(t *testing.T) {
	f := newApprovalFixture(t, "building", 20000)
	f.record(t, false)
	f.record(t, true)
	decodes, opens := GroupApprovalDecodeCount(f.g.ID), WorkgroupTraceOpenCount(f.g.ID)
	for range 200 {
		if got := feature(t, f.r1, crossMember1); got != "building" {
			t.Fatalf("feature state %q", got)
		}
	}
	if n := GroupApprovalDecodeCount(f.g.ID) - decodes; n > 1 {
		t.Fatalf("200 reads decoded the approval %d times", n)
	}
	if n := WorkgroupTraceOpenCount(f.g.ID) - opens; n > 1 {
		t.Fatalf("200 reads acquired the workgroup trace %d times", n)
	}
	decodes = GroupApprovalDecodeCount(f.g.ID)
	for range 50 {
		if ok, err := f.r1.GroupApproved(crossMember1); err != nil || !ok {
			t.Fatalf("approved=%v err=%v", ok, err)
		}
		if _, found, err := f.r1.GroupApprovalOf(crossMember1); err != nil || !found {
			t.Fatalf("found=%v err=%v", found, err)
		}
		if required, err := f.r2.GroupApprovalRequired(crossMember2); err != nil || !required {
			t.Fatalf("required=%v err=%v", required, err)
		}
		if _, found, err := f.r1.groupApproval(f.g.ID); err != nil || !found {
			t.Fatalf("found=%v err=%v", found, err)
		}
	}
	if n := GroupApprovalDecodeCount(f.g.ID) - decodes; n != 0 {
		t.Fatalf("unchanged revision decoded the approval %d times", n)
	}
}

func TestApprovalCompletionIsSeenOnTheNextWorkflowStatesCall(t *testing.T) {
	f := newApprovalFixture(t, SealPreparedState, 10)
	for range 3 {
		if got := feature(t, f.r1, crossMember1); got != "in-shed" {
			t.Fatalf("no approval: %q", got)
		}
	}
	f.record(t, false)
	for range 3 {
		if got := feature(t, f.r1, crossMember1); got != "in-shed" {
			t.Fatalf("incomplete approval: %q", got)
		}
	}
	before := GroupApprovalDecodeCount(f.g.ID)
	f.record(t, true)
	if got := feature(t, f.r1, crossMember1); got != "ratified" {
		t.Fatalf("complete approval: %q", got)
	}
	for range 3 {
		if got := feature(t, f.r1, crossMember1); got != "ratified" {
			t.Fatalf("complete approval: %q", got)
		}
	}
	// RecordGroupApproval reads the document itself; the cache adds one decode.
	if n := GroupApprovalDecodeCount(f.g.ID) - before; n > 2 {
		t.Fatalf("completion decoded the approval %d times", n)
	}
}

func TestWorkgroupsRevisionChangesApprovalRequirementOnNextRead(t *testing.T) {
	f := newApprovalFixture(t, "building", 10)
	f.record(t, false)
	for range 3 {
		if required, err := f.r1.GroupApprovalRequired(crossMember1); err != nil || !required {
			t.Fatalf("required=%v err=%v", required, err)
		}
		if ok, err := f.r1.GroupApproved(crossMember1); err != nil || ok {
			t.Fatalf("approved=%v err=%v", ok, err)
		}
	}
	groups, revision, err := Workgroups(f.wt)
	if err != nil {
		t.Fatal(err)
	}
	next := groups[0]
	next.Members = next.Members[:2]
	next.Conversation = &ConversationOrigin{Project: crossProject1, Workstream: crossMember1}
	data, err := json.Marshal(workgroupsFile{Version: 1, Workgroups: []Workgroup{next}})
	if err != nil {
		t.Fatal(err)
	}
	doc := Document{Header: Header{Schema: "osmia.trace.document", Version: Version, ID: WorkgroupsDocument, Revision: revision + 1, Project: f.wt.Project(), At: at, Actor: owner, Cause: "test"}, Path: WorkgroupsPath, Content: string(data)}
	if err := f.wt.Append(context.Background(), doc); err != nil {
		t.Fatal(err)
	}
	if required, err := f.r1.GroupApprovalRequired(crossMember1); err != nil || required {
		t.Fatalf("required=%v err=%v", required, err)
	}
	if ok, err := f.r1.GroupApproved(crossMember1); err != nil || !ok {
		t.Fatalf("approved=%v err=%v", ok, err)
	}
}

func TestColdRestartRebuildsApprovalStateFromTheTrace(t *testing.T) {
	ctx := context.Background()
	f := newApprovalFixture(t, "building", 10)
	f.record(t, false)
	f.record(t, true)
	want, wantFound, wantRequired, err := f.r1.groupApprovalState(f.g.ID)
	if err != nil || !wantFound || !want.Complete {
		t.Fatalf("approval=%+v found=%v err=%v", want, wantFound, err)
	}
	if !GroupApprovalCached(f.g.ID) {
		t.Fatal("approval answer was not cached")
	}
	oldTrace := f.wt
	// Every holder closes: the test's workgroup handle and both members.
	for _, r := range []*Repository{f.wt, f.r1, f.r2} {
		if err := r.Close(); err != nil {
			t.Fatal(err)
		}
	}
	if GroupApprovalCached(f.g.ID) {
		t.Fatal("registry still serves the cached approval after every holder closed")
	}
	if shared := sharedWorkgroupTraceFor(f.g.ID); shared.repo != nil || shared.refs != 0 {
		t.Fatalf("registry still holds the trace: refs=%d", shared.refs)
	}
	directory, err := f.root.WorkgroupTrace(f.g.ID)
	if err != nil {
		t.Fatal(err)
	}
	probe, err := openStoreDirectory(f.root, crossRepoProject(f.g.ID), "", directory)
	if err != nil {
		t.Fatal(err)
	}
	if err := probe.acquire(); err != nil {
		t.Fatalf("workgroup trace lock still held: %v", err)
	}
	probe.Close()

	r1, err := Open(f.root, f.p1)
	if err != nil {
		t.Fatal(err)
	}
	defer r1.Close()
	r2, err := Open(f.root, f.p2)
	if err != nil {
		t.Fatal(err)
	}
	defer r2.Close()
	wt, err := OpenWorkgroupTrace(f.root, f.g.ID, []CrossRepoMember{{crossMember1, r1}, {crossMember2, r2}})
	if err != nil {
		t.Fatal(err)
	}
	defer wt.Close()
	if wt == oldTrace {
		t.Fatal("reopen reused the closed trace handle")
	}
	if wt.members[crossMember1].Project != r1 || wt.members[crossMember2].Project != r2 {
		t.Fatal("reopened trace kept stale member bindings")
	}
	decodes := GroupApprovalDecodeCount(f.g.ID)
	got, found, required, err := r1.groupApprovalState(f.g.ID)
	if err != nil || found != wantFound || required != wantRequired || !reflect.DeepEqual(got, want) {
		t.Fatalf("after restart: %+v %v %v %v", got, found, required, err)
	}
	if n := GroupApprovalDecodeCount(f.g.ID) - decodes; n != 1 {
		t.Fatalf("restart rebuilt the approval with %d decodes", n)
	}
	if got := feature(t, r1, crossMember1); got != "building" {
		t.Fatalf("feature after restart: %q", got)
	}
	// A revision recorded after the reopen is seen on the next read.
	changed := want
	changed.ProjectSpec.Behavior += " Changed."
	if err := RecordGroupApproval(ctx, wt, f.host, changed, at, owner); !errors.Is(err, ErrConflict) {
		t.Fatalf("immutable decision changed: %v", err)
	}
	groups, revision, err := Workgroups(wt)
	if err != nil {
		t.Fatal(err)
	}
	next := groups[0]
	next.Members = next.Members[:2]
	next.Conversation = &ConversationOrigin{Project: crossProject1, Workstream: crossMember1}
	data, err := json.Marshal(workgroupsFile{Version: 1, Workgroups: []Workgroup{next}})
	if err != nil {
		t.Fatal(err)
	}
	if err := wt.Append(ctx, Document{Header: Header{Schema: "osmia.trace.document", Version: Version, ID: WorkgroupsDocument, Revision: revision + 1, Project: wt.Project(), At: at, Actor: owner, Cause: "test"}, Path: WorkgroupsPath, Content: string(data)}); err != nil {
		t.Fatal(err)
	}
	if required, err := r1.GroupApprovalRequired(crossMember1); err != nil || required {
		t.Fatalf("revision after reopen not seen: required=%v err=%v", required, err)
	}
}

func TestApprovalHandlesAreReleasedByTheirOwner(t *testing.T) {
	f := newApprovalFixture(t, "building", 10)
	f.record(t, true)
	if _, err := f.r1.GroupApproved(crossMember1); err != nil {
		t.Fatal(err)
	}
	if err := f.wt.Close(); err != nil {
		t.Fatal(err)
	}
	// The member repository still holds the trace for its approval reads.
	shared := sharedWorkgroupTraceFor(f.g.ID)
	shared.Lock()
	refs := shared.refs
	shared.Unlock()
	if refs != 1 {
		t.Fatalf("member holds %d references", refs)
	}
	if ok, err := f.r1.GroupApproved(crossMember1); err != nil || !ok {
		t.Fatalf("approved=%v err=%v", ok, err)
	}
	if err := f.r1.Close(); err != nil {
		t.Fatal(err)
	}
	if GroupApprovalCached(f.g.ID) {
		t.Fatal("cached approval outlived its owner")
	}
	directory, err := f.root.WorkgroupTrace(f.g.ID)
	if err != nil {
		t.Fatal(err)
	}
	probe, err := openStoreDirectory(f.root, crossRepoProject(f.g.ID), "", directory)
	if err != nil {
		t.Fatal(err)
	}
	if err := probe.acquire(); err != nil {
		t.Fatalf("lock held after owner closed: %v", err)
	}
	probe.Close()
	// A closed owner reads without keeping anything.
	if ok, err := f.r1.GroupApproved(crossMember1); err == nil {
		t.Logf("read on a closed member: approved=%v", ok)
	}
	shared.Lock()
	refs = shared.refs
	shared.Unlock()
	if refs != 0 {
		t.Fatalf("closed member kept %d references", refs)
	}
}

func TestReturnedApprovalBelongsToTheCaller(t *testing.T) {
	f := newApprovalFixture(t, "building", 10)
	f.record(t, false)
	f.record(t, true)
	want, _, _, err := f.r1.groupApprovalState(f.g.ID)
	if err != nil {
		t.Fatal(err)
	}
	mutate := func(a GroupApproval) {
		a.Members[0].Record[0] = 'X'
		a.Members[0].Workstream = "w_ffffffffffffffffffffffffffffffff"
		a.ProjectSpec.Contracts[0].Text = "changed"
		a.ProjectSpec.Graph[0].Type = EdgeHardBlock
		a.ProjectSpec.Slices[0].ContractCriteria[0] = 99
		a.ProjectSpec.Slices[1].Spec = "changed"
	}
	first, _, err := f.r1.GroupApprovalOf(crossMember1)
	if err != nil {
		t.Fatal(err)
	}
	mutate(first)
	second, _, err := f.r2.GroupApprovalOf(crossMember2)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(second, want) {
		t.Fatalf("a caller changed later reads:\n%+v\n%+v", second, want)
	}
	read, _, _, err := ReadGroupApproval(f.wt, f.host)
	if err != nil || !reflect.DeepEqual(read, want) {
		t.Fatalf("recorded approval changed: %+v %v", read, err)
	}
}

func TestConcurrentApprovalReadsShareOneDecode(t *testing.T) {
	f := newApprovalFixture(t, "building", 5000)
	f.record(t, true)
	GroupApprovalCached(f.g.ID)
	decodes := GroupApprovalDecodeCount(f.g.ID)
	var wg sync.WaitGroup
	for i := range 16 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r, stream := f.r1, crossMember1
			if i%2 == 1 {
				r, stream = f.r2, crossMember2
			}
			for range 50 {
				a, found, err := r.GroupApprovalOf(stream)
				if err != nil || !found || !a.Complete {
					t.Errorf("approval=%v found=%v err=%v", a.Complete, found, err)
					return
				}
				a.ProjectSpec.Slices[0].ContractCriteria[0]++
				if ok, err := r.GroupApproved(stream); err != nil || !ok {
					t.Errorf("approved=%v err=%v", ok, err)
					return
				}
			}
		}()
	}
	wg.Wait()
	if n := GroupApprovalDecodeCount(f.g.ID) - decodes; n > 1 {
		t.Fatalf("concurrent readers decoded the approval %d times", n)
	}
}

func TestDamagedLatestApprovalIsReportedAfterAnEarlierRevisionWasCached(t *testing.T) {
	for _, damage := range []string{"outer-json", "content-json", "stored-revision"} {
		t.Run(damage, func(t *testing.T) {
			f := newApprovalFixture(t, "building", 10)
			f.record(t, false)
			for range 2 {
				if _, found, err := f.r1.GroupApprovalOf(crossMember1); err != nil || !found {
					t.Fatalf("found=%v err=%v", found, err)
				}
			}
			f.approval.Complete = true
			data, err := json.Marshal(f.approval)
			if err != nil {
				t.Fatal(err)
			}
			d := Document{Header: Header{Schema: "osmia.trace.document", Version: Version, ID: GroupApprovalDocument, Revision: 2, Project: f.wt.Project(), Workstream: f.host, At: at, Actor: owner, Cause: GroupApprovalDocument}, Path: GroupApprovalPath, Content: string(data)}
			if err := f.wt.RecordDocuments(context.Background(), []Document{d}); err != nil {
				t.Fatal(err)
			}
			body := "{"
			switch damage {
			case "content-json":
				d.Content = "{"
			case "stored-revision":
				d.Revision = 1
			}
			if damage != "outer-json" {
				encoded, err := json.Marshal(d)
				if err != nil {
					t.Fatal(err)
				}
				body = string(encoded)
			}
			scoped := mustScoped(t, f.wt, f.host)
			if _, err := scoped.db.Exec("UPDATE records SET body = ? WHERE id = ? AND revision = 2", body, GroupApprovalDocument); err != nil {
				t.Fatal(err)
			}
			if _, _, err := f.r1.GroupApprovalOf(crossMember1); err == nil {
				t.Fatal("damaged latest approval accepted")
			}
			if _, err := f.r1.GroupApproved(crossMember1); err == nil {
				t.Fatal("damaged latest approval accepted")
			}
			if _, err := f.r1.WorkflowStates(crossMember1); err == nil {
				t.Fatal("damaged latest approval accepted by WorkflowStates")
			}
		})
	}
}
