package service

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

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/trace"
)

// observeJoinPasses instruments the running controllers, without driving a pass.
func observeJoinPasses(s *Service) func(*testing.T) {
	var phase atomic.Int32
	completed := make(chan string, 32)
	_, projects := s.runtimes()
	var observedKeys []string
	for _, p := range projects {
		st := *p.pipeline.current.Load()
		st.hooks = slices.Clone(st.hooks)
		for i, hook := range st.hooks {
			if hook.name != "events" && hook.name != "workgroup-change" {
				continue
			}
			observedKeys = append(observedKeys, fmt.Sprintf("%s:%s", p.id, hook.name))
			st.hooks[i].pass = func(ctx context.Context) error {
				current := phase.Load()
				if err := hook.pass(ctx); err != nil {
					return err
				}
				if current != 0 {
					select {
					case completed <- fmt.Sprintf("%d:%s:%s", current, p.id, hook.name):
					default:
					}
				}
				return nil
			}
		}
		p.pipeline.next.Store(&st)
	}
	return func(t *testing.T) {
		t.Helper()
		current := phase.Add(1)
		needed := map[string]bool{}
		for _, key := range observedKeys {
			needed[fmt.Sprintf("%d:%s", current, key)] = true
		}
		deadline := time.NewTimer(demoTimeout)
		defer deadline.Stop()
		for len(needed) != 0 {
			select {
			case key := <-completed:
				delete(needed, key)
			case <-s.lifetime.Done():
				t.Fatalf("controller stopped during join: %v", s.lifetime.Err())
			case <-deadline.C:
				t.Fatalf("controller did not complete join passes: %v", needed)
			}
		}
	}
}

func TestDraftWorkgroupTransferKeepsControllerRoutesAcrossRestart(t *testing.T) {
	t.Parallel()
	joining := &changeRaceGate{step: "workgroup-member-joining", reached: make(chan struct{}), released: make(chan struct{})}
	transferred := &changeRaceGate{step: "workgroup-member-transferred", reached: make(chan struct{}), released: make(chan struct{})}
	defer joining.release()
	defer transferred.release()
	f := newCrossRepoRatifyFixture(t, func(opts *Options) {
		opts.Reconciliation.Now = (&fixedClock{At: demoStart}).Now
		cfg, err := config.Load(opts.Config)
		must(t, err)
		runChief(opts, cfg, &chiefTurns{})
		opts.boundary = func(ctx context.Context, name string) error {
			if err := joining.checkpoint(ctx, name); err != nil {
				return err
			}
			return transferred.checkpoint(ctx, name)
		}
	})
	defer f.stop(t)
	observe := observeJoinPasses(f.s)
	joining.armed.Store(true)
	transferred.armed.Store(true)
	done := make(chan *APIError, 1)
	var id config.WorkgroupID
	go func() {
		var api *APIError
		id, api = f.s.DraftWorkgroup(context.Background(), []config.WorkstreamID{f.stream1, f.stream2})
		done <- api
	}()
	joining.await(t)
	repo, err := f.s.repository(f.project)
	must(t, err)
	groups, _, err := trace.Workgroups(repo)
	must(t, err)
	if _, ok := trace.WorkgroupOf(groups, f.stream1); !ok {
		t.Fatal("old route missing before transfer")
	}
	if pointer, err := repo.CrossRepoWorkgroupOf(f.stream1); err != nil || pointer != "" {
		t.Fatalf("premature shared route: %s %v", pointer, err)
	}
	observe(t)
	joining.release()
	transferred.await(t)
	pointer, err := repo.CrossRepoWorkgroupOf(f.stream1)
	must(t, err)
	groups, _, err = trace.Workgroups(repo)
	must(t, err)
	if _, ok := trace.WorkgroupOf(groups, f.stream1); ok || pointer == "" {
		t.Fatalf("transfer has no unique shared route: pointer=%s groups=%+v", pointer, groups)
	}
	if _, err := repo.ChiefOfStaffThread(f.stream1); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("local chief remains: %v", err)
	}
	// A ready event forces the real event pass through the incomplete join.
	_, err = repo.Transact(context.Background(), trace.Transaction{Transition: trace.Transition{Header: trace.Header{Schema: "osmia.trace.transition", Version: trace.Version, ID: "join-notice", Revision: 1, Project: f.project, Workstream: f.stream1, At: demoStart.Add(-time.Hour), Actor: ownerActor, Cause: "owner"}, Subject: "join-notice", To: "recorded", Reason: "Observe member transfer"}, Events: []trace.Event{{ID: "join-notice", Kind: "notice", Body: "Member transfer remains pending."}}})
	must(t, err)
	notice := func() trace.OutboxEntry {
		out, err := repo.Outbox(f.stream1)
		must(t, err)
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
	transferred.release()
	select {
	case api := <-done:
		if api != nil {
			t.Fatal(api)
		}
	case <-time.After(demoTimeout):
		t.Fatal("join did not complete")
	}
	f.awaitProjectSpec(t)
	authority, err := trace.OpenWorkgroupAuthority(f.s.current().Root, id)
	must(t, err)
	shared, err := authority.ChiefOfStaffThread(f.stream1)
	must(t, err)
	must(t, authority.Close())
	f.stop(t)
	f.start(t)
	observeJoinPasses(f.s)(t)
	for _, member := range []struct {
		project config.ProjectID
		stream  config.WorkstreamID
	}{{f.project, f.stream1}, {f.project2, f.stream2}} {
		r, err := f.s.repository(member.project)
		must(t, err)
		got, err := r.CrossRepoWorkgroupOf(member.stream)
		must(t, err)
		if got != id {
			t.Fatalf("restart replaced group: %s", got)
		}
		groups, _, err := trace.Workgroups(r)
		must(t, err)
		if _, ok := trace.WorkgroupOf(groups, member.stream); ok {
			t.Fatal("startup added replacement local group")
		}
		target, _, _, close, err := f.s.chiefRepository(context.Background(), r, member.stream)
		must(t, err)
		chief, err := target.ChiefOfStaffThread(member.stream)
		close()
		must(t, err)
		if chief.Identity != shared.Identity {
			t.Fatal("shared chief identity changed")
		}
	}
	entries, err := os.ReadDir(filepath.Join(f.s.current().Root.String(), "workgroups"))
	must(t, err)
	if len(entries) != 1 {
		t.Fatalf("duplicate workgroups: %v", entries)
	}
}

func TestChangeContextFollowsTransferredMemberRoute(t *testing.T) {
	t.Parallel()
	gate := &changeRaceGate{reached: make(chan struct{}), released: make(chan struct{})}
	defer gate.release()
	f := newCrossRepoRatifyFixture(t, func(opts *Options) { opts.boundary = gate.checkpoint })
	gate.step = "change-context-routed:" + string(f.stream1)
	defer f.stop(t)
	// Start the controller's read on its old local route, then finish the join.
	gate.armed.Store(true)
	gate.await(t)
	id, api := f.s.DraftWorkgroup(context.Background(), []config.WorkstreamID{f.stream1, f.stream2})
	if api != nil {
		t.Fatal(api)
	}
	gate.release()
	f.awaitProjectSpec(t)
	repo, g, _, close, err := f.s.changeContext(context.Background(), string(f.stream1))
	must(t, err)
	close()
	if g.ID != id || repo.Project() == f.project {
		t.Fatal("stale local route survived transfer")
	}
}

func TestDraftWorkgroupResumesAuthenticatedRetractedMemberAcrossRestart(t *testing.T) {
	t.Parallel()
	gate := &changeRaceGate{reached: make(chan struct{}), released: make(chan struct{})}
	defer gate.release()
	f := newCrossRepoRatifyFixture(t, func(opts *Options) { opts.boundary = gate.checkpoint })
	gate.step = "change-context-routed:" + string(f.stream1)
	defer f.stop(t)
	awaitJoinDrafts(t, f)
	ctx := context.Background()
	repo1, err := f.s.repository(f.project)
	must(t, err)
	repo2, err := f.s.repository(f.project2)
	must(t, err)
	authority, g, err := trace.CreateWorkgroupTrace(ctx, f.s.current().Root, []trace.CrossRepoMember{{Workstream: f.stream1, Project: repo1}, {Workstream: f.stream2, Project: repo2}}, f.clock.Now(), ownerActor, "draft-workgroup")
	must(t, err)
	original, err := authority.ChiefOfStaffThread(g.Members[0])
	must(t, err)
	must(t, authority.Close())
	observe := observeJoinPasses(f.s)
	gate.armed.Store(true)
	gate.await(t)
	// This is a durable interruption of the owner join before its locator.
	must(t, repo1.RetractChiefOfStaff(ctx, f.stream1))
	must(t, repo1.RetractWorkgroup(ctx, f.stream1, f.clock.Now(), ownerActor, "draft-workgroup"))
	gate.release()
	observe(t)
	if _, _, _, close, err := f.s.changeContext(ctx, string(f.stream1)); !errors.Is(err, errWorkgroupJoining) {
		close()
		t.Fatalf("unauthenticated missing-group result: %v", err)
	} else {
		close()
	}
	f.stop(t)
	f.start(t)
	repo1, err = f.s.repository(f.project)
	must(t, err)
	groups, _, err := trace.Workgroups(repo1)
	must(t, err)
	if _, ok := trace.WorkgroupOf(groups, f.stream1); ok {
		t.Fatal("startup replaced the pending join with a solitary group")
	}
	id, api := f.s.DraftWorkgroup(ctx, []config.WorkstreamID{f.stream1, f.stream2})
	if api != nil || id != g.ID {
		t.Fatalf("retry replaced join: %s %v", id, api)
	}
	target, _, _, close, err := f.s.chiefRepository(ctx, repo1, f.stream1)
	must(t, err)
	chief, err := target.ChiefOfStaffThread(f.stream1)
	close()
	must(t, err)
	if chief.Identity != original.Identity {
		t.Fatal("retry replaced original shared chief")
	}
	f.awaitProjectSpec(t)
	// No retraction or authoritative membership authenticates this bare stream.
	bare := config.WorkstreamID("w_00000000000000000000000000000129")
	must(t, repo1.CreateWorkstream(ctx, bare, f.clock.Now(), ownerActor))
	if _, _, _, close, err := f.s.changeContext(ctx, string(bare)); err == nil || errors.Is(err, errWorkgroupJoining) {
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
	f := newCrossRepoRatifyFixture(t, func(opts *Options) {
		opts.boundary = func(_ context.Context, name string) error {
			if armed.Load() && name == "workgroup-member-routed" && routed.Add(1) == 2 {
				return errors.New("interrupted before final local removal")
			}
			return nil
		}
	})
	defer f.stop(t)
	awaitJoinDrafts(t, f)
	armed.Store(true)
	if _, api := f.s.DraftWorkgroup(context.Background(), []config.WorkstreamID{f.stream1, f.stream2}); api == nil {
		t.Fatal("join did not interrupt before last local removal")
	}
	repo, err := f.s.repository(f.project)
	must(t, err)
	id, err := repo.CrossRepoWorkgroupOf(f.stream1)
	must(t, err)
	_, g, _, err := repo.CrossRepoProjectSpec(id)
	must(t, err)
	if _, err := f.s.crossRepoMemberRepos(context.Background(), g); !errors.Is(err, errWorkgroupJoining) {
		t.Fatalf("all locators admitted an unfinished transfer: %v", err)
	}
	f.stop(t)
	f.start(t)
	_, err = f.s.repository(f.project)
	must(t, err)
	if _, err := f.s.crossRepoMemberRepos(context.Background(), g); !errors.Is(err, errWorkgroupJoining) {
		t.Fatalf("startup admitted unfinished local removal: %v", err)
	}
	got, api := f.s.DraftWorkgroup(context.Background(), []config.WorkstreamID{f.stream1, f.stream2})
	if api != nil || got != id {
		t.Fatalf("retry replaced join: %s %v", got, api)
	}
	if _, err := f.s.crossRepoMemberRepos(context.Background(), g); err != nil {
		t.Fatalf("completed transfer remained held: %v", err)
	}
	f.awaitProjectSpec(t)
}

// awaitJoinDrafts keeps a join-recovery restart from interrupting unrelated
// member drafting. The join checkpoints still run against the real controller.
func awaitJoinDrafts(t *testing.T, f *crossRepoRatifyFixture) {
	t.Helper()
	wait, release := serviceChanges(t, f.s)
	defer release()
	deadline := time.Now().Add(demoTimeout)
	for {
		ready := true
		for _, member := range []struct {
			project config.ProjectID
			stream  config.WorkstreamID
		}{{f.project, f.stream1}, {f.project2, f.stream2}} {
			repo, err := f.s.repository(member.project)
			must(t, err)
			ops, err := repo.Operations(member.stream)
			must(t, err)
			settled := false
			for _, op := range ops {
				if op.Operation.Action == DraftAction && op.Result != nil && op.Result.Outcome == "succeeded" {
					settled = true
				}
			}
			_, _, err = memberSpecAndPlan(repo, member.stream)
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
