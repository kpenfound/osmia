package lifecycle_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kpenfound/osmia/internal/servicetest/journey"

	"github.com/kpenfound/osmia/internal/api"
	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/intake"
	"github.com/kpenfound/osmia/internal/lifecycle"
	"github.com/kpenfound/osmia/internal/projectstore"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	"github.com/kpenfound/osmia/internal/servicetest/intakefixture"
	"github.com/kpenfound/osmia/internal/trace"
)

func observeJoinPasses(s *journey.Service) func(*testing.T) {
	var phase atomic.Int32
	completed := make(chan string, 32)
	_, projects := s.Runtimes()
	var observedKeys []string
	for _, p := range projects {
		st := *p.Pipeline.Current.Load()
		st.Hooks = slices.Clone(st.Hooks)
		for i, hook := range st.Hooks {
			if hook.Name != "events" && hook.Name != "workgroup-change" {
				continue
			}
			observedKeys = append(observedKeys, fmt.Sprintf("%s:%s", p.Id, hook.Name))
			st.Hooks[i].Pass = func(ctx context.Context) error {
				current := phase.Load()
				if err := hook.Pass(ctx); err != nil {
					return err
				}
				if current != 0 {
					select {
					case completed <- fmt.Sprintf("%d:%s:%s", current, p.Id, hook.Name):
					default:
					}
				}
				return nil
			}
		}
		p.Pipeline.Next.Store(&st)
	}
	return func(t *testing.T) {
		t.Helper()
		current := phase.Add(1)
		needed := map[string]bool{}
		for _, key := range observedKeys {
			needed[fmt.Sprintf("%d:%s", current, key)] = true
		}
		deadline := time.NewTimer(fixturekit.DemoTimeout)
		defer deadline.Stop()
		for len(needed) != 0 {
			select {
			case key := <-completed:
				delete(needed, key)
			case <-s.Lifetime.Done():
				t.Fatalf("controller stopped during join: %v", s.Lifetime.Err())
			case <-deadline.C:
				t.Fatalf("controller did not complete join passes: %v", needed)
			}
		}
	}
}
func TestDraftWorkgroupTransferKeepsControllerRoutesAcrossRestart(t *testing.T) {
	t.Parallel()
	joining := &intakefixture.ChangeRaceGate{Step: "workgroup-member-joining", Reached: make(chan struct{})}
	transferred := &intakefixture.ChangeRaceGate{Step: "workgroup-member-transferred", Reached: make(chan struct{})}
	defer joining.Release()
	defer transferred.Release()
	f := newCrossRepoRatifyFixture(t, func(opts *journey.Options) {
		opts.Reconciliation.Now = (&fixturekit.FixedClock{At: fixturekit.DemoStart}).Now
		cfg, err := projectstore.LoadConfig(opts.Config)
		fixturekit.Must(t, err)
		journey.RunManager(opts, cfg, &fixturekit.ManagerTurns{})
		opts.Boundary = func(ctx context.Context, name string) error {
			if err := joining.Checkpoint(ctx, name); err != nil {
				return err
			}
			return transferred.Checkpoint(ctx, name)
		}
	})
	defer f.Stop(t)
	observe := observeJoinPasses(f.S)
	joining.Armed.Store(true)
	transferred.Armed.Store(true)
	done := make(chan *api.APIError, 1)
	var id config.WorkgroupID
	go func() {
		var apiFault *api.APIError
		id, apiFault = f.S.Lifecycle().DraftWorkgroup(context.Background(), []config.WorkstreamID{f.Stream1, f.Stream2})
		done <- apiFault
	}()
	joining.Await(t)
	repo, err := f.S.Repository(f.Project)
	fixturekit.Must(t, err)
	groups, _, err := trace.Workgroups(repo)
	fixturekit.Must(t, err)
	if _, ok := trace.WorkgroupOf(groups, f.Stream1); !ok {
		t.Fatal("old route missing before transfer")
	}
	if pointer, err := repo.CrossRepoWorkgroupOf(f.Stream1); err != nil || pointer != "" {
		t.Fatalf("premature shared route: %s %v", pointer, err)
	}
	observe(t)
	joining.Release()
	transferred.Await(t)
	pointer, err := repo.CrossRepoWorkgroupOf(f.Stream1)
	fixturekit.Must(t, err)
	groups, _, err = trace.Workgroups(repo)
	fixturekit.Must(t, err)
	if _, ok := trace.WorkgroupOf(groups, f.Stream1); ok || pointer == "" {
		t.Fatalf("transfer has no unique shared route: pointer=%s groups=%+v", pointer, groups)
	}
	if _, err := repo.ManagerThread(f.Stream1); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("local chief remains: %v", err)
	}
	_, err = repo.Transact(context.Background(), trace.Transaction{Transition: trace.Transition{Header: trace.Header{Schema: "osmia.trace.transition", Version: trace.Version, ID: "join-notice", Revision: 1, Project: f.Project, Workstream: f.Stream1, At: fixturekit.DemoStart.Add(-time.Hour), Actor: intake.OwnerActor, Cause: "owner"}, Subject: "join-notice", To: "recorded", Reason: "Observe member transfer"}, Events: []trace.Event{{ID: "join-notice", Kind: "notice", Body: "Member transfer remains pending."}}})
	fixturekit.Must(t, err)
	notice := func() trace.OutboxEntry {
		out, err := repo.Outbox(f.Stream1)
		fixturekit.Must(t, err)
		for _, e := range out {
			if e.Event.ID == "join-notice" {
				return e
			}
		}
		t.Fatal("missing join notice")
		return trace.OutboxEntry{}
	}
	before := notice()
	observe(t)
	after := notice()
	if !reflect.DeepEqual(before, after) {
		t.Fatal("incomplete join consumed or claimed events")
	}
	transferred.Release()
	select {
	case apiFault := <-done:
		if apiFault != nil {
			t.Fatal(apiFault)
		}
	case <-time.After(fixturekit.DemoTimeout):
		t.Fatal("join did not complete")
	}
	awaitProjectSpec(f, t)
	authority, err := trace.OpenWorkgroupAuthority(f.S.Current().Root, id)
	fixturekit.Must(t, err)
	shared, err := authority.ManagerThread(f.Stream1)
	fixturekit.Must(t, err)
	fixturekit.Must(t, authority.Close())
	f.Stop(t)
	f.Start(t)
	observeJoinPasses(f.S)(t)
	for _, member := range []struct {
		project config.ProjectID
		stream  config.WorkstreamID
	}{{f.Project, f.Stream1}, {f.Project2, f.Stream2}} {
		r, err := f.S.Repository(member.project)
		fixturekit.Must(t, err)
		got, err := r.CrossRepoWorkgroupOf(member.stream)
		fixturekit.Must(t, err)
		if got != id {
			t.Fatalf("restart replaced group: %s", got)
		}
		groups, _, err := trace.Workgroups(r)
		fixturekit.Must(t, err)
		if _, ok := trace.WorkgroupOf(groups, member.stream); ok {
			t.Fatal("startup added replacement local group")
		}
		target, _, _, close, err := f.S.Lifecycle().ManagerRepository(context.Background(), r, member.stream)
		fixturekit.Must(t, err)
		manager, err := target.ManagerThread(member.stream)
		close()
		fixturekit.Must(t, err)
		if manager.Identity != shared.Identity {
			t.Fatal("shared chief identity changed")
		}
	}
	entries, err := os.ReadDir(filepath.Join(f.S.Current().Root.String(), "workgroups"))
	fixturekit.Must(t, err)
	if len(entries) != 1 {
		t.Fatalf("duplicate workgroups: %v", entries)
	}
}
func TestChangeContextFollowsTransferredMemberRoute(t *testing.T) {
	t.Parallel()
	gate := &intakefixture.ChangeRaceGate{Reached: make(chan struct{})}
	defer gate.Release()
	f := newCrossRepoRatifyFixture(t, func(opts *journey.Options) { opts.Boundary = gate.Checkpoint })
	gate.Step = "change-context-routed:" + string(f.Stream1)
	defer f.Stop(t)
	gate.Armed.Store(true)
	gate.Await(t)
	id, apiFault := f.S.Lifecycle().DraftWorkgroup(context.Background(), []config.WorkstreamID{f.Stream1, f.Stream2})
	if apiFault != nil {
		t.Fatal(apiFault)
	}
	gate.Release()
	awaitProjectSpec(f, t)
	repo, g, _, close, err := f.S.Lifecycle().ChangeContext(context.Background(), string(f.Stream1))
	fixturekit.Must(t, err)
	close()
	if g.ID != id || repo.Project() == f.Project {
		t.Fatal("stale local route survived transfer")
	}
}
func TestDraftWorkgroupResumesAuthenticatedRetractedMemberAcrossRestart(t *testing.T) {
	t.Parallel()
	gate := &intakefixture.ChangeRaceGate{Reached: make(chan struct{})}
	defer gate.Release()
	f := newCrossRepoRatifyFixture(t, func(opts *journey.Options) { opts.Boundary = gate.Checkpoint })
	gate.Step = "change-context-routed:" + string(f.Stream1)
	defer f.Stop(t)
	awaitJoinDrafts(t, f)
	ctx := context.Background()
	repo1, err := f.S.Repository(f.Project)
	fixturekit.Must(t, err)
	repo2, err := f.S.Repository(f.Project2)
	fixturekit.Must(t, err)
	authority, g, err := trace.CreateWorkgroupTrace(ctx, f.S.Current().Root, []trace.CrossRepoMember{{Workstream: f.Stream1, Project: repo1}, {Workstream: f.Stream2, Project: repo2}}, f.Clock.Now(), intake.OwnerActor, "draft-workgroup")
	fixturekit.Must(t, err)
	original, err := authority.ManagerThread(g.Members[0])
	fixturekit.Must(t, err)
	fixturekit.Must(t, authority.Close())
	observe := observeJoinPasses(f.S)
	gate.Armed.Store(true)
	gate.Await(t)
	fixturekit.Must(t, repo1.RetractManager(ctx, f.Stream1))
	fixturekit.Must(t, repo1.RetractWorkgroup(ctx, f.Stream1, f.Clock.Now(), intake.OwnerActor, "draft-workgroup"))
	gate.Release()
	observe(t)
	if _, _, _, close, err := f.S.Lifecycle().ChangeContext(ctx, string(f.Stream1)); !errors.Is(err, intake.ErrWorkgroupJoining) {
		close()
		t.Fatalf("unauthenticated missing-group result: %v", err)
	} else {
		close()
	}
	f.Stop(t)
	f.Start(t)
	repo1, err = f.S.Repository(f.Project)
	fixturekit.Must(t, err)
	groups, _, err := trace.Workgroups(repo1)
	fixturekit.Must(t, err)
	if _, ok := trace.WorkgroupOf(groups, f.Stream1); ok {
		t.Fatal("startup replaced the pending join with a solitary group")
	}
	id, apiFault := f.S.Lifecycle().DraftWorkgroup(ctx, []config.WorkstreamID{f.Stream1, f.Stream2})
	if apiFault != nil || id != g.ID {
		t.Fatalf("retry replaced join: %s %v", id, apiFault)
	}
	target, _, _, close, err := f.S.Lifecycle().ManagerRepository(ctx, repo1, f.Stream1)
	fixturekit.Must(t, err)
	manager, err := target.ManagerThread(f.Stream1)
	close()
	fixturekit.Must(t, err)
	if manager.Identity != original.Identity {
		t.Fatal("retry replaced original shared chief")
	}
	awaitProjectSpec(f, t)
	bare := config.WorkstreamID("w_00000000000000000000000000000129")
	fixturekit.Must(t, repo1.CreateWorkstream(ctx, bare, f.Clock.Now(), intake.OwnerActor))
	if _, _, _, close, err := f.S.Lifecycle().ChangeContext(ctx, string(bare)); err == nil || errors.Is(err, intake.ErrWorkgroupJoining) {
		close()
		t.Fatalf("unexplained missing group was hidden: %v", err)
	} else {
		close()
	}
}
func TestDraftWorkgroupInterruptsAfterLastLocatorBeforeLocalRemoval(t *testing.T) {
	t.Parallel()
	var routed atomic.Int32
	var armed atomic.Bool
	f := newCrossRepoRatifyFixture(t, func(opts *journey.Options) {
		opts.Boundary = func(_ context.Context, name string) error {
			if armed.Load() && name == "workgroup-member-routed" && routed.Add(1) == 2 {
				return errors.New("interrupted before final local removal")
			}
			return nil
		}
	})
	defer f.Stop(t)
	awaitJoinDrafts(t, f)
	armed.Store(true)
	if _, apiFault := f.S.Lifecycle().DraftWorkgroup(context.Background(), []config.WorkstreamID{f.Stream1, f.Stream2}); apiFault == nil {
		t.Fatal("join did not interrupt before last local removal")
	}
	repo, err := f.S.Repository(f.Project)
	fixturekit.Must(t, err)
	id, err := repo.CrossRepoWorkgroupOf(f.Stream1)
	fixturekit.Must(t, err)
	_, g, _, err := repo.CrossRepoProjectSpec(id)
	fixturekit.Must(t, err)
	if _, err := f.S.Lifecycle().CrossRepoMemberRepos(context.Background(), g); !errors.Is(err, intake.ErrWorkgroupJoining) {
		t.Fatalf("all locators admitted an unfinished transfer: %v", err)
	}
	f.Stop(t)
	f.Start(t)
	_, err = f.S.Repository(f.Project)
	fixturekit.Must(t, err)
	if _, err := f.S.Lifecycle().CrossRepoMemberRepos(context.Background(), g); !errors.Is(err, intake.ErrWorkgroupJoining) {
		t.Fatalf("startup admitted unfinished local removal: %v", err)
	}
	got, apiFault := f.S.Lifecycle().DraftWorkgroup(context.Background(), []config.WorkstreamID{f.Stream1, f.Stream2})
	if apiFault != nil || got != id {
		t.Fatalf("retry replaced join: %s %v", got, apiFault)
	}
	if _, err := f.S.Lifecycle().CrossRepoMemberRepos(context.Background(), g); err != nil {
		t.Fatalf("completed transfer remained held: %v", err)
	}
	awaitProjectSpec(f, t)
}

func awaitJoinDrafts(t *testing.T, f *journey.CrossRepoRatifyFixture) {
	t.Helper()
	wait, release := journey.ServiceChanges(t, f.S)
	defer release()
	deadline := time.Now().Add(fixturekit.DemoTimeout)
	for {
		ready := true
		for _, member := range []struct {
			project config.ProjectID
			stream  config.WorkstreamID
		}{{f.Project, f.Stream1}, {f.Project2, f.Stream2}} {
			repo, err := f.S.Repository(member.project)
			fixturekit.Must(t, err)
			ops, err := repo.Operations(member.stream)
			fixturekit.Must(t, err)
			settled := false
			for _, op := range ops {
				if op.Operation.Action == intake.DraftAction && op.Result != nil && op.Result.Outcome == "succeeded" {
					settled = true
				}
			}
			_, _, err = lifecycle.MemberSpecAndPlan(repo, member.stream)
			ready = ready && settled && err == nil
		}
		if ready {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("member drafting did not settle")
		}
		wait()
	}
}
