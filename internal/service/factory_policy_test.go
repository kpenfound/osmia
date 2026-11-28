package service

import (
	"context"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/intake"
	"github.com/kpenfound/osmia/internal/isolation"
	kindreg "github.com/kpenfound/osmia/internal/kinds"
	"github.com/kpenfound/osmia/internal/plan"
	"github.com/kpenfound/osmia/internal/trace"
)

func TestFactoryPolicyAdoptsInflightWorkOnceAcrossRestart(t *testing.T) {
	t.Parallel()
	f, fresh, repo, _ := newFinalFixture(t, "policy-adoption")
	defer repo.Close()
	ctx := context.Background()
	stream, err := config.NewWorkstreamID()
	fixturekit.Must(t, err)
	at := f.s.now()
	fixturekit.Must(t, repo.CreateWorkstream(ctx, stream, at, ownerActor))
	h := trace.Header{Schema: "osmia.trace.transition", Version: trace.Version, ID: "legacy-build", Revision: 1, Project: repo.Project(), Workstream: stream, At: at, Actor: ownerActor, Cause: "ratification"}
	_, err = repo.Transact(ctx, trace.Transaction{Transition: trace.Transition{Header: h, Subject: trace.FeatureSubject, To: BuildingState, Reason: "Owner ratified intent"}})
	fixturekit.Must(t, err)
	for _, u := range []struct{ id, state string }{{"finished", UnitMerged}, {"stuck", UnitContested}} {
		h.ID, h.Unit = "legacy-"+u.id, u.id
		_, err = repo.Transact(ctx, trace.Transaction{Transition: trace.Transition{Header: h, Subject: trace.UnitSubject(u.id), To: u.state, Reason: "Explicit owner decision"}})
		fixturekit.Must(t, err)
	}
	h.Unit, h.Schema, h.ID = "", "osmia.trace.document", plan.SpecDocument
	fixturekit.Must(t, repo.RecordDocuments(ctx, []trace.Document{{Header: h, Path: plan.SpecPath, Content: fixturekit.ValidSpec}}))
	h.ID = plan.PlanDocument
	fixturekit.Must(t, repo.RecordDocuments(ctx, []trace.Document{{Header: h, Path: plan.PlanPath, Content: fixturekit.ValidPlan}}))
	before, err := trace.Read[trace.Document](repo, stream)
	fixturekit.Must(t, err)
	_, err = repo.EnqueueTurn(ctx, trace.TurnRequest{Header: trace.Header{Schema: "osmia.trace.turn-request", Version: trace.Version, ID: "request_legacy", Revision: 1, Project: repo.Project(), Workstream: stream, At: at, Actor: serviceActor, Cause: "legacy-build"}, AgentID: trace.Manager, ThreadID: trace.Manager, TurnID: "legacy", Profile: coreadapter.Profile{Backend: "claude", Name: "default", Model: "test"}, SystemPrompt: "Only the owner decides amendments.", Prompt: "Recover this workstream."})
	fixturekit.Must(t, err)
	thread, err := repo.Thread(stream, trace.Manager)
	fixturekit.Must(t, err)

	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if err := recordFactoryPolicy(canceled, repo, stream, at, true); err == nil {
		t.Fatal("canceled adoption succeeded")
	}
	fixturekit.Must(t, f.s.reconcileFactoryPolicy(ctx, repo))
	entries, err := repo.Outbox(stream)
	fixturekit.Must(t, err)
	if len(entries) != 1 || !strings.Contains(entries[0].Event.Body, "already acknowledged") {
		t.Fatalf("missing recovery notice: %+v", entries)
	}
	_, err = repo.Claim(ctx, stream, entries[0].Event.ID, "policy-token", "policy-worker", at, time.Minute)
	fixturekit.Must(t, err)
	fixturekit.Must(t, repo.Acknowledge(ctx, stream, entries[0].Event.ID, "policy-token", at))
	fixturekit.Must(t, repo.Close())
	repo, err = trace.Open(f.s.cfg.Root, f.s.cfg.Project)
	fixturekit.Must(t, err)
	defer repo.Close()
	fixturekit.Must(t, f.s.reconcileFactoryPolicy(ctx, repo))
	entries, err = repo.Outbox(stream)
	fixturekit.Must(t, err)
	if len(entries) != 1 || !entries[0].Acknowledged {
		t.Fatalf("adoption repeated after restart: %+v", entries)
	}
	after, err := trace.Read[trace.Document](repo, stream)
	fixturekit.Must(t, err)
	if !reflect.DeepEqual(before, after) {
		t.Fatal("adoption rewrote sealed documents")
	}
	afterThread, err := repo.Thread(stream, trace.Manager)
	fixturekit.Must(t, err)
	if !reflect.DeepEqual(thread, afterThread) {
		t.Fatal("adoption rewrote queued requests or conversation history")
	}
	for unit, want := range map[string]string{"finished": UnitMerged, "stuck": UnitContested} {
		st, err := repo.Workflow(stream, trace.UnitSubject(unit))
		fixturekit.Must(t, err)
		if st.Value != want || st.Version != 1 {
			t.Fatalf("owner decision changed: %+v", st)
		}
	}
	freshEvents, err := repo.Outbox(fresh)
	fixturekit.Must(t, err)
	for _, e := range freshEvents {
		if e.TransitionID == factoryPolicySubject+"-"+intake.FactoryPolicyVersion {
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
		fixturekit.Must(t, err)
		fixturekit.Must(t, repo.CreateWorkstream(ctx, stream, f.s.now(), ownerActor))
		_, err = repo.Transact(ctx, trace.Transaction{Transition: trace.Transition{Header: trace.Header{Schema: "osmia.trace.transition", Version: trace.Version, ID: "terminal", Revision: 1, Project: repo.Project(), Workstream: stream, At: f.s.now(), Actor: ownerActor, Cause: "owner-decision"}, Subject: trace.FeatureSubject, To: state, Reason: "Owner completed workstream"}})
		fixturekit.Must(t, err)
		fixturekit.Must(t, f.s.reconcileFactoryPolicy(ctx, repo))
		policy, err := repo.Workflow(stream, factoryPolicySubject)
		fixturekit.Must(t, err)
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
	for _, role := range []string{trace.Manager, masonRole, reviewerRole, architectRole, committeeRole} {
		scope := coreadapter.Scope{Workstream: string(stream), Role: role}
		if role == masonRole || role == reviewerRole {
			scope.Unit = "resume"
		}
		current, err := turns.Context(context.Background(), scope)
		fixturekit.Must(t, err)
		if !strings.Contains(current, "## Operating policy") || !strings.Contains(current, "owner controls intent") {
			t.Fatalf("%s retains stale operating authority: %s", role, current)
		}
		if scope.Unit != "" && !strings.Contains(current, "Current assignment") {
			t.Fatal("policy replaced rather than supplemented the current assignment")
		}
	}
}

var policyRoles = []string{trace.Manager, masonRole, reviewerRole, architectRole, committeeRole}

func policyFor(t *testing.T, role, kind, turn string) string {
	t.Helper()
	k, ok := kindreg.Lookup(kind)
	if !ok {
		t.Fatalf("kind %q is not registered", kind)
	}
	return FactoryInstructionsFor(coreadapter.Scope{Role: role, Turn: turn}, k)
}

func TestHillClimbArchitectAndCommitteeReceiveNoPlanAdvice(t *testing.T) {
	t.Parallel()
	planAdvice := []string{"size each unit", "mason turn can complete", "connecting the feature", "whole check suite", "rewrite an existing plan", "plan revisions", "boundaries, constraints and adaptable guidance", "Plans and units", "footprint", "unit", "plan"}
	for _, turn := range []struct{ role, turn string }{{architectRole, ""}, {committeeRole, "debate-1"}, {committeeRole, "final-3-committee-1-1"}} {
		got := policyFor(t, turn.role, kindreg.HillClimb, turn.turn)
		if !strings.Contains(got, "owner controls intent") {
			t.Fatalf("hill-climb %s turn lost the authority rule: %s", turn.role, got)
		}
		for _, banned := range planAdvice {
			if strings.Contains(strings.ToLower(got), strings.ToLower(banned)) {
				t.Errorf("hill-climb %s turn %q receives plan advice %q: %s", turn.role, turn.turn, banned, got)
			}
		}
	}
	for _, role := range []string{trace.Manager, masonRole, reviewerRole} {
		got := policyFor(t, role, kindreg.HillClimb, "")
		for _, banned := range []string{"size each unit", "connecting the feature", "plan revisions", "Neighboring assignments", "Footprints schedule work", "Plans and units"} {
			if strings.Contains(got, banned) {
				t.Errorf("hill-climb %s turn receives feature-only advice %q: %s", role, banned, got)
			}
		}
	}
}

func TestFeatureArchitectAndCommitteeStillReceivePlanAdvice(t *testing.T) {
	t.Parallel()
	for _, turn := range []struct{ role, turn string }{{architectRole, ""}, {committeeRole, "debate-1"}} {
		got := policyFor(t, turn.role, kindreg.Feature, turn.turn)
		for _, want := range []string{"size each unit so one mason turn can complete it", "connecting the feature's parts in the running product", "whole check suite to pass or run", "do not rewrite an existing plan merely to change its format"} {
			if !strings.Contains(got, want) {
				t.Errorf("feature %s turn lacks %q: %s", turn.role, want, got)
			}
		}
	}
	final := policyFor(t, committeeRole, kindreg.Feature, "final-3-committee-1-1")
	if strings.Contains(final, "whole check suite") || strings.Contains(final, "service-owned check gate") {
		t.Errorf("a final committee turn receives unit-review wording: %s", final)
	}
	if got := policyFor(t, trace.Manager, kindreg.Feature, ""); !strings.Contains(got, "do not commission a revision only to widen one") || !strings.Contains(got, "approving a candidate stay the owner's") {
		t.Errorf("feature chief lost its footprint or approval rule: %s", got)
	}
}

func TestEveryOperatingPolicyRuleCarriesItsReasonAndNamesNoSkill(t *testing.T) {
	t.Parallel()
	skillNames := []string{"behavioral-testing", "simple-code", "no-deltas", "no-comments", "lint-discipline", "unslop", "typescript-best-practices", "debugging", "handoff-reports", "design-sketch", "hill-climb-evaluator", "pr-review", "pr-writing"}
	for _, kind := range kindreg.Names() {
		for _, role := range policyRoles {
			got := policyFor(t, role, kind, "")
			if !strings.Contains(got, "## Operating policy") || !strings.Contains(got, "### ") {
				t.Errorf("%s/%s policy is not in sections: %s", kind, role, got)
			}
			for _, line := range strings.Split(got, "\n") {
				if strings.HasPrefix(line, "- ") && !strings.Contains(line, " Reason: ") {
					t.Errorf("%s/%s rule has no reason: %s", kind, role, line)
				}
			}
			for _, name := range skillNames {
				if strings.Contains(got, name) {
					t.Errorf("%s/%s policy names the skill %q", kind, role, name)
				}
			}
			for _, framing := range []string{"takes precedence over", "conversation history"} {
				if strings.Contains(got, framing) {
					t.Errorf("%s/%s policy keeps the framing %q", kind, role, framing)
				}
			}
		}
	}
	if got := policyFor(t, "foreman", kindreg.Feature, ""); got != "" {
		t.Errorf("foreman has no operating policy, got %q", got)
	}
}

func TestTurnContextCarriesTheOperatingPolicyOfTheWorkstreamsKind(t *testing.T) {
	t.Parallel()
	f, feature, repo, _ := newFinalFixture(t, "policy-by-kind")
	defer repo.Close()
	ctx := context.Background()
	climb, err := config.NewWorkstreamID()
	fixturekit.Must(t, err)
	fixturekit.Must(t, repo.CreateWorkstreamKind(ctx, climb, "", kindreg.HillClimb, f.s.now(), ownerActor))
	turns := memoryTurns(&isolation.Turns{}, f.s.cfg, repo)
	for _, role := range []string{architectRole, committeeRole} {
		onClimb, err := turns.Context(ctx, coreadapter.Scope{Workstream: string(climb), Role: role})
		fixturekit.Must(t, err)
		onFeature, err := turns.Context(ctx, coreadapter.Scope{Workstream: string(feature), Role: role})
		fixturekit.Must(t, err)
		if !strings.Contains(onClimb, "owner controls intent") || strings.Contains(onClimb, "size each unit") || strings.Contains(onClimb, "whole check suite") {
			t.Errorf("hill-climb %s turn context: %s", role, onClimb)
		}
		if !strings.Contains(onFeature, "size each unit so one mason turn can complete it") {
			t.Errorf("feature %s turn context lacks the plan advice: %s", role, onFeature)
		}
	}
}

func TestFactoryPolicyNamesTheManagerAndNeverTheChief(t *testing.T) {
	t.Parallel()
	for _, kind := range []string{kindreg.Feature, kindreg.HillClimb} {
		for _, role := range policyRoles {
			text := policyFor(t, role, kind, "turn-1")
			if strings.Contains(strings.ToLower(text), "chief") {
				t.Errorf("%s policy of a %s workstream names the chief: %s", role, kind, text)
			}
		}
	}
	if text := policyFor(t, trace.Manager, kindreg.Feature, "turn-1"); !strings.Contains(text, "## Manager: investigating and deciding") {
		t.Errorf("the manager's policy has no manager section: %s", text)
	}
	if text := policyFor(t, reviewerRole, kindreg.Feature, "turn-1"); !strings.Contains(text, "ask the manager to coordinate") {
		t.Errorf("the reviewer's policy does not send coordination to the manager: %s", text)
	}
}
