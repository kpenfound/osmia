package executionjourney

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"github.com/kpenfound/osmia/internal/api"
	"github.com/kpenfound/osmia/internal/delivery"
	"github.com/kpenfound/osmia/internal/execution"
	"github.com/kpenfound/osmia/internal/servicetest/intakefixture"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kpenfound/agentcore/agent"
	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/intake"
	"github.com/kpenfound/osmia/internal/projectstore"
	"github.com/kpenfound/osmia/internal/seal"
	"github.com/kpenfound/osmia/internal/service"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	"github.com/kpenfound/osmia/internal/thread"
	"github.com/kpenfound/osmia/internal/trace"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type pathGate struct {
	entered   chan struct{}
	release   chan struct{}
	once      sync.Once
	enterOnce sync.Once
}

func newPathGate(t *testing.T) *pathGate {
	t.Helper()
	g := &pathGate{entered: make(chan struct{}), release: make(chan struct{})}
	t.Cleanup(g.open)
	return g
}
func (g *pathGate) open() { g.once.Do(func() { close(g.release) }) }
func (g *pathGate) hold(ctx context.Context) error {
	g.enterOnce.Do(func() { close(g.entered) })
	select {
	case <-g.release:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
func pathAwait(t *testing.T, s *Service, what string, ready func() bool) {
	t.Helper()
	wait, stop := serviceChanges(t, s)
	defer stop()
	deadline := time.Now().Add(fixturekit.DemoTimeout)
	for !ready() {
		if time.Now().After(deadline) {
			t.Fatal(what)
		}
		wait()
	}
}
func pathTurnDone(repo *trace.Repository, stream config.WorkstreamID, agent, turn string) bool {
	th, err := repo.Thread(stream, agent)
	if err != nil {
		return false
	}
	for _, q := range th.Turns {
		if q.Request.TurnID == turn {
			return q.Claim != nil && q.Response != nil && q.Response.Result.FinalResponse != "" && !q.CompletedAt.IsZero()
		}
	}
	return false
}
func pathAcknowledged(repo *trace.Repository, stream config.WorkstreamID, turn string) bool {
	records, _ := repo.Operations(stream)
	for _, r := range records {
		in, err := thread.DecodeTurn(r.Operation)
		if err == nil && in.Turn == turn {
			return r.Acknowledged
		}
	}
	return false
}

func TestSessionRecoveryQueueSnapshotCannotRestoreWorking(t *testing.T) {
	t.Parallel()
	opts, cfg := conversationFixture(t, "session-snapshot-")
	ctx := context.Background()
	session := config.WorkstreamID("w_00000000000000000000000000000119")
	repo, err := trace.Open(cfg.Root, cfg.Project)
	fixturekit.Must(t, err)
	fixturekit.Must(t, repo.CreateSessionWorkstream(ctx, session, "default", "base", "snapshot", "Session", fixturekit.DemoStart, intake.OwnerActor))
	_, err = service.TestEnsureSessionThread(ctx, repo, cfg.Project.ID, session, fixturekit.DemoStart)
	fixturekit.Must(t, err)
	profile, err := cfg.NamedProfile("default")
	fixturekit.Must(t, err)
	_, err = repo.EnqueueTurn(ctx, trace.TurnRequest{Header: trace.Header{Schema: "osmia.trace.turn-request", Version: 1, Revision: 1, ID: "request_crashed", Project: cfg.Project.ID, Workstream: session, At: fixturekit.DemoStart, Actor: intake.OwnerActor, Cause: "owner-message"}, AgentID: service.TestSessionAgentID, ThreadID: service.TestSessionAgentID, TurnID: "crashed", Profile: profile, Prompt: "Continue"})
	fixturekit.Must(t, err)
	fixturekit.Must(t, service.TestSetSessionState(ctx, repo, cfg.Project.ID, session, fixturekit.DemoStart, "session-working-crashed", "session-turn-started", trace.SessionWorking))
	_, err = repo.ClaimTurn(ctx, session, service.TestSessionAgentID, "previous-service", "previous-session", fixturekit.DemoStart)
	fixturekit.Must(t, err)
	fixturekit.Must(t, repo.Close())
	backend, scan := newPathGate(t), newPathGate(t)
	var armed atomic.Bool
	var scans atomic.Int32
	settled := make(chan struct{})
	var calls atomic.Int32
	opts.boundary = func(ctx context.Context, step string) error {
		if step == "conversation-queue-read:"+string(session) && armed.Load() {
			if scans.Add(1) == 1 {
				return scan.hold(ctx)
			}
			select {
			case <-settled:
			default:
				close(settled)
			}
			return nil
		}
		return nil
	}
	opts.Threads = func(r *trace.Repository, _ *config.Config) (coreadapter.Reconciler, error) {
		return thread.Dispatcher{Runner: thread.Runner{Jev: fixturekit.UnconfiguredJudge(nil), Store: r, Now: opts.Reconciliation.Now, Turns: fixturekit.TurnsFunc(func(ctx context.Context, p coreadapter.PreparedTurn) (coreadapter.SessionResult, error) {
			calls.Add(1)
			if err := backend.hold(ctx); err != nil {
				return coreadapter.SessionResult{}, err
			}
			return coreadapter.SessionResult{Session: coreadapter.BackendSession{Backend: p.Profile.Backend, ID: "recovered-session"}, FinalResponse: "Recovered reply"}, nil
		})}, Prepare: func(context.Context, thread.TurnInput) (coreadapter.PreparedTurn, error) {
			return coreadapter.PreparedTurn{SessionDirectory: t.TempDir()}, nil
		}, Relay: func(ctx context.Context, th trace.Thread, q trace.QueuedTurn) error {
			return service.TestSessionRelay(ctx, r, opts.Reconciliation.Now, th, q)
		}}, nil
	}
	s, _ := start(t, opts)
	fixturekit.AwaitPollSignal(t, backend.entered)
	armed.Store(true)
	InspectForTest(s).WakeConversations()
	fixturekit.AwaitPollSignal(t, scan.entered)
	backend.open()
	pathAwait(t, s, "recovery did not acknowledge", func() bool { return pathAcknowledged(s.sole().repository, session, "session-recover-1") })
	scan.open()

	InspectForTest(s).WakeConversations()
	fixturekit.AwaitPollSignal(t, settled)
	armed.Store(false)
	fixturekit.Must(t, s.Close())
	repo, err = trace.Open(cfg.Root, cfg.Project)
	fixturekit.Must(t, err)
	defer repo.Close()
	state, err := repo.Workflow(session, trace.SessionSubject)
	fixturekit.Must(t, err)
	if state.Value != trace.SessionIdle || calls.Load() != 1 {
		t.Fatalf("recovery state=%s backend calls=%d", state.Value, calls.Load())
	}
	th, err := repo.Thread(session, service.TestSessionAgentID)
	fixturekit.Must(t, err)
	if len(th.Turns) != 2 || th.Turns[0].Status() != "interrupted" || th.Turns[1].CompletedAt.IsZero() || th.Turns[1].Status() != "idle" {
		t.Fatalf("recovery turns: %+v", th.Turns)
	}
}

type pathLog struct {
	mu      sync.Mutex
	records []slog.Record
}

func (*pathLog) Enabled(context.Context, slog.Level) bool { return true }
func (l *pathLog) Handle(_ context.Context, r slog.Record) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.records = append(l.records, r.Clone())
	return nil
}
func (l *pathLog) WithAttrs([]slog.Attr) slog.Handler { return l }
func (l *pathLog) WithGroup(string) slog.Handler      { return l }
func (l *pathLog) steps(project config.ProjectID, name string) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	count := 0
	for _, r := range l.records {
		if r.Message != "scheduling step exceeded threshold" {
			continue
		}
		attrs := map[string]string{}
		r.Attrs(func(a slog.Attr) bool { attrs[a.Key] = a.Value.String(); return true })
		d, e := time.ParseDuration(attrs["duration"])
		if attrs["project"] == string(project) && attrs["step"] == name && e == nil && d > 0 {
			count++
		}
	}
	return count
}

func TestServiceHeldLandingAndStepAllowEveryConversation(t *testing.T) {
	t.Parallel()
	f, a, repo := newApprovedFixture(t, "held-path")
	fixturekit.Must(t, repo.Close())
	landing, step := newPathGate(t), newPathGate(t)
	var armed atomic.Bool
	logs := &pathLog{}
	f.opts.Logger = slog.New(logs)
	f.opts.Reconciliation.SlowThreshold = time.Nanosecond
	f.opts.boundary = func(ctx context.Context, name string) error {
		if name == "land-committing" {
			return landing.hold(ctx)
		}
		if name == "schedule:factory-policy" && armed.Load() {
			return step.hold(ctx)
		}
		return nil
	}
	f.opts.Reconciliation.Hold = func(_ config.WorkstreamID, op coreadapter.Operation) bool {
		return op.Action != service.LandAction && !service.TestConversationOperation(op)
	}
	f.start(t)
	defer f.stop(t)
	fixturekit.AwaitPollSignal(t, landing.entered)
	design, err := f.c.OpenDesign(context.Background(), f.project)
	fixturekit.Must(t, err)
	session, err := f.c.OpenSession(context.Background(), api.SessionOpenRequest{Project: f.project, Profile: "default"})
	fixturekit.Must(t, err)
	armed.Store(true)
	f.repository().WakeWorkflow()
	fixturekit.AwaitPollSignal(t, step.entered)
	chief, err := f.c.Send(context.Background(), a, "Chief message")
	fixturekit.Must(t, err)
	designed, err := f.c.DesignMessage(context.Background(), design.Design.Workstream, "Design message")
	fixturekit.Must(t, err)
	messaged, err := f.c.SessionMessage(context.Background(), session.Session.Workstream, "Session message")
	fixturekit.Must(t, err)
	for _, q := range []struct {
		stream      config.WorkstreamID
		agent, turn string
	}{{a, trace.ChiefOfStaff, chief.Turn}, {design.Design.Workstream, trace.ChiefOfStaff, designed.Turn}, {session.Session.Workstream, service.TestSessionAgentID, messaged.Turn}} {
		pathAwait(t, f.s, "conversation waited for landing or scheduling", func() bool { return pathTurnDone(f.repository(), q.stream, q.agent, q.turn) })
	}
	select {
	case <-landing.release:
		t.Fatal("landing released early")
	default:
	}
	select {
	case <-step.release:
		t.Fatal("scheduling released early")
	default:
	}
	logged := logs.steps(f.project, "factory-policy")
	step.open()
	pathAwait(t, f.s, "slow scheduling step not logged", func() bool { return logs.steps(f.project, "factory-policy") > logged })
	landing.open()
}

func TestServiceIndependentLandingDraftAndChat(t *testing.T) {
	t.Parallel()
	f, a, repo := newApprovedFixture(t, "path-a")
	c := approvedStream(t, f, repo, "path-c", config.WorkspacesGit)
	fixturekit.Must(t, repo.Close())
	landing, draft := newPathGate(t), newPathGate(t)
	var allowC atomic.Bool
	f.opts.Reconciliation.Hold = func(stream config.WorkstreamID, op coreadapter.Operation) bool {
		if service.TestConversationOperation(op) || op.Action == service.DraftAction {
			return false
		}
		return op.Action != service.LandAction || (stream == c && !allowC.Load())
	}
	var landCalls atomic.Int32
	f.opts.boundary = func(ctx context.Context, name string) error {
		if name == "land-committing" && landCalls.Add(1) == 1 {
			return landing.hold(ctx)
		}
		return nil
	}
	f.script("draft-1-1", nil, func(ctx context.Context, _ agent.Request, _ *agent.Turn, _ *mcp.ClientSession) error {
		return draft.hold(ctx)
	})
	f.start(t)
	defer f.stop(t)
	fixturekit.AwaitPollSignal(t, landing.entered)
	b := f.handIn(t, "path-b", fixturekit.HandedDesign)
	fixturekit.AwaitPollSignal(t, draft.entered)
	allowC.Store(true)
	f.repository().WakeWorkflow()
	pathAwait(t, f.s, "C did not merge beside A and B", func() bool {
		state, _ := f.repository().Workflow(c, trace.UnitSubject("resume"))
		return state.Value == intake.UnitMerged
	})
	sent, err := f.c.Send(context.Background(), a, "Reply while A and B are held")
	fixturekit.Must(t, err)
	pathAwait(t, f.s, "chief did not reply", func() bool { return pathTurnDone(f.repository(), a, trace.ChiefOfStaff, sent.Turn) })
	state, err := f.repository().Workflow(a, trace.UnitSubject("resume"))
	fixturekit.Must(t, err)
	if state.Value != intake.UnitApproved {
		t.Fatalf("A=%s", state.Value)
	}
	state, err = f.repository().Workflow(b, trace.FeatureSubject)
	fixturekit.Must(t, err)
	if state.Value != intake.HandedState {
		t.Fatalf("B=%s", state.Value)
	}
	landing.open()
	draft.open()
}

func TestServiceTwoWorkstreamsLandAtOnce(t *testing.T) {
	t.Parallel()
	f, a, repo := newApprovedFixture(t, "two-land-a")
	b := approvedStream(t, f, repo, "two-land-b", config.WorkspacesGit)
	fixturekit.Must(t, repo.Close())
	gate := newPathGate(t)
	entered := make(chan struct{}, 2)
	f.opts.Reconciliation.Hold = func(_ config.WorkstreamID, op coreadapter.Operation) bool {
		return op.Action != service.LandAction && !service.TestConversationOperation(op)
	}
	f.opts.boundary = func(ctx context.Context, name string) error {
		if name == "land-committing" {
			entered <- struct{}{}
			return gate.hold(ctx)
		}
		return nil
	}
	f.start(t)
	defer f.stop(t)
	fixturekit.AwaitPollSignal(t, entered)
	fixturekit.AwaitPollSignal(t, entered)
	gate.open()
	for _, stream := range []config.WorkstreamID{a, b} {
		pathAwait(t, f.s, "concurrent landing did not merge", func() bool {
			state, _ := f.repository().Workflow(stream, trace.UnitSubject("resume"))
			return state.Value == intake.UnitMerged
		})
	}
}

func TestServiceTwoMasonsRunDuringOwnLanding(t *testing.T) {
	t.Parallel()
	f, _ := newParallelMasonFixture(t, 2, 2, disjointPlan)
	base := strings.TrimSpace(fixturekit.DemoGit(t, f.clone, "-C", f.clone, "rev-parse", "HEAD"))
	stream, repo := seedBuild(t, f, "worker-workflow", disjointPlan, config.WorkspacesGit, base)
	seedReview(t, f, repo, stream, "resume", resumeReport)
	approveDirectly(t, f.s, repo, stream, "resume", "spec#1")
	fixturekit.Must(t, repo.Close())
	landing, workers := newPathGate(t), newPathGate(t)
	entered := make(chan struct{}, 2)
	var active, peak atomic.Int32
	f.engine.Mu.Lock()
	for _, unit := range []string{"upload", "audit"} {
		f.engine.Turns[service.TestMasonTurnID(unit)] = func(ctx context.Context, req agent.Request, _ *agent.Turn, _ *mcp.ClientSession) (*agent.Result, error) {
			n := active.Add(1)
			defer active.Add(-1)
			for old := peak.Load(); n > old; old = peak.Load() {
				if peak.CompareAndSwap(old, n) {
					break
				}
			}
			entered <- struct{}{}
			if err := workers.hold(ctx); err != nil {
				return nil, err
			}
			return &agent.Result{ClaudeID: "session-" + req.Name, ResultText: "Built", SessionDir: req.SessionDir}, nil
		}
	}
	f.engine.Mu.Unlock()
	var allowWorkers atomic.Bool
	f.opts.Reconciliation.Hold = func(_ config.WorkstreamID, op coreadapter.Operation) bool {
		if service.TestConversationOperation(op) || op.Action == service.LandAction {
			return false
		}
		return !allowWorkers.Load()
	}
	f.opts.boundary = func(ctx context.Context, name string) error {
		if name == "land-committing" {
			return landing.hold(ctx)
		}
		return nil
	}
	f.start(t)
	defer f.stop(t)
	fixturekit.AwaitPollSignal(t, landing.entered)
	allowWorkers.Store(true)
	f.repository().WakeWorkflow()
	fixturekit.AwaitPollSignal(t, entered)
	fixturekit.AwaitPollSignal(t, entered)
	if active.Load() != 2 || peak.Load() != 2 {
		t.Fatalf("mason concurrency active=%d peak=%d", active.Load(), peak.Load())
	}
	if used := InspectForTest(f.s).Pool().Slots(nil).Used[intake.MasonRole]; used != 2 {
		t.Fatalf("mason capacity=%d", used)
	}
	for _, unit := range []string{"upload", "audit"} {
		th, err := f.repository().Thread(stream, intake.MasonAgent(unit))
		fixturekit.Must(t, err)
		if len(th.Turns) != 1 || th.Turns[0].Claim == nil || !th.Turns[0].CompletedAt.IsZero() {
			t.Fatalf("mason %s not running: %+v", unit, th)
		}
	}
	workers.open()
	landing.open()
}

func TestServiceSealAndBuildEffectsSurviveRestart(t *testing.T) {
	t.Parallel()
	for _, point := range []string{"seal-recorded", "build-recorded"} {
		t.Run(point, func(t *testing.T) {
			t.Parallel()
			gate := newPathGate(t)
			f := newDebateFixtureWith(t, 1, 1, "", func(o *Options) {
				o.boundary = func(ctx context.Context, name string) error {
					if name == point {
						return gate.hold(ctx)
					}
					return nil
				}
			})
			f.upstream(t)
			stream, _ := f.scenario().Ratified(t)
			fixturekit.AwaitPollSignal(t, gate.entered)
			f.stop(t)
			f.opts.boundary = nil
			fixturekit.Jump(f.clock, f.clock.Now().Add(2*time.Second))
			f.start(t)
			pathAwait(t, f.s, "seal/build did not reconcile", func() bool {
				state, _ := f.repository().Workflow(stream, trace.FeatureSubject)
				if state.Value != intake.BuildingState {
					return false
				}
				records, _ := f.repository().Operations(stream)
				var seals, builds int
				for _, r := range records {
					if r.Operation.Action == service.SealAction && r.Acknowledged {
						seals++
					}
					if r.Operation.Action == service.BuildAction && r.Acknowledged {
						builds++
					}
				}
				return seals == 1 && builds == 1
			})
			assertEffects := func() {
				t.Helper()
				docs := intakefixture.StreamDocuments(t, f.repository(), stream, seal.DocumentID)
				if len(docs) != 1 {
					t.Fatalf("seal revisions=%d", len(docs))
				}
				transitions, err := trace.Read[trace.Transition](f.repository(), stream)
				fixturekit.Must(t, err)
				seen := map[string]int{}
				for _, tr := range transitions {
					if tr.Actor == intake.BuildingActor || tr.Actor == intake.SealingActor {
						seen[tr.ID]++
					}
				}
				for id, n := range seen {
					if n != 1 {
						t.Fatalf("effect %s recorded %d times", id, n)
					}
				}
				for _, id := range []string{intake.BuildingState, "unit-resume-planned", "unit-resume-ready", "unit-dedupe-planned"} {
					if seen[id] != 1 {
						t.Fatalf("missing build effect %s: %v", id, seen)
					}
				}
			}
			assertEffects()
			f.stop(t)
			pathStartAndPass(t, f)
			defer f.stop(t)
			assertEffects()
		})
	}
}

func TestServiceLandingRestartPreservesCompletedCloneWork(t *testing.T) {
	t.Parallel()
	for _, backend := range []string{config.WorkspacesGit, config.WorkspacesJujutsu} {
		t.Run(backend, func(t *testing.T) {
			if backend == config.WorkspacesJujutsu {
				intakefixture.RequireJJ(t)
			}
			t.Parallel()
			f, a, repo := newApprovedFixtureOn(t, "restart-a", backend)
			b := approvedStream(t, f, repo, "restart-b", backend)
			fixturekit.Must(t, repo.Close())
			remote := newPathGate(t)
			var allowB atomic.Bool
			f.opts.Reconciliation.Hold = func(stream config.WorkstreamID, op coreadapter.Operation) bool {
				if service.TestConversationOperation(op) {
					return false
				}
				if op.Action != service.LandAction {
					return true
				}
				in, err := execution.DecodeLand(op)
				return err != nil || in.Unit != "resume" || (stream == b && !allowB.Load())
			}
			gate := newPathGate(t)
			var armed atomic.Bool
			f.opts.boundary = func(ctx context.Context, name string) error {
				if name == "land-local-step:"+string(a) {
					return remote.hold(ctx)
				}
				if armed.Load() && name == "land-advanced" {
					return gate.hold(ctx)
				}
				return nil
			}
			f.start(t)
			fixturekit.AwaitPollSignal(t, remote.entered)
			allowB.Store(true)
			f.repository().WakeWorkflow()
			pathAwait(t, f.s, "B did not finish before A", func() bool {
				records := landOperations(t, f.repository(), b)
				return len(records) == 1 && records[0].Acknowledged
			})
			beforeRecords, err := json.Marshal(landOperations(t, f.repository(), b))
			fixturekit.Must(t, err)
			bTip := featureTip(t, f, b)
			g := intakefixture.ProviderOf(t, intake.FeatureWorkspaces(f.s.cfg, f.repository()), b)
			bWorkspace, found, err := g.Workspace(context.Background(), string(b))
			fixturekit.Must(t, err)
			if !found {
				t.Fatal("B workspace missing")
			}
			bFile, err := os.ReadFile(filepath.Join(bWorkspace.Path, intakefixture.TrackedFile))
			fixturekit.Must(t, err)

			if backend == config.WorkspacesJujutsu {

				f.stop(t)
				crashAt := f.clock.Now().Add(time.Hour)
				pathCrashJJService(t, f.opts.Config.Root, crashAt)
				fixturekit.Jump(f.clock, crashAt.Add(time.Hour))
				if len(checkpoints(t, f, intake.BranchesDirectory)) != 1 {
					t.Fatal("crash left no outstanding local-step checkpoint")
				}
			} else {
				armed.Store(true)
				remote.open()
				f.repository().WakeWorkflow()
				fixturekit.AwaitPollSignal(t, gate.entered)
				f.stop(t)
			}
			f.opts.boundary = nil
			fixturekit.Jump(f.clock, f.clock.Now().Add(2*time.Second))
			f.start(t)
			pathAwait(t, f.s, "A landing did not recover", func() bool {
				records := landOperations(t, f.repository(), a)
				return len(records) == 1 && records[0].Acknowledged
			})
			f.stop(t)
			repo, err = trace.Open(f.s.cfg.Root, f.s.cfg.Project)
			fixturekit.Must(t, err)
			defer repo.Close()
			afterRecords, err := json.Marshal(landOperations(t, repo, b))
			fixturekit.Must(t, err)
			if !bytes.Equal(beforeRecords, afterRecords) || featureTip(t, f, b) != bTip {
				t.Fatal("B's completed operation or branch was changed by A recovery")
			}
			afterFile, err := os.ReadFile(filepath.Join(bWorkspace.Path, intakefixture.TrackedFile))
			fixturekit.Must(t, err)
			if !bytes.Equal(bFile, afterFile) {
				t.Fatal("B's workspace changed")
			}
			if backend == config.WorkspacesJujutsu {
				intakefixture.OnCommit(t, g, bWorkspace, bTip)
				if paths := checkpoints(t, f, intake.BranchesDirectory); len(paths) != 0 {
					t.Fatalf("outstanding checkpoints after recovery: %v", paths)
				}
			} else {
				if head := strings.TrimSpace(fixturekit.DemoGit(t, f.clone, "-C", bWorkspace.Path, "rev-parse", "HEAD")); head != bTip {
					t.Fatalf("B workspace head=%s, want %s", head, bTip)
				}
				if status := strings.TrimSpace(fixturekit.DemoGit(t, f.clone, "-C", bWorkspace.Path, "status", "--porcelain")); status != "" {
					t.Fatalf("B workspace dirty: %s", status)
				}
			}
			for _, stream := range []config.WorkstreamID{a, b} {
				records := landOperations(t, repo, stream)
				if len(records) != 1 || records[0].Result == nil || records[0].Result.Outcome != "succeeded" {
					t.Fatalf("landing results: %+v", records)
				}
				docs := intakefixture.StreamDocuments(t, repo, stream, execution.LandingDocument("resume"))
				if len(docs) != 1 {
					t.Fatalf("landing effects=%d", len(docs))
				}
				in, err := execution.DecodeLand(records[0].Operation)
				fixturekit.Must(t, err)
				if commits := f.landedCommits(t, stream, in.Base); len(commits) != 1 {
					t.Fatalf("landing commits: %v", commits)
				}
				state, err := repo.Workflow(stream, trace.UnitSubject("resume"))
				fixturekit.Must(t, err)
				if state.Value != intake.UnitMerged {
					t.Fatalf("unit state=%s", state.Value)
				}
			}
		})
	}
}

func TestServicePublicationEffectsSurviveRestart(t *testing.T) {
	t.Parallel()
	for _, point := range []string{"ready-marked"} {
		t.Run(point, func(t *testing.T) {
			t.Parallel()
			p := newUnpushedPublicationFixture(t, "commit-per-unit", "")
			fixturekit.DemoGit(t, filepath.Dir(p.fork), "-C", p.fork, "config", "core.logAllRefUpdates", "true")
			p.publishDraft(t)
			p.approve(t, nil)
			fixturekit.Must(t, p.repository.Close())
			gate := newPathGate(t)
			p.opts.PullRequests = p.pulls
			p.opts.boundary = func(ctx context.Context, name string) error {
				if name == point {
					return gate.hold(ctx)
				}
				return nil
			}
			p.start(t)
			fixturekit.AwaitPollSignal(t, gate.entered)
			pushed, found := p.forkBranch(t)
			if !found || pushed != p.pushed {
				t.Fatal("push did not reach fork")
			}
			p.stop(t)
			p.opts.boundary = nil
			fixturekit.Jump(p.clock, p.clock.Now().Add(2*time.Second))
			p.start(t)
			pathAwait(t, p.s, "publication did not recover", func() bool {
				records, _ := p.shedFixture.repository().Operations(p.stream)
				for _, r := range records {
					if r.Operation.Action == delivery.PublishAction {
						return r.Acknowledged && r.Result != nil && r.Result.Outcome == "succeeded"
					}
				}
				return false
			})
			p.stop(t)
			pathStartAndPass(t, p.shedFixture)
			p.stop(t)
			if tip, _ := p.forkBranch(t); tip != pushed {
				t.Fatalf("published commit changed %s -> %s", pushed, tip)
			}
			updates := strings.Fields(fixturekit.DemoGit(t, filepath.Dir(p.fork), "-C", p.fork, "reflog", "show", "--format=%H", "refs/heads/"+intake.FeatureBranch(p.stream)))
			if len(updates) != 1 || updates[0] != pushed {
				t.Fatalf("push effects: %v", updates)
			}
			p.pulls.Mu.Lock()
			defer p.pulls.Mu.Unlock()
			if p.pulls.Creates != 1 || p.pulls.Updates != 0 || p.pulls.Readies != 1 || len(p.pulls.Prs) != 1 {
				t.Fatalf("PR effects creates=%d updates=%d readies=%d PRs=%d", p.pulls.Creates, p.pulls.Updates, p.pulls.Readies, len(p.pulls.Prs))
			}
			repo, err := trace.Open(p.s.cfg.Root, p.s.cfg.Project)
			fixturekit.Must(t, err)
			defer repo.Close()
			pubs, err := delivery.Publications(repo, p.stream)
			fixturekit.Must(t, err)
			if len(pubs) != 2 || pubs[1].Status != delivery.PublicationOpened || pubs[1].Commit != pushed {
				t.Fatalf("publication effects: %+v", pubs)
			}
		})
	}
}

func TestServiceRebaseEffectSurvivesRestart(t *testing.T) {
	t.Parallel()
	f, stream, repo := newRebaseFixture(t, "service-rebase")
	units := newUnitWorkspaces(f.s.cfg, repo)
	w, _, found, err := units.find(context.Background(), stream, "resume")
	fixturekit.Must(t, err)
	if !found {
		t.Fatal("unit workspace missing")
	}
	fixturekit.Must(t, os.WriteFile(filepath.Join(w.Path, "RESUME.md"), []byte("resume\n"), 0600))
	landed := moveFeature(t, f, stream, map[string]string{"LANDED.md": "landed\n"})
	fixturekit.Must(t, repo.Close())
	gate := newPathGate(t)
	f.opts.Reconciliation.Hold = func(_ config.WorkstreamID, op coreadapter.Operation) bool {
		if service.TestConversationOperation(op) {
			return false
		}
		if op.Action != service.RebaseAction {
			return true
		}
		in, err := execution.DecodeRebase(op)
		return err != nil || in.Unit != "resume"
	}
	f.opts.boundary = func(ctx context.Context, name string) error {
		if name == "rebase-recorded" {
			return gate.hold(ctx)
		}
		return nil
	}
	f.start(t)
	fixturekit.AwaitPollSignal(t, gate.entered)
	f.stop(t)
	f.opts.boundary = nil
	fixturekit.Jump(f.clock, f.clock.Now().Add(2*time.Second))
	f.start(t)
	pathAwait(t, f.s, "rebase did not recover", func() bool {
		records := rebaseOperations(t, f.repository(), stream, "resume")
		return len(records) == 1 && records[0].Acknowledged
	})
	f.stop(t)
	pathStartAndPass(t, f)
	f.stop(t)
	repo, err = trace.Open(f.s.cfg.Root, f.s.cfg.Project)
	fixturekit.Must(t, err)
	defer repo.Close()
	rebases := unitRebases(t, repo, stream, "resume")
	if len(rebases) != 1 || rebases[0].Onto != landed {
		t.Fatalf("rebase effects: %+v", rebases)
	}
	for file, want := range map[string]string{"RESUME.md": "resume\n", "LANDED.md": "landed\n"} {
		data, err := os.ReadFile(filepath.Join(w.Path, file))
		fixturekit.Must(t, err)
		if string(data) != want {
			t.Fatalf("workspace %s=%q", file, data)
		}
	}
	if count := strings.TrimSpace(fixturekit.DemoGit(t, filepath.Dir(f.clone), "-C", f.clone, "rev-list", "--count", landed+".."+execution.UnitBranch(stream, "resume"))); count != "1" {
		t.Fatalf("rebase commits=%s", count)
	}
}

func TestServiceDriftEffectSurvivesRestart(t *testing.T) {
	t.Parallel()
	f, stream, repo, _ := newFinalFixture(t, "service-drift")
	before := moveFeature(t, f, stream, map[string]string{"FEATURE.md": "feature\n"})
	upstream := advanceUpstream(t, f, map[string]string{"UPSTREAM.md": "upstream\n"})
	fixturekit.Must(t, repo.Close())
	gate := newPathGate(t)
	var replays atomic.Int32
	f.opts.Reconciliation.Hold = func(_ config.WorkstreamID, op coreadapter.Operation) bool {
		return op.Action != service.DriftAction && !service.TestConversationOperation(op)
	}
	f.opts.boundary = func(ctx context.Context, name string) error {
		if name == "drift-replaying" {
			replays.Add(1)
		}
		if name == "drift-recorded" {
			return gate.hold(ctx)
		}
		return nil
	}
	f.start(t)
	_, err := f.c.RebaseWorkstream(context.Background(), stream)
	fixturekit.Must(t, err)
	fixturekit.AwaitPollSignal(t, gate.entered)
	f.stop(t)
	f.opts.boundary = func(_ context.Context, name string) error {
		if name == "drift-replaying" {
			replays.Add(1)
		}
		return nil
	}
	fixturekit.Jump(f.clock, f.clock.Now().Add(2*time.Second))
	f.start(t)
	pathAwait(t, f.s, "drift did not recover", func() bool {
		records := driftOperations(t, f.repository(), stream)
		return len(records) == 1 && records[0].Acknowledged
	})
	f.stop(t)
	pathStartAndPass(t, f)
	f.stop(t)
	repo, err = trace.Open(f.s.cfg.Root, f.s.cfg.Project)
	fixturekit.Must(t, err)
	defer repo.Close()
	records := driftRecords(t, repo, stream)
	if len(records) != 2 || records[0].Outcome != service.TestDriftReplayed || records[1].Outcome != service.TestDriftRebased || replays.Load() != 1 {
		t.Fatalf("drift effects=%+v replays=%d", records, replays.Load())
	}
	if tip := featureTip(t, f, stream); tip == before || parentOf(t, f, tip) != upstream {
		t.Fatalf("drift tip=%s upstream=%s", tip, upstream)
	}
	if seals := intakefixture.Seals(t, repo, stream); len(seals) != 2 || seals[1].Base.Commit != upstream {
		t.Fatalf("drift seals=%+v", seals)
	}
}

func pathStartAndPass(t *testing.T, f *shedFixture) {
	t.Helper()
	previous := f.opts.schedulePassed
	passed := make(chan struct{})
	var once sync.Once
	f.opts.schedulePassed = func() {
		if previous != nil {
			previous()
		}
		once.Do(func() { close(passed) })
	}
	f.start(t)
	f.opts.schedulePassed = previous
	fixturekit.AwaitPollSignal(t, passed)
}

func serviceStuckAtTheLandingStep() int {
	root := os.Getenv("OSMIA_TEST_CRASH_ROOT")
	at, err := time.Parse(time.RFC3339Nano, os.Getenv("OSMIA_TEST_CRASH_AT"))
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	opts := Options{Options: service.Options{Config: config.Options{Root: root}}}
	opts.Reconciliation.Now = func() time.Time { return at }
	opts.Reconciliation.Hold = func(_ config.WorkstreamID, op coreadapter.Operation) bool {
		in, err := execution.DecodeLand(op)
		return err != nil || in.Unit != "resume"
	}
	opts.boundary = func(ctx context.Context, step string) error {
		if step == "land-advanced" {
			fmt.Fprintln(os.Stdout, "clone-step-open")
			<-ctx.Done()
			return ctx.Err()
		}
		return nil
	}
	s, err := Start(context.Background(), opts)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	defer s.Close()
	if err := s.Wait(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	return 0
}

func pathCrashJJService(t *testing.T, root string, at time.Time) {
	t.Helper()
	exe, err := os.Executable()
	fixturekit.Must(t, err)
	cmd := exec.Command(exe, "-test.timeout=2m")
	cmd.Env = append(os.Environ(), "OSMIA_TEST_CRASH_ROOT="+root, "OSMIA_TEST_CRASH_AT="+at.Format(time.RFC3339Nano))
	var logs bytes.Buffer
	cmd.Stderr = &logs
	output, err := cmd.StdoutPipe()
	fixturekit.Must(t, err)
	fixturekit.Must(t, cmd.Start())
	t.Cleanup(func() {
		if cmd.ProcessState == nil {
			cmd.Process.Kill()
			cmd.Wait()
		}
	})
	reached := make(chan bool, 1)
	go func() {
		scanner := bufio.NewScanner(output)
		for scanner.Scan() {
			if scanner.Text() == "clone-step-open" {
				reached <- true
				return
			}
		}
		reached <- false
	}()
	select {
	case ok := <-reached:
		if !ok {
			cmd.Wait()
			t.Fatalf("service crash checkpoint not reached: %s", logs.String())
		}
	case <-time.After(fixturekit.DemoTimeout):
		cmd.Process.Kill()
		cmd.Wait()
		t.Fatalf("service crash checkpoint timed out: %s", logs.String())
	}
	fixturekit.Must(t, cmd.Process.Kill())
	if err := cmd.Wait(); err == nil {
		t.Fatal("crashed service returned successfully")
	}
}

func TestServiceEventProfileKeepsItsWindowAndOwnerProfile(t *testing.T) {
	t.Parallel()
	opts, _ := conversationFixture(t, "event-profile-")
	path := filepath.Join(opts.Config.Root, "config.toml")
	data, err := os.ReadFile(path)
	fixturekit.Must(t, err)
	fixturekit.Must(t, os.WriteFile(path, append(data, []byte("\n[roles.chief_of_staff]\nevents_profile = \"other\"\n")...), 0600))
	cfg, err := projectstore.LoadConfig(opts.Config)
	fixturekit.Must(t, err)
	clock := &fixturekit.FixedClock{At: fixturekit.DemoStart}
	opts.Reconciliation.Now = clock.Now
	var passes atomic.Int32
	opts.schedulePassed = func() { passes.Add(1) }
	turns := &fixturekit.ChiefTurns{}
	runChief(&opts, cfg, turns)
	s, c := start(t, opts)
	repo := s.sole().repository

	h := trace.Header{Schema: "osmia.trace.transition", Version: 1, Revision: 1, ID: "profile-notice", Project: cfg.Project.ID, Workstream: fixturekit.Stream, At: clock.Now(), Actor: intake.ServiceActor, Cause: "profile-notice"}
	_, err = repo.Transact(context.Background(), trace.Transaction{Transition: trace.Transition{Header: h, Subject: "profile-notice", To: "ready", Reason: "profile notice"}, Events: []trace.Event{{ID: "profile-notice-event", Kind: "notice", Body: "Profile notice"}}})
	fixturekit.Must(t, err)
	before := passes.Load()
	repo.WakeWorkflow()
	pathAwait(t, s, "event window pass did not finish", func() bool { return passes.Load() > before })
	th, err := repo.ChiefOfStaffThread(fixturekit.Stream)
	fixturekit.Must(t, err)
	for _, q := range th.Turns {
		if strings.Contains(q.Request.Prompt, "Profile notice") {
			t.Fatalf("event delivered before window: %+v", q)
		}
	}
	clock.Advance(cfg.EventWindow())
	repo.WakeWorkflow()
	pathAwait(t, s, "event profile turn did not finish", func() bool {
		th, _ := repo.ChiefOfStaffThread(fixturekit.Stream)
		for _, q := range th.Turns {
			if strings.Contains(q.Request.Prompt, "Profile notice") {
				return !q.CompletedAt.IsZero()
			}
		}
		return false
	})
	th, err = repo.ChiefOfStaffThread(fixturekit.Stream)
	fixturekit.Must(t, err)
	eventProfile, err := InspectForTest(s).ChiefEventsProfile(cfg)()
	fixturekit.Must(t, err)
	for _, q := range th.Turns {
		if strings.Contains(q.Request.Prompt, "Profile notice") && q.Request.Profile.Name != eventProfile.Name {
			t.Fatalf("event profile=%s selector=%s", q.Request.Profile.Name, eventProfile.Name)
		}
	}
	sent, err := c.Send(context.Background(), fixturekit.Stream, "Owner profile")
	fixturekit.Must(t, err)
	pathAwait(t, s, "owner message did not finish", func() bool { return pathTurnDone(repo, fixturekit.Stream, trace.ChiefOfStaff, sent.Turn) })
	th, err = repo.ChiefOfStaffThread(fixturekit.Stream)
	fixturekit.Must(t, err)
	_, ownerProfile, err := InspectForTest(s).ChiefOverride(cfg)
	fixturekit.Must(t, err)
	for _, q := range th.Turns {
		if q.Request.TurnID == sent.Turn && q.Request.Profile.Name != ownerProfile.Name {
			t.Fatalf("owner profile=%s role=%s", q.Request.Profile.Name, ownerProfile.Name)
		}
	}
}
