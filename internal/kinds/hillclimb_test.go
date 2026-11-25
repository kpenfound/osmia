package kinds

import (
	"slices"
	"strings"
	"testing"
	"time"
)

func TestHillClimbIsRegisteredWithItsFlags(t *testing.T) {
	k, ok := Lookup(HillClimb)
	if !ok || k.Name != "hill-climb" {
		t.Fatalf("lookup: %+v %v", k, ok)
	}
	if k.Flags != (Flags{}) {
		t.Fatalf("hill-climb flags: %+v", k.Flags)
	}
	if k.Documents.Planned() || k.Documents.SpecPath != "climb-spec.json" || k.Rounds != 1 {
		t.Fatalf("documents %+v rounds %d", k.Documents, k.Rounds)
	}
	if f, _ := Lookup(Feature); !f.Documents.Planned() || f.Rounds != 0 || len(f.SkillTurns) != 0 {
		t.Fatalf("feature documents %+v rounds %d skill turns %v", f.Documents, f.Rounds, f.SkillTurns)
	}
}

func TestHillClimbRefusesWhatItsFlagsForbid(t *testing.T) {
	k, _ := Lookup(HillClimb)
	if err := k.CheckBrief(2, false); err == nil || !strings.Contains(err.Error(), "exactly one project") {
		t.Fatalf("two projects: %v", err)
	}
	if err := k.CheckBrief(1, true); err == nil || !strings.Contains(err.Error(), "new_project") {
		t.Fatalf("new project: %v", err)
	}
	if err := k.CheckBrief(1, false); err != nil {
		t.Fatalf("one existing project: %v", err)
	}
	if err := k.CheckDependencies(); err == nil {
		t.Fatal("a hill-climb workstream cannot be a base or have dependency edges")
	}
	if err := CheckWorkgroup([]string{HillClimb, HillClimb}); err == nil || !strings.Contains(err.Error(), "exactly one member") {
		t.Fatalf("two hill-climb members: %v", err)
	}
	if err := CheckWorkgroup([]string{HillClimb, ""}); err == nil || !strings.Contains(err.Error(), "one kind") {
		t.Fatalf("mixed kinds: %v", err)
	}
	if err := CheckWorkgroup([]string{HillClimb}); err != nil {
		t.Fatalf("one member: %v", err)
	}
	if !strings.Contains(Catalog(), "- hill-climb: use when") || !strings.Contains(Catalog(), "no new_project") {
		t.Fatalf("catalog: %s", Catalog())
	}
}

func TestHillClimbSettingsDefaults(t *testing.T) {
	s, err := ParseHillClimb(nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Skills) != 0 || s.CalibrationRuns != 3 || s.Repeats != 1 || s.MeasurementTimeout != 15*time.Minute {
		t.Fatalf("defaults: %+v", s)
	}
}

func TestHillClimbSettingsParseEveryKey(t *testing.T) {
	table := map[string]any{
		"skills":              []any{"https://github.com/kpenfound/skills#hill-climb-evaluator"},
		"calibration_runs":    int64(5),
		"repeats":             int64(3),
		"measurement_timeout": "45m",
	}
	s, err := ParseHillClimb(table)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(s.Skills, []string{"https://github.com/kpenfound/skills#hill-climb-evaluator"}) || s.CalibrationRuns != 5 || s.Repeats != 3 || s.MeasurementTimeout != 45*time.Minute {
		t.Fatalf("settings: %+v", s)
	}
	k, _ := Lookup(HillClimb)
	if err := k.Settings.Validate(table); err != nil {
		t.Fatal(err)
	}
	if got := k.Settings.Skills(table); !slices.Equal(got, s.Skills) {
		t.Fatalf("skills: %v", got)
	}
}

func TestHillClimbSettingsRefuseWhatIsInvalid(t *testing.T) {
	for name, c := range map[string]struct {
		table map[string]any
		want  string
	}{
		"unknown key":          {map[string]any{"attempts": 3}, `unknown key "attempts"`},
		"skills not a list":    {map[string]any{"skills": "x"}, "list of skill references"},
		"skill not a string":   {map[string]any{"skills": []any{1}}, "list of skill references"},
		"empty skill":          {map[string]any{"skills": []any{" "}}, "empty"},
		"one calibration run":  {map[string]any{"calibration_runs": 1}, "calibration_runs must be a whole number from 2 to 25"},
		"fractional runs":      {map[string]any{"calibration_runs": 2.5}, "calibration_runs"},
		"zero repeats":         {map[string]any{"repeats": 0}, "repeats must be a whole number from 1 to 25"},
		"text repeats":         {map[string]any{"repeats": "two"}, "repeats"},
		"timeout not a string": {map[string]any{"measurement_timeout": 5}, "measurement_timeout"},
		"timeout too short":    {map[string]any{"measurement_timeout": "10s"}, "measurement_timeout"},
		"timeout too long":     {map[string]any{"measurement_timeout": "48h"}, "measurement_timeout"},
		"timeout not duration": {map[string]any{"measurement_timeout": "soon"}, "measurement_timeout"},
	} {
		if _, err := ParseHillClimb(c.table); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: %v, want %q", name, err, c.want)
		}
	}
}

func TestSkillsLoadOnlyOnTheListedTurns(t *testing.T) {
	k, _ := Lookup(HillClimb)
	for _, turn := range []Turn{TurnArchitect, TurnMetricRound, TurnEvaluatorMason, TurnEvaluatorReviewer} {
		if !k.LoadsSkillsOn(turn) {
			t.Errorf("hill-climb loads no skills on %s", turn)
		}
	}
	if k.LoadsSkillsOn("attempt-mason") || k.LoadsSkillsOn("chief") || k.LoadsSkillsOn("") {
		t.Fatal("hill-climb loads skills on a turn it does not list")
	}
	f, _ := Lookup(Feature)
	if f.LoadsSkillsOn(TurnArchitect) {
		t.Fatal("feature loads kind skills")
	}
}
