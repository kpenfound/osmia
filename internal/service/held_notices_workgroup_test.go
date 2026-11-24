package service

import (
	"context"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/events"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	"github.com/kpenfound/osmia/internal/trace"
)

func TestHeldNoticesOfACrossRepoMemberHaveTheirInboxCard(t *testing.T) {
	ctx := context.Background()
	s, chief, g := workgroupFeedFixture(t, true, 2)
	repo := s.projects[0].repository
	clock := &fixturekit.FixedClock{At: fixturekit.DemoStart}

	h := trace.Header{Schema: "osmia.trace.transition", Version: trace.Version, ID: "held-notice", Revision: 1, Project: repo.Project(), Workstream: fixturekit.Stream, At: fixturekit.DemoStart, Actor: serviceActor, Cause: "test"}
	_, err := repo.Transact(ctx, trace.Transaction{Transition: trace.Transition{Header: h, Subject: "held-notice", To: "changed", Reason: "seeded"},
		Events: []trace.Event{trace.Notice("held-notice", "chief", "The notice the chief cannot take")}})
	fixturekit.Must(t, err)
	d, err := events.New(repo, events.Options{Now: clock.Now,
		Profile: func() (coreadapter.Profile, error) {
			return coreadapter.Profile{Name: "default", Backend: "fake", Model: "test"}, nil
		},
		ChiefTarget: func(ctx context.Context, stream config.WorkstreamID) (*trace.Repository, func(), error) {
			target, _, _, close, err := s.chiefRepository(ctx, repo, stream)
			return target, close, err
		}})
	fixturekit.Must(t, err)

	host := g.Members[0]
	for i := range events.MaxDeliveryFailures {
		if _, open, err := s.intake().HeldNoticesEntry(ctx, repo, fixturekit.Stream); err != nil || open {
			t.Fatalf("after %d failed deliveries the notices card is open=%v, %v", i, open, err)
		}
		fixturekit.Must(t, d.Pass(ctx))
		q, err := chief.ClaimTurn(ctx, host, trace.ChiefOfStaff, fmt.Sprintf("held-%d", i), filepath.Join(t.TempDir(), "chief"), clock.Now())
		fixturekit.Must(t, err)
		rh := q.Request.Header
		rh.Schema, rh.ID, rh.At = "osmia.trace.turn-response", trace.EventID(q.Request.ID, "response"), clock.Now()
		fixturekit.Must(t, chief.CaptureTurn(ctx, q.Claim.Token, trace.TurnResponse{Header: rh, AgentID: trace.ChiefOfStaff, ThreadID: trace.ChiefOfStaff, TurnID: q.Request.TurnID, RequestID: q.Request.ID, RequestRevision: q.Request.Revision, Failure: "the provider is down",
			Result: coreadapter.SessionResult{Session: coreadapter.BackendSession{Backend: "fake", ID: "chief-session"}, StartedAt: q.Claim.At, SessionDirectory: q.Claim.SessionDirectory, IsError: true}}))
		fixturekit.Must(t, chief.CompleteTurn(ctx, host, trace.ChiefOfStaff, q.Request.TurnID, q.Claim.Token, clock.Now()))
		clock.Mu.Lock()
		clock.At = clock.At.Add(time.Hour)
		clock.Mu.Unlock()
	}

	e, open, err := s.intake().HeldNoticesEntry(ctx, repo, fixturekit.Stream)
	fixturekit.Must(t, err)
	if !open || e.Kind != InboxNotices || e.Workstream != fixturekit.Stream || !strings.Contains(e.Question, "the provider is down") {
		t.Fatalf("the held notices card of the cross-repo member is open=%v: %+v", open, e)
	}
	inbox, apiErr := s.intake().Inbox(ctx)
	if apiErr != nil {
		t.Fatal(apiErr.Message)
	}
	if !slices.ContainsFunc(inbox.Entries, func(e InboxEntry) bool { return e.Kind == InboxNotices && e.Workstream == fixturekit.Stream }) {
		t.Fatalf("the inbox does not list the member's held notices: %+v", inbox.Entries)
	}
}
