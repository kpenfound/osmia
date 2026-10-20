package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"reflect"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/pulls"
	"github.com/kpenfound/osmia/internal/runtime"
	"github.com/kpenfound/osmia/internal/trace"
)

// nextDue lets live-controller fixtures advance their clock to an in-memory
// polling deadline without changing the configured cadence or running a pass.
func (p *pullRequestAttempts) nextDue(project config.ProjectID, interval time.Duration, now time.Time) time.Time {
	p.mu.Lock()
	defer p.mu.Unlock()
	var next time.Time
	for key, attempt := range p.attempts {
		if key.project != project {
			continue
		}
		due := attempt.at.Add(interval)
		if due.After(now) && (next.IsZero() || due.Before(next)) {
			next = due
		}
	}
	return next
}

func TestBaseRefreshPullRequestPollInterval(t *testing.T) {
	for _, delivered := range []bool{false, true} {
		for _, interval := range []time.Duration{5 * time.Minute, 10 * time.Minute, 20 * time.Minute} {
			t.Run(fmt.Sprintf("delivered-%v/%s", delivered, interval), func(t *testing.T) {
				cfg, repo, host, refresh, _ := stopFixture(t)
				// Keep a fixed clock so both sides of the exact boundary are tested.
				clock := &fixedClock{now: demoStart}
				refresh.s.options.Reconciliation.Now = clock.Now
				cfg.Project.PullRequestPoll = interval.String()
				cfg.Projects[0] = cfg.Project
				sealStream(t, repo)
				if delivered {
					p := deliverWithPullRequest(t, repo, cfg, 42)
					host.prs = []pulls.PullRequest{{Number: 42, State: "open", Head: p.Branch, HeadRepository: p.Fork}}
				}
				ctx := context.Background()
				must(t, refresh.Pass(ctx))
				op := latestBaseRefreshOperation(t, repo, stream)
				_, err := refresh.Apply(ctx, op)
				must(t, err)
				calls := host.finds
				for _, advance := range []time.Duration{time.Minute, interval - time.Minute - time.Nanosecond} {
					clock.Advance(advance)
					must(t, refresh.Pass(ctx))
					if n := baseRefreshOperationCount(t, repo, stream); n != 1 || host.finds != calls {
						t.Fatalf("early refresh: operations=%d finds=%d, want 1/%d", n, host.finds, calls)
					}
				}
				clock.Advance(time.Nanosecond)
				must(t, refresh.Pass(ctx))
				must(t, refresh.Pass(ctx))
				if n := baseRefreshOperationCount(t, repo, stream); n != 2 {
					t.Fatalf("due operations=%d", n)
				}
				next := latestBaseRefreshOperation(t, repo, stream)
				_, err = refresh.Apply(ctx, next)
				must(t, err)
				wantCalls := calls
				if delivered {
					wantCalls++
				}
				if host.finds != wantCalls {
					t.Fatalf("finds=%d want %d", host.finds, wantCalls)
				}
			})
		}
	}
}

func TestEdgePullRequestPollAttempts(t *testing.T) {
	for _, kind := range []string{trace.EdgeHardBlock, trace.EdgeMergeOrder} {
		for _, previous := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/previous-%v", kind, previous), func(t *testing.T) {
				f := newEdgeGraphFixture(t, kind)
				clock := &fixedClock{now: f.s.now()}
				f.s.options.Reconciliation.Now = clock.Now
				p := DeliveryPublication{Status: publicationOpened, Upstream: "owner/repo", Fork: "owner/fork", Branch: featureBranch(f.upstream), PullRequest: 42}
				recordUpstreamOwnPullRequest(t, f.s, f.repository, f.upstream, p)
				host := &fakePulls{prs: []pulls.PullRequest{{Number: 42, State: "open", Head: p.Branch, HeadRepository: p.Fork}}}
				f.s.options.PullRequests = host
				ctx := context.Background()
				if previous {
					must(t, f.s.edgeResolverPass(ctx, f.repository))
					clock.Advance(10*time.Minute - time.Nanosecond)
					must(t, f.s.edgeResolverPass(ctx, f.repository))
					if host.finds != 1 {
						t.Fatal("open edge polled before interval")
					}
					clock.Advance(time.Nanosecond)
				}
				before, rev, _, err := edgeObservationAt(f.repository, f.downstream)
				must(t, err)
				failure := errors.New("upstream unavailable")
				host.findErr = failure
				err = f.s.edgeResolverPass(ctx, f.repository)
				if !errors.Is(err, failure) {
					t.Fatalf("provider error changed: %v", err)
				}
				calls := host.finds
				after, afterRev, _, err := edgeObservationAt(f.repository, f.downstream)
				must(t, err)
				if rev != afterRev || !reflect.DeepEqual(before, after) {
					t.Fatal("failed attempt wrote merge evidence")
				}
				must(t, f.s.edgeWaitPass(ctx, f.repository))
				if kind == trace.EdgeHardBlock {
					waiting, err := edgeWaitDisplay(f.repository, f.downstream)
					must(t, err)
					if waiting == "" {
						t.Fatal("failed lookup released dependent")
					}
				} else {
					_, waiting, err := unresolvedMergeOrderEdge(f.repository, f.downstream)
					must(t, err)
					if !waiting {
						t.Fatal("failed lookup released merge order")
					}
				}
				for _, advance := range []time.Duration{0, time.Minute, 9*time.Minute - time.Nanosecond} {
					clock.Advance(advance)
					must(t, f.s.edgeResolverPass(ctx, f.repository))
					if host.finds != calls {
						t.Fatal("failed attempt retried before interval")
					}
				}
				clock.Advance(time.Nanosecond)
				if err := f.s.edgeResolverPass(ctx, f.repository); !errors.Is(err, failure) {
					t.Fatalf("retry error: %v", err)
				}
				if host.finds != calls+1 {
					t.Fatalf("retry finds=%d want %d", host.finds, calls+1)
				}
				_, afterRev, _, err = edgeObservationAt(f.repository, f.downstream)
				must(t, err)
				if afterRev != rev {
					t.Fatal("retry failure recorded evidence")
				}
			})
		}
	}
}

// Run the loaded pipeline's actual polling hooks while leaving operation
// application under test control. Reload must replace these hook closures.
func pollHooks(t *testing.T, s *Service, repo *trace.Repository) {
	t.Helper()
	p := s.sole().pipeline
	if next := p.next.Swap(nil); next != nil {
		p.current.Store(next)
	}
	for _, hook := range p.current.Load().polls {
		if hook.name == "base-refresh" || hook.name == "edge-refresh" {
			must(t, hook.pass(context.Background()))
		}
	}
}

func TestPullRequestPollReloadRetainsAttempts(t *testing.T) {
	for _, interval := range []time.Duration{10 * time.Minute, 20 * time.Minute, 5 * time.Minute} {
		t.Run(interval.String(), func(t *testing.T) {
			ctx := context.Background()
			cfg, repo, host, refresh := ownPRFixture(t)
			clock := &fixedClock{now: demoStart}
			s := refresh.s
			s.options.Config = cfg.Root.Options("")
			s.options.Reconciliation.Now = clock.Now
			s.hub = newHub()
			_, err := trace.CreateWorkgroup(ctx, repo, []config.WorkstreamID{stream, quiet}, demoStart, ownerActor, "fixture")
			must(t, err)
			p := deliverWithPullRequest(t, repo, cfg, 42)
			// quiet is archived without touching stream's publication.
			archive := runtime.Archive{Project: cfg.Project.ID, Workstream: quiet, State: DeliveredState, ArchivedAt: demoStart, ResolutionEvidence: "unresolved", Publication: runtime.ArchivePublication{Upstream: p.Upstream, Fork: p.Fork, Branch: featureBranch(quiet), Commit: "archive-head", PullRequest: 43}}
			store, _, err := runtime.Open(runtime.Inputs{Config: cfg, Workstreams: map[config.ProjectID][]config.WorkstreamID{cfg.Project.ID: {stream, quiet}}})
			must(t, err)
			defer store.Close()
			s.store = store
			must(t, store.SetArchived(archive))
			// Failed archived reads must survive reload too.
			host.findErr = errors.New("provider unavailable")
			stages, err := s.stages(cfg, repo)
			must(t, err)
			pipeline := &pipeline{}
			pipeline.current.Store(stages)
			s.sole().pipeline = pipeline
			pollHooks(t, s, repo)
			_, err = refresh.Apply(ctx, latestBaseRefreshOperation(t, repo, stream))
			must(t, err)
			if host.finds != 2 {
				t.Fatalf("initial lookups=%d", host.finds)
			}
			path, err := cfg.Root.ProjectConfig(cfg.Project.ID)
			must(t, err)
			data, err := os.ReadFile(path)
			must(t, err)
			must(t, os.WriteFile(path, append([]byte("pull_request_poll = \""+interval.String()+"\"\n"), data...), 0600))
			for i := 0; i < 2; i++ {
				_, api := s.reload()
				if api != nil {
					t.Fatal(api)
				}
				pollHooks(t, s, repo)
				if host.finds != 2 || baseRefreshOperationCount(t, repo, stream) != 1 {
					t.Fatal("reload restarted cooldown")
				}
			}
			clock.Advance(interval - time.Nanosecond)
			pollHooks(t, s, repo)
			if host.finds != 2 || baseRefreshOperationCount(t, repo, stream) != 1 {
				t.Fatal("poll before reloaded interval")
			}
			clock.Advance(time.Nanosecond)
			pollHooks(t, s, repo)
			_, err = refresh.Apply(ctx, latestBaseRefreshOperation(t, repo, stream))
			must(t, err)
			if host.finds != 4 || baseRefreshOperationCount(t, repo, stream) != 2 {
				t.Fatalf("due lookups=%d operations=%d", host.finds, baseRefreshOperationCount(t, repo, stream))
			}
		})
	}
}

func TestOwnerRebaseMakesBaseAndEdgesDueOnce(t *testing.T) {
	for _, projectRequest := range []bool{false, true} {
		t.Run(fmt.Sprintf("project-%v", projectRequest), func(t *testing.T) {
			f := newEdgeGraphFixture(t, trace.EdgeHardBlock)
			f.s.setSole(runtimeFor(f.repository))
			streams, err := f.repository.Workstreams()
			must(t, err)
			store, _, err := runtime.Open(runtime.Inputs{Config: f.s.current(), Workstreams: map[config.ProjectID][]config.WorkstreamID{f.project: streams}})
			must(t, err)
			defer store.Close()
			f.s.store = store
			clock := &fixedClock{now: f.s.now()}
			f.s.options.Reconciliation.Now = clock.Now
			host := &fakePulls{}
			f.s.options.PullRequests = host
			for i, member := range []config.WorkstreamID{f.upstream, f.downstream} {
				p := DeliveryPublication{Status: publicationOpened, Upstream: "owner/repo", Fork: "owner/fork", Branch: featureBranch(member), PullRequest: 42 + i}
				recordUpstreamOwnPullRequest(t, f.s, f.repository, member, p)
				host.prs = append(host.prs, pulls.PullRequest{Number: p.PullRequest, State: "open", Head: p.Branch, HeadRepository: p.Fork})
			}
			refresh := &baseRefresher{s: f.s, repository: f.repository}
			ctx := context.Background()
			pass := func() {
				must(t, refresh.Pass(ctx))
				must(t, f.s.edgeResolverPass(ctx, f.repository))
				for _, member := range []config.WorkstreamID{f.upstream, f.downstream} {
					_, err := refresh.Apply(ctx, latestBaseRefreshOperation(t, f.repository, member))
					must(t, err)
				}
			}
			pass()
			before := host.finds
			if projectRequest {
				out, api := f.s.rebaseProject(ctx, ProjectRebaseRequest{Project: f.project})
				if api != nil || len(out.Covered) != 2 {
					t.Fatalf("project rebase: %+v %v", out, api)
				}
			} else if _, api := f.s.rebaseWorkstream(ctx, string(f.downstream)); api != nil {
				t.Fatal(api)
			}
			// The request shares its timestamp with the preceding check.
			pass()
			want := before + 2
			if projectRequest {
				want++
			}
			if host.finds != want || baseRefreshOperationCount(t, f.repository, f.downstream) != 2 {
				t.Fatalf("request did not check own PR and edge: %d -> %d", before, host.finds)
			}
			pass()
			if host.finds != want || baseRefreshOperationCount(t, f.repository, f.downstream) != 2 {
				t.Fatal("request bypassed interval more than once")
			}
		})
	}
}

func TestServiceDefaultPullRequestPollingAndRebaseAPI(t *testing.T) {
	ctx := context.Background()
	opts, clone := projectFixture(t)
	host := &fakePulls{}
	clock := &fixedClock{now: demoStart}
	ticks := make(chan time.Time)
	var passed atomic.Int64
	opts.PullRequests = host
	opts.Reconciliation.Now = clock.Now
	opts.Reconciliation.Ticks = ticks
	opts.schedulePassed = func() { passed.Add(1) }
	s, c := start(t, opts)
	added, err := c.AddProject(ctx, request(clone))
	must(t, err)
	repo := s.sole().repository
	member := config.WorkstreamID("w_00000000000000000000000000000892")
	must(t, repo.CreateWorkstream(ctx, member, clock.Now(), ownerActor))
	_, err = trace.CreateWorkgroup(ctx, repo, []config.WorkstreamID{member}, clock.Now(), ownerActor, "fixture")
	must(t, err)
	p := DeliveryPublication{Status: publicationOpened, Upstream: "dagger/dagger", Fork: "owner/dagger", Branch: featureBranch(member), PullRequest: 42}
	host.mu.Lock()
	host.prs = []pulls.PullRequest{{Number: 42, State: "open", Head: p.Branch, HeadRepository: p.Fork}}
	host.mu.Unlock()
	recordUpstreamOwnPullRequest(t, s, repo, member, p)
	must(t, s.refreshRuntime())
	tick := func() {
		t.Helper()
		before := passed.Load()
		select {
		case ticks <- clock.Now():
		case <-time.After(demoTimeout):
			t.Fatal("reconciler did not accept tick")
		}
		deadline := time.Now().Add(demoTimeout)
		for passed.Load() <= before {
			if time.Now().After(deadline) {
				t.Fatal("pass did not complete")
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
	waitChecked := func(want int) {
		t.Helper()
		deadline := time.Now().Add(demoTimeout)
		for {
			state, err := repo.Workflow(member, baseRefreshAction)
			must(t, err)
			if state.Value == "checked" && baseRefreshOperationCount(t, repo, member) == want {
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
		host.mu.Lock()
		calls := host.finds
		host.mu.Unlock()
		if calls != 1 || baseRefreshOperationCount(t, repo, member) != 1 {
			t.Fatalf("default polled early: finds=%d", calls)
		}
	}
	view, err := c.Configuration(ctx)
	must(t, err)
	if view.Effective.For(added.Project.ID).Project.PullRequestPoll != "10m" {
		t.Fatal("configuration omits default interval")
	}
	_, err = c.RebaseWorkstream(ctx, member)
	must(t, err)
	waitChecked(2)
	tick()
	host.mu.Lock()
	calls := host.finds
	host.mu.Unlock()
	if calls != 2 {
		t.Fatalf("rebase lookup count=%d", calls)
	}
}

func TestEdgePullRequestPollReload(t *testing.T) {
	for _, failed := range []bool{false, true} {
		t.Run(fmt.Sprintf("failed-%v", failed), func(t *testing.T) {
			ctx := context.Background()
			cfg, repo, host, refresh := ownPRFixture(t)
			s := refresh.s
			clock := &fixedClock{now: demoStart}
			s.options.Config = cfg.Root.Options("")
			s.options.Reconciliation.Now = clock.Now
			s.hub = newHub()
			group, err := trace.CreateWorkgroup(ctx, repo, []config.WorkstreamID{stream, quiet}, demoStart, ownerActor, "fixture")
			must(t, err)
			must(t, trace.RecordProjectSpec(ctx, repo, group, trace.ProjectSpec{Behavior: "Dependency waits for merge", Graph: []trace.Edge{{From: quiet, To: stream, Type: trace.EdgeHardBlock}}}, demoStart, ownerActor, "fixture"))
			p := DeliveryPublication{Status: publicationOpened, Upstream: cfg.Project.Upstream, Fork: cfg.Project.PushRepository(), Branch: featureBranch(quiet), PullRequest: 42}
			recordUpstreamOwnPullRequest(t, s, repo, quiet, p)
			host.prs = []pulls.PullRequest{{Number: 42, State: "open", Head: p.Branch, HeadRepository: p.Fork}}
			failure := errors.New("upstream unavailable")
			if failed {
				host.findErr = failure
			}
			store, _, err := runtime.Open(runtime.Inputs{Config: cfg, Workstreams: map[config.ProjectID][]config.WorkstreamID{cfg.Project.ID: {stream, quiet}}})
			must(t, err)
			defer store.Close()
			s.store = store
			stage, err := s.stages(cfg, repo)
			must(t, err)
			pipeline := &pipeline{}
			pipeline.current.Store(stage)
			s.sole().pipeline = pipeline
			pass := func(wantError bool) {
				t.Helper()
				if next := pipeline.next.Swap(nil); next != nil {
					pipeline.current.Store(next)
				}
				for _, hook := range pipeline.current.Load().polls {
					if hook.name == "edge-refresh" {
						err := hook.pass(ctx)
						if wantError {
							if !errors.Is(err, failure) {
								t.Fatalf("lookup error=%v", err)
							}
						} else {
							must(t, err)
						}
					}
				}
			}
			pass(failed)
			if host.finds != 1 {
				t.Fatalf("initial finds=%d", host.finds)
			}
			path, err := cfg.Root.ProjectConfig(cfg.Project.ID)
			must(t, err)
			data, err := os.ReadFile(path)
			must(t, err)
			must(t, os.WriteFile(path, append([]byte("pull_request_poll = \"20m\"\n"), data...), 0600))
			for i := 0; i < 2; i++ {
				_, api := s.reload()
				if api != nil {
					t.Fatal(api)
				}
				pass(false)
				if host.finds != 1 {
					t.Fatal("reload restarted edge cooldown")
				}
			}
			clock.Advance(20*time.Minute - time.Nanosecond)
			pass(false)
			if host.finds != 1 {
				t.Fatal("edge polled before new interval")
			}
			clock.Advance(time.Nanosecond)
			pass(failed)
			if host.finds != 2 {
				t.Fatalf("due finds=%d", host.finds)
			}
		})
	}
}

func TestUndeliveredDependencyPullRequestPollInterval(t *testing.T) {
	for _, projectRequest := range []bool{false, true} {
		t.Run(fmt.Sprintf("project-request-%v", projectRequest), func(t *testing.T) {
			f := newDebateFixture(t, 1, 1)
			f.upstream(t)
			f.stop(t)
			ctx := context.Background()
			repo, err := trace.Open(f.s.cfg.Root, f.s.cfg.Project)
			must(t, err)
			defer repo.Close()
			base, baseTip := sealDependent(t, f, repo, config.WorkspacesGit, "", map[string]string{"base.go": "base\n"})
			dependent, _ := sealDependent(t, f, repo, config.WorkspacesGit, base, map[string]string{"dependent.go": "dependent\n"})
			cfg := f.s.about(repo)
			p := DeliveryPublication{Status: publicationOpened, Upstream: cfg.Project.Upstream, Fork: cfg.Project.PushRepository(), Base: cfg.Project.BaseBranch, Branch: featureBranch(base), Commit: baseTip, PullRequest: 42}
			recordUpstreamOwnPullRequest(t, f.s, repo, base, p)
			host := &fakePulls{prs: []pulls.PullRequest{{Number: 42, State: "open", Head: p.Branch, HeadRepository: p.Fork}}}
			f.s.options.PullRequests = host
			clock := &fixedClock{now: f.s.now()}
			f.s.options.Reconciliation.Now = clock.Now
			refresh := &baseRefresher{s: f.s, repository: repo}
			must(t, refresh.Pass(ctx))
			op := latestBaseRefreshOperation(t, repo, dependent)
			_, err = refresh.Apply(ctx, op)
			must(t, err)
			if host.finds != 1 {
				t.Fatalf("dependency lookup count=%d", host.finds)
			}
			clock.Advance(10*time.Minute - time.Nanosecond)
			must(t, refresh.Pass(ctx))
			if host.finds != 1 || baseRefreshOperationCount(t, repo, dependent) != 1 {
				t.Fatal("dependency polled early")
			}
			clock.Advance(time.Nanosecond)
			must(t, refresh.Pass(ctx))
			must(t, refresh.Pass(ctx))
			if baseRefreshOperationCount(t, repo, dependent) != 2 {
				t.Fatal("due dependency refresh not scheduled once")
			}
			_, err = refresh.Apply(ctx, latestBaseRefreshOperation(t, repo, dependent))
			must(t, err)
			if host.finds != 2 {
				t.Fatalf("dependency retry count=%d", host.finds)
			}

			f.s.setSole(runtimeFor(repo))
			streams, err := repo.Workstreams()
			must(t, err)
			store, _, err := runtime.Open(runtime.Inputs{Config: f.s.current(), Workstreams: map[config.ProjectID][]config.WorkstreamID{f.project: streams}})
			must(t, err)
			defer store.Close()
			f.s.store = store
			h := trace.Header{Schema: "osmia.trace.transition", Version: trace.Version, ID: "building", Revision: 1, Project: repo.Project(), Workstream: dependent, At: clock.Now(), Actor: foremanActor, Cause: "fixture"}
			_, err = repo.SetFeatureState(ctx, h, BuildingState, "ready for drift")
			must(t, err)
			if projectRequest {
				out, api := f.s.rebaseProject(ctx, ProjectRebaseRequest{Project: f.project})
				if api != nil || len(out.Covered) != 2 {
					t.Fatalf("project request: %+v %v", out, api)
				}
			} else if _, api := f.s.rebaseWorkstream(ctx, string(dependent)); api != nil {
				t.Fatal(api)
			}
			must(t, refresh.Pass(ctx))
			_, err = refresh.Apply(ctx, latestBaseRefreshOperation(t, repo, dependent))
			must(t, err)
			if host.finds != 3 || baseRefreshOperationCount(t, repo, dependent) != 3 {
				t.Fatal("drift request did not immediately recheck dependency")
			}
			must(t, refresh.Pass(ctx))
			if host.finds != 3 || baseRefreshOperationCount(t, repo, dependent) != 3 {
				t.Fatal("dependency bypass ran more than once")
			}
		})
	}
}

// Select by the check sequence because immediate owner checks may share a
// timestamp with the preceding operation.
func latestBaseRefreshOperation(t *testing.T, repo *trace.Repository, stream config.WorkstreamID) coreadapter.Operation {
	t.Helper()
	ops, err := repo.Operations(stream)
	must(t, err)
	var latest coreadapter.Operation
	check := 0
	for _, record := range ops {
		if record.Operation.Action != baseRefreshAction {
			continue
		}
		var in baseRefreshInput
		must(t, json.Unmarshal(record.Operation.Input, &in))
		if in.Check > check {
			check, latest = in.Check, record.Operation
		}
	}
	if latest.ID == "" {
		t.Fatal("no base refresh operation")
	}
	return latest
}
