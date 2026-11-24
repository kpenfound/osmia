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

func TestKnownSecretsIncludesRegisteredValues(t *testing.T) {
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
	before := len(KnownSecrets())
	Register("")
	if got := len(KnownSecrets()); got != before {
		t.Fatalf("registering an empty secret changed KnownSecrets: %d, want %d", got, before)
	}
}

func TestScrubRedactsCallerSecretsAndSecretShapedValues(t *testing.T) {
	t.Parallel()
	const secret = "a-secret-the-service-was-given"
	in := "known " + secret + "\nghp_abcdefghijklmnopqrstuvwxyz0123456789\nAuthorization: Bearer abcdef0123456789\npassword=hunter22\nclone https://user:pw@example.com/r.git\nplain text"
	got := Scrub(in, secret)
	for _, leaked := range []string{secret, "ghp_abcdefghijklmnopqrstuvwxyz0123456789", "abcdef0123456789", "hunter22", "user:pw"} {
		if strings.Contains(got, leaked) {
			t.Fatalf("%q survived: %q", leaked, got)
		}
	}
	if !strings.Contains(got, "plain text") || !strings.Contains(got, Marker) {
		t.Fatalf("scrubbed text %q", got)
	}
}
