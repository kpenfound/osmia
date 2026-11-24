package lifecycle_test

import (
	"context"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/kpenfound/osmia/internal/api"
	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/intake"
	"github.com/kpenfound/osmia/internal/lifecycle"
	"github.com/kpenfound/osmia/internal/runtime"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	"github.com/kpenfound/osmia/internal/servicetest/intakefixture"
	"github.com/kpenfound/osmia/internal/trace"
)

func logUnsettledLifecycleOperations(t *testing.T, repo *trace.Repository, stream config.WorkstreamID, services ...*Service) {
	t.Helper()
	var lookup func(coreadapter.Operation) (string, string, trace.Thread, error)
	if len(services) > 0 {
		lookup = func(op coreadapter.Operation) (string, string, trace.Thread, error) {
			delta, err := lifecycle.DecodeDeltaTurn(op)
			if err != nil {
				return "", "", trace.Thread{}, err
			}
			wt, c, _, close, err := (deltaRunner{s: services[0]}).load(context.Background(), delta)
			if err != nil {
				return "", "", trace.Thread{}, err
			}
			defer close()
			agent, turn := delta.Agent, lifecycle.DeltaTurnID(c, delta.Agent, delta.Tool)
			th, err := wt.Thread(c.Host, agent)
			return agent, turn, th, err
		}
	}
	fixturekit.LogUnsettledLifecycleOperations(t, repo, stream, lifecycle.DeltaTurnAction, lookup)
}

func TestLiveWorkgroupLeaderHandlesRetiredMembersAndErrors(t *testing.T) {
	t.Parallel()
	const a config.WorkstreamID = "w_00000000000000000000000000000001"
	const b config.WorkstreamID = "w_00000000000000000000000000000002"
	const group config.WorkgroupID = "g_00000000000000000000000000000001"
	cfg, _, clock := fixturekit.HardPauseRepository(t, 1)
	store, _, err := runtime.Open(runtime.Inputs{Config: cfg, Workstreams: map[config.ProjectID][]config.WorkstreamID{fixturekit.Project: {a, b}}})
	fixturekit.Must(t, err)
	t.Cleanup(func() { fixturekit.Must(t, store.Close()) })
	s := &Service{cfg: cfg, store: store}
	g := trace.Workgroup{ID: group, Members: []config.WorkstreamID{fixturekit.Stream, b, a}}
	leader, err := s.liveWorkgroupLeader(g)
	fixturekit.Must(t, err)
	if leader != a {
		t.Fatalf("leader %s", leader)
	}
	for _, member := range []config.WorkstreamID{a, b} {
		fixturekit.Must(t, s.store.SetArchived(runtime.Archive{Project: fixturekit.Project, Workstream: member, Workgroup: group, ArchivedAt: clock.Now()}))
	}
	leader, err = s.liveWorkgroupLeader(g)
	fixturekit.Must(t, err)
	if leader != "" {
		t.Fatalf("retired leader %s", leader)
	}
	g.ID = "g_00000000000000000000000000000002"
	if leader, err = s.liveWorkgroupLeader(g); err == nil || leader != "" {
		t.Fatalf("invalid retirement leader=%s err=%v", leader, err)
	}
	for _, members := range [][]config.WorkstreamID{nil, {fixturekit.Stream}} {
		g.Members = members
		if leader, err = s.liveWorkgroupLeader(g); err == nil || leader != "" {
			t.Fatalf("invalid membership leader=%s err=%v", leader, err)
		}
	}
}
func TestDesignProductionFullyRetiredWorkgroupAcrossRestart(t *testing.T) {
	t.Parallel()
	for _, pending := range []bool{false, true} {
		name := "completed"
		if pending {
			name = "pending-decision"
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			testFullyRetiredWorkgroup(t, pending)
		})
	}
}

func holdRetirementSourceEvents(t *testing.T, f *productionDesignFixture) func() {
	t.Helper()
	released, reached := make(chan struct{}), make(chan struct{})
	markReached := sync.OnceFunc(func() { close(reached) })
	var release func()
	_, projects := f.s.runtimes()
	installed := false
	for _, p := range projects {
		if p.id != f.project {
			continue
		}
		original := p.pipeline.Current.Load()
		held := *original
		release = sync.OnceFunc(func() {
			close(released)
			p.pipeline.Next.Store(original)
		})
		held.Hooks = slices.Clone(held.Hooks)
		for i, hook := range held.Hooks {
			if hook.Name != "events" {
				continue
			}
			held.Hooks[i].Pass = func(ctx context.Context) error {
				select {
				case <-ctx.Done():
					release()
					return ctx.Err()
				case <-released:
					return hook.Pass(ctx)
				default:
					markReached()
					return nil
				}
			}
			installed = true
		}
		p.pipeline.Next.Store(&held)
		stopRelease := context.AfterFunc(f.s.lifetime, release)
		t.Cleanup(func() {
			release()
			stopRelease()
		})
	}
	if !installed {
		t.Fatal("source project has no event delivery stage")
	}
	lifecycleAwait(t, f, "source event delivery held", func() bool {
		select {
		case <-reached:
			return true
		default:
			return false
		}
	})
	return release
}
func testFullyRetiredWorkgroup(t *testing.T, pending bool) {
	ctx := context.Background()
	var observe atomic.Bool
	var passes atomic.Int32
	var releaseEvents func()
	f := resolutionApprovedFixture(t, productionDesignConfiguration{wait: lifecycleAwait, options: func(o *Options) {
		previous := o.schedulePassed
		o.schedulePassed = func() {
			if previous != nil {
				previous()
			}
			if observe.Load() {
				passes.Add(1)
			}
		}
	}}, trace.EdgeContract, nil, func(f *productionDesignFixture) {
		releaseEvents = holdRetirementSourceEvents(t, f)
	})
	defer f.stop(t)
	deliverResolutionMembers(t, f)
	repo, err := f.s.repository(f.project)
	fixturekit.Must(t, err)
	authority, host, _, release, err := f.s.chiefRepository(ctx, repo, f.stream1)
	fixturekit.Must(t, err)
	approval, pins, found, err := trace.ReadGroupApproval(authority, host)
	fixturekit.Must(t, err)
	release()
	if !found || !approval.Complete {
		t.Fatal("missing completed group approval")
	}
	for _, m := range resolutionMembers(f) {
		r, err := f.s.repository(m.project)
		fixturekit.Must(t, err)
		recordResolutionMerge(t, r, m.stream)
	}
	awaitMemberMergeResolution(t, f)
	var changes []lifecycle.WorkgroupChange
	var journals []trace.Document
	readJournals := func(wt *trace.Repository) []trace.Document {
		docs, err := trace.Read[trace.Document](wt, host)
		fixturekit.Must(t, err)
		return slices.DeleteFunc(docs, func(doc trace.Document) bool {
			return !strings.HasPrefix(doc.ID, "workgroup-change-")
		})
	}
	unlock := func() {}
	if pending {
		change, apiFault := f.s.requestChange(ctx, string(f.origin), lifecycle.ChangeRequest{Key: "retirement-decision", Kind: "cancel", Scope: "workgroup", Change: "Retain the owner's project decision across retirement."})
		if apiFault != nil {
			t.Fatal(apiFault)
		}
		lifecycleAwait(t, f, "presented retirement decision", func() bool {
			wt, err := trace.OpenWorkgroupAuthority(f.s.current().Root, approval.Workgroup)
			fixturekit.Must(t, err)
			defer wt.Close()
			got, err := lifecycle.ReadChanges(wt, host)
			fixturekit.Must(t, err)
			if len(got) == 1 && got[0].State == "presented" {
				change = got[0]
				return true
			}
			return false
		})
		lock := f.s.changeApplicationLock(approval.Workgroup)
		lock.RLock()
		unlock = sync.OnceFunc(lock.RUnlock)
		defer unlock()
		decided, apiFault := f.s.decideChange(ctx, string(f.origin), change.ID, lifecycle.ChangeDecision{Revision: change.Revision, Decision: "approve", PullRequests: "draft"})
		if apiFault != nil {
			t.Fatal(apiFault)
		}
		if decided.State != "approved" || decided.Decision == nil {
			t.Fatal("owner approval was not recorded")
		}
		changes = []lifecycle.WorkgroupChange{decided}
		wt, err := trace.OpenWorkgroupAuthority(f.s.current().Root, approval.Workgroup)
		fixturekit.Must(t, err)
		journals = readJournals(wt)
		fixturekit.Must(t, wt.Close())
	}
	sourceEvents, err := repo.Outbox(f.origin)
	fixturekit.Must(t, err)
	if !slices.ContainsFunc(sourceEvents, func(e trace.OutboxEntry) bool {
		return e.Event.ID == trace.EventID(intake.DesignStartTransition, "state") &&
			e.Event.Kind == trace.ProgressKind && e.Event.Operation == nil &&
			!e.Acknowledged && e.Claim == nil
	}) {
		t.Fatal("design-start event was not held before archive")
	}
	shared, err := f.c.Conversation(ctx, f.origin)
	fixturekit.Must(t, err)
	if len(shared.Entries) == 0 {
		t.Fatal("the shared conversation is empty before the members are archived")
	}
	for _, m := range resolutionMembers(f) {
		_, err := f.c.Archive(ctx, m.stream)
		fixturekit.Must(t, err)
	}
	unlock()
	var retainedEvents []trace.OutboxEntry
	fixturekit.Must(t, repo.Serialize(func() error {
		var err error
		retainedEvents, err = repo.Outbox(f.origin)
		return err
	}))
	releaseEvents()
	lifecycleAwait(t, f, "all members cleaned", func() bool {
		for _, m := range resolutionMembers(f) {
			a, ok := intakefixture.ArchiveRecord(t, f.s.store, m.stream)
			if !ok || a.CleanedAt.IsZero() {
				return false
			}
		}
		return true
	})
	if !slices.ContainsFunc(retainedEvents, func(e trace.OutboxEntry) bool {
		return e.Event.Operation == nil && !e.Acknowledged
	}) {
		t.Fatal("retained source has no pending event to preserve")
	}
	check := func() {
		t.Helper()
		r, err := f.s.repository(f.project)
		fixturekit.Must(t, err)
		id, err := r.CrossRepoWorkgroupOf(f.origin)
		fixturekit.Must(t, err)
		if id != approval.Workgroup {
			t.Fatal("adopted source lost its locator")
		}
		outbox, err := r.Outbox(f.origin)
		fixturekit.Must(t, err)
		if !intakefixture.EqualJSONTest(outbox, retainedEvents) {
			t.Fatal("fully retired group changed pending event history")
		}
		wt, err := trace.OpenWorkgroupAuthority(f.s.current().Root, id)
		fixturekit.Must(t, err)
		defer wt.Close()
		got, doc, found, err := trace.ReadGroupApproval(wt, host)
		fixturekit.Must(t, err)
		if !found || !intakefixture.EqualJSONTest(got, approval) || !intakefixture.EqualJSONTest(doc, pins) {
			t.Fatal("retirement changed completed approval")
		}
		if pending {
			retained, err := lifecycle.ReadChanges(wt, host)
			fixturekit.Must(t, err)
			if !intakefixture.EqualJSONTest(retained, changes) {
				t.Fatal("retirement changed the pending decision or its exact pins")
			}
			if !intakefixture.EqualJSONTest(journals, readJournals(wt)) {
				t.Fatal("retirement changed the recorded journal bytes or revision history")
			}
		}
		read, err := f.c.Conversation(ctx, f.origin)
		fixturekit.Must(t, err)
		for _, earlier := range shared.Entries {
			if !slices.ContainsFunc(read.Entries, func(e api.ConversationEntry) bool {
				return e.Turn == earlier.Turn && e.Kind == earlier.Kind && e.Text == earlier.Text
			}) {
				t.Fatalf("the shared conversation lost %+v", earlier)
			}
		}
		for _, e := range read.Entries {
			if strings.HasPrefix(e.Turn, "archive-") {
				t.Fatalf("an owner archive was announced: %+v", e)
			}
		}
		groups, _, err := trace.Workgroups(wt)
		fixturekit.Must(t, err)
		starting := &Service{cfg: f.s.current(), store: f.s.store}
		retired, err := starting.workgroupFullyRetired(ctx, groups[0])
		fixturekit.Must(t, err)
		if !retired {
			t.Fatal("retirement proof required a member runtime")
		}
		leader, err := f.s.liveWorkgroupLeader(groups[0])
		fixturekit.Must(t, err)
		if leader != "" {
			t.Fatal("retired group has a live leader")
		}
		resolved, revision, err := trace.ResolvedWorkgroups(wt)
		fixturekit.Must(t, err)
		if !slices.Equal(resolved, []config.WorkgroupID{id}) || revision != 1 {
			t.Fatalf("resolution changed: %v %d", resolved, revision)
		}
		active, err := trace.ProjectSpecActive(wt, id)
		fixturekit.Must(t, err)
		if active {
			t.Fatal("fully resolved spec remained active")
		}
		for _, m := range resolutionMembers(f) {
			gone, err := trace.WorkstreamTraceGone(f.s.current().Root, m.project, m.stream)
			fixturekit.Must(t, err)
			if !gone {
				t.Fatal("retired history recreated")
			}
		}
	}
	check()
	f.stop(t)
	logged, logger := captureLog(t)
	f.opts.Logger = logger
	observe.Store(true)
	f.start(t)
	lifecycleAwait(t, f, "retired group controller passes", func() bool { return passes.Load() >= 4 })
	check()
	if held := strings.Contains(logged.String(), "workgroup changes remain held"); held != pending {
		t.Fatalf("held warning logged=%v with pending decision=%v", held, pending)
	}
	if !f.s.ready.Load() || f.s.lifetime.Err() != nil {
		t.Fatal("fully retired group stopped service recovery")
	}
}
