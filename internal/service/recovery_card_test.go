package service

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	"github.com/kpenfound/osmia/internal/servicetest/intakefixture"

	"github.com/kpenfound/osmia/internal/api"
	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/execution"
	"github.com/kpenfound/osmia/internal/jev/jevfixture"
	"github.com/kpenfound/osmia/internal/router"
	"github.com/kpenfound/osmia/internal/trace"
)

func recoveryEntriesOf(t *testing.T, s *Service, repo *trace.Repository, stream config.WorkstreamID) []InboxEntry {
	t.Helper()
	statuses, err := repo.Statuses()
	fixturekit.Must(t, err)
	i := slices.IndexFunc(statuses, func(w trace.WorkstreamStatus) bool { return w.Workstream == stream })
	if i < 0 {
		t.Fatalf("workstream %s has no status", stream)
	}
	entries, err := s.recoveryEntries(repo, statuses[i])
	fixturekit.Must(t, err)
	return entries
}

func recoveryRequestOf(t *testing.T, e InboxEntry) RecoveryRequest {
	t.Helper()
	data, err := json.Marshal(e.Answer.Body)
	fixturekit.Must(t, err)
	var req RecoveryRequest
	fixturekit.Must(t, json.Unmarshal(data, &req))
	return req
}

func lastTransitionOf(t *testing.T, repo *trace.Repository, stream config.WorkstreamID, subject string) trace.Transition {
	t.Helper()
	all, err := trace.Read[trace.Transition](repo, stream)
	fixturekit.Must(t, err)
	var out trace.Transition
	for _, tr := range all {
		if tr.Subject == subject {
			out = tr
		}
	}
	return out
}

func routeRecordsOf(t *testing.T, repo *trace.Repository, stream config.WorkstreamID) []router.Record {
	t.Helper()
	records, err := router.Records(repo, stream)
	fixturekit.Must(t, err)
	return records
}

func recoveryDocuments(t *testing.T, repo *trace.Repository, stream config.WorkstreamID) int {
	t.Helper()
	docs, err := trace.Read[trace.Document](repo, stream)
	fixturekit.Must(t, err)
	n := 0
	for _, d := range docs {
		if strings.HasPrefix(d.Path, "recovery/") {
			n++
		}
	}
	return n
}

func TestAStallNeitherJevNorTheChiefClearedOpensOneRecoveryCard(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newStallFixture(t, "", nil)
	w := f.watch("45m")

	f.clock.QuietFor(46 * time.Minute)
	for _, x := range f.pass(t, w) {
		if _, err := f.s.RouteExit(ctx, x); err != nil {
			t.Fatal(err)
		}
	}
	if got := recoveryEntriesOf(t, f.s, f.repo, fixturekit.Stream); len(got) != 0 {
		t.Fatalf("a stall the router has just decided has %d cards", len(got))
	}

	f.clock.QuietFor(46*time.Minute + 30*time.Minute)
	f.pass(t, w)
	if got := recoveryEntriesOf(t, f.s, f.repo, fixturekit.Stream); len(got) != 0 {
		t.Fatalf("a stall inside the further window has %d cards", len(got))
	}

	f.clock.QuietFor(46*time.Minute + 50*time.Minute)
	for range 3 {
		if raised := f.pass(t, w); len(raised) != 0 {
			t.Fatalf("a decided stall raised %d more exits", len(raised))
		}
	}
	cards := recoveryEntriesOf(t, f.s, f.repo, fixturekit.Stream)
	if len(cards) != 1 {
		t.Fatalf("the stalled unit has %d recovery cards, want 1: %+v", len(cards), cards)
	}
	card := cards[0]
	m := card.Recovery
	if card.Kind != InboxRecovery || card.Unit != stallUnit || card.Workstream != fixturekit.Stream || m == nil {
		t.Fatalf("the card %+v", card)
	}
	if m.Subject != trace.UnitSubject(stallUnit) || m.From != UnitImplementing || m.Version != 1 || m.Target != UnitImplementing || m.Origin != api.RecoveryStall || m.Machine != execution.UnitMachine || m.Reason == "" {
		t.Fatalf("the pinned move %+v", m)
	}
	if card.Answer.Method != http.MethodPost || card.Answer.Path != Prefix+"/recovery/"+string(fixturekit.Stream) || !slices.Equal(card.Options, []string{api.RecoveryApprove}) {
		t.Fatalf("the answer %+v options %v", card.Answer, card.Options)
	}
	req := recoveryRequestOf(t, card)
	if req.Occurrence != m.Occurrence || req.Subject != m.Subject || req.From != m.From || req.Version != m.Version || req.Target != m.Target || req.Unit != stallUnit {
		t.Fatalf("the answer body %+v does not pin the move %+v", req, m)
	}
	if n := recoveryDocuments(t, f.repo, fixturekit.Stream); n != 1 {
		t.Fatalf("the card is recorded %d times", n)
	}

	f.clock.QuietFor(46*time.Minute + 3*time.Hour)
	f.pass(t, w)
	if got := recoveryEntriesOf(t, f.s, f.repo, fixturekit.Stream); len(got) != 1 || recoveryDocuments(t, f.repo, fixturekit.Stream) != 1 {
		t.Fatalf("later passes made %d cards and %d records", len(got), recoveryDocuments(t, f.repo, fixturekit.Stream))
	}

	feed, err := f.s.feedRecoveries(f.repo, fixturekit.Stream)
	fixturekit.Must(t, err)
	if len(feed) != 1 || feed[0].Kind != "recovery" || feed[0].Recovery == nil || feed[0].Recovery.State != recoveryOpen || feed[0].Recovery.Occurrence != m.Occurrence || feed[0].Recovery.Unit != stallUnit {
		t.Fatalf("the feed entry %+v", feed)
	}

	f.transition(t, "stall-moved-on", trace.UnitSubject(stallUnit), UnitImplementing, UnitChecking, stallUnit, ownerActor, f.clock.Now())
	if got := recoveryEntriesOf(t, f.s, f.repo, fixturekit.Stream); len(got) != 0 {
		t.Fatalf("a unit that moved on still has %d cards", len(got))
	}
	feed, err = f.s.feedRecoveries(f.repo, fixturekit.Stream)
	fixturekit.Must(t, err)
	if len(feed) != 1 || feed[0].Recovery.State != recoveryStale {
		t.Fatalf("the feed entry of a stale card %+v", feed)
	}
}

// With the watchdog off no stall reaches the owner.
func TestARecoveryCardIsNotOpenedWithTheWatchdogOff(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newStallFixture(t, "", nil)
	w := f.watch("45m")
	f.clock.QuietFor(46 * time.Minute)
	for _, x := range f.pass(t, w) {
		if _, err := f.s.RouteExit(ctx, x); err != nil {
			t.Fatal(err)
		}
	}
	f.clock.QuietFor(3 * time.Hour)
	off := f.watch("0")
	f.pass(t, off)
	if got := recoveryEntriesOf(t, f.s, f.repo, fixturekit.Stream); len(got) != 0 {
		t.Fatalf("a watchdog that is off opened %d cards", len(got))
	}
}

type moveRecoveryFixture struct {
	*moveFixture
	jev *jevfixture.Transitions
}

func newMoveRecoveryFixture(t *testing.T, key string) *moveRecoveryFixture {
	t.Helper()
	m := newMoveFixture(t, key)
	p := &jevfixture.Transitions{}
	m.f.s.router.Judge = jevfixture.TransitionJudge(m.f.clock.Now, p)
	out := intakefixture.Handle(t, m.move, `{"unit":"resume","to":"approved","note":"The change holds."}`)
	if !strings.Contains(out, "pending as an owner decision on the recovery card") {
		t.Fatalf("the proposal to approve: %s", out)
	}
	return &moveRecoveryFixture{moveFixture: m, jev: p}
}

func (m *moveRecoveryFixture) cards(t *testing.T) []InboxEntry {
	t.Helper()
	return recoveryEntriesOf(t, m.f.s, m.repo, m.stream)
}

func (m *moveRecoveryFixture) approve(req RecoveryRequest) (RecoveryResponse, *APIError) {
	return m.f.s.approveRecovery(context.Background(), string(m.stream), req)
}

// A chief's proposal that belongs to the owner, here approving a candidate,
// produces exactly one recovery card, which shows the pinned move and is in the
// feed. A second look finds the same card.
func TestAnOwnerBoundProposalOpensOneRecoveryCard(t *testing.T) {
	t.Parallel()
	m := newMoveRecoveryFixture(t, "recovery-owner-bound")
	state := intakefixture.UnitWorkflow(t, m.repo, m.stream, "resume")
	for range 2 {
		cards := m.cards(t)
		if len(cards) != 1 {
			t.Fatalf("an owner-bound proposal has %d recovery cards, want 1: %+v", len(cards), cards)
		}
		mv := cards[0].Recovery
		if cards[0].Kind != InboxRecovery || cards[0].Unit != "resume" || mv == nil || mv.Subject != trace.UnitSubject("resume") || mv.From != UnitReviewing || mv.Version != state.Version || mv.Target != UnitApproved || mv.Origin != api.RecoveryOwner || mv.Machine != execution.UnitMachine {
			t.Fatalf("the card %+v pins %+v, the unit is %+v", cards[0], mv, state)
		}
		if !strings.Contains(cards[0].Question, "Approve this recovery move") || !strings.Contains(cards[0].Question, UnitApproved) {
			t.Fatalf("the question %q", cards[0].Question)
		}
	}
	feed, err := m.f.s.feedRecoveries(m.repo, m.stream)
	fixturekit.Must(t, err)
	if len(feed) != 1 || feed[0].Recovery.State != recoveryOpen || feed[0].Recovery.Target != UnitApproved {
		t.Fatalf("the feed %+v", feed)
	}
}

func TestApprovingARecoveryCardAppliesThePinnedMoveAsTheOwner(t *testing.T) {
	t.Parallel()
	m := newMoveRecoveryFixture(t, "recovery-approve")
	card := m.cards(t)[0]
	asked := len(m.jev.Requests())

	res, fail := m.approve(recoveryRequestOf(t, card))
	if fail != nil {
		t.Fatal(fail.Message)
	}
	if res.Workstream != m.stream || res.Target != UnitApproved || res.From != UnitReviewing || res.Occurrence != card.Recovery.Occurrence || res.Detail == "" {
		t.Fatalf("the response %+v", res)
	}
	if got := intakefixture.UnitWorkflow(t, m.repo, m.stream, "resume"); got.Value != UnitApproved {
		t.Fatalf("the unit is %+v", got)
	}
	moved := lastTransitionOf(t, m.repo, m.stream, trace.UnitSubject("resume"))
	if moved.To != UnitApproved || moved.DecidedBy != trace.DecidedByOwner || moved.Actor.Kind != "owner" {
		t.Fatalf("the transition %+v", moved)
	}
	if n := len(m.jev.Requests()); n != asked || len(m.jev.TransitionRequests()) != 0 {
		t.Fatalf("approving asked Jev %d times (%d before) and sent %d transition requests", len(m.jev.Requests()), asked, len(m.jev.TransitionRequests()))
	}
	var pinned bool
	for _, rec := range routeRecordsOf(t, m.repo, m.stream) {
		if rec.Trigger == execution.TriggerMoveOwner && rec.Subject == trace.UnitSubject("resume") {
			pinned = rec.State == router.StateCommitted && rec.Authority == router.AuthorityOwner && rec.DecidedBy == trace.DecidedByOwner && rec.Outcome == router.OutcomeFollowed && rec.Target == UnitApproved && !rec.JevRequested
		}
	}
	if !pinned {
		t.Fatalf("no pinned owner move was routed: %+v", routeRecordsOf(t, m.repo, m.stream))
	}
	if got := m.cards(t); len(got) != 0 {
		t.Fatalf("an approved card is still open: %+v", got)
	}
	feed, err := m.f.s.feedRecoveries(m.repo, m.stream)
	fixturekit.Must(t, err)
	if len(feed) != 1 || feed[0].Recovery.State != recoveryApproved {
		t.Fatalf("the feed %+v", feed)
	}
	if _, e := m.approve(recoveryRequestOf(t, card)); e == nil || e.Code != Conflict || !strings.Contains(e.Message, "approved already") {
		t.Fatalf("a second approval = %+v", e)
	}
}

// A card whose unit has since changed state is stale: approving it is refused as
// a conflict and applies nothing, however the unit moved.
func TestApprovingAStaleRecoveryCardIsRefused(t *testing.T) {
	t.Parallel()
	m := newMoveRecoveryFixture(t, "recovery-stale")
	card := m.cards(t)[0]
	req := recoveryRequestOf(t, card)
	before := len(m.jev.Requests())

	if _, e := m.f.s.ownerMove(context.Background(), string(m.stream), "resume", UnitMoveRequest{To: UnitContested, Note: "Hold it while I look."}); e != nil {
		t.Fatal(e.Message)
	}
	moved := intakefixture.UnitWorkflow(t, m.repo, m.stream, "resume")
	if _, e := m.approve(req); e == nil || e.Code != Conflict || !strings.Contains(e.Message, "stale") {
		t.Fatalf("a stale approval = %+v", e)
	}
	if got := intakefixture.UnitWorkflow(t, m.repo, m.stream, "resume"); got.Value != UnitContested || got.Version != moved.Version {
		t.Fatalf("a refused approval moved the unit to %+v", got)
	}
	for _, tr := range intakefixture.WorkTransitions(t, m.repo, m.stream) {
		if tr.To == UnitApproved {
			t.Fatalf("a stale card approved the unit: %+v", tr)
		}
	}
	if got := m.cards(t); len(got) != 0 {
		t.Fatalf("a stale card is still offered: %+v", got)
	}
	if n := len(m.jev.Requests()); n != before {
		t.Fatalf("a refused approval asked Jev")
	}
	feed, err := m.f.s.feedRecoveries(m.repo, m.stream)
	fixturekit.Must(t, err)
	if len(feed) != 1 || feed[0].Recovery.State != recoveryStale {
		t.Fatalf("the feed %+v", feed)
	}
}

// An approval must pin the move the card shows: a request for another target,
// state, version or occurrence is refused and applies nothing.
func TestApprovingARecoveryCardRequiresItsPins(t *testing.T) {
	t.Parallel()
	m := newMoveRecoveryFixture(t, "recovery-pins")
	card := m.cards(t)[0]
	good := recoveryRequestOf(t, card)
	for name, change := range map[string]func(*RecoveryRequest){
		"target":     func(r *RecoveryRequest) { r.Target = UnitImplementing },
		"from":       func(r *RecoveryRequest) { r.From = UnitChecking },
		"version":    func(r *RecoveryRequest) { r.Version++ },
		"unit":       func(r *RecoveryRequest) { r.Unit = "dedupe" },
		"subject":    func(r *RecoveryRequest) { r.Subject = trace.UnitSubject("dedupe") },
		"occurrence": func(r *RecoveryRequest) { r.Occurrence += "x" },
	} {
		req := good
		change(&req)
		if _, e := m.approve(req); e == nil || e.Code != Conflict {
			t.Fatalf("a request with another %s = %+v", name, e)
		}
	}
	if got := intakefixture.UnitWorkflow(t, m.repo, m.stream, "resume"); got.Value != UnitReviewing {
		t.Fatalf("a refused approval moved the unit to %+v", got)
	}
	if got := m.cards(t); len(got) != 1 {
		t.Fatalf("a refused approval closed the card: %+v", got)
	}
}

// The API endpoint approves the card, and refuses a stale one as a conflict.
func TestTheRecoveryEndpointApprovesAndRefusesAStaleCard(t *testing.T) {
	t.Parallel()
	m := newMoveRecoveryFixture(t, "recovery-endpoint")
	card := m.cards(t)[0]
	body, err := json.Marshal(card.Answer.Body)
	fixturekit.Must(t, err)
	post := func() *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, card.Answer.Path, strings.NewReader(string(body)))
		req.Header.Set("Content-Type", "application/json")
		m.f.s.handle(rec, req)
		return rec
	}
	rec := post()
	if rec.Code != 200 {
		t.Fatalf("the endpoint answered %d: %s", rec.Code, rec.Body)
	}
	var out RecoveryResponse
	fixturekit.Must(t, json.Unmarshal(rec.Body.Bytes(), &out))
	if out.Target != UnitApproved || out.Subject != trace.UnitSubject("resume") || out.Workstream != m.stream {
		t.Fatalf("the endpoint's response %+v", out)
	}
	if got := intakefixture.UnitWorkflow(t, m.repo, m.stream, "resume"); got.Value != UnitApproved {
		t.Fatalf("the unit is %+v", got)
	}
	if again := post(); again.Code != 409 {
		t.Fatalf("approving again answered %d: %s", again.Code, again.Body)
	}
}
