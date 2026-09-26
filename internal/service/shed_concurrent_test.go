package service

import (
	"context"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kpenfound/busybees/core/agent"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/shed"
)

// overlapWait bounds how long a test waits for work that runs at once to be
// in flight together.
const overlapWait = 2 * time.Minute

// Two workstreams in the shed run their rounds at the same time: each round's
// member is in flight while the other's is, and while both are held another
// workstream's architect draft completes. Released, each round is heard once.
func TestShedRoundsOfTwoWorkstreamsRunAtTheSameTime(t *testing.T) {
	t.Parallel()
	f := newDebateFixture(t, 1, 1)
	defer f.stop(t)
	var mu sync.Mutex
	// Each member's session directory lies under its workstream's.
	var started []string
	release := make(chan struct{})
	f.member(1, 1, 1, func(ctx context.Context, req agent.Request, _ *agent.Turn, _ *mcp.ClientSession) error {
		mu.Lock()
		started = append(started, req.SessionDir)
		mu.Unlock()
		select {
		case <-release:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	})
	inFlight := func() []string {
		mu.Lock()
		defer mu.Unlock()
		return slices.Clone(started)
	}
	running := func(stream config.WorkstreamID) bool {
		return slices.ContainsFunc(inFlight(), func(dir string) bool { return strings.Contains(dir, string(stream)) })
	}
	first := f.handIn(t, "first", handedDesign)
	second := f.handIn(t, "second", handedDesign)
	deadline := time.Now().Add(overlapWait)
	for !running(first) || !running(second) {
		if time.Now().After(deadline) {
			t.Fatalf("members in flight %v, want the rounds of %s and %s at once", inFlight(), first, second)
		}
		time.Sleep(50 * time.Millisecond)
	}
	third := f.handIn(t, "third", handedDesign)
	f.await(t, third, func(feature, _ string) bool { return feature == SketchedState || feature == InShedState })
	for _, stream := range []config.WorkstreamID{first, second} {
		if state, err := f.repository().Workflow(stream, shedSubject); err != nil || state.Value != "round-1" {
			t.Fatalf("workstream %s shed %q while its member is held: %v", stream, state.Value, err)
		}
	}
	close(release)
	for _, stream := range []config.WorkstreamID{first, second, third} {
		f.awaitShed(t, stream, "heard-1")
		round := f.acknowledgedRoundOperations(t, stream)
		if len(round) != 1 || round[0].Result == nil || round[0].Result.Outcome != "succeeded" {
			t.Fatalf("workstream %s round operations: %+v", stream, round)
		}
		records, err := shed.Records(f.repository(), stream)
		must(t, err)
		if len(records) != 1 || records[0].Failure != "" {
			t.Fatalf("workstream %s records: %+v", stream, records)
		}
	}
}
