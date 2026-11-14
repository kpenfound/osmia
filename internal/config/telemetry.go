package config

import (
	"net/url"
	"strconv"
	"strings"
)

// The transport values of [telemetry] protocol, named as OTLP names them.
const (
	TelemetryProtocolGRPC = "grpc"
	TelemetryProtocolHTTP = "http/protobuf"
)

// Telemetry configures the optional OpenTelemetry trace export. An empty
// Endpoint (the default) turns it off and builds no exporter. Endpoint is
// host[:port], with a base path after it for the http/protobuf transport;
// Insecure alone decides whether the connection uses TLS. HeadersEnv names
// an environment variable holding the export headers as comma-separated
// name=value pairs; the variable's value is a secret that never becomes
// configuration, a response, a log record or a trace record.
type Telemetry struct {
	Endpoint   string `toml:"endpoint" json:"endpoint,omitempty"`
	Protocol   string `toml:"protocol" json:"protocol"`
	Insecure   bool   `toml:"insecure" json:"insecure"`
	HeadersEnv string `toml:"headers_env" json:"headers_env,omitempty"`
}

func defaultTelemetry() Telemetry { return Telemetry{Protocol: TelemetryProtocolGRPC} }

// Enabled reports whether an export endpoint is configured.
func (t Telemetry) Enabled() bool { return t.Endpoint != "" }

// The settings are validated whether or not telemetry is on, so setting the
// endpoint never reveals a broken protocol or variable name.
func (t Telemetry) validate(path string) error {
	switch t.Protocol {
	case TelemetryProtocolGRPC, TelemetryProtocolHTTP:
	default:
		return fieldError(path, "telemetry.protocol", "must be "+TelemetryProtocolGRPC+" or "+TelemetryProtocolHTTP)
	}
	if t.Endpoint != "" {
		if err := validateTelemetryEndpoint(t.Endpoint, t.Protocol == TelemetryProtocolHTTP); err != "" {
			return fieldError(path, "telemetry.endpoint", err)
		}
	}
	if t.HeadersEnv != "" && !environmentName.MatchString(t.HeadersEnv) {
		return fieldError(path, "telemetry.headers_env", "must name an environment variable")
	}
	return nil
}

// validateTelemetryEndpoint returns why endpoint is not host[:port], with a
// path when the transport allows one, or "" when it is.
func validateTelemetryEndpoint(endpoint string, allowPath bool) string {
	const shape = "must be host or host:port without a scheme, credentials, query or fragment"
	if strings.ContainsAny(endpoint, " \t\r\n@?#%\\") || strings.Contains(endpoint, "://") {
		return shape
	}
	u, err := url.Parse("//" + endpoint)
	if err != nil || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return shape
	}
	if port := u.Port(); port != "" {
		n, err := strconv.Atoi(port)
		if err != nil || n < 1 || n > 65535 || strconv.Itoa(n) != port {
			return "port must be between 1 and 65535"
		}
	}
	if p := strings.TrimSuffix(u.Path, "/"); p != "" && !allowPath {
		return "a path is allowed only with protocol " + TelemetryProtocolHTTP
	}
	return ""
}
