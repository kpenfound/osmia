package service

import (
	"context"
	"strings"
	"testing"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/runtime"
	"github.com/kpenfound/osmia/internal/scheduler"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	"github.com/kpenfound/osmia/internal/trace"
)

func crossProjectAdmissionFixture(t *testing.T) (*Service, []trace.CrossRepoMember, trace.Workgroup) {
	t.Helper()
	f := newTwoProjectFixture(t)
	var members []trace.CrossRepoMember
	for _, p := range f.cfg.Projects {
		r, err := trace.Open(f.cfg.Root, p)
		must(t, err)
		t.Cleanup(func() { r.Close() })
		members = append(members, trace.CrossRepoMember{Project: r, Workstream: f.streams[p.ID]})
	}
	chief, group, err := trace.CreateWorkgroupTrace(context.Background(), f.cfg.Root, members, demoStart, ownerActor, "fixture")
	must(t, err)
	t.Cleanup(func() { chief.Close() })
	for _, m := range members {
		must(t, m.Project.JoinCrossRepoWorkgroup(context.Background(), m.Workstream, group.ID, demoStart, ownerActor, "fixture"))
	}
	store, _, err := runtime.Open(runtime.Inputs{Config: f.cfg, Workstreams: f.streamsAsLists()})
	must(t, err)
	t.Cleanup(func() { store.Close() })
	s := &Service{cfg: f.cfg, options: f.opts, store: store, pool: &scheduler.Shared{}}
	for _, m := range members {
		s.projects = append(s.projects, &activeProject{id: m.Project.Project(), repository: m.Project})
		must(t, s.pool.Join(m.Project))
	}
	return s, members, group
}

func (f *twoProjectFixture) streamsAsLists() map[config.ProjectID][]config.WorkstreamID {
	result := map[config.ProjectID][]config.WorkstreamID{}
	for project, stream := range f.streams {
		result[project] = []config.WorkstreamID{stream}
	}
	return result
}

func TestCrossProjectWorkgroupPauseHoldsOnlyItsProjectMember(t *testing.T) {
	t.Parallel()
	s, members, _ := crossProjectAdmissionFixture(t)
	must(t, s.store.SetPause(runtime.Pause{Target: runtime.Target{Scope: "project", Project: project}, Mode: "soft", Source: runtime.PauseOwner, Reason: "Owner pause"}))
	for i, m := range members {
		d, err := s.dispatcher(s.cfg.For(m.Project.Project()), m.Project)
		must(t, err)
		must(t, d.Pass(context.Background()))
		ops, err := m.Project.Operations(m.Workstream)
		must(t, err)
		if (len(ops) > 0) != (i == 1) {
			t.Fatalf("member %s dispatches while A paused: %+v", m.Project.Project(), ops)
		}
	}
}

func TestCrossProjectWorkgroupCapacityCountsEachMemberInItsProject(t *testing.T) {
	t.Parallel()
	s, members, _ := crossProjectAdmissionFixture(t)
	s.cfg.Capacity.Masons = 8
	for i := range s.cfg.Projects {
		s.cfg.Projects[i].Capacity.PerWorkstream = 1
	}
	a, b := members[0], members[1]
	// A's claimed turn consumes only A's member allowance.
	_, err := a.Project.ClaimTurn(context.Background(), a.Workstream, fixturekit.DemoAgent, "capacity-held", t.TempDir(), s.now())
	must(t, err)
	fixturekit.QueueRoleTurn(t, a.Project, a.Workstream, "second-mason", "mason", "", demoStart)
	for _, m := range members {
		d, err := s.dispatcher(s.cfg.For(m.Project.Project()), m.Project)
		must(t, err)
		must(t, d.Pass(context.Background()))
	}
	aOps, err := a.Project.Operations(a.Workstream)
	must(t, err)
	bOps, err := b.Project.Operations(b.Workstream)
	must(t, err)
	if len(aOps) != 0 || len(bOps) != 1 {
		t.Fatalf("capacity crossed project boundary: A=%+v B=%+v", aOps, bOps)
	}
	// B's admitted work cannot consume A's spare allowance after A is raised.
	s.cfg.Projects[0].Capacity.PerWorkstream = 2
	d, err := s.dispatcher(s.cfg.For(a.Project.Project()), a.Project)
	must(t, err)
	must(t, d.Pass(context.Background()))
	aOps, err = a.Project.Operations(a.Workstream)
	must(t, err)
	if len(aOps) != 1 {
		t.Fatalf("B's work charged to A: %+v", aOps)
	}
}

func TestCrossProjectWorkgroupGitPermitsStayWithTheirProject(t *testing.T) {
	t.Parallel()
	s, members, _ := crossProjectAdmissionFixture(t)
	a, b := members[0].Project, members[1].Project
	for i := range s.cfg.Projects {
		s.cfg.Projects[i].Capacity.GitOperations = 1
	}
	aLimit := s.gitOperationLimit(a, nil)
	bLimit := s.gitOperationLimit(b, nil)
	op := coreadapter.Operation{Boundary: coreadapter.RepositoryBoundary, Action: LandAction}
	releaseA, ok := aLimit(op)
	if !ok {
		t.Fatal("A refused its first permit")
	}
	defer releaseA()
	if release, ok := aLimit(op); ok {
		release()
		t.Fatal("A exceeded its permits")
	}
	releaseB, ok := bLimit(op)
	if !ok {
		t.Fatal("A's saturation held B")
	}
	releaseB()
	releaseB()
	if _, ok := aLimit(op); ok {
		t.Fatal("B's release freed A's permit")
	}
	releaseA()
	releaseA()
	releaseA2, ok := aLimit(op)
	if !ok {
		t.Fatal("A did not resume after release")
	}
	releaseA2()
	releaseB2, ok := bLimit(op)
	if !ok {
		t.Fatal("B did not release its permit")
	}
	releaseB2()
}

func TestProjectRemovalNamesUnresolvedWorkgroupAndAllowsResolvedMember(t *testing.T) {
	t.Parallel()
	s, members, group := crossProjectAdmissionFixture(t)
	// A retained conversation address locates the group without being a member.
	origin := config.WorkstreamID("w_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
	must(t, members[0].Project.CreateWorkstream(context.Background(), origin, demoStart, ownerActor))
	must(t, members[0].Project.RecordCrossRepoWorkgroup(context.Background(), origin, group.ID, demoStart, ownerActor, "conversation"))
	api := s.projectRemovalGate(project)
	if api == nil || !strings.Contains(api.Message, string(group.ID)) {
		t.Fatalf("unresolved removal gate: %+v", api)
	}
	m := members[0]
	state, err := m.Project.Workflow(m.Workstream, trace.FeatureSubject)
	must(t, err)
	_, err = m.Project.Transact(context.Background(), trace.Transaction{ExpectedVersion: state.Version, Transition: trace.Transition{Header: trace.Header{Schema: "osmia.trace.transition", Version: trace.Version, ID: "cancel-project-member", Revision: 1, Project: project, Workstream: m.Workstream, At: demoStart, Actor: ownerActor, Cause: "owner"}, Subject: trace.FeatureSubject, From: state.Value, To: AbandonedState, Reason: "owner cancelled member"}})
	must(t, err)
	if api := s.projectRemovalGate(project); api != nil {
		t.Fatalf("resolved member refused: %+v", api)
	}
	if api := s.projectRemovalGate(members[1].Project.Project()); api == nil {
		t.Fatal("other unresolved member lost its gate")
	}
}

func TestRemoveProjectRefusesUnresolvedMemberThenRemovesResolvedProject(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newTwoProjectFixture(t)
	var members []trace.CrossRepoMember
	for _, p := range f.cfg.Projects {
		r, err := trace.Open(f.cfg.Root, p)
		must(t, err)
		members = append(members, trace.CrossRepoMember{Project: r, Workstream: f.streams[p.ID]})
	}
	chief, group, err := trace.CreateWorkgroupTrace(ctx, f.cfg.Root, members, demoStart, ownerActor, "fixture")
	must(t, err)
	for _, m := range members {
		must(t, m.Project.JoinCrossRepoWorkgroup(ctx, m.Workstream, group.ID, demoStart, ownerActor, "fixture"))
	}
	must(t, chief.Close())
	for _, m := range members {
		must(t, m.Project.Close())
	}
	f.opts.Threads = nil
	s, err := Start(ctx, f.opts)
	must(t, err)
	defer s.Close()
	_, api := s.removeProject(ProjectRemoveRequest{Project: project})
	if api == nil || !strings.Contains(api.Message, string(group.ID)) {
		t.Fatalf("unresolved project removal: %+v", api)
	}
	r, err := s.repository(project)
	must(t, err)
	state, err := r.Workflow(f.streams[project], trace.FeatureSubject)
	must(t, err)
	_, err = r.Transact(ctx, trace.Transaction{ExpectedVersion: state.Version, Transition: trace.Transition{Header: trace.Header{Schema: "osmia.trace.transition", Version: trace.Version, ID: "owner-cancel-project-member", Revision: 1, Project: project, Workstream: f.streams[project], At: demoStart, Actor: ownerActor, Cause: "owner"}, Subject: trace.FeatureSubject, From: state.Value, To: AbandonedState, Reason: "owner cancelled member"}})
	must(t, err)
	if _, api := s.removeProject(ProjectRemoveRequest{Project: project}); api != nil {
		t.Fatalf("resolved project removal: %+v", api)
	}
	if s.current().Active(project) || !s.current().Active(otherProject) {
		t.Fatal("removal affected the wrong project")
	}
}
