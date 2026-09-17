package cli

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kpenfound/busybees/core/agent"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/kpenfound/osmia/internal/bundle"
	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/kb"
	"github.com/kpenfound/osmia/internal/service"
	"github.com/kpenfound/osmia/internal/trace"
)

// The M2 onboarding demonstration: a project is added from a local fixture
// clone, a scripted fake librarian writes its knowledge base across a service
// restart, the owner writes the charter, and the project's local context
// resolves without any network. See docs/m2-onboarding.md.

// onboardingEntities is the entity map the fake librarian writes. Entity IDs
// name the kb/<subsystem>.md file holding their prose.
const onboardingEntities = `{
  "version": 1,
  "entities": [
    {"id": "demo", "name": "Demo command", "aliases": ["cli"], "paths": ["cmd/demo"], "owners": ["@tools"], "part_of": []},
    {"id": "internal", "name": "Internal packages", "aliases": [], "paths": ["internal"], "owners": ["@core"], "part_of": []},
    {"id": "service", "name": "Service", "aliases": ["daemon"], "paths": ["internal/service"], "owners": ["@core"], "part_of": ["internal"]},
    {"id": "trace", "name": "Trace", "aliases": ["history"], "paths": ["internal/trace"], "owners": ["@core", "@storage"], "part_of": ["internal"]}
  ]
}
`

var onboardingProse = map[string]string{
	"kb/trace.md":   "# trace\n\nThe trace repository is append-only. Run the trace tests after touching it.\n",
	"kb/service.md": "# service\n\nOne process owns the socket.\n",
	"kb/demo.md":    "# demo\n\nThe command-line entry point.\n",
}

const onboardingCharter = `# Charter

1. Keep the trace append-only.
2. Every change ships with a test.
`

// fakeLibrarian is the isolation engine the librarian's turns run in. Its
// first turn waits until the service stops it; later turns write the scripted
// knowledge base through the turn's file tools.
type fakeLibrarian struct {
	mu      sync.Mutex
	clients map[string]*mcp.ClientSession
	runs    []string
	entered chan struct{}
}

func (f *fakeLibrarian) Prepare(ctx context.Context, policy coreadapter.BoundaryPolicy) (coreadapter.IsolatedSession, error) {
	return &fakeLibrarianSession{f: f, policy: policy}, ctx.Err()
}

type fakeLibrarianSession struct {
	f      *fakeLibrarian
	policy coreadapter.BoundaryPolicy
}

func (s *fakeLibrarianSession) Inspect(context.Context) (coreadapter.BoundaryPolicy, error) {
	return s.policy, nil
}
func (s *fakeLibrarianSession) Release(context.Context) error { return nil }
func (s *fakeLibrarianSession) Run(ctx context.Context, req agent.Request) (*agent.Result, error) {
	f := s.f
	f.mu.Lock()
	f.runs = append(f.runs, req.Name)
	first := len(f.runs) == 1
	tools := f.clients[req.Env["OSMIA_MCP_TOKEN"]]
	f.mu.Unlock()
	if first {
		close(f.entered)
	}
	if tools == nil {
		return nil, fmt.Errorf("turn %s has no tools", req.Name)
	}
	if first {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	files := map[string]string{"output/kb/entities.json": onboardingEntities}
	for path, content := range onboardingProse {
		files["output/"+path] = content
	}
	for path, content := range files {
		result, err := tools.CallTool(ctx, &mcp.CallToolParams{Name: "file_write", Arguments: map[string]any{"path": path, "content": content}})
		if err == nil && result.IsError {
			err = errors.New("tool error")
		}
		if err != nil {
			return nil, fmt.Errorf("write %s: %w", path, err)
		}
	}
	return &agent.Result{ClaudeID: "session-" + req.Name, ResultText: "Knowledge base written", SessionDir: req.SessionDir, NumTurns: 1}, nil
}

func (f *fakeLibrarian) ranTurns() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.runs)
}

// fakeMCP serves a turn's scoped MCP server in memory and hands the client
// side to the fake engine under the turn's token.
type fakeMCP struct {
	f     *fakeLibrarian
	token string
}

type releaseFunc func(context.Context) error

func (r releaseFunc) Release(ctx context.Context) error { return r(ctx) }

func (m *fakeMCP) Start(ctx context.Context, server *mcp.Server) (coreadapter.Endpoint, coreadapter.Lease, error) {
	serverSide, clientSide := mcp.NewInMemoryTransports()
	served, err := server.Connect(ctx, serverSide, nil)
	if err != nil {
		return coreadapter.Endpoint{}, nil, err
	}
	client, err := mcp.NewClient(&mcp.Implementation{Name: "fake-librarian", Version: "1"}, nil).Connect(ctx, clientSide, nil)
	if err != nil {
		served.Close()
		return coreadapter.Endpoint{}, nil, err
	}
	m.f.mu.Lock()
	m.f.clients[m.token] = client
	m.f.mu.Unlock()
	release := releaseFunc(func(context.Context) error {
		m.f.mu.Lock()
		delete(m.f.clients, m.token)
		m.f.mu.Unlock()
		return errors.Join(client.Close(), served.Wait())
	})
	return coreadapter.Endpoint{URL: "http://osmia-mcp.invalid/turn", BearerTokenEnvironment: "OSMIA_MCP_TOKEN"}, release, nil
}

// onboardingGit runs git with no user or system configuration.
func onboardingGit(t *testing.T, home string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + home, "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=" + os.DevNull}
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v: %s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// treeHash hashes every path, mode and file content below dir, VCS metadata
// included.
func treeHash(t *testing.T, dir string) string {
	t.Helper()
	h := sha256.New()
	must(t, filepath.WalkDir(dir, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(dir, path)
		fmt.Fprintf(h, "%s %v\n", rel, info.Mode())
		if entry.Type().IsRegular() {
			data, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			fmt.Fprintf(h, "%x\n", sha256.Sum256(data))
		}
		return nil
	}))
	return hex.EncodeToString(h.Sum(nil))
}

// traceDocuments reads the project-level document revisions from the trace
// directory.
func traceDocuments(t *testing.T, traceDir string) map[string][]trace.Document {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(traceDir, "documents.jsonl"))
	must(t, err)
	out := map[string][]trace.Document{}
	lines := bufio.NewScanner(bytes.NewReader(data))
	lines.Buffer(nil, 1<<20)
	for lines.Scan() {
		var d trace.Document
		must(t, json.Unmarshal(lines.Bytes(), &d))
		out[d.ID] = append(out[d.ID], d)
	}
	must(t, lines.Err())
	return out
}

func onboardingStatus(t *testing.T, root string) service.ConfigResponse {
	t.Helper()
	var st struct {
		Configuration service.ConfigResponse
	}
	must(t, json.Unmarshal([]byte(successful(t, root, "status", "--json")), &st))
	return st.Configuration
}

func TestM2ProjectOnboarding(t *testing.T) {
	for _, name := range []string{"GITHUB_TOKEN", "GH_TOKEN"} {
		t.Setenv(name, "ghp_onboarding_credential_must_not_be_used")
	}
	ctx := context.Background()

	// 1. A fresh root with no project, and a fixture clone with CODEOWNERS
	// and a few subsystems.
	opts, clone := emptyFixture(t)
	home := filepath.Dir(clone)
	for path, content := range map[string]string{
		"CODEOWNERS":                "* @core\n/cmd/ @tools\n/internal/trace/ @core @storage\n",
		"README.md":                 "# demo\n",
		"cmd/demo/main.go":          "package main\n",
		"internal/service/serve.go": "package service\n",
		"internal/trace/git.go":     "package trace\n",
	} {
		must(t, os.MkdirAll(filepath.Join(clone, filepath.Dir(path)), 0700))
		must(t, os.WriteFile(filepath.Join(clone, path), []byte(content), 0600))
	}
	onboardingGit(t, home, "-C", clone, "add", ".")
	onboardingGit(t, home, "-C", clone, "-c", "user.name=Owner", "-c", "user.email=owner@example.invalid", "commit", "-qm", "base")
	cloneBefore := treeHash(t, clone)
	librarian := &fakeLibrarian{clients: map[string]*mcp.ClientSession{}, entered: make(chan struct{})}
	opts.Librarian = &service.Librarian{Engine: librarian, Hosts: func(token string) coreadapter.MCPHosts {
		return &coreadapter.MCPHost{Transport: &fakeMCP{f: librarian, token: token}}
	}}
	opts.ShutdownTimeout = 100 * time.Millisecond
	root := opts.Config.Root
	s, err := service.Start(ctx, opts)
	must(t, err)
	stopped := false
	t.Cleanup(func() {
		if !stopped {
			s.Close()
		}
	})
	if cfg := onboardingStatus(t, root); cfg.Project != nil {
		t.Fatalf("fresh root has a project: %+v", cfg.Project)
	}

	// 2. project add registers the project; the trace lives under the root.
	var added service.ProjectResponse
	must(t, json.Unmarshal([]byte(successful(t, root, "project", "add", "demo", "--upstream", "owner/demo", "--fork", "fork/demo", "--clone", clone, "--json")), &added))
	id := added.Project.ID
	traceDir := added.Project.Trace
	resolvedRoot, err := config.ResolveRoot(root, "")
	must(t, err)
	if traceDir != filepath.Join(resolvedRoot.String(), "projects", string(id)) || strings.HasPrefix(traceDir, added.Project.Clone) || added.Project.Charter != filepath.Join(traceDir, "charter.md") {
		t.Fatalf("trace %s, charter %s, clone %s", traceDir, added.Project.Charter, added.Project.Clone)
	}

	// 6. The service stops while the librarian's first turn runs, and the
	// extraction finishes after a restart.
	select {
	case <-librarian.entered:
	case <-time.After(5 * time.Minute):
		t.Fatal("extraction did not start")
	}
	if x := onboardingStatus(t, root).Project.Extraction; x == nil || x.Extraction != 1 || x.State != "running" {
		t.Fatalf("extraction before the restart: %+v", x)
	}
	must(t, s.Close())
	stopped = true
	s, err = service.Start(ctx, opts)
	must(t, err)
	stopped = false
	x := awaitExtraction(t, root)
	if x.Extraction != 1 || x.State != "succeeded" {
		t.Fatalf("extraction after the restart: %+v", x)
	}
	if runs := librarian.ranTurns(); len(runs) != 2 || runs[0] == runs[1] {
		t.Fatalf("librarian turns %v", runs)
	}
	for path, want := range onboardingProse {
		if got, err := os.ReadFile(filepath.Join(traceDir, path)); err != nil || string(got) != want {
			t.Fatalf("%s: %q %v", path, got, err)
		}
	}
	want, err := kb.Parse([]byte(onboardingEntities))
	must(t, err)
	if got, err := kb.LoadFile(filepath.Join(traceDir, "kb", "entities.json")); err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("entities: %+v %v", got, err)
	}
	// One revision per produced file, plus the seed: nothing was recorded
	// twice across the restart.
	docs := traceDocuments(t, traceDir)
	counts := map[string]int{}
	for doc, revisions := range docs {
		counts[doc] = len(revisions)
	}
	if want := map[string]int{"charter": 1, "kb-entities": 2, "subsystem-trace": 1, "subsystem-service": 1, "subsystem-demo": 1}; !reflect.DeepEqual(counts, want) {
		t.Fatalf("document revisions %v, want %v", counts, want)
	}
	// Registration records the seed as the owner; the librarian's revisions
	// follow it.
	if seed := docs["kb-entities"][0]; seed.Actor != (trace.Actor{Kind: "owner", ID: "local"}) {
		t.Fatalf("seed revision: %+v", seed.Header)
	}
	for doc, revisions := range docs {
		last := revisions[len(revisions)-1]
		if doc != "charter" && (last.Actor != (trace.Actor{Kind: "agent", ID: "agent_librarian"}) || last.Cause == "") {
			t.Fatalf("%s is not a librarian revision: %+v", doc, last.Header)
		}
	}
	if treeHash(t, clone) != cloneBefore {
		t.Fatal("onboarding changed the clone")
	}

	// 3. Hand-in is refused while the charter is empty.
	design := filepath.Join(home, "design.md")
	must(t, os.WriteFile(design, []byte("# Upload API\n"), 0600))
	code, out, diag := invoke(t, root, "handin", string(id), design)
	if code != 4 || out != "" || !strings.Contains(diag, "charter_empty") || !strings.Contains(diag, added.Project.Charter) {
		t.Fatalf("hand-in with an empty charter: %d %q %q", code, out, diag)
	}
	var all struct {
		Status service.StatusResponse
	}
	must(t, json.Unmarshal([]byte(successful(t, root, "status", "--json")), &all))
	if len(all.Status.Workstreams) != 0 {
		t.Fatalf("a refused hand-in created workstreams: %+v", all.Status.Workstreams)
	}

	// 4. The owner writes rules; status records the edit and shows the
	// charter ready and the context file-based.
	must(t, os.WriteFile(added.Project.Charter, []byte(onboardingCharter), 0600))
	status := successful(t, root, "status")
	for _, want := range []string{
		"Charter: ready (2 rules, revision 2) " + added.Project.Charter,
		"Context: " + string(id) + " context_mode=file",
		"Knowledge base: extraction 1 succeeded",
	} {
		if !strings.Contains(status, want) {
			t.Fatalf("status lacks %q:\n%s", want, status)
		}
	}
	charters := traceDocuments(t, traceDir)["charter"]
	if edit := charters[len(charters)-1]; len(charters) != 2 || edit.Revision != 2 || edit.Actor != (trace.Actor{Kind: "owner", ID: "local"}) || edit.Cause != "owner-edit" || edit.Content != onboardingCharter {
		t.Fatalf("charter revisions: %+v", charters)
	}
	if log := onboardingGit(t, home, "-C", traceDir, "log", "--format=%s", "--", "documents.jsonl"); strings.Count(log, "\n")+1 < 3 {
		t.Fatalf("trace history:\n%s", log)
	}

	// 5. Resolution reads the local entity map alone.
	local, err := kb.LoadFile(filepath.Join(traceDir, "kb", "entities.json"))
	must(t, err)
	fp := local.ResolveEntities([]string{"history", "internal"})
	if !slices.Equal(fp.Entities, []string{"internal", "service", "trace"}) || !slices.Equal(fp.Paths, []string{"internal", "internal/service", "internal/trace"}) || len(fp.Unresolved) != 0 {
		t.Fatalf("entities to paths: %+v", fp)
	}
	loc := local.ResolvePaths([]string{"internal/trace/git.go", "cmd/demo/main.go", "docs/unknown.md"})
	if want := []kb.PathMatch{{Path: "internal/trace/git.go", Entities: []string{"trace"}}, {Path: "cmd/demo/main.go", Entities: []string{"demo"}}}; !reflect.DeepEqual(loc.Matches, want) || !slices.Equal(loc.Unresolved, []string{"docs/unknown.md"}) {
		t.Fatalf("paths to entities: %+v", loc)
	}
	// The service's file provider assembles a footprint's bundle.
	provider := s.Context()
	if provider.Mode(id) != bundle.ModeFile {
		t.Fatalf("mode %s", provider.Mode(id))
	}
	b, err := provider.Assemble(ctx, id, bundle.Scope{Entities: []string{"history"}})
	must(t, err)
	if b.Charter.Revision != 2 || len(b.Charter.Rules) != 2 || len(b.Decisions) != 0 || b.Entities.Revision != 2 {
		t.Fatalf("bundle: %+v", b)
	}
	if len(b.Knowledge) != 1 || b.Knowledge[0].Subsystem != "trace" || b.Knowledge[0].Content != onboardingProse["kb/trace.md"] || len(b.Missing) != 0 {
		t.Fatalf("bundle prose: %+v %+v", b.Knowledge, b.Missing)
	}
	if len(b.Entities.Entities) != 1 || b.Entities.Entities[0].ID != "trace" {
		t.Fatalf("bundle entities: %+v", b.Entities)
	}
	rendered := b.Render()
	for _, want := range []string{"context mode: file", "- charter#1 [Charter]: Keep the trace append-only.", "- charter#2 [Charter]: Every change ships with a test.", "No rulings are recorded.", "### kb/trace.md (subsystem trace)", "The trace repository is append-only."} {
		if !strings.Contains(rendered, want) {
			t.Fatalf("rendered bundle lacks %q:\n%s", want, rendered)
		}
	}
	if strings.Contains(rendered, "One process owns the socket.") {
		t.Fatalf("bundle carries prose outside the footprint:\n%s", rendered)
	}

	// 7. Removing the project keeps the trace and the clone.
	traceBefore := onboardingGit(t, home, "-C", traceDir, "rev-parse", "HEAD")
	removed := successful(t, root, "project", "remove", string(id))
	if !strings.Contains(removed, string(id)) {
		t.Fatalf("remove output:\n%s", removed)
	}
	if cfg := onboardingStatus(t, root); cfg.Project != nil {
		t.Fatalf("project still active: %+v", cfg.Project)
	}
	if head := onboardingGit(t, home, "-C", traceDir, "rev-parse", "HEAD"); head != traceBefore {
		t.Fatalf("trace moved on removal: %s != %s", head, traceBefore)
	}
	if got, err := os.ReadFile(added.Project.Charter); err != nil || string(got) != onboardingCharter {
		t.Fatalf("charter after removal: %q %v", got, err)
	}
	if treeHash(t, clone) != cloneBefore {
		t.Fatal("removal changed the clone")
	}
	must(t, s.Close())
	stopped = true
}
