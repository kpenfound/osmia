package service

import (
	"context"
	"encoding/json"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	"github.com/kpenfound/osmia/internal/servicetest/intakefixture"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/jev"
	"github.com/kpenfound/osmia/internal/jev/jevfixture"
	"github.com/kpenfound/osmia/internal/trace"
)

type memberDiscovery struct {
	wg    *trace.Repository
	r2    *trace.Repository
	m2    config.WorkstreamID
	id    string
	scope coreadapter.Scope
	now   func() time.Time
}

// openMemberDiscovery opens a workgroup of two members, records an open
// discovery in the second member's trace and claims a turn of the shared chief
// of staff.
func openMemberDiscovery(t *testing.T) *memberDiscovery {
	t.Helper()
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
	intakefixture.Must(t, err)
	open := func(project config.ProjectID, stream config.WorkstreamID) *trace.Repository {
		clone := filepath.Join(base, string(project))
		fixturekit.DemoGit(t, base, "init", "-q", clone)
		r, err := trace.Create(ctx, root, config.Project{ID: project, Clone: clone}, at, owner)
		intakefixture.Must(t, err)
		t.Cleanup(func() { r.Close() })
		intakefixture.Must(t, r.CreateWorkstream(ctx, stream, at, owner))
		return r
	}
	r1, r2 := open(p1, m1), open(p2, m2)
	wg, g, err := trace.CreateWorkgroupTrace(ctx, root, []trace.CrossRepoMember{{Workstream: m1, Project: r1}, {Workstream: m2, Project: r2}}, at, owner, "test-workgroup")
	intakefixture.Must(t, err)
	t.Cleanup(func() { wg.Close() })
	host := g.Members[0]

	_, err = r2.SetFeatureState(ctx, trace.Header{Schema: "osmia.trace.transition", Version: trace.Version, ID: "building", Revision: 1, Project: p2, Workstream: m2, At: at, Actor: owner, Cause: "test"}, BuildingState, "test")
	intakefixture.Must(t, err)
	id := "discovery-" + strings.Repeat("a", 64)
	reviewer := trace.Header{Schema: "osmia.trace.document", Version: trace.Version, ID: id, Revision: 1, Project: p2, Workstream: m2, At: at, Actor: trace.Actor{Kind: "agent", ID: "reviewer_thread"}, Cause: "review"}
	content, _ := json.Marshal(workDiscovery{ID: id, Description: "The check run omitted a flag the plan names", Criterion: "spec#1", Disposition: "open"})
	opened := reviewer
	opened.Schema, opened.ID = "osmia.trace.transition", id+"-1"
	_, err = r2.RecordDocumentsWith(ctx, []trace.Document{{Header: reviewer, Path: "discoveries/" + id + ".json", Content: string(content)}}, trace.Transaction{Transition: trace.Transition{Header: opened, Subject: id, To: "open", Reason: "review"}})
	intakefixture.Must(t, err)

	header := trace.Header{Schema: "osmia.trace.turn-request", Version: trace.Version, ID: "request_events_1", Revision: 1, Project: wg.Project(), Workstream: host, At: at, Actor: trace.Actor{Kind: "service", ID: "controller"}, Cause: "test", Depth: 1}
	_, err = wg.EnqueueTurn(ctx, trace.TurnRequest{Header: header, AgentID: trace.ChiefOfStaff, ThreadID: trace.ChiefOfStaff, TurnID: "events_1", Profile: coreadapter.Profile{Name: "default", Backend: "fake", Model: "test"}, SystemPrompt: "test", Prompt: "test"})
	intakefixture.Must(t, err)
	_, err = wg.ClaimTurn(ctx, host, trace.ChiefOfStaff, "token_events_1", "/owned/events_1", at)
	intakefixture.Must(t, err)
	scope := coreadapter.Scope{Project: string(wg.Project()), Workstream: string(host), Thread: trace.ChiefOfStaff, Turn: "events_1", Role: trace.ChiefOfStaff}

	return &memberDiscovery{wg: wg, r2: r2, m2: m2, id: id, scope: scope, now: now}
}

func TestAnOpenDiscoveryOfAWorkgroupMemberHoldsTheMember(t *testing.T) {
	t.Parallel()
	d := openMemberDiscovery(t)
	pending, err := unresolvedWork(d.r2, d.m2)
	intakefixture.Must(t, err)
	if !pending {
		t.Fatal("an open discovery did not hold the member")
	}
}

func TestSharedChiefListsTheDiscoveriesOfAMember(t *testing.T) {
	t.Parallel()
	d := openMemberDiscovery(t)
	raw, err := factoryContext(d.wg, d.scope).Handle(context.Background(), json.RawMessage(`{"path":"discoveries","workstream":"`+string(d.m2)+`"}`))
	intakefixture.Must(t, err)
	var listed map[string]int
	intakefixture.Must(t, json.Unmarshal(raw, &listed))
	if listed["discoveries/"+d.id+".json"] != 1 {
		t.Fatalf("member discoveries listed %s", raw)
	}
}

func TestSharedChiefCannotReadAWorkstreamOutsideItsWorkgroup(t *testing.T) {
	t.Parallel()
	d := openMemberDiscovery(t)
	raw, err := factoryContext(d.wg, d.scope).Handle(context.Background(), json.RawMessage(`{"workstream":"w_00000000000000000000000000000099"}`))
	intakefixture.Must(t, err)
	if !strings.Contains(string(raw), "does not share this chief of staff") {
		t.Fatalf("a workstream outside the workgroup was read: %s", raw)
	}
}

func TestSharedChiefDeclinesAMemberDiscoveryInTheMembersTrace(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	d := openMemberDiscovery(t)
	jevfixture.Fallback(d.r2, d.r2.Project(), d.m2, jev.KindDiscovery, jev.DiscoveryKey(d.id))
	input, _ := json.Marshal(map[string]any{"workstream": d.m2, "id": d.id, "revision": 1, "disposition": "declined", "reason": "The selected checks the service ran cover the changed packages."})
	raw, err := recordDiscovery(d.wg, d.scope, d.now).Handle(ctx, input)
	intakefixture.Must(t, err)
	if !strings.Contains(string(raw), `"recorded":true`) {
		t.Fatalf("member discovery disposition: %s", raw)
	}
	ds, err := discoveries(d.r2, d.m2)
	intakefixture.Must(t, err)
	if got := ds[d.id]; got.Disposition != "declined" || got.Description != "The check run omitted a flag the plan names" {
		t.Fatalf("member trace holds %+v", got)
	}
}

func TestADeclinedMemberDiscoveryNoLongerHoldsTheMember(t *testing.T) {
	t.Parallel()
	d := openMemberDiscovery(t)
	jevfixture.Fallback(d.r2, d.r2.Project(), d.m2, jev.KindDiscovery, jev.DiscoveryKey(d.id))
	input, _ := json.Marshal(map[string]any{"workstream": d.m2, "id": d.id, "revision": 1, "disposition": "declined", "reason": "The selected checks the service ran cover the changed packages."})
	_, err := recordDiscovery(d.wg, d.scope, d.now).Handle(context.Background(), input)
	intakefixture.Must(t, err)
	pending, err := unresolvedWork(d.r2, d.m2)
	intakefixture.Must(t, err)
	if pending {
		t.Fatal("a declined member discovery still held the member")
	}
}

func TestAMasonsRecordDiscoveryOffersNoOtherWorkstream(t *testing.T) {
	t.Parallel()
	d := openMemberDiscovery(t)
	mason := d.scope
	mason.Role = masonRole
	if strings.Contains(string(recordDiscovery(d.wg, mason, d.now).InputSchema), `"workstream"`) {
		t.Fatal("a mason's record_discovery offers another workstream")
	}
}
