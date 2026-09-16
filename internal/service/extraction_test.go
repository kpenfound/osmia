package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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

func newLibrarianFixture(t *testing.T) *librarianFixture {
	t.Helper()
	opts, clone := projectFixture(t)
	home := filepath.Dir(clone)
	must(t, os.MkdirAll(filepath.Join(clone, "internal", "trace"), 0700))
	must(t, os.WriteFile(filepath.Join(clone, "internal", "trace", "git.go"), []byte("package trace\n"), 0600))
	must(t, os.WriteFile(filepath.Join(clone, "CODEOWNERS"), []byte("/internal/ @core\n"), 0600))
	must(t, os.WriteFile(filepath.Join(clone, "AGENTS.md"), []byte("# Agents\n"), 0600))
	must(t, os.WriteFile(filepath.Join(clone, "secret.env"), []byte("TOKEN="+demoSecret+"\n"), 0600))
	demoGit(t, home, "-C", clone, "add", "internal", "CODEOWNERS", "AGENTS.md")
	demoGit(t, home, "-C", clone, "-c", "user.name=Owner", "-c", "user.email=owner@example.invalid", "commit", "-qm", "base")
	sessions := &demoSessions{byKey: map[string]*mcp.ClientSession{}}
	engine := &demoEngine{sessions: sessions, turns: map[string]demoTurn{}}
	engine.resume = func(coreadapter.Profile, coreadapter.Profile, coreadapter.BackendSession) error {
		return coreadapter.ErrResumeUnavailable
	}
	clock := &demoClock{now: demoStart}
	opts.Reconciliation.Now = clock.Now
	opts.Librarian = &Librarian{Engine: engine, Hosts: func(token string) coreadapter.MCPHosts {
		return &coreadapter.MCPHost{Transport: &demoTransport{token: token, sessions: sessions}}
	}}
	return &librarianFixture{opts: opts, clone: clone, engine: engine, sessions: sessions, clock: clock}
}

// script installs the fake librarian's behaviour for one turn: it writes the
// given output files and returns a successful result.
func (f *librarianFixture) script(turn string, files map[string]string, check func(ctx context.Context, req agent.Request, policy coreadapter.BoundaryPolicy, tools *mcp.ClientSession) error) {
	f.engine.mu.Lock()
	defer f.engine.mu.Unlock()
	f.engine.turns[turn] = func(ctx context.Context, req agent.Request, policy coreadapter.BoundaryPolicy, tools *mcp.ClientSession) (*agent.Result, error) {
		var err error
		if check != nil {
			err = check(ctx, req, policy, tools)
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
func checkLibrarianBoundary(ctx context.Context, req agent.Request, policy coreadapter.BoundaryPolicy, tools *mcp.ClientSession, clone, entities, seed string) error {
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
	view := policy.Isolation.Workspace.Directory
	if len(policy.Mounts) != 1 || policy.Mounts[0].Source != view || view == clone || strings.HasPrefix(view, clone+string(filepath.Separator)) {
		fail("mounts %+v expose more than the private view", policy.Mounts)
	}
	caps := policy.Isolation.Capabilities
	if caps.Execute || caps.Network || !caps.WriteFiles || !policy.NoVCS || !policy.NoDeliveryCredentials || !policy.NoHostEnvironment {
		fail("capabilities %+v policy %+v", caps, policy)
	}
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
	seeded, err := kb.Encode(seed)
	must(t, err)
	refined := entitiesWith(t, seed, "history")
	cloneBefore := snapshot(t, f.clone)
	f.script("extract-1-1", map[string]string{"output/kb/trace.md": "# trace\n\nRun go test ./internal/trace.\n", "output/kb/service.md": "# service\n\nOne process.\n", "output/kb/entities.json": refined},
		func(ctx context.Context, req agent.Request, policy coreadapter.BoundaryPolicy, tools *mcp.ClientSession) error {
			err := checkLibrarianBoundary(ctx, req, policy, tools, f.clone, string(seeded), string(seeded))
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
		func(ctx context.Context, req agent.Request, policy coreadapter.BoundaryPolicy, tools *mcp.ClientSession) error {
			err := checkLibrarianBoundary(ctx, req, policy, tools, f.clone, refined, string(seeded))
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
	f.engine.turns["extract-3-1"] = func(ctx context.Context, req agent.Request, _ coreadapter.BoundaryPolicy, tools *mcp.ClientSession) (*agent.Result, error) {
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
	close(release)
	if x := awaitExtraction(t, c); x.Extraction != 3 || x.State != "succeeded" {
		t.Fatalf("third extraction: %+v", x)
	}
	_, err = c.ExtractProject(ctx, "p_ffffffffffffffffffffffffffffffff")
	assertCode(t, err, NotFound)
	if err := c.Do(ctx, "POST", Prefix+"/projects/extract", ProjectExtractRequest{Project: "bad"}, nil); err == nil {
		t.Fatal("malformed project ID accepted")
	}
	must(t, s.Close())
}

func TestExtractionRefusesInvalidOutput(t *testing.T) {
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
		{"nested path", map[string]string{"output/kb/nested/trace.md": "# trace\n", "output/kb/entities.json": good}, []string{"kb/nested is not a regular file"}},
		{"missing entities", map[string]string{"output/kb/trace.md": "# trace\n"}, []string{"kb/entities.json is missing"}},
		{"file outside kb", map[string]string{"output/notes.md": "x\n", "output/kb/trace.md": "# trace\n", "output/kb/entities.json": good}, []string{`unexpected output entry "notes.md"`}},
		{"empty prose", map[string]string{"output/kb/trace.md": " \n", "output/kb/entities.json": good}, []string{"kb/trace.md is empty"}},
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
	f.engine.turns["extract-8-1"] = func(context.Context, agent.Request, coreadapter.BoundaryPolicy, *mcp.ClientSession) (*agent.Result, error) {
		return nil, errors.New("model refused")
	}
	f.engine.mu.Unlock()
	_, err = c.ExtractProject(ctx, id)
	must(t, err)
	if x := awaitExtraction(t, c); x.Extraction != 8 || x.State != "failed" || !strings.Contains(x.Reason, "extract-8-1 failed") || !strings.Contains(x.Reason, "model refused") {
		t.Fatalf("backend failure: %+v", x)
	}
	if ops := operationsOf(t, s, id); len(ops) != 8 {
		t.Fatalf("operations: %d", len(ops))
	}
	must(t, s.Close())
}

func TestExtractionSurvivesRestart(t *testing.T) {
	f := newLibrarianFixture(t)
	ctx := context.Background()
	seed, err := kb.Seed(f.clone)
	must(t, err)
	refined := entitiesWith(t, seed, "history")
	// The first attempt is stopped mid-turn: the backend returns on cancellation.
	entered := make(chan struct{})
	var once sync.Once
	f.engine.mu.Lock()
	f.engine.turns["extract-1-1"] = func(ctx context.Context, req agent.Request, _ coreadapter.BoundaryPolicy, tools *mcp.ClientSession) (*agent.Result, error) {
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
		f2.engine.turns[fmt.Sprintf("extract-1-%d", attempt)] = func(ctx context.Context, req agent.Request, _ coreadapter.BoundaryPolicy, tools *mcp.ClientSession) (*agent.Result, error) {
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
