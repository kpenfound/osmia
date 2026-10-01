package plan

import (
	"reflect"
	"testing"

	"github.com/kpenfound/osmia/internal/kb"
)

const validSpec = "# Feature\n\n## Acceptance criteria\n\n1. Specs parse.\n2. Plans validate.\n"

var entities = kb.Map{Version: kb.Version, Entities: []kb.Entity{
	{ID: "internal.trace", Name: "internal/trace", Aliases: []string{"Trace"}, Paths: []string{"internal/trace"}},
	{ID: "internal.kb", Name: "internal/kb", Paths: []string{"internal/kb"}},
	{ID: "docs", Name: "docs"},
}}

// sample returns a plan that is valid against validSpec and entities.
func sample() Plan {
	return Plan{Version: Version, Units: []Unit{
		{ID: "parser", Task: "Parse the spec.", Acceptance: []string{"TestParseSpec passes"}, Criteria: []string{"spec#1"}, Footprint: []string{"trace"}},
		{ID: "validator", Task: "Validate plans.", Acceptance: []string{"Invalid plans are refused"}, Criteria: []string{"spec#2"}, DependsOn: []string{"parser"}, Footprint: []string{"internal.kb", "internal.trace"}},
	}}
}

func TestValidate(t *testing.T) {
	for _, tc := range []struct {
		name  string
		spec  string
		edit  func(*Plan)
		wants []Problem
	}{
		{name: "valid", edit: func(*Plan) {}, wants: []Problem{}},
		{
			name:  "unknown dependency",
			edit:  func(p *Plan) { p.Units[1].DependsOn = []string{"parser", "absent"} },
			wants: []Problem{{UnknownDependency, "validator", "", `depends on unit "absent", which the plan does not have`}},
		},
		{
			name:  "self dependency",
			edit:  func(p *Plan) { p.Units[0].DependsOn = []string{"parser"} },
			wants: []Problem{{DependencyCycle, "parser", "", "dependency cycle parser -> parser"}},
		},
		{
			name: "cycle",
			edit: func(p *Plan) {
				p.Units[0].DependsOn = []string{"validator"}
			},
			wants: []Problem{{DependencyCycle, "parser", "", "dependency cycle parser -> validator -> parser"}},
		},
		{
			name: "uncovered criterion",
			edit: func(p *Plan) {
				p.Units[1].Criteria = nil
			},
			wants: []Problem{{UncoveredCriterion, "", "spec#2", "no unit serves this criterion"}},
		},
		{
			name: "criterion the spec does not have",
			edit: func(p *Plan) {
				p.Units[1].Criteria = append(p.Units[1].Criteria, "spec#3")
			},
			wants: []Problem{{UnknownCriterion, "validator", "spec#3", "the spec has no criterion 3"}},
		},
		{
			name: "malformed citation",
			edit: func(p *Plan) {
				p.Units[1].Criteria = append(p.Units[1].Criteria, "charter#1")
			},
			wants: []Problem{{UnknownCriterion, "validator", "charter#1", "is not a spec#<n> citation"}},
		},
		{
			name: "criterion numbered twice in the spec",
			spec: validSpec + "2. Again.\n",
			edit: func(*Plan) {},
			wants: []Problem{
				{SpecProblem, "", "spec#2", "spec.md line 7: criterion 2 is numbered 2 times (lines 6, 7); it cannot be cited until renumbered"},
				{UnknownCriterion, "validator", "spec#2", "the spec numbers criterion 2 more than once, so it cannot be cited"},
				{UncoveredCriterion, "", "spec#2", "no unit serves this criterion"},
			},
		},
		{
			name:  "spec without criteria",
			spec:  "# Feature\n",
			edit:  func(p *Plan) { p.Units = nil },
			wants: []Problem{{SpecProblem, "", "", `spec.md: no "Acceptance criteria" section`}},
		},
		{
			name:  "missing task",
			edit:  func(p *Plan) { p.Units[0].Task = " " },
			wants: []Problem{{MissingTask, "parser", "", "no task is written"}},
		},
		{
			name:  "missing acceptance",
			edit:  func(p *Plan) { p.Units[0].Acceptance = []string{" "} },
			wants: []Problem{{MissingTask, "parser", "", "no acceptance is written"}},
		},
		{
			name:  "unresolved footprint",
			edit:  func(p *Plan) { p.Units[0].Footprint = []string{"trace", "absent", "docs"} },
			wants: []Problem{{UnresolvedFootprint, "parser", "", `footprint "absent" names no entity with a path in kb/entities.json`}, {UnresolvedFootprint, "parser", "", `footprint "docs" names no entity with a path in kb/entities.json`}},
		},
		{
			name:  "empty footprint",
			edit:  func(p *Plan) { p.Units[0].Footprint = nil },
			wants: []Problem{{UnresolvedFootprint, "parser", "", "declares no footprint"}},
		},
		{
			name: "invalid and duplicate unit ids",
			edit: func(p *Plan) {
				p.Units = append(p.Units, Unit{ID: "parser", Task: "t", Acceptance: []string{"a"}, Footprint: []string{"trace"}}, Unit{ID: "-bad", Task: "t", Acceptance: []string{"a"}, Footprint: []string{"trace"}})
			},
			wants: []Problem{
				{UnitProblem, "parser", "", "duplicate unit id"},
				{UnitProblem, "-bad", "", "id must be 1-128 letters, digits, '_' or '-', starting with a letter or digit"},
			},
		},
		{
			name:  "criterion cited twice by one unit",
			edit:  func(p *Plan) { p.Units[0].Criteria = append(p.Units[0].Criteria, "spec#1") },
			wants: []Problem{{UnitProblem, "parser", "spec#1", "cited more than once"}},
		},
		{
			name: "several errors at once",
			edit: func(p *Plan) {
				p.Units[0].DependsOn = []string{"validator", "ghost"}
				p.Units[0].Criteria = []string{"spec#9"}
				p.Units[0].Acceptance = nil
				p.Units[1].Footprint = []string{"nowhere"}
			},
			wants: []Problem{
				{UnknownDependency, "parser", "", `depends on unit "ghost", which the plan does not have`},
				{MissingTask, "parser", "", "no acceptance is written"},
				{UnknownCriterion, "parser", "spec#9", "the spec has no criterion 9"},
				{UnresolvedFootprint, "validator", "", `footprint "nowhere" names no entity with a path in kb/entities.json`},
				{UncoveredCriterion, "", "spec#1", "no unit serves this criterion"},
				{DependencyCycle, "parser", "", "dependency cycle parser -> validator -> parser"},
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			spec := tc.spec
			if spec == "" {
				spec = validSpec
			}
			p := sample()
			tc.edit(&p)
			got := Validate(ParseSpec(spec), p, entities)
			if !reflect.DeepEqual(got, tc.wants) {
				t.Fatalf("got  %#v\nwant %#v", got, tc.wants)
			}
		})
	}
}

func TestProblemError(t *testing.T) {
	for want, p := range map[string]Problem{
		`unit "a" spec#1: cited more than once`: {UnitProblem, "a", "spec#1", "cited more than once"},
		`unit "a": declares no footprint`:       {UnresolvedFootprint, "a", "", "declares no footprint"},
		`spec#2: no unit serves this`:           {UncoveredCriterion, "", "spec#2", "no unit serves this"},
		`spec.md: no section`:                   {SpecProblem, "", "", "spec.md: no section"},
	} {
		if got := p.Error(); got != want {
			t.Errorf("got %q, want %q", got, want)
		}
	}
}

func TestCyclesAreReportedOncePerCycle(t *testing.T) {
	p := Plan{Version: Version, Units: []Unit{
		{ID: "c", DependsOn: []string{"a"}},
		{ID: "b", DependsOn: []string{"c"}},
		{ID: "a", DependsOn: []string{"b", "d"}},
		{ID: "d", DependsOn: []string{"e"}},
		{ID: "e", DependsOn: []string{"d"}},
	}}
	want := [][]string{{"a", "b", "c", "a"}, {"d", "e", "d"}}
	if got := cycles(p); !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v", got)
	}
}
