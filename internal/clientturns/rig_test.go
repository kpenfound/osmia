package clientturns

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kpenfound/osmia/internal/activity"
	"github.com/kpenfound/osmia/internal/api"
	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/filetransport"
	"github.com/kpenfound/osmia/internal/fleet"
	"github.com/kpenfound/osmia/internal/isolation"
	"github.com/kpenfound/osmia/internal/jev"
	"github.com/kpenfound/osmia/internal/thread"
	"github.com/kpenfound/osmia/internal/trace"
)

const project config.ProjectID = "p_00000000000000000000000000000001"
const stream config.WorkstreamID = "w_00000000000000000000000000000001"

var timestamp = time.Date(2026, 11, 25, 9, 0, 0, 0, time.UTC)

type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *clock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func put(t *testing.T, root, name, data string) {
	t.Helper()
	path := filepath.Join(root, name)
	must(t, os.MkdirAll(filepath.Dir(path), 0o700))
	must(t, os.WriteFile(path, []byte(data), 0o600))
}

func read(t *testing.T, root, name string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(root, name))
	must(t, err)
	return string(data)
}

type lease func(context.Context) error

func (f lease) Release(ctx context.Context) error { return f(ctx) }

type source struct{ directory string }

func (s source) Acquire(_ context.Context, req coreadapter.WorkspaceRequest) (coreadapter.WorkspaceLease, error) {
	return coreadapter.WorkspaceLease{Workspace: coreadapter.Workspace{Directory: s.directory, Access: req.Access}, Lease: lease(func(context.Context) error { return nil })}, nil
}

type upstream struct {
	server *httptest.Server
	mu     sync.Mutex
	tools  map[string][]coreadapter.Tool
	tokens map[string]string
	hits   map[string]int
}

func newUpstream(t *testing.T) *upstream {
	u := &upstream{tools: map[string][]coreadapter.Tool{}, tokens: map[string]string{}, hits: map[string]int{}}
	u.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/mcp/"), "/")
		if len(parts) != 3 || parts[1] != "call" {
			http.NotFound(w, r)
			return
		}
		turn, name := parts[0], parts[2]
		u.mu.Lock()
		want := u.tokens[turn]
		u.mu.Unlock()
		if r.Header.Get("Authorization") != "Bearer "+want {
			http.Error(w, "refused", http.StatusUnauthorized)
			return
		}
		u.mu.Lock()
		tools := u.tools[turn]
		u.hits[turn]++
		u.mu.Unlock()
		body, _ := io.ReadAll(r.Body)
		for _, tool := range tools {
			if tool.Name == name {
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

func (u *upstream) Host(_ context.Context, req coreadapter.HostRequest) (coreadapter.HostedMCP, error) {
	u.mu.Lock()
	token := "token-" + req.Scope.Turn
	if _, hosted := u.tools[req.Scope.Turn]; hosted {
		token += "-hosted-again"
	}
	u.tools[req.Scope.Turn] = req.Tools
	u.tokens[req.Scope.Turn] = token
	u.mu.Unlock()
	return coreadapter.HostedMCP{Endpoint: coreadapter.Endpoint{URL: u.server.URL + "/mcp/" + req.Scope.Turn, BearerTokenEnvironment: coreadapter.TokenEnvironment, Token: token},
		Lease: lease(func(context.Context) error { return nil })}, nil
}

func (u *upstream) calls(turn string) int {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.hits[turn]
}

type captured struct {
	scope coreadapter.Scope
	files map[string]string
}

type rig struct {
	t         *testing.T
	root      config.Root
	project   config.Project
	repo      *trace.Repository
	clock     *clock
	fleet     *fleet.Registry
	transfers *filetransport.Registry
	acts      *activity.Registry
	d         *Dispatcher
	up        *upstream
	source    string
	turns     *isolation.Turns
	runner    thread.Runner

	mu      sync.Mutex
	audited []string
	capture []captured
	backend string
	skills  []string
}

func judge() *jev.Judge {
	settings := config.Jev{URL: config.DefaultJevURL, Model: "jev-test", APIKeyEnv: "TEST_JEV_KEY", Timeout: "1s"}
	return &jev.Judge{Config: func() config.Jev { return settings }, Getenv: func(string) string { return "" }}
}

func newRig(t *testing.T, role string) *rig {
	t.Helper()
	ctx := context.Background()
	base := t.TempDir()
	root, err := config.ResolveRoot(filepath.Join(base, "osmia"), "")
	must(t, err)
	p := config.Project{ID: project, Clone: filepath.Join(base, "target")}
	must(t, os.Mkdir(p.Clone, 0o700))
	owner := trace.Actor{Kind: "owner", ID: "local"}
	repo, err := trace.Create(ctx, root, p, timestamp, owner)
	must(t, err)
	t.Cleanup(func() { repo.Close() })
	must(t, repo.CreateWorkstream(ctx, stream, timestamp, owner))
	h := trace.Header{Schema: "osmia.trace.agent", Version: 1, Revision: 1, ID: "agent", Project: project, Workstream: stream, At: timestamp, Actor: owner, Cause: "message", Depth: 3}
	must(t, repo.CreateThread(ctx, trace.Agent{Header: h, Role: role, ThreadID: "thread"}))
	must(t, os.MkdirAll(filepath.Join(root.String(), "build"), 0o700))
	r := &rig{t: t, root: root, project: p, repo: repo, clock: &clock{t: timestamp}, up: newUpstream(t), source: filepath.Join(base, "source"), backend: "fake"}
	put(t, r.source, "src/main.go", "package main\n")
	put(t, r.source, "docs/notes.md", "# notes\n")
	r.start(role, repo)
	return r
}

func (r *rig) start(role string, repo *trace.Repository) {
	t := r.t
	transfers, err := r.root.Transfers()
	must(t, err)
	r.repo = repo
	r.fleet = fleet.NewRegistry(fleet.Options{Now: r.clock.now, Poll: 300 * time.Millisecond})
	r.transfers = filetransport.NewRegistry(transfers)
	r.acts = activity.New(r.clock.now)
	r.d = New(Dispatcher{Fleet: r.fleet, Transfers: r.transfers, Activity: r.acts, Directory: transfers, Views: filepath.Join(r.root.String(), "views"), Now: r.clock.now,
		LateWait: 400 * time.Millisecond, ViewWait: 400 * time.Millisecond, OfferWait: 5 * time.Second, Hold: time.Minute})
	grant := coreadapter.Capabilities{Tools: []string{"file_read", "file_write", "note"}, WriteFiles: role == "mason", Execute: false}
	r.turns = &isolation.Turns{
		Workspaces: source{r.source}, Views: isolation.Views{Directory: filepath.Join(r.root.String(), "build")},
		Grants: map[string]coreadapter.Capabilities{role: grant},
		Select: func(_ context.Context, scope coreadapter.Scope) (isolation.Selection, error) {
			sel := isolation.Selection{Paths: []string{"src", "docs"}, Execution: coreadapter.ExecutionSettings{Mode: "sbx", Skills: r.skills}}
			if scope.Role == "mason" {
				sel.Commit = "0123456789abcdef0123456789abcdef01234567"
			}
			return sel, nil
		},
		Hosts: r.up,
		Tools: []coreadapter.Tool{{Name: "note", Effect: coreadapter.ToolMemory, InputSchema: json.RawMessage(`{"type":"object"}`),
			Handle: func(_ context.Context, raw json.RawMessage) (json.RawMessage, error) {
				return json.Marshal("noted " + string(raw))
			}}},
		Audit: func(_ context.Context, scope coreadapter.Scope, tool coreadapter.Tool, raw json.RawMessage) (func(context.Context, json.RawMessage, error) error, error) {
			r.mu.Lock()
			r.audited = append(r.audited, scope.Turn+" "+tool.Name+" "+string(raw))
			r.mu.Unlock()
			return func(context.Context, json.RawMessage, error) error { return nil }, nil
		},
		Capture: func(_ context.Context, scope coreadapter.Scope, view *isolation.FileView, _ coreadapter.SessionResult) error {
			files, err := tree(view.Workspace().Directory)
			if err != nil {
				return err
			}
			r.mu.Lock()
			r.capture = append(r.capture, captured{scope, files})
			r.mu.Unlock()
			return nil
		},
		Chain: func(string) []string { return []string{"fallback"} },
	}
	r.turns.Clients = r.d.Placer()
	r.runner = thread.Runner{Jev: judge(), Store: repo, Now: func() time.Time { return timestamp.Add(time.Second) }, Turns: r.turns}
	r.d.Bind(project, Binding{Repository: repo, Runner: r.runner, Capture: r.turns.Capture, Rehost: r.turns.Rehost})
	repo.HoldTurns(r.d.Held)
}

func (r *rig) captures() []captured {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]captured(nil), r.capture...)
}

func (r *rig) audits() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.audited...)
}

func (r *rig) queue(turn string) {
	r.t.Helper()
	req := trace.TurnRequest{Header: trace.Header{Schema: "osmia.trace.turn-request", Version: 1, Revision: 1, ID: "request_" + turn, Project: project, Workstream: stream, At: timestamp,
		Actor: trace.Actor{Kind: "owner", ID: "local"}, Cause: "owner-message", Depth: 3}, AgentID: "agent", ThreadID: "thread", TurnID: turn,
		Profile: coreadapter.Profile{Name: "default", Backend: r.backend, Model: "test"}, SystemPrompt: "Role instructions", Prompt: "Do the work for " + turn}
	_, err := r.repo.EnqueueTurn(context.Background(), req)
	must(r.t, err)
}

func (r *rig) place(node fleet.Identity) {
	r.d.SetChooser(func(context.Context, coreadapter.Scope, coreadapter.Profile) (fleet.Identity, bool) {
		return node, true
	})
}

type outcome struct {
	turn trace.QueuedTurn
	err  error
}

func (r *rig) run(ctx context.Context, turn string) <-chan outcome {
	done := make(chan outcome, 1)
	prepared := coreadapter.PreparedTurn{SessionDirectory: filepath.Join(r.root.String(), "sessions", turn)}
	must(r.t, os.MkdirAll(prepared.SessionDirectory, 0o700))
	go func() {
		q, err := r.runner.RunNext(ctx, stream, "agent", prepared)
		done <- outcome{q, err}
	}()
	return done
}

func waitFor[T any](t *testing.T, ch <-chan T, what string) T {
	t.Helper()
	select {
	case v := <-ch:
		return v
	case <-time.After(20 * time.Second):
		t.Fatalf("timed out waiting for %s", what)
		panic("unreachable")
	}
}

type fakeClient struct {
	r      *rig
	node   fleet.Identity
	lease  string
	view   string
	skills string

	mu      sync.Mutex
	ack     int64
	offers  chan api.TurnOffer
	cancels chan string
	stop    context.CancelFunc
	reads   []string
}

func (r *rig) client(id string) *fakeClient {
	c := &fakeClient{r: r, node: fleet.Identity{ID: id, Name: "node-" + id}, view: filepath.Join(r.t.TempDir(), "view"), skills: filepath.Join(r.t.TempDir(), "skills"),
		offers: make(chan api.TurnOffer, 8), cancels: make(chan string, 8)}
	must(r.t, os.MkdirAll(c.view, 0o700))
	must(r.t, os.MkdirAll(c.skills, 0o700))
	return c
}

func (c *fakeClient) connect(req api.ClientConnectRequest) api.ClientConnectResponse {
	c.r.t.Helper()
	if req.Version == 0 {
		req.Version = api.ClientProtocolVersion
	}
	if req.Slots == 0 {
		req.Slots = 2
	}
	resp, err := c.r.fleet.Connect(context.Background(), c.node, req)
	must(c.r.t, err)
	c.mu.Lock()
	c.lease = resp.Lease
	c.mu.Unlock()
	return resp
}

func (c *fakeClient) serve() {
	ctx, cancel := context.WithCancel(context.Background())
	c.stop = cancel
	c.r.t.Cleanup(cancel)
	go func() {
		for ctx.Err() == nil {
			c.mu.Lock()
			ack, lease := c.ack, c.lease
			c.mu.Unlock()
			messages, err := c.r.fleet.Poll(ctx, lease, c.node, api.PollRequest{Ack: ack, WaitSeconds: 1})
			if err != nil {
				time.Sleep(20 * time.Millisecond)
				continue
			}
			for _, m := range messages {
				c.mu.Lock()
				if m.Seq > c.ack {
					c.ack = m.Seq
				}
				c.mu.Unlock()
				switch m.Kind {
				case api.MessageOffer:
					c.offers <- *m.Offer
				case api.MessageCancel:
					c.cancels <- m.Cancel.Turn
				case api.MessageRequest:
					go c.answer(*m.Request)
				}
			}
		}
	}()
}

func (c *fakeClient) answer(req api.ServerRequest) {
	reply := api.ServerReply{}
	switch req.Op {
	case api.OpViewRead, api.OpSkillRead:
		var p api.ViewReadParams
		_ = json.Unmarshal(req.Params, &p)
		c.mu.Lock()
		c.reads = append(c.reads, req.Op+" "+p.Path)
		c.mu.Unlock()
		root := c.view
		name := p.Path
		if req.Op == api.OpSkillRead {
			root, name = c.skills, strings.TrimPrefix(p.Path, "/skills/")
		}
		data, err := readIn(root, name)
		if err != nil {
			reply.Error = err.Error()
		} else {
			reply.Result, _ = json.Marshal(api.ViewReadResult{Data: data})
		}
	case api.OpViewWrite:
		var p api.ViewWriteParams
		_ = json.Unmarshal(req.Params, &p)
		if err := writeIn(c.view, p.Path, p.Content); err != nil {
			reply.Error = err.Error()
		} else {
			reply.Result = json.RawMessage(`{}`)
		}
	}
	_, _ = c.r.fleet.Reply(c.lease, c.node, req.ID, reply)
}

func readIn(root, name string) ([]byte, error) {
	r, err := os.OpenRoot(root)
	if err != nil {
		return nil, err
	}
	defer r.Close()
	name = strings.TrimSuffix(name, "/")
	if name == "." {
		entries, err := os.ReadDir(root)
		if err != nil {
			return nil, err
		}
		var names []string
		for _, e := range entries {
			if e.IsDir() {
				names = append(names, e.Name()+"/")
			} else {
				names = append(names, e.Name())
			}
		}
		return []byte(strings.Join(names, "\n")), nil
	}
	return r.ReadFile(name)
}

func writeIn(root, name string, data []byte) error {
	r, err := os.OpenRoot(root)
	if err != nil {
		return err
	}
	defer r.Close()
	if err := r.MkdirAll(filepath.Dir(name), 0o700); err != nil {
		return err
	}
	return r.WriteFile(name, data, 0o600)
}

func (c *fakeClient) claim(offer api.TurnOffer, files map[string]string) {
	c.r.t.Helper()
	resp, err := c.r.fleet.Claim(c.lease, c.node, offer.Turn, api.ClaimRequest{Accept: true})
	must(c.r.t, err)
	if resp != api.ClaimTaken {
		c.r.t.Fatalf("claim answered %s", resp)
	}
	switch offer.View.Kind {
	case api.ViewArchive:
		w := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, api.ViewPath(offer.Turn), nil)
		req.Header.Set("Authorization", "Bearer "+offer.ToolToken)
		if refusal := c.r.transfers.Download(w, req, offer.Turn); refusal != nil {
			c.r.t.Fatalf("download refused: %s", refusal.Message)
		}
		must(c.r.t, filetransport.Unpack(w.Body, c.view))
	default:
		for name, data := range files {
			put(c.r.t, c.view, name, data)
		}
	}
}

func (c *fakeClient) nextOffer() api.TurnOffer {
	c.r.t.Helper()
	return waitFor(c.r.t, c.offers, "an offer")
}

func (c *fakeClient) upload(turn, token string) (api.ViewAnswer, *api.APIError) {
	c.r.t.Helper()
	var buf bytes.Buffer
	must(c.r.t, filetransport.Pack(&buf, c.view))
	return c.uploadArchive(turn, token, buf.Bytes())
}

func (c *fakeClient) uploadArchive(turn, token string, archive []byte) (api.ViewAnswer, *api.APIError) {
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPut, api.ViewPath(turn), bytes.NewReader(archive))
	req.Header.Set("Content-Type", api.ViewArchiveType)
	req.Header.Set("Authorization", "Bearer "+token)
	out, refusal := c.r.transfers.Upload(w, req, turn)
	return out.Answer, refusal
}

func (c *fakeClient) result(turn string, res coreadapter.SessionResult, files ...api.SessionFile) (api.ViewAnswer, error) {
	return c.r.fleet.Result(context.Background(), c.lease, c.node, turn, api.TurnResult{Result: res, SessionFiles: files})
}

func complete(summary string) coreadapter.SessionResult {
	return coreadapter.SessionResult{Session: coreadapter.BackendSession{Backend: "fake", ID: "session-1"}, FinalResponse: summary, StartedAt: timestamp,
		Outcome: &coreadapter.Outcome{Status: "complete", Report: summary}}
}

func (r *rig) relay(turn, token, tool, body string) (int, string) {
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, api.MCPPath(turn)+"/call/"+tool, strings.NewReader(body))
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if refusal := r.d.Relay.Serve(w, req, turn); refusal != nil {
		return http.StatusForbidden, refusal.Message
	}
	return w.Code, w.Body.String()
}

func tree(dir string) (map[string]string, error) {
	files := map[string]string{}
	err := filepath.WalkDir(dir, func(path string, e os.DirEntry, err error) error {
		if err != nil || e.IsDir() {
			return err
		}
		data, err := os.ReadFile(path)
		rel, _ := filepath.Rel(dir, path)
		files[filepath.ToSlash(rel)] = string(data)
		return err
	})
	return files, err
}
