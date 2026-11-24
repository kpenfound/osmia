package lifecycle_test

import (
	"context"
	"encoding/json"
	"reflect"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/intake"
	"github.com/kpenfound/osmia/internal/lifecycle"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	"github.com/kpenfound/osmia/internal/trace"
)

const idleSpecUnit = "Shared behavior of the workgroup, repeated to give the approved project spec its size. "

func idleSpec(g trace.Workgroup, size int) trace.ProjectSpec {
	real := g.Members[1:]
	spec := trace.ProjectSpec{
		Behavior:  strings.Repeat(idleSpecUnit, size/len(idleSpecUnit)+1),
		Contracts: []trace.Contract{{Name: "api", Text: "the shared interface"}},
	}
	kinds := []string{trace.EdgeHardBlock, trace.EdgeMergeOrder, trace.EdgeContract}
	for i, member := range real {
		spec.Slices = append(spec.Slices, trace.Slice{Workstream: member, Spec: "slice of " + string(member), Plan: "plan of " + string(member), ContractCriteria: []int{1}})
		if i > 0 {
			spec.Graph = append(spec.Graph, trace.Edge{From: real[i-1], To: member, Type: kinds[i%3]})
		}
	}
	return spec
}

func (f *changePassFixture) idleGroup(t *testing.T, locals []config.WorkstreamID, specBytes int) trace.Workgroup {
	t.Helper()
	ctx := context.Background()
	members := []trace.CrossRepoMember{{Workstream: otherStream, Project: f.remote}}
	for _, stream := range locals {
		members = append(members, trace.CrossRepoMember{Workstream: stream, Project: f.local})
	}
	wt, g, err := trace.CreateWorkgroupTrace(ctx, f.controller.Current().Root, members, fixturekit.DemoStart, intake.OwnerActor, "fixture")
	fixturekit.Must(t, err)
	for _, member := range members {
		fixturekit.Must(t, member.Project.JoinCrossRepoWorkgroup(ctx, member.Workstream, g.ID, fixturekit.DemoStart, intake.OwnerActor, "fixture"))
	}
	approval := trace.GroupApproval{Workgroup: g.ID, ProjectSpec: idleSpec(g, specBytes), Complete: true}
	for _, member := range members {
		approval.Members = append(approval.Members, trace.GroupMemberApproval{Project: member.Project.Project(), Workstream: member.Workstream, Record: json.RawMessage(`{}`)})
	}
	fixturekit.Must(t, trace.RecordGroupApproval(ctx, wt, g.Members[0], approval, fixturekit.DemoStart, intake.OwnerActor))
	fixturekit.Must(t, wt.Close())
	return g
}

func (f *changePassFixture) repositoryOf(stream config.WorkstreamID) *trace.Repository {
	if stream == otherStream {
		return f.remote
	}
	return f.local
}

func (f *changePassFixture) setFeature(t *testing.T, stream config.WorkstreamID, state string) {
	t.Helper()
	repo := f.repositoryOf(stream)
	h := trace.Header{Schema: "osmia.trace.transition", Version: trace.Version, ID: "fixture-" + state, Revision: 1, Project: repo.Project(), Workstream: stream, At: fixturekit.DemoStart, Actor: intake.OwnerActor, Cause: "fixture"}
	_, err := repo.SetFeatureState(context.Background(), h, state, "fixture")
	fixturekit.Must(t, err)
}

func (f *changePassFixture) feature(t *testing.T, stream config.WorkstreamID) string {
	t.Helper()
	states, err := f.repositoryOf(stream).WorkflowStates(stream)
	fixturekit.Must(t, err)
	return states[trace.FeatureSubject].Value
}

func (f *changePassFixture) recordResolved(t *testing.T, g trace.Workgroup) {
	t.Helper()
	wt, err := trace.OpenWorkgroupAuthority(f.controller.Current().Root, g.ID)
	fixturekit.Must(t, err)
	_, err = trace.MarkWorkgroupsResolved(context.Background(), wt, []trace.Workgroup{{ID: g.ID, Members: g.Members[1:]}}, func(config.WorkstreamID) (bool, error) { return true, nil }, fixturekit.DemoStart, intake.OwnerActor, "fixture")
	fixturekit.Must(t, err)
	fixturekit.Must(t, wt.Close())
}

func priorIncomingMergeEdges(repo *trace.Repository, stream config.WorkstreamID) ([]trace.Edge, error) {
	approved, err := repo.GroupApproved(stream)
	if err != nil || !approved {
		return nil, err
	}
	spec, _, found, err := repo.ProjectSpecOf(stream)
	if err != nil || !found {
		return nil, err
	}
	var out []trace.Edge
	for _, e := range spec.Graph {
		if e.To == stream && (e.Type == trace.EdgeHardBlock || e.Type == trace.EdgeMergeOrder) {
			out = append(out, e)
		}
	}
	return out, nil
}

type workgroupReads struct{ approvals, specs, lookups, acquisitions uint64 }

func readsOf(id config.WorkgroupID) workgroupReads {
	return workgroupReads{trace.GroupApprovalDecodeCount(id), trace.ProjectSpecDecodeCount(id), trace.CrossRepoProjectSpecReadCount(id), trace.WorkgroupTraceOpenCount(id)}
}

func edgeCallers(t *testing.T, repo *trace.Repository, stream config.WorkstreamID) {
	t.Helper()
	if edges, err := lifecycle.IncomingMergeEdges(repo, stream); err != nil || len(edges) != 0 {
		t.Fatalf("terminal member %s edges = %v, %v", stream, edges, err)
	}
	if waiting, err := lifecycle.EdgeWaiting(repo, stream); err != nil || waiting {
		t.Fatalf("terminal member %s waiting = %v, %v", stream, waiting, err)
	}
	if display, err := lifecycle.EdgeWaitDisplay(repo, stream); err != nil || display != "" {
		t.Fatalf("terminal member %s display = %q, %v", stream, display, err)
	}
	if references, err := lifecycle.PendingMergeOrderReferences(repo, stream); err != nil || len(references) != 0 {
		t.Fatalf("terminal member %s references = %v, %v", stream, references, err)
	}
	if _, waiting, err := lifecycle.UnresolvedMergeOrderEdge(repo, stream); err != nil || waiting {
		t.Fatalf("terminal member %s merge order wait = %v, %v", stream, waiting, err)
	}
	if resolved, err := lifecycle.WorkgroupMergeOrderResolved(repo, stream); err != nil || resolved != "" {
		t.Fatalf("terminal member %s merge order resolved = %q, %v", stream, resolved, err)
	}
}

func TestTerminalMembersOfAResolvedWorkgroupReadNeitherApprovalNorProjectSpec(t *testing.T) {
	t.Parallel()
	f := newChangePassFixture(t)
	ctx := context.Background()
	locals := []config.WorkstreamID{passFirst, passSecond, passThird}
	g := f.idleGroup(t, locals, 120_000)
	real := g.Members[1:]
	for _, stream := range real {
		f.setFeature(t, stream, intake.BuildingState)
	}
	for _, stream := range real {
		want, err := priorIncomingMergeEdges(f.repositoryOf(stream), stream)
		fixturekit.Must(t, err)
		got, err := lifecycle.IncomingMergeEdges(f.repositoryOf(stream), stream)
		fixturekit.Must(t, err)
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("building member %s edges = %v, want %v", stream, got, want)
		}
	}
	if edges, _ := lifecycle.IncomingMergeEdges(f.local, passFirst); len(edges) != 1 || edges[0].Type != trace.EdgeMergeOrder || edges[0].From != otherStream {
		t.Fatalf("first member edges = %v", edges)
	}
	if edges, _ := lifecycle.IncomingMergeEdges(f.local, passSecond); len(edges) != 0 {
		t.Fatalf("a contract edge was returned: %v", edges)
	}
	if edges, _ := lifecycle.IncomingMergeEdges(f.local, passThird); len(edges) != 1 || edges[0].Type != trace.EdgeHardBlock || edges[0].From != passSecond {
		t.Fatalf("third member edges = %v", edges)
	}
	if waiting, err := lifecycle.EdgeWaiting(f.local, passThird); err != nil || !waiting {
		t.Fatalf("third member waiting = %v, %v", waiting, err)
	}
	if waiting, err := lifecycle.EdgeWaiting(f.local, passFirst); err != nil || waiting {
		t.Fatalf("first member waiting = %v, %v (a merge_order edge never blocks building)", waiting, err)
	}
	if display, err := lifecycle.EdgeWaitDisplay(f.local, passFirst); err != nil || display != lifecycle.EdgeWaitMessage(string(otherStream)) {
		t.Fatalf("first member display = %q, %v", display, err)
	}
	if references, err := lifecycle.PendingMergeOrderReferences(f.local, passFirst); err != nil || !slices.Equal(references, []string{string(otherStream)}) {
		t.Fatalf("first member references = %v, %v", references, err)
	}

	f.setFeature(t, passFirst, intake.AbandonedState)
	f.setFeature(t, passSecond, intake.AbandonedState)
	if trace.WorkgroupResolutionCurrent(g.ID) {
		t.Fatal("resolution was current before it was recorded")
	}
	if edges, _ := lifecycle.IncomingMergeEdges(f.local, passFirst); len(edges) != 1 {
		t.Fatalf("abandoned member of an unresolved group lost its edges: %v", edges)
	}
	f.recordResolved(t, g)
	if !trace.WorkgroupResolutionCurrent(g.ID) {
		t.Fatal("resolution is not current after it was recorded")
	}

	if edges, err := lifecycle.IncomingMergeEdges(f.local, passThird); err != nil || len(edges) != 1 || edges[0].Type != trace.EdgeHardBlock {
		t.Fatalf("non-terminal member edges = %v, %v", edges, err)
	}
	if waiting, err := lifecycle.EdgeWaiting(f.local, passThird); err != nil || !waiting {
		t.Fatalf("non-terminal member waiting = %v, %v", waiting, err)
	}
	before := readsOf(g.ID)
	for range 40 {
		edgeCallers(t, f.local, passFirst)
		edgeCallers(t, f.local, passSecond)
	}
	if after := readsOf(g.ID); after != before {
		t.Fatalf("terminal members read the approval or project spec: %+v -> %+v", before, after)
	}

	f.setFeature(t, passThird, intake.AbandonedState)
	f.setFeature(t, otherStream, intake.AbandonedState)
	before = readsOf(g.ID)
	for range 40 {
		for _, stream := range locals {
			edgeCallers(t, f.local, stream)
		}
		edgeCallers(t, f.remote, otherStream)
		fixturekit.Must(t, f.controller.EdgeWaitPass(ctx, f.local))
		fixturekit.Must(t, f.controller.EdgeWaitPass(ctx, f.remote))
	}
	if after := readsOf(g.ID); after != before {
		t.Fatalf("a resolved workgroup's passes read the approval or project spec: %+v -> %+v", before, after)
	}
	for _, stream := range real {
		if got := f.feature(t, stream); got != intake.AbandonedState {
			t.Fatalf("member %s is %q", stream, got)
		}
	}
}

type idleWorkgroup struct {
	f       *changePassFixture
	g       trace.Workgroup
	locals  []config.WorkstreamID
	resolve *lifecycle.WorkgroupResolver
}

func newIdleWorkgroup(t *testing.T, specBytes int) *idleWorkgroup {
	t.Helper()
	f := newChangePassFixtureWith(t, 6)
	locals := append([]config.WorkstreamID{passFirst, passSecond, passThird}, f.extra...)
	g := f.idleGroup(t, locals, specBytes)
	for _, stream := range g.Members[1:] {
		f.setFeature(t, stream, intake.BuildingState)
	}
	return &idleWorkgroup{f: f, g: g, locals: locals, resolve: &lifecycle.WorkgroupResolver{S: f.controller, Repository: f.local}}
}

func (w *idleWorkgroup) schedulerPass(t *testing.T) {
	ctx := context.Background()
	if err := w.f.controller.EdgeWaitPass(ctx, w.f.local); err != nil {
		t.Fatal(err)
	}
	if err := w.f.controller.ProjectSpecRefreshPass(ctx, w.f.local); err != nil {
		t.Fatal(err)
	}
	if err := w.resolve.Pass(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := w.f.controller.CrossRepoMemberRepos(ctx, w.g); err != nil {
		t.Fatal(err)
	}
	for _, stream := range w.locals {
		if _, err := lifecycle.EdgeWaiting(w.f.local, stream); err != nil {
			t.Fatal(err)
		}
		if _, err := lifecycle.WorkgroupMergeOrderResolved(w.f.local, stream); err != nil {
			t.Fatal(err)
		}
	}
	for _, stream := range w.locals {
		if _, err := w.f.local.WorkflowStates(stream); err != nil {
			t.Fatal(err)
		}
	}
}

func (w *idleWorkgroup) observerPoll(t *testing.T) {
	ctx := context.Background()
	if err := w.f.controller.ChangePass(ctx, w.f.local); err != nil {
		t.Fatal(err)
	}
}

func bytesPerRun(run func(), warm, rounds int) float64 {
	for range warm {
		run()
	}
	runtime.GC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	for range rounds {
		run()
	}
	runtime.ReadMemStats(&after)
	return float64(after.TotalAlloc-before.TotalAlloc) / float64(rounds)
}

var idleSpecSizes = []int{40_000, 100_000, 320_000}

func TestIdleWorkgroupPassesDoNotGrowWithProjectSpecSize(t *testing.T) {
	const slack = 32 << 10
	scheduler, observer := make([]float64, len(idleSpecSizes)), make([]float64, len(idleSpecSizes))
	for i, size := range idleSpecSizes {
		w := newIdleWorkgroup(t, size)
		scheduler[i] = bytesPerRun(func() { w.schedulerPass(t) }, 5, 20)
		observer[i] = bytesPerRun(func() { w.observerPoll(t) }, 5, 20)
		t.Logf("project spec %d bytes: scheduler pass %.0f bytes, observer poll %.0f bytes", size, scheduler[i], observer[i])
	}
	last := len(idleSpecSizes) - 1
	grown := float64(idleSpecSizes[last] - idleSpecSizes[0])
	if scheduler[last]-scheduler[0] > slack {
		t.Fatalf("a scheduler pass allocates %.0f more bytes at %d bytes of project spec than at %d (spec grew %.0f)", scheduler[last]-scheduler[0], idleSpecSizes[last], idleSpecSizes[0], grown)
	}
	if observer[last]-observer[0] > slack {
		t.Fatalf("an observer poll allocates %.0f more bytes at %d bytes of project spec than at %d (spec grew %.0f)", observer[last]-observer[0], idleSpecSizes[last], idleSpecSizes[0], grown)
	}
}
