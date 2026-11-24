package lifecycle_test

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kpenfound/osmia/internal/servicetest/journey"

	"github.com/kpenfound/osmia/internal/api"
	"github.com/kpenfound/osmia/internal/bundle"
	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/delivery"
	"github.com/kpenfound/osmia/internal/intake"
	"github.com/kpenfound/osmia/internal/lifecycle"
	"github.com/kpenfound/osmia/internal/pulls"
	"github.com/kpenfound/osmia/internal/runtime"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	"github.com/kpenfound/osmia/internal/servicetest/intakefixture"
	"github.com/kpenfound/osmia/internal/trace"
)

type resolutionPulls struct {
	*fixturekit.FakePulls
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
	found, err := p.FakePulls.Find(ctx, repository, head, branch)
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
func resolutionApprovedFixture(t *testing.T, configuration journey.ProductionDesignConfiguration, edge string, host pulls.Client, prepare ...func(*journey.ProductionDesignFixture)) *journey.ProductionDesignFixture {
	t.Helper()
	if host == nil {
		host = &fixturekit.FakePulls{}
	}
	options := configuration.Options
	configuration.Options = func(opts *journey.Options) {
		if options != nil {
			options(opts)
		}
		opts.PullRequests = host
	}
	f := journey.NewConfiguredProductionDesignFixture(t, configuration)
	for _, setup := range prepare {
		setup(f)
	}
	f.SharedDesign().ScriptResolution(t, edge, lifecycle.ProjectSpecDraftTool)
	f.StartOwner(t)
	packet := f.AwaitPacket(t)
	_, err := f.C.Ratify(context.Background(), f.Origin, packet.Packet.Revision.Spec, packet.Packet.Revision.Plan, packet.ProjectSpecRevision)
	fixturekit.Must(t, err)
	f.AwaitApproved(t)
	return f
}
func resolutionMembers(f *journey.ProductionDesignFixture) []struct {
	project config.ProjectID
	stream  config.WorkstreamID
} {
	return []struct {
		project config.ProjectID
		stream  config.WorkstreamID
	}{{f.Project, f.Stream1}, {f.Project2, f.Stream2}}
}
func deliverResolutionMembers(t *testing.T, f *journey.ProductionDesignFixture) []api.DeliveryPublication {
	t.Helper()
	return f.SharedDesign().DeliverResolutionMembers(t, delivery.PublicationDocument, delivery.PublicationPath)
}

func resolutionGroupID(t *testing.T, f *journey.ProductionDesignFixture) config.WorkgroupID {
	t.Helper()
	repo, err := f.S.Repository(f.Project)
	fixturekit.Must(t, err)
	id, err := repo.CrossRepoWorkgroupOf(f.Origin)
	fixturekit.Must(t, err)
	if id == "" {
		t.Fatal("the origin workstream has no workgroup")
	}
	return id
}

func resolutionGroupState(t *testing.T, f *journey.ProductionDesignFixture, wantResolved bool) {
	t.Helper()
	ctx := context.Background()
	id := resolutionGroupID(t, f)
	authority, err := trace.OpenWorkgroupAuthority(f.S.Current().Root, id)
	fixturekit.Must(t, err)
	defer authority.Close()
	ids, revision, err := trace.ResolvedWorkgroups(authority)
	fixturekit.Must(t, err)
	active, err := trace.ProjectSpecActive(authority, id)
	fixturekit.Must(t, err)
	if wantResolved {
		if !slices.Equal(ids, []config.WorkgroupID{id}) || revision != 1 || active {
			t.Fatalf("resolution=%v revision=%d active=%v", ids, revision, active)
		}
	} else if len(ids) != 0 || revision != 0 || !active {
		t.Fatalf("premature resolution=%v revision=%d active=%v", ids, revision, active)
	}
	for _, m := range resolutionMembers(f) {
		if _, archived := f.S.Lifecycle().ArchivedWorkstream(string(m.stream)); archived {
			continue
		}
		for _, role := range []string{intake.MasonRole, trace.ChiefOfStaff} {
			b, err := f.S.Context().Assemble(ctx, m.project, bundle.Scope{Role: role, Workstream: m.stream})
			fixturekit.Must(t, err)
			if (b.ProjectSpec != nil) == wantResolved {
				t.Fatalf("member %s role %s project spec presence=%v", m.stream, role, b.ProjectSpec != nil)
			}
		}
		r, err := f.S.Repository(m.project)
		fixturekit.Must(t, err)
		docs, err := trace.Read[trace.Document](r, m.stream)
		fixturekit.Must(t, err)
		if len(docs) == 0 {
			t.Fatal("member history disappeared")
		}
	}
}

func awaitGroupResolutionRevision(t *testing.T, f *journey.ProductionDesignFixture) {
	t.Helper()
	journey.LifecycleAwait(t, f, "group merge resolution", func() bool {
		authority, err := trace.OpenWorkgroupAuthority(f.S.Current().Root, resolutionGroupID(t, f))
		fixturekit.Must(t, err)
		defer authority.Close()
		_, revision, err := trace.ResolvedWorkgroups(authority)
		fixturekit.Must(t, err)
		return revision == 1
	})
}
func awaitMemberMergeResolution(t *testing.T, f *journey.ProductionDesignFixture) {
	t.Helper()
	awaitGroupResolutionRevision(t, f)
	resolutionGroupState(t, f, true)
}
func TestDesignProductionProjectSpecWaitsForEveryMemberMerge(t *testing.T) {
	t.Parallel()
	f := resolutionApprovedFixture(t, journey.ProductionDesignConfiguration{Wait: journey.LifecycleAwait}, "contract", nil)
	defer f.Stop(t)
	deliverResolutionMembers(t, f)
	resolutionGroupState(t, f, false)
	repo, err := f.S.Repository(f.Project)
	fixturekit.Must(t, err)
	journey.RecordResolutionMerge(t, repo, f.Stream1)
	resolutionGroupState(t, f, false)
	f.Stop(t)
	f.Start(t)
	resolutionGroupState(t, f, false)
	repo, err = f.S.Repository(f.Project2)
	fixturekit.Must(t, err)
	journey.RecordResolutionMerge(t, repo, f.Stream2)
	awaitMemberMergeResolution(t, f)
	f.Stop(t)
	f.Start(t)
	resolutionGroupState(t, f, true)
}
func TestDesignProductionObservesAllOwnPullRequestsAcrossRestart(t *testing.T) {
	t.Parallel()
	for _, edge := range []string{trace.EdgeHardBlock, trace.EdgeContract, ""} {
		t.Run("edge-"+edge, func(t *testing.T) {
			host := &resolutionPulls{FakePulls: &fixturekit.FakePulls{}}
			host.fail.Store(true)
			var hold atomic.Bool
			f := resolutionApprovedFixture(t, journey.ProductionDesignConfiguration{Wait: journey.LifecycleAwait, Checkpoint: func(ctx context.Context, step string) error {
				if (step == "workgroup-resolution-ready" || step == "auto-archive-selected") && hold.Load() {
					<-ctx.Done()
					return ctx.Err()
				}
				return nil
			}}, edge, host)
			defer func() { hold.Store(false); f.Stop(t) }()
			sink := intake.FeatureBranch(f.Stream2)
			host.sink.Store(&sink)
			publications := deliverResolutionMembers(t, f)
			awaitChecks := func(phase string, minimum int) {
				journey.LifecycleAwait(t, f, phase, func() bool {
					for _, m := range resolutionMembers(f) {
						repo, err := f.S.Repository(m.project)
						fixturekit.Must(t, err)
						transitions, err := trace.Read[trace.Transition](repo, m.stream)
						fixturekit.Must(t, err)
						n := 0
						for _, tr := range transitions {
							if tr.Subject == lifecycle.BaseRefreshAction && tr.To == "checked" {
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
					repo, err := f.S.Repository(m.project)
					fixturekit.Must(t, err)
					_, rev, err := delivery.OwnPullRequestOutcomeAt(repo, m.stream)
					fixturekit.Must(t, err)
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
			f.Clock.Advance(10 * time.Minute)
			awaitChecks("ready open provider lookups", 2)
			assertNoOutcomes()
			pending := &resolutionLookup{reached: make(chan struct{}), release: make(chan struct{})}
			host.pending.Store(pending)
			journey.LifecycleAwait(t, f, "open provider response in flight", func() bool {
				select {
				case <-pending.reached:
					return true
				default:
					return false
				}
			})
			hold.Store(true)
			host.Mu.Lock()
			for i := range host.Prs {
				host.Prs[i].Merged = true
				host.Prs[i].State = "closed"
				host.Prs[i].MergeCommit = fmt.Sprintf("merged-%d", i)
			}
			host.Mu.Unlock()
			f.Clock.Advance(10 * time.Minute)
			close(pending.release)
			journey.LifecycleAwait(t, f, "durable outcomes before resolution", func() bool {
				for _, m := range resolutionMembers(f) {
					repo, err := f.S.Repository(m.project)
					fixturekit.Must(t, err)
					if _, rev, err := delivery.OwnPullRequestOutcomeAt(repo, m.stream); err != nil || rev == 0 {
						return false
					}
				}
				return true
			})
			for i, m := range resolutionMembers(f) {
				repo, err := f.S.Repository(m.project)
				fixturekit.Must(t, err)
				outcome, rev, err := delivery.OwnPullRequestOutcomeAt(repo, m.stream)
				fixturekit.Must(t, err)
				if rev != 1 || outcome.PullRequest != publications[i].PullRequest || outcome.Outcome != "merged" || outcome.MergeCommit != fmt.Sprintf("merged-%d", i) {
					t.Fatalf("observed evidence=%+v revision=%d", outcome, rev)
				}
			}
			resolutionGroupState(t, f, false)
			for _, m := range resolutionMembers(f) {
				if _, archived := intakefixture.ArchiveRecord(t, f.S.Store, m.stream); archived {
					t.Fatalf("member %s archived before the restart", m.stream)
				}
			}
			f.Stop(t)
			hold.Store(false)
			host.Mu.Lock()
			calls := host.Finds
			host.Mu.Unlock()
			f.Start(t)
			awaitGroupResolutionRevision(t, f)
			host.Mu.Lock()
			after := host.Finds
			host.Mu.Unlock()
			if after != calls {
				t.Fatalf("finished own PRs polled after restart: %d -> %d", calls, after)
			}
			journey.LifecycleAwait(t, f, "chief of staff archives of the merged members", func() bool {
				for _, m := range resolutionMembers(f) {
					if a, ok := intakefixture.ArchiveRecord(t, f.S.Store, m.stream); !ok || a.CleanedAt.IsZero() {
						return false
					}
				}
				return true
			})
			resolutionGroupState(t, f, true)
			for i, m := range resolutionMembers(f) {
				a, _ := intakefixture.ArchiveRecord(t, f.S.Store, m.stream)
				if a.Actor != runtime.ArchiveActorChief || !lifecycle.ArchiveResolved(a) || a.ResolutionEvidence != "merged" || a.Publication.PullRequest != publications[i].PullRequest || a.MergeCommit != fmt.Sprintf("merged-%d", i) {
					t.Fatalf("archive of %s=%+v", m.stream, a)
				}
			}
			f.Stop(t)
			f.Start(t)
			resolutionGroupState(t, f, true)
		})
	}
}
