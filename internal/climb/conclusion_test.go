package climb

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kpenfound/osmia/internal/api"
	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	"github.com/kpenfound/osmia/internal/trace"
)

type concludeHost struct {
	*fakeHost
	cmu       sync.Mutex
	upstream  string
	replays   []string
	moves     []string
	conflicts []string
}

func (h *concludeHost) Target(_ context.Context, tip string) (Target, string, error) {
	h.cmu.Lock()
	defer h.cmu.Unlock()
	return Target{Remote: "origin", Branch: "main", Commit: h.upstream}, "base0001", nil
}

func (h *concludeHost) Replay(_ context.Context, base, before, onto string, _ time.Time) (string, []string, error) {
	h.cmu.Lock()
	defer h.cmu.Unlock()
	h.replays = append(h.replays, onto)
	if len(h.conflicts) > 0 {
		return "", h.conflicts, nil
	}
	return fmt.Sprintf("rb-%s-%s", before, onto), nil, nil
}

func (h *concludeHost) Move(_ context.Context, _, from, to string) error {
	h.cmu.Lock()
	defer h.cmu.Unlock()
	h.fakeHost.mu.Lock()
	defer h.fakeHost.mu.Unlock()
	if h.fakeHost.branches[loopBranch] != from {
		return fmt.Errorf("the climb branch is at %s, not %s", h.fakeHost.branches[loopBranch], from)
	}
	h.fakeHost.branches[loopBranch] = to
	h.moves = append(h.moves, from+">"+to)
	return nil
}

func (h *concludeHost) advance(upstream string) {
	h.cmu.Lock()
	defer h.cmu.Unlock()
	h.upstream = upstream
}

func (h *concludeHost) counts() (replays, moves int) {
	h.cmu.Lock()
	defer h.cmu.Unlock()
	return len(h.replays), len(h.moves)
}

type concludeRig struct {
	*loopRig
	host      *concludeHost
	crashAt   string
	crashed   bool
	steps     []string
	published int
}

func newConcludeRig(t *testing.T, keep bool) *concludeRig {
	t.Helper()
	r := &concludeRig{loopRig: newLoopRig(t, nil)}
	r.host = &concludeHost{fakeHost: r.loopRig.host, upstream: "up00001"}
	ctx := context.Background()
	if keep {
		a := keptAttempt(1)
		a.Parent, a.Commit, a.Candidate = "base0001", "keep0001", "cand0001"
		fixturekit.Must(t, RecordAttempt(ctx, r.repo, fixturekit.Stream, a))
		r.loopRig.host.branches[loopBranch] = "keep0001"
	}
	state, err := ReadInner(r.repo, fixturekit.Stream)
	fixturekit.Must(t, err)
	h := trace.Header{Schema: "osmia.trace.transition", Version: trace.Version, ID: "fixture-conclude", Revision: 1, Project: r.repo.Project(), Workstream: fixturekit.Stream, At: fixturekit.DemoStart, Actor: InnerActor, Cause: "fixture"}
	_, err = r.repo.TransactWhileFeature(ctx, trace.Transaction{ExpectedVersion: state.Version, Transition: trace.Transition{Header: h, Subject: InnerSubject, From: InnerClimbing, To: InnerConcluding, Reason: "fixture"}}, featureBuilding)
	fixturekit.Must(t, err)
	return r
}

func (r *concludeRig) concluder() *Concluder {
	return &Concluder{
		Repository: r.repo,
		Spec:       func(config.WorkstreamID) (Spec, int, error) { return r.spec, 1, nil },
		Branch:     func(stream config.WorkstreamID) string { return "osmia/" + string(stream) },
		Host:       func(config.WorkstreamID) (ConcludeHost, error) { return r.host, nil },
		Runner: func(_ config.WorkstreamID, source Source) (Runner, error) {
			return Runner{Source: source, Engine: r.engine, AcquireSlot: r.slots.acquire, Root: filepath.Join(r.t.TempDir(), "measure"), Timeout: time.Minute}, nil
		},
		Now:     func() time.Time { return r.now },
		Publish: func(_ api.EventKind, _ config.WorkstreamID) { r.published++ },
		Step: func(name string) error {
			r.steps = append(r.steps, name)
			if r.crashAt != "" && name == r.crashAt && !r.crashed {
				r.crashed = true
				return errors.New("crash at " + name)
			}
			return nil
		},
	}
}

func (r *concludeRig) step() {
	r.t.Helper()
	fixturekit.Must(r.t, r.concluder().Reconcile(context.Background(), fixturekit.Stream))
}

func (r *concludeRig) phase() ConcludePhase {
	r.t.Helper()
	state, err := r.repo.Workflow(fixturekit.Stream, trace.ClimbConclusionSubject)
	fixturekit.Must(r.t, err)
	return ConcludePhase(state.Value)
}

func (r *concludeRig) conclusion() Conclusion {
	r.t.Helper()
	rec, found, err := ReadConclusion(r.repo, fixturekit.Stream)
	fixturekit.Must(r.t, err)
	if !found {
		r.t.Fatal("no conclusion record")
	}
	return rec
}

func (r *concludeRig) conclude() Conclusion {
	r.t.Helper()
	for range 8 {
		if r.phase() == ConcludeMeasured {
			break
		}
		r.step()
	}
	return r.conclusion()
}

func rebasedOf(before, upstream string) string { return "rb-" + before + "-" + upstream }

func TestConcludingRebasesOnceOntoThePinnedTargetAndMeasuresTheRebasedCommit(t *testing.T) {
	t.Parallel()
	r := newConcludeRig(t, true)
	rebased := rebasedOf("keep0001", "up00001")
	r.engine.outputs = map[string]string{rebased: output(88, 120)}

	rec := r.conclude()
	if replays, moves := r.host.counts(); replays != 1 || moves != 1 {
		t.Fatalf("%d replays and %d moves, want one of each", replays, moves)
	}
	if rec.Phase != ConcludeMeasured || !rec.Concluded() || rec.Target.Commit != "up00001" || rec.Target.Branch != "main" || rec.Before != "keep0001" || rec.Base != "base0001" || rec.Rebased != rebased {
		t.Fatalf("conclusion %+v", rec)
	}
	if rec.Baseline != 100 || rec.BaselineCommit != "base0001" || rec.Attempts != 1 || len(rec.Kept) != 1 || rec.Kept[0] != 1 || rec.KeptNone() {
		t.Fatalf("baseline and attempts of %+v", rec)
	}
	if !rec.Pre.Valid || rec.Pre.Score != 89 || rec.Pre.Commit != "keep0001" || rec.Pre.MeasuredOn != "cand0001" || rec.Pre.Attempt != 1 {
		t.Fatalf("the final before the rebase: %+v", rec.Pre)
	}
	if rec.Post == nil || !rec.Post.Valid || rec.Post.Score != 88 || rec.Post.Commit != rebased || rec.Post.MeasuredOn != rebased || rec.Post.Measurement == nil || len(rec.Post.Measurement.Runs) != 1 {
		t.Fatalf("the final after the rebase: %+v", rec.Post)
	}
	if tip := r.loopRig.host.branches[loopBranch]; tip != rebased {
		t.Fatalf("the climb branch is at %s, not at the rebased commit %s", tip, rebased)
	}
	if calls := r.engine.calls; len(calls) != 1 || calls[0] != rebased {
		t.Fatalf("the evaluator ran on %v, want the rebased commit once", calls)
	}
	before := len(r.steps)
	r.step()
	r.step()
	if len(r.steps) != before {
		t.Fatalf("a concluded climb took more steps: %v", r.steps[before:])
	}
	for _, id := range []string{"climb-conclusion-pinned", "climb-conclusion-prepared", "climb-conclusion-moved", "climb-conclusion-measured"} {
		if got := noticeCount(t, r, id); got != 1 {
			t.Fatalf("the chief heard %d events of %s", got, id)
		}
	}
	if r.published != 4 {
		t.Fatalf("%d events published", r.published)
	}
}

func TestARestartAfterTheRebaseDoesNotRebaseAgainWhenUpstreamHasMoved(t *testing.T) {
	t.Parallel()
	r := newConcludeRig(t, true)
	rebased := rebasedOf("keep0001", "up00001")
	r.engine.outputs = map[string]string{rebased: output(87, 120), rebasedOf("keep0001", "up00002"): output(60, 120)}

	r.step()
	r.step()
	r.step()
	if got := r.phase(); got != ConcludeMoved {
		t.Fatalf("phase %s, want %s", got, ConcludeMoved)
	}
	r.host.advance("up00002")
	rec := r.conclude()
	if replays, moves := r.host.counts(); replays != 1 || moves != 1 {
		t.Fatalf("%d replays and %d moves after upstream moved, want one of each", replays, moves)
	}
	if rec.Target.Commit != "up00001" || rec.Rebased != rebased || rec.Post == nil || rec.Post.Score != 87 || rec.Post.Commit != rebased {
		t.Fatalf("the conclusion followed the new upstream: %+v", rec)
	}
	if tip := r.loopRig.host.branches[loopBranch]; tip != rebased {
		t.Fatalf("the climb branch is at %s, want %s", tip, rebased)
	}
}

func TestACrashBetweenEachSideEffectAndItsRecordNeverRebasesTwiceOrMeasuresAnotherCommit(t *testing.T) {
	t.Parallel()
	for _, point := range []string{"climb-conclusion-resolved", "climb-conclusion-replayed", "climb-conclusion-moved", "climb-conclusion-measured"} {
		t.Run(point, func(t *testing.T) {
			t.Parallel()
			r := newConcludeRig(t, true)
			r.engine.outputs = map[string]string{rebasedOf("keep0001", "up00001"): output(88, 120), rebasedOf("keep0001", "up00002"): output(88, 120)}
			target := "up00001"
			if point == "climb-conclusion-resolved" {
				target = "up00002"
			}
			rebased := rebasedOf("keep0001", target)
			r.crashAt = point
			restarted := false
			for range 10 {
				if r.phase() == ConcludeMeasured {
					break
				}
				err := r.concluder().Reconcile(context.Background(), fixturekit.Stream)
				if err == nil {
					continue
				}
				if restarted || !strings.Contains(err.Error(), "crash at "+point) {
					t.Fatal(err)
				}
				restarted = true
				r.host.advance("up00002")
			}
			if !restarted {
				t.Fatalf("no crash at %s", point)
			}
			rec := r.conclusion()
			if !rec.Concluded() || rec.Target.Commit != target || rec.Rebased != rebased || rec.Post.Commit != rebased || rec.Post.Score != 88 {
				t.Fatalf("conclusion after a crash at %s: %+v", point, rec)
			}
			if _, moves := r.host.counts(); moves != 1 {
				t.Fatalf("the branch moved %d times", moves)
			}
			if tip := r.loopRig.host.branches[loopBranch]; tip != rebased {
				t.Fatalf("the climb branch is at %s, want %s", tip, rebased)
			}
			for _, call := range r.engine.calls {
				if call != rebased {
					t.Fatalf("the evaluator ran on %s, not on the rebased commit %s", call, rebased)
				}
			}
		})
	}
}

func TestAClimbThatKeptNothingConcludesWithTheBaselineAsItsFinal(t *testing.T) {
	t.Parallel()
	r := newConcludeRig(t, false)
	rebased := rebasedOf("base0001", "up00001")
	r.engine.outputs = map[string]string{rebased: output(101, 120)}
	rec := r.conclude()
	if !rec.KeptNone() || rec.Attempts != 0 || rec.Pre.Score != 100 || rec.Pre.Commit != "base0001" || rec.Pre.Attempt != 0 || rec.Post == nil || rec.Post.Score != 101 {
		t.Fatalf("conclusion %+v", rec)
	}
}

func TestAnInvalidMeasurementAfterTheRebaseIsRecordedAsTheFinalNotRetried(t *testing.T) {
	t.Parallel()
	r := newConcludeRig(t, true)
	rebased := rebasedOf("keep0001", "up00001")
	r.engine.outputs = map[string]string{rebased: `{"schema":1,"valid":false,"reason":"the oracle disagrees","metrics":{}}`}
	rec := r.conclude()
	if !rec.Concluded() || rec.Post.Valid || !strings.Contains(rec.Post.Reason, "the oracle disagrees") || rec.Post.Measurement == nil || rec.Post.Measurement.Valid {
		t.Fatalf("conclusion %+v", rec.Post)
	}
	if len(r.engine.calls) != 1 {
		t.Fatalf("the evaluator ran %d times", len(r.engine.calls))
	}
	if got := noticeCount(t, r, "climb-conclusion-measured"); got != 1 {
		t.Fatalf("%d notices of an invalid final", got)
	}
}

func TestAConflictingReplayHoldsTheConclusionWithOneNoticeAndMovesNothing(t *testing.T) {
	t.Parallel()
	r := newConcludeRig(t, true)
	r.host.conflicts = []string{"src/work.go"}
	r.step()
	r.step()
	for range 3 {
		r.step()
	}
	rec := r.conclusion()
	if r.phase() != ConcludeConflicted || len(rec.Conflicts) != 1 || rec.Rebased != "" || rec.Post != nil {
		t.Fatalf("phase %s, conclusion %+v", r.phase(), rec)
	}
	if replays, moves := r.host.counts(); replays != 1 || moves != 0 {
		t.Fatalf("%d replays and %d moves", replays, moves)
	}
	if tip := r.loopRig.host.branches[loopBranch]; tip != "keep0001" {
		t.Fatalf("the branch moved to %s", tip)
	}
	if got := noticeCount(t, r, "climb-conclusion-conflicted"); got != 1 {
		t.Fatalf("%d conflict notices", got)
	}
}

func TestNothingConcludesBeforeTheLastAttemptSettlesOrWhileTheClimbIsStillClimbing(t *testing.T) {
	t.Parallel()
	climbing := newLoopRig(t, nil)
	concluder := &Concluder{Repository: climbing.repo, Branch: func(config.WorkstreamID) string { return loopBranch }}
	fixturekit.Must(t, concluder.Reconcile(context.Background(), fixturekit.Stream))
	if state, err := climbing.repo.Workflow(fixturekit.Stream, trace.ClimbConclusionSubject); err != nil || state.Value != "" {
		t.Fatalf("a climbing workstream began concluding: %q %v", state.Value, err)
	}

	r := newConcludeRig(t, true)
	ctx := context.Background()
	j := Journal{Number: 2, Parent: "keep0001", Phase: PhaseStarted, StartedAt: fixturekit.DemoStart}
	doc, err := journalDocument(r.repo, fixturekit.Stream, j, fixturekit.DemoStart, "started")
	fixturekit.Must(t, err)
	h := trace.Header{Schema: "osmia.trace.transition", Version: trace.Version, ID: "fixture-attempt-2", Revision: 1, Project: r.repo.Project(), Workstream: fixturekit.Stream, At: fixturekit.DemoStart, Actor: InnerActor, Cause: "fixture"}
	_, err = r.repo.RecordDocumentsWith(ctx, []trace.Document{doc}, trace.Transaction{Transition: trace.Transition{Header: h, Subject: trace.ClimbAttemptSubject(2), To: string(PhaseStarted), Reason: "fixture"}})
	fixturekit.Must(t, err)
	r.step()
	if got := r.phase(); got != "" {
		t.Fatalf("concluding began while attempt 2 was in flight: %q", got)
	}
}

func noticeCount(t *testing.T, r *concludeRig, id string) int {
	t.Helper()
	entries, err := r.repo.Outbox(fixturekit.Stream)
	fixturekit.Must(t, err)
	n := 0
	for _, e := range entries {
		if e.Event.ID == trace.EventID(id, trace.ManagerKey) {
			n++
		}
	}
	return n
}
