package lifecycle_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/intake"
	"github.com/kpenfound/osmia/internal/lifecycle"
	"github.com/kpenfound/osmia/internal/runtime"
	"github.com/kpenfound/osmia/internal/trace"
)

const (
	passFirst   config.WorkstreamID = "w_00000000000000000000000000000010"
	passSecond  config.WorkstreamID = "w_00000000000000000000000000000020"
	passThird   config.WorkstreamID = "w_00000000000000000000000000000030"
	passJoining config.WorkstreamID = "w_00000000000000000000000000000001"
)

type changePassFixture struct {
	controller    *lifecycle.Controller
	local, remote *trace.Repository
	store         *runtime.Store
	logs          bytes.Buffer
}

// No scheduler runs alongside these passes. Routing, membership and the change
// journal use real traces; the fixture supplies only the service dependencies.
func newChangePassFixture(t *testing.T) *changePassFixture {
	t.Helper()
	base := newTwoProjectFixture(t)
	f := &changePassFixture{}
	var err error
	f.local, err = trace.Open(base.cfg.Root, base.cfg.Projects[0])
	must(t, err)
	t.Cleanup(func() { must(t, f.local.Close()) })
	f.remote, err = trace.Open(base.cfg.Root, base.cfg.Projects[1])
	must(t, err)
	t.Cleanup(func() { must(t, f.remote.Close()) })
	for _, stream := range []config.WorkstreamID{passFirst, passSecond, passThird, passJoining} {
		must(t, f.local.CreateWorkstream(context.Background(), stream, demoStart, ownerActor))
	}
	repos := []*trace.Repository{f.local, f.remote}
	inputs := map[config.ProjectID][]config.WorkstreamID{}
	for _, repo := range repos {
		inputs[repo.Project()], err = repo.Workstreams()
		must(t, err)
	}
	f.store, _, err = runtime.Open(runtime.Inputs{Config: base.cfg, Workstreams: inputs})
	must(t, err)
	t.Cleanup(func() { must(t, f.store.Close()) })
	var lock sync.Mutex
	f.controller = lifecycle.New(lifecycle.Dependencies{
		Options:                   lifecycle.Execution{Committee: &intake.Committee{}},
		Current:                   func() *config.Config { return base.cfg },
		Store:                     func() *runtime.Store { return f.store },
		Now:                       func() time.Time { return demoStart },
		Step:                      func(string) error { return nil },
		Log:                       func() *slog.Logger { return slog.New(slog.NewTextHandler(&f.logs, nil)) },
		GroupApprovalLock:         func(config.WorkgroupID) *sync.Mutex { return &lock },
		ObserveSharedConversation: func(trace.Commit) {},
		Repository: func(id config.ProjectID) (*trace.Repository, error) {
			for _, repo := range repos {
				if repo.Project() == id {
					return repo, nil
				}
			}
			return nil, fmt.Errorf("unknown project %s", id)
		},
		ConversationTrace: func(raw string) (config.ProjectID, config.WorkstreamID, *trace.Repository, *lifecycle.APIError) {
			for _, repo := range repos {
				streams, err := repo.Workstreams()
				if err != nil {
					panic(err)
				}
				if slices.Contains(streams, config.WorkstreamID(raw)) {
					return repo.Project(), config.WorkstreamID(raw), repo, nil
				}
			}
			return "", "", nil, &lifecycle.APIError{Message: "unknown workstream"}
		},
	})
	return f
}

func (f *changePassFixture) localGroup(t *testing.T) trace.Workgroup {
	t.Helper()
	// Recorded host order intentionally differs from leader order.
	g, err := trace.CreateWorkgroup(context.Background(), f.local, []config.WorkstreamID{passThird, passSecond, passFirst}, demoStart, ownerActor, "fixture")
	must(t, err)
	return g
}

func (f *changePassFixture) crossGroup(t *testing.T, count int) (*trace.Repository, trace.Workgroup) {
	t.Helper()
	members := []trace.CrossRepoMember{{Workstream: otherStream, Project: f.remote}}
	for _, stream := range []config.WorkstreamID{passFirst, passSecond, passThird}[:count] {
		members = append(members, trace.CrossRepoMember{Workstream: stream, Project: f.local})
	}
	wt, g, err := trace.CreateWorkgroupTrace(context.Background(), f.controller.Current().Root, members, demoStart, ownerActor, "fixture")
	must(t, err)
	t.Cleanup(func() { must(t, wt.Close()) })
	for _, member := range members {
		must(t, member.Project.JoinCrossRepoWorkgroup(context.Background(), member.Workstream, g.ID, demoStart, ownerActor, "fixture"))
	}
	return wt, g
}

func (f *changePassFixture) pending(t *testing.T, repo *trace.Repository, g trace.Workgroup) lifecycle.WorkgroupChange {
	t.Helper()
	c := lifecycle.WorkgroupChange{ID: "pending", Workgroup: g.ID, Host: g.Members[0], Origin: passFirst, State: "debating", Request: lifecycle.ChangeRequest{Kind: "cancel", Scope: "workgroup"}}
	must(t, f.controller.SaveChange(context.Background(), repo, &c))
	return c
}

func assertPassChange(t *testing.T, repo *trace.Repository, c lifecycle.WorkgroupChange, state string, revision int) {
	t.Helper()
	changes, err := lifecycle.ReadChanges(repo, c.Host)
	must(t, err)
	if len(changes) != 1 || changes[0].ID != c.ID || changes[0].State != state || changes[0].Revision != revision {
		t.Fatalf("change journal = %+v; want %s revision %d", changes, state, revision)
	}
}

func routedPasses(c *lifecycle.Controller) *[]string {
	routed := &[]string{}
	c.Step = func(step string) error {
		if strings.HasPrefix(step, "change-context-routed:") {
			*routed = append(*routed, strings.TrimPrefix(step, "change-context-routed:"))
		}
		return nil
	}
	return routed
}

func TestChangePassLocalGroupRoutesOnlyLeader(t *testing.T) {
	f := newChangePassFixture(t)
	g := f.localGroup(t)
	groups, _, err := trace.Workgroups(f.local)
	must(t, err)
	for _, stream := range g.Members {
		host, err := f.local.ChiefOfStaffHost(stream)
		must(t, err)
		scanned, scanOK := trace.WorkgroupOf(groups, stream)
		routed, routeOK := trace.WorkgroupOf(groups, host)
		if !scanOK || !routeOK || !reflect.DeepEqual(scanned, routed) {
			t.Fatalf("local member %s and chief host %s resolve differently: %+v %+v", stream, host, scanned, routed)
		}
		wt, routed, _, close, err := f.controller.ChangeContext(context.Background(), string(stream))
		must(t, err)
		close()
		if wt != f.local || !reflect.DeepEqual(routed, scanned) {
			t.Fatal("ChangeContext differs from scan membership")
		}
	}
	c := f.pending(t, f.local, g)
	routed := routedPasses(f.controller)
	reads := f.local.WorkgroupReadCount()
	must(t, f.controller.ChangePass(context.Background(), f.local))
	if !slices.Equal(*routed, []string{string(passFirst)}) {
		t.Fatalf("routed %v", *routed)
	}
	// One scan read, one ChiefOfStaffHost read and one ChangeContext read.
	if got := f.local.WorkgroupReadCount() - reads; got != 3 {
		t.Fatalf("workgroup reads = %d, want 3", got)
	}
	assertPassChange(t, f.local, c, "answering", c.Revision+1)
}

func TestChangePassCrossRepoReadsOnceAndRoutesOnlyLeader(t *testing.T) {
	for _, count := range []int{1, 3} {
		t.Run(fmt.Sprintf("local-members-%d", count), func(t *testing.T) {
			f := newChangePassFixture(t)
			wt, g := f.crossGroup(t, count)
			c := f.pending(t, wt, g)
			// A retained non-member locator must not acquire routing authority.
			must(t, f.local.RecordCrossRepoWorkgroup(context.Background(), passJoining, g.ID, demoStart, ownerActor, "retained-locator"))
			// Measure routing's own reads separately, so only scan reads remain.
			opens, specs := trace.WorkgroupTraceOpenCount(g.ID), trace.CrossRepoProjectSpecReadCount(g.ID)
			_, _, _, close, err := f.controller.ChangeContext(context.Background(), string(passFirst))
			must(t, err)
			close()
			routeOpens, routeSpecs := trace.WorkgroupTraceOpenCount(g.ID)-opens, trace.CrossRepoProjectSpecReadCount(g.ID)-specs
			routed := routedPasses(f.controller)
			opens, specs = trace.WorkgroupTraceOpenCount(g.ID), trace.CrossRepoProjectSpecReadCount(g.ID)
			must(t, f.controller.ChangePass(context.Background(), f.local))
			if !slices.Equal(*routed, []string{string(passFirst)}) {
				t.Fatalf("routed %v", *routed)
			}
			if got := trace.WorkgroupTraceOpenCount(g.ID) - opens - routeOpens; got != 1 {
				t.Fatalf("pre-routing trace opens = %d, want 1", got)
			}
			if got := trace.CrossRepoProjectSpecReadCount(g.ID) - specs - routeSpecs; got != 1 {
				t.Fatalf("pre-routing spec reads = %d, want 1", got)
			}
			assertPassChange(t, wt, c, "answering", c.Revision+1)
		})
	}
}

func TestChangePassArchivedLeaderRoutesNextLiveMember(t *testing.T) {
	for _, cross := range []bool{false, true} {
		t.Run(fmt.Sprintf("cross-repo-%t", cross), func(t *testing.T) {
			f := newChangePassFixture(t)
			var wt *trace.Repository
			var g trace.Workgroup
			if cross {
				wt, g = f.crossGroup(t, 3)
			} else {
				wt, g = f.local, f.localGroup(t)
			}
			c := f.pending(t, wt, g)
			if cross {
				f.approve(t, wt, g)
			}
			must(t, f.store.SetArchived(runtime.Archive{Project: f.local.Project(), Workstream: passFirst, Workgroup: g.ID, ArchivedAt: demoStart}))
			routed := routedPasses(f.controller)
			must(t, f.controller.ChangePass(context.Background(), f.local))
			if !slices.Equal(*routed, []string{string(passSecond)}) {
				t.Fatalf("routed %v", *routed)
			}
			assertPassChange(t, wt, c, "answering", c.Revision+1)
		})
	}
}

func TestChangePassJoinDuringPassUsesFreshMembershipNextPass(t *testing.T) {
	for _, cross := range []bool{false, true} {
		t.Run(fmt.Sprintf("cross-repo-%t", cross), func(t *testing.T) {
			f := newChangePassFixture(t)
			var wt *trace.Repository
			var g trace.Workgroup
			if cross {
				wt, g = f.crossGroup(t, 3)
			} else {
				wt, g = f.local, f.localGroup(t)
			}
			c := f.pending(t, wt, g)
			c.State = "approved"
			c.Request.Kind = "amend"
			c.Before = trace.ProjectSpec{Behavior: "Shared intent"}
			c.After = trace.ProjectSpec{Behavior: "Shared intent with an approved clarification"}
			c.Decision = &lifecycle.ChangeDecision{Revision: c.Revision, Decision: "approve"}
			must(t, f.controller.SaveChange(context.Background(), wt, &c))
			var routed []string
			joined := false
			f.controller.Step = func(step string) error {
				if !strings.HasPrefix(step, "change-context-routed:") {
					return nil
				}
				routed = append(routed, strings.TrimPrefix(step, "change-context-routed:"))
				if joined {
					return nil
				}
				joined = true
				// This member was already enumerated, before the old leader.
				must(t, trace.AddWorkgroupMember(context.Background(), wt, g.ID, passJoining, demoStart, ownerActor, "join"))
				if cross {
					must(t, f.local.JoinCrossRepoWorkgroup(context.Background(), passJoining, g.ID, demoStart, ownerActor, "join"))
				}
				return nil
			}
			must(t, f.controller.ChangePass(context.Background(), f.local))
			if !slices.Equal(routed, []string{string(passFirst)}) {
				t.Fatalf("first pass routed %v", routed)
			}
			// The unchanged final leader gate fences the stale leader.
			assertPassChange(t, wt, c, "approved", c.Revision)
			routed = nil
			must(t, f.controller.ChangePass(context.Background(), f.local))
			if !slices.Equal(routed, []string{string(passJoining)}) {
				t.Fatalf("next pass routed %v", routed)
			}
			assertPassChange(t, wt, c, "applied", c.Revision+3)
			published, found, err := trace.ApprovedWorkgroupRevision(wt, c.Host)
			must(t, err)
			if !found || published.Change != c.ID || !reflect.DeepEqual(published.Spec, c.After) {
				t.Fatal("joined leader lost the approved shared revision")
			}
			commits, err := wt.CommitCount(string(c.Host))
			must(t, err)
			must(t, f.controller.ChangePass(context.Background(), f.local))
			assertPassChange(t, wt, c, "applied", c.Revision+3)
			after, err := wt.CommitCount(string(c.Host))
			must(t, err)
			if after != commits {
				t.Fatal("later pass reapplied the decision")
			}
		})
	}
}

func (f *changePassFixture) approve(t *testing.T, wt *trace.Repository, g trace.Workgroup) {
	t.Helper()
	approval := trace.GroupApproval{Workgroup: g.ID, Complete: true}
	for _, stream := range g.Members[1:] {
		repo := f.local
		if stream == otherStream {
			repo = f.remote
		}
		approval.Members = append(approval.Members, trace.GroupMemberApproval{Project: repo.Project(), Workstream: stream, Record: json.RawMessage(`{}`)})
	}
	must(t, trace.RecordGroupApproval(context.Background(), wt, g.Members[0], approval, demoStart, ownerActor))
}

func (f *changePassFixture) retire(t *testing.T, g trace.Workgroup) {
	t.Helper()
	for _, stream := range g.Members[1:] {
		repo := f.local
		if stream == otherStream {
			repo = f.remote
		}
		must(t, f.store.SetArchived(runtime.Archive{Project: repo.Project(), Workstream: stream, Workgroup: g.ID, ArchivedAt: demoStart}))
	}
	// The adopted conversation source keeps the locator after retirement.
	must(t, f.local.CreateWorkstream(context.Background(), g.Members[0], demoStart, ownerActor))
	must(t, f.local.RecordCrossRepoWorkgroup(context.Background(), g.Members[0], g.ID, demoStart, ownerActor, "retained-source"))
}

func TestChangePassFullyRetiredPendingDecisionsRemainHeld(t *testing.T) {
	f := newChangePassFixture(t)
	wt, g := f.crossGroup(t, 3)
	f.approve(t, wt, g)
	c := f.pending(t, wt, g)
	f.retire(t, g)
	routed := routedPasses(f.controller)
	for pass := range 3 {
		if pass == 2 {
			// A settled decision must stop warning on the next fresh read.
			c.State = "rejected"
			must(t, f.controller.SaveChange(context.Background(), wt, &c))
		}
		f.logs.Reset()
		opens, specs := trace.WorkgroupTraceOpenCount(g.ID), trace.CrossRepoProjectSpecReadCount(g.ID)
		must(t, f.controller.ChangePass(context.Background(), f.local))
		if got := trace.WorkgroupTraceOpenCount(g.ID) - opens; got != 1 {
			t.Fatalf("pass %d pre-routing trace opens = %d, want 1", pass+1, got)
		}
		if got := trace.CrossRepoProjectSpecReadCount(g.ID) - specs; got != 1 {
			t.Fatalf("pass %d pre-routing spec reads = %d, want 1", pass+1, got)
		}
		if len(*routed) != 0 {
			t.Fatalf("retired group routed %v", *routed)
		}
		if pass < 2 && (!strings.Contains(f.logs.String(), "workgroup changes remain held") || !strings.Contains(f.logs.String(), string(g.ID))) {
			t.Fatalf("missing held warning: %s", &f.logs)
		}
		if pass == 2 && strings.Contains(f.logs.String(), "workgroup changes remain held") {
			t.Fatalf("settled decision still warns: %s", &f.logs)
		}
		assertPassChange(t, wt, c, c.State, c.Revision)
	}
}

func TestChangePassRetirementRequiresApprovedArchiveIdentity(t *testing.T) {
	f := newChangePassFixture(t)
	wt, g := f.crossGroup(t, 3)
	c := f.pending(t, wt, g)
	f.retire(t, g)
	approval := trace.GroupApproval{Workgroup: g.ID, Complete: true}
	for _, stream := range g.Members[1:] {
		project := f.local.Project()
		if stream == otherStream || stream == passFirst {
			project = f.remote.Project()
		}
		approval.Members = append(approval.Members, trace.GroupMemberApproval{Project: project, Workstream: stream, Record: json.RawMessage(`{}`)})
	}
	must(t, trace.RecordGroupApproval(context.Background(), wt, g.Members[0], approval, demoStart, ownerActor))
	routed := routedPasses(f.controller)
	opens, specs := trace.WorkgroupTraceOpenCount(g.ID), trace.CrossRepoProjectSpecReadCount(g.ID)
	err := f.controller.ChangePass(context.Background(), f.local)
	if want := "identity does not match workgroup"; err == nil || !strings.Contains(err.Error(), want) {
		t.Fatalf("retirement validation = %v, want %q", err, want)
	}
	if len(*routed) != 0 {
		t.Fatalf("invalid retirement routed %v", *routed)
	}
	if got := trace.WorkgroupTraceOpenCount(g.ID) - opens; got != 1 {
		t.Fatalf("pre-routing trace opens = %d, want 1", got)
	}
	if got := trace.CrossRepoProjectSpecReadCount(g.ID) - specs; got != 1 {
		t.Fatalf("pre-routing spec reads = %d, want 1", got)
	}
	assertPassChange(t, wt, c, c.State, c.Revision)
}

// A workgroup whose members were all abandoned and archived before
// ratification has no approval. It is retired on its archive records, and
// the pass carries on instead of failing on every retry.
func TestChangePassUnratifiedRetiredGroupRemainsHeld(t *testing.T) {
	f := newChangePassFixture(t)
	wt, g := f.crossGroup(t, 3)
	c := f.pending(t, wt, g)
	f.retire(t, g)
	routed := routedPasses(f.controller)
	for pass := range 2 {
		f.logs.Reset()
		must(t, f.controller.ChangePass(context.Background(), f.local))
		if len(*routed) != 0 {
			t.Fatalf("pass %d: retired group routed %v", pass+1, *routed)
		}
		if !strings.Contains(f.logs.String(), "workgroup changes remain held") || !strings.Contains(f.logs.String(), string(g.ID)) {
			t.Fatalf("pass %d: missing held warning: %s", pass+1, &f.logs)
		}
		assertPassChange(t, wt, c, c.State, c.Revision)
	}
	retired, err := f.controller.WorkgroupFullyRetired(context.Background(), g)
	must(t, err)
	if !retired {
		t.Fatal("unratified group with every member archived is not retired")
	}
}
