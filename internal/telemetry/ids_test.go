package telemetry

import (
	"testing"

	"github.com/kpenfound/osmia/internal/config"
)

const (
	streamA config.WorkstreamID = "w_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	streamB config.WorkstreamID = "w_bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
)

func TestDerivedIDsAreStable(t *testing.T) {
	same := func(name string, derive func() any) {
		t.Helper()
		first, second := derive(), derive()
		if first != second {
			t.Fatalf("%s changes between derivations: %v vs %v", name, first, second)
		}
	}
	same("workstream trace ID", func() any { return WorkstreamTrace(streamA) })
	same("workstream root ID", func() any { return WorkstreamRoot(streamA) })
	same("record span ID", func() any { return RecordSpan(streamA, "rec-1", 2) })
	same("workstream span ID", func() any { return WorkstreamSpan(streamA, "slot", "x") })
	same("factory trace ID", func() any { return FactoryTrace("run-1") })
	same("factory span ID", func() any { return FactorySpan("run-1", "pool", "masons") })
	if !WorkstreamTrace(streamA).IsValid() || !WorkstreamRoot(streamA).IsValid() || !RecordSpan(streamA, "r", 0).IsValid() {
		t.Fatal("derived IDs must be valid")
	}
}

func TestDifferentRecordsHaveDifferentIDs(t *testing.T) {
	traces := map[string]bool{}
	for _, id := range []string{
		WorkstreamTrace(streamA).String(), WorkstreamTrace(streamB).String(),
		FactoryTrace("w_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa").String(), FactoryTrace("run-1").String(), FactoryTrace("run-2").String(),
	} {
		if traces[id] {
			t.Fatalf("trace ID %s derived from two keys", id)
		}
		traces[id] = true
	}
	spans := map[string]string{}
	for name, id := range map[string]string{
		"root a":             WorkstreamRoot(streamA).String(),
		"root b":             WorkstreamRoot(streamB).String(),
		"record 1 rev 1":     RecordSpan(streamA, "rec-1", 1).String(),
		"record 1 rev 2":     RecordSpan(streamA, "rec-1", 2).String(),
		"record 2 rev 1":     RecordSpan(streamA, "rec-2", 1).String(),
		"record 1 rev 1 b":   RecordSpan(streamB, "rec-1", 1).String(),
		"same parts, no tag": WorkstreamSpan(streamA, "rec-1", "1").String(),
		"joined parts":       WorkstreamSpan(streamA, "record", "rec-11").String(),
		"factory":            FactorySpan("run-1", "rec-1", "1").String(),
		"factory other run":  FactorySpan("run-2", "rec-1", "1").String(),
	} {
		if other, dup := spans[id]; dup {
			t.Fatalf("%s and %s share span ID %s", name, other, id)
		}
		spans[id] = name
	}
}

func TestDerivationSeparatesParts(t *testing.T) {
	if WorkstreamSpan(streamA, "ab", "c") == WorkstreamSpan(streamA, "a", "bc") {
		t.Fatal("part boundaries are ambiguous")
	}
}
