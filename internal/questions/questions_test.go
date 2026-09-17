package questions_test

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/questions"
	"github.com/kpenfound/osmia/internal/trace"
)

const (
	project config.ProjectID    = "p_00000000000000000000000000000001"
	stream  config.WorkstreamID = "w_00000000000000000000000000000001"
	other   config.WorkstreamID = "w_00000000000000000000000000000002"
)

var (
	start = time.Date(2026, 9, 16, 12, 0, 0, 0, time.UTC)
	owner = trace.Actor{Kind: "owner", ID: "local"}
)

type fixture struct {
	repo *trace.Repository
	dir  string
}

func setup(t *testing.T) fixture {
	t.Helper()
	ctx := context.Background()
	base := t.TempDir()
	root, err := config.ResolveRoot(filepath.Join(base, "osmia"), "")
	if err != nil {
		t.Fatal(err)
	}
	p := config.Project{ID: project, Clone: filepath.Join(base, "target")}
	if err := os.Mkdir(p.Clone, 0700); err != nil {
		t.Fatal(err)
	}
	repo, err := trace.Create(ctx, root, p, start, owner)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { repo.Close() })
	dir, err := root.ProjectTrace(project)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range []config.WorkstreamID{stream, other} {
		if err := repo.CreateWorkstream(ctx, s, start, owner); err != nil {
			t.Fatal(err)
		}
	}
	for name, content := range map[string]string{
		"charter.md":  "# Charter\n\n1. Every change has a test.\n2. Keep state in files.\n3. First of two.\n3. Second of two.\n",
		"kb/store.md": "# Store\nState lives in files under the root.\n",
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
	}
	document := func(id, path, content string) trace.Document {
		return trace.Document{Header: trace.Header{Schema: "osmia.trace.document", Version: trace.Version, ID: id, Revision: 1, Project: project, Workstream: stream, At: start, Actor: owner, Cause: "draft"}, Path: path, Content: content}
	}
	if err := repo.RecordDocuments(ctx, []trace.Document{
		document("spec", "spec.md", "# Uploads\n\n## Acceptance criteria\n\n1. Uploads resume after a restart.\n2. Progress is reported.\n"),
		document("plan", "plan.json", `{"version":1,"units":[{"id":"resume","addresses":[{"criterion":"spec#1","proof":{"kind":"test","name":"TestResume"}}],"depends_on":[],"footprint":[]}]}`),
	}); err != nil {
		t.Fatal(err)
	}
	return fixture{repo: repo, dir: dir}
}

// turn queues and claims a turn of the agent, creating its thread first, and
// returns the turn's scope.
func (f fixture) turn(t *testing.T, agent, role, turn string) coreadapter.Scope {
	t.Helper()
	ctx := context.Background()
	header := func(kind, id string) trace.Header {
		return trace.Header{Schema: "osmia.trace." + kind, Version: trace.Version, ID: id, Revision: 1, Project: project, Workstream: stream, At: start, Actor: trace.Actor{Kind: "service", ID: "controller"}, Cause: "test", Depth: 1}
	}
	if err := f.repo.CreateThread(ctx, trace.Agent{Header: header("agent", agent), Role: role, ThreadID: agent + "_thread"}); err != nil {
		t.Fatal(err)
	}
	req := trace.TurnRequest{Header: header("turn-request", "request_"+turn), AgentID: agent, ThreadID: agent + "_thread", TurnID: turn,
		Profile: coreadapter.Profile{Name: "default", Backend: "fake", Model: "test"}, SystemPrompt: "You are the " + role + ".", Prompt: "Work"}
	req.Unit = "resume"
	if _, err := f.repo.EnqueueTurn(ctx, req); err != nil {
		t.Fatal(err)
	}
	if _, err := f.repo.ClaimTurn(ctx, stream, agent, "token_"+turn, "/owned/"+turn, start); err != nil {
		t.Fatal(err)
	}
	return coreadapter.Scope{Project: string(project), Workstream: string(stream), Unit: "resume", Thread: agent + "_thread", Turn: turn, Role: role}
}

func (f fixture) head(t *testing.T) string {
	t.Helper()
	cmd := exec.Command("git", "-C", f.dir, "rev-parse", "HEAD")
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null"}
	out, err := cmd.Output()
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(string(out))
}

func tools(t *testing.T, f fixture, agent string, scope coreadapter.Scope, now time.Time) map[string]coreadapter.Tool {
	t.Helper()
	list, err := questions.Tools(f.repo, agent, scope, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]coreadapter.Tool{}
	for _, tool := range list {
		if tool.Effect != coreadapter.ToolMemory || tool.Handle == nil || !json.Valid(tool.InputSchema) {
			t.Fatalf("tool %s: %+v", tool.Name, tool)
		}
		out[tool.Name] = tool
	}
	return out
}

func call(t *testing.T, tool coreadapter.Tool, input string) string {
	t.Helper()
	out, err := tool.Handle(context.Background(), json.RawMessage(input))
	if err != nil {
		t.Fatalf("%s %s: %v", tool.Name, input, err)
	}
	return string(out)
}

func states(t *testing.T, f fixture) map[string]trace.QuestionState {
	t.Helper()
	list, err := f.repo.Questions(stream)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]trace.QuestionState{}
	for _, q := range list {
		out[q.Asked.ID] = q
	}
	return out
}

func TestResolveCitations(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	chief := f.turn(t, "chief", trace.ChiefOfStaff, "chief1")
	mason := f.turn(t, "mason1", "mason", "build1")
	if _, err := f.repo.Ask(ctx, "mason1", mason, "Where does state live?", start); err != nil {
		t.Fatal(err)
	}
	if _, err := f.repo.AnswerQuestion(ctx, "chief", chief, "1", "In files.", []string{"charter#2"}, start); err != nil {
		t.Fatal(err)
	}
	for _, c := range []string{"charter#1", "charter#2", "kb/store.md", "ruling#1", "spec#1", "spec#2", "plan#resume"} {
		if err := questions.Resolve(ctx, f.repo, stream, c, start); err != nil {
			t.Errorf("%s: %v", c, err)
		}
	}
	for c, reason := range map[string]string{
		"charter#3":        "the charter has no rule numbered 3 exactly once",
		"charter#9":        "the charter has no rule numbered 9 exactly once",
		"charter#0":        "it is not one of " + questions.CitationForms,
		"kb/missing.md":    "the knowledge base has no file kb/missing.md",
		"kb/../charter.md": "it is not one of " + questions.CitationForms,
		"ruling#2":         "this workstream records no ruling 2",
		"spec#3":           "the spec has no acceptance criterion numbered 3 exactly once",
		"plan#upload":      "the plan has no unit upload exactly once",
		"the code":         "it is not one of " + questions.CitationForms,
		"":                 "it is not one of " + questions.CitationForms,
	} {
		var unresolved *questions.Unresolved
		if err := questions.Resolve(ctx, f.repo, stream, c, start); !errors.As(err, &unresolved) || unresolved.Reason != reason || unresolved.Citation != c {
			t.Errorf("%q: %v, want reason %q", c, err, reason)
		}
	}
	// Rulings, the spec and the plan resolve in their own workstream only.
	for c, reason := range map[string]string{
		"ruling#1":    "this workstream records no ruling 1",
		"spec#1":      "this workstream has no recorded spec",
		"plan#resume": "this workstream has no recorded plan",
	} {
		var unresolved *questions.Unresolved
		if err := questions.Resolve(ctx, f.repo, other, c, start); !errors.As(err, &unresolved) || unresolved.Reason != reason {
			t.Errorf("%q in another workstream: %v, want reason %q", c, err, reason)
		}
	}
}

func TestToolsFollowTheRole(t *testing.T) {
	f := setup(t)
	names := func(scope coreadapter.Scope, agent string) []string {
		list, err := questions.Tools(f.repo, agent, scope, time.Now)
		if err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, tool := range list {
			out = append(out, tool.Name)
		}
		return out
	}
	if got := names(f.turn(t, "chief", trace.ChiefOfStaff, "chief1"), "chief"); !reflect.DeepEqual(got, questions.ChiefTools) || strings.Contains(strings.Join(got, " "), "ask") {
		t.Fatalf("chief of staff tools: %v", got)
	}
	for _, role := range []string{"mason", "reviewer", "architect", "committee", "foreman", "librarian"} {
		if got := names(f.turn(t, role+"1", role, "turn_"+role), role+"1"); !reflect.DeepEqual(got, []string{"ask"}) {
			t.Fatalf("%s tools: %v", role, got)
		}
	}
	if _, err := questions.Tools(nil, "chief", coreadapter.Scope{}, time.Now); err == nil {
		t.Fatal("tools without a trace")
	}
}

func TestAskAnswerAndDeliver(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	chiefScope := f.turn(t, "chief", trace.ChiefOfStaff, "chief1")
	masonScope := f.turn(t, "mason1", "mason", "build1")
	reviewerScope := f.turn(t, "reviewer1", "reviewer", "review1")
	chief := tools(t, f, "chief", chiefScope, start.Add(time.Hour))

	if got := call(t, tools(t, f, "mason1", masonScope, start.Add(time.Minute))["ask"], `{"question":"Where does state live?"}`); got != `{"recorded":true,"question":"1","next":"End your turn now. The answer arrives as your next turn on this thread."}` {
		t.Fatalf("ask: %s", got)
	}
	if got := call(t, tools(t, f, "mason1", masonScope, start.Add(time.Minute))["ask"], `{"question":"And the key?"}`); got != `{"recorded":false,"reason":"this turn already asked question 1; end the turn, the answer arrives as your next turn"}` {
		t.Fatalf("second ask: %s", got)
	}
	if got := call(t, tools(t, f, "reviewer1", reviewerScope, start.Add(2*time.Minute))["ask"], `{"question":"Is the log format fixed?"}`); !strings.Contains(got, `"question":"2"`) {
		t.Fatalf("reviewer ask: %s", got)
	}
	if _, err := tools(t, f, "mason1", masonScope, start)["ask"].Handle(ctx, json.RawMessage(`{"question":"x","thread":"other"}`)); err == nil {
		t.Fatal("ask accepted an unknown field")
	}

	// An answer without a citation, or with one that names nothing, records
	// nothing and says why.
	before := f.head(t)
	for input, want := range map[string]string{
		`{"question":"1","text":"In files.","citations":[]}`:                        `{"recorded":false,"reason":"an answer needs at least one citation; escalate a question the record does not settle"}`,
		`{"question":"1","text":"In files.","citations":["charter#2","charter#9"]}`: `{"recorded":false,"reason":"citation \"charter#9\" does not resolve: the charter has no rule numbered 9 exactly once"}`,
		`{"question":"1","text":"In files.","citations":["kb/missing.md"]}`:         `{"recorded":false,"reason":"citation \"kb/missing.md\" does not resolve: the knowledge base has no file kb/missing.md"}`,
		`{"question":"1","text":"In files.","citations":["the code"]}`:              `{"recorded":false,"reason":"citation \"the code\" does not resolve: it is not one of ` + questions.CitationForms + `"}`,
		`{"question":"5","text":"In files.","citations":["charter#2"]}`:             `{"recorded":false,"reason":"there is no question 5 in this workstream"}`,
	} {
		if got := call(t, chief["answer"], input); got != want {
			t.Fatalf("answer %s:\n%s\nwant\n%s", input, got, want)
		}
	}
	// The reserved tools say so and record nothing.
	for _, name := range []string{"route_amendment", "propose_charter"} {
		if got := call(t, chief[name], `{"question":"1","text":"Amend criterion 1."}`); got != `{"recorded":false,"reason":"reserved until amendments and standing rulings (M4)"}` {
			t.Fatalf("%s: %s", name, got)
		}
	}
	// The first charter read recorded the owner's edit; nothing else moved.
	if s := states(t, f); s["1"].State != trace.QuestionOpen || s["2"].State != trace.QuestionOpen {
		t.Fatalf("refusals changed a question: %+v", s)
	}
	recorded := f.head(t)
	for _, name := range []string{"route_amendment", "propose_charter"} {
		call(t, chief[name], `{}`)
	}
	call(t, chief["answer"], `{"question":"1","text":"In files.","citations":["spec#9"]}`)
	if got := f.head(t); got != recorded || before == "" {
		t.Fatalf("a refused or reserved call committed: %s, was %s", got, recorded)
	}

	profiles := map[string]int{}
	d := &questions.Deliverer{Repository: f.repo, Now: func() time.Time { return start.Add(2 * time.Hour) },
		Profile: func(role string) (coreadapter.Profile, error) {
			profiles[role]++
			return coreadapter.Profile{Name: "strong", Backend: "fake", Model: "for-" + role}, nil
		}}
	if err := d.Pass(ctx); err != nil {
		t.Fatal(err)
	}
	if th, _ := f.repo.Thread(stream, "mason1"); len(th.Turns) != 1 || len(profiles) != 0 {
		t.Fatalf("an open question was delivered: %+v", th.Turns)
	}

	if got := call(t, chief["answer"], `{"question":"1","text":"In files under the root.","citations":["charter#2","kb/store.md"]}`); got != `{"recorded":true,"question":"1","next":"The answer is delivered to the asker as its next turn."}` {
		t.Fatalf("answer: %s", got)
	}
	if got := call(t, chief["answer"], `{"question":"1","text":"In a database.","citations":["charter#1"]}`); got != `{"recorded":false,"reason":"question 1 is already answered"}` {
		t.Fatalf("second answer: %s", got)
	}
	if got := call(t, chief["escalate"], `{"questions":["2"],"rephrasing":"Is the log format part of the contract?","blocked":"The review of the upload unit.","options":["Fixed","Free"],"recommendation":"Fixed."}`); got != `{"recorded":true,"batch":"escalation_2","questions":["2"]}` {
		t.Fatalf("escalate: %s", got)
	}
	if got := call(t, chief["answer"], `{"question":"2","text":"Fixed.","citations":["charter#1"]}`); got != `{"recorded":false,"reason":"question 2 is escalated to the owner; only the owner's ruling answers it"}` {
		t.Fatalf("answer after escalation: %s", got)
	}

	// A ruling alone delivers nothing: only a question in the answered state
	// is delivered.
	legacy := trace.Header{Schema: "osmia.trace.question", Version: trace.Version, ID: "legacy", Revision: 1, Project: project, Workstream: stream, At: start, Actor: owner, Cause: "import"}
	if err := f.repo.Append(ctx, trace.Question{Header: legacy, AskedBy: trace.Actor{Kind: "agent", ID: "reviewer1"}, Thread: "reviewer1_thread", Turn: "review1", Question: "An older question"}); err != nil {
		t.Fatal(err)
	}
	legacy.Schema, legacy.ID = "osmia.trace.ruling", "legacy-ruling"
	if err := f.repo.Append(ctx, trace.Ruling{Header: legacy, QuestionID: "legacy", QuestionRevision: 1, Decision: "Owner ruled", ReturnedAnswer: "Not yet relayed"}); err != nil {
		t.Fatal(err)
	}

	// A skipped workstream keeps its answer undelivered.
	d.Skip = func(config.WorkstreamID) (bool, error) { return true, nil }
	if err := d.Pass(ctx); err != nil {
		t.Fatal(err)
	}
	if th, _ := f.repo.Thread(stream, "mason1"); len(th.Turns) != 1 {
		t.Fatalf("a skipped workstream was delivered: %+v", th.Turns)
	}
	d.Skip = nil
	for range 3 {
		if err := d.Pass(ctx); err != nil {
			t.Fatal(err)
		}
	}
	th, err := f.repo.Thread(stream, "mason1")
	if err != nil || len(th.Turns) != 2 {
		t.Fatalf("answer turns: %+v %v", th.Turns, err)
	}
	req := th.Turns[1].Request
	wantPrompt := "Answer to your question 1.\n\nYou asked:\nWhere does state live?\n\nAnswer:\nIn files under the root.\n\nCitations:\n- charter#2\n- kb/store.md\n"
	if req.TurnID != "answer_1" || req.TurnID != questions.TurnID("1") || req.ID != "request_answer_1" || req.AgentID != "mason1" || req.ThreadID != "mason1_thread" || req.Unit != "resume" ||
		req.Prompt != wantPrompt || req.SystemPrompt != "You are the mason." || req.Actor != questions.Actor || req.Cause != "question_1_answered" ||
		req.Profile != (coreadapter.Profile{Name: "strong", Backend: "fake", Model: "for-mason"}) || !req.At.Equal(start.Add(2*time.Hour)) {
		t.Fatalf("answer turn: %+v", req)
	}
	if !reflect.DeepEqual(profiles, map[string]int{"mason": 1}) {
		t.Fatalf("profile lookups: %v", profiles)
	}
	if th, _ := f.repo.Thread(stream, "reviewer1"); len(th.Turns) != 1 {
		t.Fatalf("an escalated question was delivered: %+v", th.Turns)
	}

	// A profile that cannot be resolved stops the pass and delivers nothing.
	f2 := setup(t)
	chief2 := f2.turn(t, "chief", trace.ChiefOfStaff, "chief1")
	if _, err := f2.repo.Ask(ctx, "mason1", f2.turn(t, "mason1", "mason", "build1"), "Where?", start); err != nil {
		t.Fatal(err)
	}
	if _, err := f2.repo.AnswerQuestion(ctx, "chief", chief2, "1", "In files.", []string{"charter#2"}, start); err != nil {
		t.Fatal(err)
	}
	broken := &questions.Deliverer{Repository: f2.repo, Now: time.Now, Profile: func(string) (coreadapter.Profile, error) { return coreadapter.Profile{}, errors.New("no profile") }}
	if err := broken.Pass(ctx); err == nil || !strings.Contains(err.Error(), "question 1") || !strings.Contains(err.Error(), "profile of role mason: no profile") {
		t.Fatalf("pass without a profile: %v", err)
	}
	if th, _ := f2.repo.Thread(stream, "mason1"); len(th.Turns) != 1 {
		t.Fatalf("delivered without a profile: %+v", th.Turns)
	}
	if err := (&questions.Deliverer{}).Pass(ctx); err == nil {
		t.Fatal("an unconfigured deliverer passed")
	}
}

type fakeTurns struct {
	result coreadapter.SessionResult
	err    error
	during func()
}

func (f *fakeTurns) Run(context.Context, coreadapter.PreparedTurn) (coreadapter.SessionResult, error) {
	if f.during != nil {
		f.during()
	}
	return f.result, f.err
}

type resumableTurns struct {
	fakeTurns
	checked int
}

func (r *resumableTurns) CheckResume(context.Context, coreadapter.Profile, coreadapter.Profile, coreadapter.BackendSession) error {
	r.checked++
	return nil
}

func TestTurnThatAskedEndsWaiting(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	asking := f.turn(t, "mason1", "mason", "build1")
	quiet := f.turn(t, "mason2", "mason", "build2")
	failure := errors.New("backend failed")
	inner := &fakeTurns{result: coreadapter.SessionResult{FinalResponse: "I asked.", Outcome: &coreadapter.Outcome{Status: "done", Report: "Finished"}}, err: failure}
	inner.during = func() {
		if _, err := f.repo.Ask(ctx, "mason1", asking, "Where does state live?", start); err != nil {
			t.Error(err)
		}
	}
	turns := &questions.Turns{Turns: inner, Repository: f.repo}
	// The recorded question decides, whatever the agent reported, and the
	// runner's error is passed on.
	got, err := turns.Run(ctx, coreadapter.PreparedTurn{Scope: asking})
	if !errors.Is(err, failure) || got.FinalResponse != "I asked." || got.Outcome == nil || *got.Outcome != (coreadapter.Outcome{Status: "waiting", Report: "Asked question 1"}) {
		t.Fatalf("asking turn: %+v %v", got.Outcome, err)
	}
	inner.during, inner.err = nil, nil
	if got, err := turns.Run(ctx, coreadapter.PreparedTurn{Scope: quiet}); err != nil || got.Outcome == nil || got.Outcome.Status != "done" {
		t.Fatalf("turn that did not ask: %+v %v", got.Outcome, err)
	}
	inner.result.Outcome = nil
	if got, err := turns.Run(ctx, coreadapter.PreparedTurn{Scope: quiet}); err != nil || got.Outcome != nil {
		t.Fatalf("turn without an outcome: %+v %v", got.Outcome, err)
	}
	if _, err := (&questions.Turns{}).Run(ctx, coreadapter.PreparedTurn{Scope: quiet}); err == nil {
		t.Fatal("unconfigured turns ran")
	}

	if err := turns.CheckResume(ctx, coreadapter.Profile{}, coreadapter.Profile{}, coreadapter.BackendSession{}); !errors.Is(err, coreadapter.ErrResumeUnavailable) {
		t.Fatalf("resume check without a checker: %v", err)
	}
	resumable := &resumableTurns{}
	if err := (&questions.Turns{Turns: resumable, Repository: f.repo}).CheckResume(ctx, coreadapter.Profile{}, coreadapter.Profile{}, coreadapter.BackendSession{}); err != nil || resumable.checked != 1 {
		t.Fatalf("resume check: %v, %d calls", err, resumable.checked)
	}
}
