package service

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kpenfound/osmia/internal/redact"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
)

func TestServiceWritesOnlyStructuredRecordsToItsLogger(t *testing.T) {
	t.Parallel()
	opts := fixture(t)
	withListen(t, opts, "tailnet = \"osmia\"\n")
	var buf fixturekit.SyncBuffer
	opts.Logger = slog.New(slog.NewJSONHandler(&buf, nil))
	opts.TailnetRetry = time.Hour
	opts.JoinTailnet = func(hostname, dir string) (TailnetListener, error) { return nil, errors.New("no tailnet in this test") }
	s, err := Start(context.Background(), opts)
	fixturekit.Must(t, err)
	text := buf.String()
	fixturekit.DecodeStructuredLines(t, text)
	if !strings.Contains(text, `"component":"tailnet"`) {
		t.Fatalf("tailnet join failure did not log through the service's logger: %s", text)
	}
	fixturekit.Must(t, s.Close())
	fixturekit.DecodeStructuredLines(t, buf.String())
}

func TestJevKeyUnderANonDefaultEnvVarIsRegisteredForRedaction(t *testing.T) {
	const envVar = "OSMIA_TEST_JEV_KEY"
	const secret = "jev-secret-under-a-custom-name"
	t.Setenv(envVar, secret)

	opts := fixture(t)
	path := filepath.Join(opts.Config.Root, "config.toml")
	top, err := os.ReadFile(path)
	fixturekit.Must(t, err)
	fixturekit.Must(t, os.WriteFile(path, append(top, []byte(
		"\n[jev]\nmodel = \"jev-test\"\napi_key_env = \""+envVar+"\"\n")...), 0600))

	for _, known := range redact.KnownSecrets() {
		if known == secret {
			t.Fatalf("the key was already known before any request read it: %v", redact.KnownSecrets())
		}
	}

	_, c := start(t, opts)
	_, err = c.Runtime(context.Background())
	fixturekit.Must(t, err)

	found := false
	for _, known := range redact.KnownSecrets() {
		if known == secret {
			found = true
		}
	}
	if !found {
		t.Fatalf("reading the key did not register it for redaction: %v", redact.KnownSecrets())
	}
	if got := redact.Secrets("an error mentioning "+secret, redact.KnownSecrets()...); strings.Contains(got, secret) {
		t.Fatalf("a record would still leak the key: %q", got)
	}
}
