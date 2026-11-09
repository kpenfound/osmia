package config

import (
	"net/url"
	"strings"
)

// The Transcribe defaults. An empty URL turns transcription off.
const (
	DefaultTranscribeModel     = "large-v3-turbo"
	DefaultTranscribeAPIKeyEnv = "WHISPER_API_KEY"
)

// Transcribe configures the optional Whisper transcription of the page's
// recorded clips. URL is the full OpenAI-compatible transcription endpoint a
// clip is posted to; empty (the default) turns transcription off. The bearer
// key is an environment reference; its value never becomes configuration, a
// response, a log record or a trace record.
type Transcribe struct {
	URL       string `toml:"url" json:"url"`
	Model     string `toml:"model" json:"model"`
	APIKeyEnv string `toml:"api_key_env" json:"api_key_env"`
}

func defaultTranscribe() Transcribe {
	return Transcribe{Model: DefaultTranscribeModel, APIKeyEnv: DefaultTranscribeAPIKeyEnv}
}

// Enabled reports whether a transcription endpoint is configured.
func (t Transcribe) Enabled() bool { return t.URL != "" }

// The settings are validated whether or not transcription is on, so setting
// the URL never reveals a broken model or key name.
func (t Transcribe) validate(path string) error {
	if t.URL != "" {
		u, err := url.Parse(t.URL)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" {
			return fieldError(path, "transcribe.url", "must be empty or an HTTP(S) URL without credentials, query or fragment")
		}
	}
	if strings.TrimSpace(t.Model) == "" || strings.ContainsAny(t.Model, " \t\r\n") || len(t.Model) > 128 {
		return fieldError(path, "transcribe.model", "must name a model in up to 128 characters without whitespace")
	}
	if !environmentName.MatchString(t.APIKeyEnv) {
		return fieldError(path, "transcribe.api_key_env", "must name an environment variable")
	}
	return nil
}
