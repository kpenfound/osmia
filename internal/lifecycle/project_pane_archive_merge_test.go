package lifecycle_test

import (
	context "context"
	fmt "fmt"
	reflect "reflect"
	slices "slices"
	strings "strings"
	testing "testing"
	time "time"

	"github.com/kpenfound/osmia/internal/servicetest/journey"

	config "github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/lifecycle"
	pulls "github.com/kpenfound/osmia/internal/pulls"
	runtime "github.com/kpenfound/osmia/internal/runtime"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	"github.com/kpenfound/osmia/internal/servicetest/intakefixture"
	trace "github.com/kpenfound/osmia/internal/trace"
)

func TestProjectPanePostCleanupMergeAcrossRestart(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	host := &fixturekit.FakePulls{}
	f := resolutionApprovedFixture(t, journey.ProductionDesignConfiguration{Wait: journey.LifecycleAwait}, trace.EdgeHardBlock, host)
	defer f.Stop(t)
	publications := deliverResolutionMembers(t, f)
	initial, err := f.C.Status(ctx, f.Stream1)
	fixturekit.Must(t, err)
	group := initial.Workgroup
	if group == nil || group.Conversation == nil || group.Conversation.Workstream != f.Origin || !slices.Equal(group.Members, []config.WorkstreamID{f.Stream1, f.Stream2}) {
		t.Fatalf("original group=%+v", group)
	}
	conversation, err := f.C.Conversation(ctx, f.Stream2)
	fixturekit.Must(t, err)
	if len(conversation.Entries) == 0 {
		t.Fatal("design conversation is empty")
	}
	groupResolution := func() ([]config.WorkgroupID, int, bool) {
		t.Helper()
		authority, err := trace.OpenWorkgroupAuthority(f.S.Current().Root, group.ID)
		fixturekit.Must(t, err)
		defer authority.Close()
		ids, revision, err := trace.ResolvedWorkgroups(authority)
		fixturekit.Must(t, err)
		active, err := trace.ProjectSpecActive(authority, group.ID)
		fixturekit.Must(t, err)
		return ids, revision, active
	}
	check := func(merged, resolved bool) {
		t.Helper()
		wantState, wantWait := "delivered (merged)", ""
		if !merged {
			wantState = "blocked"
			wantWait = fmt.Sprintf("archived member %s has no pinned merge or cancel evidence; workgroup remains unresolved", f.Stream1)
		}
		list, err := f.C.Statuses(ctx)
		fixturekit.Must(t, err)
		if len(list.Workstreams) != 2 {
			t.Fatalf("pane membership=%+v", list.Workstreams)
		}
		counts := map[config.WorkstreamID]int{}
		for _, row := range list.Workstreams {
			counts[row.Workstream]++
			if !reflect.DeepEqual(row.Workgroup, group) {
				t.Fatalf("group identity or conversation changed: %+v", row.Workgroup)
			}
			if row.Workstream == f.Stream1 && (!row.Archived || !row.TraceDeleted || row.DisplayState != wantState || row.Wait != wantWait) {
				t.Fatalf("pane archived row=%+v; want %q wait=%q", row, wantState, wantWait)
			}
			if resolved && (row.DisplayState != "delivered (merged)" || row.Wait != "") {
				t.Fatalf("resolved group's pane member=%+v", row)
			}
		}
		if counts[f.Stream1] != 1 || counts[f.Stream2] != 1 {
			t.Fatalf("pane lost or duplicated a member: %v", counts)
		}
		row, err := f.C.Status(ctx, f.Stream1)
		fixturekit.Must(t, err)
		if !row.Archived || !row.TraceDeleted || row.DisplayState != wantState || row.Wait != wantWait || !reflect.DeepEqual(row.Workgroup, group) {
			t.Fatalf("archived drill-down=%+v", row)
		}
		original, err := f.C.Conversation(ctx, f.Origin)
		fixturekit.Must(t, err)
		if len(original.Entries) < len(conversation.Entries) || !slices.Equal(original.Entries[:len(conversation.Entries)], conversation.Entries) {
			t.Fatal("the shared conversation lost its original entries")
		}
		wantArchiveEntries := 0
		if resolved {
			wantArchiveEntries = 1
		}
		archiveEntries := 0
		for _, entry := range original.Entries {
			if entry.Kind == "action" && strings.HasPrefix(entry.Turn, "archive-") {
				archiveEntries++
				if entry.Turn != "archive-"+string(f.Stream2) || !strings.Contains(entry.Text, "pull request merged") {
					t.Fatalf("unexpected archive entry %+v", entry)
				}
			}
		}
		if archiveEntries != wantArchiveEntries {
			t.Fatalf("archive entries=%d; want %d in %+v", archiveEntries, wantArchiveEntries, original.Entries)
		}
		if !resolved {
			current, err := f.C.Conversation(ctx, f.Stream2)
			fixturekit.Must(t, err)
			if !slices.Equal(current.Entries, original.Entries) {
				t.Fatal("survivor no longer reads the original shared conversation")
			}
		}
		gone, err := trace.WorkstreamTraceGone(f.S.Current().Root, f.Project, f.Stream1)
		fixturekit.Must(t, err)
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
	_, err = f.C.Archive(ctx, f.Stream1)
	fixturekit.Must(t, err)
	journey.LifecycleAwait(t, f, "unmerged upstream cleanup", func() bool {
		a, ok := intakefixture.ArchiveRecord(t, f.S.Store, f.Stream1)
		return ok && !a.CleanedAt.IsZero()
	})
	before, ok := intakefixture.ArchiveRecord(t, f.S.Store, f.Stream1)
	if !ok || before.Resolved || before.ResolutionEvidence != "unresolved" || before.Publication.PullRequest != publications[0].PullRequest {
		t.Fatalf("unmerged archive=%+v", before)
	}
	check(false, false)
	p := publications[0]
	for _, candidate := range []pulls.PullRequest{
		{Number: p.PullRequest - 1, State: "closed", Merged: true, MergeCommit: "earlier-merge", Head: p.Branch, HeadRepository: p.Fork},
		{Number: p.PullRequest, State: "closed", Head: p.Branch, HeadRepository: p.Fork},
	} {
		down, err := f.S.Repository(f.Project2)
		fixturekit.Must(t, err)
		_, revision, _, err := lifecycle.EdgeObservationAt(down, f.Stream2)
		fixturekit.Must(t, err)
		host.Mu.Lock()
		host.Prs = []pulls.PullRequest{candidate}
		host.Mu.Unlock()
		journey.LifecycleAwait(t, f, "non-merge provider observation", func() bool {
			observed, next, _, err := lifecycle.EdgeObservationAt(down, f.Stream2)
			fixturekit.Must(t, err)
			if slices.ContainsFunc(observed.Edges, func(e lifecycle.EdgeMergeStatus) bool { return e.Merged }) {
				t.Fatalf("non-merge candidate released edge: %+v", observed)
			}
			return next >= revision+2
		})
		check(false, false)
		f.Stop(t)
		f.Start(t)
		check(false, false)
	}
	host.Mu.Lock()
	host.Prs = []pulls.PullRequest{{Number: p.PullRequest, State: "closed", Merged: true, MergeCommit: "upstream-merge", Head: p.Branch, HeadRepository: p.Fork}}
	host.Mu.Unlock()
	f.Clock.Advance(10 * time.Minute)
	journey.LifecycleAwait(t, f, "pane archived member merged", func() bool {
		a, ok := intakefixture.ArchiveRecord(t, f.S.Store, f.Stream1)
		return ok && lifecycle.ArchiveResolved(a)
	})
	after, _ := intakefixture.ArchiveRecord(t, f.S.Store, f.Stream1)
	if after.MergeReference != fmt.Sprintf("%s#%d", p.Upstream, p.PullRequest) || after.MergeCommit != "upstream-merge" || after.Publication != before.Publication || after.ArchivedAt != before.ArchivedAt || after.CleanedAt != before.CleanedAt {
		t.Fatalf("archive identity or merge evidence=%+v", after)
	}
	check(true, false)
	f.Stop(t)
	f.Start(t)
	check(true, false)
	sink := publications[1]
	host.Mu.Lock()
	host.Prs = append(host.Prs, pulls.PullRequest{Number: sink.PullRequest, State: "closed", Merged: true, MergeCommit: "sink-merge", Head: sink.Branch, HeadRepository: sink.Fork})
	host.Mu.Unlock()
	f.Clock.Advance(10 * time.Minute)
	journey.LifecycleAwait(t, f, "last pane member merged and group resolved", func() bool {
		_, revision, _ := groupResolution()
		return revision == 1
	})
	journey.LifecycleAwait(t, f, "chief of staff archived and cleaned the merged sink", func() bool {
		a, ok := intakefixture.ArchiveRecord(t, f.S.Store, f.Stream2)
		return ok && a.Actor == runtime.ArchiveActorChief && !a.CleanedAt.IsZero()
	})
	check(true, true)
	f.Stop(t)
	f.Start(t)
	check(true, true)
}
