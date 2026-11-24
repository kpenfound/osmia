package lifecycle_test

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/kpenfound/osmia/internal/execution"
	"github.com/kpenfound/osmia/internal/servicetest/journey"

	"github.com/kpenfound/osmia/internal/api"
	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/delivery"
	"github.com/kpenfound/osmia/internal/intake"
	"github.com/kpenfound/osmia/internal/lifecycle"
	"github.com/kpenfound/osmia/internal/runtime"
	"github.com/kpenfound/osmia/internal/seal"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	"github.com/kpenfound/osmia/internal/servicetest/intakefixture"
	"github.com/kpenfound/osmia/internal/trace"
)

const idleStream config.WorkstreamID = "w_0000000000000000000000000000d1f7"

func setClock(f *journey.ShedFixture, at time.Time) {
	f.Clock.Mu.Lock()
	defer f.Clock.Mu.Unlock()
	f.Clock.At = at
}

func cadenceForeman(f *journey.ShedFixture, repository *trace.Repository, interval string) *journey.Foreman {
	cfg := *f.S.Cfg
	cfg.Project.UpstreamRebase = interval
	return &journey.Foreman{Masons: &journey.Masons{S: f.S, Cfg: &cfg, Repository: repository}}
}

func passDrifts(t *testing.T, fm *journey.Foreman) {
	t.Helper()
	fm.NextDrift = time.Time{}
	fixturekit.Must(t, fm.Drifts(context.Background()))
}

func sealedAt(t *testing.T, repository *trace.Repository, stream config.WorkstreamID) time.Time {
	t.Helper()
	docs := intakefixture.StreamDocuments(t, repository, stream, seal.DocumentID)
	if len(docs) == 0 {
		t.Fatalf("workstream %s has no seal", stream)
	}
	return slices.MinFunc(docs, func(a, b trace.Document) int { return a.At.Compare(b.At) }).At
}

func driftTransition(t *testing.T, repository *trace.Repository, stream config.WorkstreamID) trace.Transition {
	t.Helper()
	states, err := repository.WorkflowStates(stream)
	fixturekit.Must(t, err)
	drift, err := lifecycle.LatestDrift(repository, stream, states)
	fixturekit.Must(t, err)
	if drift == nil {
		t.Fatalf("workstream %s has no drift rebase", stream)
	}
	return intakefixture.TransitionByID(t, repository, stream, driftSubjectTransition(drift))
}

func driftSubjectTransition(d *api.DriftStatus) string {
	transition, _ := lifecycle.DriftIDs(d.Drift)
	if d.Outcome == "requested" {
		return transition
	}
	return transition + "-" + d.Outcome
}

func checkDrifts(t *testing.T, repository *trace.Repository, stream config.WorkstreamID, want int, when string) []trace.OperationRecord {
	t.Helper()
	ops := journey.DriftOperations(t, repository, stream)
	if len(ops) != want {
		var asked []string
		for _, o := range ops {
			asked = append(asked, o.Transition.ID)
		}
		t.Fatalf("%s: drift rebases %v asked for, want %d", when, asked, want)
	}
	return ops
}

func TestScheduledDriftRebasesFollowTheIntervalAcrossRestart(t *testing.T) {
	t.Parallel()
	f, stream, repository, _ := journey.NewFinalFixtureWith(t, "cadence", "")
	ctx := context.Background()
	fixturekit.Must(t, repository.CreateWorkstream(ctx, idleStream, f.S.Now(), intake.OwnerActor))
	sealed := sealedAt(t, repository, stream)
	fm := cadenceForeman(f, repository, "1h")

	setClock(f, sealed.Add(59*time.Minute+30*time.Second))
	fixturekit.Must(t, fm.Drifts(ctx))
	checkDrifts(t, repository, stream, 0, "before the interval elapsed")
	setClock(f, sealed.Add(time.Hour))
	fixturekit.Must(t, fm.Drifts(ctx))
	checkDrifts(t, repository, stream, 0, "within a minute of the last read")
	setClock(f, sealed.Add(time.Hour+30*time.Second))
	fixturekit.Must(t, fm.Drifts(ctx))
	ops := checkDrifts(t, repository, stream, 1, "once the interval elapsed since the sealing")
	if reason := intakefixture.TransitionByID(t, repository, stream, "drift-1").Reason; !strings.HasPrefix(reason, "upstream_rebase 1h0m0s has elapsed since the sealing at "+sealed.Format(time.RFC3339)+": drift rebase 1 fetches") {
		t.Fatalf("scheduled drift rebase reason %q", reason)
	}
	setClock(f, sealed.Add(5*time.Hour))
	passDrifts(t, fm)
	checkDrifts(t, repository, stream, 1, "while the drift rebase runs")
	journey.SettleOperation(t, f.S, repository, stream, ops[0].Operation, journey.Drifter{Foreman: fm})
	first := driftTransition(t, repository, stream)
	if first.To != "rebased-1" {
		t.Fatalf("drift rebase 1 is %s", first.To)
	}

	repository = fixturekit.ReopenTrace(t, f.S.Cfg, repository)
	fm = cadenceForeman(f, repository, "1h")
	setClock(f, first.At.Add(59*time.Minute))
	passDrifts(t, fm)
	checkDrifts(t, repository, stream, 1, "after a restart within the interval since drift rebase 1")
	setClock(f, first.At.Add(time.Hour))
	passDrifts(t, fm)
	ops = checkDrifts(t, repository, stream, 2, "after a restart once the interval elapsed since drift rebase 1")
	if reason := intakefixture.TransitionByID(t, repository, stream, "drift-2").Reason; !strings.HasPrefix(reason, "upstream_rebase 1h0m0s has elapsed since the drift rebase 1 at "+first.At.Format(time.RFC3339)+":") {
		t.Fatalf("second scheduled drift rebase reason %q", reason)
	}
	repository = fixturekit.ReopenTrace(t, f.S.Cfg, repository)
	fm = cadenceForeman(f, repository, "1h")
	passDrifts(t, fm)
	checkDrifts(t, repository, stream, 2, "after a restart while drift rebase 2 runs")
	journey.SettleOperation(t, f.S, repository, stream, ops[1].Operation, journey.Drifter{Foreman: fm})
	second := driftTransition(t, repository, stream)

	finalAt := second.At.Add(30 * time.Minute)
	g, err := intake.FeatureWorkspaces(f.S.About(repository), repository).Of(stream)
	fixturekit.Must(t, err)
	tip, _, err := g.Branch(ctx, intake.FeatureBranch(stream))
	fixturekit.Must(t, err)
	base, err := lifecycle.BranchBase(ctx, repository, stream, g, tip)
	fixturekit.Must(t, err)
	content, err := json.Marshal(delivery.FinalRebase{Review: 1, Operation: "planted-final-review", Branch: intake.FeatureBranch(stream), Before: tip, Commit: tip, Upstream: base})
	fixturekit.Must(t, err)
	revision, err := intake.NextRevision(repository, stream, delivery.FinalRebaseDocument)
	fixturekit.Must(t, err)
	fixturekit.Must(t, repository.RecordDocuments(ctx, []trace.Document{{Header: trace.Header{Schema: "osmia.trace.document", Version: trace.Version, ID: delivery.FinalRebaseDocument, Revision: revision, Project: repository.Project(), Workstream: stream, At: finalAt, Actor: execution.ForemanActor, Cause: "planted-final-review"},
		Path: delivery.FinalRebasePath, Content: string(content) + "\n"}}))
	setClock(f, second.At.Add(time.Hour))
	passDrifts(t, fm)
	checkDrifts(t, repository, stream, 2, "within the interval since the final rebase")
	setClock(f, finalAt.Add(time.Hour))
	passDrifts(t, fm)
	ops = checkDrifts(t, repository, stream, 3, "once the interval elapsed since the final rebase")
	if reason := intakefixture.TransitionByID(t, repository, stream, "drift-3").Reason; !strings.HasPrefix(reason, "upstream_rebase 1h0m0s has elapsed since the final rebase at "+finalAt.Format(time.RFC3339)+":") {
		t.Fatalf("scheduled drift rebase after a final rebase reason %q", reason)
	}
	journey.SettleOperation(t, f.S, repository, stream, ops[2].Operation, journey.Drifter{Foreman: fm})

	for _, disabled := range []string{"0", "0s"} {
		fm = cadenceForeman(f, repository, disabled)
		setClock(f, finalAt.Add(100*time.Hour))
		passDrifts(t, fm)
		checkDrifts(t, repository, stream, 3, "with upstream_rebase "+disabled)
	}
	if ops, err := repository.Operations(idleStream); err != nil || len(ops) != 0 {
		t.Fatalf("a workstream that is not building has operations %+v: %v", ops, err)
	}
}

func TestOwnerDriftRequestWaitsForTheLanderAcrossRestart(t *testing.T) {
	t.Parallel()
	f, stream, repository, _ := journey.NewFinalFixtureWith(t, "asked", "")
	ctx := context.Background()
	sealed := sealedAt(t, repository, stream)
	fm := cadenceForeman(f, repository, "0")
	setClock(f, sealed.Add(100*time.Hour))
	passDrifts(t, fm)
	checkDrifts(t, repository, stream, 0, "with scheduled drift rebases disabled")

	held := journey.RequestDrift(t, journey.Drifter{Foreman: fm}, stream)
	for range 2 {
		if k, err := fm.DriftDriver().AskDrift(ctx, stream, f.S.Now()); err != nil || k != 2 {
			t.Fatalf("the owner's request while drift rebase 1 runs is answered by %d: %v", k, err)
		}
	}
	transitions, err := trace.Read[trace.Transition](repository, stream)
	fixturekit.Must(t, err)
	requests := slices.DeleteFunc(transitions, func(tr trace.Transition) bool { return tr.Subject != lifecycle.DriftRequestSubject })
	if len(requests) != 1 || requests[0].ID != "drift-request-2" || requests[0].To != "requested-2" || requests[0].Actor != intake.OwnerActor {
		t.Fatalf("owner request transitions %+v", requests)
	}
	passDrifts(t, fm)
	checkDrifts(t, repository, stream, 1, "while the lander is held")
	journey.SettleOperation(t, f.S, repository, stream, held, journey.Drifter{Foreman: fm})

	repository = fixturekit.ReopenTrace(t, f.S.Cfg, repository)
	fm = cadenceForeman(f, repository, "0")
	passDrifts(t, fm)
	ops := checkDrifts(t, repository, stream, 2, "after a restart with the lander free")
	if reason := intakefixture.TransitionByID(t, repository, stream, "drift-2").Reason; !strings.HasPrefix(reason, "the owner asked for a drift rebase at "+requests[0].At.Format(time.RFC3339)+": drift rebase 2 fetches") {
		t.Fatalf("requested drift rebase reason %q", reason)
	}
	passDrifts(t, fm)
	checkDrifts(t, repository, stream, 2, "while the requested drift rebase runs")
	journey.SettleOperation(t, f.S, repository, stream, ops[1].Operation, journey.Drifter{Foreman: fm})
	passDrifts(t, fm)
	checkDrifts(t, repository, stream, 2, "once the request is answered")

	if k, err := fm.DriftDriver().AskDrift(ctx, stream, f.S.Now()); err != nil || k != 3 {
		t.Fatalf("a later request is answered by %d: %v", k, err)
	}
	fixturekit.Must(t, fm.Drifts(ctx))
	checkDrifts(t, repository, stream, 2, "within a minute of the last read")
	f.S.Lifecycle().DriftAsked.Store(true)
	fixturekit.Must(t, fm.Drifts(ctx))
	checkDrifts(t, repository, stream, 3, "at the pass after the owner asked")
}

func TestRebaseProjectThroughTheAPI(t *testing.T) {
	t.Parallel()
	f := journey.NewDebateFixture(t, 1, 1)
	f.Upstream(t)
	stream, _ := f.BuiltAs(t, "rebase-api")
	ctx := context.Background()
	fixturekit.Must(t, f.Repository().CreateWorkstream(ctx, idleStream, f.S.Now(), intake.OwnerActor))
	fixturekit.AssertCode(t, f.C.Do(ctx, "POST", api.Prefix+"/projects/rebase", api.ProjectRebaseRequest{Project: "not-a-project"}, nil), api.Validation)
	_, err := f.C.RebaseProject(ctx, "p_ffffffffffffffffffffffffffffffff")
	fixturekit.AssertCode(t, err, api.NotFound)
	idle := api.DriftSkip{Workstream: idleStream, Reason: "the workstream is not started, not building, assembled or delivered with a pull request"}

	pause := runtime.Target{Scope: "workstream", Project: f.Project, Workstream: stream}
	fixturekit.Must(t, f.C.Do(ctx, "PUT", api.Prefix+"/runtime/pause", api.PauseRequest{Target: pause, Mode: "soft", Reason: "hold", Source: "owner"}, nil))
	got, err := f.C.RebaseProject(ctx, f.Project)
	fixturekit.Must(t, err)
	skipped := []api.DriftSkip{idle, {Workstream: stream, Reason: "the workstream is paused"}}
	if stream < idleStream {
		skipped[0], skipped[1] = skipped[1], skipped[0]
	}
	if want := (api.ProjectRebaseResponse{Project: f.Project, Covered: []api.DriftCoverage{}, Skipped: skipped}); !reflect.DeepEqual(got, want) {
		t.Fatalf("request with the workstream paused: %+v", got)
	}
	if state, err := f.Repository().Workflow(stream, lifecycle.DriftRequestSubject); err != nil || state.Value != "" {
		t.Fatalf("a paused workstream's request is %q: %v", state.Value, err)
	}
	fixturekit.Must(t, f.C.Do(ctx, "DELETE", api.Prefix+"/runtime/pause", pause, nil))

	bare := filepath.Join(filepath.Dir(f.Clone), "remotes", "dagger", "dagger.git")
	fixturekit.Must(t, os.Rename(bare, bare+".held"))
	for k := 1; k <= 2; k++ {
		got, err = f.C.RebaseProject(ctx, f.Project)
		fixturekit.Must(t, err)
		if want := (api.ProjectRebaseResponse{Project: f.Project, Covered: []api.DriftCoverage{{Workstream: stream, Drift: k}}, Skipped: []api.DriftSkip{idle}}); !reflect.DeepEqual(got, want) {
			t.Fatalf("request %d: %+v", k, got)
		}
		intakefixture.Eventually(t, fmt.Sprintf("request %d: drift rebase 1 never held", k), func() bool {
			ops := journey.DriftOperations(t, f.Repository(), stream)
			if len(ops) > 1 {
				t.Fatalf("request %d: drift rebases %+v, want drift rebase 1 held", k, ops)
			}
			return len(ops) == 1 && ops[0].Result == nil && slices.ContainsFunc(ops[0].History, func(a trace.OperationAction) bool { return a.Kind == "retry" })
		})
	}
	st, err := f.C.Status(ctx, stream)
	fixturekit.Must(t, err)
	if st.Drift == nil || st.Drift.Drift != 1 || st.Drift.Outcome != "requested" || !strings.HasPrefix(st.Drift.Reason, "the owner asked for a drift rebase at ") {
		t.Fatalf("status while drift rebase 1 is held: %+v", st.Drift)
	}

	f.Stop(t)
	fixturekit.Must(t, os.Rename(bare+".held", bare))
	f.Start(t)
	intakefixture.Eventually(t, "the owner's second request never left drift at rebased-2", func() bool {
		state, err := f.Repository().Workflow(stream, lifecycle.DriftSubject)
		fixturekit.Must(t, err)
		return state.Value == "rebased-2"
	})
	checkDrifts(t, f.Repository(), stream, 2, "after both requests are answered")
	second := intakefixture.TransitionByID(t, f.Repository(), stream, "drift-2")
	if !strings.HasPrefix(second.Reason, "the owner asked for a drift rebase at ") {
		t.Fatalf("second drift rebase reason %q", second.Reason)
	}
	rebased := intakefixture.TransitionByID(t, f.Repository(), stream, "drift-2-rebased")
	st, err = f.C.Status(ctx, stream)
	fixturekit.Must(t, err)
	if want := (&api.DriftStatus{Drift: 2, Outcome: "rebased", At: rebased.At, Reason: rebased.Reason, Moved: []string{}}); !reflect.DeepEqual(st.Drift, want) {
		t.Fatalf("status drift %+v, want %+v", st.Drift, want)
	}
	list, err := f.C.Statuses(ctx)
	fixturekit.Must(t, err)
	for _, w := range list.Workstreams {
		if w.Workstream == idleStream && w.Drift != nil || w.Workstream == stream && !reflect.DeepEqual(w.Drift, st.Drift) {
			t.Fatalf("status list drift of %s: %+v", w.Workstream, w.Drift)
		}
	}
	f.Stop(t)
}

func TestRebaseWorkstreamThroughTheAPI(t *testing.T) {
	t.Parallel()
	f := journey.NewDebateFixture(t, 1, 1)
	f.Upstream(t)
	stream, _ := f.BuiltAs(t, "rebase-one")
	ctx := context.Background()
	fixturekit.Must(t, f.Repository().CreateWorkstream(ctx, idleStream, f.S.Now(), intake.OwnerActor))
	fixturekit.AssertCode(t, f.C.Do(ctx, "POST", api.Prefix+"/rebase/not-a-workstream", nil, nil), api.Validation)
	fixturekit.AssertCode(t, f.C.Do(ctx, "GET", api.Prefix+"/rebase/"+string(stream), nil, nil), api.Unsupported)
	_, err := f.C.RebaseWorkstream(ctx, "w_ffffffffffffffffffffffffffffffff")
	fixturekit.AssertCode(t, err, api.Validation)
	_, err = f.C.RebaseWorkstream(ctx, idleStream)
	fixturekit.AssertCode(t, err, api.Conflict)
	if err == nil || !strings.Contains(err.Error(), "takes no drift rebase: the workstream is not started, not building, assembled or delivered with a pull request") {
		t.Fatalf("an idle workstream's refusal: %v", err)
	}

	pause := runtime.Target{Scope: "workstream", Project: f.Project, Workstream: stream}
	fixturekit.Must(t, f.C.Do(ctx, "PUT", api.Prefix+"/runtime/pause", api.PauseRequest{Target: pause, Mode: "soft", Reason: "hold", Source: "owner"}, nil))
	_, err = f.C.RebaseWorkstream(ctx, stream)
	fixturekit.AssertCode(t, err, api.Conflict)
	if err == nil || !strings.Contains(err.Error(), "the workstream is paused") {
		t.Fatalf("a paused workstream's refusal: %v", err)
	}
	if state, err := f.Repository().Workflow(stream, lifecycle.DriftRequestSubject); err != nil || state.Value != "" {
		t.Fatalf("a paused workstream's request is %q: %v", state.Value, err)
	}
	fixturekit.Must(t, f.C.Do(ctx, "DELETE", api.Prefix+"/runtime/pause", pause, nil))

	got, err := f.C.RebaseWorkstream(ctx, stream)
	fixturekit.Must(t, err)
	if want := (api.WorkstreamRebaseResponse{Project: f.Project, Workstream: stream, Drift: 1}); got != want {
		t.Fatalf("request: %+v, want %+v", got, want)
	}
	intakefixture.Eventually(t, "the owner's request never left drift at rebased-1", func() bool {
		state, err := f.Repository().Workflow(stream, lifecycle.DriftSubject)
		fixturekit.Must(t, err)
		return state.Value == "rebased-1"
	})
	if first := intakefixture.TransitionByID(t, f.Repository(), stream, "drift-1"); !strings.HasPrefix(first.Reason, "the owner asked for a drift rebase at ") {
		t.Fatalf("drift rebase 1 reason %q", first.Reason)
	}
	if state, err := f.Repository().Workflow(idleStream, lifecycle.DriftRequestSubject); err != nil || state.Value != "" {
		t.Fatalf("the request reached another workstream: %q %v", state.Value, err)
	}
	got, err = f.C.RebaseWorkstream(ctx, stream)
	fixturekit.Must(t, err)
	if got.Drift != 2 {
		t.Fatalf("the next request answers drift rebase %d, want 2", got.Drift)
	}
	f.Stop(t)
}
