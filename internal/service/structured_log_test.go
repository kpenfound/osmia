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

// syncBuffer is a bytes.Buffer safe for the concurrent writes a running
// service's background goroutines, such as the tailnet supervisor, make
// through its logger.
type syncBuffer = fixturekit.SyncBuffer

var decodeStructuredLines = fixturekit.DecodeStructuredLines

// TestServiceWritesOnlyStructuredRecordsToItsLogger shows that a service
// started with a JSON-handler logger writes only JSON-line records through
// it, with a timestamp, a level and a message, whether the service is
// quiet or an embedded component such as the tailnet supervisor logs
// through it.
func TestServiceWritesOnlyStructuredRecordsToItsLogger(t *testing.T) {
	t.Parallel()
	opts := fixture(t)
	withListen(t, opts, "tailnet = \"osmia\"\n")
	var buf syncBuffer
	opts.Logger = slog.New(slog.NewJSONHandler(&buf, nil))
	opts.TailnetRetry = time.Hour
	opts.JoinTailnet = func(hostname, dir string) (TailnetListener, error) { return nil, errors.New("no tailnet in this test") }
	s, err := Start(context.Background(), opts)
	must(t, err)
	// The tailnet join failure logs through the service's own logger,
	// synchronously before Start returns, giving at least one record to
	// check besides the vacuous case of an otherwise quiet service.
	text := buf.String()
	decodeStructuredLines(t, text)
	if !strings.Contains(text, `"component":"tailnet"`) {
		t.Fatalf("tailnet join failure did not log through the service's logger: %s", text)
	}
	must(t, s.Close())
	decodeStructuredLines(t, buf.String())
}

// TestJevKeyUnderANonDefaultEnvVarIsRegisteredForRedaction shows that the
// Jev boost's API key is covered by redaction whichever environment
// variable api_key_env names, not only the default OPENROUTER_API_KEY:
// once the service reads the key through a real request, every later log
// write that happens to contain the key's value is redacted.
func TestJevKeyUnderANonDefaultEnvVarIsRegisteredForRedaction(t *testing.T) {
	// serial: sets process env OSMIA_TEST_JEV_KEY via t.Setenv
	const envVar = "OSMIA_TEST_JEV_KEY"
	const secret = "jev-secret-under-a-custom-name"
	t.Setenv(envVar, secret)

	opts := fixture(t)
	path := filepath.Join(opts.Config.Root, "config.toml")
	top, err := os.ReadFile(path)
	must(t, err)
	must(t, os.WriteFile(path, append(top, []byte(
		"\n[jev]\nenabled = true\nmodel = \"jev-test\"\napi_key_env = \""+envVar+"\"\n")...), 0600))

	for _, known := range redact.KnownSecrets() {
		if known == secret {
			t.Fatalf("the key was already known before any request read it: %v", redact.KnownSecrets())
		}
	}

	_, c := start(t, opts)
	// GET /v1/runtime reports the Jev boost's status, reading its key the
	// same way a real judgment request does.
	_, err = c.Runtime(context.Background())
	must(t, err)

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
