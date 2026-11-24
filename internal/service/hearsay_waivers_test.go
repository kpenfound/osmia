package service

import (
	"context"
	"encoding/json"
	"github.com/kpenfound/osmia/internal/servicetest/intakefixture"
	"strings"
	"testing"
	"time"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/lifecycle"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	"github.com/kpenfound/osmia/internal/trace"
)

func recordWaiver(t *testing.T, f *publisherFixture, waiver lifecycle.EdgeWaiver, routing trace.Routing) {
	t.Helper()
	ctx := context.Background()
	data, err := json.Marshal(lifecycle.EdgeWaivers{Waivers: []lifecycle.EdgeWaiver{waiver}})
	intakefixture.Must(t, err)
	doc := trace.Document{Header: trace.Header{Schema: "osmia.trace.document", Version: trace.Version, ID: lifecycle.EdgeWaiverDocument, Revision: 1, Project: f.repo.Project(), Workstream: fixturekit.Stream, At: waiver.At, Actor: fixturekit.OwnerActor, Cause: "fixture"}, Path: lifecycle.EdgeWaiverPath, Content: string(data)}
	subject := lifecycle.DependencySubject(waiver.Upstream, waiver.Type)
	h := trace.Header{Schema: "osmia.trace.transition", Version: trace.Version, ID: subject + "-1", Revision: 1, Project: f.repo.Project(), Workstream: fixturekit.Stream, At: waiver.At, Actor: trace.Actor{Kind: "service", ID: "foreman"}, Cause: "upstream-abandoned"}
	tx := trace.Transaction{ExpectedVersion: 0, Transition: trace.Transition{Header: h, Subject: subject, To: lifecycle.DependencyWaived, Reason: waiver.Reason, DecidedBy: routing.DecidedBy, Judgment: routing.Judgment, DefaultTo: routing.DefaultTo}}
	_, err = f.repo.RecordDocumentsWith(ctx, []trace.Document{doc}, tx)
	intakefixture.Must(t, err)
}

func TestAWaiverIsPostedToHearsayOnceAsJevsDecision(t *testing.T) {
	f := newPublisherFixture(t)
	fixturekit.RecordHanded(t, f.repo, fixturekit.Stream, "stdin", fixturekit.DemoStart)
	at := fixturekit.DemoStart.Add(time.Minute)
	upstream := "w_00000000000000000000000000000a01"
	recordWaiver(t, f, lifecycle.EdgeWaiver{Upstream: config.WorkstreamID(upstream), Type: "hard_block", Ended: "abandoned", Reason: "the dependency on w_a01 was dropped: its upstream ended (abandoned)", DecidedBy: trace.DecidedByJev, Judgment: jevJudgment, Trigger: "dep.upstream_terminal", At: at},
		trace.Routing{DecidedBy: trace.DecidedByJev, Judgment: jevJudgment, DefaultTo: lifecycle.DependencyKept})

	posts, _ := f.pass()
	var waivers []hooksPost
	for _, d := range decisions(posts) {
		if strings.Contains(d.str("decision"), "-jev-route-") {
			t.Fatalf("the drop was posted as a departure too: %s", d.Raw)
		}
		if strings.Contains(d.str("decision"), "-jev-waiver-") {
			waivers = append(waivers, d)
		}
	}
	if len(waivers) != 1 {
		t.Fatalf("%d waivers posted, want one: %v", len(waivers), kinds(posts))
	}
	d := waivers[0]
	requireWorkDecision(t, d, jevJudgment)
	if !strings.Contains(d.str("outcome"), "goes on without it") || strings.Contains(strings.ToLower(d.str("outcome")), "merged:") || !strings.Contains(d.str("outcome"), "not recorded as merged") {
		t.Fatalf("the waiver's outcome: %s", d.Raw)
	}
	posts, _ = f.pass()
	for _, d := range decisions(posts) {
		if strings.Contains(d.str("decision"), "-jev-waiver-") {
			t.Fatalf("the waiver was posted again: %s", d.Raw)
		}
	}
}

// A dependency the owner dropped is the owner's decision on the owner source,
// not Jev's.
func TestAnOwnersWaiverIsPostedAsTheOwnersDecision(t *testing.T) {
	f := newPublisherFixture(t)
	fixturekit.RecordHanded(t, f.repo, fixturekit.Stream, "stdin", fixturekit.DemoStart)
	at := fixturekit.DemoStart.Add(time.Minute)
	upstream := "w_00000000000000000000000000000a02"
	recordWaiver(t, f, lifecycle.EdgeWaiver{Upstream: config.WorkstreamID(upstream), Type: "merge_order", Ended: "owner", Reason: "the owner dropped it", DecidedBy: trace.DecidedByOwner, Trigger: "dep.upstream_terminal", At: at},
		trace.Routing{DecidedBy: trace.DecidedByOwner, DefaultTo: lifecycle.DependencyKept})

	posts, _ := f.pass()
	var waivers []hooksPost
	for _, d := range decisions(posts) {
		if strings.Contains(d.str("decision"), "-waiver-") {
			waivers = append(waivers, d)
		}
	}
	if len(waivers) != 1 || waivers[0].Source != "osmia-owner" || waivers[0].str("decider") != "owner" || strings.Contains(waivers[0].Raw, "Decided by Jev") {
		t.Fatalf("the owner's waiver: %v %v", waivers, kinds(posts))
	}
}
