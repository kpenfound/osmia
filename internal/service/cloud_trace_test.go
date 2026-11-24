package service

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/kpenfound/osmia/internal/checks"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/questions"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	"github.com/kpenfound/osmia/internal/servicetest/intakefixture"
	"github.com/kpenfound/osmia/internal/trace"
)

const cloudTraceToken = "dct_service_fixture_token_51c0e"

func fakeTraceProgram(t *testing.T, body string) (program string, calls func() int) {
	t.Helper()
	dir := t.TempDir()
	log := filepath.Join(dir, "calls")
	program = filepath.Join(dir, "dagger")
	intakefixture.Must(t, os.WriteFile(program, []byte("#!/bin/sh\nprintf '%s\\n' \"$*\" >> '"+log+"'\n"+body), 0700))
	return program, func() int {
		raw, err := os.ReadFile(log)
		if os.IsNotExist(err) {
			return 0
		}
		intakefixture.Must(t, err)
		return strings.Count(string(raw), "\n")
	}
}

type traceResult struct {
	Fetched    bool   `json:"fetched"`
	Reason     string `json:"reason"`
	Message    string `json:"message"`
	Path       string `json:"path"`
	Citation   string `json:"citation"`
	TraceID    string `json:"trace_id"`
	Check      string `json:"check"`
	WholeTrace bool   `json:"whole_trace"`
	Output     string `json:"output"`
	Truncated  bool   `json:"truncated"`
	Size       int    `json:"size"`
	Recorded   bool   `json:"recorded"`
}

func callTrace(t *testing.T, tool coreadapter.Tool, input string) traceResult {
	t.Helper()
	var out traceResult
	intakefixture.Must(t, json.Unmarshal([]byte(intakefixture.Handle(t, tool, input)), &out))
	return out
}

type traceFetch struct {
	c     *contestFixture
	calls func() int
	tool  func() coreadapter.Tool
}

func newTraceFetch(t *testing.T) *traceFetch {
	t.Helper()
	c := newContestFixture(t, "cloud-trace")
	program, calls := fakeTraceProgram(t, "printf 'output of %s\\nauth %s\\n' \"$*\" \"$DAGGER_CLOUD_TOKEN\"\n")
	c.f.s.options.CloudTrace = DaggerChecks{CloudToken: cloudTraceToken, Program: program}
	now := func() time.Time { return fixturekit.DemoStart }
	return &traceFetch{c: c, calls: calls, tool: func() coreadapter.Tool { return c.controls.cloudTrace(c.f.s.about(c.repo), c.repo, c.scope, now) }}
}

func (f *traceFetch) fetch(t *testing.T, input string) traceResult {
	t.Helper()
	return callTrace(t, f.tool(), input)
}

func TestChiefFetchesTheWholeOfATraceAndRecordsIt(t *testing.T) {
	t.Parallel()
	f := newTraceFetch(t)
	whole := f.fetch(t, `{"trace_id":"t1"}`)
	if !whole.Fetched || whole.Recorded || !whole.WholeTrace || whole.Check != "" || whole.Path != "traces/t1-all.json" || whole.Citation != "trace#t1-all" || whole.Truncated || f.calls() != 1 {
		t.Fatalf("whole trace: %+v after %d calls", whole, f.calls())
	}
}

func TestChiefTraceOutputHidesTheCloudToken(t *testing.T) {
	t.Parallel()
	whole := newTraceFetch(t).fetch(t, `{"trace_id":"t1"}`)
	if !strings.Contains(whole.Output, "output of cloud traces view t1\n") || strings.Contains(whole.Output, cloudTraceToken) || !strings.Contains(whole.Output, "[redacted]") || whole.Size != len("output of cloud traces view t1\nauth "+cloudTraceToken+"\n") {
		t.Fatalf("whole trace output %q, size %d", whole.Output, whole.Size)
	}
}

func TestChiefRefetchingARecordedTraceReadsTheRecordWithoutRunningTheCLI(t *testing.T) {
	t.Parallel()
	f := newTraceFetch(t)
	whole := f.fetch(t, `{"trace_id":"t1"}`)
	for range 2 {
		again := f.fetch(t, `{"trace_id":"t1"}`)
		if !again.Fetched || !again.Recorded || again.Output != whole.Output || again.Path != whole.Path || again.Citation != whole.Citation || f.calls() != 1 {
			t.Fatalf("repeat: %+v after %d calls", again, f.calls())
		}
	}
}

func TestChiefFetchesEachCheckOfATraceAndItsWholeSeparately(t *testing.T) {
	t.Parallel()
	f := newTraceFetch(t)
	a := f.fetch(t, `{"trace_id":"t2","check":"test:a"}`)
	if !a.Fetched || a.Recorded || a.WholeTrace || a.Check != "test:a" || f.calls() != 1 || !strings.Contains(a.Output, "output of cloud traces view --check=test:a t2") || !strings.HasPrefix(a.Path, "traces/t2-check-") {
		t.Fatalf("check A: %+v after %d calls", a, f.calls())
	}
	b := f.fetch(t, `{"trace_id":"t2","check":"test:b"}`)
	if !b.Fetched || b.Recorded || f.calls() != 2 || b.Path == a.Path || !strings.Contains(b.Output, "--check=test:b t2") || strings.Contains(b.Output, "test:a") {
		t.Fatalf("check B: %+v after %d calls", b, f.calls())
	}
	all := f.fetch(t, `{"trace_id":"t2"}`)
	if !all.Fetched || all.Recorded || f.calls() != 3 || all.Path == a.Path || all.Path == b.Path || strings.Contains(all.Output, "--check") {
		t.Fatalf("whole trace of t2: %+v after %d calls", all, f.calls())
	}
	if again := f.fetch(t, `{"trace_id":"t2","check":"test:a"}`); !again.Recorded || again.Output != a.Output || f.calls() != 3 {
		t.Fatalf("check A again: %+v after %d calls", again, f.calls())
	}
}

func TestChiefPassesACheckNamedLikeAFlagAsTheValueOfTheCheckOption(t *testing.T) {
	t.Parallel()
	dash := newTraceFetch(t).fetch(t, `{"trace_id":"t3","check":"--help"}`)
	if !dash.Fetched || !strings.Contains(dash.Output, "output of cloud traces view --check=--help t3\n") {
		t.Fatalf("dash check: %+v", dash)
	}
}

func TestFactoryContextListsEachRecordedTraceOnce(t *testing.T) {
	t.Parallel()
	f := newTraceFetch(t)
	paths := map[string]bool{}
	for _, input := range []string{`{"trace_id":"t1"}`, `{"trace_id":"t2","check":"test:a"}`, `{"trace_id":"t2","check":"test:b"}`, `{"trace_id":"t2"}`, `{"trace_id":"t3","check":"--help"}`} {
		paths[f.fetch(t, input).Path] = true
	}
	var listed map[string]int
	intakefixture.Must(t, json.Unmarshal([]byte(intakefixture.Handle(t, factoryContext(f.c.repo, f.c.scope), `{"path":"traces"}`)), &listed))
	if len(listed) != len(paths) || len(paths) != 5 {
		t.Fatalf("listed %v for %v", listed, paths)
	}
	for path := range paths {
		if listed[path] != 1 {
			t.Fatalf("listed %v lacks %s", listed, path)
		}
	}
}

func TestRecordedTraceHoldsItsTraceCheckFetchTimeAndOutput(t *testing.T) {
	t.Parallel()
	f := newTraceFetch(t)
	a := f.fetch(t, `{"trace_id":"t2","check":"test:a"}`)
	var doc struct {
		Content string `json:"content"`
	}
	intakefixture.Must(t, json.Unmarshal([]byte(intakefixture.Handle(t, factoryContext(f.c.repo, f.c.scope), `{"path":"`+a.Path+`"}`)), &doc))
	var record checks.TraceRecord
	intakefixture.Must(t, json.Unmarshal([]byte(doc.Content), &record))
	if record.TraceID != "t2" || record.Check != "test:a" || record.WholeTrace || record.Truncated || !record.FetchedAt.Equal(fixturekit.DemoStart) || record.Output != a.Output || record.Size != a.Size {
		t.Fatalf("record %+v", record)
	}
}

func TestTraceCitationsResolveOnlyForRecordedTraces(t *testing.T) {
	t.Parallel()
	f := newTraceFetch(t)
	a := f.fetch(t, `{"trace_id":"t2","check":"test:a"}`)
	intakefixture.Must(t, questions.Resolve(context.Background(), f.c.repo, f.c.stream, a.Citation))
	if err := questions.Resolve(context.Background(), f.c.repo, f.c.stream, "trace#t9-all"); err == nil {
		t.Fatal("an unrecorded trace citation resolved")
	}
}

func TestRecordedTracesNeverHoldTheCloudToken(t *testing.T) {
	t.Parallel()
	f := newTraceFetch(t)
	f.fetch(t, `{"trace_id":"t1"}`)
	docs, err := trace.Read[trace.Document](f.c.repo, f.c.stream)
	intakefixture.Must(t, err)
	for _, d := range docs {
		if strings.Contains(d.Content, cloudTraceToken) {
			t.Fatalf("%s holds the token", d.Path)
		}
	}
}

func TestFetchCloudTraceRefusesBadValuesWithoutRunningTheCLI(t *testing.T) {
	t.Parallel()
	c := newContestFixture(t, "cloud-trace-refusals")
	program, calls := fakeTraceProgram(t, "printf 'never\\n'\n")
	c.f.s.options.CloudTrace = DaggerChecks{CloudToken: cloudTraceToken, Program: program}
	tool := c.controls.cloudTrace(c.f.s.about(c.repo), c.repo, c.scope, func() time.Time { return fixturekit.DemoStart })
	long, _ := json.Marshal(strings.Repeat("a", checks.MaxTraceIDLength+1))
	for _, input := range []string{
		`{}`, `{"trace_id":""}`, `{"trace_id":` + string(long) + `}`, `{"trace_id":"a b"}`, `{"trace_id":"a\tb"}`, `{"trace_id":"a/b"}`, `{"trace_id":"a;b"}`, `{"trace_id":"a=b"}`,
		`{"trace_id":"-x"}`, `{"trace_id":"--help"}`, `{"trace_id":"t1","check":""}`, `{"trace_id":"t1","check":"a\nb"}`, `{"trace_id":"t1","flag":"-x"}`, `{"trace_id":"t1","check":"` + strings.Repeat("a", checks.MaxTraceCheckLength+1) + `"}`,
	} {
		if out, err := tool.Handle(context.Background(), json.RawMessage(input)); err == nil {
			t.Errorf("%s accepted: %s", input, out)
		}
	}
	if calls() != 0 {
		t.Fatalf("the CLI ran %d times", calls())
	}
	docs, err := trace.Read[trace.Document](c.repo, c.stream)
	intakefixture.Must(t, err)
	for _, d := range docs {
		if strings.HasPrefix(d.Path, "traces/") {
			t.Fatalf("a refused request recorded %s", d.Path)
		}
	}
}

func TestFetchCloudTraceIsGrantedOnlyToItsRoles(t *testing.T) {
	t.Parallel()
	c := newContestFixture(t, "cloud-trace-roles")
	program, calls := fakeTraceProgram(t, "printf 'trace\\n'\n")
	fetcher := checks.TraceFetcher{Repository: c.repo, Runner: DaggerChecks{CloudToken: cloudTraceToken, Program: program}}
	refused := checks.TraceTool(fetcher, c.scope, chiefActor, "committee")
	if out, err := refused.Handle(context.Background(), json.RawMessage(`{"trace_id":"t1"}`)); err == nil {
		t.Fatalf("a turn of another role fetched: %s", out)
	}
	if calls() != 0 {
		t.Fatal("a refused role ran the CLI")
	}
	granted := checks.TraceTool(fetcher, c.scope, chiefActor, trace.ChiefOfStaff)
	if !callTrace(t, granted, `{"trace_id":"t1"}`).Fetched || calls() != 1 {
		t.Fatalf("the chief of staff's fetch ran %d times", calls())
	}
	if !slices.Contains(chiefOrdinaryTools, checks.TraceToolName) || !slices.Contains(chiefGrant.Tools, checks.TraceToolName) {
		t.Fatalf("the chief of staff's grant lacks %s: %v", checks.TraceToolName, chiefGrant.Tools)
	}
	if slices.Contains(designOnlyTools, checks.TraceToolName) || slices.Contains(masonGrant.Tools, checks.TraceToolName) || slices.Contains(reviewerGrant.Tools, checks.TraceToolName) {
		t.Fatal("a role other than the chief of staff and the final reviewer holds the trace fetch")
	}
}

func TestFetchCloudTraceFailuresAreResultsNotErrors(t *testing.T) {
	t.Parallel()
	now := func() time.Time { return fixturekit.DemoStart }
	for _, tc := range []struct {
		name   string
		runner func(t *testing.T) DaggerChecks
		reason checks.TraceFailureReason
	}{
		{"no token", func(t *testing.T) DaggerChecks { return DaggerChecks{} }, checks.TraceNoToken},
		{"rejected token", func(t *testing.T) DaggerChecks {
			p, _ := fakeTraceProgram(t, "printf 'unauthorized: %s\\n' \"$DAGGER_CLOUD_TOKEN\" >&2\nexit 1\n")
			return DaggerChecks{CloudToken: cloudTraceToken, Program: p}
		}, checks.TraceTokenRejected},
		{"unknown trace", func(t *testing.T) DaggerChecks {
			p, _ := fakeTraceProgram(t, "printf 'trace not found\\n' >&2\nexit 1\n")
			return DaggerChecks{CloudToken: cloudTraceToken, Program: p}
		}, checks.TraceUnknown},
		{"timeout", func(t *testing.T) DaggerChecks {
			p, _ := fakeTraceProgram(t, "exec sleep 30\n")
			return DaggerChecks{CloudToken: cloudTraceToken, Program: p, TraceTimeout: 300 * time.Millisecond}
		}, checks.TraceTimeout},
		{"missing CLI", func(t *testing.T) DaggerChecks {
			return DaggerChecks{CloudToken: cloudTraceToken, Program: filepath.Join(t.TempDir(), "absent")}
		}, checks.TraceCLIUnavailable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			c := newContestFixture(t, "cloud-trace-"+strings.ReplaceAll(tc.name, " ", "-"))
			c.f.s.options.CloudTrace = tc.runner(t)
			tool := c.controls.cloudTrace(c.f.s.about(c.repo), c.repo, c.scope, now)
			raw, err := tool.Handle(context.Background(), json.RawMessage(`{"trace_id":"t1","check":"test:a"}`))
			if err != nil {
				t.Fatalf("the failure ended the turn: %v", err)
			}
			var got traceResult
			intakefixture.Must(t, json.Unmarshal(raw, &got))
			if got.Fetched || got.Reason != string(tc.reason) || got.Message == "" || got.Output != "" || strings.Contains(string(raw), cloudTraceToken) {
				t.Fatalf("result %s", raw)
			}
			docs, err := trace.Read[trace.Document](c.repo, c.stream)
			intakefixture.Must(t, err)
			for _, d := range docs {
				if strings.HasPrefix(d.Path, "traces/") {
					t.Fatalf("a failed fetch recorded %s", d.Path)
				}
			}
			p, _ := fakeTraceProgram(t, "printf 'now it works\\n'\n")
			c.f.s.options.CloudTrace = DaggerChecks{CloudToken: cloudTraceToken, Program: p}
			tool = c.controls.cloudTrace(c.f.s.about(c.repo), c.repo, c.scope, now)
			if next := callTrace(t, tool, `{"trace_id":"t1","check":"test:a"}`); !next.Fetched || next.Output != "now it works\n" {
				t.Fatalf("the next fetch: %+v", next)
			}
		})
	}
}

func TestFetchCloudTraceBoundsAndRecordsLargeOutput(t *testing.T) {
	t.Parallel()
	c := newContestFixture(t, "cloud-trace-large")
	program, _ := fakeTraceProgram(t, "i=0\nwhile [ $i -lt 5000 ]; do printf 'line %05d filler filler filler filler\\n' $i; i=$((i+1)); done\nprintf 'final failure\\n'\n")
	c.f.s.options.CloudTrace = DaggerChecks{CloudToken: cloudTraceToken, Program: program}
	tool := c.controls.cloudTrace(c.f.s.about(c.repo), c.repo, c.scope, func() time.Time { return fixturekit.DemoStart })
	got := callTrace(t, tool, `{"trace_id":"big"}`)
	full := 5000*len("line 00000 filler filler filler filler\n") + len("final failure\n")
	if !got.Fetched || !got.Truncated || got.Size != full || len(got.Output) > checks.MaxTraceOutput || !strings.HasSuffix(got.Output, "line 04999 filler filler filler filler\nfinal failure\n") {
		t.Fatalf("returned %d bytes, truncated %v, size %d (want %d)", len(got.Output), got.Truncated, got.Size, full)
	}
	docs, err := trace.Read[trace.Document](c.repo, c.stream)
	intakefixture.Must(t, err)
	var doc trace.Document
	for _, d := range docs {
		if d.Path == got.Path {
			doc = d
		}
	}
	var record checks.TraceRecord
	intakefixture.Must(t, json.Unmarshal([]byte(doc.Content), &record))
	if len(record.Output) > checks.MaxTraceOutput || !record.Truncated || record.Size != full || record.Output != got.Output {
		t.Fatalf("the record holds %d bytes, truncated %v, size %d", len(record.Output), record.Truncated, record.Size)
	}
}
