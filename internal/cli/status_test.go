package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"reflect"
	"strings"
	"testing"
	"time"

	wire "github.com/kpenfound/osmia/internal/api"
	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/plan"
	"github.com/kpenfound/osmia/internal/projectstore"
	"github.com/kpenfound/osmia/internal/runtime"
	"github.com/kpenfound/osmia/internal/service"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	"github.com/kpenfound/osmia/internal/status"
	"github.com/kpenfound/osmia/internal/telemetry"
	"github.com/kpenfound/osmia/internal/trace"
)

func TestStatusShowsLiveAgentFactsBelowNarrative(t *testing.T) {
	started := time.Date(2026, 9, 16, 10, 0, 0, 123000000, time.UTC)
	st := wire.WorkstreamStatus{Workstream: stream, Project: project,
		Status: &wire.StatusView{Goal: "Ship uploads.", Agents: []string{"The mason is building uploads."}, UpdatedAt: started, Revision: 1},
		Agents: []wire.AgentStatus{{Role: "mason", Unit: "resume", State: "waiting", StartedAt: started, Elapsed: 7, Profile: "mason-default", Attempt: 2, Path: "replay", QuestionID: "3"}}}
	var out bytes.Buffer
	showStatus(&out, st)
	want := "Agents:\n  The mason is building uploads.\nLive agents:\n  mason unit=resume state=waiting started_at=2026-09-16T10:00:00.123Z elapsed=7s profile=mason-default attempt=2 path=replay question_id=3\nUpdated:"
	if !strings.Contains(out.String(), want) {
		t.Fatalf("status output:\n%s", out.String())
	}
	st.Status = nil
	out.Reset()
	showStatus(&out, st)
	if !strings.Contains(out.String(), "Status: none yet; the chief of staff has not written one\nLive agents:\n  mason") {
		t.Fatalf("live turn without narrative:\n%s", out.String())
	}
}

const quiet = "w_fedcba9876543210fedcba9876543210"

var written = time.Date(2026, 9, 16, 10, 0, 0, 0, time.UTC)

func withStatus(t *testing.T) service.Options {
	t.Helper()
	ctx := context.Background()
	opts := fixture(t)
	cfg, err := projectstore.LoadConfig(opts.Config)
	must(t, err)
	cmd := exec.Command("git", "init", "--quiet", cfg.Project.Clone)
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=" + os.DevNull}
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git init: %v: %s", err, out)
	}
	owner := trace.Actor{Kind: "owner", ID: "local"}
	repo, err := trace.Create(ctx, cfg.Root, cfg.Project, written, owner)
	must(t, err)
	defer repo.Close()
	must(t, repo.CreateWorkstream(ctx, stream, written, owner))
	must(t, repo.CreateWorkstream(ctx, quiet, written, owner))
	h := trace.Header{Schema: "osmia.trace.agent", Version: 1, ID: "chief", Revision: 1, Project: project, Workstream: stream, At: written, Actor: owner, Cause: "workstream-create"}
	must(t, repo.CreateThread(ctx, trace.Agent{Header: h, Role: "chief_of_staff", ThreadID: "chief_thread"}))
	h.Schema, h.ID, h.Cause = "osmia.trace.turn-request", "request_one", "message_one"
	_, err = repo.EnqueueTurn(ctx, trace.TurnRequest{Header: h, AgentID: "chief", ThreadID: "chief_thread", TurnID: "one", Profile: coreadapter.Profile{Name: "default", Backend: "claude", Model: "test"}, Prompt: "Owner message"})
	must(t, err)
	_, err = repo.ClaimTurn(ctx, stream, "chief", "token", "/owned", written)
	must(t, err)
	h.Schema, h.ID, h.Cause = "osmia.trace.transition", "handed", "handin"
	_, err = repo.Transact(ctx, trace.Transaction{Transition: trace.Transition{Header: h, Subject: trace.FeatureSubject, To: "handed", Reason: "Owner handed in a design"}})
	must(t, err)
	scope := coreadapter.Scope{Project: project, Workstream: stream, Thread: "chief_thread", Turn: "one", Role: "chief_of_staff"}
	tool, err := status.Tool(repo, "chief", scope, func() time.Time { return written })
	must(t, err)
	out, err := tool.Handle(ctx, json.RawMessage(`{"goal":"Ship resumable uploads.","note":"The plan is drafted. Review is underway.","agents":["The architect is preparing the packet.","A reviewer is idle."]}`))
	if err != nil || string(out) != `{"stored":true,"revision":1}` {
		t.Fatalf("set_status: %s %v", out, err)
	}
	return opts
}

func startStatusFixture(t *testing.T) (service.Options, string) {
	t.Helper()
	opts := withStatus(t)
	s, err := service.Start(context.Background(), opts)
	must(t, err)
	t.Cleanup(func() { s.Close() })
	return opts, opts.Config.Root
}

func expectedWorkstreamStatuses(t *testing.T, opts service.Options, root string) (full, none wire.WorkstreamStatus) {
	t.Helper()
	cfg, err := projectstore.LoadConfig(opts.Config)
	must(t, err)
	var recorded struct {
		Workgroups []trace.Workgroup `json:"workgroups"`
	}
	must(t, json.Unmarshal([]byte(latestDocument(t, projectDatabase(t, root, cfg.Project.ID), "documents.jsonl", trace.WorkgroupsPath).Content), &recorded))
	workgroup := func(w config.WorkstreamID) *wire.WorkgroupView {
		group, ok := trace.WorkgroupOf(recorded.Workgroups, w)
		if !ok {
			t.Fatalf("workstream %s has no recorded workgroup", w)
		}
		must(t, config.CheckWorkgroupIDs(group.ID))
		return &wire.WorkgroupView{ID: group.ID, Members: []config.WorkstreamID{w}, Edges: []trace.Edge{}}
	}
	handed := "handed"
	full = wire.WorkstreamStatus{DisplayState: "planning", Workgroup: workgroup(stream), Workstream: stream, Project: project, State: &handed, Units: []wire.UnitStatus{}, Advisories: []wire.OverlapAdvisory{}, Gates: []trace.OwnerGate{}, Agents: []wire.AgentStatus{}, ContextMode: "file", Workspaces: "git", TelemetryTraceID: telemetry.WorkstreamTrace(stream).String(), TelemetryRootSpanID: telemetry.WorkstreamRoot(stream).String(), Status: &wire.StatusView{
		Goal: "Ship resumable uploads.", Note: "The plan is drafted. Review is underway.",
		Agents: []string{"The architect is preparing the packet.", "A reviewer is idle."}, Revision: 1, UpdatedAt: written}}
	none = wire.WorkstreamStatus{DisplayState: "planning", Workgroup: workgroup(quiet), Workstream: quiet, Project: project, Units: []wire.UnitStatus{}, Advisories: []wire.OverlapAdvisory{}, Gates: []trace.OwnerGate{}, Agents: []wire.AgentStatus{}, ContextMode: "file", Workspaces: "git", TelemetryTraceID: telemetry.WorkstreamTrace(quiet).String(), TelemetryRootSpanID: telemetry.WorkstreamRoot(quiet).String()}
	return full, none
}

func TestStatusOverviewShowsProfilesCapacityAndWorkstreams(t *testing.T) {
	_, root := startStatusFixture(t)
	overview := successful(t, root, "status")
	for _, want := range []string{
		"ready=true",
		"Profiles:\n",
		"Jev: unconfigured model=",
		"  mason: 0 of 4 slot(s) in use\n  reviewer: 0 of 2 slot(s) in use\n  committee: 0 of 3 slot(s) in use\n" +
			"  architect: 0 in use, unbounded (no factory-wide limit; at most one turn per workstream)\n" +
			"  checks: 0 of 2 slot(s) in use\n",
		"Workspaces: setting=auto new_workstreams=",
		"Workstreams:\n" +
			"  " + stream + " state=handed open_questions=0 context_mode=file workspaces=git\n" +
			"    Goal: Ship resumable uploads.\n" +
			"    Attention: none\n" +
			"  " + quiet + " state=not recorded open_questions=0 context_mode=file workspaces=git\n" +
			"    no status yet\n",
	} {
		if !strings.Contains(overview, want) {
			t.Fatalf("overview lacks %q:\n%s", want, overview)
		}
	}
}

func TestStatusOverviewShowsNoPoolAsZeroSlots(t *testing.T) {
	_, root := startStatusFixture(t)
	overview := successful(t, root, "status")
	if strings.Contains(overview, "of 0 slot") || strings.Contains(overview, "0/0") {
		t.Fatalf("overview shows a pool as zero slots:\n%s", overview)
	}
}

func TestStatusOverviewLeavesOutTheNote(t *testing.T) {
	_, root := startStatusFixture(t)
	if overview := successful(t, root, "status"); strings.Contains(overview, "The plan is drafted") {
		t.Fatalf("overview shows the note:\n%s", overview)
	}
}

func TestStatusJSONHoldsEveryWorkstreamProfilesCapacityAndWorkspaces(t *testing.T) {
	opts, root := startStatusFixture(t)
	full, none := expectedWorkstreamStatuses(t, opts, root)
	var all struct {
		Health        wire.HealthResponse
		Configuration wire.ConfigResponse
		Runtime       wire.RuntimeResponse
		Status        wire.StatusResponse
	}
	must(t, json.Unmarshal([]byte(successful(t, root, "status", "--json")), &all))
	capacity := &wire.CapacityStatus{PerWorkstream: 2, Roles: []wire.RoleCapacity{{Role: "mason", Limit: fixturekit.Limit(4), Waiting: []wire.SlotWait{}}, {Role: "reviewer", Limit: fixturekit.Limit(2), Waiting: []wire.SlotWait{}}, {Role: "committee", Limit: fixturekit.Limit(3), Waiting: []wire.SlotWait{}}, fixturekit.Unbounded("architect", 0, []wire.SlotWait{}), {Role: "checks", Limit: fixturekit.Limit(2), Waiting: []wire.SlotWait{}}}}
	if all.Status.ProviderUsage == nil || all.Status.ProviderUsage.Unattributed != nil {
		t.Fatalf("status --json provider usage: %+v", all.Status.ProviderUsage)
	}
	if !all.Health.Ready || all.Configuration.Project == nil || !reflect.DeepEqual(all.Status, wire.StatusResponse{Workstreams: []wire.WorkstreamStatus{full, none}, Profiles: all.Runtime.Profiles, Capacity: capacity, ProviderUsage: all.Status.ProviderUsage, Diagnostics: []wire.Diagnostic{}, Workspaces: all.Status.Workspaces}) {
		t.Fatalf("status --json: %+v", all.Status)
	}
	if ws := all.Status.Workspaces; ws.Setting != "auto" || ws.Backend != "git" && ws.Backend != "jujutsu" || ws.Problem != "" {
		t.Fatalf("status --json workspaces: %+v", ws)
	}
}

func TestStatusOfOneWorkstreamShowsItsNarrative(t *testing.T) {
	_, root := startStatusFixture(t)
	one := successful(t, root, "status", stream)
	if want := "Workstream: " + stream + " state=handed open_questions=0 context_mode=file workspaces=git\n" +
		"Goal: Ship resumable uploads.\n" +
		"Attention: none\n" +
		"Note: The plan is drafted. Review is underway.\n" +
		"Agents:\n" +
		"  The architect is preparing the packet.\n" +
		"  A reviewer is idle.\n" +
		"Updated: 2026-09-16T10:00:00Z (revision 1)\n"; one != want {
		t.Fatalf("status <workstream>:\n%s\nwant:\n%s", one, want)
	}
}

func TestStatusJSONOfOneWorkstream(t *testing.T) {
	opts, root := startStatusFixture(t)
	full, _ := expectedWorkstreamStatuses(t, opts, root)
	var got wire.WorkstreamStatus
	must(t, json.Unmarshal([]byte(successful(t, root, "status", stream, "--json")), &got))
	if !reflect.DeepEqual(got, full) {
		t.Fatalf("status <workstream> --json: %+v", got)
	}
}

func TestStatusOfAWorkstreamWithoutAStatusSaysSo(t *testing.T) {
	_, root := startStatusFixture(t)
	if out := successful(t, root, "status", quiet); out != "Workstream: "+quiet+" state=not recorded open_questions=0 context_mode=file workspaces=git\nStatus: none yet; the chief of staff has not written one\n" {
		t.Fatalf("no status:\n%s", out)
	}
}

func TestStatusJSONOfAWorkstreamWithoutAStatusHoldsNulls(t *testing.T) {
	opts, root := startStatusFixture(t)
	_, none := expectedWorkstreamStatuses(t, opts, root)
	out := successful(t, root, "status", "--json", quiet)
	var got wire.WorkstreamStatus
	must(t, json.Unmarshal([]byte(out), &got))
	if !reflect.DeepEqual(got, none) {
		t.Fatalf("no status --json: %+v", got)
	}
	raw := map[string]json.RawMessage{}
	must(t, json.Unmarshal([]byte(out), &raw))
	if string(raw["status"]) != "null" || string(raw["state"]) != "null" || string(raw["units"]) != "[]" || string(raw["gates"]) != "[]" || string(raw["context_mode"]) != `"file"` || string(raw["open_questions"]) != "0" || string(raw["drift"]) != "null" {
		t.Fatalf("no status JSON: %v", raw)
	}
}

func TestStatusOfAnUnknownWorkstreamIsNotFound(t *testing.T) {
	_, root := startStatusFixture(t)
	unknown := "w_00000000000000000000000000000009"
	code, out, diag := invoke(t, root, "status", unknown)
	if code != 4 || out != "" || diag != "not_found: workstream "+unknown+" is not in an active project; list workstreams with osmia status\n" {
		t.Fatalf("unknown workstream: %d %q %q", code, out, diag)
	}
}

func TestStatusRefusesBadWorkstreamArguments(t *testing.T) {
	_, root := startStatusFixture(t)
	for _, args := range [][]string{{"status", "not-a-workstream"}, {"status", project}, {"status", stream, quiet}} {
		code, out, diag := invoke(t, root, args...)
		if code != 2 || out != "" || !strings.Contains(diag, "invalid arguments") {
			t.Fatalf("%v: %d %q %q", args, code, out, diag)
		}
	}
}

func TestStatusWithoutWorkstreamsListsNone(t *testing.T) {
	opts := fixture(t)
	s, err := service.Start(context.Background(), opts)
	must(t, err)
	t.Cleanup(func() { s.Close() })
	root := opts.Config.Root
	if out := successful(t, root, "status"); !strings.HasSuffix(out, "Workstreams:\n  none\n") {
		t.Fatalf("no workstreams:\n%s", out)
	}
	code, out, diag := invoke(t, root, "status", stream)
	if code != 4 || out != "" || !strings.HasPrefix(diag, "not_found: workstream "+stream) {
		t.Fatalf("no trace: %d %q %q", code, out, diag)
	}
}

func TestStatusWithoutAProjectNamesProjectAdd(t *testing.T) {
	empty, _ := emptyFixture(t)
	idle, err := service.Start(context.Background(), empty)
	must(t, err)
	t.Cleanup(func() { idle.Close() })
	code, out, diag := invoke(t, empty.Config.Root, "status", stream)
	if code != 4 || out != "" || diag != "no_project: no project is configured; add one with osmia project add\n" {
		t.Fatalf("no project: %d %q %q", code, out, diag)
	}
	if out := successful(t, empty.Config.Root, "status"); !strings.Contains(out, "Workstreams:\n  none\n") {
		t.Fatalf("no project:\n%s", out)
	}
}

func TestStatusTextShowsWorkspaces(t *testing.T) {
	for _, c := range []struct {
		ws   wire.WorkspacesStatus
		want string
	}{
		{wire.WorkspacesStatus{Setting: "jujutsu", JJ: "0.44.0", Problem: "jj is too old"}, "Workspaces: setting=jujutsu new_workstreams=none jj=0.44.0\n"},
		{wire.WorkspacesStatus{Setting: "git", Backend: "git"}, "Workspaces: setting=git new_workstreams=git\n"},
		{wire.WorkspacesStatus{Setting: "auto", Backend: "jujutsu", JJ: "0.45.1"}, "Workspaces: setting=auto new_workstreams=jujutsu jj=0.45.1\n"},
	} {
		var out strings.Builder
		showWorkspaces(&out, c.ws)
		if out.String() != c.want {
			t.Fatalf("%+v: got %q, want %q", c.ws, out.String(), c.want)
		}
	}
}

func statusWithoutAttentionOrAgents() wire.WorkstreamStatus {
	return wire.WorkstreamStatus{Workstream: stream, Project: project, OpenQuestions: 2, ContextMode: "file", Workspaces: "git", Status: &wire.StatusView{
		Goal: "Ship resumable uploads.", Note: "Work is starting.", Agents: []string{}, Revision: 3, UpdatedAt: written}}
}

func TestStatusTextWithoutAttentionOrAgents(t *testing.T) {
	var one strings.Builder
	showStatus(&one, statusWithoutAttentionOrAgents())
	if want := "Workstream: " + stream + " state=not recorded open_questions=2 context_mode=file workspaces=git\n" +
		"Goal: Ship resumable uploads.\nAttention: none\nNote: Work is starting.\nAgents:\n  none active\n" +
		"Updated: 2026-09-16T10:00:00Z (revision 3)\n"; one.String() != want {
		t.Fatalf("got:\n%s\nwant:\n%s", one.String(), want)
	}
}

func TestStatusTextListsTheUnitsOfABuildingWorkstream(t *testing.T) {
	st := statusWithoutAttentionOrAgents()
	building := "building"
	st.State, st.Units = &building, []wire.UnitStatus{{Unit: "parser", State: "ready"}, {Unit: "validator", State: "planned"}}
	var one strings.Builder
	showStatus(&one, st)
	if want := "Workstream: " + stream + " state=building open_questions=2 context_mode=file workspaces=git\n" +
		"Units:\n  parser ready\n  validator planned\n" +
		"Goal: Ship resumable uploads.\nAttention: none\nNote: Work is starting.\nAgents:\n  none active\n" +
		"Updated: 2026-09-16T10:00:00Z (revision 3)\n"; one.String() != want {
		t.Fatalf("got:\n%s\nwant:\n%s", one.String(), want)
	}
}

func TestStatusOverviewShowsTheWorkstreamAndTheDiagnostic(t *testing.T) {
	var all strings.Builder
	showWorkstreams(&all, wire.StatusResponse{Workstreams: []wire.WorkstreamStatus{statusWithoutAttentionOrAgents()}, Diagnostics: []wire.Diagnostic{{Field: "workstreams", Code: wire.Internal, Message: "cannot read"}}})
	if want := "Workstreams:\n  " + stream + " state=not recorded open_questions=2 context_mode=file workspaces=git\n" +
		"    Goal: Ship resumable uploads.\n    Attention: none\nDiagnostic: workstreams: internal: cannot read\n"; all.String() != want {
		t.Fatalf("got:\n%s\nwant:\n%s", all.String(), want)
	}
}

func TestStatusOverviewWithoutWorkstreamsShowsTheDiagnostic(t *testing.T) {
	var all strings.Builder
	showWorkstreams(&all, wire.StatusResponse{Diagnostics: []wire.Diagnostic{{Field: "workstreams", Code: wire.Internal, Message: "cannot read"}}})
	if want := "Workstreams:\n  none\nDiagnostic: workstreams: internal: cannot read\n"; all.String() != want {
		t.Fatalf("got:\n%s\nwant:\n%s", all.String(), want)
	}
}

func TestStatusOverviewOmitsArchivedWorkstreams(t *testing.T) {
	building := "building"
	live := wire.WorkstreamStatus{Workstream: stream, State: &building, OpenQuestions: 2, ContextMode: "file", Workspaces: "git", Status: &wire.StatusView{
		Goal: "Ship resumable uploads.", Attention: "Review the plan."}}
	archived := wire.WorkstreamStatus{Workstream: quiet, Archived: true, Status: &wire.StatusView{Goal: "Retired work."}}
	for _, tc := range []struct {
		name        string
		workstreams []wire.WorkstreamStatus
		diagnostics []wire.Diagnostic
		want        string
	}{
		{
			name:        "mixed",
			workstreams: []wire.WorkstreamStatus{archived, live},
			want: "Workstreams:\n  " + stream + " state=building open_questions=2 context_mode=file workspaces=git\n" +
				"    Goal: Ship resumable uploads.\n    Attention: Review the plan.\n",
		},
		{
			name:        "all archived",
			workstreams: []wire.WorkstreamStatus{archived, {Workstream: stream, Archived: true}},
			want:        "Workstreams:\n  none\n",
		},
		{
			name:        "all archived with diagnostics",
			workstreams: []wire.WorkstreamStatus{archived, {Workstream: stream, Archived: true}},
			diagnostics: []wire.Diagnostic{{Field: "workstreams", Code: wire.Internal, Message: "cannot read"}},
			want:        "Workstreams:\n  none\nDiagnostic: workstreams: internal: cannot read\n",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var out strings.Builder
			showWorkstreams(&out, wire.StatusResponse{Workstreams: tc.workstreams, Diagnostics: tc.diagnostics})
			if out.String() != tc.want {
				t.Fatalf("got:\n%s\nwant:\n%s", out.String(), tc.want)
			}
		})
	}
}

func TestStatusPrintsTheDailyBudget(t *testing.T) {
	var out strings.Builder
	showDailyBudget(&out, nil)
	showDailyBudget(&out, &wire.DailyBudgetStatus{Day: "2026-09-24", SpendUSD: "12.5", LimitUSD: "150.00"})
	showDailyBudget(&out, &wire.DailyBudgetStatus{Day: "2026-09-24", SpendUSD: "150", LimitUSD: "150.00", UnknownCosts: 2, LowerBound: true})
	if want := "Daily budget: USD 12.5 of USD 150.00 spent on 2026-09-24\n" +
		"Daily budget: USD 150 of USD 150.00 spent on 2026-09-24 (at least; 2 attempt(s) have unknown cost)\n"; out.String() != want {
		t.Fatalf("got:\n%s\nwant:\n%s", out.String(), want)
	}
}

func TestStatusPrintsFailureStreaks(t *testing.T) {
	var out strings.Builder
	showFailureStreaks(&out, nil)
	at := time.Date(2026, 9, 24, 12, 0, 0, 0, time.UTC)
	showFailureStreaks(&out, []wire.FailureStreak{{Role: "mason", Profile: "default", Consecutive: 3, LastFailure: "the agent exited with code 1", LastAt: at}, {Role: "reviewer", Profile: "backup", Consecutive: 1, LastFailure: "timed out", LastAt: at.Add(time.Minute)}})
	if want := "Infrastructure failures: mason on profile default failed 3 time(s) in a row; last at 2026-09-24T12:00:00Z: the agent exited with code 1\n" +
		"Infrastructure failures: reviewer on profile backup failed 1 time(s) in a row; last at 2026-09-24T12:01:00Z: timed out\n"; out.String() != want {
		t.Fatalf("got:\n%s\nwant:\n%s", out.String(), want)
	}
}

func TestStatusPrintsOwnerGates(t *testing.T) {
	st := wire.WorkstreamStatus{Workstream: stream, Project: project, ContextMode: "file", Gates: []trace.OwnerGate{{Kind: "escalation", Reference: "12"}, {Kind: "contested", Reference: "upload-index"}}}
	var one, all strings.Builder
	showStatus(&one, st)
	showWorkstreams(&all, wire.StatusResponse{Workstreams: []wire.WorkstreamStatus{st}})
	for _, pair := range [][2]string{{one.String(), "Gate: escalation 12\nGate: contested upload-index\n"}, {all.String(), "    Gate: escalation 12\n    Gate: contested upload-index\n"}} {
		if !strings.Contains(pair[0], pair[1]) {
			t.Fatalf("status lacks gates:\n%s", pair[0])
		}
	}
}

func TestStatusPrintsLatestUnitCard(t *testing.T) {
	st := wire.WorkstreamStatus{Workstream: stream, Project: project, ContextMode: "file", Units: []wire.UnitStatus{
		{Unit: "parser", State: "reviewing", Card: &coreadapter.Card{Headline: "Parser is ready", Happened: "The parser accepts resumed chunks.", NeedsYou: "Review the candidate."}},
		{Unit: "validator", State: "ready"},
	}}
	var out strings.Builder
	showStatus(&out, st)
	for _, want := range []string{"parser reviewing\n    Headline: Parser is ready\n    Happened: The parser accepts resumed chunks.\n    Needs you: Review the candidate.\n", "validator ready\n", "Status: none yet"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("status lacks %q: %s", want, out.String())
		}
	}
}

func TestStatusPrintsWhyAReadyUnitWaits(t *testing.T) {
	waits := "Waits for units in flight it is entangled with: parser (their footprints overlap)."
	st := wire.WorkstreamStatus{Workstream: stream, Project: project, ContextMode: "file", Units: []wire.UnitStatus{
		{Unit: "parser", State: "implementing"},
		{Unit: "validator", State: "ready", Reason: waits, Deferral: &wire.UnitDispatch{Unit: "validator", Version: 1, Decision: service.DispatchDeferred, Reason: service.DeferEntangled,
			Blockers: []plan.StartBlocker{{Unit: "parser", Reason: plan.OverlapReason}}, Message: waits}},
	}}
	var out strings.Builder
	showStatus(&out, st)
	if want := "Units:\n  parser implementing\n  validator ready\n    Waiting: " + waits + "\nStatus: none yet"; !strings.Contains(out.String(), want) {
		t.Fatalf("status lacks why the unit waits:\n%s", out.String())
	}
}

func TestStatusPrintsUnitLanding(t *testing.T) {
	st := wire.WorkstreamStatus{Workstream: stream, Project: project, ContextMode: "file", Units: []wire.UnitStatus{
		{Unit: "parser", State: "merged", Landing: &wire.UnitLanding{Unit: "parser", Approval: "units/parser/review.json revision 2", Candidate: "c0ffee", Base: "ba5e", Criteria: []string{"spec#1", "spec#3"}, Branch: "osmia/feature", Commit: "1a4d3d"}},
		{Unit: "validator", State: "ready"},
	}}
	var out strings.Builder
	showStatus(&out, st)
	want := "Units:\n  parser merged\n    Landed: 1a4d3d on osmia/feature\n    Candidate: c0ffee from ba5e, approved by units/parser/review.json revision 2\n    Criteria: spec#1, spec#3\n  validator ready\n"
	if !strings.Contains(out.String(), want) {
		t.Fatalf("status lacks the landing:\n%s", out.String())
	}
}

func TestStatusPrintsOverlapAdvisories(t *testing.T) {
	warning := "Workstream w_00000000000000000000000000000002 of this project builds in subsystem internal: both sealed footprints cover internal.trace (seal 1 of this workstream, seal 1 of that one)."
	st := wire.WorkstreamStatus{Workstream: stream, Project: project, ContextMode: "file", Advisories: []wire.OverlapAdvisory{{Workstream: "w_00000000000000000000000000000002", Seal: 1, OtherSeal: 1, Subsystems: []string{"internal"}, Entities: []string{"internal.trace"}, Paths: []string{"internal/trace"}, Message: warning}}}
	var one, all strings.Builder
	showStatus(&one, st)
	showWorkstreams(&all, wire.StatusResponse{Workstreams: []wire.WorkstreamStatus{st}})
	for _, pair := range [][2]string{{one.String(), "\nOverlap: " + warning + "\n"}, {all.String(), "\n    Overlap: " + warning + "\n"}} {
		if !strings.Contains(pair[0], pair[1]) {
			t.Fatalf("status lacks the advisory:\n%s", pair[0])
		}
	}
}

const driftSkipReason = "drift rebase 1 changed nothing: the workstream is handed, not building, assembled or delivered with a pull request"

func startDriftRoot(t *testing.T) string {
	t.Helper()
	opts := withStatus(t)
	ctx := context.Background()
	cfg, err := projectstore.LoadConfig(opts.Config)
	must(t, err)
	repo, err := trace.Open(cfg.Root, cfg.Project)
	must(t, err)
	h := trace.Header{Schema: "osmia.trace.transition", Version: 1, ID: "drift-1-skipped", Revision: 1, Project: project, Workstream: stream, At: written, Actor: trace.Actor{Kind: "service", ID: "foreman"}, Cause: "drift-1-run"}
	_, err = repo.Transact(ctx, trace.Transaction{Transition: trace.Transition{Header: h, Subject: "drift", To: "skipped-1", Reason: driftSkipReason}})
	must(t, err)
	must(t, repo.Close())
	s, err := service.Start(ctx, opts)
	must(t, err)
	t.Cleanup(func() { s.Close() })
	return opts.Config.Root
}

func TestStatusOverviewShowsTheDriftRebase(t *testing.T) {
	root := startDriftRoot(t)
	if out := successful(t, root, "status"); !strings.Contains(out, "  "+stream+" state=handed open_questions=0 context_mode=file workspaces=git\n    Drift: rebase 1 skipped at 2026-09-16T10:00:00Z\n    Goal: ") {
		t.Fatalf("overview lacks the drift rebase:\n%s", out)
	}
}

func TestStatusOfAWorkstreamShowsTheDriftRebaseAndItsReason(t *testing.T) {
	root := startDriftRoot(t)
	if out := successful(t, root, "status", stream); !strings.Contains(out, "\nDrift: rebase 1 skipped at 2026-09-16T10:00:00Z\n  Reason: "+driftSkipReason+"\n") {
		t.Fatalf("status <workstream> lacks the drift rebase:\n%s", out)
	}
}

func TestStatusJSONCarriesTheDriftRebase(t *testing.T) {
	root := startDriftRoot(t)
	var one wire.WorkstreamStatus
	must(t, json.Unmarshal([]byte(successful(t, root, "status", stream, "--json")), &one))
	if want := (&wire.DriftStatus{Drift: 1, Outcome: "skipped", At: written, Reason: driftSkipReason, Moved: []string{}}); !reflect.DeepEqual(one.Drift, want) {
		t.Fatalf("status drift %+v, want %+v", one.Drift, want)
	}
}

func skippedDriftWorkstreams() []wire.DriftSkip {
	return []wire.DriftSkip{{Workstream: stream, Reason: "the workstream is handed, not building, assembled or delivered with a pull request"}, {Workstream: quiet, Reason: "the workstream is not started, not building, assembled or delivered with a pull request"}}
}

func TestProjectRebaseReportsTheSkippedWorkstreams(t *testing.T) {
	root := startDriftRoot(t)
	skipped := skippedDriftWorkstreams()
	if out := successful(t, root, "project", "rebase", project); out != "Drift rebase requested for project "+project+"\nCovered: none\nSkipped: "+stream+": "+skipped[0].Reason+"\nSkipped: "+quiet+": "+skipped[1].Reason+"\n" {
		t.Fatalf("project rebase:\n%s", out)
	}
}

func TestProjectRebaseReportsTheSkippedWorkstreamsAsJSON(t *testing.T) {
	root := startDriftRoot(t)
	var got wire.ProjectRebaseResponse
	must(t, json.Unmarshal([]byte(successful(t, root, "project", "rebase", project, "--json")), &got))
	if want := (wire.ProjectRebaseResponse{Project: project, Covered: []wire.DriftCoverage{}, Skipped: skippedDriftWorkstreams()}); !reflect.DeepEqual(got, want) {
		t.Fatalf("project rebase --json: %+v", got)
	}
}

func TestProjectRebaseRefusesAProjectThatIsNotActive(t *testing.T) {
	root := startDriftRoot(t)
	other := "p_ffffffffffffffffffffffffffffffff"
	if code, out, diag := invoke(t, root, "project", "rebase", other); code != 4 || out != "" || !strings.Contains(diag, "not_found: project "+other+" is not an active project") {
		t.Fatalf("project rebase of another project: %d %s %s", code, out, diag)
	}
}

func TestStatusShowsTheTailnetListener(t *testing.T) {
	var out bytes.Buffer
	showTailnet(&out, nil)
	showTailnet(&out, &wire.TailnetStatus{State: wire.TailnetUp})
	showTailnet(&out, &wire.TailnetStatus{State: wire.TailnetNeedsLogin, LoginURL: "https://login.example/a/1"})
	showTailnet(&out, &wire.TailnetStatus{State: wire.TailnetDown, Reason: "control unreachable"})
	if want := "Tailnet: up\nTailnet: needs_login login=https://login.example/a/1\nTailnet: down (control unreachable)\n"; out.String() != want {
		t.Fatalf("tailnet lines:\n%s", out.String())
	}

	opts := fixture(t)
	path := opts.Config.Root + "/config.toml"
	top, err := os.ReadFile(path)
	must(t, err)
	must(t, os.WriteFile(path, append(top, []byte("[listen]\ntailnet=\"osmia\"\n")...), 0600))
	opts.JoinTailnet = func(string, string) (service.TailnetListener, error) {
		return nil, os.ErrPermission
	}
	s, err := service.Start(context.Background(), opts)
	must(t, err)
	t.Cleanup(func() { s.Close() })
	overview := successful(t, opts.Config.Root, "status")
	if !strings.Contains(overview, "ready=true") || !strings.Contains(overview, "Tailnet: down (permission denied)\n") {
		t.Fatalf("status:\n%s", overview)
	}
}

func TestStatusPrintsCapacity(t *testing.T) {
	var out strings.Builder
	showCapacity(&out, nil)
	showCapacity(&out, &wire.CapacityStatus{PerWorkstream: 2, Roles: []wire.RoleCapacity{
		{Role: "mason", Used: 1, Limit: fixturekit.Limit(1), Waiting: []wire.SlotWait{
			{Workstream: stream, Unit: "parser", Agent: "mason-parser", Turn: "mason-parser-1", Reason: "capacity"},
			{Workstream: quiet, Unit: "validator", Reason: "priority"}}},
		{Role: "reviewer", Used: 0, Limit: fixturekit.Limit(2), Waiting: []wire.SlotWait{}},
		fixturekit.Unbounded("architect", 1, []wire.SlotWait{}),
		{Role: "checks", Used: 2, Limit: fixturekit.Limit(2), Waiting: []wire.SlotWait{
			{Workstream: stream, Unit: "parser", Reason: "capacity"},
			{Workstream: stream, Review: 3, Reason: "capacity"},
			{Workstream: quiet, Session: true, Reason: "capacity"},
		}},
	}})
	if want := "Capacity (per workstream 2):\n" +
		"  mason: 1 of 1 slot(s) in use\n" +
		"    Waiting: " + stream + " turn mason-parser-1 of mason-parser (capacity)\n" +
		"    Waiting: " + quiet + " unit validator (priority)\n" +
		"  reviewer: 0 of 2 slot(s) in use\n" +
		"  architect: 1 in use, unbounded (no factory-wide limit; at most one turn per workstream)\n" +
		"  checks: 2 of 2 slot(s) in use\n" +
		"    Waiting: " + stream + " unit parser (capacity)\n" +
		"    Waiting: " + stream + " final review 3 (capacity)\n" +
		"    Waiting: " + quiet + " session (capacity)\n"; out.String() != want {
		t.Fatalf("got:\n%s\nwant:\n%s", out.String(), want)
	}
}

func TestStatusPrintsProviderUsage(t *testing.T) {
	var out strings.Builder
	showProviderUsage(&out, nil)
	resets := time.Date(2026, 9, 24, 17, 0, 0, 0, time.UTC)
	showProviderUsage(&out, &wire.ProviderUsageStatus{Day: "2026-09-24", Providers: []wire.ProviderUsage{
		{Provider: "claude", ProviderSpend: wire.ProviderSpend{SpendUSD: "3.5"}, Limit: &runtime.ProviderLimit{Backend: "claude", Status: "rejected", Kind: "five_hour", ResetsAt: resets},
			Fallbacks: []wire.RoleFallback{{Role: "mason", Configured: "claude", Profile: "codex-fast"}}, PausedRoles: []string{"reviewer"}},
		{Provider: "codex", ProviderSpend: wire.ProviderSpend{SpendUSD: "1", UnknownCosts: 2, LowerBound: true}, Limit: &runtime.ProviderLimit{Backend: "codex", Status: "rejected"}},
	}, Unattributed: &wire.ProviderSpend{SpendUSD: "0.25"}})
	if want := "Provider usage on 2026-09-24:\n" +
		"  claude: USD 3.5 known spend\n" +
		"    Limit: rejected kind=five_hour until 2026-09-24T17:00:00Z\n" +
		"    Fallback: mason runs codex-fast in place of claude\n" +
		"    Paused: reviewer has no fallback available\n" +
		"  codex: USD 1 known spend (at least; 2 attempt(s) have unknown cost)\n" +
		"    Limit: rejected kind= until cleared\n" +
		"  unattributed: USD 0.25 known spend\n"; out.String() != want {
		t.Fatalf("got:\n%s\nwant:\n%s", out.String(), want)
	}
}

type configRoot struct {
	root, top string
	original  []byte
	loaded    wire.ConfigResponse
}

func startConfigRoot(t *testing.T) configRoot {
	t.Helper()
	opts := fixture(t)
	s, err := service.Start(context.Background(), opts)
	must(t, err)
	t.Cleanup(func() { s.Close() })
	c := configRoot{root: opts.Config.Root}
	must(t, json.Unmarshal([]byte(successful(t, c.root, "config", "--json")), &c.loaded))
	if len(c.loaded.Drift.Files) != 1 || len(c.loaded.Effective.Projects) != 1 {
		t.Fatalf("drift files %+v, projects %+v", c.loaded.Drift.Files, c.loaded.Effective.Projects)
	}
	c.top = c.loaded.Drift.Files[0].Path
	c.original, err = os.ReadFile(c.top)
	must(t, err)
	return c
}

func (c configRoot) append(t *testing.T, text string) {
	t.Helper()
	must(t, os.WriteFile(c.top, append(append([]byte{}, c.original...), text...), 0600))
}

func TestConfigShowsTheLoadedConfigurationAsUnchanged(t *testing.T) {
	c := startConfigRoot(t)
	p := c.loaded.Effective.Projects[0]
	registered := fmt.Sprintf("Registered project: %s (%s) upstream=%s push-repository=%s clone=%s base_branch=%s\n", p.ID, p.Name, p.Upstream, p.PushRepository(), p.Clone, p.BaseBranch)
	errorReports := "[error_reports]\nenabled = false\nrepository = \"kpenfound/osmia\"\nlabels = [\"factory-error\", \"automated\"]\nmax_issues_per_hour = 5\nmax_issues_per_day = 20\nmax_comments_per_day = 20\nsources = [\"api\", \"operation\", \"github\"]\n"
	if want := "Configuration: " + c.loaded.Digest + " (" + c.loaded.Root + ")\nConfig file: " + c.top + ": unchanged\n" + registered + errorReports; successful(t, c.root, "config") != want {
		t.Fatalf("config as loaded:\n%s\nwant:\n%s", successful(t, c.root, "config"), want)
	}
}

func TestConfigNamesAnInvalidFileOnDisk(t *testing.T) {
	c := startConfigRoot(t)
	c.append(t, "[capacity]\nmasons = 0\n")
	text := successful(t, c.root, "config")
	if !strings.Contains(text, "Config file: "+c.top+": invalid: capacity.masons: must be positive\n") || !strings.Contains(text, "Diagnostic: configuration: validation: ") {
		t.Fatalf("config with an invalid file:\n%s", text)
	}
}

func TestStatusNamesAnInvalidConfigFileAndStillShowsTheService(t *testing.T) {
	c := startConfigRoot(t)
	c.append(t, "[capacity]\nmasons = 0\n")
	if status := successful(t, c.root, "status"); !strings.Contains(status, "Config file: "+c.top+": invalid: capacity.masons: must be positive\n") || !strings.Contains(status, "Capacity (per workstream ") || !strings.Contains(status, "Provider usage on ") {
		t.Fatalf("status with an invalid file:\n%s", status)
	}
}

func TestConfigJSONReportsAnEditedFileAsDriftFromTheLoadedDigest(t *testing.T) {
	c := startConfigRoot(t)
	c.append(t, "[capacity]\nmasons = 7\n")
	var drift wire.ConfigResponse
	must(t, json.Unmarshal([]byte(successful(t, c.root, "config", "--json")), &drift))
	if !drift.Drift.Differs || drift.Drift.Files[0].State != wire.ConfigChanged || drift.Digest != c.loaded.Digest {
		t.Fatalf("config with an edited file: %+v", drift)
	}
}

func TestConfigRefusesArguments(t *testing.T) {
	c := startConfigRoot(t)
	if code, out, diag := invoke(t, c.root, "config", "extra"); code != 2 || out != "" || !strings.Contains(diag, "invalid arguments") {
		t.Fatalf("config with arguments: %d %q %q", code, out, diag)
	}
}

func TestShowErrorReportsPrintsEffectiveValues(t *testing.T) {
	t.Parallel()
	var out bytes.Buffer
	showErrorReports(&out, config.ErrorReports{Enabled: true, Repository: "owner/name", Labels: []string{"bug"}, MaxIssuesPerHour: 1, MaxIssuesPerDay: 2, MaxCommentsPerDay: 3, Sources: []string{"github"}})
	want := "[error_reports]\nenabled = true\nrepository = \"owner/name\"\nlabels = [\"bug\"]\nmax_issues_per_hour = 1\nmax_issues_per_day = 2\nmax_comments_per_day = 3\nsources = [\"github\"]\n"
	if out.String() != want {
		t.Fatalf("got:\n%s\nwant:\n%s", out.String(), want)
	}
}

func TestStatusShowsTailnetHTTPS(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		status wire.TailnetStatus
		line   string
	}{
		{wire.TailnetStatus{State: "up", HTTPSState: "up", HTTPSAddress: "https://osmia.tail0.ts.net/"}, "Tailnet HTTPS: up https://osmia.tail0.ts.net/\n"},
		{wire.TailnetStatus{State: "up", HTTPSState: "off", HTTPSReason: "disabled by listen.tailnet_https"}, "Tailnet HTTPS: off (disabled by listen.tailnet_https)\n"},
		{wire.TailnetStatus{State: "up", HTTPSState: "unavailable", HTTPSReason: "enable HTTPS certificates in the tailnet admin console"}, "Tailnet HTTPS: unavailable (enable HTTPS certificates in the tailnet admin console)\n"},
		{wire.TailnetStatus{State: "up", HTTPSState: "unavailable", HTTPSReason: "certificate retrieval failed"}, "Tailnet HTTPS: unavailable (certificate retrieval failed)\n"},
	} {
		var out bytes.Buffer
		showTailnet(&out, &tc.status)
		if out.String() != "Tailnet: up\n"+tc.line {
			t.Fatal(out.String())
		}
	}
}

func TestStatusCommandReportsTailnetHTTPS(t *testing.T) {
	for _, tc := range []struct{ setting, reason, want string }{
		{"", "", "Tailnet HTTPS: up https://osmia.tail0.ts.net/\n"},
		{"tailnet_https = false\n", "", "Tailnet HTTPS: off (disabled by listen.tailnet_https)\n"},
		{"", "enable HTTPS certificates in the tailnet admin console", "Tailnet HTTPS: unavailable (enable HTTPS certificates in the tailnet admin console; retrying in 1h0m0s)\n"},
		{"", "certificate retrieval failed", "Tailnet HTTPS: unavailable (certificate retrieval failed; retrying in 1h0m0s)\n"},
	} {
		t.Run(tc.want, func(t *testing.T) {
			opts := fixture(t)
			fixturekit.WithListen(t, opts.Config.Root, "tailnet = \"osmia\"\n"+tc.setting)
			opts.TailnetHTTPSRetry = time.Hour
			join, _ := fixturekit.TailnetJoiner("osmia.tail0.ts.net")
			opts.JoinTailnet = func(host, dir string) (service.TailnetListener, error) {
				node, err := join(host, dir)
				if node != nil && tc.reason != "" {
					node.SetHTTPSFailure(errors.New(tc.reason))
				}
				return node, err
			}
			s, err := service.Start(context.Background(), opts)
			must(t, err)
			t.Cleanup(func() { s.Close() })
			eventually(t, 10*time.Second, func() (bool, string) {
				output := successful(t, opts.Config.Root, "status")
				return strings.Contains(output, tc.want), "status did not show HTTPS state:\n" + output
			})
		})
	}
}
