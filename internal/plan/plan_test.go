package plan

import (
	"errors"
	"reflect"
	"strings"
	"testing"
)

const validPlan = `{
  "version": 1,
  "units": [
    {
      "id": "parser",
      "title": "Parse the spec",
      "addresses": [
        {"criterion": "spec#1", "proof": {"kind": "new-test", "name": "TestParseSpec"}}
      ],
      "depends_on": [],
      "footprint": ["trace"]
    },
    {
      "id": "validator",
      "addresses": [
        {"criterion": "spec#2", "proof": {"kind": "reviewer-judgement", "name": "errors name their unit"}}
      ],
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
	want := Plan{Version: 1, Units: []Unit{
		{ID: "parser", Title: "Parse the spec", Addresses: []Address{{"spec#1", Proof{NewTest, "TestParseSpec"}}}, DependsOn: []string{}, Footprint: []string{"trace"}},
		{ID: "validator", Addresses: []Address{{"spec#2", Proof{ReviewerJudgement, "errors name their unit"}}}, DependsOn: []string{"parser"}, Footprint: []string{"internal.kb"}},
	}}
	if !reflect.DeepEqual(p, want) {
		t.Fatalf("got %#v", p)
	}
	data, err := Encode(p)
	if err != nil || !strings.HasPrefix(string(data), "{\n  \"version\": 1,\n  \"units\": [\n    {\n      \"id\": \"parser\",") || !strings.HasSuffix(string(data), "}\n") {
		t.Fatalf("encode: %v\n%s", err, data)
	}
	if again, err := Parse(data); err != nil || !reflect.DeepEqual(again, p) {
		t.Fatalf("round trip: %#v %v", again, err)
	}
	data, err = Encode(Plan{Version: 1, Units: []Unit{{ID: "a"}}})
	if err != nil || !strings.Contains(string(data), `"addresses": [],`) || !strings.Contains(string(data), `"footprint": []`) {
		t.Fatalf("absent lists: %v\n%s", err, data)
	}
	if _, err := Encode(Plan{Version: 2}); !errors.Is(err, ErrUnsupportedVersion) {
		t.Fatalf("encode version 2: %v", err)
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
	twice := Plan{Version: 1, Units: []Unit{{ID: "a"}, {ID: "a"}}}
	if _, ok := twice.Unit("a"); ok {
		t.Fatal("duplicate unit id found")
	}
}

func TestParseRejects(t *testing.T) {
	for _, tc := range []struct {
		name    string
		data    string
		version bool
	}{
		{"missing version", `{"units": []}`, true},
		{"unknown version", `{"version": 2, "units": []}`, true},
		{"unknown version with unknown fields", `{"version": 0, "extra": true}`, true},
		{"null version", `{"version": null, "units": []}`, true},
		{"not an object", `[]`, false},
		{"not JSON", `version: 1`, false},
		{"unknown field", `{"version": 1, "units": [], "extra": 1}`, false},
		{"unknown unit field", `{"version": 1, "units": [{"id": "a", "size": 3}]}`, false},
		{"trailing data", `{"version": 1, "units": []} {}`, false},
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
