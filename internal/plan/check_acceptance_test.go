package plan

import (
	"strings"
	"testing"
)

func TestWholeCheckObligation(t *testing.T) {
	t.Parallel()
	for _, text := range []string{
		"all `dagger checks` pass", "dagger check passes", "full check run is green",
		"All checks pass.", "Every check must pass", "All project checks must pass",
		"The full test suite passes", "The entire project check suite is green",
		"Run the full check suite", "Run `dagger check`", "The complete test suite must be run",
		"All tests must pass", "All checks are run", "Acceptance requires all checks to pass",
		"The feature works. All checks pass.",
		"Run all checks", "Execute every test", "Run all checks in the repository",
		"Run all checks in CI", "Run all checks for the whole project",
		"Run all checks in internal/plan and run all checks",
		"Run all checks in ./internal/plan. Run all checks.",
		"Run all checks under `./internal/plan`. All checks pass.",
		"All checks must be run in ./internal/plan; run all checks",
		"Run all checks in ./internal/plan and run all checks",
		"Run all checks. Run all checks in ./internal/plan",
	} {
		t.Run(text, func(t *testing.T) {
			if !wholeCheckObligation(text) {
				t.Errorf("missed obligation: %s", text)
			}
		})
	}
	for _, text := range []string{
		"TestResume passes", "Existing TestResume keeps passing", "all internal/plan tests pass",
		"All parser tests pass", "all checks in internal/plan pass", "Run the parser tests",
		"Run all checks in internal/plan", "Run all checks for internal/plan",
		"Run every test in internal/plan", "All checks pass in internal/plan",
		"All checks must be run under internal/plan", "Every test passes for the parser test set",
		"Execute every test in the parser suite", "Run all tests from the parser test set",
		"Run all checks in package plan", "Run all checks under `internal/plan`",
		"Run all checks in ParserRegression", "Run every test of the parser package",
		"Run all `dagger checks` in internal/plan",
		"Run all checks in ./internal/plan", "Run every test in `./internal/plan`",
		"Run every test under `./internal/plan`", "Run all checks in ./internal/...",
		"Execute every test under ../internal/plan", "Run all checks for `../internal/plan`",
		"All checks must be run under ./internal/plan", "All checks are run in `./internal/plan`",
		"All checks pass in ./internal/plan", "All checks must pass for `./internal/plan`",
		"All checks in ./internal/plan pass", "Run all checks in github.com/kpenfound/osmia/internal/plan",
		"Run all checks in ./internal/plan. All checks pass in `./internal/plan`.",
		"dagger check --go-package=internal/plan passes", "Run dagger check --go-test=TestResume",
		"acceptance never requires all checks to pass", "Do not require all checks to pass",
		"No full check run is required", "Without requiring all checks to pass",
		"The validator rejects acceptance saying 'all checks pass'", "table tests flag 'full check run is green'",
		"The detector flags all checks pass", "The reviewer sees that all checks pass",
		`Examples include "all checks pass" and "dagger check passes"`,
		"Examples include `all checks pass`", "`All checks Pass`", "Table tests flag all checks pass", "The text describes all checks pass", "The policy prohibits a full check run is green",
		"A full check run is not required", "All checks need not pass", "All checks must never pass", "Acceptance doesn't require all checks to pass", "The plan doesn’t require all checks to pass", "Avoid requiring all checks to pass",
		"The help page describes how to run all checks", "Tests cover running all checks",
		"Acceptance prohibits running all checks", "Do not run all checks", "Never run the full check suite",
		"Run TestResume", "Run all ParserRegression tests", "Run all checks under ./internal/plan",
	} {
		t.Run(text, func(t *testing.T) {
			if wholeCheckObligation(text) {
				t.Errorf("flagged mention or scoped tests: %s", text)
			}
		})
	}
}

func TestWholeCheckObligationQuotes(t *testing.T) {
	t.Parallel()
	for _, quote := range []struct{ name, open, close string }{
		{"double", `"`, `"`},
		{"curly-double", "“", "”"},
		{"single", "'", "'"},
		{"curly-single", "‘", "’"},
		{"backtick", "`", "`"},
	} {
		t.Run(quote.name, func(t *testing.T) {
			for _, statement := range []string{
				"Run all checks", "Run the full check suite", "Run dagger check",
				"Execute every test", "All checks must be run", "All checks pass",
				"Full check run is green", "dagger check passes",
			} {
				quoted := quote.open + statement + quote.close
				for _, prefix := range []string{"", "The help page displays ", "The help page renders ", "The validator rejects ", "Table tests flag "} {
					text := prefix + quoted + "."
					t.Run(text, func(t *testing.T) {
						if wholeCheckObligation(text) {
							t.Errorf("flagged quoted statement: %s", text)
						}
					})
				}
				for _, text := range []string{statement, "The help page displays " + quoted + ". Run all checks.", "The help page renders " + quoted + "; all checks pass"} {
					t.Run(text, func(t *testing.T) {
						if !wholeCheckObligation(text) {
							t.Errorf("missed direct requirement: %s", text)
						}
					})
				}
			}
			for _, subject := range []string{"dagger check", "all checks", "the full check suite"} {
				quoted := quote.open + subject + quote.close
				for _, text := range []string{"Run " + quoted, quoted + " passes"} {
					t.Run(text, func(t *testing.T) {
						if !wholeCheckObligation(text) {
							t.Errorf("missed requirement with quoted command or subject: %s", text)
						}
					})
				}
			}
		})
	}
}

func TestCheckAcceptanceProblemsOnlyChangedText(t *testing.T) {
	t.Parallel()
	oldSpec := "## Acceptance criteria\n1. All checks pass\n2. The parser accepts input\n"
	graph, err := Parse([]byte(validPlan))
	if err != nil {
		t.Fatal(err)
	}
	graph.Units[0].Acceptance = []string{"dagger check passes", "The parser accepts input"}
	before, err := Encode(graph)
	if err != nil {
		t.Fatal(err)
	}
	graph.Units[0].Task = "Parse new syntax"
	graph.Units[0].Acceptance = []string{"The parser accepts new syntax", "dagger check passes"}
	after, err := Encode(graph)
	if err != nil {
		t.Fatal(err)
	}
	newSpec := strings.Replace(oldSpec, "accepts input", "accepts new syntax", 1)
	if got := CheckAcceptanceProblems(newSpec, string(after), oldSpec, string(before)); len(got) != 0 {
		t.Fatalf("legacy carryover: %v", got)
	}
	graph.Units[0].Acceptance = append(graph.Units[0].Acceptance, "All checks pass")
	after, err = Encode(graph)
	if err != nil {
		t.Fatal(err)
	}
	newSpec += "3. Full check run is green\n"
	got := CheckAcceptanceProblems(newSpec, string(after), oldSpec, string(before))
	if len(got) != 2 || !strings.Contains(got[0], "spec#3") || !strings.Contains(got[1], `unit "parser": acceptance item 3`) {
		t.Fatalf("changed items: %v", got)
	}
	for _, problem := range got {
		if !strings.Contains(problem, "observable behaviour or the tests the unit adds") {
			t.Fatal(problem)
		}
	}
	if got := CheckAcceptanceProblems(oldSpec, string(before), "", ""); len(got) != 2 {
		t.Fatalf("new draft: %v", got)
	}
}
