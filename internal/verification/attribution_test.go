package verification

import (
	"slices"
	"testing"
)

func claim(status Status, flow string) Claim {
	return Claim{Name: "c", Status: status, Flow: flow, Evidence: []string{}}
}

func flowStatuses(flows []FlowStatus) map[string]Status {
	out := map[string]Status{}
	for _, f := range flows {
		out[f.Flow] = f.Status
	}
	return out
}

func TestFlowStatusIsTheWorstOfItsTaggedClaims(t *testing.T) {
	for name, tc := range map[string]struct {
		claims []Claim
		want   Status
	}{
		"pass and fail":             {[]Claim{claim(Pass, "x"), claim(Fail, "x")}, Fail},
		"pass and inconclusive":     {[]Claim{claim(Pass, "x"), claim(Inconclusive, "x")}, Inconclusive},
		"fail and inconclusive":     {[]Claim{claim(Inconclusive, "x"), claim(Fail, "x")}, Fail},
		"all pass":                  {[]Claim{claim(Pass, "x"), claim(Pass, "x")}, Pass},
		"only inconclusive":         {[]Claim{claim(Inconclusive, "x")}, Inconclusive},
		"other flow cannot taint x": {[]Claim{claim(Pass, "x"), claim(Fail, "y")}, Pass},
	} {
		t.Run(name, func(t *testing.T) {
			got := flowStatuses(Attribute(Result{Claims: tc.claims}, nil).Flows)["x"]
			if got != tc.want {
				t.Fatalf("flow x is %q, want %q", got, tc.want)
			}
		})
	}
}

func TestUntaggedClaimsCountTowardTheRunButCoverNoFlow(t *testing.T) {
	r := Result{Claims: []Claim{claim(Pass, ""), claim(Pass, "")}}
	cov := Attribute(r, []string{"x"})
	if len(cov.Flows) != 0 || len(cov.Untagged) != 2 || cov.Status != Pass {
		t.Fatalf("coverage %+v", cov)
	}
	mixed := Attribute(Result{Claims: []Claim{claim(Pass, "x"), claim(Fail, "")}}, []string{"x"})
	if mixed.Status != Fail || flowStatuses(mixed.Flows)["x"] != Pass || len(mixed.Untagged) != 1 {
		t.Fatalf("coverage %+v", mixed)
	}
}

func TestTagsMissingFromTheListAreUnlistedAndAnUnknownListMarksNothing(t *testing.T) {
	r := Result{Claims: []Claim{claim(Pass, "x"), claim(Pass, "ghost")}}
	cov := Attribute(r, []string{"x"})
	if len(cov.Flows) != 2 || cov.Flows[0].Flow != "ghost" || !cov.Flows[0].Unlisted || cov.Flows[1].Flow != "x" || cov.Flows[1].Unlisted {
		t.Fatalf("flows %+v", cov.Flows)
	}
	for _, listed := range [][]string{nil} {
		for _, f := range Attribute(r, listed).Flows {
			if f.Unlisted {
				t.Fatalf("flow %+v marked unlisted without a list", f)
			}
		}
	}
	for _, f := range Attribute(r, []string{}).Flows {
		if !f.Unlisted {
			t.Fatalf("flow %+v listed by an empty list", f)
		}
	}
}

func TestRejectedReportCoversNoFlowAndIsInconclusive(t *testing.T) {
	cov := Attribute(Rejection("report.json is missing"), []string{"x"})
	if cov.Status != Inconclusive || cov.Rejected == "" || len(cov.Flows) != 0 || len(cov.Untagged) != 0 {
		t.Fatalf("coverage %+v", cov)
	}
}

func TestRunStatusIsPassOnlyForAtLeastOneClaimAndAllPassing(t *testing.T) {
	for name, tc := range map[string]struct {
		r    Result
		want Status
	}{
		"no claims":        {Result{}, Inconclusive},
		"rejected":         {Rejection("bad"), Inconclusive},
		"all pass":         {Result{Claims: []Claim{claim(Pass, "")}}, Pass},
		"one inconclusive": {Result{Claims: []Claim{claim(Pass, ""), claim(Inconclusive, "")}}, Inconclusive},
		"fail wins":        {Result{Claims: []Claim{claim(Inconclusive, ""), claim(Fail, "")}}, Fail},
	} {
		t.Run(name, func(t *testing.T) {
			if got := tc.r.Status(); got != tc.want {
				t.Fatalf("status %q, want %q", got, tc.want)
			}
		})
	}
}

func TestCombiningRunsGivesTheWorstStatusPerFlow(t *testing.T) {
	passing := Attribute(Result{Claims: []Claim{claim(Pass, "x"), claim(Pass, "y")}}, nil)
	inconclusive := Attribute(Result{Claims: []Claim{claim(Inconclusive, "x")}}, nil)
	failing := Attribute(Result{Claims: []Claim{claim(Fail, "y")}}, nil)
	rejected := Attribute(Rejection("bad"), nil)
	got := flowStatuses(Combine(passing, inconclusive, failing, rejected))
	if got["x"] != Inconclusive || got["y"] != Fail || len(got) != 2 {
		t.Fatalf("combined %v", got)
	}
	if got := Combine(rejected); len(got) != 0 {
		t.Fatalf("a rejected run covered %v", got)
	}
	if got := flowStatuses(Combine(passing)); got["x"] != Pass {
		t.Fatalf("combined %v", got)
	}
}

func TestInconclusiveIsNeverCountedAsAPass(t *testing.T) {
	runs := CountRuns(Rejection("bad"), Result{}, Result{Claims: []Claim{claim(Pass, "")}}, Result{Claims: []Claim{claim(Fail, "")}})
	if runs != (Counts{Pass: 1, Fail: 1, Inconclusive: 2}) || runs.Total() != 4 {
		t.Fatalf("runs %+v", runs)
	}
	claims := CountClaims(Result{Claims: []Claim{claim(Pass, ""), claim(Inconclusive, ""), {Name: "odd", Status: "unknown"}}})
	if claims != (Counts{Pass: 1, Inconclusive: 2}) {
		t.Fatalf("claims %+v", claims)
	}
	flows := CountFlows([]FlowStatus{{Flow: "a", Status: Inconclusive}, {Flow: "b", Status: Pass}})
	if flows != (Counts{Pass: 1, Inconclusive: 1}) {
		t.Fatalf("flows %+v", flows)
	}
	if Worse(Pass, "") != Inconclusive || Worse(Fail, Inconclusive) != Fail || !slices.Contains([]Status{Pass, Fail, Inconclusive}, Worse(Pass, Pass)) {
		t.Fatal("Worse")
	}
}
