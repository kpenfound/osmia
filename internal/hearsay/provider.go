package hearsay

import (
	"context"
	"encoding/json"
	"slices"
	"sync"
	"time"

	"github.com/kpenfound/osmia/internal/bundle"
	"github.com/kpenfound/osmia/internal/config"
)

const (
	Mode     bundle.Mode = "hearsay"
	Degraded bundle.Mode = "degraded"
)

// Health keeps the last observed state for one exact configuration and project.
// A changed configuration starts degraded until it supplies a valid bundle.
type Health struct {
	mu     sync.Mutex
	states map[string]bundle.Mode
}

func key(cfg *config.Config, id config.ProjectID) string {
	data, _ := json.Marshal(struct {
		H config.Hearsay
		P config.Project
	}{cfg.Hearsay, cfg.For(id).Project})
	return string(data)
}
func (h *Health) get(k string) bundle.Mode {
	h.mu.Lock()
	defer h.mu.Unlock()
	if mode, ok := h.states[k]; ok {
		return mode
	}
	return Degraded
}
func (h *Health) set(k string, m bundle.Mode) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.states == nil {
		h.states = map[string]bundle.Mode{}
	}
	h.states[k] = m
}

type Provider struct {
	Local  bundle.Provider
	Config func() *config.Config
	Health *Health
	Client func(config.Hearsay) Client
}

func (p Provider) Mode(id config.ProjectID) bundle.Mode {
	cfg := p.Config()
	if cfg.Hearsay.URL == "" || cfg.For(id).Project.HearsayScope == "" {
		return bundle.ModeFile
	}
	if p.Health == nil {
		return Degraded
	}
	return p.Health.get(key(cfg, id))
}

// Assemble always preserves the authoritative local bundle. External content
// is separately labelled and bounded to roughly two thousand tokens total.
func (p Provider) Assemble(ctx context.Context, id config.ProjectID, scope bundle.Scope) (bundle.Bundle, error) {
	out, err := p.Local.Assemble(ctx, id, scope)
	if err != nil {
		return out, err
	}
	cfg := p.Config()
	project := cfg.For(id).Project
	if cfg.Hearsay.URL == "" || project.HearsayScope == "" {
		return out, nil
	}
	client := Client{Config: cfg.Hearsay}
	if p.Client != nil {
		client = p.Client(cfg.Hearsay)
	}
	scopes := []string{}
	for _, entity := range scope.Entities {
		if mapped := project.HearsayEntities[entity]; mapped != "" && !slices.Contains(scopes, mapped) {
			scopes = append(scopes, mapped)
		}
	}
	if len(scopes) == 0 {
		scopes = append(scopes, project.HearsayScope)
	}
	// One deadline bounds the whole assembly, including multi-entity footprints.
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	out.Mode = Mode
	total := 0
	for _, entity := range scopes {
		data, err := client.Call(ctx, scope.Role, "get_bundle", map[string]string{"scope": entity})
		if err != nil {
			out.Mode = Degraded
			out.Memory = nil
			out.MemoryProblem = err.Error()
			break
		}
		var shape struct {
			Scope struct {
				ID string `json:"id"`
			} `json:"scope"`
		}
		if json.Unmarshal(data, &shape) != nil || shape.Scope.ID != entity {
			out.Mode = Degraded
			out.Memory = nil
			out.MemoryProblem = "memory bundle scope did not match the request"
			break
		}
		if total+len(data) > 8000 {
			out.Mode = Degraded
			out.MemoryProblem = "memory context exceeded its bundle budget"
			break
		}
		out.Memory = append(out.Memory, data)
		total += len(data)
	}
	if p.Health != nil {
		p.Health.set(key(cfg, id), out.Mode)
	}
	return out, nil
}

// Observe updates the status for the exact configuration that was checked.
func (h *Health) Observe(cfg *config.Config, id config.ProjectID, healthy bool) {
	mode := Degraded
	if healthy {
		mode = Mode
	}
	h.set(key(cfg, id), mode)
}
