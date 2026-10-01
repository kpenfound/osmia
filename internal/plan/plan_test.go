package plan

import (
	"errors"
	"reflect"
	"strings"
	"testing"
)

const validPlan = `{
  "version": 2,
  "units": [
    {
      "id": "parser",
      "title": "Parse the spec",
      "task": "Parse the acceptance criteria of spec.md.",
      "acceptance": ["TestParseSpec covers numbered criteria"],
      "criteria": ["spec#1"],
      "depends_on": [],
      "footprint": ["trace"]
    },
    {
      "id": "validator",
      "task": "Refuse invalid plans.",
      "acceptance": ["Errors name their unit"],
      "criteria": ["spec#2"],
      "depends_on": ["parser"],
      "footprint": ["internal.kb"]
    }
  ]
}
`

func TestParseAndEncode(t *testing.T) {
	p, err := Parse([]byte(validPlan))
	if err != nil {
		t.Fatal(err)
	}
	want := Plan{Version: Version, Units: []Unit{
		{ID: "parser", Title: "Parse the spec", Task: "Parse the acceptance criteria of spec.md.", Acceptance: []string{"TestParseSpec covers numbered criteria"}, Criteria: []string{"spec#1"}, DependsOn: []string{}, Footprint: []string{"trace"}},
		{ID: "validator", Task: "Refuse invalid plans.", Acceptance: []string{"Errors name their unit"}, Criteria: []string{"spec#2"}, DependsOn: []string{"parser"}, Footprint: []string{"internal.kb"}},
	}}
	if !reflect.DeepEqual(p, want) {
		t.Fatalf("got %#v", p)
	}
	data, err := Encode(p)
	if err != nil || !strings.HasPrefix(string(data), "{\n  \"version\": 2,\n  \"units\": [\n    {\n      \"id\": \"parser\",") || !strings.HasSuffix(string(data), "}\n") {
		t.Fatalf("encode: %v\n%s", err, data)
	}
	if again, err := Parse(data); err != nil || !reflect.DeepEqual(again, p) {
		t.Fatalf("round trip: %#v %v", again, err)
	}
	data, err = Encode(Plan{Version: Version, Units: []Unit{{ID: "a"}}})
	if err != nil || !strings.Contains(string(data), `"acceptance": [],`) || !strings.Contains(string(data), `"criteria": [],`) || !strings.Contains(string(data), `"footprint": []`) {
		t.Fatalf("absent lists: %v\n%s", err, data)
	}
	if _, err := Encode(Plan{Version: 1}); !errors.Is(err, ErrUnsupportedVersion) {
		t.Fatalf("encode version 1: %v", err)
	}
	if u, ok := p.Unit("validator"); !ok || u.ID != "validator" {
		t.Fatalf("unit lookup: %#v %v", u, ok)
	}
	if _, ok := p.Unit("absent"); ok {
		t.Fatal("absent unit found")
	}
	if got := p.Addressing(2); len(got) != 1 || got[0].ID != "validator" {
		t.Fatalf("addressing: %#v", got)
	}
	if got := p.Addressing(3); len(got) != 0 {
		t.Fatalf("addressing absent: %#v", got)
	}
	p.Units[1].Criteria = append(p.Units[1].Criteria, "spec#2")
	if got := p.Addressing(2); len(got) != 1 {
		t.Fatalf("addressing a criterion twice: %#v", got)
	}
	twice := Plan{Version: Version, Units: []Unit{{ID: "a"}, {ID: "a"}}}
	if _, ok := twice.Unit("a"); ok {
		t.Fatal("duplicate unit id found")
	}
}

func TestParseReadsCriterionProofPlans(t *testing.T) {
	p, err := Parse([]byte(`{
  "version": 1,
  "units": [
    {"id": "parser", "title": "Parse the spec", "addresses": [{"criterion": "spec#1", "proof": {"kind": "new-test", "name": "TestParseSpec"}}], "depends_on": [], "footprint": ["trace"]},
    {"id": "validator", "addresses": [{"criterion": "spec#2", "proof": {"kind": "reviewer-judgement", "name": "errors name their unit"}}], "depends_on": ["parser"], "footprint": ["internal.kb"]}
  ]
}`))
	if err != nil {
		t.Fatal(err)
	}
	want := Plan{Version: Version, Units: []Unit{
		{ID: "parser", Title: "Parse the spec", Task: "Parse the spec", Acceptance: []string{"spec#1 holds, shown by TestParseSpec (new-test)"}, Criteria: []string{"spec#1"}, DependsOn: []string{}, Footprint: []string{"trace"}},
		{ID: "validator", Task: "Make spec#2 hold.", Acceptance: []string{"spec#2 holds, shown by errors name their unit (reviewer-judgement)"}, Criteria: []string{"spec#2"}, DependsOn: []string{"parser"}, Footprint: []string{"internal.kb"}},
	}}
	if !reflect.DeepEqual(p, want) {
		t.Fatalf("got %#v", p)
	}
	if _, err := Parse([]byte(`{"version": 1, "units": [{"id": "a", "task": "t"}]}`)); err == nil || errors.Is(err, ErrUnsupportedVersion) {
		t.Fatalf("version 1 with a task field: %v", err)
	}
}

func TestParseRejects(t *testing.T) {
	for _, tc := range []struct {
		name    string
		data    string
		version bool
	}{
		{"missing version", `{"units": []}`, true},
		{"unknown version", `{"version": 3, "units": []}`, true},
		{"unknown version with unknown fields", `{"version": 0, "extra": true}`, true},
		{"null version", `{"version": null, "units": []}`, true},
		{"not an object", `[]`, false},
		{"not JSON", `version: 1`, false},
		{"unknown field", `{"version": 2, "units": [], "extra": 1}`, false},
		{"unknown unit field", `{"version": 2, "units": [{"id": "a", "size": 3}]}`, false},
		{"version 2 with addresses", `{"version": 2, "units": [{"id": "a", "addresses": []}]}`, false},
		{"trailing data", `{"version": 2, "units": []} {}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Parse([]byte(tc.data))
			if err == nil {
				t.Fatal("accepted")
			}
			if errors.Is(err, ErrUnsupportedVersion) != tc.version {
				t.Fatalf("version error = %v: %v", !tc.version, err)
			}
		})
	}
}
