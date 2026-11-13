package service

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/kpenfound/osmia/internal/api"
	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/kb"
	"github.com/kpenfound/osmia/internal/lifecycle"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	"github.com/kpenfound/osmia/internal/trace"
)

func hr(hours int) time.Time { return fixturekit.DemoStart.Add(time.Duration(hours) * time.Hour) }

// document records one revision of a workstream document.
func (f *publisherFixture) document(stream config.WorkstreamID, id string, revision int, path, content string, when time.Time, actor trace.Actor) {
	f.t.Helper()
	h := trace.Header{Schema: "osmia.trace.document", Version: trace.Version, ID: id, Revision: revision, Project: f.repo.Project(), Workstream: stream, At: when, Actor: actor, Cause: "fixture"}
	must(f.t, f.repo.RecordDocuments(context.Background(), []trace.Document{{Header: h, Path: path, Content: content}}))
}

func (f *publisherFixture) header(schema, id string, revision int, stream config.WorkstreamID, when time.Time, actor trace.Actor) trace.Header {
	return trace.Header{Schema: schema, Version: trace.Version, ID: id, Revision: revision, Project: f.repo.Project(), Workstream: stream, At: when, Actor: actor, Cause: "fixture", Depth: 1}
}

// ask records question id as the mason asked it.
func (f *publisherFixture) ask(stream config.WorkstreamID, id, text string, when time.Time) {
	f.t.Helper()
	mason := trace.Actor{Kind: "agent", ID: "mason"}
	must(f.t, f.repo.Append(context.Background(), trace.Question{Header: f.header("osmia.trace.question", id, 1, stream, when, mason), AskedBy: mason, Thread: "thread", Turn: "turn-" + id, Question: text}))
}

// escalate records the revision of question id the chief of staff sent the owner.
func (f *publisherFixture) escalate(stream config.WorkstreamID, id, text string, when time.Time) {
	f.t.Helper()
	mason := trace.Actor{Kind: "agent", ID: "mason"}
	e := &trace.Escalation{Batch: "escalation_" + id, Inbox: 1, Questions: []string{id}, Section: "spec.md: Upload limits", Conflict: "The spec caps uploads at 10 MiB but the plan streams without a cap.",
		Blocked: "The unit waits on the ruling.", Options: []string{"Cap at 10 MiB", "Stream without a cap"}, Recommendation: "Cap at 10 MiB"}
	must(f.t, f.repo.Append(context.Background(), trace.Question{Header: f.header("osmia.trace.question", id, 2, stream, when, chiefActor), AskedBy: mason, Thread: "thread", Turn: "turn-" + id, Question: text, SentToOwner: "Should uploads be capped?", Escalation: e}))
}

func (f *publisherFixture) ruling(stream config.WorkstreamID, id string, revision int, r trace.Ruling, when time.Time, actor trace.Actor) {
	f.t.Helper()
	r.Header = f.header("osmia.trace.ruling", id, revision, stream, when, actor)
	r.QuestionID, r.QuestionRevision = id, 1
	must(f.t, f.repo.Append(context.Background(), r))
}

func decisions(posts []hooksPost) []hooksPost {
	var out []hooksPost
	for _, p := range posts {
		if p.str("kind") == "decision" {
			out = append(out, p)
		}
	}
	return out
}

func decisionOf(t *testing.T, posts []hooksPost, id string) hooksPost {
	t.Helper()
	p := find(posts, "decision", "decision", id)
	if p == nil {
		var ids []string
		for _, d := range decisions(posts) {
			ids = append(ids, d.str("decision"))
		}
		t.Fatalf("no decision %s among %v", id, ids)
	}
	return *p
}

func rejectedOf(p hooksPost) []string {
	var out []string
	list, _ := p.Body["rejected_options"].([]any)
	for _, v := range list {
		out = append(out, fmt.Sprint(v))
	}
	return out
}

func TestPublishOwnerRulingIsAnOwnerDecisionAndTheChiefsRelayIsNot(t *testing.T) {
	f := newPublisherFixture(t)
	stream := fixturekit.Stream
	f.ask(stream, "1", "Is the upload cap 10 MiB or unbounded?", hr(1))
	f.escalate(stream, "1", "Is the upload cap 10 MiB or unbounded?", hr(2))
	owner := trace.Actor{Kind: "owner", ID: "local"}
	f.ruling(stream, "1", 1, trace.Ruling{Decision: trace.DecisionRuling, OwnerResponse: "Cap uploads at 10 MiB."}, hr(3), owner)
	f.pass()

	const relayed = "RELAYED-TO-THE-ASKERS"
	f.ruling(stream, "1", 2, trace.Ruling{Decision: trace.DecisionRuling, OwnerResponse: "Cap uploads at 10 MiB.", ReturnedAnswer: relayed, Scope: trace.ScopeLocal}, hr(4), chiefActor)
	posts, _ := f.pass()
	if len(posts) != 0 {
		t.Fatalf("the relay of the ruling posted %v", kinds(posts))
	}

	f.hooks.take()
	f.reopen()
	// Backfill after the digests are gone: exactly the owner's ruling.
	posted, err := f.repo.HearsayPosted()
	must(t, err)
	var cleared []trace.PostedRecord
	for _, p := range posted {
		p.Digest = ""
		cleared = append(cleared, p)
	}
	must(t, f.repo.RecordHearsayPosted(context.Background(), cleared, hr(5)))
	posts, _ = f.pass()
	rulings := decisions(posts)
	if len(rulings) != 1 {
		t.Fatalf("%d decisions, want the owner's ruling alone: %v", len(rulings), kinds(posts))
	}
	d := rulings[0]
	if d.Source != "osmia-owner" || d.str("author") != "owner" || d.str("decider") != "owner" || d.str("workstream") != string(stream) {
		t.Fatalf("ruling: %s on %s", d.Raw, d.Source)
	}
	if d.str("decision") != string(stream)+"-ruling-1" || d.str("question") != string(stream)+":1" {
		t.Fatalf("ruling ids: %s", d.Raw)
	}
	if d.str("outcome") != "Cap uploads at 10 MiB." {
		t.Fatalf("outcome: %s", d.Raw)
	}
	for _, want := range []string{"Should uploads be capped?", "spec.md: Upload limits", "The spec caps uploads at 10 MiB but the plan streams without a cap.", "- Cap at 10 MiB", "- Stream without a cap", "The unit waits on the ruling."} {
		if !strings.Contains(d.str("context"), want) {
			t.Fatalf("context lacks %q: %s", want, d.str("context"))
		}
	}
	if d.str("rationale") == "" || d.str("created_at") == "" {
		t.Fatalf("ruling: %s", d.Raw)
	}
	for _, p := range posts {
		if strings.Contains(p.Raw, relayed) {
			t.Fatalf("a post carries the chief's relay: %s", p.Raw)
		}
	}
}

func TestPublishChiefAnswersAreDecisionsUnlessTheyCiteCode(t *testing.T) {
	f := newPublisherFixture(t)
	stream := fixturekit.Stream
	for i, text := range []string{"Is resume in scope?", "Which function retries?", "Does the cap apply to retries?"} {
		id := fmt.Sprint(i + 1)
		f.ask(stream, id, text, hr(1+i))
	}
	f.ruling(stream, "1", 1, trace.Ruling{Decision: trace.DecisionAnswer, ReturnedAnswer: "Resume is in scope.", Citations: []string{"spec#1", "charter#1"}}, hr(5), chiefActor)
	f.ruling(stream, "2", 1, trace.Ruling{Decision: trace.DecisionAnswer, ReturnedAnswer: "CODE-ANSWER retry lives in client.go.", Citations: []string{"inspection#inspection-abc"}}, hr(6), chiefActor)
	f.ruling(stream, "3", 1, trace.Ruling{Decision: trace.DecisionAnswer, ReturnedAnswer: "MIXED-ANSWER the cap applies.", Citations: []string{"spec#1", "inspection#inspection-def"}}, hr(7), chiefActor)

	posts, _ := f.pass()
	got := decisions(posts)
	if len(got) != 1 {
		t.Fatalf("%d decisions: %v", len(got), kinds(posts))
	}
	d := got[0]
	if d.Source != "osmia-work" || d.str("author") != "" || d.str("decider") != "chief_of_staff" || d.str("outcome") != "Resume is in scope." || d.str("decision") != string(stream)+"-answer-1" {
		t.Fatalf("answer: %s on %s", d.Raw, d.Source)
	}
	if !strings.Contains(d.str("rationale"), "charter#1") || !strings.Contains(d.str("context"), "Is resume in scope?") || d.str("question") != string(stream)+":1" {
		t.Fatalf("answer: %s", d.Raw)
	}
	for _, p := range posts {
		if strings.Contains(p.Raw, "CODE-ANSWER") || strings.Contains(p.Raw, "MIXED-ANSWER") {
			t.Fatalf("a code answer was posted: %s", p.Raw)
		}
	}
}

func TestPublishLoggedAssumptionIsAnInferredChiefDecision(t *testing.T) {
	f := newPublisherFixture(t)
	stream := fixturekit.Stream
	f.recordSeal(stream, 1, "# Upload limits\n\nCap uploads.\n")
	content := fmt.Sprintf(`{"version":1,"assumptions":[{"workstreams":[%q],"id":"1","text":"Treat the 10 MiB cap as inclusive.","actor":{"kind":"agent","id":"chief_of_staff"},"thread":"chief","turn":"t1","at":%q}]}`, string(stream), hr(2).Format(time.RFC3339))
	f.document(stream, trace.AssumptionsDocument, 1, trace.AssumptionsPath, content, hr(2), chiefActor)

	posts, _ := f.pass()
	got := decisions(posts)
	if len(got) != 1 {
		t.Fatalf("%d decisions: %v", len(got), kinds(posts))
	}
	d := got[0]
	if d.Source != "osmia-work" || d.str("author") != "" || d.str("decider") != "chief_of_staff" || d.str("outcome") != "Treat the 10 MiB cap as inclusive." || d.str("workstream") != string(stream) {
		t.Fatalf("assumption: %s on %s", d.Raw, d.Source)
	}
	if d.str("spec_document") != string(stream)+"-spec" || d.str("context") == "" || d.str("rationale") == "" {
		t.Fatalf("assumption carries its context, rationale and spec link: %s", d.Raw)
	}
	if again, _ := f.pass(); len(again) != 0 {
		t.Fatalf("an unchanged assumption was reposted: %v", kinds(again))
	}
}

func TestPublishCharterProposalOutcomesAreOwnerDecisions(t *testing.T) {
	f := newPublisherFixture(t)
	stream := fixturekit.Stream
	proposal := func(question, rule, decision, note string, charter int) string {
		p := trace.CharterProposal{Question: question, Ruling: "workstreams/x/questions/" + question + "/rulings.jsonl", RulingRevision: 1, OwnerResponse: "Every upload must resume.", Rule: rule, Number: 2, Decision: decision, Note: note, Charter: charter}
		data, err := json.Marshal(p)
		must(t, err)
		return string(data)
	}
	f.document(stream, trace.CharterProposalID("1"), 1, trace.CharterProposalPath("1"), proposal("1", "Uploads resume after a restart.", "", "", 0), hr(1), chiefActor)
	f.document(stream, trace.CharterProposalID("2"), 1, trace.CharterProposalPath("2"), proposal("2", "Never cap uploads.", "", "", 0), hr(1), chiefActor)
	f.document(stream, trace.CharterProposalID("3"), 1, trace.CharterProposalPath("3"), proposal("3", "Still waiting.", "", "", 0), hr(1), chiefActor)
	owner := trace.Actor{Kind: "owner", ID: "local"}
	f.document(stream, trace.CharterProposalID("1"), 2, trace.CharterProposalPath("1"), proposal("1", "Uploads resume after a restart.", trace.CharterRatify, "Yes, state it as a rule.", 0), hr(2), owner)
	f.document(stream, trace.CharterProposalID("2"), 2, trace.CharterProposalPath("2"), proposal("2", "Never cap uploads.", trace.CharterDecline, "Too broad.", 0), hr(2), owner)

	posts, _ := f.pass()
	got := decisions(posts)
	if len(got) != 2 {
		t.Fatalf("%d decisions: %v", len(got), kinds(posts))
	}
	ratified, declined := decisionOf(t, posts, string(stream)+"-charter-1"), decisionOf(t, posts, string(stream)+"-charter-2")
	for _, d := range []hooksPost{ratified, declined} {
		if d.Source != "osmia-owner" || d.str("author") != "owner" || d.str("decider") != "owner" || d.str("workstream") != "" {
			t.Fatalf("charter decision: %s on %s", d.Raw, d.Source)
		}
		if !strings.Contains(d.str("context"), "Every upload must resume.") {
			t.Fatalf("the context lacks the owner's ruling: %s", d.Raw)
		}
	}
	if !strings.HasPrefix(ratified.str("outcome"), "Ratified") || !strings.Contains(ratified.str("outcome"), "Uploads resume after a restart.") || !strings.Contains(ratified.str("rationale"), "Yes, state it as a rule.") {
		t.Fatalf("ratified: %s", ratified.Raw)
	}
	if !strings.HasPrefix(declined.str("outcome"), "Declined") || !strings.Contains(declined.str("outcome"), "Never cap uploads.") || !strings.Contains(declined.str("rationale"), "Too broad.") {
		t.Fatalf("declined: %s", declined.Raw)
	}
	// Recording the ratified rule in the charter changes nothing posted.
	f.document(stream, trace.CharterProposalID("1"), 3, trace.CharterProposalPath("1"), proposal("1", "Uploads resume after a restart.", trace.CharterRatify, "Yes, state it as a rule.", 4), hr(3), chiefActor)
	if again, _ := f.pass(); len(again) != 0 {
		t.Fatalf("chartering the rule reposted %v", kinds(again))
	}
}

func (f *publisherFixture) amendment(stream config.WorkstreamID, id string, citations []string, change, reason, question string, when time.Time) {
	f.t.Helper()
	mason := trace.Actor{Kind: "agent", ID: "mason"}
	must(f.t, f.repo.Append(context.Background(), trace.Amendment{Header: f.header("osmia.trace.amendment", id, 1, stream, when, mason), Requester: mason, Role: "mason", Thread: "thread", Turn: "turn-a" + id,
		QuestionID: question, Citations: citations, Change: change, Reason: reason, Seal: 1, SealRevision: 1, SpecHash: "hash"}))
}

func (f *publisherFixture) amendmentDecision(stream config.WorkstreamID, id string, revision int, verdict, note string, when time.Time, actor trace.Actor) {
	f.t.Helper()
	data, err := json.Marshal(api.AmendmentDecision{Decision: verdict, Note: note, Round: 1, Packet: 1, Spec: 2, Plan: 2, SealRevision: 1, Actor: actor.ID})
	must(f.t, err)
	f.document(stream, "amendment_"+id+"_decision", revision, "amendments/"+id+"/decision.json", string(data), when, actor)
}

func TestPublishAmendmentDecisionsRouteByWhoDecided(t *testing.T) {
	f := newPublisherFixture(t)
	stream := fixturekit.Stream
	f.recordSeal(stream, 1, "# Upload limits\n\nCap uploads.\n")
	owner := trace.Actor{Kind: "owner", ID: "local"}
	f.amendment(stream, "1", []string{"spec#1"}, "Allow 20 MiB uploads.", "The partner needs it.", "", hr(2))
	f.amendmentDecision(stream, "1", 1, "round", "ROUND-NOTE one more debate round", hr(3), owner)
	f.amendmentDecision(stream, "1", 2, "approve", "Go ahead.", hr(4), owner)
	f.amendment(stream, "2", []string{"plan#upload"}, "Split the upload unit.", "It is too large.", "", hr(5))
	f.amendmentDecision(stream, "2", 1, "approve", "The revised assignments still fulfil the spec.", hr(6), chiefActor)
	f.amendment(stream, "3", []string{"spec#1"}, "Allow 50 MiB uploads.", "More headroom.", "", hr(7))
	f.amendmentDecision(stream, "3", 1, "approve", "Agreed.", hr(8), owner)
	f.amendment(stream, "4", []string{"spec#2"}, "UNDECIDED-REQUEST change", "no decision yet", "", hr(9))
	f.amendment(stream, "5", []string{"spec#3"}, "RETIRED-REQUEST change", "stale", "", hr(9))
	f.amendmentDecision(stream, "5", 1, "reject", "SERVICE-RETIREMENT", hr(10), trace.Actor{Kind: "service", ID: "amendments"})

	posts, _ := f.pass()
	got := decisions(posts)
	if len(got) != 3 {
		t.Fatalf("%d decisions: %v", len(got), kinds(posts))
	}
	ownerDecided, planOnly, revisited := decisionOf(t, posts, string(stream)+"-amendment-1"), decisionOf(t, posts, string(stream)+"-amendment-2"), decisionOf(t, posts, string(stream)+"-amendment-3")
	if ownerDecided.Source != "osmia-owner" || ownerDecided.str("author") != "owner" || ownerDecided.str("decider") != "owner" {
		t.Fatalf("owner-decided amendment: %s on %s", ownerDecided.Raw, ownerDecided.Source)
	}
	for _, want := range []string{"Allow 20 MiB uploads.", "The partner needs it.", "spec#1"} {
		if !strings.Contains(ownerDecided.str("context"), want) {
			t.Fatalf("context lacks %q: %s", want, ownerDecided.Raw)
		}
	}
	if !strings.HasPrefix(ownerDecided.str("outcome"), "Approved amendment 1") || ownerDecided.str("rationale") != "Go ahead." || len(rejectedOf(ownerDecided)) == 0 || ownerDecided.str("spec_document") != string(stream)+"-spec" {
		t.Fatalf("owner-decided amendment: %s", ownerDecided.Raw)
	}
	if planOnly.Source != "osmia-work" || planOnly.str("author") != "" || planOnly.str("decider") != "chief_of_staff" || planOnly.str("supersedes") != "" {
		t.Fatalf("plan-only amendment: %s on %s", planOnly.Raw, planOnly.Source)
	}
	// A later approved amendment about the same criterion revisits the earlier decision.
	if revisited.str("supersedes") != ownerDecided.str("decision") || ownerDecided.str("supersedes") != "" {
		t.Fatalf("supersedes: %s / %s", revisited.Raw, ownerDecided.Raw)
	}
	ordered := kinds(posts)
	for _, p := range posts {
		for _, never := range []string{"ROUND-NOTE", "UNDECIDED-REQUEST", "RETIRED-REQUEST", "SERVICE-RETIREMENT"} {
			if strings.Contains(p.Raw, never) {
				t.Fatalf("a post carries %s: %s", never, p.Raw)
			}
		}
	}
	// The superseded decision is posted before the one that names it.
	first := slices.IndexFunc(posts, func(p hooksPost) bool { return p.str("decision") == ownerDecided.str("decision") })
	second := slices.IndexFunc(posts, func(p hooksPost) bool { return p.str("decision") == revisited.str("decision") })
	if first < 0 || second < 0 || first > second {
		t.Fatalf("order %d %d: %v", first, second, ordered)
	}
}

func TestPublishScopeCallsAndAcceptedFindings(t *testing.T) {
	f := newPublisherFixture(t)
	stream := fixturekit.Stream
	owner := trace.Actor{Kind: "owner", ID: "local"}
	discovery := func(n int) string { return fmt.Sprintf("discovery-%064x", n) }
	record := func(id string, revision int, disposition, reason, unit string, when time.Time) {
		data, err := json.Marshal(workDiscovery{ID: id, Description: "Handle empty uploads", Criterion: "spec#1", Disposition: disposition, Unit: unit, Reason: reason})
		must(t, err)
		f.document(stream, id, revision, "discoveries/"+id+".json", string(data), when, chiefActor)
	}
	record(discovery(1), 1, "open", "", "", hr(1))
	record(discovery(1), 2, "declined", "Outside the approved intent.", "", hr(2))
	record(discovery(1), 3, "suggestion", "Worth doing after delivery.", "", hr(3))
	record(discovery(2), 1, "open", "", "", hr(1))
	record(discovery(2), 2, "assigned", "Needed for spec#1.", "upload", hr(2))
	record(discovery(3), 1, "open", "", "", hr(1))

	approval, err := json.Marshal(api.DeliveryApproval{Review: 2, Commit: "tip", At: hr(6), AcceptedBlocking: []string{"spec#2: the retry cap is not enforced"}})
	must(t, err)
	f.document(stream, "final-delivery", 1, "final/delivery.json", string(approval), hr(6), owner)
	followups := `[{"review":1,"report":1,"criterion":"spec#3","gap":"Resume is not tested after a restart","unit":{"id":"final-1"}},{"review":0,"criterion":"spec#1","gap":"Update dependency references","unit":{"id":"deps-1"}},{"review":1,"amendment":"2","criterion":"spec#1","gap":"AMENDMENT-FOLLOWUP","unit":{"id":"amend-1"}}]`
	f.document(stream, "final-followups", 1, "final/followups.json", followups, hr(5), trace.Actor{Kind: "service", ID: "final-review"})

	posts, _ := f.pass()
	got := decisions(posts)
	if len(got) != 4 {
		t.Fatalf("%d decisions: %v", len(got), kinds(posts))
	}
	declined := decisionOf(t, posts, string(stream)+"-discovery-"+shortHash(discovery(1))+"-2")
	deferred := decisionOf(t, posts, string(stream)+"-discovery-"+shortHash(discovery(1))+"-3")
	for _, d := range []hooksPost{declined, deferred} {
		if d.Source != "osmia-work" || d.str("author") != "" || d.str("decider") != "chief_of_staff" || d.str("workstream") != string(stream) {
			t.Fatalf("scope call: %s on %s", d.Raw, d.Source)
		}
	}
	if declined.str("rationale") != "Outside the approved intent." || !strings.HasPrefix(declined.str("outcome"), "Declined") || !strings.Contains(declined.str("context"), "Handle empty uploads") {
		t.Fatalf("declined: %s", declined.Raw)
	}
	if deferred.str("rationale") != "Worth doing after delivery." || !strings.Contains(deferred.str("outcome"), "Deferred") || deferred.str("supersedes") != declined.str("decision") {
		t.Fatalf("deferred: %s", deferred.Raw)
	}
	accepted := decisions(posts)
	var acceptedPost *hooksPost
	for i := range accepted {
		if accepted[i].Source == "osmia-owner" {
			acceptedPost = &accepted[i]
		}
	}
	if acceptedPost == nil || acceptedPost.str("author") != "owner" || acceptedPost.str("decider") != "owner" || !strings.Contains(acceptedPost.str("outcome"), "the retry cap is not enforced") || len(rejectedOf(*acceptedPost)) == 0 {
		t.Fatalf("accepted finding: %+v", acceptedPost)
	}
	later := decisionOf(t, posts, string(stream)+"-followup-final-1")
	if later.Source != "osmia-work" || later.str("author") != "" || later.str("decider") != "chief_of_staff" || !strings.Contains(later.str("context"), "Resume is not tested after a restart") || later.str("rationale") == "" {
		t.Fatalf("follow-up: %s on %s", later.Raw, later.Source)
	}
	for _, p := range posts {
		if strings.Contains(p.Raw, "AMENDMENT-FOLLOWUP") || strings.Contains(p.Raw, "Update dependency references") {
			t.Fatalf("a follow-up outside the final review was posted: %s", p.Raw)
		}
	}
	if again, _ := f.pass(); len(again) != 0 {
		t.Fatalf("unchanged decisions were reposted: %v", kinds(again))
	}
}

func TestPublishDecisionsWaitForAnOutageAndAreNotRepeated(t *testing.T) {
	f := newPublisherFixture(t)
	stream := fixturekit.Stream
	f.ask(stream, "1", "Is the cap per file?", hr(1))
	f.ruling(stream, "1", 1, trace.Ruling{Decision: trace.DecisionAnswer, ReturnedAnswer: "Per file.", Citations: []string{"spec#1"}}, hr(2), chiefActor)
	content := fmt.Sprintf(`{"version":1,"assumptions":[{"workstreams":[%q],"id":"1","text":"Count bytes, not characters.","actor":{"kind":"agent","id":"chief_of_staff"},"thread":"chief","turn":"t1","at":%q}]}`, string(stream), hr(3).Format(time.RFC3339))
	f.document(stream, trace.AssumptionsDocument, 1, trace.AssumptionsPath, content, hr(3), chiefActor)

	f.hooks.setStatus(http.StatusServiceUnavailable)
	res, err := f.s.publishPass(context.Background(), f.repo, f.s.current())
	must(t, err)
	if res.Posts != 0 {
		t.Fatalf("pass during the outage: %+v", res)
	}
	f.hooks.setStatus(http.StatusAccepted)
	posts, _ := f.pass()
	if len(decisions(posts)) != 2 {
		t.Fatalf("recovery posted %v", kinds(posts))
	}
	if again, _ := f.pass(); len(again) != 0 {
		t.Fatalf("after recovery: %v", kinds(again))
	}
	// A restart reposts nothing and keeps the ids.
	f.reopen()
	if again, _ := f.pass(); len(again) != 0 {
		t.Fatalf("after a restart: %v", kinds(again))
	}
}

func TestPublishPostsNothingForDecisionsOfTheLibrarianOrSessions(t *testing.T) {
	f := newPublisherFixture(t)
	librarian := librarianWorkstream(f.repo.Project())
	must(t, f.repo.CreateWorkstream(context.Background(), librarian, hr(1), registrationActor))
	f.ask(librarian, "1", "Librarian question?", hr(2))
	f.ruling(librarian, "1", 1, trace.Ruling{Decision: trace.DecisionAnswer, ReturnedAnswer: "LIBRARIAN-ANSWER", Citations: []string{"spec#1"}}, hr(3), chiefActor)
	posts, _ := f.pass()
	for _, p := range posts {
		if strings.Contains(p.Raw, "LIBRARIAN-ANSWER") || p.str("workstream") == string(librarian) {
			t.Fatalf("the librarian's workstream was posted: %s", p.Raw)
		}
	}
}

func TestDecisionIDsAreDerivedFromOsmiaIDsAndFallBackToAHash(t *testing.T) {
	if got := decisionID("w_abc", "ruling", "1"); got != "w_abc-ruling-1" {
		t.Fatalf("id: %s", got)
	}
	long := decisionID(strings.Repeat("w", 120), "ruling", "1")
	if !strings.HasPrefix(long, "d-") || long != decisionID(strings.Repeat("w", 120), "ruling", "1") || len(long) > 128 {
		t.Fatalf("long id: %s", long)
	}
	if decisionID("a:b", "c") == decisionID("a:b", "d") {
		t.Fatal("hashed ids collide")
	}
}

func testEntities(entities ...kb.Entity) kb.Map {
	return kb.Map{Version: kb.Version, Entities: entities}
}

func entity(id, name, path string, parents ...string) kb.Entity {
	return kb.Entity{ID: id, Name: name, Aliases: []string{}, Paths: []string{path}, Owners: []string{}, PartOf: append([]string{}, parents...)}
}

func TestDiffMapFindsOnlyStructuralChanges(t *testing.T) {
	base := testEntities(entity("api", "API", "internal/api/**"), entity("store", "Store", "internal/store/**", "api"), entity("old", "Old", "internal/old/**"), entity("moveme", "Move me", "internal/m/**", "api"), entity("same", "Same", "internal/same/**"))
	changed := testEntities(
		entity("api", "API", "internal/api/**"),
		// The store gains an alias, an owner, a path and a note-bearing edit: not structure.
		kb.Entity{ID: "store", Name: "Store", Aliases: []string{"storage"}, Paths: []string{"internal/store/**", "internal/store2/**"}, Owners: []string{"@owner"}, PartOf: []string{"api"}},
		entity("moveme", "Move me", "internal/m/**", "store"),
		entity("same-renamed", "Same", "internal/same/**"),
		entity("fresh", "Fresh", "internal/fresh/**", "api"),
	)
	got := map[string]string{}
	for _, c := range diffMap(base, changed) {
		got[c.kind+":"+c.id] = c.text()
	}
	for _, want := range []string{"removed:old", "renamed:same-renamed", "added:fresh", "moved:moveme"} {
		if got[want] == "" {
			t.Fatalf("no %s among %v", want, got)
		}
	}
	if len(got) != 4 {
		t.Fatalf("changes: %v", got)
	}
	if !strings.Contains(got["renamed:same-renamed"], "same") || !strings.Contains(got["moved:moveme"], "Store") {
		t.Fatalf("texts: %v", got)
	}
	// A new name under the same id is a rename; unchanged maps change nothing.
	renamed := testEntities(entity("api", "Public API", "internal/api/**"), base.Entities[1], base.Entities[2], base.Entities[3], base.Entities[4])
	if got := diffMap(base, renamed); len(got) != 1 || got[0].kind != mapRenamed || got[0].id != "api" {
		t.Fatalf("name change: %+v", got)
	}
	if got := diffMap(base, base); len(got) != 0 {
		t.Fatalf("no change: %+v", got)
	}
	// Renaming a parent does not move its children.
	parent := testEntities(entity("api-v2", "API", "internal/api/**"), entity("store", "Store", "internal/store/**", "api-v2"), base.Entities[2], base.Entities[3], base.Entities[4])
	parent.Entities[3].PartOf = []string{"api-v2"}
	got2 := diffMap(base, parent)
	if len(got2) != 1 || got2[0].kind != mapRenamed || got2[0].oldID != "api" {
		t.Fatalf("parent rename: %+v", got2)
	}
}

func (f *publisherFixture) storeMap(m kb.Map, when time.Time, cause string) {
	f.t.Helper()
	must(f.t, kb.Store(context.Background(), f.repo, m, when, trace.Actor{Kind: "agent", ID: librarianAgent}, cause))
}

func TestPublishSubsystemMapChangesAreDatedUpdatesOnTheProject(t *testing.T) {
	f := newPublisherFixture(t)
	f.storeMap(testEntities(entity("api", "API", "internal/api/**")), hr(1), "fixture")
	f.pass()

	// Notes, aliases, paths and the source ledger change nothing posted.
	f.storeMap(testEntities(kb.Entity{ID: "api", Name: "API", Aliases: []string{"rest"}, Paths: []string{"internal/api/**", "cmd/api/**"}, Owners: []string{"@owner"}, PartOf: []string{}}), hr(2), "fixture")
	f.document("", kb.ProseDocument("api"), 1, trace.ProsePath("api"), "A note about the API.\n", hr(2), trace.Actor{Kind: "agent", ID: librarianAgent})
	f.document("", "kb-sources", 1, "kb/sources.json", "[]\n", hr(2), trace.Actor{Kind: "agent", ID: librarianAgent})
	if posts, _ := f.pass(); len(posts) != 0 {
		t.Fatalf("a non-structural edit posted %v", kinds(posts))
	}

	f.storeMap(testEntities(entity("api", "API", "internal/api/**"), entity("store", "Store", "internal/store/**", "api")), hr(3), "fixture")
	posts, _ := f.pass()
	if len(posts) != 1 {
		t.Fatalf("an added subsystem posted %v", kinds(posts))
	}
	u := posts[0]
	if u.str("kind") != "update" || u.Source != "osmia-work" || u.str("author") != "" || u.str("workstream") != "" || u.str("project") != string(f.repo.Project()) {
		t.Fatalf("update: %s on %s", u.Raw, u.Source)
	}
	if !strings.Contains(u.str("text"), "Store") || !strings.Contains(u.str("text"), "Caused by") || u.str("created_at") != hearsayTime(hr(3)) || u.str("update") == "" {
		t.Fatalf("update: %s", u.Raw)
	}
	for _, field := range []string{"state", "status"} {
		if _, has := u.Body[field]; has {
			t.Fatalf("an update carries %s: %s", field, u.Raw)
		}
	}
	if strings.Contains(u.Raw, "@owner") || strings.Contains(u.Raw, "cmd/api") || strings.Contains(u.Raw, "A note about the API") {
		t.Fatalf("an update carries more than structure: %s", u.Raw)
	}
	if again, _ := f.pass(); len(again) != 0 {
		t.Fatalf("unchanged updates were reposted: %v", kinds(again))
	}

	// One update per change: a removal and a rename in one revision.
	f.storeMap(testEntities(entity("api-service", "API", "internal/api/**")), hr(4), "fixture")
	posts, _ = f.pass()
	if len(posts) != 2 {
		t.Fatalf("a removal and a rename posted %v", kinds(posts))
	}
}

func hearsayTime(t time.Time) string { return t.UTC().Format(time.RFC3339Nano) }

func TestPublishSubsystemMapUpdatesNameTheCausingWorkstream(t *testing.T) {
	f := newPublisherFixture(t)
	f.storeMap(testEntities(entity("api", "API", "internal/api/**")), hr(1), "fixture")
	f.storeMap(testEntities(entity("api", "API", "internal/api/**"), entity("store", "Store", "internal/store/**")), hr(2), "operation-refresh")

	in, err := collectPublishInputs(context.Background(), f.repo, f.s.current())
	must(t, err)
	input, err := json.Marshal(refreshInput{Workstream: fixturekit.Stream, Commit: strings.Repeat("a", 40)})
	must(t, err)
	in.operations = map[string]coreadapter.Operation{"operation-refresh": {ID: "operation-refresh", Action: RefreshAction, Input: input}}
	storeUpdate := func() string {
		for _, r := range mapUpdateRecords(in) {
			if text := r.Versions[0].Text; strings.Contains(text, `"Store"`) {
				return text
			}
		}
		t.Fatal("no update for the added subsystem")
		return ""
	}
	if text := storeUpdate(); !strings.Contains(text, "Caused by workstream "+string(fixturekit.Stream)) {
		t.Fatalf("the update does not name the workstream: %s", text)
	}
	extract, err := json.Marshal(extractionInput{Extraction: 3})
	must(t, err)
	in.operations = map[string]coreadapter.Operation{"operation-refresh": {ID: "operation-refresh", Action: ExtractAction, Input: extract}}
	if text := storeUpdate(); !strings.Contains(text, "knowledge-base extraction 3") {
		t.Fatalf("extraction: %s", text)
	}
}

// groupInputs is the view one member project has of a workgroup's decisions.
func groupInputs(project config.ProjectID, g trace.PublishedWorkgroup) *publishInputs {
	return &publishInputs{owner: "owner", workSource: "osmia-work", ownerSource: "osmia-owner", project: project,
		streams: []*publishStream{{status: trace.WorkstreamStatus{Workstream: "w_member"}, group: &g, approved: true, decisionsRead: true}}}
}

func TestPublishWorkgroupAmendmentRulingsArePostedOnEveryMemberProject(t *testing.T) {
	requester := &trace.ProjectAmendment{ID: "1", Workgroup: "g_one", Origin: "w_member", QuestionID: "4", Citations: []string{"spec#2"}, Change: "Change the retry contract.", Reason: "The client retries twice."}
	change := func(id, state string, decision *lifecycle.ChangeDecision, request *trace.ProjectAmendment, text string) trace.Document {
		c := lifecycle.WorkgroupChange{ID: id, Workgroup: "g_one", Origin: "w_member", Requester: request, Host: "w_host", Request: lifecycle.ChangeRequest{Key: id, Kind: "amend", Scope: "workgroup", Change: text}, State: state, Decision: decision}
		data, err := json.Marshal(c)
		must(t, err)
		return trace.Document{Header: trace.Header{ID: lifecycle.WorkgroupChangeDocument(id), Revision: map[string]int{"presented": 1, "approved": 2}[state], At: fixturekit.DemoStart.Add(time.Hour * time.Duration(len(state)))}, Content: string(data)}
	}
	approve := &lifecycle.ChangeDecision{Revision: 1, Decision: "approve"}
	g := trace.PublishedWorkgroup{ID: "g_one", Members: []config.WorkstreamID{"w_member", "w_other"}, Changes: []trace.Document{
		change("c1", "presented", nil, requester, "Change the retry contract."),
		change("c1", "approved", approve, requester, "Change the retry contract."),
		change("c2", "presented", nil, nil, "UNDECIDED-CHANGE"),
	}}
	var ids []string
	for _, project := range []config.ProjectID{"p_one", "p_two"} {
		in := groupInputs(project, g)
		records := decisionRecords(in)
		if len(records) != 1 {
			t.Fatalf("%s: %+v", project, records)
		}
		r := records[0]
		w := r.Versions[0]
		if r.Source != "osmia-owner" || w.Author != "owner" || w.Decider != "owner" || w.Workstream != "" || w.Project != string(project) || w.SpecDocument != "g_one-project-spec" || w.Question != "w_member:4" {
			t.Fatalf("%s: %+v on %s", project, w, r.Source)
		}
		if !strings.Contains(w.Outcome, "Approved the project spec amendment") || !strings.Contains(w.Context, "The client retries twice.") || len(w.RejectedOptions) == 0 || w.Rationale == "" {
			t.Fatalf("%s: %+v", project, w)
		}
		if strings.Contains(w.Context+w.Outcome, "UNDECIDED-CHANGE") {
			t.Fatalf("an undecided change was posted: %+v", w)
		}
		ids = append(ids, w.Decision)
	}
	// Both member projects derive the same id.
	if ids[0] != ids[1] || ids[0] != "g_one-change-c1" {
		t.Fatalf("ids: %v", ids)
	}
}

func TestPublishWorkgroupAmendmentRevisitsAnEarlierRuling(t *testing.T) {
	requester := &trace.ProjectAmendment{ID: "1", Workgroup: "g_one", Origin: "w_member", Citations: []string{"spec#2"}, Change: "x", Reason: "y"}
	doc := func(id string, rev int, at time.Time, decision string) trace.Document {
		c := lifecycle.WorkgroupChange{ID: id, Workgroup: "g_one", Origin: "w_member", Requester: requester, Request: lifecycle.ChangeRequest{Kind: "amend", Change: "change " + id}, State: "approved", Decision: &lifecycle.ChangeDecision{Revision: 1, Decision: decision}}
		data, err := json.Marshal(c)
		must(t, err)
		return trace.Document{Header: trace.Header{ID: lifecycle.WorkgroupChangeDocument(id), Revision: rev, At: at}, Content: string(data)}
	}
	g := trace.PublishedWorkgroup{ID: "g_one", Members: []config.WorkstreamID{"w_member"}, Changes: []trace.Document{doc("later", 1, hr(5), "approve"), doc("earlier", 1, hr(2), "approve")}}
	records := decisionRecords(groupInputs("p_one", g))
	if len(records) != 2 {
		t.Fatalf("%+v", records)
	}
	earlier, later := records[0], records[1]
	if earlier.Versions[0].Decision != "g_one-change-earlier" || later.Versions[0].Supersedes != earlier.Versions[0].Decision || earlier.Versions[0].Supersedes != "" {
		t.Fatalf("supersedes: %+v / %+v", earlier.Versions[0], later.Versions[0])
	}
	// The earlier decision must be held first.
	if !slices.Contains(later.After, earlier.Ref()) {
		t.Fatalf("after: %v", later.After)
	}
}
