package service

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/kpenfound/osmia/internal/amendment"
	"github.com/kpenfound/osmia/internal/api"
	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	"github.com/kpenfound/osmia/internal/shed"
	"github.com/kpenfound/osmia/internal/trace"
)

const jevJudgment = "judgment_0123456789abcdef"

var contractDeciders = []string{"owner", trace.Manager, "committee"}

func requireWorkDecision(t *testing.T, p fixturekit.HooksPost, judgment string) {
	t.Helper()
	if p.Source != "osmia-work" || p.Str("author") != "" || p.Str("decider") == "owner" || !slices.Contains(contractDeciders, p.Str("decider")) {
		t.Fatalf("Jev's decision: %s on %s", p.Raw, p.Source)
	}
	text := p.Str("context") + "\n" + p.Str("rationale")
	if !strings.Contains(text, "Jev") || !strings.Contains(text, judgment) {
		t.Fatalf("the decision does not say Jev decided it under %s: %s", judgment, p.Raw)
	}
}

func postsOf(posts []fixturekit.HooksPost, kind, field, value string) []fixturekit.HooksPost {
	var out []fixturekit.HooksPost
	for _, p := range posts {
		if p.Str("kind") == kind && p.Str(field) == value {
			out = append(out, p)
		}
	}
	return out
}

func (f *publisherFixture) jevAmendmentDecision(stream config.WorkstreamID, id string, revision int, verdict, note string, when time.Time) {
	f.t.Helper()
	data, err := json.Marshal(api.AmendmentDecision{Decision: verdict, Note: note, Round: 1, Packet: 1, Spec: 2, Plan: 2, SealRevision: 1, Actor: trace.DecidedByJev, Judgment: jevJudgment})
	fixturekit.Must(f.t, err)
	f.document(stream, "amendment_"+id+"_decision", revision, "amendments/"+id+"/decision.json", string(data), when, trace.JevActor)
}

func (f *publisherFixture) ratify(stream config.WorkstreamID, byJev bool, when time.Time) {
	f.t.Helper()
	r := shed.Ratification{Version: shed.Version, Round: 1, Revision: shed.Pin{Spec: 1, Plan: 1}}
	actor := ownerActor
	if byJev {
		r.DecidedBy, r.Judgment, actor = trace.DecidedByJev, jevJudgment, trace.JevActor
	}
	data, err := json.Marshal(r)
	fixturekit.Must(f.t, err)
	f.document(stream, shed.RatificationDocumentID(1), 1, shed.RatificationPath(1), string(data), when, actor)
}

func (f *publisherFixture) applied(stream config.WorkstreamID, id string, n int, when time.Time) {
	f.t.Helper()
	data, err := json.Marshal(amendment.Application{Amendment: id, To: amendment.Pin{Seal: n, SealRevision: n, Spec: n, Plan: 1}})
	fixturekit.Must(f.t, err)
	f.document(stream, amendment.DocumentID(id), 1, amendment.Path(id), string(data), when, trace.Actor{Kind: "service", ID: "amendments"})
}

func TestPublishJevRulingIsAWorkDecisionAndTheOwnersStaysTheOwners(t *testing.T) {
	f := newPublisherFixture(t)
	stream := fixturekit.Stream
	owner := trace.Actor{Kind: "owner", ID: "local"}
	for _, id := range []string{"1", "2", "3"} {
		f.ask(stream, id, "Is the upload cap 10 MiB or unbounded?", hr(1))
	}
	f.escalate(stream, "1", "Is the upload cap 10 MiB or unbounded?", hr(2))
	f.escalate(stream, "2", "Is the upload cap 10 MiB or unbounded?", hr(2))
	f.ruling(stream, "1", 1, trace.Ruling{Decision: trace.DecisionRuling, OwnerResponse: "Cap uploads at 10 MiB."}, hr(3), owner)
	f.ruling(stream, "2", 1, trace.Ruling{Decision: trace.DecisionRuling, OwnerResponse: "Stream without a cap.", DecidedBy: trace.DecidedByJev, Judgment: jevJudgment}, hr(3), trace.JevActor)
	f.ruling(stream, "2", 2, trace.Ruling{Decision: trace.DecisionRuling, OwnerResponse: "Stream without a cap.", ReturnedAnswer: "RELAYED-TO-THE-ASKERS", Scope: trace.ScopeLocal, DecidedBy: trace.DecidedByJev, Judgment: jevJudgment}, hr(4), managerActor)
	f.ruling(stream, "3", 1, trace.Ruling{Decision: trace.DecisionAnswer, ReturnedAnswer: "Resume is in scope.", Citations: []string{"spec#1"}, DecidedBy: trace.DecidedByJev, Judgment: jevJudgment}, hr(5), trace.JevActor)

	posts, _ := f.pass()
	got := fixturekit.Decisions(posts)
	if len(got) != 3 {
		t.Fatalf("%d decisions, want the owner's ruling and Jev's ruling and answer: %v", len(got), kinds(posts))
	}
	ruled := decisionOf(t, posts, string(stream)+"-ruling-1")
	if ruled.Source != "osmia-owner" || ruled.Str("author") != "owner" || ruled.Str("decider") != "owner" {
		t.Fatalf("the owner's ruling: %s on %s", ruled.Raw, ruled.Source)
	}
	jev := decisionOf(t, posts, string(stream)+"-jev-ruling-2")
	requireWorkDecision(t, jev, jevJudgment)
	if jev.Str("outcome") != "Stream without a cap." || jev.Str("question") != string(stream)+":2" || jev.Str("workstream") != string(stream) {
		t.Fatalf("Jev's ruling: %s", jev.Raw)
	}
	for _, want := range []string{"Should uploads be capped?", "spec.md: Upload limits", "- Stream without a cap", "The manager recommended: Cap at 10 MiB"} {
		if !strings.Contains(jev.Str("context")+jev.Str("rationale"), want) {
			t.Fatalf("Jev's ruling lacks %q: %s", want, jev.Raw)
		}
	}
	answer := decisionOf(t, posts, string(stream)+"-jev-answer-3")
	requireWorkDecision(t, answer, jevJudgment)
	if answer.Str("outcome") != "Resume is in scope." {
		t.Fatalf("Jev's answer: %s", answer.Raw)
	}
	if n := len(postsOf(posts, "decision", "question", string(stream)+":2")); n != 1 {
		t.Fatalf("%d decisions about the question Jev ruled on", n)
	}
	for _, p := range posts {
		if strings.Contains(p.Raw, "RELAYED-TO-THE-ASKERS") {
			t.Fatalf("a post carries the manager's relay: %s", p.Raw)
		}
		if p.Source == "osmia-owner" && strings.Contains(p.Raw, "Jev") {
			t.Fatalf("Jev's decision reached the owner source: %s", p.Raw)
		}
	}
	if again, _ := f.pass(); len(again) != 0 {
		t.Fatalf("unchanged decisions were reposted: %v", kinds(again))
	}
}

// A Jev amendment decision, approving or declining, is posted on the work
// source with no author and nothing for it reaches the owner source, while the
// owner's amendment decision stays on the owner source with the owner as author.
func TestPublishJevAmendmentDecisionsAreWorkDecisions(t *testing.T) {
	f := newPublisherFixture(t)
	stream := fixturekit.Stream
	f.recordSeal(stream, 1, "# Upload limits\n\nCap uploads.\n")
	owner := trace.Actor{Kind: "owner", ID: "local"}
	f.amendment(stream, "1", []string{"spec#1"}, "Allow 20 MiB uploads.", "The partner needs it.", "", hr(2))
	f.amendmentDecision(stream, "1", 1, "approve", "Go ahead.", hr(3), owner)
	f.amendment(stream, "2", []string{"spec#2"}, "Allow 50 MiB uploads.", "More headroom.", "", hr(4))
	f.jevAmendmentDecision(stream, "2", 1, "approve", "Jev agrees.", hr(5))
	f.amendment(stream, "3", []string{"spec#3"}, "Drop the upload cap.", "Not needed.", "", hr(6))
	f.jevAmendmentDecision(stream, "3", 1, "reject", "It contradicts the spec.", hr(7))

	posts, _ := f.pass()
	if got := fixturekit.Decisions(posts); len(got) != 3 {
		t.Fatalf("%d decisions: %v", len(got), kinds(posts))
	}
	ownerDecided := decisionOf(t, posts, string(stream)+"-amendment-1")
	if ownerDecided.Source != "osmia-owner" || ownerDecided.Str("author") != "owner" || ownerDecided.Str("decider") != "owner" {
		t.Fatalf("owner-decided amendment: %s on %s", ownerDecided.Raw, ownerDecided.Source)
	}
	approved := decisionOf(t, posts, string(stream)+"-jev-amendment-2")
	declined := decisionOf(t, posts, string(stream)+"-jev-amendment-3")
	requireWorkDecision(t, approved, jevJudgment)
	requireWorkDecision(t, declined, jevJudgment)
	if !strings.HasPrefix(approved.Str("outcome"), "Approved amendment 2") || !strings.HasPrefix(declined.Str("outcome"), "Rejected amendment 3") || !strings.Contains(approved.Str("rationale"), "Jev agrees.") {
		t.Fatalf("Jev's amendment decisions: %s / %s", approved.Raw, declined.Raw)
	}
	for _, p := range posts {
		if p.Source == "osmia-owner" && p.Str("kind") == "decision" && p.Str("decision") != ownerDecided.Str("decision") {
			t.Fatalf("a decision other than the owner's is on the owner source: %s", p.Raw)
		}
	}
}

// A discovery Jev dispositioned is posted on the work source with no author,
// and says Jev decided it.
func TestPublishJevDiscoveryDispositionSaysJevDecidedIt(t *testing.T) {
	f := newPublisherFixture(t)
	stream := fixturekit.Stream
	id := fmt.Sprintf("discovery-%064x", 7)
	record := func(revision int, w workDiscovery, when time.Time, actor trace.Actor) {
		data, err := json.Marshal(w)
		fixturekit.Must(t, err)
		f.document(stream, id, revision, "discoveries/"+id+".json", string(data), when, actor)
	}
	record(1, workDiscovery{ID: id, Description: "Handle empty uploads", Criterion: "spec#1", Disposition: "open"}, hr(1), managerActor)
	record(2, workDiscovery{ID: id, Description: "Handle empty uploads", Criterion: "spec#1", Disposition: "declined", Reason: "Outside the approved intent.", DecidedBy: trace.DecidedByJev, Judgment: jevJudgment}, hr(2), managerActor)

	posts, _ := f.pass()
	got := fixturekit.Decisions(posts)
	if len(got) != 1 {
		t.Fatalf("%d decisions: %v", len(got), kinds(posts))
	}
	requireWorkDecision(t, got[0], jevJudgment)
	if !strings.Contains(got[0].Str("outcome"), "Declined") || !strings.Contains(got[0].Str("rationale"), "Outside the approved intent.") {
		t.Fatalf("discovery: %s", got[0].Raw)
	}
}

// A spec sealed by a Jev ratification is not posted on the owner source and
// names no owner author; a spec sealed by the owner's ratification still is.
func TestPublishJevRatifiedSealIsInferredWorkContentWithNoAuthor(t *testing.T) {
	f := newPublisherFixture(t)
	owned, jev := fixturekit.Stream, fixturekit.Quiet
	f.recordSeal(owned, 1, "# Owner spec\n\nOwner text.\n")
	f.ratify(owned, false, hr(1))
	f.recordSeal(jev, 1, "# Jev spec\n\nJev text.\n")
	f.ratify(jev, true, hr(1))

	posts, _ := f.pass()
	var documents []fixturekit.HooksPost
	for _, p := range posts {
		if p.Str("kind") == "document" && p.Str("document") != "charter" {
			documents = append(documents, p)
		}
	}
	if len(documents) != 2 {
		t.Fatalf("%d spec documents: %v", len(documents), kinds(posts))
	}
	ownerSpec := postsOf(posts, "document", "document", string(owned)+"-spec")
	if len(ownerSpec) != 1 || ownerSpec[0].Source != "osmia-owner" || ownerSpec[0].Str("author") != "owner" || !strings.Contains(ownerSpec[0].Str("body"), "Owner text.") {
		t.Fatalf("the owner-ratified spec: %+v", ownerSpec)
	}
	jevSpec := postsOf(posts, "document", "body", "# Jev spec\n\nJev text.\n")
	if len(jevSpec) != 1 {
		t.Fatalf("Jev-ratified spec posts: %v", kinds(posts))
	}
	if jevSpec[0].Source != "osmia-work" || jevSpec[0].Str("author") != "" || jevSpec[0].Str("document") != string(jev)+"-jev-spec" || jevSpec[0].Str("document") == string(jev)+"-spec" || !strings.Contains(jevSpec[0].Str("name"), "Jev") {
		t.Fatalf("the Jev-ratified spec: %s on %s", jevSpec[0].Raw, jevSpec[0].Source)
	}
	for _, p := range posts {
		if p.Source == "osmia-owner" && strings.Contains(p.Raw, "Jev text.") {
			t.Fatalf("the owner source holds the Jev-ratified spec: %s", p.Raw)
		}
	}
	if again, _ := f.pass(); len(again) != 0 {
		t.Fatalf("unchanged specs were reposted: %v", kinds(again))
	}
}

// An amendment's reseal belongs to whoever decided the amendment. A plan-only
// amendment the manager decided leaves the spec with whoever ratified
// the seal before it.
func TestPublishSealsFollowTheDeciderOfTheAmendmentThatProducedThem(t *testing.T) {
	f := newPublisherFixture(t)
	stream := fixturekit.Stream
	owner := trace.Actor{Kind: "owner", ID: "local"}
	f.recordSeal(stream, 1, "# Spec\n\nOwner ratified.\n")
	f.ratify(stream, false, hr(1))
	f.amendment(stream, "1", []string{"spec#1"}, "Change one.", "r", "", hr(2))
	f.jevAmendmentDecision(stream, "1", 1, "approve", "Jev approved.", hr(3))
	f.recordSeal(stream, 2, "# Spec\n\nJev approved amendment.\n")
	f.applied(stream, "1", 2, hr(3))
	f.amendment(stream, "2", []string{"plan#upload"}, "Split.", "r", "", hr(4))
	f.amendmentDecision(stream, "2", 1, "approve", "Fine.", hr(5), managerActor)
	f.recordSeal(stream, 3, "# Spec\n\nJev approved amendment.\n")
	f.applied(stream, "2", 3, hr(5))
	f.amendment(stream, "3", []string{"spec#2"}, "Change three.", "r", "", hr(6))
	f.amendmentDecision(stream, "3", 1, "approve", "Owner approved.", hr(7), owner)
	f.recordSeal(stream, 4, "# Spec\n\nOwner approved amendment.\n")
	f.applied(stream, "3", 4, hr(7))

	posts, _ := f.pass()
	ownerSpec := postsOf(posts, "document", "document", string(stream)+"-spec")
	jevSpec := postsOf(posts, "document", "document", string(stream)+"-jev-spec")
	if len(ownerSpec) != 2 || len(jevSpec) != 1 {
		t.Fatalf("owner spec revisions %d, Jev spec revisions %d: %v", len(ownerSpec), len(jevSpec), kinds(posts))
	}
	for _, p := range ownerSpec {
		if p.Source != "osmia-owner" || p.Str("author") != "owner" || strings.Contains(p.Str("body"), "Jev approved") {
			t.Fatalf("owner source spec: %s", p.Raw)
		}
	}
	for _, p := range jevSpec {
		if p.Source != "osmia-work" || p.Str("author") != "" || !strings.Contains(p.Str("body"), "Jev approved") {
			t.Fatalf("work source spec: %s", p.Raw)
		}
	}
}

// What is kept for a cleaned workstream follows the same rules, since kept
// records are computed the way a pass computes them: Jev's decisions and
// Jev-ratified specs stay on the work source with no author.
func TestKeptRecordsOfJevDecisionsStayOnTheWorkSource(t *testing.T) {
	f := newPublisherFixture(t)
	stream := fixturekit.Stream
	f.recordSeal(stream, 1, "# Spec\n\nJev text.\n")
	f.ratify(stream, true, hr(1))
	f.ask(stream, "1", "Q?", hr(1))
	f.escalate(stream, "1", "Q?", hr(2))
	f.ruling(stream, "1", 1, trace.Ruling{Decision: trace.DecisionRuling, OwnerResponse: "Yes.", DecidedBy: trace.DecidedByJev, Judgment: jevJudgment}, hr(3), trace.JevActor)
	f.cleanup(stream)

	kept := f.keptKeys()
	for _, want := range []string{"osmia-work/document/" + string(stream) + "-jev-spec", "osmia-work/decision/" + string(stream) + "-jev-ruling-1"} {
		if !slices.Contains(kept, want) {
			t.Fatalf("%s is not kept: %v", want, kept)
		}
	}
	for _, k := range kept {
		if strings.HasPrefix(k, "osmia-owner/") {
			t.Fatalf("a record of a Jev decision is kept for the owner source: %v", kept)
		}
	}
	posts, _ := f.pass()
	jevSpec := postsOf(posts, "document", "document", string(stream)+"-jev-spec")
	ruling := postsOf(posts, "decision", "decision", string(stream)+"-jev-ruling-1")
	if len(jevSpec) != 1 || jevSpec[0].Source != "osmia-work" || jevSpec[0].Str("author") != "" || len(ruling) != 1 {
		t.Fatalf("kept records posted: %v", kinds(posts))
	}
	requireWorkDecision(t, ruling[0], jevJudgment)
}
