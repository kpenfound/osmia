package service

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	"github.com/kpenfound/osmia/internal/servicetest/intakefixture"
	"github.com/kpenfound/osmia/internal/trace"
)

func TestCapacityToolAndStatusSerializeUnboundedPools(t *testing.T) {
	t.Parallel()
	f := newTwoProjectFixture(t)
	f.opts.Threads = nil
	s, c := start(t, f.opts)
	ctx := context.Background()
	repo, err := s.repository(intakefixture.Project)
	intakefixture.Must(t, err)
	_, err = repo.EnsureChiefOfStaff(ctx, intakefixture.Stream, fixturekit.DemoStart, ownerActor)
	intakefixture.Must(t, err)
	req := trace.TurnRequest{Header: trace.Header{Schema: "osmia.trace.turn-request", Version: trace.Version, ID: "request_capacity", Revision: 1, Project: intakefixture.Project, Workstream: intakefixture.Stream, At: fixturekit.DemoStart, Actor: ownerActor, Cause: "owner-message"}, AgentID: trace.ChiefOfStaff, ThreadID: trace.ChiefOfStaff, TurnID: "capacity", Profile: coreadapter.Profile{Name: "default", Backend: "fake", Model: "fake"}, Prompt: "Read the capacity"}
	_, err = repo.EnqueueTurn(ctx, req)
	intakefixture.Must(t, err)
	_, err = repo.ClaimTurn(ctx, intakefixture.Stream, trace.ChiefOfStaff, "claim", "/owned/capacity", fixturekit.DemoStart)
	intakefixture.Must(t, err)
	scope := coreadapter.Scope{Project: string(intakefixture.Project), Workstream: string(intakefixture.Stream), Thread: trace.ChiefOfStaff, Turn: "capacity", Role: trace.ChiefOfStaff}
	controls := &runtimeControls{}
	controls.service.Store(s)

	tool := controls.capacity(repo, scope)
	for _, want := range []string{"config.toml", "reload", "not bounded factory-wide", "nothing to raise"} {
		if !strings.Contains(tool.Description, want) {
			t.Fatalf("capacity tool description lacks %q: %s", want, tool.Description)
		}
	}
	out, err := tool.Handle(ctx, json.RawMessage(`{}`))
	intakefixture.Must(t, err)
	fromTool := fixturekit.CapacityWire(t, out)

	var status json.RawMessage
	intakefixture.Must(t, c.Do(ctx, "GET", Prefix+"/status", nil, &status))
	fromStatus := fixturekit.CapacityWire(t, status)

	cfg := s.current().Capacity
	for i, want := range map[int]int{0: cfg.Masons, 1: cfg.Reviewers, 2: cfg.Committee, 4: cfg.Checks} {
		for name, roles := range map[string][]map[string]any{"tool": fromTool, "status": fromStatus} {
			if got := roles[i]["limit"]; got != float64(want) {
				t.Fatalf("%s %v limit %v, want %d", name, roles[i]["role"], got, want)
			}
		}
	}
	toolJSON, err := json.Marshal(fromTool)
	intakefixture.Must(t, err)
	statusJSON, err := json.Marshal(fromStatus)
	intakefixture.Must(t, err)
	if string(toolJSON) != string(statusJSON) {
		t.Fatalf("tool and status capacity differ:\n%s\n%s", toolJSON, statusJSON)
	}
}
