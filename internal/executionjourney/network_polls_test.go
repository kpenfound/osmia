package executionjourney

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/pulls"
	"github.com/kpenfound/osmia/internal/reconcile"
	"github.com/kpenfound/osmia/internal/runtime"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	"github.com/kpenfound/osmia/internal/trace"
)

// heldPoll supplies a lookup that cannot finish until shutdown and an explicit
// exit gate. Later lookups provide exact merge evidence for restart recovery.
type heldPoll struct {
	pulls.Client
	once      sync.Once
	entered   chan struct{}
	cancelled chan struct{}
	exit      chan struct{}
	calls     atomic.Int64
}

func (p *heldPoll) Find(ctx context.Context, _, fork, branch string) ([]pulls.PullRequest, error) {
	if p.calls.Add(1) == 1 {
		p.once.Do(func() { close(p.entered) })
		<-ctx.Done()
		close(p.cancelled)
		<-p.exit
		return nil, ctx.Err()
	}
	return []pulls.PullRequest{{Number: 42, Merged: true, MergeCommit: "merged-tip", State: "closed", Head: branch, HeadRepository: fork}}, nil
}

var awaitPollSignal = fixturekit.AwaitPollSignal

func TestNetworkPollingKeepsPassesAndConversationsRunning(t *testing.T) {
	for _, archived := range []bool{false, true} {
		t.Run(map[bool]string{false: "own-pr", true: "archive"}[archived], func(t *testing.T) {
			ctx := context.Background()
			opts, cfg := conversationFixture(t, "network-poll-")
			clock := &fixedClock{At: demoStart}
			opts.Reconciliation.Now = clock.Now
			turns := &chiefTurns{}
			runChief(&opts, cfg, turns)
			host := &heldPoll{entered: make(chan struct{}), cancelled: make(chan struct{}), exit: make(chan struct{})}
			var exit sync.Once
			opts.PullRequests = host
			var passes atomic.Int64
			opts.schedulePassed = func() { passes.Add(1) }
			s, client := start(t, opts)
			t.Cleanup(func() { exit.Do(func() { close(host.exit) }) })
			repo := s.sole().repository
			design, err := client.OpenDesign(ctx, cfg.Project.ID)
			must(t, err)
			session := config.WorkstreamID("w_00000000000000000000000000000111")
			must(t, repo.CreateSessionWorkstream(ctx, session, "default", "base", "session-test", "Session", s.now(), ownerActor))
			_, err = ensureSessionThread(ctx, repo, cfg.Project.ID, session, s.now())
			must(t, err)
			publication := DeliveryPublication{Status: publicationOpened, Upstream: cfg.Project.Upstream, Fork: cfg.Project.PushRepository(), Branch: featureBranch(stream), PullRequest: 42}
			if archived {
				must(t, s.store.SetArchived(runtime.Archive{Project: cfg.Project.ID, Workstream: stream, State: DeliveredState, ArchivedAt: demoStart, ResolutionEvidence: "unresolved", Publication: runtime.ArchivePublication{Upstream: publication.Upstream, Fork: publication.Fork, Branch: publication.Branch, PullRequest: 42}}))
				InspectForTest(s).WakeConversations()
			} else {
				recordUpstreamOwnPullRequest(t, s, repo, stream, publication)
			}
			awaitPollSignal(t, host.entered)
			before := passes.Load()
			repo.WakeWorkflow()
			conversationAwait(t, func() bool { return passes.Load() > before })
			chiefStream := stream
			if archived {
				chiefStream = quiet
			}
			chief, err := client.Send(ctx, chiefStream, "Chief message")
			must(t, err)
			designed, err := client.DesignMessage(ctx, design.Design.Workstream, "Design message")
			must(t, err)
			messaged, err := client.SessionMessage(ctx, session, "Session message")
			must(t, err)
			for _, want := range []struct {
				stream      config.WorkstreamID
				agent, turn string
			}{{chiefStream, trace.ChiefOfStaff, chief.Turn}, {design.Design.Workstream, trace.ChiefOfStaff, designed.Turn}, {session, sessionAgentID, messaged.Turn}} {
				conversationAwait(t, func() bool {
					th, err := repo.Thread(want.stream, want.agent)
					if err != nil {
						return false
					}
					for _, q := range th.Turns {
						if q.Request.TurnID == want.turn {
							return q.Response != nil && !q.CompletedAt.IsZero()
						}
					}
					return false
				})
			}
			// Close must cancel the client and still join it before closing trace.
			closed := make(chan error, 1)
			go func() { closed <- s.Close() }()
			awaitPollSignal(t, host.cancelled)
			select {
			case err := <-closed:
				t.Fatalf("service closed before the poll exited: %v", err)
			default:
			}
			_, err = repo.Workstreams()
			must(t, err)
			exit.Do(func() { close(host.exit) })
			select {
			case err := <-closed:
				must(t, err)
			case <-time.After(demoTimeout):
				t.Fatal("shutdown did not join the poll")
			}
			// The interrupted lookup supplied no evidence. A restart repeats it.
			clock.Advance(time.Second)
			s, _ = start(t, opts)
			repo = s.sole().repository
			conversationAwait(t, func() bool {
				if archived {
					a, found := archiveRecord(t, s.store, stream)
					return found && archiveResolved(a) && a.MergeCommit == "merged-tip"
				}
				out, revision, err := ownPullRequestOutcomeAt(repo, stream)
				must(t, err)
				return revision == 1 && out.PullRequest == 42 && out.MergeCommit == "merged-tip"
			})
			must(t, s.Close())
			if host.calls.Load() != 2 {
				t.Fatalf("lookups=%d, want interrupted lookup and restart lookup", host.calls.Load())
			}
		})
	}
}

func TestBaseRefreshBypassesOwnWorkflow(t *testing.T) {
	t.Parallel()
	cfg, repo, host, refresh := ownPRFixture(t)
	ctx := context.Background()
	// Reserve a workflow on the same workstream without running its effect.
	event := "held-network-workflow"
	op := coreadapter.Operation{ID: trace.OperationID(cfg.Project.ID, stream, event), Boundary: coreadapter.RepositoryBoundary, Action: event, Input: []byte(`{}`)}
	_, err := repo.Transact(ctx, trace.Transaction{Transition: trace.Transition{Header: trace.Header{Schema: "osmia.trace.transition", Version: 1, ID: event, Revision: 1, Project: cfg.Project.ID, Workstream: stream, At: demoStart, Actor: serviceActor, Cause: "fixture"}, Subject: event, To: "held", Reason: "Hold workflow"}, Events: []trace.Event{{ID: event, Kind: event, Body: "Hold workflow", Operation: &op}}})
	must(t, err)
	attempt, _, err := repo.BeginOperation(ctx, stream, event, trace.WorkflowOperation, serviceActor, refresh.s.now)
	must(t, err)
	if attempt == nil {
		t.Fatal("workflow was not reserved")
	}
	defer attempt.Close()
	sealStream(t, repo)
	p := deliverWithPullRequest(t, repo, cfg, 42)
	host.Prs = []pulls.PullRequest{{Number: 42, Merged: true, MergeCommit: "merged-tip", Head: p.Branch, HeadRepository: p.Fork}}
	must(t, refresh.Pass(ctx))
	controller, err := reconcile.New(repo, reconcile.Options{Worker: "poll-test", Now: refresh.s.now, Concurrent: concurrentOperation, Class: operationClass, Limit: InspectForTest(refresh.s).GitOperationLimit(repo, nil), Adapters: map[coreadapter.OperationBoundary]coreadapter.Reconciler{coreadapter.RepositoryBoundary: refresh}})
	must(t, err)
	must(t, controller.Pass(ctx))
	must(t, controller.Wait())
	out, rev, err := ownPullRequestOutcomeAt(repo, stream)
	must(t, err)
	if rev != 1 || out.MergeCommit != "merged-tip" {
		t.Fatalf("poll waited for its workflow: %+v rev=%d", out, rev)
	}
}

func TestWorkgroupChangePollingKeepsSchedulingRunning(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	opts, cfg := conversationFixture(t, "change-network-")
	opts.Reconciliation.Now = (&fixedClock{At: demoStart}).Now
	host := &heldPoll{entered: make(chan struct{}), cancelled: make(chan struct{}), exit: make(chan struct{})}
	var exit sync.Once
	opts.PullRequests = host
	var passes atomic.Int64
	opts.schedulePassed = func() { passes.Add(1) }
	s, _ := start(t, opts)
	t.Cleanup(func() { exit.Do(func() { close(host.exit) }) })
	repo := s.sole().repository
	// A publication on a member that is not terminal reaches the change
	// observer without the delivered-member own-PR observer taking the gate.
	resolutionDocument(t, repo, stream, publicationDocument, publicationPath, DeliveryPublication{Status: publicationOpened, Upstream: cfg.Project.Upstream, Fork: cfg.Project.PushRepository(), Branch: featureBranch(stream), PullRequest: 42})
	c, api := s.requestChange(ctx, string(stream), ChangeRequest{Key: "blocked-cancel", Kind: "cancel", Scope: "workgroup", Change: "Cancel the workgroup"})
	if api != nil {
		t.Fatal(api)
	}
	conversationAwait(t, func() bool { return currentChange(t, s, stream, c.ID).State == "presented" })
	c = currentChange(t, s, stream, c.ID)
	_, api = s.decideChange(ctx, string(stream), c.ID, ChangeDecision{Revision: c.Revision, Decision: "approve", PullRequests: "close"})
	if api != nil {
		t.Fatal(api)
	}
	awaitPollSignal(t, host.entered)
	before := passes.Load()
	repo.WakeWorkflow()
	conversationAwait(t, func() bool { return passes.Load() > before })
	if held, err := changeHeld(repo, stream); err != nil || !held {
		t.Fatalf("change boundary lost during lookup: held=%v err=%v", held, err)
	}
	closed := make(chan error, 1)
	go func() { closed <- s.Close() }()
	awaitPollSignal(t, host.cancelled)
	exit.Do(func() { close(host.exit) })
	select {
	case err := <-closed:
		must(t, err)
	case <-time.After(demoTimeout):
		t.Fatal("change observer did not drain")
	}
}

func TestNetworkPollingStopsOnProjectRemoval(t *testing.T) {
	t.Parallel()
	opts, cfg := conversationFixture(t, "remove-network-")
	opts.Reconciliation.Now = (&fixedClock{At: demoStart}).Now
	host := &heldPoll{entered: make(chan struct{}), cancelled: make(chan struct{}), exit: make(chan struct{})}
	opts.PullRequests = host
	s, client := start(t, opts)
	var exit sync.Once
	t.Cleanup(func() { exit.Do(func() { close(host.exit) }) })
	repo := s.sole().repository
	must(t, s.store.SetArchived(runtime.Archive{Project: cfg.Project.ID, Workstream: stream, State: DeliveredState, ArchivedAt: demoStart, ResolutionEvidence: "unresolved", Publication: runtime.ArchivePublication{Upstream: cfg.Project.Upstream, Fork: cfg.Project.PushRepository(), Branch: featureBranch(stream), PullRequest: 42}}))
	InspectForTest(s).WakeConversations()
	awaitPollSignal(t, host.entered)
	removed := make(chan error, 1)
	go func() { _, err := client.RemoveProject(context.Background(), cfg.Project.ID); removed <- err }()
	awaitPollSignal(t, host.cancelled)
	select {
	case err := <-removed:
		t.Fatalf("project removal did not join poll: %v", err)
	default:
	}
	_, err := repo.Workstreams()
	must(t, err)
	exit.Do(func() { close(host.exit) })
	select {
	case err := <-removed:
		must(t, err)
	case <-time.After(demoTimeout):
		t.Fatal("project removal did not finish")
	}
}
