package climb

import (
	"strings"
	"testing"
)

func expectations() Expectations {
	return Expectations{
		Primary:   Expectation{Name: "wall_ms", Direction: Lower},
		Secondary: []Expectation{{Name: "peak_rss_mb", Direction: Lower}},
	}
}

func TestExpectationsCheckTheRequiredMetrics(t *testing.T) {
	t.Parallel()
	metric := func(unit string, d Direction) Metric { return Metric{Value: 1, Unit: unit, Direction: d} }
	calibrated := expectations().WithUnits(map[string]string{"wall_ms": "ms", "peak_rss_mb": "MB"})
	for _, c := range []struct {
		name    string
		expect  Expectations
		metrics map[string]Metric
		want    string
	}{
		{"everything required", calibrated, map[string]Metric{"wall_ms": metric("ms", Lower), "peak_rss_mb": metric("MB", Lower)}, ""},
		{"extra metrics are fine", calibrated, map[string]Metric{"wall_ms": metric("ms", Lower), "peak_rss_mb": metric("MB", Lower), "extra": metric("x", Higher)}, ""},
		{"no unit recorded yet", expectations(), map[string]Metric{"wall_ms": metric("s", Lower), "peak_rss_mb": metric("GB", Lower)}, ""},
		{"primary omitted", calibrated, map[string]Metric{"peak_rss_mb": metric("MB", Lower)}, `lacks the primary metric "wall_ms"`},
		{"secondary omitted", calibrated, map[string]Metric{"wall_ms": metric("ms", Lower)}, `lacks the secondary metric "peak_rss_mb" that a guardrail limits`},
		{"primary direction differs", calibrated, map[string]Metric{"wall_ms": metric("ms", Higher), "peak_rss_mb": metric("MB", Lower)}, `"wall_ms" reports direction "higher", but the climb spec says "lower"`},
		{"secondary direction differs", calibrated, map[string]Metric{"wall_ms": metric("ms", Lower), "peak_rss_mb": metric("MB", Higher)}, `"peak_rss_mb" reports direction "higher"`},
		{"primary unit differs", calibrated, map[string]Metric{"wall_ms": metric("s", Lower), "peak_rss_mb": metric("MB", Lower)}, `"wall_ms" reports unit "s", but calibration recorded "ms"`},
		{"secondary unit differs", calibrated, map[string]Metric{"wall_ms": metric("ms", Lower), "peak_rss_mb": metric("GB", Lower)}, `"peak_rss_mb" reports unit "GB"`},
	} {
		err := c.expect.Check(Output{Schema: 1, Valid: true, Metrics: c.metrics})
		switch {
		case c.want == "" && err != nil:
			t.Errorf("%s: %v", c.name, err)
		case c.want != "" && (err == nil || !strings.Contains(err.Error(), c.want)):
			t.Errorf("%s: %v, want %q", c.name, err, c.want)
		}
	}
}

func TestExpectationsValidateRefusesMalformedSets(t *testing.T) {
	t.Parallel()
	if err := expectations().Validate(); err != nil {
		t.Fatal(err)
	}
	for name, e := range map[string]Expectations{
		"no primary name":     {Primary: Expectation{Direction: Lower}},
		"bad direction":       {Primary: Expectation{Name: "m", Direction: "up"}},
		"duplicate secondary": {Primary: Expectation{Name: "m", Direction: Lower}, Secondary: []Expectation{{Name: "m", Direction: Lower}}},
		"bad secondary":       {Primary: Expectation{Name: "m", Direction: Lower}, Secondary: []Expectation{{Name: "s"}}},
	} {
		if err := e.Validate(); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
}

func TestWithUnitsLeavesTheOriginalAlone(t *testing.T) {
	t.Parallel()
	e := expectations()
	got := e.WithUnits(map[string]string{"wall_ms": "ms"})
	if got.Primary.Unit != "ms" || got.Secondary[0].Unit != "" || e.Primary.Unit != "" {
		t.Fatalf("%+v from %+v", got, e)
	}
}
