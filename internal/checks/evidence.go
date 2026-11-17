package checks

import (
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/kpenfound/osmia/internal/api"
)

// diagnosticLimit bounds the failure report kept for one check that passed on
// its rerun.
const diagnosticLimit = 4096

// EngineOf names the engine an attribution says ran a check operation: local
// for an absent attribution and for runs that all used the local engine, cloud
// when all did, and api.CheckEngineMixed otherwise.
func EngineOf(engines *api.CheckAttribution) string {
	local, cloud := false, false
	if engines != nil {
		for _, run := range engines.Runs {
			if run.Engine == api.CheckEngineCloud {
				cloud = true
			} else {
				local = true
			}
		}
	}
	switch {
	case cloud && local:
		return api.CheckEngineMixed
	case cloud:
		return api.CheckEngineCloud
	}
	return api.CheckEngineLocal
}

// LocalEvidence is the evidence a local unit run of a candidate leaves: the
// checks its output reports (or, when it reports none, the links selected), the
// parallelism of its invocations, the engine attribution, the captured output
// and the failure reports of the checks that passed on their rerun. output is
// the run's complete captured output, and path and revision say where it is
// stored. Timings are the durations the report shows.
func LocalEvidence(selection api.CheckSelection, engines *api.CheckAttribution, output, path string, revision int, traceURL string) api.CheckEvidence {
	evidence := api.CheckEvidence{Engine: EngineOf(engines), Engines: engines, Parallelism: CheckParallelism, OutputPath: path, OutputRevision: revision}
	_, _, _, results := scanCheckReport(output)
	for _, r := range results {
		if !slices.Contains(evidence.CoRun, r.link) {
			evidence.CoRun = append(evidence.CoRun, r.link)
		}
		if d, ok := reportDuration(r.rest); ok {
			evidence.Timings = append(evidence.Timings, api.CheckTiming{Check: r.link, State: reportState(r.mark), Duration: d.String()})
		}
	}
	if len(evidence.CoRun) == 0 {
		evidence.CoRun = slices.Clone(selection.Links)
	}
	evidence.EveryCheck = len(selection.Links) == 0
	evidence.Diagnostics = RerunDiagnostics(output)
	if traceURL != "" {
		evidence.Traces = []api.CheckTraceRef{{Kind: api.CheckTraceRun, URL: traceURL}}
	}
	return evidence
}

// RecordedLocalRun is what a local check run's record holds when it kept no
// structured evidence: the run's selection, engine attribution, summarized
// output, the location of the captured output and trace, and its flaky links.
// Record is the path of the record itself.
type RecordedLocalRun struct {
	Selection      *api.CheckSelection
	Engines        *api.CheckAttribution
	Output         string
	OutputPath     string
	OutputRevision int
	TraceURL       string
	Record         string
}

// RecordedEvidence is the evidence a local run's record yields without having
// kept any: what its fields hold, with the parallelism and any failure report
// of a flaky check it did not keep marked unavailable, to be read in the record.
func RecordedEvidence(r RecordedLocalRun, flaky []string) api.CheckEvidence {
	evidence := api.CheckEvidence{Engine: EngineOf(r.Engines), Engines: r.Engines, OutputPath: r.OutputPath, OutputRevision: r.OutputRevision, Diagnostics: SummaryDiagnostics(r.Output)}
	if r.Selection != nil {
		evidence.CoRun, evidence.EveryCheck = slices.Clone(r.Selection.Links), len(r.Selection.Links) == 0
	}
	if r.TraceURL != "" {
		evidence.Traces = []api.CheckTraceRef{{Kind: api.CheckTraceRun, URL: r.TraceURL}}
	}
	evidence.Unavailable = []api.CheckUnavailable{{Item: api.CheckItemParallelism, Engine: evidence.Engine, Where: r.Record}}
	if len(evidence.Diagnostics) < len(flaky) {
		evidence.Unavailable = append(evidence.Unavailable, api.CheckUnavailable{Item: api.CheckItemDiagnostics, Engine: evidence.Engine, Where: r.Record})
	}
	return evidence
}

// RerunDiagnostics returns the failure report of each check that failed and
// then passed on a later report of output, as SummarizeCheckOutput marks it.
func RerunDiagnostics(output string) []api.CheckDiagnostic {
	_, failures := parseCheckReport(output)
	var out []api.CheckDiagnostic
	for _, f := range latestFailures(failures) {
		if f.passedLater {
			out = append(out, api.CheckDiagnostic{Check: f.link, Text: CheckExcerpt(failureBlock(f), diagnosticLimit)})
		}
	}
	return out
}

// SummaryDiagnostics returns the failure reports a summarized output (see
// SummarizeCheckOutput) marks `[passed on rerun]`, for records that kept the
// summary and no structured evidence.
func SummaryDiagnostics(summary string) []api.CheckDiagnostic {
	var out []api.CheckDiagnostic
	var block []string
	flush := func() {
		if len(block) == 0 {
			return
		}
		fields := strings.Fields(block[0])
		if len(fields) >= 2 {
			out = append(out, api.CheckDiagnostic{Check: fields[1], Text: CheckExcerpt(strings.Join(block, "\n"), diagnosticLimit)})
		}
		block = nil
	}
	for _, line := range strings.Split(summary, "\n") {
		switch {
		case reportCheck.MatchString(line) && strings.Contains(line, "[passed on rerun]"):
			flush()
			block = []string{line}
		case reportCheck.MatchString(line) || reportHeading.MatchString(line):
			flush()
		case block != nil:
			block = append(block, line)
		}
	}
	flush()
	return out
}

func failureBlock(f checkFailure) string {
	return fmt.Sprintf("%s [passed on rerun] [output lines %d-%d]\n%s", f.heading, f.start, f.end, strings.TrimSpace(strings.Join(f.lines, "\n")))
}

func reportState(mark string) string {
	switch mark {
	case "✔":
		return "passed"
	case "✘":
		return "failed"
	}
	return "cancelled"
}

// reportDuration reads the duration that follows a check's link in a report
// line, as in "1.5s OK".
func reportDuration(rest string) (time.Duration, bool) {
	fields := strings.Fields(rest)
	if len(fields) == 0 {
		return 0, false
	}
	d, err := time.ParseDuration(fields[0])
	return d, err == nil
}

// CloudEvidence is the evidence a Dagger Cloud result leaves, from the
// statuses read for the commit. Cloud reports neither the output text of a
// check nor the parallelism it ran with, so both are marked unavailable and
// point at the traces to read. failed are the checks that failed before the
// rerun, flaky the ones that passed it.
func CloudEvidence(result CloudResult, failed []api.CheckFailure, flaky []string) api.CheckEvidence {
	evidence := api.CheckEvidence{Engine: api.CheckEngineCloud}
	names := map[string]CloudCheck{}
	for _, c := range result.Checks {
		names[c.Name] = c
		evidence.CoRun = append(evidence.CoRun, c.Name)
		if c.Duration > 0 {
			evidence.Timings = append(evidence.Timings, api.CheckTiming{Check: c.Name, State: cloudState(c.State), Duration: c.Duration.String()})
		}
	}
	for _, f := range failed {
		if !slices.Contains(evidence.CoRun, f.Name) {
			evidence.CoRun = append(evidence.CoRun, f.Name)
		}
	}
	slices.Sort(evidence.CoRun)
	runURL := result.StatusURL
	if runURL != "" {
		evidence.Traces = append(evidence.Traces, api.CheckTraceRef{Kind: api.CheckTraceRun, URL: runURL})
	}
	var where []string
	for _, f := range failed {
		if f.TraceID != "" {
			evidence.Traces = append(evidence.Traces, api.CheckTraceRef{Kind: api.CheckTraceFailed, Check: f.Name, TraceID: f.TraceID})
			where = append(where, "dagger trace "+f.TraceID)
		}
	}
	for _, name := range flaky {
		ref := api.CheckTraceRef{Kind: api.CheckTraceRerun, Check: name, URL: runURL}
		if c, ok := names[name]; ok {
			ref.TraceID = c.TraceID
			if c.TargetURL != "" {
				ref.URL = c.TargetURL
			}
		}
		evidence.Traces = append(evidence.Traces, ref)
		if ref.TraceID != "" {
			where = append(where, "dagger trace "+ref.TraceID)
		}
	}
	if len(where) == 0 && runURL != "" {
		where = []string{runURL}
	}
	read := strings.Join(where, "; ")
	evidence.Unavailable = []api.CheckUnavailable{
		{Item: api.CheckItemOutput, Engine: api.CheckEngineCloud, Where: read},
		{Item: api.CheckItemParallelism, Engine: api.CheckEngineCloud, Where: read},
	}
	return evidence
}

func cloudState(state string) string {
	switch state {
	case "success":
		return "passed"
	case "failure", "error":
		return "failed"
	}
	return state
}
