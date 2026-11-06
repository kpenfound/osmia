package checks

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/kpenfound/osmia/internal/api"
)

const (
	linkA = "dag+check://?check=test&go-package=a"
	linkB = "dag+check://?check=test&go-package=b&go-test=T"
	linkV = "dag+check://release/version"
)

// fakeCall is one call to a fakeRunner.
type fakeCall struct {
	links, skip []string
	started     time.Time
	deadline    time.Time
}

// fakeRunner is a ReviewChecks whose Check is script. It records its calls.
type fakeRunner struct {
	calls  []fakeCall
	script func(ctx context.Context, call fakeCall) (CheckResult, error)
}

func (f *fakeRunner) List(context.Context, string, Identity) ([]string, error) { return nil, nil }

func (f *fakeRunner) Check(ctx context.Context, dir string, identity Identity, links []string) (CheckResult, error) {
	return f.CheckSkipping(ctx, dir, identity, links, nil)
}

func (f *fakeRunner) CheckSkipping(ctx context.Context, _ string, _ Identity, links, skip []string) (CheckResult, error) {
	call := fakeCall{links: links, skip: skip, started: time.Now()}
	call.deadline, _ = ctx.Deadline()
	f.calls = append(f.calls, call)
	return f.script(ctx, call)
}

// plainRunner is a ReviewChecks that cannot skip checks.
type plainRunner struct{ r *fakeRunner }

func (p plainRunner) List(ctx context.Context, dir string, identity Identity) ([]string, error) {
	return p.r.List(ctx, dir, identity)
}

func (p plainRunner) Check(ctx context.Context, dir string, identity Identity, links []string) (CheckResult, error) {
	return p.r.Check(ctx, dir, identity, links)
}

func passes(links ...string) CheckResult {
	out := fmt.Sprintf("CHECKS  ✔ %d passed\n", len(links))
	for _, link := range links {
		out += "✔ " + link + " 1s OK\n"
	}
	return CheckResult{Output: out}
}

func failure(link string) CheckResult {
	return CheckResult{ExitCode: 1, Output: "CHECKS  ✘ 1 failed\n✘ " + link + " 1s ERROR\n  wrong answer\n"}
}

// unfinished is the result of a run that reported passes and then ended.
func unfinished(passed ...string) CheckResult {
	result := passes(passed...)
	result.ExitCode = -1
	return result
}

// blocked waits for the engine's budget to end, as a run that cannot finish.
func blocked(passed ...string) func(context.Context, fakeCall) (CheckResult, error) {
	return func(ctx context.Context, _ fakeCall) (CheckResult, error) {
		<-ctx.Done()
		return unfinished(passed...), ctx.Err()
	}
}

func always(result CheckResult, err error) func(context.Context, fakeCall) (CheckResult, error) {
	return func(context.Context, fakeCall) (CheckResult, error) { return result, err }
}

func fake(script func(context.Context, fakeCall) (CheckResult, error)) *fakeRunner {
	return &fakeRunner{script: script}
}

const testBudget = 400 * time.Millisecond

func runFallback(t *testing.T, local, cloud ReviewChecks, remembered Remembered, links ...string) (CheckResult, api.CheckSelection, *api.CheckAttribution, error) {
	t.Helper()
	selection := api.CheckSelection{Mode: SelectionSelected, Links: links, Judgment: "j"}
	if len(links) == 0 {
		selection = FullSelection("unlisted", "")
	}
	return RunSelectionWithFallback(t.Context(), local, cloud, testBudget, remembered, t.TempDir(), Identity{}, selection)
}

func TestWithoutCloudRunsSelectionUnderOneBudget(t *testing.T) {
	t.Parallel()
	for name, c := range map[string]struct {
		links  []string
		script func(context.Context, fakeCall) (CheckResult, error)
	}{
		"passes":    {[]string{linkA, linkV}, func(_ context.Context, c fakeCall) (CheckResult, error) { return passes(c.links...), nil }},
		"fails":     {[]string{linkA, linkV}, always(failure(linkA), nil)},
		"times out": {[]string{linkA, linkV}, blocked(linkA)},
		"refused": {[]string{linkA, linkV}, func(_ context.Context, c fakeCall) (CheckResult, error) {
			if len(c.links) > 0 {
				return CheckResult{ExitCode: 1, Output: "Error: no checks matched\n", Refusal: "no checks matched"}, nil
			}
			return passes(linkA), nil
		}},
		"every check": {nil, always(passes(linkA), nil)},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			selection := api.CheckSelection{Mode: SelectionSelected, Links: c.links, Judgment: "j", Candidates: 4}
			if c.links == nil {
				selection = FullSelection("unlisted", "")
			}
			direct := fake(c.script)
			bounded, cancel := context.WithTimeout(t.Context(), testBudget)
			defer cancel()
			want, wantSelection, wantErr := RunSelection(bounded, direct, "dir", Identity{}, selection)
			wrapped := fake(c.script)
			got, gotSelection, attribution, gotErr := RunSelectionWithFallback(t.Context(), wrapped, nil, testBudget, Remembered{}, "dir", Identity{}, selection)
			if !reflect.DeepEqual(got, want) || !reflect.DeepEqual(gotSelection, wantSelection) || fmt.Sprint(gotErr) != fmt.Sprint(wantErr) {
				t.Fatalf("got %+v %+v %v\nwant %+v %+v %v", got, gotSelection, gotErr, want, wantSelection, wantErr)
			}
			if len(wrapped.calls) != len(direct.calls) {
				t.Fatalf("calls %d, want %d", len(wrapped.calls), len(direct.calls))
			}
			gs, ws := statusOf(got, gotErr), statusOf(want, wantErr)
			if gs != ws {
				t.Fatalf("status %s, want %s", gs, ws)
			}
			if attribution.UsedCloud() || len(attribution.Runs) == 0 {
				t.Fatalf("attribution %+v", attribution)
			}
			for _, r := range attribution.Runs {
				if r.Engine != api.CheckEngineLocal || r.Reason != "" {
					t.Fatalf("attribution %+v", attribution)
				}
			}
			// One budget spans every call.
			for _, call := range wrapped.calls {
				if !call.deadline.Equal(wrapped.calls[0].deadline) {
					t.Fatalf("deadlines differ: %v", wrapped.calls)
				}
			}
		})
	}
}

// statusOf is the status ClassifyChecks gives.
func statusOf(result CheckResult, err error) string {
	status, _, _ := ClassifyChecks(result, err)
	return status
}

func TestIncompleteLocalWorkRunsOnCloud(t *testing.T) {
	t.Parallel()
	for name, c := range map[string]struct {
		local  func(context.Context, fakeCall) (CheckResult, error)
		reason string
	}{
		"budget ran out":              {blocked(), api.CloudAfterTimeout},
		"session lost":                {always(CheckResult{ExitCode: 1, Output: "Error: engine closed session\n"}, nil), api.CloudAfterEngineFailure},
		"exit without a failed check": {always(CheckResult{ExitCode: 7, Output: "CHECKS  ✔ 0 passed\nError: boom\n"}, nil), api.CloudAfterEngineFailure},
		"killed":                      {always(CheckResult{ExitCode: -1}, nil), api.CloudAfterEngineFailure},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			local := fake(c.local)
			cloud := fake(func(_ context.Context, call fakeCall) (CheckResult, error) { return passes(call.links...), nil })
			result, selection, attribution, err := runFallback(t, local, cloud, Remembered{}, linkA)
			must(t, err)
			if status, _, _ := ClassifyChecks(result, err); status != ChecksPassed || result.ExitCode != 0 {
				t.Fatalf("%s %d: %s", status, result.ExitCode, result.Output)
			}
			if len(local.calls) != 1 || len(cloud.calls) != 1 || !slices.Equal(cloud.calls[0].links, []string{linkA}) {
				t.Fatalf("local %+v cloud %+v", local.calls, cloud.calls)
			}
			want := []api.CheckEngineRun{{Engine: api.CheckEngineCloud, Links: []string{linkA}, Reason: c.reason}}
			if !reflect.DeepEqual(attribution.Runs, want) {
				t.Fatalf("attribution %+v", attribution.Runs)
			}
			if selection.Mode != SelectionSelected || !slices.Equal(selection.Links, []string{linkA}) {
				t.Fatalf("selection %+v", selection)
			}
			for _, engine := range []string{"== local engine ==", "== cloud engine =="} {
				if !strings.Contains(result.Output, engine) {
					t.Fatalf("output lacks %q:\n%s", engine, result.Output)
				}
			}
		})
	}
}

func TestLocalInvocationsShareOneBudget(t *testing.T) {
	t.Parallel()
	local := fake(func(ctx context.Context, call fakeCall) (CheckResult, error) {
		if slices.Contains(call.links, linkA) {
			time.Sleep(testBudget / 2)
			return passes(linkA), nil
		}
		<-ctx.Done()
		return unfinished(), ctx.Err()
	})
	cloud := fake(func(_ context.Context, call fakeCall) (CheckResult, error) { return passes(call.links...), nil })
	start := time.Now()
	result, _, attribution, err := runFallback(t, local, cloud, Remembered{}, linkA, linkB, linkV)
	must(t, err)
	if len(local.calls) != 2 {
		t.Fatalf("local calls %+v: later invocations wait for cloud once the budget ran out", local.calls)
	}
	if !local.calls[0].deadline.Equal(local.calls[1].deadline) || local.calls[1].deadline.Sub(local.calls[1].started) > testBudget/2+50*time.Millisecond {
		t.Fatalf("the budget restarted: %+v", local.calls)
	}
	if elapsed := time.Since(start); elapsed > testBudget+testBudget/2 {
		t.Fatalf("took %s", elapsed)
	}
	if got := cloud.calls; len(got) != 2 || !slices.Equal(got[0].links, []string{linkB}) || !slices.Equal(got[1].links, []string{linkV}) {
		t.Fatalf("cloud calls %+v", got)
	}
	if status, _, _ := ClassifyChecks(result, nil); status != ChecksPassed {
		t.Fatalf("status %s", status)
	}
	want := []api.CheckEngineRun{
		{Engine: api.CheckEngineLocal, Links: []string{linkA}},
		{Engine: api.CheckEngineCloud, Links: []string{linkB}, Reason: api.CloudAfterTimeout},
		{Engine: api.CheckEngineCloud, Links: []string{linkV}, Reason: api.CloudAfterTimeout},
	}
	if !reflect.DeepEqual(attribution.Runs, want) {
		t.Fatalf("attribution %+v", attribution.Runs)
	}
}

func TestCloudWorkShareOneBudgetOfItsOwn(t *testing.T) {
	t.Parallel()
	// linkA is remembered, linkB times out locally, linkV passes locally with
	// a flaky check. The local run spends its whole budget.
	local := fake(func(ctx context.Context, call fakeCall) (CheckResult, error) {
		if slices.Contains(call.links, linkV) {
			result := passes(linkV)
			result.Flaky = []string{"dag://flaky"}
			return result, nil
		}
		<-ctx.Done()
		return unfinished(), ctx.Err()
	})
	cloud := fake(func(_ context.Context, call fakeCall) (CheckResult, error) {
		time.Sleep(20 * time.Millisecond)
		result := passes(call.links...)
		if slices.Contains(call.links, linkA) {
			result.Flaky = []string{"dag://cloud-flaky"}
			result.TraceURL = "https://dagger.cloud/o/traces/cloud"
		}
		return result, nil
	})
	result, _, attribution, err := runFallback(t, local, cloud, Remembered{Links: []string{linkA}}, linkA, linkV, linkB)
	must(t, err)
	if len(local.calls) != 2 || slices.ContainsFunc(local.calls, func(c fakeCall) bool { return slices.Contains(c.links, linkA) }) {
		t.Fatalf("local calls %+v", local.calls)
	}
	if len(cloud.calls) != 2 {
		t.Fatalf("cloud calls %+v", cloud.calls)
	}
	first, second := cloud.calls[0], cloud.calls[1]
	if !first.deadline.Equal(second.deadline) {
		t.Fatalf("the cloud budget restarted: %v %v", first.deadline, second.deadline)
	}
	if left := first.deadline.Sub(first.started); left < testBudget-50*time.Millisecond || left > testBudget {
		t.Fatalf("cloud budget at its start: %s", left)
	}
	if !first.deadline.After(local.calls[0].deadline) {
		t.Fatalf("cloud budget is not independent of local time: %v, local %v", first.deadline, local.calls[0].deadline)
	}
	want := []api.CheckEngineRun{
		{Engine: api.CheckEngineLocal, Links: []string{linkV}},
		{Engine: api.CheckEngineCloud, Links: []string{linkB}, Reason: api.CloudAfterTimeout},
		{Engine: api.CheckEngineCloud, Links: []string{linkA}, Reason: api.CloudByMemory},
	}
	if !reflect.DeepEqual(attribution.Runs, want) {
		t.Fatalf("attribution %+v", attribution.Runs)
	}
	if !slices.Equal(result.Flaky, []string{"dag://flaky", "dag://cloud-flaky"}) || result.TraceURL != "https://dagger.cloud/o/traces/cloud" {
		t.Fatalf("flaky %q trace %q", result.Flaky, result.TraceURL)
	}
	if status, _, _ := ClassifyChecks(result, nil); status != ChecksPassed {
		t.Fatalf("status %s", status)
	}
}

func TestOnlyUnfinishedWorkRunsOnCloud(t *testing.T) {
	t.Parallel()
	local := fake(func(ctx context.Context, call fakeCall) (CheckResult, error) {
		if slices.Contains(call.links, linkA) {
			return passes("dag://a1", "dag://a2"), nil
		}
		<-ctx.Done()
		return unfinished(), ctx.Err()
	})
	cloud := fake(func(_ context.Context, call fakeCall) (CheckResult, error) { return passes("dag://v"), nil })
	result, _, _, err := runFallback(t, local, cloud, Remembered{}, linkA, linkV)
	must(t, err)
	if len(cloud.calls) != 1 || !slices.Equal(cloud.calls[0].links, []string{linkV}) {
		t.Fatalf("cloud calls %+v", cloud.calls)
	}
	if got := PassedChecks(result.Output); !slices.Equal(got, []string{"dag://a1", "dag://a2", "dag://v"}) {
		t.Fatalf("passed %q:\n%s", got, result.Output)
	}
}

func TestPassedChecksOfAnUnfinishedInvocationAreNotRerun(t *testing.T) {
	t.Parallel()
	const merged = "dag+check://?check=test&go-package=b"
	for name, links := range map[string][]string{"filtered links": {linkA, merged}, "every check": nil} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			local := fake(blocked("dag://a"))
			cloud := fake(always(passes("dag://b"), nil))
			result, _, attribution, err := runFallback(t, local, cloud, Remembered{}, links...)
			must(t, err)
			if len(cloud.calls) != 1 || !slices.Equal(cloud.calls[0].skip, []string{"dag://a"}) {
				t.Fatalf("cloud calls %+v", cloud.calls)
			}
			if !slices.Equal(cloud.calls[0].links, links) {
				t.Fatalf("cloud links %q, want %q", cloud.calls[0].links, links)
			}
			if got := PassedChecks(result.Output); !slices.Equal(got, []string{"dag://a", "dag://b"}) {
				t.Fatalf("passed %q:\n%s", got, result.Output)
			}
			if status, _, _ := ClassifyChecks(result, err); status != ChecksPassed {
				t.Fatalf("status %s", status)
			}
			if len(attribution.Runs) != 1 || attribution.Runs[0].PassedLocally != 1 || attribution.Runs[0].EveryCheck != (links == nil) {
				t.Fatalf("attribution %+v", attribution.Runs)
			}
		})
	}
}

func TestInvocationRunsWholeOnCloudWithoutPassesOrSkipping(t *testing.T) {
	t.Parallel()
	t.Run("no passes reported", func(t *testing.T) {
		t.Parallel()
		cloud := fake(always(passes("dag://a"), nil))
		_, _, attribution, err := runFallback(t, fake(blocked()), cloud, Remembered{}, linkA)
		must(t, err)
		if len(cloud.calls) != 1 || len(cloud.calls[0].skip) != 0 || attribution.Runs[0].PassedLocally != 0 {
			t.Fatalf("cloud calls %+v, attribution %+v", cloud.calls, attribution.Runs)
		}
	})
	t.Run("runner that cannot skip", func(t *testing.T) {
		t.Parallel()
		cloud := fake(always(passes("dag://a", "dag://b"), nil))
		_, _, attribution, err := runFallback(t, fake(blocked("dag://a")), plainRunner{cloud}, Remembered{}, linkA)
		must(t, err)
		if len(cloud.calls) != 1 || len(cloud.calls[0].skip) != 0 || attribution.Runs[0].PassedLocally != 0 {
			t.Fatalf("cloud calls %+v, attribution %+v", cloud.calls, attribution.Runs)
		}
	})
}

func TestReportedFailureStaysLocal(t *testing.T) {
	t.Parallel()
	local := fake(func(_ context.Context, call fakeCall) (CheckResult, error) {
		if slices.Contains(call.links, linkA) {
			return failure("dag://a"), nil
		}
		return passes(call.links...), nil
	})
	cloud := fake(always(passes(), nil))
	result, _, attribution, err := runFallback(t, local, cloud, Remembered{Links: []string{linkV}}, linkA, linkV)
	must(t, err)
	status, failed, _ := ClassifyChecks(result, err)
	if status != ChecksFailed || !slices.Equal(failed, []string{"dag://a"}) || result.ExitCode != 1 {
		t.Fatalf("%s %q %d", status, failed, result.ExitCode)
	}
	if len(cloud.calls) != 0 {
		t.Fatalf("cloud calls %+v", cloud.calls)
	}
	if attribution.UsedCloud() {
		t.Fatalf("attribution %+v", attribution.Runs)
	}
}

func TestNoCloudAttemptWhenCancelledOrNotPrepared(t *testing.T) {
	t.Parallel()
	t.Run("cancelled", func(t *testing.T) {
		t.Parallel()
		ctx, cancel := context.WithCancel(t.Context())
		local := fake(func(ctx context.Context, _ fakeCall) (CheckResult, error) {
			cancel()
			<-ctx.Done()
			return unfinished(), ctx.Err()
		})
		cloud := fake(always(passes(), nil))
		_, _, _, err := RunSelectionWithFallback(ctx, local, cloud, testBudget, Remembered{}, "dir", Identity{}, api.CheckSelection{Mode: SelectionSelected, Links: []string{linkA}})
		if !errors.Is(err, context.Canceled) || len(cloud.calls) != 0 {
			t.Fatalf("err %v, cloud calls %+v", err, cloud.calls)
		}
	})
	t.Run("parent deadline", func(t *testing.T) {
		t.Parallel()
		ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
		defer cancel()
		cloud := fake(always(passes(), nil))
		_, _, _, err := RunSelectionWithFallback(ctx, fake(blocked()), cloud, time.Minute, Remembered{}, "dir", Identity{}, api.CheckSelection{Mode: SelectionSelected, Links: []string{linkA}})
		if !errors.Is(err, context.DeadlineExceeded) || len(cloud.calls) != 0 {
			t.Fatalf("err %v, cloud calls %+v", err, cloud.calls)
		}
	})
	t.Run("export error", func(t *testing.T) {
		t.Parallel()
		cloud := fake(always(passes(), nil))
		exportErr := errors.New("export: no space left")
		result, _, _, err := runFallback(t, fake(always(CheckResult{ExitCode: -1}, exportErr)), cloud, Remembered{}, linkA)
		if !errors.Is(err, exportErr) || len(cloud.calls) != 0 {
			t.Fatalf("err %v, cloud calls %+v", err, cloud.calls)
		}
		if status, _, _ := ClassifyChecks(result, err); status != ChecksIncomplete {
			t.Fatalf("status %s", status)
		}
	})
}

func TestRefusedLinksRunEveryCheckLocallyWithoutCloud(t *testing.T) {
	t.Parallel()
	local := fake(func(_ context.Context, call fakeCall) (CheckResult, error) {
		if len(call.links) > 0 {
			return CheckResult{ExitCode: 1, Output: "Error: no checks matched pattern\n", Refusal: "no checks matched pattern"}, nil
		}
		return passes("dag://all"), nil
	})
	cloud := fake(always(passes(), nil))
	selection := api.CheckSelection{Mode: SelectionSelected, Links: []string{linkA, linkV}, Candidates: 5, Judgment: "j"}
	listed := 9
	selection.Listed = &listed
	result, ran, attribution, err := RunSelectionWithFallback(t.Context(), local, cloud, testBudget, Remembered{}, "dir", Identity{}, selection)
	must(t, err)
	if len(cloud.calls) != 0 {
		t.Fatalf("cloud calls %+v", cloud.calls)
	}
	want := FullSelection(SelectionRefused, "no checks matched pattern")
	want.Candidates, want.Listed, want.Judgment = 5, &listed, "j"
	if !reflect.DeepEqual(ran, want) {
		t.Fatalf("selection %+v", ran)
	}
	last := local.calls[len(local.calls)-1]
	if last.links != nil || !local.calls[0].deadline.Equal(last.deadline) {
		t.Fatalf("every-check call %+v, first %+v", last, local.calls[0])
	}
	if !strings.Contains(result.Output, "Dagger refused the selected links, so every check runs.") || !strings.Contains(result.Output, "✔ dag://all") {
		t.Fatalf("output %s", result.Output)
	}
	if !slices.Equal(result.Command, Command(nil)) && result.Command != nil {
		t.Fatalf("command %q", result.Command)
	}
	if attribution.UsedCloud() || !attribution.Runs[len(attribution.Runs)-1].EveryCheck {
		t.Fatalf("attribution %+v", attribution.Runs)
	}
}

func TestRefusedLinksEveryCheckStillEscalatesWhenItDoesNotFinish(t *testing.T) {
	t.Parallel()
	local := fake(func(ctx context.Context, call fakeCall) (CheckResult, error) {
		if len(call.links) > 0 {
			return CheckResult{ExitCode: 1, Output: "Error: no checks matched pattern\n", Refusal: "no checks matched pattern"}, nil
		}
		<-ctx.Done()
		return unfinished("dag://x"), ctx.Err()
	})
	cloud := fake(always(passes("dag://y"), nil))
	_, ran, attribution, err := runFallback(t, local, cloud, Remembered{}, linkA)
	must(t, err)
	if ran.Reason != SelectionRefused || len(cloud.calls) != 1 || cloud.calls[0].links != nil || !slices.Equal(cloud.calls[0].skip, []string{"dag://x"}) {
		t.Fatalf("selection %+v cloud %+v", ran, cloud.calls)
	}
	if want := (api.CheckEngineRun{Engine: api.CheckEngineCloud, EveryCheck: true, Reason: api.CloudAfterTimeout, PassedLocally: 1}); !reflect.DeepEqual(attribution.Runs, []api.CheckEngineRun{want}) {
		t.Fatalf("attribution %+v", attribution.Runs)
	}
}

func TestCloudThatDoesNotFinishIsIncomplete(t *testing.T) {
	t.Parallel()
	for name, c := range map[string]struct {
		cloud func(context.Context, fakeCall) (CheckResult, error)
	}{
		"budget ran out":  {blocked()},
		"no failed check": {always(CheckResult{ExitCode: 3, Output: "Error: engine closed session\n"}, nil)},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			cloud := fake(c.cloud)
			result, _, attribution, err := runFallback(t, fake(blocked()), cloud, Remembered{}, linkA)
			status, _, reason := ClassifyChecks(result, err)
			if status != ChecksIncomplete || reason == nil || !strings.Contains(reason.Error(), "cloud attempt") {
				t.Fatalf("%s %v", status, reason)
			}
			if len(cloud.calls) != 1 || !attribution.UsedCloud() {
				t.Fatalf("cloud calls %+v, attribution %+v", cloud.calls, attribution.Runs)
			}
		})
	}
}

func TestCloudReportsFailureWithoutRetry(t *testing.T) {
	t.Parallel()
	local := fake(blocked())
	cloud := fake(always(failure("dag://a"), nil))
	result, _, _, err := runFallback(t, local, cloud, Remembered{}, linkA)
	if status, failed, _ := ClassifyChecks(result, err); status != ChecksFailed || !slices.Equal(failed, []string{"dag://a"}) {
		t.Fatalf("%s %q %v", status, failed, err)
	}
}

func TestRememberedLinksGoStraightToCloud(t *testing.T) {
	t.Parallel()
	local := fake(func(_ context.Context, call fakeCall) (CheckResult, error) { return passes(call.links...), nil })
	cloud := fake(func(_ context.Context, call fakeCall) (CheckResult, error) { return passes(call.links...), nil })
	_, _, attribution, err := runFallback(t, local, cloud, Remembered{Links: []string{linkA}}, linkA, linkV)
	must(t, err)
	if len(local.calls) != 1 || !slices.Equal(local.calls[0].links, []string{linkV}) || len(cloud.calls) != 1 || !slices.Equal(cloud.calls[0].links, []string{linkA}) {
		t.Fatalf("local %+v cloud %+v", local.calls, cloud.calls)
	}
	want := []api.CheckEngineRun{
		{Engine: api.CheckEngineLocal, Links: []string{linkV}},
		{Engine: api.CheckEngineCloud, Links: []string{linkA}, Reason: api.CloudByMemory},
	}
	if !reflect.DeepEqual(attribution.Runs, want) {
		t.Fatalf("attribution %+v", attribution.Runs)
	}
	// Only remembered links: the local runner is never called.
	local = fake(always(CheckResult{}, errors.New("local runner called")))
	_, _, _, err = runFallback(t, local, cloud, Remembered{Links: []string{linkA, linkV}}, linkA, linkV)
	must(t, err)
	if len(local.calls) != 0 {
		t.Fatalf("local calls %+v", local.calls)
	}
}

func TestEscalatedEveryCheckRunIsRemembered(t *testing.T) {
	t.Parallel()
	local := fake(blocked())
	cloud := fake(always(passes("dag://a"), nil))
	_, _, first, err := runFallback(t, local, cloud, Remembered{})
	must(t, err)
	remembered := RememberedFrom([]*api.CheckAttribution{nil, {}, first})
	if !remembered.EveryCheck || len(remembered.Links) != 0 {
		t.Fatalf("remembered %+v", remembered)
	}
	local = fake(always(CheckResult{}, errors.New("local runner called")))
	_, _, second, err := runFallback(t, local, cloud, remembered)
	must(t, err)
	if len(local.calls) != 0 || len(cloud.calls) != 2 || second.Runs[0].Reason != api.CloudByMemory || !second.Runs[0].EveryCheck {
		t.Fatalf("local %+v cloud %+v attribution %+v", local.calls, cloud.calls, second.Runs)
	}
	if !RememberedFrom([]*api.CheckAttribution{second}).EveryCheck {
		t.Fatal("a run routed by memory forgets the memory")
	}
}

func TestRememberedFromCollectsEscalatedAndRoutedLinks(t *testing.T) {
	t.Parallel()
	got := RememberedFrom([]*api.CheckAttribution{
		{Runs: []api.CheckEngineRun{{Engine: api.CheckEngineLocal, Links: []string{linkV}}, {Engine: api.CheckEngineCloud, Links: []string{linkA}, Reason: api.CloudAfterTimeout}}},
		nil,
		{Runs: []api.CheckEngineRun{{Engine: api.CheckEngineCloud, Links: []string{linkA, linkB}, Reason: api.CloudByMemory}}},
	})
	if !slices.Equal(got.Links, []string{linkA, linkB}) || got.EveryCheck || got.Has(linkV) || !got.Has(linkB) {
		t.Fatalf("remembered %+v", got)
	}
	if empty := RememberedFrom(nil); empty.EveryCheck || len(empty.Links) != 0 {
		t.Fatalf("remembered %+v", empty)
	}
}

func TestAttributionJSONIsOptional(t *testing.T) {
	t.Parallel()
	for _, raw := range []string{`{}`, `{"runs":[]}`, `null`} {
		var a *api.CheckAttribution
		must(t, json.Unmarshal([]byte(raw), &a))
		if a.UsedCloud() || len(a.CloudRuns()) != 0 || len(RememberedFrom([]*api.CheckAttribution{a}).Links) != 0 {
			t.Fatalf("%s: %+v", raw, a)
		}
	}
	var record struct {
		Engines *api.CheckAttribution `json:"engines,omitempty"`
	}
	must(t, json.Unmarshal([]byte(`{}`), &record))
	if record.Engines.UsedCloud() {
		t.Fatalf("absent attribution used cloud")
	}
	encoded, err := json.Marshal(api.CheckAttribution{})
	must(t, err)
	if string(encoded) != `{}` {
		t.Fatalf("encoded %s", encoded)
	}
	in := api.CheckAttribution{Runs: []api.CheckEngineRun{
		{Engine: api.CheckEngineLocal, Links: []string{linkV}},
		{Engine: api.CheckEngineCloud, EveryCheck: true, Reason: api.CloudAfterEngineFailure, PassedLocally: 2},
	}}
	encoded, err = json.Marshal(in)
	must(t, err)
	var out api.CheckAttribution
	must(t, json.Unmarshal(encoded, &out))
	if !reflect.DeepEqual(in, out) || strings.Contains(string(encoded), `"reason":""`) || strings.Contains(string(encoded), "passed_locally\":0") {
		t.Fatalf("%s -> %+v", encoded, out)
	}
	var m Remembered
	encoded, err = json.Marshal(m)
	must(t, err)
	if string(encoded) != `{}` {
		t.Fatalf("encoded %s", encoded)
	}
	must(t, json.Unmarshal([]byte(`{"links":["x"],"every_check":true}`), &m))
	if !m.Has("x") || !m.EveryCheck {
		t.Fatalf("%+v", m)
	}
}

func TestDescribeAttributionNamesWhatRanOnCloud(t *testing.T) {
	t.Parallel()
	if got := DescribeAttribution(nil); got != "" {
		t.Fatalf("nil: %q", got)
	}
	if got := DescribeAttribution(&api.CheckAttribution{Runs: []api.CheckEngineRun{{Engine: api.CheckEngineLocal, Links: []string{linkA}}}}); got != "" {
		t.Fatalf("local: %q", got)
	}
	got := DescribeAttribution(&api.CheckAttribution{Runs: []api.CheckEngineRun{
		{Engine: api.CheckEngineLocal, Links: []string{linkV}},
		{Engine: api.CheckEngineCloud, Links: []string{linkA}, Reason: api.CloudAfterTimeout, PassedLocally: 3},
		{Engine: api.CheckEngineCloud, EveryCheck: true, Reason: api.CloudByMemory},
	}})
	for _, want := range []string{linkA, "every check", "time budget", "3 checks that had passed locally", "earlier run", "ran locally"} {
		if !strings.Contains(got, want) {
			t.Fatalf("%q lacks %q", got, want)
		}
	}
}

func TestPassedChecksReadTheReport(t *testing.T) {
	t.Parallel()
	output := "CHECKS  ✘ 1 failed\n✔ dag://a 1s OK\n✘ dag://b 1s ERROR\n  boom\n✔ dag://c 1s OK\n✔ dag://a 1s OK\nCHECKS  ✔ 1 passed\n✔ dag://d 1s OK\n"
	if got := PassedChecks(output); !slices.Equal(got, []string{"dag://a", "dag://c", "dag://d"}) {
		t.Fatalf("passed %q", got)
	}
	if got := PassedChecks("CHECKS  ✔ 1 passed\n✔ dag://a 1s OK\n✘ dag://a 1s ERROR\n"); len(got) != 0 {
		t.Fatalf("a check that fails again has not passed: %q", got)
	}
	if got := PassedChecks("Error: boom\n"); len(got) != 0 {
		t.Fatalf("passed %q", got)
	}
}

// serial: installs a fake dagger executable through PATH.
func TestDaggerChecksSkipPassedChecks(t *testing.T) {
	const flags = "check --progress=report --fail-fast --parallel 4 "
	t.Run("filtered links", func(t *testing.T) {
		log := installLoggingDagger(t, "printf 'CHECKS  ✔ 1 passed\\n'\n")
		result, err := (DaggerChecks{}).CheckSkipping(t.Context(), t.TempDir(), Identity{}, []string{linkA}, []string{"dag://a1", "dag://a2"})
		must(t, err)
		if got, want := checkInvocations(t, log), []string{flags + "--skip dag://a1 --skip dag://a2 " + linkA}; !slices.Equal(got, want) {
			t.Fatalf("invocations %q, want %q", got, want)
		}
		if len(result.Flaky) != 0 || result.ExitCode != 0 {
			t.Fatalf("result %+v", result)
		}
	})
	t.Run("every check", func(t *testing.T) {
		log := installLoggingDagger(t, "printf 'CHECKS  ✔ 1 passed\\n'\n")
		_, err := (DaggerChecks{}).CheckSkipping(t.Context(), t.TempDir(), Identity{}, nil, []string{"dag://a1"})
		must(t, err)
		if got, want := checkInvocations(t, log), []string{flags + "--skip dag://a1"}; !slices.Equal(got, want) {
			t.Fatalf("invocations %q, want %q", got, want)
		}
	})
	t.Run("nothing left to run", func(t *testing.T) {
		installLoggingDagger(t, "printf 'Error: no checks selected\\n'; exit 1\n")
		result, err := (DaggerChecks{}).CheckSkipping(t.Context(), t.TempDir(), Identity{}, nil, []string{"dag://a1"})
		must(t, err)
		if status, _, _ := ClassifyChecks(result, nil); status != ChecksPassed || result.Refusal != "" {
			t.Fatalf("status %s, refusal %q", status, result.Refusal)
		}
	})
}

func TestDescribeSelectionRunNamesWhatRanOnCloud(t *testing.T) {
	t.Parallel()
	selection := api.CheckSelection{Mode: SelectionSelected, Links: []string{linkA, linkB}, Candidates: 3, Judgment: "judgment_1"}
	local := &api.CheckAttribution{Runs: []api.CheckEngineRun{{Engine: api.CheckEngineLocal, EveryCheck: true}}}
	for _, a := range []*api.CheckAttribution{nil, {}, local} {
		if got := DescribeSelectionRun(selection, a); got != DescribeSelection(selection) {
			t.Fatalf("a run without cloud was described as %q", got)
		}
	}
	cloud := &api.CheckAttribution{Runs: []api.CheckEngineRun{
		{Engine: api.CheckEngineLocal, Links: []string{linkA}},
		{Engine: api.CheckEngineCloud, Links: []string{linkB}, Reason: api.CloudByMemory},
	}}
	got := DescribeSelectionRun(selection, cloud)
	if !strings.HasPrefix(got, DescribeSelection(selection)+"; the Dagger Cloud engine ran ") || !strings.Contains(got, linkB) || !strings.Contains(got, "routed there because an earlier run in this workstream needed cloud") || !strings.Contains(got, "the other checks ran locally") || strings.HasSuffix(got, ".") {
		t.Fatalf("description %q", got)
	}
}
