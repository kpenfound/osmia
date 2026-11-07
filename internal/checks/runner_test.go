package checks

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
)

func TestCheckCommandRecordsEachRunAndLimitsParallelism(t *testing.T) {
	t.Parallel()
	check := []string{"dagger", "check", "--progress=report", "--fail-fast", "--parallel", "4"}
	if got := Command(nil); !slices.Equal(got, check) {
		t.Fatalf("every check: %q", got)
	}
	got := Command([]string{"dag://?check=test&go-package=a", "dag://?check=test&go-package=b"})
	if want := append(slices.Clone(check), "dag://?check=test&go-package=a&go-package=b"); !slices.Equal(got, want) {
		t.Fatalf("one view: %q, want %q", got, want)
	}
	got = Command([]string{"dag://?check=test&go-package=a", "dag+check://release/version"})
	want := slices.Concat(check, []string{"dag://?check=test&go-package=a", "&&"}, check, []string{"dag+check://release/version"})
	if !slices.Equal(got, want) {
		t.Fatalf("two views: %q, want %q", got, want)
	}
}

// installLoggingDagger puts a fake dagger on PATH that appends each invocation
// to the returned log, which body can read as $LOG, and then runs body.
func installLoggingDagger(t *testing.T, body string) (log string) {
	t.Helper()
	bin := t.TempDir()
	log = filepath.Join(t.TempDir(), "invocations")
	script := "#!/bin/sh\nLOG='" + log + "'\nprintf '%s\\n' \"$*\" >> \"$LOG\"\n" + body
	must(t, os.WriteFile(filepath.Join(bin, "dagger"), []byte(script), 0700))
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	return log
}

func checkInvocations(t *testing.T, log string) []string {
	t.Helper()
	raw, err := os.ReadFile(log)
	must(t, err)
	return strings.Split(strings.TrimSpace(string(raw)), "\n")
}

// serial: installs a fake dagger executable through PATH.
func TestDaggerChecksReportARefusalFromTheRunThatDecided(t *testing.T) {
	links := []string{"dag+check://?check=test&go-package=a", "dag+check://?check=test&go-package=b&go-test=T"}
	t.Run("a later run refused after an earlier one passed", func(t *testing.T) {
		installLoggingDagger(t, `case "$*" in
 *go-test=T) printf 'Error: no checks matched pattern\n'; exit 1 ;;
esac
printf 'CHECKS  ✔ 2 passed\n'
`)
		result, err := (DaggerChecks{}).Check(context.Background(), t.TempDir(), Identity{}, links)
		must(t, err)
		if result.ExitCode != 1 || result.Refusal != "no checks matched pattern" {
			t.Fatalf("result %+v", result)
		}
	})
	t.Run("every run passing", func(t *testing.T) {
		installLoggingDagger(t, "printf 'CHECKS  ✔ 2 passed\\n'\n")
		result, err := (DaggerChecks{}).Check(context.Background(), t.TempDir(), Identity{}, links)
		must(t, err)
		if result.ExitCode != 0 || result.Refusal != "" {
			t.Fatalf("result %+v", result)
		}
	})
}

// serial: installs a fake dagger executable through PATH.
func TestDaggerChecksRunEachFilteredViewSeparately(t *testing.T) {
	const (
		packages = "dag+check://?check=test&go-package=a&go-package=b"
		item     = "dag+check://?check=test&go-package=c&go-test=T"
		version  = "dag+check://release/version"
		flags    = "check --progress=report --fail-fast --parallel 4 "
	)
	links := []string{"dag+check://?check=test&go-package=a", item, "dag+check://?check=test&go-package=b", version}
	t.Run("all pass", func(t *testing.T) {
		log := installLoggingDagger(t, `printf 'CHECKS  ✔ 2 passed\nFull trace at https://dagger.cloud/o/traces/run%s\n' "$(wc -l < "$LOG" | tr -d ' ')"
`)
		result, err := (DaggerChecks{}).Check(context.Background(), t.TempDir(), Identity{}, links)
		must(t, err)
		if got, want := checkInvocations(t, log), []string{flags + packages, flags + item, flags + version}; !slices.Equal(got, want) {
			t.Fatalf("invocations %q, want %q", got, want)
		}
		status, failed, reason := ClassifyChecks(result, nil)
		if status != ChecksPassed || len(failed) != 0 || reason != nil || result.ExitCode != 0 {
			t.Fatalf("classified %s, %q, %v; exit %d", status, failed, reason, result.ExitCode)
		}
		if result.TraceURL != "https://dagger.cloud/o/traces/run3" {
			t.Fatalf("trace %q", result.TraceURL)
		}
		for _, want := range []string{"$ dagger " + flags + packages + "\n", "$ dagger " + flags + version + "\n"} {
			if !strings.Contains(result.Output, want) {
				t.Fatalf("output lacks %q:\n%s", want, result.Output)
			}
		}
		if summary, _ := SummarizeCheckOutput(result.Output, 0); strings.Count(summary, "CHECKS") != 3 {
			t.Fatalf("summary keeps each run's result: %q", summary)
		}
	})
	t.Run("a failed check stops later runs", func(t *testing.T) {
		log := installLoggingDagger(t, `case "$*" in
 *go-test=T|*dag://failed) printf 'Full trace at https://dagger.cloud/o/traces/failed\nCHECKS  ✘ 1 failed\n✘ dag://failed 1s ERROR\n    wrong answer\n'; exit 3 ;;
esac
printf 'CHECKS  ✔ 2 passed\nFull trace at https://dagger.cloud/o/traces/passed\n'
`)
		result, err := (DaggerChecks{}).Check(context.Background(), t.TempDir(), Identity{}, links)
		must(t, err)
		if got, want := checkInvocations(t, log), []string{flags + packages, flags + item, flags + "dag://failed"}; !slices.Equal(got, want) {
			t.Fatalf("invocations %q, want %q", got, want)
		}
		status, failed, reason := ClassifyChecks(result, nil)
		if status != ChecksFailed || !slices.Equal(failed, []string{"dag://failed"}) || reason != nil || result.ExitCode != 3 {
			t.Fatalf("classified %s, %q, %v; exit %d", status, failed, reason, result.ExitCode)
		}
		if result.TraceURL != "https://dagger.cloud/o/traces/failed" {
			t.Fatalf("trace %q", result.TraceURL)
		}
		if summary, _ := SummarizeCheckOutput(result.Output, 3); !strings.Contains(summary, "wrong answer") || !strings.Contains(summary, "✔ 2 passed") {
			t.Fatalf("summary: %q", summary)
		}
	})
	t.Run("a run with no result leaves the whole run incomplete", func(t *testing.T) {
		log := installLoggingDagger(t, `case "$*" in
 *go-test=T) printf 'Error: engine unavailable\n'; exit 1 ;;
esac
printf 'CHECKS  ✔ 2 passed\n'
`)
		result, err := (DaggerChecks{}).Check(context.Background(), t.TempDir(), Identity{}, links)
		must(t, err)
		if got := checkInvocations(t, log); len(got) != 2 {
			t.Fatalf("invocations %q", got)
		}
		status, _, reason := ClassifyChecks(result, nil)
		if status != ChecksIncomplete || reason == nil || !strings.Contains(result.Output, "engine unavailable") {
			t.Fatalf("classified %s, %v:\n%s", status, reason, result.Output)
		}
	})
	t.Run("the timeout covers every run", func(t *testing.T) {
		log := installLoggingDagger(t, "exec sleep 30\n")
		ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
		defer cancel()
		_, err := (DaggerChecks{}).Check(ctx, t.TempDir(), Identity{}, links)
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("err %v", err)
		}
		if raw, readErr := os.ReadFile(log); readErr == nil && strings.Count(string(raw), "\n") > 1 {
			t.Fatalf("runs after the timeout: %s", raw)
		}
	})
}

// serial: installs a fake dagger executable through PATH.
func TestDaggerChecksRerunFailedChecksOnce(t *testing.T) {
	const (
		flags = "check --progress=report --fail-fast --parallel 4 "
		view  = "dag+check://?check=test&go-package=p"
		a     = "dag://?check=test&go-package=p&go-test=A"
		b     = "dag://?check=test&go-package=p&go-test=B"
		other = "dag://?check=test&go-package=q&go-test=C"
		skipA = flags + "--skip " + a + " " + view
	)
	// fails prints a report of the links failing and exits 1.
	const fails = `fails() { printf 'CHECKS  ✘ %s failed\n' "$#"; for l in "$@"; do printf '✘ %s 1s ERROR\n    %s went wrong\n' "$l" "$l"; done; printf 'RUN LOCALLY\n'; for l in "$@"; do printf 'dagger check "%s"\n' "$l"; done; exit 1; }
passes() { printf 'CHECKS  ✔ %s passed\n' "$#"; for l in "$@"; do printf '✔ %s 1s OK\n' "$l"; done; exit 0; }
`
	// script answers each exact command with a shell statement. Any other
	// command means the runner asked for something unexpected.
	script := func(commands [][2]string) string {
		body := fails + "case \"$*\" in\n"
		for _, c := range commands {
			body += "'" + c[0] + "') " + c[1] + " ;;\n"
		}
		return body + "*) printf 'unexpected %s\\n' \"$*\"; exit 99 ;;\nesac\n"
	}
	run := func(t *testing.T, commands [][2]string) (CheckResult, []string) {
		t.Helper()
		log := installLoggingDagger(t, script(commands))
		result, err := (DaggerChecks{}).Check(context.Background(), t.TempDir(), Identity{}, []string{view})
		must(t, err)
		return result, checkInvocations(t, log)
	}
	expect := func(t *testing.T, result CheckResult, invocations, ran []string, status string, failed, flaky []string) {
		t.Helper()
		if !slices.Equal(invocations, ran) {
			t.Fatalf("invocations %q, want %q", invocations, ran)
		}
		got, gotFailed, reason := ClassifyChecks(result, nil)
		if got != status || !slices.Equal(gotFailed, failed) || !slices.Equal(result.Flaky, flaky) {
			t.Fatalf("classified %s, failed %q, flaky %q, reason %v:\n%s", got, gotFailed, result.Flaky, reason, result.Output)
		}
	}

	t.Run("a check that passes its rerun is a flake and its view still finishes", func(t *testing.T) {
		result, invocations := run(t, [][2]string{
			{flags + view, "fails '" + a + "'"},
			{flags + a, "passes '" + a + "'"},
			{skipA, "passes '" + b + "'"},
		})
		expect(t, result, invocations, []string{flags + view, flags + a, skipA}, ChecksPassed, nil, []string{a})
		if result.ExitCode != 0 {
			t.Fatalf("exit %d", result.ExitCode)
		}
		summary, _ := SummarizeCheckOutput(result.Output, result.ExitCode)
		if !strings.Contains(summary, a+" 1s ERROR [passed on rerun]") || !strings.Contains(summary, a+" went wrong") {
			t.Fatalf("summary keeps the flake's evidence: %q", summary)
		}
		for _, want := range []string{"\n$ dagger " + flags + a + "\n", "\n$ dagger " + skipA + "\n"} {
			if !strings.Contains(result.Output, want) {
				t.Fatalf("output lacks %q:\n%s", want, result.Output)
			}
		}
	})
	t.Run("a check that fails its rerun fails, and its view does not finish", func(t *testing.T) {
		result, invocations := run(t, [][2]string{
			{flags + view, "fails '" + a + "'"},
			{flags + a, "fails '" + a + "'"},
		})
		expect(t, result, invocations, []string{flags + view, flags + a}, ChecksFailed, []string{a}, nil)
		if summary, _ := SummarizeCheckOutput(result.Output, result.ExitCode); strings.Count(summary, a+" went wrong") != 1 {
			t.Fatalf("summary repeats the failure: %q", summary)
		}
	})
	t.Run("checks that differ in their rerun are told apart", func(t *testing.T) {
		result, invocations := run(t, [][2]string{
			{flags + view, "fails '" + a + "' '" + other + "'"},
			{flags + a, "passes '" + a + "'"},
			{flags + other, "fails '" + other + "'"},
		})
		expect(t, result, invocations, []string{flags + view, flags + a, flags + other}, ChecksFailed, []string{other}, []string{a})
	})
	t.Run("a check its rerun cancelled keeps its failure", func(t *testing.T) {
		both := "dag://?check=test&go-package=p&go-test=A&go-test=B"
		result, invocations := run(t, [][2]string{
			{flags + view, "fails '" + a + "' '" + b + "'"},
			{flags + both, "fails '" + b + "'"},
		})
		expect(t, result, invocations, []string{flags + view, flags + both}, ChecksFailed, []string{a, b}, nil)
	})
	t.Run("a check its rerun reports passing is a flake beside one that fails", func(t *testing.T) {
		both := "dag://?check=test&go-package=p&go-test=A&go-test=B"
		result, invocations := run(t, [][2]string{
			{flags + view, "fails '" + a + "' '" + b + "'"},
			{flags + both, "printf 'CHECKS  ✘ 1 failed  ✔ 1 passed\\n✘ " + b + " 1s ERROR\\n✔ " + a + " 1s OK\\n'; exit 1"},
		})
		expect(t, result, invocations, []string{flags + view, flags + both}, ChecksFailed, []string{b}, []string{a})
	})
	t.Run("a check the failed run cancelled gets its own rerun", func(t *testing.T) {
		skipBoth := flags + "--skip " + a + " --skip " + b + " " + view
		result, invocations := run(t, [][2]string{
			{flags + view, "fails '" + a + "'"},
			{flags + a, "passes '" + a + "'"},
			{skipA, "fails '" + b + "'"},
			{flags + b, "passes '" + b + "'"},
			{skipBoth, "passes '" + other + "'"},
		})
		expect(t, result, invocations, []string{flags + view, flags + a, skipA, flags + b, skipBoth}, ChecksPassed, nil, []string{a, b})
	})
	t.Run("a real failure the finishing run finds still fails", func(t *testing.T) {
		result, invocations := run(t, [][2]string{
			{flags + view, "fails '" + a + "'"},
			{flags + a, "passes '" + a + "'"},
			{skipA, "fails '" + b + "'"},
			{flags + b, "fails '" + b + "'"},
		})
		expect(t, result, invocations, []string{flags + view, flags + a, skipA, flags + b}, ChecksFailed, []string{b}, []string{a})
	})
	t.Run("a skipped check reported failing stops instead of looping", func(t *testing.T) {
		result, invocations := run(t, [][2]string{
			{flags + view, "fails '" + a + "'"},
			{flags + a, "passes '" + a + "'"},
			{skipA, "fails '" + a + "'"},
		})
		expect(t, result, invocations, []string{flags + view, flags + a, skipA}, ChecksFailed, []string{a}, []string{a})
	})
	for _, message := range []string{`no checks matched pattern "view"`, "no checks selected; use 'dagger check -l' to list available checks"} {
		t.Run("nothing left to finish passes: "+message, func(t *testing.T) {
			result, invocations := run(t, [][2]string{
				{flags + view, "fails '" + a + "'"},
				{flags + a, "passes '" + a + "'"},
				{skipA, "printf 'Error: " + strings.ReplaceAll(message, "'", "") + "\\n'; exit 1"},
			})
			expect(t, result, invocations, []string{flags + view, flags + a, skipA}, ChecksPassed, nil, []string{a})
			if result.ExitCode != 0 || result.Refusal != "" {
				t.Fatalf("result %+v", result)
			}
		})
	}
	t.Run("a finishing run with no result leaves the run incomplete", func(t *testing.T) {
		result, invocations := run(t, [][2]string{
			{flags + view, "fails '" + a + "'"},
			{flags + a, "passes '" + a + "'"},
			{skipA, "printf 'Error: engine unavailable\\n'; exit 1"},
		})
		expect(t, result, invocations, []string{flags + view, flags + a, skipA}, ChecksIncomplete, nil, []string{a})
	})
	t.Run("a rerun with no result leaves the failure standing", func(t *testing.T) {
		result, invocations := run(t, [][2]string{
			{flags + view, "fails '" + a + "'"},
			{flags + a, "printf 'Error: no checks matched pattern\\n'; exit 2"},
		})
		expect(t, result, invocations, []string{flags + view, flags + a}, ChecksFailed, []string{a}, nil)
		if result.ExitCode != 1 || result.Refusal != "" {
			t.Fatalf("the failed run decides the result: %+v", result)
		}
	})
	t.Run("a run with no report is not rerun", func(t *testing.T) {
		result, invocations := run(t, [][2]string{{flags + view, "printf 'Error: engine unavailable\\n'; exit 1"}})
		expect(t, result, invocations, []string{flags + view}, ChecksIncomplete, nil, nil)
	})
	t.Run("a rerun that outlasts the deadline leaves the failure standing", func(t *testing.T) {
		log := installLoggingDagger(t, script([][2]string{
			{flags + view, "fails '" + a + "'"},
			{flags + a, "exec sleep 30"},
		}))
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		result, err := (DaggerChecks{}).Check(ctx, t.TempDir(), Identity{}, []string{view})
		must(t, err)
		expect(t, result, checkInvocations(t, log), []string{flags + view, flags + a}, ChecksFailed, []string{a}, nil)
	})
	t.Run("cancelling a rerun stops the run", func(t *testing.T) {
		started := filepath.Join(t.TempDir(), "started")
		installLoggingDagger(t, script([][2]string{
			{flags + view, "fails '" + a + "'"},
			{flags + a, ": > '" + started + "'; exec sleep 30"},
		}))
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		go func() {
			for ctx.Err() == nil {
				if _, err := os.Stat(started); err == nil {
					cancel()
					return
				}
				time.Sleep(10 * time.Millisecond)
			}
		}()
		if _, err := (DaggerChecks{}).Check(ctx, t.TempDir(), Identity{}, []string{view}); !errors.Is(err, context.Canceled) {
			t.Fatalf("err %v", err)
		}
	})
	t.Run("a flake does not stop later views", func(t *testing.T) {
		later := "dag+check://release/version"
		log := installLoggingDagger(t, script([][2]string{
			{flags + view, "fails '" + a + "'"},
			{flags + a, "passes '" + a + "'"},
			{skipA, "passes '" + b + "'"},
			{flags + later, "passes '" + later + "'"},
		}))
		result, err := (DaggerChecks{}).Check(context.Background(), t.TempDir(), Identity{}, []string{view, later})
		must(t, err)
		expect(t, result, checkInvocations(t, log), []string{flags + view, flags + a, skipA, flags + later}, ChecksPassed, nil, []string{a})
	})
}

// serial: installs a fake dagger executable through PATH.
func TestDaggerChecksKeepOneOutputShapeForOneView(t *testing.T) {
	log := installLoggingDagger(t, "printf 'CHECKS  ✔ 1 passed\\n'\n")
	result, err := (DaggerChecks{}).Check(context.Background(), t.TempDir(), Identity{}, []string{"dag+check://release/version"})
	must(t, err)
	if result.Output != "CHECKS  ✔ 1 passed\n" || result.ExitCode != 0 {
		t.Fatalf("result %+v", result)
	}
	if got, want := checkInvocations(t, log), []string{"check --progress=report --fail-fast --parallel 4 dag+check://release/version"}; !slices.Equal(got, want) {
		t.Fatalf("invocations %q", got)
	}
}

// serial: installs a fake dagger executable through PATH.
func TestDaggerChecksPrepareCandidateIdentityOnceForEveryView(t *testing.T) {
	bin := t.TempDir()
	clone := t.TempDir()
	git := func(args ...string) string {
		return strings.TrimSpace(fixturekit.DemoGit(t, bin, append([]string{"-C", clone}, args...)...))
	}
	git("init", "--quiet")
	must(t, os.WriteFile(filepath.Join(clone, "candidate.txt"), []byte("candidate\n"), 0600))
	git("add", ".")
	git("-c", "user.name=Author", "-c", "user.email=author@example.invalid", "commit", "--quiet", "-m", "candidate")
	candidate := git("rev-parse", "HEAD")
	dir := t.TempDir()
	must(t, os.WriteFile(filepath.Join(dir, "candidate.txt"), []byte("candidate\n"), 0600))
	log := filepath.Join(t.TempDir(), "heads")
	must(t, os.WriteFile(filepath.Join(bin, "dagger"), []byte("#!/bin/sh\ngit rev-parse HEAD >> \""+log+"\"\nprintf 'CHECKS  ✔ 1 passed\\n'\n"), 0700))
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	result, err := (DaggerChecks{CIIdentityExport: true}).Check(context.Background(), dir, Identity{Candidate: candidate, Branch: "feature", Remote: "https://github.com/upstream/repo", Repository: clone}, []string{"dag+check://release/version", "dag+check://go/test"})
	must(t, err)
	if result.CIIdentityFallback != "" {
		t.Fatalf("fallback: %s", result.CIIdentityFallback)
	}
	if got, want := checkInvocations(t, log), []string{candidate, candidate}; !slices.Equal(got, want) {
		t.Fatalf("each view checks the candidate: %q, want %q", got, want)
	}
}

// serial: sets PATH, GITHUB_TOKEN, ANTHROPIC_API_KEY, SSH_AUTH_SOCK and EXPECTED_HOST_HOME via t.Setenv
func TestDaggerChecksCaptureOutputWithholdCredentialsAndDisableColor(t *testing.T) {
	bin := t.TempDir()
	script := `#!/bin/sh
[ "$#" = 6 ] && [ "$1" = check ] && [ "$2" = --progress=report ] && [ "$3" = --fail-fast ] && [ "$4" = --parallel ] && [ "$5" = 4 ] || exit 91
[ "$6" = 'dag+check://go/packages/tests/test?go-package=a&go-test=TestA' ] && exit 0
[ "$6" = dag+check://release/version ] || exit 98
[ -z "$GITHUB_TOKEN$ANTHROPIC_API_KEY$SSH_AUTH_SOCK" ] || exit 92
[ -d "$HOME" ] && [ "$HOME" = "$TMPDIR" ] || exit 93
[ -z "$EXPECTED_HOST_HOME" ] || exit 95
[ -f candidate.txt ] || exit 94
[ "$(git rev-parse --show-toplevel)" = "$(pwd -P)" ] || exit 96
[ "$NO_COLOR" = 1 ] || exit 97
head -c 70000 /dev/zero | tr '\000' x
printf '\nCHECKS  ✘ 1 failed\n✘ dag://release/version 1.0s ERROR\n    version mismatch\n✔ dag://passing 1s OK\n'
head -c 70000 /dev/zero | tr '\000' y
exit 3
`
	must(t, os.WriteFile(filepath.Join(bin, "dagger"), []byte(script), 0700))
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("GITHUB_TOKEN", "delivery-secret")
	t.Setenv("ANTHROPIC_API_KEY", "provider-secret")
	t.Setenv("SSH_AUTH_SOCK", "/private/signing-agent")
	t.Setenv("EXPECTED_HOST_HOME", os.Getenv("HOME"))
	dir := t.TempDir()
	must(t, os.WriteFile(filepath.Join(dir, "candidate.txt"), []byte("candidate"), 0600))
	result, err := (DaggerChecks{}).Check(context.Background(), dir, Identity{}, []string{"dag+check://go/packages/tests/test?go-package=a&go-test=TestA", "dag+check://release/version"})
	must(t, err)
	if result.ExitCode != 3 || result.Truncated || len(result.Output) < 140000 || !strings.Contains(result.Output, "version mismatch") {
		t.Fatalf("result: exit=%d truncated=%t bytes=%d", result.ExitCode, result.Truncated, len(result.Output))
	}
	if got := FailedChecks(result.Output); !slices.Equal(got, []string{"dag://release/version"}) {
		t.Fatalf("failed checks %q", got)
	}
}

// serial: sets PATH and credentials via t.Setenv.
func TestDaggerChecksListings(t *testing.T) {
	for _, all := range []bool{true, false} {
		name, args := "top-level", "list checks --format=link"
		list := (DaggerChecks{}).ListTopLevel
		if all {
			name, args = "expanded", "list checks --all --format=link"
			list = (DaggerChecks{}).List
		}
		t.Run(name, func(t *testing.T) {
			bin := t.TempDir()
			script := `#!/bin/sh
[ "$*" = "` + args + `" ] || exit 91
[ -z "$GITHUB_TOKEN$ANTHROPIC_API_KEY$SSH_AUTH_SOCK$DAGGER_CLOUD_TOKEN" ] || exit 92
[ -d "$HOME" ] && [ "$HOME" = "$TMPDIR" ] || exit 93
[ "$NO_COLOR" = 1 ] || exit 94
[ -f candidate.txt ] || exit 95
echo '[dagger x-release] running dagger'
echo 'dag+check://go/packages/tests/test?go-package=a&go-test=TestA'
echo 'dag+check://release/version'
`
			must(t, os.WriteFile(filepath.Join(bin, "dagger"), []byte(script), 0700))
			t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
			t.Setenv("GITHUB_TOKEN", "delivery-secret")
			t.Setenv("ANTHROPIC_API_KEY", "provider-secret")
			t.Setenv("SSH_AUTH_SOCK", "/private/signing-agent")
			t.Setenv("DAGGER_CLOUD_TOKEN", "host-cloud-secret")
			dir := t.TempDir()
			must(t, os.WriteFile(filepath.Join(dir, "candidate.txt"), []byte("candidate"), 0600))
			links, err := list(t.Context(), dir, Identity{})
			must(t, err)
			if !slices.Equal(links, []string{"dag+check://go/packages/tests/test?go-package=a&go-test=TestA", "dag+check://release/version"}) {
				t.Fatalf("links %q", links)
			}
			must(t, os.WriteFile(filepath.Join(bin, "dagger"), []byte("#!/bin/sh\necho 'Error: no workspace'\nexit 1\n"), 0700))
			if _, err := list(t.Context(), dir, Identity{}); err == nil || !strings.Contains(err.Error(), "no workspace") {
				t.Fatalf("a failed listing returned %v", err)
			}
		})
	}
}

// serial: installs a fake dagger executable through PATH.
func TestDaggerChecksTopLevelListingFailures(t *testing.T) {
	t.Run("failure leaves the expanded listing usable and redacts diagnostics", func(t *testing.T) {
		installLoggingDagger(t, `case "$*" in
'list checks --format=link') echo 'no workspace listing-private-token'; exit 1;;
'list checks --all --format=link') echo 'dag+check://release/version';;
*) exit 91;;
esac
`)
		d := DaggerChecks{CloudToken: "listing-private-token"}
		links, err := d.ListTopLevel(t.Context(), t.TempDir(), Identity{})
		if err == nil || links != nil || !strings.Contains(err.Error(), "no workspace") || strings.Contains(err.Error(), d.CloudToken) {
			t.Fatalf("links %q, error %v", links, err)
		}
		links, err = d.List(t.Context(), t.TempDir(), Identity{})
		must(t, err)
		if !slices.Equal(links, []string{"dag+check://release/version"}) {
			t.Fatalf("expanded listing %q", links)
		}
	})
	t.Run("oversized listing fails rather than returning partial links", func(t *testing.T) {
		installLoggingDagger(t, "head -c 4194305 /dev/zero | tr '\\000' x\nprintf '\\ndag+check://release/version\\n'\n")
		links, err := (DaggerChecks{}).ListTopLevel(t.Context(), t.TempDir(), Identity{})
		if err == nil || links != nil {
			t.Fatalf("truncated listing returned links %q, error %v", links, err)
		}
	})
	t.Run("caller deadline bounds the listing", func(t *testing.T) {
		installLoggingDagger(t, "exec sleep 30\n")
		ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
		defer cancel()
		links, err := (DaggerChecks{}).ListTopLevel(ctx, t.TempDir(), Identity{})
		if !errors.Is(err, context.DeadlineExceeded) || links != nil {
			t.Fatalf("links %q, error %v", links, err)
		}
	})
}

func TestFailedChecksReadTheReport(t *testing.T) {
	t.Parallel()
	report := `== TRACE ==  ✘ FAILED
! dag://go/packages/tests/test?go-package=p&go-test=TestAdd: Go tests in p: exit code: 1

CHECKS  ✘ 1 failed  ✔ 1 passed
✘ dag://go/packages/tests/test?go-package=p&go-test=TestAdd&go-test=TestOK 4.8s ERROR
  TESTS
    ✘ example.com/failrepo/p › TestAdd FAIL
✔ dag://go/packages/generate/stale?go-package=p 1.8s OK

RUN LOCALLY
dagger check "dag://go/packages/tests/test?go-package=p&go-test=TestAdd&go-test=TestOK"
`
	if got := FailedChecks(report); !slices.Equal(got, []string{"dag://go/packages/tests/test?go-package=p&go-test=TestAdd&go-test=TestOK"}) {
		t.Fatalf("failed checks %q", got)
	}
	if got := FailedChecks("Error: no checks matched pattern\n"); got != nil {
		t.Fatalf("an unreported run has failed checks %q", got)
	}
}
