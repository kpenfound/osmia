package service

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/kpenfound/osmia/internal/plan"
)

// TestM3TraceNavigationDemonstration follows one local workstream through
// unfinished work and delivery using both the API and the command line.
func TestM3TraceNavigationDemonstration(t *testing.T) {
	f := newWalkFixture(t)
	f.sealed()
	f.buildA()
	f.unit("b", UnitImplementing)

	s := traceService(t, f)
	socket := filepath.Join(t.TempDir(), "trace.sock")
	listener, err := net.Listen("unix", socket)
	must(t, err)
	server := &http.Server{Handler: http.HandlerFunc(s.handle)}
	go server.Serve(listener)
	t.Cleanup(func() { must(t, server.Close()) })
	client := NewClient(socket)
	t.Cleanup(func() { client.Close() })
	ctx := context.Background()

	bin := filepath.Join(t.TempDir(), "osmia")
	build := exec.Command("go", "build", "-o", bin, "./cmd/osmia")
	build.Dir = filepath.Join("..", "..")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build osmia: %v\n%s", err, out)
	}
	checkCLI := func(api any, selector ...string) string {
		t.Helper()
		args := append([]string{"--root", t.TempDir(), "--socket", socket, "trace", string(stream)}, selector...)
		cmd := exec.Command(bin, append(args, "--json")...)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("osmia trace %v: %v\n%s", selector, err, out)
		}
		encoded, err := json.Marshal(api)
		must(t, err)
		var fromAPI, fromCLI any
		must(t, json.Unmarshal(encoded, &fromAPI))
		must(t, json.Unmarshal(out, &fromCLI))
		if !reflect.DeepEqual(fromAPI, fromCLI) {
			t.Fatalf("CLI and API differ for %v\nAPI: %s\nCLI: %s", selector, encoded, out)
		}
		cmd = exec.Command(bin, args...)
		out, err = cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("osmia trace text %v: %v\n%s", selector, err, out)
		}
		return string(out)
	}

	active, err := client.TraceCriterion(ctx, stream, "spec#2")
	must(t, err)
	if active.Complete || len(active.Units) != 1 || gapStates(active.Units[0].Gaps)["units/b/report.json"] != LinkUnfinished {
		t.Fatalf("active criterion %+v", active)
	}
	if out := checkCLI(active, "criterion", "spec#2"); !strings.Contains(out, "Gap: units/b/report.json [unfinished]") {
		t.Fatal(out)
	}

	f.report("b", sha('3'), sha('a'), "spec#2", "spec#3")
	f.unit("b", UnitReviewing)
	f.review("b", 1, sha('3'), sha('a'), "satisfactory", 0, "spec#2", "spec#3")
	f.unit("b", UnitApproved)
	f.land("b", 2, sha('3'), sha('a'), sha('b'), "spec#2", "spec#3")
	f.unit("b", UnitMerged)
	f.deliver()
	// A later edit does not change the revisions governing delivered work.
	f.doc(plan.SpecDocument, plan.SpecPath, "", strings.Replace(walkSpec, "Specs parse.", "Specs parse strictly.", 1))

	criterion, err := client.TraceCriterion(ctx, stream, "spec#1")
	must(t, err)
	if !criterion.Complete || criterion.Text != "Specs parse." || criterion.Spec.Revision != 1 || criterion.Plan.Revision != 1 ||
		criterion.Units[0].Addresses[0].Proof.Name != "TestParse" || len(criterion.Units[0].Reports) != 2 ||
		criterion.Units[0].Reviews[1].Decision != "satisfactory" || len(criterion.Units[0].Reviews[1].Evidence) != 1 ||
		criterion.Units[0].Rulings[0].Owner != "The strict one." || criterion.Units[0].Landings[0].Commit != sha('a') ||
		criterion.Delivery.Published != sha('d') {
		t.Fatalf("delivered criterion %+v", criterion)
	}
	if out := checkCLI(criterion, "criterion", "spec#1"); !strings.Contains(out, "Specs parse.") || !strings.Contains(out, "published="+sha('d')) {
		t.Fatal(out)
	}

	commit, err := client.TraceCommit(ctx, stream, sha('a'))
	must(t, err)
	if !commit.Complete || len(commit.Landings) != 1 {
		t.Fatalf("landing commit %+v", commit)
	}
	landing := commit.Landings[0]
	if landing.Unit != "a" || landing.Landing.Candidate != sha('2') || landing.Landing.Base != sha('0') ||
		landing.Review.Ref.Revision != 4 || landing.Review.Candidate.Revision != sha('2') || landing.Review.Candidate.BaseRevision != sha('0') ||
		landing.Report.Ref.Revision != 2 || landing.Spec.Revision != 1 || landing.Plan.Revision != 1 ||
		landing.Criteria[0].Text != "Specs parse." || len(landing.Rulings) == 0 {
		t.Fatalf("commit provenance %+v", landing)
	}
	if out := checkCLI(commit, "commit", sha('a')); !strings.Contains(out, "Criterion spec#1: Specs parse.") {
		t.Fatal(out)
	}

	unit, err := client.TraceUnit(ctx, stream, "a")
	must(t, err)
	if !unit.Complete || unit.CostUSD != 0.25 || len(unit.Turns) != 1 || unit.Turns[0].Request.ID == "" || unit.Turns[0].Response == nil || len(unit.History) == 0 {
		t.Fatalf("unit history %+v", unit)
	}
	var request, response, cost bool
	for _, event := range unit.History {
		switch event.Ref.Kind {
		case "turn-request":
			request = true
		case "turn-response":
			response = true
		case "cost":
			cost = true
		}
	}
	if !request || !response || !cost {
		t.Fatalf("unit history lacks turn or cost: %+v", unit.History)
	}
	if out := checkCLI(unit, "unit", "a"); !strings.Contains(out, "Cost: $0.2500") {
		t.Fatal(out)
	}
	published, err := client.TraceCommit(ctx, stream, sha('d'))
	must(t, err)
	if !published.Complete || published.Delivery.Published != sha('d') || len(published.Landings) != 2 {
		t.Fatalf("published commit %+v", published)
	}
	checkCLI(published, "commit", sha('d'))
	summary, err := client.Trace(ctx, stream)
	must(t, err)
	if !summary.Complete || summary.Feature != DeliveredState || len(summary.Units) != 2 {
		t.Fatalf("summary %+v", summary)
	}
	checkCLI(summary)
}
