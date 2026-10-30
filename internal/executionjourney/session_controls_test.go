package executionjourney

import (
	context "context"
	errors "errors"
	agent "github.com/kpenfound/agentcore/agent"
	jevtest "github.com/kpenfound/agentcore/jev/jevtest"
	systemone "github.com/kpenfound/agentcore/jev/systemone"
	"github.com/kpenfound/osmia/internal/api"
	config "github.com/kpenfound/osmia/internal/config"
	runtime "github.com/kpenfound/osmia/internal/runtime"
	"github.com/kpenfound/osmia/internal/service"
	trace "github.com/kpenfound/osmia/internal/trace"
	os "os"
	filepath "path/filepath"
	slices "slices"
	strings "strings"
	testing "testing"
	time "time"
)

// newSessionControlsFixture is newSessionChatFixture but also returns the
// options the service started with, so a test can close and restart the
// service on the same root and clone, and the project ID, for building
// paths under the root.
func newSessionControlsFixture(t *testing.T, configure ...func(*Options)) (*Service, *Client, Options, config.ProjectID, config.WorkstreamID, *sessionEngine, string) {
	t.Helper()
	opts, clone := projectFixture(t)
	for _, apply := range configure {
		apply(&opts)
	}
	home := filepath.Dir(clone)
	must(t, os.WriteFile(filepath.Join(clone, "README.md"), []byte("hi\n"), 0600))
	demoGit(t, home, "-C", clone, "add", "README.md")
	demoGit(t, home, "-C", clone, "-c", "user.name=Owner", "-c", "user.email=owner@example.invalid", "commit", "-qm", "base")
	demoGit(t, home, "-C", clone, "branch", "-M", "main")
	bare := filepath.Join(home, "remotes", "dagger", "dagger.git")
	must(t, os.MkdirAll(filepath.Dir(bare), 0700))
	demoGit(t, home, "clone", "--quiet", "--bare", "--no-hardlinks", clone, bare)
	demoGit(t, home, "-C", clone, "remote", "add", "upstream", bare)
	engine := &sessionEngine{}
	enforced := Enforce(opts, Enforcement{Engine: engine, Hosts: sessionImageHosts()})
	opts.Threads, opts.controls = enforced.Threads, enforced.controls
	s, c := start(t, opts)
	added, err := c.AddProject(context.Background(), request(clone))
	must(t, err)
	s.cfg = s.current()
	writeCharter(t, c, added.Project.ID, "1. Keep changes small.\n")
	resp, err := c.OpenSession(context.Background(), SessionOpenRequest{Project: added.Project.ID, Profile: "default"})
	must(t, err)
	return s, c, opts, added.Project.ID, resp.Session.Workstream, engine, clone
}

// sessionWorktreePath returns the directory a session's worktree lives
// under, the same layout newSessionFixture's tests already rely on.
func sessionWorktreePath(root string, project config.ProjectID, stream config.WorkstreamID) string {
	return filepath.Join(root, sessionsDirectory, string(project), string(stream))
}

// TestSessionChangesReadsUncommittedAndCommittedChanges covers spec#13: the
// owner reads the worktree's uncommitted changes relative to its head, and
// the commits on the session branch relative to the base commit.
func TestSessionChangesReadsUncommittedAndCommittedChanges(t *testing.T) {
	t.Parallel()
	s, c, _, project, stream, _, _ := newSessionControlsFixture(t)
	worktree := sessionWorktreePath(s.cfg.Root.String(), project, stream)
	empty, err := c.SessionChanges(context.Background(), stream)
	must(t, err)
	if empty.Changes != "" || len(empty.Commits) != 0 {
		t.Fatalf("a fresh session's changes: %+v", empty)
	}
	must(t, os.WriteFile(filepath.Join(worktree, "note.txt"), []byte("first draft\n"), 0600))
	dirty, err := c.SessionChanges(context.Background(), stream)
	must(t, err)
	if !strings.Contains(dirty.Changes, "note.txt") || !strings.Contains(dirty.Changes, "first draft") || len(dirty.Commits) != 0 {
		t.Fatalf("uncommitted changes: %+v", dirty)
	}
	committed, err := c.SessionCommit(context.Background(), stream, "add a note", "changes-key-1")
	must(t, err)
	must(t, os.WriteFile(filepath.Join(worktree, "second.txt"), []byte("second draft\n"), 0600))
	after, err := c.SessionChanges(context.Background(), stream)
	must(t, err)
	if after.Head != committed.Commit || !strings.Contains(after.Changes, "second.txt") || strings.Contains(after.Changes, "first draft") {
		t.Fatalf("changes after a commit: %+v", after)
	}
	if len(after.Commits) != 1 || after.Commits[0].Commit != committed.Commit || after.Commits[0].Message != "add a note" {
		t.Fatalf("committed changes: %+v", after.Commits)
	}
}

// TestSessionCommitCommitsAndThreadEntryReachesNextTurn covers spec#14 and
// the second half of plan#sess-controls acceptance item 2: the commit moves
// the session head, leaves the worktree clean, and the service entry it
// adds reaches the session's next turn.
func TestSessionCommitCommitsAndThreadEntryReachesNextTurn(t *testing.T) {
	t.Parallel()
	s, c, _, project, stream, engine, clone := newSessionControlsFixture(t)
	home := filepath.Dir(clone)
	worktree := sessionWorktreePath(s.cfg.Root.String(), project, stream)
	base := strings.TrimSpace(demoGit(t, home, "-C", worktree, "rev-parse", "HEAD"))
	must(t, os.WriteFile(filepath.Join(worktree, "note.txt"), []byte("hi\n"), 0600))
	resp, err := c.SessionCommit(context.Background(), stream, "add a note", "commit-key-1")
	must(t, err)
	if resp.Commit == "" || resp.Commit == base {
		t.Fatalf("commit response: %+v", resp)
	}
	if status := strings.TrimSpace(demoGit(t, home, "-C", worktree, "status", "--porcelain")); status != "" {
		t.Fatalf("worktree after commit:\n%s", status)
	}
	tip := strings.TrimSpace(demoGit(t, home, "-C", worktree, "rev-parse", "HEAD"))
	if tip != resp.Commit {
		t.Fatalf("session head is %s, want %s", tip, resp.Commit)
	}
	var prompt string
	engine.Turn = func(_ context.Context, req agent.Request) (*agent.Result, error) {
		prompt = req.Prompt
		return &agent.Result{ClaudeID: "model-session-1", ResultText: "ok"}, nil
	}
	_, err = c.SessionMessage(context.Background(), stream, "what did I commit?")
	must(t, err)
	awaitSession(t, c, stream, func(v SessionView) bool { return v.State == trace.SessionIdle })
	if !strings.Contains(prompt, resp.Commit) || !strings.Contains(prompt, "what did I commit?") {
		t.Fatalf("next turn's prompt lacks the commit entry:\n%s", prompt)
	}
}

// headOf reads a worktree's current commit.
func headOf(t *testing.T, home, dir string) string {
	t.Helper()
	return strings.TrimSpace(demoGit(t, home, "-C", dir, "rev-parse", "HEAD"))
}

// sessionEntryCount counts the session's recorded service entries.
func sessionEntryCount(t *testing.T, s *Service, stream config.WorkstreamID) int {
	t.Helper()
	entries, err := sessionEntriesSince(s.sole().repository, stream, time.Time{})
	must(t, err)
	return len(entries)
}

// TestSessionCommitRefusalsRecordNothing covers spec#15: a commit is
// refused while a turn runs, on an empty message, with no changes, and
// after the session has ended, and each refusal records nothing.
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
		must(t, err)
		select {
		case <-entered:
		case <-time.After(demoTimeout):
			t.Fatal("turn did not start")
		}
		before := headOf(t, home, worktree)
		must(t, os.WriteFile(filepath.Join(worktree, "note.txt"), []byte("hi\n"), 0600))
		_, err = c.SessionCommit(context.Background(), stream, "message", "key-busy")
		assertCode(t, err, Conflict)
		close(hold)
		awaitSession(t, c, stream, func(v SessionView) bool { return v.State == trace.SessionIdle })
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
		must(t, os.WriteFile(filepath.Join(worktree, "note.txt"), []byte("hi\n"), 0600))
		_, err := c.SessionCommit(context.Background(), stream, "   ", "key-empty")
		assertCode(t, err, Validation)
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
		assertCode(t, err, Validation)
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
		must(t, err)
		_, err = c.SessionCommit(context.Background(), stream, "message", "key-ended")
		assertCode(t, err, Conflict)
		if n := sessionEntryCount(t, s, stream); n != 0 {
			t.Fatalf("a refused commit recorded %d entries", n)
		}
		_ = before
	})
}

// TestSessionCommitRetryWithSameRequestKeyIsIdempotentAcrossRestart covers
// spec#16: a commit request retried with the same request key, including
// across a restart, produces exactly one commit.
func TestSessionCommitRetryWithSameRequestKeyIsIdempotentAcrossRestart(t *testing.T) {
	t.Parallel()
	s, c, opts, project, stream, _, clone := newSessionControlsFixture(t)
	home := filepath.Dir(clone)
	worktree := sessionWorktreePath(s.cfg.Root.String(), project, stream)
	base := headOf(t, home, worktree)
	must(t, os.WriteFile(filepath.Join(worktree, "note.txt"), []byte("hi\n"), 0600))

	first, err := c.SessionCommit(context.Background(), stream, "add a note", "same-key")
	must(t, err)

	// A retry before any restart finds the same commit.
	again, err := c.SessionCommit(context.Background(), stream, "add a note", "same-key")
	must(t, err)
	if again.Commit != first.Commit {
		t.Fatalf("a retry before a restart made a second commit: %s, want %s", again.Commit, first.Commit)
	}

	// Restart the service on the same root and clone, then retry once more
	// with the same request key.
	must(t, s.Close())
	s, c = start(t, opts)
	afterRestart, err := c.SessionCommit(context.Background(), stream, "add a note", "same-key")
	must(t, err)
	if afterRestart.Commit != first.Commit {
		t.Fatalf("a retry across a restart made a second commit: %s, want %s", afterRestart.Commit, first.Commit)
	}
	g := sessionGit(s.cfg)
	messages, err := g.Log(context.Background(), base, first.Commit)
	must(t, err)
	if len(messages) != 1 {
		t.Fatalf("commits on the session branch: %+v, want exactly one", messages)
	}
	if n := sessionEntryCount(t, s, stream); n != 1 {
		t.Fatalf("session entries after retries: %d, want exactly one", n)
	}
}

// TestSessionCommitRetryAfterACrashBetweenTheCommitAndItsRecordStillRecords
// covers spec#16's "including across a restart": a retry whose git commit
// was already made under the request key, but whose thread entry never
// got recorded because the service was cut short in between, still records
// the entry instead of silently leaving it missing, the way landingCommit's
// commit detection still lets land call record (landing.go).
func TestSessionCommitRetryAfterACrashBetweenTheCommitAndItsRecordStillRecords(t *testing.T) {
	t.Parallel()
	s, c, project, stream, _, clone := newSessionChatFixture(t)
	home := filepath.Dir(clone)
	worktree := sessionWorktreePath(s.cfg.Root.String(), project, stream)
	g := sessionGit(s.cfg)
	wt, found, err := g.Workspace(context.Background(), string(stream))
	must(t, err)
	if !found {
		t.Fatal("the session has no worktree")
	}
	must(t, os.WriteFile(filepath.Join(worktree, "note.txt"), []byte("hi\n"), 0600))
	made, _, err := g.CommitChanges(context.Background(), wt, sessionCommitMessage("add a note", "crash-key"), s.now())
	must(t, err)
	if n := sessionEntryCount(t, s, stream); n != 0 {
		t.Fatalf("a direct commit recorded %d entries, want none", n)
	}

	resp, err := c.SessionCommit(context.Background(), stream, "add a note", "crash-key")
	must(t, err)
	if resp.Commit != made {
		t.Fatalf("retry after a crash made a second commit: %s, want %s", resp.Commit, made)
	}
	if tip := headOf(t, home, worktree); tip != made {
		t.Fatalf("session head moved: %s, want %s", tip, made)
	}
	if n := sessionEntryCount(t, s, stream); n != 1 {
		t.Fatalf("session entries after the recovering retry: %d, want exactly one", n)
	}
	entries, err := sessionEntriesSince(s.sole().repository, stream, time.Time{})
	must(t, err)
	if !strings.Contains(entries[0].Text, made) {
		t.Fatalf("the recovered entry does not name the commit: %q", entries[0].Text)
	}

	// A further retry with the same key still finds exactly one commit and
	// entry.
	again, err := c.SessionCommit(context.Background(), stream, "add a note", "crash-key")
	must(t, err)
	if again.Commit != made {
		t.Fatalf("a second retry made a second commit: %s, want %s", again.Commit, made)
	}
	if n := sessionEntryCount(t, s, stream); n != 1 {
		t.Fatalf("session entries after a second retry: %d, want exactly one", n)
	}
}

// checkCapture is a fake ReviewChecks that records the directory it ran in
// and whether it held a named file, standing in for the real export of a
// checked commit.
type checkCapture struct {
	links  []string
	file   string
	found  bool
	result CheckResult
}

func (c *checkCapture) List(context.Context, string, api.CheckIdentity) ([]string, error) {
	return c.links, nil
}

func (c *checkCapture) Check(_ context.Context, dir string, _ api.CheckIdentity, _ []string) (CheckResult, error) {
	if c.file != "" {
		_, err := os.Stat(filepath.Join(dir, c.file))
		c.found = err == nil
	}
	if c.result.Output == "" {
		return CheckResult{Output: passedReport}, nil
	}
	return c.result, nil
}

// TestSessionCheckRunsFullUnderJEVDisabled covers spec#17's full-check path
// and that the record and thread entry name the checked commit.
func TestSessionCheckRunsFullUnderJEVDisabled(t *testing.T) {
	t.Parallel()
	s, c, _, project, stream, _, clone := newSessionControlsFixture(t)
	home := filepath.Dir(clone)
	head := headOf(t, home, sessionWorktreePath(s.cfg.Root.String(), project, stream))
	run, err := c.SessionCheck(context.Background(), stream)
	must(t, err)
	if run.Kind != "full" || run.Status != ChecksPassed || run.ExitCode != 0 || run.Commit != head || run.Run != 1 {
		t.Fatalf("check run: %+v, want full/passed on %s", run, head)
	}
	if n := sessionEntryCount(t, s, stream); n != 1 {
		t.Fatalf("session entries after a check: %d, want exactly one", n)
	}
	entries, err := sessionEntriesSince(s.sole().repository, stream, time.Time{})
	must(t, err)
	if !strings.Contains(entries[0].Text, head) {
		t.Fatalf("the check's thread entry does not name the commit: %q", entries[0].Text)
	}
	runs, err := sessionCheckRuns(s.sole().repository, stream)
	must(t, err)
	if len(runs) != 1 || runs[0].Commit != head || runs[0].Status != ChecksPassed {
		t.Fatalf("recorded check runs: %+v", runs)
	}
}

// TestSessionCheckRunsSmartSelectionUnderJEV covers spec#17's smart-check
// path: with JEV enabled, only the checks the existing smart-check
// selection picks run, and the run is recorded as smart with those checks.
func TestSessionCheckRunsSmartSelectionUnderJEV(t *testing.T) {
	t.Parallel()
	s, c, _, _, stream, _, _ := newSessionControlsFixture(t)
	links := []string{
		"dag+check://release/version-round-trip",
		"dag+check://go/packages/tests/test?go-package=internal/web&go-test=TestPage",
	}
	checks := &checkCapture{links: links}
	service.InspectForTest(s.core()).SetReviewChecks(checks)
	// The existing selection narrows and orders its own candidates
	// (checkselect.Candidates sorts them), so "check-0" does not necessarily
	// key the first of links: sorted alphabetically, the go-test link below
	// links[0]'s "release" link comes first, so "check-0" keys links[1] and
	// "check-1" keys links[0]. The high-probability answer goes on
	// "check-1" so the candidate the selection actually builds for links[0]
	// is the one selected.
	p := &jevtest.Provider{Results: []jevtest.Result{{Response: systemone.Response{Model: "jev-1.0", Answers: map[string]systemone.Answer{
		"check-0": jevtest.Noul(0.02), "check-1": jevtest.Noul(0.92)}}}}}
	boostJev(s, p)
	run, err := c.SessionCheck(context.Background(), stream)
	must(t, err)
	if run.Kind != "smart" || !slices.Equal(run.Checks, []string{links[0]}) || run.Status != ChecksPassed {
		t.Fatalf("check run: %+v", run)
	}
}

// TestSessionCheckExcludesUncommittedChanges covers spec#17's "uncommitted
// changes are not part of the checked commit": a file only in the worktree,
// never committed, is absent from the export the check runs on.
func TestSessionCheckExcludesUncommittedChanges(t *testing.T) {
	t.Parallel()
	s, c, _, project, stream, _, _ := newSessionControlsFixture(t)
	worktree := sessionWorktreePath(s.cfg.Root.String(), project, stream)
	must(t, os.WriteFile(filepath.Join(worktree, "uncommitted.txt"), []byte("never committed\n"), 0600))
	checks := &checkCapture{file: "uncommitted.txt"}
	service.InspectForTest(s.core()).SetReviewChecks(checks)
	_, err := c.SessionCheck(context.Background(), stream)
	must(t, err)
	if checks.found {
		t.Fatal("the checked export included an uncommitted file")
	}
}

// TestSessionCheckWhileOneIsRunningIsRefused covers spec#17: only one check
// run per session can be in progress at a time.
func TestSessionCheckWhileOneIsRunningIsRefused(t *testing.T) {
	t.Parallel()
	s, c, _, _, stream, _, _ := newSessionControlsFixture(t)
	hold, entered := make(chan struct{}), make(chan struct{})
	service.InspectForTest(s.core()).SetReviewChecks(checkFunc(func(ctx context.Context, dir string) (CheckResult, error) {
		close(entered)
		select {
		case <-hold:
		case <-ctx.Done():
		}
		return CheckResult{Output: passedReport}, nil
	}))
	done := make(chan error, 1)
	go func() {
		_, err := c.SessionCheck(context.Background(), stream)
		done <- err
	}()
	select {
	case <-entered:
	case <-time.After(demoTimeout):
		t.Fatal("first check did not start")
	}
	_, err := c.SessionCheck(context.Background(), stream)
	assertCode(t, err, Conflict)
	close(hold)
	if err := <-done; err != nil {
		t.Fatalf("the first check failed: %v", err)
	}
}

// TestSessionPushAgainstLocalBareRemote covers spec#18: push creates or
// advances the session branch on the project's remote, leaves the default
// branch untouched, is refused without forcing when the remote branch has
// commits not on the session branch, and opens no pull request.
func TestSessionPushAgainstLocalBareRemote(t *testing.T) {
	t.Parallel()
	s, c, _, project, stream, _, clone := newSessionControlsFixture(t)
	home := filepath.Dir(clone)
	worktree := sessionWorktreePath(s.cfg.Root.String(), project, stream)
	bare := filepath.Join(home, "remotes", "dagger", "dagger.git")
	mainBefore := strings.TrimSpace(demoGit(t, home, "-C", bare, "rev-parse", "main"))

	must(t, os.WriteFile(filepath.Join(worktree, "note.txt"), []byte("hi\n"), 0600))
	committed, err := c.SessionCommit(context.Background(), stream, "add a note", "push-key")
	must(t, err)

	resp, err := c.SessionPush(context.Background(), stream)
	must(t, err)
	if resp.Remote != "upstream" || resp.Commit != committed.Commit {
		t.Fatalf("push response: %+v, want commit %s", resp, committed.Commit)
	}
	if got := strings.TrimSpace(demoGit(t, home, "-C", bare, "rev-parse", resp.Branch)); got != committed.Commit {
		t.Fatalf("remote branch %s is at %s, want %s", resp.Branch, got, committed.Commit)
	}
	if got := strings.TrimSpace(demoGit(t, home, "-C", bare, "rev-parse", "main")); got != mainBefore {
		t.Fatalf("the default branch moved: %s, want %s", got, mainBefore)
	}

	// Advance it again: pushing after a second commit still succeeds and no
	// pull request is opened (nothing in this fixture can open one).
	must(t, os.WriteFile(filepath.Join(worktree, "second.txt"), []byte("more\n"), 0600))
	second, err := c.SessionCommit(context.Background(), stream, "second note", "push-key-2")
	must(t, err)
	resp2, err := c.SessionPush(context.Background(), stream)
	must(t, err)
	if resp2.Commit != second.Commit {
		t.Fatalf("second push: %+v, want %s", resp2, second.Commit)
	}

	// Make the remote branch hold a commit the session branch does not.
	scratch := filepath.Join(home, "scratch-push")
	demoGit(t, home, "clone", "--quiet", bare, scratch)
	demoGit(t, home, "-C", scratch, "checkout", "-q", resp.Branch)
	must(t, os.WriteFile(filepath.Join(scratch, "diverged.txt"), []byte("not on the session branch\n"), 0600))
	demoGit(t, home, "-C", scratch, "add", "diverged.txt")
	demoGit(t, home, "-C", scratch, "-c", "user.name=Other", "-c", "user.email=other@example.invalid", "commit", "-qm", "diverged")
	demoGit(t, home, "-C", scratch, "push", "--quiet", "origin", resp.Branch)
	diverged := strings.TrimSpace(demoGit(t, home, "-C", bare, "rev-parse", resp.Branch))

	_, err = c.SessionPush(context.Background(), stream)
	assertCode(t, err, Conflict)
	if got := strings.TrimSpace(demoGit(t, home, "-C", bare, "rev-parse", resp.Branch)); got != diverged {
		t.Fatalf("a refused push moved the remote branch: %s, want %s", got, diverged)
	}
}

// TestSessionEndRefusesWhileRunningThenReleasesStateAndRefusesControls
// covers spec#19: ending is refused while a turn runs; once ended, the
// worktree and model-session state are released, the branch, commits,
// thread and trace stay readable, the session lists as ended, and
// messages, commits, checks and pushes are refused.
func TestSessionEndRefusesWhileRunningThenReleasesStateAndRefusesControls(t *testing.T) {
	t.Parallel()
	s, c, project, stream, engine, _ := newSessionChatFixture(t)
	worktree := sessionWorktreePath(s.cfg.Root.String(), project, stream)
	modelDir := runtime.ModelSessionDirectory(s.cfg.Root.String(), project, stream)

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
	must(t, err)
	select {
	case <-entered:
	case <-time.After(demoTimeout):
		t.Fatal("turn did not start")
	}
	_, err = c.SessionEnd(context.Background(), stream)
	assertCode(t, err, Conflict)
	close(hold)
	awaitSession(t, c, stream, func(v SessionView) bool { return v.State == trace.SessionIdle })

	must(t, os.WriteFile(filepath.Join(worktree, "note.txt"), []byte("hi\n"), 0600))
	committed, err := c.SessionCommit(context.Background(), stream, "add a note", "end-key")
	must(t, err)

	resp, err := c.SessionEnd(context.Background(), stream)
	must(t, err)
	if resp.Session.State != trace.SessionEnded {
		t.Fatalf("ended session view: %+v", resp.Session)
	}
	if _, err := os.Stat(worktree); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("worktree after end: %v", err)
	}
	if _, err := os.Stat(modelDir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("model-session state after end: %v", err)
	}
	listed, err := c.Status(context.Background(), stream)
	must(t, err)
	if listed.State == nil || *listed.State != trace.SessionEnded {
		t.Fatalf("listed state after end: %+v", listed.State)
	}
	changes, err := c.SessionChanges(context.Background(), stream)
	must(t, err)
	if len(changes.Commits) != 1 || changes.Commits[0].Commit != committed.Commit {
		t.Fatalf("commits after end: %+v", changes.Commits)
	}
	th, err := s.sole().repository.Thread(stream, sessionAgentID)
	if err != nil || len(th.Turns) != 1 {
		t.Fatalf("thread after end: %+v, %v", th, err)
	}

	if _, err := c.SessionMessage(context.Background(), stream, "hello"); err == nil {
		t.Fatal("a message was accepted after end")
	} else {
		assertCode(t, err, Conflict)
	}
	if _, err := c.SessionCommit(context.Background(), stream, "message", "end-key-2"); err == nil {
		t.Fatal("a commit was accepted after end")
	} else {
		assertCode(t, err, Conflict)
	}
	if _, err := c.SessionCheck(context.Background(), stream); err == nil {
		t.Fatal("a check was accepted after end")
	} else {
		assertCode(t, err, Conflict)
	}
	if _, err := c.SessionPush(context.Background(), stream); err == nil {
		t.Fatal("a push was accepted after end")
	} else {
		assertCode(t, err, Conflict)
	}

	// Ending an already-ended session retries releasing state idempotently.
	again, err := c.SessionEnd(context.Background(), stream)
	must(t, err)
	if again.Session.State != trace.SessionEnded {
		t.Fatalf("re-ending an ended session: %+v", again.Session)
	}
}

// TestSessionCommitAndEndRefuseWhileAMessageIsBeingStarted covers spec#15
// and spec#19's guard against sessionCommit or sessionEnd interleaving with
// a message post that is already past its own busy check: both take the
// same per-session lock sessionMessage holds from that check through
// recording the turn it starts, so a message in flight (simulated here by
// holding the lock directly) refuses both rather than letting a commit
// write the worktree, or an end move the session to ended, in between.
func TestSessionCommitAndEndRefuseWhileAMessageIsBeingStarted(t *testing.T) {
	t.Parallel()
	s, c, _, _, stream, _, _ := newSessionControlsFixture(t)
	lock := sessionLock(s.sole().repository, stream)
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
	assertCode(t, err, Conflict)
	_, err = c.SessionEnd(context.Background(), stream)
	assertCode(t, err, Conflict)
}

// TestSessionCommitAndCheckRefuseOnceEndHasBegun covers sessionEnd holding
// sessionLock across its whole transition and the worktree and
// model-session releases that follow it, not just the transition: while
// end's critical section is in progress (simulated here, like
// TestSessionCommitAndEndRefuseWhileAMessageIsBeingStarted, by holding the
// lock directly so the exact moment of release is controlled), a commit or
// a check started concurrently is refused with Conflict and records
// nothing, rather than later acquiring the lock once end has released it
// and acting on a worktree end has already torn down.
func TestSessionCommitAndCheckRefuseOnceEndHasBegun(t *testing.T) {
	t.Parallel()
	s, c, project, stream, _, _ := newSessionChatFixture(t)
	worktree := sessionWorktreePath(s.cfg.Root.String(), project, stream)
	must(t, os.WriteFile(filepath.Join(worktree, "note.txt"), []byte("hi\n"), 0600))

	lock := sessionLock(s.sole().repository, stream)
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
	assertCode(t, err, Conflict)
	if n := sessionEntryCount(t, s, stream); n != 0 {
		t.Fatalf("a commit refused while end is in progress recorded %d entries", n)
	}

	_, err = c.SessionCheck(context.Background(), stream)
	assertCode(t, err, Conflict)
	runs, err := sessionCheckRuns(s.sole().repository, stream)
	must(t, err)
	if len(runs) != 0 {
		t.Fatalf("a check refused while end is in progress recorded %d runs", len(runs))
	}
}

// TestSessionCheckOutlivesDefaultWriteTimeout covers a session check's write
// deadline: a check runs the project's own check command inline and can
// take well over the server's default WriteTimeout, so that alone must not
// cut the owner's HTTP response short even though the run completes and is
// recorded, the same contract TestProjectAddOutlivesDefaultWriteTimeout
// checks for project add.
func TestSessionCheckOutlivesDefaultWriteTimeout(t *testing.T) {
	t.Parallel()
	opts, clone := projectFixture(t)
	home := filepath.Dir(clone)
	must(t, os.WriteFile(filepath.Join(clone, "README.md"), []byte("hi\n"), 0600))
	demoGit(t, home, "-C", clone, "add", "README.md")
	demoGit(t, home, "-C", clone, "-c", "user.name=Owner", "-c", "user.email=owner@example.invalid", "commit", "-qm", "base")
	demoGit(t, home, "-C", clone, "branch", "-M", "main")
	bare := filepath.Join(home, "remotes", "dagger", "dagger.git")
	must(t, os.MkdirAll(filepath.Dir(bare), 0700))
	demoGit(t, home, "clone", "--quiet", "--bare", "--no-hardlinks", clone, bare)
	demoGit(t, home, "-C", clone, "remote", "add", "upstream", bare)
	s, c := start(t, opts)
	added, err := c.AddProject(context.Background(), request(clone))
	must(t, err)
	writeCharter(t, c, added.Project.ID, "1. Keep changes small.\n")
	resp, err := c.OpenSession(context.Background(), SessionOpenRequest{Project: added.Project.ID, Profile: "default"})
	must(t, err)
	stream := resp.Session.Workstream

	// Only the check itself needs to outlive a short write timeout, so the
	// server restarts with one now, the same restart idiom
	// TestSessionCommitRetryWithSameRequestKeyIsIdempotentAcrossRestart uses,
	// rather than lowering opts.WriteTimeout before the setup above, which
	// would make the setup calls themselves (not the check this test is
	// about) racily sensitive to the same short deadline.
	must(t, s.Close())
	opts.WriteTimeout = 20 * time.Millisecond
	s, c = start(t, opts)
	// The contract is that the write timeout alone does not cut the call
	// short; the client budget here is generous so that a check slowed by
	// parallel load, not the write timeout, cannot fail the test.
	c.SetSessionCheckTimeoutForTest(demoTimeout)
	service.InspectForTest(s.core()).SetReviewChecks(checkFunc(func(context.Context, string) (CheckResult, error) {
		time.Sleep(100 * time.Millisecond)
		return CheckResult{Output: passedReport}, nil
	}))
	run, err := c.SessionCheck(context.Background(), stream)
	if err != nil {
		t.Fatalf("session check after the write timeout: %v", err)
	}
	if run.Status != ChecksPassed {
		t.Fatalf("check run: %+v", run)
	}
}

func TestSessionCheckRequiresReadableDiff(t *testing.T) {
	t.Parallel()
	s, c, _, project, stream, _, clone := newSessionControlsFixture(t)
	home := filepath.Dir(clone)
	baseTree := strings.TrimSpace(demoGit(t, home, "-C", clone, "rev-parse", "HEAD^{tree}"))
	worktree := sessionWorktreePath(s.cfg.Root.String(), project, stream)
	must(t, os.WriteFile(filepath.Join(worktree, "note.txt"), []byte("new tree\n"), 0600))
	commit, err := c.SessionCommit(context.Background(), stream, "add a note", "diff-check")
	must(t, err)
	// The head still exports, but its base tree is unavailable for selection.
	must(t, os.Remove(filepath.Join(clone, ".git", "objects", baseTree[:2], baseTree[2:])))
	must(t, sessionGit(s.current().For(project)).Export(context.Background(), commit.Commit, t.TempDir()))
	checks := &fakeChecks{Links: []string{"dag+check://go/test"}, Outcomes: passing()}
	service.InspectForTest(s.core()).SetReviewChecks(checks)
	provider := &jevtest.Provider{}
	boostJev(s, provider)
	_, err = c.SessionCheck(context.Background(), stream)
	assertCode(t, err, Internal)
	if !strings.Contains(err.Error(), "cannot diff the session's base and head") {
		t.Fatalf("diff failure: %v", err)
	}
	if len(checks.Runs()) != 0 || len(provider.Requests()) != 0 {
		t.Fatal("unreadable change reached selection or the check runner")
	}
	runs, err := sessionCheckRuns(s.sole().repository, stream)
	must(t, err)
	if len(runs) != 0 {
		t.Fatalf("unreadable change recorded a check run: %+v", runs)
	}
}
