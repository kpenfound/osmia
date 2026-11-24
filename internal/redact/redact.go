// Package redact removes credentials from text before it reaches a log or a
// terminal.
package redact

import (
	"os"
	"regexp"
	"strings"
	"sync"
)

// CredentialURL matches the user information of a URL, which may hold a
// credential.
var CredentialURL = regexp.MustCompile(`([A-Za-z][A-Za-z0-9+.-]*://)[^/\s@]+@`)

// Secrets replaces the user information of any URL in s and every non-empty
// value in secrets with "[redacted]".
func Secrets(s string, secrets ...string) string {
	s = CredentialURL.ReplaceAllString(s, "${1}[redacted]@")
	for _, secret := range secrets {
		if secret != "" {
			s = strings.ReplaceAll(s, secret, "[redacted]")
		}
	}
	return s
}

// EnvSecrets names the environment variables whose values Secrets must
// always redact, beyond the GitHub token and any caller-supplied secret:
// the tailnet auth key and the default API key environment variables of the
// Jev and of Whisper transcription.
var EnvSecrets = []string{"GITHUB_TOKEN", "TS_AUTHKEY", "OPENROUTER_API_KEY", "WHISPER_API_KEY"}

var (
	registerMu sync.Mutex
	registered = map[string]struct{}{}
)

// Register notes secret as one more value KnownSecrets must always return,
// for a configured secret whose owner-chosen name EnvSecrets cannot list in
// advance. A caller registers each value it actually reads, as soon as it
// reads it, so every record written afterward redacts it; an empty secret
// is a no-op.
func Register(secret string) {
	if secret == "" {
		return
	}
	registerMu.Lock()
	defer registerMu.Unlock()
	registered[secret] = struct{}{}
}

// KnownSecrets reads the current values of EnvSecrets and every value
// Register has recorded, for callers that redact without a narrower list of
// secrets to hand.
func KnownSecrets() []string {
	values := make([]string, len(EnvSecrets))
	for i, name := range EnvSecrets {
		values[i] = os.Getenv(name)
	}
	registerMu.Lock()
	defer registerMu.Unlock()
	for secret := range registered {
		values = append(values, secret)
	}
	return values
}

// Rule replaces every match of Re with Repl, which may use $-references to
// the match's groups.
type Rule struct {
	Re   *regexp.Regexp
	Repl string
}

// Marker is what a redaction leaves in place of a secret.
const Marker = "[redacted]"

// ShapedRules target values shaped like a credential whether or not the
// service knows them: GitHub, API-key, JSON web token and AWS key identifiers,
// credential-bearing headers and key=value pairs. The first TokenRules of them
// match a bare token with no label.
var ShapedRules = []Rule{
	{regexp.MustCompile(`\bgh[pousr]_[A-Za-z0-9]{16,}`), Marker},
	{regexp.MustCompile(`\bgithub_pat_[A-Za-z0-9_]{16,}`), Marker},
	{regexp.MustCompile(`\bsk-[A-Za-z0-9_-]{12,}`), Marker},
	{regexp.MustCompile(`\beyJ[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]*`), Marker},
	{regexp.MustCompile(`\bAKIA[0-9A-Z]{16}\b`), Marker},
	{regexp.MustCompile(`(?i)\b(authorization|proxy-authorization|x-api-key|x-auth-token|api[-_]?key)(\s*[:=]\s*)(?:(?:bearer|basic|token)\s+)?"?[^\s",;&]+`), "${1}${2}" + Marker},
	{regexp.MustCompile(`(?i)\b(bearer)(\s+)[A-Za-z0-9._~+/=-]{6,}`), "${1}${2}" + Marker},
	{regexp.MustCompile(`(?i)\b(access_token|client_secret|password|passwd|secret|token)(=)[^\s&"']+`), "${1}${2}" + Marker},
}

// TokenRules is how many of the leading ShapedRules match a bare token.
const TokenRules = 5

// Shaped replaces every value ShapedRules match in s.
func Shaped(s string) string {
	for _, rule := range ShapedRules {
		s = rule.Re.ReplaceAllString(s, rule.Repl)
	}
	return s
}

// Scrub redacts credential URLs, the service's known secrets and the values
// shaped like a credential from s, and any further non-empty secrets given.
func Scrub(s string, secrets ...string) string {
	return Shaped(Secrets(s, append(KnownSecrets(), secrets...)...))
}
