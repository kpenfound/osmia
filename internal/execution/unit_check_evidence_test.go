package execution

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/kpenfound/osmia/internal/api"
	checkrun "github.com/kpenfound/osmia/internal/checks"
)

func TestUnitCheckRunRecordsTheCoRunEvidenceOfAFlakyCheck(t *testing.T) {
	t.Parallel()
	const link, other = "dag://go/packages/tests/test?go-test=TestFlaky", "dag://go/lint"
	output := failedReport(link) + "CHECKS  ✔ 2 passed\n✔ " + link + " 1.5s OK\n✔ " + other + " 1m2s OK\n"
	checks := &fakeChecks{Outcomes: []checkOutcome{{Result: checkrun.CheckResult{Output: output, Flaky: []string{link}}}}}
	f, stream, repository := newChecksFixture(t, "checks-flaky-evidence", checks)
	runChecks(t, f.s, repository, stream)
	if got := unitState(t, repository, stream, "resume"); got != UnitReviewing {
		t.Fatalf("unit is %s, want reviewing", got)
	}
	run := onlyRun(t, repository, stream)
	if run.Status != ChecksPassed || len(run.Failed) != 0 || !slices.Equal(run.Flaky, []string{link}) {
		t.Fatalf("the added capture changed the outcome: %+v", run)
	}
	evidence := run.Evidence
	if evidence == nil {
		t.Fatalf("the stored record holds no evidence: %+v", run)
	}
	if !slices.Equal(evidence.CoRun, []string{link, other}) || !evidence.EveryCheck {
		t.Fatalf("co-run %v, every check %v", evidence.CoRun, evidence.EveryCheck)
	}
	if evidence.Parallelism != checkrun.CheckParallelism || evidence.Engine != api.CheckEngineLocal {
		t.Fatalf("parallelism %d, engine %q", evidence.Parallelism, evidence.Engine)
	}
	if evidence.OutputPath != run.OutputPath || evidence.OutputRevision != run.OutputRevision || evidence.OutputPath == "" {
		t.Fatalf("output %q revision %d, record %q revision %d", evidence.OutputPath, evidence.OutputRevision, run.OutputPath, run.OutputRevision)
	}
	if len(evidence.Diagnostics) != 1 || evidence.Diagnostics[0].Check != link || !strings.Contains(evidence.Diagnostics[0].Text, "[passed on rerun]") || !strings.Contains(evidence.Diagnostics[0].Text, "wrong chunk") {
		t.Fatalf("diagnostics %+v", evidence.Diagnostics)
	}
	if len(evidence.Timings) != 3 || evidence.Timings[2].Check != other || evidence.Timings[2].Duration != "1m2s" {
		t.Fatalf("timings %+v", evidence.Timings)
	}
	converted := run.FlakeEvidence()
	if !slices.Equal(converted.Flaky, []string{link}) || converted.Engines != run.Engines || converted.Engines == nil || !slices.Equal(converted.CoRun, evidence.CoRun) || len(converted.Unavailable) != 0 {
		t.Fatalf("converted %+v", converted)
	}
}

func TestUnitCheckRunWithoutEvidenceConvertsFromItsOtherFields(t *testing.T) {
	t.Parallel()
	const link = "dag://go/packages/tests/test?go-test=TestFlaky"
	summary, _ := checkrun.SummarizeCheckOutput(failedReport(link)+"CHECKS  ✔ 1 passed\n✔ "+link+" 1.0s OK\n", 0)
	old := UnitCheckRun{Unit: "resume", Run: 3, Status: ChecksPassed, Selection: &CheckSelection{Mode: "selected", Links: []string{link, "dag://go/lint"}}, Output: summary, OutputPath: "units/resume/checks-3-output.txt", OutputRevision: 1, Flaky: []string{link}}
	data, err := json.Marshal(old)
	must(t, err)
	if strings.Contains(string(data), "evidence") {
		t.Fatalf("a record without evidence stores %s", data)
	}
	var loaded UnitCheckRun
	must(t, json.Unmarshal(data, &loaded))
	evidence := loaded.FlakeEvidence()
	if evidence.Engine != api.CheckEngineLocal || !slices.Equal(evidence.CoRun, []string{link, "dag://go/lint"}) || evidence.OutputPath != old.OutputPath || len(evidence.Diagnostics) != 1 || !slices.Equal(evidence.Flaky, []string{link}) {
		t.Fatalf("evidence %+v", evidence)
	}
	if len(evidence.Unavailable) != 1 || evidence.Unavailable[0].Item != api.CheckItemParallelism || evidence.Unavailable[0].Where != "units/resume/checks-3.json" {
		t.Fatalf("unavailable %+v", evidence.Unavailable)
	}
}
