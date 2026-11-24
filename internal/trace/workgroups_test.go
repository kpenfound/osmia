package trace

import (
	"context"
	"reflect"
	"strings"
	"testing"

	"github.com/kpenfound/osmia/internal/config"
)

func TestEnsureWorkgroupsCreatesOnePerMissingMemberAndIsIdempotent(t *testing.T) {
	r, _, _ := create(t)
	ctx := context.Background()

	groups, err := EnsureWorkgroups(ctx, r, []config.WorkstreamID{streamID}, at, owner, "migrate-workgroups")
	if err != nil {
		t.Fatal(err)
	}
	if len(groups) != 1 || len(groups[0].Members) != 1 || groups[0].Members[0] != streamID {
		t.Fatalf("groups after first pass: %+v", groups)
	}
	if err := config.CheckWorkgroupIDs(groups[0].ID); err != nil {
		t.Fatalf("workgroup identity: %v", err)
	}
	firstID := groups[0].ID

	again, err := EnsureWorkgroups(ctx, r, []config.WorkstreamID{streamID}, at, owner, "migrate-workgroups")
	if err != nil {
		t.Fatal(err)
	}
	if len(again) != 1 || again[0].ID != firstID {
		t.Fatalf("second pass changed groups: %+v", again)
	}
	docs, err := Read[Document](r, "")
	if err != nil {
		t.Fatal(err)
	}
	var revisions []int
	for _, d := range docs {
		if d.Path == WorkgroupsPath {
			revisions = append(revisions, d.Revision)
		}
	}
	if len(revisions) != 1 || revisions[0] != 1 {
		t.Fatalf("workgroups.json revisions after idempotent pass: %v", revisions)
	}

	second := config.WorkstreamID("w_00000000000000000000000000000002")
	if err := r.CreateWorkstream(ctx, second, at, owner); err != nil {
		t.Fatal(err)
	}
	withSecond, err := EnsureWorkgroups(ctx, r, []config.WorkstreamID{streamID, second}, at, owner, "migrate-workgroups")
	if err != nil {
		t.Fatal(err)
	}
	if len(withSecond) != 2 {
		t.Fatalf("groups after adding a second member: %+v", withSecond)
	}
	g, ok := WorkgroupOf(withSecond, second)
	if !ok || len(g.Members) != 1 || g.ID == firstID {
		t.Fatalf("second member's workgroup: %+v ok=%v", g, ok)
	}

	read, _, err := Workgroups(r)
	if err != nil {
		t.Fatal(err)
	}
	if len(read) != 2 {
		t.Fatalf("Workgroups readback: %+v", read)
	}
}

func TestWorkgroupsEmptyWhenNoneRecorded(t *testing.T) {
	r, _, _ := create(t)
	groups, revision, err := Workgroups(r)
	if err != nil || groups != nil || revision != 0 {
		t.Fatalf("groups=%+v revision=%d err=%v", groups, revision, err)
	}
}

func TestChiefOfStaffHostAndMembersOfATwoWorkstreamWorkgroup(t *testing.T) {
	r, _, _ := create(t)
	ctx := context.Background()
	second := config.WorkstreamID("w_00000000000000000000000000000002")
	if err := createWorkstreamTrace(ctx, r.root, r.project, second, manifestRecord{}, "workstream-create", "Create workstream trace", at, owner); err != nil {
		t.Fatal(err)
	}

	host, err := r.ChiefOfStaffHost(streamID)
	if err != nil || host != streamID {
		t.Fatalf("unrecorded workstream's host: %s %v", host, err)
	}
	members, err := r.WorkgroupMembers(streamID)
	if err != nil || len(members) != 1 || members[0] != streamID {
		t.Fatalf("unrecorded workstream's members: %+v %v", members, err)
	}

	if _, err := CreateWorkgroup(ctx, r, []config.WorkstreamID{streamID, second}, at, owner, "test-workgroup"); err != nil {
		t.Fatal(err)
	}
	for _, stream := range []config.WorkstreamID{streamID, second} {
		host, err := r.ChiefOfStaffHost(stream)
		if err != nil || host != streamID {
			t.Fatalf("host of %s: %s %v, want %s", stream, host, err, streamID)
		}
		members, err := r.WorkgroupMembers(stream)
		if err != nil || len(members) != 2 || members[0] != streamID || members[1] != second {
			t.Fatalf("members of %s: %+v %v", stream, members, err)
		}
	}

	if _, err := r.EnsureChiefOfStaff(ctx, streamID, at, owner); err != nil {
		t.Fatal(err)
	}
	if _, err := Get[Agent](r, streamID, ChiefOfStaff, 1); err != nil {
		t.Fatalf("the host's own chief-of-staff identity: %v", err)
	}
	if _, err := Get[Agent](r, second, ChiefOfStaff, 1); err == nil {
		t.Fatal("the second member should have no chief-of-staff identity of its own")
	}
}

func TestChiefQuestionTargetResolvesQualifiedAndUnqualifiedIDs(t *testing.T) {
	r, _, _ := create(t)
	ctx := context.Background()
	second := config.WorkstreamID("w_00000000000000000000000000000002")
	outside := config.WorkstreamID("w_00000000000000000000000000000003")
	if err := r.CreateWorkstream(ctx, second, at, owner); err != nil {
		t.Fatal(err)
	}
	if err := r.CreateWorkstream(ctx, outside, at, owner); err != nil {
		t.Fatal(err)
	}
	if _, err := CreateWorkgroup(ctx, r, []config.WorkstreamID{streamID, second}, at, owner, "test-workgroup"); err != nil {
		t.Fatal(err)
	}

	if stream, id, err := r.ChiefQuestionTarget(streamID, "1"); err != nil || stream != streamID || id != "1" {
		t.Fatalf("unqualified id: %s %s %v", stream, id, err)
	}
	if stream, id, err := r.ChiefQuestionTarget(streamID, string(second)+":1"); err != nil || stream != second || id != "1" {
		t.Fatalf("qualified id of a fellow member: %s %s %v", stream, id, err)
	}
	if _, _, err := r.ChiefQuestionTarget(streamID, string(outside)+":1"); err == nil || !strings.Contains(refusal(t, err), "does not share this chief of staff") {
		t.Fatalf("qualified id of a workstream outside the workgroup: %v", err)
	}
}

func TestMarkWorkgroupsResolvedFiresOnlyWhenEveryMemberIsResolved(t *testing.T) {
	r, _, _ := create(t)
	ctx := context.Background()
	second := config.WorkstreamID("w_00000000000000000000000000000002")
	if err := r.CreateWorkstream(ctx, second, at, owner); err != nil {
		t.Fatal(err)
	}
	g, err := CreateWorkgroup(ctx, r, []config.WorkstreamID{streamID, second}, at, owner, "test-workgroup")
	if err != nil {
		t.Fatal(err)
	}
	groups := []Workgroup{g}
	resolvedSet := map[config.WorkstreamID]bool{}
	resolved := func(stream config.WorkstreamID) (bool, error) { return resolvedSet[stream], nil }

	if newly, err := MarkWorkgroupsResolved(ctx, r, groups, resolved, at, owner, "no-members-resolved"); err != nil || len(newly) != 0 {
		t.Fatalf("no member resolved: %+v %v", newly, err)
	}

	resolvedSet[streamID] = true
	if newly, err := MarkWorkgroupsResolved(ctx, r, groups, resolved, at, owner, "one-member-resolved"); err != nil || len(newly) != 0 {
		t.Fatalf("one member resolved: %+v %v", newly, err)
	}
	if read, _, err := ResolvedWorkgroups(r); err != nil || len(read) != 0 {
		t.Fatalf("resolved workgroups after one member: %+v %v", read, err)
	}

	resolvedSet[second] = true
	newly, err := MarkWorkgroupsResolved(ctx, r, groups, resolved, at, owner, "every-member-resolved")
	if err != nil || len(newly) != 1 || newly[0] != groups[0].ID {
		t.Fatalf("every member resolved: %+v %v", newly, err)
	}
	read, _, err := ResolvedWorkgroups(r)
	if err != nil || !reflect.DeepEqual(read, []config.WorkgroupID{groups[0].ID}) {
		t.Fatalf("resolved workgroups after every member: %+v %v", read, err)
	}

	again, err := MarkWorkgroupsResolved(ctx, r, groups, resolved, at, owner, "resumed-after-a-crash")
	if err != nil || len(again) != 0 {
		t.Fatalf("resumed pass: %+v %v", again, err)
	}
	docs, err := Read[Document](r, "")
	if err != nil {
		t.Fatal(err)
	}
	var revisions []int
	for _, d := range docs {
		if d.Path == WorkgroupResolutionsPath {
			revisions = append(revisions, d.Revision)
		}
	}
	if len(revisions) != 1 || revisions[0] != 1 {
		t.Fatalf("workgroup-resolutions.json revisions: %v", revisions)
	}
}
