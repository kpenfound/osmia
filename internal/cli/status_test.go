package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/plan"
	"github.com/kpenfound/osmia/internal/service"
	"github.com/kpenfound/osmia/internal/status"
	"github.com/kpenfound/osmia/internal/trace"
)

func TestStatusShowsLiveAgentFactsBelowNarrative(t *testing.T) {
	started := time.Date(2026, 9, 16, 10, 0, 0, 123000000, time.UTC)
	st := service.WorkstreamStatus{Workstream: stream, Project: project,
		Status: &service.StatusView{Goal: "Ship uploads.", Agents: []string{"The mason is building uploads."}, UpdatedAt: started, Revision: 1},
		Agents: []service.AgentStatus{{Role: "mason", Unit: "resume", State: "waiting", StartedAt: started, Elapsed: 7, Profile: "mason-default", Attempt: 2, Path: "replay", QuestionID: "3"}}}
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

// withStatus creates the project trace with two workstreams and has a fake
// chief of staff write a status for the first one.
func withStatus(t *testing.T) service.Options {
	t.Helper()
	ctx := context.Background()
	opts := fixture(t)
	cfg, err := config.Load(opts.Config)
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

func TestStatusShowsWorkstreams(t *testing.T) {
	opts := withStatus(t)
	s, err := service.Start(context.Background(), opts)
	must(t, err)
	t.Cleanup(func() { s.Close() })
	root := opts.Config.Root

	overview := successful(t, root, "status")
	for _, want := range []string{
		"ready=true",
		"Profiles:\n",
		"Workstreams:\n" +
			"  " + stream + " state=handed open_questions=0 context_mode=file\n" +
			"    Goal: Ship resumable uploads.\n" +
			"    Attention: none\n" +
			"  " + quiet + " state=not recorded open_questions=0 context_mode=file\n" +
			"    no status yet\n",
	} {
		if !strings.Contains(overview, want) {
			t.Fatalf("overview lacks %q:\n%s", want, overview)
		}
	}
	if strings.Contains(overview, "The plan is drafted") {
		t.Fatalf("overview shows the note:\n%s", overview)
	}

	var all struct {
		Health        service.HealthResponse
		Configuration service.ConfigResponse
		Runtime       service.RuntimeResponse
		Status        service.StatusResponse
	}
	must(t, json.Unmarshal([]byte(successful(t, root, "status", "--json")), &all))
	handed := "handed"
	full := service.WorkstreamStatus{Workstream: stream, Project: project, State: &handed, Units: []service.UnitStatus{}, Advisories: []service.OverlapAdvisory{}, Gates: []trace.OwnerGate{}, Agents: []service.AgentStatus{}, ContextMode: "file", Status: &service.StatusView{
		Goal: "Ship resumable uploads.", Note: "The plan is drafted. Review is underway.",
		Agents: []string{"The architect is preparing the packet.", "A reviewer is idle."}, Revision: 1, UpdatedAt: written}}
	none := service.WorkstreamStatus{Workstream: quiet, Project: project, Units: []service.UnitStatus{}, Advisories: []service.OverlapAdvisory{}, Gates: []trace.OwnerGate{}, Agents: []service.AgentStatus{}, ContextMode: "file"}
	if !all.Health.Ready || all.Configuration.Project == nil || !reflect.DeepEqual(all.Status, service.StatusResponse{Workstreams: []service.WorkstreamStatus{full, none}, Profiles: all.Runtime.Profiles, Diagnostics: []service.Diagnostic{}}) {
		t.Fatalf("status --json: %+v", all.Status)
	}

	one := successful(t, root, "status", stream)
	if want := "Workstream: " + stream + " state=handed open_questions=0 context_mode=file\n" +
		"Goal: Ship resumable uploads.\n" +
		"Attention: none\n" +
		"Note: The plan is drafted. Review is underway.\n" +
		"Agents:\n" +
		"  The architect is preparing the packet.\n" +
		"  A reviewer is idle.\n" +
		"Updated: 2026-09-16T10:00:00Z (revision 1)\n"; one != want {
		t.Fatalf("status <workstream>:\n%s\nwant:\n%s", one, want)
	}
	var got service.WorkstreamStatus
	must(t, json.Unmarshal([]byte(successful(t, root, "status", stream, "--json")), &got))
	if !reflect.DeepEqual(got, full) {
		t.Fatalf("status <workstream> --json: %+v", got)
	}

	if out := successful(t, root, "status", quiet); out != "Workstream: "+quiet+" state=not recorded open_questions=0 context_mode=file\nStatus: none yet; the chief of staff has not written one\n" {
		t.Fatalf("no status:\n%s", out)
	}
	raw := map[string]json.RawMessage{}
	must(t, json.Unmarshal([]byte(successful(t, root, "status", "--json", quiet)), &raw))
	if string(raw["status"]) != "null" || string(raw["state"]) != "null" || string(raw["units"]) != "[]" || string(raw["gates"]) != "[]" || string(raw["context_mode"]) != `"file"` || string(raw["open_questions"]) != "0" || string(raw["drift"]) != "null" {
		t.Fatalf("no status JSON: %v", raw)
	}

	unknown := "w_00000000000000000000000000000009"
	code, out, diag := invoke(t, root, "status", unknown)
	if code != 4 || out != "" || diag != "not_found: workstream "+unknown+" is not in the active project; list workstreams with osmia status\n" {
		t.Fatalf("unknown workstream: %d %q %q", code, out, diag)
	}
	for _, args := range [][]string{{"status", "not-a-workstream"}, {"status", project}, {"status", stream, quiet}} {
		code, out, diag := invoke(t, root, args...)
		if code != 2 || out != "" || !strings.Contains(diag, "invalid arguments") {
			t.Fatalf("%v: %d %q %q", args, code, out, diag)
		}
	}
}

func TestStatusWithoutWorkstreams(t *testing.T) {
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

	empty, _ := emptyFixture(t)
	idle, err := service.Start(context.Background(), empty)
	must(t, err)
	t.Cleanup(func() { idle.Close() })
	code, out, diag = invoke(t, empty.Config.Root, "status", stream)
	if code != 4 || out != "" || diag != "no_project: no project is configured; add one with osmia project add\n" {
		t.Fatalf("no project: %d %q %q", code, out, diag)
	}
	if out := successful(t, empty.Config.Root, "status"); !strings.Contains(out, "Workstreams:\n  none\n") {
		t.Fatalf("no project:\n%s", out)
	}
}

func TestStatusTextWithoutAttentionOrAgents(t *testing.T) {
	st := service.WorkstreamStatus{Workstream: stream, Project: project, OpenQuestions: 2, ContextMode: "file", Status: &service.StatusView{
		Goal: "Ship resumable uploads.", Note: "Work is starting.", Agents: []string{}, Revision: 3, UpdatedAt: written}}
	var one strings.Builder
	showStatus(&one, st)
	if want := "Workstream: " + stream + " state=not recorded open_questions=2 context_mode=file\n" +
		"Goal: Ship resumable uploads.\nAttention: none\nNote: Work is starting.\nAgents:\n  none active\n" +
		"Updated: 2026-09-16T10:00:00Z (revision 3)\n"; one.String() != want {
		t.Fatalf("got:\n%s\nwant:\n%s", one.String(), want)
	}
	building := "building"
	st.State, st.Units = &building, []service.UnitStatus{{Unit: "parser", State: "ready"}, {Unit: "validator", State: "planned"}}
	one.Reset()
	showStatus(&one, st)
	if want := "Workstream: " + stream + " state=building open_questions=2 context_mode=file\n" +
		"Units:\n  parser ready\n  validator planned\n" +
		"Goal: Ship resumable uploads.\nAttention: none\nNote: Work is starting.\nAgents:\n  none active\n" +
		"Updated: 2026-09-16T10:00:00Z (revision 3)\n"; one.String() != want {
		t.Fatalf("got:\n%s\nwant:\n%s", one.String(), want)
	}
	st.State, st.Units = nil, nil
	var all strings.Builder
	showWorkstreams(&all, service.StatusResponse{Workstreams: []service.WorkstreamStatus{st}, Diagnostics: []service.Diagnostic{{Field: "workstreams", Code: service.Internal, Message: "cannot read"}}})
	if want := "Workstreams:\n  " + stream + " state=not recorded open_questions=2 context_mode=file\n" +
		"    Goal: Ship resumable uploads.\n    Attention: none\nDiagnostic: workstreams: internal: cannot read\n"; all.String() != want {
		t.Fatalf("got:\n%s\nwant:\n%s", all.String(), want)
	}
	all.Reset()
	showWorkstreams(&all, service.StatusResponse{Diagnostics: []service.Diagnostic{{Field: "workstreams", Code: service.Internal, Message: "cannot read"}}})
	if want := "Workstreams:\nDiagnostic: workstreams: internal: cannot read\n"; all.String() != want {
		t.Fatalf("got:\n%s\nwant:\n%s", all.String(), want)
	}
}

func TestStatusPrintsTheDailyBudget(t *testing.T) {
	var out strings.Builder
	showDailyBudget(&out, nil)
	showDailyBudget(&out, &service.DailyBudgetStatus{Day: "2026-09-24", SpendUSD: "12.5", LimitUSD: "150.00"})
	showDailyBudget(&out, &service.DailyBudgetStatus{Day: "2026-09-24", SpendUSD: "150", LimitUSD: "150.00", UnknownCosts: 2, LowerBound: true})
	if want := "Daily budget: USD 12.5 of USD 150.00 spent on 2026-09-24\n" +
		"Daily budget: USD 150 of USD 150.00 spent on 2026-09-24 (at least; 2 attempt(s) have unknown cost)\n"; out.String() != want {
		t.Fatalf("got:\n%s\nwant:\n%s", out.String(), want)
	}
}

func TestStatusPrintsFailureStreaks(t *testing.T) {
	var out strings.Builder
	showFailureStreaks(&out, nil)
	at := time.Date(2026, 9, 24, 12, 0, 0, 0, time.UTC)
	showFailureStreaks(&out, []service.FailureStreak{{Role: "mason", Profile: "default", Consecutive: 3, LastFailure: "the agent exited with code 1", LastAt: at}, {Role: "reviewer", Profile: "backup", Consecutive: 1, LastFailure: "timed out", LastAt: at.Add(time.Minute)}})
	if want := "Infrastructure failures: mason on profile default failed 3 time(s) in a row; last at 2026-09-24T12:00:00Z: the agent exited with code 1\n" +
		"Infrastructure failures: reviewer on profile backup failed 1 time(s) in a row; last at 2026-09-24T12:01:00Z: timed out\n"; out.String() != want {
		t.Fatalf("got:\n%s\nwant:\n%s", out.String(), want)
	}
}

func TestStatusPrintsOwnerGates(t *testing.T) {
	st := service.WorkstreamStatus{Workstream: stream, Project: project, ContextMode: "file", Gates: []trace.OwnerGate{{Kind: "escalation", Reference: "12"}, {Kind: "contested", Reference: "upload-index"}}}
	var one, all strings.Builder
	showStatus(&one, st)
	showWorkstreams(&all, service.StatusResponse{Workstreams: []service.WorkstreamStatus{st}})
	for _, pair := range [][2]string{{one.String(), "Gate: escalation 12\nGate: contested upload-index\n"}, {all.String(), "    Gate: escalation 12\n    Gate: contested upload-index\n"}} {
		if !strings.Contains(pair[0], pair[1]) {
			t.Fatalf("status lacks gates:\n%s", pair[0])
		}
	}
}

func TestStatusPrintsLatestUnitCard(t *testing.T) {
	st := service.WorkstreamStatus{Workstream: stream, Project: project, ContextMode: "file", Units: []service.UnitStatus{
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
	st := service.WorkstreamStatus{Workstream: stream, Project: project, ContextMode: "file", Units: []service.UnitStatus{
		{Unit: "parser", State: "implementing"},
		{Unit: "validator", State: "ready", Reason: waits, Deferral: &service.UnitDispatch{Unit: "validator", Version: 1, Decision: service.DispatchDeferred, Reason: service.DeferEntangled,
			Blockers: []plan.StartBlocker{{Unit: "parser", Reason: plan.OverlapReason}}, Message: waits}},
	}}
	var out strings.Builder
	showStatus(&out, st)
	if want := "Units:\n  parser implementing\n  validator ready\n    Waiting: " + waits + "\nStatus: none yet"; !strings.Contains(out.String(), want) {
		t.Fatalf("status lacks why the unit waits:\n%s", out.String())
	}
}

func TestStatusPrintsUnitLanding(t *testing.T) {
	st := service.WorkstreamStatus{Workstream: stream, Project: project, ContextMode: "file", Units: []service.UnitStatus{
		{Unit: "parser", State: "merged", Landing: &service.UnitLanding{Unit: "parser", Approval: "units/parser/review.json revision 2", Candidate: "c0ffee", Base: "ba5e", Criteria: []string{"spec#1", "spec#3"}, Branch: "osmia/feature", Commit: "1a4d3d"}},
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
	st := service.WorkstreamStatus{Workstream: stream, Project: project, ContextMode: "file", Advisories: []service.OverlapAdvisory{{Workstream: "w_00000000000000000000000000000002", Seal: 1, OtherSeal: 1, Subsystems: []string{"internal"}, Entities: []string{"internal.trace"}, Paths: []string{"internal/trace"}, Message: warning}}}
	var one, all strings.Builder
	showStatus(&one, st)
	showWorkstreams(&all, service.StatusResponse{Workstreams: []service.WorkstreamStatus{st}})
	for _, pair := range [][2]string{{one.String(), "\nOverlap: " + warning + "\n"}, {all.String(), "\n    Overlap: " + warning + "\n"}} {
		if !strings.Contains(pair[0], pair[1]) {
			t.Fatalf("status lacks the advisory:\n%s", pair[0])
		}
	}
}

// project rebase reports the workstreams it covers and skips, and status
// shows each workstream's latest drift rebase.
func TestProjectRebaseAndDriftStatus(t *testing.T) {
	opts := withStatus(t)
	ctx := context.Background()
	cfg, err := config.Load(opts.Config)
	must(t, err)
	repo, err := trace.Open(cfg.Root, cfg.Project)
	must(t, err)
	reason := "drift rebase 1 changed nothing: the workstream is handed, not building or assembled"
	h := trace.Header{Schema: "osmia.trace.transition", Version: 1, ID: "drift-1-skipped", Revision: 1, Project: project, Workstream: stream, At: written, Actor: trace.Actor{Kind: "service", ID: "foreman"}, Cause: "drift-1-run"}
	_, err = repo.Transact(ctx, trace.Transaction{Transition: trace.Transition{Header: h, Subject: "drift", To: "skipped-1", Reason: reason}})
	must(t, err)
	must(t, repo.Close())
	s, err := service.Start(ctx, opts)
	must(t, err)
	t.Cleanup(func() { s.Close() })
	root := opts.Config.Root

	if out := successful(t, root, "status"); !strings.Contains(out, "  "+stream+" state=handed open_questions=0 context_mode=file\n    Drift: rebase 1 skipped at 2026-09-16T10:00:00Z\n    Goal: ") {
		t.Fatalf("overview lacks the drift rebase:\n%s", out)
	}
	if out := successful(t, root, "status", stream); !strings.Contains(out, "\nDrift: rebase 1 skipped at 2026-09-16T10:00:00Z\n  Reason: "+reason+"\n") {
		t.Fatalf("status <workstream> lacks the drift rebase:\n%s", out)
	}
	var one service.WorkstreamStatus
	must(t, json.Unmarshal([]byte(successful(t, root, "status", stream, "--json")), &one))
	if want := (&service.DriftStatus{Drift: 1, Outcome: "skipped", At: written, Reason: reason, Moved: []string{}}); !reflect.DeepEqual(one.Drift, want) {
		t.Fatalf("status drift %+v, want %+v", one.Drift, want)
	}

	skipped := []service.DriftSkip{{Workstream: stream, Reason: "the workstream is handed, not building or assembled"}, {Workstream: quiet, Reason: "the workstream is not started, not building or assembled"}}
	if out := successful(t, root, "project", "rebase", project); out != "Drift rebase requested for project "+project+"\nCovered: none\nSkipped: "+stream+": "+skipped[0].Reason+"\nSkipped: "+quiet+": "+skipped[1].Reason+"\n" {
		t.Fatalf("project rebase:\n%s", out)
	}
	var got service.ProjectRebaseResponse
	must(t, json.Unmarshal([]byte(successful(t, root, "project", "rebase", project, "--json")), &got))
	if want := (service.ProjectRebaseResponse{Project: project, Covered: []service.DriftCoverage{}, Skipped: skipped}); !reflect.DeepEqual(got, want) {
		t.Fatalf("project rebase --json: %+v", got)
	}
	other := "p_ffffffffffffffffffffffffffffffff"
	if code, out, diag := invoke(t, root, "project", "rebase", other); code != 4 || out != "" || !strings.Contains(diag, "not_found: project "+other+" is not an active project") {
		t.Fatalf("project rebase of another project: %d %s %s", code, out, diag)
	}
}

func TestStatusShowsTheTailnetListener(t *testing.T) {
	var out bytes.Buffer
	showTailnet(&out, nil)
	showTailnet(&out, &service.TailnetStatus{State: service.TailnetUp})
	showTailnet(&out, &service.TailnetStatus{State: service.TailnetNeedsLogin, LoginURL: "https://login.example/a/1"})
	showTailnet(&out, &service.TailnetStatus{State: service.TailnetDown, Reason: "control unreachable"})
	if want := "Tailnet: up\nTailnet: needs_login login=https://login.example/a/1\nTailnet: down (control unreachable)\n"; out.String() != want {
		t.Fatalf("tailnet lines:\n%s", out.String())
	}

	// A tailnet that cannot be joined leaves the service and its status
	// working over the socket.
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
