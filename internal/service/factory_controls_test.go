package service

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	"github.com/kpenfound/osmia/internal/trace"
)

func TestChiefControlsAnotherProjectAndRecordsTheTarget(t *testing.T) {
	t.Parallel()
	f := newTwoProjectFixture(t)
	f.opts.Threads = nil
	s, _ := start(t, f.opts)
	ctx := context.Background()
	repo, err := s.repository(fixturekit.Project)
	fixturekit.Must(t, err)
	_, err = repo.EnsureManager(ctx, fixturekit.Stream, fixturekit.DemoStart, ownerActor)
	fixturekit.Must(t, err)
	req := trace.TurnRequest{Header: trace.Header{Schema: "osmia.trace.turn-request", Version: trace.Version, ID: "request_control", Revision: 1, Project: fixturekit.Project, Workstream: fixturekit.Stream, At: fixturekit.DemoStart, Actor: ownerActor, Cause: "owner-message"}, AgentID: trace.Manager, ThreadID: trace.Manager, TurnID: "control", Profile: coreadapter.Profile{Name: "default", Backend: "fake", Model: "fake"}, Prompt: "Prioritise and pause the other project"}
	_, err = repo.EnqueueTurn(ctx, req)
	fixturekit.Must(t, err)
	_, err = repo.ClaimTurn(ctx, fixturekit.Stream, trace.Manager, "claim", "/owned/control", fixturekit.DemoStart)
	fixturekit.Must(t, err)
	scope := coreadapter.Scope{Project: string(fixturekit.Project), Workstream: string(fixturekit.Stream), Thread: trace.Manager, Turn: "control", Role: trace.Manager}
	controls := &runtimeControls{}
	controls.service.Store(s)
	invoke := func(tool coreadapter.Tool, input any) {
		t.Helper()
		raw, err := json.Marshal(input)
		fixturekit.Must(t, err)
		out, err := tool.Handle(ctx, raw)
		fixturekit.Must(t, err)
		var result struct {
			Recorded bool `json:"recorded"`
		}
		fixturekit.Must(t, json.Unmarshal(out, &result))
		if !result.Recorded {
			t.Fatalf("tool refused: %s", out)
		}
	}
	invoke(controls.prioritise(repo, scope, s.now), map[string]any{"project": fixturekit.OtherProject, "workstreams": []string{string(fixturekit.OtherStream)}})
	changes, err := trace.Read[trace.PriorityChange](repo, fixturekit.Stream)
	fixturekit.Must(t, err)
	if len(changes) != 1 || changes[0].TargetProject != fixturekit.OtherProject || changes[0].Actor.Kind != "owner" {
		t.Fatalf("priority provenance: %+v", changes)
	}
	if got := effectivePriority(s.store, fixturekit.OtherProject); len(got) != 1 || got[0] != fixturekit.OtherStream {
		t.Fatalf("other project order: %v", got)
	}
	invoke(controls.pauseControl(repo, scope, s.now, false), map[string]any{"scope": "project", "project": fixturekit.OtherProject, "reason": "Owner review"})
	if _, paused := s.pausing(fixturekit.OtherProject, fixturekit.OtherStream); !paused {
		t.Fatal("target project did not pause")
	}
	if _, paused := s.pausing(fixturekit.Project, fixturekit.Stream); paused {
		t.Fatal("origin project paused")
	}
	invoke(controls.pauseControl(repo, scope, s.now, true), map[string]any{"scope": "project", "project": fixturekit.OtherProject})
	if _, paused := s.pausing(fixturekit.OtherProject, fixturekit.OtherStream); paused {
		t.Fatal("target project did not resume")
	}
	out, err := controls.capacity(repo, scope).Handle(ctx, json.RawMessage(`{}`))
	fixturekit.Must(t, err)
	var capacity struct {
		Capacity *CapacityStatus `json:"capacity"`
	}
	fixturekit.Must(t, json.Unmarshal(out, &capacity))
	if capacity.Capacity == nil || len(capacity.Capacity.Roles) != 6 {
		t.Fatalf("capacity: %s", out)
	}
}
