package scripts

import (
	"regexp"
	"strings"
	"testing"

	"github.com/kpenfound/osmia/internal/sandbox"
)

func TestBuildRefusesWithoutItsTools(t *testing.T) {
	eachBuild(t, func(t *testing.T, p buildPath) {
		t.Run("sbx missing", func(t *testing.T) {
			g := newGuest(t)
			g.noSbx = true
			res := p.run(g, t)
			if res.err == nil || !strings.Contains(res.output, "sbx is required on PATH") {
				t.Fatalf("err %v, output %q", res.err, res.output)
			}
			if len(res.calls) != 0 {
				t.Errorf("sbx was called: %v", res.calls)
			}
		})
		t.Run("dagger missing", func(t *testing.T) {
			g := newGuest(t)
			g.noDagger = true
			res := p.run(g, t)
			if res.err == nil || !strings.Contains(res.output, "dagger is required on PATH") {
				t.Fatalf("err %v, output %q", res.err, res.output)
			}
			if len(res.calls) != 0 {
				t.Errorf("sbx was called before the host Dagger version was read: %v", res.calls)
			}
		})
	})
}

func TestBuildRefusesAnUnreadableHostDaggerVersion(t *testing.T) {
	eachBuild(t, func(t *testing.T, p buildPath) {
		g := newGuest(t)
		g.hostDagger = "dagger without a version line"
		res := p.run(g, t)
		if res.err == nil || !strings.Contains(res.output, "Could not read the host Dagger version") && !strings.Contains(res.output, "could not read the host Dagger version") {
			t.Fatalf("err %v, output %q", res.err, res.output)
		}
		if len(res.calls) != 0 {
			t.Errorf("sbx was called: %v", res.calls)
		}
	})
}

func TestBuildInstallsTheHostDaggerRelease(t *testing.T) {
	eachBuild(t, func(t *testing.T, p buildPath) {
		g := newGuest(t)
		g.hostDagger = "version:     v0.21.3"
		g.daggerTag = "v0.21.3"
		res := p.run(g, t)
		res.wantSaved(t)
		if !res.has("DAGGER_VERSION=0.21.3 ") {
			t.Errorf("the template did not install the host's Dagger release: %v", res.calls)
		}

		g = newGuest(t)
		g.hostDagger = "version:     v0.21.3"
		g.daggerTag = "v0.19.0"
		p.run(g, t).wantRefused(t, "Expected Dagger v0.21.3")
	})
}

func TestBuildFailedSaveLeavesThePreviousTemplateInPlace(t *testing.T) {
	eachBuild(t, func(t *testing.T, p buildPath) {
		g := newGuest(t)
		g.saveTemplate("2.1.400 (Claude Code)")
		g.saveFails = true
		g.claude = "2.9.0 (Claude Code)"
		res := p.run(g, t)
		if res.err == nil {
			t.Fatalf("build succeeded although the save failed\n%s", res.output)
		}
		if res.count("template save ") != 1 {
			t.Errorf("the build did not try to save: %v", res.calls)
		}
		res.wantReason(t, "template osmia:latest")
		res.wantRemoved(t)
		if got := g.savedTemplate(); got != "2.1.400 (Claude Code)" {
			t.Errorf("saved template after the failed save = %q", got)
		}
		g.wantEarlierSandboxUntouched(t, res)
	})
}

func TestBuildRefusalLeavesThePreviousTemplateInPlace(t *testing.T) {
	eachBuild(t, func(t *testing.T, p buildPath) {
		g := newGuest(t)
		g.saveTemplate("2.1.400 (Claude Code)")
		g.process(30, "dockerd", "/usr/bin/dockerd")
		g.claude = "2.9.0 (Claude Code)"
		res := p.run(g, t)
		res.wantRefused(t, "found processes")
		if got := g.savedTemplate(); got != "2.1.400 (Claude Code)" {
			t.Errorf("saved template after the refusal = %q", got)
		}
		g.wantEarlierSandboxUntouched(t, res)
	})
}

func TestBuildFailsWhenTheSandboxCannotBeCreated(t *testing.T) {
	eachBuild(t, func(t *testing.T, p buildPath) {
		g := newGuest(t)
		g.createFail = true
		res := p.run(g, t)
		if res.err == nil || res.count("template save") != 0 {
			t.Fatalf("err %v, calls %v", res.err, res.calls)
		}
		g.wantEarlierSandboxUntouched(t, res)
	})
}

func TestBuildTouchesOnlyItsOwnSandbox(t *testing.T) {
	eachBuild(t, func(t *testing.T, p buildPath) {
		g := newGuest(t)
		g.saveTemplate("2.0.0 (Claude Code)")
		res := p.run(g, t)
		res.wantSaved(t)
		g.wantEarlierSandboxUntouched(t, res)
		own := regexp.MustCompile(`^osmia-build[.-][A-Za-z0-9]{8}$`)
		named := 0
		for _, c := range res.calls {
			if name := sandboxOf(c); name != "" {
				named++
				if !own.MatchString(name) {
					t.Errorf("call %q names %q, not a temporary build sandbox", c, name)
				}
			}
		}
		if named == 0 {
			t.Fatal("no call named a sandbox")
		}
		if got := g.savedTemplate(); got != "2.1.321 (Claude Code)" {
			t.Errorf("saved template = %q, want the new build", got)
		}
	})
}

func TestBuildSavesOnlyAfterStoppingItsSandbox(t *testing.T) {
	eachBuild(t, func(t *testing.T, p buildPath) {
		g := newGuest(t)
		res := p.run(g, t)
		res.wantSaved(t)
		stop, save := -1, -1
		for i, c := range res.calls {
			if strings.HasPrefix(c, "stop ") {
				stop = i
			}
			if strings.HasPrefix(c, "template save ") {
				save = i
			}
		}
		if stop < 0 || save < stop {
			t.Errorf("stop at %d, save at %d: %v", stop, save, res.calls)
		}
	})
}

func TestBuildCommandReportsItsReasonOnOneLine(t *testing.T) {
	g := newGuest(t)
	g.process(30, "dockerd", "/usr/bin/dockerd")
	g.process(31, "containerd", "/usr/bin/containerd")
	res := g.runBuild(t)
	if res.err == nil || strings.Contains(strings.TrimSpace(res.err.Error()), "\n") {
		t.Fatalf("reason %q", res.err)
	}
}

func TestEmbeddedGuestScriptsAreTheScriptsOwn(t *testing.T) {
	text := string(buildScript)
	between := func(start, end string) string {
		i := strings.Index(text, start)
		if i < 0 {
			t.Fatalf("script has no %q", start)
		}
		rest := text[i+len(start):]
		rest = rest[strings.Index(rest, "\n")+1:]
		j := strings.Index(rest, "\n"+end+"\n")
		if j < 0 {
			t.Fatalf("script has no %q after %q", end, start)
		}
		return rest[:j+1]
	}
	for name, tc := range map[string]struct{ got, start, end string }{
		"removal":        {sandbox.GuestRemoval(), "read -r -d '' GUEST_REMOVAL <<'REMOVAL'", "REMOVAL"},
		"probe":          {sandbox.GuestProbe(), "read -r -d '' GUEST_PROBE <<'PROBE'", "PROBE"},
		"claude install": {sandbox.ClaudeInstall(), `bash -eu <<'CLAUDE'`, "CLAUDE"},
		"dagger install": {sandbox.DaggerInstall(), `sh -eu <<'INSTALL'`, "INSTALL"},
	} {
		t.Run(name, func(t *testing.T) {
			if want := between(tc.start, tc.end); tc.got != want {
				t.Errorf("the embedded script differs from the one in scripts/build-sandbox.sh")
			}
		})
	}
}
