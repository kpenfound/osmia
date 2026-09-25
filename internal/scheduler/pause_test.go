package scheduler

import (
	"context"
	"slices"
	"sync"
	"testing"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/runtime"
	"github.com/kpenfound/osmia/internal/trace"
)

func TestHeldCoversPausedScopes(t *testing.T) {
	const other config.ProjectID = "p_00000000000000000000000000000002"
	const sibling config.WorkstreamID = "w_00000000000000000000000000000002"
	worker := Candidate{Workstream: stream, Thread: trace.Thread{Identity: trace.Agent{Role: "mason"}}}
	chief := Candidate{Workstream: stream, Thread: trace.Thread{Identity: trace.Agent{Role: trace.ChiefOfStaff}}}
	pause := func(scope string, p config.ProjectID, w config.WorkstreamID) runtime.Pause {
		return runtime.Pause{Target: runtime.Target{Scope: scope, Project: p, Workstream: w}, Mode: "soft", Source: "owner"}
	}
	for _, tc := range []struct {
		name   string
		pauses []runtime.Pause
		held   bool
	}{
		{"none", nil, false},
		{"factory", []runtime.Pause{pause("factory", "", "")}, true},
		{"project", []runtime.Pause{pause("project", project, "")}, true},
		{"other project", []runtime.Pause{pause("project", other, "")}, false},
		{"workstream", []runtime.Pause{pause("workstream", project, stream)}, true},
		{"sibling workstream", []runtime.Pause{pause("workstream", project, sibling)}, false},
		{"same workstream in other project", []runtime.Pause{pause("workstream", other, stream)}, false},
		{"after an unrelated pause", []runtime.Pause{pause("project", other, ""), pause("workstream", project, stream)}, true},
		{"unknown scope", []runtime.Pause{pause("unit", project, stream)}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := Held(tc.pauses, project, worker); got != tc.held {
				t.Fatalf("worker held %v, want %v", got, tc.held)
			}
			if Held(tc.pauses, project, chief) {
				t.Fatal("chief-of-staff turn held")
			}
			if got := Paused(tc.pauses, project, stream); got != tc.held {
				t.Fatalf("workstream paused %v, want %v", got, tc.held)
			}
			if got, ok := Pausing(tc.pauses, project, stream); ok != tc.held || ok && got != tc.pauses[len(tc.pauses)-1] {
				t.Fatalf("pausing %+v %v, want the covering pause %v", got, ok, tc.held)
			}
		})
	}
}

func TestStoppingCoversHardPausesOnly(t *testing.T) {
	const other config.ProjectID = "p_00000000000000000000000000000002"
	const sibling config.WorkstreamID = "w_00000000000000000000000000000002"
	pause := func(mode, scope string, p config.ProjectID, w config.WorkstreamID) runtime.Pause {
		return runtime.Pause{Target: runtime.Target{Scope: scope, Project: p, Workstream: w}, Mode: mode, Source: "owner", Reason: mode + " " + scope}
	}
	for _, tc := range []struct {
		name   string
		pauses []runtime.Pause
		want   int
	}{
		{"none", nil, -1},
		{"soft factory", []runtime.Pause{pause("soft", "factory", "", "")}, -1},
		{"hard factory", []runtime.Pause{pause("hard", "factory", "", "")}, 0},
		{"hard project", []runtime.Pause{pause("hard", "project", project, "")}, 0},
		{"hard other project", []runtime.Pause{pause("hard", "project", other, "")}, -1},
		{"hard workstream", []runtime.Pause{pause("hard", "workstream", project, stream)}, 0},
		{"hard sibling workstream", []runtime.Pause{pause("hard", "workstream", project, sibling)}, -1},
		{"hard same workstream in other project", []runtime.Pause{pause("hard", "workstream", other, stream)}, -1},
		{"hard workstream under soft factory", []runtime.Pause{pause("soft", "factory", "", ""), pause("hard", "workstream", project, stream)}, 1},
		{"soft workstream under hard project", []runtime.Pause{pause("hard", "project", project, ""), pause("soft", "workstream", project, stream)}, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := Stopping(tc.pauses, project, stream)
			if ok != (tc.want >= 0) || ok && got != tc.pauses[tc.want] {
				t.Fatalf("stopping %+v %v, want pause %d", got, ok, tc.want)
			}
			if ok && !Paused(tc.pauses, project, stream) {
				t.Fatal("a stopping pause does not hold new turns")
			}
		})
	}
}

func TestPauseHoldsWorkerTurnsWhileChiefOfStaffRuns(t *testing.T) {
	ctx := context.Background()
	f, repo := setup(t, "alpha")
	defer repo.Close()
	_, err := repo.EnsureChiefOfStaff(ctx, stream, f.clock.Now(), owner)
	must(t, err)
	queueChief := func(turn string) {
		t.Helper()
		req := trace.TurnRequest{Header: trace.Header{Schema: "osmia.trace.turn-request", Version: 1, Revision: 1, ID: "request_chief_" + turn, Project: project, Workstream: stream, At: f.clock.Now(), Actor: owner, Cause: "message_" + turn, Depth: 1},
			AgentID: trace.ChiefOfStaff, ThreadID: trace.ChiefOfStaff, TurnID: turn, Profile: coreadapter.Profile{Name: "default", Backend: "fake", Model: "test"}, Prompt: "Message " + turn}
		_, err := repo.EnqueueTurn(ctx, req)
		must(t, err)
	}
	f.queue(t, repo, "alpha", "one")
	f.queue(t, repo, "alpha", "two")
	queueChief("one")

	var mu sync.Mutex
	var pauses []runtime.Pause
	var ran []string
	paused := runtime.Pause{Target: runtime.Target{Scope: "workstream", Project: project, Workstream: stream}, Mode: "soft", Source: "owner"}
	turns := turnsFunc(func(_ context.Context, p coreadapter.PreparedTurn) (coreadapter.SessionResult, error) {
		mu.Lock()
		defer mu.Unlock()
		ran = append(ran, p.Scope.Role+"/"+p.Scope.Turn)
		if p.Scope.Role == "mason" && p.Scope.Turn == "one" {
			// The pause arrives while this turn is in flight.
			pauses = []runtime.Pause{paused}
		}
		return result(p.Scope.Turn).Value, nil
	})
	admit := func(_ context.Context, c Candidate) (bool, error) {
		mu.Lock()
		defer mu.Unlock()
		return !Held(pauses, project, c), nil
	}
	ranNow := func() []string {
		mu.Lock()
		defer mu.Unlock()
		return slices.Clone(ran)
	}
	c := f.controller(t, repo, turns, admit)

	must(t, c.Pass(ctx))
	must(t, c.Pass(ctx))
	queueChief("two")
	must(t, c.Pass(ctx))
	must(t, c.Pass(ctx))
	got := ranNow()
	slices.Sort(got)
	if want := []string{"chief_of_staff/one", "chief_of_staff/two", "mason/one"}; !slices.Equal(got, want) {
		t.Fatalf("paused runs %v, want %v", got, want)
	}
	th, err := repo.Thread(stream, "alpha")
	must(t, err)
	if th.Turns[0].CompletedAt.IsZero() || th.Turns[1].Claim != nil || !th.Turns[1].CompletedAt.IsZero() {
		t.Fatalf("paused thread: %+v", th)
	}
	if got := dispatched(t, repo); slices.Contains(got, "alpha/two") {
		t.Fatalf("held turn dispatched: %v", got)
	}

	mu.Lock()
	pauses = nil
	mu.Unlock()
	must(t, c.Pass(ctx))
	if got := ranNow(); got[len(got)-1] != "mason/two" || len(got) != 4 {
		t.Fatalf("resumed runs %v", got)
	}
}
