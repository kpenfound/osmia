package charter

import (
	"reflect"
	"strings"
	"testing"
)

func TestEmptyCharters(t *testing.T) {
	for name, content := range map[string]string{
		"blank":    "",
		"guidance": "# Charter\n\nWrite rules here.\n\n## Testing\n\nSome prose.\n- a bullet\n1) not our list form\n",
		"comments": "# Charter\n<!-- 1. an example rule -->\n<!--\n2. another example\n-->\n",
		"code":     "```\n1. inside a fence\n```\n~~~md\n2. inside another\n~~~\n",
		"no text":  "1.\n2.   \n",
	} {
		c := Parse(content)
		if !c.Empty() || len(c.Rules) != 0 || c.Rules == nil || c.Diagnostics == nil {
			t.Fatalf("%s: %+v", name, c)
		}
	}
	if c := Parse("1.\n"); len(c.Diagnostics) != 1 || c.Diagnostics[0].Line != 1 || !strings.Contains(c.Diagnostics[0].Message, "no text") {
		t.Fatalf("empty item: %+v", c.Diagnostics)
	}
}

func TestValidCharter(t *testing.T) {
	c := Parse(`# Charter

1. Keep changes small.
2. Write tests
   for every fix.

## Dependencies <!-- guidance -->

3. No new dependencies without a reason.
Continued lazily.
- a bullet ends the rule

### Deep ## 
   4. Indented up to three spaces.
    5. Four spaces is not a rule.
`)
	want := []Rule{
		{1, "Keep changes small.", "Charter"},
		{2, "Write tests for every fix.", "Charter"},
		{3, "No new dependencies without a reason. Continued lazily.", "Dependencies"},
		{4, "Indented up to three spaces. 5. Four spaces is not a rule.", "Deep"},
	}
	if !reflect.DeepEqual(c.Rules, want) || len(c.Diagnostics) != 0 || c.Empty() {
		t.Fatalf("%+v", c)
	}
	if r, ok := c.Rule(3); !ok || r.Heading != "Dependencies" {
		t.Fatal(r, ok)
	}
	if _, ok := c.Rule(5); ok {
		t.Fatal("missing rule is citable")
	}
}

func TestNumberingDiagnostics(t *testing.T) {
	c := Parse("# A\n2. two\n3. three\n# B\n3. three again\n7. seven\n9. nine\n3. thrice\n")
	if len(c.Rules) != 6 || c.Empty() {
		t.Fatalf("rules: %+v", c.Rules)
	}
	want := []Diagnostic{
		{2, "numbering gap: rule 1 is missing before rule 2"},
		{5, "rule 3 is numbered 3 times (lines 3, 5, 8); it cannot be cited until renumbered"},
		{6, "numbering gap: rules 4-6 are missing before rule 7"},
		{7, "numbering gap: rule 8 is missing before rule 9"},
	}
	if !reflect.DeepEqual(c.Diagnostics, want) {
		t.Fatalf("%+v", c.Diagnostics)
	}
	if _, ok := c.Rule(3); ok {
		t.Fatal("duplicated rule is citable")
	}
	if r, ok := c.Rule(2); !ok || r.Text != "two" || r.Heading != "A" {
		t.Fatal(r, ok)
	}
}

func TestCommentLinesAndFencesEndRules(t *testing.T) {
	c := Parse("1. First <!-- note --> rule\n<!-- a comment line -->\nnot continued\n2. Second\n```\ncode\n```\nnot continued either\n3. Third\n<!--\nopen\n-->\nnot continued at all\n")
	want := []Rule{{1, "First  rule", ""}, {2, "Second", ""}, {3, "Third", ""}}
	if !reflect.DeepEqual(c.Rules, want) || len(c.Diagnostics) != 0 {
		t.Fatalf("%+v", c)
	}
}
