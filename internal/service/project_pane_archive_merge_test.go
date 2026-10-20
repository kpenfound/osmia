package service

import (
	"context"
	"fmt"
	"reflect"
	"slices"
	"testing"
	"time"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/pulls"
	"github.com/kpenfound/osmia/internal/trace"
)

func TestProjectPanePostCleanupMergeAcrossRestart(t *testing.T) {
	ctx := context.Background()
	host := &fakePulls{}
	f := resolutionApprovedFixture(t, productionDesignConfiguration{wait: lifecycleAwait}, trace.EdgeHardBlock, host)
	defer f.stop(t)
	publications := deliverResolutionMembers(t, f)
	initial, err := f.c.Status(ctx, f.stream1)
	must(t, err)
	group := initial.Workgroup
	if group == nil || group.Conversation == nil || group.Conversation.Workstream != f.origin || !slices.Equal(group.Members, []config.WorkstreamID{f.stream1, f.stream2}) {
		t.Fatalf("original group=%+v", group)
	}
	conversation, err := f.c.Conversation(ctx, f.stream2)
	must(t, err)
	if len(conversation.Entries) == 0 {
		t.Fatal("design conversation is empty")
	}
	groupResolution := func() ([]config.WorkgroupID, int, bool) {
		t.Helper()
		authority, err := trace.OpenWorkgroupAuthority(f.s.current().Root, group.ID)
		must(t, err)
		defer authority.Close()
		ids, revision, err := trace.ResolvedWorkgroups(authority)
		must(t, err)
		active, err := trace.ProjectSpecActive(authority, group.ID)
		must(t, err)
		return ids, revision, active
	}
	check := func(merged, resolved bool) {
		t.Helper()
		wantState, wantWait := "delivered (merged)", ""
		if !merged {
			wantState = "blocked"
			wantWait = fmt.Sprintf("archived member %s has no pinned merge or cancel evidence; workgroup remains unresolved", f.stream1)
		}
		// The pane reads the global status API; its drill-down reads the
		// individual status API. Both must read the reconciled archive.
		list, err := f.c.Statuses(ctx)
		must(t, err)
		if len(list.Workstreams) != 2 {
			t.Fatalf("pane membership=%+v", list.Workstreams)
		}
		counts := map[config.WorkstreamID]int{}
		for _, row := range list.Workstreams {
			counts[row.Workstream]++
			if !reflect.DeepEqual(row.Workgroup, group) {
				t.Fatalf("group identity or conversation changed: %+v", row.Workgroup)
			}
			if row.Workstream == f.stream1 && (!row.Archived || !row.TraceDeleted || row.DisplayState != wantState || row.Wait != wantWait) {
				t.Fatalf("pane archived row=%+v; want %q wait=%q", row, wantState, wantWait)
			}
			if resolved && (row.DisplayState != "delivered (merged)" || row.Wait != "") {
				t.Fatalf("resolved group's pane member=%+v", row)
			}
		}
		if counts[f.stream1] != 1 || counts[f.stream2] != 1 {
			t.Fatalf("pane lost or duplicated a member: %v", counts)
		}
		row, err := f.c.Status(ctx, f.stream1)
		must(t, err)
		if !row.Archived || !row.TraceDeleted || row.DisplayState != wantState || row.Wait != wantWait || !reflect.DeepEqual(row.Workgroup, group) {
			t.Fatalf("archived drill-down=%+v", row)
		}
		current, err := f.c.Conversation(ctx, f.stream2)
		must(t, err)
		original, err := f.c.Conversation(ctx, f.origin)
		must(t, err)
		if !slices.Equal(current.Entries, original.Entries) || len(current.Entries) < len(conversation.Entries) || !slices.Equal(current.Entries[:len(conversation.Entries)], conversation.Entries) {
			t.Fatal("survivor no longer reads the original shared conversation")
		}
		gone, err := trace.WorkstreamTraceGone(f.s.current().Root, f.project, f.stream1)
		must(t, err)
		if !gone {
			t.Fatal("pane read recreated cleaned upstream history")
		}
		ids, revision, active := groupResolution()
		if resolved {
			if !slices.Equal(ids, []config.WorkgroupID{group.ID}) || revision != 1 || active {
				t.Fatalf("group resolution=%v revision=%d active=%v", ids, revision, active)
			}
		} else if len(ids) != 0 || revision != 0 || !active {
			t.Fatal("group resolved before every member merged")
		}
	}
	_, err = f.c.Archive(ctx, f.stream1)
	must(t, err)
	lifecycleAwait(t, f, "unmerged upstream cleanup", func() bool {
		a, ok := archiveRecord(t, f.s.store, f.stream1)
		return ok && !a.CleanedAt.IsZero()
	})
	before, ok := archiveRecord(t, f.s.store, f.stream1)
	if !ok || before.Resolved || before.ResolutionEvidence != "unresolved" || before.Publication.PullRequest != publications[0].PullRequest {
		t.Fatalf("unmerged archive=%+v", before)
	}
	check(false, false)
	// Neither an earlier PR's merge nor closing the pinned PR without
	// merging it supplies merge evidence, even after authorized cleanup.
	p := publications[0]
	for _, candidate := range []pulls.PullRequest{
		{Number: p.PullRequest - 1, State: "closed", Merged: true, MergeCommit: "earlier-merge", Head: p.Branch, HeadRepository: p.Fork},
		{Number: p.PullRequest, State: "closed", Head: p.Branch, HeadRepository: p.Fork},
	} {
		down, err := f.s.repository(f.project2)
		must(t, err)
		_, revision, _, err := edgeObservationAt(down, f.stream2)
		must(t, err)
		host.mu.Lock()
		host.prs = []pulls.PullRequest{candidate}
		host.mu.Unlock()
		// Two subsequent receipts also exclude a lookup already in flight
		// when the provider response changed. Only the live controller polls.
		lifecycleAwait(t, f, "non-merge provider observation", func() bool {
			observed, next, _, err := edgeObservationAt(down, f.stream2)
			must(t, err)
			if slices.ContainsFunc(observed.Edges, func(e edgeMergeStatus) bool { return e.Merged }) {
				t.Fatalf("non-merge candidate released edge: %+v", observed)
			}
			return next >= revision+2
		})
		check(false, false)
		f.stop(t)
		f.start(t)
		check(false, false)
	}
	host.mu.Lock()
	host.prs = []pulls.PullRequest{{Number: p.PullRequest, State: "closed", Merged: true, MergeCommit: "upstream-merge", Head: p.Branch, HeadRepository: p.Fork}}
	host.mu.Unlock()
	f.clock.Advance(10 * time.Minute)
	lifecycleAwait(t, f, "pane archived member merged", func() bool {
		a, ok := archiveRecord(t, f.s.store, f.stream1)
		return ok && archiveResolved(a)
	})
	after, _ := archiveRecord(t, f.s.store, f.stream1)
	if after.MergeReference != fmt.Sprintf("%s#%d", p.Upstream, p.PullRequest) || after.MergeCommit != "upstream-merge" || after.Publication != before.Publication || after.ArchivedAt != before.ArchivedAt || after.CleanedAt != before.CleanedAt {
		t.Fatalf("archive identity or merge evidence=%+v", after)
	}
	check(true, false)
	f.stop(t)
	f.start(t)
	check(true, false)
	sink := publications[1]
	host.mu.Lock()
	host.prs = append(host.prs, pulls.PullRequest{Number: sink.PullRequest, State: "closed", Merged: true, MergeCommit: "sink-merge", Head: sink.Branch, HeadRepository: sink.Fork})
	host.mu.Unlock()
	f.clock.Advance(10 * time.Minute)
	lifecycleAwait(t, f, "last pane member merged and group resolved", func() bool {
		_, revision, _ := groupResolution()
		return revision == 1
	})
	check(true, true)
	f.stop(t)
	f.start(t)
	check(true, true)
}
