package config

import (
	"net/url"
	"regexp"
	"slices"
	"strings"
)

// Hearsay names an optional context service. Credentials are environment
// references; their values never become configuration or trace records.
type Hearsay struct {
	URL       string                  `toml:"url" json:"url,omitempty"`
	Principal string                  `toml:"principal" json:"principal,omitempty"`
	TokenEnv  string                  `toml:"token_env" json:"token_env,omitempty"`
	Agents    map[string]HearsayAgent `toml:"agents" json:"agents,omitempty"`
	// Publish configures the service's own posts to Hearsay's connectors
	// listener; nil leaves publishing off.
	Publish *HearsayPublish `toml:"publish" json:"publish,omitempty"`
}
type HearsayAgent struct {
	ID       string `toml:"id" json:"id"`
	TokenEnv string `toml:"token_env" json:"token_env"`
}

// HearsayPublish names where and as whom the service posts project records.
// WorkSource and OwnerSource are Hearsay `work` source ids: records of the
// project and its workstreams go to the work source, and what the owner
// decided goes to the owner source. HooksURL is Hearsay's connectors listener,
// separate from the API URL. Agent is the dedicated publisher agent, and
// TokenEnv names the environment variable holding its token.
type HearsayPublish struct {
	WorkSource  string `toml:"work_source" json:"work_source"`
	OwnerSource string `toml:"owner_source" json:"owner_source"`
	HooksURL    string `toml:"hooks_url" json:"hooks_url"`
	Agent       string `toml:"agent" json:"agent"`
	TokenEnv    string `toml:"token_env" json:"token_env"`
}

var hearsayName = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,63}$`)

// hearsayEntity is the form of a Hearsay entity id: a lowercase namespace and
// a colon, then a body with no whitespace.
var hearsayEntity = regexp.MustCompile(`^[a-z][a-z0-9_-]*:\S+$`)

// ValidHearsayEntity reports whether s is a Hearsay entity id such as
// work:<source>:<project>, rather than a bare scope name.
func ValidHearsayEntity(s string) bool { return len(s) <= 256 && hearsayEntity.MatchString(s) }

var environmentName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

func (h Hearsay) validate(path string) error {
	if h.URL == "" {
		if h.Principal != "" || h.TokenEnv != "" || len(h.Agents) != 0 || h.Publish != nil {
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
	if h.Publish != nil {
		return h.Publish.validate(path, h)
	}
	return nil
}

// validate checks the publish table. The publisher is a dedicated agent: its
// id and token variable are distinct from the owner's and the class agents',
// so a role's read credentials are never the publisher's.
func (p HearsayPublish) validate(path string, h Hearsay) error {
	const prefix = "hearsay.publish."
	if !hearsayName.MatchString(p.WorkSource) {
		return fieldError(path, prefix+"work_source", "must name the work source using up to 64 lowercase letters, digits, hyphens or underscores")
	}
	if !hearsayName.MatchString(p.OwnerSource) {
		return fieldError(path, prefix+"owner_source", "must name the owner source using up to 64 lowercase letters, digits, hyphens or underscores")
	}
	if p.OwnerSource == p.WorkSource {
		return fieldError(path, prefix+"owner_source", "must differ from work_source")
	}
	u, err := url.Parse(p.HooksURL)
	if p.HooksURL == "" || err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return fieldError(path, prefix+"hooks_url", "must be an HTTP(S) URL without credentials, query or fragment")
	}
	if strings.TrimSpace(p.Agent) == "" || p.Agent != strings.TrimSpace(p.Agent) || strings.ContainsAny(p.Agent, "\r\n") {
		return fieldError(path, prefix+"agent", "must name the publisher agent")
	}
	if p.Agent == h.Principal {
		return fieldError(path, prefix+"agent", "must differ from hearsay.principal")
	}
	for class, agent := range h.Agents {
		if p.Agent == agent.ID {
			return fieldError(path, prefix+"agent", "must differ from the id of hearsay.agents."+class)
		}
	}
	if !environmentName.MatchString(p.TokenEnv) {
		return fieldError(path, prefix+"token_env", "must name an environment variable")
	}
	if p.TokenEnv == h.TokenEnv {
		return fieldError(path, prefix+"token_env", "must differ from hearsay.token_env")
	}
	for class, agent := range h.Agents {
		if p.TokenEnv == agent.TokenEnv {
			return fieldError(path, prefix+"token_env", "must differ from the token_env of hearsay.agents."+class)
		}
	}
	return nil
}

// Publishing reports whether the service posts records to Hearsay.
func (h Hearsay) Publishing() bool { return h.URL != "" && h.Publish != nil }

func (p Project) validateHearsay(path string) error {
	if p.HearsayScope != "" && !ValidHearsayEntity(p.HearsayScope) {
		return fieldError(path, "hearsay_scope", "must be a Hearsay entity id such as work:<source>:<project>, not a scope name")
	}
	for entity, id := range p.HearsayEntities {
		if strings.TrimSpace(entity) == "" || !ValidHearsayEntity(id) {
			return fieldError(path, "hearsay_entities", "must map local entity IDs to Hearsay entity ids such as code:<repository>:<entity>, not scope names")
		}
	}
	return nil
}

// HearsayScope is the entity id the project reads by default: its
// hearsay_scope override, or the project entity of the publish work source
// when one is configured. It is empty when neither is set, which keeps the
// project in file mode.
func (c *Config) HearsayScope() string {
	if c.Project.HearsayScope != "" {
		return c.Project.HearsayScope
	}
	if c.Hearsay.Publish != nil && c.Project.ID != "" {
		return "work:" + c.Hearsay.Publish.WorkSource + ":" + string(c.Project.ID)
	}
	return ""
}

// HearsayWorkstreamScope is the entity id of a workstream on the publish work
// source, or empty without a work source or a workstream.
func (c *Config) HearsayWorkstreamScope(workstream WorkstreamID) string {
	if c.Hearsay.Publish == nil || c.Project.ID == "" || workstream == "" {
		return ""
	}
	return "work:" + c.Hearsay.Publish.WorkSource + ":" + string(c.Project.ID) + "#" + string(workstream)
}

// HearsayReads is the ordered entity ids a bundle for the project requests:
// the entities the footprint maps, or the project default when none maps, and
// then the workstream's own entity for a workstream-tied turn. It is nil when
// the project is in file mode.
func (c *Config) HearsayReads(footprint []string, workstream WorkstreamID) []string {
	def := c.HearsayScope()
	if c.Hearsay.URL == "" || def == "" {
		return nil
	}
	var out []string
	add := func(id string) {
		if id != "" && !slices.Contains(out, id) {
			out = append(out, id)
		}
	}
	for _, entity := range footprint {
		add(c.Project.HearsayEntities[entity])
	}
	if len(out) == 0 {
		add(def)
	}
	add(c.HearsayWorkstreamScope(workstream))
	return out
}
