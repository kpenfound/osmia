package jev_test

import (
	"context"
	"testing"

	"github.com/kpenfound/agentcore/jev/jevtest"
	"github.com/kpenfound/osmia/internal/jev"
)

// A binding pick the service refused stays the occurrence's binding result,
// carries its refusal, and is not asked again, after a restart included.
func TestARefusedBindingPickKeepsItsRefusalAndIsNeverAskedAgain(t *testing.T) {
	f := newFixture(t)
	settings := testSettings()
	p := &jevtest.Provider{Results: []jevtest.Result{pick("ratify", 0.97, "defer")}}
	j := judge(&settings, p, "key")
	key := jev.RatificationKey(1, 1, 1)
	first := j.Decide(context.Background(), f.repo, decisionRequest(jev.KindRatification, key, "state", "ratify"))
	if first.Status != jev.Binding || first.Refused != "" {
		t.Fatalf("first = %+v", first)
	}
	revisions := func() int { return len(f.records(t, first.Judgment)) }
	before := revisions()

	if err := j.Refuse(context.Background(), f.repo, streamID, first.Judgment, "the plan is not valid"); err != nil {
		t.Fatal(err)
	}
	if err := j.Refuse(context.Background(), f.repo, streamID, first.Judgment, "another reason"); err != nil {
		t.Fatal(err)
	}
	if revisions() != before+1 {
		t.Fatalf("refusing twice wrote %d revisions", revisions()-before)
	}
	check := func(res jev.Result) {
		t.Helper()
		if res.Status != jev.Binding || res.Pick != "ratify" || res.Confidence != 0.97 || res.Refused != "the plan is not valid" || res.Judgment != first.Judgment || !res.Recorded {
			t.Fatalf("result = %+v", res)
		}
	}
	lookup, err := j.Lookup(f.repo, streamID, jev.KindRatification, key)
	if err != nil {
		t.Fatal(err)
	}
	check(lookup)
	f.reopen(t)
	later := &jevtest.Provider{Results: []jevtest.Result{pick("defer", 0.99, "ratify")}}
	check(judge(&settings, later, "key").Decide(context.Background(), f.repo, decisionRequest(jev.KindRatification, key, "another state", "ratify")))
	if len(later.Requests()) != 0 || len(p.Requests()) != 1 {
		t.Fatalf("requests: %d later, %d first", len(later.Requests()), len(p.Requests()))
	}
}

// Only an accepted judgment of the workstream can be refused.
func TestOnlyAnAcceptedJudgmentIsRefused(t *testing.T) {
	f := newFixture(t)
	settings := testSettings()
	off := judge(&settings, &jevtest.Provider{}, "")
	fell := off.Decide(context.Background(), f.repo, decisionRequest(jev.KindRatification, jev.RatificationKey(1, 1, 1), "state", "ratify"))
	if fell.Status != jev.FallbackOnly || fell.Judgment == "" {
		t.Fatalf("fallback = %+v", fell)
	}
	for name, id := range map[string]string{"a fallback": fell.Judgment, "an unknown judgment": "judgment_unknown"} {
		if err := off.Refuse(context.Background(), f.repo, streamID, id, "no"); err == nil {
			t.Fatalf("%s was refused", name)
		}
	}
}
