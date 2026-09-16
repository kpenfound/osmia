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

func proof(kind ProofKind) Proof { return Proof{Kind: kind, Name: "check"} }

// sample returns a plan that is valid against validSpec and entities.
func sample() Plan {
	return Plan{Version: Version, Units: []Unit{
		{ID: "parser", Addresses: []Address{{"spec#1", proof(NewTest)}}, Footprint: []string{"trace"}},
		{ID: "validator", Addresses: []Address{{"spec#2", proof(ExistingTest)}}, DependsOn: []string{"parser"}, Footprint: []string{"internal.kb", "internal.trace"}},
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
			name: "every proof kind is accepted",
			edit: func(p *Plan) {
				p.Units[0].Addresses[0].Proof = proof(ScriptedCheck)
				p.Units[1].Addresses[0].Proof = proof(ReviewerJudgement)
			},
			wants: []Problem{},
		},
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
				p.Units[1].Addresses = nil
			},
			wants: []Problem{{UncoveredCriterion, "", "spec#2", "no unit addresses this criterion"}},
		},
		{
			name: "criterion the spec does not have",
			edit: func(p *Plan) {
				p.Units[1].Addresses = append(p.Units[1].Addresses, Address{"spec#3", proof(NewTest)})
			},
			wants: []Problem{{UnknownCriterion, "validator", "spec#3", "the spec has no criterion 3"}},
		},
		{
			name: "malformed citation",
			edit: func(p *Plan) {
				p.Units[1].Addresses = append(p.Units[1].Addresses, Address{"charter#1", proof(NewTest)})
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
				{UncoveredCriterion, "", "spec#2", "no unit addresses this criterion"},
			},
		},
		{
			name:  "spec without criteria",
			spec:  "# Feature\n",
			edit:  func(p *Plan) { p.Units = nil },
			wants: []Problem{{SpecProblem, "", "", `spec.md: no "Acceptance criteria" section`}},
		},
		{
			name:  "missing proof",
			edit:  func(p *Plan) { p.Units[0].Addresses[0].Proof = Proof{} },
			wants: []Problem{{MissingProof, "parser", "spec#1", "no proof is named"}},
		},
		{
			name:  "unnamed proof",
			edit:  func(p *Plan) { p.Units[0].Addresses[0].Proof = Proof{Kind: NewTest, Name: " "} },
			wants: []Problem{{MissingProof, "parser", "spec#1", "the new-test proof has no name"}},
		},
		{
			name:  "unknown proof kind",
			edit:  func(p *Plan) { p.Units[0].Addresses[0].Proof = proof("vibes") },
			wants: []Problem{{MissingProof, "parser", "spec#1", `proof kind "vibes" is not one of new-test, existing-test, scripted-check or reviewer-judgement`}},
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
				p.Units = append(p.Units, Unit{ID: "parser", Footprint: []string{"trace"}}, Unit{ID: "-bad", Footprint: []string{"trace"}})
			},
			wants: []Problem{
				{UnitProblem, "parser", "", "duplicate unit id"},
				{UnitProblem, "-bad", "", "id must be 1-128 letters, digits, '_' or '-', starting with a letter or digit"},
			},
		},
		{
			name:  "criterion addressed twice by one unit",
			edit:  func(p *Plan) { p.Units[0].Addresses = append(p.Units[0].Addresses, Address{"spec#1", proof(NewTest)}) },
			wants: []Problem{{UnitProblem, "parser", "spec#1", "addressed more than once"}},
		},
		{
			name: "several errors at once",
			edit: func(p *Plan) {
				p.Units[0].DependsOn = []string{"validator", "ghost"}
				p.Units[0].Addresses = []Address{{"spec#9", Proof{}}}
				p.Units[1].Footprint = []string{"nowhere"}
			},
			wants: []Problem{
				{UnknownDependency, "parser", "", `depends on unit "ghost", which the plan does not have`},
				{UnknownCriterion, "parser", "spec#9", "the spec has no criterion 9"},
				{MissingProof, "parser", "spec#9", "no proof is named"},
				{UnresolvedFootprint, "validator", "", `footprint "nowhere" names no entity with a path in kb/entities.json`},
				{UncoveredCriterion, "", "spec#1", "no unit addresses this criterion"},
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
		`unit "a" spec#1: no proof is named`: {MissingProof, "a", "spec#1", "no proof is named"},
		`unit "a": declares no footprint`:    {UnresolvedFootprint, "a", "", "declares no footprint"},
		`spec#2: no unit addresses this`:     {UncoveredCriterion, "", "spec#2", "no unit addresses this"},
		`spec.md: no section`:                {SpecProblem, "", "", "spec.md: no section"},
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
