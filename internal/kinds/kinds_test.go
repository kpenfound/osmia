package kinds

import (
	"slices"
	"strings"
	"testing"
)

func TestRegistryListsFeatureOnly(t *testing.T) {
	if got := Names(); !slices.Equal(got, []string{"feature"}) {
		t.Fatalf("registered kinds: %v", got)
	}
}

func TestStoredEmptyKindIsFeatureAndUnknownKindsAreNot(t *testing.T) {
	if k, ok := Lookup(""); !ok || k.Name != Feature {
		t.Fatalf("empty kind: %+v %v", k, ok)
	}
	for _, stored := range []string{"session", "design", "mystery", "Feature"} {
		if k, ok := Lookup(stored); ok {
			t.Fatalf("%q resolved to %q", stored, k.Name)
		}
	}
}

func TestNormalizeDefaultsToFeatureAndRefusesUnregisteredKinds(t *testing.T) {
	if got, err := Normalize(""); err != nil || got != Feature {
		t.Fatalf("default: %q %v", got, err)
	}
	if got, err := Normalize("feature"); err != nil || got != Feature {
		t.Fatalf("feature: %q %v", got, err)
	}
	_, err := Normalize("mystery")
	if err == nil || !strings.Contains(err.Error(), `"mystery"`) || !strings.Contains(err.Error(), "feature") {
		t.Fatalf("unregistered kind: %v", err)
	}
}

func TestFeatureFlagsReproduceTodaysBehaviour(t *testing.T) {
	k, _ := Lookup(Feature)
	if k.Flags != (Flags{Units: true, MultiProject: true, UpstreamDrift: true, Dependencies: true, NewProject: true}) {
		t.Fatalf("flags: %+v", k.Flags)
	}
	if err := k.CheckBrief(3, true); err != nil {
		t.Fatalf("a feature brief of three projects and a new project: %v", err)
	}
	if err := k.CheckDependencies(); err != nil {
		t.Fatal(err)
	}
	if limits := k.Limits(); len(limits) != 0 {
		t.Fatalf("feature limits: %v", limits)
	}
}

func TestCheckBriefAndDependenciesRefuseWithPlainReasons(t *testing.T) {
	k := Kind{Name: "solo"}
	if err := k.CheckBrief(2, false); err == nil || !strings.Contains(err.Error(), "exactly one project") {
		t.Fatalf("two projects: %v", err)
	}
	if err := k.CheckBrief(1, true); err == nil || !strings.Contains(err.Error(), "new_project") {
		t.Fatalf("new project: %v", err)
	}
	if err := k.CheckBrief(1, false); err != nil {
		t.Fatalf("one project: %v", err)
	}
	if err := k.CheckDependencies(); err == nil || !strings.Contains(err.Error(), "base") {
		t.Fatalf("dependencies: %v", err)
	}
	if limits := k.Limits(); len(limits) != 3 {
		t.Fatalf("limits: %v", limits)
	}
}

func TestCheckWorkgroupHoldsOneKind(t *testing.T) {
	if err := CheckWorkgroup([]string{"", "feature", "feature"}); err != nil {
		t.Fatalf("a feature workgroup: %v", err)
	}
	if err := CheckWorkgroup([]string{"feature", "mystery"}); err == nil {
		t.Fatal("a workgroup with an unregistered kind")
	}
	if err := CheckWorkgroup([]string{"mystery"}); err == nil {
		t.Fatal("a workgroup of an unregistered kind")
	}
	saved := registered
	t.Cleanup(func() { registered = saved })
	registered = append(slices.Clone(registered), Kind{Name: "solo"})
	if err := CheckWorkgroup([]string{"feature", "solo"}); err == nil || !strings.Contains(err.Error(), "one kind") {
		t.Fatalf("mixed kinds: %v", err)
	}
	if err := CheckWorkgroup([]string{"solo", "solo"}); err == nil || !strings.Contains(err.Error(), "exactly one member") {
		t.Fatalf("two members of a single-member kind: %v", err)
	}
	if err := CheckWorkgroup([]string{"solo"}); err != nil {
		t.Fatalf("one member: %v", err)
	}
}

func TestCatalogListsEachKindWithUseWhenAndLimits(t *testing.T) {
	catalog := Catalog()
	for _, k := range All() {
		if !strings.Contains(catalog, "- "+k.Name+": use when "+k.UseWhen) {
			t.Fatalf("catalog lacks %s: %s", k.Name, catalog)
		}
	}
	if !strings.Contains(catalog, "Limits: none.") {
		t.Fatalf("catalog: %s", catalog)
	}
	saved := registered
	t.Cleanup(func() { registered = saved })
	registered = append(slices.Clone(registered), Kind{Name: "solo", UseWhen: "one thing."})
	if !strings.Contains(Catalog(), "- solo: use when one thing. Limits: exactly one project, never a multi-project workgroup; no new_project; no base and no dependency edges.") {
		t.Fatalf("catalog: %s", Catalog())
	}
}

func TestFeatureHooksWrapTheUnitsLoop(t *testing.T) {
	k, _ := Lookup(Feature)
	if got := k.PullRequestSection(PullRequest{Workstream: "w"}); got != "" {
		t.Fatalf("feature pull-request section: %q", got)
	}
	p := k.Progress(Units{"merged": 2, "implementing": 1})
	if p.Panel != "units" || p.Summary != "3 units: 1 implementing, 2 merged" {
		t.Fatalf("progress: %+v", p)
	}
	if got := k.Progress(nil); got.Panel != "units" || got.Summary != "" {
		t.Fatalf("progress of no units: %+v", got)
	}
	if !slices.Equal(ChiefToolNames(), []string{"move_unit"}) {
		t.Fatalf("chief tools: %v", ChiefToolNames())
	}
	if k.Loop.Name != "units" || !slices.Contains(k.Loop.States, "implementing") {
		t.Fatalf("loop: %+v", k.Loop)
	}
}
