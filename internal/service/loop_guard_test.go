package service

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/runtime"
	"github.com/kpenfound/osmia/internal/servicetest/intakefixture"
	"github.com/kpenfound/osmia/internal/trace"
)

func recordSessions(t *testing.T, f *shedFixture, repository *trace.Repository, stream config.WorkstreamID, n int) {
	t.Helper()
	for range n {
		f.clock.Mu.Lock()
		f.clock.At = f.clock.At.Add(time.Minute)
		at := f.clock.At
		f.clock.Mu.Unlock()
		id := fmt.Sprintf("session-%d", at.UnixNano())
		fixturekit.Must(t, repository.Append(context.Background(), trace.Cost{Header: trace.Header{Schema: "osmia.trace.cost", Version: trace.Version, ID: id, Revision: 1, Project: f.project, Workstream: stream, At: at, Actor: trace.Actor{Kind: "service", ID: "thread-runner"}, Cause: "loop-fixture"},
			Entry: coreadapter.LedgerEntry{Scope: coreadapter.Scope{Project: string(f.project), Workstream: string(stream), Thread: reviewerAgent("resume"), Turn: id, Role: reviewerRole}, AttemptID: id, At: at, Usage: coreadapter.Usage{CostUSD: 0.5, CostKnown: true, Turns: 1}}}))
	}
}

func openRuntime(t *testing.T, f *shedFixture, stream config.WorkstreamID) {
	t.Helper()
	store, _, err := runtime.Open(runtime.Inputs{Config: f.s.cfg, Workstreams: map[config.ProjectID][]config.WorkstreamID{f.project: {stream}}})
	fixturekit.Must(t, err)
	t.Cleanup(func() { store.Close() })
	f.s.store = store
}

func loopPause(f *shedFixture, stream config.WorkstreamID) (runtime.Pause, bool) {
	state, _ := f.s.store.Snapshot()
	i := slices.IndexFunc(state.Pauses, func(p runtime.Pause) bool {
		return p.Target.Workstream == stream && p.Source == runtime.PauseLoopGuard
	})
	if i < 0 {
		return runtime.Pause{}, false
	}
	return state.Pauses[i], true
}

func TestLoopGuardPausesAWorkstreamWithoutProgress(t *testing.T) {
	t.Parallel()
	f, stream, repository := newReviewFixture(t, "loop-guard")
	openRuntime(t, f, stream)
	f.s.cfg.Loop.MaxSessions = 3
	g := loopGuard{s: f.s, cfg: f.s.cfg, repository: repository}
	ctx := context.Background()

	recordSessions(t, f, repository, stream, 2)
	fixturekit.Must(t, g.Pass(ctx))
	if _, paused := loopPause(f, stream); paused {
		t.Fatal("the loop guard paused the workstream below the limit")
	}
	intakefixture.MoveUnitNow(t, f.clock.Now(), foremanActor, repository, stream, "resume", UnitChecking)
	recordSessions(t, f, repository, stream, 2)
	fixturekit.Must(t, g.Pass(ctx))
	if _, paused := loopPause(f, stream); paused {
		t.Fatal("the loop guard counted sessions from before the latest progress")
	}
	intakefixture.MoveUnitNow(t, f.clock.Now(), foremanActor, repository, stream, "resume", UnitReviewing)
	state, err := repository.Workflow(stream, trace.UnitSubject("resume"))
	fixturekit.Must(t, err)
	recordSessions(t, f, repository, stream, 1)
	r := &reviewers{masons: newMasonController(f.s, repository)}
	fixturekit.Must(t, r.driver().RefreshReview(ctx, stream, "resume", state, "stale base revision; review the current candidate again"))
	recordSessions(t, f, repository, stream, 2)
	fixturekit.Must(t, g.Pass(ctx))
	pause, paused := loopPause(f, stream)
	if !paused || pause.Mode != "soft" || !strings.HasPrefix(pause.Reason, "Loop guard: 3 agent sessions ran since the workstream's last progress, unit resume moving to reviewing at ") {
		t.Fatalf("the loop guard's pause %+v %v", pause, paused)
	}
	paused1 := intakefixture.TransitionByID(t, repository, stream, loopGuardSubject+"-paused-1")
	if paused1.To != "paused-1" || paused1.Reason != pause.Reason {
		t.Fatalf("the recorded pause %+v", paused1)
	}
	statuses, err := repository.Statuses()
	fixturekit.Must(t, err)
	i := slices.IndexFunc(statuses, func(w trace.WorkstreamStatus) bool { return w.Workstream == stream })
	decisions, err := f.s.openDecisions(ctx, repository, statuses[i])
	fixturekit.Must(t, err)
	j := slices.IndexFunc(decisions, func(e InboxEntry) bool { return e.Kind == InboxLoop })
	if j < 0 || decisions[j].Answer.Path != Prefix+"/runtime/pause" || decisions[j].Answer.Body["workstream"] != stream {
		t.Fatalf("the inbox does not list the loop guard pause: %+v", decisions)
	}
	recordSessions(t, f, repository, stream, 5)
	fixturekit.Must(t, g.Pass(ctx))
	if guard, err := repository.Workflow(stream, loopGuardSubject); err != nil || guard.Value != "paused-1" {
		t.Fatalf("the loop guard subject %+v: %v", guard, err)
	}

	fixturekit.Must(t, f.s.store.ClearPause(pause.Target, runtime.PauseOwner))
	fixturekit.Must(t, g.Pass(ctx))
	if guard, err := repository.Workflow(stream, loopGuardSubject); err != nil || guard.Value != "released-1" {
		t.Fatalf("the loop guard subject after the resume %+v: %v", guard, err)
	}
	recordSessions(t, f, repository, stream, 2)
	fixturekit.Must(t, g.Pass(ctx))
	if _, paused := loopPause(f, stream); paused {
		t.Fatal("the loop guard counted sessions from before the owner resumed the workstream")
	}
	recordSessions(t, f, repository, stream, 1)
	fixturekit.Must(t, g.Pass(ctx))
	if pause, paused := loopPause(f, stream); !paused || !strings.Contains(pause.Reason, "since the workstream's last progress, the owner resuming it at ") {
		t.Fatalf("the second loop guard pause %+v %v", pause, paused)
	}
	if guard, err := repository.Workflow(stream, loopGuardSubject); err != nil || guard.Value != "paused-2" {
		t.Fatalf("the loop guard subject %+v: %v", guard, err)
	}
}

// An owner's pause holds the loop guard back, and zero turns it off.
func TestLoopGuardLeavesPausedAndUnguardedWorkstreams(t *testing.T) {
	t.Parallel()
	f, stream, repository := newReviewFixture(t, "loop-guard-off")
	openRuntime(t, f, stream)
	f.s.cfg.Loop.MaxSessions = 2
	g := loopGuard{s: f.s, cfg: f.s.cfg, repository: repository}
	ctx := context.Background()
	target := runtime.Target{Scope: "workstream", Project: f.project, Workstream: stream}
	fixturekit.Must(t, f.s.store.SetPause(runtime.Pause{Target: target, Mode: "soft", Reason: "travelling", Source: runtime.PauseOwner, SetAt: f.clock.Now()}))
	recordSessions(t, f, repository, stream, 3)
	fixturekit.Must(t, g.Pass(ctx))
	if guard, err := repository.Workflow(stream, loopGuardSubject); err != nil || guard.Value != "" {
		t.Fatalf("the loop guard acted on a paused workstream: %+v %v", guard, err)
	}
	fixturekit.Must(t, f.s.store.ClearPause(target, runtime.PauseOwner))
	f.s.cfg.Loop.MaxSessions = 0
	fixturekit.Must(t, g.Pass(ctx))
	if _, paused := loopPause(f, stream); paused {
		t.Fatal("the loop guard acted while it is off")
	}
}
