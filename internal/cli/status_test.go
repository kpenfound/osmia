package cli

import (
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
	"github.com/kpenfound/osmia/internal/service"
	"github.com/kpenfound/osmia/internal/status"
	"github.com/kpenfound/osmia/internal/trace"
)

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
	out, err := tool.Handle(ctx, json.RawMessage(`{"goal":"Ship resumable uploads.","attention":"Approve the upload plan.","note":"The plan is drafted. It needs your approval.","agents":["The architect is waiting for you.","A reviewer is idle."]}`))
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
			"    Attention: Approve the upload plan.\n" +
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
	full := service.WorkstreamStatus{Workstream: stream, Project: project, State: &handed, ContextMode: "file", Status: &service.StatusView{
		Goal: "Ship resumable uploads.", Attention: "Approve the upload plan.", Note: "The plan is drafted. It needs your approval.",
		Agents: []string{"The architect is waiting for you.", "A reviewer is idle."}, Revision: 1, UpdatedAt: written}}
	none := service.WorkstreamStatus{Workstream: quiet, Project: project, ContextMode: "file"}
	if !all.Health.Ready || all.Configuration.Project == nil || !reflect.DeepEqual(all.Status, service.StatusResponse{Workstreams: []service.WorkstreamStatus{full, none}, Diagnostics: []service.Diagnostic{}}) {
		t.Fatalf("status --json: %+v", all.Status)
	}

	one := successful(t, root, "status", stream)
	if want := "Workstream: " + stream + " state=handed open_questions=0 context_mode=file\n" +
		"Goal: Ship resumable uploads.\n" +
		"Attention: Approve the upload plan.\n" +
		"Note: The plan is drafted. It needs your approval.\n" +
		"Agents:\n" +
		"  The architect is waiting for you.\n" +
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
	if string(raw["status"]) != "null" || string(raw["state"]) != "null" || string(raw["context_mode"]) != `"file"` || string(raw["open_questions"]) != "0" {
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
