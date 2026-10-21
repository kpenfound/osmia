package intakefixture

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/intake"
	"github.com/kpenfound/osmia/internal/runtime"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	"github.com/kpenfound/osmia/internal/trace"
)

func QueueFollowingTurn(t *testing.T, project config.ProjectID, now time.Time, repository *trace.Repository, stream config.WorkstreamID, agent, unit, turn string) {
	t.Helper()
	th, err := repository.Thread(stream, agent)
	Must(t, err)
	last := th.Turns[len(th.Turns)-1].Request
	h := trace.Header{Schema: "osmia.trace.turn-request", Version: trace.Version, ID: "request_" + turn, Revision: 1, Project: project, Workstream: stream, Unit: unit,
		At: now, Actor: trace.Actor{Kind: "owner", ID: "local"}, Cause: "move"}
	_, err = repository.EnqueueTurn(context.Background(), trace.TurnRequest{Header: h, AgentID: agent, ThreadID: agent, TurnID: turn, Profile: last.Profile, SystemPrompt: last.SystemPrompt, Prompt: "Carry on."})
	Must(t, err)
}

func BuiltPaused(t *testing.T, target runtime.Target, keys []string, shed func(*testing.T, string) config.WorkstreamID, pause func(), build func(*testing.T, config.WorkstreamID) trace.OperationRecord) []config.WorkstreamID {
	t.Helper()
	var streams []config.WorkstreamID
	for _, key := range keys {
		streams = append(streams, shed(t, key))
	}
	pause()
	for _, stream := range streams {
		build(t, stream)
	}
	return streams
}

func LandedCommits(t *testing.T, home, clone string, stream config.WorkstreamID, base string) []string {
	t.Helper()
	out := strings.TrimSpace(fixturekit.DemoGit(t, home, "-C", clone, "rev-list", base+".."+intake.FeatureBranch(stream)))
	if out == "" {
		return nil
	}
	return strings.Split(out, "\n")
}

func AwaitMerged(t *testing.T, turns *TurnScenario, stream config.WorkstreamID, unit string, landings func(*testing.T) []trace.OperationRecord) {
	t.Helper()
	wait, close := turns.Changes(t)
	defer close()
	deadline := time.Now().Add(fixturekit.DemoTimeout)
	for {
		got, err := turns.Repository().Workflow(stream, trace.UnitSubject(unit))
		Must(t, err)
		if got.Value == intake.UnitMerged {
			AwaitAcknowledged(t, func(t *testing.T) []trace.OperationRecord { return landings(t) })
			return
		}
		if time.Now().After(deadline) {
			data, _ := json.MarshalIndent(landings(t), "", "  ")
			t.Fatalf("unit %s of %s is %q, never merged; landings %s", unit, stream, got.Value, data)
		}
		wait()
	}
}

func DriftResolutionDiff(t *testing.T, tool func() (coreadapter.Tool, error)) string {
	t.Helper()
	diff, err := tool()
	Must(t, err)
	out, err := diff.Handle(context.Background(), json.RawMessage(`{"change":"resolution"}`))
	Must(t, err)
	var got struct{ Diff string }
	Must(t, json.Unmarshal(out, &got))
	return got.Diff
}

func PrepareRebase[B any](t *testing.T, d UnitSeedDriver[B], repository *trace.Repository, stream config.WorkstreamID, capture func(*testing.T, *trace.Repository, config.WorkstreamID, string, *coreadapter.Outcome) trace.QueuedTurn) {
	t.Helper()
	ctx := context.Background()
	for _, unit := range []string{"resume", "dedupe"} {
		b, found, err := d.Read(stream)
		Must(t, err)
		if !found {
			t.Fatal("fixture workstream is not building")
		}
		started, blocked, err := d.Start(ctx, b, unit)
		Must(t, err)
		if !started || blocked {
			t.Fatalf("fixture unit %s did not start", unit)
		}
		if unit == "resume" {
			w, _, err := d.Open(ctx, stream, unit)
			Must(t, err)
			Must(t, os.WriteFile(filepath.Join(w.Path, fixturekit.MasonWrote), []byte("package trace\n"), 0644))
			capture(t, repository, stream, unit, nil)
		}
	}
}
