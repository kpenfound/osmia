package config

import (
	"net/url"
	"strings"
	"time"

	"github.com/kpenfound/agentcore/jev"
)

// The Jev defaults reach Jev through OpenRouter's TypeSafe-compatible API.
const (
	DefaultJevURL       = "https://openrouter.ai/api"
	DefaultJevModel     = "~typesafe/jev-latest"
	DefaultJevAPIKeyEnv = "OPENROUTER_API_KEY"
	DefaultJevTimeout   = "10s"
)

// DefaultJevCheckThreshold is the probability at or above which the check
// selection runs a check.
const DefaultJevCheckThreshold = 0.5

// MaxJevTimeout bounds one Jev request, so a judgment never holds a turn for
// long.
const MaxJevTimeout = jev.MaxTimeout

// Jev configures the optional Jev boost. Enabled is the one switch for every
// judgment Osmia asks Jev for; the other settings say how to reach it. The API
// key is an environment reference; its value never becomes configuration or a
// trace record.
type Jev struct {
	Enabled bool `toml:"enabled" json:"enabled"`
	// URL is the base of a TypeSafe-compatible API; requests go to
	// <url>/v1/systemone.
	URL       string `toml:"url" json:"url"`
	Model     string `toml:"model" json:"model"`
	APIKeyEnv string `toml:"api_key_env" json:"api_key_env"`
	Timeout   string `toml:"timeout" json:"timeout"`
	// CheckThreshold is the probability at or above which the check selection
	// runs a check, above 0 and at most 1.
	CheckThreshold float64 `toml:"check_threshold" json:"check_threshold"`
	// CheckFallbackPicks runs this many highest-scored candidates when none
	// reaches the threshold. Zero runs every check instead.
	CheckFallbackPicks int `toml:"check_fallback_picks" json:"check_fallback_picks"`
}

func defaultJev() Jev {
	return Jev{URL: DefaultJevURL, Model: DefaultJevModel, APIKeyEnv: DefaultJevAPIKeyEnv, Timeout: DefaultJevTimeout, CheckThreshold: DefaultJevCheckThreshold}
}

// Settings are j as a Jev judge reads them, with the timeout parsed.
func (j Jev) Settings() jev.Settings {
	timeout, _ := time.ParseDuration(j.Timeout)
	return jev.Settings{Enabled: j.Enabled, URL: j.URL, Model: j.Model, APIKeyEnv: j.APIKeyEnv, Timeout: timeout}
}

// The settings are validated whether or not Jev is enabled, so turning the
// switch on never reveals a broken setting.
func (j Jev) validate(path string) error {
	u, err := url.Parse(j.URL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return fieldError(path, "jev.url", "must be an HTTP(S) URL without credentials, query or fragment")
	}
	if strings.TrimSpace(j.Model) == "" || strings.ContainsAny(j.Model, " \t\r\n") || len(j.Model) > 128 {
		return fieldError(path, "jev.model", "must name a model in up to 128 characters without whitespace")
	}
	if !environmentName.MatchString(j.APIKeyEnv) {
		return fieldError(path, "jev.api_key_env", "must name an environment variable")
	}
	if d, err := time.ParseDuration(j.Timeout); err != nil || d <= 0 || d > MaxJevTimeout {
		return fieldError(path, "jev.timeout", "must be a positive Go duration of at most 1m")
	}
	if !(j.CheckThreshold > 0 && j.CheckThreshold <= 1) {
		return fieldError(path, "jev.check_threshold", "must be a probability above 0 and at most 1")
	}
	if j.CheckFallbackPicks < 0 {
		return fieldError(path, "jev.check_fallback_picks", "must be a non-negative integer")
	}
	return nil
}
