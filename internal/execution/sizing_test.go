package execution

import (
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/kpenfound/agentcore/agent"
	"github.com/kpenfound/agentcore/agent/agenttest"
	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/isolation"
)

// sizedRoles is a mason that runs in sbx from a template, with the Dagger CLI
// against a host engine, and the given size lines.
func sizedRoles(sizing string) string {
	return "[roles.mason]\nsandbox = 'sbx'\nimage = 'example/osmia:1'\n" + sizing +
		"[roles.mason.dagger]\nversion = 'v0.20.5'\nengine = 'tcp://127.0.0.1:1234'\n"
}

// configLoader loads the fixture's configuration with the mason roles given to
// each call, as a reload of the file would.
func configLoader(t *testing.T, f unitsFixture) func(roles string) *config.Config {
	t.Helper()
	file := filepath.Join(f.cfg.Root.String(), "config.toml")
	base, err := os.ReadFile(file)
	must(t, err)
	return func(roles string) *config.Config {
		t.Helper()
		must(t, os.WriteFile(file, append(slices.Clone(base), roles...), 0600))
		cfg, err := config.Load(config.Options{Root: f.cfg.Root.String()})
		must(t, err)
		return cfg
	}
}

// viewHook runs inside a fake sbx when a sandbox is created. It records what
// the mounted view holds then, and does what a guest does in the view it
// mounts at the same path: it edits README and plants files under the view's
// repository.
const viewHook = `
if [ "$1" = create ]; then
  for arg in "$@"; do
    d="${arg%:ro}"
    if [ -d "$d/.git" ]; then
      (cd "$d" && find .git -type f | LC_ALL=C sort > "$here/view-repository-files.txt" && ls -A .git | LC_ALL=C sort > "$here/view-repository-top.txt" && ls -A | LC_ALL=C sort > "$here/view-root.txt")
      mkdir -p "$d/.git/hooks"
      echo planted > "$d/.git/planted"
      printf '#!/bin/sh\n' > "$d/.git/hooks/pre-commit"
      printf 'widgets, built\n' > "$d/README"
    fi
  done
fi
`

// fakeSbx is agentcore's fake sbx with viewHook, driving a fake claude that
// waits for the file finish before it reports.
func fakeSbx(t *testing.T, finish string) agent.Runner {
	t.Helper()
	sbx := agenttest.Sbx(t, "LANG")
	data, err := os.ReadFile(sbx)
	must(t, err)
	const here = "here=\"$(dirname \"$0\")\"\n"
	script := strings.Replace(string(data), here, here+viewHook, 1)
	if script == string(data) {
		t.Fatal("the fake sbx has no directory variable to hook")
	}
	must(t, os.WriteFile(sbx, []byte(script), 0o700))
	bin := agenttest.Script(t, "fake-agent", `case "$*" in
*--version*) echo '2.1.200'; exit 0 ;;
*--help*) echo '--tools --strict-mcp-config'; exit 0 ;;
esac
cat >/dev/null
while [ ! -f `+finish+` ]; do sleep 0.01; done
echo '{"type":"result","subtype":"success","result":"built"}'
`)
	return agent.Runner{SbxBin: sbx, ClaudeBin: bin, CodexBin: bin, OpenCodeBin: bin}
}

// sbxCreate is the arguments of the runner's sbx create.
func sbxCreate(t *testing.T, runner agent.Runner) []string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(filepath.Dir(runner.SbxBin), "sbx-create.txt"))
	must(t, err)
	return strings.Split(strings.TrimSuffix(string(data), "\n"), "\n")
}

func lines(t *testing.T, runner agent.Runner, name string) []string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(filepath.Dir(runner.SbxBin), name))
	must(t, err)
	return strings.Fields(string(data))
}

// masonTurnOn runs a mason turn of the unit through isolation.Turns the way the
// service lends it the unit's workspace, in the execution settings and on the
// runner given. The turn holds no tools; its sandbox is the real core sbx
// enforcer over the runner's fake sbx.
func (f unitsFixture) masonTurnOn(ctx context.Context, units unitWorkspaces, unit, turn string, execution coreadapter.ExecutionSettings, runner agent.Runner) (coreadapter.SessionResult, error) {
	session := filepath.Join(f.home, "session-"+turn)
	turns := &isolation.Turns{
		Workspaces: units,
		Views:      isolation.Views{Directory: filepath.Join(f.cfg.Root.String(), "views", turn)},
		Select: func(ctx context.Context, scope coreadapter.Scope) (isolation.Selection, error) {
			w, _, _, err := units.find(ctx, config.WorkstreamID(scope.Workstream), scope.Unit)
			if err != nil {
				return isolation.Selection{}, err
			}
			paths, err := units.paths(w)
			return isolation.Selection{Paths: paths, Execution: execution, Environment: map[string]string{"LANG": session}}, err
		},
		Grants:  map[string]coreadapter.Capabilities{masonRole: {WriteFiles: true, Execute: true}},
		Engine:  coreadapter.CoreEngine{Runner: runner},
		Capture: units.capture,
	}
	if err := os.MkdirAll(turns.Views.Directory, 0700); err != nil {
		return coreadapter.SessionResult{}, err
	}
	input := coreadapter.PreparedTurn{Scope: coreadapter.Scope{Project: string(project), Workstream: string(stream), Unit: unit, Thread: "mason-" + unit, Turn: turn, Role: masonRole},
		Profile: coreadapter.Profile{Backend: agent.AgentClaude, Timeout: time.Minute}, SessionDirectory: session, Prompt: "Build " + unit}
	return turns.Run(ctx, input)
}

// finished is a file whose existence lets a fake claude report.
func finished(t *testing.T, ready bool) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "finish")
	if ready {
		must(t, os.WriteFile(path, nil, 0600))
	}
	return path
}

// created tells whether args, the arguments of an sbx create, begin with the
// options of a create from the template with the size given, and the agent,
// and if so returns the workspaces after them.
func created(args []string, image string, size ...string) ([]string, bool) {
	if len(args) < 4 {
		return nil, false
	}
	want := []string{"create", "--no-progress", "--name", args[3], "--skills", "off", "--template", image}
	want = append(append(want, size...), agent.AgentClaude)
	if len(args) <= len(want) || !slices.Equal(args[:len(want)], want) {
		return nil, false
	}
	return args[len(want):], true
}

// A mason a loaded configuration gives an image, Dagger, cpus and memory runs
// its turn in a sandbox created with that size, mounting a view whose root
// holds the empty Git repository. Nothing under that repository, which the
// turn writes to, reaches the unit's workspace or its candidate.
func TestSizedDaggerMasonTurnRunsThroughTheTurnPathInAnEmptyRepository(t *testing.T) {
	ctx := context.Background()
	f := newUnitsFixture(t)
	units := f.units()
	w, _, err := units.open(ctx, stream, "u1")
	must(t, err)
	pointer, err := os.ReadFile(filepath.Join(w.Path, ".git"))
	must(t, err)
	cfg := configLoader(t, f)(sizedRoles("cpus = 2\nmemory = '4g'\n"))
	_, execution, err := cfg.Execution(masonRole, "default")
	must(t, err)
	if execution.Mode != agent.SandboxSbx || execution.Image != "example/osmia:1" || execution.Dagger == nil || execution.CPUs != 2 || execution.Memory != "4g" {
		t.Fatalf("the mason's execution settings: %+v", execution)
	}

	runner := fakeSbx(t, finished(t, true))
	result, err := f.masonTurnOn(ctx, units, "u1", "turn-1", execution, runner)
	if err != nil || result.IsError || result.FinalResponse != "built" {
		t.Fatalf("the mason turn: %+v %v", result, err)
	}

	args := sbxCreate(t, runner)
	workspaces, ok := created(args, "example/osmia:1", "--cpus", "2", "--memory", "4g")
	if !ok {
		t.Fatalf("sbx create arguments: %q", args)
	}
	views := filepath.Join(f.cfg.Root.String(), "views", "turn-1")
	view := ""
	for _, workspace := range workspaces {
		if strings.HasPrefix(workspace, views+string(filepath.Separator)) {
			view = workspace
		}
	}
	if view == "" || strings.HasSuffix(view, ":ro") {
		t.Fatalf("the sandbox's workspaces %q do not hold the mason's writable view", workspaces)
	}
	if files := lines(t, runner, "view-repository-files.txt"); !slices.Equal(files, []string{".git/HEAD", ".git/config"}) {
		t.Fatalf("the view's repository held %q when its sandbox was created, want HEAD and config alone", files)
	}
	for _, name := range lines(t, runner, "view-repository-top.txt") {
		if !slices.Contains([]string{"HEAD", "config", "objects", "refs"}, name) {
			t.Fatalf("the view's repository held %s when its sandbox was created", name)
		}
	}
	root := lines(t, runner, "view-root.txt")
	if !slices.Contains(root, ".git") || !slices.Contains(root, "README") || slices.Contains(root, "planted") {
		t.Fatalf("the view root held %q when its sandbox was created", root)
	}
	if _, err := os.Lstat(view); !isNotExist(err) {
		t.Fatalf("the view remains after the turn: %v", err)
	}

	if after, err := os.ReadFile(filepath.Join(w.Path, ".git")); err != nil || string(after) != string(pointer) {
		t.Fatalf("the workspace's .git after the turn: %q %v", after, err)
	}
	must(t, filepath.WalkDir(w.Path, func(name string, entry fs.DirEntry, err error) error {
		if err == nil && (entry.Name() == "planted" || entry.Name() == "pre-commit" || entry.Name() == "hooks") {
			t.Errorf("%s in the workspace after the turn", name)
		}
		return err
	}))
	_, _, candidate, err := units.snapshot(ctx, stream, "u1")
	must(t, err)
	if got := strings.TrimSpace(demoGit(t, f.home, "-C", f.cfg.Project.Clone, "diff-tree", "-r", "--name-status", "--no-renames", f.base, candidate)); got != "M\tREADME" {
		t.Fatalf("the candidate after the turn:\n%s", got)
	}
}

func isNotExist(err error) bool { return err != nil && os.IsNotExist(err) }

func hasCreate(args []string, image string, size ...string) bool {
	_, ok := created(args, image, size...)
	return ok
}

// A mason that sets neither cpus nor memory creates its sandbox with the
// command line sbx create always had: no --cpus and no --memory, the same
// options in the same order, whatever the role's template and Dagger.
func TestUnsizedMasonKeepsTheSbxCreateCommandLine(t *testing.T) {
	ctx := context.Background()
	f := newUnitsFixture(t)
	units := f.units()
	_, _, err := units.open(ctx, stream, "u1")
	must(t, err)
	cfg := configLoader(t, f)(sizedRoles(""))
	_, execution, err := cfg.Execution(masonRole, "default")
	must(t, err)
	if execution.CPUs != 0 || execution.Memory != "" {
		t.Fatalf("the mason's execution settings: %+v", execution)
	}

	runner := fakeSbx(t, finished(t, true))
	if result, err := f.masonTurnOn(ctx, units, "u1", "turn-1", execution, runner); err != nil || result.IsError || result.FinalResponse != "built" {
		t.Fatalf("the mason turn: %+v %v", result, err)
	}
	args := sbxCreate(t, runner)
	if _, ok := created(args, "example/osmia:1"); !ok {
		t.Fatalf("sbx create arguments: %q", args)
	}
	for _, arg := range args {
		if arg == "--cpus" || arg == "--memory" {
			t.Fatalf("sbx create is given %s: %q", arg, args)
		}
	}
	if files := lines(t, runner, "view-repository-files.txt"); !slices.Equal(files, []string{".git/HEAD", ".git/config"}) {
		t.Fatalf("the view's repository: %q", files)
	}
}

// A reload gives the sandboxes created after it the reloaded size. A turn
// that was running when the configuration was reloaded keeps the sandbox it
// created with the size it had then.
func TestReloadedSizeAppliesToSandboxesCreatedAfterIt(t *testing.T) {
	ctx := context.Background()
	f := newUnitsFixture(t)
	units := f.units()
	for _, unit := range []string{"u1", "u2"} {
		_, _, err := units.open(ctx, stream, unit)
		must(t, err)
	}
	load := configLoader(t, f)
	before := load(sizedRoles("cpus = 2\nmemory = '4g'\n"))
	_, running, err := before.Execution(masonRole, "default")
	must(t, err)

	// The first turn creates its sandbox, then waits.
	finish := finished(t, false)
	first := fakeSbx(t, finish)
	done := make(chan error, 1)
	go func() {
		result, err := f.masonTurnOn(ctx, units, "u1", "turn-1", running, first)
		if err == nil && result.FinalResponse != "built" {
			err = os.ErrInvalid
		}
		done <- err
	}()
	created := filepath.Join(filepath.Dir(first.SbxBin), "sbx-create.txt")
	deadline := time.Now().Add(30 * time.Second)
	for {
		if _, err := os.Stat(created); err == nil {
			break
		}
		select {
		case err := <-done:
			t.Fatalf("the first turn ended before it created its sandbox: %v", err)
		case <-time.After(10 * time.Millisecond):
		}
		if time.Now().After(deadline) {
			t.Fatal("the first turn never created its sandbox")
		}
	}

	after := load(sizedRoles("cpus = 3\nmemory = '2g'\n"))
	_, reloaded, err := after.Execution(masonRole, "default")
	must(t, err)
	if reloaded.CPUs != 3 || reloaded.Memory != "2g" {
		t.Fatalf("the reloaded settings: %+v", reloaded)
	}
	if running.CPUs != 2 || running.Memory != "4g" {
		t.Fatalf("the running turn's settings changed: %+v", running)
	}
	second := fakeSbx(t, finished(t, true))
	if result, err := f.masonTurnOn(ctx, units, "u2", "turn-2", reloaded, second); err != nil || result.FinalResponse != "built" {
		t.Fatalf("the turn after the reload: %+v %v", result, err)
	}
	if args := sbxCreate(t, second); !hasCreate(args, "example/osmia:1", "--cpus", "3", "--memory", "2g") {
		t.Fatalf("sbx create after the reload: %q", args)
	}

	// The turn that was running finishes in the sandbox it created before.
	must(t, os.WriteFile(finish, nil, 0600))
	if err := <-done; err != nil {
		t.Fatalf("the first turn: %v", err)
	}
	if args := sbxCreate(t, first); !hasCreate(args, "example/osmia:1", "--cpus", "2", "--memory", "4g") {
		t.Fatalf("sbx create before the reload: %q", args)
	}

	// A reload that drops the size gives later sandboxes sbx's own.
	_, unsized, err := load(sizedRoles("")).Execution(masonRole, "default")
	must(t, err)
	third := fakeSbx(t, finished(t, true))
	if result, err := f.masonTurnOn(ctx, units, "u2", "turn-3", unsized, third); err != nil || result.FinalResponse != "built" {
		t.Fatalf("the turn after the second reload: %+v %v", result, err)
	}
	if args := sbxCreate(t, third); !hasCreate(args, "example/osmia:1") || slices.Contains(args, "--cpus") || slices.Contains(args, "--memory") {
		t.Fatalf("sbx create after a reload that dropped the size: %q", args)
	}
}
