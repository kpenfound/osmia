package telemetry

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/kpenfound/osmia/internal/config"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
)

// The OTLP default ports, used when the endpoint names none.
const (
	defaultGRPCPort = "4317"
	defaultHTTPPort = "4318"
	tracesPath      = "/v1/traces"
)

// requestTimeout bounds one export request of the built exporters.
const requestTimeout = 10 * time.Second

var headerName = regexp.MustCompile("^[A-Za-z0-9!#$%&'*+.^_`|~-]+$")

// ParseHeaders reads export headers from raw, comma-separated name=value
// pairs with percent-encoded values, the format OTLP uses. variable names the
// environment variable raw came from. An error names the variable and the
// entry's position, never the entry, since its value is a secret.
func ParseHeaders(variable, raw string) (map[string]string, error) {
	headers := map[string]string{}
	for i, entry := range strings.Split(raw, ",") {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		name, value, ok := strings.Cut(entry, "=")
		name = strings.TrimSpace(name)
		if !ok || !headerName.MatchString(name) {
			return nil, fmt.Errorf("telemetry.headers_env: entry %d of %s is not name=value", i+1, variable)
		}
		value = strings.TrimSpace(value)
		if decoded, err := url.PathUnescape(value); err == nil {
			value = decoded
		}
		headers[name] = value
	}
	return headers, nil
}

// closingExporter shuts down the gRPC connection its exporter was given.
type closingExporter struct {
	sdktrace.SpanExporter
	conn *grpc.ClientConn
}

func (c closingExporter) Shutdown(ctx context.Context) error {
	return errors.Join(c.SpanExporter.Shutdown(ctx), c.conn.Close())
}

// NewExporter builds the OTLP trace exporter cfg describes, sending headers with
// every request. Every option is set from cfg, so no OTEL_* environment
// variable changes where or how spans are sent. Building makes no connection:
// the first export does.
func NewExporter(ctx context.Context, cfg config.Telemetry, headers map[string]string) (sdktrace.SpanExporter, error) {
	if !cfg.Enabled() {
		return nil, errors.New("telemetry.endpoint is empty")
	}
	if headers == nil {
		headers = map[string]string{}
	}
	switch cfg.Protocol {
	case config.TelemetryProtocolGRPC:
		return newGRPCExporter(ctx, cfg, headers)
	case config.TelemetryProtocolHTTP:
		return newHTTPExporter(ctx, cfg, headers)
	}
	return nil, fmt.Errorf("telemetry.protocol %q is not %s or %s", cfg.Protocol, config.TelemetryProtocolGRPC, config.TelemetryProtocolHTTP)
}

// endpoint splits the configured endpoint into host:port, with port filled in
// when missing, and the base path.
func endpoint(cfg config.Telemetry, port string) (hostPort, path string, err error) {
	u, err := url.Parse("//" + cfg.Endpoint)
	if err != nil || u.Hostname() == "" {
		return "", "", errors.New("telemetry.endpoint is not host[:port]")
	}
	hostPort = u.Host
	if u.Port() == "" {
		hostPort = net.JoinHostPort(u.Hostname(), port)
	}
	return hostPort, strings.TrimSuffix(u.Path, "/"), nil
}

func newGRPCExporter(ctx context.Context, cfg config.Telemetry, headers map[string]string) (sdktrace.SpanExporter, error) {
	hostPort, _, err := endpoint(cfg, defaultGRPCPort)
	if err != nil {
		return nil, err
	}
	creds := credentials.NewTLS(&tls.Config{MinVersion: tls.VersionTLS12})
	if cfg.Insecure {
		creds = insecure.NewCredentials()
	}
	// The exporter's own dialing reads OTEL_EXPORTER_OTLP_* settings such as the
	// compression, so the connection is dialed here and handed over.
	conn, err := grpc.NewClient(hostPort, grpc.WithTransportCredentials(creds))
	if err != nil {
		return nil, fmt.Errorf("telemetry.endpoint: %w", err)
	}
	exp, err := otlptracegrpc.New(ctx, otlptracegrpc.WithGRPCConn(conn), otlptracegrpc.WithHeaders(headers), otlptracegrpc.WithTimeout(requestTimeout))
	if err != nil {
		conn.Close()
		return nil, err
	}
	return closingExporter{exp, conn}, nil
}

func newHTTPExporter(ctx context.Context, cfg config.Telemetry, headers map[string]string) (sdktrace.SpanExporter, error) {
	hostPort, path, err := endpoint(cfg, defaultHTTPPort)
	if err != nil {
		return nil, err
	}
	scheme, tlsConfig := "https", &tls.Config{MinVersion: tls.VersionTLS12}
	if cfg.Insecure {
		scheme, tlsConfig = "http", nil
	}
	// A full URL sets host, path and security together, replacing anything the
	// environment would supply.
	return otlptracehttp.New(ctx,
		otlptracehttp.WithEndpointURL(scheme+"://"+hostPort+path+tracesPath),
		otlptracehttp.WithTLSClientConfig(tlsConfig),
		otlptracehttp.WithHeaders(headers),
		otlptracehttp.WithCompression(otlptracehttp.NoCompression),
		otlptracehttp.WithEncoding(otlptracehttp.EncodingProtobuf),
		otlptracehttp.WithTimeout(requestTimeout),
	)
}
