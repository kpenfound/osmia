package climb

import (
	"slices"
	"strings"
	"testing"
	"time"
)

const validSpec = `{
  "version": 1,
  "goal": "Make the parser faster.",
  "evaluator": "parse-bench",
  "metric": {"name": "wall_ms", "direction": "lower", "min_improvement": 5},
  "guardrails": {
    "checks": ["lint"],
    "limits": [{"metric": "peak_rss_mb", "direction": "lower", "max": 200}]
  },
  "protected": {"fixtures": ["testdata/corpus"], "add": ["scripts/bench.sh"]},
  "stop": {"max_attempts": 20, "max_cost_usd": 40, "max_wall_clock": "6h", "plateau": 5, "target": 500}
}`

const daggerToml = `[modules.osmia-dev]
source = "dev"
main = true

[modules.go]
source = "go"
`

func TestCheckAcceptsAValidClimbSpecAndAppliesDefaults(t *testing.T) {
	s, problems := Check([]byte(validSpec), Defaults{})
	if len(problems) != 0 {
		t.Fatalf("problems: %v", problems)
	}
	if s.Repeats != 1 {
		t.Fatalf("repeats default: %d", s.Repeats)
	}
	if s.Metric.Direction != Lower || s.Metric.MinImprovement != 5 || s.Evaluator != "parse-bench" {
		t.Fatalf("spec: %+v", s)
	}
	paths, err := s.ProtectedPaths([]byte(daggerToml))
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"dagger.lock", "dagger.toml", "dev", "scripts/bench.sh", "testdata/corpus"}
	if !slices.Equal(paths, want) {
		t.Fatalf("protected paths: %v, want %v", paths, want)
	}
	if d, ok := s.WallClock(); !ok || d != 6*time.Hour {
		t.Fatalf("wall clock: %v %v", d, ok)
	}
}

func TestConfiguredDefaultRepeatsApplyOnlyWhenTheSpecSetsNone(t *testing.T) {
	s, problems := Check([]byte(validSpec), Defaults{Repeats: 3})
	if len(problems) != 0 || s.Repeats != 3 {
		t.Fatalf("default repeats: %d %v", s.Repeats, problems)
	}
	explicit := strings.Replace(validSpec, `"goal"`, `"repeats": 5, "goal"`, 1)
	s, problems = Check([]byte(explicit), Defaults{Repeats: 3})
	if len(problems) != 0 || s.Repeats != 5 {
		t.Fatalf("explicit repeats: %d %v", s.Repeats, problems)
	}
}

func TestDefaultProtectedPathsHoldWithoutAdditions(t *testing.T) {
	minimal := `{"version":1,"goal":"g","evaluator":"e","metric":{"name":"m","direction":"higher","min_improvement":1},"guardrails":{},"protected":{},"stop":{"max_attempts":3}}`
	s, err := Parse([]byte(minimal), Defaults{})
	if err != nil {
		t.Fatal(err)
	}
	paths, err := s.ProtectedPaths([]byte(daggerToml))
	if err != nil || !slices.Equal(paths, []string{"dagger.lock", "dagger.toml", "dev"}) {
		t.Fatalf("defaults: %v %v", paths, err)
	}
	if _, err := s.ProtectedPaths([]byte("[modules.x]\nsource = \"x\"\n")); err == nil {
		t.Fatal("a project with no main module has no dev module to protect")
	}
}

func TestCheckRefusesMissingAndInvalidFields(t *testing.T) {
	replace := func(old, new string) string { return strings.Replace(validSpec, old, new, 1) }
	for name, c := range map[string]struct{ content, want string }{
		"not json":                  {"goal: x", "not valid JSON"},
		"two objects":               {validSpec + validSpec, "one JSON object"},
		"unknown field":             {replace(`"goal"`, `"extra": 1, "goal"`), "unknown field"},
		"no goal":                   {replace(`"goal": "Make the parser faster.",`, ``), "goal is required"},
		"blank goal":                {replace("Make the parser faster.", "  "), "goal is required"},
		"no evaluator":              {replace(`"evaluator": "parse-bench",`, ``), "evaluator is required"},
		"evaluator not a name":      {replace("parse-bench", "parse bench"), "not a function name"},
		"no metric name":            {replace(`"name": "wall_ms"`, `"name": ""`), "metric.name is required"},
		"unknown direction":         {replace(`"lower", "min`, `"smaller", "min`), `metric.direction must be "higher" or "lower"`},
		"no direction":              {replace(`"direction": "lower", "min_improvement"`, `"min_improvement"`), "metric.direction"},
		"zero minimum improvement":  {replace(`"min_improvement": 5`, `"min_improvement": 0`), "metric.min_improvement must be a number above zero"},
		"negative min improvement":  {replace(`"min_improvement": 5`, `"min_improvement": -1`), "metric.min_improvement"},
		"no minimum improvement":    {replace(`, "min_improvement": 5`, ``), "metric.min_improvement"},
		"repeats too many":          {replace(`"goal"`, `"repeats": 99, "goal"`), "repeats must be"},
		"repeats negative":          {replace(`"goal"`, `"repeats": -1, "goal"`), "repeats must be"},
		"wrong version":             {replace(`"version": 1`, `"version": 2`), "version must be 1"},
		"no attempt limit":          {replace(`"max_attempts": 20, `, ``), "stop.max_attempts is required"},
		"cost negative":             {replace(`"max_cost_usd": 40`, `"max_cost_usd": -4`), "stop.max_cost_usd"},
		"wall clock not a duration": {replace(`"6h"`, `"long"`), "stop.max_wall_clock"},
		"wall clock too short":      {replace(`"6h"`, `"5s"`), "stop.max_wall_clock"},
		"plateau negative":          {replace(`"plateau": 5`, `"plateau": -2`), "stop.plateau"},
		"limit on the primary":      {replace(`"metric": "peak_rss_mb"`, `"metric": "wall_ms"`), "limits the primary metric"},
		"limit without bound":       {replace(`, "max": 200`, ``), "needs a min or a max"},
		"limit min above max":       {replace(`"max": 200`, `"min": 300, "max": 200`), "min above its max"},
		"limit without direction":   {replace(`"direction": "lower", "max"`, `"max"`), "guardrails.limits[0].direction"},
		"duplicate limit":           {replace(`"limits": [`, `"limits": [{"metric": "peak_rss_mb", "direction": "lower", "max": 1},`), "a second time"},
		"empty check":               {replace(`"lint"`, `""`), "empty check name"},
		"absolute fixture":          {replace(`"testdata/corpus"`, `"/etc"`), "protected.fixtures"},
		"escaping addition":         {replace(`"scripts/bench.sh"`, `"../x"`), "protected.add"},
	} {
		_, problems := Check([]byte(c.content), Defaults{})
		if !slices.ContainsFunc(problems, func(p string) bool { return strings.Contains(p, c.want) }) {
			t.Errorf("%s: problems %v, want one containing %q", name, problems, c.want)
		}
		if _, err := Parse([]byte(c.content), Defaults{}); err == nil {
			t.Errorf("%s: parse accepted it", name)
		}
	}
}

func TestCheckListsEveryProblemAtOnce(t *testing.T) {
	_, problems := Check([]byte(`{"version":1,"metric":{"direction":"sideways"},"stop":{}}`), Defaults{})
	for _, want := range []string{"goal is required", "evaluator is required", "metric.name is required", "metric.direction", "metric.min_improvement", "stop.max_attempts"} {
		if !slices.ContainsFunc(problems, func(p string) bool { return strings.Contains(p, want) }) {
			t.Errorf("problems %v lack %q", problems, want)
		}
	}
}

func TestSpecSuppliesTheMeasurementInputs(t *testing.T) {
	s, err := Parse([]byte(validSpec), Defaults{})
	if err != nil {
		t.Fatal(err)
	}
	e := s.Expectations()
	if e.Primary.Name != "wall_ms" || e.Primary.Direction != Lower || len(e.Secondary) != 1 || e.Secondary[0].Name != "peak_rss_mb" || e.Secondary[0].Direction != Lower {
		t.Fatalf("expectations: %+v", e)
	}
	if err := e.Validate(); err != nil {
		t.Fatal(err)
	}
	limits := s.Limits()
	if len(limits) != 1 || limits[0].Metric != "peak_rss_mb" || limits[0].Max == nil || *limits[0].Max != 200 || limits[0].Min != nil {
		t.Fatalf("limits: %+v", limits)
	}
}

func TestEncodeRoundTripsAndRefusesAnInvalidSpec(t *testing.T) {
	s, err := Parse([]byte(validSpec), Defaults{})
	if err != nil {
		t.Fatal(err)
	}
	data, err := Encode(s)
	if err != nil {
		t.Fatal(err)
	}
	again, err := Parse(data, Defaults{})
	if err != nil || again.Goal != s.Goal || again.Repeats != 1 {
		t.Fatalf("round trip: %+v %v", again, err)
	}
	s.Metric.MinImprovement = 0
	if _, err := Encode(s); err == nil {
		t.Fatal("encoded an invalid spec")
	}
}
