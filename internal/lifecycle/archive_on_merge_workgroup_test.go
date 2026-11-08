package lifecycle_test

import (
	"context"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kpenfound/osmia/internal/api"
	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/pulls"
	"github.com/kpenfound/osmia/internal/runtime"
	"github.com/kpenfound/osmia/internal/trace"
)

// branchLookups counts the pull request lookups made for each branch.
type branchLookups struct {
	*fakePulls
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
	return p.fakePulls.Find(ctx, repository, head, branch)
}

func (p *branchLookups) lookups(branch string) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.counts[branch]
}

// autoArchiveSteps counts how often the archiver reaches each step and lets a
// test hold it at one of them until the service stops or the hold is lifted.
type autoArchiveSteps struct {
	mu      sync.Mutex
	counts  map[string]int
	holding string
	reached chan struct{}
	cleanup *lifecycleCleanupGate
}

func newAutoArchiveSteps(cleanup *lifecycleCleanupGate) *autoArchiveSteps {
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
		return s.cleanup.checkpoint(ctx, step)
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
	case <-time.After(demoTimeout):
		t.Fatalf("the archiver never reached %s", phase)
	}
}

// mergedPullRequest is the host's answer once publication has merged.
func mergedPullRequest(p DeliveryPublication, commit string) pulls.PullRequest {
	return pulls.PullRequest{Number: p.PullRequest, State: "closed", Merged: true, MergeCommit: commit, Head: p.Branch, HeadRepository: p.Fork}
}

// archiveEntries returns the entries of a conversation announcing an
// automatic archive of member.
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

// autoArchiveGroup is an approved two-member workgroup, each member delivered
// with its own pull request, whose pull request host is a branchLookups.
type autoArchiveGroup struct {
	*productionDesignFixture
	tb     *testing.T
	host   *branchLookups
	steps  *autoArchiveSteps
	gate   *lifecycleCleanupGate
	pubs   []DeliveryPublication
	member config.WorkstreamID
}

func newAutoArchiveGroup(t *testing.T) *autoArchiveGroup {
	t.Helper()
	g := &autoArchiveGroup{tb: t, host: &branchLookups{fakePulls: &fakePulls{}}}
	g.gate = &lifecycleCleanupGate{reached: make(chan struct{}), released: make(chan struct{})}
	t.Cleanup(g.gate.release)
	g.steps = newAutoArchiveSteps(g.gate)
	g.productionDesignFixture = resolutionApprovedFixture(t, productionDesignConfiguration{wait: lifecycleAwait, checkpoint: g.steps.checkpoint}, trace.EdgeHardBlock, g.host)
	t.Cleanup(func() { g.stop(t) })
	g.pubs = deliverResolutionMembers(t, g.productionDesignFixture)
	g.member = g.stream1
	return g
}

func (g *autoArchiveGroup) merge(t *testing.T) {
	t.Helper()
	g.host.Mu.Lock()
	g.host.Prs = []pulls.PullRequest{mergedPullRequest(g.pubs[0], "upstream-merge")}
	g.host.Mu.Unlock()
}

func (g *autoArchiveGroup) archive() (runtime.Archive, bool) {
	return archiveRecord(g.tb, g.s.store, g.member)
}

func TestAutoArchiveMergedWorkgroupMemberThroughBackgroundPasses(t *testing.T) {
	ctx := context.Background()
	g := newAutoArchiveGroup(t)
	group, err := g.c.Status(ctx, g.member)
	must(t, err)
	if group.Workgroup == nil {
		t.Fatal("the member has no workgroup")
	}
	// The other member merged already, so the merged member's archive is what
	// resolves the workgroup, as an owner archive of it would.
	second, err := g.s.repository(g.project2)
	must(t, err)
	recordResolutionMerge(t, second, g.stream2)
	g.merge(t)
	lifecycleAwait(t, g.productionDesignFixture, "the chief of staff archives the merged member", func() bool { _, ok := g.archive(); return ok })
	g.gate.await(t)
	a, _ := g.archive()
	if a.Actor != runtime.ArchiveActorChief || a.State != DeliveredState || a.DeliveryLink != g.pubs[0].URL || a.ResolutionEvidence != "merged" || !a.Resolved ||
		a.Publication.PullRequest != g.pubs[0].PullRequest || a.Publication.Branch != g.pubs[0].Branch || a.MergeCommit != "upstream-merge" || a.MergeReference != g.pubs[0].Upstream+"#101" || a.Workgroup != group.Workgroup.ID {
		t.Fatalf("automatic archive=%+v", a)
	}
	first, err := g.s.repository(g.project)
	must(t, err)
	docs, err := trace.Read[trace.Document](first, g.member)
	must(t, err)
	evidence, err := trace.MemberResolutionFrom(docs)
	must(t, err)
	if !evidence.OwnPullRequestMerged() {
		t.Fatalf("the own pull request outcome was not recorded before the archive: %+v", evidence)
	}
	lookups := g.host.lookups(g.pubs[0].Branch)

	view, err := g.c.Status(ctx, g.member)
	must(t, err)
	if !view.Archived || view.ArchivedBy != api.ArchivedByChief {
		t.Fatalf("archived row=%+v", view)
	}
	list, err := g.c.Statuses(ctx)
	must(t, err)
	for _, row := range list.Workstreams {
		if want := row.Workstream == g.member; row.Archived != want || want && row.ArchivedBy != api.ArchivedByChief || !want && row.ArchivedBy != "" {
			t.Fatalf("pane row=%+v", row)
		}
	}
	var feed api.WorkgroupFeedResponse
	must(t, g.c.Do(ctx, "GET", api.Prefix+"/workgroups/"+string(group.Workgroup.ID)+"/feed", nil, &feed))
	for _, m := range feed.Members {
		if want := m.Workstream == g.member; m.Archived != want || want && m.ArchivedBy != api.ArchivedByChief {
			t.Fatalf("member summary=%+v", m)
		}
	}

	// Before cleanup the member and the shared thread both read the entry once.
	for _, read := range []config.WorkstreamID{g.member, g.stream2, g.origin} {
		conversation, err := g.c.Conversation(ctx, read)
		must(t, err)
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

	awaitMemberMergeResolution(t, g.productionDesignFixture)
	g.gate.release()
	lifecycleAwait(t, g.productionDesignFixture, "the member's cleanup", func() bool { a, ok := g.archive(); return ok && !a.CleanedAt.IsZero() })
	authority, err := trace.OpenWorkgroupAuthority(g.s.current().Root, a.Workgroup)
	must(t, err)
	resolved, revision, err := trace.ResolvedWorkgroups(authority)
	must(t, err)
	active, err := trace.ProjectSpecActive(authority, a.Workgroup)
	must(t, err)
	authority.Close()
	if !slices.Equal(resolved, []config.WorkgroupID{a.Workgroup}) || revision != 1 || active {
		t.Fatalf("workgroup after the member's cleanup: %v revision=%d active=%v", resolved, revision, active)
	}
	down, err := g.s.repository(g.project2)
	must(t, err)
	observed, _, _, err := edgeObservationAt(down, g.stream2)
	must(t, err)
	if len(observed.Edges) != 1 || !observed.Edges[0].Merged || observed.Edges[0].Commit != "upstream-merge" {
		t.Fatalf("dependency edge=%+v", observed)
	}
	if _, archived := archiveRecord(t, g.s.store, g.stream2); archived {
		t.Fatal("the survivor was archived")
	}
	// The thread survives the member's cleanup and still holds the entry once.
	for _, read := range []config.WorkstreamID{g.stream2, g.origin} {
		conversation, err := g.c.Conversation(ctx, read)
		must(t, err)
		if entries := archiveEntries(conversation.Entries, g.member); len(entries) != 1 {
			t.Fatalf("conversation of %s after cleanup: %+v", read, conversation.Entries)
		}
	}
	// A merged archive makes no lookups of its own, nor does any later pass.
	g.f().stop(t)
	g.f().start(t)
	if again, _ := g.archive(); again.ArchivedAt != a.ArchivedAt || archiveCount(g.s.store, g.member) != 1 || g.host.lookups(g.pubs[0].Branch) != lookups {
		t.Fatalf("restart changed the archive or looked it up again: %+v lookups %d -> %d", again, lookups, g.host.lookups(g.pubs[0].Branch))
	}
}

func (g *autoArchiveGroup) f() *productionDesignFixture { return g.productionDesignFixture }

func TestAutoArchiveSurvivesAnInterruptionBeforeTheEntryIsVisible(t *testing.T) {
	ctx := context.Background()
	g := newAutoArchiveGroup(t)
	g.steps.hold("member-archive-recorded")
	g.merge(t)
	g.steps.await(t, "the committed archive")
	if a, ok := g.archive(); !ok || a.Actor != runtime.ArchiveActorChief {
		t.Fatalf("archive before the interruption=%+v ok=%v", a, ok)
	}
	g.f().stop(t)
	g.steps.lift()
	g.f().start(t)
	for range 2 {
		for _, read := range []config.WorkstreamID{g.stream2, g.origin} {
			conversation, err := g.c.Conversation(ctx, read)
			must(t, err)
			if entries := archiveEntries(conversation.Entries, g.member); len(entries) != 1 {
				t.Fatalf("conversation of %s after restart: %+v", read, conversation.Entries)
			}
		}
		g.gate.release()
		lifecycleAwait(t, g.f(), "later passes", func() bool { a, ok := g.archive(); return ok && !a.CleanedAt.IsZero() })
	}
	if archiveCount(g.s.store, g.member) != 1 {
		t.Fatal("the archive was duplicated")
	}
}

func TestAutoArchiveAfterARestartBetweenTheMergeAndTheArchive(t *testing.T) {
	g := newAutoArchiveGroup(t)
	g.steps.hold("auto-archive-selected")
	g.merge(t)
	g.steps.await(t, "selection")
	if _, ok := g.archive(); ok {
		t.Fatal("archived before the archiver committed")
	}
	first, err := g.s.repository(g.project)
	must(t, err)
	docs, err := trace.Read[trace.Document](first, g.member)
	must(t, err)
	if evidence, err := trace.MemberResolutionFrom(docs); err != nil || !evidence.OwnPullRequestMerged() {
		t.Fatalf("the merge was not recorded: %+v %v", evidence, err)
	}
	g.f().stop(t)
	g.steps.lift()
	g.host.Mu.Lock()
	g.host.Prs = nil
	g.host.Mu.Unlock()
	g.f().start(t)
	lifecycleAwait(t, g.f(), "the archive after restart", func() bool { _, ok := g.archive(); return ok })
	g.gate.release()
	lifecycleAwait(t, g.f(), "the member's cleanup", func() bool { a, ok := g.archive(); return ok && !a.CleanedAt.IsZero() })
	if a, _ := g.archive(); archiveCount(g.s.store, g.member) != 1 || a.Actor != runtime.ArchiveActorChief {
		t.Fatalf("archive=%+v", a)
	}
}

func TestAutoArchiveWritesNothingWhileAWorkgroupChangeHoldsTheArchive(t *testing.T) {
	ctx := context.Background()
	g := newAutoArchiveGroup(t)
	repo, err := g.s.repository(g.project)
	must(t, err)
	id, err := repo.CrossRepoWorkgroupOf(g.member)
	must(t, err)
	lock := g.s.changeApplicationLock(id)
	lock.Lock()
	var released atomic.Bool
	release := func() {
		if released.CompareAndSwap(false, true) {
			lock.Unlock()
		}
	}
	defer release()
	g.merge(t)
	lifecycleAwait(t, g.f(), "two refused attempts", func() bool { return g.steps.count("auto-archive-selected") >= 2 })
	if _, ok := g.archive(); ok {
		t.Fatal("archived while a workgroup change held the archive")
	}
	conversation, err := g.c.Conversation(ctx, g.stream2)
	must(t, err)
	if len(archiveEntries(conversation.Entries, g.member)) != 0 {
		t.Fatalf("announced a refused archive: %+v", conversation.Entries)
	}
	release()
	lifecycleAwait(t, g.f(), "the archive once the hold lifts", func() bool { a, ok := g.archive(); return ok && a.Actor == runtime.ArchiveActorChief })
	if archiveCount(g.s.store, g.member) != 1 {
		t.Fatal("the archive was duplicated")
	}
	g.gate.release()
	conversation, err = g.c.Conversation(ctx, g.stream2)
	must(t, err)
	if len(archiveEntries(conversation.Entries, g.member)) != 1 {
		t.Fatalf("conversation: %+v", conversation.Entries)
	}
}

// An owner archive that lands first is left as it is: the merged member is
// archived by the owner and no automatic-archive entry is announced.
func TestAutoArchiveLeavesAnOwnerArchiveAndAnnouncesNothing(t *testing.T) {
	ctx := context.Background()
	g := newAutoArchiveGroup(t)
	resp, err := g.c.Archive(ctx, g.member)
	must(t, err)
	if resp.ArchivedBy != api.ArchivedByOwner {
		t.Fatalf("owner archive reports %+v", resp)
	}
	owner, _ := g.archive()
	g.merge(t)
	g.gate.release()
	lifecycleAwait(t, g.f(), "the owner's archive observed and cleaned", func() bool { a, ok := g.archive(); return ok && !a.CleanedAt.IsZero() && a.Resolved })
	after, _ := g.archive()
	if after.Actor != "" || after.ArchivedAt != owner.ArchivedAt || archiveCount(g.s.store, g.member) != 1 {
		t.Fatalf("owner archive changed: %+v -> %+v", owner, after)
	}
	view, err := g.c.Status(ctx, g.member)
	must(t, err)
	if view.ArchivedBy != api.ArchivedByOwner {
		t.Fatalf("archived by %q", view.ArchivedBy)
	}
	conversation, err := g.c.Conversation(ctx, g.stream2)
	must(t, err)
	if entries := archiveEntries(conversation.Entries, g.member); len(entries) != 0 {
		t.Fatalf("announced an owner archive: %+v", entries)
	}
	if !slices.ContainsFunc(conversation.Entries, func(e api.ConversationEntry) bool { return true }) {
		t.Fatal("the shared conversation is empty")
	}
}
