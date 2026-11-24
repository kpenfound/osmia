package service

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	"github.com/kpenfound/osmia/internal/trace"
)

func recordRouted(t *testing.T, repo *trace.Repository, id, from, to string, routing trace.Routing, at time.Time) {
	t.Helper()
	h := trace.Header{Schema: "osmia.trace.transition", Version: trace.Version, ID: id, Revision: 1, Project: repo.Project(), Workstream: fixturekit.Stream, Unit: "resume", At: at, Actor: trace.Actor{Kind: "service", ID: "checks"}, Cause: "checks-1"}
	subject := trace.UnitSubject("resume")
	state, err := repo.Workflow(fixturekit.Stream, subject)
	fixturekit.Must(t, err)
	tr := trace.Transition{Header: h, Subject: subject, From: from, To: to, Reason: "check run 1 failed on candidate abc", DecidedBy: routing.DecidedBy, Judgment: routing.Judgment, DefaultTo: routing.DefaultTo}
	_, err = repo.Transact(context.Background(), trace.Transaction{ExpectedVersion: state.Version, Transition: tr})
	fixturekit.Must(t, err)
}

func TestOnlyADepartureIsPostedToHearsay(t *testing.T) {
	f := newPublisherFixture(t)
	fixturekit.RecordHanded(t, f.repo, fixturekit.Stream, "stdin", fixturekit.DemoStart)
	at := fixturekit.DemoStart.Add(time.Minute)
	recordRouted(t, f.repo, "unit-resume-checking-1", "", "checking", trace.Routing{DecidedBy: trace.DecidedByCode, DefaultTo: "checking"}, at)
	recordRouted(t, f.repo, "unit-resume-implementing-checks-1-2", "checking", "implementing", trace.Routing{DecidedBy: trace.DecidedByCode, Judgment: jevJudgment, DefaultTo: "implementing"}, at.Add(time.Minute))
	recordRouted(t, f.repo, "unit-resume-reviewing-checks-1-3", "implementing", "reviewing", trace.Routing{DecidedBy: trace.DecidedByFallback, DefaultTo: "reviewing"}, at.Add(2*time.Minute))
	recordRouted(t, f.repo, "unit-resume-move-4-checking", "reviewing", "checking", trace.Routing{DecidedBy: trace.DecidedByOwner, DefaultTo: "checking"}, at.Add(3*time.Minute))

	posts, _ := f.pass()
	for _, d := range decisions(posts) {
		if strings.Contains(d.str("decision"), "-jev-route-") {
			t.Fatalf("a transition that departed from nothing was posted: %s", d.Raw)
		}
	}

	recordRouted(t, f.repo, "unit-resume-move-5-implementing", "checking", "implementing", trace.Routing{DecidedBy: trace.DecidedByJev, Judgment: jevJudgment, DefaultTo: "contested"}, at.Add(4*time.Minute))
	posts, _ = f.pass()
	var departures []hooksPost
	for _, d := range decisions(posts) {
		if strings.Contains(d.str("decision"), "-jev-route-") {
			departures = append(departures, d)
		}
	}
	if len(departures) != 1 {
		t.Fatalf("%d departures posted, want one: %v", len(departures), kinds(posts))
	}
	d := departures[0]
	requireWorkDecision(t, d, jevJudgment)
	if d.str("decision") != string(fixturekit.Stream)+"-jev-route-unit-resume-move-5-implementing" || !strings.Contains(d.str("outcome"), "from checking to implementing instead of contested") {
		t.Fatalf("the departure: %s", d.Raw)
	}
	if d.Source != "osmia-work" {
		t.Fatalf("source %s", d.Source)
	}

	posts, _ = f.pass()
	for _, d := range decisions(posts) {
		if strings.Contains(d.str("decision"), "-jev-route-") {
			t.Fatalf("the departure was posted again: %s", d.Raw)
		}
	}
}
