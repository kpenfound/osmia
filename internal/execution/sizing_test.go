package execution

import (
	"context"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/kpenfound/agentcore/agent"
	"github.com/kpenfound/agentcore/agent/agenttest"
	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/isolation"
	"github.com/kpenfound/osmia/internal/projectstore"
)

func sizedRoles(sizing string) string {
	return "[roles.mason]\nsandbox = 'sbx'\nimage = 'example/osmia:1'\n" + sizing +
		"[dagger]\nversion = 'v0.20.5'\nengine = 'tcp://127.0.0.1:1234'\n"
}

func configLoader(t *testing.T, f unitsFixture) func(roles string) *config.Config {
	t.Helper()
	file := filepath.Join(f.cfg.Root.String(), "config.toml")
	base, err := os.ReadFile(file)
	must(t, err)
	return func(roles string) *config.Config {
		t.Helper()
		must(t, os.WriteFile(file, append(slices.Clone(base), roles...), 0600))
		cfg, err := projectstore.LoadConfig(config.Options{Root: f.cfg.Root.String()})
		must(t, err)
		return cfg
	}
}

const viewHook = `
if [ "$1" = create ]; then
  if [ -p "$here/created" ]; then echo created > "$here/created"; fi
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

func (f unitsFixture) masonTurnOn(ctx context.Context, units unitWorkspaces, unit, turn string, execution coreadapter.ExecutionSettings, runner agent.Runner) (coreadapter.SessionResult, error) {
	session := filepath.Join(f.home, "session-"+turn)
	turns := &isolation.Turns{
		Workspaces: units,
		Views:      isolation.Views{Directory: filepath.Join(f.cfg.Root.String(), "views", turn)},
		Select: func(ctx context.Context, scope coreadapter.Scope) (isolation.Selection, error) {
			w, _, _, err := units.Find(ctx, config.WorkstreamID(scope.Workstream), scope.Unit)
			if err != nil {
				return isolation.Selection{}, err
			}
			paths, err := units.Paths(w)
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

func finished(t *testing.T, ready bool) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "finish")
	if ready {
		must(t, os.WriteFile(path, nil, 0600))
	}
	return path
}

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
	_, _, candidate, err := units.Snapshot(ctx, stream, "u1")
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

	finish := finished(t, false)
	first := fakeSbx(t, finish)
	createdPipe := filepath.Join(filepath.Dir(first.SbxBin), "created")
	must(t, syscall.Mkfifo(createdPipe, 0o600))
	createdSignal := make(chan struct{})
	go func() {
		pipe, err := os.Open(createdPipe)
		if err == nil {
			_, _ = io.Copy(io.Discard, pipe)
			pipe.Close()
		}
		close(createdSignal)
	}()
	done := make(chan error, 1)
	go func() {
		result, err := f.masonTurnOn(ctx, units, "u1", "turn-1", running, first)
		if err == nil && result.FinalResponse != "built" {
			err = os.ErrInvalid
		}
		done <- err
	}()
	select {
	case <-createdSignal:
	case err := <-done:
		t.Fatalf("the first turn ended before it created its sandbox: %v", err)
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

	must(t, os.WriteFile(finish, nil, 0600))
	if err := <-done; err != nil {
		t.Fatalf("the first turn: %v", err)
	}
	if args := sbxCreate(t, first); !hasCreate(args, "example/osmia:1", "--cpus", "2", "--memory", "4g") {
		t.Fatalf("sbx create before the reload: %q", args)
	}

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
