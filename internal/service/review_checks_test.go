package service

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kpenfound/osmia/internal/coreadapter"
)

type checkFunc func(context.Context, string) (CheckResult, error)

func (f checkFunc) Check(ctx context.Context, dir string) (CheckResult, error) { return f(ctx, dir) }

func TestCandidateChecksUseFreshPinnedExports(t *testing.T) {
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
	// Moving the branch and dirtying its workspace must not change the check input.
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
	var dirs []string
	checker := checkFunc(func(_ context.Context, dir string) (CheckResult, error) {
		dirs = append(dirs, dir)
		data, err := os.ReadFile(filepath.Join(dir, masonWrote))
		if err != nil || string(data) != "package trace\n" {
			t.Fatalf("wrong candidate: %s %v", data, err)
		}
		for _, name := range []string{".git", ".jj"} {
			if _, err := os.Stat(filepath.Join(dir, name)); !os.IsNotExist(err) {
				t.Fatalf("export contains %s", name)
			}
		}
		must(t, os.WriteFile(filepath.Join(dir, masonWrote), []byte("check output"), 0600))
		return CheckResult{ExitCode: 2, Output: "proof failed"}, nil
	})
	tool := candidateCheckTool(f.s.about(repo), repo, coreadapter.Scope{Workstream: string(stream)}, identity.Candidate.Revision, checker)
	if !coreadapter.ToolPermitted(reviewerGrant, tool) || coreadapter.ToolPermitted(coreadapter.Capabilities{}, tool) {
		t.Fatal("check does not require an explicit tool grant")
	}
	if coreadapter.ToolPermitted(reviewerGrant, coreadapter.Tool{Name: runChecksTool, Effect: coreadapter.ToolExecute}) {
		t.Fatal("check grant permits arbitrary execution")
	}
	for range 2 {
		data, err := tool.Handle(ctx, json.RawMessage("{}"))
		must(t, err)
		var got struct {
			Candidate, Check string
			CheckResult
		}
		must(t, json.Unmarshal(data, &got))
		if got.Candidate != identity.Candidate.Revision || got.Check != "dagger check" || got.ExitCode != 2 || got.Output != "proof failed" {
			t.Fatalf("check result %s", data)
		}
	}
	if dirs[0] == dirs[1] {
		t.Fatal("checks shared a workspace")
	}
	for _, dir := range dirs {
		if _, err := os.Stat(dir); !os.IsNotExist(err) {
			t.Fatalf("check workspace leaked: %s", dir)
		}
	}
	data, err := os.ReadFile(filepath.Join(w.Path, masonWrote))
	must(t, err)
	if string(data) != "unreviewed\n" {
		t.Fatal("check modified implementation workspace")
	}
	for _, input := range []string{`{"command":"touch /tmp/escape"}`, `{"path":"/"}`, `{"env":{}}`} {
		if _, err := tool.Handle(ctx, json.RawMessage(input)); err == nil {
			t.Fatalf("accepted override %s", input)
		}
	}
	if len(dirs) != 2 {
		t.Fatal("invalid input ran checks")
	}
}

func TestDaggerChecksBoundOutputAndWithholdCredentials(t *testing.T) {
	bin := t.TempDir()
	script := `#!/bin/sh
[ "$#" = 1 ] && [ "$1" = check ] || exit 91
[ -z "$GITHUB_TOKEN$ANTHROPIC_API_KEY$SSH_AUTH_SOCK" ] || exit 92
[ -d "$HOME" ] && [ "$HOME" = "$TMPDIR" ] || exit 93
[ -z "$EXPECTED_HOST_HOME" ] || exit 95
[ -f candidate.txt ] || exit 94
printf 'candidate checked\n'
head -c 70000 /dev/zero | tr '\000' x
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
	result, err := (DaggerChecks{}).Check(context.Background(), dir)
	must(t, err)
	if result.ExitCode != 3 || !result.Truncated || len(result.Output) != 64*1024 || !strings.HasPrefix(result.Output, "candidate checked\n") {
		t.Fatalf("result: exit=%d truncated=%t bytes=%d", result.ExitCode, result.Truncated, len(result.Output))
	}
}
