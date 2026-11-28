package service

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kpenfound/osmia/internal/api"
	"github.com/kpenfound/osmia/internal/runtime"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	"github.com/kpenfound/osmia/internal/trace"
)

func TestTheManagersRunningTurnIsReportedAsTheManagerRole(t *testing.T) {
	t.Parallel()
	r := newAgentStatusRig(t)
	ctx := r.ctx
	h := r.header
	h.Schema, h.ID, h.Cause, h.Unit = "osmia.trace.turn-request", "request_manager_one", "owner-message", ""
	_, err := r.repo.EnqueueTurn(ctx, trace.TurnRequest{Header: h, AgentID: trace.Manager, ThreadID: trace.Manager, TurnID: "manager_one", Profile: r.profile, Prompt: "Answer"})
	fixturekit.Must(t, err)
	_, err = r.repo.ClaimTurn(ctx, fixturekit.Stream, trace.Manager, "token_manager", "/owned/manager", r.started)
	fixturekit.Must(t, err)

	got, err := agentStatuses(r.repo, fixturekit.Stream, r.started.Add(time.Second))
	fixturekit.Must(t, err)
	if len(got) != 1 || got[0].Role != "manager" {
		t.Fatalf("agent statuses %+v, want one running turn of the role manager", got)
	}
	body, err := json.Marshal(got)
	fixturekit.Must(t, err)
	if strings.Contains(string(body), trace.Manager) {
		t.Fatalf("the status reports the stored name of the manager: %s", body)
	}
	feed := feedSession(mustThread(t, r), mustQueued(t, r))
	if feed.Role != "manager" || feed.Agent != "manager" {
		t.Fatalf("feed session %+v, want role and agent manager", feed)
	}
}

func mustThread(t *testing.T, r *agentStatusRig) trace.Thread {
	t.Helper()
	th, err := r.repo.Thread(fixturekit.Stream, trace.Manager)
	fixturekit.Must(t, err)
	return th
}

func mustQueued(t *testing.T, r *agentStatusRig) trace.QueuedTurn {
	t.Helper()
	return mustThread(t, r).Turns[0]
}

func TestRuntimeStateIsShownWithTheManagerRoleAndStoredUnchanged(t *testing.T) {
	t.Parallel()
	stored := runtime.State{
		Pauses:   []runtime.Pause{{Target: runtime.Target{Scope: "role", Role: trace.Manager}}, {Target: runtime.Target{Scope: "role", Role: "mason"}}},
		Profiles: map[string]string{trace.Manager: "opus", "mason": "sonnet"},
		Archived: []runtime.Archive{{Actor: runtime.ArchiveActorManager}, {}},
	}
	shown := displayState(stored)
	if shown.Pauses[0].Target.Role != "manager" || shown.Pauses[1].Target.Role != "mason" {
		t.Fatalf("pause roles %+v", shown.Pauses)
	}
	if shown.Profiles["manager"] != "opus" || shown.Profiles["mason"] != "sonnet" || len(shown.Profiles) != 2 {
		t.Fatalf("profile overrides %v", shown.Profiles)
	}
	if shown.Archived[0].Actor != "manager" || shown.Archived[1].Actor != "" {
		t.Fatalf("archive actors %+v", shown.Archived)
	}
	if stored.Pauses[0].Target.Role != trace.Manager || stored.Profiles[trace.Manager] != "opus" || stored.Archived[0].Actor != runtime.ArchiveActorManager {
		t.Fatalf("the stored state changed: %+v", stored)
	}
	profiles := displayProfiles(map[string]EffectiveProfile{trace.Manager: {Name: "opus"}, "mason": {Name: "sonnet"}})
	if profiles["manager"].Name != "opus" || profiles["mason"].Name != "sonnet" || len(profiles) != 2 {
		t.Fatalf("effective profiles %v", profiles)
	}
}

func TestTheArchiveActorOfTheManagerIsReportedAsManager(t *testing.T) {
	t.Parallel()
	got := archivedBy(runtime.Archive{Actor: runtime.ArchiveActorManager})
	if got != api.ArchivedByManager {
		t.Fatalf("archived by %q, want %q", got, api.ArchivedByManager)
	}
	if got != "manager" {
		t.Fatalf("archived by %q, want manager", got)
	}
	if got := archivedBy(runtime.Archive{}); got != api.ArchivedByOwner {
		t.Fatalf("archived by %q, want the owner", got)
	}
}

func TestAClientNamingTheManagerRoleByItsDisplayNameReachesTheStoredRole(t *testing.T) {
	t.Parallel()
	for shown, want := range map[string]string{"manager": trace.Manager, trace.Manager: trace.Manager, "mason": "mason"} {
		if got := trace.StoredRole(shown); got != want {
			t.Errorf("a client naming role %q reaches stored role %q, want %q", shown, got, want)
		}
	}
}

func TestTheRuntimeViewNamesTheManagerRoleAndTheStoredStateKeepsItsStoredName(t *testing.T) {
	t.Parallel()
	opts := fixture(t)
	_, c := start(t, opts)
	ctx := context.Background()
	if err := c.Do(ctx, "PUT", Prefix+"/runtime/pause", PauseRequest{Target: runtime.Target{Scope: "role", Role: "manager"}, Mode: "soft", Reason: "test"}, nil); err != nil {
		t.Fatal(err)
	}
	if err := c.Do(ctx, "PUT", Prefix+"/runtime/profile", ProfileRequest{Role: "manager", Profile: "default"}, nil); err != nil {
		t.Fatal(err)
	}
	view, err := c.Runtime(ctx)
	fixturekit.Must(t, err)
	var paused bool
	for _, p := range view.Effective.Pauses {
		if p.Target.Scope == "role" && p.Target.Role == "manager" {
			paused = true
		}
	}
	if !paused || view.Effective.Profiles["manager"] != "default" || view.Profiles["manager"].Name != "" {
		t.Fatalf("runtime view %+v, want the manager paused with a profile override under the role manager", view)
	}
	for role := range view.Profiles {
		if role == trace.Manager {
			t.Fatalf("effective profiles are keyed by the stored name: %v", view.Profiles)
		}
	}
	disk, err := os.ReadFile(filepath.Join(opts.Config.Root, "runtime.json"))
	fixturekit.Must(t, err)
	if !strings.Contains(string(disk), `"`+trace.Manager+`"`) || strings.Contains(string(disk), `"manager"`) {
		t.Fatalf("the stored runtime state does not keep the stored name:\n%s", disk)
	}
	if err := c.Do(ctx, "DELETE", Prefix+"/runtime/pause", ClearPauseRequest{Scope: "role", Role: "manager"}, nil); err != nil {
		t.Fatal(err)
	}
	if err := c.Do(ctx, "DELETE", Prefix+"/runtime/profile", ClearProfileRequest{Role: "manager"}, nil); err != nil {
		t.Fatal(err)
	}
	view, err = c.Runtime(ctx)
	fixturekit.Must(t, err)
	if len(view.Effective.Pauses) != 0 {
		t.Fatalf("the manager's pause remains: %+v", view.Effective.Pauses)
	}
	disk, err = os.ReadFile(filepath.Join(opts.Config.Root, "runtime.json"))
	fixturekit.Must(t, err)
	if strings.Contains(string(disk), trace.Manager) {
		t.Fatalf("the manager's pause or override remains in the stored state:\n%s", disk)
	}
}
