package service

import (
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	"github.com/kpenfound/osmia/internal/telemetry"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

func withTelemetry(t *testing.T, opts Options, body string) {
	t.Helper()
	path := filepath.Join(opts.Config.Root, "config.toml")
	top, err := os.ReadFile(path)
	fixturekit.Must(t, err)
	fixturekit.Must(t, os.WriteFile(path, append(top, []byte("\n[telemetry]\n"+body)...), 0600))
}

func startTelemetry(t *testing.T, opts Options, hooks TestHooks) (*Service, *Client) {
	t.Helper()
	s, err := StartForTest(context.Background(), opts, hooks)
	fixturekit.Must(t, err)
	c := NewClient(s.Socket())
	t.Cleanup(func() {
		c.Close()
		if err := s.Close(); err != nil {
			t.Logf("service close: %v", err)
		}
	})
	return s, c
}

var probe = telemetry.Span{
	Name:  telemetry.SpanName("probe"),
	Trace: telemetry.WorkstreamTrace(fixturekit.Stream),
	ID:    telemetry.WorkstreamRoot(fixturekit.Stream),
	Start: time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC),
	End:   time.Date(2026, 1, 2, 3, 4, 6, 0, time.UTC),
	Attrs: []telemetry.Attr{telemetry.Workstream(fixturekit.Stream)},
}

// Without a [telemetry] endpoint the service builds no exporter: its sink
// is there to be handed spans and discards them.
func TestTelemetryIsOffByDefault(t *testing.T) {
	t.Parallel()
	s, _ := start(t, fixture(t))
	if s.telemetry == nil || s.telemetry.Enabled() {
		t.Fatalf("telemetry sink %+v", s.telemetry)
	}
	s.telemetry.Emit(probe)
	if (s.telemetry.Stats() != telemetry.Stats{}) {
		t.Fatalf("a sink that is off counted %+v", s.telemetry.Stats())
	}
}

// The test hook swaps in an in-memory exporter; spans emitted through the
// service's sink are exported by the flush at shutdown, and the exporter is
// still readable afterward.
func TestTelemetryHookReceivesSpansAndShutdownFlushes(t *testing.T) {
	t.Parallel()
	exporter := tracetest.NewInMemoryExporter()
	s, _ := startTelemetry(t, fixture(t), TestHooks{TelemetryExporter: exporter})
	if !s.telemetry.Enabled() {
		t.Fatal("the hook did not turn telemetry on")
	}
	s.telemetry.Emit(probe)
	fixturekit.Must(t, s.Close())
	got := namedFactorySpans(exporter, "osmia.probe")
	if len(got) != 1 {
		t.Fatalf("spans after shutdown: %+v", exporter.GetSpans())
	}
	attrs := map[string]string{}
	for _, kv := range got[0].Resource.Attributes() {
		attrs[string(kv.Key)] = kv.Value.String()
	}
	if attrs["service.name"] != "osmia" || attrs["service.version"] != "test" || len(attrs) != 2 {
		t.Fatalf("resource %+v", attrs)
	}
}

// A configured endpoint builds the exporter from the table and the headers
// variable; the configuration shows the variable's name, never its value, and
// nothing is logged with it.
func TestTelemetryEndpointBuildsTheExporterWithoutLeakingHeaders(t *testing.T) {
	t.Parallel()
	const secret = "s3cret-token"
	opts := fixture(t)
	withTelemetry(t, opts, "endpoint = '127.0.0.1:1'\ninsecure = true\nheaders_env = 'OSMIA_TEST_OTLP_HEADERS'\n")
	var logs fixturekit.SyncBuffer
	opts.Logger = slog.New(slog.NewJSONHandler(&logs, nil))
	s, c := startTelemetry(t, opts, TestHooks{Getenv: func(name string) string {
		if name == "OSMIA_TEST_OTLP_HEADERS" {
			return "authorization=Bearer " + secret
		}
		return ""
	}})
	if !s.telemetry.Enabled() {
		t.Fatalf("telemetry is off with an endpoint set:\n%s", logs.String())
	}
	view, err := c.Configuration(context.Background())
	fixturekit.Must(t, err)
	data, err := json.Marshal(view)
	fixturekit.Must(t, err)
	if strings.Contains(string(data), secret) || !strings.Contains(string(data), "OSMIA_TEST_OTLP_HEADERS") || view.Effective.Telemetry.Endpoint != "127.0.0.1:1" {
		t.Fatalf("configuration view: %s", data)
	}
	fixturekit.Must(t, s.Close())
	if strings.Contains(logs.String(), secret) || !strings.Contains(logs.String(), `"telemetry on"`) {
		t.Fatalf("log:\n%s", logs.String())
	}
}

func TestTelemetryBuildFailureLeavesTheServiceRunning(t *testing.T) {
	t.Parallel()
	const secret = "s3cret-token"
	opts := fixture(t)
	withTelemetry(t, opts, "endpoint = '127.0.0.1:1'\ninsecure = true\nheaders_env = 'OSMIA_TEST_OTLP_HEADERS'\n")
	var logs fixturekit.SyncBuffer
	opts.Logger = slog.New(slog.NewJSONHandler(&logs, nil))
	s, c := startTelemetry(t, opts, TestHooks{Getenv: func(string) string { return "malformed-" + secret }})
	if s.telemetry == nil || s.telemetry.Enabled() {
		t.Fatalf("telemetry sink %+v", s.telemetry)
	}
	if _, err := c.Configuration(context.Background()); err != nil {
		t.Fatal(err)
	}
	out := logs.String()
	if strings.Contains(out, secret) || !strings.Contains(out, "telemetry is off") || !strings.Contains(out, "OSMIA_TEST_OTLP_HEADERS") {
		t.Fatalf("log:\n%s", out)
	}
}

// The exporter is built once, at start: a reload that changes [telemetry]
// reports it as needing a restart and keeps the running settings.
func TestReloadKeepsTheTelemetrySettingsUntilRestart(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	opts := fixture(t)
	files := fixturekit.ConfigFiles(t, opts.Config.Root)
	_, c := start(t, opts)
	files.Write(t, files.TopText+"[telemetry]\nendpoint = 'collector.internal:4317'\nprotocol = 'http/protobuf'\n[capacity]\nreviewers = 5\n")
	reloaded, err := c.Reload(ctx)
	fixturekit.Must(t, err)
	if !reflect.DeepEqual(reloaded.RestartRequired, []string{"telemetry"}) {
		t.Fatalf("restart required: %+v", reloaded)
	}
	now, err := c.Configuration(ctx)
	fixturekit.Must(t, err)
	if now.Effective.Telemetry != (config.Telemetry{Protocol: config.TelemetryProtocolGRPC}) || now.Effective.Capacity.Reviewers != 5 {
		t.Fatalf("effective configuration after reload: %+v", now.Effective)
	}
}
