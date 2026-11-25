package service

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/kpenfound/osmia/internal/climb"
	"github.com/kpenfound/osmia/internal/config"
	kindregistry "github.com/kpenfound/osmia/internal/kinds"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	"github.com/kpenfound/osmia/internal/trace"
)

const climbStream = config.WorkstreamID("w_0000000000000000000000000000c1b1")

func climbReadFixture(t *testing.T) *contestFixture {
	t.Helper()
	c := newContestFixture(t, "climb-read")
	fixturekit.Must(t, c.repo.CreateWorkstreamKind(context.Background(), climbStream, "", kindregistry.HillClimb, fixturekit.DemoStart, trace.Actor{Kind: "owner", ID: "local"}))
	m := climb.Aggregate(climb.Expectations{Primary: climb.Expectation{Name: "wall_ms", Direction: climb.Lower}}, 1, []climb.Run{{Number: 1, Status: climb.RunScored, Output: "out", Parsed: &climb.Output{Schema: 1, Valid: true, Metrics: map[string]climb.Metric{"wall_ms": {Value: 90, Unit: "ms", Direction: climb.Lower}}}}})
	for n := 1; n <= 2; n++ {
		a := climb.Attempt{Number: n, Parent: "parent", Best: 100, Commit: "landed", Hypothesis: "cache the workload", Outcome: climb.OutcomeKept, StartedAt: fixturekit.DemoStart, FinishedAt: fixturekit.DemoStart}
		a.Measured(m, climb.Decide(climb.Candidate{Measurement: m, Best: 100, Direction: climb.Lower, MinImprovement: 5, ChecksGreen: true}))
		fixturekit.Must(t, climb.RecordAttempt(context.Background(), c.repo, climbStream, a))
	}
	return c
}

func TestClimbAttemptLogEndpointReturnsTheRecordedAttempts(t *testing.T) {
	t.Parallel()
	c := climbReadFixture(t)
	req := httptest.NewRequest(http.MethodGet, Prefix+"/climb/"+string(climbStream)+"/attempts", nil)
	rec := httptest.NewRecorder()
	c.f.s.handle(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	body := rec.Body.String()
	for _, want := range []string{`"workstream":"` + string(climbStream) + `"`, `"number":1`, `"number":2`, `"hypothesis":"cache the workload"`, `"parent":"parent"`, `"output":"out"`} {
		if !strings.Contains(body, want) {
			t.Errorf("the attempt log lacks %s: %s", want, body)
		}
	}
	got, api := c.f.s.climbAttempts(string(climbStream))
	if api != nil || len(got.Attempts) != 2 || got.Attempts[1].Number != 2 {
		t.Fatalf("%+v, %v", got, api)
	}
}

func TestClimbReadsAreRefusedForAFeatureWorkstream(t *testing.T) {
	t.Parallel()
	c := newContestFixture(t, "climb-read-feature")
	for _, read := range []func(string) *APIError{
		func(id string) *APIError { _, api := c.f.s.climbAttempts(id); return api },
		func(id string) *APIError { _, api := c.f.s.climbProgress(id); return api },
	} {
		api := read(string(c.stream))
		if api == nil || api.Code != Conflict || !strings.Contains(api.Message, "feature") {
			t.Fatalf("a feature workstream's climb read: %+v", api)
		}
	}
	if _, api := c.f.s.climbAttempts("not-a-workstream"); api == nil || api.Code != Validation {
		t.Fatalf("a malformed ID: %+v", api)
	}
}

func TestClimbProgressWaitsForTheSealedClimbSpec(t *testing.T) {
	t.Parallel()
	c := climbReadFixture(t)
	if _, api := c.f.s.climbProgress(string(climbStream)); api == nil || api.Code != Conflict {
		t.Fatalf("an unsealed climb: %+v", api)
	}
	cfg, _ := c.f.s.runtimeOf(c.f.project)
	view := climbProgressView(c.repo, cfg, climbStream, time.Now())
	if view.Panel != "climb" || view.Summary != "" {
		t.Fatalf("an unsealed climb's list view: %+v", view)
	}
}

func TestStatusShowsAHillClimbWorkstreamsKindAndLeavesFeatureAlone(t *testing.T) {
	t.Parallel()
	c := climbReadFixture(t)
	list, _, api := c.f.s.projectStatuses(&activeProject{id: c.f.project, repository: c.repo})
	if api != nil {
		t.Fatal(api)
	}
	var climbing, feature int
	for _, w := range list {
		switch w.status.Workstream {
		case climbStream:
			climbing++
			if w.status.WorkKind != kindregistry.HillClimb || w.status.Progress == nil || w.status.Progress.Panel != "climb" {
				t.Fatalf("the climb: %+v", w.status)
			}
		case c.stream:
			feature++
			if w.status.WorkKind != "" || w.status.Progress != nil {
				t.Fatalf("a feature workstream shows climb fields: %+v", w.status)
			}
		}
	}
	if climbing != 1 || feature != 1 {
		t.Fatalf("listed %d climbs and %d features", climbing, feature)
	}
}
