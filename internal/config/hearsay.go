package config

import (
	"net/url"
	"regexp"
	"strings"
)

// Hearsay names an optional context service. Credentials are environment
// references; their values never become configuration or trace records.
type Hearsay struct {
	URL       string                  `toml:"url" json:"url,omitempty"`
	Principal string                  `toml:"principal" json:"principal,omitempty"`
	TokenEnv  string                  `toml:"token_env" json:"token_env,omitempty"`
	Agents    map[string]HearsayAgent `toml:"agents" json:"agents,omitempty"`
}
type HearsayAgent struct {
	ID       string `toml:"id" json:"id"`
	TokenEnv string `toml:"token_env" json:"token_env"`
}

var hearsayName = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,63}$`)

var environmentName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

func (h Hearsay) validate(path string) error {
	if h.URL == "" {
		if h.Principal != "" || h.TokenEnv != "" || len(h.Agents) != 0 {
			return fieldError(path, "hearsay.url", "required when Hearsay settings are present")
		}
		return nil
	}
	u, err := url.Parse(h.URL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return fieldError(path, "hearsay.url", "must be an HTTP(S) URL without credentials, query or fragment")
	}
	if strings.TrimSpace(h.Principal) == "" || strings.ContainsAny(h.Principal, "\r\n") {
		return fieldError(path, "hearsay.principal", "must name the owner principal")
	}
	if !environmentName.MatchString(h.TokenEnv) {
		return fieldError(path, "hearsay.token_env", "must name an environment variable")
	}
	for _, class := range []string{"worker", "orchestrator", "observer"} {
		agent := h.Agents[class]
		if strings.TrimSpace(agent.ID) == "" || strings.ContainsAny(agent.ID, "\r\n") || !environmentName.MatchString(agent.TokenEnv) {
			return fieldError(path, "hearsay.agents."+class, "requires id and a token_env environment reference")
		}
	}
	for class := range h.Agents {
		if class != "worker" && class != "orchestrator" && class != "observer" {
			return fieldError(path, "hearsay.agents", "unknown agent class")
		}
	}
	return nil
}

func (p Project) validateHearsay(path string) error {
	if p.HearsayScope != "" && !hearsayName.MatchString(p.HearsayScope) {
		return fieldError(path, "hearsay_scope", "must name a Hearsay scope using up to 64 lowercase letters, digits, hyphens or underscores")
	}
	for entity, scope := range p.HearsayEntities {
		if strings.TrimSpace(entity) == "" || !hearsayName.MatchString(scope) {
			return fieldError(path, "hearsay_entities", "must map local entity IDs to Hearsay scope names")
		}
	}
	return nil
}
