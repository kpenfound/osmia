package flakes

import (
	"context"
	"slices"
	"testing"

	"github.com/kpenfound/osmia/internal/config"
)

func TestRunKeysKeepTheRunsOfDifferentWorkstreamsApart(t *testing.T) {
	t.Parallel()
	const other = "w_00000000000000000000000000000002"
	if RunKey(origin, "units/a/checks-1.json") == RunKey(other, "units/a/checks-1.json") {
		t.Fatal("two workstreams' runs at one path share a key")
	}
	if OccurrenceID(RunKey(origin, "units/a/checks-1.json"), groupA) == OccurrenceID(RunKey(other, "units/a/checks-1.json"), groupA) {
		t.Fatal("two workstreams' runs at one path share an occurrence")
	}
	o := Occurrence{Run: RunKey(origin, "final/checks-2.json"), Source: Source{Workstream: origin, FinalReview: true}}
	if got := o.RunPath(); got != "final/checks-2.json" {
		t.Fatalf("run path %q", got)
	}
}

func TestGroupFlakySplitsTheFlakyChecksOfARunByGroup(t *testing.T) {
	t.Parallel()
	const (
		a1 = "dag://go/packages/tests/test?go-package=internal/a&go-test=TestOne"
		a2 = "dag://go/packages/tests/test?go-package=internal/a&go-test=TestTwo"
		b1 = "dag://go/packages/tests/test?go-package=internal/b&go-test=TestOne"
	)
	got := GroupFlaky(nil, []string{a1, b1, a2, a1, " "})
	if len(got) != 2 || !slices.Equal(got[0].Links, []string{a1, a2}) || !slices.Equal(got[1].Links, []string{b1}) || got[0].Group == got[1].Group || got[0].Unresolved || got[1].Unresolved {
		t.Fatalf("groups %+v", got)
	}
	cloud := GroupFlaky([]string{a1}, []string{"test:unknown"})
	if len(cloud) != 1 || cloud[0].Group != "test:unknown" || !cloud[0].Unresolved {
		t.Fatalf("cloud groups %+v", cloud)
	}
	if links := CheckLinks("lint", a1, "dag+check://x", a1); !slices.Equal(links, []string{a1, "dag+check://x"}) {
		t.Fatalf("links %v", links)
	}
}

func TestFlakeFixWorkstreamsAreThoseTheRecordOpenedOrJoined(t *testing.T) {
	t.Parallel()
	const fix, closed, joined, bystander = "w_0000000000000000000000000000000a", "w_0000000000000000000000000000000b", "w_0000000000000000000000000000000c", "w_0000000000000000000000000000000d"
	f := newFixture(t)
	ctx := context.Background()
	record := func(group, run string) {
		t.Helper()
		_, _, err := f.store.Record(ctx, OccurrenceInput{Group: group, Run: RunKey(origin, run), Source: Source{Workstream: origin, Unit: "u"}, Links: []string{"l"}})
		if err != nil {
			t.Fatal(err)
		}
	}
	record(groupA, "units/u/checks-1.json")
	record(groupA, "units/u/checks-2.json")
	record(groupB, "units/u/checks-3.json")
	for _, step := range []func() error{
		func() error {
			_, err := f.store.Reserve(ctx, groupA, RunKey(origin, "units/u/checks-1.json"))
			return err
		},
		func() error {
			_, err := f.store.Opened(ctx, groupA, RunKey(origin, "units/u/checks-1.json"), closed)
			return err
		},
		func() error { _, err := f.store.CloseWorkstream(ctx, closed); return err },
		func() error {
			_, err := f.store.Reserve(ctx, groupA, RunKey(origin, "units/u/checks-2.json"))
			return err
		},
		func() error {
			_, err := f.store.Opened(ctx, groupA, RunKey(origin, "units/u/checks-2.json"), fix)
			return err
		},
		func() error {
			_, err := f.store.Join(ctx, groupB, RunKey(origin, "units/u/checks-3.json"), joined)
			return err
		},
	} {
		if err := step(); err != nil {
			t.Fatal(err)
		}
	}
	for workstream, want := range map[string]bool{fix: true, closed: true, joined: true, bystander: false, "": false, string(origin): false} {
		got, err := f.store.IsFlakeFix(config.WorkstreamID(workstream))
		if err != nil || got != want {
			t.Fatalf("workstream %q: flake-fix %v, %v; want %v", workstream, got, err, want)
		}
	}
}
