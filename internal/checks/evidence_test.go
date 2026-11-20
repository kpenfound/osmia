package checks

import (
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/kpenfound/osmia/internal/api"
)

const flakyLink = "dag://go/packages/tests/test?go-test=TestFlaky"

var flakyOutput = "CHECKS  ✘ 1 failed\n✘ " + flakyLink + " 2s ERROR\n    wrong chunk\n" +
	"CHECKS  ✔ 2 passed\n✔ " + flakyLink + " 1.5s OK\n✔ dag://go/lint 1m2s OK\n"

func TestLocalEvidenceHoldsTheCoRunChecksParallelismEngineOutputAndDiagnostics(t *testing.T) {
	t.Parallel()
	selection := api.CheckSelection{Mode: "selected", Links: []string{flakyLink, "dag://go/lint"}}
	engines := &api.CheckAttribution{Runs: []api.CheckEngineRun{{Engine: api.CheckEngineLocal, Links: selection.Links}}}
	evidence := LocalEvidence(selection, engines, flakyOutput, "units/u/checks-1-output.txt", 1, "https://dagger.cloud/traces/t1")
	if evidence.Engine != api.CheckEngineLocal || evidence.Parallelism != CheckParallelism || evidence.EveryCheck {
		t.Fatalf("evidence %+v", evidence)
	}
	if want := []string{flakyLink, "dag://go/lint"}; !slices.Equal(evidence.CoRun, want) {
		t.Fatalf("co-run %v, want %v", evidence.CoRun, want)
	}
	if evidence.OutputPath != "units/u/checks-1-output.txt" || evidence.OutputRevision != 1 {
		t.Fatalf("output %+v", evidence)
	}
	if len(evidence.Diagnostics) != 1 || evidence.Diagnostics[0].Check != flakyLink || !strings.Contains(evidence.Diagnostics[0].Text, "[passed on rerun]") || !strings.Contains(evidence.Diagnostics[0].Text, "wrong chunk") {
		t.Fatalf("diagnostics %+v", evidence.Diagnostics)
	}
	want := []api.CheckTiming{{Check: flakyLink, State: "failed", Duration: "2s"}, {Check: flakyLink, State: "passed", Duration: "1.5s"}, {Check: "dag://go/lint", State: "passed", Duration: "1m2s"}}
	if !slices.Equal(evidence.Timings, want) {
		t.Fatalf("timings %+v", evidence.Timings)
	}
	if len(evidence.Traces) != 1 || evidence.Traces[0].Kind != api.CheckTraceRun || evidence.Traces[0].URL != "https://dagger.cloud/traces/t1" || len(evidence.Unavailable) != 0 {
		t.Fatalf("traces %+v, unavailable %+v", evidence.Traces, evidence.Unavailable)
	}
}

func TestLocalEvidenceFallsBackToTheSelectedLinksAndMarksAFullRun(t *testing.T) {
	t.Parallel()
	selected := LocalEvidence(api.CheckSelection{Links: []string{"dag://a"}}, nil, "no report", "", 0, "")
	if !slices.Equal(selected.CoRun, []string{"dag://a"}) || selected.EveryCheck || selected.Engine != api.CheckEngineLocal {
		t.Fatalf("selected %+v", selected)
	}
	if full := LocalEvidence(api.CheckSelection{Mode: "full"}, nil, "no report", "", 0, ""); !full.EveryCheck || len(full.CoRun) != 0 {
		t.Fatalf("full %+v", full)
	}
}

func TestEngineOfNamesLocalCloudAndMixedRuns(t *testing.T) {
	t.Parallel()
	local, cloud := api.CheckEngineRun{Engine: api.CheckEngineLocal}, api.CheckEngineRun{Engine: api.CheckEngineCloud}
	for name, tc := range map[string]struct {
		engines *api.CheckAttribution
		want    string
	}{
		"absent": {nil, api.CheckEngineLocal},
		"empty":  {&api.CheckAttribution{}, api.CheckEngineLocal},
		"local":  {&api.CheckAttribution{Runs: []api.CheckEngineRun{local}}, api.CheckEngineLocal},
		"cloud":  {&api.CheckAttribution{Runs: []api.CheckEngineRun{cloud}}, api.CheckEngineCloud},
		"both":   {&api.CheckAttribution{Runs: []api.CheckEngineRun{local, cloud}}, api.CheckEngineMixed},
	} {
		if got := EngineOf(tc.engines); got != tc.want {
			t.Errorf("%s: %q, want %q", name, got, tc.want)
		}
	}
}

func TestSummaryDiagnosticsReadTheFlakeBlocksOfASummarizedOutput(t *testing.T) {
	t.Parallel()
	summary, _ := SummarizeCheckOutput(flakyOutput+"✘ dag://other 1s ERROR\n    stays failed\n", 1)
	got := SummaryDiagnostics(summary)
	if len(got) != 1 || got[0].Check != flakyLink || !strings.Contains(got[0].Text, "wrong chunk") || strings.Contains(got[0].Text, "stays failed") {
		t.Fatalf("diagnostics %+v from\n%s", got, summary)
	}
	if got := SummaryDiagnostics("nothing here"); len(got) != 0 {
		t.Fatalf("diagnostics %+v", got)
	}
}

func TestCloudStatusesKeepEveryCheckNameWithItsTraceAndDuration(t *testing.T) {
	t.Parallel()
	load := cloudStatus(1, "load", "success", "Loaded", "")
	run := cloudStatus(9, CloudRunContext, "failure", "", "https://dagger.cloud/checks/x.json")
	read := cloudRead(load, run,
		cloudStatus(3, "test:b", "success", "Passed in 1m2s. Run `dagger trace tb`", "https://dagger.cloud/traces/tb"),
		cloudStatus(2, "test:a", "failure", "Failed in 2m33s. Run `dagger trace ta`", ""),
		cloudStatus(4, "lint", "success", "", ""))
	result := InterpretCloudStatuses(read)
	want := []CloudCheck{
		{Name: "lint", State: "success", StatusID: 4},
		{Name: "test:a", State: "failure", StatusID: 2, TraceID: "ta", Duration: 2*time.Minute + 33*time.Second},
		{Name: "test:b", State: "success", StatusID: 3, TraceID: "tb", TargetURL: "https://dagger.cloud/traces/tb", Duration: time.Minute + 2*time.Second},
	}
	if !slices.Equal(result.Checks, want) {
		t.Fatalf("checks %+v, want %+v", result.Checks, want)
	}
	pending := InterpretCloudStatuses(cloudRead(load, cloudStatus(5, CloudRunContext, "pending", "", "")))
	if len(pending.Checks) != 0 {
		t.Fatalf("pending %+v", pending.Checks)
	}
}

func TestCloudEvidenceNamesTheEngineChecksAndTracesAndMarksWhatCloudDoesNotReport(t *testing.T) {
	t.Parallel()
	result := CloudResult{StatusURL: "https://dagger.cloud/checks/x.json", Checks: []CloudCheck{
		{Name: "test:a", State: "success", TraceID: "fresh-a", Duration: 90 * time.Second},
		{Name: "test:b", State: "success"},
	}}
	failed := []api.CheckFailure{{Name: "test:a", TraceID: "trace-a"}, {Name: "test:b", TraceID: "trace-b"}, {Name: "test:gone"}}
	evidence := CloudEvidence(result, failed, []string{"test:a", "test:b"})
	if evidence.Engine != api.CheckEngineCloud || evidence.Parallelism != 0 || evidence.OutputPath != "" {
		t.Fatalf("evidence %+v", evidence)
	}
	if want := []string{"test:a", "test:b", "test:gone"}; !slices.Equal(evidence.CoRun, want) {
		t.Fatalf("co-run %v", evidence.CoRun)
	}
	traces := map[string]api.CheckTraceRef{}
	for _, tr := range evidence.Traces {
		traces[tr.Kind+" "+tr.Check] = tr
	}
	if traces["failed test:a"].TraceID != "trace-a" || traces["failed test:b"].TraceID != "trace-b" || traces["rerun test:a"].TraceID != "fresh-a" || traces["rerun test:b"].URL != result.StatusURL || traces["run "].URL != result.StatusURL {
		t.Fatalf("traces %+v", evidence.Traces)
	}
	if want := []api.CheckTiming{{Check: "test:a", State: "passed", Duration: "1m30s"}}; !slices.Equal(evidence.Timings, want) {
		t.Fatalf("timings %+v", evidence.Timings)
	}
	items := map[string]api.CheckUnavailable{}
	for _, u := range evidence.Unavailable {
		items[u.Item] = u
	}
	for _, item := range []string{api.CheckItemOutput, api.CheckItemParallelism} {
		u := items[item]
		if u.Engine != api.CheckEngineCloud || !strings.Contains(u.Where, "dagger trace trace-a") || !strings.Contains(u.Where, "dagger trace fresh-a") {
			t.Fatalf("%s unavailable %+v", item, u)
		}
	}
	bare := CloudEvidence(CloudResult{StatusURL: "https://dagger.cloud/checks/x.json"}, nil, nil)
	if len(bare.Unavailable) != 2 || bare.Unavailable[0].Where != "https://dagger.cloud/checks/x.json" {
		t.Fatalf("bare %+v", bare)
	}
}
