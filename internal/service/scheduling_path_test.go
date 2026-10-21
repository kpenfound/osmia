package service

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
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
	"github.com/kpenfound/osmia/internal/seal"
	"github.com/kpenfound/osmia/internal/thread"
	"github.com/kpenfound/osmia/internal/trace"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// pathGate holds a service boundary until release or lifetime cancellation.
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
	deadline := time.Now().Add(demoTimeout)
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
	must(t, err)
	must(t, repo.CreateSessionWorkstream(ctx, session, "default", "base", "snapshot", "Session", demoStart, ownerActor))
	_, err = ensureSessionThread(ctx, repo, cfg.Project.ID, session, demoStart)
	must(t, err)
	profile, err := cfg.NamedProfile("default")
	must(t, err)
	_, err = repo.EnqueueTurn(ctx, trace.TurnRequest{Header: trace.Header{Schema: "osmia.trace.turn-request", Version: 1, Revision: 1, ID: "request_crashed", Project: cfg.Project.ID, Workstream: session, At: demoStart, Actor: ownerActor, Cause: "owner-message"}, AgentID: sessionAgentID, ThreadID: sessionAgentID, TurnID: "crashed", Profile: profile, Prompt: "Continue"})
	must(t, err)
	must(t, setSessionState(ctx, repo, cfg.Project.ID, session, demoStart, "session-working-crashed", "session-turn-started", trace.SessionWorking))
	_, err = repo.ClaimTurn(ctx, session, sessionAgentID, "previous-service", "previous-session", demoStart)
	must(t, err)
	must(t, repo.Close())
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
		return thread.Dispatcher{Runner: thread.Runner{Store: r, Now: opts.Reconciliation.Now, Turns: turnsFunc(func(ctx context.Context, p coreadapter.PreparedTurn) (coreadapter.SessionResult, error) {
			calls.Add(1)
			if err := backend.hold(ctx); err != nil {
				return coreadapter.SessionResult{}, err
			}
			return coreadapter.SessionResult{Session: coreadapter.BackendSession{Backend: p.Profile.Backend, ID: "recovered-session"}, FinalResponse: "Recovered reply"}, nil
		})}, Prepare: func(context.Context, thread.TurnInput) (coreadapter.PreparedTurn, error) {
			return coreadapter.PreparedTurn{SessionDirectory: t.TempDir()}, nil
		}, Relay: func(ctx context.Context, th trace.Thread, q trace.QueuedTurn) error {
			return sessionRelay(ctx, r, opts.Reconciliation.Now, th, q)
		}}, nil
	}
	s, _ := start(t, opts)
	awaitPollSignal(t, backend.entered)
	armed.Store(true)
	s.conversationSignal.wake()
	awaitPollSignal(t, scan.entered)
	backend.open()
	pathAwait(t, s, "recovery did not acknowledge", func() bool { return pathAcknowledged(s.sole().repository, session, "session-recover-1") })
	scan.open()
	// A following scan gives a deterministic barrier after the stale snapshot.
	s.conversationSignal.wake()
	awaitPollSignal(t, settled)
	armed.Store(false)
	must(t, s.Close())
	repo, err = trace.Open(cfg.Root, cfg.Project)
	must(t, err)
	defer repo.Close()
	state, err := repo.Workflow(session, trace.SessionSubject)
	must(t, err)
	if state.Value != trace.SessionIdle || calls.Load() != 1 {
		t.Fatalf("recovery state=%s backend calls=%d", state.Value, calls.Load())
	}
	th, err := repo.Thread(session, sessionAgentID)
	must(t, err)
	if len(th.Turns) != 2 || th.Turns[0].Status() != "interrupted" || th.Turns[1].CompletedAt.IsZero() || th.Turns[1].Status() != "idle" {
		t.Fatalf("recovery turns: %+v", th.Turns)
	}
}

// pathLog records structured scheduling diagnostics without racing readers.
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

// Approved state and local repositories are seeded while stopped. All
// admission, reconciliation, Git effects and conversation dispatch run in Start.
func TestServiceHeldLandingAndStepAllowEveryConversation(t *testing.T) {
	t.Parallel()
	f, a, repo := newApprovedFixture(t, "held-path")
	must(t, repo.Close())
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
		return op.Action != LandAction && !conversationOperation(op)
	}
	f.start(t)
	defer f.stop(t)
	awaitPollSignal(t, landing.entered)
	design, err := f.c.OpenDesign(context.Background(), f.project)
	must(t, err)
	session, err := f.c.OpenSession(context.Background(), SessionOpenRequest{Project: f.project, Profile: "default"})
	must(t, err)
	armed.Store(true)
	f.repository().WakeWorkflow()
	awaitPollSignal(t, step.entered)
	chief, err := f.c.Send(context.Background(), a, "Chief message")
	must(t, err)
	designed, err := f.c.DesignMessage(context.Background(), design.Design.Workstream, "Design message")
	must(t, err)
	messaged, err := f.c.SessionMessage(context.Background(), session.Session.Workstream, "Session message")
	must(t, err)
	for _, q := range []struct {
		stream      config.WorkstreamID
		agent, turn string
	}{{a, trace.ChiefOfStaff, chief.Turn}, {design.Design.Workstream, trace.ChiefOfStaff, designed.Turn}, {session.Session.Workstream, sessionAgentID, messaged.Turn}} {
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
	must(t, repo.Close())
	landing, draft := newPathGate(t), newPathGate(t)
	var allowC atomic.Bool
	f.opts.Reconciliation.Hold = func(stream config.WorkstreamID, op coreadapter.Operation) bool {
		if conversationOperation(op) || op.Action == DraftAction {
			return false
		}
		return op.Action != LandAction || (stream == c && !allowC.Load())
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
	awaitPollSignal(t, landing.entered)
	b := f.handIn(t, "path-b", handedDesign)
	awaitPollSignal(t, draft.entered)
	allowC.Store(true)
	f.repository().WakeWorkflow()
	pathAwait(t, f.s, "C did not merge beside A and B", func() bool {
		state, _ := f.repository().Workflow(c, trace.UnitSubject("resume"))
		return state.Value == UnitMerged
	})
	sent, err := f.c.Send(context.Background(), a, "Reply while A and B are held")
	must(t, err)
	pathAwait(t, f.s, "chief did not reply", func() bool { return pathTurnDone(f.repository(), a, trace.ChiefOfStaff, sent.Turn) })
	state, err := f.repository().Workflow(a, trace.UnitSubject("resume"))
	must(t, err)
	if state.Value != UnitApproved {
		t.Fatalf("A=%s", state.Value)
	}
	state, err = f.repository().Workflow(b, trace.FeatureSubject)
	must(t, err)
	if state.Value != HandedState {
		t.Fatalf("B=%s", state.Value)
	}
	landing.open()
	draft.open()
}

func TestServiceTwoWorkstreamsLandAtOnce(t *testing.T) {
	t.Parallel()
	f, a, repo := newApprovedFixture(t, "two-land-a")
	b := approvedStream(t, f, repo, "two-land-b", config.WorkspacesGit)
	must(t, repo.Close())
	gate := newPathGate(t)
	entered := make(chan struct{}, 2)
	f.opts.Reconciliation.Hold = func(_ config.WorkstreamID, op coreadapter.Operation) bool {
		return op.Action != LandAction && !conversationOperation(op)
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
	awaitPollSignal(t, entered)
	awaitPollSignal(t, entered)
	gate.open()
	for _, stream := range []config.WorkstreamID{a, b} {
		pathAwait(t, f.s, "concurrent landing did not merge", func() bool {
			state, _ := f.repository().Workflow(stream, trace.UnitSubject("resume"))
			return state.Value == UnitMerged
		})
	}
}

func TestServiceTwoMasonsRunDuringOwnLanding(t *testing.T) {
	t.Parallel()
	f, _ := newParallelMasonFixture(t, 2, 2, disjointPlan)
	base := strings.TrimSpace(demoGit(t, f.clone, "-C", f.clone, "rev-parse", "HEAD"))
	stream, repo := seedBuild(t, f, "worker-workflow", disjointPlan, config.WorkspacesGit, base)
	seedReview(t, f, repo, stream, "resume", resumeReport)
	approveDirectly(t, f.s, repo, stream, "resume", "spec#1")
	must(t, repo.Close())
	landing, workers := newPathGate(t), newPathGate(t)
	entered := make(chan struct{}, 2)
	var active, peak atomic.Int32
	f.engine.Mu.Lock()
	for _, unit := range []string{"upload", "audit"} {
		f.engine.Turns[masonTurnID(unit)] = func(ctx context.Context, req agent.Request, _ *agent.Turn, _ *mcp.ClientSession) (*agent.Result, error) {
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
		if conversationOperation(op) || op.Action == LandAction {
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
	awaitPollSignal(t, landing.entered)
	allowWorkers.Store(true)
	f.repository().WakeWorkflow()
	awaitPollSignal(t, entered)
	awaitPollSignal(t, entered)
	if active.Load() != 2 || peak.Load() != 2 {
		t.Fatalf("mason concurrency active=%d peak=%d", active.Load(), peak.Load())
	}
	if used := f.s.pool.Slots(nil).Used[masonRole]; used != 2 {
		t.Fatalf("mason capacity=%d", used)
	}
	for _, unit := range []string{"upload", "audit"} {
		th, err := f.repository().Thread(stream, masonAgent(unit))
		must(t, err)
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
			stream, _ := f.ratified(t)
			awaitPollSignal(t, gate.entered)
			f.stop(t)
			f.opts.boundary = nil
			jump(f.clock, f.clock.Now().Add(2*time.Second))
			f.start(t)
			pathAwait(t, f.s, "seal/build did not reconcile", func() bool {
				state, _ := f.repository().Workflow(stream, trace.FeatureSubject)
				if state.Value != BuildingState {
					return false
				}
				records, _ := f.repository().Operations(stream)
				var seals, builds int
				for _, r := range records {
					if r.Operation.Action == SealAction && r.Acknowledged {
						seals++
					}
					if r.Operation.Action == BuildAction && r.Acknowledged {
						builds++
					}
				}
				return seals == 1 && builds == 1
			})
			assertEffects := func() {
				t.Helper()
				docs := streamDocuments(t, f.repository(), stream, seal.DocumentID)
				if len(docs) != 1 {
					t.Fatalf("seal revisions=%d", len(docs))
				}
				transitions, err := trace.Read[trace.Transition](f.repository(), stream)
				must(t, err)
				seen := map[string]int{}
				for _, tr := range transitions {
					if tr.Actor == buildingActor || tr.Actor == sealingActor {
						seen[tr.ID]++
					}
				}
				for id, n := range seen {
					if n != 1 {
						t.Fatalf("effect %s recorded %d times", id, n)
					}
				}
				for _, id := range []string{BuildingState, "unit-resume-planned", "unit-resume-ready", "unit-dedupe-planned"} {
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
				requireJJ(t)
			}
			t.Parallel()
			f, a, repo := newApprovedFixtureOn(t, "restart-a", backend)
			b := approvedStream(t, f, repo, "restart-b", backend)
			must(t, repo.Close())
			remote := newPathGate(t)
			var allowB atomic.Bool
			f.opts.Reconciliation.Hold = func(stream config.WorkstreamID, op coreadapter.Operation) bool {
				if conversationOperation(op) {
					return false
				}
				if op.Action != LandAction {
					return true
				}
				in, err := decodeLand(op)
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
			awaitPollSignal(t, remote.entered)
			allowB.Store(true)
			f.repository().WakeWorkflow()
			pathAwait(t, f.s, "B did not finish before A", func() bool {
				records := landOperations(t, f.repository(), b)
				return len(records) == 1 && records[0].Acknowledged
			})
			beforeRecords, err := json.Marshal(landOperations(t, f.repository(), b))
			must(t, err)
			bTip := featureTip(t, f, b)
			g := providerOf(t, featureWorkspaces(f.s.cfg, f.repository()), b)
			bWorkspace, found, err := g.Workspace(context.Background(), string(b))
			must(t, err)
			if !found {
				t.Fatal("B workspace missing")
			}
			bFile, err := os.ReadFile(filepath.Join(bWorkspace.Path, trackedFile))
			must(t, err)

			if backend == config.WorkspacesJujutsu {
				// Stop A before its local step, then crash the next service lifetime
				// with that step's checkpoint open. B's effect is already settled.
				f.stop(t)
				crashAt := f.clock.Now().Add(time.Hour)
				pathCrashJJService(t, f.opts.Config.Root, crashAt)
				jump(f.clock, crashAt.Add(time.Hour))
				if len(checkpoints(t, f, branchesDirectory)) != 1 {
					t.Fatal("crash left no outstanding local-step checkpoint")
				}
			} else {
				armed.Store(true)
				remote.open()
				f.repository().WakeWorkflow()
				awaitPollSignal(t, gate.entered)
				f.stop(t)
			}
			f.opts.boundary = nil
			jump(f.clock, f.clock.Now().Add(2*time.Second))
			f.start(t)
			pathAwait(t, f.s, "A landing did not recover", func() bool {
				records := landOperations(t, f.repository(), a)
				return len(records) == 1 && records[0].Acknowledged
			})
			f.stop(t)
			repo, err = trace.Open(f.s.cfg.Root, f.s.cfg.Project)
			must(t, err)
			defer repo.Close()
			afterRecords, err := json.Marshal(landOperations(t, repo, b))
			must(t, err)
			if !bytes.Equal(beforeRecords, afterRecords) || featureTip(t, f, b) != bTip {
				t.Fatal("B's completed operation or branch was changed by A recovery")
			}
			afterFile, err := os.ReadFile(filepath.Join(bWorkspace.Path, trackedFile))
			must(t, err)
			if !bytes.Equal(bFile, afterFile) {
				t.Fatal("B's workspace changed")
			}
			if backend == config.WorkspacesJujutsu {
				onCommit(t, g, bWorkspace, bTip)
				if paths := checkpoints(t, f, branchesDirectory); len(paths) != 0 {
					t.Fatalf("outstanding checkpoints after recovery: %v", paths)
				}
			} else {
				if head := strings.TrimSpace(demoGit(t, f.clone, "-C", bWorkspace.Path, "rev-parse", "HEAD")); head != bTip {
					t.Fatalf("B workspace head=%s, want %s", head, bTip)
				}
				if status := strings.TrimSpace(demoGit(t, f.clone, "-C", bWorkspace.Path, "status", "--porcelain")); status != "" {
					t.Fatalf("B workspace dirty: %s", status)
				}
			}
			for _, stream := range []config.WorkstreamID{a, b} {
				records := landOperations(t, repo, stream)
				if len(records) != 1 || records[0].Result == nil || records[0].Result.Outcome != "succeeded" {
					t.Fatalf("landing results: %+v", records)
				}
				docs := streamDocuments(t, repo, stream, landingDocument("resume"))
				if len(docs) != 1 {
					t.Fatalf("landing effects=%d", len(docs))
				}
				in, err := decodeLand(records[0].Operation)
				must(t, err)
				if commits := f.landedCommits(t, stream, in.Base); len(commits) != 1 {
					t.Fatalf("landing commits: %v", commits)
				}
				state, err := repo.Workflow(stream, trace.UnitSubject("resume"))
				must(t, err)
				if state.Value != UnitMerged {
					t.Fatalf("unit state=%s", state.Value)
				}
			}
		})
	}
}

func TestServicePublicationEffectsSurviveRestart(t *testing.T) {
	t.Parallel()
	for _, point := range []string{"publish-pushed", "publish-opened"} {
		t.Run(point, func(t *testing.T) {
			t.Parallel()
			p := newPublicationFixture(t, "commit-per-unit")
			demoGit(t, filepath.Dir(p.fork), "-C", p.fork, "config", "core.logAllRefUpdates", "true")
			p.approve(t, nil)
			must(t, p.repository.Close())
			gate := newPathGate(t)
			p.opts.PullRequests = p.pulls
			p.opts.boundary = func(ctx context.Context, name string) error {
				if name == point {
					return gate.hold(ctx)
				}
				return nil
			}
			p.start(t)
			awaitPollSignal(t, gate.entered)
			pushed, found := p.forkBranch(t)
			if !found {
				t.Fatal("push did not reach fork")
			}
			p.stop(t)
			p.opts.boundary = nil
			jump(p.clock, p.clock.Now().Add(2*time.Second))
			p.start(t)
			pathAwait(t, p.s, "publication did not recover", func() bool {
				records, _ := p.shedFixture.repository().Operations(p.stream)
				for _, r := range records {
					if r.Operation.Action == PublishAction {
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
			updates := strings.Fields(demoGit(t, filepath.Dir(p.fork), "-C", p.fork, "reflog", "show", "--format=%H", "refs/heads/"+featureBranch(p.stream)))
			if len(updates) != 1 || updates[0] != pushed {
				t.Fatalf("push effects: %v", updates)
			}
			p.pulls.Mu.Lock()
			defer p.pulls.Mu.Unlock()
			if p.pulls.Creates != 1 || len(p.pulls.Prs) != 1 {
				t.Fatalf("PR effects creates=%d PRs=%d", p.pulls.Creates, len(p.pulls.Prs))
			}
			repo, err := trace.Open(p.s.cfg.Root, p.s.cfg.Project)
			must(t, err)
			defer repo.Close()
			pubs, err := publications(repo, p.stream)
			must(t, err)
			if len(pubs) != 2 || pubs[1].Status != publicationOpened || pubs[1].Commit != pushed {
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
	must(t, err)
	if !found {
		t.Fatal("unit workspace missing")
	}
	must(t, os.WriteFile(filepath.Join(w.Path, "RESUME.md"), []byte("resume\n"), 0600))
	landed := moveFeature(t, f, stream, map[string]string{"LANDED.md": "landed\n"})
	must(t, repo.Close())
	gate := newPathGate(t)
	f.opts.Reconciliation.Hold = func(_ config.WorkstreamID, op coreadapter.Operation) bool {
		if conversationOperation(op) {
			return false
		}
		if op.Action != RebaseAction {
			return true
		}
		in, err := decodeRebase(op)
		return err != nil || in.Unit != "resume"
	}
	f.opts.boundary = func(ctx context.Context, name string) error {
		if name == "rebase-recorded" {
			return gate.hold(ctx)
		}
		return nil
	}
	f.start(t)
	awaitPollSignal(t, gate.entered)
	f.stop(t)
	f.opts.boundary = nil
	jump(f.clock, f.clock.Now().Add(2*time.Second))
	f.start(t)
	pathAwait(t, f.s, "rebase did not recover", func() bool {
		records := rebaseOperations(t, f.repository(), stream, "resume")
		return len(records) == 1 && records[0].Acknowledged
	})
	f.stop(t)
	pathStartAndPass(t, f)
	f.stop(t)
	repo, err = trace.Open(f.s.cfg.Root, f.s.cfg.Project)
	must(t, err)
	defer repo.Close()
	rebases := unitRebases(t, repo, stream, "resume")
	if len(rebases) != 1 || rebases[0].Onto != landed {
		t.Fatalf("rebase effects: %+v", rebases)
	}
	for file, want := range map[string]string{"RESUME.md": "resume\n", "LANDED.md": "landed\n"} {
		data, err := os.ReadFile(filepath.Join(w.Path, file))
		must(t, err)
		if string(data) != want {
			t.Fatalf("workspace %s=%q", file, data)
		}
	}
	if count := strings.TrimSpace(demoGit(t, filepath.Dir(f.clone), "-C", f.clone, "rev-list", "--count", landed+".."+unitBranch(stream, "resume"))); count != "1" {
		t.Fatalf("rebase commits=%s", count)
	}
}

func TestServiceDriftEffectSurvivesRestart(t *testing.T) {
	t.Parallel()
	f, stream, repo, _ := newFinalFixture(t, "service-drift")
	before := moveFeature(t, f, stream, map[string]string{"FEATURE.md": "feature\n"})
	upstream := advanceUpstream(t, f, map[string]string{"UPSTREAM.md": "upstream\n"})
	must(t, repo.Close())
	gate := newPathGate(t)
	var replays atomic.Int32
	f.opts.Reconciliation.Hold = func(_ config.WorkstreamID, op coreadapter.Operation) bool {
		return op.Action != DriftAction && !conversationOperation(op)
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
	must(t, err)
	awaitPollSignal(t, gate.entered)
	f.stop(t)
	f.opts.boundary = func(_ context.Context, name string) error {
		if name == "drift-replaying" {
			replays.Add(1)
		}
		return nil
	}
	jump(f.clock, f.clock.Now().Add(2*time.Second))
	f.start(t)
	pathAwait(t, f.s, "drift did not recover", func() bool {
		records := driftOperations(t, f.repository(), stream)
		return len(records) == 1 && records[0].Acknowledged
	})
	f.stop(t)
	pathStartAndPass(t, f)
	f.stop(t)
	repo, err = trace.Open(f.s.cfg.Root, f.s.cfg.Project)
	must(t, err)
	defer repo.Close()
	records := driftRecords(t, repo, stream)
	if len(records) != 2 || records[0].Outcome != driftReplayed || records[1].Outcome != driftRebased || replays.Load() != 1 {
		t.Fatalf("drift effects=%+v replays=%d", records, replays.Load())
	}
	if tip := featureTip(t, f, stream); tip == before || parentOf(t, f, tip) != upstream {
		t.Fatalf("drift tip=%s upstream=%s", tip, upstream)
	}
	if seals := seals(t, repo, stream); len(seals) != 2 || seals[1].Base.Commit != upstream {
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
	awaitPollSignal(t, passed)
}

// TestServiceJJCrashProcess runs only as a child of the clone recovery fixture.
// The parent kills the process after a real foreman has advanced the branch
// inside LocalStep, leaving its checkpoint for Start's workspace recovery.
func TestServiceJJCrashProcess(t *testing.T) {
	root := os.Getenv("OSMIA_TEST_CRASH_ROOT")
	if root == "" {
		t.Skip("service crash subprocess")
	}
	at, err := time.Parse(time.RFC3339Nano, os.Getenv("OSMIA_TEST_CRASH_AT"))
	must(t, err)
	opts := Options{Config: config.Options{Root: root}}
	opts.Reconciliation.Now = func() time.Time { return at }
	opts.Reconciliation.Hold = func(_ config.WorkstreamID, op coreadapter.Operation) bool {
		in, err := decodeLand(op)
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
	must(t, err)
	defer s.Close()
	must(t, s.Wait())
}

func pathCrashJJService(t *testing.T, root string, at time.Time) {
	t.Helper()
	exe, err := os.Executable()
	must(t, err)
	cmd := exec.Command(exe, "-test.run=^TestServiceJJCrashProcess$", "-test.timeout=2m")
	cmd.Env = append(os.Environ(), "OSMIA_TEST_CRASH_ROOT="+root, "OSMIA_TEST_CRASH_AT="+at.Format(time.RFC3339Nano))
	var logs bytes.Buffer
	cmd.Stderr = &logs
	output, err := cmd.StdoutPipe()
	must(t, err)
	must(t, cmd.Start())
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
	case <-time.After(demoTimeout):
		cmd.Process.Kill()
		cmd.Wait()
		t.Fatalf("service crash checkpoint timed out: %s", logs.String())
	}
	must(t, cmd.Process.Kill())
	if err := cmd.Wait(); err == nil {
		t.Fatal("crashed service returned successfully")
	}
}

func TestServiceEventProfileKeepsItsWindowAndOwnerProfile(t *testing.T) {
	t.Parallel()
	opts, _ := conversationFixture(t, "event-profile-")
	path := filepath.Join(opts.Config.Root, "config.toml")
	data, err := os.ReadFile(path)
	must(t, err)
	must(t, os.WriteFile(path, append(data, []byte("\n[roles.chief_of_staff]\nevents_profile = \"other\"\n")...), 0600))
	cfg, err := config.Load(opts.Config)
	must(t, err)
	clock := &fixedClock{At: demoStart}
	opts.Reconciliation.Now = clock.Now
	var passes atomic.Int32
	opts.schedulePassed = func() { passes.Add(1) }
	turns := &chiefTurns{}
	runChief(&opts, cfg, turns)
	s, c := start(t, opts)
	repo := s.sole().repository
	// An informational notice uses the ordinary event window, not the
	// longer progress window. Neither queue nor dispatch may move it early.
	h := trace.Header{Schema: "osmia.trace.transition", Version: 1, Revision: 1, ID: "profile-notice", Project: cfg.Project.ID, Workstream: stream, At: clock.Now(), Actor: serviceActor, Cause: "profile-notice"}
	_, err = repo.Transact(context.Background(), trace.Transaction{Transition: trace.Transition{Header: h, Subject: "profile-notice", To: "ready", Reason: "profile notice"}, Events: []trace.Event{{ID: "profile-notice-event", Kind: "notice", Body: "Profile notice"}}})
	must(t, err)
	before := passes.Load()
	repo.WakeWorkflow()
	pathAwait(t, s, "event window pass did not finish", func() bool { return passes.Load() > before })
	th, err := repo.ChiefOfStaffThread(stream)
	must(t, err)
	for _, q := range th.Turns {
		if strings.Contains(q.Request.Prompt, "Profile notice") {
			t.Fatalf("event delivered before window: %+v", q)
		}
	}
	clock.Advance(cfg.EventWindow())
	repo.WakeWorkflow()
	pathAwait(t, s, "event profile turn did not finish", func() bool {
		th, _ := repo.ChiefOfStaffThread(stream)
		for _, q := range th.Turns {
			if strings.Contains(q.Request.Prompt, "Profile notice") {
				return !q.CompletedAt.IsZero()
			}
		}
		return false
	})
	th, err = repo.ChiefOfStaffThread(stream)
	must(t, err)
	eventProfile, err := s.chiefEventsProfile(cfg)()
	must(t, err)
	for _, q := range th.Turns {
		if strings.Contains(q.Request.Prompt, "Profile notice") && q.Request.Profile.Name != eventProfile.Name {
			t.Fatalf("event profile=%s selector=%s", q.Request.Profile.Name, eventProfile.Name)
		}
	}
	sent, err := c.Send(context.Background(), stream, "Owner profile")
	must(t, err)
	pathAwait(t, s, "owner message did not finish", func() bool { return pathTurnDone(repo, stream, trace.ChiefOfStaff, sent.Turn) })
	th, err = repo.ChiefOfStaffThread(stream)
	must(t, err)
	_, ownerProfile, err := s.chiefOverride(cfg)
	must(t, err)
	for _, q := range th.Turns {
		if q.Request.TurnID == sent.Turn && q.Request.Profile.Name != ownerProfile.Name {
			t.Fatalf("owner profile=%s role=%s", q.Request.Profile.Name, ownerProfile.Name)
		}
	}
}
