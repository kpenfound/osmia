package executionjourney

import (
	context "context"
	"github.com/kpenfound/osmia/internal/intake"
	"github.com/kpenfound/osmia/internal/service"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	"github.com/kpenfound/osmia/internal/servicetest/intakefixture"
	os "os"
	filepath "path/filepath"
	testing "testing"

	trace "github.com/kpenfound/osmia/internal/trace"
)

func TestDesignProductionAbandonAcrossMasonEnqueue(t *testing.T) {
	t.Parallel()
	for _, step := range []string{"mason-implementing", "mason-turn-queued"} {
		t.Run(step, func(t *testing.T) {
			ctx := context.Background()
			gate := &intakefixture.ChangeRaceGate{Step: step, Reached: make(chan struct{}), Released: make(chan struct{})}
			cleanup := &lifecycleCleanupGate{reached: make(chan struct{}), released: make(chan struct{})}
			t.Cleanup(gate.Release)
			t.Cleanup(cleanup.release)
			f := newConfiguredProductionDesignFixture(t, productionDesignConfiguration{
				wait: lifecycleAwait,
				options: func(opts *Options) {
					file, err := os.OpenFile(filepath.Join(opts.Config.Root, "config.toml"), os.O_APPEND|os.O_WRONLY, 0600)
					fixturekit.Must(t, err)
					_, err = file.WriteString("[roles.mason]\nsandbox = \"container\"\nimage = \"fixture-image\"\n")
					fixturekit.Must(t, err)
					fixturekit.Must(t, file.Close())
				},
				checkpoint: func(ctx context.Context, step string) error {
					if err := gate.Checkpoint(ctx, step); err != nil {
						return err
					}
					return cleanup.checkpoint(ctx, step)
				},
			})
			defer f.stop(t)
			f.startOwner(t)
			packet := f.awaitPacket(t)
			address, unit := f.stream1, "accept"
			gate.Step = step + ":" + string(address) + ":" + unit
			gate.Armed.Store(true)
			_, err := f.c.Ratify(ctx, f.origin, packet.Packet.Revision.Spec, packet.Packet.Revision.Plan, packet.ProjectSpecRevision)
			fixturekit.Must(t, err)
			gate.Await(t)
			repo, err := f.s.repository(f.project)
			fixturekit.Must(t, err)
			state, err := repo.Workflow(address, trace.UnitSubject(unit))
			fixturekit.Must(t, err)
			if state.Value != intake.UnitImplementing {
				t.Fatal("checkpoint preceded the implementing transition")
			}
			if step == "mason-implementing" {
				if _, err := repo.Thread(address, intake.MasonAgent(unit)); !os.IsNotExist(err) {
					t.Fatalf("first request already exists before enqueue: %v", err)
				}
			} else {
				th, err := repo.Thread(address, intake.MasonAgent(unit))
				fixturekit.Must(t, err)
				if len(th.Turns) != 1 || th.Turns[0].Claim != nil || th.Turns[0].Response != nil || len(th.Turns[0].Attempts) != 0 {
					t.Fatalf("checkpoint did not hold an unclaimed request: %+v", th)
				}
			}
			_, err = f.c.Abandon(ctx, address, "Owner cancelled the member at its enqueue boundary.")
			fixturekit.Must(t, err)
			_, err = f.c.Archive(ctx, address)
			fixturekit.Must(t, err)
			gate.Release()
			cleanup.await(t)
			th, err := repo.Thread(address, intake.MasonAgent(unit))
			fixturekit.Must(t, err)
			if len(th.Turns) != 1 {
				t.Fatalf("late enqueue duplicated the request: %+v", th)
			}
			q := th.Turns[0]
			if q.Request.TurnID != service.TestMasonTurnID(unit) || q.CompletedAt.IsZero() || q.Response == nil || !q.Response.Result.Cancelled || q.Response.Actor != intake.AbandonActor || q.Response.Failure != intake.CancelReason || len(q.Attempts) != 0 || th.Active != "" {
				t.Fatalf("late request did not settle as cancelled in the same session: %+v", th)
			}
			responses, err := trace.Read[trace.TurnResponse](repo, address)
			fixturekit.Must(t, err)
			count := 0
			for _, response := range responses {
				if response.TurnID == q.Request.TurnID && response.AgentID == q.Request.AgentID {
					count++
				}
			}
			if count != 1 {
				t.Fatalf("late request has %d cancellation responses", count)
			}
			cleanup.release()
			lifecycleAwait(t, f, "same-session late-request archive cleanup", func() bool {
				a, found := intakefixture.ArchiveRecord(t, f.s.store, address)
				return found && !a.CleanedAt.IsZero()
			})
			project := repo.Project()
			root := f.s.current().Root
			f.stop(t)
			f.start(t)
			if gone, err := trace.WorkstreamTraceGone(root, project, address); err != nil || !gone {
				t.Fatalf("restart recreated retired member history: gone=%v error=%v", gone, err)
			}
		})
	}
}
