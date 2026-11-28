package delivery

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kpenfound/osmia/internal/api"
	checkrun "github.com/kpenfound/osmia/internal/checks"
	"github.com/kpenfound/osmia/internal/pulls"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	"github.com/kpenfound/osmia/internal/trace"
)

func (f *watchFixture) fakeTraceCLI(body string) (log string) {
	f.t.Helper()
	dir := f.t.TempDir()
	log = filepath.Join(dir, "trace.log")
	f.traceProgram = filepath.Join(dir, "dagger")
	script := fmt.Sprintf("#!/bin/sh\nprintf '%%s\\n' \"$*\" >> '%s'\n%s\n", log, body)
	fixturekit.Must(f.t, os.WriteFile(f.traceProgram, []byte(script), 0700))
	f.restart()
	return log
}

const fetchedOutput = `yes 'padding line of the trace output' | head -n 4000; echo "output of: $*"; echo "token: $DAGGER_CLOUD_TOKEN"`

func cliCalls(t *testing.T, log string) []string {
	t.Helper()
	data, err := os.ReadFile(log)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	fixturekit.Must(t, err)
	return strings.Split(strings.TrimSpace(string(data)), "\n")
}

func (f *watchFixture) traceFetcher() checkrun.TraceFetcher {
	return checkrun.TraceFetcher{Repository: f.repo, Now: f.clock.Now}
}

func (f *watchFixture) traceRecordFiles() map[string]string {
	f.t.Helper()
	docs, err := trace.Read[trace.Document](f.repo, stream)
	fixturekit.Must(f.t, err)
	out := map[string]string{}
	for _, d := range docs {
		if strings.HasPrefix(d.Path, checkrun.TracePathPrefix) {
			out[d.Path] = d.Content
		}
	}
	return out
}

func failing(f *watchFixture, specs ...[2]string) []func(time.Duration) pulls.CommitStatus {
	var out []func(time.Duration) pulls.CommitStatus
	for i, spec := range specs {
		id, name, traceID := int64(30+i), spec[0], spec[1]
		out = append(out, func(at time.Duration) pulls.CommitStatus { return cloudFailedStatus(f, id, name, traceID, at) })
	}
	return out
}

func (f *watchFixture) view() map[string]string {
	f.t.Helper()
	files, err := FinalCheckFiles(f.repo, stream, f.report)
	fixturekit.Must(f.t, err)
	return files
}

func viewPathOf(traceID, check string) string {
	return finalTraceView(1) + "/" + checkrun.TraceDocumentID(traceID, check) + ".json"
}

func TestFailedCloudRunRecordsEachFailedChecksTraceAndShowsItToTheFinalReader(t *testing.T) {
	t.Parallel()
	f := newWatchFixture(t)
	f.assemble()
	log := f.fakeTraceCLI(fetchedOutput)
	run := settleFailedRun(f, 89, failing(f, [2]string{"test:a", traceA}, [2]string{"docs", traceA}, [2]string{"build", traceB}, [2]string{"lint", ""})...)
	if run.Status != "failed" || len(run.FailedChecks) != 4 || len(f.rerunCalls()) != 1 {
		t.Fatalf("run %+v, reruns %q", run, f.rerunCalls())
	}

	calls := cliCalls(t, log)
	slices.Sort(calls)
	want := []string{"cloud traces view --check=build " + traceB, "cloud traces view --check=docs " + traceA, "cloud traces view --check=test:a " + traceA}
	if !slices.Equal(calls, want) {
		t.Fatalf("the CLI ran %q, want %q", calls, want)
	}

	view := f.view()
	if view[FinalChecksPath(1)] == "" {
		t.Fatalf("the view lacks the check result: %v", view)
	}
	fetcher := f.traceFetcher()
	for _, c := range []struct{ trace, check string }{{traceA, "test:a"}, {traceA, "docs"}, {traceB, "build"}} {
		got, ok, err := fetcher.Lookup(stream, c.trace, c.check)
		fixturekit.Must(t, err)
		if !ok || got.Check != c.check || got.WholeTrace || !got.Truncated || got.Size < 100000 || len(got.Output) > checkrun.MaxTraceOutput {
			t.Fatalf("record of %s %s: %+v, found %v", c.trace, c.check, got, ok)
		}
		if !strings.Contains(got.Output, "output of: cloud traces view --check="+c.check+" "+c.trace) || strings.Contains(got.Output, cloudToken) {
			t.Errorf("record of %s %s holds the wrong or an unscrubbed output (%d bytes)", c.trace, c.check, len(got.Output))
		}
		shown := view[viewPathOf(c.trace, c.check)]
		if !strings.Contains(shown, "output of: cloud traces view --check="+c.check+" "+c.trace) || strings.Contains(shown, cloudToken) {
			t.Errorf("the view lacks the output of %s %s: %v", c.trace, c.check, slices.Sorted(maps.Keys(view)))
		}
	}
	if _, ok, _ := fetcher.Lookup(stream, traceA, ""); ok {
		t.Error("a narrowed fetch answers for the whole trace")
	}
	if len(view) != 4 {
		t.Errorf("the view holds %v", slices.Sorted(maps.Keys(view)))
	}

	if records := f.traceRecordFiles(); len(records) != 3 {
		t.Fatalf("records %v", slices.Sorted(maps.Keys(records)))
	}
	tool := checkrun.TraceTool(checkrun.TraceFetcher{Repository: f.repo, Runner: checkrun.DaggerChecks{Program: f.traceProgram, CloudToken: cloudToken}, Now: f.clock.Now}, f.manager("trace"), fixturekit.ServiceActor, trace.Manager)
	same := f.call(tool, `{"trace_id":"`+traceA+`","check":"test:a"}`)
	if same["recorded"] != true || same["path"] != checkrun.TraceDocumentPath(traceA, "test:a") || len(cliCalls(t, log)) != 3 {
		t.Fatalf("action for a recorded check: %v, CLI ran %d times", same, len(cliCalls(t, log)))
	}
	whole := f.call(tool, `{"trace_id":"`+traceA+`"}`)
	if whole["fetched"] != true || whole["recorded"] == true || len(cliCalls(t, log)) != 4 || whole["whole_trace"] != true {
		t.Fatalf("action for the whole trace: %v, CLI ran %d times", whole, len(cliCalls(t, log)))
	}
	records := f.traceRecordFiles()
	if _, ok := records[checkrun.TraceDocumentPath(traceA, "")]; !ok || len(records) != 4 {
		t.Fatalf("records %v", slices.Sorted(maps.Keys(records)))
	}
	if narrowed, _, _ := fetcher.Lookup(stream, traceA, "test:a"); strings.Contains(narrowed.Output, "cloud traces view "+traceA) {
		t.Error("the whole-trace output replaced a narrowed record")
	}

	f.mustComplete()
	f.restart()
	f.mustComplete()
	if got := len(cliCalls(t, log)); got != 4 || len(f.runs()) != 1 {
		t.Fatalf("the CLI ran %d times, %d results recorded", got, len(f.runs()))
	}
}

func TestFailedLoadFetchesTheLoadCheckTrace(t *testing.T) {
	t.Parallel()
	f := newWatchFixture(t)
	f.assemble()
	log := f.fakeTraceCLI(`echo "load failed: $*"`)
	f.afterWait()
	f.api.set(pulls.CommitStatus{ID: 1, Context: "load", State: "failure", Description: "Failed in 12s. Run `dagger trace " + traceLoad + "` to view the full trace.", CreatedAt: f.at(30 * time.Second)})
	run := f.mustComplete()
	if run.Status != "failed" {
		t.Fatalf("run %+v", run)
	}
	if calls := cliCalls(t, log); !slices.Equal(calls, []string{"cloud traces view --check=load " + traceLoad}) {
		t.Fatalf("the CLI ran %q", calls)
	}
	if shown := f.view()[viewPathOf(traceLoad, "load")]; !strings.Contains(shown, "load failed: cloud traces view --check=load "+traceLoad) {
		t.Fatalf("the view lacks the load trace: %v", slices.Sorted(maps.Keys(f.view())))
	}
}

func TestNoTraceIsFetchedWithoutAFailedCheckThatNamesOne(t *testing.T) {
	t.Parallel()
	t.Run("failed checks without a trace id", func(t *testing.T) {
		t.Parallel()
		f := newWatchFixture(t)
		f.assemble()
		log := f.fakeTraceCLI(`echo output`)
		run := settleFailedRun(f, 90, failing(f, [2]string{"lint", ""})...)
		if run.Status != "failed" || len(cliCalls(t, log)) != 0 || len(f.traceRecordFiles()) != 0 || len(f.view()) != 1 {
			t.Fatalf("run %+v, CLI ran %q, records %v", run, cliCalls(t, log), f.traceRecordFiles())
		}
	})
	t.Run("a failed run naming no failing check", func(t *testing.T) {
		t.Parallel()
		f := newWatchFixture(t)
		f.assemble()
		log := f.fakeTraceCLI(`echo output`)
		settleFailedRun(f, 91)
		if len(cliCalls(t, log)) != 0 || len(f.traceRecordFiles()) != 0 {
			t.Fatalf("the CLI ran %q", cliCalls(t, log))
		}
	})
	t.Run("a passed result", func(t *testing.T) {
		t.Parallel()
		f := newWatchFixture(t)
		log := f.fakeTraceCLI(`echo output`)
		f.afterWait()
		f.api.setWithChecks(92, f.loaded(), f.done(3, "success", 100*time.Second))
		if run := f.mustComplete(); run.Status != "passed" || len(cliCalls(t, log)) != 0 || len(f.traceRecordFiles()) != 0 {
			t.Fatalf("run %+v, CLI ran %q", run, cliCalls(t, log))
		}
	})
	t.Run("an incomplete result", func(t *testing.T) {
		t.Parallel()
		f := newWatchFixture(t)
		f.cfg.Project.ChecksTimeout = "30m"
		log := f.fakeTraceCLI(`echo output`)
		f.afterWait()
		f.api.setWithChecks(90, f.loaded(), f.done(3, "success", 100*time.Second), pulls.CommitStatus{ID: 70, Context: "slow-a", State: "pending"})
		if _, err := f.check(); err == nil {
			t.Fatal("an incomplete run settled")
		}
		if runs := f.runs(); len(runs) != 1 || runs[0].Status != ChecksIncomplete || len(cliCalls(t, log)) != 0 || len(f.traceRecordFiles()) != 0 {
			t.Fatalf("runs %+v, CLI ran %q", runs, cliCalls(t, log))
		}
	})
	t.Run("a service with no trace fetch", func(t *testing.T) {
		t.Parallel()
		f := newWatchFixture(t)
		f.assemble()
		run := settleFailedRun(f, 89, failing(f, [2]string{"test:a", traceA})...)
		if run.Status != "failed" || len(f.traceRecordFiles()) != 0 {
			t.Fatalf("run %+v, records %v", run, f.traceRecordFiles())
		}
	})
}

func TestFailedTraceFetchIsRecordedWithItsReasonAndChangesNothing(t *testing.T) {
	t.Parallel()
	baseline := newWatchFixture(t)
	baseline.assemble()
	want := settleFailedRun(baseline, 89, failing(baseline, [2]string{"test:a", traceA}, [2]string{"build", traceB})...)

	hang := &hangingTraces{release: make(chan struct{})}
	t.Cleanup(func() { close(hang.release) })
	cases := []struct {
		name   string
		reason checkrun.TraceFailureReason
		setup  func(f *watchFixture) (calls func() int)
	}{
		{"the CLI fails", checkrun.TraceTokenRejected, func(f *watchFixture) func() int {
			log := f.fakeTraceCLI(`echo "Error: 401 Unauthorized token=$DAGGER_CLOUD_TOKEN" >&2; exit 1`)
			return func() int { return len(cliCalls(t, log)) }
		}},
		{"the CLI cannot run", checkrun.TraceCLIUnavailable, func(f *watchFixture) func() int {
			f.traceProgram = filepath.Join(t.TempDir(), "missing-dagger")
			return func() int { return 0 }
		}},
		{"the CLI hangs", checkrun.TraceTimeout, func(f *watchFixture) func() int {
			log := f.fakeTraceCLI(`exec sleep 60`)
			f.cliTimeout = 300 * time.Millisecond
			return func() int { return len(cliCalls(t, log)) }
		}},
		{"the fetch ignores its deadline", checkrun.TraceTimeout, func(f *watchFixture) func() int {
			f.traces = hang
			f.autoTimeout = 200 * time.Millisecond
			return func() int { return int(hang.calls.Load()) }
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newWatchFixture(t)
			f.assemble()
			calls := c.setup(f)
			f.restart()
			began := time.Now()
			got := settleFailedRun(f, 89, failing(f, [2]string{"test:a", traceA}, [2]string{"build", traceB})...)
			if elapsed := time.Since(began); elapsed > 30*time.Second {
				t.Fatalf("the review was held %s", elapsed)
			}
			if !sameResult(t, want, got) || len(f.runs()) != 1 || len(f.rerunCalls()) != 1 {
				t.Fatalf("result %+v, want %+v; %d records, %d reruns", got, want, len(f.runs()), len(f.rerunCalls()))
			}
			fetcher := f.traceFetcher()
			for _, p := range []struct{ trace, check string }{{traceA, "test:a"}, {traceB, "build"}} {
				rec, ok, err := fetcher.LookupFailure(stream, p.trace, p.check)
				fixturekit.Must(t, err)
				if !ok || rec.Reason != c.reason || rec.Check != p.check || rec.Message == "" || strings.Contains(rec.Message, cloudToken) {
					t.Fatalf("failure of %s %s: %+v, found %v", p.trace, p.check, rec, ok)
				}
				if _, ok, _ := fetcher.Lookup(stream, p.trace, p.check); ok {
					t.Errorf("a failed fetch left an output record for %s %s", p.trace, p.check)
				}
				if shown := f.view()[finalTraceView(1)+"/"+checkrun.TraceFailureDocumentID(p.trace, p.check)+".json"]; !strings.Contains(shown, string(c.reason)) {
					t.Errorf("the view lacks the failure of %s %s", p.trace, p.check)
				}
			}
			made := calls()
			f.mustComplete()
			f.restart()
			f.mustComplete()
			if calls() != made {
				t.Errorf("a retry fetched again: %d then %d", made, calls())
			}
			if _, ok, _ := fetcher.Lookup(stream, traceA, "test:a"); ok {
				t.Error("the failure answers the action")
			}
		})
	}
}

func sameResult(t *testing.T, want, got FinalCheckRun) bool {
	t.Helper()
	a, err := json.Marshal(want)
	fixturekit.Must(t, err)
	b, err := json.Marshal(got)
	fixturekit.Must(t, err)
	return string(a) == string(b)
}

type hangingTraces struct {
	calls   atomic.Int32
	release chan struct{}
}

func (h *hangingTraces) FetchTrace(context.Context, string, string) (checkrun.TraceOutput, *checkrun.TraceFailure) {
	h.calls.Add(1)
	<-h.release
	return checkrun.TraceOutput{}, nil
}

var errInterrupted = errors.New("interrupted")

func TestInterruptedFinalChecksFillOnlyTheMissingTraces(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name      string
		interrupt string
		survive   int
	}{
		{"before any trace is recorded", "final-checks-recorded", 0},
		{"after one of three traces", "final-trace-recorded", 1},
		{"after two of three traces", "final-trace-recorded", 2},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			f := newWatchFixture(t)
			f.assemble()
			log := f.fakeTraceCLI(`echo "output of: $*"`)
			seen := 0
			f.step = func(name string) error {
				if name != c.interrupt {
					return nil
				}
				if seen++; seen >= max(c.survive, 1) {
					return errInterrupted
				}
				return nil
			}
			f.restart()
			driveFailedRun(f, 88, failing(f, [2]string{"test:a", traceA}, [2]string{"docs", traceA}, [2]string{"build", traceB})...)
			if _, err := f.check(); !errors.Is(err, errInterrupted) {
				t.Fatalf("the interrupted read returned %v", err)
			}
			recorded := f.runs()
			if len(recorded) != 1 || recorded[0].Status != "failed" || len(f.rerunCalls()) != 1 {
				t.Fatalf("recorded %+v, reruns %q", recorded, f.rerunCalls())
			}
			before := len(cliCalls(t, log))
			if before != c.survive || len(f.traceRecordFiles()) != c.survive {
				t.Fatalf("%d fetches and %d records before the retry, want %d", before, len(f.traceRecordFiles()), c.survive)
			}
			had := f.traceRecordFiles()

			f.step = nil
			f.restart()
			run := f.mustComplete()
			if !sameResult(t, recorded[0], run) || len(f.runs()) != 1 || len(f.rerunCalls()) != 1 {
				t.Fatalf("the retry changed the result: %+v then %+v; %d records, %d reruns", recorded[0], run, len(f.runs()), len(f.rerunCalls()))
			}
			after := cliCalls(t, log)
			if len(after) != 3 {
				t.Fatalf("the CLI ran %q, want each of the three traces once", after)
			}
			for path, content := range had {
				if f.traceRecordFiles()[path] != content {
					t.Errorf("%s was rewritten", path)
				}
			}
			view := f.view()
			for _, p := range []struct{ trace, check string }{{traceA, "test:a"}, {traceA, "docs"}, {traceB, "build"}} {
				if !strings.Contains(view[viewPathOf(p.trace, p.check)], "output of: cloud traces view --check="+p.check+" "+p.trace) {
					t.Errorf("the view lacks %s %s: %v", p.trace, p.check, slices.Sorted(maps.Keys(view)))
				}
			}
			if run.Status != "failed" || len(run.FailedChecks) != 3 {
				t.Errorf("run %+v", run)
			}
		})
	}
}

func TestFinalReaderViewIsBuiltFromRecordsOfTheReviewedCommitOnly(t *testing.T) {
	t.Parallel()
	_, repo := deliveryTrace(t)
	run := FinalCheckRun{Review: 1, Operation: "review", Commit: reviewedCommit, Source: api.CheckSourcePullRequest, PullRequest: 7, Push: 1, PushedCommit: pushedCommit, Status: "failed",
		FailedChecks: []api.CheckFailure{{Name: "test:a", TraceID: "-x"}, {Name: "build", TraceID: traceB}, {Name: "lint"}}, Failed: []string{"test:a", "build", "lint"}}
	data, err := json.MarshalIndent(run, "", "  ")
	fixturekit.Must(t, err)
	fixturekit.Must(t, repo.RecordDocuments(context.Background(), []trace.Document{{Header: checkRecordHeader(repo, FinalChecksDocument(1), 1), Path: FinalChecksPath(1), Content: string(data) + "\n"}}))
	files, err := FinalCheckFiles(repo, stream, FinalReport{Review: 1, Commit: reviewedCommit})
	fixturekit.Must(t, err)
	if len(files) != 1 {
		t.Fatalf("a result with no recorded trace holds %v", slices.Sorted(maps.Keys(files)))
	}
	if pairs := failedTracePairs(run); len(pairs) != 1 || pairs[0].Check != "build" {
		t.Fatalf("pairs %+v: a flag-shaped trace ID or a check with none must not be fetched", pairs)
	}
}
