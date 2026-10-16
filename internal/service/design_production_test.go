package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kpenfound/busybees/core/agent"
	"github.com/kpenfound/osmia/internal/bundle"
	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/isolation"
	"github.com/kpenfound/osmia/internal/plan"
	"github.com/kpenfound/osmia/internal/questions"
	"github.com/kpenfound/osmia/internal/scheduler"
	"github.com/kpenfound/osmia/internal/seal"
	"github.com/kpenfound/osmia/internal/status"
	"github.com/kpenfound/osmia/internal/thread"
	"github.com/kpenfound/osmia/internal/trace"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const productionBrief = "Decisions: keep the acknowledged offset contract stable.\nIntent and tradeoffs: implement chunk storage in the server and resumption in the client, changing both repos.\nRejected options: client-only retries.\n"

type designInterruption struct {
	step     string
	nth      int32
	count    atomic.Int32
	lifetime context.Context
	reached  chan struct{}
}

type productionDesignFixture struct {
	*crossRepoRatifyFixture
	origin        config.WorkstreamID
	before        trace.Thread
	handed        trace.Document
	clock         *fixedClock
	hook          atomic.Pointer[designInterruption]
	repos         []productionDesignRepo
	owner         ConversationEntry
	ownerAddress  config.WorkstreamID
	dispatch      *productionChiefDispatch
	configuration productionDesignConfiguration
}

type productionChiefDispatch struct {
	mu                         sync.Mutex
	bindings                   int
	prepared                   thread.TurnInput
	prepareError               string
	scope                      coreadapter.Scope
	selectionError, toolsError string
	engineName, handler        string
}

func productionWorker(scope coreadapter.Scope) bool {
	return scope.Role == masonRole && slices.Contains([]string{"worker_first", "worker_second", "worker_unaffected"}, scope.Thread)
}

type productionWorkerWorkspaces struct{ production coreadapter.Workspaces }

func (w productionWorkerWorkspaces) Acquire(ctx context.Context, req coreadapter.WorkspaceRequest) (coreadapter.WorkspaceLease, error) {
	if productionWorker(req.Scope) {
		return stagedWorkspaces{}.Acquire(ctx, req)
	}
	return w.production.Acquire(ctx, req)
}

type productionDesignRepo struct {
	id                            config.ProjectID
	clone, upstream, remote, main string
}

type productionDesignConfiguration struct {
	options    func(*Options)
	wait       func(*testing.T, *productionDesignFixture, string, func() bool)
	checkpoint func(context.Context, string) error
	handIn     bool
}

// newProductionDesignFixture prepares repos and injected engines only. The
// owner endpoint and production architect tools create all feature members,
// handed inputs and draft documents after the real design exchange.
func newProductionDesignFixture(t *testing.T) *productionDesignFixture {
	t.Helper()
	return newConfiguredProductionDesignFixture(t, productionDesignConfiguration{})
}

func newConfiguredProductionDesignFixture(t *testing.T, configuration productionDesignConfiguration) *productionDesignFixture {
	t.Helper()
	ctx := context.Background()
	opts, clone, engine, sessions, _ := newArchitectOptions(t)
	file, err := os.OpenFile(filepath.Join(opts.Config.Root, "config.toml"), os.O_APPEND|os.O_WRONLY, 0600)
	must(t, err)
	_, err = file.WriteString("[roles.chief_of_staff]\nsandbox = \"container\"\nimage = \"fixture-image\"\n[roles.committee]\nsandbox = \"container\"\nimage = \"fixture-image\"\n[capacity]\ncommittee = 2\n[shed]\nmax_rounds = 1\n")
	must(t, errors.Join(err, file.Close()))
	clock := &fixedClock{now: demoStart}
	opts.Reconciliation.Now = clock.Now
	enforced := Enforce(opts, Enforcement{Engine: engine, Hosts: opts.Architect.Hosts})
	opts = enforced
	opts.Librarian = nil
	if configuration.options != nil {
		configuration.options(&opts)
	}
	production := opts.Threads
	dispatch := &productionChiefDispatch{}
	opts.Threads = func(r *trace.Repository, cfg *config.Config) (coreadapter.Reconciler, error) {
		bound, err := production(r, cfg)
		if err != nil {
			return nil, err
		}
		d := bound.(thread.Dispatcher)
		if _, shared := trace.WorkgroupProjectID(r.Project()); shared {
			dispatch.mu.Lock()
			dispatch.bindings++
			dispatch.mu.Unlock()
			prepare := d.Prepare
			d.Prepare = func(ctx context.Context, in thread.TurnInput) (coreadapter.PreparedTurn, error) {
				prepared, err := prepare(ctx, in)
				dispatch.mu.Lock()
				dispatch.prepared, dispatch.prepareError = in, fmt.Sprint(err)
				dispatch.mu.Unlock()
				return prepared, err
			}
		}
		turns := d.Runner.Turns.(*questions.Turns).Turns.(*verdictTurns).Turns.(*reportingTurns).Turns.(*retriedTurns).Turns.(*isolation.Turns)
		originalSelect, originalScoped := turns.Select, turns.Scoped
		originalCapture := turns.Capture
		turns.Workspaces = productionWorkerWorkspaces{production: turns.Workspaces}
		turns.Capture = func(ctx context.Context, scope coreadapter.Scope, view *isolation.FileView, result coreadapter.SessionResult) error {
			if productionWorker(scope) {
				return nil
			}
			return originalCapture(ctx, scope, view, result)
		}
		turns.Select = func(ctx context.Context, scope coreadapter.Scope) (isolation.Selection, error) {
			if !productionWorker(scope) {
				selection, err := originalSelect(ctx, scope)
				if scope.Role == trace.ChiefOfStaff {
					dispatch.mu.Lock()
					dispatch.scope, dispatch.selectionError = scope, fmt.Sprint(err)
					dispatch.mu.Unlock()
				}
				return selection, err
			}
			directory := filepath.Join(cfg.Root.String(), "workers", scope.Thread)
			return isolation.Selection{Workspace: coreadapter.WorkspaceRequest{SourceDirectory: directory, Directory: directory}, Narrow: &coreadapter.Capabilities{Tools: []string{questions.AskTool}}, Execution: coreadapter.ExecutionSettings{Mode: "container", Image: "fixture-image"}}, os.MkdirAll(directory, 0700)
		}
		turns.Scoped = func(ctx context.Context, scope coreadapter.Scope) ([]coreadapter.Tool, error) {
			if !productionWorker(scope) {
				tools, err := originalScoped(ctx, scope)
				if scope.Role == trace.ChiefOfStaff {
					dispatch.mu.Lock()
					dispatch.toolsError = fmt.Sprint(err)
					dispatch.mu.Unlock()
				}
				return tools, err
			}
			return questions.Tools(r, scope.Thread, scope, clock.Now)
		}
		return d, nil
	}
	af := &architectFixture{opts: opts, clone: clone, engine: engine, sessions: sessions}
	f := &productionDesignFixture{crossRepoRatifyFixture: &crossRepoRatifyFixture{architectFixture: af}, clock: clock, dispatch: dispatch, configuration: configuration}
	af.opts.boundary = func(lifetime context.Context, step string) error {
		hook := f.hook.Load()
		if hook != nil && hook.step == step && hook.count.Add(1) == hook.nth {
			hook.lifetime = lifetime
			close(hook.reached)
			<-lifetime.Done()
			return lifetime.Err()
		}
		if configuration.checkpoint != nil {
			return configuration.checkpoint(lifetime, step)
		}
		return nil
	}
	if configuration.wait != nil {
		t.Cleanup(func() {
			if f.s != nil {
				f.stop(t)
			}
		})
	}
	f.start(t)
	added, err := f.c.AddProject(ctx, request(clone))
	must(t, err)
	f.project, f.trace = added.Project.ID, added.Project.Trace
	must(t, os.WriteFile(added.Project.Charter, []byte(shedCharter), 0600))
	main := f.upstream(t)
	f.repos = append(f.repos, productionDesignRepo{f.project, clone, "dagger/dagger", filepath.Join(filepath.Dir(clone), "remotes", "dagger", "dagger.git"), main})
	home := filepath.Dir(clone)
	second := filepath.Join(home, "client")
	must(t, os.MkdirAll(filepath.Join(second, "internal", "trace"), 0700))
	demoGit(t, home, "init", "--quiet", second)
	must(t, os.WriteFile(filepath.Join(second, "internal", "trace", "git.go"), []byte("package trace\n"), 0600))
	demoGit(t, home, "-C", second, "add", "internal")
	demoGit(t, home, "-C", second, "-c", "user.name=Owner", "-c", "user.email=owner@example.invalid", "commit", "-qm", "base")
	demoGit(t, home, "-C", second, "branch", "-M", "main")
	bare := filepath.Join(home, "remotes", "owner", "client.git")
	must(t, os.MkdirAll(filepath.Dir(bare), 0700))
	demoGit(t, home, "clone", "--quiet", "--bare", "--no-hardlinks", second, bare)
	demoGit(t, home, "-C", second, "remote", "add", "upstream", bare)
	other, err := f.c.AddRepo(ctx, RepoAddRequest{Project: f.project, Upstream: "owner/client", Clone: second})
	must(t, err)
	for _, repo := range other.Project.Repos {
		if repo.Clone == second {
			f.project2 = repo.ID
		}
	}
	if f.project2 == "" {
		t.Fatal("second repo was not registered")
	}
	f.repos = append(f.repos, productionDesignRepo{f.project2, second, "owner/client", bare, strings.TrimSpace(demoGit(t, home, "-C", bare, "rev-parse", "refs/heads/main"))})
	f.checkRepos(t)
	if configuration.handIn {
		f.origin = HandInWorkstream(f.project, "production-handin")
	} else {
		opened, err := f.c.OpenDesign(ctx, f.project)
		must(t, err)
		f.origin = opened.Design.Workstream
	}
	f.stream1 = HandInWorkstream(f.project, "design-"+string(f.origin))
	f.stream2 = HandInWorkstream(f.project2, "design-"+string(f.origin))
	f.scriptDrafts(t)
	for i := 1; i <= 2; i++ {
		turn := roundTurnID(1, committeeAgent(i), 1)
		engine.mu.Lock()
		engine.turns[turn] = func(ctx context.Context, req agent.Request, _ *agent.Turn, tools *mcp.ClientSession) (*agent.Result, error) {
			if f.stream1 == f.origin {
				return questionResult(req, "session-committee-"+req.Name, "The single-repo draft fits the charter."), nil
			}
			context, err := readTool(ctx, tools, "context.md")
			if err != nil {
				return nil, err
			}
			if !strings.Contains(context, "Draft project spec") || !strings.Contains(context, "acknowledgment") || !strings.Contains(context, string(f.stream1)) || !strings.Contains(context, string(f.stream2)) {
				return nil, errors.New("committee did not receive the shared draft, contracts and graph")
			}
			return questionResult(req, "session-committee-"+req.Name, "The proposed contract and slice fit the charter."), nil
		}
		engine.mu.Unlock()
	}
	af.script(replyTurnID(1, 1), nil, nil)
	engine.mu.Lock()
	engine.turns["select-repos-1"] = func(ctx context.Context, req agent.Request, verified *agent.Turn, tools *mcp.ClientSession) (*agent.Result, error) {
		if verified.VCS || req.Grants.VCS || req.Profile.VCSAccess || req.Workspace.VCS() != nil || len(req.VCSEnv) != 0 {
			return nil, errors.New("architect has mutation or VCS authority")
		}
		_, err := callTool(ctx, tools, affectedReposTool, map[string]any{"repos": []string{string(f.project), string(f.project2)}})
		return questionResult(req, "session-selection", "Both repos are affected."), err
	}
	engine.turns["*"] = func(ctx context.Context, req agent.Request, _ *agent.Turn, tools *mcp.ClientSession) (*agent.Result, error) {
		names, err := toolNames(ctx, tools)
		if err != nil {
			return nil, err
		}
		if slices.Contains(names, draftBriefToolName) {
			_, err = callTool(ctx, tools, draftBriefToolName, map[string]any{"content": productionBrief})
			return questionResult(req, "session-design", productionBrief), err
		}
		return questionResult(req, "session-chief", "The service is following the agreed design."), nil
	}
	engine.mu.Unlock()
	if configuration.handIn {
		return f
	}
	_, err = f.c.DesignMessage(ctx, f.origin, "Build resumable uploads in both the server and client repos.")
	must(t, err)
	f.wait(t, "design reply", func() bool {
		conversation, err := f.c.DesignConversation(ctx, f.origin)
		must(t, err)
		return len(conversation.Entries) == 2 && conversation.Entries[1].State == TurnDone
	})
	repo, err := f.s.repository(f.project)
	must(t, err)
	f.before, err = repo.ChiefOfStaffThread(f.origin)
	must(t, err)
	return f
}

func (f *productionDesignFixture) start(t *testing.T) {
	t.Helper()
	f.architectFixture.start(t)
	f.checkRepos(t)
}

func (f *productionDesignFixture) checkRepos(t *testing.T) {
	t.Helper()
	ctx := context.Background()
	for _, expected := range f.repos {
		cfg := f.s.current().For(expected.id)
		if cfg.Project.Clone != expected.clone || cfg.Project.Upstream != expected.upstream || cfg.Project.BaseBranch != "main" {
			t.Fatalf("repo %s config changed: %+v", expected.id, cfg.Project)
		}
		provider := workspaces(cfg, branchesDirectory, config.WorkspacesGit)
		remote, err := provider.Remote(ctx, cfg.Project.Upstream)
		must(t, err)
		if remote != "upstream" {
			t.Fatalf("repo %s resolved remote %q, want upstream", expected.id, remote)
		}
		url := strings.TrimSpace(demoGit(t, filepath.Dir(expected.clone), "-C", expected.clone, "remote", "get-url", remote))
		if url != expected.remote {
			t.Fatalf("repo %s remote URL %q, want %q", expected.id, url, expected.remote)
		}
		commit, found, err := provider.RemoteBranch(ctx, remote, cfg.Project.BaseBranch)
		must(t, err)
		if !found || commit != expected.main {
			t.Fatalf("repo %s upstream main=%q exists=%v, want %q", expected.id, commit, found, expected.main)
		}
	}
}

func (f *productionDesignFixture) wait(t *testing.T, phase string, done func() bool) {
	t.Helper()
	if f.configuration.wait != nil {
		f.configuration.wait(t, f, phase, done)
		return
	}
	deadline := time.Now().Add(demoTimeout)
	for !done() {
		if time.Now().After(deadline) {
			f.timeout(t, phase)
		}
		f.clock.Advance(time.Second)
		time.Sleep(20 * time.Millisecond)
	}
}

func (f *productionDesignFixture) timeout(t *testing.T, phase string) {
	t.Helper()
	t.Logf("phase=%s design=%s members=%s/%s now=%v ready=%v lifetime=%v engine-runs=%v", phase, f.origin, f.stream1, f.stream2, f.clock.Now(), f.s.ready.Load(), f.s.lifetime.Err(), f.runs())
	if hook := f.hook.Load(); hook != nil {
		t.Logf("injection step=%s hits=%d target=%d", hook.step, hook.count.Load(), hook.nth)
	}
	select {
	case <-f.s.done:
		t.Logf("service terminated: %v", f.s.err)
	default:
		t.Log("service terminal error: none recorded; shutdown has not completed")
	}
	for i, member := range []config.WorkstreamID{f.origin, f.stream1, f.stream2} {
		if member == "" || (i == 1 && member == f.origin) {
			continue
		}
		project := f.project
		if i == 2 {
			project = f.project2
		}
		repo, err := f.s.repository(project)
		if err != nil {
			t.Logf("project=%s member=%s repository error=%v", project, member, err)
			continue
		}
		states, stateErr := repo.WorkflowStates(member)
		prepared, _, sealed, sealErr := seal.PreparedLatest(repo, member)
		t.Logf("project=%s member=%s states=%+v state-error=%v prepared=%v seal=%+v seal-error=%v", project, member, states, stateErr, sealed, prepared, sealErr)
		approval, found, approvalErr := repo.GroupApprovalOf(member)
		t.Logf("member=%s group=%s decision=%v complete=%v slices=%d graph=%+v group-error=%v", member, approval.Workgroup, found, approval.Complete, len(approval.ProjectSpec.Slices), approval.ProjectSpec.Graph, approvalErr)
		for _, pin := range approval.Members {
			t.Logf("pinned project=%s member=%s spec=%d plan=%d round=%d", pin.Project, pin.Workstream, pin.Spec, pin.Plan, pin.Round)
		}
		ops, err := repo.Operations(member)
		if err != nil {
			t.Logf("member=%s operations error=%v", member, err)
		}
		for _, op := range ops {
			failure := ""
			for _, h := range op.History {
				if h.Failure != "" {
					failure = h.Failure
				}
			}
			t.Logf("member=%s operation=%s action=%s input=%s result=%+v retry=%v acknowledged=%v failure=%s", member, op.Operation.ID, op.Operation.Action, op.Operation.Input, op.Result, op.RetryAt, op.Acknowledged, failure)
		}
		f.logTurns(t, repo, member)
		docs, err := trace.Read[trace.Document](repo, member)
		if err != nil {
			t.Logf("member=%s documents error=%v", member, err)
		}
		for _, doc := range docs {
			if strings.HasPrefix(doc.Path, "tools/") {
				t.Logf("member=%s tool=%s revision=%d cause=%s", member, doc.Path, doc.Revision, doc.Cause)
			}
		}
		if i == 1 {
			chief, host, _, close, err := f.s.chiefRepository(context.Background(), repo, member)
			if err == nil {
				f.logTurns(t, chief, host)
				close()
			} else {
				t.Logf("shared chief error=%v", err)
			}
		}
	}
	f.ownerSummary(t)
	t.Fatalf("production design workflow timed out in %s", phase)
}

// ownerSummary follows the broad diagnostics so bounded check output retains
// the accepted request and the dispatch evidence nearest to the failure.
func (f *productionDesignFixture) ownerSummary(t *testing.T) {
	t.Helper()
	if f.owner.Turn == "" {
		return
	}
	sent := f.owner
	terminal := "running"
	select {
	case <-f.s.done:
		terminal = fmt.Sprint(f.s.err)
	default:
	}
	t.Logf("OWNER turn=%s address=%s design=%s members=%s/%s accepted=%v now=%v ready=%v lifetime=%v terminal=%s", sent.Turn, f.ownerAddress, f.origin, f.stream1, f.stream2, sent.At, f.clock.Now(), f.s.ready.Load(), f.s.lifetime.Err(), terminal)
	f.dispatch.mu.Lock()
	t.Logf("OWNER dispatch-bindings=%d prepared=%+v preparation-error=%s scope=%+v selection-error=%s tools-error=%s engine-name=%s handler=%s", f.dispatch.bindings, f.dispatch.prepared, f.dispatch.prepareError, f.dispatch.scope, f.dispatch.selectionError, f.dispatch.toolsError, f.dispatch.engineName, f.dispatch.handler)
	f.dispatch.mu.Unlock()
	f.engine.mu.Lock()
	engineRan := slices.Contains(f.engine.runs, sent.Turn)
	handler := "fallback"
	if f.engine.turns[sent.Turn] != nil {
		handler = "named"
	}
	f.engine.mu.Unlock()
	t.Logf("OWNER sent-engine-ran=%v selected-handler=%s", engineRan, handler)
	repo, err := f.s.repository(f.project)
	if err != nil {
		t.Logf("OWNER source error=%v", err)
		return
	}
	chief, host, _, release, err := f.s.chiefRepository(context.Background(), repo, f.origin)
	if err != nil {
		t.Logf("OWNER shared error=%v", err)
		return
	}
	defer release()
	id, err := repo.CrossRepoWorkgroupOf(f.origin)
	if err != nil {
		t.Logf("OWNER group error=%v", err)
		return
	}
	_, group, _, err := repo.CrossRepoProjectSpec(id)
	if err != nil {
		t.Logf("OWNER group read error=%v", err)
		return
	}
	leader := leastWorkstream(group.Members[1:])
	for i, address := range []config.WorkstreamID{f.origin, f.stream1, f.stream2} {
		project := f.project
		if i == 2 {
			project = f.project2
		}
		member, err := f.s.repository(project)
		if err != nil {
			t.Logf("OWNER address=%s project=%s error=%v", address, project, err)
			continue
		}
		target, routedHost, _, closeTarget, err := f.s.chiefRepository(context.Background(), member, address)
		if err != nil {
			t.Logf("OWNER address=%s route error=%v", address, err)
			continue
		}
		t.Logf("OWNER address=%s project=%s shared-project=%s host=%s shared-handle=%v leader=%s", address, project, target.Project(), routedHost, target == chief && routedHost == host, leader)
		closeTarget()
		if address == leader {
			streams, err := member.Workstreams()
			t.Logf("OWNER leader-project=%s stream-order=%v error=%v", project, streams, err)
		}
		ops, err := member.Operations(address)
		if err != nil {
			t.Logf("OWNER address=%s operation-error=%v", address, err)
		}
		for _, op := range ops {
			if !op.Acknowledged && op.RetryAt.After(f.clock.Now()) {
				t.Logf("OWNER pending-retry member=%s operation=%s action=%s retry=%v", address, op.Operation.ID, op.Operation.Action, op.RetryAt)
			}
		}
	}
	th, err := chief.Thread(host, trace.ChiefOfStaff)
	if err != nil {
		t.Logf("OWNER thread error=%v", err)
		return
	}
	q, ready := nextReadyTurn(th)
	t.Logf("OWNER shared-project=%s host=%s identity-project=%s identity-thread=%s status=%s active=%s parked=%v oldest=%s ready=%v", chief.Project(), host, th.Identity.Project, th.Identity.ThreadID, th.Status, th.Active, th.Parked(), q.Request.TurnID, ready)
	for _, turn := range th.Turns {
		if turn.Request.TurnID != sent.Turn && turn.Request.TurnID != q.Request.TurnID {
			continue
		}
		failure, response := "", ""
		if turn.Response != nil {
			failure, response = turn.Response.Failure, turn.Response.ID
		}
		t.Logf("OWNER turn=%s request=%s revision=%d project=%s workstream=%s actor=%+v text-matches=%v status=%s claim=%+v response=%s completed=%v failure=%s", turn.Request.TurnID, turn.Request.ID, turn.Request.Revision, turn.Request.Project, turn.Request.Workstream, turn.Request.Actor, turn.Request.Prompt == sent.Text, turn.Status(), turn.Claim, response, turn.CompletedAt, failure)
	}
	op, err := thread.TurnOperation(chief.Project(), "workgroup-chief-"+sent.Turn, thread.TurnInput{Workstream: host, Agent: trace.ChiefOfStaff, Turn: sent.Turn})
	if err != nil {
		t.Logf("OWNER operation error=%v", err)
		return
	}
	observation, err := (thread.Dispatcher{Runner: thread.Runner{Store: chief}}).Inspect(context.Background(), op)
	t.Logf("OWNER dispatch=%s observation=%+v error=%v; direct dispatch has no retry timer", op.ID, observation, err)
}

func (f *productionDesignFixture) recordEngine(req agent.Request, handler string) {
	f.dispatch.mu.Lock()
	f.dispatch.engineName, f.dispatch.handler = req.Name, handler
	f.dispatch.mu.Unlock()
}

func (f *productionDesignFixture) logTurns(t *testing.T, repo *trace.Repository, member config.WorkstreamID) {
	t.Helper()
	threads, err := repo.Threads(member)
	if err != nil {
		t.Logf("member=%s threads error=%v", member, err)
	}
	for _, thread := range threads {
		t.Logf("project=%s member=%s thread=%s role=%s status=%s active=%s parked=%v", repo.Project(), member, thread.Identity.ThreadID, thread.Identity.Role, thread.Status, thread.Active, thread.Parked())
		for _, turn := range thread.Turns {
			failure := ""
			if turn.Response != nil {
				failure = turn.Response.Failure
			}
			t.Logf("member=%s thread=%s sequence=%d turn=%s request=%s revision=%d claim=%+v status=%s completed=%v response=%v failure=%s", member, thread.Identity.ThreadID, turn.Sequence, turn.Request.TurnID, turn.Request.ID, turn.Request.Revision, turn.Claim, turn.Status(), turn.CompletedAt, turn.Response != nil, failure)
			for _, attempt := range turn.Attempts {
				t.Logf("turn=%s attempt=%d at=%v path=%s replay-from=%d omitted=%d failure=%s", turn.Request.TurnID, attempt.Number, attempt.At, attempt.Path, attempt.ReplayFrom, attempt.Omitted, attempt.Failure)
			}
		}
	}
}

// awaitOwnerTurnRunning waits for the injected engine, while the live service
// alone dispatches the durable request. Future retry deadlines advance the
// fixed clock only when an unfinished operation actually records one.
func (f *productionDesignFixture) awaitOwnerTurnRunning(t *testing.T, sent ConversationEntry, reached <-chan agent.Request) {
	t.Helper()
	wait, closeWait := serviceChanges(t, f.s)
	defer closeWait()
	repo, err := f.s.repository(f.project)
	must(t, err)
	chief, host, _, closeChief, err := f.s.chiefRepository(context.Background(), repo, f.origin)
	must(t, err)
	defer closeChief()
	type memberTrace struct {
		repo   *trace.Repository
		stream config.WorkstreamID
	}
	var members []memberTrace
	for i, address := range []config.WorkstreamID{f.origin, f.stream1, f.stream2} {
		project := f.project
		if i == 2 {
			project = f.project2
		}
		member, err := f.s.repository(project)
		must(t, err)
		members = append(members, memberTrace{member, address})
		target, routedHost, _, release, err := f.s.chiefRepository(context.Background(), member, address)
		must(t, err)
		same := target == chief && routedHost == host
		t.Logf("owner address=%s project=%s authoritative-project=%s host=%s shared-handle=%v", address, project, target.Project(), routedHost, same)
		release()
		if !same {
			t.Fatal("continued owner address and members do not share the authoritative chief")
		}
	}
	diagnose := func() {
		t.Helper()
		th, err := chief.Thread(host, trace.ChiefOfStaff)
		if err != nil {
			t.Logf("owner turn=%s shared thread error=%v", sent.Turn, err)
			return
		}
		q, ready := nextReadyTurn(th)
		t.Logf("owner turn=%s now=%v shared-project=%s host=%s thread=%s status=%s active=%s parked=%v next=%s ready=%v", sent.Turn, f.clock.Now(), chief.Project(), host, th.Identity.ThreadID, th.Status, th.Active, th.Parked(), q.Request.TurnID, ready)
		for _, turn := range th.Turns {
			if turn.Request.TurnID == sent.Turn {
				t.Logf("sent request=%s revision=%d project=%s host=%s actor=%+v accepted=%v prompt-matches=%v claim=%+v completed=%v", turn.Request.ID, turn.Request.Revision, turn.Request.Project, turn.Request.Workstream, turn.Request.Actor, turn.Request.At, turn.Request.Prompt == sent.Text, turn.Claim, turn.CompletedAt)
				if response := turn.Response; response != nil {
					t.Logf("sent turn=%s response=%s request-link=%s revision-link=%d failure=%s", sent.Turn, response.ID, response.RequestID, response.RequestRevision, response.Failure)
				}
			}
		}
		op, err := thread.TurnOperation(chief.Project(), "workgroup-chief-"+sent.Turn, thread.TurnInput{Workstream: host, Agent: trace.ChiefOfStaff, Turn: sent.Turn})
		must(t, err)
		observed, err := (thread.Dispatcher{Runner: thread.Runner{Store: chief}}).Inspect(context.Background(), op)
		t.Logf("owner dispatch operation=%s observation=%+v error=%v; shared chief dispatch applies directly without an operation retry timer", op.ID, observed, err)
		f.logTurns(t, chief, host)
	}
	diagnose()
	deadline := time.Now().Add(demoTimeout)
	for {
		select {
		case req := <-reached:
			t.Logf("injected engine handler=fallback name=%s workspace=%s session=%s now=%v", req.Name, req.Workspace.Directory(), req.SessionDir, f.clock.Now())
			if req.Name != sent.Turn || !strings.Contains(req.Prompt, sent.Text) {
				t.Fatalf("engine matched a different request: name=%s sent=%s", req.Name, sent.Turn)
			}
			return
		default:
		}
		if time.Now().After(deadline) {
			diagnose()
			f.timeout(t, "continued owner turn engine entry")
		}
		now := f.clock.Now()
		var next trace.OperationRecord
		var due memberTrace
		for _, member := range members {
			ops, err := member.repo.Operations(member.stream)
			must(t, err)
			for _, op := range ops {
				if !op.Acknowledged && op.RetryAt.After(now) && (next.RetryAt.IsZero() || op.RetryAt.Before(next.RetryAt)) {
					next, due = op, member
				}
			}
		}
		if !next.RetryAt.IsZero() {
			t.Logf("owner turn=%s advances injected now=%v to retry=%v project=%s member=%s operation=%s action=%s observation=%+v", sent.Turn, now, next.RetryAt, due.repo.Project(), due.stream, next.Operation.ID, next.Operation.Action, next.Observation)
			f.clock.Advance(next.RetryAt.Sub(now))
		}
		wait()
	}
}

func (f *productionDesignFixture) startOwner(t *testing.T) {
	t.Helper()
	_, err := f.c.DesignStart(context.Background(), f.origin)
	must(t, err)
	repo, err := f.s.repository(f.project)
	must(t, err)
	f.handed, err = trace.Get[trace.Document](repo, f.origin, handedDocument, 1)
	must(t, err)
}

func (f *productionDesignFixture) awaitPacket(t *testing.T) PacketResponse {
	t.Helper()
	var packet PacketResponse
	f.wait(t, "ratification packet", func() bool {
		a, err := f.c.Packet(context.Background(), f.origin)
		if err != nil || a.ProjectSpec == nil {
			return false
		}
		if _, err := f.c.Packet(context.Background(), f.stream2); err != nil {
			return false
		}
		packet = a
		return true
	})
	for i, member := range []config.WorkstreamID{f.stream1, f.stream2} {
		project := f.project
		if i == 1 {
			project = f.project2
		}
		repo, err := f.s.repository(project)
		must(t, err)
		for n := 1; n <= 2; n++ {
			th, err := repo.Thread(member, committeeAgent(n))
			must(t, err)
			if len(th.Turns) != 1 {
				t.Fatalf("member %s committee %d has %d turns", member, n, len(th.Turns))
			}
			turn := th.Turns[0]
			if turn.Response == nil || turn.Response.Failure != "" || turn.CompletedAt.IsZero() || turn.Status() != "idle" || turn.Response.Result.FinalResponse != "The proposed contract and slice fit the charter." {
				failure, reply := "no response", ""
				if turn.Response != nil {
					failure, reply = turn.Response.Failure, turn.Response.Result.FinalResponse
				}
				t.Fatalf("member %s committee %d did not review its actual project context: turn=%s request=%s status=%s completed=%v attempts=%d failure=%s reply=%s", member, n, turn.Request.TurnID, turn.Request.ID, turn.Status(), turn.CompletedAt, len(turn.Attempts), failure, reply)
			}
		}
	}
	return packet
}

func (f *productionDesignFixture) checkContinuation(t *testing.T) {
	t.Helper()
	ctx := context.Background()
	for i, m := range []config.WorkstreamID{f.stream1, f.stream2} {
		p := f.project
		if i == 1 {
			p = f.project2
		}
		repo, err := f.s.repository(p)
		must(t, err)
		streams, err := repo.Workstreams()
		must(t, err)
		features := slices.DeleteFunc(slices.Clone(streams), func(stream config.WorkstreamID) bool {
			return stream == librarianWorkstream(p) || stream == f.origin
		})
		if !slices.Equal(features, []config.WorkstreamID{m}) {
			t.Fatalf("repo has lost or duplicated its allocated member: %v", features)
		}
		if f.s.Context().Mode(p) != bundle.ModeFile {
			t.Fatal("production path requires Hearsay")
		}
		handed, err := trace.Get[trace.Document](repo, m, handedDocument, 1)
		must(t, err)
		if handed.Content != f.handed.Content || handed.Source != f.handed.Source {
			t.Fatal("member input differs from the immutable brief")
		}
		cfg := f.s.current().For(p)
		for _, name := range []string{"spec.md", "plan.json", "handed", "tools/design-brief.json", "workgroups", ".osmia"} {
			if _, err := os.Stat(filepath.Join(cfg.Project.Clone, name)); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("factory state leaked into target repo at %s: %v", name, err)
			}
		}
		if _, err := repo.Thread(m, trace.ChiefOfStaff); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("member retains an independent chief: %v", err)
		}
		groups, _, err := trace.Workgroups(repo)
		must(t, err)
		if _, found := trace.WorkgroupOf(groups, m); found {
			t.Fatal("member retains a one-workstream group")
		}
		target, host, _, close, err := f.s.chiefRepository(ctx, repo, m)
		must(t, err)
		chief, err := target.Thread(host, trace.ChiefOfStaff)
		close()
		must(t, err)
		if len(chief.Turns) < len(f.before.Turns) {
			t.Fatal("design turns were lost")
		}
		if !equalJSONTest(f.before.Identity, chief.Identity) || len(f.before.Turns) > 0 && !equalJSONTest(f.before.Turns, chief.Turns[:len(f.before.Turns)]) {
			t.Fatal("design identity, requests, responses or recovery state changed at adoption")
		}
	}
	repo, err := f.s.repository(f.project)
	must(t, err)
	id, err := repo.CrossRepoWorkgroupOf(f.origin)
	must(t, err)
	_, group, _, err := repo.CrossRepoProjectSpec(id)
	must(t, err)
	if group.Conversation == nil || group.Conversation.Workstream != f.origin || group.Conversation.Project != f.project ||
		len(group.Members) != 3 || group.Members[0] != f.origin ||
		!slices.Contains(group.Members[1:], f.stream1) || !slices.Contains(group.Members[1:], f.stream2) {
		t.Fatalf("continued conversation or exact member coverage changed: %+v", group)
	}
	groups, err := os.ReadDir(filepath.Join(f.s.current().Root.String(), "workgroups"))
	must(t, err)
	if len(groups) != 1 || groups[0].Name() != string(id) {
		t.Fatalf("start or recovery duplicated workgroup authority: %v", groups)
	}
	handed, err := trace.Get[trace.Document](repo, f.origin, handedDocument, 1)
	must(t, err)
	if !equalJSONTest(handed, f.handed) {
		t.Fatal("immutable handed brief changed")
	}
	alias, err := f.c.Status(ctx, f.origin)
	must(t, err)
	if alias.Workstream != f.stream1 || alias.Project != f.project {
		t.Fatalf("design status did not resolve to its member: %+v", alias)
	}
	listed, err := f.c.Statuses(ctx)
	must(t, err)
	if slices.ContainsFunc(listed.Workstreams, func(w WorkstreamStatus) bool { return w.Workstream == f.origin }) {
		t.Fatal("status list exposes the adopted design host as independent work")
	}
	if _, err := repo.Thread(f.origin, trace.ChiefOfStaff); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("source retains an authoritative queue")
	}
	historical, err := trace.Read[trace.TurnRequest](repo, f.origin)
	must(t, err)
	if len(f.before.Turns) > 0 && !slices.ContainsFunc(historical, func(req trace.TurnRequest) bool { return req.ID == f.before.Turns[0].Request.ID }) {
		t.Fatal("historical design request was lost")
	}
	chief, _, _, closeChief, err := f.s.chiefRepository(ctx, repo, f.stream1)
	must(t, err)
	defer closeChief()
	matching, err := consistentRead(func() (bool, error) {
		var entries []ConversationEntry
		var feed []FeedEntry
		for i, member := range []config.WorkstreamID{f.origin, f.stream1, f.stream2} {
			conversation, err := f.c.Conversation(ctx, member)
			if err != nil {
				return false, err
			}
			got, api := f.s.feedList(string(member))
			if api != nil {
				return false, apiErrAsError{api}
			}
			shared := slices.DeleteFunc(slices.Clone(got.Entries), func(e FeedEntry) bool {
				return e.Kind != "message" && e.Kind != "response" && e.Kind != "action" && e.Kind != "status"
			})
			if i == 0 {
				entries, feed = conversation.Entries, shared
			} else if !equalJSONTest(entries, conversation.Entries) || !equalJSONTest(feed, shared) {
				return false, nil
			}
		}
		return true, nil
	}, chief.Generation)
	must(t, err)
	if !matching {
		t.Fatal("continued address and member endpoints resolve to different conversations or feeds")
	}
}

func equalJSONTest(a, b any) bool {
	x, _ := json.Marshal(a)
	y, _ := json.Marshal(b)
	return string(x) == string(y)
}

func (f *productionDesignFixture) assertBarrier(t *testing.T) {
	t.Helper()
	alias, err := f.c.Status(context.Background(), f.origin)
	must(t, err)
	if alias.Workstream != f.stream1 || (alias.State != nil && (*alias.State == RatifiedState || *alias.State == BuildingState)) {
		t.Fatalf("design status bypassed pending group authority: %+v", alias)
	}
	for i, m := range []config.WorkstreamID{f.stream1, f.stream2} {
		p := f.project
		if i == 1 {
			p = f.project2
		}
		repo, err := f.s.repository(p)
		must(t, err)
		approved, err := repo.GroupApproved(m)
		must(t, err)
		if approved {
			t.Fatal("incomplete group is approved")
		}
		approval, found, err := repo.GroupApprovalOf(m)
		must(t, err)
		if !found || approval.Complete || len(approval.Members) != 2 || !slices.ContainsFunc(approval.Members, func(pin trace.GroupMemberApproval) bool {
			return pin.Project == p && pin.Workstream == m && pin.Spec == 1 && pin.Plan == 1
		}) {
			t.Fatal("pending group decision lost its exact member pins")
		}
		st, err := f.c.Status(context.Background(), m)
		must(t, err)
		if st.State != nil && (*st.State == RatifiedState || *st.State == BuildingState) {
			t.Fatalf("partial effective member state: %+v", st)
		}
		if _, _, sealed, err := seal.Latest(repo, m); err != nil || sealed {
			t.Fatalf("partial effective seal: %v %v", sealed, err)
		}
		if _, ok, err := (&masons{s: f.s, repository: repo}).read(m); err != nil || ok {
			t.Fatalf("unit admission bypasses approval: %v %v", ok, err)
		}
		held, err := f.s.holds(p, librarianWorkstream(p), repo, scheduler.Candidate{Workstream: m, Thread: trace.Thread{Identity: trace.Agent{Role: masonRole}}, Turn: trace.QueuedTurn{Request: trace.TurnRequest{Header: trace.Header{Unit: "accept"}}}})
		must(t, err)
		if !held {
			t.Fatal("scheduler bypasses group barrier")
		}
		if wait, err := edgeWaiting(repo, m); err != nil || !wait {
			t.Fatalf("contracts released before approval: %v %v", wait, err)
		}
		_, event := buildIDs(1)
		build := coreadapter.Operation{ID: trace.OperationID(p, m, event), Boundary: coreadapter.RepositoryBoundary, Action: BuildAction, Input: json.RawMessage(`{"seal":1}`)}
		result, err := (&builder{s: f.s, repository: repo}).Apply(context.Background(), build)
		must(t, err)
		if result.Outcome != "failed" {
			t.Fatalf("direct build bypassed completion: %+v", result)
		}
		_, event = landIDs("accept", 1)
		input, err := json.Marshal(landInput{Unit: "accept", Review: 1, Candidate: "candidate", Base: "base"})
		must(t, err)
		land := coreadapter.Operation{ID: trace.OperationID(p, m, event), Boundary: coreadapter.RepositoryBoundary, Action: LandAction, Input: input}
		result, err = (&foreman{masons: &masons{s: f.s, repository: repo}}).Apply(context.Background(), land)
		must(t, err)
		if result.Outcome != "failed" || !strings.Contains(result.Evidence, "workgroup seal preparations") {
			t.Fatalf("direct landing bypassed completion: %+v", result)
		}
		ops, err := repo.Operations(m)
		must(t, err)
		if slices.ContainsFunc(ops, func(o trace.OperationRecord) bool { return o.Operation.Action == BuildAction }) {
			t.Fatal("build admitted before group completion")
		}
	}
}

func (f *productionDesignFixture) awaitApproved(t *testing.T) {
	t.Helper()
	f.wait(t, "group approval completion", func() bool {
		repo, err := f.s.repository(f.project)
		must(t, err)
		ok, err := repo.GroupApproved(f.stream1)
		must(t, err)
		return ok
	})
	for i, m := range []config.WorkstreamID{f.stream1, f.stream2} {
		p := f.project
		if i == 1 {
			p = f.project2
		}
		repo, err := f.s.repository(p)
		must(t, err)
		record, _, ok, err := seal.Latest(repo, m)
		must(t, err)
		if !ok || record.Revision.Spec != 1 || record.Revision.Plan != 1 {
			t.Fatalf("wrong pinned seal: %+v", record)
		}
		expected := f.repos[i]
		if record.Base.Remote != "upstream" || record.Base.Branch != "main" || record.Base.Commit != expected.main || record.Branch != featureBranch(m) {
			t.Fatalf("repo %s seal has wrong upstream base or branch: %+v", p, record)
		}
		fetched := strings.TrimSpace(demoGit(t, filepath.Dir(expected.clone), "-C", expected.clone, "rev-parse", "refs/remotes/upstream/main"))
		if fetched != expected.main {
			t.Fatalf("repo %s fetched main=%q, want %q", p, fetched, expected.main)
		}
		docs, err := trace.Read[trace.Document](repo, m)
		must(t, err)
		seals, decisions := 0, 0
		for _, d := range docs {
			if d.ID == seal.DocumentID {
				seals++
			}
			if strings.HasSuffix(d.Path, "/ratification.json") {
				decisions++
			}
		}
		if seals != 1 || decisions != 1 {
			t.Fatalf("duplicate member effects: seals=%d decisions=%d", seals, decisions)
		}
	}
}

func TestDesignProductionStartDraftRatifyAndRestart(t *testing.T) {
	t.Parallel()
	f := newProductionDesignFixture(t)
	defer f.stop(t)
	f.startOwner(t)
	packet := f.awaitPacket(t)
	f.checkContinuation(t)
	if _, err := f.c.Ratify(context.Background(), f.origin, packet.Packet.Revision.Spec, packet.Packet.Revision.Plan); err != nil {
		t.Fatal(err)
	}
	f.awaitApproved(t)
	f.wait(t, "members building", func() bool {
		for i, member := range []config.WorkstreamID{f.stream1, f.stream2} {
			project := f.project
			if i == 1 {
				project = f.project2
			}
			repo, err := f.s.repository(project)
			must(t, err)
			state, err := repo.Workflow(member, trace.FeatureSubject)
			must(t, err)
			if state.Value != BuildingState {
				return false
			}
		}
		return true
	})
	f.stop(t)
	f.start(t)
	f.checkContinuation(t)
	f.awaitApproved(t)
}

func TestDesignProductionStartAndApprovalRecoverAtBoundaries(t *testing.T) {
	for _, step := range []string{"design-member-allocated", "design-workgroup-prepared", "design-member-joined", "design-conversation-imported", "design-conversation-adopted", "group-decision-recorded", "group-member-decision-prepared", "seal-branch-created", "seal-recorded", "group-member-seal-prepared", "group-before-completion"} {
		t.Run(step, func(t *testing.T) {
			t.Parallel()
			f := newProductionDesignFixture(t)
			defer f.stop(t)
			if step == "group-before-completion" {
				// Arm this boundary on a new service to cover startup hook binding.
				f.stop(t)
				f.start(t)
			}
			hook := &designInterruption{step: step, nth: 1, reached: make(chan struct{})}
			f.hook.Store(hook)
			f.startOwner(t)
			var ratifyDone chan error
			if strings.HasPrefix(step, "group-") || strings.HasPrefix(step, "seal-") {
				packet := f.awaitPacket(t)
				ratifyDone = make(chan error, 1)
				go func() {
					_, err := f.c.Ratify(context.Background(), f.stream1, packet.Packet.Revision.Spec, packet.Packet.Revision.Plan)
					ratifyDone <- err
				}()
			}
			select {
			case <-hook.reached:
				t.Logf("reached %s design=%s members=%s/%s repos=%s/%s", step, f.origin, f.stream1, f.stream2, f.project, f.project2)
			case <-time.After(demoTimeout):
				f.timeout(t, "interruption boundary "+step)
			}
			if hook.lifetime != f.s.lifetime || hook.lifetime.Err() != nil {
				t.Fatal("interruption did not bind the running service lifetime")
			}
			if ratifyDone != nil {
				f.assertBarrier(t)
			}
			f.stop(t)
			t.Logf("stopped service at %s", step)
			if !errors.Is(hook.lifetime.Err(), context.Canceled) {
				t.Fatal("service stop did not cancel the intercepted lifetime")
			}
			if ratifyDone != nil {
				<-ratifyDone
			}
			f.hook.Store(nil)
			f.start(t)
			t.Logf("restarted service after %s with injection cleared", step)
			packet := f.awaitPacket(t)
			f.checkContinuation(t)
			if ratifyDone == nil {
				_, err := f.c.Ratify(context.Background(), f.stream1, packet.Packet.Revision.Spec, packet.Packet.Revision.Plan)
				must(t, err)
			}
			f.awaitApproved(t)
			f.checkContinuation(t)
			t.Logf("completed both pinned seals after %s without repeating owner start or accepted ratification", step)
		})
	}
}

func TestDesignProductionRatificationRefusesInvalidPacketBeforeDecisions(t *testing.T) {
	for _, invalid := range []string{"cycle", "missing-slice"} {
		t.Run(invalid, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			f := newProductionDesignFixture(t)
			defer f.stop(t)
			f.startOwner(t)
			packet := f.awaitPacket(t)
			repo, err := f.s.repository(f.project)
			must(t, err)
			id, err := repo.CrossRepoWorkgroupOf(f.stream1)
			must(t, err)
			wt, _, err := f.s.approvalTrace(ctx, repo, id)
			must(t, err)
			spec := *packet.ProjectSpec
			if invalid == "cycle" {
				spec.Graph = append(slices.Clone(spec.Graph), trace.Edge{From: f.stream2, To: f.stream1, Type: trace.EdgeContract})
			} else {
				spec.Slices = slices.DeleteFunc(slices.Clone(spec.Slices), func(slice trace.Slice) bool { return slice.Workstream == f.stream2 })
			}
			// Corrupt only the finished production packet to exercise the owner
			// endpoint's validation independently of architect-tool validation.
			writeCorruptedProjectSpec(t, wt, id, spec)
			must(t, wt.Close())
			if _, err := f.c.Ratify(ctx, f.origin, packet.Packet.Revision.Spec, packet.Packet.Revision.Plan); err == nil {
				t.Fatal("invalid packet received owner approval")
			}
			for i, member := range []config.WorkstreamID{f.stream1, f.stream2} {
				project := f.project
				if i == 1 {
					project = f.project2
				}
				r, err := f.s.repository(project)
				must(t, err)
				if _, found, err := r.GroupApprovalOf(member); err != nil || found {
					t.Fatalf("refusal recorded a group decision: %v %v", found, err)
				}
				if _, found, err := latestRatification(r, member); err != nil || found {
					t.Fatalf("refusal recorded a member decision: %v %v", found, err)
				}
				if _, _, found, err := seal.PreparedLatest(r, member); err != nil || found {
					t.Fatalf("refusal prepared a member seal: %v %v", found, err)
				}
			}
			f.checkContinuation(t)
		})
	}
}

func TestDesignProductionExistingMemberApprovalsRecoverCompletion(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newProductionDesignFixture(t)
	defer f.stop(t)
	f.startOwner(t)
	f.awaitPacket(t)
	// Existing installations can have individual owner decisions without
	// a group receipt. Exercise their reconciliation on production members.
	for _, stream := range []config.WorkstreamID{f.stream1, f.stream2} {
		owner, api := f.s.shedAction(string(stream), InShedState, SketchedState)
		if api != nil {
			t.Fatal(api)
		}
		if _, api := f.s.ratifyMember(ctx, owner, owner.pin); api != nil {
			t.Fatal(api)
		}
	}
	f.awaitApproved(t)
	f.stop(t)
	f.start(t)
	f.awaitApproved(t)
	f.checkContinuation(t)
}

func TestDesignProductionApprovalRecoveryKeepsPinnedDrafts(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newProductionDesignFixture(t)
	defer f.stop(t)
	f.startOwner(t)
	packet := f.awaitPacket(t)
	hook := &designInterruption{step: "group-decision-recorded", nth: 1, reached: make(chan struct{})}
	f.hook.Store(hook)
	returned := make(chan error, 1)
	go func() {
		_, err := f.c.Ratify(ctx, f.origin, packet.Packet.Revision.Spec, packet.Packet.Revision.Plan)
		returned <- err
	}()
	select {
	case <-hook.reached:
	case <-time.After(demoTimeout):
		f.timeout(t, "pinned owner decision")
	}
	f.assertBarrier(t)
	for i, stream := range []config.WorkstreamID{f.stream1, f.stream2} {
		project := f.project
		if i == 1 {
			project = f.project2
		}
		repo, err := f.s.repository(project)
		must(t, err)
		for _, id := range []string{plan.SpecDocument, plan.PlanDocument} {
			doc, err := trace.Get[trace.Document](repo, stream, id, 1)
			must(t, err)
			doc.Revision, doc.At, doc.Cause = 2, f.clock.Now(), "later-draft"
			if id == plan.SpecDocument {
				doc.Content += "\nAdditional unapproved draft text.\n"
			}
			must(t, repo.RecordDocuments(ctx, []trace.Document{doc}))
		}
	}
	repo, err := f.s.repository(f.project)
	must(t, err)
	id, err := repo.CrossRepoWorkgroupOf(f.stream1)
	must(t, err)
	wt, group, err := f.s.approvalTrace(ctx, repo, id)
	must(t, err)
	decision, _, found, err := trace.ReadGroupApproval(wt, group.Members[0])
	must(t, err)
	if !found || decision.Complete || !equalJSONTest(decision.ProjectSpec, *packet.ProjectSpec) {
		t.Fatal("owner decision did not pin the entire draft")
	}
	later := decision.ProjectSpec
	later.Behavior = "A later unapproved project draft."
	must(t, trace.RecordProjectSpec(ctx, wt, group, later, f.clock.Now(), ownerActor, "later-draft"))
	must(t, wt.Close())
	f.stop(t)
	<-returned
	f.hook.Store(nil)
	f.start(t)
	f.awaitApproved(t)
	repo, err = f.s.repository(f.project)
	must(t, err)
	completed, found, err := repo.GroupApprovalOf(f.stream1)
	must(t, err)
	decision.Complete = true
	if !found || !equalJSONTest(decision, completed) {
		t.Fatal("recovery changed the immutable decision or its pins")
	}
	effective, _, found, err := repo.ProjectSpecOf(f.stream1)
	must(t, err)
	if !found || !equalJSONTest(effective, decision.ProjectSpec) {
		t.Fatal("unapproved project draft became effective")
	}
	wt, group, err = f.s.approvalTrace(ctx, repo, id)
	must(t, err)
	defer wt.Close()
	draft, found, err := trace.ProjectSpecFor(wt, group.ID)
	must(t, err)
	if !found || draft.Behavior != later.Behavior {
		t.Fatal("recovery rewrote draft history")
	}
	f.checkContinuation(t)
}

func TestDesignProductionSelectionRefusesInvalidReposBeforeAllocation(t *testing.T) {
	for _, kind := range []string{"empty", "duplicate", "outside"} {
		t.Run(kind, func(t *testing.T) {
			t.Parallel()
			f := newProductionDesignFixture(t)
			defer f.stop(t)
			selected := []string{}
			if kind == "duplicate" {
				selected = []string{string(f.project), string(f.project)}
			}
			if kind == "outside" {
				selected = []string{"p_000000000000000000000000000000ff"}
			}
			f.engine.mu.Lock()
			f.engine.turns["select-repos-1"] = func(ctx context.Context, req agent.Request, _ *agent.Turn, tools *mcp.ClientSession) (*agent.Result, error) {
				if _, err := callTool(ctx, tools, affectedReposTool, map[string]any{"repos": selected}); err == nil {
					return nil, errors.New("invalid selection accepted")
				}
				return questionResult(req, "session-refused", "Selection refused."), nil
			}
			f.engine.mu.Unlock()
			f.startOwner(t)
			f.wait(t, "invalid selection completed", func() bool {
				repo, err := f.s.repository(f.project)
				must(t, err)
				state, err := repo.Workflow(f.origin, designStartSubject)
				must(t, err)
				return state.Value == "invalid"
			})
			for _, p := range []config.ProjectID{f.project, f.project2} {
				repo, err := f.s.repository(p)
				must(t, err)
				streams, err := repo.Workstreams()
				must(t, err)
				for _, m := range streams {
					if m != f.origin && m != librarianWorkstream(p) {
						t.Fatalf("invalid selection allocated a member %s", m)
					}
				}
			}
			repo, err := f.s.repository(f.project)
			must(t, err)
			chief, err := repo.ChiefOfStaffThread(f.origin)
			must(t, err)
			if !equalJSONTest(chief, f.before) {
				t.Fatal("invalid selection changed original conversation")
			}
			brief, err := trace.Get[trace.Document](repo, f.origin, handedDocument, 1)
			must(t, err)
			if !equalJSONTest(brief, f.handed) {
				t.Fatal("invalid selection rewrote handed input")
			}
		})
	}
}

func TestDesignProductionContinuedStatusEscalationAndRuling(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newProductionDesignFixture(t)
	defer f.stop(t)
	f.startOwner(t)
	packet := f.awaitPacket(t)
	_, err := f.c.Ratify(ctx, f.stream1, packet.Packet.Revision.Spec, packet.Packet.Revision.Plan)
	must(t, err)
	f.awaitApproved(t)
	f.stop(t)
	f.start(t)
	f.checkContinuation(t)
	const ruling = "Keep the offset contract stable."
	const escalation = "May both repos change the offset contract?"
	var escalated, relayed atomic.Bool
	var unaffectedRuns atomic.Int32
	f.engine.mu.Lock()
	f.engine.turns["*"] = func(ctx context.Context, req agent.Request, _ *agent.Turn, tools *mcp.ClientSession) (*agent.Result, error) {
		f.recordEngine(req, "fallback")
		names, err := toolNames(ctx, tools)
		if err != nil {
			return nil, err
		}
		if !slices.Contains(names, status.ToolName) {
			return questionResult(req, "session-idle", "Working within the slice."), nil
		}
		if strings.Contains(req.Prompt, ruling) && relayed.CompareAndSwap(false, true) {
			_, err := callTool(ctx, tools, questions.RelayRulingTool, map[string]any{"question": string(f.stream1) + ":1", "text": ruling, "scope": "local"})
			return questionResult(req, "session-chief", "Ruling relayed."), err
		}
		_, err = callTool(ctx, tools, status.ToolName, map[string]any{"goal": "Implement resumable uploads.", "note": "The offset contract remains agreed.", "agents": []string{}})
		if err != nil {
			return nil, err
		}
		if strings.Contains(req.Prompt, "affected-one") && strings.Contains(req.Prompt, "affected-two") && escalated.CompareAndSwap(false, true) {
			_, err = callTool(ctx, tools, questions.EscalateTool, map[string]any{"questions": []string{string(f.stream1) + ":1", string(f.stream2) + ":1"}, "rephrasing": escalation, "blocked": "Both asking origins.", "options": []string{"Keep", "Change"}, "recommendation": "Keep."})
		}
		return questionResult(req, "session-chief", "The service is following the agreed design."), err
	}
	f.engine.turns["affected-ask"] = func(ctx context.Context, req agent.Request, _ *agent.Turn, tools *mcp.ClientSession) (*agent.Result, error) {
		question := "affected-one: may the offset contract change?"
		if strings.Contains(req.SessionDir, "worker_second") {
			question = "affected-two: may the offset contract change?"
		}
		_, err := callTool(ctx, tools, questions.AskTool, map[string]any{"question": question})
		return questionResult(req, "session-worker", "Asked the chief."), err
	}
	f.engine.turns["unaffected-work"] = func(_ context.Context, req agent.Request, _ *agent.Turn, _ *mcp.ClientSession) (*agent.Result, error) {
		unaffectedRuns.Add(1)
		return questionResult(req, "session-unaffected", "Completed independent work."), nil
	}
	f.engine.turns["answer_1"] = func(_ context.Context, req agent.Request, _ *agent.Turn, _ *mcp.ClientSession) (*agent.Result, error) {
		if !strings.Contains(req.Prompt, ruling) {
			return nil, errors.New("origin resumed without its owner ruling")
		}
		return questionResult(req, "session-worker", "Resumed within the stable contract."), nil
	}
	f.engine.mu.Unlock()
	sent, err := f.c.Send(ctx, f.stream2, "What is the current status of both repos?")
	must(t, err)
	f.owner, f.ownerAddress = sent, f.stream2
	f.wait(t, "owner status", func() bool {
		feed, api := f.s.feedList(string(f.stream1))
		if api != nil {
			t.Fatal(api)
		}
		return slices.ContainsFunc(feed.Entries, func(e FeedEntry) bool { return e.Kind == "status" && e.Status.Goal == "Implement resumable uploads." })
	})
	enqueue := func(p config.ProjectID, m config.WorkstreamID, id, turn string) {
		repo, err := f.s.repository(p)
		must(t, err)
		h := trace.Header{Schema: "osmia.trace.agent", Version: 1, Revision: 1, ID: id, Project: p, Workstream: m, At: f.clock.Now(), Actor: ownerActor, Cause: "workstream_created"}
		must(t, repo.CreateThread(ctx, trace.Agent{Header: h, Role: masonRole, ThreadID: id}))
		h.Schema, h.ID = "osmia.trace.turn-request", "request_"+id
		_, err = repo.EnqueueTurn(ctx, trace.TurnRequest{Header: h, AgentID: id, ThreadID: id, TurnID: turn, Profile: coreadapter.Profile{Name: "default", Backend: "claude", Model: "test"}, SystemPrompt: "You are a member mason.", Prompt: "Perform scoped work."})
		must(t, err)
	}
	enqueue(f.project, f.stream1, "worker_first", "affected-ask")
	enqueue(f.project2, f.stream2, "worker_second", "affected-ask")
	var inbox InboxResponse
	f.wait(t, "member escalation", func() bool {
		got, err := f.c.Inbox(ctx)
		must(t, err)
		got.Entries = slices.DeleteFunc(slices.Clone(got.Entries), func(entry InboxEntry) bool { return entry.Question != escalation })
		if len(got.Entries) > 1 {
			t.Fatalf("consolidated escalation duplicated: %+v", got)
		}
		if len(got.Entries) != 1 || len(got.Entries[0].Asked) != 2 {
			return false
		}
		for _, id := range []string{"worker_first", "worker_second"} {
			if !slices.ContainsFunc(got.Entries[0].Asked, func(q InboxQuestion) bool { return q.AskedBy == id }) {
				return false
			}
		}
		inbox = got
		return true
	})
	if len(inbox.Entries) != 1 {
		t.Fatalf("consolidated escalation duplicated: %+v", inbox)
	}
	entry := inbox.Entries[0]
	repo, err := f.s.repository(f.project)
	must(t, err)
	group, err := repo.CrossRepoWorkgroupOf(f.stream1)
	must(t, err)
	inboxGroup, ok := trace.WorkgroupProjectID(entry.Project)
	if !ok || inboxGroup != group || entry.Number < 1 || entry.Batch == "" || entry.Workstream != f.stream1 || entry.Kind != InboxEscalation || len(entry.Asked) != 2 {
		t.Fatalf("escalation lost workgroup/batch identity or affected origins: %+v; group=%s", entry, group)
	}
	for i, id := range []string{"worker_first", "worker_second"} {
		question := []string{"affected-one", "affected-two"}[i] + ": may the offset contract change?"
		if !slices.ContainsFunc(entry.Asked, func(q InboxQuestion) bool { return q.AskedBy == id && q.Question == question }) {
			t.Fatalf("consolidated entry omits origin %s: %+v", id, entry)
		}
	}
	enqueue(f.project, f.stream1, "worker_unaffected", "unaffected-work")
	f.wait(t, "workers settled before escalation restart", func() bool {
		for i, id := range []string{"worker_first", "worker_second", "worker_unaffected"} {
			p, m := f.project, f.stream1
			if i == 1 {
				p, m = f.project2, f.stream2
			}
			repo, err := f.s.repository(p)
			must(t, err)
			worker, err := repo.Thread(m, id)
			must(t, err)
			if len(worker.Turns) != 1 || worker.Turns[0].CompletedAt.IsZero() {
				return false
			}
			if i < 2 && !worker.Parked() {
				t.Fatalf("affected origin %s did not park on its question: %+v", id, worker)
			}
			if i == 2 && worker.Turns[0].Status() != "idle" {
				t.Fatalf("unaffected work did not complete successfully: %+v", worker)
			}
		}
		return unaffectedRuns.Load() == 1
	})
	before := map[string]trace.Thread{}
	asked := map[string]trace.QuestionState{}
	for i, id := range []string{"worker_first", "worker_second", "worker_unaffected"} {
		p, m := f.project, f.stream1
		if i == 1 {
			p, m = f.project2, f.stream2
		}
		repo, err := f.s.repository(p)
		must(t, err)
		before[id], err = repo.Thread(m, id)
		must(t, err)
		if i < 2 {
			qs, err := repo.Questions(m)
			must(t, err)
			if len(qs) != 1 || qs[0].State != trace.QuestionEscalated || qs[0].Asked.Thread != id || qs[0].Asked.Turn != "affected-ask" || qs[0].Latest.Escalation == nil || qs[0].Latest.Escalation.Batch != entry.Batch || qs[0].Latest.Escalation.Inbox != entry.Number {
				t.Fatalf("origin %s lost its asking link or escalation identity: %+v", id, qs)
			}
			asked[id] = qs[0]
		}
	}
	f.stop(t)
	f.start(t)
	continued, err := f.c.Inbox(ctx)
	must(t, err)
	continued.Entries = slices.DeleteFunc(slices.Clone(continued.Entries), func(entry InboxEntry) bool { return entry.Question != escalation })
	if len(continued.Entries) != 1 || !equalJSONTest(entry, continued.Entries[0]) {
		t.Fatalf("restart lost escalation: %+v", continued)
	}
	for i, id := range []string{"worker_first", "worker_second"} {
		p, m := f.project, f.stream1
		if i == 1 {
			p, m = f.project2, f.stream2
		}
		repo, err := f.s.repository(p)
		must(t, err)
		qs, err := repo.Questions(m)
		must(t, err)
		if len(qs) != 1 || !equalJSONTest(asked[id], qs[0]) {
			t.Fatalf("restart changed origin %s's durable asking link: %+v", id, qs)
		}
	}
	answer, err := f.c.Answer(ctx, continued.Entries[0].Number, ruling, continued.Entries[0].Project)
	must(t, err)
	if answer.Number != entry.Number || answer.Batch != entry.Batch || answer.Ruling != ruling || len(answer.Questions) != 2 {
		t.Fatalf("owner ruling did not cover the selected consolidated question: %+v", answer)
	}
	f.wait(t, "owner ruling resumes affected origins", func() bool {
		for i, id := range []string{"worker_first", "worker_second"} {
			p, m := f.project, f.stream1
			if i == 1 {
				p, m = f.project2, f.stream2
			}
			repo, err := f.s.repository(p)
			must(t, err)
			worker, err := repo.Thread(m, id)
			must(t, err)
			if len(worker.Turns) != 2 || worker.Turns[1].CompletedAt.IsZero() {
				return false
			}
		}
		return true
	})
	for i, id := range []string{"worker_first", "worker_second", "worker_unaffected"} {
		p, m := f.project, f.stream1
		if i == 1 {
			p, m = f.project2, f.stream2
		}
		repo, err := f.s.repository(p)
		must(t, err)
		worker, err := repo.Thread(m, id)
		must(t, err)
		if i == 2 && !equalJSONTest(before[id], worker) {
			t.Fatal("owner ruling resumed an unaffected origin")
		}
		if i < 2 && (!equalJSONTest(before[id].Turns[0], worker.Turns[0]) || worker.Parked()) {
			t.Fatal("owner ruling changed the asking turn or failed to resume it")
		}
		if i < 2 {
			qs, err := repo.Questions(m)
			must(t, err)
			if len(qs) != 1 || !equalJSONTest(asked[id].Asked, qs[0].Asked) || qs[0].State != trace.QuestionAnswered || qs[0].Ruling == nil || qs[0].Ruling.Revision != 2 || qs[0].Ruling.OwnerResponse != ruling || qs[0].Ruling.ReturnedAnswer != ruling {
				t.Fatalf("origin %s did not retain its exact owner ruling and asking link: %+v", id, qs)
			}
			if len(worker.Turns) != 2 || worker.Turns[1].Request.TurnID != "answer_1" || worker.Turns[1].Response == nil || worker.Turns[1].Status() != "idle" || !strings.Contains(worker.Turns[1].Request.Prompt, ruling) {
				t.Fatalf("origin %s did not complete exactly one ruling continuation: %+v", id, worker)
			}
		}
	}
	f.checkContinuation(t)
	if unaffectedRuns.Load() != 1 || !relayed.Load() {
		t.Fatalf("unaffected work or ruling relay failed: runs=%d relay=%v", unaffectedRuns.Load(), relayed.Load())
	}
}

func TestDesignProductionSingleAffectedRepoKeepsOrdinaryRatification(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newProductionDesignFixture(t)
	defer f.stop(t)
	f.stream1 = f.origin
	f.engine.mu.Lock()
	f.engine.turns["select-repos-1"] = func(ctx context.Context, req agent.Request, _ *agent.Turn, tools *mcp.ClientSession) (*agent.Result, error) {
		_, err := callTool(ctx, tools, affectedReposTool, map[string]any{"repos": []string{string(f.project)}})
		return questionResult(req, "session-selection", "Only the server needs a change."), err
	}
	f.engine.mu.Unlock()
	f.startOwner(t)
	var packet PacketResponse
	f.wait(t, "single-repo packet", func() bool {
		got, err := f.c.Packet(ctx, f.origin)
		if err != nil {
			return false
		}
		packet = got
		return true
	})
	if packet.ProjectSpec != nil {
		t.Fatal("one affected repo introduced a project-spec gate")
	}
	out, err := f.c.Ratify(ctx, f.origin, packet.Packet.Revision.Spec, packet.Packet.Revision.Plan)
	must(t, err)
	if len(out.Members) != 0 || out.Sealing != "requested" {
		t.Fatalf("single-member owner contract changed: %+v", out)
	}
	f.wait(t, "single-repo seal", func() bool {
		repo, err := f.s.repository(f.project)
		must(t, err)
		_, _, found, err := seal.Latest(repo, f.origin)
		must(t, err)
		return found
	})
	repo, err := f.s.repository(f.project)
	must(t, err)
	chief, err := repo.ChiefOfStaffThread(f.origin)
	must(t, err)
	if !equalJSONTest(chief.Identity, f.before.Identity) || !equalJSONTest(chief.Turns[:len(f.before.Turns)], f.before.Turns) {
		t.Fatal("single-member continuation changed")
	}
	groups, _, err := trace.Workgroups(repo)
	must(t, err)
	group, found := trace.WorkgroupOf(groups, f.origin)
	if !found || !slices.Equal(group.Members, []config.WorkstreamID{f.origin}) {
		t.Fatal("single-member workgroup contract changed")
	}
}

func TestDesignProductionContinuedPendingTurnRecoversWithBoundedReplay(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newProductionDesignFixture(t)
	defer f.stop(t)
	f.startOwner(t)
	packet := f.awaitPacket(t)
	_, err := f.c.Ratify(ctx, f.origin, packet.Packet.Revision.Spec, packet.Packet.Revision.Plan)
	must(t, err)
	f.awaitApproved(t)
	reached := make(chan agent.Request, 1)
	var hit atomic.Bool
	const message = "Give the owner the current upload status after restart."
	f.engine.mu.Lock()
	f.engine.turns["*"] = func(ctx context.Context, req agent.Request, _ *agent.Turn, _ *mcp.ClientSession) (*agent.Result, error) {
		f.recordEngine(req, "fallback")
		if strings.Contains(req.Prompt, message) && hit.CompareAndSwap(false, true) {
			reached <- req
			<-ctx.Done()
			return nil, ctx.Err()
		}
		return questionResult(req, "session-chief", "Following the approved contract."), nil
	}
	f.engine.mu.Unlock()
	sent, err := f.c.Send(ctx, f.origin, message)
	must(t, err)
	f.owner, f.ownerAddress = sent, f.origin
	f.awaitOwnerTurnRunning(t, sent, reached)
	repo, err := f.s.repository(f.project)
	must(t, err)
	chief, host, _, close, err := f.s.chiefRepository(ctx, repo, f.stream1)
	must(t, err)
	before, err := chief.Thread(host, trace.ChiefOfStaff)
	close()
	must(t, err)
	index := slices.IndexFunc(before.Turns, func(q trace.QueuedTurn) bool { return q.Request.TurnID == sent.Turn })
	if index < 0 || before.Active != sent.Turn || before.Turns[index].Claim == nil || before.Turns[index].Response != nil || !before.Turns[index].CompletedAt.IsZero() {
		t.Logf("continued chief status=%s active=%s sent=%s queue-index=%d", before.Status, before.Active, sent.Turn, index)
		t.Fatal("continued pending owner turn was not the durably claimed active request")
	}
	request := before.Turns[index].Request
	if request.Prompt != message || request.Actor != ownerActor || !request.At.Equal(sent.At) || request.Workstream != f.origin || !equalJSONTest(before.Identity, f.before.Identity) {
		t.Fatalf("running owner request did not retain its accepted content or original conversation identity: header=%+v thread=%+v", request.Header, before.Identity)
	}
	f.stop(t)
	var replayed atomic.Bool
	f.engine.mu.Lock()
	f.engine.turns["*"] = func(ctx context.Context, req agent.Request, _ *agent.Turn, tools *mcp.ClientSession) (*agent.Result, error) {
		if strings.Contains(req.Prompt, message) {
			replayed.Store(true)
			_, err := callTool(ctx, tools, status.ToolName, map[string]any{"goal": "Continue resumable uploads.", "note": "Restart retained the owner status request.", "agents": []string{}})
			return questionResult(req, "session-recovered", "Status continued after restart."), err
		}
		return questionResult(req, "session-chief", "Following the approved contract."), nil
	}
	f.engine.mu.Unlock()
	f.start(t)
	f.wait(t, "pending owner turn replay", func() bool { return replayed.Load() })
	repo, err = f.s.repository(f.project)
	must(t, err)
	chief, host, _, close, err = f.s.chiefRepository(ctx, repo, f.stream1)
	must(t, err)
	defer close()
	f.wait(t, "recovery turn completion", func() bool {
		recovered, err := chief.Thread(host, trace.ChiefOfStaff)
		must(t, err)
		return recovered.Active == "" && slices.ContainsFunc(recovered.Turns, func(q trace.QueuedTurn) bool {
			return q.Request.Actor == recoveryActor && strings.Contains(q.Request.Prompt, message) && q.Response != nil && !q.CompletedAt.IsZero()
		})
	})
	recovered, err := chief.Thread(host, trace.ChiefOfStaff)
	must(t, err)
	q := recovered.Turns[index]
	if !equalJSONTest(q.Request, request) || q.Response == nil || q.Response.RequestID != request.ID || q.Response.RequestRevision != request.Revision {
		t.Fatal("restart lost the pending request or response link")
	}
	followups := 0
	for _, turn := range recovered.Turns {
		if turn.Request.Actor == recoveryActor && strings.Contains(turn.Request.Prompt, message) {
			followups++
		}
	}
	if followups != 1 {
		t.Fatalf("recovery duplicated the continued turn: %d", followups)
	}
	if len(q.Attempts) > 2 {
		t.Fatalf("recovery exceeded the persisted attempt bound: %d", len(q.Attempts))
	}
	f.checkContinuation(t)
}
