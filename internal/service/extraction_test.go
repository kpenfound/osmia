package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kpenfound/busybees/core/agent"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/kb"
	"github.com/kpenfound/osmia/internal/scheduler"
	"github.com/kpenfound/osmia/internal/trace"
)

// awaitExtraction polls status until the latest extraction is terminal.
func awaitExtraction(t *testing.T, c *Client) ExtractionState {
	t.Helper()
	deadline := time.Now().Add(demoTimeout)
	for {
		cfg, err := c.Configuration(context.Background())
		must(t, err)
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

// awaitExtractionAcknowledged polls the librarian workstream until the latest
// extraction operation is acknowledged. The reconcile controller records the
// acknowledgement as a commit after the one carrying the result, so a terminal
// extraction state is not yet the operation's last commit.
func awaitExtractionAcknowledged(t *testing.T, s *Service) {
	t.Helper()
	deadline := time.Now().Add(demoTimeout)
	for {
		if s.active != nil {
			ops, err := s.active.repository.Operations(librarianWorkstream(s.active.repository.Project()))
			must(t, err)
			extractions, acknowledged := 0, 0
			for _, o := range ops {
				if o.Operation.Action != ExtractAction {
					continue
				}
				extractions++
				if o.Acknowledged {
					acknowledged++
				}
			}
			if extractions > 0 && extractions == acknowledged {
				return
			}
		}
		if time.Now().After(deadline) {
			t.Fatal("extraction was not acknowledged")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// librarianFixture is a project root, a clone with tracked and untracked files
// and the fake engine the librarian's turns run in.
type librarianFixture struct {
	opts     Options
	clone    string
	engine   *demoEngine
	sessions *demoSessions
	clock    *demoClock
}

// librarianContainer runs the librarian in a container, the only sandbox
// the core executor accepts.
const librarianContainer = `[roles.librarian]
sandbox = "container"
image = "fixture-image"
`

func newLibrarianFixture(t *testing.T) *librarianFixture {
	t.Helper()
	opts, clone := projectFixture(t)
	home := filepath.Dir(clone)
	configFile, err := os.OpenFile(filepath.Join(opts.Config.Root, "config.toml"), os.O_APPEND|os.O_WRONLY, 0)
	must(t, err)
	_, err = configFile.WriteString(librarianContainer)
	must(t, errors.Join(err, configFile.Close()))
	must(t, os.MkdirAll(filepath.Join(clone, "internal", "trace"), 0700))
	must(t, os.WriteFile(filepath.Join(clone, "internal", "trace", "git.go"), []byte("package trace\n"), 0600))
	must(t, os.WriteFile(filepath.Join(clone, "CODEOWNERS"), []byte("/internal/ @core\n"), 0600))
	must(t, os.WriteFile(filepath.Join(clone, "AGENTS.md"), []byte("# Agents\n"), 0600))
	must(t, os.WriteFile(filepath.Join(clone, "secret.env"), []byte("TOKEN="+demoSecret+"\n"), 0600))
	must(t, os.MkdirAll(filepath.Join(clone, "scratch"), 0700))
	must(t, os.WriteFile(filepath.Join(clone, "scratch", "notes.txt"), []byte("untracked\n"), 0600))
	must(t, os.MkdirAll(filepath.Join(clone, "node_modules", "left-pad"), 0700))
	must(t, os.WriteFile(filepath.Join(clone, "node_modules", "left-pad", "index.js"), []byte("ignored\n"), 0600))
	must(t, os.WriteFile(filepath.Join(clone, ".git", "info", "exclude"), []byte("node_modules/\n"), 0600))
	demoGit(t, home, "-C", clone, "add", "internal", "CODEOWNERS", "AGENTS.md")
	demoGit(t, home, "-C", clone, "-c", "user.name=Owner", "-c", "user.email=owner@example.invalid", "commit", "-qm", "base")
	sessions := &demoSessions{byKey: map[string]*mcp.ClientSession{}}
	engine := &demoEngine{sessions: sessions, turns: map[string]demoTurn{}}
	engine.resume = func(coreadapter.Profile, coreadapter.Profile, coreadapter.BackendSession) error {
		return coreadapter.ErrResumeUnavailable
	}
	clock := &demoClock{now: demoStart}
	opts.Reconciliation.Now = clock.Now
	opts.Librarian = &Librarian{Engine: engine, Hosts: &coreadapter.MCPHost{Transport: &demoTransport{sessions: sessions}}}
	return &librarianFixture{opts: opts, clone: clone, engine: engine, sessions: sessions, clock: clock}
}

// script installs the fake librarian's behaviour for one turn: it writes the
// given output files and returns a successful result.
func (f *librarianFixture) script(turn string, files map[string]string, check func(ctx context.Context, req agent.Request, verified *agent.Turn, tools *mcp.ClientSession) error) {
	f.engine.mu.Lock()
	defer f.engine.mu.Unlock()
	f.engine.turns[turn] = func(ctx context.Context, req agent.Request, verified *agent.Turn, tools *mcp.ClientSession) (*agent.Result, error) {
		var err error
		if check != nil {
			err = check(ctx, req, verified, tools)
		}
		for path, content := range files {
			if _, e := callTool(ctx, tools, "file_write", map[string]any{"path": path, "content": content}); e != nil {
				err = errors.Join(err, fmt.Errorf("write %s: %w", path, e))
			}
		}
		if err != nil {
			return nil, err
		}
		return &agent.Result{ClaudeID: "session-" + turn, ResultText: "Knowledge base written", SessionDir: req.SessionDir, NumTurns: 2}, nil
	}
}

// trackedSeed is the entity seed of the clone's tracked files alone: the
// untracked scratch directory seeds an entity from the clone on disk but not
// from the librarian's copy.
func (f *librarianFixture) trackedSeed(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	must(t, copyTracked(context.Background(), f.clone, dir))
	seed, err := kb.Seed(dir)
	must(t, err)
	full, err := kb.Seed(f.clone)
	must(t, err)
	has := func(m kb.Map, id string) bool {
		return slices.ContainsFunc(m.Entities, func(e kb.Entity) bool { return e.ID == id })
	}
	if !has(full, "scratch") || !has(full, "node-modules") || has(seed, "scratch") || has(seed, "node-modules") {
		t.Fatalf("fixture seeds: clone %v, tracked %v", full, seed)
	}
	data, err := kb.Encode(seed)
	must(t, err)
	return string(data)
}

// queueLibrarianTurn accepts a librarian turn of extraction n the way the
// extractor does, without claiming it.
func queueLibrarianTurn(t *testing.T, repo *trace.Repository, cfg *config.Config, n, attempt int, at time.Time) trace.TurnRequest {
	t.Helper()
	name := cfg.Roles[librarianRole].Profile
	p := cfg.Profiles[name]
	timeout, err := time.ParseDuration(p.Timeout)
	must(t, err)
	stream := librarianWorkstream(repo.Project())
	_, event := extractionIDs(n)
	turn := turnID(n, attempt)
	req := trace.TurnRequest{Header: trace.Header{Schema: "osmia.trace.turn-request", Version: trace.Version, ID: "request_" + turn, Revision: 1, Project: repo.Project(), Workstream: stream, At: at, Actor: trace.Actor{Kind: "service", ID: "librarian-extraction"}, Cause: trace.OperationID(repo.Project(), stream, event), Depth: 1},
		AgentID: librarianAgent, ThreadID: librarianThread, TurnID: turn, Profile: coreadapter.Profile{Name: name, Backend: p.Agent, Model: p.Model, Effort: p.Effort, Timeout: timeout, MaxTurns: p.MaxTurns}, SystemPrompt: librarianSystemPrompt(cfg.Project), Prompt: librarianPrompt(cfg.Project)}
	_, err = repo.EnqueueTurn(context.Background(), req)
	must(t, err)
	return req
}

// reopen opens the registered project's trace while the service is stopped.
func (f *librarianFixture) reopen(t *testing.T, added ProjectResponse) (*trace.Repository, *config.Config) {
	t.Helper()
	cfg, err := config.Load(f.opts.Config)
	must(t, err)
	repo, err := trace.Open(cfg.Root, config.Project{ID: added.Project.ID, Clone: added.Project.Clone})
	must(t, err)
	return repo, cfg
}

// extractStatus posts an extraction request and returns the HTTP status.
func extractStatus(t *testing.T, s *Service, body string) int {
	t.Helper()
	w := httptest.NewRecorder()
	s.handle(w, httptest.NewRequest(http.MethodPost, Prefix+"/projects/extract", strings.NewReader(body)))
	return w.Code
}

func (f *librarianFixture) runs() []string {
	f.engine.mu.Lock()
	defer f.engine.mu.Unlock()
	return slices.Clone(f.engine.runs)
}

func entitiesWith(t *testing.T, seed kb.Map, alias string) string {
	t.Helper()
	m := kb.Map{Version: kb.Version}
	for _, e := range seed.Entities {
		if e.ID == "internal.trace" {
			e.Aliases = append(e.Aliases, alias)
		}
		m.Entities = append(m.Entities, e)
	}
	data, err := kb.Encode(m)
	must(t, err)
	return string(data)
}

// checkLibrarianBoundary makes the negative assertions from inside the turn:
// only tracked files, the current knowledge base and the seed are visible, the
// role has file tools and notes only, and nothing carries VCS access.
func checkLibrarianBoundary(ctx context.Context, req agent.Request, verified *agent.Turn, tools *mcp.ClientSession, clone, entities, seed string) error {
	var problems []error
	fail := func(format string, args ...any) { problems = append(problems, fmt.Errorf(format, args...)) }
	listed, err := tools.ListTools(ctx, nil)
	if err != nil {
		return err
	}
	var names []string
	for _, tool := range listed.Tools {
		names = append(names, tool.Name)
	}
	slices.Sort(names)
	if want := []string{"file_read", "file_write", "notes_read", "notes_write"}; !slices.Equal(names, want) {
		fail("role tools %v, want %v", names, want)
	}
	for _, name := range []string{"shell", "git_push"} {
		if _, err := callTool(ctx, tools, name, map[string]any{}); err == nil {
			fail("runtime called %s", name)
		}
	}
	for path, want := range map[string]string{"repo/internal/trace/git.go": "package trace\n", "repo/AGENTS.md": "# Agents\n", "kb/entities.json": entities, "seed/entities.json": seed} {
		got, err := callTool(ctx, tools, "file_read", map[string]any{"path": path})
		var text string
		if err == nil {
			err = json.Unmarshal([]byte(got), &text)
		}
		if err != nil || text != want {
			fail("read %s: %q %v", path, got, err)
		}
	}
	for _, path := range []string{"repo/secret.env", "repo/.git/HEAD", ".git/HEAD", filepath.Join(clone, "AGENTS.md"), "../config.toml"} {
		if _, err := callTool(ctx, tools, "file_read", map[string]any{"path": path}); err == nil {
			fail("runtime read %s", path)
		}
	}
	// The private copy of the repository is writable and discarded.
	if _, err := callTool(ctx, tools, "file_write", map[string]any{"path": "repo/AGENTS.md", "content": "tampered"}); err != nil {
		fail("write in the private copy: %v", err)
	}
	checkGrants(req, verified, clone, fail)
	if req.Profile.VCSAccess || req.Workspace == nil || req.Workspace.VCS() != nil || len(req.VCSEnv) != 0 || req.Profile.Name != "librarian" {
		fail("request carries VCS access or another role: %+v", req.Profile)
	}
	for key, value := range req.Env {
		if key != "OSMIA_MCP_TOKEN" || strings.Contains(value, demoSecret) {
			fail("runtime environment exposes %s", key)
		}
	}
	for _, want := range []string{"output/kb/<subsystem>.md", "output/kb/entities.json", "seed/entities.json", "CLAUDE.md", "AGENTS.md", "CONTRIBUTING.md", "^[a-z0-9][a-z0-9-]*$", "tests that matter", "what breaks when you touch what", "decisions behind it"} {
		if !strings.Contains(req.Prompt, want) {
			fail("prompt lacks %q", want)
		}
	}
	if strings.Contains(req.Prompt, demoSecret) || strings.Contains(req.SystemPrompt, demoSecret) {
		fail("prompt exposes the untracked secret")
	}
	return errors.Join(problems...)
}

func operationsOf(t *testing.T, s *Service, id config.ProjectID) []trace.OperationRecord {
	t.Helper()
	ops, err := s.active.repository.Operations(librarianWorkstream(id))
	must(t, err)
	var out []trace.OperationRecord
	for _, o := range ops {
		if o.Operation.Action == ExtractAction {
			out = append(out, o)
		}
	}
	return out
}

func proseOnDisk(t *testing.T, traceDir string) map[string]string {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(traceDir, "kb"))
	must(t, err)
	out := map[string]string{}
	for _, e := range entries {
		data, err := os.ReadFile(filepath.Join(traceDir, "kb", e.Name()))
		must(t, err)
		out[e.Name()] = string(data)
	}
	return out
}

func TestExtractionRecordsKnowledgeBaseAndReruns(t *testing.T) {
	for _, name := range []string{"GITHUB_TOKEN", "GH_TOKEN"} {
		t.Setenv(name, demoSecret)
	}
	f := newLibrarianFixture(t)
	ctx := context.Background()
	seed, err := kb.Seed(f.clone)
	must(t, err)
	tracked := f.trackedSeed(t)
	refined := entitiesWith(t, seed, "history")
	cloneBefore := snapshot(t, f.clone)
	f.script("extract-1-1", map[string]string{"output/kb/trace.md": "# trace\n\nRun go test ./internal/trace.\n", "output/kb/service.md": "# service\n\nOne process.\n", "output/kb/entities.json": refined},
		func(ctx context.Context, req agent.Request, verified *agent.Turn, tools *mcp.ClientSession) error {
			err := checkLibrarianBoundary(ctx, req, verified, tools, f.clone, tracked, tracked)
			if req.ResumeID != "" || strings.Contains(req.Prompt, "Knowledge base written") {
				err = errors.Join(err, fmt.Errorf("first turn carries history: %q", req.ResumeID))
			}
			_, e := callTool(ctx, tools, "notes_write", map[string]any{"text": "librarian: trace tests are slow"})
			return errors.Join(err, e)
		})
	s, c := start(t, f.opts)
	added, err := c.AddProject(ctx, request(f.clone))
	must(t, err)
	id := added.Project.ID
	traceDir := added.Project.Trace
	if x := awaitExtraction(t, c); x.Extraction != 1 || x.State != "succeeded" || x.Reason != "" || x.At.IsZero() {
		t.Fatalf("first extraction: %+v", x)
	}
	if runs := f.runs(); !slices.Equal(runs, []string{"extract-1-1"}) {
		t.Fatalf("backend runs %v", runs)
	}
	// Every file landed in one commit as librarian-authored revisions.
	for id, want := range map[string]struct {
		revision int
		path     string
		content  string
	}{"subsystem-trace": {1, "kb/trace.md", "# trace\n\nRun go test ./internal/trace.\n"}, "subsystem-service": {1, "kb/service.md", "# service\n\nOne process.\n"}, trace.EntitiesDocument: {2, trace.EntitiesPath, refined}} {
		docs := documentRevisions(t, s, id)
		last := docs[len(docs)-1]
		if len(docs) != want.revision || last.Path != want.path || last.Content != want.content || last.Actor != librarianActor || last.Cause == "" {
			t.Fatalf("%s: %+v", id, docs)
		}
	}
	ops := operationsOf(t, s, id)
	if len(ops) != 1 || !ops[0].Acknowledged || ops[0].Result == nil || ops[0].Result.Outcome != "succeeded" {
		t.Fatalf("operation: %+v", ops)
	}
	docs := documentRevisions(t, s, "subsystem-trace")
	if docs[0].Cause != ops[0].Operation.ID {
		t.Fatalf("revision cause %q, want the operation %q", docs[0].Cause, ops[0].Operation.ID)
	}
	// The write is keyed by the operation: applying the operation again, as a
	// retry after a stop between recording and the result would, records
	// nothing and reports the recorded result.
	again := &extractor{s: s, repository: s.active.repository}
	sameResult := func(got *coreadapter.OperationResult) bool {
		var a, b struct{ Subsystems, Removed []string }
		return got != nil && got.Outcome == ops[0].Result.Outcome && got.Evidence == ops[0].Result.Evidence &&
			json.Unmarshal(got.Data, &a) == nil && json.Unmarshal(ops[0].Result.Data, &b) == nil && reflect.DeepEqual(a, b) && len(a.Subsystems) == 2
	}
	if observed, err := again.Inspect(ctx, ops[0].Operation); err != nil || observed.State != coreadapter.EffectCompleted || !sameResult(observed.Result) {
		t.Fatalf("inspect after completion: %+v %v", observed, err)
	}
	if result, err := again.Apply(ctx, ops[0].Operation); err != nil || !sameResult(&result) {
		t.Fatalf("apply after completion: %+v %v", result, err)
	}
	if docs := documentRevisions(t, s, trace.EntitiesDocument); len(docs) != 2 {
		t.Fatalf("retry duplicated revisions: %d", len(docs))
	}
	commit := demoGit(t, filepath.Dir(f.clone), "-C", traceDir, "log", "-1", "--format=%H", "--", "kb/trace.md")
	if other := demoGit(t, filepath.Dir(f.clone), "-C", traceDir, "log", "-1", "--format=%H", "--", "kb/entities.json"); other != commit {
		t.Fatalf("revisions landed in separate commits: %s %s", commit, other)
	}
	if got := proseOnDisk(t, traceDir); !reflect.DeepEqual(got, map[string]string{"trace.md": "# trace\n\nRun go test ./internal/trace.\n", "service.md": "# service\n\nOne process.\n", "entities.json": refined}) {
		t.Fatalf("kb on disk: %v", got)
	}
	if notes, err := os.ReadFile(filepath.Join(traceDir, "notes", "librarian.md")); err != nil || string(notes) != "librarian: trace tests are slow" {
		t.Fatalf("librarian notes: %q %v", notes, err)
	}
	// The librarian's workstream carries no feature: workstream status does
	// not list it.
	if all, err := c.Statuses(ctx); err != nil || len(all.Workstreams) != 0 || len(all.Diagnostics) != 0 {
		t.Fatalf("workstream status lists the librarian's workstream: %+v %v", all, err)
	}
	_, err = c.Status(ctx, librarianWorkstream(id))
	assertCode(t, err, NotFound)
	if !reflect.DeepEqual(cloneBefore, snapshot(t, f.clone)) {
		t.Fatal("the extraction changed the clone")
	}
	root := f.opts.Config.Root
	if entries, err := os.ReadDir(filepath.Join(root, "views")); err != nil || len(entries) != 0 {
		t.Fatalf("views leaked: %v %v", entries, err)
	}
	if _, err := os.Lstat(filepath.Join(root, "librarian", string(id), "extract-1-1", "workspace")); !os.IsNotExist(err) {
		t.Fatal("staged workspace retained")
	}
	for _, dir := range []string{"projects", "librarian"} {
		for path, content := range snapshot(t, filepath.Join(root, dir)) {
			if strings.Contains(content, demoSecret) {
				t.Fatalf("untracked secret copied into %s", path)
			}
		}
	}

	// A re-run sees the recorded knowledge base, replaces it, and removes the
	// subsystem it no longer produces.
	f.script("extract-2-1", map[string]string{"output/kb/trace.md": "# trace\n\nRevised.\n", "output/kb/entities.json": refined},
		func(ctx context.Context, req agent.Request, verified *agent.Turn, tools *mcp.ClientSession) error {
			err := checkLibrarianBoundary(ctx, req, verified, tools, f.clone, refined, tracked)
			got, e := callTool(ctx, tools, "file_read", map[string]any{"path": "kb/service.md"})
			if e != nil || got != `"# service\n\nOne process.\n"` {
				err = errors.Join(err, fmt.Errorf("previous prose: %q %v", got, e))
			}
			if !strings.Contains(req.Prompt, "Knowledge base written") {
				err = errors.Join(err, errors.New("second turn did not replay the first"))
			}
			return err
		})
	started, err := c.ExtractProject(ctx, id)
	must(t, err)
	if started.Extraction.Extraction != 2 || started.Extraction.State != "pending" || started.Project.ID != id {
		t.Fatalf("re-run: %+v", started)
	}
	if x := awaitExtraction(t, c); x.Extraction != 2 || x.State != "succeeded" {
		t.Fatalf("second extraction: %+v", x)
	}
	if docs := documentRevisions(t, s, "subsystem-trace"); len(docs) != 2 || docs[1].Content != "# trace\n\nRevised.\n" {
		t.Fatalf("trace prose: %+v", docs)
	}
	if docs := documentRevisions(t, s, "subsystem-service"); len(docs) != 2 || docs[1].Content != "" || docs[1].Path != "kb/service.md" {
		t.Fatalf("removed prose: %+v", docs)
	}
	if docs := documentRevisions(t, s, trace.EntitiesDocument); len(docs) != 3 {
		t.Fatalf("entity revisions: %d", len(docs))
	}
	if got := proseOnDisk(t, traceDir); !reflect.DeepEqual(got, map[string]string{"trace.md": "# trace\n\nRevised.\n", "entities.json": refined}) {
		t.Fatalf("kb on disk after re-run: %v", got)
	}
	if out := demoGit(t, filepath.Dir(f.clone), "-C", traceDir, "ls-tree", "--name-only", "HEAD", "kb/"); strings.Contains(out, "service.md") {
		t.Fatalf("removed prose still tracked:\n%s", out)
	}
	ops = operationsOf(t, s, id)
	var result struct{ Subsystems, Removed []string }
	must(t, json.Unmarshal(ops[1].Result.Data, &result))
	if len(ops) != 2 || !slices.Equal(result.Subsystems, []string{"trace"}) || !slices.Equal(result.Removed, []string{"service"}) {
		t.Fatalf("second operation: %+v %+v", ops, result)
	}

	// A third run is refused while it is still running.
	entered, release := make(chan struct{}), make(chan struct{})
	f.engine.mu.Lock()
	f.engine.turns["extract-3-1"] = func(ctx context.Context, req agent.Request, _ *agent.Turn, tools *mcp.ClientSession) (*agent.Result, error) {
		close(entered)
		select {
		case <-release:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		if _, err := callTool(ctx, tools, "file_write", map[string]any{"path": "output/kb/trace.md", "content": "# trace\n\nThird.\n"}); err != nil {
			return nil, err
		}
		if _, err := callTool(ctx, tools, "file_write", map[string]any{"path": "output/kb/entities.json", "content": refined}); err != nil {
			return nil, err
		}
		return &agent.Result{ClaudeID: "session-three", ResultText: "done", SessionDir: req.SessionDir}, nil
	}
	f.engine.mu.Unlock()
	_, err = c.ExtractProject(ctx, id)
	must(t, err)
	select {
	case <-entered:
	case <-time.After(demoTimeout):
		t.Fatal("third extraction did not start")
	}
	cfg, err := c.Configuration(ctx)
	must(t, err)
	if x := cfg.Project.Extraction; x == nil || x.Extraction != 3 || x.State != "running" {
		t.Fatalf("running state: %+v", x)
	}
	_, err = c.ExtractProject(ctx, id)
	assertCode(t, err, Conflict)
	if !strings.Contains(err.Error(), "extraction 3") || !strings.Contains(err.Error(), "running") {
		t.Fatal(err)
	}
	if code := extractStatus(t, s, `{"project":"`+string(id)+`"}`); code != http.StatusConflict {
		t.Fatalf("running extraction status %d", code)
	}
	close(release)
	if x := awaitExtraction(t, c); x.Extraction != 3 || x.State != "succeeded" {
		t.Fatalf("third extraction: %+v", x)
	}
	_, err = c.ExtractProject(ctx, "p_ffffffffffffffffffffffffffffffff")
	assertCode(t, err, NotFound)
	assertCode(t, c.Do(ctx, "POST", Prefix+"/projects/extract", ProjectExtractRequest{Project: "bad"}, nil), Validation)
	if code := extractStatus(t, s, `{"project":"bad"}`); code != http.StatusUnprocessableEntity {
		t.Fatalf("malformed project ID status %d", code)
	}
	if code := extractStatus(t, s, `{"project":"p_ffffffffffffffffffffffffffffffff"}`); code != http.StatusNotFound {
		t.Fatalf("unknown project status %d", code)
	}
	must(t, s.Close())
}

func TestProjectAddSeedsTrackedFiles(t *testing.T) {
	t.Parallel()
	f := newLibrarianFixture(t)
	ctx := context.Background()
	tracked := f.trackedSeed(t)
	s, c := start(t, f.opts)
	added, err := c.AddProject(ctx, request(f.clone))
	must(t, err)
	must(t, s.Close())
	c.Close()
	repo, _ := f.reopen(t, added)
	defer repo.Close()
	docs, err := trace.Read[trace.Document](repo, "")
	must(t, err)
	var seeded []trace.Document
	for _, d := range docs {
		if d.ID == "kb-entities" && d.Revision == 1 {
			seeded = append(seeded, d)
		}
	}
	if len(seeded) != 1 || seeded[0].Content != tracked {
		t.Fatalf("revision 1 kb-entities = %+v, want %s", seeded, tracked)
	}
	for _, id := range []string{`"scratch"`, `"node_modules"`} {
		if strings.Contains(seeded[0].Content, id) {
			t.Fatalf("seed contains %s: %s", id, seeded[0].Content)
		}
	}
	for _, id := range []string{`"internal"`, `"trace"`} {
		if !strings.Contains(seeded[0].Content, id) {
			t.Fatalf("seed lacks %s: %s", id, seeded[0].Content)
		}
	}
}

func TestExtractionRefusesInvalidOutput(t *testing.T) {
	t.Parallel()
	f := newLibrarianFixture(t)
	ctx := context.Background()
	seed, err := kb.Seed(f.clone)
	must(t, err)
	good := entitiesWith(t, seed, "history")
	cases := []struct {
		name  string
		files map[string]string
		want  []string
	}{
		{"duplicate entity id", map[string]string{"output/kb/trace.md": "# trace\n", "output/kb/entities.json": `{"version":1,"entities":[{"id":"a","name":"a","aliases":[],"paths":["a"],"owners":[],"part_of":[]},{"id":"a","name":"b","aliases":[],"paths":["b"],"owners":[],"part_of":[]}]}`}, []string{"kb/entities.json", "duplicate id"}},
		{"bad subsystem name", map[string]string{"output/kb/Trace_Repo.md": "# trace\n", "output/kb/entities.json": good}, []string{"kb/Trace_Repo.md", "^[a-z0-9][a-z0-9-]*$"}},
		{"no output", map[string]string{}, []string{"kb/ is missing"}},
	}
	f.script("extract-1-1", cases[0].files, nil)
	s, c := start(t, f.opts)
	added, err := c.AddProject(ctx, request(f.clone))
	must(t, err)
	id := added.Project.ID
	seededDocs := documentRevisions(t, s, trace.EntitiesDocument)
	kbBefore := proseOnDisk(t, added.Project.Trace)
	for i, tc := range cases {
		n := i + 1
		if n > 1 {
			f.script(fmt.Sprintf("extract-%d-1", n), tc.files, nil)
			_, err := c.ExtractProject(ctx, id)
			must(t, err)
		}
		x := awaitExtraction(t, c)
		if x.Extraction != n || x.State != "failed" || !strings.HasPrefix(x.Reason, "invalid librarian output: ") {
			t.Fatalf("%s: %+v", tc.name, x)
		}
		for _, want := range tc.want {
			if !strings.Contains(x.Reason, want) {
				t.Fatalf("%s: reason %q lacks %q", tc.name, x.Reason, want)
			}
		}
		// The previous knowledge base stays in place.
		if docs := documentRevisions(t, s, trace.EntitiesDocument); !reflect.DeepEqual(docs, seededDocs) {
			t.Fatalf("%s: entity map changed: %+v", tc.name, docs)
		}
		if got := proseOnDisk(t, added.Project.Trace); !reflect.DeepEqual(got, kbBefore) {
			t.Fatalf("%s: kb changed: %v", tc.name, got)
		}
	}
	// A turn that fails in the backend is a failed extraction with its reason.
	f.engine.mu.Lock()
	f.engine.turns["extract-4-1"] = func(context.Context, agent.Request, *agent.Turn, *mcp.ClientSession) (*agent.Result, error) {
		return nil, errors.New("model refused")
	}
	f.engine.mu.Unlock()
	_, err = c.ExtractProject(ctx, id)
	must(t, err)
	if x := awaitExtraction(t, c); x.Extraction != 4 || x.State != "failed" || !strings.Contains(x.Reason, "extract-4-1 failed") || !strings.Contains(x.Reason, "model refused") {
		t.Fatalf("backend failure: %+v", x)
	}
	if ops := operationsOf(t, s, id); len(ops) != 4 {
		t.Fatalf("operations: %d", len(ops))
	}
	must(t, s.Close())
}

func TestExtractionSurvivesRestart(t *testing.T) {
	t.Parallel()
	f := newLibrarianFixture(t)
	ctx := context.Background()
	seed, err := kb.Seed(f.clone)
	must(t, err)
	refined := entitiesWith(t, seed, "history")
	// The first attempt is stopped mid-turn: the backend returns on cancellation.
	entered := make(chan struct{})
	var once sync.Once
	f.engine.mu.Lock()
	f.engine.turns["extract-1-1"] = func(ctx context.Context, req agent.Request, _ *agent.Turn, tools *mcp.ClientSession) (*agent.Result, error) {
		once.Do(func() { close(entered) })
		<-ctx.Done()
		return nil, ctx.Err()
	}
	f.engine.mu.Unlock()
	s, err := Start(ctx, f.opts)
	must(t, err)
	c := NewClient(s.Socket())
	added, err := c.AddProject(ctx, request(f.clone))
	must(t, err)
	id := added.Project.ID
	select {
	case <-entered:
	case <-time.After(demoTimeout):
		t.Fatal("extraction did not start")
	}
	cfg, err := c.Configuration(ctx)
	must(t, err)
	if x := cfg.Project.Extraction; x == nil || x.State != "running" {
		t.Fatalf("mid-turn state: %+v", x)
	}
	must(t, s.Close())
	c.Close()

	// The stopped service left the turn interrupted and the operation claimed.
	resolved, err := config.ResolveRoot(f.opts.Config.Root, "")
	must(t, err)
	project := config.Project{ID: id, Clone: added.Project.Clone}
	repo, err := trace.Open(resolved, project)
	must(t, err)
	stream := librarianWorkstream(id)
	th, err := repo.Thread(stream, librarianAgent)
	must(t, err)
	if len(th.Turns) != 1 || th.Turns[0].Status() != "interrupted" || th.Active != "" {
		t.Fatalf("interrupted turn: %+v", th)
	}
	// A second kind of interruption: a claim with no captured result, as a
	// killed process leaves it. The next session may not rerun that claim.
	second := th.Turns[0].Request
	second.ID, second.TurnID = "request_extract-1-2", "extract-1-2"
	_, err = repo.EnqueueTurn(ctx, second)
	must(t, err)
	_, err = repo.ClaimTurn(ctx, stream, librarianAgent, "dead", filepath.Join(f.opts.Config.Root, "dead"), f.clock.Now())
	must(t, err)
	must(t, repo.Close())

	f.script("extract-1-3", map[string]string{"output/kb/trace.md": "# trace\n", "output/kb/entities.json": refined}, nil)
	s, err = Start(ctx, f.opts)
	must(t, err)
	c = NewClient(s.Socket())
	defer c.Close()
	if x := awaitExtraction(t, c); x.Extraction != 1 || x.State != "succeeded" {
		t.Fatalf("after restart: %+v", x)
	}
	if runs := f.runs(); !slices.Equal(runs, []string{"extract-1-1", "extract-1-3"}) {
		t.Fatalf("backend runs %v", runs)
	}
	th, err = s.active.repository.Thread(stream, librarianAgent)
	must(t, err)
	if len(th.Turns) != 3 || th.Turns[1].Status() != "interrupted" || th.Turns[1].Response.Failure == "" || th.Turns[2].Status() != "idle" || th.Active != "" {
		t.Fatalf("thread after recovery: %+v", th)
	}
	// One result, one set of revisions: the retries produced no duplicates.
	ops := operationsOf(t, s, id)
	kinds := map[string]int{}
	for _, a := range ops[0].History {
		kinds[a.Kind]++
	}
	if len(ops) != 1 || !ops[0].Acknowledged || kinds["result"] != 1 || kinds["claim"] < 2 {
		t.Fatalf("operation history: %+v", kinds)
	}
	if docs := documentRevisions(t, s, "subsystem-trace"); len(docs) != 1 {
		t.Fatalf("prose revisions: %+v", docs)
	}
	if docs := documentRevisions(t, s, trace.EntitiesDocument); len(docs) != 2 {
		t.Fatalf("entity revisions: %+v", docs)
	}
	must(t, s.Close())

	// An extraction interrupted at every attempt fails with the count.
	f2 := newLibrarianFixture(t)
	f2.engine.mu.Lock()
	for attempt := 1; attempt <= maxExtractionAttempts; attempt++ {
		f2.engine.turns[fmt.Sprintf("extract-1-%d", attempt)] = func(ctx context.Context, req agent.Request, _ *agent.Turn, tools *mcp.ClientSession) (*agent.Result, error) {
			return nil, errors.Join(context.Canceled, errors.New("connection dropped"))
		}
	}
	f2.engine.mu.Unlock()
	s2, c2 := start(t, f2.opts)
	_, err = c2.AddProject(ctx, request(f2.clone))
	must(t, err)
	if x := awaitExtraction(t, c2); x.State != "failed" || !strings.Contains(x.Reason, fmt.Sprintf("interrupted %d times", maxExtractionAttempts)) {
		t.Fatalf("exhausted attempts: %+v", x)
	}
	if runs := f2.runs(); len(runs) != maxExtractionAttempts {
		t.Fatalf("attempts %v", runs)
	}
	must(t, s2.Close())
}

func TestExtractionStateWhileRetrying(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	home, err := os.MkdirTemp("", "xs-")
	must(t, err)
	t.Cleanup(func() { os.RemoveAll(home) })
	opts := fixtureAt(t, home)
	cfg, err := config.Load(opts.Config)
	must(t, err)
	clock := &demoClock{now: demoStart}
	repo, err := trace.Create(ctx, cfg.Root, cfg.Project, clock.Now(), registrationActor)
	must(t, err)
	defer repo.Close()
	if x, err := extractionState(repo); err != nil || x != nil {
		t.Fatalf("without a librarian workstream: %+v %v", x, err)
	}
	stream, err := ensureLibrarianThread(ctx, repo, clock.Now(), registrationActor)
	must(t, err)
	must(t, requestExtraction(ctx, repo, 1, clock.Now(), registrationActor, "project-add", "requested"))
	_, event := extractionIDs(1)
	expect := func(step, state, reason string) {
		t.Helper()
		x, err := extractionState(repo)
		must(t, err)
		if x == nil || x.Extraction != 1 || x.State != state || x.Reason != reason {
			t.Fatalf("%s: %+v, want %s %q", step, x, state, reason)
		}
	}
	expect("requested", "pending", "")
	// The effect starts and fails: the controller records the effect, then a
	// retry that releases the claim.
	now := clock.Now()
	must(t, repo.WithOperation(ctx, stream, event, serviceActor, func() time.Time { return now }, func(a *trace.OperationAttempt, _ trace.OperationRecord) error {
		expect("claimed", "pending", "")
		observe := a.Action("observe", now)
		observe.Observation = &coreadapter.Observation{State: coreadapter.EffectAbsent, Evidence: "no librarian turn has run"}
		must(t, a.Record(ctx, observe))
		must(t, a.Record(ctx, a.Action("effect", now)))
		expect("effect started", "running", "")
		retry := a.Action("retry", now)
		retry.Failure, retry.RetryAt = "Effect returned error: trace unavailable", now.Add(time.Minute)
		must(t, a.Record(ctx, retry))
		return nil
	}))
	expect("waiting to retry", "pending", "Effect returned error: trace unavailable")
	// The next attempt claims the operation again.
	later := now.Add(2 * time.Minute)
	must(t, repo.WithOperation(ctx, stream, event, serviceActor, func() time.Time { return later }, func(a *trace.OperationAttempt, _ trace.OperationRecord) error {
		expect("claimed again", "pending", "")
		observe := a.Action("observe", later)
		observe.Observation = &coreadapter.Observation{State: coreadapter.EffectAbsent, Evidence: "no librarian turn has run"}
		must(t, a.Record(ctx, observe))
		must(t, a.Record(ctx, a.Action("effect", later)))
		expect("running again", "running", "")
		result := a.Action("result", later)
		result.Result = &coreadapter.OperationResult{Outcome: "failed", Evidence: "invalid librarian output: kb/ is missing"}
		must(t, a.Record(ctx, result))
		return nil
	}))
	expect("failed", "failed", "invalid librarian output: kb/ is missing")
	// Acknowledging the result later does not move the extraction's time.
	acked := later.Add(90 * time.Second)
	must(t, repo.WithOperation(ctx, stream, event, serviceActor, func() time.Time { return acked }, func(a *trace.OperationAttempt, _ trace.OperationRecord) error {
		return a.Record(ctx, a.Action("acknowledge", acked))
	}))
	expect("acknowledged", "failed", "invalid librarian output: kb/ is missing")
	if x, err := extractionState(repo); err != nil || !x.At.Equal(later) {
		t.Fatalf("acknowledged: at %v %v, want the result time %v", x.At, err, later)
	}
}

func TestSchedulerLeavesLibrarianTurnsToTheExtractor(t *testing.T) {
	t.Parallel()
	f := newLibrarianFixture(t)
	ctx := context.Background()
	seed, err := kb.Seed(f.clone)
	must(t, err)
	refined := entitiesWith(t, seed, "history")
	// The bound thread reconciler would complete any turn operation the
	// scheduler published for the librarian without running the librarian's
	// isolated turn path.
	f.opts.Threads = func(r *trace.Repository, _ *config.Config) (coreadapter.Reconciler, error) {
		return &completedRunner{}, nil
	}
	f.script("extract-1-1", map[string]string{"output/kb/trace.md": "# trace\n", "output/kb/entities.json": refined}, nil)
	s, c := start(t, f.opts)
	added, err := c.AddProject(ctx, request(f.clone))
	must(t, err)
	id := added.Project.ID
	if x := awaitExtraction(t, c); x.Extraction != 1 || x.State != "succeeded" {
		t.Fatalf("first extraction: %+v", x)
	}
	must(t, s.active.repository.CreateWorkstream(ctx, stream, f.clock.Now(), ownerActor))
	gate := s.admit(s.current(), s.active.repository)
	if admitted, err := gate(ctx, scheduler.Candidate{Workstream: librarianWorkstream(id)}); err != nil || admitted {
		t.Fatalf("librarian workstream admitted: %t %v", admitted, err)
	}
	if admitted, err := gate(ctx, scheduler.Candidate{Workstream: stream}); err != nil || !admitted {
		t.Fatalf("other workstream declined: %t %v", admitted, err)
	}
	must(t, s.Close())
	c.Close()

	// A librarian turn queued and unclaimed when the service starts, as an
	// attempt stopped between accepting and reserving the turn leaves it, is
	// the extractor's to run: the scheduler publishes no operation for it.
	repo, cfg := f.reopen(t, added)
	must(t, requestExtraction(ctx, repo, 2, f.clock.Now(), registrationActor, "owner-request", "requested"))
	queueLibrarianTurn(t, repo, cfg, 2, 1, f.clock.Now())
	must(t, repo.Close())
	tracked := f.trackedSeed(t)
	f.script("extract-2-1", map[string]string{"output/kb/trace.md": "# trace\n\nRevised.\n", "output/kb/entities.json": refined},
		func(ctx context.Context, req agent.Request, verified *agent.Turn, tools *mcp.ClientSession) error {
			return checkLibrarianBoundary(ctx, req, verified, tools, f.clone, refined, tracked)
		})
	s, c = start(t, f.opts)
	defer c.Close()
	if x := awaitExtraction(t, c); x.Extraction != 2 || x.State != "succeeded" {
		t.Fatalf("second extraction: %+v", x)
	}
	if runs := f.runs(); !slices.Equal(runs, []string{"extract-1-1", "extract-2-1"}) {
		t.Fatalf("backend runs %v", runs)
	}
	ops, err := s.active.repository.Operations(librarianWorkstream(id))
	must(t, err)
	for _, o := range ops {
		if o.Operation.Action != ExtractAction {
			t.Fatalf("the scheduler published %s for the librarian's turn", o.Operation.Action)
		}
	}
	if docs := documentRevisions(t, s, "subsystem-trace"); len(docs) != 2 || docs[1].Content != "# trace\n\nRevised.\n" {
		t.Fatalf("trace prose: %+v", docs)
	}
	must(t, s.Close())
}

func TestExtractionRecordsPersistedOutputAfterRestart(t *testing.T) {
	t.Parallel()
	for _, completed := range []bool{true, false} {
		name := "captured"
		if completed {
			name = "completed"
		}
		t.Run(name, func(t *testing.T) {
			f := newLibrarianFixture(t)
			ctx := context.Background()
			seed, err := kb.Seed(f.clone)
			must(t, err)
			refined := entitiesWith(t, seed, "history")
			f.script("extract-1-1", map[string]string{"output/kb/trace.md": "# trace\n", "output/kb/entities.json": refined}, nil)
			s, c := start(t, f.opts)
			added, err := c.AddProject(ctx, request(f.clone))
			must(t, err)
			id := added.Project.ID
			if x := awaitExtraction(t, c); x.Extraction != 1 || x.State != "succeeded" {
				t.Fatalf("first extraction: %+v", x)
			}
			must(t, s.Close())
			c.Close()

			// The stopped service ran extraction 2's turn and captured its
			// output, but did not record the knowledge base before it stopped.
			repo, cfg := f.reopen(t, added)
			stream := librarianWorkstream(id)
			must(t, requestExtraction(ctx, repo, 2, f.clock.Now(), registrationActor, "owner-request", "requested"))
			req := queueLibrarianTurn(t, repo, cfg, 2, 1, f.clock.Now())
			directory := filepath.Join(cfg.Root.String(), "librarian", string(id), req.TurnID)
			claimed, err := repo.ClaimTurn(ctx, stream, librarianAgent, "earlier-session", filepath.Join(directory, "session"), f.clock.Now())
			must(t, err)
			h := req.Header
			h.Schema, h.ID, h.At, h.Actor = "osmia.trace.turn-response", trace.EventID(req.ID, "response"), f.clock.Now(), trace.Actor{Kind: "service", ID: "thread-runner"}
			response := trace.TurnResponse{Header: h, AgentID: librarianAgent, ThreadID: req.ThreadID, TurnID: req.TurnID, RequestID: req.ID, RequestRevision: req.Revision,
				Result: coreadapter.SessionResult{Session: coreadapter.BackendSession{Backend: req.Profile.Backend, ID: "session-" + req.TurnID}, SessionDirectory: claimed.Claim.SessionDirectory, StartedAt: claimed.Claim.At, Duration: time.Second, FinalResponse: "Knowledge base written"}}
			must(t, repo.CaptureTurn(ctx, "earlier-session", response))
			if completed {
				must(t, repo.CompleteTurn(ctx, stream, librarianAgent, req.TurnID, "earlier-session", f.clock.Now()))
			}
			must(t, repo.Close())
			output := filepath.Join(directory, kb.OutputDirectory, "kb")
			must(t, os.MkdirAll(output, 0700))
			must(t, os.WriteFile(filepath.Join(output, "trace.md"), []byte("# trace\n\nPersisted.\n"), 0600))
			must(t, os.WriteFile(filepath.Join(output, "entities.json"), []byte(refined), 0600))

			// The turn is not run again: the persisted output is recorded.
			s, c = start(t, f.opts)
			defer c.Close()
			if x := awaitExtraction(t, c); x.Extraction != 2 || x.State != "succeeded" {
				t.Fatalf("after restart: %+v", x)
			}
			if runs := f.runs(); !slices.Equal(runs, []string{"extract-1-1"}) {
				t.Fatalf("backend runs %v", runs)
			}
			th, err := s.active.repository.Thread(stream, librarianAgent)
			must(t, err)
			if len(th.Turns) != 2 || th.Turns[1].Status() != "idle" || th.Active != "" {
				t.Fatalf("thread after recovery: %+v", th)
			}
			if docs := documentRevisions(t, s, "subsystem-trace"); len(docs) != 2 || docs[1].Content != "# trace\n\nPersisted.\n" {
				t.Fatalf("trace prose: %+v", docs)
			}
			if docs := documentRevisions(t, s, trace.EntitiesDocument); len(docs) != 3 || docs[2].Content != refined {
				t.Fatalf("entity revisions: %+v", docs)
			}
			if ops := operationsOf(t, s, id); len(ops) != 2 || ops[1].Result == nil || ops[1].Result.Outcome != "succeeded" {
				t.Fatalf("operations: %+v", ops)
			}
			must(t, s.Close())
		})
	}
}
