package journey

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/jev/jevfixture"
	"github.com/kpenfound/osmia/internal/router"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	"github.com/kpenfound/osmia/internal/trace"
)

// RouteThrough makes the service's router decide through p's script and
// returns the log of the spans it routes.
func RouteThrough(s *Service, p *jevfixture.Transitions) *SpanLog {
	InspectForTest(s).SetJudge(jevfixture.TransitionJudge(s.Now, p))
	log := &SpanLog{}
	s.Core().Router().Spans = log.Add
	return log
}

// RouteRecords returns the stream's route records, each at its latest
// revision, in the order the routes were first recorded.
func RouteRecords(t *testing.T, repo *trace.Repository, stream config.WorkstreamID) []router.Record {
	t.Helper()
	docs, err := trace.Read[trace.Document](repo, stream)
	fixturekit.Must(t, err)
	latest := map[string]trace.Document{}
	var order []string
	for _, d := range docs {
		if !strings.HasPrefix(d.Path, "routes/") {
			continue
		}
		if _, ok := latest[d.ID]; !ok {
			order = append(order, d.ID)
		}
		if d.Revision >= latest[d.ID].Revision {
			latest[d.ID] = d
		}
	}
	var out []router.Record
	for _, id := range order {
		var rec router.Record
		fixturekit.Must(t, json.Unmarshal([]byte(latest[id].Content), &rec))
		out = append(out, rec)
	}
	return out
}

// LookupRoute returns the first route of trigger and whether there is one.
func LookupRoute(t *testing.T, repo *trace.Repository, stream config.WorkstreamID, trigger string) (router.Record, bool) {
	t.Helper()
	for _, rec := range RouteRecords(t, repo, stream) {
		if rec.Trigger == trigger {
			return rec, true
		}
	}
	return router.Record{}, false
}

// RouteOf returns the first route of trigger and fails the test when there
// is none.
func RouteOf(t *testing.T, repo *trace.Repository, stream config.WorkstreamID, trigger string) router.Record {
	t.Helper()
	rec, ok := LookupRoute(t, repo, stream, trigger)
	if !ok {
		t.Fatalf("no exit of trigger %s was routed: %+v", trigger, RouteRecords(t, repo, stream))
	}
	return rec
}

// DeparturesInFeed returns the stream's feed transitions that departed from
// their default.
func DeparturesInFeed(t *testing.T, s *Service, repo *trace.Repository, stream config.WorkstreamID) []trace.Transition {
	t.Helper()
	s.SetSole(RuntimeFor(repo))
	feed, apiErr := s.FeedList(string(stream))
	if apiErr != nil {
		t.Fatal(apiErr.Message)
	}
	var out []trace.Transition
	for _, e := range feed.Entries {
		if e.Transition != nil && e.Transition.Departed {
			out = append(out, trace.Transition{Subject: e.Transition.Subject, To: e.Transition.To, DefaultTo: e.Transition.DefaultTo, DecidedBy: e.Transition.DecidedBy})
		}
	}
	return out
}
