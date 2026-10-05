package service

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/kpenfound/osmia/internal/coreadapter"
)

// checkFunc runs every check with its function, and lists no links.
type checkFunc func(context.Context, string) (CheckResult, error)

func (f checkFunc) List(context.Context, string) ([]string, error) { return nil, nil }
func (f checkFunc) Check(ctx context.Context, dir string, _ []string) (CheckResult, error) {
	return f(ctx, dir)
}

// passingChecks is the check runner of service fixtures: every check passes.
var passingChecks = checkFunc(func(context.Context, string) (CheckResult, error) {
	return CheckResult{Output: "== CHECKS ==  ✔ 1 passed\n✔ dag://go/packages/tests/test 1.0s OK\n"}, nil
})

func TestUnitReviewReadsThePinnedCandidate(t *testing.T) {
	f, stream, repo := newReviewFixture(t, "checks")
	ctx := context.Background()
	r := &reviewers{masons: newMasonController(f.s, repo)}
	_, identity, err := r.unitReviewEvidence(ctx, stream, "resume")
	must(t, err)

	must(t, r.Pass(ctx))
	th, err := repo.Thread(stream, reviewerAgent("resume"))
	must(t, err)
	scope := coreadapter.Scope{Project: string(repo.Project()), Workstream: string(stream), Unit: "resume", Thread: th.Identity.ThreadID, Turn: th.Turns[0].Request.TurnID, Role: reviewerRole}
	pinned, err := unitReviewerIdentity(repo, scope)
	must(t, err)
	if pinned != identity {
		t.Fatalf("review turn identity %+v, want %+v", pinned, identity)
	}
	// Moving the branch and dirtying its workspace must not change the review input.
	w, _, err := newUnitWorkspaces(f.s.cfg, repo).open(ctx, stream, "resume")
	must(t, err)
	must(t, os.WriteFile(filepath.Join(w.Path, masonWrote), []byte("unreviewed\n"), 0600))
	demoGit(t, f.clone, "-C", f.clone, "update-ref", "refs/heads/"+unitBranch(stream, "resume"), identity.Candidate.BaseRevision)

	selection, err := unitReviewerSelection(ctx, f.s.about(repo), repo, scope, coreadapter.ExecutionSettings{})
	must(t, err)
	selected, err := os.ReadFile(filepath.Join(selection.Workspace.SourceDirectory, masonWrote))
	must(t, err)
	if string(selected) != "package trace\n" || selection.Workspace.BaseRevision != identity.Candidate.Revision {
		t.Fatalf("review input is not pinned: %s", selected)
	}
}

func TestDaggerChecksBoundOutputWithholdCredentialsAndDisableColor(t *testing.T) {
	bin := t.TempDir()
	script := `#!/bin/sh
[ "$#" = 4 ] && [ "$1" = check ] && [ "$2" = --progress=report ] && [ "$3" = 'dag+check://go/packages/tests/test?go-package=a&go-test=TestA' ] && [ "$4" = dag+check://release/version ] || exit 91
[ -z "$GITHUB_TOKEN$ANTHROPIC_API_KEY$SSH_AUTH_SOCK" ] || exit 92
[ -d "$HOME" ] && [ "$HOME" = "$TMPDIR" ] || exit 93
[ -z "$EXPECTED_HOST_HOME" ] || exit 95
[ -f candidate.txt ] || exit 94
[ "$(git rev-parse --show-toplevel)" = "$(pwd -P)" ] || exit 96
[ "$NO_COLOR" = 1 ] || exit 97
head -c 70000 /dev/zero | tr '\000' x
printf '\n== CHECKS ==  ✘ 1 failed\n✘ dag://release/version 1.0s ERROR\n'
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
	if result.ExitCode != 3 || !result.Truncated || len(result.Output) != 64*1024 || !strings.HasSuffix(result.Output, "✘ dag://release/version 1.0s ERROR\n") {
		t.Fatalf("result: exit=%d truncated=%t bytes=%d", result.ExitCode, result.Truncated, len(result.Output))
	}
	if got := failedChecks(result.Output); !slices.Equal(got, []string{"dag://release/version"}) {
		t.Fatalf("failed checks %q", got)
	}
}

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
	if got := failedChecks(report); !slices.Equal(got, []string{"dag://go/packages/tests/test?go-package=p&go-test=TestAdd&go-test=TestOK"}) {
		t.Fatalf("failed checks %q", got)
	}
	if got := failedChecks("Error: no checks matched pattern\n"); got != nil {
		t.Fatalf("an unreported run has failed checks %q", got)
	}
}
