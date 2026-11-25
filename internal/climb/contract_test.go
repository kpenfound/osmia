package climb

import (
	"strings"
	"testing"
)

const conforming = `{"schema":1,"valid":true,"reason":"all 200 outputs match the oracle","metrics":{"wall_ms":{"value":812.4,"unit":"ms","direction":"lower"},"peak_rss_mb":{"value":143,"unit":"MB","direction":"lower"}}}`

func TestParseOutputAcceptsAConformingString(t *testing.T) {
	t.Parallel()
	for name, raw := range map[string]string{
		"object":           conforming,
		"surrounding text": "\n  " + conforming + "\n",
		"encoded string":   `"` + strings.ReplaceAll(conforming, `"`, `\"`) + `"`,
	} {
		out, err := ParseOutput(raw)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if out.Schema != 1 || !out.Valid || out.Reason != "all 200 outputs match the oracle" || len(out.Metrics) != 2 {
			t.Fatalf("%s: %+v", name, out)
		}
		if m := out.Metrics["wall_ms"]; m.Value != 812.4 || m.Unit != "ms" || m.Direction != Lower {
			t.Fatalf("%s: wall_ms %+v", name, m)
		}
	}
}

func TestParseOutputReadsAnInvalidRunWithoutMetrics(t *testing.T) {
	t.Parallel()
	out, err := ParseOutput(`{"schema":1,"valid":false,"reason":"output differs from the oracle","metrics":{}}`)
	if err != nil || out.Valid || out.Reason != "output differs from the oracle" || len(out.Metrics) != 0 {
		t.Fatalf("%+v, %v", out, err)
	}
}

func TestParseOutputRefusesNonConformingOutput(t *testing.T) {
	t.Parallel()
	metric := func(body string) string {
		return `{"schema":1,"valid":true,"reason":"ok","metrics":{"m":` + body + `}}`
	}
	for _, c := range []struct{ name, raw, want string }{
		{"empty", ``, "no output"},
		{"blank", "  \n", "no output"},
		{"not json", `wall_ms=812`, "not a JSON object"},
		{"truncated json", `{"schema":1,"valid":true`, "not a JSON object"},
		{"an array", `[1]`, "not a JSON object"},
		{"null", `null`, "not a JSON object"},
		{"a number", `7`, "not a JSON object"},
		{"trailing data", conforming + ` {}`, "after the JSON object"},
		{"bad encoded string", `"{`, "not valid JSON"},
		{"encoded non-object", `"7"`, "not a JSON object"},
		{"missing schema", `{"valid":true,"reason":"r","metrics":{}}`, `"schema" field is missing`},
		{"null schema", `{"schema":null,"valid":true,"reason":"r","metrics":{}}`, `"schema" field is missing`},
		{"wrong schema", `{"schema":2,"valid":true,"reason":"r","metrics":{}}`, "unsupported schema 2"},
		{"string schema", `{"schema":"1","valid":true,"reason":"r","metrics":{}}`, "unsupported schema"},
		{"missing valid", `{"schema":1,"reason":"r","metrics":{}}`, `"valid" field is missing`},
		{"string valid", `{"schema":1,"valid":"true","reason":"r","metrics":{}}`, `"valid" field has the wrong type`},
		{"missing reason", `{"schema":1,"valid":true,"metrics":{}}`, `"reason" field is missing`},
		{"numeric reason", `{"schema":1,"valid":true,"reason":3,"metrics":{}}`, `"reason" field has the wrong type`},
		{"missing metrics", `{"schema":1,"valid":true,"reason":"r"}`, `"metrics" field is missing`},
		{"metrics array", `{"schema":1,"valid":true,"reason":"r","metrics":[]}`, `"metrics" must be an object`},
		{"metrics null", `{"schema":1,"valid":true,"reason":"r","metrics":null}`, `"metrics" field is missing`},
		{"metric not an object", metric(`5`), "is not an object"},
		{"metric null", metric(`null`), "is not an object"},
		{"metric without value", metric(`{"unit":"ms","direction":"lower"}`), `"value" field is missing`},
		{"string value", metric(`{"value":"5","unit":"ms","direction":"lower"}`), `"value" field has the wrong type`},
		{"value overflow", metric(`{"value":1e999,"unit":"ms","direction":"lower"}`), `"value" field is invalid`},
		{"metric without unit", metric(`{"value":5,"direction":"lower"}`), `"unit" field is missing`},
		{"numeric unit", metric(`{"value":5,"unit":1,"direction":"lower"}`), `"unit" field has the wrong type`},
		{"metric without direction", metric(`{"value":5,"unit":"ms"}`), `"direction" field is missing`},
		{"unknown direction", metric(`{"value":5,"unit":"ms","direction":"up"}`), `must be "higher" or "lower"`},
		{"empty metric name", `{"schema":1,"valid":true,"reason":"r","metrics":{"":{"value":1,"unit":"","direction":"lower"}}}`, "empty name"},
	} {
		out, err := ParseOutput(c.raw)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: %+v, %v; want an error containing %q", c.name, out, err, c.want)
		}
	}
}

func TestDirectionImprovementPointsTheBetterWay(t *testing.T) {
	t.Parallel()
	if got := Higher.Improvement(12, 10); got != 2 {
		t.Fatalf("higher: %v", got)
	}
	if got := Lower.Improvement(8, 10); got != 2 {
		t.Fatalf("lower: %v", got)
	}
	if got := Lower.Improvement(12, 10); got != -2 {
		t.Fatalf("lower, worse: %v", got)
	}
}
