package service

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	"github.com/kpenfound/osmia/internal/trace"
)

// The capacity view a running service serializes, in the chief's capacity
// tool result and in /v1/status, shows the architect and librarian pools as
// unbounded in words and with no numeric limit, and the other pools with
// their configured limits, in the same order.
func TestCapacityToolAndStatusSerializeUnboundedPools(t *testing.T) {
	t.Parallel()
	f := newTwoProjectFixture(t)
	f.opts.Threads = nil
	s, c := start(t, f.opts)
	ctx := context.Background()
	repo, err := s.repository(project)
	must(t, err)
	_, err = repo.EnsureChiefOfStaff(ctx, stream, demoStart, ownerActor)
	must(t, err)
	req := trace.TurnRequest{Header: trace.Header{Schema: "osmia.trace.turn-request", Version: trace.Version, ID: "request_capacity", Revision: 1, Project: project, Workstream: stream, At: demoStart, Actor: ownerActor, Cause: "owner-message"}, AgentID: trace.ChiefOfStaff, ThreadID: trace.ChiefOfStaff, TurnID: "capacity", Profile: coreadapter.Profile{Name: "default", Backend: "fake", Model: "fake"}, Prompt: "Read the capacity"}
	_, err = repo.EnqueueTurn(ctx, req)
	must(t, err)
	_, err = repo.ClaimTurn(ctx, stream, trace.ChiefOfStaff, "claim", "/owned/capacity", demoStart)
	must(t, err)
	scope := coreadapter.Scope{Project: string(project), Workstream: string(stream), Thread: trace.ChiefOfStaff, Turn: "capacity", Role: trace.ChiefOfStaff}
	controls := &runtimeControls{}
	controls.service.Store(s)

	tool := controls.capacity(repo, scope)
	for _, want := range []string{"config.toml", "reload", "not bounded factory-wide", "nothing to raise"} {
		if !strings.Contains(tool.Description, want) {
			t.Fatalf("capacity tool description lacks %q: %s", want, tool.Description)
		}
	}
	out, err := tool.Handle(ctx, json.RawMessage(`{}`))
	must(t, err)
	fromTool := fixturekit.CapacityWire(t, out)

	var status json.RawMessage
	must(t, c.Do(ctx, "GET", Prefix+"/status", nil, &status))
	fromStatus := fixturekit.CapacityWire(t, status)

	cfg := s.current().Capacity
	for i, want := range map[int]int{0: cfg.Masons, 1: cfg.Reviewers, 2: cfg.Committee, 5: cfg.Checks} {
		for name, roles := range map[string][]map[string]any{"tool": fromTool, "status": fromStatus} {
			if got := roles[i]["limit"]; got != float64(want) {
				t.Fatalf("%s %v limit %v, want %d", name, roles[i]["role"], got, want)
			}
		}
	}
	// The tool and the status API carry the same entries.
	toolJSON, err := json.Marshal(fromTool)
	must(t, err)
	statusJSON, err := json.Marshal(fromStatus)
	must(t, err)
	if string(toolJSON) != string(statusJSON) {
		t.Fatalf("tool and status capacity differ:\n%s\n%s", toolJSON, statusJSON)
	}
}
