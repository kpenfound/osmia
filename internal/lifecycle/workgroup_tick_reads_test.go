package lifecycle_test

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"testing"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/runtime"
	"github.com/kpenfound/osmia/internal/trace"
)

// tickWorkgroupPasses runs the production hooks that consume shared authority,
// with the fixture's controllers joined so each tick has a definite boundary.
func tickWorkgroupPasses(t *testing.T, f *crossRepoRatifyFixture) {
	t.Helper()
	for _, project := range []config.ProjectID{f.project, f.project2} {
		repo := mustRepo(t, f.s, project)
		must(t, f.s.changePass(context.Background(), repo))
		must(t, (&workgroupResolver{s: f.s, repository: repo}).Pass(context.Background()))
		must(t, f.s.projectSpecDraftPass(context.Background(), repo))
		must(t, f.s.projectSpecRefreshPass(context.Background(), repo))
	}
}

func resolvedTickFixture(t *testing.T) (*crossRepoRatifyFixture, *trace.Repository, trace.Workgroup, trace.ProjectSpec) {
	t.Helper()
	f, id, _ := projectSliceRefreshFixture(t)
	ctx := context.Background()
	wt, err := trace.OpenWorkgroupAuthority(f.s.current().Root, id)
	must(t, err)
	t.Cleanup(func() { must(t, wt.Close()) })
	groups, _, err := trace.Workgroups(wt)
	must(t, err)
	g := groups[0]
	host := g.Members[0]
	spec, found, err := trace.ProjectSpecFor(wt, id)
	must(t, err)
	if !found {
		t.Fatal("missing project spec")
	}
	// Pin the fixture's owner decision before member retirement, as ratification
	// does, so the production resolver can locate both archived members.
	approval := trace.GroupApproval{Workgroup: id, ProjectSpec: spec, Complete: true, Members: []trace.GroupMemberApproval{
		{Project: f.project, Workstream: f.stream1, Record: json.RawMessage(`{}`)},
		{Project: f.project2, Workstream: f.stream2, Record: json.RawMessage(`{}`)},
	}}
	must(t, trace.RecordGroupApproval(ctx, wt, host, approval, f.s.now(), ownerActor))
	return f, wt, g, spec
}

func TestArchivedResolvedWorkgroupTicksAvoidTraceReadsAndObserveReopen(t *testing.T) {
	f, wt, g, spec := resolvedTickFixture(t)
	ctx := context.Background()
	id, host := g.ID, g.Members[0]
	docs := make([]trace.Document, 1200)
	for i := range docs {
		docs[i] = trace.Document{Header: trace.Header{Schema: "osmia.trace.document", Version: trace.Version, ID: fmt.Sprintf("history-%d", i), Revision: 1, Project: wt.Project(), Workstream: host, At: f.s.now(), Actor: ownerActor, Cause: "fixture"}, Path: fmt.Sprintf("tools/history-%d.json", i), Content: `{}`}
	}
	must(t, wt.RecordDocuments(ctx, docs))
	// Live members still drive the real hooks.
	before := trace.WorkgroupTraceOpenCount(id)
	tickWorkgroupPasses(t, f)
	if trace.WorkgroupTraceOpenCount(id) <= before {
		t.Fatal("live workgroup was skipped")
	}
	must(t, f.s.refreshRuntime())
	first := runtime.Archive{Project: f.project, Workstream: f.stream1, Workgroup: id, Title: "Cancelled member", State: AbandonedState, Resolved: true, ResolutionEvidence: "cancelled", ArchivedAt: f.s.now()}
	second := runtime.Archive{Project: f.project2, Workstream: f.stream2, Workgroup: id, Title: "Open publication", State: DeliveredState, ResolutionEvidence: "unresolved", ArchivedAt: f.s.now(), Publication: runtime.ArchivePublication{Upstream: "upstream/second", Fork: "owner/second", Branch: "osmia/second", PullRequest: 7}}
	must(t, f.s.store.SetArchived(first))
	must(t, f.s.store.SetArchived(second))
	before = trace.WorkgroupTraceOpenCount(id)
	tickWorkgroupPasses(t, f)
	if trace.WorkgroupTraceOpenCount(id) <= before {
		t.Fatal("unresolved archived member was skipped")
	}
	resolved, rev, err := trace.ResolvedWorkgroups(wt)
	must(t, err)
	if len(resolved) != 0 || rev != 0 {
		t.Fatalf("premature resolution %v revision %d", resolved, rev)
	}
	// Every member was already archived before the last PR's merge arrived.
	must(t, f.s.store.RecordArchivePullRequestMerge(second.Project, second.Workstream, second.Publication, "merged-second"))
	tickWorkgroupPasses(t, f)
	resolved, rev, err = trace.ResolvedWorkgroups(wt)
	must(t, err)
	if !slices.Equal(resolved, []config.WorkgroupID{id}) || rev != 1 {
		t.Fatalf("late merge resolution %v revision %d", resolved, rev)
	}
	assertQuiet := func() {
		t.Helper()
		opens := trace.WorkgroupTraceOpenCount(id)
		decodes, err := wt.RecordDecodeCount(host)
		must(t, err)
		for range 5 {
			tickWorkgroupPasses(t, f)
		}
		if got := trace.WorkgroupTraceOpenCount(id); got != opens {
			t.Fatalf("resolved ticks acquired trace %d times", got-opens)
		}
		got, err := wt.RecordDecodeCount(host)
		must(t, err)
		if got != decodes {
			t.Fatalf("resolved ticks decoded host %d times", got-decodes)
		}
	}
	assertQuiet()
	// Publish an approved shared revision and apply the same durable reopen
	// used by ApplyChange. The process and all controller handles stay alive.
	spec.Behavior += "\nOwner-approved clarification."
	must(t, trace.RecordWorkgroupRevision(ctx, wt, g, "approved-clarification", spec, f.s.now(), ownerActor))
	must(t, trace.ReopenWorkgroup(ctx, wt, id, f.s.now(), ownerActor, "approved-clarification"))
	if trace.WorkgroupResolutionCurrent(id) {
		t.Fatal("reopen left resolution cached")
	}
	before = trace.WorkgroupTraceOpenCount(id)
	tickWorkgroupPasses(t, f)
	if trace.WorkgroupTraceOpenCount(id) <= before {
		t.Fatal("reopened group was skipped")
	}
	resolved, rev, err = trace.ResolvedWorkgroups(wt)
	must(t, err)
	if !slices.Equal(resolved, []config.WorkgroupID{id}) || rev != 3 {
		t.Fatalf("second resolution period %v revision %d", resolved, rev)
	}
	assertQuiet()
	_, rev, err = trace.ResolvedWorkgroups(wt)
	must(t, err)
	if rev != 3 {
		t.Fatalf("duplicate second resolution: revision %d", rev)
	}
}

func TestApprovedChangeReactivatesResolvedWorkgroupWithoutRestart(t *testing.T) {
	f, wt, g, spec := resolvedTickFixture(t)
	ctx := context.Background()
	first, second := mustRepo(t, f.s, f.project), mustRepo(t, f.s, f.project2)
	for _, member := range []struct {
		repo   *trace.Repository
		stream config.WorkstreamID
	}{{first, f.stream1}, {second, f.stream2}} {
		h := trace.Header{Schema: "osmia.trace.transition", Version: trace.Version, ID: "resolve-fixture", Revision: 1, Project: member.repo.Project(), Workstream: member.stream, At: f.s.now(), Actor: ownerActor, Cause: "fixture"}
		_, err := member.repo.SetFeatureState(ctx, h, AbandonedState, "cancelled fixture work")
		must(t, err)
	}
	must(t, f.s.refreshRuntime())
	must(t, f.s.store.SetArchived(runtime.Archive{Project: f.project, Workstream: f.stream1, Workgroup: g.ID, Title: "Cancelled source", State: AbandonedState, Resolved: true, ResolutionEvidence: "cancelled", ArchivedAt: f.s.now()}))
	resolver := &workgroupResolver{s: f.s, repository: first}
	must(t, resolver.Pass(ctx))
	resolved, rev, err := trace.ResolvedWorkgroups(wt)
	must(t, err)
	if !slices.Equal(resolved, []config.WorkgroupID{g.ID}) || rev != 1 {
		t.Fatalf("initial resolution %v revision %d", resolved, rev)
	}
	opens := trace.WorkgroupTraceOpenCount(g.ID)
	for range 3 {
		must(t, resolver.Pass(ctx))
	}
	if trace.WorkgroupTraceOpenCount(g.ID) != opens {
		t.Fatal("resolved archive kept reopening authority")
	}
	// A clarification changes only shared prose, leaving member slices intact.
	after := spec
	after.Behavior += "\nClarify the approved shared intent."
	c := WorkgroupChange{ID: "clarification", Workgroup: g.ID, Host: g.Members[0], Origin: f.stream2, Request: ChangeRequest{Key: "clarification", Kind: "amend", Scope: "workgroup", Change: "Clarify the shared prose"}, Before: spec, After: after, State: "presented"}
	must(t, f.s.saveChange(ctx, wt, &c))
	c, api := f.s.decideChange(ctx, string(f.stream2), c.ID, ChangeDecision{Revision: c.Revision, Decision: "approve"})
	if api != nil {
		t.Fatal(api)
	}
	must(t, f.s.applyChange(ctx, wt, g, &c))
	if c.State != "applied" || trace.WorkgroupResolutionCurrent(g.ID) {
		t.Fatalf("approved change did not reopen: %+v", c)
	}
	resolved, rev, err = trace.ResolvedWorkgroups(wt)
	must(t, err)
	if len(resolved) != 0 || rev != 2 {
		t.Fatalf("reopen %v revision %d", resolved, rev)
	}
	must(t, f.s.store.SetArchived(runtime.Archive{Project: f.project2, Workstream: f.stream2, Workgroup: g.ID, Title: "Cancelled survivor", State: AbandonedState, Resolved: true, ResolutionEvidence: "cancelled", ArchivedAt: f.s.now()}))
	opens = trace.WorkgroupTraceOpenCount(g.ID)
	tickWorkgroupPasses(t, f)
	if trace.WorkgroupTraceOpenCount(g.ID) <= opens {
		t.Fatal("approved reopen was skipped")
	}
	resolved, rev, err = trace.ResolvedWorkgroups(wt)
	must(t, err)
	if !slices.Equal(resolved, []config.WorkgroupID{g.ID}) || rev != 3 {
		t.Fatalf("new resolution %v revision %d", resolved, rev)
	}
	opens = trace.WorkgroupTraceOpenCount(g.ID)
	for range 3 {
		tickWorkgroupPasses(t, f)
	}
	if trace.WorkgroupTraceOpenCount(g.ID) != opens {
		t.Fatal("new resolution did not take effect")
	}
}
