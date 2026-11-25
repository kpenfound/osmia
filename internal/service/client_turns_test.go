package service

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kpenfound/osmia/internal/api"
	"github.com/kpenfound/osmia/internal/clientturns"
	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/coreadapter/adaptertest"
	"github.com/kpenfound/osmia/internal/execution"
	"github.com/kpenfound/osmia/internal/filetransport"
	"github.com/kpenfound/osmia/internal/fleet"
	"github.com/kpenfound/osmia/internal/isolation"
	"github.com/kpenfound/osmia/internal/jev"
	"github.com/kpenfound/osmia/internal/projectstore"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	"github.com/kpenfound/osmia/internal/servicetest/intakefixture"
	"github.com/kpenfound/osmia/internal/thread"
	"github.com/kpenfound/osmia/internal/trace"
)

type ctUpstream struct {
	server *httptest.Server
	mu     sync.Mutex
	tools  map[string][]coreadapter.Tool
	hits   int
}

func newCTUpstream(t *testing.T) *ctUpstream {
	u := &ctUpstream{tools: map[string][]coreadapter.Tool{}}
	u.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/mcp/"), "/")
		if len(parts) != 3 || r.Header.Get("Authorization") != "Bearer token-"+parts[0] {
			http.Error(w, "refused", http.StatusUnauthorized)
			return
		}
		u.mu.Lock()
		tools := u.tools[parts[0]]
		u.hits++
		u.mu.Unlock()
		body, _ := io.ReadAll(r.Body)
		for _, tool := range tools {
			if tool.Name == parts[2] {
				out, err := tool.Handle(r.Context(), body)
				if err != nil {
					http.Error(w, err.Error(), http.StatusUnprocessableEntity)
					return
				}
				_, _ = w.Write(out)
				return
			}
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(u.server.Close)
	return u
}

func (u *ctUpstream) Host(_ context.Context, req coreadapter.HostRequest) (coreadapter.HostedMCP, error) {
	u.mu.Lock()
	u.tools[req.Scope.Turn] = req.Tools
	u.mu.Unlock()
	return coreadapter.HostedMCP{Endpoint: coreadapter.Endpoint{URL: u.server.URL + "/mcp/" + req.Scope.Turn, BearerTokenEnvironment: coreadapter.TokenEnvironment, Token: "token-" + req.Scope.Turn},
		Lease: ctLease(func(context.Context) error { return nil })}, nil
}

func (u *ctUpstream) call(turn, tool string, input json.RawMessage) (json.RawMessage, error) {
	u.mu.Lock()
	tools := u.tools[turn]
	u.mu.Unlock()
	for _, candidate := range tools {
		if candidate.Name == tool {
			return candidate.Handle(context.Background(), input)
		}
	}
	return nil, os.ErrNotExist
}

type ctLease func(context.Context) error

func (f ctLease) Release(ctx context.Context) error { return f(ctx) }

type ctSource struct{ directory string }

func (s ctSource) Acquire(_ context.Context, req coreadapter.WorkspaceRequest) (coreadapter.WorkspaceLease, error) {
	return coreadapter.WorkspaceLease{Workspace: coreadapter.Workspace{Directory: s.directory, Access: req.Access}, Lease: ctLease(func(context.Context) error { return nil })}, nil
}

type ctRig struct {
	*transferRig
	fr     *fleetRig
	repo   *trace.Repository
	up     *ctUpstream
	engine *adaptertest.Engine
	turns  *isolation.Turns
	runner thread.Runner
	files  string
	home   string

	mu       sync.Mutex
	captured []map[string]string
}

func ctJudge() *jev.Judge {
	settings := config.Jev{URL: config.DefaultJevURL, Model: "jev-test", APIKeyEnv: "TEST_JEV_KEY", Timeout: "1s"}
	return &jev.Judge{Config: func() config.Jev { return settings }, Getenv: func(string) string { return "" }}
}

func ctTree(t *testing.T, dir string) map[string]string {
	t.Helper()
	files := map[string]string{}
	fixturekit.Must(t, filepath.WalkDir(dir, func(path string, e os.DirEntry, err error) error {
		if err != nil || e.IsDir() {
			return err
		}
		if rel, _ := filepath.Rel(dir, path); strings.HasPrefix(rel, ".git") {
			return nil
		}
		data, err := os.ReadFile(path)
		rel, _ := filepath.Rel(dir, path)
		files[filepath.ToSlash(rel)] = string(data)
		return err
	}))
	return files
}

func newCTRig(t *testing.T, role string) *ctRig {
	t.Helper()
	ctx := context.Background()
	rig := newTransferRigWith(t, func(opts *Options) {
		opts.Fleet = fleet.Options{Poll: 300 * time.Millisecond}
		opts.ClientIdentity = func(r *http.Request) (fleet.Identity, error) {
			id := r.Header.Get(testNodeHeader)
			return fleet.Identity{ID: id, Name: "name-of-" + id}, nil
		}
		opts.Workstreams = nil
		cfg, err := projectstore.LoadConfig(opts.Config)
		fixturekit.Must(t, err)
		repo, err := trace.Create(ctx, cfg.Root, cfg.Project, fixturekit.DemoStart, ownerActor)
		fixturekit.Must(t, err)
		fixturekit.Must(t, repo.CreateWorkstream(ctx, fixturekit.Stream, fixturekit.DemoStart, ownerActor))
		h := trace.Header{Schema: "osmia.trace.agent", Version: trace.Version, Revision: 1, ID: "agent", Project: cfg.Project.ID, Workstream: fixturekit.Stream, At: fixturekit.DemoStart, Actor: ownerActor, Cause: "message", Depth: 3}
		fixturekit.Must(t, repo.CreateThread(ctx, trace.Agent{Header: h, Role: role, ThreadID: "agent"}))
		fixturekit.Must(t, repo.Close())
	})
	p := rig.s.sole()
	if p == nil {
		t.Fatal("the project is not running")
	}
	r := &ctRig{transferRig: rig, fr: &fleetRig{s: rig.s, base: rig.base}, repo: p.repository, up: newCTUpstream(t), engine: &adaptertest.Engine{}, files: t.TempDir(), home: rig.home}
	for name, data := range map[string]string{"README.md": "first\n", "turn.txt": "the turn's starting point\n"} {
		fixturekit.Must(t, os.WriteFile(filepath.Join(r.files, name), []byte(data), 0o600))
	}
	grant := coreadapter.Capabilities{Tools: []string{"file_read", "file_write"}, WriteFiles: role == "mason"}
	r.turns = &isolation.Turns{
		Workspaces: ctSource{r.files}, Views: isolation.Views{Directory: mkdir(t, filepath.Join(t.TempDir(), "views"))}, Engine: r.engine,
		Grants: map[string]coreadapter.Capabilities{role: grant}, Hosts: r.up,
		Select: func(_ context.Context, scope coreadapter.Scope) (isolation.Selection, error) {
			sel := isolation.Selection{Paths: []string{"README.md", "turn.txt"}, Execution: coreadapter.ExecutionSettings{Mode: "none"}}
			if scope.Role == "mason" {
				sel.Commit = rig.hidden
			}
			return sel, nil
		},
		Audit: func(ctx context.Context, scope coreadapter.Scope, tool coreadapter.Tool, raw json.RawMessage) (func(context.Context, json.RawMessage, error) error, error) {
			return p.repository.BeginTool(ctx, scope, tool, raw, func() time.Time { return time.Now().UTC() })
		},
		Capture: func(_ context.Context, _ coreadapter.Scope, view *isolation.FileView, _ coreadapter.SessionResult) error {
			files := ctTree(t, view.Workspace().Directory)
			r.mu.Lock()
			r.captured = append(r.captured, files)
			r.mu.Unlock()
			return nil
		},
	}
	r.turns.Clients = rig.s.clients.Placer()
	r.runner = thread.Runner{Jev: ctJudge(), Store: p.repository, Now: func() time.Time { return fixturekit.DemoStart.Add(time.Second) }, Turns: r.turns}
	rig.s.clients.Bind(fixturekit.Project, clientturns.Binding{Repository: p.repository, Runner: r.runner, Capture: r.turns.Capture})
	return r
}

func mkdir(t *testing.T, dir string) string {
	t.Helper()
	fixturekit.Must(t, os.MkdirAll(dir, 0o700))
	return dir
}

func (r *ctRig) queue(t *testing.T, turn string) {
	t.Helper()
	req := trace.TurnRequest{Header: trace.Header{Schema: "osmia.trace.turn-request", Version: trace.Version, Revision: 1, ID: "request_" + turn, Project: fixturekit.Project, Workstream: fixturekit.Stream, At: fixturekit.DemoStart, Actor: ownerActor, Cause: "owner-message", Depth: 3},
		AgentID: "agent", ThreadID: "agent", TurnID: turn, Profile: coreadapter.Profile{Name: "default", Backend: "claude", Model: "test"}, SystemPrompt: "Role instructions", Prompt: "Do the work for " + turn}
	_, err := r.repo.EnqueueTurn(context.Background(), req)
	fixturekit.Must(t, err)
}

func (r *ctRig) captures() []map[string]string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]map[string]string(nil), r.captured...)
}

type ctClient struct {
	t      *testing.T
	rig    *ctRig
	node   string
	lease  string
	view   string
	offers chan api.TurnOffer
	stop   chan struct{}
	done   chan struct{}
}

func (r *ctRig) client(t *testing.T, node string) *ctClient {
	t.Helper()
	c := &ctClient{t: t, rig: r, node: node, view: mkdir(t, filepath.Join(t.TempDir(), "view")), offers: make(chan api.TurnOffer, 4), stop: make(chan struct{}), done: make(chan struct{})}
	resp, out := r.fr.connect(t, node, hello(2))
	if out.status != http.StatusOK {
		t.Fatalf("connect: %d %s", out.status, out.body)
	}
	c.lease = resp.Lease
	go c.serve()
	t.Cleanup(func() { close(c.stop); <-c.done })
	return c
}

func (c *ctClient) post(path string, body any) (int, []byte) {
	data, _ := json.Marshal(body)
	req, err := http.NewRequest(http.MethodPost, c.rig.base+path, bytes.NewReader(data))
	if err != nil {
		return 0, nil
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(testNodeHeader, c.node)
	resp, err := intakefixture.WebHTTP().Do(req)
	if err != nil {
		return 0, nil
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, out
}

func (c *ctClient) serve() {
	defer close(c.done)
	var ack int64
	for {
		select {
		case <-c.stop:
			return
		default:
		}
		status, body := c.post(api.ClientRoute(c.lease, "poll"), api.PollRequest{Ack: ack, WaitSeconds: 1})
		if status != http.StatusOK {
			time.Sleep(20 * time.Millisecond)
			continue
		}
		var resp api.PollResponse
		if json.Unmarshal(body, &resp) != nil {
			continue
		}
		for _, m := range resp.Messages {
			ack = max(ack, m.Seq)
			switch m.Kind {
			case api.MessageOffer:
				c.offers <- *m.Offer
			case api.MessageRequest:
				c.answer(*m.Request)
			}
		}
	}
}

func (c *ctClient) answer(req api.ServerRequest) {
	reply := api.ServerReply{}
	root, err := os.OpenRoot(c.view)
	if err != nil {
		return
	}
	defer root.Close()
	switch req.Op {
	case api.OpViewRead:
		var p api.ViewReadParams
		_ = json.Unmarshal(req.Params, &p)
		data, err := root.ReadFile(strings.TrimSuffix(p.Path, "/"))
		if err != nil {
			reply.Error = err.Error()
		} else {
			reply.Result, _ = json.Marshal(api.ViewReadResult{Data: data})
		}
	case api.OpViewWrite:
		var p api.ViewWriteParams
		_ = json.Unmarshal(req.Params, &p)
		err = root.MkdirAll(filepath.Dir(p.Path), 0o700)
		if err == nil {
			err = root.WriteFile(p.Path, p.Content, 0o600)
		}
		if err != nil {
			reply.Error = err.Error()
		} else {
			reply.Result = json.RawMessage(`{}`)
		}
	}
	c.post(api.ClientRoute(c.lease, "requests/"+req.ID+"/reply"), reply)
}

func (c *ctClient) nextOffer() api.TurnOffer {
	c.t.Helper()
	select {
	case offer := <-c.offers:
		return offer
	case <-time.After(20 * time.Second):
		c.t.Fatal("the server offered the client no turn")
		panic("unreachable")
	}
}

func (c *ctClient) claim(offer api.TurnOffer) {
	c.t.Helper()
	status, body := c.post(api.ClientRoute(c.lease, "turns/"+offer.Turn+"/claim"), api.ClaimRequest{Accept: true})
	if status != http.StatusOK || !strings.Contains(string(body), api.ClaimTaken) {
		c.t.Fatalf("claim: %d %s", status, body)
	}
}

func (c *ctClient) fetchView(offer api.TurnOffer) {
	c.t.Helper()
	cache := mkdir(c.t, filepath.Join(c.t.TempDir(), "cache"))
	if out, err := gitClient(c.t, cache, "init", "--quiet"); err != nil {
		c.t.Fatalf("init: %v\n%s", err, out)
	}
	if out, err := gitClient(c.t, cache, "fetch", "--quiet", c.rig.base+api.GitPath(config.ProjectID(offer.View.Project)), offer.View.Commit); err != nil {
		c.t.Fatalf("fetch the starting commit through the git route: %v\n%s", err, out)
	}
	if out, err := gitClient(c.t, cache, "--work-tree="+c.view, "read-tree", offer.View.Commit); err != nil {
		c.t.Fatalf("read-tree: %v\n%s", err, out)
	}
	args := append([]string{"--work-tree=" + c.view, "checkout-index", "-f", "--"}, offer.View.Paths...)
	if out, err := gitClient(c.t, cache, args...); err != nil {
		c.t.Fatalf("export: %v\n%s", err, out)
	}
}

func (c *ctClient) tool(turn, token, tool, body string) (int, string) {
	c.t.Helper()
	headers := map[string]string{"Content-Type": "application/json"}
	if token != "" {
		headers["Authorization"] = "Bearer " + token
	}
	out := send(c.t, http.MethodPost, c.rig.base+api.MCPPath(turn)+"/call/"+tool, "", headers, []byte(body))
	return out.status, string(out.body)
}

func (c *ctClient) upload(turn, token string) api.ViewAnswer {
	c.t.Helper()
	var buf bytes.Buffer
	fixturekit.Must(c.t, filetransport.Pack(&buf, c.view))
	out := send(c.t, http.MethodPut, c.rig.base+api.ViewPath(turn), "", map[string]string{"Content-Type": api.ViewArchiveType, "Authorization": "Bearer " + token}, buf.Bytes())
	if out.status != http.StatusOK {
		c.t.Fatalf("upload: %d %s", out.status, out.body)
	}
	var resp api.ViewUploadResponse
	fixturekit.Must(c.t, json.Unmarshal(out.body, &resp))
	return resp.Answer
}

func (c *ctClient) result(turn string, res coreadapter.SessionResult) api.ViewAnswer {
	c.t.Helper()
	status, body := c.post(api.ClientRoute(c.lease, "turns/"+turn+"/result"), api.TurnResult{Result: res})
	if status != http.StatusOK {
		c.t.Fatalf("result: %d %s", status, body)
	}
	var resp api.TurnResultResponse
	fixturekit.Must(c.t, json.Unmarshal(body, &resp))
	return resp.Answer
}

func ctComplete() coreadapter.SessionResult {
	return coreadapter.SessionResult{Session: coreadapter.BackendSession{Backend: "claude", ID: "session-1"}, FinalResponse: "done", StartedAt: fixturekit.DemoStart,
		Outcome: &coreadapter.Outcome{Status: "complete", Report: "done"}}
}

func commitTree(t *testing.T, home, files string) string {
	t.Helper()
	dir := mkdir(t, filepath.Join(t.TempDir(), "repo"))
	fixturekit.Must(t, execution.Mirror(files, dir))
	fixturekit.DemoGit(t, home, "init", "--quiet", "-b", "main", dir)
	gitIn(t, home, dir, "add", "-A")
	gitIn(t, home, dir, "commit", "--quiet", "-m", "the turn's files")
	return gitIn(t, home, dir, "rev-parse", "HEAD^{tree}")
}

func TestAFakeClientRunsAMasonTurnAndTheServerRecordsTheCommit(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	r := newCTRig(t, "mason")
	c := r.client(t, "node-1")
	r.s.clients.SetChooser(func(context.Context, coreadapter.Scope, coreadapter.Profile) (fleet.Identity, bool) {
		return fleet.Identity{ID: "node-1", Name: "name-of-node-1"}, true
	})
	r.queue(t, "t1")
	done := make(chan error, 1)
	go func() {
		_, err := r.runner.RunNext(ctx, fixturekit.Stream, "agent", coreadapter.PreparedTurn{SessionDirectory: mkdir(t, filepath.Join(t.TempDir(), "session"))})
		done <- err
	}()

	offer := c.nextOffer()
	if offer.View.Kind != api.ViewGit || offer.View.Commit != r.hidden || offer.View.Project != string(fixturekit.Project) || offer.ToolEndpoint != api.MCPPath("t1") || offer.ToolToken != "token-t1" {
		t.Fatalf("offer: %+v", offer)
	}
	c.claim(offer)
	c.fetchView(offer)
	if got := ctTree(t, c.view); !reflect.DeepEqual(got, map[string]string{"README.md": "first\n", "turn.txt": "the turn's starting point\n"}) {
		t.Fatalf("the view the client built from the commit: %v", got)
	}

	fixturekit.Must(t, os.WriteFile(filepath.Join(c.view, "turn.txt"), []byte("changed by the session directly\n"), 0o600))
	status, body := c.tool("t1", offer.ToolToken, "file_read", `{"path":"turn.txt"}`)
	if status != http.StatusOK || body != `"changed by the session directly\n"` {
		t.Fatalf("relayed read: %d %s", status, body)
	}
	if status, body = c.tool("t1", offer.ToolToken, "file_write", `{"path":"added.txt","content":"written by a relayed call\n"}`); status != http.StatusOK {
		t.Fatalf("relayed write: %d %s", status, body)
	}
	for name, token := range map[string]string{"another turn's token": "token-t2", "no token": ""} {
		if status, _ := c.tool("t1", token, "file_read", `{"path":"turn.txt"}`); status != http.StatusForbidden {
			t.Fatalf("a call with %s was answered %d", name, status)
		}
	}
	if status, _ := c.tool("t1", offer.ToolToken, "file_read", `{"path":"../outside"}`); status == http.StatusOK {
		t.Fatal("a path outside the view was read")
	}
	if got := ctTree(t, c.view)["added.txt"]; got != "written by a relayed call\n" {
		t.Fatalf("the relayed write is not in the client's view: %q", got)
	}

	if answer := c.upload("t1", offer.ToolToken); answer != api.ViewAccepted {
		t.Fatalf("upload: %s", answer)
	}
	if answer := c.result("t1", ctComplete()); answer != api.ViewAccepted {
		t.Fatalf("result: %s", answer)
	}
	fixturekit.Must(t, <-done)

	captures := r.captures()
	want := map[string]string{"README.md": "first\n", "turn.txt": "changed by the session directly\n", "added.txt": "written by a relayed call\n"}
	if len(captures) != 1 || !reflect.DeepEqual(captures[0], want) {
		t.Fatalf("captures: %v", captures)
	}
	remoteCommit := commitTree(t, r.home, c.view)

	r.queue(t, "t2")
	r.s.clients.SetChooser(nil)
	r.turns.Audit = nil
	r.engine.OnRun = func() error {
		for _, call := range []struct{ tool, input string }{{"file_write", `{"path":"turn.txt","content":"changed by the session directly\n"}`}, {"file_write", `{"path":"added.txt","content":"written by a relayed call\n"}`}} {
			if _, err := r.up.call("t2", call.tool, json.RawMessage(call.input)); err != nil {
				return err
			}
		}
		return nil
	}
	local, err := r.turns.Run(ctx, coreadapter.PreparedTurn{Scope: coreadapter.Scope{Project: string(fixturekit.Project), Workstream: string(fixturekit.Stream), Thread: "agent", Turn: "t2", Role: "mason"},
		Profile: coreadapter.Profile{Name: "default", Backend: "claude", Model: "test"}, SessionDirectory: mkdir(t, filepath.Join(t.TempDir(), "local")), Prompt: "Do the work"})
	if err != nil || local.Client != coreadapter.LocalClient {
		t.Fatalf("the local run: %+v %v", local, err)
	}
	captures = r.captures()
	if len(captures) != 2 || !reflect.DeepEqual(captures[1], want) {
		t.Fatalf("the local capture of the same files: %v", captures)
	}
	localView := t.TempDir()
	for name, data := range captures[1] {
		fixturekit.Must(t, os.WriteFile(filepath.Join(localView, name), []byte(data), 0o600))
	}
	if localCommit := commitTree(t, r.home, localView); localCommit != remoteCommit {
		t.Fatalf("the commit recorded from the returned view differs from the local one: %s %s", remoteCommit, localCommit)
	}

	th, err := r.repo.Thread(fixturekit.Stream, "agent")
	fixturekit.Must(t, err)
	if len(th.Turns) < 1 || th.Turns[0].Response == nil || th.Turns[0].Response.Result.Client != "node-1" {
		t.Fatalf("the trace does not record the client that ran the turn: %+v", th.Turns)
	}
	if holder, ok := th.SessionClient(); !ok || holder != "node-1" {
		t.Fatalf("session holder: %q %v", holder, ok)
	}
	if answer := c.upload("t1", offer.ToolToken); answer != api.ViewAlreadyRecorded {
		t.Fatalf("the view again: %s", answer)
	}
	if answer := c.result("t1", ctComplete()); answer != api.ViewAlreadyRecorded {
		t.Fatalf("the result again: %s", answer)
	}
	if len(r.captures()) != 2 {
		t.Fatal("a resubmission was captured")
	}
}

func TestAFakeClientRunsAnArchitectTurnFromAnArchiveView(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	r := newCTRig(t, "architect")
	c := r.client(t, "node-1")
	r.s.clients.SetChooser(func(context.Context, coreadapter.Scope, coreadapter.Profile) (fleet.Identity, bool) {
		return fleet.Identity{ID: "node-1", Name: "name-of-node-1"}, true
	})
	r.queue(t, "t1")
	done := make(chan error, 1)
	go func() {
		_, err := r.runner.RunNext(ctx, fixturekit.Stream, "agent", coreadapter.PreparedTurn{SessionDirectory: mkdir(t, filepath.Join(t.TempDir(), "session"))})
		done <- err
	}()
	offer := c.nextOffer()
	if offer.View.Kind != api.ViewArchive || offer.ReturnView {
		t.Fatalf("offer: %+v", offer)
	}
	c.claim(offer)
	out := send(t, http.MethodGet, r.base+api.ViewPath("t1"), "", bearer(offer.ToolToken), nil)
	if out.status != http.StatusOK || out.header.Get("Content-Type") != api.ViewArchiveType {
		t.Fatalf("download: %d %s", out.status, out.body)
	}
	fixturekit.Must(t, filetransport.Unpack(bytes.NewReader(out.body), c.view))
	if got := ctTree(t, c.view); !reflect.DeepEqual(got, map[string]string{"README.md": "first\n", "turn.txt": "the turn's starting point\n"}) {
		t.Fatalf("the archive: %v", got)
	}
	if out := send(t, http.MethodGet, r.base+api.ViewPath("t1"), "", bearer("token-t2"), nil); out.status != http.StatusForbidden {
		t.Fatalf("another turn's token downloaded the view: %d", out.status)
	}
	if answer := c.result("t1", ctComplete()); answer != api.ViewAccepted {
		t.Fatalf("result: %s", answer)
	}
	fixturekit.Must(t, <-done)
	th, err := r.repo.Thread(fixturekit.Stream, "agent")
	fixturekit.Must(t, err)
	if len(th.Turns) != 1 || th.Turns[0].Response == nil || th.Turns[0].Response.Result.Outcome == nil || th.Turns[0].Response.Result.Client != "node-1" {
		t.Fatalf("recorded: %+v", th.Turns)
	}
}
