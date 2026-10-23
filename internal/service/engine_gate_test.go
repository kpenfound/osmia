package service

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"testing/synctest"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/engine"
	"github.com/kpenfound/osmia/internal/thread"
)

type gateEngine struct {
	run func(context.Context) error
}

func (f gateEngine) Applicable(string) error           { return nil }
func (f gateEngine) Restart(ctx context.Context) error { return f.run(ctx) }
func (f gateEngine) Prune(ctx context.Context) error   { return f.run(ctx) }

var gateActions = []struct {
	name string
	run  func(*Service, context.Context, EngineControl) error
}{
	{"restart", (*Service).RestartEngine},
	{"prune", (*Service).PruneEngine},
}

func TestEngineGateNamesAdmittedChecksOnly(t *testing.T) {
	t.Parallel()
	for _, action := range gateActions {
		t.Run(action.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				p := &checkSlotPool{limit: 1}
				s := &Service{checkSlots: p}
				held := CheckSlotRun{Project: project, Workstream: stream, Unit: "admitted"}
				release, err := p.acquire(t.Context(), held)
				must(t, err)
				ctx, cancel := context.WithCancel(t.Context())
				queued := queuedSlot(ctx, p, CheckSlotRun{Project: project, Workstream: sibling, Review: 17})
				synctest.Wait()
				before := s.CheckSlots()
				if before.Limit != 1 || before.Used != 1 || len(before.Waiting) != 1 || !reflect.DeepEqual(before.Active, []CheckSlotRun{held}) {
					t.Fatalf("admission snapshot: %+v", before)
				}
				commands := 0
				control := engine.New(config.ResolvedDagger{Mode: config.DaggerEngineManaged, Version: "v0.20.5"}, engine.Options{
					LookPath: func(s string) (string, error) { return s, nil },
					Command: func(context.Context, string, []string, ...string) (string, error) {
						commands++
						return "[]", nil
					},
				})
				err = action.run(s, t.Context(), control)
				var busy *EngineBusyError
				if !errors.As(err, &busy) || busy.Action != action.name || !reflect.DeepEqual(busy.Checks, []CheckSlotRun{held}) || len(busy.Turns) != 0 {
					t.Fatalf("busy refusal: %#v (%v)", busy, err)
				}
				if !strings.Contains(err.Error(), "admitted") || strings.Contains(err.Error(), string(sibling)) || strings.Contains(err.Error(), "review 17") || commands != 0 {
					t.Fatalf("refusal %v, commands %d", err, commands)
				}
				if after := s.CheckSlots(); !reflect.DeepEqual(after, before) {
					t.Fatalf("refusal changed capacity: %+v -> %+v", before, after)
				}
				cancel()
				synctest.Wait()
				if r := <-queued; r != nil {
					t.Fatal("cancelled waiter acquired capacity")
				}
				release()
				release()
				if st := s.CheckSlots(); st.Used != 0 || len(st.Active) != 0 || len(st.Waiting) != 0 {
					t.Fatalf("released run retained: %+v", st)
				}
				called := false
				must(t, action.run(s, t.Context(), gateEngine{run: func(context.Context) error { called = true; return nil }}))
				if !called {
					t.Fatal("idle operation did not proceed")
				}
			})
		})
	}
}

func TestEngineGatePassesNotApplicableThroughWhileBusy(t *testing.T) {
	t.Parallel()
	for _, mode := range []config.DaggerEngineMode{config.DaggerEngineAuto, config.DaggerEngineAddress, config.DaggerEngineCloud} {
		for _, action := range gateActions {
			p := &checkSlotPool{limit: 1}
			release, err := p.acquire(t.Context(), CheckSlotRun{Unit: "busy"})
			must(t, err)
			control := engine.New(config.ResolvedDagger{Mode: mode}, engine.Options{Command: func(context.Context, string, []string, ...string) (string, error) {
				t.Fatal("not-applicable operation ran a command")
				return "", nil
			}})
			err = action.run(&Service{checkSlots: p}, t.Context(), control)
			var refusal *engine.NotApplicableError
			if !errors.As(err, &refusal) || refusal.Mode != mode || refusal.Action != action.name {
				t.Fatalf("mode %s action %s: %v", mode, action.name, err)
			}
			release()
		}
	}
}

func TestEngineGateRetainsEveryActiveIdentity(t *testing.T) {
	t.Parallel()
	p := &checkSlotPool{limit: 3}
	runs := []CheckSlotRun{
		{Project: project, Workstream: stream, Unit: "implement"},
		{Project: project, Workstream: sibling, Review: 3},
		{Project: project, Workstream: otherStream, Session: true},
	}
	for _, run := range runs {
		release, err := p.acquire(t.Context(), run)
		must(t, err)
		t.Cleanup(release)
	}
	turn := EngineTurn{Project: project, Workstream: stream, Unit: "mason-unit", Agent: "mason", Turn: "implementation"}
	release, err := p.startEngineTurn(t.Context(), turn)
	must(t, err)
	t.Cleanup(release)
	control := gateEngine{run: func(context.Context) error { t.Fatal("busy engine was touched"); return nil }}
	for _, action := range gateActions {
		err := action.run(&Service{checkSlots: p}, t.Context(), control)
		var busy *EngineBusyError
		if !errors.As(err, &busy) || len(busy.Checks) != 3 || !reflect.DeepEqual(busy.Turns, []EngineTurn{turn}) {
			t.Fatalf("combined blockers: %v", err)
		}
		for _, run := range runs {
			found := false
			for _, active := range busy.Checks {
				found = found || active == run
			}
			if !found {
				t.Fatalf("identity %+v missing from %+v", run, busy.Checks)
			}
		}
		for _, name := range []string{"unit implement", "review 3", "session", "unit mason-unit", "turn implementation"} {
			if !strings.Contains(err.Error(), name) {
				t.Fatalf("refusal omits %s: %v", name, err)
			}
		}
		// A caller may retain and edit its error snapshot without changing
		// the gate's registry.
		busy.Checks[0] = CheckSlotRun{}
		busy.Turns[0] = EngineTurn{}
	}
}

func TestEngineGateTracksDispatchedDaggerTurns(t *testing.T) {
	t.Parallel()
	for _, dagger := range []bool{false, true} {
		t.Run(map[bool]string{false: "without Dagger", true: "with Dagger"}[dagger], func(t *testing.T) {
			cfg, repo, clock := hardPauseRepository(t, 1)
			if dagger {
				role := cfg.Roles[masonRole]
				role.Dagger = &config.Dagger{Engine: "docker-container://fake", Version: "v0.20.5"}
				cfg.Roles[masonRole] = role
			}
			entered, finish := make(chan struct{}), make(chan struct{})
			backend := turnFunc(func(context.Context, coreadapter.PreparedTurn) (coreadapter.SessionResult, error) {
				close(entered)
				<-finish
				return coreadapter.SessionResult{Session: coreadapter.BackendSession{Backend: "fake", ID: "engine-turn"}, FinalResponse: "Ready"}, nil
			})
			s, adapter := hardPauseService(t, cfg, repo, clock, backend)
			s.checkSlots = &checkSlotPool{limit: 1}
			adapter.cfg = cfg
			op, err := thread.TurnOperation(project, "engine-turn", thread.TurnInput{Workstream: stream, Agent: "agent-0", Turn: "t-0"})
			must(t, err)
			finished := make(chan error, 1)
			go func() { _, err := adapter.Apply(t.Context(), op); finished <- err }()
			awaitCheck(t, entered)
			commands := 0
			control := gateEngine{run: func(context.Context) error { commands++; return nil }}
			for _, action := range gateActions {
				err := action.run(s, t.Context(), control)
				if dagger {
					var busy *EngineBusyError
					want := EngineTurn{Project: project, Workstream: stream, Agent: "agent-0", Turn: "t-0"}
					if !errors.As(err, &busy) || !reflect.DeepEqual(busy.Turns, []EngineTurn{want}) || len(busy.Checks) != 0 || commands != 0 {
						t.Fatalf("Dagger turn refusal: %v, commands %d", err, commands)
					}
					for _, name := range []string{string(project), string(stream), "agent-0", "t-0"} {
						if !strings.Contains(err.Error(), name) {
							t.Fatalf("refusal omits %s: %v", name, err)
						}
					}
				} else {
					must(t, err)
				}
			}
			close(finish)
			must(t, awaitCheck(t, finished))
			must(t, s.RestartEngine(t.Context(), control))
			if commands != map[bool]int{false: 3, true: 1}[dagger] {
				t.Fatalf("commands %d", commands)
			}
		})
	}
}

func TestEngineGateExcludesStartsDuringMaintenance(t *testing.T) {
	t.Parallel()
	for _, action := range gateActions {
		t.Run(action.name, func(t *testing.T) {
			cfg, repo, clock := hardPauseRepository(t, 1)
			role := cfg.Roles[masonRole]
			role.Dagger = &config.Dagger{Engine: "docker-container://fake", Version: "v0.20.5"}
			cfg.Roles[masonRole] = role
			synctest.Test(t, func(t *testing.T) {
				backend := turnFunc(func(context.Context, coreadapter.PreparedTurn) (coreadapter.SessionResult, error) {
					return coreadapter.SessionResult{Session: coreadapter.BackendSession{Backend: "fake", ID: "engine-turn"}, FinalResponse: "Ready"}, nil
				})
				s, adapter := hardPauseService(t, cfg, repo, clock, backend)
				s.checkSlots = &checkSlotPool{limit: 1}
				adapter.cfg = cfg
				inside, finish := make(chan struct{}), make(chan struct{})
				operationDone := make(chan error, 1)
				go func() {
					operationDone <- action.run(s, t.Context(), gateEngine{run: func(context.Context) error {
						close(inside)
						<-finish
						return nil
					}})
				}()
				<-inside
				check := queuedSlot(t.Context(), s.checkSlots, CheckSlotRun{Project: project, Workstream: sibling, Unit: "new-check"})
				op, err := thread.TurnOperation(project, "maintenance-turn", thread.TurnInput{Workstream: stream, Agent: "agent-0", Turn: "t-0"})
				must(t, err)
				turnDone := make(chan error, 1)
				go func() { _, err := adapter.Apply(t.Context(), op); turnDone <- err }()
				synctest.Wait()
				if st := s.CheckSlots(); st.Used != 0 || len(st.Active) != 0 || len(st.Waiting) != 1 {
					t.Fatalf("check started during maintenance: %+v", st)
				}
				th, err := repo.Thread(stream, "agent-0")
				must(t, err)
				q := th.Turns[0]
				if q.Claim != nil || q.Response != nil || len(q.Attempts) != 0 || !q.CompletedAt.IsZero() {
					t.Fatalf("maintenance wait recorded turn evidence: %+v", q)
				}
				select {
				case <-turnDone:
					t.Fatal("turn settled during maintenance")
				default:
				}
				close(finish)
				synctest.Wait()
				must(t, <-operationDone)
				release := <-check
				if release == nil {
					t.Fatal("check did not proceed")
				}
				release()
				must(t, <-turnDone)
				th, err = repo.Thread(stream, "agent-0")
				must(t, err)
				q = th.Turns[0]
				if q.Response == nil || q.Response.Failure != "" || len(q.Attempts) != 1 || q.CompletedAt.IsZero() {
					t.Fatalf("turn did not proceed cleanly: %+v", q)
				}
			})
		})
	}
}

func TestEngineGateCancellationAndFailedMaintenanceReleaseWaiters(t *testing.T) {
	t.Parallel()
	for _, action := range gateActions {
		t.Run(action.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				p := &checkSlotPool{limit: 1}
				s := &Service{checkSlots: p}
				ctx, cancel := context.WithCancel(t.Context())
				inside := make(chan struct{})
				operationDone := make(chan error, 1)
				go func() {
					operationDone <- action.run(s, ctx, gateEngine{run: func(ctx context.Context) error {
						close(inside)
						<-ctx.Done()
						return ctx.Err()
					}})
				}()
				<-inside
				waiting, cancelWait := context.WithCancel(t.Context())
				check := queuedSlot(waiting, p, CheckSlotRun{Unit: "cancelled"})
				turn := make(chan error, 1)
				go func() { _, err := p.startEngineTurn(waiting, EngineTurn{Turn: "cancelled"}); turn <- err }()
				secondAction := make(chan error, 1)
				go func() {
					secondAction <- s.RestartEngine(waiting, gateEngine{run: func(context.Context) error {
						t.Error("overlapping engine operation")
						return nil
					}})
				}()
				synctest.Wait()
				cancelWait()
				synctest.Wait()
				if release := <-check; release != nil {
					t.Fatal("cancelled check was admitted")
				}
				if err := <-turn; !errors.Is(err, context.Canceled) {
					t.Fatalf("cancelled turn start: %v", err)
				}
				if err := <-secondAction; !errors.Is(err, context.Canceled) {
					t.Fatalf("cancelled engine operation: %v", err)
				}
				next := queuedSlot(t.Context(), p, CheckSlotRun{Unit: "next"})
				synctest.Wait()
				cancel()
				synctest.Wait()
				if err := <-operationDone; !errors.Is(err, context.Canceled) {
					t.Fatalf("cancelled maintenance: %v", err)
				}
				release := <-next
				if release == nil {
					t.Fatal("failed maintenance did not release waiter")
				}
				release()
				must(t, s.RestartEngine(t.Context(), gateEngine{run: func(context.Context) error { return nil }}))
			})
		})
	}
}
