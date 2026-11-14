package config

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func TestTelemetryDefaultsToOff(t *testing.T) {
	for _, section := range []string{"", "[telemetry]\n", "[telemetry]\nendpoint = ''\n"} {
		cfg, err := Load(fixture(t, topConfig+section, projectConfig))
		if err != nil {
			t.Fatalf("%q: %v", section, err)
		}
		if cfg.Telemetry.Enabled() || cfg.Telemetry != (Telemetry{Protocol: TelemetryProtocolGRPC}) {
			t.Fatalf("%q: telemetry %+v", section, cfg.Telemetry)
		}
	}
}

func TestTelemetryConfiguration(t *testing.T) {
	for _, tc := range []struct {
		name, section string
		want          Telemetry
	}{
		{"grpc", "endpoint = 'collector.internal:4317'\ninsecure = true\nheaders_env = 'OSMIA_OTLP_HEADERS'\n", Telemetry{Endpoint: "collector.internal:4317", Protocol: "grpc", Insecure: true, HeadersEnv: "OSMIA_OTLP_HEADERS"}},
		{"http with path", "endpoint = 'collector.internal:4318/otlp'\nprotocol = 'http/protobuf'\n", Telemetry{Endpoint: "collector.internal:4318/otlp", Protocol: "http/protobuf"}},
		{"host only", "endpoint = 'collector.example.com'\n", Telemetry{Endpoint: "collector.example.com", Protocol: "grpc"}},
		{"ipv6", "endpoint = '[::1]:4317'\ninsecure = true\n", Telemetry{Endpoint: "[::1]:4317", Protocol: "grpc", Insecure: true}},
		{"settings without an endpoint", "protocol = 'http/protobuf'\nheaders_env = 'OSMIA_OTLP_HEADERS'\n", Telemetry{Protocol: "http/protobuf", HeadersEnv: "OSMIA_OTLP_HEADERS"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg, err := Load(fixture(t, topConfig+"[telemetry]\n"+tc.section, projectConfig))
			if err != nil || cfg.Telemetry != tc.want {
				t.Fatalf("telemetry %+v, %v; want %+v", cfg, err, tc.want)
			}
		})
	}
}

func TestTelemetryValidationNamesTheField(t *testing.T) {
	const secret = "private-secret"
	for _, tc := range []struct{ name, section, field string }{
		{"bad protocol", "protocol = 'private-secret'", "telemetry.protocol"},
		{"empty protocol", "protocol = ''", "telemetry.protocol"},
		{"bad protocol without an endpoint", "protocol = 'http'", "telemetry.protocol"},
		{"scheme", "endpoint = 'https://private-secret:4317'", "telemetry.endpoint"},
		{"credentials", "endpoint = 'user:private-secret@collector:4317'", "telemetry.endpoint"},
		{"query", "endpoint = 'collector:4317?private-secret'", "telemetry.endpoint"},
		{"space", "endpoint = 'collector private-secret'", "telemetry.endpoint"},
		{"bad port", "endpoint = 'collector:99999'", "telemetry.endpoint"},
		{"zero port", "endpoint = 'collector:0'", "telemetry.endpoint"},
		{"path with grpc", "endpoint = 'collector:4317/private-secret'", "telemetry.endpoint"},
		{"no host", "endpoint = ':4317'", "telemetry.endpoint"},
		{"bad headers variable", "headers_env = 'private-secret=1'", "telemetry.headers_env"},
		{"unknown key", "token = 'private-secret'", "telemetry.token"},
		{"nested key", "[telemetry.extra]\nvalue = 'private-secret'", "telemetry.extra"},
		{"wrong endpoint type", "endpoint = ['private-secret']", "telemetry.endpoint"},
		{"wrong insecure type", "insecure = 'private-secret'", "telemetry.insecure"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg, err := LoadTopLevel(fixture(t, topConfig+"[telemetry]\n"+tc.section+"\n", projectConfig))
			var field *FieldError
			if cfg != nil || !errors.As(err, &field) || field.Field != tc.field {
				t.Fatalf("load: %+v, %v; want field %s", cfg, err, tc.field)
			}
			if strings.Contains(err.Error(), secret) {
				t.Fatalf("field error exposed the configured value: %v", err)
			}
		})
	}
}

// The configuration shows the name of the headers variable and never reads
// its value.
func TestTelemetryConfigurationNeverReadsTheHeaders(t *testing.T) {
	t.Setenv("OSMIA_OTLP_HEADERS", "authorization=private-token")
	cfg, err := Load(fixture(t, topConfig+"[telemetry]\nendpoint = 'collector:4317'\nheaders_env = 'OSMIA_OTLP_HEADERS'\n", projectConfig))
	if err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(cfg)
	if err != nil || strings.Contains(string(data), "private-token") || !strings.Contains(string(data), "OSMIA_OTLP_HEADERS") {
		t.Fatalf("configuration JSON: %s, %v", data, err)
	}
}
