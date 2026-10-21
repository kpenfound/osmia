package intakefixture

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/kpenfound/agentcore/agent"
	"github.com/kpenfound/agentcore/vcs"
	"github.com/kpenfound/osmia/internal/api"
	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/intake"
	"github.com/kpenfound/osmia/internal/kb"
	"github.com/kpenfound/osmia/internal/questions"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	"github.com/kpenfound/osmia/internal/trace"
	"github.com/kpenfound/osmia/internal/workspace"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// TurnScenario provides trace and clock access shared by live-service and controller fixtures.
type TurnScenario struct {
	Root         func() config.Root
	Project      config.ProjectID
	Now          func() time.Time
	Repository   func() *trace.Repository
	Changes      func(*testing.T) (func(), func())
	Done         func() <-chan struct{}
	WaitError    func() error
	ForemanActor trace.Actor
}

// PassReconciler is a controller driven by a stopped-service fixture.
type PassReconciler interface {
	coreadapter.Reconciler
	Pass(context.Context) error
}

func AbandonCall(t *testing.T, handle http.HandlerFunc, id config.WorkstreamID, body string) (int, string) {
	t.Helper()
	w := httptest.NewRecorder()
	handle(w, httptest.NewRequest(http.MethodPost, api.Prefix+"/abandon/"+string(id), strings.NewReader(body)))
	return w.Code, w.Body.String()
}

func AttemptOperation(t *testing.T, now time.Time, actor trace.Actor, repository *trace.Repository, stream config.WorkstreamID, op coreadapter.Operation, r coreadapter.Reconciler) (coreadapter.OperationResult, error) {
	t.Helper()
	ctx := context.Background()
	ops, err := repository.Operations(stream)
	Must(t, err)
	i := slices.IndexFunc(ops, func(o trace.OperationRecord) bool { return o.Operation.ID == op.ID })
	if i < 0 {
		t.Fatalf("operation %s is not recorded", op.ID)
	}
	var result coreadapter.OperationResult
	var applied error

	Must(t, repository.WithOperation(ctx, stream, ops[i].EventID, actor, func() time.Time { return now }, func(a *trace.OperationAttempt, _ trace.OperationRecord) error {
		seen, err := r.Inspect(ctx, op)
		if err != nil {
			return err
		}
		observe := a.Action("observe", now)
		observe.Observation = &seen
		if err := a.Record(ctx, observe); err != nil {
			return err
		}
		if seen.State == coreadapter.EffectCompleted {
			result = *seen.Result
		} else {
			if err := a.Record(ctx, a.Action("effect", now)); err != nil {
				return err
			}
			if result, applied = r.Apply(ctx, op); applied != nil {
				retry := a.Action("retry", now)
				retry.Failure, retry.RetryAt = applied.Error(), now.Add(time.Nanosecond)
				return a.Record(ctx, retry)
			}
		}
		done := a.Action("result", now)
		done.Result = &result
		return a.Record(ctx, done)
	}))
	return result, applied
}

func AwaitExtraction(t *testing.T, configuration func(context.Context) (api.ConfigResponse, error)) api.ExtractionState {
	t.Helper()
	deadline := time.Now().Add(DemoTimeout)
	for {
		cfg, err := configuration(context.Background())
		Must(t, err)
		if cfg.Project != nil && cfg.Project.Extraction != nil {
			if x := cfg.Project.Extraction; x.State == "succeeded" || x.State == "failed" {
				return *x
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("extraction did not finish: %+v", cfg.Project)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

func (f *TurnScenario) AwaitMasonRan(t *testing.T, stream config.WorkstreamID, unit string) trace.Thread {
	t.Helper()
	wait, close := f.Changes(t)
	defer close()
	deadline := time.Now().Add(DemoTimeout)
	for {
		select {
		case <-f.Done():
			t.Fatalf("the mason of unit %s of %s could not run: service stopped: %v", unit, stream, f.WaitError())
		default:
		}
		th, err := f.Repository().Thread(stream, MasonAgent(unit))
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			t.Fatal(err)
		}
		if err == nil && len(th.Turns) > 0 && !th.Turns[0].CompletedAt.IsZero() {
			if status := th.Turns[0].Status(); status != "idle" {
				t.Fatalf("the mason of unit %s of %s ended %s: %+v", unit, stream, status, th.Turns[0])
			}
			return th
		}
		if time.Now().After(deadline) {
			t.Fatalf("the mason of unit %s of %s never ran: %+v", unit, stream, th)
		}
		wait()
	}
}

func (f *TurnScenario) AwaitUnit(t *testing.T, stream config.WorkstreamID, unit, state string) {
	t.Helper()
	wait, close := f.Changes(t)
	defer close()
	deadline := time.Now().Add(DemoTimeout)
	for {
		got, err := f.Repository().Workflow(stream, trace.UnitSubject(unit))
		Must(t, err)
		if got.Value == state {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("unit %s of %s is %q, never %s", unit, stream, got.Value, state)
		}
		wait()
	}
}

func BackendOf(t *testing.T, options config.Options, project config.ProjectID, stream config.WorkstreamID) string {
	t.Helper()
	root, err := config.ResolveRoot(options.Root, "")
	Must(t, err)
	data, ok := fixturekit.TraceFile(t, WorkstreamTraceDirectory(t, root, project, stream), "workstreams/"+string(stream)+"/workstream.json")
	if !ok {
		t.Fatalf("workstream %s has no manifest", stream)
	}
	var manifest struct {
		Workspaces string `json:"workspaces"`
	}
	Must(t, json.Unmarshal(data, &manifest))
	if manifest.Workspaces == "" {
		return config.WorkspacesGit
	}
	return manifest.Workspaces
}

func (f *TurnScenario) CaptureTurn(t *testing.T, repository *trace.Repository, stream config.WorkstreamID, agent string, outcome *coreadapter.Outcome) trace.QueuedTurn {
	t.Helper()
	ctx := context.Background()
	th, err := repository.Thread(stream, agent)
	Must(t, err)
	token := "done-" + th.Turns[len(th.Turns)-1].Request.TurnID
	directory := filepath.Join(f.Root().String(), "threads", string(f.Project), string(stream), agent, token)
	q, err := repository.ClaimTurn(ctx, stream, agent, token, directory, f.Now())
	Must(t, err)
	h := q.Request.Header
	h.Schema, h.ID, h.At = "osmia.trace.turn-response", trace.EventID(q.Request.ID, "response"), f.Now()
	response := trace.TurnResponse{Header: h, AgentID: agent, ThreadID: agent, TurnID: q.Request.TurnID, RequestID: q.Request.ID, RequestRevision: q.Request.Revision,
		Result: coreadapter.SessionResult{Session: coreadapter.BackendSession{Backend: q.Request.Profile.Backend, ID: token}, SessionDirectory: directory, StartedAt: q.Claim.At, FinalResponse: "Built", Outcome: outcome}}
	Must(t, repository.CaptureTurn(ctx, q.Claim.Token, response))
	Must(t, repository.CompleteTurn(ctx, stream, agent, q.Request.TurnID, q.Claim.Token, f.Now()))
	return q
}

func (f *TurnScenario) ChiefRecordsAnswer(t *testing.T, repository *trace.Repository, stream config.WorkstreamID, id, text, cite string) {
	t.Helper()
	ctx := context.Background()
	Must(t, func() error {
		_, err := repository.EnsureChiefOfStaff(ctx, stream, f.Now(), fixturekit.OwnerActor)
		return err
	}())
	turn := "chief-answer-" + id
	req := trace.TurnRequest{Header: trace.Header{Schema: "osmia.trace.turn-request", Version: trace.Version, ID: "request_" + turn, Revision: 1, Project: repository.Project(), Workstream: stream, At: f.Now(), Actor: f.ForemanActor, Cause: "test", Depth: 1},
		AgentID: trace.ChiefOfStaff, ThreadID: trace.ChiefOfStaff, TurnID: turn, Profile: coreadapter.Profile{Name: "default", Backend: "claude", Model: "test"}, SystemPrompt: "chief", Prompt: "Answer question " + id}
	_, err := repository.EnqueueTurn(ctx, req)
	Must(t, err)
	token := "claim-" + turn
	directory := filepath.Join(f.Root().String(), "threads", string(f.Project), string(stream), trace.ChiefOfStaff, token)
	q, err := repository.ClaimTurn(ctx, stream, trace.ChiefOfStaff, token, directory, f.Now())
	Must(t, err)
	chiefScope := coreadapter.Scope{Project: string(f.Project), Workstream: string(stream), Role: trace.ChiefOfStaff, Thread: trace.ChiefOfStaff, Turn: turn}
	tools, err := questions.Tools(repository, trace.ChiefOfStaff, chiefScope, f.Now)
	Must(t, err)
	i := slices.IndexFunc(tools, func(tool coreadapter.Tool) bool { return tool.Name == questions.AnswerTool })
	if i < 0 {
		t.Fatal("the chief of staff has no answer tool")
	}
	input, err := json.Marshal(map[string]any{"question": id, "text": text, "citations": []string{cite}})
	Must(t, err)
	out, err := tools[i].Handle(ctx, input)
	Must(t, err)
	if !strings.Contains(string(out), `"recorded":true`) {
		t.Fatalf("the chief of staff's answer to question %s: %s", id, out)
	}
	f.CompleteClaimedTurn(t, repository, stream, trace.ChiefOfStaff, q, &coreadapter.Outcome{Status: "done", Report: "Answered question " + id})
}

func (f *TurnScenario) CompleteClaimedTurn(t *testing.T, repository *trace.Repository, stream config.WorkstreamID, agent string, q trace.QueuedTurn, outcome *coreadapter.Outcome) {
	t.Helper()
	ctx := context.Background()
	h := q.Request.Header
	h.Schema, h.ID, h.At = "osmia.trace.turn-response", trace.EventID(q.Request.ID, "response"), f.Now()
	response := trace.TurnResponse{Header: h, AgentID: agent, ThreadID: agent, TurnID: q.Request.TurnID, RequestID: q.Request.ID, RequestRevision: q.Request.Revision,
		Result: coreadapter.SessionResult{Session: coreadapter.BackendSession{Backend: q.Request.Profile.Backend, ID: q.Claim.Token}, SessionDirectory: q.Claim.SessionDirectory, StartedAt: q.Claim.At, Outcome: outcome}}
	Must(t, repository.CaptureTurn(ctx, q.Claim.Token, response))
	Must(t, repository.CompleteTurn(ctx, stream, agent, q.Request.TurnID, q.Claim.Token, f.Now()))
}

func (f *TurnScenario) CompleteMasonTurn(t *testing.T, repository *trace.Repository, stream config.WorkstreamID, unit string, outcome string) trace.QueuedTurn {
	t.Helper()
	content, err := json.Marshal(struct {
		Outcome string `json:"outcome"`
	}{Outcome: outcome})
	Must(t, err)
	return f.CaptureTurn(t, repository, stream, MasonAgent(unit), &coreadapter.Outcome{Status: "done", Report: string(content), Card: &ExampleCard})
}

func FinalOperations(t *testing.T, repository *trace.Repository, stream config.WorkstreamID, action string) []trace.OperationRecord {
	t.Helper()
	ops, err := repository.Operations(stream)
	Must(t, err)
	ops = slices.DeleteFunc(ops, func(o trace.OperationRecord) bool { return o.Operation.Action != action })
	slices.SortFunc(ops, func(a, b trace.OperationRecord) int { return a.Transition.At.Compare(b.Transition.At) })
	return ops
}

func LandOperations(t *testing.T, repository *trace.Repository, stream config.WorkstreamID, action string) []trace.OperationRecord {
	t.Helper()
	ops, err := repository.Operations(stream)
	Must(t, err)
	return slices.DeleteFunc(ops, func(o trace.OperationRecord) bool { return o.Operation.Action != action })
}

func FinalTurn(engine *fixturekit.DemoEngine, turn string, run func(context.Context, *mcp.ClientSession) error) string {

	engine.Mu.Lock()
	defer engine.Mu.Unlock()
	engine.Turns[turn] = func(ctx context.Context, req agent.Request, _ *agent.Turn, tools *mcp.ClientSession) (*agent.Result, error) {
		if err := run(ctx, tools); err != nil {
			return nil, err
		}
		return &agent.Result{ClaudeID: "session-" + turn, ResultText: "Read the branch", SessionDir: req.SessionDir, NumTurns: 1}, nil
	}
	return turn
}

func MoveFeature(t *testing.T, g workspace.Provider, stream config.WorkstreamID, files map[string]string) string {
	t.Helper()
	ctx := context.Background()

	acquired, err := g.Acquire(ctx, vcs.Request{Name: string(stream), Branch: intake.FeatureBranch(stream)})
	Must(t, err)
	w := acquired.(workspace.Worktree)
	tip, _, err := g.Branch(ctx, intake.FeatureBranch(stream))
	Must(t, err)
	for name, content := range files {
		if content == "" {
			Must(t, os.Remove(filepath.Join(w.Path, name)))
			continue
		}
		Must(t, os.MkdirAll(filepath.Dir(filepath.Join(w.Path, name)), 0700))
		Must(t, os.WriteFile(filepath.Join(w.Path, name), []byte(content), 0600))
	}
	moved, err := g.Snapshot(ctx, w, tip)
	Must(t, err)
	return moved
}

func MergeDirectly(t *testing.T, repository *trace.Repository, stream config.WorkstreamID, unit string, commit string, actor trace.Actor, merged string) string {
	t.Helper()

	state, err := repository.Workflow(stream, trace.UnitSubject(unit))
	Must(t, err)
	h := trace.Header{Schema: "osmia.trace.transition", Version: trace.Version, ID: trace.UnitSubject(unit) + "-" + merged, Revision: 1, Project: repository.Project(), Workstream: stream, Unit: unit, At: time.Now().UTC(), Actor: actor, Cause: "landing-" + unit}
	_, err = repository.Transact(context.Background(), trace.Transaction{ExpectedVersion: state.Version,
		Transition: trace.Transition{Header: h, Subject: trace.UnitSubject(unit), From: state.Value, To: merged, Reason: fmt.Sprintf("unit %s landed as %s", unit, commit)}})
	Must(t, err)
	return commit
}

func OnWorkspaces(t *testing.T, root, backend string) {
	t.Helper()
	if backend == config.WorkspacesGit {
		return
	}
	if backend == config.WorkspacesJujutsu {
		RequireJJ(t)
	}
	path := filepath.Join(root, "config.toml")
	data, err := os.ReadFile(path)
	Must(t, err)
	lines := strings.Split(string(data), "\n")
	i := slices.IndexFunc(lines, func(line string) bool { return strings.HasPrefix(line, "workspaces = ") })
	if i < 0 {
		t.Fatalf("%s sets no workspaces", path)
	}
	lines[i] = fmt.Sprintf("workspaces = %q", backend)
	Must(t, os.WriteFile(path, []byte(strings.Join(lines, "\n")), 0600))
}

func RunChecks(t *testing.T, repository *trace.Repository, stream config.WorkstreamID, c PassReconciler, action string, attempt func(coreadapter.Operation, coreadapter.Reconciler) (coreadapter.OperationResult, error)) {
	t.Helper()
	ctx := context.Background()

	for {
		Must(t, c.Pass(ctx))
		ops, err := repository.Operations(stream)
		Must(t, err)
		i := slices.IndexFunc(ops, func(op trace.OperationRecord) bool { return op.Operation.Action == action && op.Result == nil })
		if i < 0 {
			return
		}
		_, err = attempt(ops[i].Operation, c)
		Must(t, err)
	}
}

func AssembleBoth(t *testing.T, repository *trace.Repository, a PassReconciler, stream config.WorkstreamID, merge func(string, map[string]string) string, resume, dedupe map[string]string, assembled, action string) (string, coreadapter.Operation) {
	t.Helper()
	ctx := context.Background()
	building := func() {
		t.Helper()
		Must(t, a.Pass(ctx))
		if feature, err := repository.Workflow(stream, trace.FeatureSubject); err != nil || feature.Value != intake.BuildingState {
			t.Fatalf("the workstream is %+v with a unit unmerged: %v", feature, err)
		}
	}
	building()
	merge("resume", resume)
	building()
	head := merge("dedupe", dedupe)
	Must(t, a.Pass(ctx))
	if feature, err := repository.Workflow(stream, trace.FeatureSubject); err != nil || feature.Value != assembled {
		t.Fatalf("the workstream is %+v with every unit merged: %v", feature, err)
	}
	if ops := FinalOperations(t, repository, stream, action); len(ops) != 0 {
		t.Fatalf("final reviews asked for in the pass that assembled: %+v", ops)
	}
	Must(t, a.Pass(ctx))
	Must(t, a.Pass(ctx))
	ops := FinalOperations(t, repository, stream, action)
	if len(ops) != 1 {
		t.Fatalf("final review operations %+v", ops)
	}
	return head, ops[0].Operation
}

// UnitSeedDriver binds a controller's read/start/finish boundary without depending on its state type.
type UnitSeedDriver[B any] struct {
	Read          func(config.WorkstreamID) (B, bool, error)
	Start, Finish func(context.Context, B, string) (bool, bool, error)
	Open          func(context.Context, config.WorkstreamID, string) (workspace.Worktree, string, error)
	Complete      func(*testing.T, *trace.Repository, config.WorkstreamID, string, string) trace.QueuedTurn
}

func SeedChecking[B any](t *testing.T, d UnitSeedDriver[B], repository *trace.Repository, stream config.WorkstreamID, unit, outcome string) {
	t.Helper()

	ctx := context.Background()
	b, found, err := d.Read(stream)
	Must(t, err)
	if !found {
		t.Fatal("fixture workstream is not building")
	}
	started, blocked, err := d.Start(ctx, b, unit)
	Must(t, err)
	if !started || blocked {
		t.Fatalf("fixture unit %s did not start", unit)
	}
	w, _, err := d.Open(ctx, stream, unit)
	Must(t, err)
	Must(t, os.WriteFile(filepath.Join(w.Path, MasonWrote), []byte("package trace\n"), 0644))
	d.Complete(t, repository, stream, unit, outcome)
	b, _, err = d.Read(stream)
	Must(t, err)
	moved, blocked, err := d.Finish(ctx, b, unit)
	Must(t, err)
	if !moved || blocked {
		t.Fatalf("fixture unit %s did not enter checking", unit)
	}
}

func SeedReview(t *testing.T, repository *trace.Repository, stream config.WorkstreamID, unit, outcome, reviewing string, seed func(*testing.T, *trace.Repository, config.WorkstreamID, string, string), checks func(*testing.T, *trace.Repository, config.WorkstreamID)) {
	t.Helper()
	seed(t, repository, stream, unit, outcome)
	checks(t, repository, stream)
	if state, err := repository.Workflow(stream, trace.UnitSubject(unit)); err != nil || state.Value != reviewing {
		t.Fatalf("fixture unit %s is %s, not reviewing: %v", unit, state.Value, err)
	}
}

func ApproveDirectly[I any](t *testing.T, repository *trace.Repository, stream config.WorkstreamID, unit string, prepare func(context.Context, config.WorkstreamID, string) (coreadapter.ReviewRequest, I, error), encode func(I, string) ([]byte, error), turnID func(string, uint64) string, document func(string) string, apply func(context.Context, config.WorkstreamID, string, trace.WorkflowState, bool) error, approvedState string) {
	t.Helper()
	ctx := context.Background()

	state, err := repository.Workflow(stream, trace.UnitSubject(unit))
	Must(t, err)
	_, identity, err := prepare(ctx, stream, unit)
	Must(t, err)
	turn := turnID(unit, state.Version)
	data, err := encode(identity, turn)
	Must(t, err)
	docs, err := trace.Read[trace.Document](repository, stream)
	Must(t, err)
	var latest trace.Document
	for _, d := range docs {
		if d.ID == document(unit) {
			latest = d
		}
	}
	latest.Revision++
	latest.Content = string(data) + "\n"
	latest.At = time.Now()
	latest.Cause = turn
	Must(t, repository.RecordDocuments(ctx, []trace.Document{latest}))
	Must(t, apply(ctx, stream, unit, state, false))
	if approved, err := repository.Workflow(stream, trace.UnitSubject(unit)); err != nil || approved.Value != approvedState {
		t.Fatalf("unit %s is %+v: %v", unit, approved, err)
	}
}

func ApprovedReview[R any](t *testing.T, stream config.WorkstreamID, unit string, approval func(config.WorkstreamID, string, int) (trace.Document, R, bool, error)) (trace.Document, R) {
	t.Helper()

	review, result, ok, err := approval(stream, unit, 0)
	Must(t, err)
	if !ok {
		t.Fatalf("unit %s has no satisfactory review: %+v", unit, review)
	}
	return review, result
}

func SeedBuild(t *testing.T, stop func(*testing.T), cfg func() *config.Config, record func(*testing.T, *trace.Repository, string, string, string, string) config.WorkstreamID, key, graph, backend, base string) (config.WorkstreamID, *trace.Repository) {
	t.Helper()
	stop(t)
	repository, err := trace.Open(cfg().Root, cfg().Project)
	Must(t, err)
	t.Cleanup(func() { repository.Close() })
	return record(t, repository, key, graph, backend, base), repository
}

// ConfigureMasonThreads sets workspace/capacity inputs and classifier selection for the fake workers.
func ConfigureMasonThreads(t *testing.T, root, backend, capacity, classifier string, original func(*trace.Repository, *config.Config) (coreadapter.Reconciler, error)) func(*trace.Repository, *config.Config) (coreadapter.Reconciler, error) {
	t.Helper()
	OnWorkspaces(t, root, backend)
	path := filepath.Join(root, "config.toml")
	data, err := os.ReadFile(path)
	Must(t, err)
	Must(t, os.WriteFile(path, []byte(strings.Replace(string(data), "committee = 1\n", "committee = 1\n"+capacity, 1)), 0600))
	if classifier == "" {
		return original
	}
	return func(r *trace.Repository, cfg *config.Config) (coreadapter.Reconciler, error) {
		cfg.Project.Classifier = classifier
		return original(r, cfg)
	}
}

// ScriptMasons installs the first worker turns and failing clarification turns.
func ScriptMasons(engine *fixturekit.DemoEngine, turnID func(string) string) *FakeMasons {
	fake := &FakeMasons{Runs: map[string][]agent.Request{}, Play: map[string]func(context.Context, agent.Request, *mcp.ClientSession) error{}}
	fake.Chief = &Chief{P: &Faults{}, Released: map[string]bool{}, Held: map[string]chan struct{}{}}
	engine.Mu.Lock()
	defer engine.Mu.Unlock()
	engine.Turns["*"] = fake.Chief.Turn
	engine.Turns[turnID("resume")] = fake.Turn
	engine.Turns[turnID("dedupe")] = fake.Turn
	for _, unit := range []string{"resume", "dedupe"} {
		for i := 1; i <= 3; i++ {
			name := fmt.Sprintf("%s-clarify-%d", MasonAgent(unit), i)
			fake.Play[name] = FailMasonTurn
			engine.Turns[name] = fake.Turn
		}
	}
	return fake
}

func ExtendParallelMasons(t *testing.T, repository *trace.Repository, now time.Time, engine *fixturekit.DemoEngine, fake *FakeMasons, turnID func(string) string, actor trace.Actor) {
	t.Helper()
	mapping, err := kb.Load(repository)
	Must(t, err)
	for _, name := range []string{"upload", "audit"} {
		mapping.Entities = append(mapping.Entities, kb.Entity{ID: "internal." + name, Name: "internal/" + name, Aliases: []string{}, Paths: []string{"internal/" + name}, Owners: []string{}, PartOf: []string{}})
	}
	Must(t, kb.Store(context.Background(), repository, mapping, now, actor, "fixture"))
	engine.Mu.Lock()
	defer engine.Mu.Unlock()
	for _, unit := range []string{"upload", "audit"} {
		engine.Turns[turnID(unit)] = fake.Turn
		for i := 1; i <= 3; i++ {
			name := fmt.Sprintf("%s-clarify-%d", MasonAgent(unit), i)
			fake.Play[name] = FailMasonTurn
			engine.Turns[name] = fake.Turn
		}
	}
}
func RaiseFixtureContest(t *testing.T, r *trace.Repository, stream config.WorkstreamID, unit string, contest func() (trace.Header, bool, error), actor trace.Actor, escalate string, path func(string, string) string) {
	t.Helper()
	header, found, err := contest()
	Must(t, err)
	if !found {
		t.Fatal("fixture unit has no contest")
	}
	data, err := json.Marshal(api.ChiefContestDecision{Contest: header.ID, Decision: escalate, Turn: "fixture"})
	Must(t, err)
	h := header
	h.Schema = "osmia.trace.document"
	h.ID = trace.UnitSubject(unit) + "-chief-" + header.ID
	h.Revision = 1
	h.Actor = actor
	h.Cause = header.ID
	Must(t, r.RecordDocuments(context.Background(), []trace.Document{{Header: h, Path: path(unit, header.ID), Content: string(data)}}))
}

func SeedApprovedUnits(t *testing.T, seed func(string, string), approve func(string, string)) {
	t.Helper()
	seed("resume", "resume from the last chunk")
	seed("dedupe", "skip acknowledged chunks")
	approve("resume", "spec#1")
	approve("dedupe", "spec#2")
}

func SeedBuilding(t *testing.T, clone, key, graph string, backend func() (string, error), record func(string, string, string, string) config.WorkstreamID, refresh func() error) config.WorkstreamID {
	t.Helper()
	selected, err := backend()
	Must(t, err)
	base := strings.TrimSpace(fixturekit.DemoGit(t, clone, "-C", clone, "rev-parse", "HEAD"))
	stream := record(key, graph, selected, base)
	Must(t, refresh())
	return stream
}

func SeedDelivery(t *testing.T, repository *trace.Repository, stream config.WorkstreamID, merge func(string, map[string]string) string, pass func(context.Context) error, governing func(context.Context) (api.FinalReport, error), actor trace.Actor, document, path, outcome string) api.FinalReport {
	t.Helper()
	ctx := context.Background()
	merge("resume", map[string]string{"resume.go": "package demo\n"})
	merge("dedupe", map[string]string{"dedupe.go": "package demo\n"})
	Must(t, pass(ctx))
	report, err := governing(ctx)
	Must(t, err)
	report.Review, report.Outcome, report.Branch = 1, outcome, intake.FeatureBranch(stream)
	report.Summary = "Resumable uploads"
	report.Criteria = []api.FinalCriterion{
		{Criterion: "spec#1", Text: "Resume upload", Evidence: "resume.go and TestResume"},
		{Criterion: "spec#2", Text: "Skip duplicate chunks", Evidence: "dedupe.go and TestDedupe"},
	}
	record := func(r api.FinalReport, rev int) {
		content, err := json.Marshal(r)
		Must(t, err)
		Must(t, repository.RecordDocuments(ctx, []trace.Document{{Header: trace.Header{Schema: "osmia.trace.document", Version: trace.Version, ID: document, Revision: rev, Project: repository.Project(), Workstream: stream, At: time.Now().UTC(), Actor: actor, Cause: "final-review"}, Path: path, Content: string(content)}}))
	}
	record(report, 1)

	return report
}

func WithWeb(t *testing.T, root, addr string) {
	t.Helper()
	path := filepath.Join(root, "config.toml")
	top, err := os.ReadFile(path)
	Must(t, err)
	Must(t, os.WriteFile(path, append(top, []byte("[listen]\nweb = \""+addr+"\"\n")...), 0600))
}

func DrivePass(t *testing.T, repo *trace.Repository, halt func() error, pass func() error) {
	t.Helper()
	if err := halt(); err != nil {
		t.Fatalf("idle the controller: %v", err)
	}
	if err := repo.Serialize(pass); err != nil {
		t.Fatalf("drive the pass: %v", err)
	}
}
