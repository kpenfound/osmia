package intakejourney

import (
	context "context"
	"github.com/kpenfound/osmia/internal/intake"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	"github.com/kpenfound/osmia/internal/servicetest/journey"
	strings "strings"
	testing "testing"
	time "time"

	agent "github.com/kpenfound/agentcore/agent"
	runtime "github.com/kpenfound/osmia/internal/runtime"
	mcp "github.com/modelcontextprotocol/go-sdk/mcp"
)

const committeeProfiles = `[profiles.third]
agent = "opencode"
model = "third"
fallback = "other"
[committee]
profiles = ["default", "third"]
`

func TestCommitteeMembersTakeTheirPerspectivesAndProfiles(t *testing.T) {
	t.Parallel()
	const members = 4
	f := journey.NewDebateFixtureWith(t, members, 1, committeeProfiles, nil)
	defer f.Stop(t)
	for i := 1; i <= members; i++ {
		f.Member(1, i, 1, func(context.Context, agent.Request, *agent.Turn, *mcp.ClientSession) error { return nil })
	}
	stream := f.HandIn(t, "design", fixturekit.HandedDesign)
	f.AwaitShed(t, stream, "heard-1")

	want := []struct{ perspective, profile string }{
		{"correctness", "default"}, {"integration", "third"}, {"scope", "default"}, {"correctness", "third"},
	}
	for i, w := range want {
		th, err := f.Repository().Thread(stream, intake.CommitteeAgent(i+1))
		fixturekit.Must(t, err)
		if len(th.Turns) == 0 {
			t.Fatalf("member %d ran no turn", i+1)
		}
		req := th.Turns[0].Request
		if req.Profile.Name != w.profile {
			t.Errorf("member %d profile %q, want %q", i+1, req.Profile.Name, w.profile)
		}
		for name, focus := range intake.CommitteePerspectives {
			if strings.Contains(req.SystemPrompt, focus) != (name == w.perspective) {
				t.Errorf("member %d system prompt and perspective %s disagree; want only %s", i+1, name, w.perspective)
			}
		}
		if !strings.Contains(req.SystemPrompt, "You are a member of the committee that debates one workstream's feature spec and plan") {
			t.Errorf("member %d system prompt lost the committee prompt: %s", i+1, req.SystemPrompt)
		}
	}
}

func TestCommitteeProfilesFollowProviderLimitsAndTheOwnersOverride(t *testing.T) {
	t.Parallel()
	const members = 4
	f := journey.NewDebateFixtureWith(t, members, 1, committeeProfiles, nil)
	defer f.Stop(t)
	cfg := f.S.Current()
	profile := func(role, agent string) string {
		t.Helper()
		p, err := f.S.AgentProfile(cfg, role, agent)
		fixturekit.Must(t, err)
		return p.Name
	}
	if got := profile(intake.CommitteeRole, intake.CommitteeAgent(5)); got != "default" {
		t.Errorf("member 5 profile %q, want default", got)
	}
	fixturekit.Must(t, f.S.Store.SetProviderLimit(runtime.ProviderLimit{Backend: "opencode", Status: "blocked", SetAt: time.Now().UTC()}))
	if got := profile(intake.CommitteeRole, intake.CommitteeAgent(2)); got != "other" {
		t.Errorf("member 2 with opencode limited: %q, want its fallback other", got)
	}
	fixturekit.Must(t, f.S.Store.SetProviderLimit(runtime.ProviderLimit{Backend: "codex", Status: "blocked", SetAt: time.Now().UTC()}))
	if got := profile(intake.CommitteeRole, intake.CommitteeAgent(2)); got != "default" {
		t.Errorf("member 2 with its whole chain limited: %q, want the committee role's default", got)
	}
	fixturekit.Must(t, f.S.Store.ClearProviderLimit("codex"))
	fixturekit.Must(t, f.S.Store.SetProfile(intake.CommitteeRole, "other"))
	for i := 1; i <= members; i++ {
		if got := profile(intake.CommitteeRole, intake.CommitteeAgent(i)); got != "other" {
			t.Errorf("member %d under the owner's override: %q, want other", i, got)
		}
	}
	if got := profile(intake.ArchitectRole, "agent_architect"); got != "default" {
		t.Errorf("architect profile %q, want default", got)
	}
}
