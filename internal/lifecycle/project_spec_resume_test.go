package lifecycle_test

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kpenfound/agentcore/agent"
	"github.com/kpenfound/osmia/internal/api"
	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/intake"
	"github.com/kpenfound/osmia/internal/lifecycle"
	"github.com/kpenfound/osmia/internal/plan"
	"github.com/kpenfound/osmia/internal/runtime"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	"github.com/kpenfound/osmia/internal/servicetest/intakefixture"
	"github.com/kpenfound/osmia/internal/servicetest/journey"
	"github.com/kpenfound/osmia/internal/trace"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const refusalNotice = "The workgroup spec could not be drafted"

var errHoldDispatch = errors.New("hold the draft before its turn runs")

type draftView struct {
	state       trace.WorkflowState
	input       lifecycle.ProjectSpecDraftInput
	revision    int
	failures    int
	turns       []trace.QueuedTurn
	transitions []trace.Transition
}

func (v draftView) turnIDs() []string {
	var ids []string
	for _, q := range v.turns {
		ids = append(ids, q.Request.TurnID)
	}
	return ids
}

func (v draftView) drafting() []trace.Transition {
	var out []trace.Transition
	for _, tr := range v.transitions {
		if tr.To == "drafting" {
			out = append(out, tr)
		}
	}
	return out
}

func (v draftView) turn(t *testing.T, id string) trace.QueuedTurn {
	t.Helper()
	for _, q := range v.turns {
		if q.Request.TurnID == id {
			return q
		}
	}
	t.Fatalf("turn %s is not requested; turns are %v", id, v.turnIDs())
	return trace.QueuedTurn{}
}

func readDraft(t *testing.T, f *journey.CrossRepoRatifyFixture, id config.WorkgroupID) draftView {
	t.Helper()
	wt, err := trace.OpenWorkgroupAuthority(f.S.Current().Root, id)
	fixturekit.Must(t, err)
	defer wt.Close()
	groups, _, err := trace.Workgroups(wt)
	fixturekit.Must(t, err)
	host := groups[0].Members[0]
	var v draftView
	v.state, err = wt.Workflow(host, lifecycle.ProjectSpecDraftSubject)
	fixturekit.Must(t, err)
	v.input, v.revision, err = lifecycle.ReadProjectSpecDraftInput(wt, host)
	fixturekit.Must(t, err)
	v.failures, _, err = lifecycle.ProjectSpecDraftFailures(wt, host)
	fixturekit.Must(t, err)
	th, err := wt.Thread(host, intake.ArchitectAgent)
	fixturekit.Must(t, err)
	v.turns = th.Turns
	all, err := trace.Read[trace.Transition](wt, host)
	fixturekit.Must(t, err)
	for _, tr := range all {
		if tr.Subject == lifecycle.ProjectSpecDraftSubject {
			v.transitions = append(v.transitions, tr)
		}
	}
	return v
}

func stoppedByService(q trace.QueuedTurn) bool {
	return q.Response != nil && q.Status() == "interrupted" && q.Response.Result.Cancelled && q.Response.Stop == nil
}

func pinnedLikeBefore(t *testing.T, before, after lifecycle.ProjectSpecDraftInput) {
	t.Helper()
	if after.Turn == before.Turn {
		t.Fatalf("replacement reuses turn %s", before.Turn)
	}
	before.Turn, after.Turn = "", ""
	if !reflect.DeepEqual(before, after) {
		t.Fatalf("replacement changed the pinned input:\nbefore %+v\nafter  %+v", before, after)
	}
}

func scriptServiceStop(f *journey.CrossRepoRatifyFixture, turn string, stop func()) {
	f.Engine.Mu.Lock()
	defer f.Engine.Mu.Unlock()
	f.Engine.Turns[turn] = func(ctx context.Context, _ agent.Request, _ *agent.Turn, _ *mcp.ClientSession) (*agent.Result, error) {
		stop()
		<-ctx.Done()
		return nil, ctx.Err()
	}
}

func stopServiceDuring(t *testing.T, f *journey.CrossRepoRatifyFixture, turn string) func() {
	t.Helper()
	reached := make(chan struct{})
	var once sync.Once
	scriptServiceStop(f, turn, func() { once.Do(func() { close(reached) }) })
	return func() {
		t.Helper()
		select {
		case <-reached:
		case <-time.After(fixturekit.DemoTimeout):
			t.Fatalf("%s did not start", turn)
		}
		haltGuardControllers(t, f)
	}
}

func stopPassDuring(t *testing.T, f *journey.CrossRepoRatifyFixture, turn string) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	scriptServiceStop(f, turn, cancel)
	for _, project := range []config.ProjectID{f.Project, f.Project2} {
		_ = f.S.Lifecycle().ProjectSpecDraftPass(ctx, mustRepo(t, f.S, project))
	}
}

func draftPass(t *testing.T, f *journey.CrossRepoRatifyFixture) {
	t.Helper()
	for _, project := range []config.ProjectID{f.Project, f.Project2} {
		fixturekit.Must(t, f.S.Lifecycle().ProjectSpecDraftPass(context.Background(), mustRepo(t, f.S, project)))
	}
}

func requestOnlyPass(t *testing.T, f *journey.CrossRepoRatifyFixture) {
	t.Helper()
	f.S.Boundary = func(step string) error {
		if step == "project-spec-draft-requested" {
			return errHoldDispatch
		}
		return nil
	}
	defer f.S.ClearBoundary()
	for _, project := range []config.ProjectID{f.Project, f.Project2} {
		_ = f.S.Lifecycle().ProjectSpecDraftPass(context.Background(), mustRepo(t, f.S, project))
	}
}

func deliverValidDraft(f *journey.CrossRepoRatifyFixture, turn string, calls *atomic.Int32) {
	scriptProjectSpecTurn(f, turn, func(ctx context.Context, _ agent.Request, tools *mcp.ClientSession) error {
		calls.Add(1)
		_, err := fixturekit.CallTool(ctx, tools, lifecycle.ProjectSpecDraftTool, validProjectSpecDraft(f))
		return err
	})
}

func TestProjectSpecDraftResumesAfterServiceStop(t *testing.T) {
	t.Parallel()
	f := newCrossRepoRatifyFixture(t)
	defer f.Stop(t)
	stopped := stopServiceDuring(t, f, "project-spec-draft-1")
	id, apiFault := f.S.Lifecycle().DraftWorkgroup(context.Background(), []config.WorkstreamID{f.Stream1, f.Stream2})
	if apiFault != nil {
		t.Fatal(apiFault)
	}
	stopped()
	before := readDraft(t, f, id)
	if before.state.Value != "drafting" || !reflect.DeepEqual(before.turnIDs(), []string{"project-spec-draft-1"}) || !stoppedByService(before.turn(t, "project-spec-draft-1")) {
		t.Fatalf("service stop did not leave an interrupted drafting turn: %q %v", before.state.Value, before.turnIDs())
	}
	var calls atomic.Int32
	deliverValidDraft(f, "project-spec-draft-2", &calls)
	requestOnlyPass(t, f)
	held := readDraft(t, f, id)
	if held.state.Value != "drafting" || held.input.Turn != "project-spec-draft-2" || held.revision != before.revision+1 || calls.Load() != 0 {
		t.Fatalf("first pass did not request the replacement turn: %q %+v", held.state.Value, held.input)
	}
	pinnedLikeBefore(t, before.input, held.input)
	draftPass(t, f)
	after := readDraft(t, f, id)
	if calls.Load() != 1 || after.state.Value != "drafted" || !projectSpecRecorded(t, f, id) || !reflect.DeepEqual(after.turnIDs(), []string{"project-spec-draft-1", "project-spec-draft-2"}) {
		t.Fatalf("replacement draft not recorded: calls=%d state=%q turns=%v", calls.Load(), after.state.Value, after.turnIDs())
	}
	pinnedLikeBefore(t, before.input, after.input)
}

func TestProjectSpecDraftRepeatsAfterEachServiceStop(t *testing.T) {
	t.Parallel()
	f := newCrossRepoRatifyFixture(t)
	defer f.Stop(t)
	stopped := stopServiceDuring(t, f, "project-spec-draft-1")
	id, apiFault := f.S.Lifecycle().DraftWorkgroup(context.Background(), []config.WorkstreamID{f.Stream1, f.Stream2})
	if apiFault != nil {
		t.Fatal(apiFault)
	}
	stopped()
	first := readDraft(t, f, id)
	if first.state.Value != "drafting" || !stoppedByService(first.turn(t, "project-spec-draft-1")) {
		t.Fatalf("first stop left %q", first.state.Value)
	}
	stopPassDuring(t, f, "project-spec-draft-2")
	second := readDraft(t, f, id)
	if second.state.Value != "drafting" || !reflect.DeepEqual(second.turnIDs(), []string{"project-spec-draft-1", "project-spec-draft-2"}) || !stoppedByService(second.turn(t, "project-spec-draft-2")) {
		t.Fatalf("replacement was not interrupted: %q %v", second.state.Value, second.turnIDs())
	}
	var calls atomic.Int32
	deliverValidDraft(f, "project-spec-draft-3", &calls)
	requestOnlyPass(t, f)
	held := readDraft(t, f, id)
	if held.input.Turn != "project-spec-draft-3" || len(held.drafting()) != 3 {
		t.Fatalf("one interruption did not lead to exactly one new turn: %+v, %d drafting transitions", held.input, len(held.drafting()))
	}
	pinnedLikeBefore(t, first.input, held.input)
	draftPass(t, f)
	draftPass(t, f)
	after := readDraft(t, f, id)
	if calls.Load() != 1 || after.state.Value != "drafted" || !projectSpecRecorded(t, f, id) || len(after.drafting()) != 3 || !reflect.DeepEqual(after.turnIDs(), []string{"project-spec-draft-1", "project-spec-draft-2", "project-spec-draft-3"}) {
		t.Fatalf("draft not recorded after the second interruption: calls=%d state=%q turns=%v", calls.Load(), after.state.Value, after.turnIDs())
	}
}

func TestProjectSpecDraftResumeLeavesNoOwnerOrManagerAction(t *testing.T) {
	t.Parallel()
	f := newCrossRepoRatifyFixture(t)
	defer f.Stop(t)
	stopped := stopServiceDuring(t, f, "project-spec-draft-1")
	id, apiFault := f.S.Lifecycle().DraftWorkgroup(context.Background(), []config.WorkstreamID{f.Stream1, f.Stream2})
	if apiFault != nil {
		t.Fatal(apiFault)
	}
	stopped()
	stuck := readDraft(t, f, id)
	if stuck.state.Value != "drafting" || stuck.revision != 1 || len(stuck.transitions) != 1 || !stoppedByService(stuck.turn(t, "project-spec-draft-1")) {
		t.Fatalf("deploy state not reached: %q revision=%d", stuck.state.Value, stuck.revision)
	}
	var calls atomic.Int32
	deliverValidDraft(f, "project-spec-draft-2", &calls)
	draftPass(t, f)
	after := readDraft(t, f, id)
	if calls.Load() != 1 || after.state.Value != "drafted" || !reflect.DeepEqual(after.turnIDs(), []string{"project-spec-draft-1", "project-spec-draft-2"}) {
		t.Fatalf("first pass did not replace the stuck turn: calls=%d state=%q turns=%v", calls.Load(), after.state.Value, after.turnIDs())
	}
	for _, tr := range after.transitions {
		if tr.Actor != lifecycle.ProjectSpecDraftActor {
			t.Fatalf("transition %s was made by %+v, not by the service", tr.ID, tr.Actor)
		}
	}
	if got := leaderNotices(t, f, refusalNotice); len(got) != 0 {
		t.Fatalf("interruption reached the manager: %v", got)
	}
}

func TestProjectSpecRedraftResumesAfterServiceStop(t *testing.T) {
	t.Parallel()
	f, id, _ := projectSliceRefreshFixture(t)
	old := refreshDocument(t, f, id)
	spec := strings.Replace(intakefixture.CrossSpec1, "chunk's offset.", "chunk's byte offset.", 1)
	reviseRefreshMember(t, f, f.Stream1, plan.SpecDocument, spec)
	stopPassDuring(t, f, "project-spec-draft-3")
	before := readDraft(t, f, id)
	if before.state.Value != "drafting" || len(before.input.Guarded) == 0 || !stoppedByService(before.turn(t, "project-spec-draft-3")) {
		t.Fatalf("service stop did not interrupt the redraft: %q %+v", before.state.Value, before.input)
	}
	assertGuardRefusal(t, f, id, 2, "")
	var calls atomic.Int32
	scriptGuardDraft(t, f, "project-spec-draft-4", 2, spec, &calls)
	requestOnlyPass(t, f)
	held := readDraft(t, f, id)
	if held.input.Turn != "project-spec-draft-4" || held.state.Value != "drafting" {
		t.Fatalf("replacement redraft not requested: %q %+v", held.state.Value, held.input)
	}
	pinnedLikeBefore(t, before.input, held.input)
	runGuardDraft(t, f)
	runRefresh(t, f)
	if calls.Load() != 1 || refreshDocument(t, f, id).Revision != old.Revision+1 {
		t.Fatalf("redraft not recorded once: calls=%d", calls.Load())
	}
	assertGuardRatifiable(t, f, id, 2, old.Revision+1)
}

func TestProjectSpecDraftServiceStopKeepsRefusalHistory(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newCrossRepoRatifyFixture(t)
	defer f.Stop(t)
	var refused atomic.Int32
	refuse := func(context.Context, agent.Request, *mcp.ClientSession) error {
		refused.Add(1)
		return nil
	}
	for _, turn := range []string{"project-spec-draft-1", "project-spec-draft-3", "project-spec-draft-5"} {
		scriptProjectSpecTurn(f, turn, refuse)
	}
	id, apiFault := f.S.Lifecycle().DraftWorkgroup(ctx, []config.WorkstreamID{f.Stream1, f.Stream2})
	if apiFault != nil {
		t.Fatal(apiFault)
	}
	intakefixture.Eventually(t, "the manager is told drafting stopped", func() bool { return len(leaderNotices(t, f, refusalNotice)) == 1 })
	haltGuardControllers(t, f)

	note := "Cite no charter rule; neither charter numbers any."
	manager := trace.Actor{Kind: "agent", ID: trace.Manager}
	if _, apiFault := f.S.Lifecycle().MoveWorkstream(ctx, mustRepo(t, f.S, f.Project), f.Stream1, api.WorkstreamMoveRequest{To: intake.SketchedState, Note: note}, manager, "manager-turn-1"); apiFault != nil {
		t.Fatal(apiFault)
	}
	scriptProjectSpecTurn(f, "project-spec-draft-7", refuse)
	draftPass(t, f)
	refusal := readDraft(t, f, id)
	if refusal.failures != 1 || refusal.input.Note != note {
		t.Fatalf("requested draft was not refused once: failures=%d input=%+v", refusal.failures, refusal.input)
	}

	stopPassDuring(t, f, "project-spec-draft-9")
	interrupted := readDraft(t, f, id)
	if interrupted.state.Value != "drafting" || interrupted.failures != 1 || interrupted.input.Problems == "" || interrupted.input.Note != note || !stoppedByService(interrupted.turn(t, "project-spec-draft-9")) {
		t.Fatalf("retry was not interrupted with the refusal pinned: %q failures=%d input=%+v", interrupted.state.Value, interrupted.failures, interrupted.input)
	}
	requestOnlyPass(t, f)
	held := readDraft(t, f, id)
	if held.state.Value != "drafting" || held.input.Turn != "project-spec-draft-10" || len(held.drafting()) != len(interrupted.drafting())+1 {
		t.Fatalf("replacement not requested once: %q %+v", held.state.Value, held.input)
	}
	pinnedLikeBefore(t, interrupted.input, held.input)
	if held.input.Problems == "" || held.input.Note != note {
		t.Fatalf("replacement lost the refusal: %+v", held.input)
	}
	if held.failures != interrupted.failures {
		t.Fatalf("interruption changed the failure count from %d to %d", interrupted.failures, held.failures)
	}
	for _, tr := range held.transitions[len(interrupted.transitions):] {
		if tr.To == "failed" {
			t.Fatalf("interruption was recorded as a refusal: %+v", tr)
		}
	}
	if got := leaderNotices(t, f, refusalNotice); len(got) != 1 {
		t.Fatalf("interruption sent the manager a refusal notice: %d notices", len(got))
	}

	for _, turn := range []string{"project-spec-draft-10", "project-spec-draft-12", "project-spec-draft-14"} {
		scriptProjectSpecTurn(f, turn, refuse)
	}
	refused.Store(0)
	for range 5 {
		draftPass(t, f)
	}
	final := readDraft(t, f, id)
	if refused.Load() != lifecycle.ProjectSpecDraftAttempts-1 || final.failures != lifecycle.ProjectSpecDraftAttempts {
		t.Fatalf("limit not reached after %d refusals past the request: refused after the interruption=%d failures=%d turns=%v", lifecycle.ProjectSpecDraftAttempts, refused.Load(), final.failures, final.turnIDs())
	}
	if last := final.turnIDs()[len(final.turnIDs())-1]; last != "project-spec-draft-12" || final.state.Value != "failed" || projectSpecRecorded(t, f, id) {
		t.Fatalf("drafting continued past the limit: last turn %s, state %q", last, final.state.Value)
	}
	if got := leaderNotices(t, f, refusalNotice); len(got) != 2 || !strings.Contains(got[1], "last 3 drafts were refused") {
		t.Fatalf("the second run of refusals was not reported once: %v", got)
	}
}

func TestProjectSpecDraftOwnerStopIsNotAskedAgain(t *testing.T) {
	t.Parallel()
	f, id, _ := projectSliceRefreshFixture(t)
	reviseRefreshMember(t, f, f.Stream1, plan.SpecDocument, strings.Replace(intakefixture.CrossSpec1, "chunk's offset.", "chunk's byte offset.", 1))
	fixturekit.Must(t, f.S.Store.SetPause(runtime.Pause{Target: runtime.Target{Scope: "factory"}, Mode: "hard", Source: runtime.PauseOwner, Reason: "Stop now", SetAt: f.S.Now()}))
	var calls atomic.Int32
	deliverValidDraft(f, "project-spec-draft-3", &calls)
	deliverValidDraft(f, "project-spec-draft-4", &calls)
	for range 4 {
		for _, project := range []config.ProjectID{f.Project, f.Project2} {
			_ = f.S.Lifecycle().ProjectSpecDraftPass(context.Background(), mustRepo(t, f.S, project))
		}
	}
	v := readDraft(t, f, id)
	q := v.turn(t, "project-spec-draft-3")
	if q.Response == nil || q.Response.Stop == nil || stoppedByService(q) {
		t.Fatalf("the owner's stop did not end the turn: %+v", q.Response)
	}
	if !reflect.DeepEqual(v.turnIDs(), []string{"project-spec-draft-1", "project-spec-draft-3"}) || v.state.Value != "drafting" || calls.Load() != 0 {
		t.Fatalf("an owner-stopped draft was asked for again: state=%q turns=%v calls=%d", v.state.Value, v.turnIDs(), calls.Load())
	}
}
