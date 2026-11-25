package config

import (
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/kinds"
)

const evaluatorSkill = "https://github.com/kpenfound/skills#hill-climb-evaluator"

const hillClimbTable = `[kinds.hill-climb]
skills = ["` + evaluatorSkill + `"]
calibration_runs = 5
repeats = 3
measurement_timeout = "40m"
`

func TestHillClimbTableParsesItsKeysAndDefaults(t *testing.T) {
	c, err := load(fixture(t, topConfig+hillClimbTable, projectConfig))
	if err != nil {
		t.Fatal(err)
	}
	s := c.HillClimb()
	if !slices.Equal(s.Skills, []string{evaluatorSkill}) || s.CalibrationRuns != 5 || s.Repeats != 3 || s.MeasurementTimeout != 40*time.Minute {
		t.Fatalf("settings: %+v", s)
	}
	for _, c := range []*Config{mustLoad(t, topConfig), mustLoad(t, topConfig+"[kinds.hill-climb]\n")} {
		s := c.HillClimb()
		if len(s.Skills) != 0 || s.CalibrationRuns != 3 || s.Repeats != 1 || s.MeasurementTimeout != 15*time.Minute {
			t.Fatalf("defaults: %+v", s)
		}
	}
}

func mustLoad(t *testing.T, top string) *Config {
	t.Helper()
	c, err := load(fixture(t, top, projectConfig))
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestHillClimbTableRefusesInvalidSettings(t *testing.T) {
	for name, c := range map[string]struct{ table, want string }{
		"unknown key":      {"[kinds.hill-climb]\nattempts = 3\n", "kinds.hill-climb"},
		"one calibration":  {"[kinds.hill-climb]\ncalibration_runs = 1\n", "calibration_runs must be a whole number from 2 to 25"},
		"zero repeats":     {"[kinds.hill-climb]\nrepeats = 0\n", "repeats must be a whole number from 1 to 25"},
		"timeout too long": {"[kinds.hill-climb]\nmeasurement_timeout = '72h'\n", "measurement_timeout"},
		"skills not list":  {"[kinds.hill-climb]\nskills = 'x'\n", "skills"},
	} {
		if _, err := load(fixture(t, topConfig+c.table, projectConfig)); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: %v, want %q", name, err, c.want)
		}
	}
}

func TestKindSkillsLoadOnlyOnTheTurnsTheKindLists(t *testing.T) {
	c := mustLoad(t, topConfig+hillClimbTable)
	for _, turn := range []kinds.Turn{kinds.TurnArchitect, kinds.TurnMetricRound, kinds.TurnEvaluatorMason, kinds.TurnEvaluatorReviewer} {
		if got := c.KindTurnSkills(kinds.HillClimb, turn); !slices.Equal(got, []string{evaluatorSkill}) {
			t.Errorf("%s: %v", turn, got)
		}
	}
	for _, turn := range []kinds.Turn{"attempt-mason", "attempt-reviewer", "chief", "committee", ""} {
		if got := c.KindTurnSkills(kinds.HillClimb, turn); len(got) != 0 {
			t.Errorf("%s: %v", turn, got)
		}
	}
	if got := c.KindTurnSkills(kinds.Feature, kinds.TurnArchitect); len(got) != 0 {
		t.Errorf("feature: %v", got)
	}
	if got := c.KindTurnSkills("mystery", kinds.TurnArchitect); len(got) != 0 {
		t.Errorf("an unregistered kind: %v", got)
	}
}

func TestWithKindSkillsAddsToTheRolesOwnSkillsOnce(t *testing.T) {
	c := mustLoad(t, topConfig+hillClimbTable)
	role := coreadapter.ExecutionSettings{Mode: "container", Skills: []string{"https://example.com/role-skill", evaluatorSkill}}
	got := c.WithKindSkills(role, kinds.HillClimb, kinds.TurnMetricRound)
	if !slices.Equal(got.Skills, []string{"https://example.com/role-skill", evaluatorSkill}) || got.Mode != "container" {
		t.Fatalf("merged: %+v", got)
	}
	plain := coreadapter.ExecutionSettings{Skills: []string{"https://example.com/role-skill"}}
	if got := c.WithKindSkills(plain, kinds.HillClimb, kinds.TurnArchitect); !slices.Equal(got.Skills, []string{"https://example.com/role-skill", evaluatorSkill}) || len(plain.Skills) != 1 {
		t.Fatalf("added: %+v, original %+v", got, plain)
	}
	if got := c.WithKindSkills(plain, kinds.HillClimb, "attempt-mason"); !slices.Equal(got.Skills, plain.Skills) {
		t.Fatalf("a turn the kind does not list: %+v", got)
	}
	if got := c.WithKindSkills(plain, kinds.Feature, kinds.TurnArchitect); !slices.Equal(got.Skills, plain.Skills) {
		t.Fatalf("feature: %+v", got)
	}
}

func TestOnlyTheEvaluatorUnitsMasonAndReviewerLoadTheKindSkills(t *testing.T) {
	c := mustLoad(t, topConfig+hillClimbTable)
	base := coreadapter.ExecutionSettings{}
	for _, role := range []string{"mason", "reviewer"} {
		if got := c.WithUnitKindSkills(base, kinds.HillClimb, role, kinds.EvaluatorUnit); !slices.Equal(got.Skills, []string{evaluatorSkill}) {
			t.Errorf("evaluator unit %s: %v", role, got.Skills)
		}
		if got := c.WithUnitKindSkills(base, kinds.HillClimb, role, "attempt-1"); len(got.Skills) != 0 {
			t.Errorf("another unit's %s: %v", role, got.Skills)
		}
		if got := c.WithUnitKindSkills(base, kinds.Feature, role, kinds.EvaluatorUnit); len(got.Skills) != 0 {
			t.Errorf("a feature workstream's %s: %v", role, got.Skills)
		}
	}
	for _, role := range []string{"chief_of_staff", "architect", "committee", "session"} {
		if got := c.WithUnitKindSkills(base, kinds.HillClimb, role, kinds.EvaluatorUnit); len(got.Skills) != 0 {
			t.Errorf("role %s: %v", role, got.Skills)
		}
	}
}
