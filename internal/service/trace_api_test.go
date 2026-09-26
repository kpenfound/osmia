package service

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/trace"
)

func traceService(t *testing.T, f *walkFixture) *Service {
	t.Helper()
	opts := fixture(t)
	cfg, err := config.Load(opts.Config)
	must(t, err)
	return &Service{cfg: cfg, active: &activeProject{repository: f.repo}}
}

func TestTraceAPIAndClient(t *testing.T) {
	t.Parallel()
	f := newWalkFixture(t)
	f.sealed()
	f.buildA()
	f.buildB()
	f.deliver()
	s := traceService(t, f)
	before := f.snapshot()
	c := &Client{http: &http.Client{Transport: traceTransport{s}}, defaultTimeout: defaultClientTimeout}
	ctx := context.Background()

	summary, err := c.Trace(ctx, stream)
	must(t, err)
	if summary.Feature != DeliveredState || !summary.Complete || len(summary.Criteria) != 3 || len(summary.Units) != 2 || summary.Spec.Revision != 1 || summary.Delivery.Publication.Revision != 1 {
		t.Fatalf("summary %+v", summary)
	}
	unit, err := c.TraceUnit(ctx, stream, "a")
	must(t, err)
	if unit.Definition.ID != "plan" || len(unit.Reviews) != 2 || unit.Reviews[1].Ref.Revision != 4 || unit.Landings[0].Commit != sha('a') {
		t.Fatalf("unit %+v", unit)
	}
	criterion, err := c.TraceCriterion(ctx, stream, "spec#1")
	must(t, err)
	if criterion.Spec.Revision != 1 || criterion.Units[0].Reports[1].Ref.Revision != 2 || criterion.Delivery.Publication == nil {
		t.Fatalf("criterion %+v", criterion)
	}
	commit, err := c.TraceCommit(ctx, stream, sha('a'))
	must(t, err)
	if len(commit.Landings) != 1 || commit.Landings[0].Review.Ref.Revision != 4 || commit.Landings[0].Criteria[0].Criterion != "spec#1" {
		t.Fatalf("commit %+v", commit)
	}
	if !reflect.DeepEqual(before, f.snapshot()) {
		t.Fatal("API trace walk changed repository")
	}
	for _, path := range []string{"bad", string(stream) + "/unit/missing", string(stream) + "/criterion/spec%239", string(stream) + "/commit/" + sha('8'), string(stream) + "/other/a", string(stream) + "/unit"} {
		var response ErrorResponse
		err := c.Do(ctx, http.MethodGet, Prefix+"/trace/"+path, nil, &response)
		if err == nil {
			t.Fatalf("%s accepted", path)
		}
	}
}

type traceTransport struct{ service *Service }

func (tr traceTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	w := httptest.NewRecorder()
	tr.service.handle(w, r)
	return w.Result(), nil
}

func TestTraceAPIGapsAndTerminal(t *testing.T) {
	t.Parallel()
	f := newWalkFixture(t)
	f.sealed()
	f.buildA()
	f.unit("b", UnitImplementing)
	s := traceService(t, f)
	result, api := s.traceView(context.Background(), string(stream), "criterion", "spec#2")
	if api != nil {
		t.Fatal(api)
	}
	if got := gapStates(result.(CriterionTrace).Units[0].Gaps)["units/b/report.json"]; got != LinkUnfinished {
		t.Fatalf("active gap %s", got)
	}
	f.move(trace.FeatureSubject, "", AbandonedState)
	result, api = s.traceView(context.Background(), string(stream), "criterion", "spec#2")
	if api != nil {
		t.Fatal(api)
	}
	if got := gapStates(result.(CriterionTrace).Units[0].Gaps)["units/b/report.json"]; got != LinkNotCreated {
		t.Fatalf("terminal gap %s", got)
	}
	for _, test := range []struct {
		kind, selector string
		code           Code
	}{{"criterion", "bad", Validation}, {"commit", "bad", Validation}, {"commit", strings.Repeat("a", 40) + ") | all()", Validation}, {"unit", "missing", NotFound}, {"other", "a", Validation}} {
		_, api := s.traceView(context.Background(), string(stream), test.kind, test.selector)
		if api == nil || api.Code != test.code {
			t.Fatalf("%s %s: %+v", test.kind, test.selector, api)
		}
	}
	_, api = s.traceView(context.Background(), "bad", "", "")
	if api == nil || api.Code != Validation {
		t.Fatalf("workstream error %+v", api)
	}
	// A repository read failure is returned without record content.
	must(t, os.RemoveAll(f.dir))
	_, api = s.traceView(context.Background(), string(stream), "", "")
	if api == nil || api.Code != Internal || !strings.Contains(api.Message, "trace repository") {
		t.Fatalf("unavailable trace %+v", api)
	}
}
