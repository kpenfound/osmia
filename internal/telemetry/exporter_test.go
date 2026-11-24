package telemetry

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/redact"
	coltrace "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"
	"google.golang.org/protobuf/proto"
)

type received struct {
	header   http.Header
	metadata metadata.MD
	path     string
	request  *coltrace.ExportTraceServiceRequest
}

type fakeCollector struct {
	addr   string
	mu     sync.Mutex
	got    []received
	conns  atomic.Int64
	status int
	body   string
	coltrace.UnimplementedTraceServiceServer
}

func (c *fakeCollector) record(r received) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.got = append(c.got, r)
}

func (c *fakeCollector) requests() []received {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]received(nil), c.got...)
}

func (c *fakeCollector) connections() int64 { return c.conns.Load() }

func (c *fakeCollector) Export(ctx context.Context, req *coltrace.ExportTraceServiceRequest) (*coltrace.ExportTraceServiceResponse, error) {
	md, _ := metadata.FromIncomingContext(ctx)
	c.record(received{metadata: md, request: req})
	return &coltrace.ExportTraceServiceResponse{}, nil
}

type countingListener struct {
	net.Listener
	n *atomic.Int64
}

func (l countingListener) Accept() (net.Conn, error) {
	conn, err := l.Listener.Accept()
	if err == nil {
		l.n.Add(1)
	}
	return conn, err
}

func collector(t *testing.T) *fakeCollector {
	t.Helper()
	c := &fakeCollector{status: http.StatusOK}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	c.addr = listener.Addr().String()
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		req := &coltrace.ExportTraceServiceRequest{}
		if proto.Unmarshal(body, req) != nil {
			req = nil
		}
		c.record(received{header: r.Header.Clone(), path: r.URL.Path, request: req})
		if c.status != http.StatusOK {
			http.Error(w, c.body, c.status)
			return
		}
		w.Header().Set("Content-Type", "application/x-protobuf")
		out, _ := proto.Marshal(&coltrace.ExportTraceServiceResponse{})
		w.Write(out)
	})}
	go server.Serve(countingListener{listener, &c.conns})
	t.Cleanup(func() { server.Close() })
	return c
}

func grpcCollector(t *testing.T) *fakeCollector {
	t.Helper()
	c := &fakeCollector{}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	c.addr = listener.Addr().String()
	server := grpc.NewServer()
	coltrace.RegisterTraceServiceServer(server, c)
	go server.Serve(countingListener{listener, &c.conns})
	t.Cleanup(server.Stop)
	return c
}

func resourceAttributes(req *coltrace.ExportTraceServiceRequest) map[string]string {
	attrs := map[string]string{}
	for _, rs := range req.GetResourceSpans() {
		for _, kv := range rs.GetResource().GetAttributes() {
			attrs[kv.Key] = kv.GetValue().GetStringValue()
		}
	}
	return attrs
}

func TestParseHeaders(t *testing.T) {
	got, err := ParseHeaders("V", " authorization = Bearer abc%3D , x-team=osmia,, ")
	if err != nil || len(got) != 2 || got["authorization"] != "Bearer abc=" || got["x-team"] != "osmia" {
		t.Fatalf("headers %+v, %v", got, err)
	}
	for _, bad := range []string{"novalue", "=value", "bad name=v", "ok=1,broken"} {
		_, err := ParseHeaders("OSMIA_TEST_HEADERS", bad)
		if err == nil || !strings.Contains(err.Error(), "OSMIA_TEST_HEADERS") || strings.Contains(err.Error(), "broken") || strings.Contains(err.Error(), "novalue") {
			t.Fatalf("%q: error %v must name the variable and no entry", bad, err)
		}
	}
}

func sinkFor(t *testing.T, cfg config.Telemetry, opts Options) *Sink {
	t.Helper()
	sink, err := New(cfg, opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { sink.Close() })
	return sink
}

func TestHeadersFromTheEnvironmentVariableReachTheExporter(t *testing.T) {
	const headers = "authorization=Bearer s3cret-token,x-team=osmia"
	getenv := func(name string) string {
		if name == "OSMIA_TEST_HEADERS" {
			return headers
		}
		return ""
	}
	t.Run("http/protobuf", func(t *testing.T) {
		c := collector(t)
		sink := sinkFor(t, config.Telemetry{Endpoint: c.addr + "/otlp/", Protocol: config.TelemetryProtocolHTTP, Insecure: true, HeadersEnv: "OSMIA_TEST_HEADERS"}, Options{Getenv: getenv, Version: "v9"})
		sink.Emit(span)
		flush(t, sink)
		got := c.requests()
		if len(got) != 1 || got[0].path != "/otlp/v1/traces" || got[0].header.Get("Authorization") != "Bearer s3cret-token" || got[0].header.Get("X-Team") != "osmia" {
			t.Fatalf("collector saw %+v", got)
		}
		if got[0].request == nil || len(got[0].request.ResourceSpans) != 1 || got[0].request.ResourceSpans[0].ScopeSpans[0].Spans[0].Name != "osmia.unit.state" {
			t.Fatalf("request %+v", got[0].request)
		}
	})
	t.Run("grpc", func(t *testing.T) {
		c := grpcCollector(t)
		sink := sinkFor(t, config.Telemetry{Endpoint: c.addr, Protocol: config.TelemetryProtocolGRPC, Insecure: true, HeadersEnv: "OSMIA_TEST_HEADERS"}, Options{Getenv: getenv})
		sink.Emit(span)
		flush(t, sink)
		got := c.requests()
		if len(got) != 1 || got[0].metadata.Get("authorization")[0] != "Bearer s3cret-token" || got[0].metadata.Get("x-team")[0] != "osmia" {
			t.Fatalf("collector saw %+v", got)
		}
	})
}

func TestHeaderValuesNeverAppearInLogsOrErrors(t *testing.T) {
	const secret = "s3cret-token"
	getenv := func(string) string { return "authorization=Bearer " + secret }
	c := collector(t)
	c.status, c.body = http.StatusUnauthorized, "bad credential Bearer "+secret
	var logs bytes.Buffer
	sink := sinkFor(t, config.Telemetry{Endpoint: c.addr, Protocol: config.TelemetryProtocolHTTP, Insecure: true, HeadersEnv: "OSMIA_TEST_HEADERS"}, Options{Getenv: getenv, Logger: slog.New(slog.NewTextHandler(&logs, nil)), ExportTimeout: 2 * time.Second})
	sink.Emit(span)
	flush(t, sink)
	if sink.Stats().Failed != 1 || !strings.Contains(logs.String(), "export failed") {
		t.Fatalf("expected a logged failure: %+v\n%s", sink.Stats(), logs.String())
	}
	if err := sink.Close(); err != nil && strings.Contains(err.Error(), secret) {
		t.Fatalf("shutdown error: %v", err)
	}
	if strings.Contains(logs.String(), secret) {
		t.Fatalf("log exposed the header value:\n%s", logs.String())
	}
	if got := redact.Secrets("sent Bearer "+secret, redact.KnownSecrets()...); strings.Contains(got, secret) {
		t.Fatalf("redact does not know the header value: %q", got)
	}
	_, err := New(config.Telemetry{Endpoint: c.addr, Protocol: config.TelemetryProtocolHTTP, Insecure: true, HeadersEnv: "OSMIA_TEST_HEADERS"}, Options{Getenv: func(string) string { return "oops-" + secret }})
	if err == nil || strings.Contains(err.Error(), secret) || !strings.Contains(err.Error(), "OSMIA_TEST_HEADERS") {
		t.Fatalf("malformed headers: %v", err)
	}
}

func TestOTELEnvironmentChangesNeitherDestinationNorResource(t *testing.T) {
	decoy := collector(t)
	decoyGRPC := grpcCollector(t)
	for name, value := range map[string]string{
		"OTEL_EXPORTER_OTLP_ENDPOINT":           "http://" + decoy.addr,
		"OTEL_EXPORTER_OTLP_TRACES_ENDPOINT":    "http://" + decoy.addr + "/decoy/v1/traces",
		"OTEL_EXPORTER_OTLP_HEADERS":            "x-from-env=1",
		"OTEL_EXPORTER_OTLP_TRACES_HEADERS":     "x-from-env-traces=1",
		"OTEL_EXPORTER_OTLP_COMPRESSION":        "gzip",
		"OTEL_EXPORTER_OTLP_PROTOCOL":           "http/json",
		"OTEL_EXPORTER_OTLP_TIMEOUT":            "1",
		"OTEL_EXPORTER_OTLP_INSECURE":           "false",
		"OTEL_EXPORTER_OTLP_CERTIFICATE":        "/nonexistent/ca.pem",
		"OTEL_EXPORTER_OTLP_CLIENT_KEY":         "/nonexistent/key.pem",
		"OTEL_EXPORTER_OTLP_CLIENT_CERTIFICATE": "/nonexistent/cert.pem",
		"OTEL_SERVICE_NAME":                     "from-env",
		"OTEL_RESOURCE_ATTRIBUTES":              "deployment.environment=env,service.name=also-env",
		"OTEL_TRACES_EXPORTER":                  "console",
		"OTEL_SDK_DISABLED":                     "true",
	} {
		t.Setenv(name, value)
	}
	t.Run("http/protobuf", func(t *testing.T) {
		c := collector(t)
		sink := sinkFor(t, config.Telemetry{Endpoint: c.addr, Protocol: config.TelemetryProtocolHTTP, Insecure: true}, Options{Version: "v9"})
		sink.Emit(span)
		flush(t, sink)
		got := c.requests()
		if len(got) != 1 || got[0].path != "/v1/traces" || got[0].header.Get("Content-Type") != "application/x-protobuf" || got[0].header.Get("Content-Encoding") != "" || got[0].header.Get("X-From-Env") != "" || got[0].header.Get("X-From-Env-Traces") != "" {
			t.Fatalf("collector saw %+v", got)
		}
		if res := resourceAttributes(got[0].request); len(res) != 2 || res["service.name"] != "osmia" || res["service.version"] != "v9" {
			t.Fatalf("resource %+v", res)
		}
	})
	t.Run("grpc", func(t *testing.T) {
		c := grpcCollector(t)
		sink := sinkFor(t, config.Telemetry{Endpoint: c.addr, Protocol: config.TelemetryProtocolGRPC, Insecure: true}, Options{})
		sink.Emit(span)
		flush(t, sink)
		got := c.requests()
		if len(got) != 1 || len(got[0].metadata.Get("x-from-env")) != 0 || len(got[0].metadata.Get("grpc-encoding")) != 0 {
			t.Fatalf("collector saw %+v", got)
		}
		if res := resourceAttributes(got[0].request); len(res) != 1 || res["service.name"] != "osmia" {
			t.Fatalf("resource %+v", res)
		}
	})
	if n := len(decoy.requests()) + len(decoyGRPC.requests()); n != 0 || decoy.connections() != 0 || decoyGRPC.connections() != 0 {
		t.Fatalf("the environment's endpoint received %d requests", n)
	}
}

func TestBuildingAnExporterMakesNoConnection(t *testing.T) {
	for _, protocol := range []string{config.TelemetryProtocolGRPC, config.TelemetryProtocolHTTP} {
		c := grpcCollector(t)
		sink := sinkFor(t, config.Telemetry{Endpoint: c.addr, Protocol: protocol, Insecure: true}, Options{})
		time.Sleep(50 * time.Millisecond)
		if !sink.Enabled() || c.connections() != 0 {
			t.Fatalf("%s: enabled %v, %d connections before any span", protocol, sink.Enabled(), c.connections())
		}
	}
}
