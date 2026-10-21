package redact

import (
	"strings"
	"testing"
)

func TestSecretsRedactsCredentialURLsAndCallerSecrets(t *testing.T) {
	t.Parallel()
	got := Secrets("push https://x-access-token:hunter2@github.com/acme/widgets.git; key=mysecret", "mysecret")
	if strings.Contains(got, "hunter2") || strings.Contains(got, "mysecret") {
		t.Fatalf("leaked a secret: %q", got)
	}
}

func TestKnownSecretsRedactsJevCredentials(t *testing.T) {
	// Serial: sets the supported Jev API key environment variables.
	for _, name := range []string{"TYPESAFE_API_KEY", "OPENROUTER_API_KEY"} {
		t.Run(name, func(t *testing.T) {
			key := "synthetic-jev-credential"
			t.Setenv(name, key)
			if got := Secrets("request failed with credential "+key, KnownSecrets()...); got != "request failed with credential [redacted]" {
				t.Fatal("Jev credential was not redacted")
			}
		})
	}
}

// TestKnownSecretsIncludesRegisteredValues shows that a value Register
// records joins EnvSecrets' fixed names in every later call to
// KnownSecrets, so a caller-configured secret whose environment variable
// name is not one of EnvSecrets' defaults is still redacted once something
// has read it.
func TestKnownSecretsIncludesRegisteredValues(t *testing.T) {
	// serial: Register writes to the package's shared registry.
	const secret = "registered-under-a-custom-name"
	for _, known := range KnownSecrets() {
		if known == secret {
			t.Fatalf("secret already known before Register: %v", KnownSecrets())
		}
	}
	Register(secret)
	found := false
	for _, known := range KnownSecrets() {
		if known == secret {
			found = true
		}
	}
	if !found {
		t.Fatalf("KnownSecrets did not include the registered secret: %v", KnownSecrets())
	}
	if got := Secrets("the value is "+secret, KnownSecrets()...); strings.Contains(got, secret) {
		t.Fatalf("a registered secret was not redacted: %q", got)
	}
}

func TestRegisterIgnoresAnEmptySecret(t *testing.T) {
	// serial: inspects the package's shared registry via KnownSecrets.
	before := len(KnownSecrets())
	Register("")
	if got := len(KnownSecrets()); got != before {
		t.Fatalf("registering an empty secret changed KnownSecrets: %d, want %d", got, before)
	}
}
