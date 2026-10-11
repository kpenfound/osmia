package service

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kpenfound/osmia/internal/redact"
)

// syncBuffer is a bytes.Buffer safe for the concurrent writes a running
// service's background goroutines, such as the tailnet supervisor, make
// through its logger.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}
func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// Reset discards everything buffered so far, so a test can assert on only
// what is logged after a given point.
func (b *syncBuffer) Reset() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.buf.Reset()
}

// decodeStructuredLines fails the test if any line of text is not a JSON
// object with a timestamp, a level and a message.
func decodeStructuredLines(t *testing.T, text string) {
	t.Helper()
	for _, line := range strings.Split(strings.TrimRight(text, "\n"), "\n") {
		if line == "" {
			continue
		}
		var record struct {
			Time, Level, Msg string
		}
		if err := json.Unmarshal([]byte(line), &record); err != nil {
			t.Fatalf("line is not a JSON object: %s: %v", line, err)
		}
		if record.Time == "" || record.Level == "" || record.Msg == "" {
			t.Fatalf("line is missing a timestamp, level or message: %s", line)
		}
	}
}

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
