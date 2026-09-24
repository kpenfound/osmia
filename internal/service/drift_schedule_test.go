package service

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/runtime"
	"github.com/kpenfound/osmia/internal/seal"
	"github.com/kpenfound/osmia/internal/trace"
)

// idleStream is a workstream that never starts, so it is neither building
// nor assembled.
const idleStream config.WorkstreamID = "w_0000000000000000000000000000d1f7"

// setClock sets the fixture's clock, which advances a second on every read.
func setClock(f *shedFixture, at time.Time) {
	f.clock.mu.Lock()
	defer f.clock.mu.Unlock()
	f.clock.now = at
}

// cadenceForeman returns a foreman over repository whose project schedules
// drift rebases every interval.
func cadenceForeman(f *shedFixture, repository *trace.Repository, interval string) *foreman {
	cfg := *f.s.cfg
	cfg.Project.UpstreamRebase = interval
	return &foreman{masons: &masons{s: f.s, cfg: &cfg, repository: repository}}
}

// passDrifts runs the foreman's drift schedule as a pass that reads it does.
func passDrifts(t *testing.T, fm *foreman) {
	t.Helper()
	fm.nextDrift = time.Time{}
	must(t, fm.drifts(context.Background()))
}

// restartTrace closes repository and opens the project trace again, as a
// service restart does.
func restartTrace(t *testing.T, f *shedFixture, repository *trace.Repository) *trace.Repository {
	t.Helper()
	must(t, repository.Close())
	reopened, err := trace.Open(f.s.cfg.Root, f.s.cfg.Project)
	must(t, err)
	t.Cleanup(func() { reopened.Close() })
	return reopened
}

// sealedAt returns when the workstream's first seal was recorded.
func sealedAt(t *testing.T, repository *trace.Repository, stream config.WorkstreamID) time.Time {
	t.Helper()
	docs := streamDocuments(t, repository, stream, seal.DocumentID)
	if len(docs) == 0 {
		t.Fatalf("workstream %s has no seal", stream)
	}
	return slices.MinFunc(docs, func(a, b trace.Document) int { return a.At.Compare(b.At) }).At
}

// driftTransition returns the workstream's latest transition of the drift
// subject.
func driftTransition(t *testing.T, repository *trace.Repository, stream config.WorkstreamID) trace.Transition {
	t.Helper()
	states, err := repository.WorkflowStates(stream)
	must(t, err)
	drift, err := latestDrift(repository, stream, states)
	must(t, err)
	if drift == nil {
		t.Fatalf("workstream %s has no drift rebase", stream)
	}
	return transitionByID(t, repository, stream, driftSubjectTransition(drift))
}

// driftSubjectTransition names the transition that recorded a drift status.
func driftSubjectTransition(d *DriftStatus) string {
	transition, _ := driftIDs(d.Drift)
	if d.Outcome == "requested" {
		return transition
	}
	return transition + "-" + d.Outcome
}

// checkDrifts checks how many drift rebases the workstream was asked for.
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

// A scheduled drift rebase is asked for once the project's upstream_rebase
// interval has elapsed since the workstream's sealing, then since its latest
// drift rebase or final rebase. Only one is asked for while it runs, and a
// restart neither resets an elapsed interval nor asks for it again. A zero
// interval schedules nothing, and a workstream that is not building is never
// asked for.
func TestScheduledDriftRebasesFollowTheIntervalAcrossRestart(t *testing.T) {
	t.Parallel()
	f, stream, repository, _ := newFinalFixture(t, "cadence")
	ctx := context.Background()
	must(t, repository.CreateWorkstream(ctx, idleStream, f.s.now(), ownerActor))
	sealed := sealedAt(t, repository, stream)
	fm := cadenceForeman(f, repository, "1h")

	setClock(f, sealed.Add(59*time.Minute+30*time.Second))
	must(t, fm.drifts(ctx))
	checkDrifts(t, repository, stream, 0, "before the interval elapsed")
	// The schedule is read again a minute later, not at every pass.
	setClock(f, sealed.Add(time.Hour))
	must(t, fm.drifts(ctx))
	checkDrifts(t, repository, stream, 0, "within a minute of the last read")
	setClock(f, sealed.Add(time.Hour+30*time.Second))
	must(t, fm.drifts(ctx))
	ops := checkDrifts(t, repository, stream, 1, "once the interval elapsed since the sealing")
	if reason := transitionByID(t, repository, stream, "drift-1").Reason; !strings.HasPrefix(reason, "upstream_rebase 1h0m0s has elapsed since the sealing at "+sealed.Format(time.RFC3339)+": drift rebase 1 fetches") {
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
	if reason := transitionByID(t, repository, stream, "drift-2").Reason; !strings.HasPrefix(reason, "upstream_rebase 1h0m0s has elapsed since the drift rebase 1 at "+first.At.Format(time.RFC3339)+":") {
		t.Fatalf("second scheduled drift rebase reason %q", reason)
	}
	repository = restartTrace(t, f, repository)
	fm = cadenceForeman(f, repository, "1h")
	passDrifts(t, fm)
	checkDrifts(t, repository, stream, 2, "after a restart while drift rebase 2 runs")
	settleOperation(t, f.s, repository, stream, ops[1].Operation, drifter{fm})
	second := driftTransition(t, repository, stream)

	// A final rebase after the drift rebase restarts the interval.
	finalAt := second.At.Add(30 * time.Minute)
	content, err := json.Marshal(FinalRebase{Review: 1, Operation: "planted-final-review", Branch: featureBranch(stream), Before: "before", Commit: "after"})
	must(t, err)
	revision, err := nextRevision(repository, stream, finalRebaseDocument)
	must(t, err)
	must(t, repository.RecordDocuments(ctx, []trace.Document{{Header: trace.Header{Schema: "osmia.trace.document", Version: trace.Version, ID: finalRebaseDocument, Revision: revision, Project: repository.Project(), Workstream: stream, At: finalAt, Actor: foremanActor, Cause: "planted-final-review"},
		Path: finalRebasePath, Content: string(content) + "\n"}}))
	setClock(f, second.At.Add(time.Hour))
	passDrifts(t, fm)
	checkDrifts(t, repository, stream, 2, "within the interval since the final rebase")
	setClock(f, finalAt.Add(time.Hour))
	passDrifts(t, fm)
	ops = checkDrifts(t, repository, stream, 3, "once the interval elapsed since the final rebase")
	if reason := transitionByID(t, repository, stream, "drift-3").Reason; !strings.HasPrefix(reason, "upstream_rebase 1h0m0s has elapsed since the final rebase at "+finalAt.Format(time.RFC3339)+":") {
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

// The owner's request for a drift rebase is durable: it waits for the
// project's lander, survives a restart, is answered once with cadence
// disabled, and asking again before it is answered records nothing more.
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
	must(t, err)
	requests := slices.DeleteFunc(transitions, func(tr trace.Transition) bool { return tr.Subject != driftRequestSubject })
	if len(requests) != 1 || requests[0].ID != "drift-request-2" || requests[0].To != "requested-2" || requests[0].Actor != ownerActor {
		t.Fatalf("owner request transitions %+v", requests)
	}
	passDrifts(t, fm)
	checkDrifts(t, repository, stream, 1, "while the lander is held")
	settleOperation(t, f.s, repository, stream, held, drifter{fm})

	repository = restartTrace(t, f, repository)
	fm = cadenceForeman(f, repository, "0")
	passDrifts(t, fm)
	ops := checkDrifts(t, repository, stream, 2, "after a restart with the lander free")
	if reason := transitionByID(t, repository, stream, "drift-2").Reason; !strings.HasPrefix(reason, "the owner asked for a drift rebase at "+requests[0].At.Format(time.RFC3339)+": drift rebase 2 fetches") {
		t.Fatalf("requested drift rebase reason %q", reason)
	}
	passDrifts(t, fm)
	checkDrifts(t, repository, stream, 2, "while the requested drift rebase runs")
	settleOperation(t, f.s, repository, stream, ops[1].Operation, drifter{fm})
	passDrifts(t, fm)
	checkDrifts(t, repository, stream, 2, "once the request is answered")

	// A recorded request is read at the next pass, not a minute later.
	if k, err := fm.askDrift(ctx, stream, f.s.now()); err != nil || k != 3 {
		t.Fatalf("a later request is answered by %d: %v", k, err)
	}
	must(t, fm.drifts(ctx))
	checkDrifts(t, repository, stream, 2, "within a minute of the last read")
	f.s.driftAsked.Store(true)
	must(t, fm.drifts(ctx))
	checkDrifts(t, repository, stream, 3, "at the pass after the owner asked")
}

// POST /v1/projects/rebase covers the building workstreams that are not
// paused and skips the others with why. Its requests are served on the
// project's lander, one after another, across a service restart, and status
// shows each workstream's latest drift rebase.
func TestRebaseProjectThroughTheAPI(t *testing.T) {
	t.Parallel()
	f := newDebateFixture(t, 1, 1)
	f.upstream(t)
	stream, _ := f.builtAs(t, "rebase-api")
	ctx := context.Background()
	must(t, f.repository().CreateWorkstream(ctx, idleStream, f.s.now(), ownerActor))
	assertCode(t, f.c.Do(ctx, "POST", Prefix+"/projects/rebase", ProjectRebaseRequest{Project: "not-a-project"}, nil), Validation)
	_, err := f.c.RebaseProject(ctx, "p_ffffffffffffffffffffffffffffffff")
	assertCode(t, err, NotFound)
	idle := DriftSkip{Workstream: idleStream, Reason: "the workstream is not started, not building or assembled"}

	pause := runtime.Target{Scope: "workstream", Project: f.project, Workstream: stream}
	must(t, f.c.Do(ctx, "PUT", Prefix+"/runtime/pause", PauseRequest{Target: pause, Mode: "soft", Reason: "hold", Source: "owner"}, nil))
	got, err := f.c.RebaseProject(ctx, f.project)
	must(t, err)
	skipped := []DriftSkip{idle, {Workstream: stream, Reason: "the workstream is paused"}}
	if stream < idleStream {
		skipped[0], skipped[1] = skipped[1], skipped[0]
	}
	if want := (ProjectRebaseResponse{Project: f.project, Covered: []DriftCoverage{}, Skipped: skipped}); !reflect.DeepEqual(got, want) {
		t.Fatalf("request with the workstream paused: %+v", got)
	}
	if state, err := f.repository().Workflow(stream, driftRequestSubject); err != nil || state.Value != "" {
		t.Fatalf("a paused workstream's request is %q: %v", state.Value, err)
	}
	must(t, f.c.Do(ctx, "DELETE", Prefix+"/runtime/pause", pause, nil))

	// Upstream cannot be fetched, so drift rebase 1 holds the lander.
	bare := filepath.Join(filepath.Dir(f.clone), "remotes", "dagger", "dagger.git")
	must(t, os.Rename(bare, bare+".held"))
	for k := 1; k <= 2; k++ {
		got, err = f.c.RebaseProject(ctx, f.project)
		must(t, err)
		if want := (ProjectRebaseResponse{Project: f.project, Covered: []DriftCoverage{{Workstream: stream, Drift: k}}, Skipped: []DriftSkip{idle}}); !reflect.DeepEqual(got, want) {
			t.Fatalf("request %d: %+v", k, got)
		}
		deadline := time.Now().Add(demoTimeout)
		for {
			ops := driftOperations(t, f.repository(), stream)
			if len(ops) == 1 && ops[0].Result == nil && slices.ContainsFunc(ops[0].History, func(a trace.OperationAction) bool { return a.Kind == "retry" }) {
				break
			}
			if len(ops) > 1 || time.Now().After(deadline) {
				t.Fatalf("request %d: drift rebases %+v, want drift rebase 1 held", k, ops)
			}
			time.Sleep(50 * time.Millisecond)
		}
	}
	st, err := f.c.Status(ctx, stream)
	must(t, err)
	if st.Drift == nil || st.Drift.Drift != 1 || st.Drift.Outcome != "requested" || !strings.HasPrefix(st.Drift.Reason, "the owner asked for a drift rebase at ") {
		t.Fatalf("status while drift rebase 1 is held: %+v", st.Drift)
	}

	f.stop(t)
	must(t, os.Rename(bare+".held", bare))
	f.start(t)
	deadline := time.Now().Add(demoTimeout)
	for {
		state, err := f.repository().Workflow(stream, driftSubject)
		must(t, err)
		if state.Value == "rebased-2" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the owner's second request left drift at %q", state.Value)
		}
		time.Sleep(50 * time.Millisecond)
	}
	checkDrifts(t, f.repository(), stream, 2, "after both requests are answered")
	second := transitionByID(t, f.repository(), stream, "drift-2")
	if !strings.HasPrefix(second.Reason, "the owner asked for a drift rebase at ") {
		t.Fatalf("second drift rebase reason %q", second.Reason)
	}
	rebased := transitionByID(t, f.repository(), stream, "drift-2-rebased")
	st, err = f.c.Status(ctx, stream)
	must(t, err)
	if want := (&DriftStatus{Drift: 2, Outcome: "rebased", At: rebased.At, Reason: rebased.Reason, Moved: []string{}}); !reflect.DeepEqual(st.Drift, want) {
		t.Fatalf("status drift %+v, want %+v", st.Drift, want)
	}
	list, err := f.c.Statuses(ctx)
	must(t, err)
	for _, w := range list.Workstreams {
		if w.Workstream == idleStream && w.Drift != nil || w.Workstream == stream && !reflect.DeepEqual(w.Drift, st.Drift) {
			t.Fatalf("status list drift of %s: %+v", w.Workstream, w.Drift)
		}
	}
	f.stop(t)
}
