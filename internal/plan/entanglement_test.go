package plan

import (
	"reflect"
	"testing"

	"github.com/kpenfound/osmia/internal/kb"
)

func TestDecideStart(t *testing.T) {
	m := kb.Map{Version: kb.Version, Entities: []kb.Entity{
		{ID: "a", Paths: []string{"src/a"}},
		{ID: "b", Paths: []string{"src/b"}},
		{ID: "a-file", Paths: []string{"src/a/file.go"}},
		{ID: "go", Paths: []string{"src/*.go"}},
		{ID: "tests", Paths: []string{"src/*_test.go"}},
		{ID: "empty"},
	}}
	p := Plan{Version: Version, Units: []Unit{
		{ID: "base", Footprint: []string{"a"}},
		{ID: "middle", DependsOn: []string{"base"}, Footprint: []string{"b"}},
		{ID: "last", DependsOn: []string{"middle"}, Footprint: []string{"b"}},
		{ID: "file", Footprint: []string{"a-file"}},
		{ID: "glob", Footprint: []string{"go"}},
		{ID: "test", Footprint: []string{"tests"}},
		{ID: "unknown", Footprint: []string{"missing"}},
		{ID: "empty", Footprint: []string{"empty"}},
		{ID: "none"},
	}}
	for _, tc := range []struct {
		name, candidate string
		inFlight        []string
		want            []StartBlocker
	}{
		{"empty in flight", "unknown", nil, nil},
		{"disjoint", "base", []string{"glob"}, nil},
		{"direct dependency", "middle", []string{"base"}, []StartBlocker{{"base", DependencyReason}}},
		{"transitive dependency", "last", []string{"base"}, []StartBlocker{{"base", DependencyReason}}},
		{"reverse dependency", "base", []string{"last"}, []StartBlocker{{"last", DependencyReason}}},
		{"directory contains file", "file", []string{"base"}, []StartBlocker{{"base", OverlapReason}}},
		{"intersecting globs", "test", []string{"glob"}, []StartBlocker{{"glob", OverlapReason}}},
		{"unknown candidate", "unknown", []string{"glob", "base"}, []StartBlocker{{"base", MappingReason}, {"glob", MappingReason}}},
		{"unknown in flight", "base", []string{"unknown"}, []StartBlocker{{"unknown", MappingReason}}},
		{"unknown in flight blocks disjoint", "glob", []string{"unknown"}, []StartBlocker{{"unknown", MappingReason}}},
		{"no paths", "empty", []string{"base"}, []StartBlocker{{"base", MappingReason}}},
		{"no footprint", "none", []string{"base"}, []StartBlocker{{"base", MappingReason}}},
		{"sorted and unique", "unknown", []string{"test", "base", "test", "glob"}, []StartBlocker{{"base", MappingReason}, {"glob", MappingReason}, {"test", MappingReason}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := DecideStart(p, m, tc.candidate, tc.inFlight)
			if err != nil {
				t.Fatal(err)
			}
			if got.CanStart != (len(tc.want) == 0) || len(got.Blockers) != len(tc.want) {
				t.Fatalf("got %#v, want blockers %#v", got, tc.want)
			}
			if len(tc.want) > 0 && !reflect.DeepEqual(got.Blockers, tc.want) {
				t.Fatalf("got blockers %#v, want %#v", got.Blockers, tc.want)
			}
		})
	}
}

func TestDecideStartIgnoresSetOrder(t *testing.T) {
	m := kb.Map{Version: kb.Version, Entities: []kb.Entity{
		{ID: "a", Paths: []string{"src/a"}},
		{ID: "b", Paths: []string{"src/b"}},
		{ID: "c", Paths: []string{"src/a/file.go"}},
	}}
	p := Plan{Version: Version, Units: []Unit{
		{ID: "candidate", Footprint: []string{"a"}},
		{ID: "overlap", Footprint: []string{"c"}},
		{ID: "dependent", DependsOn: []string{"candidate"}, Footprint: []string{"b"}},
	}}
	want, err := DecideStart(p, m, "candidate", []string{"overlap", "dependent"})
	if err != nil {
		t.Fatal(err)
	}
	p.Units[0], p.Units[2] = p.Units[2], p.Units[0]
	m.Entities[0], m.Entities[2] = m.Entities[2], m.Entities[0]
	got, err := DecideStart(p, m, "candidate", []string{"dependent", "overlap", "dependent"})
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("got %#v, %v; want %#v", got, err, want)
	}
}

func TestDecideStartAmbiguousName(t *testing.T) {
	m := kb.Map{Version: kb.Version, Entities: []kb.Entity{
		{ID: "a", Aliases: []string{"shared"}, Paths: []string{"src/a"}},
		{ID: "b", Aliases: []string{"shared"}, Paths: []string{"src/b"}},
	}}
	p := Plan{Version: Version, Units: []Unit{{ID: "candidate", Footprint: []string{"shared"}}, {ID: "active", Footprint: []string{"b"}}}}
	got, err := DecideStart(p, m, "candidate", []string{"active"})
	if err != nil || got.CanStart || !reflect.DeepEqual(got.Blockers, []StartBlocker{{"active", MappingReason}}) {
		t.Fatalf("got %#v, %v", got, err)
	}
}
