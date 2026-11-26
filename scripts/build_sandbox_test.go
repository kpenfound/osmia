package scripts

import (
	"net"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func eachBuild(t *testing.T, fn func(t *testing.T, p buildPath)) {
	t.Helper()
	for _, p := range buildPaths {
		t.Run(p.name, func(t *testing.T) { fn(t, p) })
	}
}

func TestBuildSavesCleanGuest(t *testing.T) {
	eachBuild(t, func(t *testing.T, p buildPath) {
		g := newGuest(t)
		res := p.run(g, t)
		res.wantSaved(t)

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
	})
}

func TestBuildHasNoDockerVariant(t *testing.T) {
	cmd := exec.Command("bash", scriptPath(t), "--docker")
	cmd.Env = []string{"PATH=" + os.Getenv("PATH")}
	out, err := cmd.CombinedOutput()
	if err == nil || !strings.Contains(string(out), "Usage:") {
		t.Fatalf("script accepted an argument: %v\n%s", err, out)
	}
}

func TestBuildRefusesRunningEngine(t *testing.T) {
	eachBuild(t, func(t *testing.T, p buildPath) {
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
				p.run(g, t).wantRefused(t, "Refusing to save template osmia:latest", "found processes", tt.want)
			})
		}
	})
}

func TestBuildRefusesReachableDaemonWithoutDockerExecutable(t *testing.T) {
	eachBuild(t, func(t *testing.T, p buildPath) {
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
				p.run(g, t).wantRefused(t, "Refusing to save template osmia:latest", "found endpoints")
			})
		}
	})
}

func TestBuildRefusesLeftoverEngineExecutables(t *testing.T) {
	eachBuild(t, func(t *testing.T, p buildPath) {
		for _, bin := range []string{"usr/bin/docker", "usr/bin/dockerd", "usr/local/bin/containerd"} {
			t.Run(bin, func(t *testing.T) {
				g := newGuest(t)
				g.file(bin)
				p.run(g, t).wantRefused(t, "found binaries", "/"+bin)
			})
		}
	})
}

func TestBuildRefusesWhenAProbeCannotAnswer(t *testing.T) {
	eachBuild(t, func(t *testing.T, p buildPath) {
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
				p.run(g, t).wantRefused(t, "Refusing to save template osmia:latest", tt.want)
			})
		}
	})
}

func TestBuildSavesWhenNoDaemonListens(t *testing.T) {
	eachBuild(t, func(t *testing.T, p buildPath) {
		g := newGuest(t)
		g.serve(t, "var/run/docker.sock", pingOK, true)
		g.dockerHost = "tcp://127.0.0.1:1"
		p.run(g, t).wantSaved(t)
	})
}

func TestBuildKeepsVersionChecks(t *testing.T) {
	eachBuild(t, func(t *testing.T, p buildPath) {
		t.Run("claude missing", func(t *testing.T) {
			g := newGuest(t)
			g.noClaude = true
			p.run(g, t).wantRefused(t)
		})
		t.Run("wrong dagger release", func(t *testing.T) {
			g := newGuest(t)
			g.daggerTag = "v0.1.0"
			p.run(g, t).wantRefused(t, "Expected Dagger v0.19.0")
		})
	})
}

func TestBuildChecksTheCodexMinimum(t *testing.T) {
	eachBuild(t, func(t *testing.T, p buildPath) {
		for _, tt := range []struct {
			name, version string
			saved         bool
		}{
			{"older release", "codex-cli 0.135.0", false},
			{"older patch of an older minor", "codex-cli 0.99.9", false},
			{"unreadable version", "codex-cli unknown", false},
			{"minimum release", "codex-cli 0.136.0", true},
			{"newer patch", "codex-cli 0.136.1", true},
			{"newer minor with prerelease", "codex-cli 0.173.0-alpha.4", true},
			{"newer major", "codex-cli 1.0.0", true},
		} {
			t.Run(tt.name, func(t *testing.T) {
				g := newGuest(t)
				g.codexOut = tt.version
				res := p.run(g, t)
				if tt.saved {
					res.wantSaved(t)
					return
				}
				res.wantRefused(t, "0.136.0", "skills/extraRoots/set")
				if n := res.count("claude --version"); n != 0 {
					t.Errorf("build went on after the Codex check: %v", res.calls)
				}
			})
		}
	})
}
