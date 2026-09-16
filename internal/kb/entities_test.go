package kb

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func entity(id string, paths []string, partOf ...string) Entity {
	return Entity{ID: id, Name: id, Paths: paths, PartOf: partOf}
}

func TestValidateReportsEveryProblem(t *testing.T) {
	cases := []struct {
		name     string
		entities []Entity
		want     []Problem
	}{
		{"valid", []Entity{entity("internal", []string{"internal"}), entity("internal.trace", []string{"internal/trace", "**/*.trace"}, "internal")}, nil},
		{"duplicate id", []Entity{entity("a", nil), entity("a", nil)}, []Problem{{"a", "duplicate id"}}},
		{"invalid id", []Entity{entity("Bad_ID", nil)}, []Problem{{"Bad_ID", "id must be non-empty lowercase letters, digits, dots and hyphens"}}},
		{"missing name", []Entity{{ID: "a"}}, []Problem{{"a", "name is required and must be one line"}}},
		{"alias collides with id", []Entity{entity("trace", nil), {ID: "b", Name: "b", Aliases: []string{"TRACE"}}}, []Problem{{"b", `alias "TRACE" collides with an id or alias of entity "trace"`}}},
		{"alias collides with alias", []Entity{{ID: "a", Name: "a", Aliases: []string{"Store"}}, {ID: "b", Name: "b", Aliases: []string{"store"}}}, []Problem{{"b", `alias "store" collides with an id or alias of entity "a"`}}},
		{"empty alias", []Entity{{ID: "a", Name: "a", Aliases: []string{" "}}}, []Problem{{"a", `alias " " must be non-empty and one line`}}},
		{"empty owner", []Entity{{ID: "a", Name: "a", Owners: []string{""}}}, []Problem{{"a", `owner "" must be non-empty and one line`}}},
		{"unknown part_of", []Entity{entity("a", nil, "missing")}, []Problem{{"a", `part_of names unknown entity "missing"`}}},
		{"self cycle", []Entity{entity("a", nil, "a")}, []Problem{{"a", "part_of cycle a -> a"}}},
		{"cycle", []Entity{entity("c", nil, "b"), entity("b", nil, "a"), entity("a", nil, "c")}, []Problem{{"a", "part_of cycle a -> c -> b -> a"}}},
		{"invalid glob", []Entity{entity("a", []string{"src/[a"})}, []Problem{{"a", `path pattern "src/[a" is not a valid glob`}}},
		{"double star inside segment", []Entity{entity("a", []string{"src/a**"})}, []Problem{{"a", `path pattern "src/a**" uses ** inside a segment`}}},
		{"absolute", []Entity{entity("a", []string{"/etc"})}, []Problem{{"a", `path pattern "/etc" is absolute`}}},
		{"escapes", []Entity{entity("a", []string{"src/../../etc"})}, []Problem{{"a", `path pattern "src/../../etc" escapes the repository`}}},
		{"parent only", []Entity{entity("a", []string{".."})}, []Problem{{"a", `path pattern ".." escapes the repository`}}},
		{"unclean", []Entity{entity("a", []string{"src//x", "./src", "src/"})}, []Problem{{"a", `path pattern "src//x" has an empty or "." segment`}, {"a", `path pattern "./src" has an empty or "." segment`}, {"a", `path pattern "src/" has an empty or "." segment`}}},
		{"empty pattern", []Entity{entity("a", []string{""})}, []Problem{{"a", `path pattern "" must be non-empty and one line`}}},
		{"several entities", []Entity{entity("a", []string{"/x"}, "zz"), entity("b", []string{"../y"})}, []Problem{{"a", `path pattern "/x" is absolute`}, {"a", `part_of names unknown entity "zz"`}, {"b", `path pattern "../y" escapes the repository`}}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := Validate(Map{Version: Version, Entities: c.entities})
			var got []Problem
			if joined, ok := err.(interface{ Unwrap() []error }); ok {
				for _, e := range joined.Unwrap() {
					var p Problem
					if !errors.As(e, &p) {
						t.Fatalf("problem without entity: %v", e)
					}
					got = append(got, p)
				}
			} else if err != nil {
				t.Fatalf("unexpected error shape: %v", err)
			}
			if !reflect.DeepEqual(got, c.want) {
				t.Fatalf("got %#v\nwant %#v", got, c.want)
			}
			if _, err := Encode(Map{Version: Version, Entities: c.entities}); (err == nil) != (c.want == nil) {
				t.Fatalf("encode: %v", err)
			}
		})
	}
}

func TestParse(t *testing.T) {
	empty := Map{Version: Version, Entities: []Entity{}}
	for _, data := range []string{"", "{}", " {}\n"} {
		m, err := Parse([]byte(data))
		if err != nil || !reflect.DeepEqual(m, empty) {
			t.Fatalf("%q: %#v, %v", data, m, err)
		}
	}
	m, err := LoadFile(filepath.Join(t.TempDir(), "missing.json"))
	if err != nil || !reflect.DeepEqual(m, empty) {
		t.Fatalf("missing file: %#v, %v", m, err)
	}
	m, err = Parse([]byte(`{"version":1}`))
	if err != nil || !reflect.DeepEqual(m, empty) {
		t.Fatalf("no entities: %#v, %v", m, err)
	}
	for _, data := range []string{`{"version":2,"entities":[]}`, `{"entities":[]}`, `{"version":1,"entities":[],"extra":1}`, `{"version":1,"entities":[]} {}`, `[]`, `{"version":1,"entities":[{"id":"a","name":"a","paths":["/x"]}]}`} {
		if _, err := Parse([]byte(data)); err == nil {
			t.Fatalf("%s: accepted", data)
		}
	}
	file := filepath.Join(t.TempDir(), "entities.json")
	if err := os.WriteFile(file, []byte(`{"version":1,"entities":[{"id":"a","name":"A","aliases":["x"],"paths":["a"],"owners":["@o"],"part_of":[]}]}`), 0600); err != nil {
		t.Fatal(err)
	}
	m, err = LoadFile(file)
	want := Map{Version: 1, Entities: []Entity{{ID: "a", Name: "A", Aliases: []string{"x"}, Paths: []string{"a"}, Owners: []string{"@o"}, PartOf: []string{}}}}
	if err != nil || !reflect.DeepEqual(m, want) {
		t.Fatalf("file: %#v, %v", m, err)
	}
	if _, err := LoadFile(t.TempDir()); err == nil {
		t.Fatal("directory accepted as a map file")
	}
}

func TestEncodeIsCanonical(t *testing.T) {
	m := Map{Version: Version, Entities: []Entity{
		{ID: "b", Name: "B", Paths: []string{"z", "a"}, Owners: []string{"@z", "@a"}},
		{ID: "a", Name: "A<&>", Aliases: []string{"y", "x"}, PartOf: []string{"b"}},
	}}
	got, err := Encode(m)
	if err != nil {
		t.Fatal(err)
	}
	want := `{
  "version": 1,
  "entities": [
    {
      "id": "a",
      "name": "A<&>",
      "aliases": [
        "x",
        "y"
      ],
      "paths": [],
      "owners": [],
      "part_of": [
        "b"
      ]
    },
    {
      "id": "b",
      "name": "B",
      "aliases": [],
      "paths": [
        "z",
        "a"
      ],
      "owners": [
        "@a",
        "@z"
      ],
      "part_of": []
    }
  ]
}
`
	if string(got) != want {
		t.Fatalf("got\n%s", got)
	}
	if m.Entities[0].ID != "b" || m.Entities[0].Owners[0] != "@z" {
		t.Fatal("encode changed its input")
	}
	parsed, err := Parse(got)
	if err != nil {
		t.Fatal(err)
	}
	again, err := Encode(parsed)
	if err != nil || string(again) != want {
		t.Fatalf("round trip: %s, %v", again, err)
	}
}

func TestIDRule(t *testing.T) {
	for in, want := range map[string]string{
		"internal/trace":       "internal.trace",
		"cmd/osmia":            "cmd.osmia",
		"Docs/API_v2":          "docs.api-v2",
		"packages/@scope/ui x": "packages.-scope.ui-x",
		"web.app/next-js":      "web.app.next-js",
	} {
		if got := ID(in); got != want {
			t.Errorf("ID(%q) = %q, want %q", in, got, want)
		}
	}
	if !strings.Contains(ID("é"), "-") {
		t.Fatal("non-ASCII letters must be replaced")
	}
}
