package runtime

import (
	"slices"
	"testing"
	"time"

	"github.com/kpenfound/osmia/internal/config"
)

func TestProviderLimitsFallbackPauseOverrideResetAndRestart(t *testing.T) {
	in := fixture(t)
	defaultProfile := in.Config.Profiles["default"]
	defaultProfile.Fallback = "other"
	in.Config.Profiles["default"] = defaultProfile
	other := in.Config.Profiles["other"]
	other.Fallback = "third"
	in.Config.Profiles["other"] = other
	in.Config.Profiles["third"] = config.Profile{Agent: "opencode", Model: "third"}
	at := time.Date(2026, 9, 24, 12, 0, 0, 0, time.UTC)
	s := open(t, in)
	must(t, s.SetProviderLimit(ProviderLimit{Backend: "claude", Status: "blocked", SetAt: at, ResetsAt: at.Add(time.Hour)}))
	state, _ := s.EffectiveAt(at)
	if state.Profiles["mason"] != "other" || len(state.ProviderLimits) != 1 {
		t.Fatalf("first fallback: %+v", state)
	}
	must(t, s.SetProviderLimit(ProviderLimit{Backend: "codex", Status: "rejected", SetAt: at}))
	state, _ = s.EffectiveAt(at)
	if state.Profiles["mason"] != "third" {
		t.Fatalf("chain: %+v", state.Profiles)
	}
	must(t, s.SetProviderLimit(ProviderLimit{Backend: "opencode", Status: "blocked", SetAt: at}))
	state, _ = s.EffectiveAt(at)
	if !slices.ContainsFunc(state.Pauses, func(p Pause) bool {
		return p.Target == (Target{Scope: "role", Role: "mason"}) && p.Source == PauseProviderUsageLimit
	}) {
		t.Fatalf("attributed role pause: %+v", state.Pauses)
	}
	must(t, s.SetProfile("mason", "third"))
	state, _ = s.EffectiveAt(at)
	if state.Profiles["mason"] != "third" || slices.ContainsFunc(state.Pauses, func(p Pause) bool { return p.Target.Role == "mason" }) {
		t.Fatalf("override: %+v", state)
	}
	must(t, s.ClearProfile("mason"))
	reopened, _, err := Open(in)
	must(t, err)
	t.Cleanup(func() { reopened.Close() })
	state, _ = reopened.EffectiveAt(at)
	if len(state.ProviderLimits) != 3 || !slices.ContainsFunc(state.Pauses, func(p Pause) bool { return p.Target.Role == "mason" }) {
		t.Fatalf("restart: %+v", state)
	}
	state, _ = reopened.EffectiveAt(at.Add(time.Hour))
	if state.Profiles["mason"] != "default" || len(state.ProviderLimits) != 2 {
		t.Fatalf("reset: %+v", state)
	}
	must(t, reopened.ClearProviderLimit("codex"))
	must(t, reopened.ClearProviderLimit("opencode"))
	state, _ = reopened.EffectiveAt(at)
	if state.Profiles["mason"] != "other" {
		t.Fatalf("owner clear: %+v", state)
	}
}

func TestTheRegistryLimitPausesEverySbxRoleUntilItResets(t *testing.T) {
	in := fixture(t)
	defaultProfile := in.Config.Profiles["default"]
	defaultProfile.Fallback = "other"
	in.Config.Profiles["default"] = defaultProfile
	mason := in.Config.Roles["mason"]
	mason.Sandbox = "sbx"
	in.Config.Roles["mason"] = mason
	var host string
	for role, binding := range in.Config.Roles {
		if binding.Sandbox != "sbx" {
			host = role
			break
		}
	}
	if host == "" {
		t.Fatal("the fixture has no role outside sbx")
	}
	at := time.Date(2026, 11, 18, 21, 30, 0, 0, time.UTC)
	s := open(t, in)
	must(t, s.SetProviderLimit(ProviderLimit{Backend: SandboxRegistryLimit, Status: "blocked", Kind: "registry", SetAt: at, ResetsAt: at.Add(10 * time.Minute)}))
	paused := func(state State, role string) bool {
		return slices.ContainsFunc(state.Pauses, func(p Pause) bool {
			return p.Target == (Target{Scope: "role", Role: role}) && p.Source == PauseProviderUsageLimit && p.Reason == RegistryLimitReason
		})
	}
	state, _ := s.EffectiveAt(at)
	if !paused(state, "mason") || state.Profiles["mason"] != in.Config.Roles["mason"].Profile {
		t.Fatalf("the sbx mason was not paused on its own profile: profiles %v, pauses %+v", state.Profiles, state.Pauses)
	}
	if paused(state, host) {
		t.Fatalf("role %s, outside sbx, was paused: %+v", host, state.Pauses)
	}
	state, _ = s.EffectiveAt(at.Add(10 * time.Minute))
	if paused(state, "mason") || len(state.ProviderLimits) != 0 {
		t.Fatalf("the limit outlived its reset: %+v", state)
	}
	must(t, s.ClearProviderLimit(SandboxRegistryLimit))
	state, _ = s.EffectiveAt(at)
	if paused(state, "mason") {
		t.Fatalf("the owner's clear did not lift the pause: %+v", state.Pauses)
	}
}
