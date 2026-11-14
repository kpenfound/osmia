package trace

import (
	"context"
	"encoding/json"
	"reflect"
	"sync"
	"testing"

	"github.com/kpenfound/osmia/internal/config"
)

func specVariant(label string) ProjectSpec {
	return ProjectSpec{
		Behavior:  "Behavior " + label,
		Contracts: []Contract{{Name: "api-" + label, Text: "contract " + label}},
		Graph: []Edge{
			{From: crossMember1, To: crossMember2, Type: EdgeMergeOrder},
			{From: crossMember1, To: crossMember2, Type: EdgeContract},
			{From: crossMember1, To: crossMember2, Type: EdgeHardBlock},
		},
		Slices: []Slice{{Workstream: crossMember1, Spec: label, ContractCriteria: []int{1, 2}}, {Workstream: crossMember2, ContractCriteria: []int{3}}},
	}
}

func (f *approvalFixture) draft(t *testing.T, spec ProjectSpec) {
	t.Helper()
	if err := RecordProjectSpec(context.Background(), f.wt, f.g, spec, at, owner, "draft"); err != nil {
		t.Fatal(err)
	}
}

func (f *approvalFixture) approveWith(t *testing.T, spec ProjectSpec, complete bool) {
	t.Helper()
	f.approval.ProjectSpec = spec
	f.record(t, complete)
}

// readSpecs reads the effective spec through every cross-repo read and
// reports each one that disagrees with want.
func (f *approvalFixture) readSpecs(t *testing.T, want ProjectSpec) {
	t.Helper()
	spec, g, found, err := f.r1.CrossRepoProjectSpec(f.g.ID)
	if err != nil || !found || !reflect.DeepEqual(spec, want) || g.ID != f.g.ID {
		t.Fatalf("CrossRepoProjectSpec: %+v %v %v", spec, found, err)
	}
	wt, spec, g, found, err := f.r2.OpenCrossRepoProjectSpec(f.g.ID)
	if err != nil || !found || !reflect.DeepEqual(spec, want) || g.ID != f.g.ID {
		t.Fatalf("OpenCrossRepoProjectSpec: %+v %v %v", spec, found, err)
	}
	if err := wt.Close(); err != nil {
		t.Fatal(err)
	}
	spec, id, found, err := f.r1.ProjectSpecOf(crossMember1)
	if err != nil || !found || id != f.g.ID || !reflect.DeepEqual(spec, want) {
		t.Fatalf("ProjectSpecOf: %+v %v %v", spec, found, err)
	}
}

// decodes counts every document decode a project spec read can make.
func decodes(id config.WorkgroupID) uint64 {
	return GroupApprovalDecodeCount(id) + ProjectSpecDecodeCount(id)
}

func TestRepeatedProjectSpecReadsDecodeOncePerRevision(t *testing.T) {
	f := newApprovalFixture(t, "building", 10)
	id := f.g.ID
	draft := specVariant("draft")
	f.draft(t, draft)

	before, opens := decodes(id), WorkgroupTraceOpenCount(id)
	for range 100 {
		f.readSpecs(t, draft)
	}
	if n := decodes(id) - before; n > 1 {
		t.Fatalf("300 draft reads decoded %d documents", n)
	}
	// OpenCrossRepoProjectSpec hands out its own handle; the other reads reuse
	// the one their project instance keeps.
	if n := WorkgroupTraceOpenCount(id) - opens; n > 101 {
		t.Fatalf("300 reads acquired the workgroup trace %d times", n)
	}

	// An approval embeds its own spec and replaces the draft.
	approved := specVariant("approved")
	f.approveWith(t, approved, false)
	before = decodes(id)
	for range 100 {
		f.readSpecs(t, approved)
	}
	if n := decodes(id) - before; n > 1 {
		t.Fatalf("reads after the approval decoded %d documents", n)
	}
	f.approveWith(t, approved, true)
	before = decodes(id)
	f.readSpecs(t, approved)
	f.readSpecs(t, approved)
	if n := decodes(id) - before; n > 1 {
		t.Fatalf("reads after the completion decoded %d documents", n)
	}

	// A newer draft recorded after approval never replaces the approved spec,
	// and changes nothing the choice reads.
	f.draft(t, specVariant("newer-draft"))
	before = decodes(id)
	for range 20 {
		f.readSpecs(t, approved)
	}
	if n := decodes(id) - before; n != 0 {
		t.Fatalf("a draft recorded after approval caused %d decodes", n)
	}

	// A published approved revision takes over on the next read, and the next
	// published revision after it.
	for _, label := range []string{"revision-1", "revision-2"} {
		revised := specVariant(label)
		if err := RecordWorkgroupRevision(context.Background(), f.wt, f.g, label, revised, at, owner); err != nil {
			t.Fatal(err)
		}
		before = decodes(id)
		for range 20 {
			f.readSpecs(t, revised)
		}
		if n := decodes(id) - before; n > 1 {
			t.Fatalf("reads of %s decoded %d documents", label, n)
		}
	}
	f.draft(t, specVariant("draft-after-revision"))
	f.readSpecs(t, specVariant("revision-2"))
}

func TestProjectSpecDraftIsReplacedByTheApprovalOnTheNextRead(t *testing.T) {
	f := newApprovalFixture(t, "building", 10)
	f.draft(t, specVariant("one"))
	f.readSpecs(t, specVariant("one"))
	f.draft(t, specVariant("two"))
	f.readSpecs(t, specVariant("two"))
	f.approveWith(t, specVariant("three"), false)
	f.readSpecs(t, specVariant("three"))
}

func TestProjectSpecAbsentIsReportedAsNotFound(t *testing.T) {
	f := newApprovalFixture(t, "building", 10)
	for range 3 {
		spec, g, found, err := f.r1.CrossRepoProjectSpec(f.g.ID)
		if err != nil || found || g.ID != f.g.ID || !reflect.DeepEqual(spec, ProjectSpec{}) {
			t.Fatalf("spec=%+v group=%+v found=%v err=%v", spec, g, found, err)
		}
	}
	f.draft(t, specVariant("late"))
	f.readSpecs(t, specVariant("late"))
	other := config.WorkgroupID("g_00000000000000000000000000000fff")
	if _, _, found, err := f.r1.CrossRepoProjectSpec(other); err != nil || found {
		t.Fatalf("unknown workgroup: found=%v err=%v", found, err)
	}
}

func TestReturnedProjectSpecBelongsToTheCaller(t *testing.T) {
	mutate := func(spec ProjectSpec, g Workgroup) {
		spec.Contracts[0].Text = "changed"
		spec.Graph[0].Type = EdgeHardBlock
		spec.Slices[0].ContractCriteria[0] = 99
		spec.Slices[1].Spec = "changed"
		g.Members[0] = "w_ffffffffffffffffffffffffffffffff"
	}
	phases := []struct {
		name  string
		spec  ProjectSpec
		setup func(t *testing.T, f *approvalFixture, spec ProjectSpec)
	}{
		{"draft", specVariant("draft"), func(t *testing.T, f *approvalFixture, s ProjectSpec) { f.draft(t, s) }},
		{"approval", specVariant("approval"), func(t *testing.T, f *approvalFixture, s ProjectSpec) { f.approveWith(t, s, true) }},
		{"revision", specVariant("revision"), func(t *testing.T, f *approvalFixture, s ProjectSpec) {
			if err := RecordWorkgroupRevision(context.Background(), f.wt, f.g, "delta", s, at, owner); err != nil {
				t.Fatal(err)
			}
		}},
	}
	for _, phase := range phases {
		t.Run(phase.name, func(t *testing.T) {
			f := newApprovalFixture(t, "building", 10)
			phase.setup(t, f, phase.spec)
			spec, g, _, err := f.r1.CrossRepoProjectSpec(f.g.ID)
			if err != nil {
				t.Fatal(err)
			}
			mutate(spec, g)
			wt, spec, g, _, err := f.r2.OpenCrossRepoProjectSpec(f.g.ID)
			if err != nil {
				t.Fatal(err)
			}
			defer wt.Close()
			mutate(spec, g)
			spec, _, _, err = f.r1.ProjectSpecOf(crossMember1)
			if err != nil {
				t.Fatal(err)
			}
			mutate(spec, Workgroup{Members: []config.WorkstreamID{""}})
			edges, _, err := f.r2.IncomingEdgesOf(crossMember2)
			if err != nil {
				t.Fatal(err)
			}
			edges[0].Type = "changed"
			f.readSpecs(t, phase.spec)
			_, g, _, err = f.r1.CrossRepoProjectSpec(f.g.ID)
			if err != nil || g.Members[0] != f.g.Members[0] || !reflect.DeepEqual(g, f.g) {
				t.Fatalf("group changed: %+v %v", g, err)
			}
			if edges, _, err := f.r2.IncomingEdgesOf(crossMember2, EdgeMergeOrder); err != nil || len(edges) != 1 || edges[0].Type != EdgeMergeOrder {
				t.Fatalf("edges changed: %+v %v", edges, err)
			}
		})
	}
}

func TestConcurrentProjectSpecReadsShareOneDecode(t *testing.T) {
	f := newApprovalFixture(t, "building", 10)
	want := specVariant("draft")
	f.draft(t, want)
	before := decodes(f.g.ID)
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
				spec, g, found, err := r.CrossRepoProjectSpec(f.g.ID)
				if err != nil || !found || !reflect.DeepEqual(spec, want) {
					t.Errorf("spec=%+v found=%v err=%v", spec, found, err)
					return
				}
				spec.Graph[0].Type = "changed"
				spec.Slices[0].ContractCriteria[0]++
				g.Members[0] = ""
				edges, _, err := r.IncomingEdgesOf(stream)
				if err != nil {
					t.Errorf("edges: %v", err)
					return
				}
				for j := range edges {
					edges[j].Type = "changed"
				}
				if _, _, _, err := r.ProjectSpecOf(stream); err != nil {
					t.Errorf("ProjectSpecOf: %v", err)
					return
				}
			}
		}()
	}
	wg.Wait()
	if n := decodes(f.g.ID) - before; n > 1 {
		t.Fatalf("concurrent readers decoded %d documents", n)
	}
	f.readSpecs(t, want)
}

func TestIncomingEdgesAreDerivedOncePerRevision(t *testing.T) {
	f := newApprovalFixture(t, "building", 10)
	if edges, found, err := f.r2.IncomingEdgesOf(crossMember2); err != nil || found || len(edges) != 0 {
		t.Fatalf("no spec: %+v %v %v", edges, found, err)
	}
	f.draft(t, specVariant("draft"))
	before := decodes(f.g.ID)
	for range 100 {
		edges, found, err := f.r2.IncomingEdgesOf(crossMember2, EdgeHardBlock, EdgeMergeOrder)
		want := []Edge{{From: crossMember1, To: crossMember2, Type: EdgeMergeOrder}, {From: crossMember1, To: crossMember2, Type: EdgeHardBlock}}
		if err != nil || !found || !reflect.DeepEqual(edges, want) {
			t.Fatalf("edges=%+v found=%v err=%v", edges, found, err)
		}
		if all, _, err := f.r2.IncomingEdgesOf(crossMember2); err != nil || len(all) != 3 {
			t.Fatalf("all edges=%+v err=%v", all, err)
		}
		if edges, found, err := f.r1.IncomingEdgesOf(crossMember1); err != nil || !found || len(edges) != 0 {
			t.Fatalf("edges into the first member: %+v %v %v", edges, found, err)
		}
	}
	if n := decodes(f.g.ID) - before; n > 1 {
		t.Fatalf("300 reads decoded %d documents", n)
	}
	// The edges follow the effective spec: the approval, then a revision.
	next := specVariant("approved")
	next.Graph = []Edge{{From: crossMember2, To: crossMember1, Type: EdgeHardBlock}}
	f.approveWith(t, next, true)
	if edges, _, err := f.r2.IncomingEdgesOf(crossMember2); err != nil || len(edges) != 0 {
		t.Fatalf("after approval: %+v %v", edges, err)
	}
	if edges, _, err := f.r1.IncomingEdgesOf(crossMember1); err != nil || !reflect.DeepEqual(edges, next.Graph) {
		t.Fatalf("after approval: %+v %v", edges, err)
	}
	f.draft(t, specVariant("newer-draft"))
	if edges, _, err := f.r2.IncomingEdgesOf(crossMember2); err != nil || len(edges) != 0 {
		t.Fatalf("draft replaced the approval: %+v %v", edges, err)
	}
	revised := specVariant("revised")
	if err := RecordWorkgroupRevision(context.Background(), f.wt, f.g, "delta", revised, at, owner); err != nil {
		t.Fatal(err)
	}
	if edges, _, err := f.r2.IncomingEdgesOf(crossMember2, EdgeMergeOrder); err != nil || len(edges) != 1 {
		t.Fatalf("after revision: %+v %v", edges, err)
	}
}

func TestDamagedLatestProjectSpecSourceIsReportedAfterAnEarlierRevisionWasCached(t *testing.T) {
	type source struct {
		name  string
		id    string
		scope func(f *approvalFixture) config.WorkstreamID
		first func(t *testing.T, f *approvalFixture)
		next  func(t *testing.T, f *approvalFixture)
	}
	revision := func(label string) func(t *testing.T, f *approvalFixture) {
		return func(t *testing.T, f *approvalFixture) {
			if err := RecordWorkgroupRevision(context.Background(), f.wt, f.g, label, specVariant(label), at, owner); err != nil {
				t.Fatal(err)
			}
		}
	}
	sources := []source{
		{"draft", ProjectSpecsDocument, func(*approvalFixture) config.WorkstreamID { return "" },
			func(t *testing.T, f *approvalFixture) { f.draft(t, specVariant("one")) },
			func(t *testing.T, f *approvalFixture) { f.draft(t, specVariant("two")) }},
		{"revision", WorkgroupRevisionDocument, func(f *approvalFixture) config.WorkstreamID { return f.host },
			revision("one"), revision("two")},
	}
	for _, s := range sources {
		damages := []string{"outer-json", "content-json"}
		if s.id == WorkgroupRevisionDocument {
			// Only a read by stored identity checks the stored revision.
			damages = append(damages, "stored-revision")
		}
		for _, damage := range damages {
			t.Run(s.name+"/"+damage, func(t *testing.T) {
				f := newApprovalFixture(t, "building", 10)
				s.first(t, f)
				f.readSpecs(t, specVariant("one"))
				s.next(t, f)
				scoped := mustScoped(t, f.wt, s.scope(f))
				var body string
				var seq int64
				if err := scoped.db.QueryRow("SELECT body, seq FROM records WHERE id = ? ORDER BY seq DESC LIMIT 1", s.id).Scan(&body, &seq); err != nil {
					t.Fatal(err)
				}
				switch damage {
				case "outer-json":
					body = "{"
				case "content-json", "stored-revision":
					var doc map[string]any
					if err := json.Unmarshal([]byte(body), &doc); err != nil {
						t.Fatal(err)
					}
					if damage == "content-json" {
						doc["content"] = "{"
					} else {
						doc["revision"] = 1
					}
					encoded, err := json.Marshal(doc)
					if err != nil {
						t.Fatal(err)
					}
					body = string(encoded)
				}
				if _, err := scoped.db.Exec("UPDATE records SET body = ? WHERE seq = ?", body, seq); err != nil {
					t.Fatal(err)
				}
				scoped.cache.mu.Lock()
				scoped.resetCache()
				scoped.cache.mu.Unlock()
				if _, _, _, err := f.r1.CrossRepoProjectSpec(f.g.ID); err == nil {
					t.Fatal("damaged latest record accepted by CrossRepoProjectSpec")
				}
				if wt, _, _, _, err := f.r2.OpenCrossRepoProjectSpec(f.g.ID); err == nil {
					wt.Close()
					t.Fatal("damaged latest record accepted by OpenCrossRepoProjectSpec")
				}
				if _, _, _, err := f.r1.ProjectSpecOf(crossMember1); err == nil {
					t.Fatal("damaged latest record accepted by ProjectSpecOf")
				}
				if _, _, err := f.r2.IncomingEdgesOf(crossMember2); err == nil {
					t.Fatal("damaged latest record accepted by IncomingEdgesOf")
				}
			})
		}
	}
}

func TestDamagedLatestApprovalIsReportedByProjectSpecReads(t *testing.T) {
	f := newApprovalFixture(t, "building", 10)
	f.approveWith(t, specVariant("approved"), false)
	f.readSpecs(t, specVariant("approved"))
	f.approveWith(t, specVariant("approved"), true)
	scoped := mustScoped(t, f.wt, f.host)
	if _, err := scoped.db.Exec("UPDATE records SET body = '{' WHERE id = ? AND revision = 2", GroupApprovalDocument); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := f.r1.CrossRepoProjectSpec(f.g.ID); err == nil {
		t.Fatal("damaged latest approval accepted")
	}
	if _, _, _, err := f.r1.ProjectSpecOf(crossMember1); err == nil {
		t.Fatal("damaged latest approval accepted by ProjectSpecOf")
	}
}

func TestProjectSpecCacheIsReleasedWithItsHolders(t *testing.T) {
	f := newApprovalFixture(t, "building", 10)
	f.draft(t, specVariant("draft"))
	f.readSpecs(t, specVariant("draft"))
	if !ProjectSpecCached(f.g.ID) {
		t.Fatal("spec was not cached")
	}
	for _, r := range []*Repository{f.wt, f.r1, f.r2} {
		if err := r.Close(); err != nil {
			t.Fatal(err)
		}
	}
	if ProjectSpecCached(f.g.ID) {
		t.Fatal("cached spec outlived every holder")
	}
	r1, err := Open(f.root, f.p1)
	if err != nil {
		t.Fatal(err)
	}
	defer r1.Close()
	before := ProjectSpecDecodeCount(f.g.ID)
	spec, _, found, err := r1.CrossRepoProjectSpec(f.g.ID)
	if err != nil || !found || !reflect.DeepEqual(spec, specVariant("draft")) {
		t.Fatalf("after restart: %+v %v %v", spec, found, err)
	}
	if n := ProjectSpecDecodeCount(f.g.ID) - before; n != 1 {
		t.Fatalf("restart rebuilt the spec with %d decodes", n)
	}
}
