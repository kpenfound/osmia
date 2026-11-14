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
	"github.com/kpenfound/osmia/internal/lifecycle"
	"github.com/kpenfound/osmia/internal/trace"
)

const idleSpecUnit = "Shared behavior of the workgroup, repeated to give the approved project spec its size. "

// idleSpec returns a project spec for g whose behavior is about size bytes. Its
// graph chains the real members and cycles through hard_block, merge_order and
// contract edges, starting with merge_order.
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

// idleGroup creates a cross-repo workgroup of otherStream and locals whose
// approval is complete and embeds a project spec of about specBytes, and
// leaves no handle on its trace open.
func (f *changePassFixture) idleGroup(t *testing.T, locals []config.WorkstreamID, specBytes int) trace.Workgroup {
	t.Helper()
	ctx := context.Background()
	members := []trace.CrossRepoMember{{Workstream: otherStream, Project: f.remote}}
	for _, stream := range locals {
		members = append(members, trace.CrossRepoMember{Workstream: stream, Project: f.local})
	}
	wt, g, err := trace.CreateWorkgroupTrace(ctx, f.controller.Current().Root, members, demoStart, ownerActor, "fixture")
	must(t, err)
	for _, member := range members {
		must(t, member.Project.JoinCrossRepoWorkgroup(ctx, member.Workstream, g.ID, demoStart, ownerActor, "fixture"))
	}
	approval := trace.GroupApproval{Workgroup: g.ID, ProjectSpec: idleSpec(g, specBytes), Complete: true}
	for _, member := range members {
		approval.Members = append(approval.Members, trace.GroupMemberApproval{Project: member.Project.Project(), Workstream: member.Workstream, Record: json.RawMessage(`{}`)})
	}
	must(t, trace.RecordGroupApproval(ctx, wt, g.Members[0], approval, demoStart, ownerActor))
	must(t, wt.Close())
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
	h := trace.Header{Schema: "osmia.trace.transition", Version: trace.Version, ID: "fixture-" + state, Revision: 1, Project: repo.Project(), Workstream: stream, At: demoStart, Actor: ownerActor, Cause: "fixture"}
	_, err := repo.SetFeatureState(context.Background(), h, state, "fixture")
	must(t, err)
}

func (f *changePassFixture) feature(t *testing.T, stream config.WorkstreamID) string {
	t.Helper()
	states, err := f.repositoryOf(stream).WorkflowStates(stream)
	must(t, err)
	return states[trace.FeatureSubject].Value
}

// recordResolved records the workgroup resolved in its own trace and releases
// the handle, leaving the in-process resolution hint set.
func (f *changePassFixture) recordResolved(t *testing.T, g trace.Workgroup) {
	t.Helper()
	wt, err := trace.OpenWorkgroupAuthority(f.controller.Current().Root, g.ID)
	must(t, err)
	_, err = trace.MarkWorkgroupsResolved(context.Background(), wt, []trace.Workgroup{{ID: g.ID, Members: g.Members[1:]}}, func(config.WorkstreamID) (bool, error) { return true, nil }, demoStart, ownerActor, "fixture")
	must(t, err)
	must(t, wt.Close())
}

// priorIncomingMergeEdges is IncomingMergeEdges as it read before terminal
// members were skipped: the approval answer, then the whole project spec.
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

// edgeCallers runs every caller of IncomingMergeEdges that the scheduling path
// uses, for one member.
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
	f := newChangePassFixture(t)
	ctx := context.Background()
	locals := []config.WorkstreamID{passFirst, passSecond, passThird}
	g := f.idleGroup(t, locals, 120_000)
	real := g.Members[1:]
	for _, stream := range real {
		f.setFeature(t, stream, BuildingState)
	}
	// Members: otherStream, first, second, third. The graph chains them with
	// merge_order into first, contract into second and hard_block into third.
	for _, stream := range real {
		want, err := priorIncomingMergeEdges(f.repositoryOf(stream), stream)
		must(t, err)
		got, err := lifecycle.IncomingMergeEdges(f.repositoryOf(stream), stream)
		must(t, err)
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

	// Abandoned members are read like any other until the group is recorded
	// resolved in this process.
	f.setFeature(t, passFirst, AbandonedState)
	f.setFeature(t, passSecond, AbandonedState)
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

	// The third member has not finished: it keeps its edges and its wait, and
	// the terminal members read neither document however often they are asked.
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

	f.setFeature(t, passThird, AbandonedState)
	f.setFeature(t, otherStream, AbandonedState)
	before = readsOf(g.ID)
	for range 40 {
		for _, stream := range locals {
			edgeCallers(t, f.local, stream)
		}
		edgeCallers(t, f.remote, otherStream)
		must(t, f.controller.EdgeWaitPass(ctx, f.local))
		must(t, f.controller.EdgeWaitPass(ctx, f.remote))
	}
	if after := readsOf(g.ID); after != before {
		t.Fatalf("a resolved workgroup's passes read the approval or project spec: %+v -> %+v", before, after)
	}
	for _, stream := range real {
		if got := f.feature(t, stream); got != AbandonedState {
			t.Fatalf("member %s is %q", stream, got)
		}
	}
}

// idleWorkgroup is a cross-repo workgroup of ten real members, all building,
// whose approved project spec is about specBytes.
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
		f.setFeature(t, stream, BuildingState)
	}
	return &idleWorkgroup{f: f, g: g, locals: locals, resolve: &lifecycle.WorkgroupResolver{S: f.controller, Repository: f.local}}
}

// schedulerPass runs the scheduler hooks the controller owns that touch a
// workgroup's approval or project spec: edge availability, project spec
// refresh, workgroup resolution, member binding and the edge reads the
// masons, final review and publication make for every member.
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

// observerPoll runs the workgroup-change observer.
func (w *idleWorkgroup) observerPoll(t *testing.T) {
	ctx := context.Background()
	if err := w.f.controller.ChangePass(ctx, w.f.local); err != nil {
		t.Fatal(err)
	}
}

// bytesPerRun reports the bytes the process allocates for each call of run
// once it is warm. The count is exact for the process, so it does not depend
// on how fast the machine is.
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

// idleSpecSizes are the project spec sizes the evidence compares: about 40 KB,
// 100 KB and over 300 KB.
var idleSpecSizes = []int{40_000, 100_000, 320_000}

// TestIdleWorkgroupPassesDoNotGrowWithProjectSpecSize measures the bytes a warm
// scheduler pass and a warm observer poll allocate over a ten-member workgroup
// at three project spec sizes. Decoding, marshalling or copying the approval
// or the spec on a pass would add its size to every pass; the measure is an
// exact allocation count, not a duration.
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
