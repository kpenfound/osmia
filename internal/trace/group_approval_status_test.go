package trace

import (
	"reflect"
	"testing"
)

func TestGroupApprovalStatusFollowsRevisionsWithoutDecodingAgain(t *testing.T) {
	f := newApprovalFixture(t, "building", 20000)
	id := f.g.ID
	status := func() GroupApprovalStatus {
		t.Helper()
		s, err := f.r1.GroupApprovalStatusOf(id)
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
	if s := status(); s.Found || s.Complete || !s.Required || s.Revision != 0 {
		t.Fatalf("no approval: %+v", s)
	}
	f.record(t, false)
	incomplete := status()
	if !incomplete.Found || incomplete.Complete || !incomplete.Required || incomplete.Revision == 0 {
		t.Fatalf("incomplete approval: %+v", incomplete)
	}
	f.record(t, true)
	complete := status()
	if !complete.Found || !complete.Complete || complete.Revision == incomplete.Revision {
		t.Fatalf("complete approval: %+v after %+v", complete, incomplete)
	}
	member, err := f.r1.MemberGroupApprovalStatus(crossMember1)
	if err != nil || member != complete {
		t.Fatalf("member status %+v, %v; want %+v", member, err, complete)
	}
	decodes := GroupApprovalDecodeCount(id)
	for range 100 {
		if got := status(); got != complete {
			t.Fatalf("status changed: %+v", got)
		}
	}
	if n := GroupApprovalDecodeCount(id) - decodes; n != 0 {
		t.Fatalf("100 status reads decoded the approval %d times", n)
	}
}

func TestApprovedMembersOfMatchesTheApprovalItReplaces(t *testing.T) {
	f := newApprovalFixture(t, "building", 20000)
	if members, found, err := f.r1.ApprovedMembersOf(f.g); err != nil || found || members != nil {
		t.Fatalf("no approval: %v %v %v", members, found, err)
	}
	f.record(t, true)
	approval, _, found, err := ReadGroupApproval(f.wt, f.host)
	if err != nil || !found {
		t.Fatalf("approval: %v %v", found, err)
	}
	want, err := ApprovedWorkgroupMembers(f.wt, f.g, approval)
	if err != nil {
		t.Fatal(err)
	}
	decodes := GroupApprovalDecodeCount(f.g.ID)
	for _, repo := range []*Repository{f.r1, f.r2, f.wt} {
		for range 50 {
			got, found, err := repo.ApprovedMembersOf(f.g)
			if err != nil || !found || !reflect.DeepEqual(got, want) {
				t.Fatalf("approved members %v %v %v, want %v", got, found, err, want)
			}
			got[crossMember1] = "changed"
		}
	}
	if n := GroupApprovalDecodeCount(f.g.ID) - decodes; n > 1 {
		t.Fatalf("150 reads decoded the approval %d times", n)
	}
}

func TestCrossRepoWorkgroupAnswersLikeCrossRepoProjectSpecWithoutTheSpec(t *testing.T) {
	f := newApprovalFixture(t, "building", 20000)
	f.record(t, true)
	_, want, wantFound, err := f.r1.CrossRepoProjectSpec(f.g.ID)
	if err != nil {
		t.Fatal(err)
	}
	got, found, err := f.r1.CrossRepoWorkgroup(f.g.ID)
	if err != nil || found != wantFound || !reflect.DeepEqual(got, want) {
		t.Fatalf("workgroup %+v %v %v, want %+v %v", got, found, err, want, wantFound)
	}
	got.Members[0] = "changed"
	again, _, err := f.r1.CrossRepoWorkgroup(f.g.ID)
	if err != nil || !reflect.DeepEqual(again, want) {
		t.Fatalf("a changed result reached later reads: %+v %v", again, err)
	}
	wt, opened, openedFound, err := f.r1.OpenCrossRepoWorkgroup(f.g.ID)
	if err != nil || wt == nil || openedFound != wantFound || !reflect.DeepEqual(opened, want) {
		t.Fatalf("opened workgroup %+v %v %v", opened, openedFound, err)
	}
	if err := wt.Close(); err != nil {
		t.Fatal(err)
	}
	if wt, g, found, err := f.r1.OpenCrossRepoWorkgroup("g_00000000000000000000000000000000"); err != nil || wt != nil || found || g.ID != "" {
		t.Fatalf("a missing trace returned %v %+v %v %v", wt, g, found, err)
	}
	decodes, specs := GroupApprovalDecodeCount(f.g.ID), ProjectSpecDecodeCount(f.g.ID)
	for range 100 {
		if _, _, err := f.r1.CrossRepoWorkgroup(f.g.ID); err != nil {
			t.Fatal(err)
		}
	}
	if GroupApprovalDecodeCount(f.g.ID) != decodes || ProjectSpecDecodeCount(f.g.ID) != specs {
		t.Fatal("repeated workgroup reads decoded the approval or the project spec")
	}
}

func TestKeptHandleServesLaterAcquisitionsWithoutOpeningTheTraceFromDisk(t *testing.T) {
	f := newApprovalFixture(t, "building", 10)
	f.record(t, true)
	if _, err := f.r1.GroupApproved(crossMember1); err != nil {
		t.Fatal(err)
	}
	disk := WorkgroupTraceDiskOpenCount(f.g.ID)
	for range 50 {
		wt, err := OpenWorkgroupAuthority(f.root, f.g.ID)
		if err != nil {
			t.Fatal(err)
		}
		if err := wt.Close(); err != nil {
			t.Fatal(err)
		}
	}
	if n := WorkgroupTraceDiskOpenCount(f.g.ID) - disk; n != 0 {
		t.Fatalf("50 acquisitions opened the trace from disk %d times", n)
	}
}
