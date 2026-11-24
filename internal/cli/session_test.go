package cli

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kpenfound/agentcore/agent"
	wire "github.com/kpenfound/osmia/internal/api"
	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/service"
)

type fakeSessionEngine struct{}

func (fakeSessionEngine) Enforcer(coreadapter.ExecutionSettings) (agent.Enforcer, error) {
	return nil, errors.New("no agent runner configured for this test")
}

type fakeSessionChecks struct{}

func (fakeSessionChecks) List(context.Context, string, wire.CheckIdentity) ([]string, error) {
	return nil, nil
}
func (fakeSessionChecks) Check(context.Context, string, wire.CheckIdentity, []string) (wire.CheckResult, error) {
	return wire.CheckResult{ExitCode: 0, Output: "CHECKS all passed\n"}, nil
}

func gitIn(t *testing.T, home string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + home, "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=" + os.DevNull}
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return string(out)
}

func sessionFixture(t *testing.T) (string, config.ProjectID, string) {
	t.Helper()
	return sessionFixtureWithChecks(t, fakeSessionChecks{})
}

func sessionFixtureWithChecks(t *testing.T, checks service.ReviewChecks) (string, config.ProjectID, string) {
	t.Helper()
	opts, clone := emptyFixture(t)
	home := filepath.Dir(clone)
	must(t, os.WriteFile(filepath.Join(clone, "README.md"), []byte("hi\n"), 0600))
	gitIn(t, home, "-C", clone, "add", "README.md")
	gitIn(t, home, "-C", clone, "-c", "user.name=Owner", "-c", "user.email=owner@example.invalid", "commit", "-qm", "base")
	gitIn(t, home, "-C", clone, "branch", "-M", "main")
	bare := filepath.Join(home, "remotes", "dagger", "dagger.git")
	must(t, os.MkdirAll(filepath.Dir(bare), 0700))
	gitIn(t, home, "clone", "--quiet", "--bare", "--no-hardlinks", clone, bare)
	gitIn(t, home, "-C", clone, "remote", "add", "upstream", bare)
	opts = service.Enforce(opts, service.Enforcement{Engine: fakeSessionEngine{}, Checks: checks})
	s, err := service.Start(context.Background(), opts)
	must(t, err)
	t.Cleanup(func() { s.Close() })
	root := opts.Config.Root
	var added wire.ProjectResponse
	must(t, json.Unmarshal([]byte(successful(t, root, "project", "add", "dagger", "--upstream", "dagger/dagger", "--fork", "owner/dagger", "--clone", clone, "--json")), &added))
	return root, added.Project.ID, bare
}

func sessionWorktree(root string, project config.ProjectID, stream config.WorkstreamID) string {
	return filepath.Join(root, "sessions", string(project), string(stream))
}

func openSession(t *testing.T, root string, project config.ProjectID) wire.SessionView {
	t.Helper()
	var opened wire.SessionResponse
	must(t, json.Unmarshal([]byte(successful(t, root, "session", "open", string(project), "--profile", "default", "--title", "exploring", "--json")), &opened))
	return opened.Session
}

func commitNotes(t *testing.T, root string, project config.ProjectID, session wire.SessionView) (worktree string, committed wire.SessionCommitResponse) {
	t.Helper()
	worktree = sessionWorktree(root, project, session.Workstream)
	must(t, os.WriteFile(filepath.Join(worktree, "notes.md"), []byte("# Notes\n"), 0600))
	must(t, json.Unmarshal([]byte(successful(t, root, "session", "commit", string(session.Workstream), "Add notes", "--json")), &committed))
	return worktree, committed
}

func TestSessionOpenRefusesAnUnknownProfile(t *testing.T) {
	root, project, _ := sessionFixture(t)
	code, out, diag := invoke(t, root, "session", "open", string(project), "--profile", "nonexistent")
	if code != 4 || out != "" || !strings.Contains(diag, "validation") || !strings.Contains(diag, `profile "nonexistent"`) {
		t.Fatalf("unknown profile: %d %s %s", code, out, diag)
	}
}

func TestSessionOpenReportsTheIdleSession(t *testing.T) {
	root, project, _ := sessionFixture(t)
	session := openSession(t, root, project)
	if session.Project != project || session.Profile != "default" || session.Title != "exploring" || session.State != "idle" || session.Branch == "" || session.Base == "" || session.Workstream == "" {
		t.Fatalf("opened session: %+v", session)
	}
}

func TestSessionOpenTextNamesTheProfileStateAndTitle(t *testing.T) {
	root, project, _ := sessionFixture(t)
	text := successful(t, root, "session", "open", string(project), "--profile", "default", "--title", "second")
	if !strings.Contains(text, "profile=default") || !strings.Contains(text, "state=idle") || !strings.Contains(text, "Title: second") {
		t.Fatalf("open text output:\n%s", text)
	}
}

func TestSessionCommitRecordsTheWorktreeChanges(t *testing.T) {
	root, project, _ := sessionFixture(t)
	session := openSession(t, root, project)
	_, committed := commitNotes(t, root, project, session)
	if committed.Commit == "" || committed.Commit == session.Base {
		t.Fatalf("commit response: %+v", committed)
	}
}

func TestSessionCommitRefusesAnUnchangedWorktree(t *testing.T) {
	root, project, _ := sessionFixture(t)
	session := openSession(t, root, project)
	code, out, diag := invoke(t, root, "session", "commit", string(session.Workstream), "Nothing to add")
	if code == 0 || out != "" || !strings.Contains(diag, "validation") || !strings.Contains(diag, "no changes") {
		t.Fatalf("empty commit: %d %s %s", code, out, diag)
	}
}

func TestSessionCheckRunsAFullCheckOnTheCommit(t *testing.T) {
	root, project, _ := sessionFixture(t)
	session := openSession(t, root, project)
	_, committed := commitNotes(t, root, project, session)
	checkText := successful(t, root, "session", "check", string(session.Workstream))
	if !strings.Contains(checkText, "Check run 1 on commit "+committed.Commit) || !strings.Contains(checkText, "passed") || !strings.Contains(checkText, "full check") {
		t.Fatalf("check text output:\n%s", checkText)
	}
	var checked wire.SessionCheckRun
	must(t, json.Unmarshal([]byte(successful(t, root, "session", "check", string(session.Workstream), "--json")), &checked))
	if checked.Run != 2 || checked.Commit != committed.Commit || checked.Kind != "full" || checked.Status != "passed" || checked.ExitCode != 0 {
		t.Fatalf("check JSON output: %+v", checked)
	}
}

func TestSessionPushPublishesTheBranchToTheRemote(t *testing.T) {
	root, project, bare := sessionFixture(t)
	home := filepath.Dir(filepath.Dir(bare))
	session := openSession(t, root, project)
	_, committed := commitNotes(t, root, project, session)
	var pushed wire.SessionPushResponse
	must(t, json.Unmarshal([]byte(successful(t, root, "session", "push", string(session.Workstream), "--json")), &pushed))
	if pushed.Branch != session.Branch || pushed.Commit != committed.Commit {
		t.Fatalf("push response: %+v", pushed)
	}
	landed := strings.TrimSpace(gitIn(t, home, "-C", bare, "rev-parse", "refs/heads/"+session.Branch))
	if landed != committed.Commit {
		t.Fatalf("remote branch %s, want %s", landed, committed.Commit)
	}
}

func TestSessionEndReleasesTheWorktree(t *testing.T) {
	root, project, _ := sessionFixture(t)
	session := openSession(t, root, project)
	worktree, _ := commitNotes(t, root, project, session)
	var ended wire.SessionResponse
	must(t, json.Unmarshal([]byte(successful(t, root, "session", "end", string(session.Workstream), "--json")), &ended))
	if ended.Session.State != "ended" {
		t.Fatalf("ended session: %+v", ended.Session)
	}
	if _, err := os.Stat(worktree); !os.IsNotExist(err) {
		t.Fatalf("worktree %s still exists after end: %v", worktree, err)
	}
}

func TestEndedSessionRefusesCommitCheckAndPush(t *testing.T) {
	root, project, _ := sessionFixture(t)
	session := openSession(t, root, project)
	commitNotes(t, root, project, session)
	successful(t, root, "session", "end", string(session.Workstream), "--json")
	for _, verb := range [][]string{{"commit", string(session.Workstream), "too late"}, {"check", string(session.Workstream)}, {"push", string(session.Workstream)}} {
		code, out, diag := invoke(t, root, append([]string{"session"}, verb...)...)
		if code != 5 || out != "" || diag != "conflict: the session has ended\n" {
			t.Fatalf("%s after end: %d %s %s", verb[0], code, out, diag)
		}
	}
}

func TestSessionOpenUnknownProject(t *testing.T) {
	root, _, _ := sessionFixture(t)
	unknown := "p_00000000000000000000000000000099"
	code, out, diag := invoke(t, root, "session", "open", unknown, "--profile", "default")
	if code != 4 || out != "" || !strings.Contains(diag, "not_found") {
		t.Fatalf("unknown project: %d %s %s", code, out, diag)
	}
}

func TestSessionUsage(t *testing.T) {
	root := fixture(t).Config.Root
	for _, args := range [][]string{
		{"session"}, {"session", "open"}, {"session", "open", project},
		{"session", "open", project, "--profile", ""}, {"session", "open", project, "--title", "x"},
		{"session", "open", "not-an-id", "--profile", "default"},
		{"session", "open", project, "--profile", "default", "--title", "x", "extra"},
		{"session", "commit"}, {"session", "commit", stream}, {"session", "commit", stream, "a", "b"},
		{"session", "commit", "not-an-id", "a"},
		{"session", "check"}, {"session", "check", stream, "extra"}, {"session", "check", "not-an-id"},
		{"session", "push"}, {"session", "push", stream, "extra"}, {"session", "push", "not-an-id"},
		{"session", "end"}, {"session", "end", stream, "extra"}, {"session", "end", "not-an-id"},
		{"session", "destroy"}, {"session", "destroy", stream, "extra"}, {"session", "destroy", "not-an-id"},
		{"session", "unknown", stream},
		{"session", "open", project, "--profile", "default", "--hard"},
		{"status", "--profile", "default"}, {"status", "--title", "x"},
	} {
		code, out, diag := invoke(t, root, args...)
		if code != 2 || out != "" || !strings.Contains(diag, "invalid arguments") {
			t.Fatalf("%v: %d %s %s", args, code, out, diag)
		}
	}
}
