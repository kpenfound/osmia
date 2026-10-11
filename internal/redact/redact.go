// Package redact removes credentials from text before it reaches a log or a
// terminal.
package redact

import (
	"os"
	"regexp"
	"strings"
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

// KnownSecrets reads the current values of EnvSecrets, for callers that
// redact without a narrower list of secrets to hand.
func KnownSecrets() []string {
	values := make([]string, len(EnvSecrets))
	for i, name := range EnvSecrets {
		values[i] = os.Getenv(name)
	}
	return values
}
