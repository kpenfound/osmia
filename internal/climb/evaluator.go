package climb

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/kinds"
	"github.com/kpenfound/osmia/internal/plan"
	"github.com/kpenfound/osmia/internal/trace"
)

// EvaluatorUnitID is the ID of the one unit the evaluator step runs.
const EvaluatorUnitID = kinds.EvaluatorUnit

// EvaluatorFootprint is the footprint of the evaluator unit: the unit writes
// the evaluator in the dev module and any fixtures it reads, wherever the
// project keeps them, and nothing else of the workstream is in flight beside
// it.
var EvaluatorFootprint = []string{"**"}

// EvaluatorUnit returns the unit the evaluator step runs for a climb spec. A
// Hill Climb workstream has no sealed plan, so the unit is made from the
// sealed climb spec each time it is read; it is the only unit the workstream
// has.
func EvaluatorUnit(s Spec) plan.Unit {
	var task strings.Builder
	fmt.Fprintf(&task, "Write the evaluator function %q in the project's dev module, the module dagger.toml marks `main = true`, and any fixtures it reads. The service calls the function to measure every attempt of this climb, and the owner approves it before any attempt starts.\n\n", s.Evaluator)
	fmt.Fprintf(&task, "The climb's goal: %s\n\n", strings.TrimSpace(s.Goal))
	fmt.Fprintf(&task, "The function is not a @check. It takes one caller argument, `run: String!`, and may also declare `ws: Workspace!`, which the engine fills in. It threads `run` into the operations it measures, so the engine's cache cannot replay an earlier result. It returns one JSON string and nothing else: {\"schema\": 1, \"valid\": <bool>, \"reason\": <string>, \"metrics\": {<name>: {\"value\": <number>, \"unit\": <string>, \"direction\": \"higher\" | \"lower\"}}}. A result that is incorrect but scores well is `valid: false` with a reason, never a score.\n\n")
	fmt.Fprintf(&task, "It reports the primary metric %q with direction %q", s.Metric.Name, s.Metric.Direction)
	for _, l := range s.Guardrails.Limits {
		fmt.Fprintf(&task, ", the secondary metric %q with direction %q", l.Metric, l.Direction)
	}
	fmt.Fprintf(&task, ", and the same unit for each metric on every run. A run is the unit of measurement: repeated runs on unchanged code should agree to well within the minimum improvement of %v.\n\n", s.Metric.MinImprovement)
	task.WriteString("The measurement machinery belongs inside the protected paths, which freeze when the owner approves the evaluator: the evaluator, its scoring and timing helpers, the correctness oracle, the fixtures and the workload. Keep the implementation being climbed outside them. Follow the hill-climb-evaluator skill when it is loaded.")
	acceptance := []string{
		fmt.Sprintf("The dev module has a function named %q that takes `run: String!` and no other required argument.", s.Evaluator),
		fmt.Sprintf("Calling it returns one JSON string with schema 1, valid, reason and metrics, reporting %q with direction %q.", s.Metric.Name, s.Metric.Direction),
		"Every fixture and helper the evaluator reads is a file of the project that the climb spec's protected paths cover.",
		"An incorrect result is reported as valid: false with a reason rather than as a score.",
	}
	return plan.Unit{
		ID: EvaluatorUnitID, Title: "Write the evaluator " + s.Evaluator,
		Task: task.String(), Acceptance: acceptance, Criteria: []string{}, DependsOn: []string{}, Footprint: slices.Clone(EvaluatorFootprint),
		Constraints: []string{"The evaluator is a function of the dev module, never a @check and never an arbitrary shell command.", "The evaluator takes no secrets beyond the environment a check run has."},
		Boundaries:  []string{"Attempts, calibration and the approval of the evaluator are the service's. The unit writes the evaluator and its fixtures only."},
	}
}

// EvaluatorPlan returns the one-unit plan the evaluator step runs.
func EvaluatorPlan(s Spec) plan.Plan { return plan.Plan{Version: plan.Version, Units: []plan.Unit{EvaluatorUnit(s)}} }

const (
	maxEvaluatorFiles    = 4000
	maxEvaluatorFileSize = 1 << 20
)

var skippedModuleDirs = []string{".git", "node_modules", "vendor", "sdk", ".venv", "__pycache__"}

// FindEvaluator reports whether the dev module under dir already has a
// function that matches the climb spec, and what it matched. A function
// matches when the module's sources name it, spelled as the engine does in
// any case or as a snake_case, kebab-case or camelCase identifier, take a
// `run` argument and name the primary metric. The sources are read as text,
// so the check holds for every SDK; the service's calibration is what proves
// the function works.
func FindEvaluator(dir string, s Spec) (found bool, evidence string, err error) {
	want := identity(s.Evaluator)
	var files, named, run, metric []string
	err = filepath.WalkDir(dir, func(name string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			if slices.Contains(skippedModuleDirs, entry.Name()) && name != dir {
				return fs.SkipDir
			}
			return nil
		}
		if len(files) >= maxEvaluatorFiles || !entry.Type().IsRegular() {
			return nil
		}
		info, err := entry.Info()
		if err != nil || info.Size() > maxEvaluatorFileSize {
			return nil
		}
		data, err := os.ReadFile(name)
		if err != nil || strings.ContainsRune(string(data[:min(len(data), 512)]), 0) {
			return nil
		}
		rel, err := filepath.Rel(dir, name)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		files = append(files, rel)
		text := string(data)
		tokens := identifiers(text)
		if slices.Contains(tokens, want) {
			named = append(named, rel)
		}
		if slices.Contains(tokens, "run") {
			run = append(run, rel)
		}
		if strings.Contains(text, s.Metric.Name) {
			metric = append(metric, rel)
		}
		return nil
	})
	if err != nil {
		return false, "", err
	}
	switch {
	case len(named) == 0:
		return false, fmt.Sprintf("no source of the dev module names a function %q", s.Evaluator), nil
	case len(run) == 0:
		return false, fmt.Sprintf("%s names %q but no source of the dev module has a run argument", named[0], s.Evaluator), nil
	case len(metric) == 0:
		return false, fmt.Sprintf("%s names %q but no source of the dev module names the metric %q", named[0], s.Evaluator, s.Metric.Name), nil
	}
	return true, fmt.Sprintf("%s names the function %q, a run argument and the metric %q", named[0], s.Evaluator, s.Metric.Name), nil
}

func identity(name string) string {
	return strings.ToLower(strings.NewReplacer("-", "", "_", "").Replace(name))
}

func identifiers(text string) []string {
	var out []string
	seen := map[string]bool{}
	fields := strings.FieldsFunc(text, func(r rune) bool {
		return !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '_' || r == '-')
	})
	for _, f := range fields {
		id := identity(f)
		if id != "" && !seen[id] {
			seen[id] = true
			out = append(out, id)
		}
	}
	return out
}

// SealedEvaluatorPlan returns the one-unit plan the evaluator step runs for
// the climb spec of the given sealed revision. The unit reads its evaluator,
// metric and goal from the climb spec alone, so the defaults the climb spec
// takes from the settings do not matter to it.
func SealedEvaluatorPlan(r *trace.Repository, stream config.WorkstreamID, revision int) (plan.Plan, trace.Document, error) {
	spec, doc, err := SealedSpec(r, stream, revision, Defaults{})
	if err != nil {
		return plan.Plan{}, doc, err
	}
	return EvaluatorPlan(spec), doc, nil
}
