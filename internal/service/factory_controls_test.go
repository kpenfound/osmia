package service

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	"github.com/kpenfound/osmia/internal/servicetest/intakefixture"
	"github.com/kpenfound/osmia/internal/trace"
)

func TestChiefControlsAnotherProjectAndRecordsTheTarget(t *testing.T) {
	t.Parallel()
	f := newTwoProjectFixture(t)
	f.opts.Threads = nil
	s, _ := start(t, f.opts)
	ctx := context.Background()
	repo, err := s.repository(intakefixture.Project)
	intakefixture.Must(t, err)
	_, err = repo.EnsureChiefOfStaff(ctx, intakefixture.Stream, fixturekit.DemoStart, ownerActor)
	intakefixture.Must(t, err)
	req := trace.TurnRequest{Header: trace.Header{Schema: "osmia.trace.turn-request", Version: trace.Version, ID: "request_control", Revision: 1, Project: intakefixture.Project, Workstream: intakefixture.Stream, At: fixturekit.DemoStart, Actor: ownerActor, Cause: "owner-message"}, AgentID: trace.ChiefOfStaff, ThreadID: trace.ChiefOfStaff, TurnID: "control", Profile: coreadapter.Profile{Name: "default", Backend: "fake", Model: "fake"}, Prompt: "Prioritise and pause the other project"}
	_, err = repo.EnqueueTurn(ctx, req)
	intakefixture.Must(t, err)
	_, err = repo.ClaimTurn(ctx, intakefixture.Stream, trace.ChiefOfStaff, "claim", "/owned/control", fixturekit.DemoStart)
	intakefixture.Must(t, err)
	scope := coreadapter.Scope{Project: string(intakefixture.Project), Workstream: string(intakefixture.Stream), Thread: trace.ChiefOfStaff, Turn: "control", Role: trace.ChiefOfStaff}
	controls := &runtimeControls{}
	controls.service.Store(s)
	invoke := func(tool coreadapter.Tool, input any) {
		t.Helper()
		raw, err := json.Marshal(input)
		intakefixture.Must(t, err)
		out, err := tool.Handle(ctx, raw)
		intakefixture.Must(t, err)
		var result struct {
			Recorded bool `json:"recorded"`
		}
		intakefixture.Must(t, json.Unmarshal(out, &result))
		if !result.Recorded {
			t.Fatalf("tool refused: %s", out)
		}
	}
	invoke(controls.prioritise(repo, scope, s.now), map[string]any{"project": otherProject, "workstreams": []string{string(otherStream)}})
	changes, err := trace.Read[trace.PriorityChange](repo, intakefixture.Stream)
	intakefixture.Must(t, err)
	if len(changes) != 1 || changes[0].TargetProject != otherProject || changes[0].Actor.Kind != "owner" {
		t.Fatalf("priority provenance: %+v", changes)
	}
	if got := effectivePriority(s.store, otherProject); len(got) != 1 || got[0] != otherStream {
		t.Fatalf("other project order: %v", got)
	}
	invoke(controls.pauseControl(repo, scope, s.now, false), map[string]any{"scope": "project", "project": otherProject, "reason": "Owner review"})
	if _, paused := s.pausing(otherProject, otherStream); !paused {
		t.Fatal("target project did not pause")
	}
	if _, paused := s.pausing(intakefixture.Project, intakefixture.Stream); paused {
		t.Fatal("origin project paused")
	}
	invoke(controls.pauseControl(repo, scope, s.now, true), map[string]any{"scope": "project", "project": otherProject})
	if _, paused := s.pausing(otherProject, otherStream); paused {
		t.Fatal("target project did not resume")
	}
	out, err := controls.capacity(repo, scope).Handle(ctx, json.RawMessage(`{}`))
	intakefixture.Must(t, err)
	var capacity struct {
		Capacity *CapacityStatus `json:"capacity"`
	}
	intakefixture.Must(t, json.Unmarshal(out, &capacity))
	if capacity.Capacity == nil || len(capacity.Capacity.Roles) != 5 {
		t.Fatalf("capacity: %s", out)
	}
}
