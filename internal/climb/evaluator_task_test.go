package climb

import (
	"strings"
	"testing"
)

func TestEvaluatorUnitTaskStatesTheContractAndNamesNoSkill(t *testing.T) {
	t.Parallel()
	spec := Spec{Version: 1, Goal: "Make the build faster", Evaluator: "measure-wall", Metric: MetricSpec{Name: "wall", Direction: Lower, MinImprovement: 0.5}}
	task := EvaluatorUnit(spec).Task
	for _, want := range []string{
		"`run: String!`", "`ws: Workspace!`", "one JSON string", `"schema": 1`, "`valid: false`",
		"not a @check", "dev module", "calibration runs the evaluator", "no baseline", "the attempt is discarded",
		"Go alone decides whether an attempt is kept", "protected paths", "dagger.toml, dagger.lock",
		`primary metric "wall" with direction "lower"`, "minimum improvement of 0.5",
	} {
		if !strings.Contains(task, want) {
			t.Errorf("the evaluator task lacks %q:\n%s", want, task)
		}
	}
	lower := strings.ToLower(task)
	for _, word := range []string{"skill", "hill-climb-evaluator", "when it is loaded"} {
		if strings.Contains(lower, word) {
			t.Errorf("the evaluator task mentions %q", word)
		}
	}
}

func TestAttemptUnitTaskKeepsTheRuleThatGoAloneDecides(t *testing.T) {
	t.Parallel()
	spec := Spec{Version: 1, Goal: "Make the build faster", Evaluator: "measure-wall", Metric: MetricSpec{Name: "wall", Direction: Lower, MinImprovement: 0.5}}
	unit := AttemptPlanUnit(spec, 3)
	if !strings.Contains(unit.Task, "Go alone decides whether it is kept") || !strings.Contains(unit.Task, "you never judge whether a score improved") {
		t.Errorf("the attempt task lost the rule that Go alone decides:\n%s", unit.Task)
	}
	if strings.Contains(strings.ToLower(unit.Task), "skill") {
		t.Errorf("the attempt task names a skill")
	}
}
