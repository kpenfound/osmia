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

// fakeSessionEngine refuses every turn, so the librarian's extraction a
// project registration starts automatically fails cleanly, the way it does
// with no runner configured, instead of reaching a live model. Session
// open, commit, check, push and end never start a turn, so nothing in these
// tests depends on it beyond keeping project registration from panicking.
type fakeSessionEngine struct{}

func (fakeSessionEngine) Enforcer(coreadapter.ExecutionSettings) (agent.Enforcer, error) {
	return nil, errors.New("no agent runner configured for this test")
}

// fakeSessionChecks is a check runner that always passes without a live
// Dagger engine, so the session check command can be exercised end to end.
type fakeSessionChecks struct{}

func (fakeSessionChecks) List(context.Context, string, wire.CheckIdentity) ([]string, error) {
	return nil, nil
}
func (fakeSessionChecks) Check(context.Context, string, wire.CheckIdentity, []string) (wire.CheckResult, error) {
	return wire.CheckResult{ExitCode: 0, Output: "CHECKS all passed\n"}, nil
}

// gitIn runs git against home's isolated configuration, the same way the
// fixtures in project_test.go and handin_test.go do.
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

// sessionFixture starts a service with one registered project backed by a
// committed local clone and a local bare "upstream" remote, so a session
// can open from a real default-branch head and a push has somewhere to
// land, with no live model, Dagger engine or remote delivery. It returns
// the root, the project ID and the bare remote's path.
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
	// The bare remote's path ends in "dagger/dagger.git", so workspace.Git's
	// Remote matches it against the project's upstream "dagger/dagger" by
	// URL suffix, the same way a real GitHub clone URL would.
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

// sessionWorktree is the worktree a session's open created, under the
// "sessions" directory sess-open keeps outside the target repository.
func sessionWorktree(root string, project config.ProjectID, stream config.WorkstreamID) string {
	return filepath.Join(root, "sessions", string(project), string(stream))
}

func TestSessionCommands(t *testing.T) {
	root, project, bare := sessionFixture(t)
	home := filepath.Dir(filepath.Dir(bare))

	// An unknown profile is refused, with nothing opened.
	code, out, diag := invoke(t, root, "session", "open", string(project), "--profile", "nonexistent")
	if code != 4 || out != "" || !strings.Contains(diag, "validation") || !strings.Contains(diag, `profile "nonexistent"`) {
		t.Fatalf("unknown profile: %d %s %s", code, out, diag)
	}
	var opened wire.SessionResponse
	must(t, json.Unmarshal([]byte(successful(t, root, "session", "open", string(project), "--profile", "default", "--title", "exploring", "--json")), &opened))
	session := opened.Session
	if session.Project != project || session.Profile != "default" || session.Title != "exploring" || session.State != "idle" || session.Branch == "" || session.Base == "" || session.Workstream == "" {
		t.Fatalf("opened session: %+v", session)
	}

	// The text form names the same facts the JSON form does.
	text := successful(t, root, "session", "open", string(project), "--profile", "default", "--title", "second")
	if !strings.Contains(text, "profile=default") || !strings.Contains(text, "state=idle") || !strings.Contains(text, "Title: second") {
		t.Fatalf("open text output:\n%s", text)
	}

	// Write an uncommitted change into the session's own worktree, then
	// commit it onto the session branch.
	worktree := sessionWorktree(root, project, session.Workstream)
	must(t, os.WriteFile(filepath.Join(worktree, "notes.md"), []byte("# Notes\n"), 0600))
	var committed wire.SessionCommitResponse
	must(t, json.Unmarshal([]byte(successful(t, root, "session", "commit", string(session.Workstream), "Add notes", "--json")), &committed))
	if committed.Commit == "" || committed.Commit == session.Base {
		t.Fatalf("commit response: %+v", committed)
	}

	// A commit with no changes is refused, and nothing is recorded.
	code, out, diag = invoke(t, root, "session", "commit", string(session.Workstream), "Nothing to add")
	if code == 0 || out != "" || !strings.Contains(diag, "validation") || !strings.Contains(diag, "no changes") {
		t.Fatalf("empty commit: %d %s %s", code, out, diag)
	}

	// Check runs the fake runner against the committed head.
	checkText := successful(t, root, "session", "check", string(session.Workstream))
	if !strings.Contains(checkText, "Check run 1 on commit "+committed.Commit) || !strings.Contains(checkText, "passed") || !strings.Contains(checkText, "full check") {
		t.Fatalf("check text output:\n%s", checkText)
	}
	var checked wire.SessionCheckRun
	must(t, json.Unmarshal([]byte(successful(t, root, "session", "check", string(session.Workstream), "--json")), &checked))
	if checked.Run != 2 || checked.Commit != committed.Commit || checked.Kind != "full" || checked.Status != "passed" || checked.ExitCode != 0 {
		t.Fatalf("check JSON output: %+v", checked)
	}

	// Push lands the session branch on the local bare remote.
	var pushed wire.SessionPushResponse
	must(t, json.Unmarshal([]byte(successful(t, root, "session", "push", string(session.Workstream), "--json")), &pushed))
	if pushed.Branch != session.Branch || pushed.Commit != committed.Commit {
		t.Fatalf("push response: %+v", pushed)
	}
	landed := strings.TrimSpace(gitIn(t, home, "-C", bare, "rev-parse", "refs/heads/"+session.Branch))
	if landed != committed.Commit {
		t.Fatalf("remote branch %s, want %s", landed, committed.Commit)
	}

	// End releases the worktree; the session is listed as ended.
	var ended wire.SessionResponse
	must(t, json.Unmarshal([]byte(successful(t, root, "session", "end", string(session.Workstream), "--json")), &ended))
	if ended.Session.State != "ended" {
		t.Fatalf("ended session: %+v", ended.Session)
	}
	if _, err := os.Stat(worktree); !os.IsNotExist(err) {
		t.Fatalf("worktree %s still exists after end: %v", worktree, err)
	}

	// A refused request once the session has ended shows the service's
	// own reason, not a generic failure.
	code, out, diag = invoke(t, root, "session", "commit", string(session.Workstream), "too late")
	if code != 5 || out != "" || diag != "conflict: the session has ended\n" {
		t.Fatalf("commit after end: %d %s %s", code, out, diag)
	}
	code, out, diag = invoke(t, root, "session", "check", string(session.Workstream))
	if code != 5 || out != "" || diag != "conflict: the session has ended\n" {
		t.Fatalf("check after end: %d %s %s", code, out, diag)
	}
	code, out, diag = invoke(t, root, "session", "push", string(session.Workstream))
	if code != 5 || out != "" || diag != "conflict: the session has ended\n" {
		t.Fatalf("push after end: %d %s %s", code, out, diag)
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
