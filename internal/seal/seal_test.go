package seal

import (
	"slices"
	"strings"
	"testing"

	"github.com/kpenfound/osmia/internal/kb"
	"github.com/kpenfound/osmia/internal/plan"
	"github.com/kpenfound/osmia/internal/shed"
)

func valid() Seal {
	return Seal{Version: Version, Seal: 1, Round: 2, Revision: shed.Pin{Spec: 3, Plan: 1}, SpecHash: SpecHash("# Spec\n"),
		Base: Base{Remote: "upstream", Branch: "main", Commit: "0123456789abcdef0123456789abcdef01234567"}, Branch: "osmia/w_1", Workspace: "/root/branches/p_1/w_1",
		Footprints: []Footprint{{Unit: "resume", Entities: []string{"internal.trace"}, Paths: []string{"internal/trace/**"}}, {Unit: "dedupe"}}}
}

func TestEncodeAndParseRoundTrip(t *testing.T) {
	t.Parallel()
	data, err := Encode(valid())
	if err != nil {
		t.Fatal(err)
	}
	want := `{
  "version": 1,
  "seal": 1,
  "round": 2,
  "revision": {
    "spec": 3,
    "plan": 1
  },
  "spec_hash": "sha256:a5a24af8f9c9e3ba6bd1a0a6bd1f2b7d5a2c8e5b9c6d0a1f2e3d4c5b6a7f8e9d",
  "base": {
    "remote": "upstream",
    "branch": "main",
    "commit": "0123456789abcdef0123456789abcdef01234567"
  },
  "branch": "osmia/w_1",
  "workspace": "/root/branches/p_1/w_1",
  "footprints": [
    {
      "unit": "resume",
      "entities": [
        "internal.trace"
      ],
      "paths": [
        "internal/trace/**"
      ]
    },
    {
      "unit": "dedupe",
      "entities": [],
      "paths": []
    }
  ]
}
`
	// The hash is of the content, not of the example.
	want = strings.Replace(want, "sha256:a5a24af8f9c9e3ba6bd1a0a6bd1f2b7d5a2c8e5b9c6d0a1f2e3d4c5b6a7f8e9d", SpecHash("# Spec\n"), 1)
	if string(data) != want {
		t.Fatalf("encoded:\n%s\nwant:\n%s", data, want)
	}
	parsed, err := Parse(data)
	if err != nil {
		t.Fatal(err)
	}
	if parsed.Seal != 1 || parsed.Round != 2 || parsed.Revision != (shed.Pin{Spec: 3, Plan: 1}) || parsed.Base.Commit != valid().Base.Commit || len(parsed.Footprints) != 2 || parsed.Footprints[1].Unit != "dedupe" || len(parsed.Footprints[1].Entities) != 0 {
		t.Fatalf("parsed %+v", parsed)
	}
	if !strings.HasPrefix(SpecHash("x"), "sha256:") || len(SpecHash("x")) != 71 || SpecHash("x") == SpecHash("y") {
		t.Fatalf("spec hash %q", SpecHash("x"))
	}
}

func TestInvalidSealsAreRefused(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		change func(*Seal)
		want   string
	}{
		"version":   {func(s *Seal) { s.Version = 2 }, "unsupported seal version 2"},
		"sealing":   {func(s *Seal) { s.Seal = 0 }, "requires the sealing and the round"},
		"round":     {func(s *Seal) { s.Round = 0 }, "requires the sealing and the round"},
		"revision":  {func(s *Seal) { s.Revision.Plan = 0 }, "requires the revisions it seals"},
		"hash":      {func(s *Seal) { s.SpecHash = "abc" }, "requires the hash of the spec"},
		"base":      {func(s *Seal) { s.Base.Commit = "" }, "requires the upstream remote, branch and commit"},
		"branch":    {func(s *Seal) { s.Branch = "" }, "requires the feature branch and its workspace"},
		"workspace": {func(s *Seal) { s.Workspace = "" }, "requires the feature branch and its workspace"},
		"unit":      {func(s *Seal) { s.Footprints[1].Unit = "" }, `footprint of unit "" is missing its unit`},
		"twice":     {func(s *Seal) { s.Footprints[1].Unit = "resume" }, `footprint of unit "resume" is missing its unit or repeats one`},
	} {
		s := valid()
		tc.change(&s)
		if _, err := Encode(s); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: encoding: %v, want %q", name, err, tc.want)
		}
		data, err := Encode(valid())
		if err != nil {
			t.Fatal(err)
		}
		if _, err := Parse(data); err != nil {
			t.Fatalf("%s: parsing the valid seal: %v", name, err)
		}
	}
	for name, data := range map[string]string{
		"unknown field": `{"version":1,"seal":1,"round":1,"revision":{"spec":1,"plan":1},"spec_hash":"` + SpecHash("") + `","base":{"remote":"u","branch":"main","commit":"c"},"branch":"b","workspace":"w","footprints":[],"extra":1}`,
		"two objects":   `{"version":1,"seal":1,"round":1,"revision":{"spec":1,"plan":1},"spec_hash":"` + SpecHash("") + `","base":{"remote":"u","branch":"main","commit":"c"},"branch":"b","workspace":"w","footprints":[]} {}`,
		"not json":      `seal`,
	} {
		if _, err := Parse([]byte(data)); err == nil {
			t.Errorf("%s was parsed", name)
		}
	}
}

func TestTakeResolvesEveryUnitAndNamesWhatItCannot(t *testing.T) {
	t.Parallel()
	m := kb.Map{Version: kb.Version, Entities: []kb.Entity{
		{ID: "internal.trace", Name: "trace", Aliases: []string{"history"}, Paths: []string{"internal/trace/**"}},
		{ID: "internal.trace.git", Name: "git", PartOf: []string{"internal.trace"}, Paths: []string{"internal/trace/git.go"}},
		{ID: "docs", Name: "docs"},
	}}
	p := plan.Plan{Version: plan.Version, Units: []plan.Unit{
		{ID: "resume", Footprint: []string{"history"}},
		{ID: "dedupe", Footprint: []string{"internal.trace.git", "docs", "nowhere"}},
		{ID: "empty"},
	}}
	footprints, unresolved := Take(p, m)
	want := []Footprint{
		{Unit: "resume", Entities: []string{"internal.trace", "internal.trace.git"}, Paths: []string{"internal/trace/**", "internal/trace/git.go"}},
		{Unit: "dedupe", Entities: []string{"internal.trace.git"}, Paths: []string{"internal/trace/git.go"}},
		{Unit: "empty", Entities: []string{}, Paths: []string{}},
	}
	if len(footprints) != len(want) {
		t.Fatalf("footprints %+v", footprints)
	}
	for i := range want {
		if footprints[i].Unit != want[i].Unit || !slices.Equal(footprints[i].Entities, want[i].Entities) || !slices.Equal(footprints[i].Paths, want[i].Paths) {
			t.Errorf("footprint %d: %+v, want %+v", i, footprints[i], want[i])
		}
	}
	if !slices.Equal(unresolved, []string{"dedupe: docs", "dedupe: nowhere"}) {
		t.Fatalf("unresolved %v", unresolved)
	}
}
