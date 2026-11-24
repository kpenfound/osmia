package lifecycle_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kpenfound/osmia/internal/execution"
	"github.com/kpenfound/osmia/internal/servicetest/journey"

	"github.com/kpenfound/osmia/internal/api"
	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/delivery"
	"github.com/kpenfound/osmia/internal/eventstream"
	"github.com/kpenfound/osmia/internal/intake"
	"github.com/kpenfound/osmia/internal/lifecycle"
	"github.com/kpenfound/osmia/internal/projectstore"
	"github.com/kpenfound/osmia/internal/pulls"
	"github.com/kpenfound/osmia/internal/runtime"
	"github.com/kpenfound/osmia/internal/service"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	"github.com/kpenfound/osmia/internal/servicetest/intakefixture"
	"github.com/kpenfound/osmia/internal/trace"
)

func TestBaseRefreshPullRequestPollInterval(t *testing.T) {
	t.Parallel()
	for _, delivered := range []bool{false, true} {
		for _, interval := range []time.Duration{5 * time.Minute, 10 * time.Minute, 20 * time.Minute} {
			t.Run(fmt.Sprintf("delivered-%v/%s", delivered, interval), func(t *testing.T) {
				cfg, repo, host, refresh, _ := stopFixture(t)
				clock := &fixturekit.FixedClock{At: fixturekit.DemoStart}
				refresh.S.Options.Reconciliation.Now = clock.Now
				cfg.Project.PullRequestPoll = interval.String()
				cfg.Projects[0] = cfg.Project
				journey.SealStream(t, repo)
				if delivered {
					p := journey.DeliverWithPullRequest(t, repo, cfg, 42)
					host.Prs = []pulls.PullRequest{{Number: 42, State: "open", Head: p.Branch, HeadRepository: p.Fork}}
				}
				ctx := context.Background()
				fixturekit.Must(t, refresh.Pass(ctx))
				op := latestBaseRefreshOperation(t, repo, fixturekit.Stream)
				_, err := refresh.Apply(ctx, op)
				fixturekit.Must(t, err)
				calls := host.Finds
				for _, advance := range []time.Duration{time.Minute, interval - time.Minute - time.Nanosecond} {
					clock.Advance(advance)
					fixturekit.Must(t, refresh.Pass(ctx))
					if n := fixturekit.OperationCount(t, repo, fixturekit.Stream, lifecycle.BaseRefreshAction); n != 1 || host.Finds != calls {
						t.Fatalf("early refresh: operations=%d finds=%d, want 1/%d", n, host.Finds, calls)
					}
				}
				clock.Advance(time.Nanosecond)
				fixturekit.Must(t, refresh.Pass(ctx))
				fixturekit.Must(t, refresh.Pass(ctx))
				if n := fixturekit.OperationCount(t, repo, fixturekit.Stream, lifecycle.BaseRefreshAction); n != 2 {
					t.Fatalf("due operations=%d", n)
				}
				next := latestBaseRefreshOperation(t, repo, fixturekit.Stream)
				_, err = refresh.Apply(ctx, next)
				fixturekit.Must(t, err)
				wantCalls := calls
				if delivered {
					wantCalls++
				}
				if host.Finds != wantCalls {
					t.Fatalf("finds=%d want %d", host.Finds, wantCalls)
				}
			})
		}
	}
}
func TestEdgePullRequestPollAttempts(t *testing.T) {
	t.Parallel()
	for _, kind := range []string{trace.EdgeHardBlock, trace.EdgeMergeOrder} {
		for _, previous := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/previous-%v", kind, previous), func(t *testing.T) {
				f := newEdgeGraphFixture(t, kind)
				clock := &fixturekit.FixedClock{At: f.s.Now()}
				f.s.Options.Reconciliation.Now = clock.Now
				p := api.DeliveryPublication{Status: delivery.PublicationOpened, Upstream: "owner/repo", Fork: "owner/fork", Branch: intake.FeatureBranch(f.upstream), PullRequest: 42}
				journey.RecordUpstreamOwnPullRequest(t, f.s, f.repository, f.upstream, p)
				host := &fixturekit.FakePulls{Prs: []pulls.PullRequest{{Number: 42, State: "open", Head: p.Branch, HeadRepository: p.Fork}}}
				f.s.Options.PullRequests = host
				ctx := context.Background()
				if previous {
					fixturekit.Must(t, f.s.Lifecycle().EdgeResolverPass(ctx, f.repository))
					clock.Advance(10*time.Minute - time.Nanosecond)
					fixturekit.Must(t, f.s.Lifecycle().EdgeResolverPass(ctx, f.repository))
					if host.Finds != 1 {
						t.Fatal("open edge polled before interval")
					}
					clock.Advance(time.Nanosecond)
				}
				before, rev, _, err := lifecycle.EdgeObservationAt(f.repository, f.downstream)
				fixturekit.Must(t, err)
				failure := errors.New("upstream unavailable")
				host.FindErr = failure
				err = f.s.Lifecycle().EdgeResolverPass(ctx, f.repository)
				if !errors.Is(err, failure) {
					t.Fatalf("provider error changed: %v", err)
				}
				calls := host.Finds
				after, afterRev, _, err := lifecycle.EdgeObservationAt(f.repository, f.downstream)
				fixturekit.Must(t, err)
				if rev != afterRev || !reflect.DeepEqual(before, after) {
					t.Fatal("failed attempt wrote merge evidence")
				}
				fixturekit.Must(t, f.s.Lifecycle().EdgeWaitPass(ctx, f.repository))
				if kind == trace.EdgeHardBlock {
					waiting, err := lifecycle.EdgeWaitDisplay(f.repository, f.downstream)
					fixturekit.Must(t, err)
					if waiting == "" {
						t.Fatal("failed lookup released dependent")
					}
				} else {
					_, waiting, err := lifecycle.UnresolvedMergeOrderEdge(f.repository, f.downstream)
					fixturekit.Must(t, err)
					if !waiting {
						t.Fatal("failed lookup released merge order")
					}
				}
				for _, advance := range []time.Duration{0, time.Minute, 9*time.Minute - time.Nanosecond} {
					clock.Advance(advance)
					fixturekit.Must(t, f.s.Lifecycle().EdgeResolverPass(ctx, f.repository))
					if host.Finds != calls {
						t.Fatal("failed attempt retried before interval")
					}
				}
				clock.Advance(time.Nanosecond)
				if err := f.s.Lifecycle().EdgeResolverPass(ctx, f.repository); !errors.Is(err, failure) {
					t.Fatalf("retry error: %v", err)
				}
				if host.Finds != calls+1 {
					t.Fatalf("retry finds=%d want %d", host.Finds, calls+1)
				}
				_, afterRev, _, err = lifecycle.EdgeObservationAt(f.repository, f.downstream)
				fixturekit.Must(t, err)
				if afterRev != rev {
					t.Fatal("retry failure recorded evidence")
				}
			})
		}
	}
}

func pollHooks(t *testing.T, s *journey.Service, repo *trace.Repository) {
	t.Helper()
	p := s.Sole().Pipeline
	if next := p.Next.Swap(nil); next != nil {
		p.Current.Store(next)
	}
	for _, hook := range p.Current.Load().Polls {
		if hook.Name == "base-refresh" || hook.Name == "edge-refresh" {
			fixturekit.Must(t, hook.Pass(context.Background()))
		}
	}
}

func setPullRequestPoll(t *testing.T, s *journey.Service, cfg *config.Config, interval string) {
	t.Helper()
	id := cfg.Project.ID
	first, err := projectstore.Open(cfg.Root).Current(id)
	fixturekit.Must(t, err)
	saved, apiFault := service.InspectForTest(s.Core()).SaveProjectConfig(context.Background(), id, api.ProjectConfigEdit{Pin: first.Pin, Set: api.ProjectConfigValues{PullRequestPoll: &interval}})
	if apiFault != nil {
		t.Fatal(apiFault)
	}
	if saved.Outcome != api.ProjectConfigApplied {
		t.Fatalf("save: %+v", saved)
	}
}

func TestPullRequestPollReloadRetainsAttempts(t *testing.T) {
	t.Parallel()
	for _, interval := range []time.Duration{10 * time.Minute, 20 * time.Minute, 5 * time.Minute} {
		t.Run(interval.String(), func(t *testing.T) {
			ctx := context.Background()
			cfg, repo, host, refresh := journey.OwnPRFixture(t)
			clock := &fixturekit.FixedClock{At: fixturekit.DemoStart}
			s := refresh.S
			s.Options.Config = cfg.Root.Options("")
			s.Options.Reconciliation.Now = clock.Now
			s.Hub = eventstream.New()
			_, err := trace.CreateWorkgroup(ctx, repo, []config.WorkstreamID{fixturekit.Stream, fixturekit.Quiet}, fixturekit.DemoStart, intake.OwnerActor, "fixture")
			fixturekit.Must(t, err)
			p := journey.DeliverWithPullRequest(t, repo, cfg, 42)
			archive := runtime.Archive{Project: cfg.Project.ID, Workstream: fixturekit.Quiet, State: intake.DeliveredState, ArchivedAt: fixturekit.DemoStart, ResolutionEvidence: "unresolved", Publication: runtime.ArchivePublication{Upstream: p.Upstream, Fork: p.Fork, Branch: intake.FeatureBranch(fixturekit.Quiet), Commit: "archive-head", PullRequest: 43}}
			store, _, err := runtime.Open(runtime.Inputs{Config: cfg, Workstreams: map[config.ProjectID][]config.WorkstreamID{cfg.Project.ID: {fixturekit.Stream, fixturekit.Quiet}}})
			fixturekit.Must(t, err)
			defer store.Close()
			s.Store = store
			fixturekit.Must(t, store.SetArchived(archive))
			host.FindErr = errors.New("provider unavailable")
			stages, err := journey.InspectForTest(s).Stages(cfg, repo)
			fixturekit.Must(t, err)
			pipeline := service.NewPipelineForTest()
			pipeline.Current.Store(stages)
			journey.InspectForTest(s).SetPipeline(s.Cfg.Project.ID, pipeline)
			pollHooks(t, s, repo)
			_, err = refresh.Apply(ctx, latestBaseRefreshOperation(t, repo, fixturekit.Stream))
			fixturekit.Must(t, err)
			if host.Finds != 2 {
				t.Fatalf("initial lookups=%d", host.Finds)
			}
			setPullRequestPoll(t, s, cfg, interval.String())
			for i := 0; i < 2; i++ {
				_, apiFault := journey.InspectForTest(s).Reload()
				if apiFault != nil {
					t.Fatal(apiFault)
				}
				pollHooks(t, s, repo)
				if host.Finds != 2 || fixturekit.OperationCount(t, repo, fixturekit.Stream, lifecycle.BaseRefreshAction) != 1 {
					t.Fatal("reload restarted cooldown")
				}
			}
			clock.Advance(interval - time.Nanosecond)
			pollHooks(t, s, repo)
			if host.Finds != 2 || fixturekit.OperationCount(t, repo, fixturekit.Stream, lifecycle.BaseRefreshAction) != 1 {
				t.Fatal("poll before reloaded interval")
			}
			clock.Advance(time.Nanosecond)
			pollHooks(t, s, repo)
			_, err = refresh.Apply(ctx, latestBaseRefreshOperation(t, repo, fixturekit.Stream))
			fixturekit.Must(t, err)
			if host.Finds != 4 || fixturekit.OperationCount(t, repo, fixturekit.Stream, lifecycle.BaseRefreshAction) != 2 {
				t.Fatalf("due lookups=%d operations=%d", host.Finds, fixturekit.OperationCount(t, repo, fixturekit.Stream, lifecycle.BaseRefreshAction))
			}
		})
	}
}
func TestOwnerRebaseMakesBaseAndEdgesDueOnce(t *testing.T) {
	t.Parallel()
	for _, projectRequest := range []bool{false, true} {
		t.Run(fmt.Sprintf("project-%v", projectRequest), func(t *testing.T) {
			f := newEdgeGraphFixture(t, trace.EdgeHardBlock)
			f.s.SetSole(journey.RuntimeFor(f.repository))
			streams, err := f.repository.Workstreams()
			fixturekit.Must(t, err)
			store, _, err := runtime.Open(runtime.Inputs{Config: f.s.Current(), Workstreams: map[config.ProjectID][]config.WorkstreamID{f.project: streams}})
			fixturekit.Must(t, err)
			defer store.Close()
			f.s.Store = store
			clock := &fixturekit.FixedClock{At: f.s.Now()}
			f.s.Options.Reconciliation.Now = clock.Now
			host := &fixturekit.FakePulls{}
			f.s.Options.PullRequests = host
			for i, member := range []config.WorkstreamID{f.upstream, f.downstream} {
				p := api.DeliveryPublication{Status: delivery.PublicationOpened, Upstream: "owner/repo", Fork: "owner/fork", Branch: intake.FeatureBranch(member), PullRequest: 42 + i}
				journey.RecordUpstreamOwnPullRequest(t, f.s, f.repository, member, p)
				host.Prs = append(host.Prs, pulls.PullRequest{Number: p.PullRequest, State: "open", Head: p.Branch, HeadRepository: p.Fork})
			}
			refresh := &journey.BaseRefresher{S: f.s, Repository: f.repository}
			ctx := context.Background()
			pass := func() {
				fixturekit.Must(t, refresh.Pass(ctx))
				fixturekit.Must(t, f.s.Lifecycle().EdgeResolverPass(ctx, f.repository))
				for _, member := range []config.WorkstreamID{f.upstream, f.downstream} {
					_, err := refresh.Apply(ctx, latestBaseRefreshOperation(t, f.repository, member))
					fixturekit.Must(t, err)
				}
			}
			pass()
			before := host.Finds
			if projectRequest {
				out, apiFault := f.s.Lifecycle().RebaseProject(ctx, api.ProjectRebaseRequest{Project: f.project})
				if apiFault != nil || len(out.Covered) != 2 {
					t.Fatalf("project rebase: %+v %v", out, apiFault)
				}
			} else if _, apiFault := f.s.Lifecycle().RebaseWorkstream(ctx, string(f.downstream)); apiFault != nil {
				t.Fatal(apiFault)
			}
			pass()
			want := before + 2
			if projectRequest {
				want++
			}
			if host.Finds != want || fixturekit.OperationCount(t, f.repository, f.downstream, lifecycle.BaseRefreshAction) != 2 {
				t.Fatalf("request did not check own PR and edge: %d -> %d", before, host.Finds)
			}
			pass()
			if host.Finds != want || fixturekit.OperationCount(t, f.repository, f.downstream, lifecycle.BaseRefreshAction) != 2 {
				t.Fatal("request bypassed interval more than once")
			}
		})
	}
}
func TestServiceDefaultPullRequestPollingAndRebaseAPI(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	opts, clone := journey.ProjectFixture(t)
	host := &fixturekit.FakePulls{}
	clock := &fixturekit.FixedClock{At: fixturekit.DemoStart}
	ticks := make(chan time.Time)
	var passed atomic.Int64
	opts.PullRequests = host
	opts.Reconciliation.Now = clock.Now
	opts.Reconciliation.Ticks = ticks
	opts.SchedulePassed = func() { passed.Add(1) }
	s, c := journey.StartT(t, opts)
	added, err := c.AddProject(ctx, fixturekit.Request(clone))
	fixturekit.Must(t, err)
	repo := s.Sole().Repository
	member := config.WorkstreamID("w_00000000000000000000000000000892")
	fixturekit.Must(t, repo.CreateWorkstream(ctx, member, clock.Now(), intake.OwnerActor))
	_, err = trace.CreateWorkgroup(ctx, repo, []config.WorkstreamID{member}, clock.Now(), intake.OwnerActor, "fixture")
	fixturekit.Must(t, err)
	p := api.DeliveryPublication{Status: delivery.PublicationOpened, Upstream: "dagger/dagger", Fork: "owner/dagger", Branch: intake.FeatureBranch(member), PullRequest: 42}
	host.Mu.Lock()
	host.Prs = []pulls.PullRequest{{Number: 42, State: "open", Head: p.Branch, HeadRepository: p.Fork}}
	host.Mu.Unlock()
	journey.RecordUpstreamOwnPullRequest(t, s, repo, member, p)
	fixturekit.Must(t, s.RefreshRuntime())
	tick := func() {
		t.Helper()
		before := passed.Load()
		select {
		case ticks <- clock.Now():
		case <-time.After(fixturekit.DemoTimeout):
			t.Fatal("reconciler did not accept tick")
		}
		intakefixture.Eventually(t, "pass did not complete", func() bool { return passed.Load() > before })
	}
	waitChecked := func(want int) {
		t.Helper()
		deadline := time.Now().Add(fixturekit.DemoTimeout)
		for {
			state, err := repo.Workflow(member, lifecycle.BaseRefreshAction)
			fixturekit.Must(t, err)
			if state.Value == "checked" && fixturekit.OperationCount(t, repo, member, lifecycle.BaseRefreshAction) == want {
				return
			}
			if time.Now().After(deadline) {
				t.Fatalf("refresh never checked: %+v", state)
			}
			tick()
		}
	}
	waitChecked(1)
	for _, advance := range []time.Duration{time.Minute, 4 * time.Minute, 5*time.Minute - time.Nanosecond} {
		clock.Advance(advance)
		tick()
		host.Mu.Lock()
		calls := host.Finds
		host.Mu.Unlock()
		if calls != 1 || fixturekit.OperationCount(t, repo, member, lifecycle.BaseRefreshAction) != 1 {
			t.Fatalf("default polled early: finds=%d", calls)
		}
	}
	view, err := c.Configuration(ctx)
	fixturekit.Must(t, err)
	if view.Effective.For(added.Project.ID).Project.PullRequestPoll != "10m" {
		t.Fatal("configuration omits default interval")
	}
	_, err = c.RebaseWorkstream(ctx, member)
	fixturekit.Must(t, err)
	waitChecked(2)
	tick()
	host.Mu.Lock()
	calls := host.Finds
	host.Mu.Unlock()
	if calls != 2 {
		t.Fatalf("rebase lookup count=%d", calls)
	}
}
func TestEdgePullRequestPollReload(t *testing.T) {
	t.Parallel()
	for _, failed := range []bool{false, true} {
		t.Run(fmt.Sprintf("failed-%v", failed), func(t *testing.T) {
			ctx := context.Background()
			cfg, repo, host, refresh := journey.OwnPRFixture(t)
			s := refresh.S
			clock := &fixturekit.FixedClock{At: fixturekit.DemoStart}
			s.Options.Config = cfg.Root.Options("")
			s.Options.Reconciliation.Now = clock.Now
			s.Hub = eventstream.New()
			group, err := trace.CreateWorkgroup(ctx, repo, []config.WorkstreamID{fixturekit.Stream, fixturekit.Quiet}, fixturekit.DemoStart, intake.OwnerActor, "fixture")
			fixturekit.Must(t, err)
			fixturekit.Must(t, trace.RecordProjectSpec(ctx, repo, group, trace.ProjectSpec{Behavior: "Dependency waits for merge", Graph: []trace.Edge{{From: fixturekit.Quiet, To: fixturekit.Stream, Type: trace.EdgeHardBlock}}}, fixturekit.DemoStart, intake.OwnerActor, "fixture"))
			p := api.DeliveryPublication{Status: delivery.PublicationOpened, Upstream: cfg.Project.Upstream, Fork: cfg.Project.PushRepository(), Branch: intake.FeatureBranch(fixturekit.Quiet), PullRequest: 42}
			journey.RecordUpstreamOwnPullRequest(t, s, repo, fixturekit.Quiet, p)
			host.Prs = []pulls.PullRequest{{Number: 42, State: "open", Head: p.Branch, HeadRepository: p.Fork}}
			failure := errors.New("upstream unavailable")
			if failed {
				host.FindErr = failure
			}
			store, _, err := runtime.Open(runtime.Inputs{Config: cfg, Workstreams: map[config.ProjectID][]config.WorkstreamID{cfg.Project.ID: {fixturekit.Stream, fixturekit.Quiet}}})
			fixturekit.Must(t, err)
			defer store.Close()
			s.Store = store
			stage, err := journey.InspectForTest(s).Stages(cfg, repo)
			fixturekit.Must(t, err)
			pipeline := service.NewPipelineForTest()
			pipeline.Current.Store(stage)
			journey.InspectForTest(s).SetPipeline(s.Cfg.Project.ID, pipeline)
			pass := func(wantError bool) {
				t.Helper()
				if next := pipeline.Next.Swap(nil); next != nil {
					pipeline.Current.Store(next)
				}
				for _, hook := range pipeline.Current.Load().Polls {
					if hook.Name == "edge-refresh" {
						err := hook.Pass(ctx)
						if wantError {
							if !errors.Is(err, failure) {
								t.Fatalf("lookup error=%v", err)
							}
						} else {
							fixturekit.Must(t, err)
						}
					}
				}
			}
			pass(failed)
			if host.Finds != 1 {
				t.Fatalf("initial finds=%d", host.Finds)
			}
			setPullRequestPoll(t, s, cfg, "20m")
			for i := 0; i < 2; i++ {
				_, apiFault := journey.InspectForTest(s).Reload()
				if apiFault != nil {
					t.Fatal(apiFault)
				}
				pass(false)
				if host.Finds != 1 {
					t.Fatal("reload restarted edge cooldown")
				}
			}
			clock.Advance(20*time.Minute - time.Nanosecond)
			pass(false)
			if host.Finds != 1 {
				t.Fatal("edge polled before new interval")
			}
			clock.Advance(time.Nanosecond)
			pass(failed)
			if host.Finds != 2 {
				t.Fatalf("due finds=%d", host.Finds)
			}
		})
	}
}
func TestUndeliveredDependencyPullRequestPollInterval(t *testing.T) {
	t.Parallel()
	for _, projectRequest := range []bool{false, true} {
		t.Run(fmt.Sprintf("project-request-%v", projectRequest), func(t *testing.T) {
			f := journey.NewDebateFixture(t, 1, 1)
			f.Upstream(t)
			f.Stop(t)
			ctx := context.Background()
			repo, err := trace.Open(f.S.Cfg.Root, f.S.Cfg.Project)
			fixturekit.Must(t, err)
			defer repo.Close()
			base, baseTip := journey.SealDependent(t, f, repo, config.WorkspacesGit, "", map[string]string{"base.go": "base\n"})
			dependent, _ := journey.SealDependent(t, f, repo, config.WorkspacesGit, base, map[string]string{"dependent.go": "dependent\n"})
			cfg := f.S.About(repo)
			p := api.DeliveryPublication{Status: delivery.PublicationOpened, Upstream: cfg.Project.Upstream, Fork: cfg.Project.PushRepository(), Base: cfg.Project.BaseBranch, Branch: intake.FeatureBranch(base), Commit: baseTip, PullRequest: 42}
			journey.RecordUpstreamOwnPullRequest(t, f.S, repo, base, p)
			host := &fixturekit.FakePulls{Prs: []pulls.PullRequest{{Number: 42, State: "open", Head: p.Branch, HeadRepository: p.Fork}}}
			f.S.Options.PullRequests = host
			clock := &fixturekit.FixedClock{At: f.S.Now()}
			f.S.Options.Reconciliation.Now = clock.Now
			refresh := &journey.BaseRefresher{S: f.S, Repository: repo}
			fixturekit.Must(t, refresh.Pass(ctx))
			op := latestBaseRefreshOperation(t, repo, dependent)
			_, err = refresh.Apply(ctx, op)
			fixturekit.Must(t, err)
			if host.Finds != 1 {
				t.Fatalf("dependency lookup count=%d", host.Finds)
			}
			clock.Advance(10*time.Minute - time.Nanosecond)
			fixturekit.Must(t, refresh.Pass(ctx))
			if host.Finds != 1 || fixturekit.OperationCount(t, repo, dependent, lifecycle.BaseRefreshAction) != 1 {
				t.Fatal("dependency polled early")
			}
			clock.Advance(time.Nanosecond)
			fixturekit.Must(t, refresh.Pass(ctx))
			fixturekit.Must(t, refresh.Pass(ctx))
			if fixturekit.OperationCount(t, repo, dependent, lifecycle.BaseRefreshAction) != 2 {
				t.Fatal("due dependency refresh not scheduled once")
			}
			_, err = refresh.Apply(ctx, latestBaseRefreshOperation(t, repo, dependent))
			fixturekit.Must(t, err)
			if host.Finds != 2 {
				t.Fatalf("dependency retry count=%d", host.Finds)
			}

			f.S.SetSole(journey.RuntimeFor(repo))
			streams, err := repo.Workstreams()
			fixturekit.Must(t, err)
			store, _, err := runtime.Open(runtime.Inputs{Config: f.S.Current(), Workstreams: map[config.ProjectID][]config.WorkstreamID{f.Project: streams}})
			fixturekit.Must(t, err)
			defer store.Close()
			f.S.Store = store
			h := trace.Header{Schema: "osmia.trace.transition", Version: trace.Version, ID: "building", Revision: 1, Project: repo.Project(), Workstream: dependent, At: clock.Now(), Actor: execution.ForemanActor, Cause: "fixture"}
			_, err = repo.SetFeatureState(ctx, h, intake.BuildingState, "ready for drift")
			fixturekit.Must(t, err)
			if projectRequest {
				out, apiFault := f.S.Lifecycle().RebaseProject(ctx, api.ProjectRebaseRequest{Project: f.Project})
				if apiFault != nil || len(out.Covered) != 2 {
					t.Fatalf("project request: %+v %v", out, apiFault)
				}
			} else if _, apiFault := f.S.Lifecycle().RebaseWorkstream(ctx, string(dependent)); apiFault != nil {
				t.Fatal(apiFault)
			}
			fixturekit.Must(t, refresh.Pass(ctx))
			_, err = refresh.Apply(ctx, latestBaseRefreshOperation(t, repo, dependent))
			fixturekit.Must(t, err)
			if host.Finds != 3 || fixturekit.OperationCount(t, repo, dependent, lifecycle.BaseRefreshAction) != 3 {
				t.Fatal("drift request did not immediately recheck dependency")
			}
			fixturekit.Must(t, refresh.Pass(ctx))
			if host.Finds != 3 || fixturekit.OperationCount(t, repo, dependent, lifecycle.BaseRefreshAction) != 3 {
				t.Fatal("dependency bypass ran more than once")
			}
		})
	}
}

func latestBaseRefreshOperation(t *testing.T, repo *trace.Repository, stream config.WorkstreamID) coreadapter.Operation {
	t.Helper()
	ops, err := repo.Operations(stream)
	fixturekit.Must(t, err)
	var latest coreadapter.Operation
	check := 0
	for _, record := range ops {
		if record.Operation.Action != lifecycle.BaseRefreshAction {
			continue
		}
		var in lifecycle.BaseRefreshInput
		fixturekit.Must(t, json.Unmarshal(record.Operation.Input, &in))
		if in.Check > check {
			check, latest = in.Check, record.Operation
		}
	}
	if latest.ID == "" {
		t.Fatal("no base refresh operation")
	}
	return latest
}
