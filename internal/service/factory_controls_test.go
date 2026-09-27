package service

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/trace"
)

func TestChiefControlsAnotherProjectAndRecordsTheTarget(t *testing.T) {
	f := newTwoProjectFixture(t)
	f.opts.Threads = nil
	s, _ := start(t, f.opts)
	ctx := context.Background()
	repo, err := s.repository(project)
	must(t, err)
	_, err = repo.EnsureChiefOfStaff(ctx, stream, demoStart, ownerActor)
	must(t, err)
	req := trace.TurnRequest{Header: trace.Header{Schema: "osmia.trace.turn-request", Version: trace.Version, ID: "request_control", Revision: 1, Project: project, Workstream: stream, At: demoStart, Actor: ownerActor, Cause: "owner-message"}, AgentID: trace.ChiefOfStaff, ThreadID: trace.ChiefOfStaff, TurnID: "control", Profile: coreadapter.Profile{Name: "default", Backend: "fake", Model: "fake"}, Prompt: "Prioritise and pause the other project"}
	_, err = repo.EnqueueTurn(ctx, req)
	must(t, err)
	_, err = repo.ClaimTurn(ctx, stream, trace.ChiefOfStaff, "claim", "/owned/control", demoStart)
	must(t, err)
	scope := coreadapter.Scope{Project: string(project), Workstream: string(stream), Thread: trace.ChiefOfStaff, Turn: "control", Role: trace.ChiefOfStaff}
	controls := &runtimeControls{}
	controls.service.Store(s)
	invoke := func(tool coreadapter.Tool, input any) {
		t.Helper()
		raw, err := json.Marshal(input)
		must(t, err)
		out, err := tool.Handle(ctx, raw)
		must(t, err)
		var result struct {
			Recorded bool `json:"recorded"`
		}
		must(t, json.Unmarshal(out, &result))
		if !result.Recorded {
			t.Fatalf("tool refused: %s", out)
		}
	}
	invoke(controls.prioritise(repo, scope, s.now), map[string]any{"project": otherProject, "workstreams": []string{string(otherStream)}})
	changes, err := trace.Read[trace.PriorityChange](repo, stream)
	must(t, err)
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
	if _, paused := s.pausing(project, stream); paused {
		t.Fatal("origin project paused")
	}
	invoke(controls.pauseControl(repo, scope, s.now, true), map[string]any{"scope": "project", "project": otherProject})
	if _, paused := s.pausing(otherProject, otherStream); paused {
		t.Fatal("target project did not resume")
	}
	out, err := controls.capacity(repo, scope).Handle(ctx, json.RawMessage(`{}`))
	must(t, err)
	var capacity struct {
		Capacity *CapacityStatus `json:"capacity"`
	}
	must(t, json.Unmarshal(out, &capacity))
	if capacity.Capacity == nil || len(capacity.Capacity.Roles) != 3 {
		t.Fatalf("capacity: %s", out)
	}
}
