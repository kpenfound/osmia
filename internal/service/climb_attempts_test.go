package service

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/kpenfound/osmia/internal/climb"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	"github.com/kpenfound/osmia/internal/servicetest/intakefixture"
	"github.com/kpenfound/osmia/internal/trace"
)

func TestFactoryContextListsAndReadsRecordedClimbAttempts(t *testing.T) {
	t.Parallel()
	c := newContestFixture(t, "climb-attempts")
	m := climb.Aggregate(climb.Expectations{Primary: climb.Expectation{Name: "wall_ms", Direction: climb.Lower}}, 1, []climb.Run{{Number: 1, Status: climb.RunScored, Output: "out", Parsed: &climb.Output{Schema: 1, Valid: true, Metrics: map[string]climb.Metric{"wall_ms": {Value: 90, Unit: "ms", Direction: climb.Lower}}}}})
	for n := 1; n <= 2; n++ {
		a := climb.Attempt{Number: n, Parent: "parent", Best: 100, Commit: "landed", Hypothesis: "cache the workload", Outcome: climb.OutcomeKept, StartedAt: fixturekit.DemoStart, FinishedAt: fixturekit.DemoStart}
		a.Measured(m, climb.Decide(climb.Candidate{Measurement: m, Best: 100, Direction: climb.Lower, MinImprovement: 5, ChecksGreen: true}))
		fixturekit.Must(t, climb.RecordAttempt(context.Background(), c.repo, c.stream, a))
	}
	tool := factoryContext(c.repo, factoryContextScope(c))

	var listed map[string]int
	fixturekit.Must(t, json.Unmarshal([]byte(intakefixture.Handle(t, tool, `{}`)), &listed))
	if listed[trace.ClimbAttemptPath(1)] != 1 || listed[trace.ClimbAttemptPath(2)] != 1 {
		t.Fatalf("documents %v", listed)
	}
	var folder map[string]int
	fixturekit.Must(t, json.Unmarshal([]byte(intakefixture.Handle(t, tool, `{"path":"climb/attempts"}`)), &folder))
	if len(folder) != 2 || folder["climb/attempts/2.json"] != 1 {
		t.Fatalf("folder %v", folder)
	}
	var doc struct {
		Content string `json:"content"`
	}
	fixturekit.Must(t, json.Unmarshal([]byte(intakefixture.Handle(t, tool, `{"path":"climb/attempts/2.json"}`)), &doc))
	var got climb.Attempt
	fixturekit.Must(t, json.Unmarshal([]byte(doc.Content), &got))
	if got.Number != 2 || got.Parent != "parent" || got.Hypothesis != "cache the workload" || got.Scores == nil || got.Scores.Primary != 90 || got.Measurement == nil || got.Measurement.Runs[0].Output != "out" {
		t.Fatalf("attempt read through factory_context: %+v", got)
	}
}
