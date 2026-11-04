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
 *go-test=T) printf 'Full trace at https://dagger.cloud/o/traces/failed\nCHECKS  ✘ 1 failed\n✘ dag://failed 1s ERROR\n    wrong answer\n'; exit 3 ;;
esac
printf 'CHECKS  ✔ 2 passed\nFull trace at https://dagger.cloud/o/traces/passed\n'
`)
		result, err := (DaggerChecks{}).Check(context.Background(), t.TempDir(), Identity{}, links)
		must(t, err)
		if got, want := checkInvocations(t, log), []string{flags + packages, flags + item}; !slices.Equal(got, want) {
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

// serial: sets PATH and GITHUB_TOKEN via t.Setenv
func TestDaggerChecksListExpandedLinks(t *testing.T) {
	bin := t.TempDir()
	script := `#!/bin/sh
[ "$*" = "list checks --all --format=link" ] || exit 91
[ -z "$GITHUB_TOKEN" ] || exit 92
echo '[dagger x-release] running dagger'
echo 'dag+check://go/packages/tests/test?go-package=a&go-test=TestA'
echo 'dag+check://release/version'
`
	must(t, os.WriteFile(filepath.Join(bin, "dagger"), []byte(script), 0700))
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("GITHUB_TOKEN", "delivery-secret")
	links, err := (DaggerChecks{}).List(context.Background(), t.TempDir(), Identity{})
	must(t, err)
	if !slices.Equal(links, []string{"dag+check://go/packages/tests/test?go-package=a&go-test=TestA", "dag+check://release/version"}) {
		t.Fatalf("links %q", links)
	}
	must(t, os.WriteFile(filepath.Join(bin, "dagger"), []byte("#!/bin/sh\necho 'Error: no workspace'\nexit 1\n"), 0700))
	if _, err := (DaggerChecks{}).List(context.Background(), t.TempDir(), Identity{}); err == nil || !strings.Contains(err.Error(), "no workspace") {
		t.Fatalf("a failed listing returned %v", err)
	}
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
