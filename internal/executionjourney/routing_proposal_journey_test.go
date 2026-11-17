package executionjourney

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/kpenfound/osmia/internal/api"
	"github.com/kpenfound/osmia/internal/execution"
	"github.com/kpenfound/osmia/internal/jev/jevfixture"
	"github.com/kpenfound/osmia/internal/router"
	"github.com/kpenfound/osmia/internal/service"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	"github.com/kpenfound/osmia/internal/trace"
)

// The chief of staff proposes a move that its own policy would not make by
// default, and Jev departs from the policy. The proposal is the chief's tool
// call, the router asks Jev, and the departure is committed through the shared
// effects of the target. It is recorded with who decided it, the default and the
// judgment and the rule of the table; the route span says departed; the feed and
// the unit's trace call it Jev's decision, on this transition only; Hearsay is
// told once, as Jev's decision on the work source with no author, and not of the
// proposals that followed their defaults; and the chief's tool results say what
// became of each proposal without asking the owner to say a phrase.
func TestJourneyAChiefProposalDepartingFromASoftPolicyIsRoutedAndCommitted(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	m := newMoveFixture(t, "journey-proposal")
	p := &jevfixture.Transitions{}
	spans := routeThrough(m.f.s, p)
	fixturekit.RecordHanded(t, m.repo, m.stream, "stdin", demoStart)
	os.Setenv(service.RoutingPublishTokenEnv, "journey-publisher-token")

	// The chief holds the unit as contested itself, which is a proposal that
	// followed its default.
	if out := m.moveUnit(t, `{"unit":"resume","to":"contested","note":"The reviews keep finding the same gap."}`); !strings.Contains(out, `"recorded":true`) {
		t.Fatalf("hold: %s", out)
	}
	// Moving out of a contest it raised is, by policy, not the default: the unit
	// stays contested and the tool says so.
	out := m.moveUnit(t, `{"unit":"resume","to":"reviewing","note":"The engine is back; review it."}`)
	if !strings.Contains(out, `"recorded":false`) || !strings.Contains(out, "stays contested") {
		t.Fatalf("the proposal that followed the default: %s", out)
	}
	assertNoPhraseRequest(t, out)
	if m.state(t).Value != UnitContested {
		t.Fatalf("the unit is %s", m.state(t).Value)
	}

	// Jev departs from the policy.
	p.Script = departingTo(UnitReviewing)
	out = m.moveUnit(t, `{"unit":"resume","to":"reviewing","note":"The engine is back and the gap was fixed upstream; review it."}`)
	if !strings.Contains(out, `"recorded":true`) || !strings.Contains(out, `"by":"chief of staff"`) {
		t.Fatalf("the departure: %s", out)
	}
	assertNoPhraseRequest(t, out)
	if m.state(t).Value != UnitReviewing {
		t.Fatalf("the unit is %s", m.state(t).Value)
	}
	moved := latestOf(t, m.repo, m.stream, trace.UnitSubject("resume"))
	if moved.From != UnitContested || moved.To != UnitReviewing || moved.DecidedBy != trace.DecidedByJev || moved.DefaultTo != execution.TargetWait || !moved.Departed() || moved.Judgment == "" || !strings.Contains(moved.Reason, "rule 6:departure") {
		t.Fatalf("the departure's transition %+v", moved)
	}
	recs := proposalRecords(t, m)
	if len(recs) != 3 || recs[2].State != router.StateCommitted || recs[2].Outcome != router.OutcomeDeparted || recs[2].Judgment != moved.Judgment || recs[2].Rule == "" || recs[2].Table != router.TableVersion {
		t.Fatalf("route %+v", recs)
	}
	var span *router.SpanRoute
	for _, s := range spans.all() {
		if s.Trigger == execution.TriggerMoveProposal && s.Outcome == router.OutcomeDeparted {
			s := s
			span = &s
		}
	}
	if span == nil || span.Default != execution.TargetWait || span.Chosen != UnitReviewing || span.Judgment != moved.Judgment {
		t.Fatalf("route span %+v", span)
	}

	// The feed and the unit's trace call the decision Jev's, on this
	// transition alone.
	feed, feedErr := m.f.s.feedList(string(m.stream))
	if feedErr != nil {
		t.Fatal(feedErr.Message)
	}
	departures := 0
	for _, e := range feed.Entries {
		if e.Kind == "transition" && e.Transition != nil && e.Transition.Departed {
			departures++
			if e.Transition.To != UnitReviewing || e.Transition.DecidedBy != trace.DecidedByJev || e.Transition.DefaultTo != execution.TargetWait || e.Transition.Judgment != moved.Judgment {
				t.Fatalf("the feed's departure %+v", e.Transition)
			}
		}
	}
	if departures != 1 {
		t.Fatalf("%d departures in the feed, want 1", departures)
	}
	view, apiErr := m.f.s.traceView(ctx, string(m.stream), "unit", "resume")
	if apiErr != nil {
		t.Fatal(apiErr.Message)
	}
	data, err := json.Marshal(view)
	must(t, err)
	var unit unitTraceView
	must(t, json.Unmarshal(data, &unit))
	traced := 0
	for _, e := range unit.History {
		if e.Departed {
			traced++
		}
	}
	if traced != 1 {
		t.Fatalf("%d departures in the unit's trace, want 1", traced)
	}

	// Hearsay is told of the departure and of nothing the router followed.
	hooks := fixturekit.NewHooks(t)
	res, err := service.InspectForTest(m.f.s.core()).PublishToHearsay(ctx, m.repo, hooks.URL)
	must(t, err)
	if res.Unreachable || len(res.Failures) != 0 {
		t.Fatalf("the publish pass: %+v", res)
	}
	var departed int
	for _, d := range fixturekit.Decisions(hooks.Take()) {
		if !strings.Contains(d.Str("decision"), "-jev-route-") {
			continue
		}
		departed++
		if d.Source != "osmia-work" || d.Str("author") != "" || d.Str("decider") == "owner" || !strings.Contains(d.Str("context")+d.Str("rationale"), moved.Judgment) {
			t.Fatalf("the departure's decision: %s on %s", d.Raw, d.Source)
		}
	}
	if departed != 1 {
		t.Fatalf("%d departures posted to Hearsay, want 1", departed)
	}
	if all, narrow := len(p.Requests()), len(p.TransitionRequests()); all != narrow {
		t.Fatalf("Jev got %d requests, %d of them transition requests", all, narrow)
	}
}

// unitTraceView is the unit trace the API sends, read back from its JSON.
type unitTraceView = api.UnitTrace
