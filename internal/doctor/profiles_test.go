package doctor

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/kpenfound/agentcore/agent"
)

func TestCheckToolsRunsTheServersToolChecksOnThisMachine(t *testing.T) {
	var ran []string
	o := Options{
		LookPath: func(name string) (string, error) {
			if name == "codex" || name == agent.SandboxCLI {
				return "", errors.New("not found")
			}
			return "/bin/" + name, nil
		},
		Command: func(_ context.Context, path string, args ...string) (string, error) {
			ran = append(ran, path+" "+strings.Join(args, " "))
			if path == "/bin/git" && slices.Contains(args, "https://example.com/missing.git") {
				return "", errors.New("repository not found")
			}
			return "tool 1.0\n", nil
		},
	}
	checks := CheckTools(context.Background(), o, Tools{Agent: "claude", Container: true, Sbx: true, Dagger: true,
		Skills: []string{"https://example.com/skills.git#tdd", "https://example.com/missing.git"}})
	status := map[string]Status{}
	for _, c := range checks {
		status[c.Name] = c.Status
		if c.Status != Pass && c.Remediation == "" {
			t.Errorf("%s has no fix", c.Name)
		}
	}
	want := map[string]Status{"claude": Pass, agent.ContainerEngine: Pass, agent.SandboxCLI: Fail, "dagger": Pass,
		"skill https://example.com/skills.git#tdd": Pass, "skill https://example.com/missing.git": Fail}
	for name, s := range want {
		if status[name] != s {
			t.Errorf("%s: %q, want %q (%v)", name, status[name], s, status)
		}
	}
	if !slices.Contains(ran, "/bin/claude --version") || !slices.Contains(ran, "/bin/git ls-remote --exit-code https://example.com/skills.git") {
		t.Errorf("commands run: %v", ran)
	}
	if got := CheckTools(context.Background(), o, Tools{Agent: "codex"}); len(got) != 1 || got[0].Status != Fail || got[0].Name != "codex" || !strings.Contains(got[0].Remediation, "install codex") {
		t.Errorf("a missing backend: %+v", got)
	}
	if got := CheckTools(context.Background(), o, Tools{Skills: []string{"-bad"}}); len(got) != 1 || got[0].Status != Fail {
		t.Errorf("a bad skill reference: %+v", got)
	}
}
