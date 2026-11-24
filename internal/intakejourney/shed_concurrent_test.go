package intakejourney

import (
	context "context"
	slices "slices"
	strings "strings"
	testing "testing"
	time "time"

	"github.com/kpenfound/osmia/internal/intake"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	"github.com/kpenfound/osmia/internal/servicetest/journey"

	agent "github.com/kpenfound/agentcore/agent"
	config "github.com/kpenfound/osmia/internal/config"
	shed "github.com/kpenfound/osmia/internal/shed"
	mcp "github.com/modelcontextprotocol/go-sdk/mcp"
)

const overlapWait = 2 * time.Minute

func TestShedRoundsOfTwoWorkstreamsRunAtTheSameTime(t *testing.T) {
	t.Parallel()
	f := journey.NewDebateFixture(t, 1, 1)
	defer f.Stop(t)
	entered := make(chan string, 16)
	release := make(chan struct{})
	f.Member(1, 1, 1, func(ctx context.Context, req agent.Request, _ *agent.Turn, _ *mcp.ClientSession) error {
		select {
		case entered <- req.SessionDir:
		default:
		}
		select {
		case <-release:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	})
	var started []string
	running := func(stream config.WorkstreamID) bool {
		return slices.ContainsFunc(started, func(dir string) bool { return strings.Contains(dir, string(stream)) })
	}
	first := f.HandIn(t, "first", fixturekit.HandedDesign)
	second := f.HandIn(t, "second", fixturekit.HandedDesign)
	overlap := time.NewTimer(overlapWait)
	defer overlap.Stop()
	for !running(first) || !running(second) {
		select {
		case dir := <-entered:
			started = append(started, dir)
		case <-overlap.C:
			t.Fatalf("members in flight %v, want the rounds of %s and %s at once", started, first, second)
		}
	}
	third := f.HandIn(t, "third", fixturekit.HandedDesign)
	f.Await(t, third, func(feature, _ string) bool {
		return feature == intake.SketchedState || feature == intake.InShedState
	})
	for _, stream := range []config.WorkstreamID{first, second} {
		if state, err := f.Repository().Workflow(stream, intake.ShedSubject); err != nil || state.Value != "round-1" {
			t.Fatalf("workstream %s shed %q while its member is held: %v", stream, state.Value, err)
		}
	}
	close(release)
	for _, stream := range []config.WorkstreamID{first, second, third} {
		f.AwaitShed(t, stream, "heard-1")
		round := f.AcknowledgedRoundOperations(t, stream)
		if len(round) != 1 || round[0].Result == nil || round[0].Result.Outcome != "succeeded" {
			t.Fatalf("workstream %s round operations: %+v", stream, round)
		}
		records, err := shed.Records(f.Repository(), stream)
		fixturekit.Must(t, err)
		if len(records) != 1 || records[0].Failure != "" {
			t.Fatalf("workstream %s records: %+v", stream, records)
		}
	}
}
