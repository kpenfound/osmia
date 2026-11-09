package lifecycle_test

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kpenfound/osmia/internal/bundle"
	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/pulls"
	"github.com/kpenfound/osmia/internal/runtime"
	"github.com/kpenfound/osmia/internal/trace"
)

// resolutionPulls fails or delays only the sink member's own PR lookup. Upstream edge
// observation keeps its existing provider behavior.
type resolutionPulls struct {
	*fakePulls
	fail    atomic.Bool
	sink    atomic.Pointer[string]
	pending atomic.Pointer[resolutionLookup]
}

type resolutionLookup struct {
	reached chan struct{}
	release chan struct{}
}

func (p *resolutionPulls) Find(ctx context.Context, repository, head, branch string) ([]pulls.PullRequest, error) {
	sink := p.sink.Load()
	if p.fail.Load() && sink != nil && *sink == branch {
		p.Mu.Lock()
		p.Finds++
		p.Mu.Unlock()
		return nil, errors.New("provider temporarily unavailable")
	}
	found, err := p.fakePulls.Find(ctx, repository, head, branch)
	if sink != nil && *sink == branch {
		if pending := p.pending.Swap(nil); pending != nil {
			close(pending.reached)
			select {
			case <-pending.release:
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
	}
	return found, err
}
func resolutionApprovedFixture(t *testing.T, configuration productionDesignConfiguration, edge string, host pulls.Client, prepare ...func(*productionDesignFixture)) *productionDesignFixture {
	t.Helper()
	if host == nil {
		host = &fakePulls{}
	}
	options := configuration.options
	configuration.options = func(opts *Options) {
		if options != nil {
			options(opts)
		}
		opts.PullRequests = host
	}
	f := newConfiguredProductionDesignFixture(t, configuration)
	for _, setup := range prepare {
		setup(f)
	}
	f.sharedDesign().ScriptResolution(t, edge, ProjectSpecDraftTool)
	f.startOwner(t)
	packet := f.awaitPacket(t)
	_, err := f.c.Ratify(context.Background(), f.origin, packet.Packet.Revision.Spec, packet.Packet.Revision.Plan, packet.ProjectSpecRevision)
	must(t, err)
	f.awaitApproved(t)
	return f
}
func resolutionMembers(f *productionDesignFixture) []struct {
	project config.ProjectID
	stream  config.WorkstreamID
} {
	return []struct {
		project config.ProjectID
		stream  config.WorkstreamID
	}{{f.project, f.stream1}, {f.project2, f.stream2}}
}
func deliverResolutionMembers(t *testing.T, f *productionDesignFixture) []DeliveryPublication {
	t.Helper()
	return f.sharedDesign().DeliverResolutionMembers(t, publicationDocument, publicationPath)
}

// resolutionGroupID reads the workgroup through the design's origin workstream,
// whose trace stays in place when archive cleanup removes the members' traces.
func resolutionGroupID(t *testing.T, f *productionDesignFixture) config.WorkgroupID {
	t.Helper()
	repo, err := f.s.repository(f.project)
	must(t, err)
	id, err := repo.CrossRepoWorkgroupOf(f.origin)
	must(t, err)
	if id == "" {
		t.Fatal("the origin workstream has no workgroup")
	}
	return id
}

func resolutionGroupState(t *testing.T, f *productionDesignFixture, wantResolved bool) {
	t.Helper()
	ctx := context.Background()
	id := resolutionGroupID(t, f)
	authority, err := trace.OpenWorkgroupAuthority(f.s.current().Root, id)
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
	for _, m := range resolutionMembers(f) {
		if _, archived := f.s.archivedWorkstream(string(m.stream)); archived {
			continue
		}
		// Both member roles and the one shared chief keep the project contract.
		for _, role := range []string{masonRole, trace.ChiefOfStaff} {
			b, err := f.s.Context().Assemble(ctx, m.project, bundle.Scope{Role: role, Workstream: m.stream})
			must(t, err)
			if (b.ProjectSpec != nil) == wantResolved {
				t.Fatalf("member %s role %s project spec presence=%v", m.stream, role, b.ProjectSpec != nil)
			}
		}
		r, err := f.s.repository(m.project)
		must(t, err)
		docs, err := trace.Read[trace.Document](r, m.stream)
		must(t, err)
		if len(docs) == 0 {
			t.Fatal("member history disappeared")
		}
	}
}

// awaitGroupResolutionRevision waits until the group's resolution is recorded.
// It reads only the shared authority, so it is safe while the chief of staff
// archives and cleans the members.
func awaitGroupResolutionRevision(t *testing.T, f *productionDesignFixture) {
	t.Helper()
	lifecycleAwait(t, f, "group merge resolution", func() bool {
		authority, err := trace.OpenWorkgroupAuthority(f.s.current().Root, resolutionGroupID(t, f))
		must(t, err)
		defer authority.Close()
		_, revision, err := trace.ResolvedWorkgroups(authority)
		must(t, err)
		return revision == 1
	})
}
func awaitMemberMergeResolution(t *testing.T, f *productionDesignFixture) {
	t.Helper()
	awaitGroupResolutionRevision(t, f)
	resolutionGroupState(t, f, true)
}
func TestDesignProductionProjectSpecWaitsForEveryMemberMerge(t *testing.T) {
	f := resolutionApprovedFixture(t, productionDesignConfiguration{wait: lifecycleAwait}, "contract", nil)
	defer f.stop(t)
	deliverResolutionMembers(t, f)
	resolutionGroupState(t, f, false)
	repo, err := f.s.repository(f.project)
	must(t, err)
	recordResolutionMerge(t, repo, f.stream1)
	resolutionGroupState(t, f, false)
	f.stop(t)
	f.start(t)
	resolutionGroupState(t, f, false)
	repo, err = f.s.repository(f.project2)
	must(t, err)
	recordResolutionMerge(t, repo, f.stream2)
	awaitMemberMergeResolution(t, f)
	f.stop(t)
	f.start(t)
	resolutionGroupState(t, f, true)
}
func TestDesignProductionObservesAllOwnPullRequestsAcrossRestart(t *testing.T) {
	for _, edge := range []string{trace.EdgeHardBlock, trace.EdgeContract, ""} {
		t.Run("edge-"+edge, func(t *testing.T) {
			host := &resolutionPulls{fakePulls: &fakePulls{}}
			host.fail.Store(true)
			var hold atomic.Bool
			// Once both merges are observed, the chief of staff's archive and the
			// group's resolution both wait, so the restart lands between the
			// durable observations and everything they cause.
			f := resolutionApprovedFixture(t, productionDesignConfiguration{wait: lifecycleAwait, checkpoint: func(ctx context.Context, step string) error {
				if (step == "workgroup-resolution-ready" || step == "auto-archive-selected") && hold.Load() {
					<-ctx.Done()
					return ctx.Err()
				}
				return nil
			}}, edge, host)
			defer func() { hold.Store(false); f.stop(t) }()
			sink := featureBranch(f.stream2)
			host.sink.Store(&sink)
			publications := deliverResolutionMembers(t, f)
			awaitChecks := func(phase string, minimum int) {
				lifecycleAwait(t, f, phase, func() bool {
					for _, m := range resolutionMembers(f) {
						repo, err := f.s.repository(m.project)
						must(t, err)
						transitions, err := trace.Read[trace.Transition](repo, m.stream)
						must(t, err)
						n := 0
						for _, tr := range transitions {
							if tr.Subject == baseRefreshAction && tr.To == "checked" {
								n++
							}
						}
						if n < minimum {
							return false
						}
					}
					return true
				})
			}
			assertNoOutcomes := func() {
				for _, m := range resolutionMembers(f) {
					repo, err := f.s.repository(m.project)
					must(t, err)
					_, rev, err := ownPullRequestOutcomeAt(repo, m.stream)
					must(t, err)
					if rev != 0 {
						t.Fatal("open or failed lookup fabricated an outcome")
					}
				}
				resolutionGroupState(t, f, false)
			}
			awaitChecks("failed provider lookups", 1)
			assertNoOutcomes()
			host.Mu.Lock()
			host.fail.Store(false)
			for _, p := range publications {
				host.Prs = append(host.Prs, pulls.PullRequest{Number: p.PullRequest, State: "open", Head: p.Branch, HeadRepository: p.Fork})
			}
			host.Mu.Unlock()
			f.clock.Advance(10 * time.Minute)
			awaitChecks("ready open provider lookups", 2)
			assertNoOutcomes()
			// Keep an open response in flight across the merge and clock advance.
			pending := &resolutionLookup{reached: make(chan struct{}), release: make(chan struct{})}
			host.pending.Store(pending)
			lifecycleAwait(t, f, "open provider response in flight", func() bool {
				select {
				case <-pending.reached:
					return true
				default:
					return false
				}
			})
			// Hold the archive and final resolution after every durable observation, then perform a real restart.
			hold.Store(true)
			host.Mu.Lock()
			for i := range host.Prs {
				host.Prs[i].Merged = true
				host.Prs[i].State = "closed"
				host.Prs[i].MergeCommit = fmt.Sprintf("merged-%d", i)
			}
			host.Mu.Unlock()
			f.clock.Advance(10 * time.Minute)
			close(pending.release)
			// Poll completions can schedule another cadence while the host changes.
			// Advance through those deadlines until both outcomes are durable.
			lifecycleAwait(t, f, "durable outcomes before resolution", func() bool {
				for _, m := range resolutionMembers(f) {
					repo, err := f.s.repository(m.project)
					must(t, err)
					if _, rev, err := ownPullRequestOutcomeAt(repo, m.stream); err != nil || rev == 0 {
						return false
					}
				}
				return true
			})
			for i, m := range resolutionMembers(f) {
				repo, err := f.s.repository(m.project)
				must(t, err)
				outcome, rev, err := ownPullRequestOutcomeAt(repo, m.stream)
				must(t, err)
				if rev != 1 || outcome.PullRequest != publications[i].PullRequest || outcome.Outcome != "merged" || outcome.MergeCommit != fmt.Sprintf("merged-%d", i) {
					t.Fatalf("observed evidence=%+v revision=%d", outcome, rev)
				}
			}
			resolutionGroupState(t, f, false)
			for _, m := range resolutionMembers(f) {
				if _, archived := archiveRecord(t, f.s.store, m.stream); archived {
					t.Fatalf("member %s archived before the restart", m.stream)
				}
			}
			f.stop(t)
			hold.Store(false)
			host.Mu.Lock()
			calls := host.Finds
			host.Mu.Unlock()
			f.start(t)
			// The chief of staff's archives and the group's resolution run together
			// once the hold is released, in either order.
			awaitGroupResolutionRevision(t, f)
			host.Mu.Lock()
			after := host.Finds
			host.Mu.Unlock()
			if after != calls {
				t.Fatalf("finished own PRs polled after restart: %d -> %d", calls, after)
			}
			// The chief of staff archives each merged member, and the archive keeps the
			// observed merge evidence after cleanup removes the member's trace.
			lifecycleAwait(t, f, "chief of staff archives of the merged members", func() bool {
				for _, m := range resolutionMembers(f) {
					if a, ok := archiveRecord(t, f.s.store, m.stream); !ok || a.CleanedAt.IsZero() {
						return false
					}
				}
				return true
			})
			resolutionGroupState(t, f, true)
			for i, m := range resolutionMembers(f) {
				a, _ := archiveRecord(t, f.s.store, m.stream)
				if a.Actor != runtime.ArchiveActorChief || !archiveResolved(a) || a.ResolutionEvidence != "merged" || a.Publication.PullRequest != publications[i].PullRequest || a.MergeCommit != fmt.Sprintf("merged-%d", i) {
					t.Fatalf("archive of %s=%+v", m.stream, a)
				}
			}
			f.stop(t)
			f.start(t)
			resolutionGroupState(t, f, true)
		})
	}
}
