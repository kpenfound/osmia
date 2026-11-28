package lifecycle_test

import (
	"context"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kpenfound/osmia/internal/servicetest/journey"

	"github.com/kpenfound/osmia/internal/api"
	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/intake"
	"github.com/kpenfound/osmia/internal/lifecycle"
	"github.com/kpenfound/osmia/internal/pulls"
	"github.com/kpenfound/osmia/internal/runtime"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	"github.com/kpenfound/osmia/internal/servicetest/intakefixture"
	"github.com/kpenfound/osmia/internal/trace"
)

type branchLookups struct {
	*fixturekit.FakePulls
	mu     sync.Mutex
	counts map[string]int
}

func (p *branchLookups) Find(ctx context.Context, repository, head, branch string) ([]pulls.PullRequest, error) {
	p.mu.Lock()
	if p.counts == nil {
		p.counts = map[string]int{}
	}
	p.counts[branch]++
	p.mu.Unlock()
	return p.FakePulls.Find(ctx, repository, head, branch)
}

func (p *branchLookups) lookups(branch string) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.counts[branch]
}

type autoArchiveSteps struct {
	mu      sync.Mutex
	counts  map[string]int
	holding string
	reached chan struct{}
	cleanup *journey.LifecycleCleanupGate
}

func newAutoArchiveSteps(cleanup *journey.LifecycleCleanupGate) *autoArchiveSteps {
	return &autoArchiveSteps{counts: map[string]int{}, cleanup: cleanup}
}

func (s *autoArchiveSteps) hold(step string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.holding, s.reached = step, make(chan struct{})
}

func (s *autoArchiveSteps) lift() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.holding = ""
}

func (s *autoArchiveSteps) count(step string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.counts[step]
}

func (s *autoArchiveSteps) checkpoint(ctx context.Context, step string) error {
	s.mu.Lock()
	s.counts[step]++
	hold, reached := s.holding == step, s.reached
	s.mu.Unlock()
	if hold {
		select {
		case <-reached:
		default:
			close(reached)
		}
		for {
			s.mu.Lock()
			held := s.holding == step
			s.mu.Unlock()
			if !held {
				break
			}
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(10 * time.Millisecond):
			}
		}
	}
	if s.cleanup != nil {
		return s.cleanup.Checkpoint(ctx, step)
	}
	return nil
}

func (s *autoArchiveSteps) await(t *testing.T, phase string) {
	t.Helper()
	s.mu.Lock()
	reached := s.reached
	s.mu.Unlock()
	select {
	case <-reached:
	case <-time.After(fixturekit.DemoTimeout):
		t.Fatalf("the archiver never reached %s", phase)
	}
}

func mergedPullRequest(p api.DeliveryPublication, commit string) pulls.PullRequest {
	return pulls.PullRequest{Number: p.PullRequest, State: "closed", Merged: true, MergeCommit: commit, Head: p.Branch, HeadRepository: p.Fork}
}

func archiveEntries(entries []api.ConversationEntry, member config.WorkstreamID) []api.ConversationEntry {
	var out []api.ConversationEntry
	for _, e := range entries {
		if e.Kind == "action" && strings.Contains(e.Text, "Archived workstream "+string(member)) {
			out = append(out, e)
		}
	}
	return out
}

func archiveCount(store *runtime.Store, stream config.WorkstreamID) int {
	state, _ := store.Snapshot()
	n := 0
	for _, a := range state.Archived {
		if a.Workstream == stream {
			n++
		}
	}
	return n
}

type autoArchiveGroup struct {
	*journey.ProductionDesignFixture
	tb     *testing.T
	host   *branchLookups
	steps  *autoArchiveSteps
	gate   *journey.LifecycleCleanupGate
	pubs   []api.DeliveryPublication
	member config.WorkstreamID
}

func newAutoArchiveGroup(t *testing.T) *autoArchiveGroup {
	t.Helper()
	g := &autoArchiveGroup{tb: t, host: &branchLookups{FakePulls: &fixturekit.FakePulls{}}}
	g.gate = &journey.LifecycleCleanupGate{Reached: make(chan struct{})}
	t.Cleanup(g.gate.Release)
	g.steps = newAutoArchiveSteps(g.gate)
	g.ProductionDesignFixture = resolutionApprovedFixture(t, journey.ProductionDesignConfiguration{Wait: journey.LifecycleAwait, Checkpoint: g.steps.checkpoint}, trace.EdgeHardBlock, g.host)
	t.Cleanup(func() { g.Stop(t) })
	g.pubs = deliverResolutionMembers(t, g.ProductionDesignFixture)
	g.member = g.Stream1
	return g
}

func (g *autoArchiveGroup) merge(t *testing.T) {
	t.Helper()
	g.host.Mu.Lock()
	g.host.Prs = []pulls.PullRequest{mergedPullRequest(g.pubs[0], "upstream-merge")}
	g.host.Mu.Unlock()
}

func (g *autoArchiveGroup) archive() (runtime.Archive, bool) {
	return intakefixture.ArchiveRecord(g.tb, g.S.Store, g.member)
}

func TestAutoArchiveMergedWorkgroupMemberThroughBackgroundPasses(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	g := newAutoArchiveGroup(t)
	group, err := g.C.Status(ctx, g.member)
	fixturekit.Must(t, err)
	if group.Workgroup == nil {
		t.Fatal("the member has no workgroup")
	}
	second, err := g.S.Repository(g.Project2)
	fixturekit.Must(t, err)
	journey.RecordResolutionMerge(t, second, g.Stream2)
	g.merge(t)
	journey.LifecycleAwait(t, g.ProductionDesignFixture, "the chief of staff archives the merged member", func() bool { _, ok := g.archive(); return ok })
	g.gate.Await(t)
	a, _ := g.archive()
	if a.Actor != runtime.ArchiveActorManager || a.State != intake.DeliveredState || a.DeliveryLink != g.pubs[0].URL || a.ResolutionEvidence != "merged" || !a.Resolved ||
		a.Publication.PullRequest != g.pubs[0].PullRequest || a.Publication.Branch != g.pubs[0].Branch || a.MergeCommit != "upstream-merge" || a.MergeReference != g.pubs[0].Upstream+"#101" || a.Workgroup != group.Workgroup.ID {
		t.Fatalf("automatic archive=%+v", a)
	}
	first, err := g.S.Repository(g.Project)
	fixturekit.Must(t, err)
	docs, err := trace.Read[trace.Document](first, g.member)
	fixturekit.Must(t, err)
	evidence, err := trace.MemberResolutionFrom(docs)
	fixturekit.Must(t, err)
	if !evidence.OwnPullRequestMerged() {
		t.Fatalf("the own pull request outcome was not recorded before the archive: %+v", evidence)
	}
	lookups := g.host.lookups(g.pubs[0].Branch)

	view, err := g.C.Status(ctx, g.member)
	fixturekit.Must(t, err)
	if !view.Archived || view.ArchivedBy != api.ArchivedByManager {
		t.Fatalf("archived row=%+v", view)
	}
	list, err := g.C.Statuses(ctx)
	fixturekit.Must(t, err)
	for _, row := range list.Workstreams {
		if want := row.Workstream == g.member; row.Archived != want || want && row.ArchivedBy != api.ArchivedByManager || !want && row.ArchivedBy != "" {
			t.Fatalf("pane row=%+v", row)
		}
	}
	var feed api.WorkgroupFeedResponse
	fixturekit.Must(t, g.C.Do(ctx, "GET", api.Prefix+"/workgroups/"+string(group.Workgroup.ID)+"/feed", nil, &feed))
	for _, m := range feed.Members {
		if want := m.Workstream == g.member; m.Archived != want || want && m.ArchivedBy != api.ArchivedByManager {
			t.Fatalf("member summary=%+v", m)
		}
	}

	for _, read := range []config.WorkstreamID{g.member, g.Stream2, g.Origin} {
		conversation, err := g.C.Conversation(ctx, read)
		fixturekit.Must(t, err)
		entries := archiveEntries(conversation.Entries, g.member)
		if len(entries) != 1 || !strings.Contains(entries[0].Text, g.pubs[0].URL) || !strings.Contains(entries[0].Text, "because its pull request merged") {
			t.Fatalf("conversation of %s: %+v", read, conversation.Entries)
		}
	}
	shared := 0
	for _, e := range feed.Entries {
		if e.Kind == "action" && strings.Contains(e.Text, "Archived workstream "+string(g.member)) {
			shared++
			if e.Workstream != g.member {
				t.Fatalf("shared entry names %q", e.Workstream)
			}
		}
	}
	if shared != 1 {
		t.Fatalf("shared workgroup feed holds %d entries: %+v", shared, feed.Entries)
	}

	awaitMemberMergeResolution(t, g.ProductionDesignFixture)
	g.gate.Release()
	journey.LifecycleAwait(t, g.ProductionDesignFixture, "the member's cleanup", func() bool { a, ok := g.archive(); return ok && !a.CleanedAt.IsZero() })
	authority, err := trace.OpenWorkgroupAuthority(g.S.Current().Root, a.Workgroup)
	fixturekit.Must(t, err)
	resolved, revision, err := trace.ResolvedWorkgroups(authority)
	fixturekit.Must(t, err)
	active, err := trace.ProjectSpecActive(authority, a.Workgroup)
	fixturekit.Must(t, err)
	authority.Close()
	if !slices.Equal(resolved, []config.WorkgroupID{a.Workgroup}) || revision != 1 || active {
		t.Fatalf("workgroup after the member's cleanup: %v revision=%d active=%v", resolved, revision, active)
	}
	down, err := g.S.Repository(g.Project2)
	fixturekit.Must(t, err)
	var observed lifecycle.EdgeObservation
	journey.LifecycleAwait(t, g.ProductionDesignFixture, "the dependent's edge observed merged", func() bool {
		var err error
		observed, _, _, err = lifecycle.EdgeObservationAt(down, g.Stream2)
		fixturekit.Must(t, err)
		return len(observed.Edges) == 1 && observed.Edges[0].Merged
	})
	if observed.Edges[0].Commit != "upstream-merge" {
		t.Fatalf("dependency edge=%+v", observed)
	}
	if _, archived := intakefixture.ArchiveRecord(t, g.S.Store, g.Stream2); archived {
		t.Fatal("the survivor was archived")
	}
	for _, read := range []config.WorkstreamID{g.Stream2, g.Origin} {
		conversation, err := g.C.Conversation(ctx, read)
		fixturekit.Must(t, err)
		if entries := archiveEntries(conversation.Entries, g.member); len(entries) != 1 {
			t.Fatalf("conversation of %s after cleanup: %+v", read, conversation.Entries)
		}
	}
	g.f().Stop(t)
	g.f().Start(t)
	if again, _ := g.archive(); again.ArchivedAt != a.ArchivedAt || archiveCount(g.S.Store, g.member) != 1 || g.host.lookups(g.pubs[0].Branch) != lookups {
		t.Fatalf("restart changed the archive or looked it up again: %+v lookups %d -> %d", again, lookups, g.host.lookups(g.pubs[0].Branch))
	}
}

func (g *autoArchiveGroup) f() *journey.ProductionDesignFixture { return g.ProductionDesignFixture }

func TestAutoArchiveSurvivesAnInterruptionBeforeTheEntryIsVisible(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	g := newAutoArchiveGroup(t)
	g.steps.hold("member-archive-recorded")
	g.merge(t)
	g.steps.await(t, "the committed archive")
	if a, ok := g.archive(); !ok || a.Actor != runtime.ArchiveActorManager {
		t.Fatalf("archive before the interruption=%+v ok=%v", a, ok)
	}
	g.f().Stop(t)
	g.steps.lift()
	g.f().Start(t)
	for range 2 {
		for _, read := range []config.WorkstreamID{g.Stream2, g.Origin} {
			conversation, err := g.C.Conversation(ctx, read)
			fixturekit.Must(t, err)
			if entries := archiveEntries(conversation.Entries, g.member); len(entries) != 1 {
				t.Fatalf("conversation of %s after restart: %+v", read, conversation.Entries)
			}
		}
		g.gate.Release()
		journey.LifecycleAwait(t, g.f(), "later passes", func() bool { a, ok := g.archive(); return ok && !a.CleanedAt.IsZero() })
	}
	if archiveCount(g.S.Store, g.member) != 1 {
		t.Fatal("the archive was duplicated")
	}
}

func TestAutoArchiveAfterARestartBetweenTheMergeAndTheArchive(t *testing.T) {
	t.Parallel()
	g := newAutoArchiveGroup(t)
	g.steps.hold("auto-archive-selected")
	g.merge(t)
	g.steps.await(t, "selection")
	if _, ok := g.archive(); ok {
		t.Fatal("archived before the archiver committed")
	}
	first, err := g.S.Repository(g.Project)
	fixturekit.Must(t, err)
	docs, err := trace.Read[trace.Document](first, g.member)
	fixturekit.Must(t, err)
	if evidence, err := trace.MemberResolutionFrom(docs); err != nil || !evidence.OwnPullRequestMerged() {
		t.Fatalf("the merge was not recorded: %+v %v", evidence, err)
	}
	g.f().Stop(t)
	g.steps.lift()
	g.host.Mu.Lock()
	g.host.Prs = nil
	g.host.Mu.Unlock()
	g.f().Start(t)
	journey.LifecycleAwait(t, g.f(), "the archive after restart", func() bool { _, ok := g.archive(); return ok })
	g.gate.Release()
	journey.LifecycleAwait(t, g.f(), "the member's cleanup", func() bool { a, ok := g.archive(); return ok && !a.CleanedAt.IsZero() })
	if a, _ := g.archive(); archiveCount(g.S.Store, g.member) != 1 || a.Actor != runtime.ArchiveActorManager {
		t.Fatalf("archive=%+v", a)
	}
}

func TestAutoArchiveWritesNothingWhileAWorkgroupChangeHoldsTheArchive(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	g := newAutoArchiveGroup(t)
	repo, err := g.S.Repository(g.Project)
	fixturekit.Must(t, err)
	id, err := repo.CrossRepoWorkgroupOf(g.member)
	fixturekit.Must(t, err)
	lock := g.S.Lifecycle().ChangeApplicationLock(id)
	lock.Lock()
	var released atomic.Bool
	release := func() {
		if released.CompareAndSwap(false, true) {
			lock.Unlock()
		}
	}
	defer release()
	g.merge(t)
	journey.LifecycleAwait(t, g.f(), "two refused attempts", func() bool { return g.steps.count("auto-archive-selected") >= 2 })
	if _, ok := g.archive(); ok {
		t.Fatal("archived while a workgroup change held the archive")
	}
	conversation, err := g.C.Conversation(ctx, g.Stream2)
	fixturekit.Must(t, err)
	if len(archiveEntries(conversation.Entries, g.member)) != 0 {
		t.Fatalf("announced a refused archive: %+v", conversation.Entries)
	}
	release()
	journey.LifecycleAwait(t, g.f(), "the archive once the hold lifts", func() bool { a, ok := g.archive(); return ok && a.Actor == runtime.ArchiveActorManager })
	if archiveCount(g.S.Store, g.member) != 1 {
		t.Fatal("the archive was duplicated")
	}
	g.gate.Release()
	conversation, err = g.C.Conversation(ctx, g.Stream2)
	fixturekit.Must(t, err)
	if len(archiveEntries(conversation.Entries, g.member)) != 1 {
		t.Fatalf("conversation: %+v", conversation.Entries)
	}
}

func TestAutoArchiveLeavesAnOwnerArchiveAndAnnouncesNothing(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	g := newAutoArchiveGroup(t)
	resp, err := g.C.Archive(ctx, g.member)
	fixturekit.Must(t, err)
	if resp.ArchivedBy != api.ArchivedByOwner {
		t.Fatalf("owner archive reports %+v", resp)
	}
	owner, _ := g.archive()
	g.merge(t)
	g.gate.Release()
	journey.LifecycleAwait(t, g.f(), "the owner's archive observed and cleaned", func() bool { a, ok := g.archive(); return ok && !a.CleanedAt.IsZero() && a.Resolved })
	after, _ := g.archive()
	if after.Actor != "" || after.ArchivedAt != owner.ArchivedAt || archiveCount(g.S.Store, g.member) != 1 {
		t.Fatalf("owner archive changed: %+v -> %+v", owner, after)
	}
	view, err := g.C.Status(ctx, g.member)
	fixturekit.Must(t, err)
	if view.ArchivedBy != api.ArchivedByOwner {
		t.Fatalf("archived by %q", view.ArchivedBy)
	}
	conversation, err := g.C.Conversation(ctx, g.Stream2)
	fixturekit.Must(t, err)
	if entries := archiveEntries(conversation.Entries, g.member); len(entries) != 0 {
		t.Fatalf("announced an owner archive: %+v", entries)
	}
	if !slices.ContainsFunc(conversation.Entries, func(e api.ConversationEntry) bool { return true }) {
		t.Fatal("the shared conversation is empty")
	}
}
