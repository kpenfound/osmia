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

	"github.com/kpenfound/osmia/internal/api"
	"github.com/kpenfound/osmia/internal/delivery"
	"github.com/kpenfound/osmia/internal/intake"
	"github.com/kpenfound/osmia/internal/lifecycle"
	"github.com/kpenfound/osmia/internal/service"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	"github.com/kpenfound/osmia/internal/servicetest/intakefixture"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/runtime"
	"github.com/kpenfound/osmia/internal/seal"
	"github.com/kpenfound/osmia/internal/trace"
)

const idleStream config.WorkstreamID = "w_0000000000000000000000000000d1f7"

func setClock(f *shedFixture, at time.Time) {
	f.clock.Mu.Lock()
	defer f.clock.Mu.Unlock()
	f.clock.At = at
}

func cadenceForeman(f *shedFixture, repository *trace.Repository, interval string) *foreman {
	cfg := *f.s.cfg
	cfg.Project.UpstreamRebase = interval
	return &foreman{masons: &masons{s: f.s, cfg: &cfg, repository: repository}}
}

func passDrifts(t *testing.T, fm *foreman) {
	t.Helper()
	fm.nextDrift = time.Time{}
	intakefixture.Must(t, fm.drifts(context.Background()))
}

func restartTrace(t *testing.T, f *shedFixture, repository *trace.Repository) *trace.Repository {
	t.Helper()
	return fixturekit.ReopenTrace(t, f.s.cfg, repository)
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
	intakefixture.Must(t, err)
	drift, err := lifecycle.LatestDrift(repository, stream, states)
	intakefixture.Must(t, err)
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
	ops := driftOperations(t, repository, stream)
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
	f, stream, repository, _ := newFinalFixture(t, "cadence")
	ctx := context.Background()
	intakefixture.Must(t, repository.CreateWorkstream(ctx, idleStream, f.s.now(), intake.OwnerActor))
	sealed := sealedAt(t, repository, stream)
	fm := cadenceForeman(f, repository, "1h")

	setClock(f, sealed.Add(59*time.Minute+30*time.Second))
	intakefixture.Must(t, fm.drifts(ctx))
	checkDrifts(t, repository, stream, 0, "before the interval elapsed")
	setClock(f, sealed.Add(time.Hour))
	intakefixture.Must(t, fm.drifts(ctx))
	checkDrifts(t, repository, stream, 0, "within a minute of the last read")
	setClock(f, sealed.Add(time.Hour+30*time.Second))
	intakefixture.Must(t, fm.drifts(ctx))
	ops := checkDrifts(t, repository, stream, 1, "once the interval elapsed since the sealing")
	if reason := intakefixture.TransitionByID(t, repository, stream, "drift-1").Reason; !strings.HasPrefix(reason, "upstream_rebase 1h0m0s has elapsed since the sealing at "+sealed.Format(time.RFC3339)+": drift rebase 1 fetches") {
		t.Fatalf("scheduled drift rebase reason %q", reason)
	}
	setClock(f, sealed.Add(5*time.Hour))
	passDrifts(t, fm)
	checkDrifts(t, repository, stream, 1, "while the drift rebase runs")
	settleOperation(t, f.s, repository, stream, ops[0].Operation, drifter{fm})
	first := driftTransition(t, repository, stream)
	if first.To != "rebased-1" {
		t.Fatalf("drift rebase 1 is %s", first.To)
	}

	repository = restartTrace(t, f, repository)
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
	repository = restartTrace(t, f, repository)
	fm = cadenceForeman(f, repository, "1h")
	passDrifts(t, fm)
	checkDrifts(t, repository, stream, 2, "after a restart while drift rebase 2 runs")
	settleOperation(t, f.s, repository, stream, ops[1].Operation, drifter{fm})
	second := driftTransition(t, repository, stream)

	finalAt := second.At.Add(30 * time.Minute)
	g, err := intake.FeatureWorkspaces(f.s.about(repository), repository).Of(stream)
	intakefixture.Must(t, err)
	tip, _, err := g.Branch(ctx, intake.FeatureBranch(stream))
	intakefixture.Must(t, err)
	base, err := lifecycle.BranchBase(ctx, repository, stream, g, tip)
	intakefixture.Must(t, err)
	content, err := json.Marshal(delivery.FinalRebase{Review: 1, Operation: "planted-final-review", Branch: intake.FeatureBranch(stream), Before: tip, Commit: tip, Upstream: base})
	intakefixture.Must(t, err)
	revision, err := intake.NextRevision(repository, stream, delivery.FinalRebaseDocument)
	intakefixture.Must(t, err)
	intakefixture.Must(t, repository.RecordDocuments(ctx, []trace.Document{{Header: trace.Header{Schema: "osmia.trace.document", Version: trace.Version, ID: delivery.FinalRebaseDocument, Revision: revision, Project: repository.Project(), Workstream: stream, At: finalAt, Actor: service.TestForemanActor, Cause: "planted-final-review"},
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
	settleOperation(t, f.s, repository, stream, ops[2].Operation, drifter{fm})

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
	f, stream, repository, _ := newFinalFixture(t, "asked")
	ctx := context.Background()
	sealed := sealedAt(t, repository, stream)
	fm := cadenceForeman(f, repository, "0")
	setClock(f, sealed.Add(100*time.Hour))
	passDrifts(t, fm)
	checkDrifts(t, repository, stream, 0, "with scheduled drift rebases disabled")

	held := requestDrift(t, drifter{fm}, stream)
	for range 2 {
		if k, err := fm.askDrift(ctx, stream, f.s.now()); err != nil || k != 2 {
			t.Fatalf("the owner's request while drift rebase 1 runs is answered by %d: %v", k, err)
		}
	}
	transitions, err := trace.Read[trace.Transition](repository, stream)
	intakefixture.Must(t, err)
	requests := slices.DeleteFunc(transitions, func(tr trace.Transition) bool { return tr.Subject != lifecycle.DriftRequestSubject })
	if len(requests) != 1 || requests[0].ID != "drift-request-2" || requests[0].To != "requested-2" || requests[0].Actor != intake.OwnerActor {
		t.Fatalf("owner request transitions %+v", requests)
	}
	passDrifts(t, fm)
	checkDrifts(t, repository, stream, 1, "while the lander is held")
	settleOperation(t, f.s, repository, stream, held, drifter{fm})

	repository = restartTrace(t, f, repository)
	fm = cadenceForeman(f, repository, "0")
	passDrifts(t, fm)
	ops := checkDrifts(t, repository, stream, 2, "after a restart with the lander free")
	if reason := intakefixture.TransitionByID(t, repository, stream, "drift-2").Reason; !strings.HasPrefix(reason, "the owner asked for a drift rebase at "+requests[0].At.Format(time.RFC3339)+": drift rebase 2 fetches") {
		t.Fatalf("requested drift rebase reason %q", reason)
	}
	passDrifts(t, fm)
	checkDrifts(t, repository, stream, 2, "while the requested drift rebase runs")
	settleOperation(t, f.s, repository, stream, ops[1].Operation, drifter{fm})
	passDrifts(t, fm)
	checkDrifts(t, repository, stream, 2, "once the request is answered")

	if k, err := fm.askDrift(ctx, stream, f.s.now()); err != nil || k != 3 {
		t.Fatalf("a later request is answered by %d: %v", k, err)
	}
	intakefixture.Must(t, fm.drifts(ctx))
	checkDrifts(t, repository, stream, 2, "within a minute of the last read")
	f.s.lifecycle().DriftAsked.Store(true)
	intakefixture.Must(t, fm.drifts(ctx))
	checkDrifts(t, repository, stream, 3, "at the pass after the owner asked")
}

func TestRebaseProjectThroughTheAPI(t *testing.T) {
	t.Parallel()
	f := newDebateFixture(t, 1, 1)
	f.upstream(t)
	stream, _ := f.builtAs(t, "rebase-api")
	ctx := context.Background()
	intakefixture.Must(t, f.repository().CreateWorkstream(ctx, idleStream, f.s.now(), intake.OwnerActor))
	fixturekit.AssertCode(t, f.c.Do(ctx, "POST", api.Prefix+"/projects/rebase", api.ProjectRebaseRequest{Project: "not-a-project"}, nil), api.Validation)
	_, err := f.c.RebaseProject(ctx, "p_ffffffffffffffffffffffffffffffff")
	fixturekit.AssertCode(t, err, api.NotFound)
	idle := api.DriftSkip{Workstream: idleStream, Reason: "the workstream is not started, not building, assembled or delivered with a pull request"}

	pause := runtime.Target{Scope: "workstream", Project: f.project, Workstream: stream}
	intakefixture.Must(t, f.c.Do(ctx, "PUT", api.Prefix+"/runtime/pause", api.PauseRequest{Target: pause, Mode: "soft", Reason: "hold", Source: "owner"}, nil))
	got, err := f.c.RebaseProject(ctx, f.project)
	intakefixture.Must(t, err)
	skipped := []api.DriftSkip{idle, {Workstream: stream, Reason: "the workstream is paused"}}
	if stream < idleStream {
		skipped[0], skipped[1] = skipped[1], skipped[0]
	}
	if want := (api.ProjectRebaseResponse{Project: f.project, Covered: []api.DriftCoverage{}, Skipped: skipped}); !reflect.DeepEqual(got, want) {
		t.Fatalf("request with the workstream paused: %+v", got)
	}
	if state, err := f.repository().Workflow(stream, lifecycle.DriftRequestSubject); err != nil || state.Value != "" {
		t.Fatalf("a paused workstream's request is %q: %v", state.Value, err)
	}
	intakefixture.Must(t, f.c.Do(ctx, "DELETE", api.Prefix+"/runtime/pause", pause, nil))

	bare := filepath.Join(filepath.Dir(f.clone), "remotes", "dagger", "dagger.git")
	intakefixture.Must(t, os.Rename(bare, bare+".held"))
	for k := 1; k <= 2; k++ {
		got, err = f.c.RebaseProject(ctx, f.project)
		intakefixture.Must(t, err)
		if want := (api.ProjectRebaseResponse{Project: f.project, Covered: []api.DriftCoverage{{Workstream: stream, Drift: k}}, Skipped: []api.DriftSkip{idle}}); !reflect.DeepEqual(got, want) {
			t.Fatalf("request %d: %+v", k, got)
		}
		intakefixture.Eventually(t, fmt.Sprintf("request %d: drift rebase 1 never held", k), func() bool {
			ops := driftOperations(t, f.repository(), stream)
			if len(ops) > 1 {
				t.Fatalf("request %d: drift rebases %+v, want drift rebase 1 held", k, ops)
			}
			return len(ops) == 1 && ops[0].Result == nil && slices.ContainsFunc(ops[0].History, func(a trace.OperationAction) bool { return a.Kind == "retry" })
		})
	}
	st, err := f.c.Status(ctx, stream)
	intakefixture.Must(t, err)
	if st.Drift == nil || st.Drift.Drift != 1 || st.Drift.Outcome != "requested" || !strings.HasPrefix(st.Drift.Reason, "the owner asked for a drift rebase at ") {
		t.Fatalf("status while drift rebase 1 is held: %+v", st.Drift)
	}

	f.stop(t)
	intakefixture.Must(t, os.Rename(bare+".held", bare))
	f.start(t)
	intakefixture.Eventually(t, "the owner's second request never left drift at rebased-2", func() bool {
		state, err := f.repository().Workflow(stream, lifecycle.DriftSubject)
		intakefixture.Must(t, err)
		return state.Value == "rebased-2"
	})
	checkDrifts(t, f.repository(), stream, 2, "after both requests are answered")
	second := intakefixture.TransitionByID(t, f.repository(), stream, "drift-2")
	if !strings.HasPrefix(second.Reason, "the owner asked for a drift rebase at ") {
		t.Fatalf("second drift rebase reason %q", second.Reason)
	}
	rebased := intakefixture.TransitionByID(t, f.repository(), stream, "drift-2-rebased")
	st, err = f.c.Status(ctx, stream)
	intakefixture.Must(t, err)
	if want := (&api.DriftStatus{Drift: 2, Outcome: "rebased", At: rebased.At, Reason: rebased.Reason, Moved: []string{}}); !reflect.DeepEqual(st.Drift, want) {
		t.Fatalf("status drift %+v, want %+v", st.Drift, want)
	}
	list, err := f.c.Statuses(ctx)
	intakefixture.Must(t, err)
	for _, w := range list.Workstreams {
		if w.Workstream == idleStream && w.Drift != nil || w.Workstream == stream && !reflect.DeepEqual(w.Drift, st.Drift) {
			t.Fatalf("status list drift of %s: %+v", w.Workstream, w.Drift)
		}
	}
	f.stop(t)
}

func TestRebaseWorkstreamThroughTheAPI(t *testing.T) {
	t.Parallel()
	f := newDebateFixture(t, 1, 1)
	f.upstream(t)
	stream, _ := f.builtAs(t, "rebase-one")
	ctx := context.Background()
	intakefixture.Must(t, f.repository().CreateWorkstream(ctx, idleStream, f.s.now(), intake.OwnerActor))
	fixturekit.AssertCode(t, f.c.Do(ctx, "POST", api.Prefix+"/rebase/not-a-workstream", nil, nil), api.Validation)
	fixturekit.AssertCode(t, f.c.Do(ctx, "GET", api.Prefix+"/rebase/"+string(stream), nil, nil), api.Unsupported)
	_, err := f.c.RebaseWorkstream(ctx, "w_ffffffffffffffffffffffffffffffff")
	fixturekit.AssertCode(t, err, api.Validation)
	_, err = f.c.RebaseWorkstream(ctx, idleStream)
	fixturekit.AssertCode(t, err, api.Conflict)
	if err == nil || !strings.Contains(err.Error(), "takes no drift rebase: the workstream is not started, not building, assembled or delivered with a pull request") {
		t.Fatalf("an idle workstream's refusal: %v", err)
	}

	pause := runtime.Target{Scope: "workstream", Project: f.project, Workstream: stream}
	intakefixture.Must(t, f.c.Do(ctx, "PUT", api.Prefix+"/runtime/pause", api.PauseRequest{Target: pause, Mode: "soft", Reason: "hold", Source: "owner"}, nil))
	_, err = f.c.RebaseWorkstream(ctx, stream)
	fixturekit.AssertCode(t, err, api.Conflict)
	if err == nil || !strings.Contains(err.Error(), "the workstream is paused") {
		t.Fatalf("a paused workstream's refusal: %v", err)
	}
	if state, err := f.repository().Workflow(stream, lifecycle.DriftRequestSubject); err != nil || state.Value != "" {
		t.Fatalf("a paused workstream's request is %q: %v", state.Value, err)
	}
	intakefixture.Must(t, f.c.Do(ctx, "DELETE", api.Prefix+"/runtime/pause", pause, nil))

	got, err := f.c.RebaseWorkstream(ctx, stream)
	intakefixture.Must(t, err)
	if want := (api.WorkstreamRebaseResponse{Project: f.project, Workstream: stream, Drift: 1}); got != want {
		t.Fatalf("request: %+v, want %+v", got, want)
	}
	intakefixture.Eventually(t, "the owner's request never left drift at rebased-1", func() bool {
		state, err := f.repository().Workflow(stream, lifecycle.DriftSubject)
		intakefixture.Must(t, err)
		return state.Value == "rebased-1"
	})
	if first := intakefixture.TransitionByID(t, f.repository(), stream, "drift-1"); !strings.HasPrefix(first.Reason, "the owner asked for a drift rebase at ") {
		t.Fatalf("drift rebase 1 reason %q", first.Reason)
	}
	if state, err := f.repository().Workflow(idleStream, lifecycle.DriftRequestSubject); err != nil || state.Value != "" {
		t.Fatalf("the request reached another workstream: %q %v", state.Value, err)
	}
	got, err = f.c.RebaseWorkstream(ctx, stream)
	intakefixture.Must(t, err)
	if got.Drift != 2 {
		t.Fatalf("the next request answers drift rebase %d, want 2", got.Drift)
	}
	f.stop(t)
}
