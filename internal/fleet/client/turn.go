package client

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/kpenfound/agentcore/agent"
	"github.com/kpenfound/osmia/internal/api"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/isolation"
	"github.com/kpenfound/osmia/internal/redact"
)

// The limits the server puts on the session files a result returns.
const (
	maxSessionFiles = 64
	maxSessionBytes = 4 << 20
)

// turn is a turn this client runs.
type turn struct {
	offer  api.TurnOffer
	ctx    context.Context
	cancel context.CancelCauseFunc

	mu     sync.Mutex
	phase  string
	token  string
	files  isolation.Files
	viewed bool
}

func (t *turn) setPhase(phase string) {
	t.mu.Lock()
	t.phase = phase
	t.mu.Unlock()
}

func (t *turn) currentPhase() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.phase
}

// setToken replaces the token the turn's tool calls and view upload carry, as
// the server does for a turn that reattaches after it restarted.
func (t *turn) setToken(token string) {
	t.mu.Lock()
	t.token = token
	t.mu.Unlock()
}

func (t *turn) uploadToken() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.token
}

func (t *turn) toolToken() string { return t.uploadToken() }

func (t *turn) setFiles(f isolation.Files) {
	t.mu.Lock()
	t.files, t.viewed = f, true
	t.mu.Unlock()
}

func (t *turn) fileSet() (isolation.Files, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.files, t.viewed
}

func (c *Client) cancelTurn(id, reason string) {
	c.mu.Lock()
	t := c.running[id]
	c.mu.Unlock()
	if t != nil {
		c.log.Warn("cancelling a turn", "turn", id, "reason", reason)
		t.cancel(errors.New(reason))
	}
}

func (c *Client) cancelAll(cause error) {
	c.mu.Lock()
	var all []*turn
	for _, t := range c.running {
		all = append(all, t)
	}
	c.mu.Unlock()
	for _, t := range all {
		t.cancel(cause)
	}
}

func (c *Client) handleOffer(offer api.TurnOffer) {
	c.mu.Lock()
	reason := ""
	switch {
	case c.draining:
		reason = "the client is draining"
	case c.running[offer.Turn] != nil:
		reason = "the client already runs the turn"
	case len(c.running) >= c.slots:
		reason = "the client has no free slot"
	case !plainName.MatchString(offer.Turn) || offer.Project != "" && !plainName.MatchString(offer.Project) || offer.Thread != "" && !plainName.MatchString(offer.Thread):
		reason = "the offer names a turn, project or thread this client cannot keep files for"
	}
	var t *turn
	if reason == "" {
		t = &turn{offer: offer, token: offer.ToolToken, phase: "starting"}
		t.ctx, t.cancel = context.WithCancelCause(c.rootCtx)
		c.running[offer.Turn] = t
	}
	lease := ""
	if c.connected {
		lease = c.lease
	}
	c.mu.Unlock()
	var answer api.ClaimResponse
	err := error(nil)
	if lease == "" {
		err = errors.New("not connected")
	} else {
		err = c.post(c.rootCtx, api.ClientRoute(lease, "turns/"+offer.Turn+"/claim"), api.ClaimRequest{Accept: reason == "", Reason: reason}, &answer, callTimeout)
	}
	if t == nil {
		return
	}
	if err != nil || answer.Answer != api.ClaimTaken {
		c.log.Warn("the turn was not taken", "turn", offer.Turn, "answer", answer.Answer, "error", err)
		c.drop(t)
		return
	}
	c.log.Info("took a turn", "turn", offer.Turn, "role", offer.ShownRole(), "profile", offer.Profile)
	c.wg.Add(1)
	go func() {
		defer c.wg.Done()
		c.runTurn(t)
	}()
}

func (c *Client) drop(t *turn) {
	t.cancel(nil)
	c.mu.Lock()
	delete(c.running, t.offer.Turn)
	c.mu.Unlock()
	c.changed()
}

// runTurn builds the turn's view, runs its session, and hands the result and
// the finished view to the delivery loop, which keeps them on disk until the
// server has answered for them.
func (c *Client) runTurn(t *turn) {
	offer := t.offer
	r := &record{Turn: offer.Turn, Project: offer.Project, Workstream: offer.Workstream, Thread: offer.Thread, Role: offer.Role, ReturnView: offer.ReturnView, Token: offer.ToolToken, State: stateRunning}
	if err := c.saveRecord(r); err != nil {
		c.log.Error("cannot keep a record of the turn", "turn", offer.Turn, "error", err)
	}
	res, viewDir := c.execute(t)
	r.mu.Lock()
	r.ViewDir, r.State, r.Token = viewDir, stateFinished, t.uploadToken()
	r.mu.Unlock()
	if err := c.saveResult(r, res); err != nil {
		c.log.Error("cannot keep the turn's result on disk", "turn", offer.Turn, "error", err)
		r.result = &res
	}
	if err := c.saveRecord(r); err != nil {
		c.log.Error("cannot keep a record of the turn", "turn", offer.Turn, "error", err)
	}
	c.mu.Lock()
	c.pending[offer.Turn] = r
	delete(c.running, offer.Turn)
	c.mu.Unlock()
	t.cancel(nil)
	c.log.Info("a turn ended", "turn", offer.Turn, "failed", res.Result.IsError, "cancelled", res.Result.Cancelled)
	c.kick()
}

func (c *Client) failure(t *turn, started time.Time, err error) api.TurnResult {
	res := coreadapter.SessionResult{StartedAt: started, IsError: true, ErrorSubtype: "client", FinalResponse: err.Error()}
	if t.ctx.Err() != nil {
		res.Cancelled, res.ErrorSubtype = true, "cancelled"
	}
	return api.TurnResult{Result: res}
}

// execute runs one turn and returns its result and the directory of its finished
// view, which is empty when no view was built.
func (c *Client) execute(t *turn) (api.TurnResult, string) {
	offer := t.offer
	started := c.now().UTC()
	t.setPhase("fetching the files")
	views := filepath.Join(c.turnDir(offer.Turn), "views")
	view, err := c.buildView(t.ctx, offer, t.uploadToken(), views)
	if err != nil {
		c.log.Error("cannot build the turn's view", "turn", offer.Turn, "error", err)
		return c.failure(t, started, fmt.Errorf("the client could not build the turn's view: %w", err)), ""
	}
	dir := view.Workspace().Directory
	defer func() { _ = view.Detach() }()
	files := isolation.Files{View: view}
	if skills := isolation.NewSkillReader(c.engine, coreadapter.TurnSkills(offer.Execution, offer.Settings.Backend)); skills != nil {
		files.Skills = skills
	}
	t.setFiles(files)
	t.setPhase("running the session")
	res, err := c.session(t, view)
	if err != nil {
		res.IsError = true
		if res.FinalResponse == "" {
			res.FinalResponse = err.Error()
		}
		c.log.Warn("the session failed", "turn", offer.Turn, "error", err)
	}
	t.setPhase("sending the result")
	out := api.TurnResult{Result: res, SessionFiles: c.sessionFiles(res.SessionDirectory)}
	out.Result.SessionDirectory = ""
	return out, dir
}

func (c *Client) sessionDir(offer api.TurnOffer) string {
	return filepath.Join(c.dir, "sessions", cmpOr(offer.Project, "project"), cmpOr(offer.Thread, "thread"), offer.Turn)
}

// session runs the turn's model session through the execution seam the server
// uses, in the turn's view, under the role's configured sandbox mode, with this
// machine's own provider login and Dagger setting.
func (c *Client) session(t *turn, view *isolation.FileView) (coreadapter.SessionResult, error) {
	offer := t.offer
	sessionDir := c.sessionDir(offer)
	if err := os.MkdirAll(sessionDir, 0o700); err != nil {
		return coreadapter.SessionResult{}, err
	}
	env := maps.Clone(offer.Environment)
	if env == nil {
		env = map[string]string{}
	}
	execution, cloud, err := c.dagger(offer.Execution, offer.RunsDagger)
	if err != nil {
		return coreadapter.SessionResult{}, err
	}
	maps.Copy(env, cloud)
	var endpoints []coreadapter.Endpoint
	if offer.ToolEndpoint != "" {
		endpoint, stop, err := c.startMCP(t.ctx, t, execution.Mode)
		if err != nil {
			return coreadapter.SessionResult{}, err
		}
		defer stop()
		env[coreadapter.TokenEnvironment] = endpoint.Token
		endpoints = []coreadapter.Endpoint{endpoint}
	}
	isolated := coreadapter.Isolation{Workspace: view.Workspace(), Capabilities: offer.Capabilities, Environment: env, DenyVCS: true, DenyInheritedEnvironment: true, DenyDeliveryCredentials: true}
	prepared := coreadapter.PreparedTurn{Execution: execution, Scope: coreadapter.Scope{Project: offer.Project, Workstream: offer.Workstream, Unit: offer.Unit, Thread: offer.Thread, Turn: offer.Turn, Role: offer.Role},
		Profile: offer.Settings, Sandbox: coreadapter.SandboxLease{Verified: isolated}, MCP: endpoints, SessionDirectory: sessionDir,
		SystemPrompt: offer.SystemPrompt, Prompt: offer.Prompt, History: offer.History, AllowedOutcomes: offer.AllowedOutcomes}
	if offer.ResumeID != "" {
		prepared.Resume = &coreadapter.BackendSession{Backend: offer.Settings.Backend, ID: offer.ResumeID}
	}
	res, err := (&coreadapter.TurnRunner{Executor: coreadapter.CoreExecutor{Required: isolated, Runner: c.engine}}).Run(t.ctx, prepared)
	res.SessionDirectory = sessionDir
	return res, err
}

// dagger gives a turn that runs Dagger this machine's own setting: Dagger Cloud
// when the machine holds a compute token, otherwise the engine its Dagger CLI is
// pointed at. The server's engine address and token are never used here.
func (c *Client) dagger(execution coreadapter.ExecutionSettings, runs bool) (coreadapter.ExecutionSettings, map[string]string, error) {
	execution.DaggerCloudTokenEnv = ""
	if !runs {
		return execution, nil, nil
	}
	if c.getenv(daggerCloudToken) != "" {
		execution.Dagger, execution.DaggerCloudTokenEnv = nil, daggerCloudToken
		env, err := coreadapter.DaggerCloudEnvironment(execution, c.getenv)
		if err != nil {
			return execution, nil, err
		}
		redact.Register(env[coreadapter.DaggerCloudTokenEnvironment])
		return execution, env, nil
	}
	engine := c.getenv(daggerEngineHost)
	if engine == "" {
		return execution, nil, fmt.Errorf("this machine does not say where Dagger runs: set %s or %s", daggerCloudToken, daggerEngineHost)
	}
	version := ""
	if execution.Dagger != nil {
		version = execution.Dagger.Version
	}
	execution.Dagger = &agent.Dagger{Engine: engine, Version: version}
	return execution, nil, nil
}

// sessionFiles reads what the server reads from a session directory after a turn:
// the plain files at its top, within the limits the server takes. The session
// directory itself stays here, for resume.
func (c *Client) sessionFiles(dir string) []api.SessionFile {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	type candidate struct {
		name string
		size int64
	}
	var found []candidate
	for _, e := range entries {
		info, err := e.Info()
		if err != nil || !info.Mode().IsRegular() || e.Name()[0] == '.' {
			continue
		}
		found = append(found, candidate{e.Name(), info.Size()})
	}
	sort.Slice(found, func(i, j int) bool { return found[i].size < found[j].size })
	var out []api.SessionFile
	var total int64
	for _, f := range found {
		if len(out) == maxSessionFiles || total+f.size > maxSessionBytes {
			break
		}
		data, err := os.ReadFile(filepath.Join(dir, f.name))
		if err != nil {
			continue
		}
		total += int64(len(data))
		out = append(out, api.SessionFile{Name: f.name, Data: data})
	}
	return out
}
