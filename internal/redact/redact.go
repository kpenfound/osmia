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
// the tailnet auth key and the Jev boost's default API key environment
// variable.
var EnvSecrets = []string{"GITHUB_TOKEN", "TS_AUTHKEY", "OPENROUTER_API_KEY"}

// registered holds secret values Register has recorded: configured values
// with no fixed environment variable name, such as the Jev boost's API key
// when an owner points api_key_env at a name other than EnvSecrets' default.
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
