package execution

import (
	context "context"
	json "encoding/json"
	slices "slices"
	strings "strings"
	testing "testing"
	time "time"

	jevtest "github.com/kpenfound/agentcore/jev/jevtest"
	systemone "github.com/kpenfound/agentcore/jev/systemone"
	"github.com/kpenfound/osmia/internal/api"
	checkrun "github.com/kpenfound/osmia/internal/checks"
	config "github.com/kpenfound/osmia/internal/config"
	jev "github.com/kpenfound/osmia/internal/jev"
	"github.com/kpenfound/osmia/internal/servicetest/daggerfixture"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	trace "github.com/kpenfound/osmia/internal/trace"
)

// engineFailure is the result of an attempt that exits without reporting a
// failed check.
var engineFailure = api.CheckResult{ExitCode: 1, Output: "Error: the engine session was lost\n"}

// cloudPrompt is what a review prompt says about a run that used the cloud
// engine.
const cloudPrompt = "Dagger Cloud engine ran"

func selectLinks(f *buildFixture, answers int, scores ...float64) {
	results := make([]jevtest.Result, answers)
	for i := range results {
		a := map[string]systemone.Answer{}
		for j, score := range scores {
			a["check-"+string(rune('0'+j))] = jevtest.Noul(score)
		}
		results[i] = jevtest.Result{Response: systemone.Response{Model: "stub", Answers: a}}
	}
	p := &jevtest.Provider{Results: results}
	settings := config.Jev{Enabled: true, URL: config.DefaultJevURL, Model: config.DefaultJevModel, APIKeyEnv: config.DefaultJevAPIKeyEnv, Timeout: "1s", CheckThreshold: .5}
	f.s.Jev = &jev.Judge{Config: func() config.Jev { return settings }, Provider: p.Factory(), Getenv: func(string) string { return "key" }, Now: f.s.Now}
}

func advanceClock(f *buildFixture) {
	f.clock.Mu.Lock()
	f.clock.At = f.clock.At.Add(checkRetryDelay + time.Minute)
	f.clock.Mu.Unlock()
}

func recordedRun(t *testing.T, repository *trace.Repository, stream config.WorkstreamID, run int) (UnitCheckRun, string) {
	t.Helper()
	docs, err := trace.Read[trace.Document](repository, stream)
	must(t, err)
	for _, d := range docs {
		if d.Path == checkPath("resume", run) {
			var saved UnitCheckRun
			must(t, json.Unmarshal([]byte(d.Content), &saved))
			return saved, d.Content
		}
	}
	t.Fatalf("check run %d is not recorded", run)
	return UnitCheckRun{}, ""
}

func transitionReasons(t *testing.T, repository *trace.Repository, stream config.WorkstreamID, to string) []string {
	t.Helper()
	transitions, err := trace.Read[trace.Transition](repository, stream)
	must(t, err)
	var reasons []string
	for _, tr := range transitions {
		if tr.To == to {
			reasons = append(reasons, tr.Reason)
		}
	}
	return reasons
}

func TestLocalTimeoutPassesOnCloudInTheSameOperation(t *testing.T) {
	t.Parallel()
	local := &fakeChecks{Outcomes: []checkOutcome{{Block: true}}}
	cloud := &fakeChecks{Outcomes: passing()}
	f, stream, repository := newChecksFixture(t, "fallback-timeout", local)
	f.s.CloudChecks = cloud
	f.cfg.Project.ChecksTimeout = "50ms"
	runChecks(t, f.s, repository, stream)
	if got := unitState(t, repository, stream, "resume"); got != UnitReviewing {
		t.Fatalf("unit is %s, want reviewing without waiting for a retry", got)
	}
	run, content := recordedRun(t, repository, stream, 1)
	want := &api.CheckAttribution{Runs: []api.CheckEngineRun{{Engine: api.CheckEngineCloud, EveryCheck: true, Reason: api.CloudAfterTimeout}}}
	if run.Status != ChecksPassed || run.Engines == nil || len(run.Engines.Runs) != 1 || !engineRunsEqual(run.Engines.Runs[0], want.Runs[0]) || !strings.Contains(content, `"engines"`) {
		t.Fatalf("check run %+v\n%s", run, content)
	}
	if len(local.Runs()) != 1 || len(cloud.Runs()) != 1 {
		t.Fatalf("local ran %q, cloud ran %q", local.Runs(), cloud.Runs())
	}
	docs, err := trace.Read[trace.Document](repository, stream)
	must(t, err)
	for _, d := range docs {
		if d.Path == run.OutputPath && (!strings.Contains(d.Content, "== local engine ==") || !strings.Contains(d.Content, "== cloud engine ==")) {
			t.Fatalf("output does not label its engines:\n%s", d.Content)
		}
	}
	reasons := transitionReasons(t, repository, stream, UnitReviewing)
	if len(reasons) != 1 || !strings.Contains(reasons[0], cloudPrompt+" every check") || !strings.Contains(reasons[0], "time budget") {
		t.Fatalf("pass reason %q", reasons)
	}
	if prompt := reviewTurn(t, f, repository, stream).Prompt; !strings.Contains(prompt, cloudPrompt+" every check") || !strings.Contains(prompt, "time budget") {
		t.Fatalf("review evidence:\n%s", prompt)
	}
}

func TestChecksWithoutCloudUseDescribeNothingAboutIt(t *testing.T) {
	t.Parallel()
	checks := &fakeChecks{Outcomes: passing()}
	f, stream, repository := newChecksFixture(t, "fallback-off", checks)
	runChecks(t, f.s, repository, stream)
	run, _ := recordedRun(t, repository, stream, 1)
	if run.Engines == nil || run.Engines.UsedCloud() || len(run.Engines.Runs) != 1 || run.Engines.Runs[0].Engine != api.CheckEngineLocal || run.Engines.Runs[0].Reason != "" {
		t.Fatalf("attribution %+v", run.Engines)
	}
	for _, text := range append(transitionReasons(t, repository, stream, UnitReviewing), reviewTurn(t, f, repository, stream).Prompt) {
		if strings.Contains(text, "Dagger Cloud engine") {
			t.Fatalf("text mentions cloud: %s", text)
		}
	}
}

func TestLocalTimeoutWithoutFallbackKeepsASingleBudget(t *testing.T) {
	t.Parallel()
	local := &fakeChecks{Outcomes: []checkOutcome{{Block: true}}}
	f, stream, repository := newChecksFixture(t, "fallback-off-timeout", local)
	f.cfg.Project.ChecksTimeout = "50ms"
	runChecks(t, f.s, repository, stream)
	run := onlyRun(t, repository, stream)
	if run.Status != ChecksIncomplete || run.Error != "checks did not finish within checks_timeout 50ms" || run.Engines.UsedCloud() {
		t.Fatalf("check run %+v", run)
	}
}

func TestOperationIsBoundedByOneLocalAndOneCloudBudget(t *testing.T) {
	t.Parallel()
	const timeout = 400 * time.Millisecond
	local := &fakeChecks{Outcomes: []checkOutcome{{Block: true}}}
	cloud := fixturekit.CheckFunc(func(ctx context.Context, _ string) (api.CheckResult, error) {
		select {
		case <-time.After(timeout * 5 / 8):
			return api.CheckResult{Output: passedReport}, nil
		case <-ctx.Done():
			return api.CheckResult{ExitCode: -1}, ctx.Err()
		}
	})
	f, stream, repository := newChecksFixture(t, "fallback-budgets", local)
	f.s.CloudChecks = cloud
	f.cfg.Project.ChecksTimeout = timeout.String()
	started := time.Now()
	runChecks(t, f.s, repository, stream)
	run := onlyRun(t, repository, stream)
	if run.Status != ChecksPassed || !run.Engines.UsedCloud() {
		t.Fatalf("cloud work was cut short by the local budget: %+v", run)
	}
	if elapsed := time.Since(started); elapsed < timeout {
		t.Fatalf("the local attempt did not use its budget: %s", elapsed)
	}
}

func TestIncompleteCloudAttemptKeepsTheRetryAndContest(t *testing.T) {
	t.Parallel()
	local := &fakeChecks{Outcomes: []checkOutcome{{Result: engineFailure}}}
	cloud := &fakeChecks{Outcomes: []checkOutcome{{Result: engineFailure}}}
	f, stream, repository := newChecksFixture(t, "fallback-incomplete", local)
	f.s.CloudChecks = cloud
	runChecks(t, f.s, repository, stream)
	if got := unitState(t, repository, stream, "resume"); got != UnitChecking {
		t.Fatalf("unit is %s, want checking", got)
	}
	run := onlyRun(t, repository, stream)
	if run.Status != ChecksIncomplete || !strings.Contains(run.Error, "the cloud attempt did not finish") || len(run.Engines.CloudRuns()) != 1 || run.Engines.CloudRuns()[0].Reason != api.CloudAfterEngineFailure {
		t.Fatalf("check run %+v", run)
	}
	outbox, err := repository.Outbox(stream)
	must(t, err)
	i := slices.IndexFunc(outbox, func(e trace.OutboxEntry) bool {
		return e.TransitionID == checkDocumentID("resume", 1)+"-incomplete" && e.Event.Kind == trace.NoticeKind
	})
	if i < 0 || !strings.Contains(outbox[i].Event.Body, "held in checking") || !strings.Contains(outbox[i].Event.Body, cloudPrompt+" every check") {
		t.Fatalf("chief notices %+v", outbox)
	}
	runChecks(t, f.s, repository, stream)
	if len(local.Runs()) != 1 || len(cloud.Runs()) != 1 {
		t.Fatalf("the retry delay was not kept: local %q cloud %q", local.Runs(), cloud.Runs())
	}
	for range maxIncompleteChecks - 1 {
		advanceClock(f)
		runChecks(t, f.s, repository, stream)
	}
	if got := unitState(t, repository, stream, "resume"); got != UnitContested {
		t.Fatalf("unit is %s after %d incomplete runs", got, maxIncompleteChecks)
	}
	contest := transitionReasons(t, repository, stream, UnitContested)
	if len(contest) != 1 || !strings.Contains(contest[0], "did not complete") || !strings.Contains(contest[0], cloudPrompt+" every check") {
		t.Fatalf("the contest %q", contest)
	}
}

func TestLinksThatNeededCloudRunThereDirectlyLater(t *testing.T) {
	t.Parallel()
	links := []string{daggerfixture.PackageA, daggerfixture.TestB}
	local := &fakeChecks{Links: append(slices.Clone(links), daggerfixture.Version), Outcomes: []checkOutcome{
		{Result: api.CheckResult{Output: passedReport}}, {Block: true}, {Result: api.CheckResult{Output: passedReport}},
	}}
	cloud := &fakeChecks{Outcomes: []checkOutcome{{Result: engineFailure}, {Result: api.CheckResult{Output: passedReport}}}}
	f, stream, repository := newChecksFixture(t, "fallback-memory", local)
	f.s.CloudChecks = cloud
	f.cfg.Project.ChecksTimeout = "100ms"
	selectLinks(f, 2, .9, .9, .01)
	runChecks(t, f.s, repository, stream)
	first, _ := recordedRun(t, repository, stream, 1)
	wantFirst := []api.CheckEngineRun{{Engine: api.CheckEngineLocal, Links: links[:1]}, {Engine: api.CheckEngineCloud, Links: links[1:], Reason: api.CloudAfterTimeout}}
	if first.Status != ChecksIncomplete || !slices.EqualFunc(first.Engines.Runs, wantFirst, engineRunsEqual) {
		t.Fatalf("first run %+v engines %+v", first, first.Engines)
	}
	if got := unitState(t, repository, stream, "resume"); got != UnitChecking {
		t.Fatalf("unit is %s, want checking", got)
	}

	advanceClock(f)
	runChecks(t, f.s, repository, stream)
	if got := unitState(t, repository, stream, "resume"); got != UnitReviewing {
		t.Fatalf("unit is %s, want reviewing", got)
	}
	second, _ := recordedRun(t, repository, stream, 2)
	wantSecond := []api.CheckEngineRun{{Engine: api.CheckEngineLocal, Links: links[:1]}, {Engine: api.CheckEngineCloud, Links: links[1:], Reason: api.CloudByMemory}}
	if second.Status != ChecksPassed || !slices.EqualFunc(second.Engines.Runs, wantSecond, engineRunsEqual) {
		t.Fatalf("second run %+v engines %+v", second, second.Engines)
	}
	if ran := local.Runs(); len(ran) != 3 || !slices.Equal(ran[2], links[:1]) {
		t.Fatalf("local ran %q, want the remembered link kept off it", ran)
	}
	if ran := cloud.Runs(); len(ran) != 2 || !slices.Equal(ran[1], links[1:]) {
		t.Fatalf("cloud ran %q", ran)
	}
	reasons := transitionReasons(t, repository, stream, UnitReviewing)
	if len(reasons) != 1 || !strings.Contains(reasons[0], "routed there because an earlier run in this workstream needed cloud") {
		t.Fatalf("pass reason %q", reasons)
	}
	if prompt := reviewTurn(t, f, repository, stream).Prompt; !strings.Contains(prompt, "routed there because an earlier run") {
		t.Fatalf("review evidence:\n%s", prompt)
	}

	remembered, err := checkrun.RememberedIn(repository, stream)
	must(t, err)
	if !slices.Equal(remembered.Links, links[1:]) || remembered.EveryCheck {
		t.Fatalf("remembered %+v", remembered)
	}
	const otherStream config.WorkstreamID = "w_00000000000000000000000000000000"
	must(t, repository.CreateWorkstream(context.Background(), otherStream, f.clock.Now(), checksActor))
	other, err := checkrun.RememberedIn(repository, otherStream)
	must(t, err)
	if len(other.Links) != 0 || other.EveryCheck {
		t.Fatalf("another workstream remembers %+v", other)
	}
}

func engineRunsEqual(a, b api.CheckEngineRun) bool {
	return a.Engine == b.Engine && slices.Equal(a.Links, b.Links) && a.EveryCheck == b.EveryCheck && a.Reason == b.Reason
}

func TestRecordsWithoutEnginesReadAsLocalOnly(t *testing.T) {
	t.Parallel()
	checks := &fakeChecks{Outcomes: passing()}
	f, stream, repository := newChecksFixture(t, "fallback-old-records", checks)
	old := `{
  "unit": "elder",
  "run": 7,
  "operation": "op",
  "candidate": "c",
  "base": "b",
  "diff_sha256": "d",
  "report": 1,
  "status": "incomplete",
  "selection": {"mode": "full", "reason": "disabled"},
  "exit_code": 1,
  "output": "Error: engine lost\n",
  "error": "engine unreachable",
  "started_at": "2026-01-01T00:00:00Z",
  "finished_at": "2026-01-01T00:01:00Z"
}
`
	h := trace.Header{Schema: "osmia.trace.document", Version: trace.Version, ID: checkDocumentID("elder", 7), Revision: 1, Project: repository.Project(), Workstream: stream, Unit: "elder", At: f.clock.Now(), Actor: checksActor, Cause: checkRequestID("elder", 7)}
	must(t, repository.RecordDocuments(context.Background(), []trace.Document{{Header: h, Path: checkPath("elder", 7), Content: old}}))
	runs, err := checkRuns(repository, stream)
	must(t, err)
	if len(runs) != 1 || runs[0].Status != ChecksIncomplete || runs[0].Error != "engine unreachable" || runs[0].Output != "Error: engine lost\n" || runs[0].Engines != nil {
		t.Fatalf("old record read as %+v", runs)
	}
	remembered, err := checkrun.RememberedIn(repository, stream)
	must(t, err)
	if len(remembered.Links) != 0 || remembered.EveryCheck {
		t.Fatalf("an old record is remembered: %+v", remembered)
	}
}
