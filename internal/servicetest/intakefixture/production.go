package intakefixture

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

	"github.com/kpenfound/agentcore/agent"
	"github.com/kpenfound/osmia/internal/api"
	"github.com/kpenfound/osmia/internal/bundle"
	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/intake"
	"github.com/kpenfound/osmia/internal/isolation"
	"github.com/kpenfound/osmia/internal/questions"
	"github.com/kpenfound/osmia/internal/readview"
	"github.com/kpenfound/osmia/internal/scheduler"
	"github.com/kpenfound/osmia/internal/seal"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	"github.com/kpenfound/osmia/internal/thread"
	"github.com/kpenfound/osmia/internal/trace"
	"github.com/kpenfound/osmia/internal/workspace"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// ProductionDesignClient is the owner API used by the shared design scenario.
type ProductionDesignClient interface {
	AddProject(context.Context, api.ProjectAddRequest) (api.ProjectResponse, error)
	AddRepo(context.Context, api.RepoAddRequest) (api.ProjectResponse, error)
	OpenDesign(context.Context, config.ProjectID) (api.DesignResponse, error)
	DesignMessage(context.Context, config.WorkstreamID, string) (api.ConversationEntry, error)
	DesignConversation(context.Context, config.WorkstreamID) (api.ConversationResponse, error)
	DesignStart(context.Context, config.WorkstreamID) (api.DesignStartResponse, error)
	Packet(context.Context, config.WorkstreamID) (api.PacketResponse, error)
	Status(context.Context, config.WorkstreamID) (api.WorkstreamStatus, error)
	Statuses(context.Context) (api.StatusResponse, error)
	Conversation(context.Context, config.WorkstreamID) (api.ConversationResponse, error)
}

// ProductionDesignRepo pins the source repository used by a design scenario.
type ProductionDesignRepo struct {
	ID                            config.ProjectID
	Clone, Upstream, Remote, Main string
}

// ProductionChiefDispatch records execution-boundary observations for diagnostics.
type ProductionChiefDispatch struct {
	Mu                         sync.Mutex
	Bindings                   int
	Prepared                   thread.TurnInput
	PrepareError               string
	Scope                      coreadapter.Scope
	SelectionError, ToolsError string
	EngineName, Handler        string
}

// ProductionDesign shares production-design fixture behaviour without importing
// the service. State references and callbacks follow the adapter across restarts.
// Only the test adapter supplies access to controller internals.
type ProductionDesign struct {
	Changes           func(*testing.T) (func(), func())
	Scheduled         func() (*config.Config, []ScheduledProject)
	LastEdgePoll      func(config.ProjectID, config.WorkstreamID) (time.Time, bool)
	EdgeObservationAt func(*trace.Repository, config.WorkstreamID) (int, time.Time, error)

	ContextMode              func(config.ProjectID) bundle.Mode
	Project, Project2        *config.ProjectID
	Trace                    *string
	Origin, Stream1, Stream2 *config.WorkstreamID
	Before                   *trace.Thread
	Handed                   *trace.Document
	Owner                    *api.ConversationEntry
	OwnerAddress             *config.WorkstreamID
	Repos                    *[]ProductionDesignRepo
	Clone                    string
	Engine                   *fixturekit.DemoEngine
	Clock                    *fixturekit.FixedClock
	Dispatch                 *ProductionChiefDispatch
	Client                   func() ProductionDesignClient
	Config                   func() *config.Config
	Repository               func(config.ProjectID) (*trace.Repository, error)
	ChiefRepository          func(context.Context, *trace.Repository, config.WorkstreamID) (*trace.Repository, config.WorkstreamID, config.WorkstreamID, func(), error)
	Feed                     func(string) (api.FeedResponse, *api.APIError)
	Ready                    func() bool
	Done                     func() <-chan struct{}
	Lifetime                 func() context.Context
	Error                    func() error
	Runs                     func() []string
	WaitMode                 func(*testing.T, string, func() bool)
	LogHook                  func(*testing.T)
	WriteCharter             func(*testing.T, config.ProjectID, string)
	Upstream                 func(*testing.T) string
	Script                   func(string, map[string]string, func(context.Context, agent.Request, *agent.Turn, *mcp.ClientSession) error)
	ScriptDrafts             func(*testing.T)
	Workspaces               func(*config.Config, string, string) workspace.Provider
	ReadBuilding             func(*trace.Repository, config.WorkstreamID) (bool, error)
	Holds                    func(config.ProjectID, config.WorkstreamID, *trace.Repository, scheduler.Candidate) (bool, error)
	EdgeWaiting              func(*trace.Repository, config.WorkstreamID) (bool, error)
	Builder                  func(*trace.Repository) coreadapter.Reconciler
	Land                     func(context.Context, *trace.Repository, config.WorkstreamID) (coreadapter.OperationResult, error)
	LeastWorkstream          func([]config.WorkstreamID) config.WorkstreamID
	NextReadyTurn            func(trace.Thread) (trace.QueuedTurn, bool)
	LibrarianWorkstream      func(config.ProjectID) config.WorkstreamID
}

const ProductionBrief = "Decisions: keep the acknowledged offset contract stable.\nIntent and tradeoffs: implement chunk storage in the server and resumption in the client, changing both repos.\nRejected options: client-only retries.\n"

func (f *ProductionDesign) CheckRepos(t *testing.T) {
	t.Helper()
	ctx := context.Background()
	for _, expected := range *f.Repos {
		cfg := f.Config().For(expected.ID)
		if cfg.Project.Clone != expected.Clone || cfg.Project.Upstream != expected.Upstream || cfg.Project.BaseBranch != "main" {
			t.Fatalf("repo %s config changed: %+v", expected.ID, cfg.Project)
		}
		provider := f.Workspaces(cfg, intake.BranchesDirectory, config.WorkspacesGit)
		remote, err := provider.Remote(ctx, cfg.Project.Upstream)
		Must(t, err)
		if remote != "upstream" {
			t.Fatalf("repo %s resolved remote %q, want upstream", expected.ID, remote)
		}
		url := strings.TrimSpace(fixturekit.DemoGit(t, filepath.Dir(expected.Clone), "-C", expected.Clone, "remote", "get-url", remote))
		if url != expected.Remote {
			t.Fatalf("repo %s remote URL %q, want %q", expected.ID, url, expected.Remote)
		}
		commit, found, err := provider.RemoteBranch(ctx, remote, cfg.Project.BaseBranch)
		Must(t, err)
		if !found || commit != expected.Main {
			t.Fatalf("repo %s upstream main=%q exists=%v, want %q", expected.ID, commit, found, expected.Main)
		}
	}
}

func (f *ProductionDesign) Wait(t *testing.T, phase string, done func() bool) {
	t.Helper()
	if f.WaitMode != nil {
		f.WaitMode(t, phase, done)
		return
	}
	deadline := time.Now().Add(DemoTimeout)
	for !done() {
		if time.Now().After(deadline) {
			f.Timeout(t, phase)
		}
		f.Clock.Advance(time.Second)
		time.Sleep(20 * time.Millisecond)
	}
}

func (f *ProductionDesign) Timeout(t *testing.T, phase string) {
	t.Helper()
	t.Logf("phase=%s design=%s members=%s/%s now=%v ready=%v lifetime=%v engine-runs=%v", phase, *f.Origin, *f.Stream1, *f.Stream2, f.Clock.Now(), f.Ready(), f.Lifetime().Err(), f.Runs())
	if f.LogHook != nil {
		f.LogHook(t)
	}
	select {
	case <-f.Done():
		t.Logf("service terminated: %v", f.Error())
	default:
		t.Log("service terminal error: none recorded; shutdown has not completed")
	}
	for i, member := range []config.WorkstreamID{*f.Origin, *f.Stream1, *f.Stream2} {
		if member == "" || (i == 1 && member == *f.Origin) {
			continue
		}
		project := *f.Project
		if i == 2 {
			project = *f.Project2
		}
		repo, err := f.Repository(project)
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
		f.LogTurns(t, repo, member)
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
			chief, host, _, close, err := f.ChiefRepository(context.Background(), repo, member)
			if err == nil {
				f.LogTurns(t, chief, host)
				close()
			} else {
				t.Logf("shared chief error=%v", err)
			}
		}
	}
	f.OwnerSummary(t)
	t.Fatalf("production design workflow timed out in %s", phase)
}

func (f *ProductionDesign) OwnerSummary(t *testing.T) {
	t.Helper()
	if f.Owner.Turn == "" {
		return
	}
	sent := *f.Owner
	terminal := "running"
	select {
	case <-f.Done():
		terminal = fmt.Sprint(f.Error())
	default:
	}
	t.Logf("OWNER turn=%s address=%s design=%s members=%s/%s accepted=%v now=%v ready=%v lifetime=%v terminal=%s", sent.Turn, *f.OwnerAddress, *f.Origin, *f.Stream1, *f.Stream2, sent.At, f.Clock.Now(), f.Ready(), f.Lifetime().Err(), terminal)
	f.Dispatch.Mu.Lock()
	t.Logf("OWNER dispatch-bindings=%d prepared=%+v preparation-error=%s scope=%+v selection-error=%s tools-error=%s engine-name=%s handler=%s", f.Dispatch.Bindings, f.Dispatch.Prepared, f.Dispatch.PrepareError, f.Dispatch.Scope, f.Dispatch.SelectionError, f.Dispatch.ToolsError, f.Dispatch.EngineName, f.Dispatch.Handler)
	f.Dispatch.Mu.Unlock()
	f.Engine.Mu.Lock()
	engineRan := slices.Contains(f.Engine.Runs, sent.Turn)
	handler := "fallback"
	if f.Engine.Turns[sent.Turn] != nil {
		handler = "named"
	}
	f.Engine.Mu.Unlock()
	t.Logf("OWNER sent-engine-ran=%v selected-handler=%s", engineRan, handler)
	repo, err := f.Repository(*f.Project)
	if err != nil {
		t.Logf("OWNER source error=%v", err)
		return
	}
	chief, host, _, release, err := f.ChiefRepository(context.Background(), repo, *f.Origin)
	if err != nil {
		t.Logf("OWNER shared error=%v", err)
		return
	}
	defer release()
	id, err := repo.CrossRepoWorkgroupOf(*f.Origin)
	if err != nil {
		t.Logf("OWNER group error=%v", err)
		return
	}
	_, group, _, err := repo.CrossRepoProjectSpec(id)
	if err != nil {
		t.Logf("OWNER group read error=%v", err)
		return
	}
	leader := f.LeastWorkstream(group.Members[1:])
	for i, address := range []config.WorkstreamID{*f.Origin, *f.Stream1, *f.Stream2} {
		project := *f.Project
		if i == 2 {
			project = *f.Project2
		}
		member, err := f.Repository(project)
		if err != nil {
			t.Logf("OWNER address=%s project=%s error=%v", address, project, err)
			continue
		}
		target, routedHost, _, closeTarget, err := f.ChiefRepository(context.Background(), member, address)
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
			if !op.Acknowledged && op.RetryAt.After(f.Clock.Now()) {
				t.Logf("OWNER pending-retry member=%s operation=%s action=%s retry=%v", address, op.Operation.ID, op.Operation.Action, op.RetryAt)
			}
		}
	}
	th, err := chief.Thread(host, trace.ChiefOfStaff)
	if err != nil {
		t.Logf("OWNER thread error=%v", err)
		return
	}
	q, ready := f.NextReadyTurn(th)
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

func (f *ProductionDesign) LogTurns(t *testing.T, repo *trace.Repository, member config.WorkstreamID) {
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

func (f *ProductionDesign) StartOwner(t *testing.T) {
	t.Helper()
	_, err := f.Client().DesignStart(context.Background(), *f.Origin)
	Must(t, err)
	repo, err := f.Repository(*f.Project)
	Must(t, err)
	*f.Handed, err = trace.Get[trace.Document](repo, *f.Origin, intake.HandedDocument, 1)
	Must(t, err)
}

func (f *ProductionDesign) AwaitPacket(t *testing.T) api.PacketResponse {
	t.Helper()
	var packet api.PacketResponse
	f.Wait(t, "ratification packet", func() bool {
		a, err := f.Client().Packet(context.Background(), *f.Origin)
		if err != nil || a.ProjectSpec == nil {
			return false
		}
		if _, err := f.Client().Packet(context.Background(), *f.Stream2); err != nil {
			return false
		}
		packet = a
		return true
	})
	for i, member := range []config.WorkstreamID{*f.Stream1, *f.Stream2} {
		project := *f.Project
		if i == 1 {
			project = *f.Project2
		}
		repo, err := f.Repository(project)
		Must(t, err)
		for n := 1; n <= 2; n++ {
			th, err := repo.Thread(member, intake.CommitteeAgent(n))
			Must(t, err)
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

func (f *ProductionDesign) CheckContinuation(t *testing.T) {
	t.Helper()
	ctx := context.Background()
	for i, m := range []config.WorkstreamID{*f.Stream1, *f.Stream2} {
		p := *f.Project
		if i == 1 {
			p = *f.Project2
		}
		repo, err := f.Repository(p)
		Must(t, err)
		streams, err := repo.Workstreams()
		Must(t, err)
		features := slices.DeleteFunc(slices.Clone(streams), func(stream config.WorkstreamID) bool {
			return stream == f.LibrarianWorkstream(p) || stream == *f.Origin
		})
		if !slices.Equal(features, []config.WorkstreamID{m}) {
			t.Fatalf("repo has lost or duplicated its allocated member: %v", features)
		}
		if f.ContextMode(p) != bundle.ModeFile {
			t.Fatal("production path requires Hearsay")
		}
		handed, err := trace.Get[trace.Document](repo, m, intake.HandedDocument, 1)
		Must(t, err)
		if handed.Content != f.Handed.Content || handed.Source != f.Handed.Source {
			t.Fatal("member input differs from the immutable brief")
		}
		cfg := f.Config().For(p)
		for _, name := range []string{"spec.md", "plan.json", "handed", "tools/design-brief.json", "workgroups", ".osmia"} {
			if _, err := os.Stat(filepath.Join(cfg.Project.Clone, name)); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("factory state leaked into target repo at %s: %v", name, err)
			}
		}
		if _, err := repo.Thread(m, trace.ChiefOfStaff); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("member retains an independent chief: %v", err)
		}
		groups, _, err := trace.Workgroups(repo)
		Must(t, err)
		if _, found := trace.WorkgroupOf(groups, m); found {
			t.Fatal("member retains a one-workstream group")
		}
		target, host, _, close, err := f.ChiefRepository(ctx, repo, m)
		Must(t, err)
		chief, err := target.Thread(host, trace.ChiefOfStaff)
		close()
		Must(t, err)
		if len(chief.Turns) < len(f.Before.Turns) {
			t.Fatal("design turns were lost")
		}
		if !EqualJSONTest(f.Before.Identity, chief.Identity) || len(f.Before.Turns) > 0 && !EqualJSONTest(f.Before.Turns, chief.Turns[:len(f.Before.Turns)]) {
			t.Fatal("design identity, requests, responses or recovery state changed at adoption")
		}
	}
	repo, err := f.Repository(*f.Project)
	Must(t, err)
	id, err := repo.CrossRepoWorkgroupOf(*f.Origin)
	Must(t, err)
	_, group, _, err := repo.CrossRepoProjectSpec(id)
	Must(t, err)
	if group.Conversation == nil || group.Conversation.Workstream != *f.Origin || group.Conversation.Project != *f.Project ||
		len(group.Members) != 3 || group.Members[0] != *f.Origin ||
		!slices.Contains(group.Members[1:], *f.Stream1) || !slices.Contains(group.Members[1:], *f.Stream2) {
		t.Fatalf("continued conversation or exact member coverage changed: %+v", group)
	}
	groups, err := os.ReadDir(filepath.Join(f.Config().Root.String(), "workgroups"))
	Must(t, err)
	if len(groups) != 1 || groups[0].Name() != string(id) {
		t.Fatalf("start or recovery duplicated workgroup authority: %v", groups)
	}
	handed, err := trace.Get[trace.Document](repo, *f.Origin, intake.HandedDocument, 1)
	Must(t, err)
	if !EqualJSONTest(handed, *f.Handed) {
		t.Fatal("immutable handed brief changed")
	}
	alias, err := f.Client().Status(ctx, *f.Origin)
	Must(t, err)
	if alias.Workstream != *f.Stream1 || alias.Project != *f.Project {
		t.Fatalf("design status did not resolve to its member: %+v", alias)
	}
	listed, err := f.Client().Statuses(ctx)
	Must(t, err)
	if slices.ContainsFunc(listed.Workstreams, func(w api.WorkstreamStatus) bool { return w.Workstream == *f.Origin }) {
		t.Fatal("status list exposes the adopted design host as independent work")
	}
	if _, err := repo.Thread(*f.Origin, trace.ChiefOfStaff); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("source retains an authoritative queue")
	}
	historical, err := trace.Read[trace.TurnRequest](repo, *f.Origin)
	Must(t, err)
	if len(f.Before.Turns) > 0 && !slices.ContainsFunc(historical, func(req trace.TurnRequest) bool { return req.ID == f.Before.Turns[0].Request.ID }) {
		t.Fatal("historical design request was lost")
	}
	chief, _, _, closeChief, err := f.ChiefRepository(ctx, repo, *f.Stream1)
	Must(t, err)
	defer closeChief()
	matching, err := readview.Consistent(func() (bool, error) {
		var entries []api.ConversationEntry
		var feed []api.FeedEntry
		for i, member := range []config.WorkstreamID{*f.Origin, *f.Stream1, *f.Stream2} {
			conversation, err := f.Client().Conversation(ctx, member)
			if err != nil {
				return false, err
			}
			got, apiError := f.Feed(string(member))
			if apiError != nil {
				return false, apiError
			}
			shared := slices.DeleteFunc(slices.Clone(got.Entries), func(e api.FeedEntry) bool {
				return e.Kind != "message" && e.Kind != "response" && e.Kind != "action" && e.Kind != "status"
			})
			if i == 0 {
				entries, feed = conversation.Entries, shared
			} else if !EqualJSONTest(entries, conversation.Entries) || !EqualJSONTest(feed, shared) {
				return false, nil
			}
		}
		return true, nil
	}, chief.Generation)
	Must(t, err)
	if !matching {
		t.Fatal("continued address and member endpoints resolve to different conversations or feeds")
	}
}

func (f *ProductionDesign) AssertBarrier(t *testing.T) {
	t.Helper()
	alias, err := f.Client().Status(context.Background(), *f.Origin)
	Must(t, err)
	if alias.Workstream != *f.Stream1 || (alias.State != nil && (*alias.State == intake.RatifiedState || *alias.State == intake.BuildingState)) {
		t.Fatalf("design status bypassed pending group authority: %+v", alias)
	}
	for i, m := range []config.WorkstreamID{*f.Stream1, *f.Stream2} {
		p := *f.Project
		if i == 1 {
			p = *f.Project2
		}
		repo, err := f.Repository(p)
		Must(t, err)
		approved, err := repo.GroupApproved(m)
		Must(t, err)
		if approved {
			t.Fatal("incomplete group is approved")
		}
		approval, found, err := repo.GroupApprovalOf(m)
		Must(t, err)
		if !found || approval.Complete || len(approval.Members) != 2 || !slices.ContainsFunc(approval.Members, func(pin trace.GroupMemberApproval) bool {
			return pin.Project == p && pin.Workstream == m && pin.Spec == 1 && pin.Plan == 1
		}) {
			t.Fatal("pending group decision lost its exact member pins")
		}
		st, err := f.Client().Status(context.Background(), m)
		Must(t, err)
		if st.State != nil && (*st.State == intake.RatifiedState || *st.State == intake.BuildingState) {
			t.Fatalf("partial effective member state: %+v", st)
		}
		if _, _, sealed, err := seal.Latest(repo, m); err != nil || sealed {
			t.Fatalf("partial effective seal: %v %v", sealed, err)
		}
		if ok, err := f.ReadBuilding(repo, m); err != nil || ok {
			t.Fatalf("unit admission bypasses approval: %v %v", ok, err)
		}
		held, err := f.Holds(p, f.LibrarianWorkstream(p), repo, scheduler.Candidate{Workstream: m, Thread: trace.Thread{Identity: trace.Agent{Role: MasonRole}}, Turn: trace.QueuedTurn{Request: trace.TurnRequest{Header: trace.Header{Unit: "accept"}}}})
		Must(t, err)
		if !held {
			t.Fatal("scheduler bypasses group barrier")
		}
		if wait, err := f.EdgeWaiting(repo, m); err != nil || !wait {
			t.Fatalf("contracts released before approval: %v %v", wait, err)
		}
		_, event := intake.BuildIDs(1)
		build := coreadapter.Operation{ID: trace.OperationID(p, m, event), Boundary: coreadapter.RepositoryBoundary, Action: intake.BuildAction, Input: json.RawMessage(`{"seal":1}`)}
		result, err := f.Builder(repo).Apply(context.Background(), build)
		Must(t, err)
		if result.Outcome != "failed" {
			t.Fatalf("direct build bypassed completion: %+v", result)
		}
		result, err = f.Land(context.Background(), repo, m)
		Must(t, err)
		if result.Outcome != "failed" || !strings.Contains(result.Evidence, "workgroup seal preparations") {
			t.Fatalf("direct landing bypassed completion: %+v", result)
		}
		ops, err := repo.Operations(m)
		Must(t, err)
		if slices.ContainsFunc(ops, func(o trace.OperationRecord) bool { return o.Operation.Action == intake.BuildAction }) {
			t.Fatal("build admitted before group completion")
		}
	}
}

func (f *ProductionDesign) AwaitApproved(t *testing.T) {
	t.Helper()
	f.Wait(t, "group approval completion", func() bool {
		repo, err := f.Repository(*f.Project)
		Must(t, err)
		ok, err := repo.GroupApproved(*f.Stream1)
		Must(t, err)
		return ok
	})
	for i, m := range []config.WorkstreamID{*f.Stream1, *f.Stream2} {
		p := *f.Project
		if i == 1 {
			p = *f.Project2
		}
		repo, err := f.Repository(p)
		Must(t, err)
		record, _, ok, err := seal.Latest(repo, m)
		Must(t, err)
		if !ok || record.Revision.Spec != 1 || record.Revision.Plan != 1 {
			t.Fatalf("wrong pinned seal: %+v", record)
		}
		expected := (*f.Repos)[i]
		if record.Base.Remote != "upstream" || record.Base.Branch != "main" || record.Base.Commit != expected.Main || record.Branch != intake.FeatureBranch(m) {
			t.Fatalf("repo %s seal has wrong upstream base or branch: %+v", p, record)
		}
		fetched := strings.TrimSpace(fixturekit.DemoGit(t, filepath.Dir(expected.Clone), "-C", expected.Clone, "rev-parse", "refs/remotes/upstream/main"))
		if fetched != expected.Main {
			t.Fatalf("repo %s fetched main=%q, want %q", p, fetched, expected.Main)
		}
		docs, err := trace.Read[trace.Document](repo, m)
		Must(t, err)
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

func ConfigureProductionDesign(t *testing.T, root string) {
	t.Helper()
	file, err := os.OpenFile(filepath.Join(root, "config.toml"), os.O_APPEND|os.O_WRONLY, 0600)
	Must(t, err)
	_, err = file.WriteString("[roles.chief_of_staff]\nsandbox = \"container\"\nimage = \"fixture-image\"\n[roles.committee]\nsandbox = \"container\"\nimage = \"fixture-image\"\n[capacity]\ncommittee = 2\n[shed]\nmax_rounds = 1\n")
	Must(t, errors.Join(err, file.Close()))
}

// BindProductionDesign wraps the fake worker boundary around production thread dispatch.
func (f *ProductionDesign) BindProductionDesign(production func(*trace.Repository, *config.Config) (coreadapter.Reconciler, error), unwrap func(coreadapter.Turns) *isolation.Turns) func(*trace.Repository, *config.Config) (coreadapter.Reconciler, error) {
	return func(r *trace.Repository, cfg *config.Config) (coreadapter.Reconciler, error) {
		bound, err := production(r, cfg)
		if err != nil {
			return nil, err
		}
		d := bound.(thread.Dispatcher)
		if _, shared := trace.WorkgroupProjectID(r.Project()); shared {
			f.Dispatch.Mu.Lock()
			f.Dispatch.Bindings++
			f.Dispatch.Mu.Unlock()
			prepare := d.Prepare
			d.Prepare = func(ctx context.Context, in thread.TurnInput) (coreadapter.PreparedTurn, error) {
				prepared, err := prepare(ctx, in)
				f.Dispatch.Mu.Lock()
				f.Dispatch.Prepared, f.Dispatch.PrepareError = in, fmt.Sprint(err)
				f.Dispatch.Mu.Unlock()
				return prepared, err
			}
		}
		turns := unwrap(d.Runner.Turns)
		originalSelect, originalScoped := turns.Select, turns.Scoped
		originalCapture := turns.Capture
		turns.Workspaces = ProductionWorkerWorkspaces{Production: turns.Workspaces}
		turns.Capture = func(ctx context.Context, scope coreadapter.Scope, view *isolation.FileView, result coreadapter.SessionResult) error {
			if ProductionWorker(scope) {
				return nil
			}
			return originalCapture(ctx, scope, view, result)
		}
		turns.Select = func(ctx context.Context, scope coreadapter.Scope) (isolation.Selection, error) {
			if !ProductionWorker(scope) {
				selection, err := originalSelect(ctx, scope)
				if scope.Role == trace.ChiefOfStaff {
					f.Dispatch.Mu.Lock()
					f.Dispatch.Scope, f.Dispatch.SelectionError = scope, fmt.Sprint(err)
					f.Dispatch.Mu.Unlock()
				}
				return selection, err
			}
			directory := filepath.Join(cfg.Root.String(), "workers", scope.Thread)
			return isolation.Selection{Workspace: coreadapter.WorkspaceRequest{SourceDirectory: directory, Directory: directory}, Narrow: &coreadapter.Capabilities{Tools: []string{questions.AskTool}}, Execution: coreadapter.ExecutionSettings{Mode: "container", Image: "fixture-image"}}, os.MkdirAll(directory, 0700)
		}
		turns.Scoped = func(ctx context.Context, scope coreadapter.Scope) ([]coreadapter.Tool, error) {
			if !ProductionWorker(scope) {
				tools, err := originalScoped(ctx, scope)
				if scope.Role == trace.ChiefOfStaff {
					f.Dispatch.Mu.Lock()
					f.Dispatch.ToolsError = fmt.Sprint(err)
					f.Dispatch.Mu.Unlock()
				}
				return tools, err
			}
			return questions.Tools(r, scope.Thread, scope, f.Clock.Now)
		}
		return d, nil
	}
}

func (f *ProductionDesign) Setup(t *testing.T, handIn bool) {
	t.Helper()
	ctx := context.Background()
	added, err := f.Client().AddProject(ctx, fixturekit.Request(f.Clone))
	Must(t, err)
	*f.Project, *f.Trace = added.Project.ID, added.Project.Trace
	f.WriteCharter(t, added.Project.ID, ShedCharter)
	main := f.Upstream(t)
	*f.Repos = append(*f.Repos, ProductionDesignRepo{*f.Project, f.Clone, "dagger/dagger", filepath.Join(filepath.Dir(f.Clone), "remotes", "dagger", "dagger.git"), main})
	home := filepath.Dir(f.Clone)
	second := filepath.Join(home, "client")
	Must(t, os.MkdirAll(filepath.Join(second, "internal", "trace"), 0700))
	fixturekit.DemoGit(t, home, "init", "--quiet", second)
	Must(t, os.WriteFile(filepath.Join(second, "internal", "trace", "git.go"), []byte("package trace\n"), 0600))
	fixturekit.DemoGit(t, home, "-C", second, "add", "internal")
	fixturekit.DemoGit(t, home, "-C", second, "-c", "user.name=Owner", "-c", "user.email=owner@example.invalid", "commit", "-qm", "base")
	fixturekit.DemoGit(t, home, "-C", second, "branch", "-M", "main")
	bare := filepath.Join(home, "remotes", "owner", "client.git")
	Must(t, os.MkdirAll(filepath.Dir(bare), 0700))
	fixturekit.DemoGit(t, home, "clone", "--quiet", "--bare", "--no-hardlinks", second, bare)
	fixturekit.DemoGit(t, home, "-C", second, "remote", "add", "upstream", bare)
	other, err := f.Client().AddRepo(ctx, api.RepoAddRequest{Project: *f.Project, Upstream: "owner/client", Clone: second})
	Must(t, err)
	for _, repo := range other.Project.Repos {
		if repo.Clone == second {
			*f.Project2 = repo.ID
		}
	}
	if *f.Project2 == "" {
		t.Fatal("second repo was not registered")
	}
	*f.Repos = append(*f.Repos, ProductionDesignRepo{*f.Project2, second, "owner/client", bare, strings.TrimSpace(fixturekit.DemoGit(t, home, "-C", bare, "rev-parse", "refs/heads/main"))})
	f.CheckRepos(t)
	if handIn {
		*f.Origin = intake.HandInWorkstream(*f.Project, "production-handin")
	} else {
		opened, err := f.Client().OpenDesign(ctx, *f.Project)
		Must(t, err)
		*f.Origin = opened.Design.Workstream
	}
	*f.Stream1 = intake.HandInWorkstream(*f.Project, "design-"+string(*f.Origin))
	*f.Stream2 = intake.HandInWorkstream(*f.Project2, "design-"+string(*f.Origin))
	f.ScriptDrafts(t)
	for i := 1; i <= 2; i++ {
		turn := intake.RoundTurnID(1, intake.CommitteeAgent(i), 1)
		f.Engine.Mu.Lock()
		f.Engine.Turns[turn] = func(ctx context.Context, req agent.Request, _ *agent.Turn, tools *mcp.ClientSession) (*agent.Result, error) {
			if *f.Stream1 == *f.Origin {
				return QuestionResult(req, "session-committee-"+req.Name, "The single-repo draft fits the charter."), nil
			}
			context, err := ReadTool(ctx, tools, "context.md")
			if err != nil {
				return nil, err
			}
			if !strings.Contains(context, "Draft project spec") || !strings.Contains(context, "acknowledgment") || !strings.Contains(context, string(*f.Stream1)) || !strings.Contains(context, string(*f.Stream2)) {
				return nil, errors.New("committee did not receive the shared draft, contracts and graph")
			}
			return QuestionResult(req, "session-committee-"+req.Name, "The proposed contract and slice fit the charter."), nil
		}
		f.Engine.Mu.Unlock()
	}
	f.Script(intake.ReplyTurnID(1, 1), nil, nil)
	f.Engine.Mu.Lock()
	f.Engine.Turns["select-repos-1"] = func(ctx context.Context, req agent.Request, verified *agent.Turn, tools *mcp.ClientSession) (*agent.Result, error) {
		if verified.VCS || req.Grants.VCS || req.Profile.VCSAccess || req.Workspace.VCS() != nil || len(req.VCSEnv) != 0 {
			return nil, errors.New("architect has mutation or VCS authority")
		}
		// The recorded selection is returned, again when it is delivered
		// again.
		var results []string
		for range 2 {
			got, err := CallTool(ctx, tools, intake.AffectedReposTool, map[string]any{"repos": []string{string(*f.Project), string(*f.Project2)}})
			if err != nil {
				return nil, err
			}
			results = append(results, got)
		}
		var selected struct {
			Recorded bool                  `json:"recorded"`
			Members  []intake.DesignMember `json:"members"`
		}
		if err := json.Unmarshal([]byte(results[0]), &selected); err != nil {
			return nil, err
		}
		if !selected.Recorded || len(selected.Members) != 2 || selected.Members[0].Repo != *f.Project || selected.Members[1].Repo != *f.Project2 || selected.Members[0].Workstream == "" || results[1] != results[0] {
			return nil, fmt.Errorf("%s returned %v", intake.AffectedReposTool, results)
		}
		return QuestionResult(req, "session-selection", "Both repos are affected."), nil
	}
	f.Engine.Turns["*"] = func(ctx context.Context, req agent.Request, _ *agent.Turn, tools *mcp.ClientSession) (*agent.Result, error) {
		names, err := ToolNames(ctx, tools)
		if err != nil {
			return nil, err
		}
		if slices.Contains(names, intake.DraftBriefToolName) {
			_, err = CallTool(ctx, tools, intake.DraftBriefToolName, map[string]any{"content": ProductionBrief})
			return QuestionResult(req, "session-design", ProductionBrief), err
		}
		return QuestionResult(req, "session-chief", "The service is following the agreed design."), nil
	}
	f.Engine.Mu.Unlock()
	if handIn {
		return
	}
	_, err = f.Client().DesignMessage(ctx, *f.Origin, "Build resumable uploads in both the server and client repos.")
	Must(t, err)
	f.Wait(t, "design reply", func() bool {
		conversation, err := f.Client().DesignConversation(ctx, *f.Origin)
		Must(t, err)
		return len(conversation.Entries) == 2 && conversation.Entries[1].State == api.TurnDone
	})
	repo, err := f.Repository(*f.Project)
	Must(t, err)
	*f.Before, err = repo.ChiefOfStaffThread(*f.Origin)
	Must(t, err)
}

// ScheduledProject exposes only the deadlines needed by the fixture clock.
type ScheduledProject struct {
	ID         config.ProjectID
	Repository *trace.Repository
	Interval   time.Duration
	NextPoll   time.Time
}

func (f *ProductionDesign) LifecycleAwait(t *testing.T, phase string, ready func() bool) {
	t.Helper()
	changes, closeChanges := f.Changes(t)
	defer closeChanges()
	deadline := time.Now().Add(DemoTimeout)
	for !ready() {
		if time.Now().After(deadline) {
			f.Timeout(t, phase)
		}
		f.LifecycleAdvanceScheduled(t)
		changes()
	}
}

func (f *ProductionDesign) LifecycleAdvanceScheduled(t *testing.T) {
	t.Helper()
	if *f.Stream1 != "" && *f.Project != "" {
		repo, err := f.Repository(*f.Project)
		Must(t, err)
		decision, found, err := repo.GroupApprovalOf(*f.Origin)
		if err != nil || !found || !decision.Complete {
			return
		}
	} else {
		return
	}
	now := f.Clock.Now()
	next := now.Add(time.Second)
	scheduled := false
	record := func(at time.Time) {
		if at.After(now) && (!scheduled || at.Before(next)) {
			next, scheduled = at, true
		}
	}
	cfg, projects := f.Scheduled()
	for _, active := range projects {
		interval := active.Interval
		record(active.NextPoll)
		streams, err := active.Repository.Workstreams()
		Must(t, err)
		for _, stream := range streams {
			transitions, err := trace.Read[trace.Transition](active.Repository, stream)
			if err == nil {
				for _, transition := range slices.Backward(transitions) {
					if transition.Subject == "base-refresh" {
						record(transition.At.Add(interval))
						break
					}
				}
			}
			revision, at, err := f.EdgeObservationAt(active.Repository, stream)
			if err == nil && revision > 0 {
				if attempt, ok := f.LastEdgePoll(active.ID, stream); ok && attempt.After(at) {
					at = attempt
				}
				record(at.Add(interval))
			}
			ops, err := active.Repository.Operations(stream)
			if err != nil {
				continue
			} // A concurrently moved trace is not a deadline.
			for _, op := range ops {
				if !op.Acknowledged {
					record(op.RetryAt)
				}
			}
			outbox, err := active.Repository.Outbox(stream)
			if err != nil {
				continue
			}
			for _, e := range outbox {
				if e.Acknowledged || e.Event.Operation != nil || e.Claim != nil {
					continue
				}
				window := cfg.EventWindow()
				if e.Event.Kind == trace.ProgressKind {
					window = cfg.EventProgressWindow()
				}
				record(e.At.Add(window))
			}
		}
	}
	f.Clock.Advance(next.Sub(now))
}

func (f *ProductionDesign) LifecycleWorker(t *testing.T, project config.ProjectID, stream config.WorkstreamID, worker, turn, prompt string) {
	t.Helper()
	repo, err := f.Repository(project)
	Must(t, err)
	threads, err := repo.Threads(stream)
	Must(t, err)
	if !slices.ContainsFunc(threads, func(th trace.Thread) bool { return th.Identity.ID == worker }) {
		h := trace.Header{Schema: "osmia.trace.agent", Version: 1, Revision: 1, ID: worker, Project: project, Workstream: stream, At: f.Clock.Now(), Actor: fixturekit.OwnerActor, Cause: "workstream_created"}
		Must(t, repo.CreateThread(context.Background(), trace.Agent{Header: h, Role: MasonRole, ThreadID: worker}))
	}
	h := trace.Header{Schema: "osmia.trace.turn-request", Version: 1, Revision: 1, ID: "request_" + turn + "_" + worker, Project: project, Workstream: stream, At: f.Clock.Now(), Actor: fixturekit.OwnerActor, Cause: "workstream_created"}
	_, err = repo.EnqueueTurn(context.Background(), trace.TurnRequest{Header: h, AgentID: worker, ThreadID: worker, TurnID: turn, Profile: coreadapter.Profile{Name: "default", Backend: "claude", Model: "test"}, SystemPrompt: "You are a member mason.", Prompt: prompt})
	Must(t, err)
}

// DesignInterruption pauses one durable checkpoint until that service lifetime ends.
type DesignInterruption struct {
	Step     string
	Nth      int32
	Count    atomic.Int32
	Lifetime context.Context
	Reached  chan struct{}
}

func DesignBoundary(hooks *atomic.Pointer[DesignInterruption], checkpoint func(context.Context, string) error) func(context.Context, string) error {
	return func(lifetime context.Context, step string) error {
		hook := hooks.Load()
		if hook != nil && hook.Step == step && hook.Count.Add(1) == hook.Nth {
			hook.Lifetime = lifetime
			close(hook.Reached)
			<-lifetime.Done()
			return lifetime.Err()
		}
		if checkpoint != nil {
			return checkpoint(lifetime, step)
		}
		return nil
	}
}

// ProductionDesignStart supplies only the test adapter's service bindings.
type ProductionDesignStart[F any] struct {
	Fixture     F
	Scenario    *ProductionDesign
	Threads     func(*trace.Repository, *config.Config) (coreadapter.Reconciler, error)
	Unwrap      func(coreadapter.Turns) *isolation.Turns
	Hooks       *atomic.Pointer[DesignInterruption]
	Bind        func(func(*trace.Repository, *config.Config) (coreadapter.Reconciler, error), func(context.Context, string) error)
	Start, Stop func(*testing.T)
	Running     func() bool
}

// NewProductionDesign prepares and starts the real-service design scenario.
func NewProductionDesign[F any](t *testing.T, create func(*fixturekit.FixedClock) ProductionDesignStart[F], handIn, customWait bool, checkpoint func(context.Context, string) error) F {
	t.Helper()
	f := create(&fixturekit.FixedClock{At: fixturekit.DemoStart})
	f.Bind(f.Scenario.BindProductionDesign(f.Threads, f.Unwrap), DesignBoundary(f.Hooks, checkpoint))
	if customWait {
		t.Cleanup(func() {
			if f.Running() {
				f.Stop(t)
			}
		})
	}
	f.Start(t)
	f.Scenario.Setup(t, handIn)
	return f.Fixture
}

// ScheduledProjects adapts service runtimes to the shared lifecycle clock driver.
func ScheduledProjects[P any](projects []P, inspect func(P) ScheduledProject) []ScheduledProject {
	var out []ScheduledProject
	for _, p := range projects {
		out = append(out, inspect(p))
	}
	return out
}

// TryLanding exercises the production landing boundary with fixture input.
func TryLanding(ctx context.Context, repository *trace.Repository, stream config.WorkstreamID, event, action string, input any, apply func(context.Context, coreadapter.Operation) (coreadapter.OperationResult, error)) (coreadapter.OperationResult, error) {
	data, err := json.Marshal(input)
	if err != nil {
		return coreadapter.OperationResult{}, err
	}
	return apply(ctx, coreadapter.Operation{ID: trace.OperationID(repository.Project(), stream, event), Boundary: coreadapter.RepositoryBoundary, Action: action, Input: data})
}
