package lifecycle_test

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/runtime"
	"github.com/kpenfound/osmia/internal/service"
	"github.com/kpenfound/osmia/internal/trace"
)

// idleSchedulerSteps are the production schedule hooks that read a cross-repo
// workgroup's approval, project spec, membership or edges.
var idleSchedulerSteps = []string{"group-approval", "edge", "base", "workgroup-resolution", "workgroup-project-spec", "workgroup-project-spec-refresh", "masons", "final-review", "publish", "archive-on-merge"}

// TestIdleLargeSpecWorkgroupStaysBoundedThroughServicePasses runs the service's
// own schedule hooks, network observers and conversation-path discovery over a
// cross-repo workgroup with a large approval and project spec. It does so
// while the members are building, after every member is abandoned and the
// workgroup is recorded resolved, and once the members are archived. Across
// each phase the approval and project spec are decoded and the workgroup trace
// is opened from disk a bounded number of times, however many passes run.
func TestIdleLargeSpecWorkgroupStaysBoundedThroughServicePasses(t *testing.T) {
	f, id, _ := projectSliceRefreshFixture(t)
	ctx := context.Background()
	root := f.s.current().Root
	wt, err := trace.OpenWorkgroupAuthority(root, id)
	must(t, err)
	groups, _, err := trace.Workgroups(wt)
	must(t, err)
	g := groups[0]
	spec, found, err := trace.ProjectSpecFor(wt, id)
	must(t, err)
	if !found {
		t.Fatal("missing project spec")
	}
	spec.Behavior += strings.Repeat(idleSpecUnit, 3500)
	approval := trace.GroupApproval{Workgroup: id, ProjectSpec: spec, Complete: true, Members: []trace.GroupMemberApproval{
		{Project: f.project, Workstream: f.stream1, Record: json.RawMessage(`{}`)},
		{Project: f.project2, Workstream: f.stream2, Record: json.RawMessage(`{}`)},
	}}
	must(t, trace.RecordGroupApproval(ctx, wt, g.Members[0], approval, f.s.now(), ownerActor))
	must(t, wt.Close())

	members := []struct {
		project config.ProjectID
		stream  config.WorkstreamID
	}{{f.project, f.stream1}, {f.project2, f.stream2}}
	setFeature := func(state string) {
		t.Helper()
		for _, m := range members {
			repo := mustRepo(t, f.s, m.project)
			h := trace.Header{Schema: "osmia.trace.transition", Version: trace.Version, ID: "idle-" + state, Revision: 1, Project: m.project, Workstream: m.stream, At: f.s.now(), Actor: ownerActor, Cause: "fixture"}
			_, err := repo.SetFeatureState(ctx, h, state, "fixture")
			must(t, err)
		}
		must(t, f.s.refreshRuntime())
	}
	setFeature(BuildingState)

	type step struct {
		name string
		pass func(context.Context) error
	}
	var schedule, observe []step
	var discoveries []func(context.Context) error
	for _, m := range members {
		cfg, _ := f.s.runtimeOf(m.project)
		repo := mustRepo(t, f.s, m.project)
		stages, err := f.s.stages(cfg, repo)
		must(t, err)
		for _, hook := range stages.Hooks {
			if slices.Contains(idleSchedulerSteps, hook.Name) {
				schedule = append(schedule, step{hook.Name, hook.Pass})
			}
		}
		for _, hook := range stages.Polls {
			observe = append(observe, step{hook.Name, hook.Pass})
		}
		discover, release := service.InspectForTest(f.s.core()).ConversationDiscovery(repo)
		t.Cleanup(release)
		discoveries = append(discoveries, discover)
	}
	for _, required := range []string{"group-approval", "workgroup-resolution", "workgroup-project-spec-refresh"} {
		if !slices.ContainsFunc(schedule, func(s step) bool { return s.name == required }) {
			t.Fatalf("the schedule has no %s step", required)
		}
	}
	for _, required := range []string{"workgroup-change"} {
		if !slices.ContainsFunc(observe, func(s step) bool { return s.name == required }) {
			t.Fatalf("the observers have no %s poll", required)
		}
	}
	// readers names, for each step of a round, what it read of the workgroup.
	readers := map[string]workgroupReads{}
	measure := func(name string, run func() error) {
		t.Helper()
		before := readsOf(id)
		disk := trace.WorkgroupTraceDiskOpenCount(id)
		if err := run(); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		after := readsOf(id)
		sum := readers[name]
		sum.approvals += after.approvals - before.approvals
		sum.specs += after.specs - before.specs
		sum.lookups += after.lookups - before.lookups
		sum.acquisitions += trace.WorkgroupTraceDiskOpenCount(id) - disk
		readers[name] = sum
	}
	round := func() {
		t.Helper()
		for _, s := range schedule {
			measure("schedule "+s.name, func() error { return s.pass(ctx) })
		}
		for _, s := range observe {
			measure("observer "+s.name, func() error { return s.pass(ctx) })
		}
		for _, discover := range discoveries {
			measure("conversation discovery", func() error { return discover(ctx) })
		}
	}
	reported := func(want string) {
		t.Helper()
		for _, m := range members {
			repo := mustRepo(t, f.s, m.project)
			states, err := repo.WorkflowStates(m.stream)
			must(t, err)
			stored, err := repo.Workflow(m.stream, trace.FeatureSubject)
			must(t, err)
			if got := states[trace.FeatureSubject].Value; got != want || stored.Value != want {
				t.Fatalf("member %s reports %q (stored %q), want %q", m.stream, got, stored.Value, want)
			}
		}
	}
	resolution := func(wantResolved bool) {
		t.Helper()
		authority, err := trace.OpenWorkgroupAuthority(root, id)
		must(t, err)
		defer authority.Close()
		ids, revision, err := trace.ResolvedWorkgroups(authority)
		must(t, err)
		active, err := trace.ProjectSpecActive(authority, id)
		must(t, err)
		if wantResolved {
			if !slices.Equal(ids, []config.WorkgroupID{id}) || revision != 1 || active {
				t.Fatalf("resolution=%v revision=%d active=%v", ids, revision, active)
			}
		} else if len(ids) != 0 || revision != 0 || !active {
			t.Fatalf("premature resolution=%v revision=%d active=%v", ids, revision, active)
		}
	}
	// bounded runs passes until the answers are warm, then many more, and
	// requires that the extra passes decoded no document and opened no trace
	// from disk. Shared-trace acquisitions only bump a count on the handle a
	// project keeps, so they are bounded per pass, not required to be zero.
	bounded := func(phase string) {
		t.Helper()
		for range 3 {
			round()
		}
		clear(readers)
		before := readsOf(id)
		disk := trace.WorkgroupTraceDiskOpenCount(id)
		const passes = 25
		for range passes {
			round()
		}
		after := readsOf(id)
		if after.approvals != before.approvals || after.specs != before.specs {
			t.Fatalf("%s: %d passes decoded the approval %d times and the project spec %d times; by step (approval decodes, project spec decodes, disk opens): %+v", phase, passes, after.approvals-before.approvals, after.specs-before.specs, readers)
		}
		if got := trace.WorkgroupTraceDiskOpenCount(id) - disk; got != 0 {
			t.Fatalf("%s: %d passes opened the workgroup trace from disk %d times; by step: %+v", phase, passes, got, readers)
		}
		if got := after.acquisitions - before.acquisitions; got > passes*16 {
			t.Fatalf("%s: %d passes acquired the shared trace %d times", phase, passes, got)
		}
	}

	bounded("building")
	reported(BuildingState)
	resolution(false)

	setFeature(AbandonedState)
	bounded("abandoned")
	reported(AbandonedState)
	resolution(true)

	for _, m := range members {
		must(t, f.s.store.SetArchived(runtime.Archive{Project: m.project, Workstream: m.stream, Workgroup: id, Title: "Cancelled member", State: AbandonedState, Resolved: true, ResolutionEvidence: "cancelled", ArchivedAt: f.s.now()}))
	}
	must(t, f.s.refreshRuntime())
	bounded("archived")
	reported(AbandonedState)
	resolution(true)
}
