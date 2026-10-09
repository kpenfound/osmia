package service

import (
	"context"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/isolation"
	"github.com/kpenfound/osmia/internal/plan"
	"github.com/kpenfound/osmia/internal/trace"
)

func TestFactoryPolicyAdoptsInflightWorkOnceAcrossRestart(t *testing.T) {
	t.Parallel()
	f, fresh, repo, _ := newFinalFixture(t, "policy-adoption")
	defer repo.Close()
	ctx := context.Background()
	stream, err := config.NewWorkstreamID()
	must(t, err)
	at := f.s.now()
	must(t, repo.CreateWorkstream(ctx, stream, at, ownerActor))
	h := trace.Header{Schema: "osmia.trace.transition", Version: trace.Version, ID: "legacy-build", Revision: 1, Project: repo.Project(), Workstream: stream, At: at, Actor: ownerActor, Cause: "ratification"}
	_, err = repo.Transact(ctx, trace.Transaction{Transition: trace.Transition{Header: h, Subject: trace.FeatureSubject, To: BuildingState, Reason: "Owner ratified intent"}})
	must(t, err)
	for _, u := range []struct{ id, state string }{{"finished", UnitMerged}, {"stuck", UnitContested}} {
		h.ID, h.Unit = "legacy-"+u.id, u.id
		_, err = repo.Transact(ctx, trace.Transaction{Transition: trace.Transition{Header: h, Subject: trace.UnitSubject(u.id), To: u.state, Reason: "Explicit owner decision"}})
		must(t, err)
	}
	h.Unit, h.Schema, h.ID = "", "osmia.trace.document", plan.SpecDocument
	must(t, repo.RecordDocuments(ctx, []trace.Document{{Header: h, Path: plan.SpecPath, Content: validSpec}}))
	h.ID = plan.PlanDocument
	must(t, repo.RecordDocuments(ctx, []trace.Document{{Header: h, Path: plan.PlanPath, Content: validPlan}}))
	before, err := trace.Read[trace.Document](repo, stream)
	must(t, err)
	_, err = repo.EnqueueTurn(ctx, trace.TurnRequest{Header: trace.Header{Schema: "osmia.trace.turn-request", Version: trace.Version, ID: "request_legacy", Revision: 1, Project: repo.Project(), Workstream: stream, At: at, Actor: serviceActor, Cause: "legacy-build"}, AgentID: trace.ChiefOfStaff, ThreadID: trace.ChiefOfStaff, TurnID: "legacy", Profile: coreadapter.Profile{Backend: "claude", Name: "default", Model: "test"}, SystemPrompt: "Only the owner decides amendments.", Prompt: "Recover this workstream."})
	must(t, err)
	thread, err := repo.Thread(stream, trace.ChiefOfStaff)
	must(t, err)

	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if err := recordFactoryPolicy(canceled, repo, stream, at, true); err == nil {
		t.Fatal("canceled adoption succeeded")
	}
	must(t, f.s.reconcileFactoryPolicy(ctx, repo))
	entries, err := repo.Outbox(stream)
	must(t, err)
	if len(entries) != 1 || !strings.Contains(entries[0].Event.Body, "already acknowledged") {
		t.Fatalf("missing recovery notice: %+v", entries)
	}
	_, err = repo.Claim(ctx, stream, entries[0].Event.ID, "policy-token", "policy-worker", at, time.Minute)
	must(t, err)
	must(t, repo.Acknowledge(ctx, stream, entries[0].Event.ID, "policy-token", at))
	must(t, repo.Close())
	repo, err = trace.Open(f.s.cfg.Root, f.s.cfg.Project)
	must(t, err)
	defer repo.Close()
	must(t, f.s.reconcileFactoryPolicy(ctx, repo))
	entries, err = repo.Outbox(stream)
	must(t, err)
	if len(entries) != 1 || !entries[0].Acknowledged {
		t.Fatalf("adoption repeated after restart: %+v", entries)
	}
	after, err := trace.Read[trace.Document](repo, stream)
	must(t, err)
	if !reflect.DeepEqual(before, after) {
		t.Fatal("adoption rewrote sealed documents")
	}
	afterThread, err := repo.Thread(stream, trace.ChiefOfStaff)
	must(t, err)
	if !reflect.DeepEqual(thread, afterThread) {
		t.Fatal("adoption rewrote queued requests or conversation history")
	}
	for unit, want := range map[string]string{"finished": UnitMerged, "stuck": UnitContested} {
		st, err := repo.Workflow(stream, trace.UnitSubject(unit))
		must(t, err)
		if st.Value != want || st.Version != 1 {
			t.Fatalf("owner decision changed: %+v", st)
		}
	}
	freshEvents, err := repo.Outbox(fresh)
	must(t, err)
	for _, e := range freshEvents {
		if e.TransitionID == factoryPolicySubject+"-"+factoryPolicyVersion {
			t.Fatal("new workstream received an adoption notice")
		}
	}
}

func TestFactoryPolicyLeavesTerminalWorkstreamsUntouched(t *testing.T) {
	t.Parallel()
	f, _, repo, _ := newFinalFixture(t, "terminal-policy")
	defer repo.Close()
	ctx := context.Background()
	for _, state := range []string{DeliveredState, AbandonedState} {
		stream, err := config.NewWorkstreamID()
		must(t, err)
		must(t, repo.CreateWorkstream(ctx, stream, f.s.now(), ownerActor))
		_, err = repo.Transact(ctx, trace.Transaction{Transition: trace.Transition{Header: trace.Header{Schema: "osmia.trace.transition", Version: trace.Version, ID: "terminal", Revision: 1, Project: repo.Project(), Workstream: stream, At: f.s.now(), Actor: ownerActor, Cause: "owner-decision"}, Subject: trace.FeatureSubject, To: state, Reason: "Owner completed workstream"}})
		must(t, err)
		must(t, f.s.reconcileFactoryPolicy(ctx, repo))
		policy, err := repo.Workflow(stream, factoryPolicySubject)
		must(t, err)
		if policy.Version != 0 {
			t.Fatalf("terminal workstream adopted policy: %+v", policy)
		}
	}
}

func TestResumedRolesReceiveCurrentFactoryInstructionsWithoutHearsay(t *testing.T) {
	t.Parallel()
	f, stream, repo, _ := newFinalFixture(t, "resumed-policy")
	defer repo.Close()
	turns := memoryTurns(&isolation.Turns{}, f.s.cfg, repo)
	for _, role := range []string{trace.ChiefOfStaff, masonRole, reviewerRole, architectRole, committeeRole} {
		scope := coreadapter.Scope{Workstream: string(stream), Role: role}
		if role == masonRole || role == reviewerRole {
			scope.Unit = "resume"
		}
		current, err := turns.Context(context.Background(), scope)
		must(t, err)
		if !strings.Contains(current, "takes precedence over older role instructions") || !strings.Contains(current, "owner controls intent") {
			t.Fatalf("%s retains stale operating authority: %s", role, current)
		}
		if scope.Unit != "" && !strings.Contains(current, "Current assignment") {
			t.Fatal("policy replaced rather than supplemented the current assignment")
		}
	}
}
