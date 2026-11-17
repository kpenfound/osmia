package executionjourney

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kpenfound/osmia/internal/api"
	"github.com/kpenfound/osmia/internal/cli"
	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/execution"
	"github.com/kpenfound/osmia/internal/jev"
	"github.com/kpenfound/osmia/internal/jev/jevfixture"
	"github.com/kpenfound/osmia/internal/router"
	"github.com/kpenfound/osmia/internal/service"
	"github.com/kpenfound/osmia/internal/servicetest"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	"github.com/kpenfound/osmia/internal/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

const (
	// routingUnit is the unit of the routing journeys: checking, with a
	// recorded candidate, in a building workstream.
	routingUnit      = "u1"
	routingCandidate = "candidate-0001"
)

// askedLog collects the triggers Jev was asked about.
type askedLog struct {
	mu       sync.Mutex
	triggers []string
}

func (l *askedLog) add(trigger string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.triggers = append(l.triggers, trigger)
}

func (l *askedLog) all() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return slices.Clone(l.triggers)
}

// routingJourney is a running service over a building workstream with one unit
// in checking and a recorded candidate, Jev answering as a fixture does, a
// clock the test moves, a span exporter and a fake Hearsay listener. The
// scheduler, the router's worker, the trace, the API, the feed and the
// command line are the product's own.
type routingJourney struct {
	t     *testing.T
	opts  Options
	s     *Service
	c     *Client
	repo  *trace.Repository
	clock *fixedClock
	jev   *jevfixture.Transitions
	asked *askedLog
	hooks *fixturekit.Hooks
	spans *tracetest.InMemoryExporter
}

// newRoutingJourney starts the service. The fixture Jev p answers with its
// script, or finds every default fits when it has none.
func newRoutingJourney(t *testing.T, p *jevfixture.Transitions) *routingJourney {
	t.Helper()
	ctx := context.Background()
	opts, cfg := conversationFixture(t, "routing-")
	repo, err := trace.Open(cfg.Root, cfg.Project)
	must(t, err)
	fixturekit.RecordHanded(t, repo, stream, "stdin", demoStart)
	seeder := trace.Actor{Kind: "service", ID: "journey-seeder"}
	seed := func(id, subject, unit, to string) {
		t.Helper()
		h := trace.Header{Schema: "osmia.trace.transition", Version: trace.Version, ID: id, Revision: 1, Project: repo.Project(), Workstream: stream, Unit: unit, At: demoStart, Actor: seeder, Cause: "seed"}
		_, err := repo.Transact(ctx, trace.Transaction{Transition: trace.Transition{Header: h, Subject: subject, To: to, Reason: "seeded"}})
		must(t, err)
	}
	seed("journey-feature", trace.FeatureSubject, "", BuildingState)
	seed("journey-unit", trace.UnitSubject(routingUnit), routingUnit, UnitChecking)
	report, err := json.Marshal(execution.UnitReport{Unit: routingUnit, Turn: "turn-1", Seal: 1, Outcome: "done", Branch: "unit-branch", Base: "base-commit", Candidate: routingCandidate})
	must(t, err)
	must(t, repo.RecordDocuments(ctx, []trace.Document{{Header: trace.Header{Schema: "osmia.trace.document", Version: trace.Version, ID: trace.UnitSubject(routingUnit) + "-report", Revision: 1, Project: repo.Project(), Workstream: stream, Unit: routingUnit, At: demoStart, Actor: seeder, Cause: "seed"},
		Path: "units/" + routingUnit + "/report.json", Content: string(report) + "\n"}}))
	must(t, repo.Close())

	j := &routingJourney{t: t, opts: opts, clock: &fixedClock{At: demoStart}, jev: p, asked: &askedLog{}, hooks: fixturekit.NewHooks(t), spans: tracetest.NewInMemoryExporter()}
	script := p.Script
	p.Script = func(a jevfixture.Ask) jevfixture.Reply {
		j.asked.add(a.Trigger)
		if script == nil {
			return jevfixture.DefaultFits(a)
		}
		return script(a)
	}
	j.start(&opts)
	return j
}

// start gives opts Jev, the clock and the exporter and starts the service.
func (j *routingJourney) start(opts *Options) {
	j.t.Helper()
	opts.Reconciliation.Now = j.clock.Now
	appendConfig(j.t, opts, jevfixture.Config(jevfixture.Key(j.t, true)))
	opts.JevProvider = j.jev.Factory()
	opts.telemetryExporter = j.spans
	// The publisher's credential is read from the environment; its value is
	// the same for every journey.
	os.Setenv(service.RoutingPublishTokenEnv, "journey-publisher-token")
	j.opts = *opts
	j.s, j.c = start(j.t, *opts)
	j.repo = j.s.sole().repository
}

// access returns the service's test hooks.
func (j *routingJourney) access() service.TestAccess { return service.InspectForTest(j.s.core()) }

// stall moves the clock to d after the start and lets the next pass of the
// scheduler read the records for stalls.
func (j *routingJourney) stall(d time.Duration) {
	j.t.Helper()
	j.clock.Mu.Lock()
	j.clock.At = demoStart.Add(d)
	j.clock.Mu.Unlock()
	j.access().ArmStallWatchdog(j.repo.Project())
}

// awaitJourney polls until ready, failing the test after the demo timeout.
func awaitJourney(t *testing.T, what string, ready func() bool) {
	t.Helper()
	deadline := time.Now().Add(demoTimeout)
	for !ready() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// awaitRoutes waits until at least n exits of the trigger on the subject are
// decided and returns them in the order they were raised.
func (j *routingJourney) awaitRoutes(what, trigger, subject string, n int) []router.Record {
	j.t.Helper()
	var found []router.Record
	awaitJourney(j.t, what, func() bool {
		found = found[:0]
		for _, rec := range routeRecords(j.t, j.repo, stream) {
			if rec.Trigger == trigger && rec.Subject == subject && rec.Finished() {
				found = append(found, rec)
			}
		}
		return len(found) >= n
	})
	return found
}

// routeSpans returns the route spans of the trigger, waiting for n of them.
func (j *routingJourney) routeSpans(trigger string, n int) servicetest.Spans {
	j.t.Helper()
	var spans servicetest.Spans
	awaitJourney(j.t, "the route span of "+trigger, func() bool {
		spans = servicetest.SpansOf(j.spans).Named("osmia.transition.route").With("osmia.trigger", trigger)
		return len(spans) >= n
	})
	return spans
}

// publish runs one Hearsay publish pass and returns what the fake listener
// received.
func (j *routingJourney) publish() []fixturekit.HooksPost {
	j.t.Helper()
	res, err := j.access().PublishToHearsay(context.Background(), j.repo, j.hooks.URL)
	must(j.t, err)
	if res.Unreachable || len(res.Failures) != 0 {
		j.t.Fatalf("the publish pass: %+v", res)
	}
	return j.hooks.Take()
}

// feedTransitions returns the transitions the workstream's feed lists.
func (j *routingJourney) feedTransitions() []api.FeedTransition {
	j.t.Helper()
	var feed api.FeedResponse
	must(j.t, j.c.Do(context.Background(), "GET", api.Prefix+"/feed/"+string(stream), nil, &feed))
	var out []api.FeedTransition
	for _, e := range feed.Entries {
		if e.Kind == "transition" && e.Transition != nil {
			out = append(out, *e.Transition)
		}
	}
	return out
}

// cli runs `osmia` against the service's root and returns its output.
func (j *routingJourney) cli(args ...string) string {
	j.t.Helper()
	root, err := config.ResolveRoot(j.opts.Config.Root, "")
	must(j.t, err)
	var out, diag bytes.Buffer
	code := cli.Run(context.Background(), append([]string{"--root", root.String()}, args...), strings.NewReader(""), &out, &diag)
	if code != 0 || diag.Len() != 0 {
		j.t.Fatalf("osmia %s: code=%d stderr=%s\n%s", strings.Join(args, " "), code, diag.String(), out.String())
	}
	return out.String()
}

// recoveryCards returns the open recovery cards of the inbox.
func (j *routingJourney) recoveryCards() []api.InboxEntry {
	j.t.Helper()
	inbox, err := j.c.Inbox(context.Background())
	must(j.t, err)
	var out []api.InboxEntry
	for _, e := range inbox.Entries {
		if e.Kind == api.InboxRecovery {
			out = append(out, e)
		}
	}
	return out
}

// jevOnlyAskedAboutExits checks the Jev requests the journey made were the
// narrow questions of routed exits and nothing else: every request a step
// journal, the outbox or turn admission would make carries no trigger the
// router knows, and the fixture counts the others.
func (j *routingJourney) jevOnlyAskedAboutExits() {
	j.t.Helper()
	for _, trigger := range j.asked.all() {
		if !router.ValidTrigger(trigger) {
			j.t.Fatalf("Jev was asked about %q, which is not the trigger of a routed exit", trigger)
		}
	}
	if all, narrow := len(j.jev.Requests()), len(j.jev.TransitionRequests()); all != narrow {
		j.t.Fatalf("Jev got %d requests and %d of them were transition requests", all, narrow)
	}
}

// noticesContaining counts the notices the chief of staff got that carry text.
func (j *routingJourney) noticesContaining(text string) int {
	j.t.Helper()
	outbox, err := j.repo.Outbox(stream)
	must(j.t, err)
	n := 0
	for _, e := range outbox {
		if strings.Contains(e.Event.Body, text) {
			n++
		}
	}
	return n
}

// stalledNotices counts the notices that the unit made no progress, which the
// router sends when it commits a stalled exit; holdNotices counts the notices
// that Jev held the unit's stalled exit, which carry Jev's view.
func (j *routingJourney) stalledNotices() int {
	j.t.Helper()
	return j.noticesContaining("Unit " + routingUnit + " has made no progress")
}

func (j *routingJourney) holdNotices() int {
	j.t.Helper()
	return j.noticesContaining("Jev held " + trace.UnitSubject(routingUnit) + " at " + UnitChecking + " without moving it (trigger " + execution.TriggerStalled + ")")
}

// A unit goes quiet and the watchdog raises `stalled` to the router. Jev
// departs from the default of waiting and has the unit's candidate reviewed
// again. The departure is committed through the running service's worker and
// recorded with who decided it, the default and the judgment; the route span
// says departed; the feed, the trace API and `osmia trace` call it Jev's
// decision, and nothing else; Hearsay gets exactly one decision from Jev with
// no author; and no step journal asked Jev anything.
func TestJourneyAStalledUnitIsDepartedFromByJev(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	p := &jevfixture.Transitions{Script: func(a jevfixture.Ask) jevfixture.Reply {
		if a.Trigger == execution.TriggerStalled {
			return jevfixture.Departs(a, UnitReviewing, 0.93, 0.93)
		}
		return jevfixture.DefaultFits(a)
	}}
	j := newRoutingJourney(t, p)

	// Nothing is raised while the unit is within its window.
	j.stall(40 * time.Minute)
	time.Sleep(2 * time.Second)
	if recs := routeRecords(t, j.repo, stream); len(recs) != 0 {
		t.Fatalf("a unit quiet for 40 minutes raised %+v", recs)
	}

	j.stall(46 * time.Minute)
	rec := j.awaitRoutes("the stalled exit of the unit", execution.TriggerStalled, trace.UnitSubject(routingUnit), 1)[0]
	if rec.State != router.StateCommitted || rec.Outcome != router.OutcomeDeparted || rec.Default != execution.TargetWait || rec.Target != UnitReviewing || rec.DecidedBy != trace.DecidedByJev || rec.Judgment == "" || !rec.JevRequested {
		t.Fatalf("route %+v", rec)
	}
	if state := workflowOf(t, j.repo, stream, routingUnit); state.Value != UnitReviewing {
		t.Fatalf("the unit is %s", state.Value)
	}
	moved := latestOf(t, j.repo, stream, trace.UnitSubject(routingUnit))
	if moved.From != UnitChecking || moved.To != UnitReviewing || moved.DecidedBy != trace.DecidedByJev || moved.DefaultTo != execution.TargetWait || moved.Judgment != rec.Judgment || !moved.Departed() {
		t.Fatalf("the departure's transition %+v", moved)
	}

	// The route span says departed, with the trigger, the default and the
	// state chosen, and nothing but fixed words and IDs.
	spans := j.routeSpans(execution.TriggerStalled, 1)
	if len(spans) != 1 {
		t.Fatalf("%d route spans for the stalled unit", len(spans))
	}
	for key, want := range map[string]string{"osmia.outcome": router.OutcomeDeparted, "osmia.default_state": execution.TargetWait, "osmia.next_state": UnitReviewing, "osmia.judgment": rec.Judgment, "osmia.unit.id": routingUnit} {
		if got, _ := servicetest.Attr(spans[0], key); got != want {
			t.Fatalf("route span %s = %q, want %q: %v", key, got, want, servicetest.Strings(spans[0]))
		}
	}

	// The feed, the trace API and `osmia trace` show the decision as Jev's, on
	// this transition only.
	departures := 0
	for _, tr := range j.feedTransitions() {
		if tr.Departed {
			departures++
			if tr.Subject != trace.UnitSubject(routingUnit) || tr.To != UnitReviewing || tr.DecidedBy != trace.DecidedByJev || tr.DefaultTo != execution.TargetWait || tr.Judgment != rec.Judgment {
				t.Fatalf("the feed's departure %+v", tr)
			}
		}
	}
	if departures != 1 {
		t.Fatalf("%d departures in the feed, want 1", departures)
	}
	unit, err := j.c.TraceUnit(ctx, stream, routingUnit)
	must(t, err)
	traced := 0
	for _, e := range unit.History {
		if e.Departed {
			traced++
			if e.DecidedBy != trace.DecidedByJev || e.Judgment != rec.Judgment || e.DefaultTo != execution.TargetWait {
				t.Fatalf("the trace's departure %+v", e)
			}
		}
	}
	if traced != 1 {
		t.Fatalf("%d departures in the unit's trace, want 1: %+v", traced, unit.History)
	}
	out := j.cli("trace", string(stream), "unit", routingUnit)
	if strings.Count(out, "decided by Jev") != 1 || !strings.Contains(out, "reviewing") {
		t.Fatalf("osmia trace:\n%s", out)
	}
	feed := j.cli("feed", string(stream))
	if strings.Count(feed, "decided by Jev") != 1 {
		t.Fatalf("osmia feed:\n%s", feed)
	}

	// Hearsay is told once, as Jev's decision on the work source, with no author.
	posts := j.publish()
	var departed []fixturekit.HooksPost
	for _, d := range fixturekit.Decisions(posts) {
		if strings.Contains(d.Str("decision"), "-jev-route-") {
			departed = append(departed, d)
		}
	}
	if len(departed) != 1 {
		t.Fatalf("%d departures posted to Hearsay, want 1: %d decisions", len(departed), len(fixturekit.Decisions(posts)))
	}
	d := departed[0]
	if d.Source != "osmia-work" || d.Str("author") != "" || d.Str("decider") == "owner" || !strings.Contains(d.Str("context")+d.Str("rationale"), rec.Judgment) {
		t.Fatalf("the departure's decision: %s on %s", d.Raw, d.Source)
	}
	for _, d := range fixturekit.Decisions(j.publish()) {
		if strings.Contains(d.Str("decision"), "-jev-route-") {
			t.Fatalf("the departure was posted again: %s", d.Raw)
		}
	}

	// The status shows Jev ready, and Jev was asked about this one exit.
	if runtime, err := j.c.Runtime(ctx); err != nil || runtime.Jev.Mode != jev.ModeReady {
		t.Fatalf("Jev's status: %+v %v", runtime.Jev, err)
	}
	if n := len(j.jev.TransitionRequests()); n != 1 {
		t.Fatalf("Jev got %d transition requests, want 1", n)
	}
	j.jevOnlyAskedAboutExits()
}

// A hold that nobody resolves becomes a stall, and a stall that nobody clears
// becomes a recovery card the owner approves in one click. Jev holds the first
// stall of the unit (its two orderings disagree), which moves nothing and tells
// the chief of staff once. When the window passes the unit stalls again and
// Jev, asked again, finds waiting fits, which tells the chief once more. A
// further window later the owner has one recovery card pinning the exact move,
// in the inbox and the feed. Approving it with the answer the card carries
// applies the move through the router as a pinned owner decision: Jev is not
// asked, the owner is the decider, and the card is closed.
func TestJourneyAnUnresolvedHoldBecomesAStallAndARecoveryCardTheOwnerApproves(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	var stalls int
	var mu sync.Mutex
	p := &jevfixture.Transitions{Script: func(a jevfixture.Ask) jevfixture.Reply {
		r := jevfixture.DefaultFits(a)
		if a.Trigger != execution.TriggerStalled {
			return r
		}
		mu.Lock()
		defer mu.Unlock()
		stalls++
		if stalls == 1 {
			r.PickA, r.PickB = UnitReviewing, execution.TargetWait
		}
		return r
	}}
	j := newRoutingJourney(t, p)
	subject := trace.UnitSubject(routingUnit)

	// The first stall is held.
	j.stall(46 * time.Minute)
	first := j.awaitRoutes("the first stall", execution.TriggerStalled, subject, 1)[0]
	if first.State != router.StateHeld || first.Outcome != router.OutcomeHeld || first.Hold != router.HoldOrdersDisagree {
		t.Fatalf("the first stall: %+v", first)
	}
	if state := workflowOf(t, j.repo, stream, routingUnit); state.Value != UnitChecking {
		t.Fatalf("a held stall moved the unit to %s", state.Value)
	}
	if hold, stalled := j.holdNotices(), j.stalledNotices(); hold != 1 || stalled != 0 {
		t.Fatalf("the chief of staff got %d notices that Jev held the first stall and %d that the unit stalled, want 1 and 0", hold, stalled)
	}
	if cards := j.recoveryCards(); len(cards) != 0 {
		t.Fatalf("a hold just made has %d recovery cards", len(cards))
	}
	if spans := j.routeSpans(execution.TriggerStalled, 1); spans.With("osmia.outcome", router.OutcomeHeld) == nil {
		t.Fatalf("the hold has no held route span")
	}

	// Once the window passes the hold counts as a stall of its own: another
	// occurrence, which Jev answers as waiting.
	j.stall(96 * time.Minute)
	second := j.awaitRoutes("the stall after the hold", execution.TriggerStalled, subject, 2)[1]
	if second.Occurrence == first.Occurrence || second.State != router.StateCommitted || second.Outcome != router.OutcomeFollowed || second.Target != execution.TargetWait {
		t.Fatalf("the stall after the hold: %+v", second)
	}
	if hold, stalled := j.holdNotices(), j.stalledNotices(); hold != 1 || stalled != 1 {
		t.Fatalf("the chief of staff got %d notices of the hold and %d of the second stall, want one each", hold, stalled)
	}
	if cards := j.recoveryCards(); len(cards) != 0 {
		t.Fatalf("a stall inside its further window has %d recovery cards", len(cards))
	}

	// A further window later nobody has cleared it: the recovery card.
	j.stall(146 * time.Minute)
	var card api.InboxEntry
	awaitJourney(t, "the recovery card", func() bool {
		cards := j.recoveryCards()
		if len(cards) == 1 {
			card = cards[0]
		}
		return len(cards) == 1
	})
	m := card.Recovery
	if m == nil || m.Origin != api.RecoveryStall || m.Subject != subject || m.From != UnitChecking || m.Target != UnitChecking || m.Machine != execution.UnitMachine || m.Occurrence != second.Occurrence || card.Unit != routingUnit {
		t.Fatalf("the card %+v pins %+v", card, m)
	}
	if card.Answer.Method != "POST" || card.Answer.Path != api.Prefix+"/recovery/"+string(stream) {
		t.Fatalf("the card's answer %+v", card.Answer)
	}
	var open []api.FeedEntry
	var feed api.FeedResponse
	must(t, j.c.Do(ctx, "GET", api.Prefix+"/feed/"+string(stream), nil, &feed))
	for _, e := range feed.Entries {
		if e.Kind == "recovery" {
			open = append(open, e)
		}
	}
	if len(open) != 1 || open[0].Recovery == nil || open[0].Recovery.State != "open" || open[0].Recovery.Occurrence != m.Occurrence {
		t.Fatalf("the feed's recovery entries %+v", open)
	}
	// However many passes find the stall, there is one card.
	j.stall(200 * time.Minute)
	time.Sleep(2 * time.Second)
	if cards := j.recoveryCards(); len(cards) != 1 {
		t.Fatalf("later passes made %d cards", len(cards))
	}

	j.approveRecoveryCard(card, subject)
	j.jevOnlyAskedAboutExits()
}

// approveRecoveryCard is the owner's one click: the answer the card carries,
// sent as it is. The move is applied through the router as a pinned owner
// decision (Jev is not asked, the owner is the decider and the authority),
// the route span says followed, the card is closed, and a second click is
// refused.
func (j *routingJourney) approveRecoveryCard(card api.InboxEntry, subject string) {
	t, ctx := j.t, context.Background()
	t.Helper()
	// One click: the answer the card carries, sent as it is.
	body, err := json.Marshal(card.Answer.Body)
	must(t, err)
	var req api.RecoveryRequest
	must(t, json.Unmarshal(body, &req))
	asked := len(j.jev.Requests())
	res, err := j.c.Recovery(ctx, stream, req)
	must(t, err)
	if res.Subject != subject || res.Target != UnitChecking || res.From != UnitChecking || res.Detail == "" {
		t.Fatalf("the approval's response %+v", res)
	}
	if len(j.jev.Requests()) != asked {
		t.Fatal("approving the card asked Jev")
	}
	moved := latestOf(t, j.repo, stream, subject)
	if moved.To != UnitChecking || moved.DecidedBy != trace.DecidedByOwner || moved.Actor.Kind != "owner" {
		t.Fatalf("the approved move %+v", moved)
	}
	var pinned bool
	for _, rec := range routeRecords(t, j.repo, stream) {
		if rec.Trigger == execution.TriggerMoveOwner && rec.Subject == subject {
			pinned = rec.State == router.StateCommitted && rec.Authority == router.AuthorityOwner && rec.DecidedBy == trace.DecidedByOwner && rec.Outcome == router.OutcomeFollowed && !rec.JevRequested
		}
	}
	if !pinned {
		t.Fatalf("no pinned owner move was routed: %+v", routeRecords(t, j.repo, stream))
	}
	if spans := j.routeSpans(execution.TriggerMoveOwner, 1); spans.With("osmia.outcome", router.OutcomeFollowed) == nil {
		t.Fatal("the pinned move has no followed route span")
	}
	if cards := j.recoveryCards(); len(cards) != 0 {
		t.Fatalf("an approved card is still open: %+v", cards)
	}
	if _, err := j.c.Recovery(ctx, stream, req); err == nil {
		t.Fatal("the card was approved twice")
	}
}

// A Jev that holds every stall of the unit does not leave the hold silent: the
// first hold tells the chief of staff, the window passes, the unit stalls again
// and is held again with a second notice, and a further window later the owner
// has the recovery card instead of a third request to Jev. The owner approves it
// in one click.
func TestJourneyAStallJevKeepsHoldingEndsOnARecoveryCardTheOwnerApproves(t *testing.T) {
	t.Parallel()
	p := &jevfixture.Transitions{Script: func(a jevfixture.Ask) jevfixture.Reply {
		r := jevfixture.DefaultFits(a)
		if a.Trigger == execution.TriggerStalled {
			r.PickA, r.PickB = UnitReviewing, execution.TargetWait
		}
		return r
	}}
	j := newRoutingJourney(t, p)
	subject := trace.UnitSubject(routingUnit)

	j.stall(46 * time.Minute)
	j.awaitRoutes("the first stall", execution.TriggerStalled, subject, 1)
	j.stall(96 * time.Minute)
	holds := j.awaitRoutes("the stall after the first hold", execution.TriggerStalled, subject, 2)
	for _, rec := range holds {
		if rec.State != router.StateHeld || rec.Outcome != router.OutcomeHeld || rec.Hold != router.HoldOrdersDisagree {
			t.Fatalf("a stall Jev held: %+v", rec)
		}
	}
	if n := j.holdNotices(); n != 2 {
		t.Fatalf("the chief of staff got %d notices of the holds, want 2", n)
	}
	if cards := j.recoveryCards(); len(cards) != 0 {
		t.Fatalf("a stall held once has %d recovery cards", len(cards))
	}

	j.stall(146 * time.Minute)
	var card api.InboxEntry
	awaitJourney(t, "the recovery card", func() bool {
		cards := j.recoveryCards()
		if len(cards) == 1 {
			card = cards[0]
		}
		return len(cards) == 1
	})
	if m := card.Recovery; m == nil || m.Origin != api.RecoveryStall || m.Subject != subject || m.Target != UnitChecking || m.Occurrence != holds[1].Occurrence {
		t.Fatalf("the card %+v", card.Recovery)
	}
	j.stall(200 * time.Minute)
	time.Sleep(2 * time.Second)
	if n := len(j.awaitRoutes("the stalled exits", execution.TriggerStalled, subject, 2)); n != 2 || len(j.recoveryCards()) != 1 {
		t.Fatalf("a stall held twice was routed %d times and has %d cards", n, len(j.recoveryCards()))
	}
	j.approveRecoveryCard(card, subject)
	j.jevOnlyAskedAboutExits()
}
