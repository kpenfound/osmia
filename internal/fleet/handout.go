package fleet

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"maps"
	"slices"

	"github.com/kpenfound/osmia/internal/api"
	"github.com/kpenfound/osmia/internal/config"
)

// Handout builds the profile handout of cfg: its profiles and what each role
// needs. It copies only the fields of the wire types, so no provider key, no
// Dagger Cloud token and no environment variable name reaches a client.
func Handout(cfg *config.Config) api.ProfileHandout {
	h := api.ProfileHandout{Profiles: []api.ProfileSpec{}, Roles: []api.RoleRequirement{}}
	for _, name := range slices.Sorted(maps.Keys(cfg.Profiles)) {
		p := cfg.Profiles[name]
		h.Profiles = append(h.Profiles, api.ProfileSpec{Name: name, Backend: p.Agent, Model: p.Model, Effort: p.Effort, Fallback: p.Fallback, Timeout: p.Timeout, MaxTurns: p.MaxTurns})
	}
	for _, role := range slices.Sorted(maps.Keys(cfg.Roles)) {
		r := cfg.Roles[role]
		main := cfg.Profiles[r.Profile]
		req := api.RoleRequirement{Role: role, Profile: r.Profile, EventsProfile: r.EventsProfile, Chain: chain(cfg, r.Profile),
			Backend: main.Agent, Model: main.Model, Sandbox: r.Sandbox, Image: r.Image, CPUs: r.CPUs, Memory: r.Memory,
			Domains: []string{}, Skills: slices.Clone(r.Skills)}
		if req.Skills == nil {
			req.Skills = []string{}
		}
		if role == "mason" {
			engine := cfg.Dagger.Resolve()
			req.Dagger = &api.DaggerRequirement{Mode: string(engine.Mode), Version: cfg.Dagger.Version}
		}
		h.Roles = append(h.Roles, req)
	}
	data, _ := json.Marshal(h)
	sum := sha256.Sum256(data)
	h.Revision = hex.EncodeToString(sum[:8])
	return h
}

func chain(cfg *config.Config, profile string) []string {
	out := []string{}
	for name := profile; name != "" && !slices.Contains(out, name); name = cfg.Profiles[name].Fallback {
		out = append(out, name)
	}
	return out
}
