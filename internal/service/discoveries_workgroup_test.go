package service

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/trace"
)

// TestSharedChiefDispositionsMemberDiscoveries covers a cross-repo
// workgroup's shared chief of staff, whose conversation belongs to the
// workgroup trace's host: factory_context and record_discovery name a member,
// read and change that member's own discoveries in its own trace, and a
// declined discovery no longer holds that member's assembly.
func TestSharedChiefDispositionsMemberDiscoveries(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	at := time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)
	now := func() time.Time { return at }
	owner := trace.Actor{Kind: "owner", ID: "local"}
	const (
		p1 config.ProjectID    = "p_00000000000000000000000000000021"
		p2 config.ProjectID    = "p_00000000000000000000000000000022"
		m1 config.WorkstreamID = "w_00000000000000000000000000000021"
		m2 config.WorkstreamID = "w_00000000000000000000000000000022"
	)
	base := t.TempDir()
	root, err := config.ResolveRoot(filepath.Join(base, "osmia"), "")
	must(t, err)
	open := func(project config.ProjectID, stream config.WorkstreamID) *trace.Repository {
		clone := filepath.Join(base, string(project))
		demoGit(t, base, "init", "-q", clone)
		r, err := trace.Create(ctx, root, config.Project{ID: project, Clone: clone}, at, owner)
		must(t, err)
		t.Cleanup(func() { r.Close() })
		must(t, r.CreateWorkstream(ctx, stream, at, owner))
		return r
	}
	r1, r2 := open(p1, m1), open(p2, m2)
	wg, g, err := trace.CreateWorkgroupTrace(ctx, root, []trace.CrossRepoMember{{Workstream: m1, Project: r1}, {Workstream: m2, Project: r2}}, at, owner, "test-workgroup")
	must(t, err)
	t.Cleanup(func() { wg.Close() })
	host := g.Members[0]

	_, err = r2.SetFeatureState(ctx, trace.Header{Schema: "osmia.trace.transition", Version: trace.Version, ID: "building", Revision: 1, Project: p2, Workstream: m2, At: at, Actor: owner, Cause: "test"}, BuildingState, "test")
	must(t, err)
	id := "discovery-" + strings.Repeat("a", 64)
	reviewer := trace.Header{Schema: "osmia.trace.document", Version: trace.Version, ID: id, Revision: 1, Project: p2, Workstream: m2, At: at, Actor: trace.Actor{Kind: "agent", ID: "reviewer_thread"}, Cause: "review"}
	content, _ := json.Marshal(workDiscovery{ID: id, Description: "The check run omitted a flag the plan names", Criterion: "spec#1", Disposition: "open"})
	opened := reviewer
	opened.Schema, opened.ID = "osmia.trace.transition", id+"-1"
	_, err = r2.RecordDocumentsWith(ctx, []trace.Document{{Header: reviewer, Path: "discoveries/" + id + ".json", Content: string(content)}}, trace.Transaction{Transition: trace.Transition{Header: opened, Subject: id, To: "open", Reason: "review"}})
	must(t, err)
	pending, err := unresolvedWork(r2, m2)
	must(t, err)
	if !pending {
		t.Fatal("an open discovery did not hold the member")
	}

	header := trace.Header{Schema: "osmia.trace.turn-request", Version: trace.Version, ID: "request_events_1", Revision: 1, Project: wg.Project(), Workstream: host, At: at, Actor: trace.Actor{Kind: "service", ID: "controller"}, Cause: "test", Depth: 1}
	_, err = wg.EnqueueTurn(ctx, trace.TurnRequest{Header: header, AgentID: trace.ChiefOfStaff, ThreadID: trace.ChiefOfStaff, TurnID: "events_1", Profile: coreadapter.Profile{Name: "default", Backend: "fake", Model: "test"}, SystemPrompt: "test", Prompt: "test"})
	must(t, err)
	_, err = wg.ClaimTurn(ctx, host, trace.ChiefOfStaff, "token_events_1", "/owned/events_1", at)
	must(t, err)
	scope := coreadapter.Scope{Project: string(wg.Project()), Workstream: string(host), Thread: trace.ChiefOfStaff, Turn: "events_1", Role: trace.ChiefOfStaff}

	read := factoryContext(wg, scope)
	raw, err := read.Handle(ctx, json.RawMessage(`{"path":"discoveries","workstream":"`+string(m2)+`"}`))
	must(t, err)
	var listed map[string]int
	must(t, json.Unmarshal(raw, &listed))
	if listed["discoveries/"+id+".json"] != 1 {
		t.Fatalf("member discoveries listed %s", raw)
	}
	raw, err = read.Handle(ctx, json.RawMessage(`{"workstream":"w_00000000000000000000000000000099"}`))
	must(t, err)
	if !strings.Contains(string(raw), "does not share this chief of staff") {
		t.Fatalf("a workstream outside the workgroup was read: %s", raw)
	}

	record := recordDiscovery(wg, scope, now)
	input, _ := json.Marshal(map[string]any{"workstream": m2, "id": id, "revision": 1, "disposition": "declined", "reason": "The selected checks the service ran cover the changed packages."})
	raw, err = record.Handle(ctx, input)
	must(t, err)
	if !strings.Contains(string(raw), `"recorded":true`) {
		t.Fatalf("member discovery disposition: %s", raw)
	}
	pending, err = unresolvedWork(r2, m2)
	must(t, err)
	if pending {
		t.Fatal("a declined member discovery still held the member")
	}
	ds, err := discoveries(r2, m2)
	must(t, err)
	if d := ds[id]; d.Disposition != "declined" || d.Description != "The check run omitted a flag the plan names" {
		t.Fatalf("member trace holds %+v", d)
	}

	// A mason or reviewer addresses only its own workstream.
	mason := scope
	mason.Role = masonRole
	if strings.Contains(string(recordDiscovery(wg, mason, now).InputSchema), `"workstream"`) {
		t.Fatal("a mason's record_discovery offers another workstream")
	}
}
