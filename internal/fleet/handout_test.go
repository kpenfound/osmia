package fleet

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/kpenfound/osmia/internal/api"
	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/trace"
)

func handoutConfig() *config.Config {
	return &config.Config{
		Profiles: map[string]config.Profile{
			"fast":   {Agent: "claude", Model: "sonnet", Effort: "low", Fallback: "slow", Timeout: "20m", MaxTurns: 30},
			"slow":   {Agent: "codex", Model: "gpt-5", Fallback: "fast", Timeout: "40m"},
			"review": {Agent: "claude", Model: "opus"},
		},
		Roles: map[string]config.Role{
			"mason":            {Profile: "fast", Sandbox: "sbx", Image: "osmia:latest", CPUs: 4, Memory: "8g", Skills: []string{"https://example.com/skills.git@main#go"}},
			"reviewer":         {Profile: "review", Sandbox: "container", Image: "reviewer:1"},
			config.ManagerRole: {Profile: "review", EventsProfile: "slow", Sandbox: "none"},
		},
		Dagger: config.DaggerConfig{Version: "v0.20.5", Engine: "cloud", CloudComputeTokenEnv: "OSMIA_TEST_DAGGER_COMPUTE", CloudAPITokenEnv: "OSMIA_TEST_DAGGER_API"},
		Jev:    config.Jev{APIKeyEnv: "OSMIA_TEST_JEV_KEY"},
	}
}

func TestTheHandoutCarriesEveryRoleRequirement(t *testing.T) {
	t.Parallel()
	h := Handout(handoutConfig())
	roles := map[string]api.RoleRequirement{}
	for _, r := range h.Roles {
		roles[r.Role] = r
	}
	mason := roles["mason"]
	if mason.Profile != "fast" || mason.Backend != "claude" || mason.Model != "sonnet" || mason.Sandbox != "sbx" || mason.Image != "osmia:latest" || mason.CPUs != 4 || mason.Memory != "8g" ||
		!slices.Equal(mason.Skills, []string{"https://example.com/skills.git@main#go"}) || !slices.Equal(mason.Chain, []string{"fast", "slow"}) || mason.Domains == nil {
		t.Fatalf("mason: %+v", mason)
	}
	if mason.Dagger == nil || mason.Dagger.Mode != "cloud" || mason.Dagger.Version != "v0.20.5" {
		t.Fatalf("mason dagger: %+v", mason.Dagger)
	}
	if roles["reviewer"].Dagger != nil || roles[trace.Manager].Dagger != nil {
		t.Fatalf("only the mason takes a Dagger requirement: %+v", roles)
	}
	if cos := roles[trace.Manager]; cos.EventsProfile != "slow" || cos.Sandbox != "none" || cos.Skills == nil {
		t.Fatalf("manager: %+v", cos)
	}
	if got := len(h.Profiles); got != 3 || h.Profiles[0].Name != "fast" || h.Profiles[0].Backend != "claude" || h.Profiles[0].Model != "sonnet" || h.Profiles[0].Fallback != "slow" || h.Profiles[0].MaxTurns != 30 {
		t.Fatalf("profiles: %+v", h.Profiles)
	}
	if h.Revision == "" || Handout(handoutConfig()).Revision != h.Revision {
		t.Fatalf("revision %q is not stable", h.Revision)
	}
	changed := handoutConfig()
	role := changed.Roles["mason"]
	role.Image = "osmia:next"
	changed.Roles["mason"] = role
	if Handout(changed).Revision == h.Revision {
		t.Fatal("the revision does not change with the content")
	}
}

func TestTheHandoutHoldsNoSecretValueOrCredentialName(t *testing.T) {
	secrets := map[string]string{
		"OSMIA_TEST_DAGGER_COMPUTE": "dagger-compute-secret-value",
		"OSMIA_TEST_DAGGER_API":     "dagger-api-secret-value",
		"OSMIA_TEST_JEV_KEY":        "provider-key-secret-value",
		"ANTHROPIC_API_KEY":         "anthropic-secret-value",
		"GITHUB_TOKEN":              "github-secret-value",
	}
	for name, value := range secrets {
		t.Setenv(name, value)
	}
	r, _ := newRegistry(t)
	r.SetHandout(Handout(handoutConfig()))
	resp, err := r.Connect(context.Background(), node("n1"), api.ClientConnectRequest{Version: api.ClientProtocolVersion, Slots: 1})
	if err != nil {
		t.Fatal(err)
	}
	wire, err := json.Marshal(resp)
	if err != nil {
		t.Fatal(err)
	}
	for name, value := range secrets {
		if strings.Contains(string(wire), value) || strings.Contains(string(wire), name) {
			t.Fatalf("the connect response holds %s: %s", name, wire)
		}
	}
	if !strings.Contains(string(wire), `"mode":"cloud"`) {
		t.Fatalf("the Dagger mode is missing: %s", wire)
	}
}
