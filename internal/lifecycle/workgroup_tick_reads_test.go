package lifecycle_test

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	"slices"
	"testing"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/intake"
	"github.com/kpenfound/osmia/internal/lifecycle"
	"github.com/kpenfound/osmia/internal/runtime"
	"github.com/kpenfound/osmia/internal/trace"
)

func tickWorkgroupPasses(t *testing.T, f *crossRepoRatifyFixture) {
	t.Helper()
	for _, project := range []config.ProjectID{f.project, f.project2} {
		repo := mustRepo(t, f.s, project)
		fixturekit.Must(t, f.s.changePass(context.Background(), repo))
		fixturekit.Must(t, (&workgroupResolver{s: f.s, repository: repo}).Pass(context.Background()))
		fixturekit.Must(t, f.s.projectSpecDraftPass(context.Background(), repo))
		fixturekit.Must(t, f.s.projectSpecRefreshPass(context.Background(), repo))
	}
}

func resolvedTickFixture(t *testing.T) (*crossRepoRatifyFixture, *trace.Repository, trace.Workgroup, trace.ProjectSpec) {
	t.Helper()
	f, id, _ := projectSliceRefreshFixture(t)
	ctx := context.Background()
	wt, err := trace.OpenWorkgroupAuthority(f.s.current().Root, id)
	fixturekit.Must(t, err)
	t.Cleanup(func() { fixturekit.Must(t, wt.Close()) })
	groups, _, err := trace.Workgroups(wt)
	fixturekit.Must(t, err)
	g := groups[0]
	host := g.Members[0]
	spec, found, err := trace.ProjectSpecFor(wt, id)
	fixturekit.Must(t, err)
	if !found {
		t.Fatal("missing project spec")
	}
	approval := trace.GroupApproval{Workgroup: id, ProjectSpec: spec, Complete: true, Members: []trace.GroupMemberApproval{
		{Project: f.project, Workstream: f.stream1, Record: json.RawMessage(`{}`)},
		{Project: f.project2, Workstream: f.stream2, Record: json.RawMessage(`{}`)},
	}}
	fixturekit.Must(t, trace.RecordGroupApproval(ctx, wt, host, approval, f.s.now(), intake.OwnerActor))
	return f, wt, g, spec
}

func TestArchivedResolvedWorkgroupTicksAvoidTraceReadsAndObserveReopen(t *testing.T) {
	t.Parallel()
	f, wt, g, spec := resolvedTickFixture(t)
	ctx := context.Background()
	id, host := g.ID, g.Members[0]
	docs := make([]trace.Document, 1200)
	for i := range docs {
		docs[i] = trace.Document{Header: trace.Header{Schema: "osmia.trace.document", Version: trace.Version, ID: fmt.Sprintf("history-%d", i), Revision: 1, Project: wt.Project(), Workstream: host, At: f.s.now(), Actor: intake.OwnerActor, Cause: "fixture"}, Path: fmt.Sprintf("tools/history-%d.json", i), Content: `{}`}
	}
	fixturekit.Must(t, wt.RecordDocuments(ctx, docs))
	before := trace.WorkgroupTraceOpenCount(id)
	tickWorkgroupPasses(t, f)
	if trace.WorkgroupTraceOpenCount(id) <= before {
		t.Fatal("live workgroup was skipped")
	}
	fixturekit.Must(t, f.s.refreshRuntime())
	first := runtime.Archive{Project: f.project, Workstream: f.stream1, Workgroup: id, Title: "Cancelled member", State: intake.AbandonedState, Resolved: true, ResolutionEvidence: "cancelled", ArchivedAt: f.s.now()}
	second := runtime.Archive{Project: f.project2, Workstream: f.stream2, Workgroup: id, Title: "Open publication", State: intake.DeliveredState, ResolutionEvidence: "unresolved", ArchivedAt: f.s.now(), Publication: runtime.ArchivePublication{Upstream: "upstream/second", Fork: "owner/second", Branch: "osmia/second", PullRequest: 7}}
	fixturekit.Must(t, f.s.store.SetArchived(first))
	fixturekit.Must(t, f.s.store.SetArchived(second))
	before = trace.WorkgroupTraceOpenCount(id)
	tickWorkgroupPasses(t, f)
	if trace.WorkgroupTraceOpenCount(id) <= before {
		t.Fatal("unresolved archived member was skipped")
	}
	resolved, rev, err := trace.ResolvedWorkgroups(wt)
	fixturekit.Must(t, err)
	if len(resolved) != 0 || rev != 0 {
		t.Fatalf("premature resolution %v revision %d", resolved, rev)
	}
	fixturekit.Must(t, f.s.store.RecordArchivePullRequestMerge(second.Project, second.Workstream, second.Publication, "merged-second"))
	tickWorkgroupPasses(t, f)
	resolved, rev, err = trace.ResolvedWorkgroups(wt)
	fixturekit.Must(t, err)
	if !slices.Equal(resolved, []config.WorkgroupID{id}) || rev != 1 {
		t.Fatalf("late merge resolution %v revision %d", resolved, rev)
	}
	assertQuiet := func() {
		t.Helper()
		opens := trace.WorkgroupTraceOpenCount(id)
		decodes, err := wt.RecordDecodeCount(host)
		fixturekit.Must(t, err)
		for range 5 {
			tickWorkgroupPasses(t, f)
		}
		if got := trace.WorkgroupTraceOpenCount(id); got != opens {
			t.Fatalf("resolved ticks acquired trace %d times", got-opens)
		}
		got, err := wt.RecordDecodeCount(host)
		fixturekit.Must(t, err)
		if got != decodes {
			t.Fatalf("resolved ticks decoded host %d times", got-decodes)
		}
	}
	assertQuiet()
	spec.Behavior += "\nOwner-approved clarification."
	fixturekit.Must(t, trace.RecordWorkgroupRevision(ctx, wt, g, "approved-clarification", spec, f.s.now(), intake.OwnerActor))
	fixturekit.Must(t, trace.ReopenWorkgroup(ctx, wt, id, f.s.now(), intake.OwnerActor, "approved-clarification"))
	if trace.WorkgroupResolutionCurrent(id) {
		t.Fatal("reopen left resolution cached")
	}
	before = trace.WorkgroupTraceOpenCount(id)
	tickWorkgroupPasses(t, f)
	if trace.WorkgroupTraceOpenCount(id) <= before {
		t.Fatal("reopened group was skipped")
	}
	resolved, rev, err = trace.ResolvedWorkgroups(wt)
	fixturekit.Must(t, err)
	if !slices.Equal(resolved, []config.WorkgroupID{id}) || rev != 3 {
		t.Fatalf("second resolution period %v revision %d", resolved, rev)
	}
	assertQuiet()
	_, rev, err = trace.ResolvedWorkgroups(wt)
	fixturekit.Must(t, err)
	if rev != 3 {
		t.Fatalf("duplicate second resolution: revision %d", rev)
	}
}

func TestApprovedChangeReactivatesResolvedWorkgroupWithoutRestart(t *testing.T) {
	t.Parallel()
	f, wt, g, spec := resolvedTickFixture(t)
	ctx := context.Background()
	first, second := mustRepo(t, f.s, f.project), mustRepo(t, f.s, f.project2)
	for _, member := range []struct {
		repo   *trace.Repository
		stream config.WorkstreamID
	}{{first, f.stream1}, {second, f.stream2}} {
		h := trace.Header{Schema: "osmia.trace.transition", Version: trace.Version, ID: "resolve-fixture", Revision: 1, Project: member.repo.Project(), Workstream: member.stream, At: f.s.now(), Actor: intake.OwnerActor, Cause: "fixture"}
		_, err := member.repo.SetFeatureState(ctx, h, intake.AbandonedState, "cancelled fixture work")
		fixturekit.Must(t, err)
	}
	fixturekit.Must(t, f.s.refreshRuntime())
	fixturekit.Must(t, f.s.store.SetArchived(runtime.Archive{Project: f.project, Workstream: f.stream1, Workgroup: g.ID, Title: "Cancelled source", State: intake.AbandonedState, Resolved: true, ResolutionEvidence: "cancelled", ArchivedAt: f.s.now()}))
	resolver := &workgroupResolver{s: f.s, repository: first}
	fixturekit.Must(t, resolver.Pass(ctx))
	resolved, rev, err := trace.ResolvedWorkgroups(wt)
	fixturekit.Must(t, err)
	if !slices.Equal(resolved, []config.WorkgroupID{g.ID}) || rev != 1 {
		t.Fatalf("initial resolution %v revision %d", resolved, rev)
	}
	opens := trace.WorkgroupTraceOpenCount(g.ID)
	for range 3 {
		fixturekit.Must(t, resolver.Pass(ctx))
	}
	if trace.WorkgroupTraceOpenCount(g.ID) != opens {
		t.Fatal("resolved archive kept reopening authority")
	}
	after := spec
	after.Behavior += "\nClarify the approved shared intent."
	c := lifecycle.WorkgroupChange{ID: "clarification", Workgroup: g.ID, Host: g.Members[0], Origin: f.stream2, Request: lifecycle.ChangeRequest{Key: "clarification", Kind: "amend", Scope: "workgroup", Change: "Clarify the shared prose"}, Before: spec, After: after, State: "presented"}
	fixturekit.Must(t, f.s.saveChange(ctx, wt, &c))
	c, apiFault := f.s.decideChange(ctx, string(f.stream2), c.ID, lifecycle.ChangeDecision{Revision: c.Revision, Decision: "approve"})
	if apiFault != nil {
		t.Fatal(apiFault)
	}
	fixturekit.Must(t, f.s.applyChange(ctx, wt, g, &c))
	if c.State != "applied" || trace.WorkgroupResolutionCurrent(g.ID) {
		t.Fatalf("approved change did not reopen: %+v", c)
	}
	resolved, rev, err = trace.ResolvedWorkgroups(wt)
	fixturekit.Must(t, err)
	if len(resolved) != 0 || rev != 2 {
		t.Fatalf("reopen %v revision %d", resolved, rev)
	}
	fixturekit.Must(t, f.s.store.SetArchived(runtime.Archive{Project: f.project2, Workstream: f.stream2, Workgroup: g.ID, Title: "Cancelled survivor", State: intake.AbandonedState, Resolved: true, ResolutionEvidence: "cancelled", ArchivedAt: f.s.now()}))
	opens = trace.WorkgroupTraceOpenCount(g.ID)
	tickWorkgroupPasses(t, f)
	if trace.WorkgroupTraceOpenCount(g.ID) <= opens {
		t.Fatal("approved reopen was skipped")
	}
	resolved, rev, err = trace.ResolvedWorkgroups(wt)
	fixturekit.Must(t, err)
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
