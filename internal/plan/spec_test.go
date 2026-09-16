package plan

import (
	"reflect"
	"testing"
)

func TestParseSpec(t *testing.T) {
	for _, tc := range []struct {
		name     string
		content  string
		criteria []Criterion
		diags    []Diagnostic
	}{
		{
			name:     "criteria section only",
			content:  "# Feature\n\n1. Not a criterion\n\n## Acceptance criteria\n\n1. First\n2. Second\n   continues\n- bullet\n\n## Notes\n\n3. Not a criterion\n",
			criteria: []Criterion{{1, "First"}, {2, "Second continues"}},
			diags:    []Diagnostic{},
		},
		{
			name:     "nested headings stay in the section",
			content:  "## ACCEPTANCE CRITERIA ##\n### Parsing\n1. First\n# Next\n2. Outside\n",
			criteria: []Criterion{{1, "First"}},
			diags:    []Diagnostic{},
		},
		{
			name:     "code and comments are ignored",
			content:  "## Acceptance criteria\n```\n2. code\n```\n<!-- 3. hidden\n4. hidden -->\n1. First\n",
			criteria: []Criterion{{1, "First"}},
			diags:    []Diagnostic{},
		},
		{
			name:     "duplicate number",
			content:  "## Acceptance criteria\n1. First\n\n1. Again\n",
			criteria: []Criterion{{1, "First"}, {1, "Again"}},
			diags:    []Diagnostic{{4, 1, "criterion 1 is numbered 2 times (lines 2, 4); it cannot be cited until renumbered"}},
		},
		{
			name:     "missing numbers",
			content:  "## Acceptance criteria\n2. Second\n\n5. Fifth\n",
			criteria: []Criterion{{2, "Second"}, {5, "Fifth"}},
			diags: []Diagnostic{
				{2, 1, "numbering gap: criterion 1 is missing before criterion 2"},
				{4, 3, "numbering gap: criteria 3-4 are missing before criterion 5"},
			},
		},
		{
			name:     "item without text",
			content:  "## Acceptance criteria\n1.\n",
			criteria: []Criterion{},
			diags: []Diagnostic{
				{1, 0, `"Acceptance criteria" section has no numbered criteria`},
				{2, 1, "item 1 has no text and is not a criterion"},
			},
		},
		{
			name:     "no section",
			content:  "# Feature\n1. First\n",
			criteria: []Criterion{},
			diags:    []Diagnostic{{0, 0, `no "Acceptance criteria" section`}},
		},
		{
			name:     "repeated section",
			content:  "## Acceptance criteria\n1. First\n## Acceptance criteria\n2. Second\n",
			criteria: []Criterion{{1, "First"}, {2, "Second"}},
			diags:    []Diagnostic{{3, 0, `"Acceptance criteria" section appears 2 times (lines 1, 3)`}},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := ParseSpec(tc.content)
			if !reflect.DeepEqual(got.Criteria, tc.criteria) {
				t.Errorf("criteria: got %#v, want %#v", got.Criteria, tc.criteria)
			}
			if !reflect.DeepEqual(got.Diagnostics, tc.diags) {
				t.Errorf("diagnostics: got %#v, want %#v", got.Diagnostics, tc.diags)
			}
		})
	}
}

func TestSpecCriterionAndCitations(t *testing.T) {
	s := ParseSpec("## Acceptance criteria\n1. First\n2. Second\n2. Again\n")
	if c, ok := s.Criterion(1); !ok || c.Text != "First" {
		t.Fatalf("criterion 1: %#v %v", c, ok)
	}
	if _, ok := s.Criterion(2); ok {
		t.Fatal("duplicate number is citable")
	}
	if _, ok := s.Criterion(3); ok {
		t.Fatal("absent number is citable")
	}
	if Cite(12) != "spec#12" {
		t.Fatal(Cite(12))
	}
	for in, want := range map[string]int{"spec#1": 1, "spec#42": 42, "spec#0": 0, "spec#01": 0, "charter#1": 0, "spec#": 0, "spec#1 ": 0, "spec#1234567890": 0} {
		n, ok := ParseCitation(in)
		if n != want || ok != (want > 0) {
			t.Errorf("ParseCitation(%q) = %d, %v", in, n, ok)
		}
	}
}
