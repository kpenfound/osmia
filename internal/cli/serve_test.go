package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kpenfound/agentcore/agent"
	"github.com/kpenfound/agentcore/agent/agenttest/enforcertest"
	wire "github.com/kpenfound/osmia/internal/api"
	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/questions"
	"github.com/kpenfound/osmia/internal/service"
	"github.com/kpenfound/osmia/internal/status"
	"github.com/kpenfound/osmia/internal/trace"
)

var launches = &launchEngine{runs: map[string]*enforcertest.Turn{}, views: map[string]map[string]string{}, started: make(chan struct{})}

var production = enforcement

func TestMain(m *testing.M) {
	enforcement = func(root config.Root) service.Enforcement {
		e := production(root)
		e.Engine = launches
		return e
	}
	os.Exit(m.Run())
}

const refusal = "this platform cannot hold the claude sandbox"

type launchEngine struct {
	mu      sync.Mutex
	turns   []string
	runs    map[string]*enforcertest.Turn
	views   map[string]map[string]string
	started chan struct{}
}

func (e *launchEngine) Enforcer(settings coreadapter.ExecutionSettings) (agent.Enforcer, error) {
	f := &enforcertest.Enforcer{Sandbox: settings.Mode, Image: settings.Image,
		Agent: func(_ context.Context, turn *enforcertest.Turn) (*agent.Result, error) {
			var view map[string]string
			if strings.HasPrefix(turn.Request.Name, "message_") && turn.Request.Grants != nil {
				var err error
				if view, err = viewFiles(turn.Request.Grants.Mounts[0].Path); err != nil {
					return nil, err
				}
			}
			e.mu.Lock()
			e.turns = append(e.turns, turn.Request.Name)
			e.runs[turn.Request.Name] = turn
			if view != nil {
				e.views[turn.Request.Name] = view
			}
			close(e.started)
			e.started = make(chan struct{})
			e.mu.Unlock()
			return &agent.Result{ClaudeID: "session-" + turn.Request.Name, ResultText: "Noted", NumTurns: 1}, nil
		}}
	if settings.Mode == agent.SandboxClaude {
		f.PrepareErr = fmt.Errorf("%w: %s", agent.ErrUnsupported, refusal)
	}
	return f, nil
}

func viewFiles(dir string) (map[string]string, error) {
	files := map[string]string{}
	err := filepath.WalkDir(dir, func(path string, entry os.DirEntry, err error) error {
		if err != nil || entry.IsDir() {
			return err
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(dir, path)
		files[filepath.ToSlash(rel)] = string(data)
		return err
	})
	return files, err
}

func (e *launchEngine) await(t *testing.T, name string, diagnostics ...func()) *enforcertest.Turn {
	t.Helper()
	timedOut := time.After(time.Minute)
	for {
		e.mu.Lock()
		turn, started := e.runs[name], e.started
		e.mu.Unlock()
		if turn != nil {
			return turn
		}
		select {
		case <-started:
		case <-timedOut:
			for _, diagnostic := range diagnostics {
				diagnostic()
			}
			e.mu.Lock()
			defer e.mu.Unlock()
			t.Fatalf("turn %s did not run; ran %v", name, e.turns)
		}
	}
}

func (e *launchEngine) reset() {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.turns = nil
	e.runs = map[string]*enforcertest.Turn{}
	e.views = map[string]map[string]string{}
	close(e.started)
	e.started = make(chan struct{})
}

func serve(t *testing.T, root string) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan int, 1)
	var diag bytes.Buffer
	go func() {
		result <- Run(ctx, []string{"serve", "--root", root}, strings.NewReader(""), &bytes.Buffer{}, &diag)
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case code := <-result:
			if t.Failed() {
				t.Logf("service diagnostics: %s", diag.String())
			}
			if code != 0 {
				t.Errorf("serve exited %d: %s", code, diag.String())
			}
		case <-time.After(time.Minute):
			t.Error("serve did not stop")
		}
	})
	c := service.NewClient(filepath.Join(root, "osmia.sock"))
	defer c.Close()
	awaitHealth(t, c, true, 5*time.Second, diag.String)
}

type servedProject struct {
	root   string
	clone  string
	client *service.Client
	added  wire.ProjectResponse
}

func serveWithProject(t *testing.T) servedProject {
	t.Helper()
	launches.reset()
	opts, clone := emptyFixture(t)
	root := opts.Config.Root
	serve(t, root)
	var added wire.ProjectResponse
	must(t, json.Unmarshal([]byte(successful(t, root, "project", "add", "dagger", "--upstream", "dagger/dagger", "--fork", "owner/dagger", "--clone", clone, "--json")), &added))
	c := service.NewClient(filepath.Join(root, "osmia.sock"))
	t.Cleanup(c.Close)
	return servedProject{root: root, clone: clone, client: c, added: added}
}

func (p servedProject) handIn(t *testing.T) wire.HandInResponse {
	t.Helper()
	setCharter(t, p.root, p.added.Project.ID, "1. Keep changes small.\n")
	design := filepath.Join(filepath.Dir(p.clone), "design.md")
	must(t, os.WriteFile(design, []byte("# Design\n"), 0600))
	var handed wire.HandInResponse
	must(t, json.Unmarshal([]byte(successful(t, p.root, "handin", string(p.added.Project.ID), design, "--json")), &handed))
	return handed
}

func TestAddingAProjectRunsNoRoleTurns(t *testing.T) {
	serveWithProject(t)
	launches.mu.Lock()
	ran := slices.Clone(launches.turns)
	launches.mu.Unlock()
	if len(ran) != 0 {
		t.Fatalf("adding a project ran turns %v", ran)
	}
}

func TestHandInStartsTheFirstDraftTurn(t *testing.T) {
	p := serveWithProject(t)
	p.handIn(t)
	launches.await(t, "draft-1-1")
}

func TestOwnerMessageRunsOneOrdinaryTurnToCompletion(t *testing.T) {
	p := serveWithProject(t)
	handed := p.handIn(t)
	launches.await(t, "draft-1-1")

	var sent wire.ConversationEntry
	must(t, json.Unmarshal([]byte(successful(t, p.root, "send", string(handed.Workstream), "Start with uploads.", "--json")), &sent))
	diagnostics := func() { chiefDispatchDiagnostics(t, p.client, p.root, p.added.Project.ID, handed.Workstream) }
	t.Cleanup(func() {
		if t.Failed() {
			diagnostics()
		}
	})
	chief(t, launches.await(t, sent.Turn, diagnostics), p.root, string(p.added.Project.ID), string(handed.Workstream))
	var conversation wire.ConversationResponse
	var messages, responses []wire.ConversationEntry
	eventually(t, time.Minute, func() (bool, string) {
		var err error
		conversation, err = p.client.Conversation(context.Background(), handed.Workstream)
		must(t, err)
		messages, responses = nil, nil
		for _, entry := range conversation.Entries {
			if entry.Turn != sent.Turn {
				continue
			}
			switch entry.Kind {
			case "message":
				messages = append(messages, entry)
			case "response":
				responses = append(responses, entry)
			}
		}
		return len(responses) > 0, fmt.Sprintf("ordinary owner turn %s did not complete: %+v", sent.Turn, conversation)
	})
	if len(messages) != 1 || len(responses) != 1 || messages[0].State != wire.TurnDone || responses[0].State != wire.TurnDone || responses[0].Text != "Noted" {
		t.Fatalf("ordinary owner turn did not complete exactly once: %+v", conversation)
	}
}

func chiefDispatchDiagnostics(t *testing.T, c *service.Client, root string, project config.ProjectID, stream config.WorkstreamID) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	logView := func(name string, value any, err error) {
		data, encodeErr := json.Marshal(value)
		t.Logf("chief %s: %s; error: %v", name, data, errors.Join(err, encodeErr))
	}
	conversation, err := c.Conversation(ctx, stream)
	logView("conversation", conversation, err)
	var feed wire.FeedResponse
	err = c.Do(ctx, "GET", wire.Prefix+"/feed/"+string(stream), nil, &feed)
	logView("feed", feed, err)
	state, err := c.Status(ctx, stream)
	logView("status", state, err)
	for _, body := range traceBodies(t, workstreamDatabase(t, root, project, string(stream)), "workstreams/"+string(stream)+"/agents/"+trace.Manager+"/log.jsonl") {
		t.Logf("durable chief log record: %s", body)
	}
}

func chief(t *testing.T, turn *enforcertest.Turn, root, project, workstream string) {
	t.Helper()
	resolved, err := filepath.EvalSymlinks(root)
	must(t, err)
	threads := filepath.Join(resolved, "threads", project, workstream) + string(filepath.Separator)
	if !strings.HasPrefix(turn.Request.SessionDir, threads) {
		t.Fatalf("session directory %s is not under %s", turn.Request.SessionDir, threads)
	}
	if entries, err := os.ReadDir(turn.Request.Workspace.Directory()); err != nil || len(entries) != 0 {
		t.Fatalf("working directory %s is not empty: %v %v", turn.Request.Workspace.Directory(), entries, err)
	}
	launches.mu.Lock()
	view := launches.views[turn.Request.Name]
	launches.mu.Unlock()
	var handed []string
	for path, content := range view {
		if strings.HasPrefix(path, "tools/") || strings.HasPrefix(path, "inspections/") {
			t.Fatalf("view holds service record %s", path)
		}
		if strings.HasPrefix(path, "handed/") && content == "# Design\n" {
			handed = append(handed, path)
		}
	}
	if len(handed) != 1 {
		t.Fatalf("view does not hold the handed design: %v", slices.Collect(maps.Keys(view)))
	}
	if !strings.Contains(turn.Request.SystemPrompt, "file_read") {
		t.Fatalf("system prompt does not describe the view: %s", turn.Request.SystemPrompt)
	}
	if !strings.Contains(turn.Request.Prompt, "read-only directory "+turn.Request.Grants.Mounts[0].Path) || strings.Contains(turn.Request.SystemPrompt, "read-only directory") {
		t.Fatalf("the view's location belongs to the turn's prompt, not the system prompt: %s", turn.Request.Prompt)
	}
	if work := turn.Request.Workspace.Directory(); filepath.Dir(work) != turn.Request.SessionDir {
		t.Fatalf("working directory %s is not the session's own", work)
	}
	granted := append([]string{"file_read", "factory_context", "record_discovery", "read_remote_file", "read_cloud_trace", status.ToolName, "notify", "capacity", "inspect_code", "preview", "record_delivery_brief", "pull_request_checks", "ask_owner_about_checks", "prioritise", "pause", "resume", "request_change", "decide_change", "decide_amendment", "decide_charter", "resolve_contested", "resolve_objection", "decide", "flake_fix", "triage_finding", "move_unit", "move_workstream", "hand_back_drift", "drop_dependency", "retry_change", "follow_up"}, questions.ChiefTools...)
	granted = append(granted, "project_context")
	var tools []string
	for _, allowed := range turn.Request.Profile.AllowedTools {
		server, tool, ok := strings.Cut(strings.TrimPrefix(allowed, "mcp__"), "__")
		if !ok || !turn.Policy.Allows("mcp__"+server+"__"+tool) {
			t.Fatalf("allowed tool %s is outside the policy", allowed)
		}
		tools = append(tools, tool)
	}
	if !slices.Equal(tools, granted) || slices.Contains(tools, "file_write") {
		t.Fatalf("chief tools %v, want %v", tools, granted)
	}
}

func productionFixture(t *testing.T) (config.Root, service.Enforcement) {
	t.Helper()
	root, err := config.ResolveRoot(t.TempDir(), "")
	must(t, err)
	return root, production(root)
}

func TestProductionCachesSkillsUnderTheRoot(t *testing.T) {
	root, e := productionFixture(t)
	if e.Skills == nil || e.Skills.Dir != filepath.Join(root.String(), "skills") {
		t.Fatalf("skills %#v", e.Skills)
	}
}

func TestProductionEngineMountsTheSkillCache(t *testing.T) {
	root, e := productionFixture(t)
	cache := filepath.Join(root.String(), "skills")
	engine, ok := e.Engine.(coreadapter.CoreEngine)
	if !ok || engine.Runner.Skills != e.Skills || !slices.Equal(engine.Runner.SkillMountDirs, []string{cache}) || !slices.Equal(engine.SkillDirs(), []string{cache}) {
		t.Fatalf("engine %#v", e.Engine)
	}
}

func TestProductionEngineHoldsNothingBesidesTheSkills(t *testing.T) {
	_, e := productionFixture(t)
	engine, ok := e.Engine.(coreadapter.CoreEngine)
	if !ok {
		t.Fatalf("engine %#v", e.Engine)
	}
	engine.Runner.Skills, engine.Runner.SkillMountDirs = nil, nil
	if !reflect.DeepEqual(engine, coreadapter.CoreEngine{}) {
		t.Fatalf("engine %#v", e.Engine)
	}
}

func TestProductionHostsUseTheCoreTransportAndContainer(t *testing.T) {
	_, e := productionFixture(t)
	host, ok := e.Hosts.(*coreadapter.MCPHost)
	if !ok || !reflect.DeepEqual(host.Transport, coreadapter.CoreTransport{}) || host.Container == nil || !reflect.DeepEqual(host.Sbx, coreadapter.SbxTransport()) {
		t.Fatalf("hosts %#v", e.Hosts)
	}
}

func TestEnforceGivesTheArchitectAndCommitteeTheProductionEngineAndHosts(t *testing.T) {
	_, e := productionFixture(t)
	opts := service.Enforce(service.Options{}, e)
	if opts.Threads == nil || opts.Architect == nil || opts.Committee == nil {
		t.Fatalf("options %+v", opts)
	}
	if !reflect.DeepEqual(*opts.Architect, service.Architect{Engine: e.Engine, Hosts: e.Hosts}) {
		t.Fatalf("architect enforcement %#v", *opts.Architect)
	}
	if !reflect.DeepEqual(*opts.Committee, service.Committee{Engine: e.Engine, Hosts: e.Hosts}) {
		t.Fatalf("committee enforcement %#v", *opts.Committee)
	}
}
