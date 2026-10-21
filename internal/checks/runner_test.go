package checks

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// serial: sets PATH, GITHUB_TOKEN, ANTHROPIC_API_KEY, SSH_AUTH_SOCK and EXPECTED_HOST_HOME via t.Setenv
func TestDaggerChecksCaptureOutputWithholdCredentialsAndDisableColor(t *testing.T) {
	bin := t.TempDir()
	script := `#!/bin/sh
[ "$#" = 5 ] && [ "$1" = check ] && [ "$2" = --progress=report ] && [ "$3" = --failfast ] && [ "$4" = 'dag+check://go/packages/tests/test?go-package=a&go-test=TestA' ] && [ "$5" = dag+check://release/version ] || exit 91
[ -z "$GITHUB_TOKEN$ANTHROPIC_API_KEY$SSH_AUTH_SOCK" ] || exit 92
[ -d "$HOME" ] && [ "$HOME" = "$TMPDIR" ] || exit 93
[ -z "$EXPECTED_HOST_HOME" ] || exit 95
[ -f candidate.txt ] || exit 94
[ "$(git rev-parse --show-toplevel)" = "$(pwd -P)" ] || exit 96
[ "$NO_COLOR" = 1 ] || exit 97
head -c 70000 /dev/zero | tr '\000' x
printf '\n== CHECKS ==  ✘ 1 failed\n✘ dag://release/version 1.0s ERROR\n    version mismatch\n✔ dag://passing 1s OK\n'
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
	result, err := (DaggerChecks{}).Check(context.Background(), dir, []string{"dag+check://go/packages/tests/test?go-package=a&go-test=TestA", "dag+check://release/version"})
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
	links, err := (DaggerChecks{}).List(context.Background(), t.TempDir())
	must(t, err)
	if !slices.Equal(links, []string{"dag+check://go/packages/tests/test?go-package=a&go-test=TestA", "dag+check://release/version"}) {
		t.Fatalf("links %q", links)
	}
	must(t, os.WriteFile(filepath.Join(bin, "dagger"), []byte("#!/bin/sh\necho 'Error: no workspace'\nexit 1\n"), 0700))
	if _, err := (DaggerChecks{}).List(context.Background(), t.TempDir()); err == nil || !strings.Contains(err.Error(), "no workspace") {
		t.Fatalf("a failed listing returned %v", err)
	}
}

func TestFailedChecksReadTheReport(t *testing.T) {
	t.Parallel()
	report := `== TRACE ==  ✘ FAILED
! dag://go/packages/tests/test?go-package=p&go-test=TestAdd: Go tests in p: exit code: 1

== CHECKS ==  ✘ 1 failed  ✔ 1 passed
✘ dag://go/packages/tests/test?go-package=p&go-test=TestAdd&go-test=TestOK 4.8s ERROR
  == TESTS ==
    ✘ example.com/failrepo/p › TestAdd FAIL
✔ dag://go/packages/generate/stale?go-package=p 1.8s OK

== RUN LOCALLY ==
dagger check "dag://go/packages/tests/test?go-package=p&go-test=TestAdd&go-test=TestOK"
`
	if got := FailedChecks(report); !slices.Equal(got, []string{"dag://go/packages/tests/test?go-package=p&go-test=TestAdd&go-test=TestOK"}) {
		t.Fatalf("failed checks %q", got)
	}
	if got := FailedChecks("Error: no checks matched pattern\n"); got != nil {
		t.Fatalf("an unreported run has failed checks %q", got)
	}
}
