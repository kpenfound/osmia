package service

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kpenfound/busybees/core/agent"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/isolation"
	"github.com/kpenfound/osmia/internal/runtime"
	"github.com/kpenfound/osmia/internal/thread"
	"github.com/kpenfound/osmia/internal/trace"
)

// interruptedEdits are what a mason turn does in its view before it is cut
// short: it changes a file of the clone and adds one.
var interruptedEdits = map[string]string{
	trackedFile:              "package trace\n\n// changed before the interruption\n",
	"internal/trace/kept.go": "package trace\n\n// kept\n",
}

// keptOnJujutsu is what a continuation's prompt says of interruptedEdits on
// Jujutsu workspaces, where the turn's start is recorded.
const keptOnJujutsu = "Your workspace includes the files left by %s, which changed internal/trace/git.go, internal/trace/kept.go."

func writeEdits(dir string) error {
	for name, content := range interruptedEdits {
		if err := os.MkdirAll(filepath.Dir(filepath.Join(dir, name)), 0755); err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0644); err != nil {
			return err
		}
	}
	return nil
}

// holdsEdits reports whether read returns every file of interruptedEdits.
func holdsEdits(read func(name string) (string, error)) bool {
	for name, content := range interruptedEdits {
		if got, err := read(name); err != nil || got != content {
			return false
		}
	}
	return true
}

// inDirectory reads files under dir.
func inDirectory(dir string) func(string) (string, error) {
	return func(name string) (string, error) {
		data, err := os.ReadFile(filepath.Join(dir, name))
		return string(data), err
	}
}

// inCommit reads files of a commit of the fixture's clone.
func inCommit(f *shedFixture, commit string) func(string) (string, error) {
	return func(name string) (string, error) {
		cmd := exec.Command("git", "-C", f.clone, "show", commit+":"+name)
		cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + filepath.Dir(f.clone), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=" + os.DevNull}
		out, err := cmd.Output()
		return string(out), err
	}
}

// checkReviewedEdits checks that the unit went to review with a candidate
// holding interruptedEdits.
func checkReviewedEdits(t *testing.T, f *shedFixture, stream config.WorkstreamID, unit string) {
	t.Helper()
	f.awaitUnit(t, stream, unit, UnitReviewing)
	_, report := latestReport(t, f.repository(), stream, unit)
	if report.Candidate == "" || !holdsEdits(inCommit(f, report.Candidate)) {
		t.Fatalf("unit %s's candidate %q does not hold the interrupted turn's edits", unit, report.Candidate)
	}
}

// awaitFurtherPasses blocks until n further reconciliation passes have
// completed since it was called, counted by passed, which an Options'
// schedulePassed hook increments once per pass's schedule stage. It gives a
// readiness signal for "nothing more happens while the pause holds" checks
// in place of a sleep.
func awaitFurtherPasses(t *testing.T, passed *atomic.Int64, n int64) {
	t.Helper()
	target := passed.Load() + n
	deadline := time.Now().Add(demoTimeout)
	for passed.Load() < target {
		if time.Now().After(deadline) {
			t.Fatalf("reconciliation passes: %d, want at least %d", passed.Load(), target)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// A mason turn a hard pause stops keeps the files it changed in its unit's
// workspace, and its unit stays implementing: the turn is not retried and
// nothing continues it while the pause holds. Lifting the pause continues
// the unit on the same thread by resuming the stopped session, and the
// continuation's prompt says the files were kept. On Jujutsu workspaces it
// names the files the stopped turn changed; on git worktrees it does not.
// The continuation's view holds the edits, and its candidate carries them.
func TestStoppedMasonTurnKeepsItsEditsAndTheContinuationNamesThem(t *testing.T) {
	t.Parallel()
	for _, backend := range []string{config.WorkspacesGit, config.WorkspacesJujutsu} {
		t.Run(backend, func(t *testing.T) {
			t.Parallel()
			var passed atomic.Int64
			f, _ := newMasonFixtureWith(t, backend, "masons = 1\n", validPlan, "", func(opts *Options) {
				opts.schedulePassed = func() { passed.Add(1) }
			})
			defer func() { f.stop(t) }()
			recoverTurn := masonAgent("resume") + "-recover-1"
			entered := make(chan struct{})
			var mu sync.Mutex
			var continuation, resumed string
			var sawEdits bool
			f.engine.mu.Lock()
			f.engine.resume = func(coreadapter.Profile, coreadapter.Profile, coreadapter.BackendSession) error { return nil }
			f.engine.turns[masonTurnID("resume")] = func(ctx context.Context, req agent.Request, _ *agent.Turn, _ *mcp.ClientSession) (*agent.Result, error) {
				if err := writeEdits(req.Workspace.Directory()); err != nil {
					return nil, err
				}
				close(entered)
				<-ctx.Done()
				return &agent.Result{ClaudeID: "session-stopped", ResultText: "Half built", SessionDir: req.SessionDir, NumTurns: 1, IsError: true, Signal: 15}, nil
			}
			f.engine.turns[recoverTurn] = func(ctx context.Context, req agent.Request, _ *agent.Turn, tools *mcp.ClientSession) (*agent.Result, error) {
				mu.Lock()
				continuation, resumed, sawEdits = req.Prompt, req.ResumeID, holdsEdits(inDirectory(req.Workspace.Directory()))
				mu.Unlock()
				if err := reportDone("Continued")(ctx, req, tools); err != nil {
					return nil, err
				}
				return &agent.Result{ClaudeID: "session-" + req.Name, ResultText: "Continued", SessionDir: req.SessionDir, NumTurns: 1}, nil
			}
			f.engine.mu.Unlock()

			stream := f.seedBuilding(t, "stopped-"+backend, validPlan)
			if got := backendOf(t, f, stream); got != backend {
				t.Fatalf("the workstream is on %s", got)
			}
			select {
			case <-entered:
			case <-time.After(demoTimeout):
				t.Fatal("the mason did not start")
			}
			target := runtime.Target{Scope: "workstream", Project: f.project, Workstream: stream}
			mutation(t, f.c, "PUT", "pause", PauseRequest{Target: target, Mode: "hard", Reason: "Stop the mason", Source: "owner"})
			stopped := f.awaitCompleted(t, stream, masonAgent("resume"), masonTurnID("resume"))
			checkPauseStop(t, stopped, "workstream", "Stop the mason")
			awaitFurtherPasses(t, &passed, 2)
			workspace := filepath.Join(f.opts.Config.Root, unitsDirectory, string(f.project), string(stream), "resume")
			if !holdsEdits(inDirectory(workspace)) {
				t.Fatal("the stopped turn's edits are not in the unit's workspace")
			}
			if state, err := f.unitState(stream, "resume"); err != nil || state != UnitImplementing {
				t.Fatalf("stopped unit is %q: %v", state, err)
			}
			ops, err := f.repository().Operations(stream)
			must(t, err)
			for _, op := range ops {
				in, err := thread.DecodeTurn(op.Operation)
				if err != nil || in.Agent != masonAgent("resume") {
					continue
				}
				switch in.Turn {
				case masonTurnID("resume"):
					checkNoRetry(t, op, "interrupted")
				case recoverTurn:
					t.Fatal("the continuation was dispatched under the hard pause")
				}
			}
			f.engine.mu.Lock()
			ran := slices.Contains(f.engine.runs, recoverTurn)
			f.engine.mu.Unlock()
			if ran {
				t.Fatal("the continuation ran under the hard pause")
			}

			mutation(t, f.c, "DELETE", "pause", target)
			checkReviewedEdits(t, f, stream, "resume")
			next := f.awaitCompleted(t, stream, masonAgent("resume"), recoverTurn)
			if next.Request.ThreadID != stopped.Request.ThreadID || !strings.HasPrefix(next.Request.Prompt, stopped.Request.Prompt) {
				t.Fatalf("continuation request %+v", next.Request)
			}
			if len(next.Attempts) != 1 || next.Attempts[0].Path != "resume" || next.Attempts[0].SourceSession.ID != "session-stopped" || next.Attempts[0].SourceSequence != stopped.Sequence {
				t.Fatalf("the continuation did not resume the stopped session: %+v", next.Attempts)
			}
			want := "A hard pause stopped your last turn. Your workspace includes the files left by that turn. Continue from those files"
			if backend == config.WorkspacesJujutsu {
				want = "A hard pause stopped your last turn. " + strings.Replace(keptOnJujutsu, "%s", "that turn", 1) + " Continue from those files"
			}
			mu.Lock()
			defer mu.Unlock()
			if !strings.Contains(continuation, want) || !sawEdits || resumed != "session-stopped" {
				t.Fatalf("the continuation resuming %q saw the edits %t with prompt %q, want %q", resumed, sawEdits, continuation, want)
			}
		})
	}
}

// An attempt of a mason turn that follows one that timed out works on the
// files the timed-out attempt left. On Jujutsu workspaces its prompt names
// the files the earlier attempts changed; on git worktrees its prompt is the
// turn's own. The candidate carries the edits.
func TestTimedOutMasonAttemptKeepsItsEditsAndTheRetryNamesThem(t *testing.T) {
	t.Parallel()
	for _, backend := range []string{config.WorkspacesGit, config.WorkspacesJujutsu} {
		t.Run(backend, func(t *testing.T) {
			t.Parallel()
			f, _ := newMasonFixtureOn(t, backend, "masons = 1\n", validPlan, "")
			defer func() { f.stop(t) }()
			var mu sync.Mutex
			var prompts []string
			var sawEdits bool
			f.engine.mu.Lock()
			f.engine.turns[masonTurnID("resume")] = func(ctx context.Context, req agent.Request, _ *agent.Turn, tools *mcp.ClientSession) (*agent.Result, error) {
				mu.Lock()
				prompts = append(prompts, req.Prompt)
				first := len(prompts) == 1
				if !first {
					sawEdits = holdsEdits(inDirectory(req.Workspace.Directory()))
				}
				mu.Unlock()
				if first {
					if err := writeEdits(req.Workspace.Directory()); err != nil {
						return nil, err
					}
					return &agent.Result{ClaudeID: "session-timed-out", ResultText: "Half built", SessionDir: req.SessionDir, NumTurns: 1, IsError: true, TimedOut: true}, nil
				}
				if err := reportDone("Built")(ctx, req, tools); err != nil {
					return nil, err
				}
				return &agent.Result{ClaudeID: "session-" + req.Name, ResultText: "Built", SessionDir: req.SessionDir, NumTurns: 1}, nil
			}
			f.engine.mu.Unlock()

			stream := f.seedBuilding(t, "timed-out-"+backend, validPlan)
			if got := backendOf(t, f, stream); got != backend {
				t.Fatalf("the workstream is on %s", got)
			}
			checkReviewedEdits(t, f, stream, "resume")
			q := f.awaitCompleted(t, stream, masonAgent("resume"), masonTurnID("resume"))
			if len(q.Attempts) != 2 || q.Attempts[0].Result == nil || !q.Attempts[0].Result.TimedOut || q.Attempts[0].FailureClass != coreadapter.Infrastructure {
				t.Fatalf("the turn's attempts %+v", q.Attempts)
			}
			want := prompts[0]
			if backend == config.WorkspacesJujutsu {
				want += "\n\nAn earlier attempt at this turn timed out. " + strings.Replace(keptOnJujutsu, "%s", "the earlier attempts", 1) + " Continue from those files."
			}
			mu.Lock()
			defer mu.Unlock()
			if len(prompts) != 2 || prompts[1] != want || !sawEdits {
				t.Fatalf("the retry saw the edits %t with prompts %q, want %q", sawEdits, prompts, want)
			}
		})
	}
}

// A mason turn running when the service died leaves its edits in its view.
// On restart the mason controller copies them into the unit's workspace and
// queues one continuation whose prompt says what the turn left: on Jujutsu
// workspaces, which files it changed since the turn started, recorded as the
// turn was lent its workspace; on git worktrees, only that the files are
// there.
func TestMasonTurnInterruptedByARestartKeepsItsEditsAndTheContinuationNamesThem(t *testing.T) {
	t.Parallel()
	for _, backend := range []string{config.WorkspacesGit, config.WorkspacesJujutsu} {
		t.Run(backend, func(t *testing.T) {
			t.Parallel()
			f, _ := newMasonFixtureOn(t, backend, "masons = 1\n", validPlan, "")
			defer func() { f.stop(t) }()
			stream := f.seedBuildingPaused(t, runtime.Target{Scope: "factory"}, validPlan, "restart-"+backend)[0]
			if got := backendOf(t, f, stream); got != backend {
				t.Fatalf("the workstream is on %s", got)
			}
			f.stop(t)
			ctx := context.Background()
			repo, err := trace.Open(f.s.cfg.Root, f.s.cfg.Project)
			must(t, err)
			m := newMasonController(f.s, repo)
			b, found, err := m.read(stream)
			must(t, err)
			if !found {
				t.Fatal("building workstream missing")
			}
			if started, _, err := m.start(ctx, b, "resume"); err != nil || !started {
				t.Fatalf("the mason did not start: %v", err)
			}
			// The turn is lent its workspace as the thread runner lends it, and
			// the service dies while it writes into its view.
			units := newUnitWorkspaces(f.s.cfg, repo)
			scope := coreadapter.Scope{Project: string(f.project), Workstream: string(stream), Unit: "resume", Thread: masonAgent("resume"), Turn: masonTurnID("resume"), Role: masonRole}
			selection, err := units.selection(ctx, scope, coreadapter.ExecutionSettings{})
			must(t, err)
			w, _, found, err := units.find(ctx, stream, "resume")
			must(t, err)
			if !found {
				t.Fatal("unit workspace missing")
			}
			viewRoot := turnViews(f.s.cfg, f.project, stream, masonAgent("resume"), masonTurnID("resume"))
			must(t, os.MkdirAll(viewRoot, 0700))
			view, err := (isolation.Views{Directory: viewRoot}).Create(ctx, coreadapter.Workspace{Directory: w.Path, Access: coreadapter.ReadWrite}, selection.Paths)
			must(t, err)
			must(t, os.WriteFile(filepath.Join(viewRoot, "ready"), []byte(filepath.Base(view.Workspace().Directory)), 0600))
			must(t, writeEdits(view.Workspace().Directory))
			_, err = repo.ClaimTurn(ctx, stream, masonAgent("resume"), "crashed", filepath.Join(f.s.cfg.Root.String(), "threads", string(f.project), string(stream), masonAgent("resume"), masonTurnID("resume")), f.clock.Now())
			must(t, err)
			must(t, repo.Close())

			repo, err = trace.Open(f.s.cfg.Root, f.s.cfg.Project)
			must(t, err)
			defer repo.Close()
			m.repository = repo
			must(t, m.Pass(ctx))
			must(t, m.Pass(ctx))
			th, err := repo.Thread(stream, masonAgent("resume"))
			must(t, err)
			want := "The service stopped during your last turn. Your workspace includes the files left by that turn. Continue from those files"
			if backend == config.WorkspacesJujutsu {
				want = "The service stopped during your last turn. " + strings.Replace(keptOnJujutsu, "%s", "that turn", 1) + " Continue from those files"
			}
			if len(th.Turns) != 2 || th.Turns[0].Status() != "interrupted" || th.Turns[1].Request.TurnID != masonAgent("resume")+"-recover-1" || !strings.Contains(th.Turns[1].Request.Prompt, want) {
				t.Fatalf("recovered mason thread %+v, want a continuation saying %q", th.Turns, want)
			}
			if !holdsEdits(inDirectory(w.Path)) {
				t.Fatal("the interrupted turn's edits are not in the unit's workspace")
			}
		})
	}
}

// On Jujutsu workspaces, a landing that moves the feature branch after a
// hard pause stopped a mason mid-turn rebases the unit's workspace with the
// stopped turn's edits: its snapshot and the rebased commit hold them, and
// so does the workspace, beside what landed.
func TestJujutsuRebaseCarriesTheEditsOfAStoppedMasonTurn(t *testing.T) {
	t.Parallel()
	var passed atomic.Int64
	f, _ := newMasonFixtureWith(t, config.WorkspacesJujutsu, "masons = 1\n", validPlan, "", func(opts *Options) {
		opts.schedulePassed = func() { passed.Add(1) }
	})
	defer func() { f.stop(t) }()
	entered := make(chan struct{})
	f.engine.mu.Lock()
	f.engine.turns[masonTurnID("resume")] = func(ctx context.Context, req agent.Request, _ *agent.Turn, _ *mcp.ClientSession) (*agent.Result, error) {
		if err := writeEdits(req.Workspace.Directory()); err != nil {
			return nil, err
		}
		close(entered)
		<-ctx.Done()
		return &agent.Result{ClaudeID: "session-stopped", ResultText: "Half built", SessionDir: req.SessionDir, NumTurns: 1, IsError: true, Signal: 15}, nil
	}
	f.engine.mu.Unlock()
	stream := f.seedBuilding(t, "stopped-rebase", validPlan)
	select {
	case <-entered:
	case <-time.After(demoTimeout):
		t.Fatal("the mason did not start")
	}
	target := runtime.Target{Scope: "workstream", Project: f.project, Workstream: stream}
	mutation(t, f.c, "PUT", "pause", PauseRequest{Target: target, Mode: "hard", Reason: "Stop the mason", Source: "owner"})
	checkPauseStop(t, f.awaitCompleted(t, stream, masonAgent("resume"), masonTurnID("resume")), "workstream", "Stop the mason")
	awaitFurtherPasses(t, &passed, 2)
	f.stop(t)

	ctx := context.Background()
	repository, err := trace.Open(f.s.cfg.Root, f.s.cfg.Project)
	must(t, err)
	defer repository.Close()
	lands := &foreman{masons: newMasonController(f.s, repository)}
	if writing, err := lands.writing(stream, "resume", nil); err != nil || writing {
		t.Fatalf("the stopped mason still writes: %t %v", writing, err)
	}
	const landedFile = "internal/landed/landed.go"
	landed := moveFeature(t, f, stream, map[string]string{landedFile: "package landed\n"})
	must(t, lands.requestRebase(ctx, stream, "resume", landed))
	asked := rebaseOperations(t, repository, stream, "resume")
	if len(asked) != 1 {
		t.Fatalf("rebases asked: %+v", asked)
	}
	if result, err := (rebaser{lands}).Apply(ctx, asked[0].Operation); err != nil || result.Outcome != "succeeded" {
		t.Fatalf("rebase: %+v %v", result, err)
	}
	rebases := unitRebases(t, repository, stream, "resume")
	if len(rebases) != 1 || len(rebases[0].Conflicts) != 0 || rebases[0].Onto != landed {
		t.Fatalf("recorded rebases %+v", rebases)
	}
	for _, commit := range []string{rebases[0].Snapshot, rebases[0].Commit} {
		if !holdsEdits(inCommit(f, commit)) {
			t.Fatalf("commit %s does not hold the stopped turn's edits", commit)
		}
	}
	w, _, found, err := newUnitWorkspaces(f.s.cfg, repository).find(ctx, stream, "resume")
	must(t, err)
	if !found || !holdsEdits(inDirectory(w.Path)) {
		t.Fatalf("the rebased workspace %s does not hold the stopped turn's edits", w.Path)
	}
	if got, err := inDirectory(w.Path)(landedFile); err != nil || got != "package landed\n" {
		t.Fatalf("the rebased workspace's %s: %q %v", landedFile, got, err)
	}
}
