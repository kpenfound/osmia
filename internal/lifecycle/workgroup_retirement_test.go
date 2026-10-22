package lifecycle_test

import (
	"context"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/runtime"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	"github.com/kpenfound/osmia/internal/trace"
)

func logUnsettledLifecycleOperations(t *testing.T, repo *trace.Repository, stream config.WorkstreamID, services ...*Service) {
	t.Helper()
	var lookup func(coreadapter.Operation) (string, string, trace.Thread, error)
	if len(services) > 0 {
		lookup = func(op coreadapter.Operation) (string, string, trace.Thread, error) {
			delta, err := decodeDeltaTurn(op)
			if err != nil {
				return "", "", trace.Thread{}, err
			}
			wt, c, _, close, err := (deltaRunner{s: services[0]}).load(context.Background(), delta)
			if err != nil {
				return "", "", trace.Thread{}, err
			}
			defer close()
			agent, turn := delta.Agent, deltaTurnID(c, delta.Agent, delta.Tool)
			th, err := wt.Thread(c.Host, agent)
			return agent, turn, th, err
		}
	}
	fixturekit.LogUnsettledLifecycleOperations(t, repo, stream, deltaTurnAction, lookup)
}

func TestLiveWorkgroupLeaderHandlesRetiredMembersAndErrors(t *testing.T) {
	const a config.WorkstreamID = "w_00000000000000000000000000000001"
	const b config.WorkstreamID = "w_00000000000000000000000000000002"
	const group config.WorkgroupID = "g_00000000000000000000000000000001"
	cfg, _, clock := hardPauseRepository(t, 1)
	store, _, err := runtime.Open(runtime.Inputs{Config: cfg, Workstreams: map[config.ProjectID][]config.WorkstreamID{project: {a, b}}})
	must(t, err)
	t.Cleanup(func() { must(t, store.Close()) })
	s := &Service{cfg: cfg, store: store}
	g := trace.Workgroup{ID: group, Members: []config.WorkstreamID{stream, b, a}}
	leader, err := s.liveWorkgroupLeader(g)
	must(t, err)
	if leader != a {
		t.Fatalf("leader %s", leader)
	}
	for _, member := range []config.WorkstreamID{a, b} {
		must(t, s.store.SetArchived(runtime.Archive{Project: project, Workstream: member, Workgroup: group, ArchivedAt: clock.Now()}))
	}
	leader, err = s.liveWorkgroupLeader(g)
	must(t, err)
	if leader != "" {
		t.Fatalf("retired leader %s", leader)
	}
	g.ID = "g_00000000000000000000000000000002"
	if leader, err = s.liveWorkgroupLeader(g); err == nil || leader != "" {
		t.Fatalf("invalid retirement leader=%s err=%v", leader, err)
	}
	for _, members := range [][]config.WorkstreamID{nil, {stream}} {
		g.Members = members
		if leader, err = s.liveWorkgroupLeader(g); err == nil || leader != "" {
			t.Fatalf("invalid membership leader=%s err=%v", leader, err)
		}
	}
}
func TestDesignProductionFullyRetiredWorkgroupAcrossRestart(t *testing.T) {
	for _, pending := range []bool{false, true} {
		name := "completed"
		if pending {
			name = "pending-decision"
		}
		t.Run(name, func(t *testing.T) { testFullyRetiredWorkgroup(t, pending) })
	}
}

// holdRetirementSourceEvents keeps the design-start event pending while the
// live controllers plan, resolve and archive the group. Publishing the stage
// through next waits out any pass already using the previous event hook.
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
	must(t, err)
	authority, host, _, release, err := f.s.chiefRepository(ctx, repo, f.stream1)
	must(t, err)
	approval, pins, found, err := trace.ReadGroupApproval(authority, host)
	must(t, err)
	release()
	if !found || !approval.Complete {
		t.Fatal("missing completed group approval")
	}
	for _, m := range resolutionMembers(f) {
		r, err := f.s.repository(m.project)
		must(t, err)
		recordResolutionMerge(t, r, m.stream)
	}
	awaitMemberMergeResolution(t, f)
	var changes []WorkgroupChange
	var journals []trace.Document
	readJournals := func(wt *trace.Repository) []trace.Document {
		docs, err := trace.Read[trace.Document](wt, host)
		must(t, err)
		return slices.DeleteFunc(docs, func(doc trace.Document) bool {
			return !strings.HasPrefix(doc.ID, "workgroup-change-")
		})
	}
	unlock := func() {}
	if pending {
		change, api := f.s.requestChange(ctx, string(f.origin), ChangeRequest{Key: "retirement-decision", Kind: "cancel", Scope: "workgroup", Change: "Retain the owner's project decision across retirement."})
		if api != nil {
			t.Fatal(api)
		}
		lifecycleAwait(t, f, "presented retirement decision", func() bool {
			wt, err := trace.OpenWorkgroupAuthority(f.s.current().Root, approval.Workgroup)
			must(t, err)
			defer wt.Close()
			got, err := readChanges(wt, host)
			must(t, err)
			if len(got) == 1 && got[0].State == "presented" {
				change = got[0]
				return true
			}
			return false
		})
		// Hold effects at the same boundary as owner archive. The real
		// controller presents the request; the owner approves its exact pin.
		lock := f.s.changeApplicationLock(approval.Workgroup)
		lock.RLock()
		unlock = sync.OnceFunc(lock.RUnlock)
		defer unlock()
		decided, api := f.s.decideChange(ctx, string(f.origin), change.ID, ChangeDecision{Revision: change.Revision, Decision: "approve", PullRequests: "draft"})
		if api != nil {
			t.Fatal(api)
		}
		if decided.State != "approved" || decided.Decision == nil {
			t.Fatal("owner approval was not recorded")
		}
		changes = []WorkgroupChange{decided}
		wt, err := trace.OpenWorkgroupAuthority(f.s.current().Root, approval.Workgroup)
		must(t, err)
		journals = readJournals(wt)
		must(t, wt.Close())
	}
	// The owner's real design start records this progress event in the
	// retained source. It must still be undelivered before members retire.
	sourceEvents, err := repo.Outbox(f.origin)
	must(t, err)
	if !slices.ContainsFunc(sourceEvents, func(e trace.OutboxEntry) bool {
		return e.Event.ID == trace.EventID(designStartTransition, "state") &&
			e.Event.Kind == trace.ProgressKind && e.Event.Operation == nil &&
			!e.Acknowledged && e.Claim == nil
	}) {
		t.Fatal("design-start event was not held before archive")
	}
	for _, m := range resolutionMembers(f) {
		_, err := f.c.Archive(ctx, m.stream)
		must(t, err)
	}
	unlock()
	var retainedEvents []trace.OutboxEntry
	must(t, repo.Serialize(func() error {
		var err error
		retainedEvents, err = repo.Outbox(f.origin)
		return err
	}))
	releaseEvents()
	lifecycleAwait(t, f, "all members cleaned", func() bool {
		for _, m := range resolutionMembers(f) {
			a, ok := archiveRecord(t, f.s.store, m.stream)
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
		must(t, err)
		id, err := r.CrossRepoWorkgroupOf(f.origin)
		must(t, err)
		if id != approval.Workgroup {
			t.Fatal("adopted source lost its locator")
		}
		outbox, err := r.Outbox(f.origin)
		must(t, err)
		if !equalJSONTest(outbox, retainedEvents) {
			t.Fatal("fully retired group changed pending event history")
		}
		wt, err := trace.OpenWorkgroupAuthority(f.s.current().Root, id)
		must(t, err)
		defer wt.Close()
		got, doc, found, err := trace.ReadGroupApproval(wt, host)
		must(t, err)
		if !found || !equalJSONTest(got, approval) || !equalJSONTest(doc, pins) {
			t.Fatal("retirement changed completed approval")
		}
		if pending {
			retained, err := readChanges(wt, host)
			must(t, err)
			if !equalJSONTest(retained, changes) {
				t.Fatal("retirement changed the pending decision or its exact pins")
			}
			if !equalJSONTest(journals, readJournals(wt)) {
				t.Fatal("retirement changed the recorded journal bytes or revision history")
			}
		}
		groups, _, err := trace.Workgroups(wt)
		must(t, err)
		// Startup can inspect a fully retired group before publishing member
		// runtimes. Exact archive proof needs only the retained authority.
		starting := &Service{cfg: f.s.current(), store: f.s.store}
		retired, err := starting.workgroupFullyRetired(ctx, groups[0])
		must(t, err)
		if !retired {
			t.Fatal("retirement proof required a member runtime")
		}
		leader, err := f.s.liveWorkgroupLeader(groups[0])
		must(t, err)
		if leader != "" {
			t.Fatal("retired group has a live leader")
		}
		resolved, revision, err := trace.ResolvedWorkgroups(wt)
		must(t, err)
		if !slices.Equal(resolved, []config.WorkgroupID{id}) || revision != 1 {
			t.Fatalf("resolution changed: %v %d", resolved, revision)
		}
		active, err := trace.ProjectSpecActive(wt, id)
		must(t, err)
		if active {
			t.Fatal("fully resolved spec remained active")
		}
		for _, m := range resolutionMembers(f) {
			gone, err := trace.WorkstreamTraceGone(f.s.current().Root, m.project, m.stream)
			must(t, err)
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
	// Only a pending decision is held; a settled group passes quietly.
	if held := strings.Contains(logged.String(), "workgroup changes remain held"); held != pending {
		t.Fatalf("held warning logged=%v with pending decision=%v", held, pending)
	}
	if !f.s.ready.Load() || f.s.lifetime.Err() != nil {
		t.Fatal("fully retired group stopped service recovery")
	}
}
