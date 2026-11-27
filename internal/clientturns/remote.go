package clientturns

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"

	"github.com/kpenfound/osmia/internal/api"
	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/filetransport"
	"github.com/kpenfound/osmia/internal/fleet"
	"github.com/kpenfound/osmia/internal/isolation"
	"github.com/kpenfound/osmia/internal/trace"
)

type remote struct {
	d    *Dispatcher
	node fleet.Identity
}

var _ isolation.Client = (*remote)(nil)

func (c *remote) ID() string   { return c.node.ID }
func (c *remote) Remote() bool { return true }

// ErrClientLost is the failure of a turn whose client's lease ended before it
// returned a result.
var ErrClientLost = errors.New("the client running the turn was lost")

func (c *remote) call(turn string) func(ctx context.Context, op string, params any) (json.RawMessage, error) {
	return func(ctx context.Context, op string, params any) (json.RawMessage, error) {
		return c.d.Fleet.Call(ctx, c.node.ID, turn, op, params)
	}
}

// Files returns the tools' view and skills: the client's own, reached through
// the client's connection, never a copy on the service.
func (c *remote) Files(ctx context.Context, t isolation.ClientTurn) (isolation.Files, error) {
	turn := t.Prepared.Scope.Turn
	files := isolation.Files{View: remoteFiles{ctx: ctx, call: c.call(turn)}}
	if len(t.SkillRefs) != 0 {
		files.Skills = remoteSkills{call: c.call(turn)}
	}
	return files, nil
}

func (c *remote) offer(t isolation.ClientTurn, token string) api.TurnOffer {
	scope, p := t.Prepared.Scope, t.Prepared
	execution := p.Execution
	execution.DaggerCloudTokenEnv = ""
	offer := api.TurnOffer{Turn: scope.Turn, Project: scope.Project, Workstream: scope.Workstream, Unit: scope.Unit, Thread: scope.Thread, Role: scope.Role,
		Profile: p.Profile.Name, Fallbacks: c.runnable(t.Fallbacks), Settings: p.Profile, SystemPrompt: p.SystemPrompt, Prompt: p.Prompt, History: p.History,
		AllowedOutcomes: p.AllowedOutcomes, Capabilities: t.Capabilities, Execution: execution, RunsDagger: t.RunsDagger, Environment: t.Environment,
		ToolToken: token, ReturnView: t.ReturnView, View: api.TurnView{Kind: api.ViewArchive}}
	if p.Resume != nil {
		offer.ResumeID = p.Resume.ID
	}
	if t.Endpoint.URL != "" {
		offer.ToolEndpoint = api.MCPPath(scope.Turn)
	}
	if t.Start != nil {
		offer.View = api.TurnView{Kind: api.ViewGit, Project: t.Start.Project, Commit: t.Start.Commit, Paths: t.Start.Paths, Overlay: t.Overlay != nil}
	}
	return offer
}

// runnable keeps the profiles the node's client reported it can run: the
// client is not offered a fallback profile it cannot run.
func (c *remote) runnable(profiles []string) []string {
	client, ok := c.d.Fleet.Client(c.node.ID)
	if !ok || client.Capabilities == nil {
		return profiles
	}
	return slices.DeleteFunc(slices.Clone(profiles), func(name string) bool { return !client.CanRun(name) })
}

// Run offers the turn to the node's client and waits for it to end.
//
// It ends when the client returns its result and, for a turn that writes
// files, its view. When the client's lease ends first, it waits a bounded time
// for the view the client returns, captures it when it comes, and ends the turn
// as interrupted, which starts the service's interrupted-turn recovery from
// those files. Whatever a client sends after the turn has ended is not used and
// is answered superseded.
func (c *remote) Run(ctx context.Context, t isolation.ClientTurn) (isolation.ClientRun, error) {
	d, scope := c.d, t.Prepared.Scope
	token := t.Endpoint.Token
	if token == "" {
		token = randomToken()
	}
	rec := turnRecord{Turn: scope.Turn, Project: scope.Project, Workstream: scope.Workstream, Unit: scope.Unit, Thread: scope.Thread, Role: scope.Role,
		Node: c.node.ID, Name: c.node.Name, TokenSum: filetransport.TokenSum(token), ReturnView: t.ReturnView, Directory: t.Prepared.SessionDirectory, At: d.Now().UTC(),
		Capabilities: t.Capabilities, Skills: len(t.SkillRefs) != 0}
	if err := d.saveRecord(rec); err != nil {
		return isolation.ClientRun{}, err
	}
	rec.Agent = c.agent(scope)
	if err := d.saveRecord(rec); err != nil {
		return isolation.ClientRun{}, err
	}
	st := newState(d, rec)
	d.mu.Lock()
	d.states[scope.Turn] = st
	d.mu.Unlock()
	defer st.end()

	transfer := filetransport.Turn{Token: token}
	if t.View != nil {
		transfer.Download = t.View.Workspace().Directory
	} else if t.Overlay != nil {
		transfer.Download = t.Overlay.Workspace().Directory
	}
	if t.ReturnView {
		transfer.Upload = d.upload
	}
	st.keep(d.Transfers.Register(scope.Turn, transfer))
	if t.Endpoint.URL != "" {
		release, err := d.Relay.Register(scope.Turn, t.Endpoint.URL, token)
		if err != nil {
			return isolation.ClientRun{}, err
		}
		st.keep(release)
	}

	offerCtx, cancel := context.WithTimeout(ctx, d.OfferWait)
	claim, err := d.Fleet.Offer(offerCtx, c.node.ID, c.offer(t, token))
	cancel()
	switch {
	case err != nil && ctx.Err() == nil:
		return isolation.ClientRun{}, fmt.Errorf("client %s did not take the turn: %w", c.node.Name, err)
	case err != nil:
		return isolation.ClientRun{}, ctx.Err()
	case !claim.Accepted:
		return isolation.ClientRun{}, fmt.Errorf("client %s declined the turn: %s", c.node.Name, claim.Reason)
	}
	started := d.Now().UTC()
	st.begin(d.Activity.Start("client-turn-"+scope.Turn, api.ActivityClientTurn, config.ProjectID(scope.Project), config.WorkstreamID(scope.Workstream), "running on "+st.name()))
	st.recordEvent(ctx, trace.ClientPlaced, "", c.attempt(scope), api.ViewAccepted)

	cancelled := false
	select {
	case <-st.resultReady:
		st.waitView(ctx, d.ViewWait)
	case <-st.lost:
		st.waitView(ctx, d.LateWait)
	case <-ctx.Done():
		cancelled = true
		_ = d.Fleet.Cancel(c.node.ID, scope.Turn, "the turn was cancelled")
		st.waitView(context.WithoutCancel(ctx), d.ViewWait)
	}

	st.mu.Lock()
	st.finished = true
	res, dir := st.result, st.viewDir
	st.mu.Unlock()

	run := isolation.ClientRun{}
	var runErr error
	if dir != "" {
		view, err := isolation.AdoptView(dir, coreadapter.ReadWrite)
		if err != nil {
			return run, err
		}
		run.View = view
	}
	if res != nil {
		run.Result = res.Result
		if t.ReturnView && run.View == nil {
			run.Result.IsError, run.Result.ErrorSubtype = true, "view_missing"
			return run, fmt.Errorf("client %s returned its result without the view of its files", st.name())
		}
		return run, nil
	}
	run.Result = coreadapter.SessionResult{StartedAt: started, Cancelled: true, IsError: true, ErrorSubtype: "interrupted"}
	switch {
	case cancelled:
		run.Result.ErrorSubtype = "cancelled"
		runErr = ctx.Err()
	default:
		runErr = fmt.Errorf("%w: %s: %w", ErrClientLost, st.name(), context.Canceled)
	}
	return run, runErr
}

func (c *remote) agent(scope coreadapter.Scope) string {
	b, ok := c.d.binding(config.ProjectID(scope.Project))
	if !ok || b.Repository == nil {
		return scope.Thread
	}
	threads, err := b.Repository.Threads(config.WorkstreamID(scope.Workstream))
	if err != nil {
		return scope.Thread
	}
	for _, th := range threads {
		if th.Identity.ThreadID == scope.Thread {
			return th.Identity.ID
		}
	}
	return scope.Thread
}

func (c *remote) attempt(scope coreadapter.Scope) int {
	b, ok := c.d.binding(config.ProjectID(scope.Project))
	if !ok || b.Repository == nil {
		return 1
	}
	th, err := b.Repository.Thread(config.WorkstreamID(scope.Workstream), c.agent(scope))
	if err != nil {
		return 1
	}
	for _, q := range th.Turns {
		if q.Request.TurnID == scope.Turn {
			return max(len(q.Attempts), 1)
		}
	}
	return 1
}
