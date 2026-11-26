package scripts

import (
	"strings"
	"testing"
)

func (r result) wantReady(t *testing.T) {
	t.Helper()
	if r.err != nil {
		t.Fatalf("the check failed: %v\n%s", r.err, r.output)
	}
	r.wantCheckSandboxOnly(t)
}

func (r result) wantNotReady(t *testing.T, reason ...string) {
	t.Helper()
	if r.err == nil {
		t.Fatalf("the check passed\n%s", r.output)
	}
	r.wantReason(t, reason...)
	if strings.Contains(strings.TrimSpace(r.err.Error()), "\n") {
		t.Errorf("reason is not one line: %q", r.err)
	}
	r.wantCheckSandboxOnly(t)
}

// wantCheckSandboxOnly checks that the check made one sandbox from the saved
// template, ran only a version query in it, removed it, and neither stopped
// nor saved anything.
func (r result) wantCheckSandboxOnly(t *testing.T) {
	t.Helper()
	for _, c := range r.calls {
		switch {
		case strings.HasPrefix(c, "stop "), strings.HasPrefix(c, "template "):
			t.Errorf("the check made the call %q", c)
		case strings.HasPrefix(c, "exec ") && !strings.HasSuffix(c, " claude --version"):
			t.Errorf("the check ran %q", c)
		}
	}
	if r.count("create ") == 1 {
		r.wantRemoved(t)
	}
}

func TestCheckPassesForAClaudeThatRunsFable(t *testing.T) {
	for _, claude := range []string{
		"2.1.321 (Claude Code)",
		"2.1.322 (Claude Code)",
		"2.1.1000 (Claude Code)",
		"2.2.0 (Claude Code)",
		"3.0.0 (Claude Code)",
		"10.0.0",
	} {
		t.Run(claude, func(t *testing.T) {
			g := newGuest(t)
			g.saveTemplate(claude)
			res := g.runCheck(t)
			res.wantReady(t)
			if !res.has("--template osmia:latest") || !res.has("claude --version") {
				t.Errorf("the check did not ask the saved template: %v", res.calls)
			}
			g.wantEarlierSandboxUntouched(t, res)
		})
	}
}

func TestCheckFailsWithAReasonWhenTheTemplateIsNotReady(t *testing.T) {
	for _, tt := range []struct {
		name, claude string
		reason       []string
	}{
		{"one patch older", "2.1.320 (Claude Code)", []string{"osmia:latest", "2.1.320", "2.1.321"}},
		{"older minor", "2.0.999 (Claude Code)", []string{"2.0.999", "2.1.321"}},
		{"older major", "1.9.9", []string{"1.9.9", "2.1.321"}},
		{"no version in the output", "claude: command output changed", []string{"could not read", "osmia:latest"}},
		{"claude not installed", "MISSING", []string{"could not read", "osmia:latest"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			g := newGuest(t)
			g.saveTemplate(tt.claude)
			res := g.runCheck(t)
			res.wantNotReady(t, tt.reason...)
			g.wantEarlierSandboxUntouched(t, res)
		})
	}
}

func TestCheckFailsWhenNoTemplateIsSaved(t *testing.T) {
	g := newGuest(t)
	res := g.runCheck(t)
	res.wantNotReady(t, "osmia:latest", "osmia sandbox build")
	g.wantEarlierSandboxUntouched(t, res)
}

func TestCheckFailsWithoutSbx(t *testing.T) {
	g := newGuest(t)
	g.noSbx = true
	res := g.runCheck(t)
	if res.err == nil || !strings.Contains(res.output, "sbx is required on PATH") || len(res.calls) != 0 {
		t.Fatalf("err %v, output %q, calls %v", res.err, res.output, res.calls)
	}
}

func TestCheckWarnsButKeepsItsVerdictWhenItsSandboxCannotBeRemoved(t *testing.T) {
	g := newGuest(t)
	g.saveTemplate("2.1.321 (Claude Code)")
	g.removeFails = true
	res := g.runCheck(t)
	if res.err != nil || !strings.Contains(res.output, "warning: could not remove temporary sandbox osmia-check-") {
		t.Fatalf("err %v, output %q", res.err, res.output)
	}
}

func TestCheckFollowsTheTemplateReplacedByTheManualScript(t *testing.T) {
	g := newGuest(t)
	g.runBuild(t).wantSaved(t)
	g.runCheck(t).wantReady(t)

	g.claude = "2.0.0 (Claude Code)"
	g.runScript(t).wantSaved(t)
	g.runCheck(t).wantNotReady(t, "2.0.0", "2.1.321")

	g.claude = "2.1.400 (Claude Code)"
	g.runScript(t).wantSaved(t)
	g.runCheck(t).wantReady(t)

	g.claude = "2.1.400 (Claude Code)"
	g.runBuild(t).wantSaved(t)
	g.claude = "1.0.0 (Claude Code)"
	g.runScript(t).wantSaved(t)
	g.runCheck(t).wantNotReady(t, "1.0.0")
}

func TestCheckFailsWhenTheTemplateIsRemovedAfterABuild(t *testing.T) {
	g := newGuest(t)
	g.runBuild(t).wantSaved(t)
	g.runCheck(t).wantReady(t)
	g.removeTemplate()
	g.runCheck(t).wantNotReady(t, "osmia:latest")
}

func TestCheckIgnoresASandboxLeftBehindByABuild(t *testing.T) {
	g := newGuest(t)
	g.claude = "2.5.0 (Claude Code)"
	g.runBuild(t).wantSaved(t)
	g.leakSandbox("osmia-build-deadbeef", "2.5.0 (Claude Code)")
	g.saveTemplate("2.0.0 (Claude Code)")
	res := g.runCheck(t)
	res.wantNotReady(t, "2.0.0")
	for _, c := range res.calls {
		if strings.Contains(c, "osmia-build-deadbeef") {
			t.Errorf("the check used the leftover build sandbox: %q", c)
		}
	}
}

func TestCheckSurvivesAFailedBuildThatLeavesAHealthyTemplate(t *testing.T) {
	for _, p := range buildPaths {
		t.Run(p.name, func(t *testing.T) {
			g := newGuest(t)
			g.runBuild(t).wantSaved(t)
			g.runCheck(t).wantReady(t)

			g.saveFails = true
			g.claude = "9.9.9 (Claude Code)"
			if res := p.run(g, t); res.err == nil {
				t.Fatalf("the build succeeded although its save failed\n%s", res.output)
			}
			if got := g.savedTemplate(); got != "2.1.321 (Claude Code)" {
				t.Fatalf("saved template after the failed save = %q", got)
			}
			g.runCheck(t).wantReady(t)

			g.saveFails = false
			g.claude = "2.1.321 (Claude Code)"
			g.process(30, "dockerd", "/usr/bin/dockerd")
			if res := p.run(g, t); res.err == nil {
				t.Fatalf("the build succeeded with a Docker Engine running\n%s", res.output)
			}
			g.runCheck(t).wantReady(t)
		})
	}
}

func TestCheckJudgesADifferentTemplateSavedAfterAFailedAttempt(t *testing.T) {
	for _, tt := range []struct {
		name, saved string
		ready       bool
		reason      []string
	}{
		{"new enough", "2.3.0 (Claude Code)", true, nil},
		{"older", "2.1.100 (Claude Code)", false, []string{"2.1.100"}},
		{"unreadable", "no version here", false, []string{"could not read"}},
		{"no Claude Code", "MISSING", false, []string{"could not read"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			g := newGuest(t)
			g.claude = "9.0.0 (Claude Code)"
			g.runBuild(t).wantSaved(t)

			g.saveFails = true
			if res := g.runBuild(t); res.err == nil {
				t.Fatalf("the build succeeded although its save failed\n%s", res.output)
			}
			g.saveFails = false

			g.claude = tt.saved
			if tt.saved == "MISSING" {
				g.saveTemplate("MISSING")
			} else {
				g.runScript(t).wantSaved(t)
			}
			res := g.runCheck(t)
			if tt.ready {
				res.wantReady(t)
				return
			}
			res.wantNotReady(t, tt.reason...)
		})
	}
}

func TestCheckNeverReadsEvidenceOfAFailedAttempt(t *testing.T) {
	g := newGuest(t)
	g.saveTemplate("2.0.0 (Claude Code)")
	g.saveFails = true
	g.claude = "9.0.0 (Claude Code)"
	if res := g.runBuild(t); res.err == nil {
		t.Fatal("the build succeeded although its save failed")
	}
	g.saveFails = false
	res := g.runCheck(t)
	res.wantNotReady(t, "2.0.0")
}
