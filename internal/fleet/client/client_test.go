package client

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kpenfound/agentcore/agent"
	"github.com/kpenfound/agentcore/agent/agenttest/enforcertest"
	"github.com/kpenfound/osmia/internal/api"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/doctor"
	"github.com/kpenfound/osmia/internal/fleet"
	"github.com/kpenfound/osmia/internal/isolation"
)

// snapshot is every file and directory under dir with its content and whether
// it is executable.
func snapshot(t *testing.T, dir string) map[string]string {
	t.Helper()
	out := map[string]string{}
	must(t, filepath.WalkDir(dir, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(dir, path)
		info, err := entry.Info()
		if err != nil {
			return err
		}
		switch {
		case rel == ".":
		case entry.IsDir():
			out[rel] = "dir"
		case info.Mode().IsRegular():
			data, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			out[rel] = fmt.Sprintf("%v:%s", info.Mode().Perm()&0o100 != 0, data)
		default:
			out[rel] = "other"
		}
		return nil
	}))
	return out
}

func TestTheClientReportsWhichProfilesItCanRunAndReportsAgainAfterAReload(t *testing.T) {
	r := newRig(t, fleet.Options{})
	main := api.ProfileSpec{Name: "main", Backend: "claude", Model: "m"}
	other := api.ProfileSpec{Name: "other", Backend: "codex", Model: "m"}
	r.srv.reg().SetHandout(handout("rev-1", main, other))
	r.start()
	r.waitConnected()
	waitFor(t, "the first report", func() bool { report, n := r.srv.lastReport(); return n > 0 && report.Revision == "rev-1" })
	report, _ := r.srv.lastReport()
	if len(report.Profiles) != 2 || !report.Profiles[0].Runnable || report.Profiles[1].Runnable {
		t.Fatalf("the client should run main and not other: %+v", report.Profiles)
	}
	fixes := doctor.CheckTools(context.Background(), doctor.Options{LookPath: func(string) (string, error) { return "", fmt.Errorf("missing") }}, doctor.Tools{Agent: "codex"})
	problem := report.Profiles[1].Problems
	if len(problem) != 1 || problem[0].Check != "codex" || problem[0].Fix == "" || problem[0].Fix != fixes[0].Remediation {
		t.Fatalf("the report holds %+v, not the doctor's fix %+v", problem, fixes)
	}
	if client, _ := r.srv.reg().Client(node.ID); !client.CanRun("main") || client.CanRun("other") {
		t.Fatalf("the server's registry holds %+v", client.Capabilities)
	}

	r.install("codex")
	r.srv.reg().SetHandout(handout("rev-2", main, other))
	waitFor(t, "the report after the reload", func() bool { report, _ := r.srv.lastReport(); return report.Revision == "rev-2" })
	report, _ = r.srv.lastReport()
	if !report.Profiles[0].Runnable || !report.Profiles[1].Runnable || len(report.Profiles[1].Problems) != 0 {
		t.Fatalf("after the reload both profiles can run: %+v", report.Profiles)
	}
	if client, _ := r.srv.reg().Client(node.ID); !client.CanRun("other") || !client.ReportCurrent() {
		t.Fatalf("the server's registry holds %+v", client.Capabilities)
	}
}

func TestAProfileNeedsTheSandboxDaggerAndSkillsOfItsRoles(t *testing.T) {
	r := newRig(t, fleet.Options{})
	r.have["git"] = false
	h := api.ProfileHandout{Revision: "rev", Profiles: []api.ProfileSpec{{Name: "main", Backend: "claude"}},
		Roles: []api.RoleRequirement{{Role: "mason", Profile: "main", Chain: []string{"main"}, Sandbox: "sbx", Skills: []string{"https://example.com/skills.git#tdd"}, Dagger: &api.DaggerRequirement{Mode: "cloud"}}}}
	c, err := newClient(r.options())
	must(t, err)
	report := c.check(context.Background(), h)
	capability := report.Profiles[0]
	if capability.Runnable {
		t.Fatalf("a machine with no sbx, dagger or git runs the profile: %+v", capability)
	}
	var names []string
	for _, p := range capability.Problems {
		names = append(names, p.Check)
		if p.Fix == "" {
			t.Fatalf("problem %q has no fix", p.Check)
		}
	}
	for _, want := range []string{"sbx", "dagger", "skill https://example.com/skills.git#tdd"} {
		if !slices.Contains(names, want) {
			t.Fatalf("no problem named %q in %v", want, names)
		}
	}
	r.install("sbx")
	r.install("dagger")
	r.install("git")
	report = c.check(context.Background(), h)
	if capability = report.Profiles[0]; capability.Runnable || len(capability.Problems) != 1 || capability.Problems[0].Check != "dagger engine" {
		t.Fatalf("only the machine's Dagger setting is missing: %+v", capability)
	}
	r.env[daggerCloudToken] = "token"
	if capability = c.check(context.Background(), h).Profiles[0]; !capability.Runnable {
		t.Fatalf("with every tool and a Dagger setting the profile runs: %+v", capability)
	}
}

func TestAViewBuiltFromTheCachedCloneHoldsTheFilesOfTheServersViewAndLaterTurnsFetchIncrementally(t *testing.T) {
	r := newRig(t, fleet.Options{})
	c, err := newClient(r.options())
	must(t, err)
	ctx := context.Background()
	first := r.repo.git("rev-parse", "HEAD")
	paths := []string{"src", "docs/notes.md"}
	for _, dagger := range []bool{false, true} {
		offer := r.offer(fmt.Sprintf("turn-%v", dagger), func(o *api.TurnOffer) { o.RunsDagger = dagger; o.View.Paths = paths })
		view, err := c.buildView(ctx, offer, offer.ToolToken, filepath.Join(c.dir, "turns", offer.Turn, "views"))
		must(t, err)
		settings := coreadapter.ExecutionSettings{}
		if dagger {
			settings.DaggerCloudTokenEnv = "TOKEN"
		}
		served, err := (isolation.Views{Directory: t.TempDir()}).CreateFor(ctx, coreadapter.Workspace{Directory: r.repo.dir, Access: coreadapter.ReadWrite}, paths, settings)
		must(t, err)
		got, want := snapshot(t, view.Workspace().Directory), snapshot(t, served.Workspace().Directory)
		if fmt.Sprint(got) != fmt.Sprint(want) {
			t.Fatalf("dagger=%v: the client's view holds %v, the server's %v", dagger, got, want)
		}
		if got["src/main.go"] != "true:package main\n" || got["docs/notes.md"] != "false:# notes\n" {
			t.Fatalf("files or modes differ: %v", got)
		}
		if _, linked := got["src/link"]; linked {
			t.Fatal("the view holds a symlink the server's view leaves out")
		}
		_, gitErr := os.Lstat(filepath.Join(view.Workspace().Directory, ".git"))
		if dagger == (gitErr != nil) {
			t.Fatalf("dagger=%v but .git: %v", dagger, gitErr)
		}
		must(t, view.Release(ctx))
		must(t, served.Release(ctx))
	}

	put(t, r.repo.dir, "src/new.txt", "new\n")
	put(t, r.repo.dir, "src/main.go", "package main // changed\n")
	second := r.repo.commit("second")
	before := len(r.srv.gitRequests())
	offer := r.offer("turn-second", func(o *api.TurnOffer) { o.View.Paths = paths })
	view, err := c.buildView(ctx, offer, offer.ToolToken, filepath.Join(c.dir, "turns", offer.Turn, "views"))
	must(t, err)
	got := snapshot(t, view.Workspace().Directory)
	if got["src/new.txt"] != "false:new\n" || got["src/main.go"] != "true:package main // changed\n" {
		t.Fatalf("the second turn's view: %v", got)
	}
	incremental := false
	for _, body := range r.srv.gitRequests()[before:] {
		incremental = incremental || strings.Contains(body, "have "+first)
	}
	if !incremental {
		t.Fatalf("the second fetch did not build on the first commit %s: %q", first, r.srv.gitRequests()[before:])
	}
	entries, err := os.ReadDir(filepath.Join(c.dir, "clones"))
	must(t, err)
	if len(entries) != 1 {
		t.Fatalf("the project has %d cached clones", len(entries))
	}
	before = len(r.srv.gitRequests())
	again := r.offer("turn-again", func(o *api.TurnOffer) { o.View.Paths = paths })
	_, err = c.buildView(ctx, again, again.ToolToken, filepath.Join(c.dir, "turns", again.Turn, "views"))
	must(t, err)
	if n := len(r.srv.gitRequests()); n != before {
		t.Fatalf("a commit the cache holds was fetched again (%d requests)", n-before)
	}
	if second == first {
		t.Fatal("no second commit")
	}

	missing := r.offer("turn-missing", func(o *api.TurnOffer) { o.View.Paths = []string{"src", "nothing/here"} })
	if _, err := c.buildView(ctx, missing, missing.ToolToken, filepath.Join(c.dir, "turns", missing.Turn, "views")); err == nil {
		t.Fatal("a selected path the commit lacks was accepted")
	}
	link := r.offer("turn-link", func(o *api.TurnOffer) { o.View.Paths = []string{"src/link"} })
	if _, err := c.buildView(ctx, link, link.ToolToken, filepath.Join(c.dir, "turns", link.Turn, "views")); err == nil {
		t.Fatal("a selected symlink was accepted")
	}
}

func TestAViewOfTraceDocumentsIsUnpackedFromItsArchive(t *testing.T) {
	r := newRig(t, fleet.Options{})
	c, err := newClient(r.options())
	must(t, err)
	documents := t.TempDir()
	put(t, documents, "spec.md", "# spec\n")
	put(t, documents, "units/a.md", "a\n")
	r.srv.register("turn-docs", "token-docs", documents)
	offer := r.offer("turn-docs", func(o *api.TurnOffer) {
		o.Role, o.View, o.ToolToken, o.Capabilities, o.ReturnView = "chief_of_staff", api.TurnView{Kind: api.ViewArchive}, "token-docs", coreadapter.Capabilities{Tools: []string{"file_read"}}, false
	})
	view, err := c.buildView(context.Background(), offer, "token-docs", filepath.Join(c.dir, "turns", "turn-docs", "views"))
	must(t, err)
	if got, want := snapshot(t, view.Workspace().Directory), snapshot(t, documents); fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("the unpacked view holds %v, the archive's source %v", got, want)
	}
	if view.Workspace().Access != coreadapter.ReadOnly {
		t.Fatalf("a role that writes nothing got access %q", view.Workspace().Access)
	}
	if _, err := c.buildView(context.Background(), offer, "another token", filepath.Join(c.dir, "turns", "turn-docs", "views")); err == nil {
		t.Fatal("the archive was served for another token")
	}
}

func TestAToolCallToTheLocalListenerReachesTheServerWithTheTurnsToken(t *testing.T) {
	r := newRig(t, fleet.Options{})
	type attempt struct {
		withToken, withoutToken, withTurnToken int
		body, local                            string
	}
	var got attempt
	var mu sync.Mutex
	r.engine.script = func(_ context.Context, turn *enforcertest.Turn) (*agent.Result, error) {
		entry := turn.Request.Profile.MCP["osmia_0"]
		local := turn.Request.Env[coreadapter.TokenEnvironment]
		post := func(token string) (int, string) {
			req, _ := http.NewRequest(http.MethodPost, entry.URL, strings.NewReader(`{"method":"tools/call"}`))
			req.Header.Set("Content-Type", "application/json")
			if token != "" {
				req.Header.Set("Authorization", "Bearer "+token)
			}
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				return 0, err.Error()
			}
			defer resp.Body.Close()
			body, _ := io.ReadAll(resp.Body)
			return resp.StatusCode, string(body)
		}
		mu.Lock()
		defer mu.Unlock()
		got.local = local
		got.withToken, got.body = post(local)
		got.withoutToken, _ = post("")
		got.withTurnToken, _ = post("token-turn-a")
		return finished(turn)
	}
	r.start()
	r.waitConnected()
	r.give(r.offer("turn-a"))
	r.waitResult("turn-a")
	mu.Lock()
	defer mu.Unlock()
	if got.local == "" || got.local == "token-turn-a" {
		t.Fatalf("the sandbox holds %q for the local listener, which must be a token of its own", got.local)
	}
	if got.withToken != 200 || !strings.Contains(got.body, "relayed") || got.withoutToken != 401 || got.withTurnToken != 401 {
		t.Fatalf("local listener answers: %+v", got)
	}
	calls := r.srv.mcpCalls()
	if len(calls) != 1 || calls[0].auth != "Bearer token-turn-a" || calls[0].turn != "turn-a" || calls[0].path != api.MCPPath("turn-a") || !strings.Contains(calls[0].body, "tools/call") {
		t.Fatalf("the server received %+v", calls)
	}
}

func TestTheClientCarriesOutTheServersViewAndSkillRequestsOnItsOwnFiles(t *testing.T) {
	r := newRig(t, fleet.Options{})
	cache := t.TempDir()
	const ref = "https://github.com/acme/skills#skills/tdd"
	put(t, cache, "repos/skills/skills/tdd/SKILL.md", "tdd skill")
	put(t, cache, "plugins/skills-tdd/.claude-plugin/plugin.json", "{}")
	must(t, os.Symlink(filepath.Join(cache, "repos/skills/skills/tdd"), filepath.Join(cache, "plugins/skills-tdd/skills")))
	r.engine.dirs, r.engine.plugins = []string{cache}, map[string]string{ref: filepath.Join(cache, "plugins/skills-tdd")}
	var mu sync.Mutex
	var problems []string
	note := func(format string, args ...any) {
		mu.Lock()
		defer mu.Unlock()
		problems = append(problems, fmt.Sprintf(format, args...))
	}
	r.engine.script = func(ctx context.Context, turn *enforcertest.Turn) (*agent.Result, error) {
		dir := r.viewDirOf(turn)
		call := func(op string, params any) (string, error) {
			raw, err := r.srv.reg().Call(ctx, node.ID, "turn-a", op, params)
			if err != nil {
				return "", err
			}
			var out api.ViewReadResult
			_ = json.Unmarshal(raw, &out)
			return string(out.Data), nil
		}
		if _, err := call(api.OpViewWrite, api.ViewWriteParams{Path: "src/new.txt", Content: []byte("from a tool")}); err != nil {
			note("write: %v", err)
		}
		if data, err := os.ReadFile(filepath.Join(dir, "src/new.txt")); err != nil || string(data) != "from a tool" {
			note("the session does not see the tool's write: %q %v", data, err)
		}
		writeFrom(t, filepath.Join(dir, "src/main.go"), "changed by the session\n")
		if data, err := call(api.OpViewRead, api.ViewReadParams{Path: "src/main.go"}); err != nil || data != "changed by the session\n" {
			note("a read does not return what the session last wrote: %q %v", data, err)
		}
		if data, err := call(api.OpViewRead, api.ViewReadParams{Path: "."}); err != nil || !strings.Contains(data, "src/") {
			note("root listing: %q %v", data, err)
		}
		if data, err := call(api.OpSkillRead, api.ViewReadParams{Path: filepath.Join(cache, "plugins/skills-tdd/skills/SKILL.md")}); err != nil || data != "tdd skill" {
			note("skill read: %q %v", data, err)
		}
		for _, path := range []string{"../outside", "/etc/passwd", ".git/config", "src/../../x", "src/nested/../../../x"} {
			if _, err := call(api.OpViewRead, api.ViewReadParams{Path: path}); err == nil {
				note("a read of %q was answered", path)
			}
			if _, err := call(api.OpViewWrite, api.ViewWriteParams{Path: path, Content: []byte("x")}); err == nil {
				note("a write to %q was answered", path)
			}
		}
		if _, err := os.Stat(filepath.Join(filepath.Dir(dir), "outside")); err == nil {
			note("a write escaped the view")
		}
		for _, path := range []string{"/etc/passwd", filepath.Join(cache, "plugins/skills-tdd/.claude-plugin/plugin.json"), filepath.Join(cache, "../x")} {
			if _, err := call(api.OpSkillRead, api.ViewReadParams{Path: path}); err == nil {
				note("a skill read of %q was answered", path)
			}
		}
		return finished(turn)
	}
	r.start()
	r.waitConnected()
	r.give(r.offer("turn-a", func(o *api.TurnOffer) { o.Execution.Skills = []string{ref} }))
	r.waitResult("turn-a")
	mu.Lock()
	defer mu.Unlock()
	if len(problems) != 0 {
		t.Fatalf("%s", strings.Join(problems, "\n"))
	}
	waitFor(t, "the uploaded view", func() bool { _, ok := r.srv.uploadedView("turn-a"); return ok })
	dir, _ := r.srv.uploadedView("turn-a")
	files := snapshot(t, dir)
	if files["src/new.txt"] != "false:from a tool" || files["src/main.go"] != "true:changed by the session\n" {
		t.Fatalf("the uploaded view holds %v", files)
	}
}

func TestTheFirstInterruptDrainsTheClient(t *testing.T) {
	r := newRig(t, fleet.Options{})
	release, started := make(chan struct{}), make(chan struct{}, 1)
	r.engine.script = func(ctx context.Context, turn *enforcertest.Turn) (*agent.Result, error) {
		started <- struct{}{}
		select {
		case <-release:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		return finished(turn)
	}
	r.start()
	r.waitConnected()
	if claim := r.give(r.offer("turn-a")); !claim.Accepted {
		t.Fatalf("claim: %+v", claim)
	}
	<-started
	r.interrupts <- struct{}{}
	waitFor(t, "the server to mark the client draining", func() bool { c, ok := r.srv.reg().Client(node.ID); return ok && c.Draining })
	r.client.handleOffer(r.offer("turn-b"))
	if n := r.running(); n != 1 {
		t.Fatalf("a draining client holds %d running turns", n)
	}
	beats, _, _ := r.srv.counts()
	waitFor(t, "heartbeats during the drain", func() bool { b, _, _ := r.srv.counts(); return b >= beats+2 })
	if logs := r.logs.String(); !strings.Contains(logs, "waiting for a running turn") || !strings.Contains(logs, "turn-a") {
		t.Fatalf("the drain did not list the running turn:\n%s", logs)
	}
	select {
	case err := <-r.done:
		t.Fatalf("the client exited with a turn running: %v", err)
	default:
	}
	close(release)
	if err := r.waitExit(); err != nil {
		t.Fatal(err)
	}
	results := r.srv.resultsOf("turn-a")
	if len(results) != 1 || results[0].Result.Cancelled || results[0].Result.IsError {
		t.Fatalf("results: %+v", results)
	}
	if _, ok := r.srv.uploadedView("turn-a"); !ok {
		t.Fatal("the view was not sent")
	}
	if _, ok := r.srv.reg().Client(node.ID); ok {
		t.Fatal("the client left its lease behind")
	}
	if len(r.srv.resultsOf("turn-b")) != 0 {
		t.Fatal("a turn offered during the drain ran")
	}
}

func TestTheSecondInterruptCancelsTheSessionAndStillSendsTheView(t *testing.T) {
	r := newRig(t, fleet.Options{})
	started, cancelled := make(chan struct{}, 1), atomic.Bool{}
	r.engine.script = func(ctx context.Context, turn *enforcertest.Turn) (*agent.Result, error) {
		writeFrom(t, filepath.Join(r.viewDirOf(turn), "src/draft.txt"), "unfinished\n")
		started <- struct{}{}
		<-ctx.Done()
		cancelled.Store(true)
		return nil, ctx.Err()
	}
	r.start()
	r.waitConnected()
	r.give(r.offer("turn-a"))
	<-started
	r.interrupts <- struct{}{}
	r.interrupts <- struct{}{}
	res := r.waitResult("turn-a")
	if !cancelled.Load() || !res.Result.Cancelled {
		t.Fatalf("cancelled=%v result %+v", cancelled.Load(), res.Result)
	}
	if err := r.waitExit(); err != nil {
		t.Fatal(err)
	}
	dir, ok := r.srv.uploadedView("turn-a")
	if !ok {
		t.Fatal("the cancelled mason's view was not sent")
	}
	if got := snapshot(t, dir)["src/draft.txt"]; got != "false:unfinished\n" {
		t.Fatalf("the uploaded view holds %q", got)
	}
}

func TestASessionKeepsRunningAcrossADroppedConnectionAndReattaches(t *testing.T) {
	r := newRig(t, fleet.Options{Grace: 8 * time.Second})
	release, started, cancelled := make(chan struct{}), make(chan struct{}, 1), atomic.Bool{}
	r.engine.script = func(ctx context.Context, turn *enforcertest.Turn) (*agent.Result, error) {
		started <- struct{}{}
		select {
		case <-release:
		case <-ctx.Done():
			cancelled.Store(true)
			return nil, ctx.Err()
		}
		return finished(turn)
	}
	r.start()
	r.waitConnected()
	r.give(r.offer("turn-a"))
	<-started
	_, connects, _ := r.srv.counts()
	lease, _ := r.srv.reg().Client(node.ID)
	r.srv.setDown(true)
	time.Sleep(1500 * time.Millisecond)
	r.srv.setDown(false)
	waitFor(t, "the client to reconnect", func() bool { _, n, _ := r.srv.counts(); return n > connects })
	waitFor(t, "the server to hold the turn again", func() bool {
		c, ok := r.srv.reg().Client(node.ID)
		return ok && c.Lease == lease.Lease && len(c.Held) == 1 && c.Held[0].Turn == "turn-a"
	})
	if cancelled.Load() {
		t.Fatal("the session was cancelled within the grace window")
	}
	close(release)
	res := r.waitResult("turn-a")
	if res.Result.Cancelled || res.Result.IsError {
		t.Fatalf("result %+v", res.Result)
	}
	waitFor(t, "the view", func() bool { _, ok := r.srv.uploadedView("turn-a"); return ok })
	waitFor(t, "the turn to be discarded", func() bool { return len(r.pendingTurns()) == 0 })
	if n := len(r.srv.resultsOf("turn-a")); n != 1 {
		t.Fatalf("the result was sent %d times", n)
	}
}

func TestASessionReattachesToARestartedServerByTurnID(t *testing.T) {
	options := fleet.Options{Grace: 8 * time.Second, Heartbeat: time.Second, Poll: time.Second}
	r := newRig(t, options)
	release, started, cancelled := make(chan struct{}), make(chan struct{}, 1), atomic.Bool{}
	r.engine.script = func(ctx context.Context, turn *enforcertest.Turn) (*agent.Result, error) {
		started <- struct{}{}
		select {
		case <-release:
		case <-ctx.Done():
			cancelled.Store(true)
			return nil, ctx.Err()
		}
		return finished(turn)
	}
	r.start()
	r.waitConnected()
	r.give(r.offer("turn-a"))
	<-started
	restarted := r.srv.restart(options)
	hooks := r.srv.hooks()
	hooks.Reattach = func(_ context.Context, _ fleet.Identity, turn string) (string, bool) {
		r.srv.register(turn, "token-after-restart", "")
		return "token-after-restart", turn == "turn-a"
	}
	restarted.SetHooks(hooks)
	waitFor(t, "the server to hold the turn again", func() bool {
		c, ok := r.srv.reg().Client(node.ID)
		return ok && len(c.Held) == 1
	})
	r.client.mu.Lock()
	token := r.client.running["turn-a"].uploadToken()
	r.client.mu.Unlock()
	if token != "token-after-restart" || cancelled.Load() {
		t.Fatalf("token %q cancelled %v", token, cancelled.Load())
	}
	close(release)
	r.waitResult("turn-a")
	waitFor(t, "the view to be taken under the new token", func() bool { _, ok := r.srv.uploadedView("turn-a"); return ok })
}

func TestPastTheGraceWindowTheClientCancelsKeepsTheViewAndSendsItOnceTheServerAnswers(t *testing.T) {
	r := newRig(t, fleet.Options{Grace: 3 * time.Second})
	started, cancelled := make(chan struct{}, 1), atomic.Bool{}
	r.engine.script = func(ctx context.Context, turn *enforcertest.Turn) (*agent.Result, error) {
		writeFrom(t, filepath.Join(r.viewDirOf(turn), "src/work.txt"), "work\n")
		started <- struct{}{}
		<-ctx.Done()
		cancelled.Store(true)
		return nil, ctx.Err()
	}
	r.srv.mu.Lock()
	r.srv.viewAns["turn-a"] = []api.ViewAnswer{"", api.ViewSuperseded}
	r.srv.resultAns["turn-a"] = []api.ViewAnswer{api.ViewSuperseded}
	r.srv.mu.Unlock()
	r.start()
	r.waitConnected()
	r.give(r.offer("turn-a"))
	<-started
	r.srv.setDown(true)
	waitFor(t, "the client to cancel the session", cancelled.Load)
	turnDir := filepath.Join(r.dir, "turns", "turn-a")
	waitFor(t, "the cancelled turn to be kept", func() bool { return len(r.pendingTurns()) == 1 })
	time.Sleep(time.Second)
	views, _ := filepath.Glob(filepath.Join(turnDir, "views", "turn-*", "src", "work.txt"))
	if len(views) != 1 || len(r.pendingTurns()) != 1 {
		t.Fatalf("the cancelled mason's view is not kept while the server is unreachable: %v %v", views, r.pendingTurns())
	}
	if len(r.srv.resultsOf("turn-a")) != 0 {
		t.Fatal("a result reached an unreachable server")
	}
	r.srv.setDown(false)
	waitFor(t, "the turn to be discarded after the server's answers", func() bool { return len(r.pendingTurns()) == 0 })
	if _, err := os.Stat(turnDir); !os.IsNotExist(err) {
		t.Fatalf("the turn's directory is left behind: %v", err)
	}
	r.srv.mu.Lock()
	tries := r.srv.viewTries["turn-a"]
	r.srv.mu.Unlock()
	results := r.srv.resultsOf("turn-a")
	if tries < 2 || len(results) != 1 || !results[0].Result.Cancelled {
		t.Fatalf("view attempts %d, results %+v: an answer that was not definitive discarded the turn", tries, results)
	}
}

func TestAViewAndResultTheServerNeverAnsweredForAreSentByTheNextRun(t *testing.T) {
	r := newRig(t, fleet.Options{})
	release, started := make(chan struct{}), make(chan struct{}, 1)
	r.engine.script = func(ctx context.Context, turn *enforcertest.Turn) (*agent.Result, error) {
		started <- struct{}{}
		<-release
		writeFrom(t, filepath.Join(r.viewDirOf(turn), "src/last.txt"), "last\n")
		return finished(turn)
	}
	r.start()
	r.waitConnected()
	r.give(r.offer("turn-a"))
	<-started
	r.srv.setDown(true)
	close(release)
	waitFor(t, "the finished turn to be kept", func() bool { return len(r.pendingTurns()) == 1 })
	r.stop()
	r.srv.setDown(false)
	r.engine.script = nil
	r.start()
	waitFor(t, "the second run to send the turn", func() bool { return len(r.srv.resultsOf("turn-a")) == 1 })
	dir, ok := r.srv.uploadedView("turn-a")
	if !ok || snapshot(t, dir)["src/last.txt"] != "false:last\n" {
		t.Fatalf("uploaded view %q %v", dir, ok)
	}
	waitFor(t, "the turn to be discarded", func() bool { return len(r.pendingTurns()) == 0 })
}

func TestTheClientCreatesNoTraceStoreOrTelemetryAndLogsToStderrOnly(t *testing.T) {
	var exports atomic.Int32
	collector := newCollector(t, &exports)
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", collector)
	t.Setenv("OTEL_TRACES_EXPORTER", "otlp")
	restoreOut, restoreErr := capture(t, &os.Stdout), capture(t, &os.Stderr)
	r := newRig(t, fleet.Options{})
	options := r.options()
	options.Logger = nil
	c, err := newClient(options)
	must(t, err)
	ctx, cancel := context.WithCancel(context.Background())
	r.client, r.cancel, r.done = c, cancel, make(chan error, 1)
	go func() { r.done <- c.run(ctx) }()
	r.waitConnected()
	r.give(r.offer("turn-a"))
	r.waitResult("turn-a")
	waitFor(t, "the turn to be discarded", func() bool { return len(r.pendingTurns()) == 0 })
	r.interrupts <- struct{}{}
	must(t, r.waitExit())
	out, errOut := restoreOut(), restoreErr()
	if out != "" {
		t.Fatalf("the client wrote to stdout: %q", out)
	}
	if !strings.Contains(errOut, "connected to the server") {
		t.Fatalf("the client's log is not on stderr: %q", errOut)
	}
	if exports.Load() != 0 {
		t.Fatalf("the client exported telemetry %d times", exports.Load())
	}
	entries, err := os.ReadDir(r.dir)
	must(t, err)
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	slices.Sort(names)
	if fmt.Sprint(names) != "[clones sessions skills stage turns]" {
		t.Fatalf("the client directory holds %v", names)
	}
	must(t, filepath.WalkDir(r.dir, func(path string, e fs.DirEntry, err error) error {
		if err == nil && !e.IsDir() && (strings.HasSuffix(path, ".db") || strings.HasSuffix(path, ".sqlite") || strings.Contains(path, "trace")) {
			t.Errorf("the client created %s", path)
		}
		return err
	}))
}

func TestTheClientPackageImportsNoTraceStoreOrTelemetry(t *testing.T) {
	entries, err := os.ReadDir(".")
	must(t, err)
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") || strings.HasSuffix(e.Name(), "_test.go") {
			continue
		}
		data, err := os.ReadFile(e.Name())
		must(t, err)
		for _, banned := range []string{"internal/trace\"", "internal/telemetry", "internal/slotspans", "opentelemetry", "internal/recordspans"} {
			if bytes.Contains(data, []byte(banned)) {
				t.Errorf("%s imports %s", e.Name(), banned)
			}
		}
	}
}
