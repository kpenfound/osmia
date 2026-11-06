package delivery

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/kpenfound/agentcore/jev/jevtest"
	"github.com/kpenfound/agentcore/jev/systemone"
	"github.com/kpenfound/osmia/internal/api"
	checkrun "github.com/kpenfound/osmia/internal/checks"
	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/jev"
	"github.com/kpenfound/osmia/internal/servicetest/daggerfixture"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	"github.com/kpenfound/osmia/internal/trace"
)

// engineFailure is the result of an attempt that exits without reporting a
// failed check.
var engineFailure = api.CheckResult{ExitCode: 1, Output: "Error: the engine session was lost\n"}

func recordedFinalRuns(t *testing.T, repository *trace.Repository, k int) []string {
	t.Helper()
	docs, err := trace.Read[trace.Document](repository, stream)
	fixturekit.Must(t, err)
	var records []string
	for _, d := range docs {
		if d.Path == FinalChecksPath(k) {
			records = append(records, d.Content)
		}
	}
	return records
}

func TestFinalChecksIncompleteLocallyRunOnCloudInTheSameCall(t *testing.T) {
	t.Parallel()
	local := &fixturekit.FakeChecks{Outcomes: []fixturekit.CheckOutcome{{Block: true}}}
	cloud := &fixturekit.FakeChecks{Outcomes: fixturekit.Passing()}
	reviewer, cfg, report := finalChecksFixture(t, local)
	reviewer.S.CloudChecks = cloud
	cfg.Project.ChecksTimeout = "50ms"
	run, err := reviewer.FinalChecks(t.Context(), cfg, stream, report)
	fixturekit.Must(t, err)
	if run.Status != checkrun.ChecksPassed || len(local.Runs()) != 1 || len(cloud.Runs()) != 1 {
		t.Fatalf("run %+v, local %q cloud %q", run, local.Runs(), cloud.Runs())
	}
	records := recordedFinalRuns(t, reviewer.Repository, 1)
	if len(records) != 1 {
		t.Fatalf("records %q", records)
	}
	var saved FinalCheckRun
	fixturekit.Must(t, json.Unmarshal([]byte(records[0]), &saved))
	cloudRuns := saved.Engines.CloudRuns()
	if len(saved.Engines.Runs) != 1 || len(cloudRuns) != 1 || !cloudRuns[0].EveryCheck || cloudRuns[0].Reason != api.CloudAfterTimeout || !strings.Contains(records[0], `"engines"`) {
		t.Fatalf("attribution %+v\n%s", saved.Engines, records[0])
	}
	if evidence := FinalCheckEvidence(run); !strings.Contains(evidence, "the Dagger Cloud engine ran every check") || !strings.Contains(evidence, "time budget") {
		t.Fatalf("evidence:\n%s", evidence)
	}
}

func TestFinalChecksWithoutCloudDescribeNothingAboutIt(t *testing.T) {
	t.Parallel()
	checks := &fixturekit.FakeChecks{Outcomes: fixturekit.Passing()}
	reviewer, cfg, report := finalChecksFixture(t, checks)
	run, err := reviewer.FinalChecks(t.Context(), cfg, stream, report)
	fixturekit.Must(t, err)
	if run.Engines == nil || run.Engines.UsedCloud() || strings.Contains(FinalCheckEvidence(run), "Cloud") {
		t.Fatalf("run %+v\n%s", run.Engines, FinalCheckEvidence(run))
	}
}

func TestFinalChecksTimeoutWithoutFallbackKeepsASingleBudget(t *testing.T) {
	t.Parallel()
	local := &fixturekit.FakeChecks{Outcomes: []fixturekit.CheckOutcome{{Block: true}}}
	reviewer, cfg, report := finalChecksFixture(t, local)
	cfg.Project.ChecksTimeout = "50ms"
	run, err := reviewer.FinalChecks(t.Context(), cfg, stream, report)
	if err == nil || !strings.Contains(err.Error(), "checks_timeout 50ms") || strings.Contains(err.Error(), "Cloud") || run.Status != checkrun.ChecksIncomplete {
		t.Fatalf("run %+v error %v", run, err)
	}
}

func TestFinalChecksWhoseCloudAttemptDoesNotFinishBackOff(t *testing.T) {
	t.Parallel()
	local := &fixturekit.FakeChecks{Outcomes: []fixturekit.CheckOutcome{{Result: engineFailure}}}
	cloud := &fixturekit.FakeChecks{Outcomes: []fixturekit.CheckOutcome{{Result: engineFailure}}}
	reviewer, cfg, report := finalChecksFixture(t, local)
	reviewer.S.CloudChecks = cloud
	run, err := reviewer.FinalChecks(t.Context(), cfg, stream, report)
	if err == nil || run.Status != checkrun.ChecksIncomplete || !strings.Contains(run.Error, "the cloud attempt did not finish") {
		t.Fatalf("run %+v error %v", run, err)
	}
	for _, want := range []string{"final review 1 waits for its checks: 1 runs in a row", "Dagger Cloud engine ran every check", "they run again at"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error lacks %q: %v", want, err)
		}
	}
	// The retry delay holds: nothing runs again before it has passed.
	if _, err := reviewer.FinalChecks(t.Context(), cfg, stream, report); err == nil || len(local.Runs()) != 1 || len(cloud.Runs()) != 1 {
		t.Fatalf("ran again: local %q cloud %q error %v", local.Runs(), cloud.Runs(), err)
	}
}

func TestFinalChecksSendLinksThatNeededCloudThereDirectly(t *testing.T) {
	t.Parallel()
	links := []string{daggerfixture.PackageA, daggerfixture.TestB}
	local := &fixturekit.FakeChecks{Links: append(slices.Clone(links), daggerfixture.Version), Outcomes: []fixturekit.CheckOutcome{
		{Result: api.CheckResult{Output: fixturekit.PassedReport}}, {Block: true}, {Result: api.CheckResult{Output: fixturekit.PassedReport}},
	}}
	cloud := &fixturekit.FakeChecks{Outcomes: fixturekit.Passing()}
	reviewer, cfg, report := finalChecksFixture(t, local)
	reviewer.S.CloudChecks = cloud
	cfg.Project.ChecksTimeout = "100ms"
	answers := map[string]systemone.Answer{"check-0": jevtest.Noul(.9), "check-1": jevtest.Noul(.9), "check-2": jevtest.Noul(.01)}
	result := jevtest.Result{Response: systemone.Response{Model: "stub", Answers: answers}}
	p := &jevtest.Provider{Results: []jevtest.Result{result, result}}
	settings := config.Jev{Enabled: true, URL: config.DefaultJevURL, Model: config.DefaultJevModel, APIKeyEnv: config.DefaultJevAPIKeyEnv, Timeout: "1s", CheckThreshold: .5}
	reviewer.S.Jev = &jev.Judge{Config: func() config.Jev { return settings }, Provider: p.Factory(), Getenv: func(string) string { return "key" }}

	first, err := reviewer.FinalChecks(t.Context(), cfg, stream, report)
	fixturekit.Must(t, err)
	if first.Status != checkrun.ChecksPassed || len(first.Engines.CloudRuns()) != 1 || first.Engines.CloudRuns()[0].Reason != api.CloudAfterTimeout || !slices.Equal(first.Engines.CloudRuns()[0].Links, links[1:]) {
		t.Fatalf("first run %+v engines %+v", first, first.Engines)
	}

	report.Review = 2
	second, err := reviewer.FinalChecks(t.Context(), cfg, stream, report)
	fixturekit.Must(t, err)
	cloudRuns := second.Engines.CloudRuns()
	if second.Status != checkrun.ChecksPassed || len(cloudRuns) != 1 || cloudRuns[0].Reason != api.CloudByMemory || !slices.Equal(cloudRuns[0].Links, links[1:]) {
		t.Fatalf("second run %+v engines %+v", second, second.Engines)
	}
	if ran := local.Runs(); len(ran) != 3 || !slices.Equal(ran[2], links[:1]) {
		t.Fatalf("local ran %q, want the remembered link kept off it", ran)
	}
	if ran := cloud.Runs(); len(ran) != 2 || !slices.Equal(ran[1], links[1:]) {
		t.Fatalf("cloud ran %q", ran)
	}
	if evidence := FinalCheckEvidence(second); !strings.Contains(evidence, "routed there because an earlier run in this workstream needed cloud") {
		t.Fatalf("evidence:\n%s", evidence)
	}
}

func TestFinalCheckRecordsWithoutEnginesReadAsLocalOnly(t *testing.T) {
	t.Parallel()
	reviewer, _, report := finalChecksFixture(t, &fixturekit.FakeChecks{Outcomes: fixturekit.Passing()})
	old := `{
  "review": 4,
  "operation": "review",
  "commit": "` + report.Commit + `",
  "status": "incomplete",
  "selection": {"mode": "full", "reason": "disabled"},
  "exit_code": 1,
  "output": "Error: engine lost\n",
  "error": "engine unreachable",
  "started_at": "2026-01-01T00:00:00Z",
  "finished_at": "2026-01-01T00:01:00Z"
}
`
	h := trace.Header{Schema: "osmia.trace.document", Version: trace.Version, ID: FinalChecksDocument(4), Revision: 1, Project: reviewer.Repository.Project(), Workstream: stream, At: fixturekit.DemoStart, Actor: checksActor, Cause: "review"}
	fixturekit.Must(t, reviewer.Repository.RecordDocuments(t.Context(), []trace.Document{{Header: h, Path: FinalChecksPath(4), Content: old}}))
	n, latest, err := reviewer.IncompleteChecks(stream, 4, report.Commit)
	fixturekit.Must(t, err)
	if n != 1 || latest.Status != checkrun.ChecksIncomplete || latest.Error != "engine unreachable" || latest.Output != "Error: engine lost\n" || latest.Engines != nil {
		t.Fatalf("old record read as %d %+v", n, latest)
	}
	remembered, err := checkrun.RememberedIn(reviewer.Repository, stream)
	fixturekit.Must(t, err)
	if len(remembered.Links) != 0 || remembered.EveryCheck {
		t.Fatalf("an old record is remembered: %+v", remembered)
	}
	if evidence := FinalCheckEvidence(latest); strings.Contains(evidence, "Cloud") {
		t.Fatalf("evidence:\n%s", evidence)
	}
}
