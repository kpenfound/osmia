package trace

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/kpenfound/osmia/internal/config"
)

const secondStream config.WorkstreamID = "w_00000000000000000000000000000002"
const thirdStream config.WorkstreamID = "w_00000000000000000000000000000003"

// errText requires a non-nil error and returns its message, for assertions
// against plain errors rather than the agent-facing *QuestionRefused
// refusal.go handles.
func errText(t *testing.T, err error) string {
	t.Helper()
	if err == nil {
		t.Fatal("expected an error")
	}
	return err.Error()
}

// TestValidateGraphAcceptsEveryEdgeType shows a graph whose edges are each of
// the three supported types, naming only known members, validates.
func TestValidateGraphAcceptsEveryEdgeType(t *testing.T) {
	members := []config.WorkstreamID{streamID, secondStream, thirdStream}
	edges := []Edge{
		{From: streamID, To: secondStream, Type: EdgeContract},
		{From: secondStream, To: thirdStream, Type: EdgeHardBlock},
		{From: streamID, To: thirdStream, Type: EdgeMergeOrder},
	}
	if err := ValidateGraph(members, edges); err != nil {
		t.Fatalf("valid graph refused: %v", err)
	}
}

// TestValidateGraphRefusesAnEdgeNamingAnUnknownWorkstream shows that an edge
// whose From or To is not one of the workgroup's own members is refused,
// naming the unknown workstream.
func TestValidateGraphRefusesAnEdgeNamingAnUnknownWorkstream(t *testing.T) {
	members := []config.WorkstreamID{streamID, secondStream}
	outside := config.WorkstreamID("w_00000000000000000000000000000099")
	edges := []Edge{{From: streamID, To: outside, Type: EdgeContract}}
	err := ValidateGraph(members, edges)
	if !strings.Contains(errText(t, err), "unknown workstream "+string(outside)) {
		t.Fatalf("edge naming an unknown workstream: %v", err)
	}
}

// TestValidateGraphRefusesACycleOfAnyEdgeTypesTogether shows that a cycle
// formed by edges of different types together, not only of one type, is
// still refused.
func TestValidateGraphRefusesACycleOfAnyEdgeTypesTogether(t *testing.T) {
	members := []config.WorkstreamID{streamID, secondStream, thirdStream}
	edges := []Edge{
		{From: streamID, To: secondStream, Type: EdgeContract},
		{From: secondStream, To: thirdStream, Type: EdgeHardBlock},
		{From: thirdStream, To: streamID, Type: EdgeMergeOrder},
	}
	if !strings.Contains(errText(t, ValidateGraph(members, edges)), "cycle") {
		t.Fatalf("cyclic graph: %v", ValidateGraph(members, edges))
	}
}

// TestValidateGraphRefusesAnUnknownEdgeType shows that an edge type other
// than contract, hard_block or merge_order is refused.
func TestValidateGraphRefusesAnUnknownEdgeType(t *testing.T) {
	members := []config.WorkstreamID{streamID, secondStream}
	edges := []Edge{{From: streamID, To: secondStream, Type: "integration_gate"}}
	if !strings.Contains(errText(t, ValidateGraph(members, edges)), "unknown type") {
		t.Fatalf("unknown edge type: %v", ValidateGraph(members, edges))
	}
}

// TestContractCitationNamesOnlyAWorkstreamsOwnContractCriteria shows that
// ContractCitation reports true only for a citation naming one of the
// member's own slice's contract-criteria numbers, false for a citation of
// another member's slice, a non-contract citation of its own slice, and a
// citation that does not parse as spec#<n>.
func TestContractCitationNamesOnlyAWorkstreamsOwnContractCriteria(t *testing.T) {
	spec := ProjectSpec{
		Behavior: "Two repos exchange a fixed interface.",
		Slices: []Slice{
			{Workstream: streamID, Spec: "spec.md", Plan: "plan.json", ContractCriteria: []int{3, 4}},
			{Workstream: secondStream, Spec: "spec.md", Plan: "plan.json", ContractCriteria: []int{1}},
		},
	}
	cases := []struct {
		stream   config.WorkstreamID
		citation string
		want     bool
	}{
		{streamID, "spec#3", true},
		{streamID, "spec#4", true},
		{streamID, "spec#1", false},
		{secondStream, "spec#1", true},
		{secondStream, "spec#3", false},
		{thirdStream, "spec#3", false}, // no slice recorded for this member
		{streamID, "plan#unit", false},
		{streamID, "spec#0", false},
		{streamID, "not-a-citation", false},
	}
	for _, c := range cases {
		if got := ContractCitation(spec, c.stream, c.citation); got != c.want {
			t.Errorf("ContractCitation(%s, %q) = %v, want %v", c.stream, c.citation, got, c.want)
		}
	}
}

// TestSliceForReturnsFalseWhenNoSliceIsRecorded shows SliceFor's reported ok
// distinguishes a recorded slice from none.
func TestSliceForReturnsFalseWhenNoSliceIsRecorded(t *testing.T) {
	spec := ProjectSpec{Slices: []Slice{{Workstream: streamID, Spec: "spec.md", Plan: "plan.json"}}}
	if _, ok := SliceFor(spec, secondStream); ok {
		t.Fatal("no slice is recorded for the second member")
	}
	slice, ok := SliceFor(spec, streamID)
	if !ok || slice.Workstream != streamID {
		t.Fatalf("recorded slice: %+v ok=%v", slice, ok)
	}
}

// TestProjectSpecValidRefusesAnInvalidGraphOrSlice shows that recording a
// project spec is refused, and nothing written, when its graph is invalid or
// a slice is malformed or repeated, mirroring RecordProjectSpec's own checks.
func TestProjectSpecValidRefusesAnInvalidGraphOrSlice(t *testing.T) {
	members := []config.WorkstreamID{streamID, secondStream}
	cases := []struct {
		name string
		spec ProjectSpec
		want string
	}{
		{"no behaviour", ProjectSpec{Slices: []Slice{{Workstream: streamID}, {Workstream: secondStream}}}, "requires its intended behaviour"},
		{"contract missing text", ProjectSpec{Behavior: "b", Contracts: []Contract{{Name: "seam"}}, Slices: []Slice{{Workstream: streamID}, {Workstream: secondStream}}}, "name and text"},
		{"unknown edge", ProjectSpec{Behavior: "b", Graph: []Edge{{From: streamID, To: "w_00000000000000000000000000000099", Type: EdgeContract}}, Slices: []Slice{{Workstream: streamID}, {Workstream: secondStream}}}, "unknown workstream"},
		{"repeated slice", ProjectSpec{Behavior: "b", Slices: []Slice{{Workstream: streamID}, {Workstream: streamID}}}, "recorded twice"},
		{"invalid slice", ProjectSpec{Behavior: "b", Slices: []Slice{{Workstream: "not-a-workstream"}}}, "is invalid"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := c.spec.valid(members)
			if !strings.Contains(errText(t, err), c.want) {
				t.Fatalf("valid(): %v, want containing %q", err, c.want)
			}
		})
	}
}

// TestRecordProjectSpecRefusesAnInvalidGraphAndWritesNothing shows that
// RecordProjectSpec, given a graph with a cycle, refuses and records no
// document: a later read still finds none.
func TestRecordProjectSpecRefusesAnInvalidGraphAndWritesNothing(t *testing.T) {
	r, _, _ := create(t)
	ctx := context.Background()
	if err := r.CreateWorkstream(ctx, secondStream, at, owner); err != nil {
		t.Fatal(err)
	}
	wg, err := CreateWorkgroup(ctx, r, []config.WorkstreamID{streamID, secondStream}, at, owner, "test-workgroup")
	if err != nil {
		t.Fatal(err)
	}
	cyclic := ProjectSpec{
		Behavior: "Two repos exchange a fixed interface.",
		Graph: []Edge{
			{From: streamID, To: secondStream, Type: EdgeContract},
			{From: secondStream, To: streamID, Type: EdgeHardBlock},
		},
		Slices: []Slice{{Workstream: streamID, Spec: "spec.md", Plan: "plan.json"}, {Workstream: secondStream, Spec: "spec.md", Plan: "plan.json"}},
	}
	if !strings.Contains(errText(t, RecordProjectSpec(ctx, r, wg, cyclic, at, owner, "owner-ratified")), "cycle") {
		t.Fatalf("cyclic graph accepted")
	}
	_, found, err := ProjectSpecFor(r, wg.ID)
	if err != nil || found {
		t.Fatalf("a refused ratification recorded a project spec: found=%v err=%v", found, err)
	}
}

// TestRecordProjectSpecSealsTheGraphAndEverySlice shows a valid acyclic
// graph and every member's slice recorded together, readable by
// ProjectSpecFor and ProjectSpecOf, and that a later revision replaces the
// earlier one rather than appending beside it.
func TestRecordProjectSpecSealsTheGraphAndEverySlice(t *testing.T) {
	r, _, _ := create(t)
	ctx := context.Background()
	if err := r.CreateWorkstream(ctx, secondStream, at, owner); err != nil {
		t.Fatal(err)
	}
	wg, err := CreateWorkgroup(ctx, r, []config.WorkstreamID{streamID, secondStream}, at, owner, "test-workgroup")
	if err != nil {
		t.Fatal(err)
	}
	spec := ProjectSpec{
		Behavior:  "Two repos exchange a fixed interface.",
		Contracts: []Contract{{Name: "upload-api", Text: "POST /v1/upload returns {id}."}},
		Graph:     []Edge{{From: secondStream, To: streamID, Type: EdgeContract}},
		Slices: []Slice{
			{Workstream: streamID, Spec: "spec.md", Plan: "plan.json", ContractCriteria: []int{1}},
			{Workstream: secondStream, Spec: "spec.md", Plan: "plan.json"},
		},
	}
	if err := RecordProjectSpec(ctx, r, wg, spec, at, owner, "owner-ratified"); err != nil {
		t.Fatal(err)
	}
	read, found, err := ProjectSpecFor(r, wg.ID)
	if err != nil || !found || read.Behavior != spec.Behavior || len(read.Slices) != 2 {
		t.Fatalf("ProjectSpecFor: %+v found=%v err=%v", read, found, err)
	}
	fromMember, group, found, err := r.ProjectSpecOf(secondStream)
	if err != nil || !found || group != wg.ID || fromMember.Behavior != spec.Behavior {
		t.Fatalf("ProjectSpecOf(%s): %+v group=%s found=%v err=%v", secondStream, fromMember, group, found, err)
	}
	active, err := ProjectSpecActive(r, wg.ID)
	if err != nil || !active {
		t.Fatalf("project spec active before any member resolves: active=%v err=%v", active, err)
	}

	// A later revision replaces the earlier one rather than appending beside
	// it: reading back still finds exactly one spec for the workgroup, with
	// the new behaviour.
	revised := spec
	revised.Behavior = "Two repos exchange a revised fixed interface."
	if err := RecordProjectSpec(ctx, r, wg, revised, at, owner, "owner-ratified-again"); err != nil {
		t.Fatal(err)
	}
	entries, _, err := readProjectSpecs(r)
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	for _, e := range entries {
		if e.Workgroup == wg.ID {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("project spec entries for %s after a revision: %d, want 1", wg.ID, count)
	}
	read, _, err = ProjectSpecFor(r, wg.ID)
	if err != nil || read.Behavior != revised.Behavior {
		t.Fatalf("latest project spec after a revision: %+v err=%v", read, err)
	}
}

// TestProjectSpecActiveUntilEveryMemberResolves shows that a workgroup's
// project spec stays active while any member is unresolved, and leaves
// active state only once every member of the workgroup is marked resolved,
// matching the two-level retention spec#22 describes: its own trace history
// stays readable regardless.
func TestProjectSpecActiveUntilEveryMemberResolves(t *testing.T) {
	r, _, _ := create(t)
	ctx := context.Background()
	if err := r.CreateWorkstream(ctx, secondStream, at, owner); err != nil {
		t.Fatal(err)
	}
	wg, err := CreateWorkgroup(ctx, r, []config.WorkstreamID{streamID, secondStream}, at, owner, "test-workgroup")
	if err != nil {
		t.Fatal(err)
	}
	spec := ProjectSpec{Behavior: "b", Slices: []Slice{{Workstream: streamID, Spec: "spec.md", Plan: "plan.json"}, {Workstream: secondStream, Spec: "spec.md", Plan: "plan.json"}}}
	if err := RecordProjectSpec(ctx, r, wg, spec, at, owner, "owner-ratified"); err != nil {
		t.Fatal(err)
	}

	resolvedSet := map[config.WorkstreamID]bool{}
	resolved := func(stream config.WorkstreamID) (bool, error) { return resolvedSet[stream], nil }

	// Resolving one member leaves the project spec active for the chief of
	// staff and the other, unresolved member.
	resolvedSet[streamID] = true
	if _, err := MarkWorkgroupsResolved(ctx, r, []Workgroup{wg}, resolved, at, owner, "first-member-resolved"); err != nil {
		t.Fatal(err)
	}
	active, err := ProjectSpecActive(r, wg.ID)
	if err != nil || !active {
		t.Fatalf("project spec active after one of two members resolves: active=%v err=%v", active, err)
	}
	if _, found, err := ProjectSpecFor(r, wg.ID); err != nil || !found {
		t.Fatalf("project spec still readable after one member resolves: found=%v err=%v", found, err)
	}

	// Resolving the second member retires the project spec from active
	// state, though its trace history stays readable.
	resolvedSet[secondStream] = true
	if _, err := MarkWorkgroupsResolved(ctx, r, []Workgroup{wg}, resolved, at, owner, "every-member-resolved"); err != nil {
		t.Fatal(err)
	}
	active, err = ProjectSpecActive(r, wg.ID)
	if err != nil || active {
		t.Fatalf("project spec active after every member resolves: active=%v err=%v", active, err)
	}
	if _, found, err := ProjectSpecFor(r, wg.ID); err != nil || !found {
		t.Fatalf("project spec trace history unreadable after the workgroup resolves: found=%v err=%v", found, err)
	}
}

func TestProjectSpecActiveFollowsCrossRepoAuthority(t *testing.T) {
	root, r1, r2, _, _ := crossRepoFixture(t)
	wg, group := createCrossRepoWorkgroup(t, root, r1, r2)
	check := func(want bool) {
		t.Helper()
		for _, receiver := range []*Repository{r1, r2, wg} {
			active, err := ProjectSpecActive(receiver, group.ID)
			if err != nil || active != want {
				t.Fatalf("receiver %s: active=%v want=%v err=%v", receiver.Project(), active, want, err)
			}
		}
	}
	check(true)
	if _, err := MarkWorkgroupsResolved(context.Background(), wg, []Workgroup{group}, func(config.WorkstreamID) (bool, error) {
		return true, nil
	}, at, owner, "members-merged"); err != nil {
		t.Fatal(err)
	}
	check(false)
	for _, member := range []*Repository{r1, r2} {
		resolved, _, err := ResolvedWorkgroups(member)
		if err != nil || len(resolved) != 0 {
			t.Fatalf("member has replicated retirement: %v err=%v", resolved, err)
		}
	}
	if err := wg.Close(); err != nil {
		t.Fatal(err)
	}
	// A cold authority read needs neither member bindings nor member history.
	if active, err := ProjectSpecActive(r1, group.ID); err != nil || active {
		t.Fatalf("cold authority read: active=%v err=%v", active, err)
	}
	if _, _, err := Workgroups(r1); err != nil {
		t.Fatalf("authority read closed the member receiver: %v", err)
	}
}

func TestProjectSpecActiveReturnsCrossRepoAuthorityErrors(t *testing.T) {
	for _, failure := range []string{"missing", "corrupt"} {
		t.Run(failure, func(t *testing.T) {
			root, r1, r2, _, _ := crossRepoFixture(t)
			wg, group := createCrossRepoWorkgroup(t, root, r1, r2)
			if failure == "corrupt" {
				if err := wg.Append(context.Background(), Document{
					Header: Header{Schema: "osmia.trace.document", Version: Version, ID: WorkgroupResolutionsDocument, Revision: 1, Project: wg.Project(), At: at, Actor: owner, Cause: "test"},
					Path:   WorkgroupResolutionsPath, Content: "{",
				}); err != nil {
					t.Fatal(err)
				}
			} else {
				if err := wg.Close(); err != nil {
					t.Fatal(err)
				}
				directory, err := root.WorkgroupTrace(group.ID)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.Rename(directory, directory+"-unavailable"); err != nil {
					t.Fatal(err)
				}
			}
			if active, err := ProjectSpecActive(r1, group.ID); err == nil {
				t.Fatalf("%s authority returned active=%v without an error", failure, active)
			}
		})
	}
}
