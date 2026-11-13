package scripts

import (
	_ "embed"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// buildScript is scripts/build-sandbox.sh itself. The Go check's container
// carries only Go sources, testdata and embedded files, so the test embeds the
// script and runs it from a temporary directory.
//
//go:embed build-sandbox.sh
var buildScript []byte

// scriptPath writes the embedded script to a temporary file and returns it.
func scriptPath(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "build-sandbox.sh")
	must(os.WriteFile(path, buildScript, 0o755))
	return path
}

// guest is the directory the fake sbx presents as the build sandbox's
// filesystem.
type guest struct {
	root       string
	dockerHost string
	probe      string
	noClaude   bool
	daggerTag  string
}

func newGuest(t *testing.T) *guest {
	t.Helper()
	// Unix socket paths are short, so the guest lives directly under /tmp.
	root, err := os.MkdirTemp("", "g")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(root) })
	g := &guest{root: root}
	g.process(1, "systemd", "/sbin/init")
	g.process(2, "bash", "bash")
	for _, dir := range []string{"run", "tmp", "home/agent", "root", "usr/bin"} {
		if err := os.MkdirAll(filepath.Join(root, dir), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return g
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

// serve answers HTTP on a unix socket inside the guest and returns its guest
// path. A closed server leaves the socket file behind when stale is set.
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

// run executes the real scripts/build-sandbox.sh with the fake sbx and a fake
// host dagger first on PATH.
func (g *guest) run(t *testing.T) result {
	t.Helper()
	for _, tool := range []string{"bash", "curl", "find"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Fatalf("%s is required: %v", tool, err)
		}
	}
	bin := t.TempDir()
	fake, err := os.ReadFile("testdata/fake-sbx")
	if err != nil {
		t.Fatal(err)
	}
	must(os.WriteFile(filepath.Join(bin, "sbx"), fake, 0o755))
	must(os.WriteFile(filepath.Join(bin, "dagger"), []byte("#!/bin/sh\necho 'version:     v0.19.0'\n"), 0o755))
	tmp := t.TempDir()
	log := filepath.Join(t.TempDir(), "calls")
	cmd := exec.Command("bash", scriptPath(t))
	env := []string{
		"PATH=" + bin + string(os.PathListSeparator) + os.Getenv("PATH"),
		"HOME=" + t.TempDir(),
		"TMPDIR=" + tmp,
		"FAKE_SBX_LOG=" + log,
		"FAKE_GUEST_ROOT=" + g.root,
		"FAKE_GUEST_DOCKER_HOST=" + g.dockerHost,
		"FAKE_SBX_PROBE=" + g.probe,
	}
	if g.noClaude {
		env = append(env, "FAKE_SBX_NO_CLAUDE=1")
	}
	if g.daggerTag != "" {
		env = append(env, "FAKE_SBX_DAGGER_VERSION="+g.daggerTag)
	}
	cmd.Env = env
	out, runErr := cmd.CombinedOutput()
	data, _ := os.ReadFile(log)
	return result{output: string(out), err: runErr, calls: strings.Split(strings.TrimSpace(string(data)), "\n"), tmp: tmp}
}

// wantSaved asserts a clean build: exactly one template save, as the last
// sbx call before the sandbox is removed.
func (r result) wantSaved(t *testing.T) {
	t.Helper()
	if r.err != nil {
		t.Fatalf("script failed: %v\n%s", r.err, r.output)
	}
	if n := r.count("template save "); n != 1 {
		t.Fatalf("template saves = %d, want 1\n%v", n, r.calls)
	}
	r.wantRemoved(t)
}

// wantRefused asserts the script failed naming want and never saved, and that
// the temporary sandbox and its directory were removed.
func (r result) wantRefused(t *testing.T, want ...string) {
	t.Helper()
	if r.err == nil {
		t.Fatalf("script succeeded, want refusal\n%s", r.output)
	}
	if n := r.count("template save"); n != 0 {
		t.Fatalf("template saved %d times on refusal\n%s", n, r.output)
	}
	if n := r.count("stop"); n != 0 {
		t.Fatalf("sandbox stopped on refusal: %v", r.calls)
	}
	for _, w := range want {
		if !strings.Contains(r.output, w) {
			t.Errorf("output does not name %q:\n%s", w, r.output)
		}
	}
	r.wantRemoved(t)
}

func (r result) wantRemoved(t *testing.T) {
	t.Helper()
	var created, removed string
	for _, c := range r.calls {
		if f := strings.Fields(c); len(f) > 2 && f[0] == "create" && f[1] == "--name" {
			created = f[2]
		}
		if f := strings.Fields(c); len(f) == 3 && f[0] == "rm" && f[1] == "--force" {
			removed = f[2]
		}
	}
	if created == "" || created != removed {
		t.Errorf("created sandbox %q, removed %q\n%v", created, removed, r.calls)
	}
	if entries, _ := os.ReadDir(r.tmp); len(entries) != 0 {
		t.Errorf("temporary build directory left behind: %v", entries)
	}
}

func TestBuildSandboxSavesCleanGuest(t *testing.T) {
	g := newGuest(t)
	res := g.run(t)
	res.wantSaved(t)

	// Docker is removed first, the version checks and both users' probes come
	// before the save, and the save is the last step before cleanup.
	steps := []string{
		"create --name",
		"osmia-docker-removal",
		"codex --version",
		"claude --version",
		"dagger version",
		"-u root",
		"osmia-docker-probe",
		"stop ",
		"template save ",
		"rm --force",
	}
	at := -1
	for _, step := range steps {
		next := -1
		for i := at + 1; i < len(res.calls); i++ {
			if strings.Contains(res.calls[i], step) {
				next = i
				break
			}
		}
		if next < 0 {
			t.Fatalf("no call containing %q after call %d in %v", step, at, res.calls)
		}
		at = next
	}
	probes := 0
	for _, c := range res.calls {
		if strings.Contains(c, "osmia-docker-probe") {
			probes++
		}
	}
	if probes != 2 {
		t.Errorf("probe ran %d times, want once as root and once as the agent user: %v", probes, res.calls)
	}
	if got := res.calls[len(res.calls)-1]; !strings.HasPrefix(got, "rm --force") {
		t.Errorf("last call is %q, want the sandbox removal", got)
	}
}

func TestBuildSandboxHasNoDockerVariant(t *testing.T) {
	cmd := exec.Command("bash", scriptPath(t), "--docker")
	cmd.Env = []string{"PATH=" + os.Getenv("PATH")}
	out, err := cmd.CombinedOutput()
	if err == nil || !strings.Contains(string(out), "Usage:") {
		t.Fatalf("script accepted an argument: %v\n%s", err, out)
	}
}

func TestBuildSandboxRefusesRunningEngine(t *testing.T) {
	tests := []struct {
		name  string
		setup func(g *guest)
		want  string
	}{
		{"dockerd", func(g *guest) { g.process(30, "dockerd", "/usr/bin/dockerd") }, "dockerd"},
		{"containerd", func(g *guest) { g.process(31, "containerd", "/usr/bin/containerd") }, "containerd"},
		{"containerd shim", func(g *guest) { g.process(32, "containerd-shim", "/usr/bin/containerd-shim-runc-v2") }, "containerd-shim"},
		{"engine hidden behind another process name", func(g *guest) { g.process(33, "exe", "/usr/bin/dockerd") }, "found processes"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g := newGuest(t)
			tt.setup(g)
			g.run(t).wantRefused(t, "Refusing to save template osmia:latest", "found processes", tt.want)
		})
	}
}

func TestBuildSandboxRefusesReachableDaemonWithoutDockerExecutable(t *testing.T) {
	tests := []struct {
		name  string
		setup func(t *testing.T, g *guest)
	}{
		{"default socket", func(t *testing.T, g *guest) { g.serve(t, "var/run/docker.sock", pingOK, false) }},
		{"run socket", func(t *testing.T, g *guest) { g.serve(t, "run/docker.sock", pingOK, false) }},
		{"DOCKER_HOST unix socket", func(t *testing.T, g *guest) {
			g.serve(t, "tmp/custom.sock", pingOK, false)
			g.dockerHost = "unix:///tmp/custom.sock"
		}},
		{"DOCKER_HOST tcp address", func(t *testing.T, g *guest) {
			srv := httptest.NewServer(pingOK)
			t.Cleanup(srv.Close)
			g.dockerHost = "tcp://" + strings.TrimPrefix(srv.URL, "http://")
		}},
		{"forwarded socket under a user directory", func(t *testing.T, g *guest) {
			g.serve(t, "run/user/1000/docker.sock", pingOK, false)
		}},
		{"relocated socket", func(t *testing.T, g *guest) { g.serve(t, "tmp/forwarded/dockerd-fwd.sock", pingOK, false) }},
		{"containerd socket", func(t *testing.T, g *guest) { g.serve(t, "run/containerd/containerd.sock", pingBroken, false) }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g := newGuest(t)
			tt.setup(t, g)
			if _, err := os.Stat(filepath.Join(g.root, "usr/bin/docker")); err == nil {
				t.Fatal("the guest has a docker executable")
			}
			g.run(t).wantRefused(t, "Refusing to save template osmia:latest", "found endpoints")
		})
	}
}

func TestBuildSandboxRefusesLeftoverEngineExecutables(t *testing.T) {
	for _, bin := range []string{"usr/bin/docker", "usr/bin/dockerd", "usr/local/bin/containerd"} {
		t.Run(bin, func(t *testing.T) {
			g := newGuest(t)
			g.file(bin)
			g.run(t).wantRefused(t, "found binaries", "/"+bin)
		})
	}
}

func TestBuildSandboxRefusesWhenAProbeCannotAnswer(t *testing.T) {
	tests := []struct {
		name  string
		setup func(t *testing.T, g *guest)
		want  string
	}{
		{"sbx exec fails", func(t *testing.T, g *guest) { g.probe = "fail" }, "the probe could not run"},
		{"probe prints nothing", func(t *testing.T, g *guest) { g.probe = "silent" }, "gave no answer"},
		{"probe stops early", func(t *testing.T, g *guest) { g.probe = "truncated" }, "did not run to completion"},
		{"processes are not visible", func(t *testing.T, g *guest) {
			must(os.RemoveAll(filepath.Join(g.root, "proc")))
		}, "unknown processes"},
		{"socket answers something else", func(t *testing.T, g *guest) { g.serve(t, "var/run/docker.sock", pingBroken, false) }, "unknown endpoints"},
		{"DOCKER_HOST scheme cannot be asked", func(t *testing.T, g *guest) { g.dockerHost = "ssh://user@elsewhere" }, "unknown endpoints"},
		{"socket path is not a socket", func(t *testing.T, g *guest) { g.file("var/run/docker.sock") }, "not a socket"},
		{"DOCKER_HOST tcp address times out", func(t *testing.T, g *guest) {
			ln, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { ln.Close() })
			// Accepts connections but never answers.
			go func() {
				for {
					c, err := ln.Accept()
					if err != nil {
						return
					}
					t.Cleanup(func() { c.Close() })
				}
			}()
			g.dockerHost = "tcp://" + ln.Addr().String()
		}, "curl exit 28"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g := newGuest(t)
			tt.setup(t, g)
			g.run(t).wantRefused(t, "Refusing to save template osmia:latest", tt.want)
		})
	}
}

func TestBuildSandboxSavesWhenNoDaemonListens(t *testing.T) {
	g := newGuest(t)
	// A socket file nobody listens on is a clear absence.
	g.serve(t, "var/run/docker.sock", pingOK, true)
	// An unresolvable or unused DOCKER_HOST address is too.
	g.dockerHost = "tcp://127.0.0.1:1"
	g.run(t).wantSaved(t)
}

func TestBuildSandboxKeepsVersionChecks(t *testing.T) {
	t.Run("claude missing", func(t *testing.T) {
		g := newGuest(t)
		g.noClaude = true
		g.run(t).wantRefused(t)
	})
	t.Run("wrong dagger release", func(t *testing.T) {
		g := newGuest(t)
		g.daggerTag = "v0.1.0"
		g.run(t).wantRefused(t, "Expected Dagger v0.19.0")
	})
}
