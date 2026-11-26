package scripts

import (
	"bytes"
	"context"
	_ "embed"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kpenfound/osmia/internal/sandbox"
)

//go:embed build-sandbox.sh
var buildScript []byte

func scriptPath(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "build-sandbox.sh")
	must(os.WriteFile(path, buildScript, 0o755))
	return path
}

type guest struct {
	root       string
	dockerHost string
	probe      string
	noClaude   bool
	daggerTag  string
	codexOut   string

	// claude is what Claude Code prints in a sandbox made from the base image.
	claude string
	// hostDagger is what the host's `dagger version` prints.
	hostDagger  string
	noSbx       bool
	noDagger    bool
	saveFails   bool
	createFail  bool
	removeFails bool

	state string
	bin   string
	home  string
}

const earlierSandbox = "running-earlier"

func newGuest(t *testing.T) *guest {
	t.Helper()
	root, err := os.MkdirTemp("", "g")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(root) })
	g := &guest{root: root, state: t.TempDir(), bin: t.TempDir(), home: t.TempDir()}
	g.process(1, "systemd", "/sbin/init")
	g.process(2, "bash", "bash")
	for _, dir := range []string{"run", "tmp", "home/agent", "root", "usr/bin"} {
		if err := os.MkdirAll(filepath.Join(root, dir), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	// A sandbox already running from an earlier template: no build or check
	// may create, stop, remove or run anything in it.
	must(os.MkdirAll(filepath.Join(g.state, "sandboxes", earlierSandbox), 0o755))
	must(os.WriteFile(filepath.Join(g.state, "sandboxes", earlierSandbox, "claude"), []byte("1.0.0 (Claude Code)\n"), 0o644))
	fake, err := os.ReadFile("testdata/fake-sbx")
	if err != nil {
		t.Fatal(err)
	}
	must(os.WriteFile(filepath.Join(g.bin, "sbx"), fake, 0o755))
	return g
}

// savedTemplate is what Claude Code prints in the saved osmia:latest, or ""
// when no template is saved.
func (g *guest) savedTemplate() string {
	data, err := os.ReadFile(filepath.Join(g.state, "templates", "osmia_latest", "claude"))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}

func (g *guest) saveTemplate(claude string) {
	dir := filepath.Join(g.state, "templates", "osmia_latest")
	must(os.MkdirAll(dir, 0o755))
	must(os.WriteFile(filepath.Join(dir, "claude"), []byte(claude+"\n"), 0o644))
}

func (g *guest) removeTemplate() {
	must(os.RemoveAll(filepath.Join(g.state, "templates", "osmia_latest")))
}

// sandboxes lists the sandboxes that exist now.
func (g *guest) sandboxes() []string {
	entries, err := os.ReadDir(filepath.Join(g.state, "sandboxes"))
	must(err)
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names
}

func (g *guest) process(pid int, comm, argv0 string) {
	dir := filepath.Join(g.root, "proc", fmt.Sprint(pid))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		panic(err)
	}
	must(os.WriteFile(filepath.Join(dir, "comm"), []byte(comm+"\n"), 0o644))
	must(os.WriteFile(filepath.Join(dir, "cmdline"), []byte(argv0+"\x00--flag\x00"), 0o644))
}

func (g *guest) file(rel string) {
	path := filepath.Join(g.root, rel)
	must(os.MkdirAll(filepath.Dir(path), 0o755))
	must(os.WriteFile(path, []byte("#!/bin/sh\n"), 0o755))
}

func must(err error) {
	if err != nil {
		panic(err)
	}
}

func (g *guest) serve(t *testing.T, rel string, handler http.Handler, stale bool) {
	t.Helper()
	path := filepath.Join(g.root, rel)
	must(os.MkdirAll(filepath.Dir(path), 0o755))
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	ln.(*net.UnixListener).SetUnlinkOnClose(!stale)
	if stale {
		ln.Close()
		return
	}
	srv := &http.Server{Handler: handler}
	go srv.Serve(ln)
	t.Cleanup(func() { srv.Close() })
}

var pingOK = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/_ping" {
		fmt.Fprint(w, "OK")
		return
	}
	http.NotFound(w, r)
})

var pingBroken = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
	http.Error(w, "boom", http.StatusInternalServerError)
})

type result struct {
	output string
	err    error
	calls  []string
	tmp    string
	// script is set when the result is of scripts/build-sandbox.sh and not of
	// an osmia command.
	script bool
}

func (r result) count(prefix string) int {
	n := 0
	for _, c := range r.calls {
		if strings.HasPrefix(c, prefix) {
			n++
		}
	}
	return n
}

func (r result) has(sub string) bool {
	for _, c := range r.calls {
		if strings.Contains(c, sub) {
			return true
		}
	}
	return false
}

// environment is the environment of one run: the fake sbx and dagger in front
// of the real tools the script and the probe need.
func (g *guest) environment(t *testing.T) (env []string, log string) {
	t.Helper()
	for _, tool := range []string{"bash", "curl", "find"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Fatalf("%s is required: %v", tool, err)
		}
	}
	dagger := g.hostDagger
	if dagger == "" {
		dagger = "version:     v0.19.0"
	}
	must(os.WriteFile(filepath.Join(g.bin, "dagger"), []byte("#!/bin/sh\necho '"+dagger+"'\n"), 0o755))
	path := g.bin + string(os.PathListSeparator) + os.Getenv("PATH")
	if g.noSbx || g.noDagger {
		only := t.TempDir()
		for _, tool := range []string{"sbx", "dagger"} {
			if g.noSbx && tool == "sbx" || g.noDagger && tool == "dagger" {
				continue
			}
			data, err := os.ReadFile(filepath.Join(g.bin, tool))
			must(err)
			must(os.WriteFile(filepath.Join(only, tool), data, 0o755))
		}
		path = only
	}
	log = filepath.Join(t.TempDir(), "calls")
	env = []string{
		"PATH=" + path,
		"HOME=" + g.home,
		"FAKE_SBX_LOG=" + log,
		"FAKE_SBX_STATE=" + g.state,
		"FAKE_GUEST_ROOT=" + g.root,
		"FAKE_GUEST_DOCKER_HOST=" + g.dockerHost,
		"FAKE_SBX_PROBE=" + g.probe,
	}
	if g.noClaude {
		env = append(env, "FAKE_SBX_NO_CLAUDE=1")
	}
	if g.claude != "" {
		env = append(env, "FAKE_SBX_CLAUDE="+g.claude)
	}
	if g.daggerTag != "" {
		env = append(env, "FAKE_SBX_DAGGER_VERSION="+g.daggerTag)
	}
	if g.codexOut != "" {
		env = append(env, "FAKE_SBX_CODEX_OUTPUT="+g.codexOut)
	}
	if g.saveFails {
		env = append(env, "FAKE_SBX_SAVE_FAIL=1")
	}
	if g.createFail {
		env = append(env, "FAKE_SBX_CREATE_FAIL=1")
	}
	if g.removeFails {
		env = append(env, "FAKE_SBX_RM_STATUS=1")
	}
	return env, log
}

// leakSandbox leaves a sandbox behind, as an interrupted build would.
func (g *guest) leakSandbox(name, claude string) {
	dir := filepath.Join(g.state, "sandboxes", name)
	must(os.MkdirAll(dir, 0o755))
	must(os.WriteFile(filepath.Join(dir, "claude"), []byte(claude+"\n"), 0o644))
}

func calls(log string) []string {
	data, _ := os.ReadFile(log)
	if strings.TrimSpace(string(data)) == "" {
		return nil
	}
	return strings.Split(strings.TrimSpace(string(data)), "\n")
}

// runScript runs scripts/build-sandbox.sh, the manual path.
func (g *guest) runScript(t *testing.T) result {
	t.Helper()
	env, log := g.environment(t)
	tmp := t.TempDir()
	cmd := exec.Command("bash", scriptPath(t))
	cmd.Env = append(env, "TMPDIR="+tmp)
	out, runErr := cmd.CombinedOutput()
	return result{output: string(out), err: runErr, calls: calls(log), tmp: tmp, script: true}
}

// runBuild runs the build the way osmia sandbox build does.
func (g *guest) runBuild(t *testing.T) result {
	t.Helper()
	return g.runHost(t, sandbox.Host.Build)
}

// runCheck runs the readiness check the way osmia sandbox check does.
func (g *guest) runCheck(t *testing.T) result {
	t.Helper()
	return g.runHost(t, sandbox.Host.Check)
}

func (g *guest) runHost(t *testing.T, run func(sandbox.Host, context.Context) error) result {
	t.Helper()
	env, log := g.environment(t)
	var out bytes.Buffer
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	err := run(sandbox.Host{Env: env, Stdout: &out}, ctx)
	if err != nil {
		out.WriteString(err.Error() + "\n")
	}
	return result{output: out.String(), err: err, calls: calls(log)}
}

// buildPaths are the two ways to build the template.
type buildPath struct {
	name string
	run  func(*guest, *testing.T) result
}

var buildPaths = []buildPath{
	{"script", (*guest).runScript},
	{"command", (*guest).runBuild},
}

func (r result) wantSaved(t *testing.T) {
	t.Helper()
	if r.err != nil {
		t.Fatalf("build failed: %v\n%s", r.err, r.output)
	}
	if n := r.count("template save "); n != 1 {
		t.Fatalf("template saves = %d, want 1\n%v", n, r.calls)
	}
	r.wantRemoved(t)
}

func (r result) wantRefused(t *testing.T, want ...string) {
	t.Helper()
	if r.err == nil {
		t.Fatalf("build succeeded, want refusal\n%s", r.output)
	}
	if n := r.count("template save"); n != 0 {
		t.Fatalf("template saved %d times on refusal\n%s", n, r.output)
	}
	if n := r.count("stop"); n != 0 {
		t.Fatalf("sandbox stopped on refusal: %v", r.calls)
	}
	r.wantReason(t, want...)
	r.wantRemoved(t)
}

// wantReason checks that the output names every want, ignoring case.
func (r result) wantReason(t *testing.T, want ...string) {
	t.Helper()
	for _, w := range want {
		if !strings.Contains(strings.ToLower(r.output), strings.ToLower(w)) {
			t.Errorf("output does not name %q:\n%s", w, r.output)
		}
	}
}

// sandboxOf returns the sandbox a call names, or "" for a call that names
// none.
func sandboxOf(call string) string {
	f := strings.Fields(call)
	if len(f) == 0 {
		return ""
	}
	switch f[0] {
	case "create":
		for i, w := range f {
			if w == "--name" && i+1 < len(f) {
				return f[i+1]
			}
		}
	case "stop":
		return f[len(f)-1]
	case "rm":
		return f[len(f)-1]
	case "template":
		if len(f) == 4 && f[1] == "save" {
			return f[2]
		}
	case "exec":
		for i := 1; i < len(f); i++ {
			if f[i] == "-u" {
				i++
				continue
			}
			if strings.HasPrefix(f[i], "-") {
				continue
			}
			return f[i]
		}
	}
	return ""
}

// wantRemoved checks that the run created one sandbox, removed that sandbox,
// and created, stopped, removed and ran commands in no other.
func (r result) wantRemoved(t *testing.T) {
	t.Helper()
	var created, removed string
	for _, c := range r.calls {
		f := strings.Fields(c)
		if len(f) > 0 && f[0] == "create" {
			created = sandboxOf(c)
		}
		if len(f) == 3 && f[0] == "rm" && f[1] == "--force" {
			removed = f[2]
		}
	}
	if created == "" || created != removed {
		t.Errorf("created sandbox %q, removed %q\n%v", created, removed, r.calls)
	}
	for _, c := range r.calls {
		if name := sandboxOf(c); name != "" && name != created {
			t.Errorf("call %q names sandbox %q, not the run's own %q", c, name, created)
		}
	}
	if r.tmp != "" {
		if entries, _ := os.ReadDir(r.tmp); len(entries) != 0 {
			t.Errorf("temporary build directory left behind: %v", entries)
		}
	}
}

// wantEarlierSandboxUntouched checks that no call named the sandbox that was
// already running and that it still exists, and that no sandbox of the run is
// left.
func (g *guest) wantEarlierSandboxUntouched(t *testing.T, r result) {
	t.Helper()
	for _, c := range r.calls {
		if strings.Contains(c, earlierSandbox) {
			t.Errorf("call %q touches a sandbox that was already running", c)
		}
	}
	if got := g.sandboxes(); len(got) != 1 || got[0] != earlierSandbox {
		t.Errorf("sandboxes after the run = %v, want only %s", got, earlierSandbox)
	}
}
