package executionjourney

import (
	context "context"
	errors "errors"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	os "os"
	filepath "path/filepath"
	slices "slices"
	strings "strings"
	testing "testing"
	time "time"

	agent "github.com/kpenfound/agentcore/agent"
	jevtest "github.com/kpenfound/agentcore/jev/jevtest"
	systemone "github.com/kpenfound/agentcore/jev/systemone"
	"github.com/kpenfound/osmia/internal/api"
	checkrun "github.com/kpenfound/osmia/internal/checks"
	config "github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/jev"
	runtime "github.com/kpenfound/osmia/internal/runtime"
	"github.com/kpenfound/osmia/internal/service"
	trace "github.com/kpenfound/osmia/internal/trace"
)

func newSessionControlsFixture(t *testing.T, configure ...func(*Options)) (*Service, *Client, Options, config.ProjectID, config.WorkstreamID, *fixturekit.SessionEngine, string) {
	t.Helper()
	opts, clone := projectFixture(t)
	for _, apply := range configure {
		apply(&opts)
	}
	home := filepath.Dir(clone)
	fixturekit.Must(t, os.WriteFile(filepath.Join(clone, "README.md"), []byte("hi\n"), 0600))
	fixturekit.DemoGit(t, home, "-C", clone, "add", "README.md")
	fixturekit.DemoGit(t, home, "-C", clone, "-c", "user.name=Owner", "-c", "user.email=owner@example.invalid", "commit", "-qm", "base")
	fixturekit.DemoGit(t, home, "-C", clone, "branch", "-M", "main")
	bare := filepath.Join(home, "remotes", "dagger", "dagger.git")
	fixturekit.Must(t, os.MkdirAll(filepath.Dir(bare), 0700))
	fixturekit.DemoGit(t, home, "clone", "--quiet", "--bare", "--no-hardlinks", clone, bare)
	fixturekit.DemoGit(t, home, "-C", clone, "remote", "add", "upstream", bare)
	engine := &fixturekit.SessionEngine{}
	enforced := Enforce(opts, service.Enforcement{Engine: engine, Hosts: sessionImageHosts()})
	opts.Threads, opts.controls = enforced.Threads, enforced.controls
	s, c := start(t, opts)
	added, err := c.AddProject(context.Background(), fixturekit.Request(clone))
	fixturekit.Must(t, err)
	s.cfg = s.current()
	writeCharter(t, c, added.Project.ID, "1. Keep changes small.\n")
	resp, err := c.OpenSession(context.Background(), api.SessionOpenRequest{Project: added.Project.ID, Profile: "default"})
	fixturekit.Must(t, err)
	return s, c, opts, added.Project.ID, resp.Session.Workstream, engine, clone
}

func sessionWorktreePath(root string, project config.ProjectID, stream config.WorkstreamID) string {
	return filepath.Join(root, service.SessionsDirectory, string(project), string(stream))
}

func TestSessionChangesReadsUncommittedAndCommittedChanges(t *testing.T) {
	t.Parallel()
	s, c, _, project, stream, _, _ := newSessionControlsFixture(t)
	worktree := sessionWorktreePath(s.cfg.Root.String(), project, stream)
	empty, err := c.SessionChanges(context.Background(), stream)
	fixturekit.Must(t, err)
	if empty.Changes != "" || len(empty.Commits) != 0 {
		t.Fatalf("a fresh session's changes: %+v", empty)
	}
	fixturekit.Must(t, os.WriteFile(filepath.Join(worktree, "note.txt"), []byte("first draft\n"), 0600))
	dirty, err := c.SessionChanges(context.Background(), stream)
	fixturekit.Must(t, err)
	if !strings.Contains(dirty.Changes, "note.txt") || !strings.Contains(dirty.Changes, "first draft") || len(dirty.Commits) != 0 {
		t.Fatalf("uncommitted changes: %+v", dirty)
	}
	committed, err := c.SessionCommit(context.Background(), stream, "add a note", "changes-key-1")
	fixturekit.Must(t, err)
	fixturekit.Must(t, os.WriteFile(filepath.Join(worktree, "second.txt"), []byte("second draft\n"), 0600))
	after, err := c.SessionChanges(context.Background(), stream)
	fixturekit.Must(t, err)
	if after.Head != committed.Commit || !strings.Contains(after.Changes, "second.txt") || strings.Contains(after.Changes, "first draft") {
		t.Fatalf("changes after a commit: %+v", after)
	}
	if len(after.Commits) != 1 || after.Commits[0].Commit != committed.Commit || after.Commits[0].Message != "add a note" {
		t.Fatalf("committed changes: %+v", after.Commits)
	}
}

func TestSessionCommitCommitsAndThreadEntryReachesNextTurn(t *testing.T) {
	t.Parallel()
	s, c, _, project, stream, engine, clone := newSessionControlsFixture(t)
	home := filepath.Dir(clone)
	worktree := sessionWorktreePath(s.cfg.Root.String(), project, stream)
	base := strings.TrimSpace(fixturekit.DemoGit(t, home, "-C", worktree, "rev-parse", "HEAD"))
	fixturekit.Must(t, os.WriteFile(filepath.Join(worktree, "note.txt"), []byte("hi\n"), 0600))
	resp, err := c.SessionCommit(context.Background(), stream, "add a note", "commit-key-1")
	fixturekit.Must(t, err)
	if resp.Commit == "" || resp.Commit == base {
		t.Fatalf("commit response: %+v", resp)
	}
	if status := strings.TrimSpace(fixturekit.DemoGit(t, home, "-C", worktree, "status", "--porcelain")); status != "" {
		t.Fatalf("worktree after commit:\n%s", status)
	}
	tip := strings.TrimSpace(fixturekit.DemoGit(t, home, "-C", worktree, "rev-parse", "HEAD"))
	if tip != resp.Commit {
		t.Fatalf("session head is %s, want %s", tip, resp.Commit)
	}
	var prompt string
	engine.Turn = func(_ context.Context, req agent.Request) (*agent.Result, error) {
		prompt = req.Prompt
		return &agent.Result{ClaudeID: "model-session-1", ResultText: "ok"}, nil
	}
	_, err = c.SessionMessage(context.Background(), stream, "what did I commit?")
	fixturekit.Must(t, err)
	fixturekit.AwaitSession(t, c, stream, func(v api.SessionView) bool { return v.State == trace.SessionIdle })
	if !strings.Contains(prompt, resp.Commit) || !strings.Contains(prompt, "what did I commit?") {
		t.Fatalf("next turn's prompt lacks the commit entry:\n%s", prompt)
	}
}

func headOf(t *testing.T, home, dir string) string {
	t.Helper()
	return strings.TrimSpace(fixturekit.DemoGit(t, home, "-C", dir, "rev-parse", "HEAD"))
}

func sessionEntryCount(t *testing.T, s *Service, stream config.WorkstreamID) int {
	t.Helper()
	entries, err := service.SessionEntriesSince(s.sole().repository, stream, time.Time{})
	fixturekit.Must(t, err)
	return len(entries)
}

func TestSessionCommitRefusalsRecordNothing(t *testing.T) {
	t.Parallel()

	t.Run("while_a_turn_runs", func(t *testing.T) {
		t.Parallel()
		s, c, _, project, stream, engine, clone := newSessionControlsFixture(t)
		home := filepath.Dir(clone)
		worktree := sessionWorktreePath(s.cfg.Root.String(), project, stream)
		hold, entered := make(chan struct{}), make(chan struct{})
		engine.Turn = func(ctx context.Context, req agent.Request) (*agent.Result, error) {
			close(entered)
			select {
			case <-hold:
			case <-ctx.Done():
			}
			return &agent.Result{ClaudeID: "model-session-1", ResultText: "working"}, nil
		}
		_, err := c.SessionMessage(context.Background(), stream, "go")
		fixturekit.Must(t, err)
		select {
		case <-entered:
		case <-time.After(fixturekit.DemoTimeout):
			t.Fatal("turn did not start")
		}
		before := headOf(t, home, worktree)
		fixturekit.Must(t, os.WriteFile(filepath.Join(worktree, "note.txt"), []byte("hi\n"), 0600))
		_, err = c.SessionCommit(context.Background(), stream, "message", "key-busy")
		fixturekit.AssertCode(t, err, api.Conflict)
		close(hold)
		fixturekit.AwaitSession(t, c, stream, func(v api.SessionView) bool { return v.State == trace.SessionIdle })
		if after := headOf(t, home, worktree); after != before {
			t.Fatalf("a refused commit moved the head: %s, want %s", after, before)
		}
		if n := sessionEntryCount(t, s, stream); n != 0 {
			t.Fatalf("a refused commit recorded %d entries", n)
		}
	})

	t.Run("empty_message", func(t *testing.T) {
		t.Parallel()
		s, c, _, project, stream, _, clone := newSessionControlsFixture(t)
		home := filepath.Dir(clone)
		worktree := sessionWorktreePath(s.cfg.Root.String(), project, stream)
		before := headOf(t, home, worktree)
		fixturekit.Must(t, os.WriteFile(filepath.Join(worktree, "note.txt"), []byte("hi\n"), 0600))
		_, err := c.SessionCommit(context.Background(), stream, "   ", "key-empty")
		fixturekit.AssertCode(t, err, api.Validation)
		if after := headOf(t, home, worktree); after != before {
			t.Fatalf("a refused commit moved the head: %s, want %s", after, before)
		}
		if n := sessionEntryCount(t, s, stream); n != 0 {
			t.Fatalf("a refused commit recorded %d entries", n)
		}
	})

	t.Run("no_changes", func(t *testing.T) {
		t.Parallel()
		s, c, _, project, stream, _, clone := newSessionControlsFixture(t)
		home := filepath.Dir(clone)
		worktree := sessionWorktreePath(s.cfg.Root.String(), project, stream)
		before := headOf(t, home, worktree)
		_, err := c.SessionCommit(context.Background(), stream, "message", "key-no-changes")
		fixturekit.AssertCode(t, err, api.Validation)
		if after := headOf(t, home, worktree); after != before {
			t.Fatalf("a refused commit moved the head: %s, want %s", after, before)
		}
		if n := sessionEntryCount(t, s, stream); n != 0 {
			t.Fatalf("a refused commit recorded %d entries", n)
		}
	})

	t.Run("ended", func(t *testing.T) {
		t.Parallel()
		s, c, _, project, stream, _, clone := newSessionControlsFixture(t)
		home := filepath.Dir(clone)
		worktree := sessionWorktreePath(s.cfg.Root.String(), project, stream)
		before := headOf(t, home, worktree)
		_, err := c.SessionEnd(context.Background(), stream)
		fixturekit.Must(t, err)
		_, err = c.SessionCommit(context.Background(), stream, "message", "key-ended")
		fixturekit.AssertCode(t, err, api.Conflict)
		if n := sessionEntryCount(t, s, stream); n != 0 {
			t.Fatalf("a refused commit recorded %d entries", n)
		}
		_ = before
	})
}

func TestSessionCommitRetryWithSameRequestKeyIsIdempotentAcrossRestart(t *testing.T) {
	t.Parallel()
	s, c, opts, project, stream, _, clone := newSessionControlsFixture(t)
	home := filepath.Dir(clone)
	worktree := sessionWorktreePath(s.cfg.Root.String(), project, stream)
	base := headOf(t, home, worktree)
	fixturekit.Must(t, os.WriteFile(filepath.Join(worktree, "note.txt"), []byte("hi\n"), 0600))

	first, err := c.SessionCommit(context.Background(), stream, "add a note", "same-key")
	fixturekit.Must(t, err)

	again, err := c.SessionCommit(context.Background(), stream, "add a note", "same-key")
	fixturekit.Must(t, err)
	if again.Commit != first.Commit {
		t.Fatalf("a retry before a restart made a second commit: %s, want %s", again.Commit, first.Commit)
	}

	fixturekit.Must(t, s.Close())
	s, c = start(t, opts)
	afterRestart, err := c.SessionCommit(context.Background(), stream, "add a note", "same-key")
	fixturekit.Must(t, err)
	if afterRestart.Commit != first.Commit {
		t.Fatalf("a retry across a restart made a second commit: %s, want %s", afterRestart.Commit, first.Commit)
	}
	g := service.SessionGit(s.cfg)
	messages, err := g.Log(context.Background(), base, first.Commit)
	fixturekit.Must(t, err)
	if len(messages) != 1 {
		t.Fatalf("commits on the session branch: %+v, want exactly one", messages)
	}
	if n := sessionEntryCount(t, s, stream); n != 1 {
		t.Fatalf("session entries after retries: %d, want exactly one", n)
	}
}

func TestSessionCommitRetryAfterACrashBetweenTheCommitAndItsRecordStillRecords(t *testing.T) {
	t.Parallel()
	s, c, project, stream, _, clone := newSessionChatFixture(t)
	home := filepath.Dir(clone)
	worktree := sessionWorktreePath(s.cfg.Root.String(), project, stream)
	g := service.SessionGit(s.cfg)
	wt, found, err := g.Workspace(context.Background(), string(stream))
	fixturekit.Must(t, err)
	if !found {
		t.Fatal("the session has no worktree")
	}
	fixturekit.Must(t, os.WriteFile(filepath.Join(worktree, "note.txt"), []byte("hi\n"), 0600))
	made, _, err := g.CommitChanges(context.Background(), wt, service.SessionCommitMessage("add a note", "crash-key"), s.now())
	fixturekit.Must(t, err)
	if n := sessionEntryCount(t, s, stream); n != 0 {
		t.Fatalf("a direct commit recorded %d entries, want none", n)
	}

	resp, err := c.SessionCommit(context.Background(), stream, "add a note", "crash-key")
	fixturekit.Must(t, err)
	if resp.Commit != made {
		t.Fatalf("retry after a crash made a second commit: %s, want %s", resp.Commit, made)
	}
	if tip := headOf(t, home, worktree); tip != made {
		t.Fatalf("session head moved: %s, want %s", tip, made)
	}
	if n := sessionEntryCount(t, s, stream); n != 1 {
		t.Fatalf("session entries after the recovering retry: %d, want exactly one", n)
	}
	entries, err := service.SessionEntriesSince(s.sole().repository, stream, time.Time{})
	fixturekit.Must(t, err)
	if !strings.Contains(entries[0].Text, made) {
		t.Fatalf("the recovered entry does not name the commit: %q", entries[0].Text)
	}

	again, err := c.SessionCommit(context.Background(), stream, "add a note", "crash-key")
	fixturekit.Must(t, err)
	if again.Commit != made {
		t.Fatalf("a second retry made a second commit: %s, want %s", again.Commit, made)
	}
	if n := sessionEntryCount(t, s, stream); n != 1 {
		t.Fatalf("session entries after a second retry: %d, want exactly one", n)
	}
}

type checkCapture struct {
	links  []string
	file   string
	found  bool
	result api.CheckResult
}

func (c *checkCapture) List(context.Context, string, api.CheckIdentity) ([]string, error) {
	return c.links, nil
}

func (c *checkCapture) Check(_ context.Context, dir string, _ api.CheckIdentity, _ []string) (api.CheckResult, error) {
	if c.file != "" {
		_, err := os.Stat(filepath.Join(dir, c.file))
		c.found = err == nil
	}
	if c.result.Output == "" {
		return api.CheckResult{Output: fixturekit.PassedReport}, nil
	}
	return c.result, nil
}

func TestSessionCheckRunsFullUnderJEVDisabled(t *testing.T) {
	t.Parallel()
	s, c, _, project, stream, _, clone := newSessionControlsFixture(t, func(opts *Options) {
		path := filepath.Join(opts.Config.Root, "config.toml")
		data, err := os.ReadFile(path)
		fixturekit.Must(t, err)
		fixturekit.Must(t, os.WriteFile(path, append(data, []byte("\n[jev]\ncheck_fallback_picks = 2\n")...), 0600))
	})
	checker := &fixturekit.FakeChecks{Links: []string{"dag+check://a"}, Outcomes: fixturekit.Passing()}
	service.InspectForTest(s.core()).SetReviewChecks(checker)
	home := filepath.Dir(clone)
	head := headOf(t, home, sessionWorktreePath(s.cfg.Root.String(), project, stream))
	run, err := c.SessionCheck(context.Background(), stream)
	fixturekit.Must(t, err)
	if ran := checker.Runs(); len(ran) != 1 || len(ran[0]) != 0 {
		t.Fatalf("full session ran %v", ran)
	}
	if run.Kind != "full" || run.Status != checkrun.ChecksPassed || run.ExitCode != 0 || run.Commit != head || run.Run != 1 {
		t.Fatalf("check run: %+v, want full/passed on %s", run, head)
	}
	if n := sessionEntryCount(t, s, stream); n != 1 {
		t.Fatalf("session entries after a check: %d, want exactly one", n)
	}
	entries, err := service.SessionEntriesSince(s.sole().repository, stream, time.Time{})
	fixturekit.Must(t, err)
	if !strings.Contains(entries[0].Text, head) {
		t.Fatalf("the check's thread entry does not name the commit: %q", entries[0].Text)
	}
	runs, err := service.SessionCheckRuns(s.sole().repository, stream)
	fixturekit.Must(t, err)
	if len(runs) != 1 || runs[0].Commit != head || runs[0].Status != checkrun.ChecksPassed {
		t.Fatalf("recorded check runs: %+v", runs)
	}
}

func TestSessionCheckRunsSmartSelectionUnderJEV(t *testing.T) {
	t.Parallel()
	s, c, _, _, stream, _, _ := newSessionControlsFixture(t)
	links := []string{
		"dag+check://release/version-round-trip",
		"dag+check://go/packages/tests/test?go-package=internal/web&go-test=TestPage",
	}
	checker := &checkCapture{links: links}
	service.InspectForTest(s.core()).SetReviewChecks(checker)

	p := &jevtest.Provider{Results: []jevtest.Result{{Response: systemone.Response{Model: "jev-1.0", Answers: map[string]systemone.Answer{
		"check-0": jevtest.Noul(0.02), "check-1": jevtest.Noul(0.92)}}}}}
	useJevProvider(s, p)
	run, err := c.SessionCheck(context.Background(), stream)
	fixturekit.Must(t, err)
	if run.Kind != "smart" || !slices.Equal(run.Checks, []string{links[0]}) || run.Status != checkrun.ChecksPassed {
		t.Fatalf("check run: %+v", run)
	}
}

func TestSessionCheckRunsFullWhenDaggerRefusesTheSelection(t *testing.T) {
	t.Parallel()
	s, c, _, _, stream, _, _ := newSessionControlsFixture(t)
	links := []string{"dag+check://release/version-round-trip", "dag+check://go/packages/tests/test?go-package=internal/web&go-test=TestPage"}
	const message = "no checks matched pattern"
	refused := &refusingChecks{links: links, message: message}
	service.InspectForTest(s.core()).SetReviewChecks(refused)
	p := &jevtest.Provider{Results: []jevtest.Result{{Response: systemone.Response{Model: "jev-1.0", Answers: map[string]systemone.Answer{
		"check-0": jevtest.Noul(0.02), "check-1": jevtest.Noul(0.92)}}}}}
	useJevProvider(s, p)
	run, err := c.SessionCheck(context.Background(), stream)
	fixturekit.Must(t, err)
	if run.Kind != "full" || len(run.Checks) != 0 || run.Status != checkrun.ChecksPassed || run.Selection.Reason != checkrun.SelectionRefused || run.Selection.Detail != message {
		t.Fatalf("check run: %+v, selection %+v", run, run.Selection)
	}
	if !slices.Equal(run.Command, checkrun.Command(nil)) || !strings.Contains(run.Output, "CHECKS") {
		t.Fatalf("check run: %+v", run)
	}
	if len(refused.ran) != 2 || len(refused.ran[0]) != 1 || refused.ran[1] != nil {
		t.Fatalf("ran %q", refused.ran)
	}
}

func TestSessionCheckNotesFlakyChecks(t *testing.T) {
	t.Parallel()
	s, c, _, _, stream, _, _ := newSessionControlsFixture(t)
	const link = "dag://go/packages/tests/test?go-test=TestFlaky"
	checker := &fixturekit.FakeChecks{Outcomes: []fixturekit.CheckOutcome{{Result: api.CheckResult{Output: fixturekit.FailedReport(link) + "CHECKS  ✔ 1 passed\n✔ " + link + " 1s OK\n", Flaky: []string{link}}}}}
	service.InspectForTest(s.core()).SetReviewChecks(checker)
	run, err := c.SessionCheck(context.Background(), stream)
	fixturekit.Must(t, err)
	if run.Status != checkrun.ChecksPassed || run.ExitCode != 0 || len(run.Failed) != 0 || !slices.Equal(run.Flaky, []string{link}) {
		t.Fatalf("check run: %+v", run)
	}
	entries, err := service.SessionEntriesSince(s.sole().repository, stream, time.Time{})
	fixturekit.Must(t, err)
	if !strings.Contains(entries[0].Text, checkrun.DescribeFlaky([]string{link})) {
		t.Fatalf("the check's thread entry does not note the flake: %q", entries[0].Text)
	}
	feed, err := service.FeedEntries(s.sole().repository, s.sole().repository, stream)
	fixturekit.Must(t, err)
	if !slices.ContainsFunc(feed, func(e api.FeedEntry) bool {
		return e.SessionChecks != nil && slices.Equal(e.SessionChecks.Flaky, []string{link})
	}) {
		t.Fatalf("the feed does not note the flake: %s", describeFeed(feed))
	}
}

type refusingChecks struct {
	links   []string
	message string
	ran     [][]string
}

func (r *refusingChecks) List(context.Context, string, checkrun.Identity) ([]string, error) {
	return r.links, nil
}

func (r *refusingChecks) Check(_ context.Context, _ string, _ checkrun.Identity, links []string) (api.CheckResult, error) {
	r.ran = append(r.ran, links)
	if len(links) > 0 {
		return api.CheckResult{ExitCode: 1, Output: "Error: " + r.message + "\n", Refusal: r.message}, nil
	}
	return api.CheckResult{Output: "CHECKS  ✔ 2 passed\n"}, nil
}

func TestSessionCheckExcludesUncommittedChanges(t *testing.T) {
	t.Parallel()
	s, c, _, project, stream, _, _ := newSessionControlsFixture(t)
	worktree := sessionWorktreePath(s.cfg.Root.String(), project, stream)
	fixturekit.Must(t, os.WriteFile(filepath.Join(worktree, "uncommitted.txt"), []byte("never committed\n"), 0600))
	checker := &checkCapture{file: "uncommitted.txt"}
	service.InspectForTest(s.core()).SetReviewChecks(checker)
	_, err := c.SessionCheck(context.Background(), stream)
	fixturekit.Must(t, err)
	if checker.found {
		t.Fatal("the checked export included an uncommitted file")
	}
}

func TestSessionCheckWhileOneIsRunningIsRefused(t *testing.T) {
	t.Parallel()
	s, c, _, _, stream, _, _ := newSessionControlsFixture(t)
	hold, entered := make(chan struct{}), make(chan struct{})
	service.InspectForTest(s.core()).SetReviewChecks(fixturekit.CheckFunc(func(ctx context.Context, dir string) (api.CheckResult, error) {
		close(entered)
		select {
		case <-hold:
		case <-ctx.Done():
		}
		return api.CheckResult{Output: fixturekit.PassedReport}, nil
	}))
	done := make(chan error, 1)
	go func() {
		_, err := c.SessionCheck(context.Background(), stream)
		done <- err
	}()
	select {
	case <-entered:
	case <-time.After(fixturekit.DemoTimeout):
		t.Fatal("first check did not start")
	}
	_, err := c.SessionCheck(context.Background(), stream)
	fixturekit.AssertCode(t, err, api.Conflict)
	close(hold)
	if err := <-done; err != nil {
		t.Fatalf("the first check failed: %v", err)
	}
}

func TestSessionPushAgainstLocalBareRemote(t *testing.T) {
	t.Parallel()
	s, c, _, project, stream, _, clone := newSessionControlsFixture(t)
	home := filepath.Dir(clone)
	worktree := sessionWorktreePath(s.cfg.Root.String(), project, stream)
	bare := filepath.Join(home, "remotes", "dagger", "dagger.git")
	mainBefore := strings.TrimSpace(fixturekit.DemoGit(t, home, "-C", bare, "rev-parse", "main"))

	fixturekit.Must(t, os.WriteFile(filepath.Join(worktree, "note.txt"), []byte("hi\n"), 0600))
	committed, err := c.SessionCommit(context.Background(), stream, "add a note", "push-key")
	fixturekit.Must(t, err)

	resp, err := c.SessionPush(context.Background(), stream)
	fixturekit.Must(t, err)
	if resp.Remote != "upstream" || resp.Commit != committed.Commit {
		t.Fatalf("push response: %+v, want commit %s", resp, committed.Commit)
	}
	if got := strings.TrimSpace(fixturekit.DemoGit(t, home, "-C", bare, "rev-parse", resp.Branch)); got != committed.Commit {
		t.Fatalf("remote branch %s is at %s, want %s", resp.Branch, got, committed.Commit)
	}
	if got := strings.TrimSpace(fixturekit.DemoGit(t, home, "-C", bare, "rev-parse", "main")); got != mainBefore {
		t.Fatalf("the default branch moved: %s, want %s", got, mainBefore)
	}

	fixturekit.Must(t, os.WriteFile(filepath.Join(worktree, "second.txt"), []byte("more\n"), 0600))
	second, err := c.SessionCommit(context.Background(), stream, "second note", "push-key-2")
	fixturekit.Must(t, err)
	resp2, err := c.SessionPush(context.Background(), stream)
	fixturekit.Must(t, err)
	if resp2.Commit != second.Commit {
		t.Fatalf("second push: %+v, want %s", resp2, second.Commit)
	}

	scratch := filepath.Join(home, "scratch-push")
	fixturekit.DemoGit(t, home, "clone", "--quiet", bare, scratch)
	fixturekit.DemoGit(t, home, "-C", scratch, "checkout", "-q", resp.Branch)
	fixturekit.Must(t, os.WriteFile(filepath.Join(scratch, "diverged.txt"), []byte("not on the session branch\n"), 0600))
	fixturekit.DemoGit(t, home, "-C", scratch, "add", "diverged.txt")
	fixturekit.DemoGit(t, home, "-C", scratch, "-c", "user.name=Other", "-c", "user.email=other@example.invalid", "commit", "-qm", "diverged")
	fixturekit.DemoGit(t, home, "-C", scratch, "push", "--quiet", "origin", resp.Branch)
	diverged := strings.TrimSpace(fixturekit.DemoGit(t, home, "-C", bare, "rev-parse", resp.Branch))

	_, err = c.SessionPush(context.Background(), stream)
	fixturekit.AssertCode(t, err, api.Conflict)
	if got := strings.TrimSpace(fixturekit.DemoGit(t, home, "-C", bare, "rev-parse", resp.Branch)); got != diverged {
		t.Fatalf("a refused push moved the remote branch: %s, want %s", got, diverged)
	}
}

func TestSessionEndIsRefusedWhileATurnRuns(t *testing.T) {
	t.Parallel()
	_, c, _, stream, engine, _ := newSessionChatFixture(t)
	hold, entered := make(chan struct{}), make(chan struct{})
	engine.Turn = func(ctx context.Context, req agent.Request) (*agent.Result, error) {
		close(entered)
		select {
		case <-hold:
		case <-ctx.Done():
		}
		return &agent.Result{ClaudeID: "model-session-1", ResultText: "working"}, nil
	}
	_, err := c.SessionMessage(context.Background(), stream, "go")
	fixturekit.Must(t, err)
	select {
	case <-entered:
	case <-time.After(fixturekit.DemoTimeout):
		t.Fatal("turn did not start")
	}
	_, err = c.SessionEnd(context.Background(), stream)
	fixturekit.AssertCode(t, err, api.Conflict)
	close(hold)
	fixturekit.AwaitSession(t, c, stream, func(v api.SessionView) bool { return v.State == trace.SessionIdle })
}

type endedSession struct {
	s         *Service
	c         *Client
	stream    config.WorkstreamID
	worktree  string
	modelDir  string
	committed string
}

func endSessionAfterACommit(t *testing.T) endedSession {
	t.Helper()
	s, c, project, stream, engine, _ := newSessionChatFixture(t)
	worktree := sessionWorktreePath(s.cfg.Root.String(), project, stream)
	modelDir := runtime.ModelSessionDirectory(s.cfg.Root.String(), project, stream)
	engine.Turn = func(context.Context, agent.Request) (*agent.Result, error) {
		return &agent.Result{ClaudeID: "model-session-1", ResultText: "working"}, nil
	}
	_, err := c.SessionMessage(context.Background(), stream, "go")
	fixturekit.Must(t, err)
	fixturekit.AwaitSession(t, c, stream, func(v api.SessionView) bool { return v.State == trace.SessionIdle })
	fixturekit.Must(t, os.WriteFile(filepath.Join(worktree, "note.txt"), []byte("hi\n"), 0600))
	committed, err := c.SessionCommit(context.Background(), stream, "add a note", "end-key")
	fixturekit.Must(t, err)
	resp, err := c.SessionEnd(context.Background(), stream)
	fixturekit.Must(t, err)
	if resp.Session.State != trace.SessionEnded {
		t.Fatalf("ended session view: %+v", resp.Session)
	}
	return endedSession{s: s, c: c, stream: stream, worktree: worktree, modelDir: modelDir, committed: committed.Commit}
}

func TestSessionEndReleasesItsStateAndKeepsItsRecord(t *testing.T) {
	t.Parallel()
	e := endSessionAfterACommit(t)
	if _, err := os.Stat(e.worktree); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("worktree after end: %v", err)
	}
	if _, err := os.Stat(e.modelDir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("model-session state after end: %v", err)
	}
	listed, err := e.c.Status(context.Background(), e.stream)
	fixturekit.Must(t, err)
	if listed.State == nil || *listed.State != trace.SessionEnded {
		t.Fatalf("listed state after end: %+v", listed.State)
	}
	changes, err := e.c.SessionChanges(context.Background(), e.stream)
	fixturekit.Must(t, err)
	if len(changes.Commits) != 1 || changes.Commits[0].Commit != e.committed {
		t.Fatalf("commits after end: %+v", changes.Commits)
	}
	th, err := e.s.sole().repository.Thread(e.stream, service.SessionAgentID)
	if err != nil || len(th.Turns) != 1 {
		t.Fatalf("thread after end: %+v, %v", th, err)
	}
}

func TestAnEndedSessionRefusesEveryControl(t *testing.T) {
	t.Parallel()
	e := endSessionAfterACommit(t)
	ctx := context.Background()
	if _, err := e.c.SessionMessage(ctx, e.stream, "hello"); err == nil {
		t.Fatal("a message was accepted after end")
	} else {
		fixturekit.AssertCode(t, err, api.Conflict)
	}
	if _, err := e.c.SessionCommit(ctx, e.stream, "message", "end-key-2"); err == nil {
		t.Fatal("a commit was accepted after end")
	} else {
		fixturekit.AssertCode(t, err, api.Conflict)
	}
	if _, err := e.c.SessionCheck(ctx, e.stream); err == nil {
		t.Fatal("a check was accepted after end")
	} else {
		fixturekit.AssertCode(t, err, api.Conflict)
	}
	if _, err := e.c.SessionPush(ctx, e.stream); err == nil {
		t.Fatal("a push was accepted after end")
	} else {
		fixturekit.AssertCode(t, err, api.Conflict)
	}
}

func TestEndingAnEndedSessionChangesNothing(t *testing.T) {
	t.Parallel()
	e := endSessionAfterACommit(t)
	again, err := e.c.SessionEnd(context.Background(), e.stream)
	fixturekit.Must(t, err)
	if again.Session.State != trace.SessionEnded {
		t.Fatalf("re-ending an ended session: %+v", again.Session)
	}
}

func TestSessionCommitAndEndRefuseWhileAMessageIsBeingStarted(t *testing.T) {
	t.Parallel()
	s, c, _, _, stream, _, _ := newSessionControlsFixture(t)
	lock := service.SessionLock(s.sole().repository, stream)
	hold, held := make(chan struct{}), make(chan struct{})
	go func() {
		lock.Lock()
		close(held)
		<-hold
		lock.Unlock()
	}()
	t.Cleanup(func() { close(hold) })
	<-held

	_, err := c.SessionCommit(context.Background(), stream, "message", "lock-key")
	fixturekit.AssertCode(t, err, api.Conflict)
	_, err = c.SessionEnd(context.Background(), stream)
	fixturekit.AssertCode(t, err, api.Conflict)
}

func TestSessionCommitAndCheckRefuseOnceEndHasBegun(t *testing.T) {
	t.Parallel()
	s, c, project, stream, _, _ := newSessionChatFixture(t)
	worktree := sessionWorktreePath(s.cfg.Root.String(), project, stream)
	fixturekit.Must(t, os.WriteFile(filepath.Join(worktree, "note.txt"), []byte("hi\n"), 0600))

	lock := service.SessionLock(s.sole().repository, stream)
	hold, held := make(chan struct{}), make(chan struct{})
	go func() {
		lock.Lock()
		close(held)
		<-hold
		lock.Unlock()
	}()
	t.Cleanup(func() { close(hold) })
	<-held

	_, err := c.SessionCommit(context.Background(), stream, "message", "end-race-key")
	fixturekit.AssertCode(t, err, api.Conflict)
	if n := sessionEntryCount(t, s, stream); n != 0 {
		t.Fatalf("a commit refused while end is in progress recorded %d entries", n)
	}

	_, err = c.SessionCheck(context.Background(), stream)
	fixturekit.AssertCode(t, err, api.Conflict)
	runs, err := service.SessionCheckRuns(s.sole().repository, stream)
	fixturekit.Must(t, err)
	if len(runs) != 0 {
		t.Fatalf("a check refused while end is in progress recorded %d runs", len(runs))
	}
}

func TestSessionCheckOutlivesDefaultWriteTimeout(t *testing.T) {
	t.Parallel()
	opts, clone := projectFixture(t)
	home := filepath.Dir(clone)
	fixturekit.Must(t, os.WriteFile(filepath.Join(clone, "README.md"), []byte("hi\n"), 0600))
	fixturekit.DemoGit(t, home, "-C", clone, "add", "README.md")
	fixturekit.DemoGit(t, home, "-C", clone, "-c", "user.name=Owner", "-c", "user.email=owner@example.invalid", "commit", "-qm", "base")
	fixturekit.DemoGit(t, home, "-C", clone, "branch", "-M", "main")
	bare := filepath.Join(home, "remotes", "dagger", "dagger.git")
	fixturekit.Must(t, os.MkdirAll(filepath.Dir(bare), 0700))
	fixturekit.DemoGit(t, home, "clone", "--quiet", "--bare", "--no-hardlinks", clone, bare)
	fixturekit.DemoGit(t, home, "-C", clone, "remote", "add", "upstream", bare)
	s, c := start(t, opts)
	added, err := c.AddProject(context.Background(), fixturekit.Request(clone))
	fixturekit.Must(t, err)
	writeCharter(t, c, added.Project.ID, "1. Keep changes small.\n")
	resp, err := c.OpenSession(context.Background(), api.SessionOpenRequest{Project: added.Project.ID, Profile: "default"})
	fixturekit.Must(t, err)
	stream := resp.Session.Workstream

	fixturekit.Must(t, s.Close())
	opts.WriteTimeout = 20 * time.Millisecond
	s, c = start(t, opts)

	c.SetSessionCheckTimeoutForTest(fixturekit.DemoTimeout)
	service.InspectForTest(s.core()).SetReviewChecks(fixturekit.CheckFunc(func(context.Context, string) (api.CheckResult, error) {
		time.Sleep(100 * time.Millisecond)
		return api.CheckResult{Output: fixturekit.PassedReport}, nil
	}))
	run, err := c.SessionCheck(context.Background(), stream)
	if err != nil {
		t.Fatalf("session check after the write timeout: %v", err)
	}
	if run.Status != checkrun.ChecksPassed {
		t.Fatalf("check run: %+v", run)
	}
}

func TestSessionCheckRequiresReadableDiff(t *testing.T) {
	t.Parallel()
	s, c, _, project, stream, _, clone := newSessionControlsFixture(t)
	home := filepath.Dir(clone)
	baseTree := strings.TrimSpace(fixturekit.DemoGit(t, home, "-C", clone, "rev-parse", "HEAD^{tree}"))
	worktree := sessionWorktreePath(s.cfg.Root.String(), project, stream)
	fixturekit.Must(t, os.WriteFile(filepath.Join(worktree, "note.txt"), []byte("new tree\n"), 0600))
	commit, err := c.SessionCommit(context.Background(), stream, "add a note", "diff-check")
	fixturekit.Must(t, err)

	fixturekit.Must(t, os.Remove(filepath.Join(clone, ".git", "objects", baseTree[:2], baseTree[2:])))
	fixturekit.Must(t, service.SessionGit(s.current().For(project)).Export(context.Background(), commit.Commit, t.TempDir()))
	checker := &fixturekit.FakeChecks{Links: []string{"dag+check://go/test"}, Outcomes: fixturekit.Passing()}
	service.InspectForTest(s.core()).SetReviewChecks(checker)
	provider := &jevtest.Provider{}
	useJevProvider(s, provider)
	_, err = c.SessionCheck(context.Background(), stream)
	fixturekit.AssertCode(t, err, api.Internal)
	if !strings.Contains(err.Error(), "cannot diff the session's base and head") {
		t.Fatalf("diff failure: %v", err)
	}
	if len(checker.Runs()) != 0 || len(provider.Requests()) != 0 {
		t.Fatal("unreadable change reached selection or the check runner")
	}
	runs, err := service.SessionCheckRuns(s.sole().repository, stream)
	fixturekit.Must(t, err)
	if len(runs) != 0 {
		t.Fatalf("unreadable change recorded a check run: %+v", runs)
	}
}

func TestSessionCheckRunsFallbackPicksAndReportsReloadedConfiguration(t *testing.T) {
	t.Parallel()
	s, c, opts, _, stream, _, _ := newSessionControlsFixture(t)
	path := filepath.Join(opts.Config.Root, "config.toml")
	top, readErr := os.ReadFile(path)
	fixturekit.Must(t, readErr)
	fixturekit.Must(t, os.WriteFile(path, append(top, []byte("\n[jev]\ncheck_fallback_picks = 2\n")...), 0600))
	_, err := c.Reload(t.Context())
	fixturekit.Must(t, err)
	reported, err := c.Configuration(t.Context())
	fixturekit.Must(t, err)
	if reported.Effective.Jev.CheckFallbackPicks != 2 {
		t.Fatalf("reported config %+v", reported)
	}
	checker := &fixturekit.FakeChecks{Links: []string{"dag+check://a", "dag+check://b", "dag+check://c"}, Outcomes: fixturekit.Passing()}
	service.InspectForTest(s.core()).SetReviewChecks(checker)
	p := &jevtest.Provider{Results: []jevtest.Result{{Response: systemone.Response{Model: "jev-test", Answers: map[string]systemone.Answer{"check-0": jevtest.Noul(.1), "check-1": jevtest.Noul(.4), "check-2": jevtest.Noul(.3)}}}}}
	j := &jev.Judge{Config: func() config.Jev { return s.current().Jev }, Provider: p.Factory(), Getenv: func(string) string { return "key" }, Now: s.now}
	service.InspectForTest(s.core()).SetJudge(j)
	run, err := c.SessionCheck(t.Context(), stream)
	fixturekit.Must(t, err)
	want := []string{"dag+check://b", "dag+check://c"}
	if ran := checker.Runs(); len(ran) != 1 || !slices.Equal(ran[0], want) || run.Kind != "smart" || run.Selection.FallbackPicks != 2 || !slices.Equal(run.Checks, want) {
		t.Fatalf("ran %v, run %+v", ran, run)
	}
}
