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

func errText(t *testing.T, err error) string {
	t.Helper()
	if err == nil {
		t.Fatal("expected an error")
	}
	return err.Error()
}

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

func TestValidateGraphRefusesAnEdgeNamingAnUnknownWorkstream(t *testing.T) {
	members := []config.WorkstreamID{streamID, secondStream}
	outside := config.WorkstreamID("w_00000000000000000000000000000099")
	edges := []Edge{{From: streamID, To: outside, Type: EdgeContract}}
	err := ValidateGraph(members, edges)
	if !strings.Contains(errText(t, err), "unknown workstream "+string(outside)) {
		t.Fatalf("edge naming an unknown workstream: %v", err)
	}
}

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

func TestValidateGraphRefusesAnUnknownEdgeType(t *testing.T) {
	members := []config.WorkstreamID{streamID, secondStream}
	edges := []Edge{{From: streamID, To: secondStream, Type: "integration_gate"}}
	if !strings.Contains(errText(t, ValidateGraph(members, edges)), "unknown type") {
		t.Fatalf("unknown edge type: %v", ValidateGraph(members, edges))
	}
}

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
		{thirdStream, "spec#3", false},
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
