package service

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"testing"
	"time"

	"github.com/kpenfound/agentcore/agent"
	"github.com/kpenfound/osmia/internal/runtime"
	"github.com/kpenfound/osmia/internal/trace"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestDailyBudgetHoldsAdmissionAfterAMasonCompletesDuringScheduling(t *testing.T) {
	t.Parallel()
	f, masons, _ := newParallelMasonFixtureCounting(t, 1, 4, reliabilityPlan)
	defer f.stop(t)
	f.stop(t)
	path := filepath.Join(f.opts.Config.Root, "config.toml")
	configText, err := os.ReadFile(path)
	must(t, err)
	must(t, os.WriteFile(path, append(configText, []byte("\n[budget]\nper_day = \"1.00\"\n")...), 0600))
	f.opts.Location = time.UTC
	f.start(t)

	release := make(chan struct{})
	masons.mu.Lock()
	masons.play[masonTurnID("upload")] = reportDone("Chunks are sent")
	masons.mu.Unlock()
	f.engine.mu.Lock()
	f.engine.turns[masonTurnID("upload")] = func(ctx context.Context, req agent.Request, verified *agent.Turn, tools *mcp.ClientSession) (*agent.Result, error) {
		select {
		case <-release:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		result, err := masons.turn(ctx, req, verified, tools)
		if result != nil {
			result.CostUSD, result.CostKnown = 1.25, true
		}
		return result, err
	}
	f.engine.mu.Unlock()
	stream, _ := f.builtAs(t, "budget-admission")
	f.awaitUnit(t, stream, "upload", UnitImplementing)

	// Let the turn complete after this pass's budget observation and before
	// its mason step. Completing the turn must remain independent of the pass.
	_, projects := f.s.runtimes()
	active := projects[0]
	next := *active.pipeline.current.Load()
	next.hooks = slices.Clone(next.hooks)
	completed, checked := false, make(chan struct{}, 1)
	for i, hook := range next.hooks {
		switch hook.name {
		case "daily-budget":
			next.hooks[i].pass = func(ctx context.Context) error {
				if err := hook.pass(ctx); err != nil || completed {
					return err
				}
				close(release)
				timer := time.NewTimer(demoTimeout)
				defer timer.Stop()
				tick := time.NewTicker(time.Millisecond)
				defer tick.Stop()
				for {
					th, err := f.repository().Thread(stream, masonAgent("upload"))
					if err != nil {
						return err
					}
					if len(th.Turns) > 0 && !th.Turns[0].CompletedAt.IsZero() {
						completed = true
						return nil
					}
					select {
					case <-ctx.Done():
						return ctx.Err()
					case <-timer.C:
						return fmt.Errorf("mason result did not complete during the scheduling step")
					case <-tick.C:
					}
				}
			}
		case "masons":
			next.hooks[i].pass = func(ctx context.Context) error {
				err := hook.pass(ctx)
				select {
				case checked <- struct{}{}:
				default:
				}
				return err
			}
		}
	}
	active.pipeline.next.Store(&next)
	f.repository().WakeWorkflow()
	select {
	case <-checked:
	case <-time.After(demoTimeout):
		t.Fatal("mason admission did not complete")
	}
	want := f.deferred(t, stream, "audit", UnitDispatch{Reason: DeferPaused, Pause: &runtime.Pause{Target: factoryTarget}, Message: "Waits while a factory pause is in force, set by daily-budget."})
	if got := unitStatus(t, f, stream, "audit"); !reflect.DeepEqual(got, want) {
		t.Fatalf("audit after concurrent completion: %+v, want %+v", got, want)
	}
	if got := starts(t, f, stream); !slices.Equal(got, []string{trace.UnitSubject("upload")}) {
		t.Fatalf("units started under the budget pause: %v", got)
	}
}
