package service

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/kpenfound/busybees/core/agent"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/kpenfound/osmia/internal/runtime"
)

// Members take committee.perspectives and committee.profiles in turn by
// member number, each assigned profile falls along its own fallback chain
// past limited providers, and the owner's committee override wins.
func TestCommitteeMembersTakeTheirPerspectivesAndProfiles(t *testing.T) {
	t.Parallel()
	const members = 4
	f := newDebateFixtureWith(t, members, 1, `[profiles.third]
agent = "opencode"
model = "third"
fallback = "other"
[committee]
profiles = ["default", "third"]
`, nil)
	defer f.stop(t)
	for i := 1; i <= members; i++ {
		f.member(1, i, 1, func(context.Context, agent.Request, *agent.Turn, *mcp.ClientSession) error { return nil })
	}
	stream := f.handIn(t, "design", handedDesign)
	f.awaitShed(t, stream, "heard-1")

	want := []struct{ perspective, profile string }{
		{"correctness", "default"}, {"integration", "third"}, {"scope", "default"}, {"correctness", "third"},
	}
	for i, w := range want {
		th, err := f.repository().Thread(stream, committeeAgent(i+1))
		must(t, err)
		if len(th.Turns) == 0 {
			t.Fatalf("member %d ran no turn", i+1)
		}
		req := th.Turns[0].Request
		if req.Profile.Name != w.profile {
			t.Errorf("member %d profile %q, want %q", i+1, req.Profile.Name, w.profile)
		}
		for name, focus := range committeePerspectives {
			if strings.Contains(req.SystemPrompt, focus) != (name == w.perspective) {
				t.Errorf("member %d system prompt and perspective %s disagree; want only %s", i+1, name, w.perspective)
			}
		}
		// A literal fragment of the committee's shared framing, not the output of
		// committeeSystemPrompt itself: computing the expected text by calling
		// the function under test would hide a wording bug in that function.
		if !strings.Contains(req.SystemPrompt, "You are a member of the committee that debates one workstream's feature spec and plan") {
			t.Errorf("member %d system prompt lost the committee prompt: %s", i+1, req.SystemPrompt)
		}
	}

	cfg := f.s.current()
	profile := func(agent string) string {
		t.Helper()
		p, err := f.s.agentProfile(cfg, committeeRole, agent)
		must(t, err)
		return p.Name
	}
	if got := profile(committeeAgent(5)); got != "default" {
		t.Errorf("member 5 profile %q, want default", got)
	}
	must(t, f.s.store.SetProviderLimit(runtime.ProviderLimit{Backend: "opencode", Status: "blocked", SetAt: time.Now().UTC()}))
	if got := profile(committeeAgent(2)); got != "other" {
		t.Errorf("member 2 with opencode limited: %q, want its fallback other", got)
	}
	must(t, f.s.store.SetProviderLimit(runtime.ProviderLimit{Backend: "codex", Status: "blocked", SetAt: time.Now().UTC()}))
	if got := profile(committeeAgent(2)); got != "default" {
		t.Errorf("member 2 with its whole chain limited: %q, want the committee role's default", got)
	}
	must(t, f.s.store.ClearProviderLimit("codex"))
	must(t, f.s.store.SetProfile(committeeRole, "other"))
	for i := 1; i <= members; i++ {
		if got := profile(committeeAgent(i)); got != "other" {
			t.Errorf("member %d under the owner's override: %q, want other", i, got)
		}
	}
	if p, err := f.s.agentProfile(cfg, architectRole, "agent_architect"); err != nil || p.Name != "default" {
		t.Errorf("architect profile: %q %v", p.Name, err)
	}
}
