package clientjourney

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kpenfound/agentcore/agent"
	"github.com/kpenfound/agentcore/agent/agenttest/enforcertest"
	"github.com/kpenfound/osmia/internal/api"
	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/execution"
	"github.com/kpenfound/osmia/internal/intake"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	"github.com/kpenfound/osmia/internal/servicetest/intakefixture"
	"github.com/kpenfound/osmia/internal/trace"
)

const (
	relayedFile   = "internal/trace/relayed.go"
	relayedSource = "package trace\n\n// written through a relayed file_write\n"
)

// remoteMason is the fake agent a client runs in place of a model session. Its
// first turn edits the client's view directly and works through the relayed
// tools and ends without a report; the turn that resumes its session holds
// until gate is closed and then reports done.
type remoteMason struct {
	gate    chan struct{}
	running chan string

	mu              sync.Mutex
	unit            string
	tools           []string
	onDisk          string
	readBefore      string
	readAfter       string
	sessionEdit     string
	writeOnDisk     string
	outsideRefused  bool
	resumed         map[string]string
	continuationEnd error
}

func newRemoteMason() *remoteMason {
	return &remoteMason{gate: make(chan struct{}), running: make(chan string, 8), resumed: map[string]string{}}
}

func decoded(text string) string {
	var s string
	if json.Unmarshal([]byte(text), &s) != nil {
		return text
	}
	return s
}

func (rm *remoteMason) script(ctx context.Context, turn *enforcertest.Turn) (*agent.Result, error) {
	req := turn.Request
	finished := &agent.Result{ClaudeID: "session-" + req.Name, ResultText: "Built", SessionDir: req.SessionDir, NumTurns: 2}
	if !strings.HasPrefix(req.Name, "mason-") {
		return finished, nil
	}
	dir := req.Workspace.Directory()
	rm.mu.Lock()
	if rm.unit == "" && strings.HasSuffix(req.Name, "-implement") {
		rm.unit = strings.TrimSuffix(req.Name, "-implement")
	}
	mine := strings.HasPrefix(req.Name, rm.unit+"-")
	rm.mu.Unlock()
	if !mine {
		return finished, nil
	}
	tools, err := dial(ctx, req)
	if err != nil {
		return nil, err
	}
	defer tools.Close()
	if !strings.HasSuffix(req.Name, "-implement") {
		rm.mu.Lock()
		rm.resumed[req.Name] = req.ResumeID
		rm.mu.Unlock()
		rm.running <- req.Name
		select {
		case <-rm.gate:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		err := intakefixture.ReportDone("Continued on the same client")(ctx, req, tools)
		rm.mu.Lock()
		rm.continuationEnd = err
		rm.mu.Unlock()
		return finished, err
	}

	listed, err := tools.ListTools(ctx, nil)
	if err != nil {
		return nil, err
	}
	var names []string
	for _, tool := range listed.Tools {
		names = append(names, tool.Name)
	}
	slices.Sort(names)
	disk, err := os.ReadFile(filepath.Join(dir, intakefixture.TrackedFile))
	if err != nil {
		return nil, err
	}
	before, err := fixturekit.CallTool(ctx, tools, "file_read", map[string]any{"path": intakefixture.TrackedFile})
	if err != nil {
		return nil, err
	}
	edit := string(disk) + "\n// changed by the session in the client's view\n"
	if err := os.WriteFile(filepath.Join(dir, intakefixture.TrackedFile), []byte(edit), 0o644); err != nil {
		return nil, err
	}
	after, err := fixturekit.CallTool(ctx, tools, "file_read", map[string]any{"path": intakefixture.TrackedFile})
	if err != nil {
		return nil, err
	}
	if _, err := fixturekit.CallTool(ctx, tools, "file_write", map[string]any{"path": relayedFile, "content": relayedSource}); err != nil {
		return nil, err
	}
	written, err := os.ReadFile(filepath.Join(dir, relayedFile))
	if err != nil {
		return nil, err
	}
	_, outside := fixturekit.CallTool(ctx, tools, "file_read", map[string]any{"path": "../outside"})
	rm.mu.Lock()
	rm.tools, rm.onDisk, rm.readBefore, rm.readAfter, rm.sessionEdit, rm.writeOnDisk, rm.outsideRefused = names, string(disk), decoded(before), decoded(after), edit, string(written), outside != nil
	rm.mu.Unlock()
	return finished, nil
}

func gitOut(t *testing.T, args ...string) (string, error) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0", "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL="+os.DevNull)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func completedTurn(t *testing.T, r *serverRig, stream config.WorkstreamID, unit, turn string) trace.QueuedTurn {
	t.Helper()
	return intakefixture.AwaitCompleted(t, r.f.Repository(), stream, intake.MasonAgent(unit), turn)
}

func TestAClientTakesTurnsAlongsideTheLocalClientAndDrains(t *testing.T) {
	r := newServerRig(t, parallel(2, 2, intakefixture.DisjointPlan))
	f := r.f
	const node = "node-a"
	rm := newRemoteMason()
	m := r.machine(node)
	m.script = rm.script

	m.start(r.address(), 2)
	connected := r.awaitCapable(node)
	if connected.Slots != 2 || connected.Draining || connected.Identity.Name != "name-of-"+node {
		t.Fatalf("the connected client: %+v", connected)
	}
	handout := r.f.S.Fleet().Handout()
	if len(handout.Profiles) == 0 || len(handout.Roles) == 0 {
		t.Fatalf("the server handed the client no profiles: %+v", handout)
	}
	raw, err := json.Marshal(handout)
	fixturekit.Must(t, err)
	for _, secret := range []string{"api_key", "token", "secret", "password"} {
		if strings.Contains(strings.ToLower(string(raw)), secret) {
			t.Fatalf("the profile handout names %q: %s", secret, raw)
		}
	}
	for _, p := range handout.Profiles {
		if !connected.CanRun(p.Name) {
			t.Fatalf("the client's report does not cover profile %s: %+v", p.Name, connected.Capabilities)
		}
	}

	stream := build(f, t, "journey", intakefixture.DisjointPlan)
	first := map[string]trace.QueuedTurn{}
	for _, unit := range []string{"resume", "upload"} {
		first[unit] = completedTurn(t, r, stream, unit, execution.MasonTurnID(unit))
	}
	clientOf := func(unit string) string { return first[unit].Response.Result.Client }
	var remoteUnit, localUnit string
	for unit := range first {
		switch clientOf(unit) {
		case node:
			remoteUnit = unit
		case coreadapter.LocalClient:
			localUnit = unit
		}
	}
	if remoteUnit == "" || localUnit == "" {
		t.Fatalf("new turns did not rotate between the local client and the remote client: resume ran on %q, upload on %q", clientOf("resume"), clientOf("upload"))
	}

	rm.mu.Lock()
	tools, onDisk, readBefore, readAfter, edit, written, outside := rm.tools, rm.onDisk, rm.readBefore, rm.readAfter, rm.sessionEdit, rm.writeOnDisk, rm.outsideRefused
	rm.mu.Unlock()
	if !slices.Contains(tools, "file_read") || !slices.Contains(tools, "file_write") || !slices.Contains(tools, execution.DoneTool) {
		t.Fatalf("the remote session was given tools %v", tools)
	}
	if onDisk == "" || readBefore != onDisk {
		t.Fatalf("a relayed read does not return the file in the client's view: %q, view holds %q", readBefore, onDisk)
	}
	if readAfter != edit {
		t.Fatalf("a relayed read does not return the file as the session changed it: %q, want %q", readAfter, edit)
	}
	if written != relayedSource {
		t.Fatalf("a relayed write is not seen by the session: %q", written)
	}
	if !outside {
		t.Fatal("a relayed read of a path outside the view was answered")
	}

	gitDirs, err := filepath.Glob(filepath.Join(m.dir, "clones", "*.git"))
	if err != nil || len(gitDirs) != 1 {
		t.Fatalf("the client's cached clones: %v %v", gitDirs, err)
	}
	fetched, err := gitOut(t, "--git-dir", gitDirs[0], "for-each-ref", "--format=%(refname) %(objectname)", "refs/osmia/")
	if err != nil || strings.TrimSpace(fetched) == "" {
		t.Fatalf("the client fetched no starting commit through the git route: %q %v", fetched, err)
	}
	refsBefore, err := gitOut(t, "-C", f.Clone, "for-each-ref", "--format=%(refname) %(objectname)")
	fixturekit.Must(t, err)
	commit := strings.Fields(fetched)[1]
	out, err := gitOut(t, "--git-dir", gitDirs[0], "push", "http://"+r.address()+api.GitPath(f.Project), commit+":refs/heads/pushed-by-a-client")
	if err == nil {
		t.Fatalf("the git route took a push from a client: %s", out)
	}
	if refsAfter, err := gitOut(t, "-C", f.Clone, "for-each-ref", "--format=%(refname) %(objectname)"); err != nil || refsAfter != refsBefore {
		t.Fatalf("a refused push changed the project's refs: %q then %q (%v)", refsBefore, refsAfter, err)
	}

	continuation := intake.MasonAgent(remoteUnit) + "-clarify-1"
	select {
	case name := <-rm.running:
		if name != continuation {
			t.Fatalf("the remote client runs %s, want %s", name, continuation)
		}
	case <-time.After(fixturekit.DemoTimeout):
		t.Fatalf("the resumed turn did not return to the client holding its session\n%s", m.logs.String())
	}
	rm.mu.Lock()
	resumedSession := rm.resumed[continuation]
	rm.mu.Unlock()
	if want := "session-" + execution.MasonTurnID(remoteUnit); resumedSession != want {
		t.Fatalf("the resumed turn resumed %q, want %q", resumedSession, want)
	}
	th, err := f.Repository().Thread(stream, intake.MasonAgent(remoteUnit))
	fixturekit.Must(t, err)
	if holder, ok := th.SessionClient(); !ok || holder != node {
		t.Fatalf("the trace records the thread's session on %q %v", holder, ok)
	}

	ctx := context.Background()
	status, err := f.C.Statuses(ctx)
	fixturekit.Must(t, err)
	clients := map[string]api.ClientStatus{}
	for _, c := range status.Capacity.Clients {
		clients[c.ID] = c
	}
	remote, local := clients[node], clients[coreadapter.LocalClient]
	if !local.Local || remote.Local || remote.Draining || remote.Used != 1 || len(remote.Turns) != 1 || remote.Turns[0].Turn != continuation {
		t.Fatalf("status shows the clients %+v", status.Capacity.Clients)
	}

	tookBefore := strings.Count(m.logs.String(), "took a turn")
	m.interrupt()
	intakefixture.Soon(t, "the server to mark the client draining", func() bool {
		c, ok := r.connected(node)
		return ok && c.Draining
	})
	status, err = f.C.Statuses(ctx)
	fixturekit.Must(t, err)
	for _, c := range status.Capacity.Clients {
		if c.ID == node && (!c.Draining || c.Idle != api.ClientIdleDraining || len(c.Turns) != 1) {
			t.Fatalf("status shows the draining client as %+v", c)
		}
	}
	if c, ok := r.connected(node); !ok || len(c.Held) != 1 || c.Held[0].Turn != continuation {
		t.Fatalf("a draining client stops heartbeating its running turn: %+v", c)
	}
	select {
	case err := <-m.done:
		t.Fatalf("the client exited with a turn running: %v", err)
	case <-time.After(300 * time.Millisecond):
	}

	close(rm.gate)
	if err := m.exited(fixturekit.DemoTimeout); err != nil {
		t.Fatalf("a drained client exits cleanly: %v", err)
	}
	if _, ok := r.connected(node); ok {
		t.Fatal("the drained client still holds its lease")
	}
	if took := strings.Count(m.logs.String(), "took a turn"); took != tookBefore {
		t.Fatalf("a draining client took %d more turns", took-tookBefore)
	}
	rm.mu.Lock()
	ended := rm.continuationEnd
	rm.mu.Unlock()
	if ended != nil {
		t.Fatal(ended)
	}

	f.AwaitUnit(t, stream, remoteUnit, intake.UnitReviewing)
	report := latestReport(t, f.Repository(), stream, remoteUnit)
	for name, want := range map[string]string{intakefixture.TrackedFile: edit, relayedFile: relayedSource} {
		if got, err := inCommit(f, report.Candidate, name); err != nil || got != want {
			t.Fatalf("the commit recorded from the remote mason's view holds %s as %q (%v), want %q", name, got, err, want)
		}
	}
	done := completedTurn(t, r, stream, remoteUnit, continuation)
	if done.Response.Result.Client != node || done.Response.Result.Outcome == nil {
		t.Fatalf("the resumed turn ran on %q: %+v", done.Response.Result.Client, done.Response.Result)
	}

	entries, err := os.ReadDir(m.dir)
	fixturekit.Must(t, err)
	for _, e := range entries {
		if !slices.Contains([]string{"clones", "stage", "turns", "sessions", "skills"}, e.Name()) {
			t.Fatalf("the client keeps %q, which is no clone, session, skill or undelivered file", e.Name())
		}
	}
	for _, e := range findNamed(t, m.dir, "trace") {
		t.Fatalf("the client keeps a trace store: %s", e)
	}
	events, err := f.Repository().ClientEvents(stream, execution.MasonTurnID(remoteUnit))
	fixturekit.Must(t, err)
	if !slices.ContainsFunc(events, func(e trace.ClientEvent) bool {
		return e.Kind == trace.ClientReturned && e.What == "view" && e.Client == node
	}) {
		t.Fatalf("the server did not record the view the client returned: %+v", events)
	}
}

func findNamed(t *testing.T, root, name string) []string {
	t.Helper()
	var found []string
	fixturekit.Must(t, filepath.WalkDir(root, func(path string, e os.DirEntry, err error) error {
		if err == nil && strings.Contains(strings.ToLower(e.Name()), name) {
			found = append(found, path)
		}
		return err
	}))
	return found
}
