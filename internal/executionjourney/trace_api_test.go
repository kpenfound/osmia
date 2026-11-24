package executionjourney

import (
	context "context"
	http "net/http"
	httptest "net/http/httptest"
	reflect "reflect"
	strings "strings"
	testing "testing"

	"github.com/kpenfound/osmia/internal/api"
	"github.com/kpenfound/osmia/internal/intake"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	"github.com/kpenfound/osmia/internal/servicetest/journey"

	"github.com/kpenfound/osmia/internal/projectstore"
	"github.com/kpenfound/osmia/internal/service"
)

func traceService(t *testing.T, f *walkFixture) *journey.Service {
	t.Helper()
	opts := journey.Fixture(t)
	cfg, err := projectstore.LoadConfig(opts.Config)
	fixturekit.Must(t, err)
	return &journey.Service{Cfg: cfg, Projects: []*journey.ActiveProject{journey.RuntimeFor(f.repo)}}
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
	c := &journey.Client{Client: service.ClientForTest(&http.Client{Transport: traceTransport{s}})}
	ctx := context.Background()

	summary, err := c.Trace(ctx, fixturekit.Stream)
	fixturekit.Must(t, err)
	if summary.Feature != intake.DeliveredState || !summary.Complete || len(summary.Criteria) != 3 || len(summary.Units) != 2 || summary.Spec.Revision != 1 || summary.Delivery.Publication.Revision != 1 {
		t.Fatalf("summary %+v", summary)
	}
	unit, err := c.TraceUnit(ctx, fixturekit.Stream, "a")
	fixturekit.Must(t, err)
	if unit.Definition.ID != "plan" || len(unit.Reviews) != 2 || unit.Reviews[1].Ref.Revision != 4 || unit.Landings[0].Commit != sha('a') {
		t.Fatalf("unit %+v", unit)
	}
	criterion, err := c.TraceCriterion(ctx, fixturekit.Stream, "spec#1")
	fixturekit.Must(t, err)
	if criterion.Spec.Revision != 1 || criterion.Units[0].Reports[1].Ref.Revision != 2 || criterion.Delivery.Publication == nil {
		t.Fatalf("criterion %+v", criterion)
	}
	commit, err := c.TraceCommit(ctx, fixturekit.Stream, sha('a'))
	fixturekit.Must(t, err)
	if len(commit.Landings) != 1 || commit.Landings[0].Review.Ref.Revision != 4 || commit.Landings[0].Criteria[0].Criterion != "spec#1" {
		t.Fatalf("commit %+v", commit)
	}
	if !reflect.DeepEqual(before, f.snapshot()) {
		t.Fatal("API trace walk changed repository")
	}
	for _, path := range []string{"bad", string(fixturekit.Stream) + "/unit/missing", string(fixturekit.Stream) + "/criterion/spec%239", string(fixturekit.Stream) + "/commit/" + sha('8'), string(fixturekit.Stream) + "/other/a", string(fixturekit.Stream) + "/unit"} {
		var response api.ErrorResponse
		err := c.Do(ctx, http.MethodGet, service.Prefix+"/trace/"+path, nil, &response)
		if err == nil {
			t.Fatalf("%s accepted", path)
		}
	}
}

type traceTransport struct{ service *journey.Service }

func (tr traceTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	w := httptest.NewRecorder()
	tr.service.Handle(w, r)
	return w.Result(), nil
}

func TestTraceAPIErrors(t *testing.T) {
	t.Parallel()
	f := newWalkFixture(t)
	f.sealed()
	s := traceService(t, f)
	for _, test := range []struct {
		kind, selector string
		code           api.Code
	}{{"criterion", "bad", api.Validation}, {"commit", "bad", api.Validation}, {"commit", strings.Repeat("a", 40) + ") | all()", api.Validation}, {"unit", "missing", api.NotFound}, {"other", "a", api.Validation}} {
		_, apiErr := journey.InspectForTest(s).TraceView(context.Background(), string(fixturekit.Stream), test.kind, test.selector)
		if apiErr == nil || apiErr.Code != test.code {
			t.Fatalf("%s %s: %+v", test.kind, test.selector, apiErr)
		}
	}
	_, apiErr := journey.InspectForTest(s).TraceView(context.Background(), "bad", "", "")
	if apiErr == nil || apiErr.Code != api.Validation {
		t.Fatalf("workstream error %+v", apiErr)
	}

	streamDir, err := f.root.WorkstreamTrace(fixturekit.Project, fixturekit.Stream)
	fixturekit.Must(t, err)
	fixturekit.TraceExec(t, streamDir, "INSERT INTO records (commit_seq, path, schema, id, revision, body) VALUES (1, ?, 'osmia.trace.document', 'damaged', 1, 'secret damaged content')", "workstreams/"+string(fixturekit.Stream)+"/documents.jsonl")
	_, apiErr = journey.InspectForTest(s).TraceView(context.Background(), string(fixturekit.Stream), "", "")
	if apiErr == nil || apiErr.Code != api.Internal || !strings.Contains(apiErr.Message, "trace repository") {
		t.Fatalf("unavailable trace %+v", apiErr)
	}
}
