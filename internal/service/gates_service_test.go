package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/intake"
	"github.com/kpenfound/osmia/internal/jev"
	"github.com/kpenfound/osmia/internal/kb"
	"github.com/kpenfound/osmia/internal/seal"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	"github.com/kpenfound/osmia/internal/servicetest/intakefixture"
	"github.com/kpenfound/osmia/internal/shed"
	"github.com/kpenfound/osmia/internal/trace"
)

var gateKeys atomic.Int64

// gateKey names an environment variable holding Jev's API key, set when on.
// Setting it later is how a test configures Jev once its setup is done.
func gateKey(t *testing.T, on bool) string {
	t.Helper()
	name := fmt.Sprintf("OSMIA_GATE_TEST_KEY_%d", gateKeys.Add(1))
	t.Cleanup(func() { os.Unsetenv(name) })
	if on {
		os.Setenv(name, "gate-test-"+name)
	}
	return name
}

// gateJevConfig is the [jev] table that reads its key from the variable.
func gateJevConfig(key string) string {
	return fmt.Sprintf("[jev]\napi_key_env = %q\ntimeout = \"20s\"\n", key)
}

// newJevGateFixture is a debate fixture of one member in one round whose Jev
// answers through p, configured when the key variable is set.
func newJevGateFixture(t *testing.T, p *gateJev, key string, prepare func(*Options)) *shedFixture {
	t.Helper()
	return newDebateFixtureWith(t, 1, 1, gateJevConfig(key), func(o *Options) {
		o.JevProvider = p.Factory()
		if prepare != nil {
			prepare(o)
		}
	})
}

// reachPacket hands in a design whose debate concludes by consensus, and
// returns once the shed rests at its decision point. Nobody ratifies it.
func (f *shedFixture) reachPacket(t *testing.T) config.WorkstreamID {
	t.Helper()
	f.upstream(t)
	faults := &intakefixture.Faults{}
	f.member(1, 1, 1, intakefixture.Concedes(faults))
	stream := f.handIn(t, "design", intakefixture.HandedDesign)
	f.awaitShed(t, stream, "concluded-1")
	faults.Check(t)
	return stream
}

// judgments returns the latest revision of every judgment record of the
// workstream, by ID, and how many revisions there are in all.
func judgments(t *testing.T, repo *trace.Repository, stream config.WorkstreamID) (map[string]jev.Record, int) {
	t.Helper()
	docs, err := trace.Read[trace.Document](repo, stream)
	must(t, err)
	latest := map[string]trace.Document{}
	revisions := 0
	for _, d := range docs {
		if strings.HasPrefix(d.Path, "judgments/") {
			revisions++
			if d.Revision >= latest[d.ID].Revision {
				latest[d.ID] = d
			}
		}
	}
	out := map[string]jev.Record{}
	for id, d := range latest {
		var r jev.Record
		must(t, json.Unmarshal([]byte(d.Content), &r))
		out[id] = r
	}
	return out, revisions
}

// decisionJudgment is the latest record of the workstream's judgment of the
// occurrence, if any.
func decisionJudgment(t *testing.T, repo *trace.Repository, stream config.WorkstreamID, kind jev.DecisionKind, key string) (string, jev.Record, bool) {
	t.Helper()
	records, _ := judgments(t, repo, stream)
	for id, r := range records {
		if o := r.Occurrence; o != nil && o.Kind == string(kind) && o.Key == key {
			return id, r, true
		}
	}
	return "", jev.Record{}, false
}

// ratifications returns the recorded revisions of the ratification of round 1.
func (f *shedFixture) ratifications(t *testing.T, stream config.WorkstreamID) []trace.Document {
	t.Helper()
	return f.documents(t, stream, shed.RatificationDocumentID(1))
}

// requireJevRatification checks the one ratification record is Jev's, with the
// judgment it names, and returns the judgment's ID.
func (f *shedFixture) requireJevRatification(t *testing.T, stream config.WorkstreamID) string {
	t.Helper()
	docs := f.ratifications(t, stream)
	if len(docs) != 1 || docs[0].Path != shed.RatificationPath(1) || docs[0].Actor != trace.JevActor || docs[0].Cause != "owner-shed" {
		t.Fatalf("ratification records: %+v", docs)
	}
	record, err := shed.ParseRatification([]byte(docs[0].Content))
	must(t, err)
	if record.DecidedBy != trace.DecidedByJev || record.Judgment == "" || record.Revision != (shed.Pin{Spec: 1, Plan: 1}) || record.Round != 1 {
		t.Fatalf("ratification %+v", record)
	}
	transitions, err := trace.Read[trace.Transition](f.repository(), stream)
	must(t, err)
	found := false
	for _, tr := range transitions {
		if tr.Subject == intake.OwnerSubject && tr.To == "ratified-1" {
			found = true
			if tr.Actor != trace.JevActor || !strings.HasPrefix(tr.Reason, "Jev ratified") {
				t.Fatalf("the transition of Jev's ratification: %+v", tr)
			}
		}
	}
	if !found {
		t.Fatal("no transition records the ratification")
	}
	records, _ := judgments(t, f.repository(), stream)
	r, ok := records[record.Judgment]
	if !ok || r.State != jev.StateAccepted || r.Occurrence == nil || r.Occurrence.Kind != string(jev.KindRatification) || r.Occurrence.Key != jev.RatificationKey(1, 1, 1) || r.Acceptance != "decision_threshold=0.9" {
		t.Fatalf("judgment %s: %+v", record.Judgment, r)
	}
	return record.Judgment
}

func (f *shedFixture) ratificationEntries(t *testing.T, stream config.WorkstreamID) []InboxEntry {
	t.Helper()
	return f.scenario().InboxEntries(t, stream, InboxRatification)
}

func optionNames(p *gateJev) []string {
	var names []string
	for _, o := range p.Requests()[0].Questions["decision"].Options {
		names = append(names, o.Name)
	}
	return names
}

// A confident Jev ratifies the packet on its own: the ratification is recorded
// with Jev as its actor and its judgment, the workstream is sealed and built as
// after the owner's ratification, and no ratification card is left. No client
// reads the inbox or the status until the end.
func TestAConfidentRatificationIsDecidedByJevAndTheWorkstreamBuilds(t *testing.T) {
	t.Parallel()
	p := &gateJev{confidence: 0.97}
	f := newJevGateFixture(t, p, gateKey(t, true), nil)
	defer f.stop(t)
	stream := f.reachPacket(t)
	f.scenario().AwaitFeature(t, stream, BuildingState)
	judgment := f.requireJevRatification(t, stream)

	if requests := p.Requests(); len(requests) != 1 || !slices.Equal(optionNames(p), []string{"ratify", jev.Defer}) {
		t.Fatalf("requests: %d, options %v", len(requests), optionNames(p))
	}
	state, _ := p.Requests()[0].State.(string)
	for _, owners := range []string{"abandon", "request changes", "redraft"} {
		if strings.Contains(strings.ToLower(state), owners) {
			t.Fatalf("the ratification state offers %q:\n%s", owners, state)
		}
	}
	if docs := f.documents(t, stream, seal.DocumentID); len(docs) != 1 {
		t.Fatalf("seal records: %+v", docs)
	}
	if entries := f.ratificationEntries(t, stream); len(entries) != 0 {
		t.Fatalf("a ratification card remains: %+v", entries)
	}
	res, err := f.s.jev.Lookup(f.repository(), stream, jev.KindRatification, jev.RatificationKey(1, 1, 1))
	must(t, err)
	if res.Status != jev.Binding || res.Judgment != judgment || res.Pick != "ratify" {
		t.Fatalf("lookup %+v", res)
	}
}

// While Jev's judgment of a gate is pending no read shows the gate as waiting
// for the owner, none asks Jev and none records anything. A read after the
// judgment finished shows the decision's result.
func TestReadsNeverAskJevOrRecordWhileAGatesJudgmentIsPending(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	p := &gateJev{confidence: 0.97, wait: func(ctx context.Context, _ string) error {
		once.Do(func() { close(entered) })
		select {
		case <-release:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}}
	f := newJevGateFixture(t, p, gateKey(t, true), nil)
	defer f.stop(t)
	stream := f.reachPacket(t)
	select {
	case <-entered:
	case <-time.After(60 * time.Second):
		t.Fatal("Jev was never asked")
	}
	// The debate's conclusion is also routed, asking Jev for a transition
	// judgment of its own in the background; it is final before the reads are
	// compared with the records.
	eventually(t, "the concluded debate's transition judgment", func() bool {
		records, _ := judgments(t, f.repository(), stream)
		for _, r := range records {
			if o := r.Occurrence; o != nil && o.Kind == jev.TransitionKind && strings.Contains(o.Key, "ws.shed_concluded") {
				return r.State != jev.StateStarted
			}
		}
		return false
	})
	_, before := judgments(t, f.repository(), stream)
	for range 5 {
		if entries := f.ratificationEntries(t, stream); len(entries) != 0 {
			t.Fatalf("a pending gate is in the inbox: %+v", entries)
		}
		status, err := f.c.Status(ctx, stream)
		must(t, err)
		for _, gate := range status.Gates {
			if gate.Kind == "ratification" {
				t.Fatalf("a pending gate is in the status: %+v", status.Gates)
			}
		}
		if _, err := f.c.Packet(ctx, stream); err != nil {
			t.Fatal(err)
		}
	}
	if _, after := judgments(t, f.repository(), stream); after != before || len(p.Requests()) != 1 || len(f.ratifications(t, stream)) != 0 {
		t.Fatalf("reads changed the records: %d judgment revisions, was %d; %d requests", after, before, len(p.Requests()))
	}
	close(release)
	f.scenario().AwaitFeature(t, stream, BuildingState)
	f.requireJevRatification(t, stream)
	if len(p.Requests()) != 1 {
		t.Fatalf("requests: %d", len(p.Requests()))
	}
}

// A below-threshold answer sends the card to the owner with Jev's pick and
// confidence as its recommendation. Later reads do not ask again, and the
// owner's ratification is still recorded as the owner's.
func TestABelowThresholdAnswerSendsTheCardToTheOwnerWithJevsPick(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	p := &gateJev{confidence: 0.6}
	f := newJevGateFixture(t, p, gateKey(t, true), nil)
	defer f.stop(t)
	stream := f.reachPacket(t)
	var entry InboxEntry
	eventually(t, "the card", func() bool {
		entries := f.ratificationEntries(t, stream)
		if len(entries) == 1 {
			entry = entries[0]
		}
		return len(entries) == 1
	})
	if !strings.HasPrefix(entry.Recommendation, "Jev recommends ratify with confidence 0.60, below the 0.90 it needs to decide.") || entry.QuickReply != "" ||
		entry.Jev == nil || entry.Jev.Pick != "ratify" || entry.Jev.Confidence != 0.6 || entry.Jev.Threshold != 0.9 || entry.Jev.Binding || entry.Jev.Judgment == "" || !slices.Equal(entry.Options, []string{"ratify"}) {
		t.Fatalf("card %+v (jev %+v)", entry, entry.Jev)
	}
	for range 3 {
		f.ratificationEntries(t, stream)
		_, err := f.c.Status(ctx, stream)
		must(t, err)
	}
	time.Sleep(500 * time.Millisecond)
	if len(p.Requests()) != 1 || len(f.ratifications(t, stream)) != 0 {
		t.Fatalf("%d requests, %d ratifications", len(p.Requests()), len(f.ratifications(t, stream)))
	}
	f.stop(t)
	f.start(t)
	stream = f.awaitStreamAfterRestart(t, stream)
	time.Sleep(500 * time.Millisecond)
	if len(p.Requests()) != 1 || len(f.ratificationEntries(t, stream)) != 1 {
		t.Fatalf("after the restart: %d requests", len(p.Requests()))
	}

	if _, err := f.c.Ratify(ctx, stream, 1, 1); err != nil {
		t.Fatal(err)
	}
	f.scenario().AwaitFeature(t, stream, BuildingState)
	docs := f.ratifications(t, stream)
	if len(docs) != 1 || docs[0].Actor != ownerActor {
		t.Fatalf("the owner's ratification: %+v", docs)
	}
	if record, err := shed.ParseRatification([]byte(docs[0].Content)); err != nil || record.DecidedBy != "" || record.Judgment != "" {
		t.Fatalf("the owner's ratification %+v: %v", record, err)
	}
}

// awaitStreamAfterRestart waits until the restarted service serves the
// workstream's packet.
func (f *shedFixture) awaitStreamAfterRestart(t *testing.T, stream config.WorkstreamID) config.WorkstreamID {
	t.Helper()
	eventually(t, "the packet after the restart", func() bool {
		_, err := f.c.Packet(context.Background(), stream)
		return err == nil
	})
	return stream
}

// Without Jev's key the card reaches the owner with the chief of staff's own
// recommendation, nothing is sent, and the fallback is recorded.
func TestWithoutJevsKeyTheCardKeepsTheChiefOfStaffsRecommendation(t *testing.T) {
	t.Parallel()
	p := &gateJev{confidence: 0.97}
	f := newJevGateFixture(t, p, gateKey(t, false), nil)
	defer f.stop(t)
	stream := f.reachPacket(t)
	var entry InboxEntry
	eventually(t, "the card", func() bool {
		entries := f.ratificationEntries(t, stream)
		if len(entries) == 1 {
			entry = entries[0]
		}
		return len(entries) == 1
	})
	if entry.Recommendation != "ratify: no objection stands" || entry.Jev != nil || !slices.Equal(entry.Options, []string{"ratify"}) {
		t.Fatalf("card %+v", entry)
	}
	eventually(t, "the recorded fallback", func() bool {
		_, r, ok := decisionJudgment(t, f.repository(), stream, jev.KindRatification, jev.RatificationKey(1, 1, 1))
		return ok && r.State == jev.StateFallback && r.Reason == jev.ReasonUnconfigured
	})
	if len(p.Requests()) != 0 || len(f.ratifications(t, stream)) != 0 {
		t.Fatalf("%d requests, %d ratifications", len(p.Requests()), len(f.ratifications(t, stream)))
	}
}

// A judgment recorded as confident and then interrupted before its decision is
// applied after the restart: the workstream is ratified once with the same
// pick, as Jev, and Jev is not asked again.
func TestAConfidentJudgmentInterruptedBeforeItsDecisionIsAppliedOnceAfterARestart(t *testing.T) {
	t.Parallel()
	p := &gateJev{confidence: 0.97}
	reached := make(chan struct{})
	var once sync.Once
	f := newJevGateFixture(t, p, gateKey(t, true), func(o *Options) {
		o.boundary = func(ctx context.Context, step string) error {
			if step == "gate-judged:"+string(jev.KindRatification) {
				once.Do(func() { close(reached) })
				<-ctx.Done()
				return errors.New("power loss")
			}
			return nil
		}
	})
	defer f.stop(t)
	stream := f.reachPacket(t)
	select {
	case <-reached:
	case <-time.After(60 * time.Second):
		t.Fatal("the judgment never finished")
	}
	id, record, ok := decisionJudgment(t, f.repository(), stream, jev.KindRatification, jev.RatificationKey(1, 1, 1))
	if !ok || record.State != jev.StateAccepted || len(f.ratifications(t, stream)) != 0 {
		t.Fatalf("judgment %s %+v, %d ratifications", id, record, len(f.ratifications(t, stream)))
	}
	f.stop(t)

	f.opts.boundary = nil
	f.start(t)
	f.scenario().AwaitFeature(t, stream, BuildingState)
	if judgment := f.requireJevRatification(t, stream); judgment != id {
		t.Fatalf("ratified on judgment %s, recorded %s", judgment, id)
	}
	if len(p.Requests()) != 1 {
		t.Fatalf("requests: %d", len(p.Requests()))
	}
}

// A decision recorded with its downstream effects unfinished is finished after
// the restart, once: the workstream is sealed from the one ratification, with
// no second decision and no request.
func TestAJevRatificationWhoseSealingWasInterruptedIsSealedOnceAfterARestart(t *testing.T) {
	t.Parallel()
	p := &gateJev{confidence: 0.97}
	var holdSealing atomic.Bool
	var crashes atomic.Int32
	holdSealing.Store(true)
	f := newJevGateFixture(t, p, gateKey(t, true), func(o *Options) {
		o.boundary = func(_ context.Context, step string) error {
			if step == "seal-branch-created" && holdSealing.Load() {
				crashes.Add(1)
				return errors.New("power loss")
			}
			return nil
		}
	})
	defer f.stop(t)
	stream := f.reachPacket(t)
	eventually(t, "the interrupted sealing", func() bool { return crashes.Load() > 0 })
	judgment := f.requireJevRatification(t, stream)
	if _, _, found, err := seal.Latest(f.repository(), stream); err != nil || found {
		t.Fatalf("the interrupted sealing recorded a seal: %v %v", found, err)
	}
	f.stop(t)

	holdSealing.Store(false)
	f.opts.boundary = nil
	f.start(t)
	f.scenario().AwaitFeature(t, stream, BuildingState)
	if again := f.requireJevRatification(t, stream); again != judgment {
		t.Fatalf("ratified on judgment %s, then %s", judgment, again)
	}
	if docs := f.documents(t, stream, seal.DocumentID); len(docs) != 1 || len(p.Requests()) != 1 {
		t.Fatalf("%d seal records, %d requests", len(docs), len(p.Requests()))
	}
}

// If the owner ratifies while Jev's judgment is pending, the owner's decision
// stands and Jev's pick is not applied.
func TestAnOwnerDecisionMadeFirstStandsOverJevsPick(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	p := &gateJev{confidence: 0.97, wait: func(ctx context.Context, _ string) error {
		once.Do(func() { close(entered) })
		select {
		case <-release:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}}
	f := newJevGateFixture(t, p, gateKey(t, true), nil)
	defer f.stop(t)
	stream := f.reachPacket(t)
	<-entered
	if _, err := f.c.Ratify(ctx, stream, 1, 1); err != nil {
		t.Fatal(err)
	}
	close(release)
	f.scenario().AwaitFeature(t, stream, BuildingState)
	time.Sleep(500 * time.Millisecond)
	docs := f.ratifications(t, stream)
	if len(docs) != 1 || docs[0].Actor != ownerActor {
		t.Fatalf("ratification records: %+v", docs)
	}
	if res, err := f.s.jev.Lookup(f.repository(), stream, jev.KindRatification, jev.RatificationKey(1, 1, 1)); err != nil || res.Status != jev.Binding {
		t.Fatalf("lookup %+v %v", res, err)
	}
}

// A confident pick the service refuses goes to the owner's inbox with Jev's
// judgment shown, and Jev is not asked again, after a restart included.
func TestARefusedConfidentPickLeavesTheCardWithTheOwnerShowingJevsJudgment(t *testing.T) {
	t.Parallel()
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	p := &gateJev{confidence: 0.97, wait: func(ctx context.Context, _ string) error {
		once.Do(func() { close(entered) })
		select {
		case <-release:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}}
	f := newJevGateFixture(t, p, gateKey(t, true), nil)
	defer f.stop(t)
	stream := f.reachPacket(t)
	<-entered
	// The plan stops validating while Jev decides, so the service refuses the
	// ratification the pick calls for.
	must(t, kb.Store(context.Background(), f.repository(), kb.Map{Version: kb.Version}, f.clock.Now(), ownerActor, "gate-test"))
	close(release)

	var entry InboxEntry
	eventually(t, "the refused card", func() bool {
		entries := f.ratificationEntries(t, stream)
		if len(entries) == 1 {
			entry = entries[0]
		}
		return len(entries) == 1
	})
	if entry.Jev == nil || !entry.Jev.Binding || entry.Jev.Pick != "ratify" || !strings.Contains(entry.Jev.Refused, "the plan is not valid") || entry.Jev.Judgment == "" ||
		!strings.HasPrefix(entry.Recommendation, "Jev decided ratify with confidence 0.97, but the service refused it:") || len(f.ratifications(t, stream)) != 0 {
		t.Fatalf("card %+v (jev %+v)", entry, entry.Jev)
	}
	f.stop(t)
	f.start(t)
	f.awaitStreamAfterRestart(t, stream)
	time.Sleep(500 * time.Millisecond)
	if entries := f.ratificationEntries(t, stream); len(entries) != 1 || entries[0].Jev == nil || !entries[0].Jev.Binding || len(p.Requests()) != 1 || len(f.ratifications(t, stream)) != 0 {
		t.Fatalf("after the restart: %+v, %d requests", entries, len(p.Requests()))
	}
}

// presentedAmendment is a fixture whose Jev is unconfigured while the building
// workstream is set up and the amendment presented, and configured after: the
// amendment is what Jev decides.
func presentedAmendment(t *testing.T, p *gateJev) (*shedFixture, config.WorkstreamID, string, seal.Seal, string) {
	t.Helper()
	key := gateKey(t, false)
	f := newDebateFixtureWith(t, 1, 1, gateJevConfig(key), func(o *Options) { o.JevProvider = p.Factory() })
	stream := f.buildFixture().Amendment(t)
	before, _, _, err := seal.Latest(f.repository(), stream)
	must(t, err)
	requester := presentAmendment(t, f, stream, masonRole, amendedSpec, validPlan)
	return f, stream, requester, before, key
}

// A confident Jev decides an amendment that changes the approved intent, as
// the owner would: approving versions and reseals the spec, declining keeps the
// sealed one, the requester's ruling names Jev, and no amendment card is left.
func TestAConfidentJevApprovesOrDeclinesAnIntentChangingAmendment(t *testing.T) {
	t.Parallel()
	for _, choice := range []string{"approve", "reject"} {
		t.Run(choice, func(t *testing.T) {
			t.Parallel()
			p := &gateJev{confidence: 0.96, pick: func(string, []string) string { return choice }}
			f, stream, requester, before, key := presentedAmendment(t, p)
			defer f.stop(t)
			os.Setenv(key, "gate-test-"+key)
			f.awaitAmendment(t, stream, amendmentRuled)

			docs := f.documents(t, stream, "amendment_1_decision")
			if len(docs) != 1 || docs[0].Actor != trace.JevActor || docs[0].Path != "amendments/1/decision.json" {
				t.Fatalf("decision records: %+v", docs)
			}
			var decision struct{ Decision, Actor, Judgment, Note string }
			must(t, json.Unmarshal([]byte(docs[0].Content), &decision))
			if !strings.EqualFold(decision.Decision, choice) || decision.Actor != trace.DecidedByJev || decision.Judgment == "" || !strings.Contains(decision.Note, decision.Judgment) {
				t.Fatalf("decision %+v", decision)
			}
			records, _ := judgments(t, f.repository(), stream)
			if r, ok := records[decision.Judgment]; !ok || r.Occurrence == nil || r.Occurrence.Kind != string(jev.KindIntentAmendment) || r.Occurrence.Key != jev.AmendmentKey("1") {
				t.Fatalf("judgment %s: %+v", decision.Judgment, r)
			}
			if got := optionNames(p); !slices.Equal(got, []string{"approve", "reject"}) || len(p.Requests()) != 1 {
				t.Fatalf("options %v after %d requests", got, len(p.Requests()))
			}
			after, _, _, err := seal.Latest(f.repository(), stream)
			must(t, err)
			wantSeal := before.Seal
			if choice == "approve" {
				wantSeal++
			}
			if after.Seal != wantSeal {
				t.Fatalf("seal %d, want %d", after.Seal, wantSeal)
			}
			turns := f.ruling(t, stream, requester)
			if len(turns) != 1 || !strings.Contains(turns[0].Request.Prompt, "Actor: jev.") || !strings.Contains(turns[0].Request.Prompt, "jev_response") {
				t.Fatalf("the requester's ruling: %+v", turns)
			}
			if entries := f.scenario().InboxEntries(t, stream, InboxAmendment); len(entries) != 0 {
				t.Fatalf("an amendment card remains: %+v", entries)
			}
		})
	}
}

// An amendment that leaves the approved intent unchanged is the chief of
// staff's to decide, not the owner's gate: Jev is never asked about it.
func TestJevIsNeverAskedAboutAPlanOnlyAmendment(t *testing.T) {
	t.Parallel()
	p := &gateJev{confidence: 0.99}
	f := newDebateFixtureWith(t, 1, 1, gateJevConfig(gateKey(t, true)), func(o *Options) { o.JevProvider = p.Factory() })
	stream := f.buildFixture().Amendment(t)
	defer f.stop(t)
	presentAmendment(t, f, stream, masonRole, validSpec, amendedPlan)
	time.Sleep(2 * time.Second)
	if state, err := f.repository().Workflow(stream, amendmentSubject("1")); err != nil || state.Value != intake.AmendmentPresented {
		t.Fatalf("amendment 1 is %q: %v", state.Value, err)
	}
	if _, revisions := judgments(t, f.repository(), stream); len(p.Requests()) != 0 || revisions != 0 {
		t.Fatalf("%d requests, %d judgment revisions", len(p.Requests()), revisions)
	}
}

// newEscalationFixture is a trace holding one workstream whose escalations the
// gate worker decides through a Jev answering through p, configured.
func newEscalationFixture(t *testing.T, p *gateJev) (*gateFixture, *trace.Repository, config.WorkstreamID) {
	t.Helper()
	f := newGateFixture(t, p)
	return f, f.repo, fixturekit.Stream
}

// claimThread creates a thread of the role with one claimed turn and returns
// the scope of that turn.
func claimThread(t *testing.T, repo *trace.Repository, now time.Time, agent, role, turn, unit string) coreadapter.Scope {
	t.Helper()
	ctx := context.Background()
	owner := trace.Actor{Kind: "owner", ID: "local"}
	h := func(schema, id string) trace.Header {
		return trace.Header{Schema: "osmia.trace." + schema, Version: trace.Version, ID: id, Revision: 1, Project: repo.Project(), Workstream: fixturekit.Stream, Unit: unit, At: now, Actor: owner, Cause: "gate-test"}
	}
	thread := agent + "_thread"
	must(t, repo.CreateThread(ctx, trace.Agent{Header: h("agent", agent), Role: role, ThreadID: thread}))
	_, err := repo.EnqueueTurn(ctx, trace.TurnRequest{Header: h("turn-request", "request_"+turn), AgentID: agent, ThreadID: thread, TurnID: turn, Profile: coreadapter.Profile{Name: "default", Backend: "fake", Model: "test"}, Prompt: "Work"})
	must(t, err)
	_, err = repo.ClaimTurn(ctx, fixturekit.Stream, agent, "token_"+turn, "/owned/"+turn, now)
	must(t, err)
	return coreadapter.Scope{Project: string(repo.Project()), Workstream: string(fixturekit.Stream), Unit: unit, Thread: thread, Turn: turn, Role: role}
}

// escalate asks a question as a worker of the role and has the chief of staff
// escalate it with the given options.
func escalate(t *testing.T, f *gateFixture, role string, options ...string) coreadapter.Scope {
	t.Helper()
	ctx := context.Background()
	chief := claimThread(t, f.repo, f.now(), "chief", trace.StatusRole, "chief1", "")
	asker := claimThread(t, f.repo, f.now(), "asker", role, "build_asker", "unit1")
	_, err := f.repo.Ask(ctx, "asker", asker, "Should uploads resume after a restart?", f.now())
	must(t, err)
	_, err = f.repo.EscalateQuestions(ctx, "chief", chief, trace.EscalationRequest{Questions: []string{"1"}, Rephrasing: "Should uploads resume?", Section: "spec#1", Conflict: "The spec says resume; the plan says restart.", Blocked: "The upload unit.", Options: options, Recommendation: "Resume from the last chunk."}, f.now())
	must(t, err)
	return chief
}

func (f *gateFixture) gates(t *testing.T) []intake.Gate {
	t.Helper()
	gates, err := f.s.intake().ReachedGates(context.Background(), &intake.Project{ID: f.repo.Project(), Repository: f.repo})
	must(t, err)
	return gates
}

// Jev rules on an escalated question from the options the chief of staff wrote,
// as the owner would: the ruling is recorded with Jev as its actor and its
// judgment, the card is never shown, and the chief of staff relays the ruling
// as it relays the owner's.
func TestAConfidentJevRulesOnAnEscalatedQuestionFromTheChiefsOptions(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	p := &gateJev{confidence: 0.95, pick: func(_ string, options []string) string { return options[1] }}
	f, repo, stream := newEscalationFixture(t, p)
	chief := escalate(t, f, masonRole, "Resume from the last chunk.", "Restart the upload.")

	gates := f.gates(t)
	if len(gates) != 1 || gates[0].Kind != jev.KindEscalatedQuestion || gates[0].Key != jev.QuestionKey("1") || len(gates[0].Options) != 2 || gates[0].Options[1].Description != "Restart the upload." {
		t.Fatalf("gates %+v", gates)
	}
	entries, err := repo.Inbox()
	must(t, err)
	if _, open, err := f.s.intake().EscalationCard(repo, entries[0], intake.InboxView(entries[0])); err != nil || open {
		t.Fatalf("the card of a gate Jev is deciding is shown: %v %v", open, err)
	}
	f.w.handle(ctx, gates[0])
	if len(p.Requests()) != 1 || !slices.Equal(optionNames(p), []string{"option-1", "option-2"}) {
		t.Fatalf("requests %d, options %v", len(p.Requests()), optionNames(p))
	}
	states, err := repo.Questions(stream)
	must(t, err)
	judgment, _, _ := decisionJudgment(t, repo, stream, jev.KindEscalatedQuestion, jev.QuestionKey("1"))
	ruling := states[0].Ruling
	if states[0].State != trace.QuestionRuled || ruling == nil || ruling.Actor != trace.JevActor || ruling.DecidedBy != trace.DecidedByJev || ruling.Judgment != judgment || judgment == "" || ruling.OwnerResponse != "Restart the upload." {
		t.Fatalf("question %+v", states[0])
	}
	if gates := f.gates(t); len(gates) != 0 {
		t.Fatalf("gates after the ruling: %+v", gates)
	}
	if after, err := repo.Inbox(); err != nil || after[0].State == trace.QuestionEscalated {
		t.Fatalf("the escalation is still open: %+v %v", after, err)
	}
	// The chief of staff relays the ruling as it relays the owner's.
	if _, err := repo.RelayRuling(ctx, "chief", chief, "1", "Restart the upload.", trace.ScopeLocal, f.now()); err != nil {
		t.Fatal(err)
	}
	relayed, err := repo.Questions(stream)
	must(t, err)
	if relayed[0].State != trace.QuestionAnswered || relayed[0].Ruling.DecidedBy != trace.DecidedByJev || relayed[0].Ruling.Judgment != judgment || relayed[0].Ruling.Actor.Kind != "agent" {
		t.Fatalf("relayed %+v", relayed[0])
	}
	f.w.handle(ctx, gates[0])
	if len(p.Requests()) != 1 {
		t.Fatalf("a ruled escalation was asked again: %d requests", len(p.Requests()))
	}
}

// An escalation Jev may not decide is never sent: an architect's question, such
// as the judgment that a project needs no change, and one with no options the
// chief of staff wrote are the owner's, and their cards show at once.
func TestEscalationsJevMayNotDecideAreNeverSent(t *testing.T) {
	t.Parallel()
	for name, c := range map[string]struct {
		role    string
		options []string
	}{
		"an architect's question": {architectRole, []string{"Keep the project.", "Abandon the hand-in."}},
		"no written options":      {masonRole, nil},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			p := &gateJev{confidence: 0.99}
			f := newGateFixture(t, p)
			escalate(t, f, c.role, c.options...)
			if gates := f.gates(t); len(gates) != 0 {
				t.Fatalf("gates %+v", gates)
			}
			entries, err := f.repo.Inbox()
			must(t, err)
			card, open, err := f.s.intake().EscalationCard(f.repo, entries[0], intake.InboxView(entries[0]))
			if err != nil || !open || card.Jev != nil || card.Recommendation != "Resume from the last chunk." {
				t.Fatalf("card %+v open %v: %v", card, open, err)
			}
			if _, revisions := judgments(t, f.repo, fixturekit.Stream); len(p.Requests()) != 0 || revisions != 0 {
				t.Fatalf("%d requests, %d judgment revisions", len(p.Requests()), revisions)
			}
		})
	}
}

// An escalation that carries a binding Jev judgment of the chief of staff's
// decision the service could not carry out reaches the owner with the judgment
// shown, and the gate sends nothing.
func TestAnEscalationCarryingABindingJudgmentGoesToTheOwnerWithoutAGateRequest(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	p := &gateJev{confidence: 0.99}
	f := newGateFixture(t, p)
	escalate(t, f, masonRole, "Resume from the last chunk.", "Restart the upload.")
	// The chief's own question-routing decision was a binding pick to answer it.
	chiefJev := &gateJev{confidence: 0.97, pick: func(string, []string) string { return "answer" }}
	routed := gateJudge(f.now, chiefJev).Decide(ctx, f.repo, jev.DecisionRequest{Scope: coreadapter.Scope{Project: string(f.repo.Project()), Workstream: string(fixturekit.Stream)}, Cause: "chief-decide", Kind: jev.KindQuestionRouting, Occurrence: jev.QuestionKey("1"), State: "state",
		Options: []jev.DecisionOption{{Name: "answer", Description: "Answer it."}, {Name: "escalate", Description: "Escalate it."}}})
	if routed.Status != jev.Binding {
		t.Fatalf("routing %+v", routed)
	}
	if gates := f.gates(t); len(gates) != 0 {
		t.Fatalf("gates %+v", gates)
	}
	entries, err := f.repo.Inbox()
	must(t, err)
	card, open, err := f.s.intake().EscalationCard(f.repo, entries[0], intake.InboxView(entries[0]))
	if err != nil || !open || card.Jev == nil || !card.Jev.Binding || card.Jev.Pick != "answer" || card.Jev.Judgment != routed.Judgment || !strings.Contains(card.Recommendation, "Jev decided answer") {
		t.Fatalf("card %+v open %v: %v", card, open, err)
	}
	if len(p.Requests()) != 0 {
		t.Fatalf("%d gate requests", len(p.Requests()))
	}
	// A routing pick that was to escalate is executable: the gate judges it.
	f2 := newGateFixture(t, p)
	escalate(t, f2, masonRole, "Resume from the last chunk.", "Restart the upload.")
	gateJudge(f2.now, &gateJev{confidence: 0.97, pick: func(string, []string) string { return "escalate" }}).Decide(ctx, f2.repo, jev.DecisionRequest{Scope: coreadapter.Scope{Project: string(f2.repo.Project()), Workstream: string(fixturekit.Stream)}, Cause: "chief-decide", Kind: jev.KindQuestionRouting, Occurrence: jev.QuestionKey("1"), State: "state",
		Options: []jev.DecisionOption{{Name: "answer", Description: "Answer it."}, {Name: "escalate", Description: "Escalate it."}}})
	if gates := f2.gates(t); len(gates) != 1 {
		t.Fatalf("gates %+v", gates)
	}
}

// Jev rules on a contested unit the chief of staff escalated, from the one
// ruling that needs none of the owner's words: the unit's candidate is reviewed
// again, as Jev's ruling, and the card is never shown. A contest the chief of
// staff did not escalate is not a gate.
func TestAConfidentJevReviewsAnEscalatedContestAgain(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	c := newContestFixture(t, "gate-contest")
	p := &gateJev{confidence: 0.95}
	c.f.s.jev = gateJudge(c.f.clock.Now, p)
	contest := c.contest(t)
	if gate, carried, err := c.f.s.contestedGate(c.repo, c.stream, "resume"); err != nil || gate != nil || carried != nil {
		t.Fatalf("a contest the chief of staff did not raise is a gate: %+v %+v %v", gate, carried, err)
	}
	if out := c.resolve(t, `{"unit":"resume","decision":"escalate","note":"The reviewer ends every turn without a verdict."}`); !strings.Contains(out, `"recorded":true`) {
		t.Fatalf("escalate: %s", out)
	}
	gate, carried, err := c.f.s.contestedGate(c.repo, c.stream, "resume")
	if err != nil || gate == nil || carried != nil || gate.Kind != jev.KindEscalatedContest || gate.Key != jev.ContestKey(contest.ID) || len(gate.Options) != 1 || gate.Options[0].Name != "review" {
		t.Fatalf("gate %+v carried %+v: %v", gate, carried, err)
	}
	if _, open, err := c.f.s.intake().ContestedCard(c.repo, c.stream, "resume"); err != nil || open {
		t.Fatalf("the card of a gate Jev is deciding is shown: %v %v", open, err)
	}
	w := newGateWorker(c.f.s, &activeProject{id: c.repo.Project(), repository: c.repo})
	w.handle(ctx, *gate)

	if got := c.state(t).Value; got != UnitReviewing {
		t.Fatalf("the unit is %q", got)
	}
	transitions, err := trace.Read[trace.Transition](c.repo, c.stream)
	must(t, err)
	var ruled *trace.Transition
	for i, tr := range transitions {
		if tr.Subject == trace.UnitSubject("resume") && tr.From == UnitContested && tr.To == UnitReviewing {
			ruled = &transitions[i]
		}
	}
	if ruled == nil || ruled.Actor != trace.JevActor || !strings.Contains(ruled.Reason, "jev ruled review") {
		t.Fatalf("the ruling: %+v", ruled)
	}
	if res, err := c.f.s.jev.Lookup(c.repo, c.stream, jev.KindEscalatedContest, jev.ContestKey(contest.ID)); err != nil || res.Status != jev.Binding || res.Pick != "review" {
		t.Fatalf("lookup %+v %v", res, err)
	}
	w.handle(ctx, *gate)
	if len(p.Requests()) != 1 {
		t.Fatalf("requests: %d", len(p.Requests()))
	}
}

// Delivery approval is the owner's alone: a workstream that waits for it has
// no gate Jev may decide, so nothing is sent for it and no judgment is
// recorded.
func TestDeliveryApprovalIsNeverAGateForJev(t *testing.T) {
	t.Parallel()
	f, stream, repository, _ := deliveryFixture(t)
	defer f.stop(t)
	inbox, api := f.s.inbox(context.Background())
	if api != nil {
		t.Fatal(api)
	}
	delivery := 0
	for _, e := range inbox.Entries {
		if e.Kind == InboxDelivery && e.Workstream == stream {
			delivery++
		}
	}
	if delivery != 1 {
		t.Fatalf("delivery cards: %+v", inbox.Entries)
	}
	gates, err := f.s.intake().ReachedGates(context.Background(), &intake.Project{ID: repository.Project(), Repository: repository})
	must(t, err)
	if len(gates) != 0 {
		t.Fatalf("gates of a delivery waiting for approval: %+v", gates)
	}
	if _, revisions := judgments(t, repository, stream); revisions != 0 {
		t.Fatalf("%d judgment revisions", revisions)
	}
}

// A cancelled workstream's open gate is not Jev's: abandoning is the owner's
// command and ends the gate.
func TestACancelledWorkstreamHasNoGateForJev(t *testing.T) {
	t.Parallel()
	p := &gateJev{confidence: 0.97}
	f := newJevGateFixture(t, p, gateKey(t, false), nil)
	defer f.stop(t)
	stream := f.reachPacket(t)
	if _, err := f.c.Abandon(context.Background(), stream, "cancelled by the owner"); err != nil {
		t.Fatal(err)
	}
	gates, err := f.s.intake().ReachedGates(context.Background(), &intake.Project{ID: f.repository().Project(), Repository: f.repository()})
	must(t, err)
	if len(gates) != 0 || len(p.Requests()) != 0 {
		t.Fatalf("gates %+v, %d requests", gates, len(p.Requests()))
	}
}
